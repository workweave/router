package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/router"
)

func TestOpenAIHarnessForRequestRequiresUsableCodexSubscription(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want router.Harness
	}{
		{
			name: "no subscription",
			ctx:  context.Background(),
			want: router.HarnessAPI,
		},
		{
			name: "responses body without subscription",
			ctx:  context.WithValue(context.Background(), codexResponsesBodyContextKey{}, []byte(`{"model":"gpt-5.5"}`)),
			want: router.HarnessAPI,
		},
		{
			name: "token without account id",
			ctx:  context.WithValue(context.Background(), OpenAISubscriptionContextKey{}, codexTestJWT),
			want: router.HarnessAPI,
		},
		{
			name: "codex client identity",
			ctx: context.WithValue(context.Background(), ClientIdentityContextKey{}, ClientIdentity{
				ClientApp: ClientAppCodex,
			}),
			want: router.HarnessCodex,
		},
		{
			name: "cursor client identity",
			ctx: context.WithValue(context.Background(), ClientIdentityContextKey{}, ClientIdentity{
				ClientApp: ClientAppCursor,
			}),
			want: router.HarnessCursor,
		},
		{
			name: "claude code client identity",
			ctx: context.WithValue(context.Background(), ClientIdentityContextKey{}, ClientIdentity{
				ClientApp: ClientAppClaudeCode,
			}),
			want: router.HarnessClaudeCode,
		},
		{
			name: "complete codex subscription",
			ctx: func() context.Context {
				ctx := context.WithValue(context.Background(), OpenAISubscriptionContextKey{}, codexTestJWT)
				return context.WithValue(ctx, OpenAIAccountIDContextKey{}, "acct-1")
			}(),
			want: router.HarnessCodex,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, openAIHarnessForRequest(tc.ctx))
		})
	}
}
