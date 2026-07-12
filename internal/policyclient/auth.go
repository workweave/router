package policyclient

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials/idtoken"
	"cloud.google.com/go/auth/httptransport"
)

// NewGoogleIDToken builds a Client that attaches a Google-signed ID token
// (audience = sidecar origin) to every request; for Cloud Run sidecars only.
func NewGoogleIDToken(baseURL string, timeout time.Duration) (*Client, error) {
	normalizedBaseURL, audience, err := googleIDTokenURLs(baseURL)
	if err != nil {
		return nil, fmt.Errorf("build Google ID-token policy client: %w", err)
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
	return New(normalizedBaseURL, httpClient, timeout), nil
}

func googleIDTokenURLs(baseURL string) (string, string, error) {
	normalized := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if normalized == "" {
		return "", "", fmt.Errorf("sidecar URL is empty")
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", "", fmt.Errorf("parse sidecar URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", "", fmt.Errorf("sidecar URL must be an absolute HTTP(S) URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("sidecar URL must not contain a query or fragment")
	}
	audience := (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
	return normalized, audience, nil
}

func newGoogleIDTokenHTTPClient(credentials *auth.Credentials, timeout time.Duration) (*http.Client, error) {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if err := httptransport.AddAuthorizationMiddleware(client, credentials); err != nil {
		return nil, err
	}
	return client, nil
}
