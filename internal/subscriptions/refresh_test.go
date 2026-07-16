package subscriptions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRefresher(anthropicURL, chatGPTIssuer string) *OAuthRefresher {
	r := NewOAuthRefresher(nil)
	r.anthropicTokenURL = anthropicURL
	r.chatGPTIssuer = chatGPTIssuer
	return r
}

func TestOAuthRefresher_AnthropicSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat01-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer srv.Close()

	r := newTestRefresher(srv.URL, "")
	res, err := r.Refresh(context.Background(), providers.ProviderAnthropic, "refresh-old")
	require.NoError(t, err)
	assert.Equal(t, "sk-ant-oat01-new", res.AccessToken)
	assert.Equal(t, "refresh-new", res.RefreshToken)
	assert.False(t, res.ExpiresAt.IsZero())
}

func TestOAuthRefresher_KeepsOldRefreshTokenWhenOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat01-new","expires_in":3600}`))
	}))
	defer srv.Close()

	r := newTestRefresher(srv.URL, "")
	res, err := r.Refresh(context.Background(), providers.ProviderAnthropic, "refresh-old")
	require.NoError(t, err)
	assert.Equal(t, "refresh-old", res.RefreshToken, "OAuth issuer may omit a new refresh token; keep the old one")
}

func TestOAuthRefresher_4xxIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	r := newTestRefresher(srv.URL, "")
	_, err := r.Refresh(context.Background(), providers.ProviderAnthropic, "dead")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRefreshRejected), "a 4xx must be terminal so the credential drops from the pool")
}

func TestOAuthRefresher_5xxIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := newTestRefresher(srv.URL, "")
	_, err := r.Refresh(context.Background(), providers.ProviderAnthropic, "tok")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrRefreshRejected), "a 5xx must be transient so the credential is retried next turn")
}

func TestOAuthRefresher_ChatGPTFormEncoded(t *testing.T) {
	var gotGrant, gotClient string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		require.NoError(t, req.ParseForm())
		gotGrant = req.Form.Get("grant_type")
		gotClient = req.Form.Get("client_id")
		_, _ = w.Write([]byte(`{"access_token":"jwt-new","refresh_token":"r2","expires_in":3600}`))
	}))
	defer srv.Close()

	r := newTestRefresher("", srv.URL)
	res, err := r.Refresh(context.Background(), providers.ProviderOpenAI, "r1")
	require.NoError(t, err)
	assert.Equal(t, "refresh_token", gotGrant)
	assert.Equal(t, chatGPTClientID, gotClient)
	assert.Equal(t, "jwt-new", res.AccessToken)
}

// fakeRefresher counts calls and lets the test hold a refresh open to exercise
// single-flight coalescing.
type fakeRefresher struct {
	calls   atomic.Int32
	release chan struct{}
}

func (f *fakeRefresher) Refresh(_ context.Context, _, _ string) (RefreshResult, error) {
	f.calls.Add(1)
	if f.release != nil {
		<-f.release
	}
	return RefreshResult{AccessToken: "fresh", RefreshToken: "fresh-r"}, nil
}

func TestService_RefreshSingleFlight(t *testing.T) {
	fr := &fakeRefresher{release: make(chan struct{})}
	repo := newFakeRepo()
	cred := seedExpiredCredential(repo)
	svc := NewService(repo, fr, testLogger())

	const goroutines = 5
	var wg sync.WaitGroup
	results := make([]*Credential, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got, err := svc.freshCredential(context.Background(), cred)
			require.NoError(t, err)
			results[idx] = got
		}(i)
	}
	// Let the concurrent callers all land on the in-flight refresh before it
	// completes, then release.
	assert.Eventually(t, func() bool { return fr.calls.Load() >= 1 }, waitShort, pollShort)
	close(fr.release)
	wg.Wait()

	assert.Equal(t, int32(1), fr.calls.Load(), "concurrent refreshes of one credential must coalesce")
	for _, r := range results {
		require.NotNil(t, r)
		assert.Equal(t, []byte("fresh"), r.AccessToken)
	}
}
