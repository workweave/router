package policyclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router/policy"
)

type staticTokenProvider struct{}

func (staticTokenProvider) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "test-id-token", Type: "Bearer"}, nil
}

func TestGoogleIDTokenClientAuthenticatesEveryEndpoint(t *testing.T) {
	paths := make([]string, 0, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "Bearer test-id-token", request.Header.Get("Authorization"))
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/capabilities":
			_ = json.NewEncoder(w).Encode(policy.Capabilities{SchemaVersion: policy.SchemaVersionV1})
		case "/route":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version":     policy.SchemaVersionV1,
				"selected_roster_id": "model-a",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer server.Close()

	httpClient, err := newGoogleIDTokenHTTPClient(
		&auth.Credentials{TokenProvider: staticTokenProvider{}},
		time.Second,
	)
	require.NoError(t, err)
	client := New(server.URL, httpClient, time.Second)

	require.NoError(t, client.CheckHealth(context.Background()))
	_, err = client.Capabilities(context.Background())
	require.NoError(t, err)
	_, err = client.Decide(context.Background(), policy.Query{
		Candidates: []policy.Candidate{{RosterID: "model-a"}},
	})
	require.NoError(t, err)
	require.NoError(t, client.ReportOutcome(context.Background(), map[string]any{"route_id": "route-1"}))
	require.NoError(t, client.ReportFeedback(context.Background(), map[string]any{"route_id": "route-1"}))

	assert.Equal(t, []string{"/readyz", "/capabilities", "/route", "/outcome", "/feedback"}, paths)
}

func TestNewGoogleIDTokenRejectsEmptySidecarURL(t *testing.T) {
	client, err := NewGoogleIDToken(" ", time.Second)

	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "sidecar URL is empty")
}

func TestGoogleIDTokenURLsUseOriginAsAudience(t *testing.T) {
	baseURL, audience, err := googleIDTokenURLs(" https://service.run.app/policy/ ")

	require.NoError(t, err)
	assert.Equal(t, "https://service.run.app/policy", baseURL)
	assert.Equal(t, "https://service.run.app", audience)
}

func TestGoogleIDTokenClientDoesNotFollowRedirects(t *testing.T) {
	redirected := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected = true
	}))
	defer destination.Close()

	source := httptest.NewServer(http.RedirectHandler(destination.URL, http.StatusTemporaryRedirect))
	defer source.Close()

	httpClient, err := newGoogleIDTokenHTTPClient(
		&auth.Credentials{TokenProvider: staticTokenProvider{}},
		time.Second,
	)
	require.NoError(t, err)

	response, err := httpClient.Get(source.URL)
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	assert.False(t, redirected)
}
