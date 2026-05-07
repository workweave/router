package gemini

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitModelAction(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantModel  string
		wantStream bool
		wantOK     bool
	}{
		{
			name:      "generate content",
			in:        "gemini-1.5-pro:generateContent",
			wantModel: "gemini-1.5-pro",
			wantOK:    true,
		},
		{
			name:       "stream generate content",
			in:         "gemini-1.5-pro:streamGenerateContent",
			wantModel:  "gemini-1.5-pro",
			wantStream: true,
			wantOK:     true,
		},
		{
			name:      "model with version segments",
			in:        "gemini-2.5-flash-lite:generateContent",
			wantModel: "gemini-2.5-flash-lite",
			wantOK:    true,
		},
		{
			name: "no action suffix",
			in:   "gemini-1.5-pro",
		},
		{
			name: "unknown action",
			in:   "gemini-1.5-pro:countTokens",
		},
		{
			name: "missing model name",
			in:   ":generateContent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, stream, ok := splitModelAction(tc.in)
			assert.Equal(t, tc.wantOK, ok)
			if !ok {
				return
			}
			assert.Equal(t, tc.wantModel, model)
			assert.Equal(t, tc.wantStream, stream)
		})
	}
}

func TestInjectModelAndStream(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	out, err := injectModelAndStream(body, "gemini-1.5-pro", true)
	assert.NoError(t, err)
	assert.Contains(t, string(out), `"model":"gemini-1.5-pro"`)
	assert.Contains(t, string(out), `"stream":true`)
}
