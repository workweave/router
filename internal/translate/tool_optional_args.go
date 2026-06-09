package translate

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// parseToolRequiredParams parses Anthropic tool definitions into a per-tool set
// of REQUIRED parameter names (from each tool's `input_schema.required`). The
// set tells stripEmptyOptionalArgs which empty-string args are safe to drop:
// any param NOT in this set is optional. Returns nil when the body carries no
// tools.
func parseToolRequiredParams(anthropicBody []byte) map[string]map[string]struct{} {
	tools := gjson.GetBytes(anthropicBody, "tools")
	if !tools.IsArray() {
		return nil
	}
	out := make(map[string]map[string]struct{})
	tools.ForEach(func(_, tool gjson.Result) bool {
		name := tool.Get("name").String()
		if name == "" {
			return true
		}
		reqSet := make(map[string]struct{})
		tool.Get("input_schema.required").ForEach(func(_, r gjson.Result) bool {
			reqSet[r.String()] = struct{}{}
			return true
		})
		out[name] = reqSet
		return true
	})
	return out
}

// stripEmptyOptionalArgs removes object keys whose value is an empty string and
// that are not in the tool's required-parameter set.
//
// Reasoning models on the OpenAI Responses API (gpt-5.x) emit optional string
// params as "" rather than omitting them. For a param like Read.pages, the
// empty string fails the client's tool validation ("Invalid pages parameter")
// and the model — which does not self-correct off the error feedback even with
// its prior reasoning in context — re-issues the byte-identical call until a
// loop-breaker fires. Dropping the empty optional arg before it reaches the
// client turns the otherwise-doomed call into a valid one. Required params are
// never stripped: a genuinely-missing required arg must still surface its error.
func stripEmptyOptionalArgs(args string, required map[string]struct{}) string {
	parsed := gjson.Parse(args)
	if !parsed.IsObject() {
		return args
	}
	out := args
	parsed.ForEach(func(key, val gjson.Result) bool {
		if val.Type != gjson.String || val.Str != "" {
			return true
		}
		if _, req := required[key.String()]; req {
			return true
		}
		if next, err := sjson.Delete(out, key.String()); err == nil {
			out = next
		}
		return true
	})
	return out
}
