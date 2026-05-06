package proxy

import (
	"net/http"
	"strings"

	"workweave/router/internal/auth"
)

// Credentials holds the API key to use for an upstream request.
type Credentials struct {
	APIKey []byte // never logged
	Source string // "byok" | "client"
}

// ExternalAPIKeysContextKey is the request-context key for external API keys
// stashed by the auth middleware.
type ExternalAPIKeysContextKey struct{}

// BuildCredentialsMap builds a map of provider -> Credentials from external keys.
func BuildCredentialsMap(keys []*auth.ExternalAPIKey) map[string]*Credentials {
	if len(keys) == 0 {
		return nil
	}
	m := make(map[string]*Credentials, len(keys))
	for _, key := range keys {
		m[key.Provider] = &Credentials{
			APIKey: key.Plaintext,
			Source: "byok",
		}
	}
	return m
}

// ExtractClientCredentials extracts provider credentials from request headers.
// Anthropic uses x-api-key; OpenAI and Google use Authorization: Bearer.
//
// The same headers are also consumed by router auth (WithAuth in
// internal/server/middleware/auth.go), so a router-issued bearer (rk_...)
// would otherwise leak to the upstream provider when BYOK is absent.
// Reject any token carrying auth.APIKeyPrefix so router credentials never
// cross the trust boundary into third-party provider infrastructure.
//
// Header values are TrimSpace'd before the prefix check to match the auth
// middleware's normalization — without this, embedded whitespace (e.g.
// "Bearer  rk_xxx") would authenticate as a router key but slip past the
// HasAPIKeyPrefix guard and leak upstream.
func ExtractClientCredentials(provider string, headers http.Header) *Credentials {
	switch provider {
	case "anthropic":
		if key := strings.TrimSpace(headers.Get("x-api-key")); key != "" && !auth.HasAPIKeyPrefix(key) {
			return &Credentials{APIKey: []byte(key), Source: "client"}
		}
	case "openai", "google":
		authHeader := headers.Get("Authorization")
		if raw, found := strings.CutPrefix(authHeader, "Bearer "); found {
			key := strings.TrimSpace(raw)
			if key != "" && !auth.HasAPIKeyPrefix(key) {
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

