package proxy

import "testing"

// forcedReasoningEffort encodes the escalate-on-failure policy: gpt-5.x low by
// default / high on a failed prior turn; gemini-3.x pinned low; everything else
// untouched ("").
func TestForcedReasoningEffort(t *testing.T) {
	cases := []struct {
		model    string
		escalate bool
		want     string
	}{
		{"gpt-5.5", false, "low"},
		{"gpt-5.5", true, "high"},
		{"gpt-5.4-mini", false, "low"},
		{"gpt-5.4-mini", true, "high"},
		{"gemini-3.1-pro-preview", false, "low"},
		{"gemini-3.1-pro-preview", true, "low"}, // effort-immune: escalation ignored
		{"claude-opus-4-8", false, ""},          // adaptive path untouched
		{"claude-opus-4-8", true, ""},
		{"deepseek/deepseek-v4-pro", true, ""},
		{"gemini-2.5-flash", true, ""}, // only gemini-3.x is pinned
	}
	for _, tc := range cases {
		got := forcedReasoningEffort(tc.model, tc.escalate)
		if got != tc.want {
			t.Errorf("forcedReasoningEffort(%q, %v) = %q, want %q", tc.model, tc.escalate, got, tc.want)
		}
	}
}
