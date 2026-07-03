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
// /force-model chat command. Needed for headless clients (eval harness, CI
// smoke runs): Claude Code eats "/force-model …" as a client-side slash
// command before it reaches the router. The header rides on every request,
// so the pin is (re)written and served on the same turn. Unrecognized values
// are ignored; routing proceeds automatically rather than failing.
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
	"sonnet":         "claude-sonnet-5",
	"claude-sonnet":  "claude-sonnet-5",
	"sonnet-5":       "claude-sonnet-5",
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
	"qwen3.7-plus":   "qwen/qwen3.7-plus",
	"kimi":           "moonshotai/kimi-k2.7",
	"kimi-k2.7":      "moonshotai/kimi-k2.7",
	"kimi-k2.6":      "moonshotai/kimi-k2.6",
	// Generic glm/zai aliases stay on 5.1 (DeepInfra+OpenRouter, no Fireworks
	// key needed); 5.2 is Fireworks-only day-0, so it requires an explicit pin.
	"glm":          "z-ai/glm-5.1",
	"zai":          "z-ai/glm-5.1",
	"z-ai":         "z-ai/glm-5.1",
	"glm-5.2":      "z-ai/glm-5.2",
	"glm-5.1":      "z-ai/glm-5.1",
	"glm-5":        "z-ai/glm-5",
	"minimax":      "minimax/minimax-m3",
	"minimax-m3":   "minimax/minimax-m3",
	"minimax-m2.7": "minimax/minimax-m2.7",
	"mistral":      "mistralai/mistral-small-2603",
}

// resolveForceModel maps a user-typed model identifier to its canonical
// catalog ID and primary provider. The catalog is the source of truth;
// naming heuristics are only a best-effort provider guess for inputs not in
// it. Resolution order: alias -> exact catalog.ByID match -> suffix match
// (so bare names like `qwen3-235b-a22b-2507` resolve to their real binding,
// e.g. `qwen/qwen3-235b-a22b-2507`, instead of misclassifying as Anthropic)
// -> naming heuristic.
//
// `known` is true only for catalog matches. False means there's no catalog
// entry to confirm this is a real, servable model — callers must reject the
// command rather than pin it; the heuristic provider is still returned for
// logging.
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

// setForceModelPin upserts an immutable user-forced session pin. It preserves
// the prior pin's LastServedModel so the next turn can detect a mid-session
// model switch and strip stale Anthropic thinking-block signatures. No-op if
// the pin store is unconfigured or installationID is nil.
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
		PinnedUntil:     pinNeverExpires,
		LastServedModel: lastServedModel,
	}
	// context.Background(): ctx may already be canceled here (response written,
	// client disconnected); a canceled ctx would leave the prior pin stuck.
	return s.pinStore.Upsert(context.Background(), forced)
}

// applyForceModelHeader honors the x-weave-force-model request header,
// writing the same session pin the /force-model command writes. It's
// (re)written on every request carrying the header. Unrecognized models are
// ignored (routing proceeds automatically) rather than failing the request.
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

