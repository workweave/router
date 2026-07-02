package version_test

import (
	"testing"

	"workweave/router/internal/version"

	"github.com/stretchr/testify/assert"
)

func TestShortCommit(t *testing.T) {
	orig := version.Commit
	t.Cleanup(func() { version.Commit = orig })

	tests := map[string]struct {
		commit string
		want   string
	}{
		"full sha truncates to 7":   {commit: "0fb46ee9707c8db7d0ef69b7308a79a95d559e25", want: "0fb46ee"},
		"unknown default unchanged": {commit: "unknown", want: "unknown"},
		"exactly 7 unchanged":       {commit: "abcdef0", want: "abcdef0"},
		"shorter than 7 unchanged":  {commit: "abc", want: "abc"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			version.Commit = tc.commit
			assert.Equal(t, tc.want, version.ShortCommit())
		})
	}
}

func TestDisplay(t *testing.T) {
	origCommit, origPR := version.Commit, version.PR
	t.Cleanup(func() { version.Commit, version.PR = origCommit, origPR })
	version.Commit = "886d9df40857f1fa0d0d0407100dd30127fb6c11"

	tests := map[string]struct {
		pr   string
		want string
	}{
		"known PR shows number + commit":  {pr: "572", want: "#572 (886d9df)"},
		"unknown PR falls back to commit": {pr: "unknown", want: "886d9df"},
		"empty PR falls back to commit":   {pr: "", want: "886d9df"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			version.PR = tc.pr
			assert.Equal(t, tc.want, version.Display())
		})
	}
}
