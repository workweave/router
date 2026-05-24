package proxy

import (
	"net/http"
	"strings"

	"workweave/router/internal/auth"
	"workweave/router/internal/providers"
)

// Credentials holds the API key to use for an upstream request.
type Credentials struct {
	APIKey []byte // never logged
	Source string // "byok" | "client"
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
			Source: "byok",
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
			return &Credentials{APIKey: []byte(key), Source: "client"}
		}
		// Authorization: Bearer with a real Anthropic API key (sk-ant-api-…)
		// is legitimate client creds; OAuth bearers (sk-ant-oat-…) are
		// session tokens for Claude Code's Claude.ai login and can't be used
		// for direct API calls — they must not enable Anthropic upstream
		// (would 401) nor be misidentified as creds for any other provider.
		if raw, found := strings.CutPrefix(headers.Get("Authorization"), "Bearer "); found {
			key := strings.TrimSpace(raw)
			if strings.HasPrefix(key, "sk-ant-api-") && !auth.HasAPIKeyPrefix(key) {
				return &Credentials{APIKey: []byte(key), Source: "client"}
			}
		}
	case providers.ProviderOpenAI, providers.ProviderGoogle, providers.ProviderOpenRouter, providers.ProviderFireworks, providers.ProviderDeepInfra, providers.ProviderBedrock:
		authHeader := headers.Get("Authorization")
		if raw, found := strings.CutPrefix(authHeader, "Bearer "); found {
			key := strings.TrimSpace(raw)
			// Reject Anthropic-shaped tokens (API keys AND OAuth bearers)
			// here so one Bearer header doesn't get misidentified as creds
			// for every Bearer-using provider.
			if key != "" && !auth.HasAPIKeyPrefix(key) && !strings.HasPrefix(key, "sk-ant-") {
				return &Credentials{APIKey: []byte(key), Source: "client"}
			}
		}
	}
	return nil
}

// ResolveCredentials returns BYOK credentials if available, otherwise falls
// back to client credentials extracted from the inbound request headers.
func ResolveCredentials(provider string, byok map[string]*Credentials, headers http.Header) *Credentials {
	if creds, ok := byok[provider]; ok {
		return creds
	}
	return ExtractClientCredentials(provider, headers)
}
