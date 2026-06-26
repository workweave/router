package proxy

import (
	"context"

	"workweave/router/internal/auth"
)

// credentialKeyParts returns the safe display parts and source of the turn's
// resolved upstream credential. Empty values mean the turn ran on the deployment
// key or no credential was resolved.
func (s *Service) credentialKeyParts(ctx context.Context) (prefix, suffix, source string) {
	creds := CredentialsFromContext(ctx)
	if creds == nil || len(creds.APIKey) == 0 {
		return "", "", ""
	}
	key := string(creds.APIKey)
	if len(key) <= auth.APITokenPrefixLen+auth.APITokenSuffixLen {
		return key, "", creds.Source
	}
	return key[:auth.APITokenPrefixLen], key[len(key)-auth.APITokenSuffixLen:], creds.Source
}
