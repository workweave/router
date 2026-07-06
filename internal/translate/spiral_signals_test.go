package translate_test

import (
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolResultErrors_FlagsIsErrorAndCountsStreak(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "Bash", "input": map[string]any{"command": "run"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": "ok", "is_error": false},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "2", "name": "Bash", "input": map[string]any{"command": "run"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "2", "content": "boom", "is_error": true},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	stats := env.ToolResultErrors()
	assert.Equal(t, 2, stats.Total)
	assert.Equal(t, 1, stats.Errored)
	assert.Equal(t, 1, stats.TrailingErrStreak)
}

func TestToolResultErrors_DetectsMarkerTextWithoutIsErrorFlag(t *testing.T) {
	// A test suite can exit non-zero without the client setting is_error;
	// the marker-text scan is the fallback.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": "Traceback (most recent call last):\n  boom"},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	stats := env.ToolResultErrors()
	assert.Equal(t, 1, stats.Total)
	assert.Equal(t, 1, stats.Errored, "marker text must be detected even without is_error set")
	assert.Equal(t, 1, stats.TrailingErrStreak)
}

func TestToolResultErrors_MarkerTextInArrayContent(t *testing.T) {
	// tool_result content can be an array of text blocks rather than a plain
	// string; the marker scan must walk into it.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": []any{
					map[string]any{"type": "text", "text": "running tests...\nFAILED test_foo"},
				}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	stats := env.ToolResultErrors()
	assert.Equal(t, 1, stats.Errored, "marker in array-form content must be detected")
}

func TestToolResultErrors_TrailingStreakResetsOnSuccess(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": "boom", "is_error": true},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "2", "content": "boom again", "is_error": true},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "3", "content": "ok now", "is_error": false},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	stats := env.ToolResultErrors()
	assert.Equal(t, 3, stats.Total)
	assert.Equal(t, 2, stats.Errored)
	assert.Equal(t, 0, stats.TrailingErrStreak, "a healthy result at the tail must reset the streak")
}

func TestToolResultErrors_ScanLimitCapsMarkerSearch(t *testing.T) {
	// The marker text sits well past the 2048-byte scan limit; it must not
	// be found, proving the cap is enforced rather than scanning the whole body.
	padding := make([]byte, 3000)
	for i := range padding {
		padding[i] = 'a'
	}
	content := string(padding) + "Traceback (most recent call last):"
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": content},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	stats := env.ToolResultErrors()
	assert.Equal(t, 0, stats.Errored, "marker past the scan limit must not be detected")
}

func TestToolResultErrors_NonAnthropicFormatReturnsZeroStats(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "tool", "tool_call_id": "1", "content": "Traceback (most recent call last):"},
		},
	})
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	stats := env.ToolResultErrors()
	assert.Equal(t, translate.ToolResultErrorStats{}, stats)
}

func TestAssistantToolCallFilePaths_ExtractsFilePathAndNotebookPath(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "Read", "input": map[string]any{"file_path": "/foo.go"}},
				map[string]any{"type": "tool_use", "id": "2", "name": "NotebookEdit", "input": map[string]any{"notebook_path": "/nb.ipynb"}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	paths := env.AssistantToolCallFilePaths()
	require.Len(t, paths, 2)
	assert.Equal(t, translate.ToolCallFilePath{Name: "Read", Path: "/foo.go"}, paths[0])
	assert.Equal(t, translate.ToolCallFilePath{Name: "NotebookEdit", Path: "/nb.ipynb"}, paths[1])
}

func TestAssistantToolCallFilePaths_SkipsNudgeAndPathlessCalls(t *testing.T) {
	// Router-synthesized recovery nudges (id prefix toolu_router_nudge_) carry
	// command/description args, never file_path/notebook_path, so they must
	// never surface as a file touch in the same-file-thrash detector.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_router_nudge_msg1", "name": "Bash", "input": map[string]any{
					"command":     "echo nudge",
					"description": "router recovery nudge: previous turn had no tool_use",
				}},
				map[string]any{"type": "tool_use", "id": "2", "name": "Grep", "input": map[string]any{"pattern": "foo"}},
				map[string]any{"type": "tool_use", "id": "3", "name": "Read", "input": map[string]any{"file_path": "/bar.go"}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	paths := env.AssistantToolCallFilePaths()
	require.Len(t, paths, 1, "nudge and Grep (no file_path/notebook_path) must be filtered")
	assert.Equal(t, translate.ToolCallFilePath{Name: "Read", Path: "/bar.go"}, paths[0])
}

func TestAssistantToolCallFilePaths_NonAnthropicFormatReturnsNil(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{"file_path":"/foo.go"}`}},
			}},
		},
	})
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	assert.Nil(t, env.AssistantToolCallFilePaths())
}

func TestTrailingAssistantMonologue_StopsAtRealToolUse(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": "ok"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "thinking out loud"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "still thinking"},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	assert.Equal(t, 2, env.TrailingAssistantMonologue(), "two tool-less assistant turns since the last real tool_use")
}

func TestTrailingAssistantMonologue_StopsAtUserRealContent(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "user", "content": "keep going"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "prose only"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "still prose"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "more prose"},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	assert.Equal(t, 3, env.TrailingAssistantMonologue(), "three trailing tool-less assistant turns, stopping at the real user message")
}

func TestTrailingAssistantMonologue_ToolResultOnlyUserDoesNotStopStreak(t *testing.T) {
	// A user turn carrying only a tool_result (no real content) must NOT
	// count as "real input" that stops the monologue streak.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "1", "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "1", "content": "ok"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "prose only"},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	assert.Equal(t, 1, env.TrailingAssistantMonologue())
}

func TestTrailingAssistantMonologue_NudgeCountsAsRealToolUse(t *testing.T) {
	// A router-synthesized nudge tool_use still counts as tool activity, per
	// assistantHasRealToolUse's contract, so it must stop the streak too.
	body := mustMarshalJSON(t, map[string]any{
		"model": "claude-sonnet-4-6",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "prose before nudge"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_router_nudge_1", "name": "Bash", "input": map[string]any{
					"command":     "echo nudge",
					"description": "router recovery nudge: previous turn had no tool_use",
				}},
			}},
		},
		"max_tokens": 256,
	})
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	assert.Equal(t, 0, env.TrailingAssistantMonologue(), "the trailing message is a nudge tool_use, which stops the streak at 0")
}

func TestTrailingAssistantMonologue_NonAnthropicFormatReturnsZero(t *testing.T) {
	body := mustMarshalJSON(t, map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "prose only"},
		},
	})
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	assert.Equal(t, 0, env.TrailingAssistantMonologue())
}
