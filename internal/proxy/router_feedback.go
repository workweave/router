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
	Feedback       string
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
	if feedback == "" {
		// Acknowledgment text is formatted as a routing marker so the existing
		// StripRoutingMarkerFromMessages ingress stripper removes it from
		// subsequent inbound requests (see handleForceModelCommand).
		msg := "✦ **Weave Router** → router-feedback needs a message, e.g. /router-feedback got stuck on Haiku for too long\n\n"
		if env.SourceFormat() == translate.FormatOpenAI {
			msg = "Weave Router: router-feedback needs a message, e.g. /router-feedback got stuck on Haiku for too long"
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
			Feedback:       feedback,
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
		Attrs: otel.NewAttrBuilder(10).
			String("external_id", externalID).
			String("router_user_id", routerUserID).
			String("client.device_id", clientID.DeviceID).
			String("client.session_id", clientID.SessionID).
			String("client.user_agent", clientID.UserAgent).
			String("client.app", clientID.ClientApp).
			String("requested.model", env.Model()).
			String("feedback.served_model", servedModel).
			String("feedback.role", role).
			String("feedback.text", feedback).
			Build(),
	})
	otel.Flush(ctx)

	log.Info("router.feedback",
		"feedback", feedback,
		"served_model", servedModel,
		"requested_model", env.Model(),
		"role", role,
	)

	msg := "✦ **Weave Router** → feedback recorded, thank you\n\n"
	if env.SourceFormat() == translate.FormatOpenAI {
		msg = "Weave Router: feedback recorded, thank you."
	}
	return writeSyntheticCommandResponse(w, env, msg, inputTokens)
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
