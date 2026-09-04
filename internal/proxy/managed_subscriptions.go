package proxy

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/billing"
	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/subscriptions"
)

// ManagedSubscriptionProvidersContextKey carries provider pools enrolled for
// the authenticated Router key. Values describe enrollment, not availability,
// so disabled or cooling accounts still fail closed instead of spending credits.
type ManagedSubscriptionProvidersContextKey struct{}

// ManagedSubscriptionEnrollmentUnavailableContextKey marks a request whose
// enrollment snapshot could not be loaded. Control-plane handlers remain
// available, while inference fails closed before any paid upstream dispatch.
type ManagedSubscriptionEnrollmentUnavailableContextKey struct{}

// ManagedSubscriptionUsageContextKey carries per-request billing attribution.
type ManagedSubscriptionUsageContextKey struct{}

// ManagedSubscriptionUsage is request-local attribution shared by the auth
// middleware context and provider-specific dispatch attempt contexts.
type ManagedSubscriptionUsage struct {
	Served bool
}

var (
	ErrSubscriptionPoolExhausted   = errors.New("subscription account pool exhausted")
	ErrSubscriptionPoolUnavailable = errors.New("subscription account pool unavailable")
)

func isSubscriptionPoolError(err error) bool {
	return errors.Is(err, ErrSubscriptionPoolExhausted) || errors.Is(err, ErrSubscriptionPoolUnavailable)
}

// WithManagedSubscriptionUsage prepares request-local subscription attribution.
func WithManagedSubscriptionUsage(ctx context.Context) context.Context {
	return context.WithValue(ctx, ManagedSubscriptionUsageContextKey{}, &ManagedSubscriptionUsage{})
}

func managedSubscriptionProviderFromUpstream(provider, model string) (subscriptions.Provider, bool) {
	switch provider {
	case providers.ProviderAnthropic:
		return subscriptions.ProviderClaude, true
	case providers.ProviderOpenAI:
		if codexSubscriptionCoversModel(model) {
			return subscriptions.ProviderCodex, true
		}
	}
	return "", false
}

func managedSubscriptionEnrolled(ctx context.Context, provider subscriptions.Provider) bool {
	if subscriptionRoutingDisabledForRequest(ctx) {
		return false
	}
	enrolled, _ := ctx.Value(ManagedSubscriptionProvidersContextKey{}).(map[auth.SubscriptionProvider]struct{})
	_, ok := enrolled[auth.SubscriptionProvider(provider)]
	return ok
}

func managedSubscriptionCanServe(ctx context.Context, provider, model string) bool {
	poolProvider, eligible := managedSubscriptionProviderFromUpstream(provider, model)
	return eligible && managedSubscriptionEnrolled(ctx, poolProvider)
}

func managedSubscriptionEnrollmentUnavailable(ctx context.Context) bool {
	unavailable, _ := ctx.Value(ManagedSubscriptionEnrollmentUnavailableContextKey{}).(bool)
	return unavailable
}

func markManagedSubscriptionServed(ctx context.Context) {
	usage, _ := ctx.Value(ManagedSubscriptionUsageContextKey{}).(*ManagedSubscriptionUsage)
	if usage != nil {
		usage.Served = true
	}
}

func managedSubscriptionServed(ctx context.Context) bool {
	usage, _ := ctx.Value(ManagedSubscriptionUsageContextKey{}).(*ManagedSubscriptionUsage)
	return usage != nil && usage.Served
}

