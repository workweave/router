package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/tidwall/gjson"
)

// ToolCallSig identifies a single tool invocation by callee + canonical-JSON
// hash of its arguments. Two invocations with the same Name and InputHash are
// treated as identical for loop-detection purposes.
type ToolCallSig struct {
	Name      string
	InputHash string
}

// AssistantToolCallSignatures returns the ordered list of tool invocations
// emitted by assistant messages in the request body. Order matches message
// order, and within a message, content-block order.
//
// Used by the proxy's loop detector to identify runaway tool-call cycles. An
// OSS model (notably qwen3 variants) that fails to recognize when a task is
// complete will alternate or repeat the same tool calls indefinitely; counting
// signature occurrences in a recent window catches both patterns.
//
// Returns nil for Gemini-format requests (not currently supported) and for
// requests with no assistant tool calls.
func (e *RequestEnvelope) AssistantToolCallSignatures() []ToolCallSig {
	switch e.format {
	case FormatAnthropic:
		return anthropicAssistantToolCallSigs(e.body)
	case FormatOpenAI:
		return openAIAssistantToolCallSigs(e.body)
	default:
		return nil
	}
}

func anthropicAssistantToolCallSigs(body []byte) []ToolCallSig {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return nil
	}
	var sigs []ToolCallSig
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "assistant" {
			return true
		}
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "tool_use" {
				return true
			}
			name := block.Get("name").String()
			input := block.Get("input").Raw
			sigs = append(sigs, ToolCallSig{Name: name, InputHash: hashCanonicalJSON(input)})
			return true
		})
		return true
	})
	return sigs
}

func openAIAssistantToolCallSigs(body []byte) []ToolCallSig {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return nil
	}
	var sigs []ToolCallSig
	msgs.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "assistant" {
			return true
		}
		toolCalls := msg.Get("tool_calls")
		if !toolCalls.IsArray() {
			return true
		}
		toolCalls.ForEach(func(_, tc gjson.Result) bool {
			if tc.Get("type").String() != "function" {
				return true
			}
			name := tc.Get("function.name").String()
			// OpenAI delivers arguments as a JSON-encoded string. Hash the
			// canonical form so semantically-identical args with different
			// whitespace still collide.
			argsRaw := tc.Get("function.arguments").String()
			sigs = append(sigs, ToolCallSig{Name: name, InputHash: hashCanonicalJSON(argsRaw)})
			return true
		})
		return true
	})
	return sigs
}

// hashCanonicalJSON returns a stable hex sha256 of the canonical form of a
// JSON document. Whitespace and key order are normalized via gjson's parsed
// representation so equivalent JSON values produce identical hashes. Invalid
// JSON is hashed verbatim — same-string inputs still collide, which is the
// property the loop detector relies on.
func hashCanonicalJSON(raw string) string {
	if raw == "" {
		h := sha256.Sum256(nil)
		return hex.EncodeToString(h[:])
	}
	parsed := gjson.Parse(raw)
	if !parsed.Exists() {
		h := sha256.Sum256([]byte(raw))
		return hex.EncodeToString(h[:])
	}
	canonical := canonicalize(parsed)
	h := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(h[:])
}

// canonicalize serializes a gjson.Result with sorted object keys.
func canonicalize(r gjson.Result) string {
	var b strings.Builder
	canonicalizeInto(&b, r)
	return b.String()
}

func canonicalizeInto(b *strings.Builder, r gjson.Result) {
	switch {
	case r.IsObject():
		keys := make([]string, 0, 8)
		fields := make(map[string]gjson.Result, 8)
		r.ForEach(func(k, v gjson.Result) bool {
			ks := k.String()
			keys = append(keys, ks)
			fields[ks] = v
			return true
		})
		sortStrings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonQuote(k))
			b.WriteByte(':')
			canonicalizeInto(b, fields[k])
		}
		b.WriteByte('}')
	case r.IsArray():
		b.WriteByte('[')
		first := true
		r.ForEach(func(_, v gjson.Result) bool {
			if !first {
				b.WriteByte(',')
			}
			first = false
			canonicalizeInto(b, v)
			return true
		})
		b.WriteByte(']')
	default:
		b.WriteString(r.Raw)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func jsonQuote(s string) string {
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if r < 0x20 {
				out = append(out, '\\', 'u', '0', '0',
					hexDigit(byte(r>>4)), hexDigit(byte(r&0xf)))
			} else {
				out = append(out, []byte(string(r))...)
			}
		}
	}
	out = append(out, '"')
	return string(out)
}

func hexDigit(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + n - 10
}