// handleForceModelCommand processes a /force-model or /unforce-model directive:
// writes (or expires) the session pin and returns a synthetic acknowledgment
// response without dispatching upstream. inputTokens should be the request's
// RoutingFeatures.Tokens so the token counter reflects actual turn input, not
// just the synthetic response text.
func (s *Service) handleForceModelCommand(
	ctx context.Context,
	w http.ResponseWriter,
	env *translate.RequestEnvelope,
	cmd translate.ForceModelResult,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	inputTokens int,
) error {
	log := observability.FromContext(ctx)
	role := roleForTier(catalog.TierFor(env.Model()))

	// Formatted as a routing marker (✦ **Weave Router** → …\n\n) so
	// StripRoutingMarkerFromMessages strips it from later inbound requests;
	// otherwise it'd persist in history and leak router internals upstream.
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
			// context.Background(): ctx may be canceled by the time the
			// synthetic response is written; a canceled ctx would strand the prior pin.
			if err := s.pinStore.Upsert(context.Background(), expired); err != nil {
				log.Error("/unforce-model: pin store upsert failed", "err", err)
				return err
			}
		}
		msg = "✦ **Weave Router** → force-model cleared · resuming automatic model selection\n\n"
		if env.SourceFormat() == translate.FormatOpenAI {
			msg = "Weave Router: force-model cleared; resuming automatic model selection"
		}
		// Debug not Info: fires on every command use, not a major business event.
		log.Debug("/unforce-model: session pin cleared",
			"session_key_hex", fmt.Sprintf("%x", sessionKey),
			"role", role,
		)
	} else if canonicalModel, provider, known := resolveForceModel(cmd.Model); !known {
		// Not in the catalog (e.g. truncated "/force-model gpt-") — reject
		// rather than pin something we can't honor; prior pin left untouched.
		log.Info("/force-model: rejected unknown model",
			"input_model", cmd.Model,
			"session_key_hex", fmt.Sprintf("%x", sessionKey),
			"role", role,
		)
		msg = fmt.Sprintf("✦ **Weave Router** → force-model: %q isn't a recognized model · keeping automatic routing. Use a full model ID, e.g. claude-opus-4-8, gpt-5.5, or gemini-3-pro-preview.\n\n", cmd.Model)
		if env.SourceFormat() == translate.FormatOpenAI {
			msg = fmt.Sprintf("Weave Router: force-model: %q isn't a recognized model; keeping automatic routing. Use a full model ID, e.g. claude-opus-4-8, gpt-5.5, or gemini-3-pro-preview.", cmd.Model)
		}
	} else {
		if err := s.setForceModelPin(ctx, sessionKey, role, installationID, canonicalModel, provider); err != nil {
			log.Error("/force-model: pin store upsert failed", "err", err)
			return err
		}
		msg = fmt.Sprintf("✦ **Weave Router** → force-model applied: %s (%s) · Use /unforce-model to clear\n\n", canonicalModel, provider)
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
		return writeSyntheticOpenAIResponse(w, env, msg, inputTokens)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg, inputTokens)
	}
}

// writeSyntheticAnthropicResponse writes a minimal Anthropic Messages API
// response without hitting an upstream, handling both streaming and
// non-streaming shapes.
func writeSyntheticAnthropicResponse(w http.ResponseWriter, env *translate.RequestEnvelope, text string, inputTokens int) error {
	msgID := fmt.Sprintf("msg_router_cmd_%x", time.Now().UnixNano())
	if env.Stream() {
		return writeSyntheticAnthropicSSE(w, msgID, text, inputTokens)
	}
	return writeSyntheticAnthropicJSON(w, msgID, text, inputTokens)
}

func writeSyntheticAnthropicJSON(w http.ResponseWriter, msgID, text string, inputTokens int) error {
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
			"input_tokens":  inputTokens,
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

func writeSyntheticAnthropicSSE(w http.ResponseWriter, msgID, text string, inputTokens int) error {
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
				"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
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
// response without hitting an upstream, handling both streaming and
// non-streaming shapes.
func writeSyntheticOpenAIResponse(w http.ResponseWriter, env *translate.RequestEnvelope, text string, inputTokens int) error {
	respID := fmt.Sprintf("chatcmpl_router_cmd_%x", time.Now().UnixNano())
	if env.Stream() {
		return writeSyntheticOpenAISSE(w, respID, text, inputTokens)
	}
	return writeSyntheticOpenAIJSON(w, respID, text, inputTokens)
}

func writeSyntheticOpenAIJSON(w http.ResponseWriter, respID, text string, inputTokens int) error {
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
			"prompt_tokens":     inputTokens,
			"completion_tokens": outTokens,
			"total_tokens":      inputTokens + outTokens,
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

func writeSyntheticOpenAISSE(w http.ResponseWriter, respID, text string, inputTokens int) error {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	bw := bufio.NewWriterSize(w, 4096)
	created := time.Now().Unix()
	outTokens := len(text) / 4
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
		"usage": map[string]any{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outTokens,
			"total_tokens":      inputTokens + outTokens,
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
