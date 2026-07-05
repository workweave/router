package proxy

import (
	"log/slog"
	"strings"

	"workweave/router/internal/translate"
)

// preview returns a log-safe excerpt of s, truncated to n bytes with an
// ellipsis suffix. Full bodies stay behind LOG_LEVEL=debug.
func preview(s string, n int) string {
	if s == "" || n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// Trim on a rune boundary so we don't slice a multi-byte codepoint.
	cut := s[:n]
	if i := strings.LastIndexAny(cut, " \n\t"); i > n/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// logInboundToolTraffic logs names + short arg previews of the last few
// assistant tool_use calls, to correlate a broken turn without dumping the
// full body. Gemini envelopes are skipped — preview only knows Anthropic +
// OpenAI shapes today.
func logInboundToolTraffic(log *slog.Logger, env *translate.RequestEnvelope) {
	if env == nil {
		return
	}
	const tailWindow = 5
	sigs := env.AssistantToolCallSignatures()
	if len(sigs) == 0 {
		return
	}
	offset := len(sigs) - tailWindow
	if offset < 0 {
		offset = 0
	}
	args := env.AssistantToolCallArgsPreview(offset, 160)
	names := make([]string, 0, len(sigs))
	for _, s := range sigs {
		names = append(names, s.Name)
	}
	log.Debug("inbound tool_use history",
		"total_tool_calls", len(sigs),
		"tool_names", names,
		"tail_window_args", args,
	)
}

func logInboundRequestDiagnostics(log *slog.Logger, env *translate.RequestEnvelope) {
	logInboundToolTraffic(log, env)
	logInboundConversationTail(log, env)
	logInboundSystemTail(log, env)
}

// logInboundConversationTail logs role + per-block type/name/preview for the
// last few inbound messages — the visible prose context the model is about
// to react to, complementing logInboundToolTraffic's view of prior tool calls.
func logInboundConversationTail(log *slog.Logger, env *translate.RequestEnvelope) {
	if env == nil {
		return
	}
	const tailWindow = 4
	const maxLen = 500
	msgs := env.MessageTailPreview(tailWindow, maxLen)
	if len(msgs) == 0 {
		return
	}
	lines := make([]string, 0, len(msgs))
	for _, m := range msgs {
		var b strings.Builder
		b.WriteString(m.Role)
		b.WriteString(" :: ")
		for i, blk := range m.Blocks {
			if i > 0 {
				b.WriteString(" || ")
			}
			b.WriteString(blk.Type)
			if blk.Name != "" {
				b.WriteByte('[')
				b.WriteString(blk.Name)
				b.WriteByte(']')
			}
			if blk.Preview != "" {
				b.WriteString(": ")
				b.WriteString(strings.ReplaceAll(blk.Preview, "\n", " "))
			}
		}
		lines = append(lines, b.String())
	}
	log.Info("inbound conversation tail",
		"window", tailWindow,
		"max_len", maxLen,
		"messages", lines,
	)
}

// logInboundSystemTail logs system-prompt length plus head/tail excerpts,
// surfacing per-turn system_reminder injections without logging the whole
// prompt (which the static Claude Code prompt otherwise dwarfs).
func logInboundSystemTail(log *slog.Logger, env *translate.RequestEnvelope) {
	if env == nil {
		return
	}
	const maxLen = 500
	length, head, tail := env.SystemTextTail(maxLen)
	if length == 0 {
		return
	}
	log.Info("inbound system tail",
		"system_len", length,
		"system_head", head,
		"system_tail", tail,
	)
}

// logAssistantOutputSummary logs counts of text/tool_use/thinking blocks and
// tool_use previews for the assistant's turn. Streaming providers call it
// once the stream closes; non-streaming callers pass the parsed body directly.
func logAssistantOutputSummary(
	log *slog.Logger,
	textBlocks, thinkingBlocks int,
	toolCalls []ToolCallPreview,
	stopReason string,
	outputTokens int,
) {
	names := make([]string, 0, len(toolCalls))
	args := make([]string, 0, len(toolCalls))
	for _, tc := range toolCalls {
		names = append(names, tc.Name)
		args = append(args, preview(tc.ArgsJSON, 160))
	}
	log.Debug("assistant output summary",
		"text_blocks", textBlocks,
		"thinking_blocks", thinkingBlocks,
		"tool_use_count", len(toolCalls),
		"tool_names", names,
		"tool_args_preview", args,
		"stop_reason", stopReason,
		"output_tokens", outputTokens,
	)
}

// ToolCallPreview is a log-friendly snapshot of one assistant tool_use block,
// kept here to avoid coupling translate/providers to a logging type.
type ToolCallPreview struct {
	Name     string
	ArgsJSON string
}
