package proxy_test

// Golden-file helpers for the translation-conformance suite. The router writes
// translated responses with a fixed key order (hand-written JSON buffers, not
// Go maps), and every input here is fixture-driven, so output is deterministic.
// normalizeResponse re-canonicalizes anyway — it parses the response, redacts
// the handful of volatile values (generated message ids, timestamps), and
// re-serializes with sorted keys — so a golden never flakes on key order and a
// diff pinpoints a real translation change. Run `go test -update` to (re)write
// goldens, then review the diff before committing.

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

// normalizeResponse canonicalizes a client-facing Anthropic response (SSE stream
// or one-shot JSON) into stable, diffable bytes: volatile values redacted, JSON
// keys sorted. SSE vs JSON is detected from the recorded body.
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

// marshalCanonical renders v as indented JSON with sorted map keys (encoding/json
// sorts map keys) and HTML escaping off, so goldens stay readable (`<id>`, not
// the <id> escape) and any < > & in model text isn't mangled.
func marshalCanonical(t *testing.T, v interface{}) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(v))
	return buf.Bytes()
}

// redactJSON unmarshals one JSON object and replaces values that the translator
// generates fresh (so they vary even with a fixed fixture) with stable
// placeholders: a message's generated id and any wire timestamp. Everything that
// flows deterministically from the fixture (text, tool args, tool_use ids that
// echo the upstream call id, usage, stop_reason) is left intact so the golden
// asserts real translation behavior.
func redactJSON(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &m), "response frame is not a JSON object: %s", string(data))
	redactVolatile(m)
	return m
}

// synthToolID matches a tool-call id the translator generates from scratch
// (Gemini has no native call id: gemini_response.go uses "call_"+randomHex(4) =
// 8 hex chars). Upstream-echoed ids in fixtures ("call_abc", "call_x") don't
// match, so they stay in the golden and assert the echo behavior; only the
// random ones get neutralized.
var synthToolID = regexp.MustCompile(`^call_[0-9a-f]{8}$`)

// redactVolatile walks a decoded response frame and replaces the values the
// translator generates fresh — a message's id and any synthesized tool-call id —
// with stable placeholders, and drops wire timestamps. Everything fixture-driven
// stays intact so the golden still asserts real translation behavior.
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
			if s, ok := val.(string); ok && synthToolID.MatchString(s) {
				x[k] = "<tool_id>"
				continue
			}
			redactVolatile(val)
		}
	case []interface{}:
		for _, item := range x {
			redactVolatile(item)
		}
	}
}
