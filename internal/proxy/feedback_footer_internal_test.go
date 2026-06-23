package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/feedback"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type footerFakeStore struct{}

func (footerFakeStore) InsertRouterFeedback(context.Context, RouterFeedbackEvent) error { return nil }

func TestFeedbackFooter_ClientGating(t *testing.T) {
	withStore := (&Service{}).WithRouterFeedbackStore(footerFakeStore{})

	t.Run("terminal agents get the rating hint", func(t *testing.T) {
		for _, app := range []string{ClientAppClaudeCode, ClientAppCodex, ClientAppOpencode} {
			assert.Equal(t, feedbackFooterText, withStore.feedbackFooter(app, uuid.Nil, "", "", ""), "expected hint for %q", app)
		}
	})

	t.Run("ide and unknown clients are suppressed", func(t *testing.T) {
		for _, app := range []string{ClientAppCursor, ClientAppGeminiCLI, "", "some-bot"} {
			assert.Empty(t, withStore.feedbackFooter(app, uuid.Nil, "", "", ""), "expected no footer for %q", app)
		}
	})

	t.Run("no token and no durable store suppresses the hint entirely", func(t *testing.T) {
		assert.Empty(t, (&Service{}).feedbackFooter(ClientAppClaudeCode, uuid.Nil, "", "", ""), "advertising a command we cannot record is misleading")
	})
}

func TestFeedbackFooter_ClickableThumbsWhenTokenMintable(t *testing.T) {
	signer := feedback.NewSigner("test-secret", time.Hour)
	require.NotNil(t, signer)
	svc := (&Service{}).WithFeedback(nil, signer, "https://feedback.example")

	footer := svc.feedbackFooter(ClientAppClaudeCode, uuid.New(), "ext-1", uuid.New().String(), "")

	require.NotEmpty(t, footer, "a mintable token must produce a footer")
	assert.Contains(t, footer, "[👍](https://feedback.example/v1/feedback/rate?t=", "thumbs up must be a clickable rate link")
	assert.Contains(t, footer, "&r=down)", "thumbs down must target the down rating")
	assert.Contains(t, footer, "`/rf+`", "the /rf companion trails the links for non-OSC-8 terminals")
	assert.Contains(t, footer, "[👎]", "both thumbs are rendered as links")
	assert.True(t, strings.Index(footer, "[👍]") < strings.Index(footer, "`/rf+`"), "thumbs precede the reply commands")
}
