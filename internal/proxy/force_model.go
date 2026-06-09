package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// ForceModelHeader pins the session to a specific model, mirroring the
// /force-model chat command. The header form exists so the directive is usable
// from headless clients (the eval harness, CI smoke runs) where the slash
// command is unreliable: Claude Code eats "/force-model …" as a client-side
// slash command before it ever reaches the router, and a typed two-call pin
// consumes a turn. A header rides on every request, so the pin is (re)written
// and served on the same turn. Empty or unrecognized values are ignored and
// routing proceeds automatically rather than failing the request.
const ForceModelHeader = "x-weave-force-model"

var forceModelAliases = map[string]string{
	"anthropic":      "claude-opus-4-8",
	"claude":         "claude-opus-4-8",
	"opus":           "claude-opus-4-8",
	"claude-opus":    "claude-opus-4-8",
	"opus-4-8":       "claude-opus-4-8",
	"opus-4.8":       "claude-opus-4-8",
	"claude-4-8":     "claude-opus-4-8",
	"claude-4.8":     "claude-opus-4-8",
	"fable":          "claude-fable-5",
	"fable-5":        "claude-fable-5",
	"fable5":         "claude-fable-5",
	"claude-fable":   "claude-fable-5",
	"sonnet":         "claude-sonnet-4-6",
	"claude-sonnet":  "claude-sonnet-4-6",
	"sonnet-4-6":     "claude-sonnet-4-6",
	"sonnet-4.6":     "claude-sonnet-4-6",
	"haiku":          "claude-haiku-4-5",
	"claude-haiku":   "claude-haiku-4-5",
	"haiku-4-5":      "claude-haiku-4-5",
	"haiku-4.5":      "claude-haiku-4-5",
	"gpt":            "gpt-5.5",
	"openai":         "gpt-5.5",
	"gpt-5-5":        "gpt-5.5",
	"gpt-5-5-pro":    "gpt-5.5-pro",
	"gpt-5-5-mini":   "gpt-5.5-mini",
	"gpt-5-5-nano":   "gpt-5.5-nano",
	"gpt-5-4":        "gpt-5.4",
	"gpt-5-4-pro":    "gpt-5.4-pro",
	"gpt-5-4-mini":   "gpt-5.4-mini",
	"gpt-5-4-nano":   "gpt-5.4-nano",
	"google":         "gemini-3-pro-preview",
	"gemini":         "gemini-3-pro-preview",
	"gemini-pro":     "gemini-3-pro-preview",
	"gemini-flash":   "gemini-3-flash-preview",
	"deepseek":       "deepseek/deepseek-v4-pro",
	"deepseek-pro":   "deepseek/deepseek-v4-pro",
	"deepseek-flash": "deepseek/deepseek-v4-flash",
	"qwen":           "qwen/qwen3-coder",
	"qwen-coder":     "qwen/qwen3-coder",
	"kimi":           "moonshotai/kimi-k2.6",
	"glm":            "z-ai/glm-5.1",
	"zai":            "z-ai/glm-5.1",
	"z-ai":           "z-ai/glm-5.1",
	"minimax":        "minimax/minimax-m2.7",
	"mistral":        "mistralai/mistral-small-2603",
}

