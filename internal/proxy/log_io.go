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
