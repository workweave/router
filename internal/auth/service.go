package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"workweave/router/internal/observability"
)

type Clock func() time.Time

// Service authenticates incoming bearer tokens. Routing and provider dispatch
// live one ring out in proxy.Service; this package owns identity only.
type Service struct {
	installations InstallationRepository
	apiKeys       APIKeyRepository
	externalKeys  ExternalAPIKeyRepository
	users         UserRepository
	cache         APIKeyCache
	now           Clock
}

func NewService(
	installations InstallationRepository,
	apiKeys APIKeyRepository,
	externalKeys ExternalAPIKeyRepository,
	users UserRepository,
	cache APIKeyCache,
	now Clock,
) *Service {
	return &Service{
		installations: installations,
		apiKeys:       apiKeys,
		externalKeys:  externalKeys,
		users:         users,
		cache:         cache,
		now:           now,
	}
}

// VerifyAPIKey authenticates a raw bearer token. Returns ErrInvalidPrefix or
// ErrInvalidToken for unauthenticated cases; repo transport errors propagate
// as-is so they aren't masked as 401s.
//
// Reads through s.cache: hits short-circuit the DB. ErrNoRows populates a
// negative entry (defends against credential-stuffing); transport errors
// are not cached so the next request can retry.
//
// The returned []*ExternalAPIKey slice contains the installation's active
// customer-owned provider keys (with Plaintext populated). It is nil when no
// external keys exist or when s.externalKeys is nil.
func (s *Service) VerifyAPIKey(ctx context.Context, rawToken string) (*Installation, *APIKey, []*ExternalAPIKey, error) {
	if !HasAPIKeyPrefix(rawToken) {
		return nil, nil, nil, ErrInvalidPrefix
	}

	keyHash := HashAPIKeySHA256(rawToken)

	if cached, ok := s.cache.Get(keyHash); ok {
		if cached.Negative {
			return nil, nil, nil, ErrInvalidToken
		}
		if cached.APIKey != nil {
			s.fireMarkUsed(cached.APIKey.ID)
			return cached.Installation, cached.APIKey, cached.ExternalKeys, nil
		}
		// Malformed positive entry (nil APIKey): fall through to DB lookup.
	}

	apiKey, installation, err := s.apiKeys.GetActiveByHashWithInstallation(ctx, keyHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.cache.Set(keyHash, CachedKey{Negative: true})
			return nil, nil, nil, ErrInvalidToken
		}
		return nil, nil, nil, err
	}

	// Fetch external API keys for this installation.
	var externalKeys []*ExternalAPIKey
	if s.externalKeys != nil {
		externalKeys, err = s.externalKeys.GetForInstallation(ctx, apiKey.InstallationID)
		if err != nil {
			observability.Get().Warn("Failed to fetch external API keys", "installation_id", apiKey.InstallationID, "err", err)
			// Non-fatal: proceed without external keys.
		}
	}

	s.cache.Set(keyHash, CachedKey{APIKey: apiKey, Installation: installation, ExternalKeys: externalKeys})
	s.fireMarkUsed(apiKey.ID)
	return installation, apiKey, externalKeys, nil
}

// ResolveAndStashUser upserts a router user keyed on (installationID, email)
// and stashes the resolved user ID on ctx via UserIDContextKey. Returns the
// original ctx unchanged when email is empty or the upsert fails — user
// resolution is best-effort and must never fail an authenticated request.
//
// Callers normalize email (lower-case, trim) before calling. claudeAccountUUID
// is optional; pass "" when the client isn't Claude Code.
func (s *Service) ResolveAndStashUser(ctx context.Context, installationID, email, claudeAccountUUID string) context.Context {
	if s.users == nil || installationID == "" || email == "" {
		return ctx
	}
	var accountPtr *string
	if claudeAccountUUID != "" {
		accountPtr = &claudeAccountUUID
	}
	user, err := s.users.Upsert(ctx, UpsertUserParams{
		InstallationID:    installationID,
		Email:             email,
		ClaudeAccountUUID: accountPtr,
	})
	if err != nil {
		observability.Get().Warn(
			"Failed to resolve router user",
			"installation_id", installationID,
			"err", err,
		)
		return ctx
	}
	return context.WithValue(ctx, UserIDContextKey{}, user.ID)
}

// fireMarkUsed runs the last_used_at update off the request path. We use
// context.Background because the parent context is often canceled (response
// already written) before the UPDATE completes.
func (s *Service) fireMarkUsed(apiKeyID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.apiKeys.MarkUsed(ctx, apiKeyID); err != nil {
			observability.Get().Warn(
				"Failed to mark router api key used",
				"api_key_id", apiKeyID,
				"err", err,
			)
		}
	}()
}
