// Package subscriptions manages the per-user pool of enrolled Claude/ChatGPT
// subscription OAuth credentials: storage via an injected Repository, hourly
// access-token refresh against the public OAuth token endpoints, and selection
// of the next usable credential when a caller's own subscription is exhausted.
package subscriptions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
)

// Credential is one enrolled subscription account, decrypted for use.
// AccessToken/RefreshToken are plaintext in memory for the request lifetime
// only and must never be logged.
type Credential struct {
	ID               string
	ExternalID       string
	InstallationID   string
	UserEmail        string
	Provider         string
	AccountLabel     string
	ChatGPTAccountID string
	AccessToken      []byte
	RefreshToken     []byte
	ExpiresAt        time.Time
	LastUsedAt       time.Time
	RefreshFailedAt  time.Time
	CreatedAt        time.Time
}

// CreateParams carries a new enrollment into the Repository. Tokens are
// plaintext; the repository encrypts at the adapter boundary.
type CreateParams struct {
	InstallationID     string
	ExternalID         string
	UserEmail          string
	Provider           string
	AccountLabel       string
	AccountFingerprint string
	ChatGPTAccountID   string
	AccessToken        []byte
	RefreshToken       []byte
	ExpiresAt          time.Time
	CreatedBy          string
}

// Repository is the persistence contract for subscription credentials.
// Implemented by internal/postgres, which owns encryption at rest.
type Repository interface {
	Create(ctx context.Context, params CreateParams) (*Credential, error)
	// ReplaceByFingerprint atomically soft-deletes any existing credential with
	// the same (installation, user, provider, account fingerprint) and inserts
	// params, in one transaction — so a failed insert can never destroy the
	// credential it was meant to replace on re-enrollment.
	ReplaceByFingerprint(ctx context.Context, params CreateParams) (*Credential, error)
	// GetActiveForUser returns the usable pool (excludes refresh-failed rows),
	// oldest-enrolled first.
	GetActiveForUser(ctx context.Context, installationID, userEmail string) ([]*Credential, error)
	// ListForUser returns every non-deleted credential, including refresh-failed
	// ones, for the enrollment listing endpoint.
	ListForUser(ctx context.Context, installationID, userEmail string) ([]*Credential, error)
	UpdateTokens(ctx context.Context, id, externalID, provider string, accessToken, refreshToken []byte, expiresAt time.Time) error
	MarkRefreshFailed(ctx context.Context, id string) error
	MarkUsed(ctx context.Context, id string) error
	// SoftDelete removes one credential scoped to (installation, email); returns
	// false when no row matched (foreign or unknown id).
	SoftDelete(ctx context.Context, installationID, userEmail, id string) (bool, error)
}

// externalIDPrefix fronts subscription-credential external IDs ("scid_...").
const externalIDPrefix = "scid"

// refreshSkew is how close to expiry an access token may get before selection
// refreshes it synchronously. Wide enough to cover the upstream turn's own
// latency so a token never expires mid-dispatch.
const refreshSkew = 2 * time.Minute

// ErrCredentialNotFound reports a Remove that matched no row for the caller.
var ErrCredentialNotFound = errors.New("subscription credential not found")

// Service owns the pool: enrollment, listing, removal, cached loads, and
// refresh-on-selection. Constructed once in the composition root; nil means
// the feature is off.
type Service struct {
	repo      Repository
	refresher TokenRefresher
	cache     *poolCache
	now       func() time.Time
	log       *slog.Logger

	refreshMu sync.Mutex
	inflight  map[string]*refreshCall
}

type refreshCall struct {
	done chan struct{}
	cred *Credential
	err  error
}

// NewService constructs the pool service.
func NewService(repo Repository, refresher TokenRefresher, log *slog.Logger) *Service {
	return &Service{
		repo:      repo,
		refresher: refresher,
		cache:     newPoolCache(),
		now:       time.Now,
		log:       log,
		inflight:  make(map[string]*refreshCall),
	}
}

