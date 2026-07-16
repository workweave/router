package proxy

import (
	"context"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy/usage"
	"workweave/router/internal/subscriptions"

	"github.com/google/uuid"
)

// SubscriptionPoolSource is the seam the proxy needs onto the per-user
// subscription pool: select the next usable credential (refreshing tokens as
// needed), test for pool presence cheaply, and record usage. Implemented by
// *subscriptions.Service; nil disables pooled rotation.
type SubscriptionPoolSource interface {
	// SelectCredential returns the first usable pooled credential for the user
	// and provider, skipping any the caller vetoes via skip. Nil when the pool
	// is empty or every candidate is skipped/unrefreshable this turn.
	SelectCredential(ctx context.Context, installationID, userEmail, provider string, skip func(credentialID string) bool) (*subscriptions.Credential, error)
	// PoolExists reports whether the user has at least one active credential for
	// provider (cache-served; cheap enough for the hot path).
	PoolExists(ctx context.Context, installationID, userEmail, provider string) bool
	// HasUsableCredential reports whether the user has an active credential for
	// provider that skip does not veto. Side-effect free (no token refresh, no
	// refresh-failed marking), unlike SelectCredential — safe for a probe.
	HasUsableCredential(ctx context.Context, installationID, userEmail, provider string, skip func(credentialID string) bool) bool
	// MarkUsed records a pooled credential serving a turn (best-effort).
	MarkUsed(id string)
}

// WithSubscriptionPool wires the per-user subscription credential pool.
// Left unset, pooled rotation is off (inbound sub -> BYOK -> deployment key).
func (s *Service) WithSubscriptionPool(pool SubscriptionPoolSource) *Service {
	s.subscriptionPool = pool
	return s
}

// poolObserverKey namespaces a pooled credential's usage-observer key by its
// stable row UUID, so exhaustion snapshots survive token refresh (the token
// itself rotates, so keying by the token would orphan the reading). The "pool:"
// prefix can't collide with a raw-token key.
func (s *Service) poolObserverKey(credentialID string) usage.CredentialKey {
	return s.usageObserver.Key([]byte("pool:" + credentialID))
}

// pooledCredentialFor returns the next usable pooled credential for the
// request's user and provider. DB errors never fail the turn: they log and
// return nil so resolution falls through to BYOK/deployment key.
func (s *Service) pooledCredentialFor(ctx context.Context, provider string) *Credentials {
	if s.subscriptionPool == nil {
		return nil
	}
	installationID := installationIDFromContext(ctx)
	if installationID == (uuid.UUID{}) {
		return nil
	}
	email := ClientIdentityFrom(ctx).Email
	if email == "" {
		return nil
	}

	cred, err := s.subscriptionPool.SelectCredential(ctx, installationID.String(), email, provider, s.poolExhaustionSkip())
	if err != nil {
		observability.FromContext(ctx).Warn("Subscription pool selection failed; falling through to BYOK/deployment key",
			"provider", provider, "err", err)
		return nil
	}
	if cred == nil {
		return nil
	}

	source := credSourcePooledSubscription
	var accountID []byte
	if provider == providers.ProviderOpenAI {
		source = credSourcePooledCodexSubscription
		accountID = []byte(cred.ChatGPTAccountID)
	}
	s.subscriptionPool.MarkUsed(cred.ID)
	observability.FromContext(ctx).Info("Resolved pooled subscription credential",
		"credential_source", source, "pool_credential_id", cred.ID)
	return &Credentials{
		APIKey:           cred.AccessToken,
		AccountID:        accountID,
		Source:           source,
		OAuth:            true,
		PoolCredentialID: cred.ID,
	}
}

// recordPoolExhaustionIfPooled marks the resolved credential exhausted in
// the usage observer so a same-turn failover retry skips it. The synthetic
// snapshot uses a 5h window; real headroom overwrites it on the next response.
func (s *Service) recordPoolExhaustionIfPooled(ctx context.Context) {
	if s.usageObserver == nil {
		return
	}
	creds := CredentialsFromContext(ctx)
	if creds == nil || creds.PoolCredentialID == "" {
		return
	}
	s.usageObserver.Record(s.poolObserverKey(creds.PoolCredentialID), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 1.0, WindowMinutes: 300},
	})
}

// poolExhaustionSkip returns the veto predicate SelectCredential uses to pass
// over pooled credentials the usage observer has recorded as exhausted, so
// rotation advances past a spent account. Always false when the observer is
// unwired (no exhaustion signal to act on).
func (s *Service) poolExhaustionSkip() func(credentialID string) bool {
	return func(credentialID string) bool {
		if s.usageObserver == nil {
			return false
		}
		snap, ok := s.usageObserver.Snapshot(s.poolObserverKey(credentialID))
		return ok && snap.Exhausted()
	}
}

// poolHasCandidate reports whether the request's user has any active pooled
// credential for provider. Cache-served; used to enroll the provider for
// routing.
func (s *Service) poolHasCandidate(ctx context.Context, provider string) bool {
	if s.subscriptionPool == nil {
		return false
	}
	installationID := installationIDFromContext(ctx)
	if installationID == (uuid.UUID{}) {
		return false
	}
	email := ClientIdentityFrom(ctx).Email
	if email == "" {
		return false
	}
	return s.subscriptionPool.PoolExists(ctx, installationID.String(), email, provider)
}

// poolHasUsableCandidate reports whether the user has a pooled credential for
// provider that isn't already exhausted this turn. Unlike poolHasCandidate
// (which counts any active row), it applies the same exhaustion veto as
// selection, so an all-spent pool can't masquerade as a usable fallback and
// wrongly suppress the inbound subscription (leaving the turn with no
// credential instead of preserving the upstream 429).
func (s *Service) poolHasUsableCandidate(ctx context.Context, provider string) bool {
	if s.subscriptionPool == nil {
		return false
	}
	installationID := installationIDFromContext(ctx)
	if installationID == (uuid.UUID{}) {
		return false
	}
	email := ClientIdentityFrom(ctx).Email
	if email == "" {
		return false
	}
	return s.subscriptionPool.HasUsableCredential(ctx, installationID.String(), email, provider, s.poolExhaustionSkip())
}
