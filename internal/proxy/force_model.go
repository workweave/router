package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router/capability"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// inferProviderForModel returns the provider name for a given model identifier
// using well-known naming conventions. Falls back to Anthropic for unrecognized
// models so the forced pin is routable; the upstream call will 400 if wrong,
// surfacing the mistyped model name to the user.
func inferProviderForModel(model string) string {
	switch {
	case strings.HasPrefix(model, "claude-"):
		return providers.ProviderAnthropic
	case strings.HasPrefix(model, "gpt-"),
		model == "o1", model == "o3", model == "o1-pro", model == "o3-pro",
		strings.HasPrefix(model, "o1-"), strings.HasPrefix(model, "o3-"), strings.HasPrefix(model, "o4-"):
		return providers.ProviderOpenAI
	case strings.HasPrefix(model, "gemini-"):
		return providers.ProviderGoogle
	case strings.Contains(model, "/"):
		// Slash-prefixed IDs (deepseek/deepseek-v4-pro, qwen/qwen3-*, etc.)
		// are OpenRouter-namespaced.
		return providers.ProviderOpenRouter
	default:
		return providers.ProviderAnthropic
	}
}

// handleForceModelCommand processes a /force-model or /unforce-model directive.
// It writes (or expires) the session pin and returns a synthetic Anthropic-format
// acknowledgment response without dispatching to any upstream.
func (s *Service) handleForceModelCommand(
	w http.ResponseWriter,
	env *translate.RequestEnvelope,
	cmd translate.ForceModelResult,
	apiKeyID string,
	installationID uuid.UUID,
) error {
	log := observability.Get()
	sessionKey := DeriveSessionKey(env, apiKeyID)
	role := roleForTier(capability.TierFor(env.Model()))
	pinCacheKey := sessionPinCacheKey(sessionKey, role)

	var msg string
	if cmd.Clear {
		if s.pinStore != nil {
			// Write an immediately-expired pin so loadPin sees a miss on the next turn.
			// The cache entry is evicted synchronously; the DB row is updated async.
			if s.pinCache != nil {
				s.pinCache.Remove(pinCacheKey)
			}
			if installationID != uuid.Nil {
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
				s.enqueuePinUpsert(expired, pinCacheKey)
			}
		}
		msg = "Routing unpinned — will resume automatic model selection."
		log.Info("/unforce-model: session pin cleared",
			"session_key_hex", fmt.Sprintf("%x", sessionKey),
			"role", role,
		)
	} else {
		provider := inferProviderForModel(cmd.Model)
		if s.pinStore != nil && installationID != uuid.Nil {
			forced := sessionpin.Pin{
				SessionKey:     sessionKey,
				Role:           role,
				InstallationID: installationID,
				Provider:       provider,
				Model:          cmd.Model,
				Reason:         translate.ReasonUserForceModel,
				TurnCount:      1,
				PinnedUntil:    time.Now().Add(pinSessionTTL),
			}
			s.enqueuePinUpsert(forced, pinCacheKey)
		}
		msg = fmt.Sprintf("Routing pinned to %s (%s). Use /unforce-model to resume automatic selection.", cmd.Model, provider)
		log.Info("/force-model: session pin set",
			"model", cmd.Model,
			"provider", provider,
			"session_key_hex", fmt.Sprintf("%x", sessionKey),
			"role", role,
		)
	}

	return writeSyntheticAnthropicResponse(w, env, msg)
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

func sseEvent(eventType, data string) string {
	return "event: " + eventType + "\ndata: " + data + "\n\n"
}

func mustMarshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