// resolveForceModel maps a user-typed model identifier to its canonical
// catalog ID and primary provider binding. The catalog is the source of
// truth — heuristics are only a best-effort provider guess for inputs that
// are not in it.
//
// Resolution order:
//  1. Alias match for common user-facing shortcuts.
//  2. Exact match against `catalog.ByID` (the input is already canonical).
//  3. Suffix match: scan the catalog for any model whose ID ends with
//     "/" + input. Lets users type bare names like `qwen3-235b-a22b-2507`
//     and have the pin route to `qwen/qwen3-235b-a22b-2507` on its real
//     binding (bedrock, in this case) instead of misclassifying it as
//     Anthropic.
//  4. Naming heuristic for IDs not in the catalog (provider guess only).
//
// The returned `known` flag is true only for catalog matches (1, 2, and 3). A
// false `known` means the input has no catalog entry and therefore no known
// tier: the requested-model tier ceiling would rewrite a pin to it on every
// turn (clampToCeiling treats unknown tiers as above-ceiling), so the user
// would never actually be served the model. Callers must reject the command
// rather than pin an unservable directive. The heuristic provider is still
// returned for logging.
func resolveForceModel(model string) (canonicalID, provider string, known bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if alias, ok := forceModelAliases[model]; ok {
		model = alias
	}
	if m, ok := catalog.ByID(model); ok && len(m.Providers) > 0 {
		return m.ID, m.Providers[0].Provider, true
	}
	if !strings.Contains(model, "/") {
		suffix := "/" + model
		var matched catalog.Model
		var matches int
		for _, m := range catalog.Models {
			if strings.HasSuffix(m.ID, suffix) {
				matched = m
				matches++
			}
		}
		if matches == 1 && len(matched.Providers) > 0 {
			return matched.ID, matched.Providers[0].Provider, true
		}
	}
	switch {
	case strings.HasPrefix(model, "claude-"):
		return model, providers.ProviderAnthropic, false
	case strings.HasPrefix(model, "gpt-"),
		model == "o1", model == "o3", model == "o1-pro", model == "o3-pro",
		strings.HasPrefix(model, "o1-"), strings.HasPrefix(model, "o3-"), strings.HasPrefix(model, "o4-"):
		return model, providers.ProviderOpenAI, false
	case strings.HasPrefix(model, "gemini-"):
		return model, providers.ProviderGoogle, false
	case strings.Contains(model, "/"):
		return model, providers.ProviderOpenRouter, false
	default:
		return model, providers.ProviderAnthropic, false
	}
}

// setForceModelPin upserts an immutable user-forced session pin for the given
// session/role. It preserves the prior pin's LastServedModel so the next turn
// can still detect a mid-session model switch and strip stale Anthropic
// thinking-block signatures. No-op when the pin store is unconfigured or
// installationID is nil (pin rows require an installation_id).
func (s *Service) setForceModelPin(
	ctx context.Context,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
	installationID uuid.UUID,
	canonicalModel, provider string,
) error {
	if s.pinStore == nil || installationID == uuid.Nil {
		return nil
	}
	log := observability.FromContext(ctx)
	var lastServedModel string
	existing, found, err := s.pinStore.Get(ctx, sessionKey, role)
	if err != nil {
		log.Error("force-model: prior pin lookup failed", "err", err)
	} else if found {
		lastServedModel = existing.LastServedModel
	}
	forced := sessionpin.Pin{
		SessionKey:      sessionKey,
		Role:            role,
		InstallationID:  installationID,
		Provider:        provider,
		Model:           canonicalModel,
		Reason:          translate.ReasonUserForceModel,
		TurnCount:       1,
		PinnedUntil:     time.Now().Add(pinSessionTTL),
		LastServedModel: lastServedModel,
	}
	// context.Background(): the request ctx may already be canceled by the time
	// this runs (synthetic response written, or client disconnected). Upserting
	// on a canceled context would leave Postgres holding the prior pin.
	return s.pinStore.Upsert(context.Background(), forced)
}

// applyForceModelHeader honors the x-weave-force-model request header by writing
// a user-forced session pin, the same sticky the /force-model command writes.
// The pin is (re)written on every request that carries the header, so the turn
// loop's user-forced branch serves the requested model on this turn and stays on
// it for the session. Unrecognized models are ignored (routing proceeds
// automatically); the request is never failed on a bad header value.
func (s *Service) applyForceModelHeader(
	ctx context.Context,
	r *http.Request,
	env *translate.RequestEnvelope,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
) {
	if s.pinStore == nil {
		return
	}
	raw := strings.TrimSpace(r.Header.Get(ForceModelHeader))
	if raw == "" {
		return
	}
	log := observability.FromContext(ctx)
	canonicalModel, provider, known := resolveForceModel(raw)
	if !known {
		log.Info("x-weave-force-model: ignoring unrecognized model; routing automatically",
			"input_model", raw,
			"session_key_hex", fmt.Sprintf("%x", sessionKey),
		)
		return
	}
	role := roleForTier(catalog.TierFor(env.Model()))
	if err := s.setForceModelPin(ctx, sessionKey, role, installationID, canonicalModel, provider); err != nil {
		log.Error("x-weave-force-model: pin store upsert failed", "err", err)
		return
	}
	log.Info("x-weave-force-model applied",
		"input_model", raw,
		"canonical_model", canonicalModel,
		"provider", provider,
		"session_key_hex", fmt.Sprintf("%x", sessionKey),
		"role", role,
	)
}

