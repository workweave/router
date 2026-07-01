package proxy

import (
	"context"
	"testing"

	"workweave/router/internal/router/turntype"

	"github.com/stretchr/testify/assert"
)

type footerFakeStore struct{}

func (footerFakeStore) InsertRouterFeedback(context.Context, RouterFeedbackEvent) error { return nil }

func TestFeedbackFooter_ClientGating(t *testing.T) {
	withStore := (&Service{}).WithRouterFeedbackStore(footerFakeStore{})

	t.Run("terminal agents get the link-free rating hint", func(t *testing.T) {
		for _, app := range []string{ClientAppClaudeCode, ClientAppCodex, ClientAppOpencode} {
			footer := withStore.feedbackFooter(app, turntype.MainLoop)
			assert.Equal(t, feedbackFooterText, footer, "expected hint for %q", app)
			assert.NotContains(t, footer, "http", "footer must never embed a raw link")
		}
	})

	t.Run("ide and unknown clients are suppressed", func(t *testing.T) {
		for _, app := range []string{ClientAppCursor, ClientAppGeminiCLI, "", "some-bot"} {
			assert.Empty(t, withStore.feedbackFooter(app, turntype.MainLoop), "expected no footer for %q", app)
		}
	})

	t.Run("no durable store suppresses the hint entirely", func(t *testing.T) {
		assert.Empty(t, (&Service{}).feedbackFooter(ClientAppClaudeCode, turntype.MainLoop), "advertising a command we cannot record is misleading")
	})
}

func TestFeedbackFooter_TurnTypeGating(t *testing.T) {
	withStore := (&Service{}).WithRouterFeedbackStore(footerFakeStore{})

	t.Run("the user's own conversation turns get the hint", func(t *testing.T) {
		for _, tt := range []turntype.TurnType{turntype.MainLoop, turntype.ToolResult} {
			assert.Equal(t, feedbackFooterText, withStore.feedbackFooter(ClientAppClaudeCode, tt), "expected hint for %q", tt)
		}
	})

	t.Run("subagent and machine turns are suppressed", func(t *testing.T) {
		for _, tt := range []turntype.TurnType{
			turntype.SubAgentDispatch,
			turntype.Compaction,
			turntype.Probe,
			turntype.TitleGen,
			turntype.Classifier,
		} {
			assert.Empty(t, withStore.feedbackFooter(ClientAppClaudeCode, tt), "expected no footer for %q — hint would strand under output the user never initiated", tt)
		}
	})
}
