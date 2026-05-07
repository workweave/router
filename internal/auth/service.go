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
	cache         APIKeyCache
	now           Clock
	encryptor     Encryptor

	// Admin dashboard auth: a single shared password (typically from the
	// ROUTER_ADMIN_PASSWORD env var) plus a derived HMAC key used to sign
	// session cookies. Empty when admin login is disabled.
	adminPassword   string
	adminSessionKey []byte
}

func NewService(
	installations InstallationRepository,
	apiKeys APIKeyRepository,
	externalKeys ExternalAPIKeyRepository,
	cache APIKeyCache,
	now Clock,
) *Service {
	return &Service{
		installations: installations,
		apiKeys:       apiKeys,
		externalKeys:  externalKeys,
		cache:         cache,
		now:           now,
		encryptor:     NoOpEncryptor{},
	}
}

// WithEncryptor sets the encryptor used when creating external API keys.
func (s *Service) WithEncryptor(e Encryptor) *Service {
	s.encryptor = e
	return s
}

// IssueAPIKey creates a new router API key and returns the domain object plus
// the raw token (only time it is visible; not stored in plaintext).
func (s *Service) IssueAPIKey(ctx context.Context, installationID string, name *string, createdBy *string) (*APIKey, string, error) {
	rawToken := GenerateID(APIKeyPrefix)
	keyHash, keyPrefix, keySuffix := APITokenFingerprint(rawToken)
	externalID := GenerateID("kid")
	key, err := s.apiKeys.Create(ctx, CreateAPIKeyParams{
		InstallationID: installationID,
		ExternalID:     externalID,
		Name:           name,
		KeyPrefix:      keyPrefix,
		KeyHash:        keyHash,
		KeySuffix:      keySuffix,
		CreatedBy:      createdBy,
	})
	if err != nil {
		return nil, "", err
	}
	return key, rawToken, nil
}

// ListAPIKeys returns all active API keys for an installation.
func (s *Service) ListAPIKeys(ctx context.Context, installationID string) ([]*APIKey, error) {
	return s.apiKeys.ListForInstallation(ctx, installationID)
}

// DeleteAPIKey soft-deletes an API key. The LRU cache will TTL-expire the
// entry; any in-flight request using the key within the TTL window will still
// succeed, which is acceptable for the rare delete-key path.
func (s *Service) DeleteAPIKey(ctx context.Context, id string) error {
	return s.apiKeys.SoftDelete(ctx, id)
}

// ListExternalAPIKeys returns all active provider API keys for an installation.
func (s *Service) ListExternalAPIKeys(ctx context.Context, installationID string) ([]*ExternalAPIKey, error) {
	return s.externalKeys.GetForInstallation(ctx, installationID)
}

// UpsertExternalAPIKey replaces any existing key for the provider and inserts a
// new one. The raw key is encrypted before storage.
func (s *Service) UpsertExternalAPIKey(ctx context.Context, installationID, provider, rawKey string, name *string, createdBy *string) (*ExternalAPIKey, error) {
	// Generate the external ID first so it can be bound into the
	// ciphertext as AAD. Decrypt callers re-derive the AAD from
	// (external_id, provider) on the row, so the binding is verified
	// on every read.
	externalID := GenerateID("ekid")
	ciphertext, err := s.encryptor.Encrypt([]byte(rawKey), externalID, provider)
	if err != nil {
		return nil, err
	}
	if err := s.externalKeys.SoftDeleteByProvider(ctx, installationID, provider); err != nil {
		return nil, err
	}
	hash, prefix, suffix := APITokenFingerprint(rawKey)
	key, err := s.externalKeys.Create(ctx, CreateExternalAPIKeyParams{
		InstallationID: installationID,
		ExternalID:     externalID,
		Provider:       provider,
		KeyCiphertext:  ciphertext,
		KeyPrefix:      prefix,
		KeySuffix:      suffix,
		KeyFingerprint: hash,
		Name:           name,
		CreatedBy:      createdBy,
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

// DeleteExternalAPIKey soft-deletes a specific provider API key.
func (s *Service) DeleteExternalAPIKey(ctx context.Context, installationID, id string) error {
	return s.externalKeys.SoftDelete(ctx, installationID, id)
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
