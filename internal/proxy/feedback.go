package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/feedback"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/router/turntype"

	"github.com/google/uuid"
)

// HeaderRouterFeedbackURL carries the no-login feedback page URL for the
// served request. Omitted when the feedback-link feature is unwired.
const HeaderRouterFeedbackURL = "x-router-feedback-url"

// routerFeedbackSpanName is the OTLP span the router emits on each submission so
// the Weave backend mirrors feedback into its own router_request_feedback table.
const routerFeedbackSpanName = "router.feedback"

// feedbackSourceLink labels feedback that arrived through the signed link page.
const feedbackSourceLink = "link"

// ErrFeedbackUnavailable is returned by the feedback service methods when no
// repository is wired (ROUTER_FEEDBACK_LINK_SECRET / DB not configured).
var ErrFeedbackUnavailable = errors.New("proxy: feedback repository not configured")

// FeedbackContext is the routing context shown on the feedback page for one
// request, plus any feedback already submitted for it.
type FeedbackContext struct {
	RequestID      string
	ChosenModel    string
	ChosenProvider string
	ClientApp      string
	RoutedAt       time.Time
	// Rating is "" when no feedback exists yet, otherwise "up" or "down".
	Rating  string
	Comment *string
}

// UpsertFeedbackParams carries one feedback row to persist.
type UpsertFeedbackParams struct {
	InstallationID string
	ExternalID     string
	RequestID      string
	Rating         string
	Comment        *string
	Source         string
	RouterUserID   string
}

// SubmitFeedbackParams carries one feedback submission from the API handler.
type SubmitFeedbackParams struct {
	InstallationID string
	ExternalID     string
	RequestID      string
	RouterUserID   string
	Rating         string
	Comment        *string
}

// FeedbackRepository persists and reads per-request human feedback. GetContext
// returns sql.ErrNoRows when the request id is unknown for the installation.
type FeedbackRepository interface {
	Upsert(ctx context.Context, p UpsertFeedbackParams) error
	GetContext(ctx context.Context, installationID, requestID string) (FeedbackContext, error)
}

// WithFeedback wires the feedback repository, signed-link signer, and public
// base URL of the feedback page. Any left nil/empty disables that capability
// (DB access, token verification, link emission respectively).
func (s *Service) WithFeedback(repo FeedbackRepository, signer *feedback.Signer, baseURL string) *Service {
	s.feedbackRepo = repo
	s.feedbackSigner = signer
	s.feedbackBaseURL = strings.TrimRight(baseURL, "/")
	return s
}

// FeedbackEnabled reports whether the signed feedback-link endpoints should
// be mounted (i.e. ROUTER_FEEDBACK_LINK_SECRET is set). The repository can
// still be nil in tests, so handlers guard independently.
func (s *Service) FeedbackEnabled() bool {
	return s != nil && s.feedbackSigner != nil
}

// VerifyFeedbackToken verifies a feedback-link token without leaking the
// signer to the presentation layer. Returns feedback.ErrInvalidToken /
// ErrExpiredToken for the handler to map to HTTP 404 / 410.
func (s *Service) VerifyFeedbackToken(token string) (feedback.Claims, error) {
	return s.feedbackSigner.Verify(token)
}

// GetFeedbackContext returns the routing context and any existing feedback for
// one request. Returns sql.ErrNoRows when the request id is unknown.
func (s *Service) GetFeedbackContext(ctx context.Context, installationID, requestID string) (FeedbackContext, error) {
	if s.feedbackRepo == nil {
		return FeedbackContext{}, ErrFeedbackUnavailable
	}
	return s.feedbackRepo.GetContext(ctx, installationID, requestID)
}

// SubmitFeedback persists feedback to the router's source-of-truth table and
// emits a router.feedback OTLP span so Weave mirrors it. The DB write is
// authoritative; the span is best-effort (dropped if the exporter queue is full).
func (s *Service) SubmitFeedback(ctx context.Context, p SubmitFeedbackParams) error {
	if s.feedbackRepo == nil {
		return ErrFeedbackUnavailable
	}
	err := s.feedbackRepo.Upsert(ctx, UpsertFeedbackParams{
		InstallationID: p.InstallationID,
		ExternalID:     p.ExternalID,
		RequestID:      p.RequestID,
		Rating:         p.Rating,
		Comment:        p.Comment,
		Source:         feedbackSourceLink,
		RouterUserID:   p.RouterUserID,
	})
	if err != nil {
		return err
	}
	s.emitFeedbackSpan(p)
	return nil
}

