package policyclient

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials/idtoken"
	"cloud.google.com/go/auth/httptransport"
)

// NewGoogleIDToken builds a policy client that authenticates every request
// with a Google-signed ID token whose audience is the exact sidecar origin.
// It is intended for managed Cloud Run sidecars; local and self-hosted callers
// should continue to use New with an ordinary HTTP client.
func NewGoogleIDToken(baseURL string, timeout time.Duration) (*Client, error) {
	audience := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if audience == "" {
		return nil, fmt.Errorf("build Google ID-token policy client: sidecar URL is empty")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	credentials, err := idtoken.NewCredentials(&idtoken.Options{Audience: audience})
	if err != nil {
		return nil, fmt.Errorf("build Google ID-token credentials for %q: %w", audience, err)
	}
	httpClient, err := newGoogleIDTokenHTTPClient(credentials, timeout)
	if err != nil {
		return nil, fmt.Errorf("build Google ID-token HTTP client for %q: %w", audience, err)
	}
	return New(audience, httpClient, timeout), nil
}

func newGoogleIDTokenHTTPClient(credentials *auth.Credentials, timeout time.Duration) (*http.Client, error) {
	client := &http.Client{Timeout: timeout}
	if err := httptransport.AddAuthorizationMiddleware(client, credentials); err != nil {
		return nil, err
	}
	return client, nil
}
