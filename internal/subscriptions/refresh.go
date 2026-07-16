package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"workweave/router/internal/config"
	"workweave/router/internal/providers"
)

// OAuth client IDs and token endpoints. These mirror the public values baked
// into the opencode plugin (install/opencode-weave/src/index.ts) and Claude
// Code / Codex themselves — they identify the app, not the user, so keeping
// them in source is fine. URLs are env-overridable for self-hosted auth proxies
// and tests.
const (
	anthropicClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	chatGPTClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"

	defaultAnthropicTokenURL = "https://console.anthropic.com/v1/oauth/token"
	defaultChatGPTIssuer     = "https://auth.openai.com"

	defaultTokenExpirySeconds = 3600
)

// ErrRefreshRejected marks a terminal (4xx) refusal from the token endpoint —
// the refresh token is dead and the credential needs re-enrollment. Transient
// (5xx/network) failures return a plain error and are retried next turn.
var ErrRefreshRejected = errors.New("subscription token refresh rejected")

// RefreshResult carries rotated tokens from a successful refresh.
type RefreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// TokenRefresher exchanges a refresh token for a fresh access token.
type TokenRefresher interface {
	Refresh(ctx context.Context, provider, refreshToken string) (RefreshResult, error)
}

// OAuthRefresher is the production TokenRefresher, calling the Claude and
// ChatGPT OAuth token endpoints.
type OAuthRefresher struct {
	client            *http.Client
	anthropicTokenURL string
	chatGPTIssuer     string
	now               func() time.Time
}

// NewOAuthRefresher constructs an OAuthRefresher, reading URL overrides from
// the environment.
func NewOAuthRefresher(client *http.Client) *OAuthRefresher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &OAuthRefresher{
		client:            client,
		anthropicTokenURL: config.GetOr("ROUTER_ANTHROPIC_OAUTH_TOKEN_URL", defaultAnthropicTokenURL),
		chatGPTIssuer:     config.GetOr("ROUTER_CODEX_OAUTH_ISSUER", defaultChatGPTIssuer),
		now:               time.Now,
	}
}

// Refresh dispatches to the provider-specific token endpoint.
func (r *OAuthRefresher) Refresh(ctx context.Context, provider, refreshToken string) (RefreshResult, error) {
	switch provider {
	case providers.ProviderAnthropic:
		return r.refreshAnthropic(ctx, refreshToken)
	case providers.ProviderOpenAI:
		return r.refreshChatGPT(ctx, refreshToken)
	default:
		return RefreshResult{}, fmt.Errorf("unsupported subscription provider %q", provider)
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// refreshAnthropic mirrors refreshAnthropicToken in the opencode plugin: a JSON
// POST to the console token endpoint. The issuer may omit a new refresh token,
// in which case the existing one is kept.
func (r *OAuthRefresher) refreshAnthropic(ctx context.Context, refreshToken string) (RefreshResult, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     anthropicClientID,
	})
	if err != nil {
		return RefreshResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.anthropicTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return RefreshResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	return r.do(req, refreshToken)
}

// refreshChatGPT mirrors refreshAccessToken in the opencode plugin: a
// form-encoded POST to the issuer's token endpoint.
func (r *OAuthRefresher) refreshChatGPT(ctx context.Context, refreshToken string) (RefreshResult, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {chatGPTClientID},
	}
	endpoint := strings.TrimRight(r.chatGPTIssuer, "/") + "/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r.do(req, refreshToken)
}

func (r *OAuthRefresher) do(req *http.Request, oldRefreshToken string) (RefreshResult, error) {
	resp, err := r.client.Do(req)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("token refresh request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var tok tokenResponse
		if err := json.Unmarshal(respBody, &tok); err != nil {
			return RefreshResult{}, fmt.Errorf("decode token response: %w", err)
		}
		if tok.AccessToken == "" {
			return RefreshResult{}, errors.New("token refresh returned empty access token")
		}
		newRefresh := tok.RefreshToken
		if newRefresh == "" {
			newRefresh = oldRefreshToken
		}
		expiresIn := tok.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = defaultTokenExpirySeconds
		}
		return RefreshResult{
			AccessToken:  tok.AccessToken,
			RefreshToken: newRefresh,
			ExpiresAt:    r.now().Add(time.Duration(expiresIn) * time.Second),
		}, nil
	}

	// 4xx = dead refresh token (revoked / expired / re-consent needed): terminal.
	// 5xx / anything else = transient; retry next turn on the same token.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return RefreshResult{}, fmt.Errorf("%w: status %d", ErrRefreshRejected, resp.StatusCode)
	}
	return RefreshResult{}, fmt.Errorf("token refresh failed: status %d", resp.StatusCode)
}
