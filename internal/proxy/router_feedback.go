package proxy

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
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
	// SuggestedLabel is the complexity label from a --label flag ("fast" | "explore" | "balanced" | "high" | "maximum").
	SuggestedLabel string
	// Feedback is the persisted submission text; verdict-only submissions
	// get a compact emoji so the column is never empty.
	Feedback string
	// Source is how the feedback was submitted: "user" (explicit /rf command)
	// or "auto" (automated judge at session stop).
	Source string
	// RequestID is the telemetry request_id of the rated turn; empty when no sequence was specified.
	RequestID string
	// RouteID is the opaque sidecar join key (HMM/RL) for credit assignment; empty when no sequence was specified.
	RouteID string
}

// RouterFeedbackSource values for validated event persistence.
const (
	RouterFeedbackSourceUser = "user"
	RouterFeedbackSourceAuto = "auto"
)

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

	// The model to attribute: pin's last served (or target), overridden by the resolved telemetry turn when a sequence was specified.
	var servedModel string
	var telemetryRequestID string
	var telemetryRouteID string
	var telemetryStrategy string
	if cmd.Sequence != 0 && s.telemetry != nil {
		turn, err := s.telemetry.GetTelemetryBySessionSequence(ctx, installationID, sessionKey[:], role, cmd.Sequence)
		if err != nil {
			log.Error("/router-feedback: sequence lookup failed", "sequence", cmd.Sequence, "err", err)
			if errors.Is(err, sql.ErrNoRows) {
				msg := "✦ **Weave Router** → No turn found at that sequence number. Try `/rf` without a number for the last turn.\n\n"
				if env.SourceFormat() == translate.FormatOpenAI {
					msg = "Weave Router: No turn found at that sequence number. Try `/rf` without a number for the last turn."
				}
				return writeSyntheticCommandResponse(w, env, msg, inputTokens)
			}
			// Infrastructure error: log it, fall through to the pin path so the
			// rating is persisted with the pin's servedModel rather than dropped.
		} else {
			servedModel = turn.DecisionModel
			telemetryRequestID = turn.RequestID
			telemetryRouteID = turn.RouteID
			telemetryStrategy = turn.Strategy
			log.Info("/router-feedback: resolved sequence to telemetry turn",
				"sequence", cmd.Sequence,
				"request_id", telemetryRequestID,
				"served_model", servedModel,
			)
		}
	}
	if servedModel == "" && s.pinStore != nil {
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
			SuggestedLabel: cmd.SuggestedLabel,
			Feedback:       persistedFeedbackText(rating, feedback),
			Source:         RouterFeedbackSourceUser,
			RequestID:      telemetryRequestID,
			RouteID:        telemetryRouteID,
		}
		// context.Background(): ctx may already be canceled (client disconnected
		// mid-command); don't drop feedback the user explicitly typed.
		if err := s.feedbackStore.InsertRouterFeedback(context.Background(), event); err != nil {
			log.Error("/router-feedback: feedback insert failed", "err", err)
			return err
		}
	}
	// Skip upsert for note-only ratings: up/down-only column rejects empty rating and would overwrite a prior thumb.
	if telemetryRequestID != "" && rating != "" && s.feedbackRepo != nil {
		comment := feedback
		upsertParams := UpsertFeedbackParams{
			InstallationID: installationID.String(),
			ExternalID:     externalID,
			RequestID:      telemetryRequestID,
			Rating:         rating,
			Comment:        &comment,
			Source:         "router-feedback-command",
			RouterUserID:   routerUserID,
		}
		if err := s.feedbackRepo.Upsert(context.Background(), upsertParams); err != nil {
			log.Error("/router-feedback: request_feedback upsert failed", "request_id", telemetryRequestID, "err", err)
		}
	}

	// Use the resolved turn's strategy, not the current request: StrategyFromContext credits the wrong reporter
	// when the rated turn ran under a different strategy. A resolved strategy with no reporter (e.g. cluster) suppresses feedback — falling back would credit the active reporter with another strategy's request_id/route_id.
	strategy := router.StrategyFromContext(ctx)
	if telemetryStrategy != "" {
		strategy = router.Strategy(telemetryStrategy)
	}
	if registered, ok := s.strategies[strategy]; ok && registered.feedback != nil {
		trainingAllowed, _ := ctx.Value(PolicyTrainingAllowedContextKey{}).(bool)
		// Delta only matches the rated turn for sequence 0 (latest) or -1 (the last assistant segment in env).
		// For anything older, suppress the delta; request_id + route_id give the sidecar the join key.
		trainingDelta := []router.ConversationMessage(nil)
		if (cmd.Sequence == 0 || cmd.Sequence == -1) && router.IsHMMStrategy(strategy) && trainingAllowed {
			trainingDelta = routerFeedbackTrainingDelta(env)
		}
		s.reportRouterFeedback(ctx, registered.feedback, strategy, externalID, installationID, sessionKey, role, routerUserID, clientID, env.Model(), servedModel, rating, feedback, cmd.SuggestedLabel, RouterFeedbackSourceUser, trainingDelta, telemetryRequestID, telemetryRouteID)
	}

	now := time.Now()
	attrs := otel.NewAttrBuilder(15).
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
		Int64("feedback.sequence", int64(cmd.Sequence)).
		String("feedback.text", feedback).
		String("feedback.source", RouterFeedbackSourceUser)
	if telemetryRequestID != "" {
		attrs = attrs.String("feedback.request_id", telemetryRequestID)
	}
	if telemetryRouteID != "" {
		attrs = attrs.String("feedback.route_id", telemetryRouteID)
	}
	otel.Record(ctx, otel.Span{
		Name:  routerFeedbackCommandSpanName,
		Start: now,
		End:   now,
		Attrs: attrs.Build(),
	})
	otel.Flush(ctx)

	log.Info("router.feedback.command",
		"rating", rating,
		"feedback", feedback,
		"served_model", servedModel,
		"requested_model", env.Model(),
		"role", role,
		"sequence", cmd.Sequence,
		"request_id", telemetryRequestID,
		"route_id", telemetryRouteID,
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
	suggestedLabel string,
	source string,
	trainingDelta []router.ConversationMessage,
	requestID string,
	routeID string,
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
		"source":            source,
		"request_id":        requestID,
		"route_id":          routeID,
	}
	if suggestedLabel != "" {
		payload["suggested_label"] = suggestedLabel
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
	if router.IsHMMStrategy(strategy) && trainingAllowed && len(trainingDelta) > 0 {
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
