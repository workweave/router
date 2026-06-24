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

	t.Run("terminal agents get the link-free rating hint", func(t *testing.T) {
		for _, app := range []string{ClientAppClaudeCode, ClientAppCodex, ClientAppOpencode} {
			footer := withStore.feedbackFooter(app)
			assert.Equal(t, feedbackFooterText, footer, "expected hint for %q", app)
			assert.NotContains(t, footer, "http", "footer must never embed a raw link")
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
