package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// RouterFeedbackStore persists /router-feedback submissions durably
// (router.router_feedback). Nil degrades to span + log only.
type RouterFeedbackStore interface {
	InsertRouterFeedback(ctx context.Context, p RouterFeedbackEvent) error
}

// RouterFeedbackEvent mirrors one router.router_feedback row.
type RouterFeedbackEvent struct {
	InstallationID string
	SessionKey     []byte
	Role           string
	RouterUserID   string
	ClientApp      string
	SessionID      string
	RequestedModel string
	ServedModel    string
	// Rating is the thumbs verdict ("up", "down", or "" for a note-only
	// submission), parsed from the /rf+ /rf- shortcuts or a leading verdict
	// token in the note.
	Rating string
	// Feedback is the human-readable submission persisted to
	// router.router_feedback. When the user gave only a verdict, it carries a
	// compact label ("👍" / "👎") so the column is never empty and stays
	// self-describing without a dedicated rating column.
	Feedback string
}

// handleRouterFeedbackCommand processes a /router-feedback directive: it
// persists the feedback to router.router_feedback, emits a router.feedback
// telemetry span (the same OTel pipeline the per-request decision/upstream
// spans ride, so the WorkWeave backend ingests it like any other event), and
// returns a synthetic acknowledgment without dispatching to any upstream.
func (s *Service) handleRouterFeedbackCommand(
	ctx context.Context,
	w http.ResponseWriter,
	env *translate.RequestEnvelope,
	cmd translate.RouterFeedbackResult,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	inputTokens int,
) error {
	log := observability.FromContext(ctx)
	role := roleForTier(catalog.TierFor(env.Model()))

	feedback := strings.TrimSpace(cmd.Feedback)
	rating := cmd.Rating
	if rating == "" && feedback == "" {
		// Nothing actionable: no verdict and no note. Acknowledgment text is
		// formatted as a routing marker so the existing
		// StripRoutingMarkerFromMessages ingress stripper removes it from
		// subsequent inbound requests (see handleForceModelCommand).
		msg := "✦ **Weave Router** → Router-feedback needs a verdict or a note, e.g. /rf+ or /rf- too slow.\n\n"
		if env.SourceFormat() == translate.FormatOpenAI {
			msg = "Weave Router: router-feedback needs a verdict or a note, e.g. /rf+ or /rf- too slow."
		}
		return writeSyntheticCommandResponse(w, env, msg, inputTokens)
	}

	// The model the user is most likely commenting on: what the session pin
	// last served, falling back to the pin's target model.
	var servedModel string
	if s.pinStore != nil {
		if pin, found, err := s.pinStore.Get(ctx, sessionKey, role); err != nil {
			log.Error("/router-feedback: pin lookup failed", "err", err)
		} else if found {
			servedModel = pin.LastServedModel
			if servedModel == "" {
				servedModel = pin.Model
			}
		}
	}

	clientID := ClientIdentityFrom(ctx)
	routerUserID := auth.UserIDFrom(ctx)
	externalID, _ := ctx.Value(ExternalIDContextKey{}).(string)

	if s.feedbackStore != nil && installationID != uuid.Nil {
		event := RouterFeedbackEvent{
			InstallationID: installationID.String(),
			SessionKey:     sessionKey[:],
			Role:           role,
			RouterUserID:   routerUserID,
			ClientApp:      clientID.ClientApp,
			SessionID:      clientID.SessionID,
			RequestedModel: env.Model(),
			ServedModel:    servedModel,
			Rating:         rating,
			Feedback:       persistedFeedbackText(rating, feedback),
		}
		// context.Background(): the request ctx may already be canceled by the
		// time this runs (client disconnected mid-command). Feedback the user
		// explicitly typed must not be dropped on a canceled context.
		if err := s.feedbackStore.InsertRouterFeedback(context.Background(), event); err != nil {
			log.Error("/router-feedback: feedback insert failed", "err", err)
			return err
		}
	}

	now := time.Now()
	otel.Record(ctx, otel.Span{
		Name:  "router.feedback",
		Start: now,
		End:   now,
		Attrs: otel.NewAttrBuilder(11).
			String("external_id", externalID).
			String("router_user_id", routerUserID).
			String("client.device_id", clientID.DeviceID).
			String("client.session_id", clientID.SessionID).
			String("client.user_agent", clientID.UserAgent).
			String("client.app", clientID.ClientApp).
			String("requested.model", env.Model()).
			String("feedback.served_model", servedModel).
			String("feedback.role", role).
			String("feedback.rating", rating).
			String("feedback.text", feedback).
			Build(),
	})
	otel.Flush(ctx)

	log.Info("router.feedback",
		"rating", rating,
		"feedback", feedback,
		"served_model", servedModel,
		"requested_model", env.Model(),
		"role", role,
	)

	return writeSyntheticCommandResponse(w, env, routerFeedbackAck(env.SourceFormat(), rating), inputTokens)
}

// routerFeedbackAck renders the synthetic acknowledgment for a recorded
// submission, echoing the verdict so the user sees their rating landed. The
// Anthropic-format ack is wrapped as a routing marker so the existing ingress
// stripper removes it from subsequent turns.
func routerFeedbackAck(format translate.Format, rating string) string {
	verdict := ""
	switch rating {
	case translate.RouterFeedbackRatingUp:
		verdict = " 👍"
	case translate.RouterFeedbackRatingDown:
		verdict = " 👎"
	}
	if format == translate.FormatOpenAI {
		return "Weave Router: Feedback recorded" + verdict + ". Thank you."
	}
	return "✦ **Weave Router** → Feedback recorded" + verdict + ". Thank you.\n\n"
}

// persistedFeedbackText is the value written to router.router_feedback.feedback.
// A verdict-only submission stores a compact emoji so the NOT NULL column is
// never empty and stays self-describing.
func persistedFeedbackText(rating, feedback string) string {
	if feedback != "" {
		return feedback
	}
	switch rating {
	case translate.RouterFeedbackRatingUp:
		return "👍"
	case translate.RouterFeedbackRatingDown:
		return "👎"
	}
	return feedback
}

// writeSyntheticCommandResponse writes a router-command acknowledgment in the
// inbound wire format without dispatching upstream. inputTokens is the
// request's RoutingFeatures.Tokens so the client's token counter reflects the
// actual turn input.
func writeSyntheticCommandResponse(w http.ResponseWriter, env *translate.RequestEnvelope, msg string, inputTokens int) error {
	switch env.SourceFormat() {
	case translate.FormatOpenAI:
		return writeSyntheticOpenAIResponse(w, env, msg, inputTokens)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg, inputTokens)
	}
}
