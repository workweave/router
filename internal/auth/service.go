package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"workweave/router/internal/observability"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// ErrUnknownModel is returned when a requested model ID is not in the caller-supplied allowed set.
var ErrUnknownModel = errors.New("auth: unknown model id")

type Clock func() time.Time

// InstallationChangeNotifier fans out installation-change events to peer replicas.
// Fire-and-forget: implementations must not block the caller.
type InstallationChangeNotifier interface {
	NotifyInstallationChanged(installationID string)
}

// NoOpInstallationChangeNotifier is the Null Object when no cross-replica fanout is configured.
type NoOpInstallationChangeNotifier struct{}

// NotifyInstallationChanged is a no-op.
func (NoOpInstallationChangeNotifier) NotifyInstallationChanged(string) {}

// Service authenticates incoming bearer tokens. Identity only; routing/dispatch lives in proxy.Service.
type Service struct {
	installations InstallationRepository
	apiKeys       APIKeyRepository
	externalKeys  ExternalAPIKeyRepository
	users         UserRepository
	cache         APIKeyCache
	userCache     UserCache
	notifier      InstallationChangeNotifier
	now           Clock
	encryptor     Encryptor

	// adminPassword and adminSessionKey are empty when admin login is disabled.
	adminPassword   string
	adminSessionKey []byte

	// adminLoginFailures throttles per-IP brute-force login attempts.
	adminLoginFailures *expirable.LRU[string, int]
	adminLoginMu       sync.Mutex
}

func NewService(
	installations InstallationRepository,
	apiKeys APIKeyRepository,
	externalKeys ExternalAPIKeyRepository,
	users UserRepository,
	cache APIKeyCache,
	userCache UserCache,
	now Clock,
) *Service {
	if userCache == nil {
		userCache = NoOpUserCache{}
	}
	return &Service{
		installations: installations,
		apiKeys:       apiKeys,
		externalKeys:  externalKeys,
		users:         users,
		cache:         cache,
		userCache:     userCache,
		notifier:      NoOpInstallationChangeNotifier{},
		now:           now,
		encryptor:     NoOpEncryptor{},
	}
}

func (s *Service) WithEncryptor(e Encryptor) *Service {
	s.encryptor = e
	return s
}

// WithInstallationChangeNotifier wires a cross-replica fanout. Pass nil to disable.
func (s *Service) WithInstallationChangeNotifier(n InstallationChangeNotifier) *Service {
	if n == nil {
		s.notifier = NoOpInstallationChangeNotifier{}
		return s
	}
	s.notifier = n
	return s
}

// invalidateInstallation evicts the local cache and fans out to peer replicas.
// Always called after a successful DB commit so listeners observe the new state.
func (s *Service) invalidateInstallation(installationID string) {
	if installationID == "" {
		return
	}
	s.cache.InvalidateInstallation(installationID)
	s.notifier.NotifyInstallationChanged(installationID)
}

// IssueAPIKey creates a new router API key and returns the raw token.
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

// RotateAPIKey soft-deletes all active keys and issues a new one, carrying forward the name.
// Not wrapped in a tx: a brief "no active key" window is acceptable because rotation's purpose
// is to invalidate the old token anyway.
func (s *Service) RotateAPIKey(ctx context.Context, installationID string, createdBy *string) (*APIKey, string, error) {
	existing, err := s.apiKeys.ListForInstallation(ctx, installationID)
	if err != nil {
		return nil, "", err
	}
	var name *string
	for _, k := range existing {
		if k.Name != nil && name == nil {
			name = k.Name
		}
		if err := s.apiKeys.SoftDelete(ctx, k.ID); err != nil {
			return nil, "", err
		}
	}
	key, raw, err := s.IssueAPIKey(ctx, installationID, name, createdBy)
	if err != nil {
		return nil, "", err
	}
	s.invalidateInstallation(installationID)
	return key, raw, nil
}

// DeleteAPIKey soft-deletes an API key.
func (s *Service) DeleteAPIKey(ctx context.Context, id string) error {
	return s.apiKeys.SoftDelete(ctx, id)
}

// ListExternalAPIKeys returns all active provider API keys for an installation.
func (s *Service) ListExternalAPIKeys(ctx context.Context, installationID string) ([]*ExternalAPIKey, error) {
	return s.externalKeys.GetForInstallation(ctx, installationID)
}

// UpsertExternalAPIKey replaces any existing key for the provider and inserts a new one.
func (s *Service) UpsertExternalAPIKey(ctx context.Context, installationID, provider, rawKey string, name *string, createdBy *string) (*ExternalAPIKey, error) {
	// Generate external ID first so it binds into the ciphertext as AAD.
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
	s.invalidateInstallation(installationID)
	return key, nil
}

