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