// EnrollParams is a validated enrollment request.
type EnrollParams struct {
	InstallationID   string
	UserEmail        string
	Provider         string
	AccountLabel     string
	ChatGPTAccountID string
	// ClaudeAccountID is the Claude account's stable uuid (from the OAuth token
	// response). Used only to fingerprint the account so re-enrolling the same
	// Claude account replaces its row instead of piling up duplicates — the
	// refresh token rotates on every login and can't identify the account.
	ClaudeAccountID string
	AccessToken     string
	RefreshToken    string
	ExpiresAt       time.Time
	CreatedBy       string
}

// Enroll stores a new pool credential, replacing any existing enrollment of
// the same account (matched by fingerprint).
func (s *Service) Enroll(ctx context.Context, p EnrollParams) (*Credential, error) {
	fingerprint := accountFingerprint(p.Provider, p.ChatGPTAccountID, p.ClaudeAccountID, p.RefreshToken)
	cred, err := s.repo.ReplaceByFingerprint(ctx, CreateParams{
		InstallationID:     p.InstallationID,
		ExternalID:         auth.GenerateID(externalIDPrefix),
		UserEmail:          p.UserEmail,
		Provider:           p.Provider,
		AccountLabel:       p.AccountLabel,
		AccountFingerprint: fingerprint,
		ChatGPTAccountID:   p.ChatGPTAccountID,
		AccessToken:        []byte(p.AccessToken),
		RefreshToken:       []byte(p.RefreshToken),
		ExpiresAt:          p.ExpiresAt,
		CreatedBy:          p.CreatedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("replace subscription credential: %w", err)
	}
	s.cache.evict(p.InstallationID, p.UserEmail)
	return cred, nil
}

// List returns every non-deleted credential for the user, including
// refresh-failed ones (surfaced so the user knows to re-enroll).
func (s *Service) List(ctx context.Context, installationID, userEmail string) ([]*Credential, error) {
	return s.repo.ListForUser(ctx, installationID, userEmail)
}

// Remove soft-deletes one credential scoped to the caller's installation and
// email. Returns ErrCredentialNotFound when the id doesn't belong to them.
func (s *Service) Remove(ctx context.Context, installationID, userEmail, id string) error {
	deleted, err := s.repo.SoftDelete(ctx, installationID, userEmail, id)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrCredentialNotFound
	}
	s.cache.evict(installationID, userEmail)
	return nil
}

// SelectCredential returns the first usable pooled credential for the user and
// provider, refreshing its access token when close to expiry. skip lets the
// caller veto candidates (the proxy passes its per-credential exhaustion
// check). Returns nil when the pool is empty or every candidate is skipped or
// unrefreshable this turn.
func (s *Service) SelectCredential(ctx context.Context, installationID, userEmail, provider string, skip func(credentialID string) bool) (*Credential, error) {
	pool, err := s.poolFor(ctx, installationID, userEmail, provider)
	if err != nil {
		return nil, err
	}
	log := observability.FromContext(ctx)
	for _, cred := range pool {
		if skip != nil && skip(cred.ID) {
			continue
		}
		fresh, err := s.freshCredential(ctx, cred)
		if err != nil {
			// Terminal rejections are marked in the repo and drop out of the
			// active pool; transient failures just skip the candidate this turn.
			// Either way the pool walk continues — errors here must not fail the
			// turn while another candidate (or the BYOK fallback) can serve it.
			log.Warn("Skipping pool credential after refresh failure", "credential_id", cred.ID, "provider", provider, "err", err)
			continue
		}
		return fresh, nil
	}
	return nil, nil
}

// PoolExists reports whether the user has at least one active credential for
// provider. Served from the pool cache; used for cheap capability checks
// (provider enrollment, exhaustion-suppression gating) on the hot path.
func (s *Service) PoolExists(ctx context.Context, installationID, userEmail, provider string) bool {
	pool, err := s.poolFor(ctx, installationID, userEmail, provider)
	if err != nil {
		observability.FromContext(ctx).Warn("Failed to load subscription pool for existence check", "err", err)
		return false
	}
	return len(pool) > 0
}