// emitFeedbackSpan ships a router.feedback span to the Weave OTLP ingest. The
// attribute keys are the contract the Weave backend's buildFeedbackRow reads.
func (s *Service) emitFeedbackSpan(p SubmitFeedbackParams) {
	if s.emitter == nil {
		return
	}
	now := time.Now()
	b := otel.NewAttrBuilder(6).
		String("external_id", p.ExternalID).
		String("request_id", p.RequestID).
		String("feedback.rating", p.Rating).
		String("feedback.source", feedbackSourceLink)
	if p.Comment != nil {
		b = b.String("feedback.comment", *p.Comment)
	}
	if p.RouterUserID != "" {
		b = b.String("router_user_id", p.RouterUserID)
	}
	buf := s.newTelemetryBuffer()
	buf.Record(otel.Span{Name: routerFeedbackSpanName, Start: now, End: now, Attrs: b.Build()})
	buf.Flush()
}

// mintFeedbackToken signs a feedback-link token, or returns "" when the
// feature is unwired or any required id is missing. Callers treat "" as
// "feedback disabled".
func (s *Service) mintFeedbackToken(installationID uuid.UUID, externalID, requestID, routerUserID string) string {
	if s.feedbackSigner == nil || s.feedbackBaseURL == "" {
		return ""
	}
	if installationID == uuid.Nil || externalID == "" || requestID == "" {
		return ""
	}
	return s.feedbackSigner.Mint(installationID.String(), externalID, requestID, routerUserID)
}

// setFeedbackLinkHeader mints a signed feedback link and sets it on the
// response. No-op when the feature is unwired or any required id is missing.
func (s *Service) setFeedbackLinkHeader(w http.ResponseWriter, installationID uuid.UUID, externalID, requestID, routerUserID string) {
	token := s.mintFeedbackToken(installationID, externalID, requestID, routerUserID)
	if token == "" {
		return
	}
	w.Header().Set(HeaderRouterFeedbackURL, s.feedbackBaseURL+"/f/"+token)
}

// terminalFeedbackClients are agents whose entire streamed response is
// user-facing chat, so a trailing /rf rating hint renders cleanly. IDEs
// (cursor) and unknown clients are excluded: they reuse the endpoint for
// inline edits, applied diffs, and commit messages, where a footer would
// contaminate non-chat output.
var terminalFeedbackClients = map[string]struct{}{
	ClientAppClaudeCode: {},
	ClientAppCodex:      {},
	ClientAppOpencode:   {},
}

// feedbackFooterText is deliberately link-free: Claude Code's markdown
// renderer prints "[label](url)" as "label (url)", so embedding the signed
// feedback link would dump the whole token inline. /rf+ and /rf- render
// identically across TUI, print mode, and IDEs.
// Keep in sync with translate.feedbackFooterPattern, which strips this back
// out of upstream context on ingress.
const feedbackFooterText = "\n\n_Weave Router feedback:_ `/rf +` good experience · `/rf -` poor experience · note optional, e.g. `/rf - too slow`"

// feedbackFooter returns the in-terminal rating hint for a streamed response,
// or "" to stay transparent. Gated to terminalFeedbackClients (avoid
// contaminating non-chat surfaces), to deployments with durable feedback
// storage (never advertise a command we can't record — the header-based link
// still ships for GUI clients), and to MainLoop/ToolResult turns: subagent
// dispatches and machine turns (compaction, probe, title-gen, classifier)
// render final text the user didn't directly initiate, where a trailing /rf
// hint would strand beneath it (e.g. under an Explore subagent's result).
func (s *Service) feedbackFooter(clientApp string, tt turntype.TurnType) string {
	if s.feedbackStore == nil {
		return ""
	}
	if _, ok := terminalFeedbackClients[clientApp]; !ok {
		return ""
	}
	if tt != turntype.MainLoop && tt != turntype.ToolResult {
		return ""
	}
	return feedbackFooterText
}
