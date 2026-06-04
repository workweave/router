package proxy

import (
	"log/slog"
	"strings"

	"workweave/router/internal/translate"
)

// preview returns the first n runes of s with an ellipsis suffix when
// truncated. Empty in, empty out. Used everywhere we want a log-safe excerpt
// of free-text payloads — full bodies stay behind LOG_LEVEL=debug.
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

// logInboundToolTraffic emits a Debug log line summarising the tool-use blocks
// the model is about to see on this turn. Keeps payload small (just names +
// short arg previews of the last few assistant calls) so we can correlate a
// broken turn back to the prior tool_use / tool_result shape without dumping
// the entire body.
//
// Gemini envelopes are skipped — the underlying preview helper only knows
// Anthropic + OpenAI shapes today.
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
	log.Info("inbound tool_use history",
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

// logInboundConversationTail emits a structured summary of the last few
// inbound messages, with role + per-block type/name/preview. Pairs with
// logInboundToolTraffic: the tool-call view shows what the model previously
// invoked, this view shows the visible prose context (assistant text, last
// user prompt, last tool_result) the model is about to react to. Used to
// diagnose stuck conversations where bytes change but the model keeps
// emitting the same prefix.
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

// logInboundSystemTail emits the system-prompt length plus head/tail
// excerpts. The static Claude Code system prompt dwarfs any per-turn
// system_reminder injections; logging head + tail makes those injections
// visible without paying to log the whole prompt every turn.
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

// logAssistantOutputSummary emits a Debug summary of the assistant blocks the
// upstream produced this turn: counts of text / tool_use / thinking blocks and
// short previews of each tool_use call. Streaming providers should call this
// once the response stream has closed and the buffered text/tool blocks are
// known; for non-streaming responses it can be invoked directly on the parsed
// body.
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
	log.Info("assistant output summary",
		"text_blocks", textBlocks,
		"thinking_blocks", thinkingBlocks,
		"tool_use_count", len(toolCalls),
		"tool_names", names,
		"tool_args_preview", args,
		"stop_reason", stopReason,
		"output_tokens", outputTokens,
	)
}

// ToolCallPreview is a log-friendly snapshot of one assistant tool_use block.
// Kept in this package to avoid coupling translate or providers to a logging
// type; populated by the streaming/non-streaming response parsers when they
// can cheaply observe the block.
type ToolCallPreview struct {
	Name     string
	ArgsJSON string
}
