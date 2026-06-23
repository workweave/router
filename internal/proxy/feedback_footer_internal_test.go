package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

type footerFakeStore struct{}

func (footerFakeStore) InsertRouterFeedback(context.Context, RouterFeedbackEvent) error { return nil }

func TestFeedbackFooter_ClientGating(t *testing.T) {
	withStore := (&Service{}).WithRouterFeedbackStore(footerFakeStore{})

	t.Run("terminal agents get the rating hint", func(t *testing.T) {
		for _, app := range []string{ClientAppClaudeCode, ClientAppCodex, ClientAppOpencode} {
			assert.Equal(t, feedbackFooterText, withStore.feedbackFooter(app), "expected hint for %q", app)
		}
	})

	t.Run("ide and unknown clients are suppressed", func(t *testing.T) {
		for _, app := range []string{ClientAppCursor, ClientAppGeminiCLI, "", "some-bot"} {
			assert.Empty(t, withStore.feedbackFooter(app), "expected no footer for %q", app)
		}
	})

	t.Run("no durable store suppresses the hint entirely", func(t *testing.T) {
		assert.Empty(t, (&Service{}).feedbackFooter(ClientAppClaudeCode), "advertising a command we cannot record is misleading")
	})
}
