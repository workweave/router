//go:build smoke

package smoke

import (
	"encoding/json"
	"testing"
)

// requestBuilder constructs Claude-Code-shaped Anthropic Messages request
// bodies for the scenarios. It defaults to a realistic shape: the large stable
// system prefix as a block array, a realistic tool registry, and a MainLoop-
// sized max_tokens so turn classification doesn't hard-pin the request onto a
// cheap probe path.
type requestBuilder struct {
	userID      string
	stream      bool
	maxTokens   int
	userText    string
	systemTTL   string // cache_control ttl on the last system block ("" = none)
	toolTTL     string // cache_control ttl on the last tool ("" = none)
	extraTools  int    // extra tools each carrying a cache_control breakpoint
	messageTTL  string // cache_control ttl on the final user message block ("" = none)
	noTools     bool
	customTools []map[string]any // additional raw tool definitions, appended verbatim
}

func newRequest(userID string) *requestBuilder {
	return &requestBuilder{
		userID:    userID,
		maxTokens: 4096,
		userText:  "Briefly, in one sentence, restate the first guideline in the system prompt.",
	}
}

func (b *requestBuilder) streaming() *requestBuilder    { b.stream = true; return b }
func (b *requestBuilder) tokens(n int) *requestBuilder  { b.maxTokens = n; return b }
func (b *requestBuilder) text(s string) *requestBuilder { b.userText = s; return b }

// withTool appends a raw tool definition (as built by tool()) to the request's
// tool registry, alongside the default Bash/Read/Edit set.
func (b *requestBuilder) withTool(t map[string]any) *requestBuilder {
	b.customTools = append(b.customTools, t)
	return b
}

// The following configure explicit client-side cache_control breakpoints,
// mirroring what native Claude Code clients pin.
func (b *requestBuilder) sysCache(ttl string) *requestBuilder  { b.systemTTL = ttl; return b }
func (b *requestBuilder) msgCache(ttl string) *requestBuilder  { b.messageTTL = ttl; return b }
func (b *requestBuilder) toolCache(ttl string) *requestBuilder { b.toolTTL = ttl; return b }
func (b *requestBuilder) cachedTools(n int) *requestBuilder    { b.extraTools = n; return b }

// build marshals the request body.
func (b *requestBuilder) build(t *testing.T) []byte {
	t.Helper()

	sysBlock := map[string]any{"type": "text", "text": systemPrompt}
	if b.systemTTL != "" {
		sysBlock["cache_control"] = cacheControl(b.systemTTL)
	}

	userBlock := map[string]any{"type": "text", "text": b.userText}
	if b.messageTTL != "" {
		userBlock["cache_control"] = cacheControl(b.messageTTL)
	}

	req := map[string]any{
		"model":      "claude-sonnet-4-5", // ignored by router; the force-model header pins the served model
		"max_tokens": b.maxTokens,
		"system":     []any{sysBlock},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{userBlock}},
		},
		"metadata": map[string]any{"user_id": b.userID},
	}
	if b.stream {
		req["stream"] = true
	}
	if !b.noTools {
		req["tools"] = b.buildTools()
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return out
}

// buildTools returns a realistic Claude Code tool registry plus any extra
// cache-breakpoint-carrying tools and withTool() additions. toolTTL tags the
// last built-in tool (Edit) BEFORE the extra tools are appended, so toolCache
// and cachedTools are additive and each contribute an independent breakpoint.
func (b *requestBuilder) buildTools() []any {
	tools := []any{
		tool("Bash", "Run a shell command", map[string]any{
			"command": map[string]any{"type": "string", "description": "The command to run"},
		}),
		tool("Read", "Read a file from disk", map[string]any{
			"file_path": map[string]any{"type": "string", "description": "Absolute path to the file"},
		}),
		tool("Edit", "Replace a string in a file", map[string]any{
			"file_path":  map[string]any{"type": "string"},
			"old_string": map[string]any{"type": "string"},
			"new_string": map[string]any{"type": "string"},
		}),
	}
	if b.toolTTL != "" {
		tools[len(tools)-1].(map[string]any)["cache_control"] = cacheControl(b.toolTTL)
	}
	for _, t := range b.customTools {
		tools = append(tools, t)
	}
	// Extra tools each carrying their own explicit breakpoint, to drive the
	// request up to Anthropic's 4-breakpoint capacity from the tools side.
	for i := 0; i < b.extraTools; i++ {
		tl := tool(toolName(i), "Extra cached tool", map[string]any{
			"arg": map[string]any{"type": "string"},
		})
		tl["cache_control"] = cacheControl("5m")
		tools = append(tools, tl)
	}
	return tools
}

func tool(name, desc string, props map[string]any) map[string]any {
	return map[string]any{
		"name":        name,
		"description": desc,
		"input_schema": map[string]any{
			"type":       "object",
			"properties": props,
		},
	}
}

func toolName(i int) string {
	return "ExtraTool" + string(rune('A'+i))
}

// cacheControl returns an ephemeral cache_control object; ttl "" means the
// default (Anthropic treats absent ttl as 5m).
func cacheControl(ttl string) map[string]any {
	cc := map[string]any{"type": "ephemeral"}
	if ttl != "" {
		cc["ttl"] = ttl
	}
	return cc
}

// forceModelBody builds a /force-model command turn body for a session. Sent as
// the first user message, it pins the session (synthetic response) so callers
// can verify the command surface directly.
func forceModelBody(t *testing.T, userID, model string) []byte {
	t.Helper()
	req := map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 4096,
		"messages": []any{
			map[string]any{"role": "user", "content": "/force-model " + model},
		},
		"metadata": map[string]any{"user_id": userID},
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal force-model body: %v", err)
	}
	return out
}
