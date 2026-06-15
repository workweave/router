package proxy

import (
	"testing"

	"workweave/router/internal/router/turntype"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolResultBytesPtr_GatedOnTurnType locks the fix for the trailing-assistant
// case: LastUserMessage() reports the last *user* message in the whole history,
// so a request ending in an assistant reply after a prior tool_result still has
// HasToolResult==true. tool_result_bytes must only be written on a turn actually
// classified as ToolResult, so it stays NULL on non-tool_result turns.
func TestToolResultBytesPtr_GatedOnTurnType(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]}]}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)
	inbound := env.LastUserMessage() // snapshot taken before any handover rewrite

	// A real tool_result turn records the incoming tool-output bytes.
	got := toolResultBytesPtr(inbound, turntype.ToolResult)
	require.NotNil(t, got, "tool_result turn must record bytes")
	assert.Equal(t, int32(5), *got) // `"out"` raw JSON = 5 bytes

	// Same snapshot, but the turn was NOT classified as a tool_result (e.g. a
	// trailing-assistant request whose last user message merely happens to
	// carry a stale tool_result). Must be NULL, not a spurious value.
	assert.Nil(t, toolResultBytesPtr(inbound, turntype.MainLoop), "non-tool_result turn must be NULL")
}
