package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripRouterFeedbackArtifacts_StripsSequenceNotFoundAck(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "first request"},
			map[string]any{"role": "assistant", "content": "first response"},
			map[string]any{"role": "user", "content": "/rf -9 - never happened"},
			map[string]any{"role": "assistant", "content": "✦ **Weave Router** → No turn found at that sequence number. Try `/rf` without a number for the last turn.\n\n"},
			map[string]any{"role": "user", "content": "/rf + all good now"},
		},
		"max_tokens": 1024,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	removed := env.StripRouterFeedbackArtifacts()
	assert.Equal(t, 2, removed, "both the prior command and its sequence-not-found ack must be stripped")

	raw, _ := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-sonnet-4-6"})
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw.Body, &got))
	msgs, _ := got["messages"].([]any)
	require.Len(t, msgs, 3, "only the real turns plus the current command survive")
}
