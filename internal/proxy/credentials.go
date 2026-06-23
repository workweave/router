package proxy

import (
	"context"
	"net/http"
	"strings"

	"workweave/router/internal/auth"
	"workweave/router/internal/providers"
)

// subscriptionTokenPrefix marks a Claude subscription (Claude.ai OAuth) bearer.
// Matches the "oat" (OAuth token) stem so it covers both the sk-ant-oat- and
// the real-world sk-ant-oat01-… Claude Code subscription/access-token shapes.
const subscriptionTokenPrefix = "sk-ant-oat"

// chatGPTAccountIDHeader is the inbound header Codex sends alongside a ChatGPT
// subscription bearer (self-hosted/passthrough path). Its presence disambiguates
// a Codex subscription JWT from a plain OpenAI client API key. http.Header.Get
// canonicalizes the key, so casing variants resolve to the same value.
const chatGPTAccountIDHeader = "ChatGPT-Account-ID"

// Credential sources, for logging and precedence reasoning. Never log the key
// itself — only the source.
const (
	credSourceBYOK              = "byok"
	credSourceClient            = "client"
	credSourceSubscription      = "subscription"
	credSourceCodexSubscription = "codex_subscription"
)

// Credentials holds the API key to use for an upstream request.
type Credentials struct {
	APIKey []byte // never logged
	Source string // credSourceBYOK | credSourceClient | credSourceSubscription | credSourceCodexSubscription
	// OAuth marks a subscription bearer — a Claude subscription token
	// (sk-ant-oat-, resolved only for Anthropic) or a Codex ChatGPT JWT
	// (resolved only for OpenAI). It authenticates via Authorization: Bearer
	// and never x-api-key. Zero value (false) = a normal API key.
	OAuth bool
	// AccountID is the ChatGPT-Account-ID paired with a Codex subscription
	// bearer; the Codex backend 401/403s without it. Set only for Codex
	// subscription credentials, never logged, empty otherwise.
	AccountID []byte
}

// ExternalAPIKeysContextKey is the request-context key for external API keys
// stashed by the auth middleware.
type ExternalAPIKeysContextKey struct{}

