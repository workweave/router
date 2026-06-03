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

// resolveForceModel maps a user-typed model identifier to its canonical
// catalog ID and primary provider binding. The catalog is the source of
// truth — heuristics are only a best-effort provider guess for inputs that
// are not in it.
//
// Resolution order:
//  1. Exact match against `catalog.ByID` (the input is already canonical).
//  2. Suffix match: scan the catalog for any model whose ID ends with
//     "/" + input. Lets users type bare names like `qwen3-235b-a22b-2507`
//     and have the pin route to `qwen/qwen3-235b-a22b-2507` on its real
//     binding (bedrock, in this case) instead of misclassifying it as
//     Anthropic.
//  3. Naming heuristic for IDs not in the catalog (provider guess only).
//
// The returned `known` flag is true only for catalog matches (1 and 2). A
// false `known` means the input has no catalog entry and therefore no known
// tier: the requested-model tier ceiling would rewrite a pin to it on every
// turn (clampToCeiling treats unknown tiers as above-ceiling), so the user
// would never actually be served the model. Callers must reject the command
// rather than pin an unservable directive. The heuristic provider is still
// returned for logging.
func resolveForceModel(model string) (canonicalID, provider string, known bool) {
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
	pinCacheKey := sessionPinCacheKey(sessionKey, role)

	// Acknowledgment text is formatted as a routing marker (✦ **Weave Router** → …\n\n)
	// so the existing StripRoutingMarkerFromMessages ingress stripper removes it from
	// subsequent inbound requests. Without this, the text persists in conversation
	// history and leaks router internals to the upstream on every following turn.
	// Pin writes for /force-model and /unforce-model are SYNCHRONOUS by design.
	// The async enqueuePinUpsert path drops on semaphore saturation, which here
	// would leave Postgres holding the old active forced pin while the client
	// gets a "cleared" acknowledgment — a subsequent loadPin would evict the
	// in-proc expired entry and resurrect the stale row from Postgres. These
	// are explicit user commands, not hot-path turns; an extra DB round-trip is
	// acceptable to guarantee the pin state matches the acknowledgment.
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
		// Evict the in-proc cache entry AFTER Postgres is updated so a racing
		// reader can't repopulate the LRU from a stale Postgres row.
		if s.pinCache != nil {
			s.pinCache.Remove(pinCacheKey)
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
		// Preserve LastServedModel from the existing LRU entry so the next turn
		// can still detect a model switch and strip stale Anthropic thinking-block
		// signatures. Without this carry-forward, the full Pin replacement here
		// would zero out LastServedModel, defeating the /force-model switch detection.
		var lastServedModel string
		if s.pinCache != nil {
			if existing, ok := s.pinCache.Get(pinCacheKey); ok {
				lastServedModel = existing.LastServedModel
			}
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
		if s.pinStore != nil && installationID != uuid.Nil {
			if err := s.pinStore.Upsert(context.Background(), forced); err != nil {
				log.Error("/force-model: pin store upsert failed", "err", err)
				return err
			}
		}
		if s.pinCache != nil {
			s.pinCache.Add(pinCacheKey, forced)
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
