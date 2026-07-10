package proxy

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/observability/otel"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/policy"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// routerFeedbackCommandSpanName is the OTLP span for the /router-feedback (/rf)
// slash command. Distinct from routerFeedbackSpanName ("router.feedback" in
// feedback.go), which is a downstream contract (buildFeedbackRow); do not reuse
// that name or alter its schema.
const routerFeedbackCommandSpanName = "router.feedback.command"

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
	// Rating is the thumbs verdict ("up", "down", or "" for note-only),
	// parsed from /rf+ /rf- or a leading verdict token in the note.
	Rating string
	// Feedback is the persisted submission text; verdict-only submissions
	// get a compact emoji so the column is never empty.
	Feedback string
}

// handleRouterFeedbackCommand persists a /router-feedback submission, emits a
// router.feedback.command span on the standard OTel pipeline, and returns a
// synthetic acknowledgment without dispatching to any upstream.
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
		// No verdict and no note. Message is formatted as a routing marker so
		// StripRoutingMarkerFromMessages strips it from later requests.
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
		// context.Background(): ctx may already be canceled (client disconnected
		// mid-command); don't drop feedback the user explicitly typed.
		if err := s.feedbackStore.InsertRouterFeedback(context.Background(), event); err != nil {
			log.Error("/router-feedback: feedback insert failed", "err", err)
			return err
		}
	}
	strategy := router.StrategyFromContext(ctx)
	if registered, ok := s.strategies[strategy]; ok && registered.feedback != nil {
		trainingAllowed, _ := ctx.Value(PolicyTrainingAllowedContextKey{}).(bool)
		var trainingDelta []router.ConversationMessage
		if strategy == router.StrategyHMM && trainingAllowed {
			trainingDelta = routerFeedbackTrainingDelta(env)
		}
		s.reportRouterFeedback(ctx, registered.feedback, strategy, externalID, installationID, sessionKey, role, routerUserID, clientID, env.Model(), servedModel, rating, feedback, trainingDelta)
	}

	now := time.Now()
	otel.Record(ctx, otel.Span{
		Name:  routerFeedbackCommandSpanName,
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

	log.Info("router.feedback.command",
		"rating", rating,
		"feedback", feedback,
		"served_model", servedModel,
		"requested_model", env.Model(),
		"role", role,
	)

	return writeSyntheticCommandResponse(w, env, routerFeedbackAck(env.SourceFormat(), rating), inputTokens)
}

func (s *Service) reportRouterFeedback(
	ctx context.Context,
	reporter policy.FeedbackReporter,
	strategy router.Strategy,
	organizationID string,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
	routerUserID string,
	clientID ClientIdentity,
	requestedModel string,
	servedModel string,
	rating string,
	feedback string,
	trainingDelta []router.ConversationMessage,
) {
	payload := map[string]interface{}{
		"strategy":          string(strategy),
		"feedback_key":      hex.EncodeToString(sessionKey[:]),
		"feedback_role":     role,
		"rating":            rating,
		"feedback":          feedback,
		"requested_model":   requestedModel,
		"served_model":      servedModel,
		"router_user_id":    routerUserID,
		"client_app":        clientID.ClientApp,
		"client_session_id": clientID.SessionID,
	}
	if organizationID != "" {
		payload["organization_id"] = organizationID
	}
	rolloutID := clientID.RolloutID
	if persistedRolloutID, ok := ctx.Value(PolicyRolloutIDContextKey{}).(string); ok && persistedRolloutID != "" {
		rolloutID = persistedRolloutID
	}
	payload["rollout_id"] = rolloutID
	trainingAllowed, _ := ctx.Value(PolicyTrainingAllowedContextKey{}).(bool)
	payload["training_allowed"] = trainingAllowed
	if installationID != uuid.Nil {
		payload["installation_id"] = installationID.String()
	}
	if strategy == router.StrategyHMM && trainingAllowed && len(trainingDelta) > 0 {
		payload["training_conversation_delta"] = trainingDelta
	}
	log := observability.FromContext(ctx)
	observability.SafeGo(log, policyFeedbackReportTimeout, "reportPolicyFeedback", func(reportCtx context.Context) {
		if err := reporter.ReportFeedback(reportCtx, payload); err != nil {
			log.Error("/router-feedback: policy feedback report failed", "strategy", strategy, "err", err)
		}
	})
}

func routerFeedbackTrainingDelta(env *translate.RequestEnvelope) []router.ConversationMessage {
	messages := conversationMessagesForRouting(env)
	if len(messages) == 0 {
		return nil
	}
	ratedAssistant := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
			ratedAssistant = i
			break
		}
	}
	if ratedAssistant < 0 {
		return nil
	}
	start := 0
	for i := ratedAssistant - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
			start = i + 1
			break
		}
	}
	if start == 0 {
		for start < ratedAssistant && !strings.EqualFold(strings.TrimSpace(messages[start].Role), "user") {
			start++
		}
	}
	return append([]router.ConversationMessage(nil), messages[start:ratedAssistant+1]...)
}

// routerFeedbackAck renders the acknowledgment, echoing the verdict. The
// Anthropic-format ack is wrapped as a routing marker so it gets stripped
// from subsequent turns.
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
// Verdict-only submissions get a compact emoji so the NOT NULL column is never empty.
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
	return ""
}

// writeSyntheticCommandResponse writes a router-command acknowledgment in the
// inbound wire format without dispatching upstream.
func writeSyntheticCommandResponse(w http.ResponseWriter, env *translate.RequestEnvelope, msg string, inputTokens int) error {
	switch env.SourceFormat() {
	case translate.FormatOpenAI:
		return writeSyntheticOpenAIResponse(w, env, msg, inputTokens)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg, inputTokens)
	}
}
