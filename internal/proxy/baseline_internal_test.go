package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBaselineFor(t *testing.T) {
	t.Run("known model returns itself", func(t *testing.T) {
		s := &Service{defaultBaselineModel: "claude-sonnet-4-5"}
		assert.Equal(t, "claude-opus-4-7", s.baselineFor("claude-opus-4-7"))
	})

	t.Run("unknown model returns baseline", func(t *testing.T) {
		s := &Service{defaultBaselineModel: "claude-sonnet-4-5"}
		assert.Equal(t, "claude-sonnet-4-5", s.baselineFor("weave-router"))
	})

	t.Run("empty model returns baseline", func(t *testing.T) {
		s := &Service{defaultBaselineModel: "claude-sonnet-4-5"}
		assert.Equal(t, "claude-sonnet-4-5", s.baselineFor(""))
	})

	t.Run("unknown model with no baseline returns empty", func(t *testing.T) {
		s := &Service{}
		assert.Equal(t, "", s.baselineFor("weave-router"))
	})
}

func TestWithDefaultBaselineModel(t *testing.T) {
	s := &Service{}
	s.WithDefaultBaselineModel("claude-sonnet-4-5")
	assert.Equal(t, "claude-sonnet-4-5", s.defaultBaselineModel)
}
