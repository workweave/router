package translate_test

import (
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestEnvelope_ClientSessionID_Anthropic(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "claude code bundle yields bare session uuid",
			body: `{"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"user_abcd1234-5678-90ab-cdef-1234567890ab_account__session_4dbee464-ebf7-437f-9f20-db5a6f7fe3b4"}}`,
			want: "4dbee464-ebf7-437f-9f20-db5a6f7fe3b4",
		},
		{
			name: "bare uuid in metadata.user_id",
			body: `{"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"31ab679a-f79b-4860-881e-a184dc3d2e2d"}}`,
			want: "31ab679a-f79b-4860-881e-a184dc3d2e2d",
		},
		{
			name: "non-uuid free-form string truncated to 64",
			body: `{"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"some-very-long-free-form-identifier-that-keeps-going-past-the-sixty-four-character-limit-and-then-some"}}`,
			want: "some-very-long-free-form-identifier-that-keeps-going-past-the-si",
		},
		{
			name: "missing metadata returns empty",
			body: `{"messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
		{
			name: "empty user_id returns empty",
			body: `{"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":""}}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseAnthropic([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.want, env.ClientSessionID())
		})
	}
}

func TestRequestEnvelope_ClientSessionID_OpenAI(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "user field with bare uuid (codex / opencode shape)",
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"user":"4dbee464-ebf7-437f-9f20-db5a6f7fe3b4"}`,
			want: "4dbee464-ebf7-437f-9f20-db5a6f7fe3b4",
		},
		{
			name: "user field with session marker",
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"user":"codex_session_31ab679a-f79b-4860-881e-a184dc3d2e2d"}`,
			want: "31ab679a-f79b-4860-881e-a184dc3d2e2d",
		},
		{
			name: "falls back to metadata.user_id when user is empty",
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"50a14434-357e-4b84-9d59-39cc6ee6eb1d"}}`,
			want: "50a14434-357e-4b84-9d59-39cc6ee6eb1d",
		},
		{
			name: "no identifier at all",
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseOpenAI([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.want, env.ClientSessionID())
		})
	}
}