// handleForceModelCommand processes a /force-model or /unforce-model directive.
// It writes (or expires) the session pin and returns a synthetic Anthropic-format
// acknowledgment response without dispatching to any upstream.
func (s *Service) handleForceModelCommand(
	ctx context.Context,
	w http.ResponseWriter,
	env *translate.RequestEnvelope,
	cmd translate.ForceModelResult,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
) error {
	log := observability.FromContext(ctx)
	role := roleForTier(catalog.TierFor(env.Model()))

	// Acknowledgment text is formatted as a routing marker (✦ **Weave Router** → …\n\n)
	// so the existing StripRoutingMarkerFromMessages ingress stripper removes it from
	// subsequent inbound requests. Without this, the text persists in conversation
	// history and leaks router internals to the upstream on every following turn.
	var msg string
	if cmd.Clear {
		if s.pinStore != nil && installationID != uuid.Nil {
			expired := sessionpin.Pin{
				SessionKey:     sessionKey,
				Role:           role,
				InstallationID: installationID,
				Provider:       "",
				Model:          "",
				Reason:         "user_unforced",
				TurnCount:      1,
				PinnedUntil:    time.Now().Add(-time.Second),
			}
			// context.Background(): the request ctx may be canceled by the
			// time the synthetic response has been written. Upserting on a
			// canceled context would leave Postgres holding the prior pin.
			if err := s.pinStore.Upsert(context.Background(), expired); err != nil {
				log.Error("/unforce-model: pin store upsert failed", "err", err)
				return err
			}
		}
		msg = "✦ **Weave Router** → force-model cleared · resuming automatic model selection\n\n"
		if env.SourceFormat() == translate.FormatOpenAI {
			msg = "Weave Router: force-model cleared; resuming automatic model selection"
		}
		// Debug (not Info) per router logging rules: session_key_hex is a stable
		// per-session identifier and this fires on every command use. The Info
		// signal "a session pin was cleared" isn't a major business event worth
		// emitting at default verbosity.
		log.Debug("/unforce-model: session pin cleared",
			"session_key_hex", fmt.Sprintf("%x", sessionKey),
			"role", role,
		)
	} else if canonicalModel, provider, known := resolveForceModel(cmd.Model); !known {
		// The model isn't in the catalog, so it has no known tier. A pin to an
		// unknown-tier model is rewritten by the requested-model tier ceiling on
		// every turn (clampToCeiling treats unknown tiers as above-ceiling), so
		// the user would silently be served a tier-default model instead of the
		// one they asked for — e.g. a truncated "/force-model gpt-" routing to an
		// OSS fallback. Reject the directive rather than pin something we can't
		// honor; the previous pin (if any) is left untouched.
		log.Info("/force-model: rejected unknown model",
			"input_model", cmd.Model,
			"session_key_hex", fmt.Sprintf("%x", sessionKey),
			"role", role,
		)
		msg = fmt.Sprintf("✦ **Weave Router** → force-model: %q isn't a recognized model · keeping automatic routing. Use a full model id, e.g. claude-opus-4-8, gpt-5.5, or gemini-3-pro-preview.\n\n", cmd.Model)
		if env.SourceFormat() == translate.FormatOpenAI {
			msg = fmt.Sprintf("Weave Router: force-model: %q isn't a recognized model; keeping automatic routing. Use a full model id, e.g. claude-opus-4-8, gpt-5.5, or gemini-3-pro-preview.", cmd.Model)
		}
	} else {
		if err := s.setForceModelPin(ctx, sessionKey, role, installationID, canonicalModel, provider); err != nil {
			log.Error("/force-model: pin store upsert failed", "err", err)
			return err
		}
		msg = fmt.Sprintf("✦ **Weave Router** → force-model applied: %s (%s) · use /unforce-model to clear\n\n", canonicalModel, provider)
		if env.SourceFormat() == translate.FormatOpenAI {
			msg = fmt.Sprintf("Weave Router: force-model applied: %s (%s). Use /unforce-model to clear.", canonicalModel, provider)
		}
		log.Debug("/force-model: session pin set",
			"input_model", cmd.Model,
			"canonical_model", canonicalModel,
			"provider", provider,
			"session_key_hex", fmt.Sprintf("%x", sessionKey),
			"role", role,
		)
	}

	switch env.SourceFormat() {
	case translate.FormatOpenAI:
		return writeSyntheticOpenAIResponse(w, env, msg)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg)
	}
}

