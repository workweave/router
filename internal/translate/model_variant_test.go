package translate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"workweave/router/internal/translate"
)

func TestCanonicalModel(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		canonical string
		hadTag    bool
	}{
		{"opus 1m variant", "claude-opus-4-8[1m]", "claude-opus-4-8", true},
		{"sonnet 1m variant", "claude-sonnet-4-6[1m]", "claude-sonnet-4-6", true},
		{"fable 1m variant", "claude-fable-5[1m]", "claude-fable-5", true},
		{"no tag", "claude-opus-4-8", "claude-opus-4-8", false},
		{"oss model untouched", "deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-pro", false},
		{"empty", "", "", false},
		{"tag only at end", "claude[1m]-opus", "claude[1m]-opus", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, had := translate.CanonicalModel(tc.model)
			assert.Equal(t, tc.canonical, got)
			assert.Equal(t, tc.hadTag, had)
		})
	}
}

func TestCanonicalizeModelInBody(t *testing.T) {
	t.Run("rewrites tagged model and preserves other fields", func(t *testing.T) {
		body := []byte(`{"model":"claude-opus-4-8[1m]","max_tokens":1024,"stream":true}`)
		out, had, err := translate.CanonicalizeModelInBody(body)
		require.NoError(t, err)
		assert.True(t, had)
		assert.Equal(t, "claude-opus-4-8", gjson.GetBytes(out, "model").String())
		assert.Equal(t, int64(1024), gjson.GetBytes(out, "max_tokens").Int())
		assert.True(t, gjson.GetBytes(out, "stream").Bool())
	})

	t.Run("untagged body returned unchanged", func(t *testing.T) {
		body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024}`)
		out, had, err := translate.CanonicalizeModelInBody(body)
		require.NoError(t, err)
		assert.False(t, had)
		assert.Equal(t, body, out, "untagged body must be returned byte-identical")
	})

	t.Run("missing model field returned unchanged", func(t *testing.T) {
		body := []byte(`{"max_tokens":1024}`)
		out, had, err := translate.CanonicalizeModelInBody(body)
		require.NoError(t, err)
		assert.False(t, had)
		assert.Equal(t, body, out)
	})
}
