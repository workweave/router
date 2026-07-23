package proxy_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/proxy/usage"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	oaiSubFailoverToken = "sk-ant-oat01-openai-sub-failover-token"
	oaiSubFailoverModel = "claude-haiku-4-5"
	oaiSubFailoverOK    = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"claude-haiku-4-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
)

// oauthRejectThenDeployOK fails under OAuth subscription creds, then succeeds on the deploy-key path.
type oauthRejectThenDeployOK struct {
	mu      sync.Mutex
	onOAuth error
	calls   []oauthCallSnap
}

type oauthCallSnap struct {
	nilCreds bool
	oauth    bool
	source   string
	key      string
	err      error
}

func (p *oauthRejectThenDeployOK) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	creds := proxy.CredentialsFromContext(ctx)
	snap := oauthCallSnap{nilCreds: creds == nil}
	if creds != nil {
		snap.oauth = creds.OAuth
		snap.source = creds.Source
		snap.key = string(creds.APIKey)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if creds != nil && creds.OAuth {
		snap.err = p.onOAuth
		p.calls = append(p.calls, snap)
		return p.onOAuth
	}
	p.calls = append(p.calls, snap)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(oaiSubFailoverOK))
	return nil
}

func (p *oauthRejectThenDeployOK) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return fmt.Errorf("unused")
}

func oaiSubSlackObs(t *testing.T) *usage.Observer {
	t.Helper()
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	// Slack: preemptive suppress must not fire; only reactive subscription retry recovers.
	obs.Record(obs.Key([]byte(oaiSubFailoverToken)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.50, WindowMinutes: 300},
	})
	return obs
}

func oaiSubCtx() context.Context {
	return context.WithValue(context.Background(), proxy.AnthropicSubscriptionContextKey{}, oaiSubFailoverToken)
}

func oaiSubMainLoopBody() []byte {
	return []byte(`{"model":"` + oaiSubFailoverModel + `","max_tokens":4096,"messages":[{"role":"user","content":"Refactor the auth middleware and add tests."}],"tools":[{"type":"function","function":{"name":"edit_file","parameters":{"type":"object","properties":{}}}}]}`)
}

func oaiSubFailoverSvc(t *testing.T, p providers.Client) *proxy.Service {
	t.Helper()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: oaiSubFailoverModel, Reason: "cluster:test"}}
	return proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, oaiSubFailoverModel, nil).
		WithSubscriptionAwareRouting(oaiSubSlackObs(t), 0.05, 2.0).
		WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderAnthropic: {}})
}

// TestProxyOpenAI_SubscriptionRetry_Live429FailsOverToDeployKey: live 429 on OAuth → Weave/BYOK retry.
func TestProxyOpenAI_SubscriptionRetry_Live429FailsOverToDeployKey(t *testing.T) {
	reject := &providers.UpstreamErrorResponse{
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"weekly limit exceeded"}}`),
	}
	p := &oauthRejectThenDeployOK{onOAuth: reject}
	svc := oaiSubFailoverSvc(t, p)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	err := svc.ProxyOpenAIChatCompletion(oaiSubCtx(), oaiSubMainLoopBody(), rec, req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	p.mu.Lock()
	defer p.mu.Unlock()
	require.GreaterOrEqual(t, len(p.calls), 2, "must retry after the subscription 429")
	assert.True(t, p.calls[0].oauth && p.calls[0].source == "subscription")
	last := p.calls[len(p.calls)-1]
	assert.True(t, last.nilCreds || !last.oauth, "final dispatch must use the deploy key, not the spent OAuth token")
	assert.Contains(t, rec.Body.String(), `"object":"chat.completion"`)
}

// TestProxyOpenAI_SubscriptionRetry_OAuth401FailsOverToDeployKey: OAuth authentication_error → deploy key.
func TestProxyOpenAI_SubscriptionRetry_OAuth401FailsOverToDeployKey(t *testing.T) {
	reject := &providers.UpstreamErrorResponse{
		Status: http.StatusUnauthorized,
		Body:   []byte(`{"type":"error","error":{"type":"authentication_error","message":"Invalid authentication credentials"}}`),
	}
	p := &oauthRejectThenDeployOK{onOAuth: reject}
	svc := oaiSubFailoverSvc(t, p)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	err := svc.ProxyOpenAIChatCompletion(oaiSubCtx(), oaiSubMainLoopBody(), rec, req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	p.mu.Lock()
	defer p.mu.Unlock()
	require.GreaterOrEqual(t, len(p.calls), 2)
	assert.True(t, p.calls[0].oauth)
	last := p.calls[len(p.calls)-1]
	assert.True(t, last.nilCreds || !last.oauth)
}

// TestProxyOpenAI_SubscriptionRetry_OAuth403FailsOverToDeployKey: OAuth permission_error → deploy key.
func TestProxyOpenAI_SubscriptionRetry_OAuth403FailsOverToDeployKey(t *testing.T) {
	reject := &providers.UpstreamErrorResponse{
		Status: http.StatusForbidden,
		Body:   []byte(`{"type":"error","error":{"type":"permission_error","message":"OAuth authentication is currently not allowed for this organization."}}`),
	}
	p := &oauthRejectThenDeployOK{onOAuth: reject}
	svc := oaiSubFailoverSvc(t, p)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	err := svc.ProxyOpenAIChatCompletion(oaiSubCtx(), oaiSubMainLoopBody(), rec, req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	p.mu.Lock()
	defer p.mu.Unlock()
	require.GreaterOrEqual(t, len(p.calls), 2)
	assert.True(t, p.calls[0].oauth)
	last := p.calls[len(p.calls)-1]
	assert.True(t, last.nilCreds || !last.oauth)
}