// writeSyntheticAnthropicResponse writes a minimal Anthropic Messages API
// response without hitting an upstream. Handles both streaming and
// non-streaming request shapes.
func writeSyntheticAnthropicResponse(w http.ResponseWriter, env *translate.RequestEnvelope, text string) error {
	msgID := fmt.Sprintf("msg_router_cmd_%x", time.Now().UnixNano())
	if env.Stream() {
		return writeSyntheticAnthropicSSE(w, msgID, text)
	}
	return writeSyntheticAnthropicJSON(w, msgID, text)
}

func writeSyntheticAnthropicJSON(w http.ResponseWriter, msgID, text string) error {
	resp := map[string]any{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         "weave-router",
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"content": []any{
			map[string]any{"type": "text", "text": text},
		},
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": len(text) / 4,
		},
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal synthetic response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, writeErr := w.Write(body)
	return writeErr
}

func writeSyntheticAnthropicSSE(w http.ResponseWriter, msgID, text string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	bw := bufio.NewWriterSize(w, 4096)

	outTokens := len(text) / 4

	events := []string{
		sseEvent("message_start", mustMarshalJSON(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": msgID, "type": "message", "role": "assistant",
				"content": []any{}, "model": "weave-router",
				"stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})),
		sseEvent("content_block_start", mustMarshalJSON(map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})),
		sseEvent("ping", `{"type":"ping"}`),
		sseEvent("content_block_delta", mustMarshalJSON(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("message_delta", mustMarshalJSON(map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": outTokens},
		})),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}

	for _, ev := range events {
		bw.WriteString(ev)
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// writeSyntheticOpenAIResponse writes a minimal OpenAI Chat Completions
// response without hitting an upstream. Handles both streaming and
// non-streaming request shapes.
func writeSyntheticOpenAIResponse(w http.ResponseWriter, env *translate.RequestEnvelope, text string) error {
	respID := fmt.Sprintf("chatcmpl_router_cmd_%x", time.Now().UnixNano())
	if env.Stream() {
		return writeSyntheticOpenAISSE(w, respID, text)
	}
	return writeSyntheticOpenAIJSON(w, respID, text)
}

func writeSyntheticOpenAIJSON(w http.ResponseWriter, respID, text string) error {
	outTokens := len(text) / 4
	resp := map[string]any{
		"id":      respID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "weave-router",
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": outTokens,
			"total_tokens":      outTokens,
		},
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal synthetic openai response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, writeErr := w.Write(body)
	return writeErr
}

func writeSyntheticOpenAISSE(w http.ResponseWriter, respID, text string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	bw := bufio.NewWriterSize(w, 4096)
	created := time.Now().Unix()
	chunkStart := mustMarshalJSON(map[string]any{
		"id":      respID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   "weave-router",
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": nil,
			},
		},
	})
	chunkStop := mustMarshalJSON(map[string]any{
		"id":      respID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   "weave-router",
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	})
	events := []string{
		openAISSEData(chunkStart),
		openAISSEData(chunkStop),
		openAISSEData("[DONE]"),
	}
	for _, ev := range events {
		bw.WriteString(ev)
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func sseEvent(eventType, data string) string {
	return "event: " + eventType + "\ndata: " + data + "\n\n"
}

func openAISSEData(data string) string {
	return "data: " + data + "\n\n"
}

func mustMarshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