func (s *Service) leaseManagedSubscription(ctx context.Context, provider, model string) (context.Context, subscriptions.Lease, bool, error) {
	poolProvider, eligible := managedSubscriptionProviderFromUpstream(provider, model)
	if !eligible || s.managedSubscriptions == nil {
		return ctx, subscriptions.Lease{}, false, nil
	}
	if managedSubscriptionEnrollmentUnavailable(ctx) {
		return ctx, subscriptions.Lease{}, true, ErrSubscriptionPoolUnavailable
	}
	currentCredentials := CredentialsFromContext(ctx)
	if !managedSubscriptionEnrolled(ctx, poolProvider) || (currentCredentials != nil && currentCredentials.OAuth) {
		return ctx, subscriptions.Lease{}, false, nil
	}
	if subscriptionPlanAwareRoutingEnabled(ctx) && managedSubscriptionPlansAllExhausted(ctx) {
		if billing.SubscriptionOnlyFromContext(ctx) {
			return ctx, subscriptions.Lease{}, true, ErrSubscriptionPoolExhausted
		}
		return ctx, subscriptions.Lease{}, false, nil
	}
	lease, present, err := s.managedSubscriptions.Lease(ctx, apiKeyIDFromContext(ctx), poolProvider, ClientIdentityFrom(ctx).SessionID)
	if err != nil {
		if errors.Is(err, subscriptions.ErrNoAvailableAccount) {
			return ctx, subscriptions.Lease{}, true, ErrSubscriptionPoolExhausted
		}
		return ctx, subscriptions.Lease{}, present, errors.Join(ErrSubscriptionPoolUnavailable, err)
	}
	if !present {
		return ctx, subscriptions.Lease{}, true, ErrSubscriptionPoolExhausted
	}
	creds := &Credentials{APIKey: []byte(lease.AccessToken), OAuth: true, Source: credSourceSubscription}
	if poolProvider == subscriptions.ProviderCodex {
		creds.Source = credSourceCodexSubscription
		creds.AccountID = []byte(lease.ProviderAccount)
	}
	return context.WithValue(ctx, CredentialsContextKey{}, creds), lease, true, nil
}

func (s *Service) recordManagedSubscriptionFailure(ctx context.Context, provider, model string, lease subscriptions.Lease, attemptErr error) bool {
	poolProvider, eligible := managedSubscriptionProviderFromUpstream(provider, model)
	if !eligible || lease.AccountID == "" {
		return false
	}
	status := upstreamStatus(attemptErr)
	switch status {
	case http.StatusTooManyRequests:
		resetAt := managedSubscriptionResetAt(attemptErr, s.clockNow())
		if err := s.managedSubscriptions.Cooldown(ctx, apiKeyIDFromContext(ctx), poolProvider, lease.AccountID, resetAt); err != nil {
			observability.FromContext(ctx).Error("Failed to persist subscription account cooldown",
				"provider", poolProvider, "account_id", lease.AccountID, "err", err)
		}
		observability.FromContext(ctx).Warn("Subscription account quota exhausted",
			"provider", poolProvider, "account_id", lease.AccountID, "cooldown_until", resetAt)
		return true
	case http.StatusUnauthorized, http.StatusForbidden:
		if err := s.managedSubscriptions.Disable(ctx, apiKeyIDFromContext(ctx), poolProvider, lease.AccountID); err != nil {
			observability.FromContext(ctx).Error("Failed to disable rejected subscription account",
				"provider", poolProvider, "account_id", lease.AccountID, "err", err)
		}
		observability.FromContext(ctx).Warn("Subscription account credential rejected",
			"provider", poolProvider, "account_id", lease.AccountID, "upstream_status", status)
		return true
	default:
		return false
	}
}

func managedSubscriptionResetAt(err error, now time.Time) time.Time {
	var upstream *providers.UpstreamErrorResponse
	if !errors.As(err, &upstream) {
		return now.Add(time.Minute)
	}
	if retryAfter := upstream.Headers.Get("Retry-After"); retryAfter != "" {
		if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil && seconds > 0 {
			return now.Add(time.Duration(seconds) * time.Second)
		}
		if resetAt, parseErr := http.ParseTime(retryAfter); parseErr == nil && resetAt.After(now) {
			return resetAt
		}
	}
	return now.Add(time.Minute)
}