// DeleteExternalAPIKey soft-deletes a specific provider API key.
func (s *Service) DeleteExternalAPIKey(ctx context.Context, installationID, id string) error {
	if err := s.externalKeys.SoftDelete(ctx, installationID, id); err != nil {
		return err
	}
	s.invalidateInstallation(installationID)
	return nil
}

// SetInstallationExcludedModels replaces the per-installation model exclusion list.
// allowed is the set of valid model IDs; passing nil skips validation.
func (s *Service) SetInstallationExcludedModels(ctx context.Context, externalID, installationID string, models []string, allowed map[string]struct{}) ([]string, error) {
	if models == nil {
		models = []string{}
	}
	if allowed != nil {
		for _, m := range models {
			if _, ok := allowed[m]; !ok {
				return nil, fmt.Errorf("%w: %q", ErrUnknownModel, m)
			}
		}
	}
	// De-dupe while preserving order so the persisted list is stable.
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, m := range models {
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	if err := s.installations.UpdateExcludedModels(ctx, externalID, installationID, out); err != nil {
		return nil, err
	}
	s.invalidateInstallation(installationID)
	return out, nil
}

// VerifyAPIKey authenticates a raw bearer token against the API key cache then Postgres.
// Returns ErrInvalidPrefix/ErrInvalidToken for unauthenticated cases.
// Returned ExternalAPIKey slice has Plaintext populated; nil when none exist.
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

	var externalKeys []*ExternalAPIKey
	if s.externalKeys != nil {
		externalKeys, err = s.externalKeys.GetForInstallation(ctx, apiKey.InstallationID)
		if err != nil {
			// Non-fatal: proceed without external keys.
			observability.Get().Warn("Failed to fetch external API keys", "installation_id", apiKey.InstallationID, "err", err)
		}
	}

	s.cache.Set(keyHash, CachedKey{APIKey: apiKey, Installation: installation, ExternalKeys: externalKeys})
	s.fireMarkUsed(apiKey.ID)
	return installation, apiKey, externalKeys, nil
}

// ResolveAndStashUser upserts a router user and stashes the ID on ctx.
// Email takes precedence as the lookup key. When only claudeAccountUUID is present,
// the row is keyed on account_uuid with NULL email.
// Best-effort: returns ctx unchanged on failure — must never fail an authenticated request.
func (s *Service) ResolveAndStashUser(ctx context.Context, installationID, email, claudeAccountUUID string) context.Context {
	log := observability.Get()
	if s.users == nil || installationID == "" {
		log.Info("ResolveAndStashUser bailout", "reason", "nil_users_or_empty_inst", "users_nil", s.users == nil, "inst_empty", installationID == "")
		return ctx
	}
	if email == "" && claudeAccountUUID == "" {
		log.Info("ResolveAndStashUser bailout", "reason", "no_identity_signal", "installation_id", installationID)
		return ctx
	}

	identityKey := userIdentityKey(email, claudeAccountUUID)
	if cached, ok := s.userCache.Get(installationID, identityKey); ok {
		log.Debug("ResolveAndStashUser cache hit", "installation_id", installationID, "user_id", cached)
		return context.WithValue(ctx, UserIDContextKey{}, cached)
	}

	log.Debug("ResolveAndStashUser upsert", "installation_id", installationID, "email_present", email != "", "account_present", claudeAccountUUID != "")
	var user *User
	var err error
	if email != "" {
		var accountPtr *string
		if claudeAccountUUID != "" {
			accountPtr = &claudeAccountUUID
		}
		user, err = s.users.UpsertByEmail(ctx, UpsertUserParams{
			InstallationID:    installationID,
			Email:             email,
			ClaudeAccountUUID: accountPtr,
		})
	} else {
		user, err = s.users.UpsertByAccountUUID(ctx, UpsertUserByAccountUUIDParams{
			InstallationID:    installationID,
			ClaudeAccountUUID: claudeAccountUUID,
		})
	}
	if err != nil {
		observability.Get().Warn(
			"Failed to resolve router user",
			"installation_id", installationID,
			"err", err,
		)
		return ctx
	}
	s.userCache.Set(installationID, identityKey, user.ID)
	log.Debug("ResolveAndStashUser upsert ok", "installation_id", installationID, "user_id", user.ID)
	return context.WithValue(ctx, UserIDContextKey{}, user.ID)
}

func userIdentityKey(email, claudeAccountUUID string) string {
	if email != "" {
		return "email:" + email
	}
	return "account:" + claudeAccountUUID
}

// fireMarkUsed runs the last_used_at update off the request path. Uses context.Background because
// the parent ctx is often canceled (response written) before the UPDATE completes.
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
