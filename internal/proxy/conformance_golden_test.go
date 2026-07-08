package proxy_test

// Golden-file helpers for the translation-conformance suite. Output is
// deterministic (fixture-driven), but normalizeResponse still redacts
// volatile values (ids, timestamps) and sorts keys so a diff always
// reflects a real translation change. Run `go test -update` to regenerate.

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"workweave/router/internal/sse"

	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update", false, "rewrite conformance golden files from current output")

// goldenPath maps a case name (e.g. "openai_chat/basic_text") to its golden file.
func goldenPath(name string) string {
	return filepath.Join("testdata", "conformance", name+".golden")
}

// fixturePath maps an upstream-fixture name to its file under testdata.
func fixturePath(name string) string {
	return filepath.Join("testdata", "conformance", name)
}

// readFixture loads an upstream-response fixture (what the mock provider replays).
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err, "read upstream fixture %s", name)
	return b
}

// golden compares got against the committed golden for name. With -update it
// rewrites the golden instead (review the diff before committing).
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := goldenPath(name)
	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s — run `go test -update` to generate it", path)
	require.Equal(t, string(want), string(got),
		"golden mismatch for %s — the router's translated output changed; run `go test -update` and review the diff", name)
}

// normalizedFrame is one canonicalized SSE event (or the single body of a
// non-streaming response, with event "").
type normalizedFrame struct {
	Event string                 `json:"event,omitempty"`
	Data  map[string]interface{} `json:"data"`
}

// normalizeResponse canonicalizes an Anthropic response (SSE or one-shot JSON)
// into stable, diffable bytes: volatile values redacted, keys sorted.
func normalizeResponse(t *testing.T, contentType string, raw []byte) []byte {
	t.Helper()
	trimmed := bytes.TrimSpace(raw)
	isSSE := strings.Contains(contentType, "event-stream") ||
		bytes.Contains(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte("data:"))
	if isSSE {
		return normalizeSSE(t, raw)
	}
	return normalizeJSON(t, raw)
}

func normalizeSSE(t *testing.T, raw []byte) []byte {
	t.Helper()
	var frames []normalizedFrame
	buf := raw
	for {
		event, n := sse.SplitNext(buf)
		if n == 0 {
			break
		}
		buf = buf[n:]
		eventType, data := sse.ParseEvent(event)
		if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			continue
		}
		frames = append(frames, normalizedFrame{Event: string(eventType), Data: redactJSON(t, data)})
	}
	return marshalCanonical(t, frames)
}

func normalizeJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	return marshalCanonical(t, normalizedFrame{Data: redactJSON(t, bytes.TrimSpace(raw))})
}

// marshalCanonical renders v as indented JSON with sorted keys and HTML
// escaping off, so `<id>` etc. stay readable in the golden.
func marshalCanonical(t *testing.T, v interface{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(v))
	return buf.Bytes()
}

// redactJSON unmarshals one JSON object and replaces the translator's
// freshly-generated values (message id, timestamp) with stable placeholders;
// everything fixture-derived is left intact.
func redactJSON(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &m), "response frame is not a JSON object: %s", string(data))
	redactVolatile(m)
	return m
}

// synthToolID matches tool-call ids the translator invents (Gemini has no
// native call id: gemini_response.go uses "call_"+randomHex(4)). Upstream-
// echoed ids like "call_abc" don't match this pattern and stay in the golden.
var synthToolID = regexp.MustCompile(`^call_[0-9a-f]{8}$`)

// nonceSuffixedToolID matches an upstream-echoed tool-call id that
// uniqueToolUseIDWithNonce suffixed with a per-response 12-hex-char nonce
// (functions_Bash_0_<nonce>). The nonce is volatile per response, so redact
// only the suffix and keep the readable upstream prefix in the golden. The
// prefix is anchored to a tool-call id shape (call_/toolu_/functions_/tc_…) so
// an unrelated stable id that merely ends in _<12hex> is left untouched and its
// real changes still show in the golden diff.
var nonceSuffixedToolID = regexp.MustCompile(`^((?:call_|toolu_|tc_|functions?[._]).*)_[0-9a-f]{12}$`)

// redactVolatile walks a decoded frame, replacing message ids and synthesized
// tool-call ids with placeholders and dropping wire timestamps.
func redactVolatile(v interface{}) {
	switch x := v.(type) {
	case map[string]interface{}:
		if t, _ := x["type"].(string); t == "message" || t == "message_start" {
			if _, has := x["id"]; has {
				x["id"] = "<id>"
			}
			if msg, ok := x["message"].(map[string]interface{}); ok {
				if _, has := msg["id"]; has {
					msg["id"] = "<id>"
				}
			}
		}
		delete(x, "created")
		for k, val := range x {
			if s, ok := val.(string); ok {
				// Tool-use ids and their paired tool_use_id carry a per-response
				// nonce suffix (uniqueToolUseIDWithNonce). Strip the volatile
				// suffix first, then redact the prefix: a fully-synthetic prefix
				// (Gemini's "call_<8hex>") is itself volatile and collapses to
				// <tool_id>; an upstream-echoed prefix stays readable.
				prefix := s
				hasNonce := false
				if m := nonceSuffixedToolID.FindStringSubmatch(s); m != nil {
					prefix, hasNonce = m[1], true
				}
				if synthToolID.MatchString(prefix) {
					x[k] = "<tool_id>"
					continue
				}
				if hasNonce && (k == "id" || k == "tool_use_id") {
					x[k] = prefix + "_<nonce>"
					continue
				}
			}
			redactVolatile(val)
		}
	case []interface{}:
		for _, item := range x {
			redactVolatile(item)
		}
	}
}
