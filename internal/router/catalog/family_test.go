package catalog

import "testing"

func TestFamilyAndVersion(t *testing.T) {
	tests := []struct {
		id      string
		family  string
		version [2]int
		ok      bool
	}{
		{"claude-sonnet-4-5", "claude-sonnet", [2]int{4, 5}, true},
		{"claude-sonnet-4-6", "claude-sonnet", [2]int{4, 6}, true},
		{"claude-sonnet-5", "claude-sonnet", [2]int{5, 0}, true},
		{"claude-opus-4-6", "claude-opus", [2]int{4, 6}, true},
		{"claude-opus-4-7", "claude-opus", [2]int{4, 7}, true},
		{"claude-opus-4-8", "claude-opus", [2]int{4, 8}, true},
		{"claude-haiku-4-5", "claude-haiku", [2]int{4, 5}, true},
		{"claude-fable-5", "claude-fable", [2]int{5, 0}, true},
		{"gpt-4.1", "gpt", [2]int{4, 1}, true},
		{"gpt-4.1-mini", "gpt-mini", [2]int{4, 1}, true},
		{"gpt-4.1-nano", "gpt-nano", [2]int{4, 1}, true},
		{"gpt-5", "gpt", [2]int{5, 0}, true},
		{"gpt-5-mini", "gpt-mini", [2]int{5, 0}, true},
		{"gpt-5-nano", "gpt-nano", [2]int{5, 0}, true},
		{"gpt-5-chat", "gpt-chat", [2]int{5, 0}, true},
		{"gpt-5.4", "gpt", [2]int{5, 4}, true},
		{"gpt-5.4-mini", "gpt-mini", [2]int{5, 4}, true},
		{"gpt-5.4-pro", "gpt-pro", [2]int{5, 4}, true},
		{"gpt-5.5", "gpt", [2]int{5, 5}, true},
		{"gpt-5.5-mini", "gpt-mini", [2]int{5, 5}, true},
		{"gpt-5.5-pro", "gpt-pro", [2]int{5, 5}, true},
		{"gpt-4o", "", [2]int{}, false},
		{"gpt-4o-mini", "", [2]int{}, false},
		{"gemini-2.0-flash", "gemini-flash", [2]int{2, 0}, true},
		{"gemini-2.5-flash", "gemini-flash", [2]int{2, 5}, true},
		{"gemini-3.5-flash", "gemini-flash", [2]int{3, 5}, true},
		{"gemini-3-pro-preview", "gemini-pro-preview", [2]int{3, 0}, true},
		{"gemini-3.1-pro-preview", "gemini-pro-preview", [2]int{3, 1}, true},
		{"z-ai/glm-5", "z-ai/glm", [2]int{5, 0}, true},
		{"z-ai/glm-5.1", "z-ai/glm", [2]int{5, 1}, true},
		{"z-ai/glm-5.2", "z-ai/glm", [2]int{5, 2}, true},
		{"moonshotai/kimi-k2.5", "moonshotai/kimi-k", [2]int{2, 5}, true},
		{"moonshotai/kimi-k2.6", "moonshotai/kimi-k", [2]int{2, 6}, true},
		{"moonshotai/kimi-k2.7", "moonshotai/kimi-k", [2]int{2, 7}, true},
		{"minimax/minimax-m2.7", "minimax/minimax-m", [2]int{2, 7}, true},
		{"minimax/minimax-m3", "minimax/minimax-m", [2]int{3, 0}, true},
		{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v-pro", [2]int{4, 0}, true},
		{"deepseek/deepseek-v4-flash", "deepseek/deepseek-v-flash", [2]int{4, 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			family, version, ok := FamilyAndVersion(tt.id)
			if ok != tt.ok {
				t.Fatalf("FamilyAndVersion(%q) ok = %v, want %v", tt.id, ok, tt.ok)
			}
			if !ok {
				return
			}
			if family != tt.family {
				t.Errorf("FamilyAndVersion(%q) family = %q, want %q", tt.id, family, tt.family)
			}
			if version != tt.version {
				t.Errorf("FamilyAndVersion(%q) version = %v, want %v", tt.id, version, tt.version)
			}
		})
	}
}

func TestFamilyDuplicates(t *testing.T) {
	ids := []string{
		"claude-haiku-4-5",
		"claude-sonnet-4-6",
		"claude-sonnet-5",
		"claude-opus-4-7",
		"claude-opus-4-8",
		"moonshotai/kimi-k2.6",
		"moonshotai/kimi-k2.7",
		"gpt-5.5",
	}
	dups := FamilyDuplicates(ids)
	got := make(map[string]string, len(dups))
	for _, d := range dups {
		got[d.Superseded] = d.SupersededBy
	}
	want := map[string]string{
		"claude-sonnet-4-6":    "claude-sonnet-5",
		"claude-opus-4-7":      "claude-opus-4-8",
		"moonshotai/kimi-k2.6": "moonshotai/kimi-k2.7",
	}
	if len(got) != len(want) {
		t.Fatalf("FamilyDuplicates(%v) = %v, want %v", ids, dups, want)
	}
	for supersededID, wantBy := range want {
		gotBy, ok := got[supersededID]
		if !ok {
			t.Errorf("expected %q to be flagged as superseded, was not", supersededID)
			continue
		}
		if gotBy != wantBy {
			t.Errorf("FamilyDuplicates: %q superseded by %q, want %q", supersededID, gotBy, wantBy)
		}
	}
}

func TestFamilyDuplicates_NoFalsePositiveOnDistinctSizes(t *testing.T) {
	ids := []string{
		"deepseek/deepseek-v4-flash",
		"deepseek/deepseek-v4-pro",
		"gpt-5.5-mini",
	}
	if dups := FamilyDuplicates(ids); len(dups) != 0 {
		t.Errorf("FamilyDuplicates(%v) = %v, want no duplicates", ids, dups)
	}
}