// BuildCredentialsMap builds a map of provider -> Credentials from external
// keys. Entries with empty plaintext are dropped: an empty-keyed row would
// otherwise enroll the provider into the routing eligibility set and cause
// the scorer to pick a model the upstream call would 401 on.
func BuildCredentialsMap(keys []*auth.ExternalAPIKey) map[string]*Credentials {
	if len(keys) == 0 {
		return nil
	}
	m := make(map[string]*Credentials, len(keys))
	for _, key := range keys {
		if len(key.Plaintext) == 0 {
			continue
		}
		m[key.Provider] = &Credentials{
			APIKey: key.Plaintext,
			Source: credSourceBYOK,
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// ExtractClientCredentials extracts provider credentials from request headers.
// Anthropic uses x-api-key; OpenAI and Google use Authorization: Bearer.
//
// Router-issued bearers (rk_...) authenticate the same headers via WithAuth,
// so we reject any token carrying auth.APIKeyPrefix to prevent router
// credentials from leaking upstream. TrimSpace matches the auth middleware's
// normalization so embedded whitespace can't slip past the prefix guard.
func ExtractClientCredentials(provider string, headers http.Header) *Credentials {
	switch provider {
	case providers.ProviderAnthropic:
		// Real Anthropic API keys carry the sk-ant- prefix; requiring it here
		// prevents a misplaced cross-provider key (e.g. an OpenAI `sk-…`
		// passed in `x-api-key` by mistake) from being misclassified as
		// Anthropic creds and routed through the summarizer / upstream call.
		if key := strings.TrimSpace(headers.Get("x-api-key")); key != "" &&
			!auth.HasAPIKeyPrefix(key) && strings.HasPrefix(key, "sk-ant-") {
			return &Credentials{APIKey: []byte(key), Source: credSourceClient}
		}
		// Authorization: Bearer with a real Anthropic API key (sk-ant-api-…) is
		// legitimate client creds. OAuth bearers (sk-ant-oat-…) are Claude
		// subscription (Claude.ai login) session tokens: they DO work against
		// /v1/messages, but only with Authorization: Bearer + the oauth beta
		// header and no x-api-key — the anthropic client applies both.
		// We forward them so a caller's subscription pays for their Claude turns.
		if raw, found := strings.CutPrefix(headers.Get("Authorization"), "Bearer "); found {
			key := strings.TrimSpace(raw)
			if !auth.HasAPIKeyPrefix(key) {
				if strings.HasPrefix(key, "sk-ant-api-") {
					return &Credentials{APIKey: []byte(key), Source: credSourceClient}
				}
				if sub := subscriptionCredsFromToken(key); sub != nil {
					return sub
				}
			}
		}
	case providers.ProviderOpenAI, providers.ProviderGoogle, providers.ProviderOpenRouter, providers.ProviderFireworks, providers.ProviderDeepInfra, providers.ProviderBedrock:
		authHeader := headers.Get("Authorization")
		if raw, found := strings.CutPrefix(authHeader, "Bearer "); found {
			key := strings.TrimSpace(raw)
			// A Codex ChatGPT subscription bearer arrives on the OpenAI surface
			// with a ChatGPT-Account-ID header; that pairing is the signal that
			// distinguishes the subscription JWT from a plain client API key, so
			// we resolve it before the client-key branch. OpenAI-only — the JWT
			// can't authenticate any other Bearer-using upstream, and a caller's
			// stray ChatGPT-Account-ID on a non-OpenAI route must not reclassify
			// that route's bearer.
			if provider == providers.ProviderOpenAI {
				if sub := codexSubscriptionCreds(key, headers.Get(chatGPTAccountIDHeader)); sub != nil {
					return sub
				}
			}
			// Reject Anthropic-shaped tokens (API keys AND OAuth bearers)
			// here so one Bearer header doesn't get misidentified as creds
			// for every Bearer-using provider.
			if key != "" && !auth.HasAPIKeyPrefix(key) && !strings.HasPrefix(key, "sk-ant-") {
				return &Credentials{APIKey: []byte(key), Source: credSourceClient}
			}
		}
	}
	return nil
}

// ResolveCredentials returns the credentials to use for provider, in precedence
// order: a caller's Claude subscription token (Anthropic only) first, then BYOK,
// then any other client-supplied header credential. Subscription-first lets a
// caller's own subscription pay for their Claude turns even when an
// installation BYOK Anthropic key is also configured.
func ResolveCredentials(provider string, byok map[string]*Credentials, headers http.Header) *Credentials {
	client := ExtractClientCredentials(provider, headers)
	if client != nil && client.OAuth {
		return client
	}
	if creds, ok := byok[provider]; ok {
		return creds
	}
	return client
}

// subscriptionCredsFromToken returns subscription credentials for a bare token
// (router-key prefix already excluded by the caller), or nil if it isn't a
// Claude subscription bearer. Anthropic-only: the token authenticates only
// against Anthropic's Messages API.
func subscriptionCredsFromToken(token string) *Credentials {
	if !strings.HasPrefix(token, subscriptionTokenPrefix) {
		return nil
	}
	return &Credentials{APIKey: []byte(token), Source: credSourceSubscription, OAuth: true}
}

// codexSubscriptionCreds returns Codex (ChatGPT) subscription credentials for a
// bare OAuth access token paired with its ChatGPT account id, or nil if the
// pair isn't a usable Codex subscription. The token is a ChatGPT-login JWT
// (eyJ…); we reject router keys (rk_) and OpenAI API keys (sk-…), and require a
// non-empty account id because the Codex backend (chatgpt.com/backend-api/codex)
// 401/403s without the ChatGPT-Account-ID header. OpenAI-only: the JWT can't
// authenticate any other upstream.
func codexSubscriptionCreds(token, accountID string) *Credentials {
	token = strings.TrimSpace(token)
	accountID = strings.TrimSpace(accountID)
	if token == "" || accountID == "" {
		return nil
	}
	if auth.HasAPIKeyPrefix(token) || strings.HasPrefix(token, "sk-") {
		return nil
	}
	return &Credentials{
		APIKey:    []byte(token),
		AccountID: []byte(accountID),
		Source:    credSourceCodexSubscription,
		OAuth:     true,
	}
}

// subscriptionCredsFromHeaderValue resolves the dedicated
// X-Weave-Anthropic-Subscription header value into subscription credentials.
// Rejects empty, router-key-prefixed, and non-subscription values so token
// shape knowledge stays in this file.
func subscriptionCredsFromHeaderValue(sub string) *Credentials {
	sub = strings.TrimSpace(sub)
	if sub == "" || auth.HasAPIKeyPrefix(sub) {
		return nil
	}
	return subscriptionCredsFromToken(sub)
}

// clearCredentials returns a context whose resolved-credentials value is an
// explicit nil, so CredentialsFromContext reports none and the provider client
// falls back to its deployment key. Used to keep a caller's subscription/client
// credential off the router's own synthetic upstream calls (e.g. the handover
// summarizer), which lack the Claude Code identity a subscription token needs.
func clearCredentials(ctx context.Context) context.Context {
	return context.WithValue(ctx, CredentialsContextKey{}, (*Credentials)(nil))
}