// MarkUsed records a pooled credential serving a turn. Best-effort and off the
// request path.
func (s *Service) MarkUsed(id string) {
	observability.SafeGo(s.log, 2*time.Second, "subscriptionMarkUsed", func(ctx context.Context) {
		if err := s.repo.MarkUsed(ctx, id); err != nil {
			s.log.Warn("Failed to mark subscription credential used", "credential_id", id, "err", err)
		}
	})
}

// poolFor loads the user's active credentials for provider, via the cache.
func (s *Service) poolFor(ctx context.Context, installationID, userEmail, provider string) ([]*Credential, error) {
	all, ok := s.cache.get(installationID, userEmail)
	if !ok {
		var err error
		all, err = s.repo.GetActiveForUser(ctx, installationID, userEmail)
		if err != nil {
			return nil, fmt.Errorf("load subscription pool: %w", err)
		}
		s.cache.set(installationID, userEmail, all)
	}
	pool := make([]*Credential, 0, len(all))
	for _, cred := range all {
		if cred.Provider == provider {
			pool = append(pool, cred)
		}
	}
	return pool, nil
}

// freshCredential returns cred with a valid access token, refreshing (and
// persisting the rotated tokens) when within refreshSkew of expiry.
// Concurrent selections of the same credential coalesce onto one refresh.
func (s *Service) freshCredential(ctx context.Context, cred *Credential) (*Credential, error) {
	if cred.ExpiresAt.IsZero() || s.now().Add(refreshSkew).Before(cred.ExpiresAt) {
		return cred, nil
	}

	s.refreshMu.Lock()
	if call, ok := s.inflight[cred.ID]; ok {
		s.refreshMu.Unlock()
		select {
		case <-call.done:
			return call.cred, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	s.inflight[cred.ID] = call
	s.refreshMu.Unlock()

	call.cred, call.err = s.refreshAndPersist(ctx, cred)
	close(call.done)

	s.refreshMu.Lock()
	delete(s.inflight, cred.ID)
	s.refreshMu.Unlock()

	return call.cred, call.err
}

func (s *Service) refreshAndPersist(ctx context.Context, cred *Credential) (*Credential, error) {
	result, err := s.refresher.Refresh(ctx, cred.Provider, string(cred.RefreshToken))
	if err != nil {
		if errors.Is(err, ErrRefreshRejected) {
			if markErr := s.repo.MarkRefreshFailed(ctx, cred.ID); markErr != nil {
				observability.FromContext(ctx).Error("Failed to mark subscription credential refresh-failed", "credential_id", cred.ID, "err", markErr)
			}
			s.cache.evict(cred.InstallationID, cred.UserEmail)
		}
		return nil, err
	}
	err = s.repo.UpdateTokens(ctx, cred.ID, cred.ExternalID, cred.Provider,
		[]byte(result.AccessToken), []byte(result.RefreshToken), result.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("persist rotated tokens: %w", err)
	}
	s.cache.evict(cred.InstallationID, cred.UserEmail)

	fresh := *cred
	fresh.AccessToken = []byte(result.AccessToken)
	fresh.RefreshToken = []byte(result.RefreshToken)
	fresh.ExpiresAt = result.ExpiresAt
	return &fresh, nil
}

// accountFingerprint derives the stable identity of an enrolled account so a
// re-enrollment of the same account replaces its row rather than duplicating
// it: the ChatGPT account id for Codex, the account uuid for Claude — both
// stable across the token rotations that happen on every fresh OAuth login.
// Falls back to the refresh token only when the provider account id is absent
// (identity of last resort; not stable across re-enrollment).
func accountFingerprint(provider, chatGPTAccountID, claudeAccountID, refreshToken string) string {
	material := refreshToken
	switch provider {
	case providers.ProviderOpenAI:
		if chatGPTAccountID != "" {
			material = chatGPTAccountID
		}
	case providers.ProviderAnthropic:
		if claudeAccountID != "" {
			material = claudeAccountID
		}
	}
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// NormalizeProvider maps accepted provider aliases to the canonical constant,
// returning "" for anything unsupported.
func NormalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providers.ProviderAnthropic, "claude":
		return providers.ProviderAnthropic
	case providers.ProviderOpenAI, "chatgpt", "codex":
		return providers.ProviderOpenAI
	}
	return ""
}
