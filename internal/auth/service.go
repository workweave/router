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
	cache         APIKeyCache
	now           Clock
}

func NewService(
	installations InstallationRepository,
	apiKeys APIKeyRepository,
	cache APIKeyCache,
	now Clock,
) *Service {
	return &Service{
		installations: installations,
		apiKeys:       apiKeys,
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
func (s *Service) VerifyAPIKey(ctx context.Context, rawToken string) (*Installation, *APIKey, error) {
	if !HasAPIKeyPrefix(rawToken) {
		return nil, nil, ErrInvalidPrefix
	}

	keyHash := HashAPIKeySHA256(rawToken)

	if cached, ok := s.cache.Get(keyHash); ok {
		if cached.Negative {
			return nil, nil, ErrInvalidToken
		}
		if cached.APIKey != nil {
			s.fireMarkUsed(cached.APIKey.ID)
			return cached.Installation, cached.APIKey, nil
		}
		// Malformed positive entry (nil APIKey): fall through to DB lookup.
	}

	apiKey, installation, err := s.apiKeys.GetActiveByHashWithInstallation(ctx, keyHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.cache.Set(keyHash, CachedKey{Negative: true})
			return nil, nil, ErrInvalidToken
		}
		return nil, nil, err
	}

	s.cache.Set(keyHash, CachedKey{APIKey: apiKey, Installation: installation})
	s.fireMarkUsed(apiKey.ID)
	return installation, apiKey, nil
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
