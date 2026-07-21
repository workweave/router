package billing

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Spend reservation scope_kind values (match spend_reservations CHECK).
const (
	ScopeOrgMonth  = "org_month"
	ScopeUserMonth = "user_month"
	ScopeAPIKey    = "api_key"
)

// DefaultReserveAmountMicros is the v1 fixed reservation slot ($1) — p95-ish turn cost,
// not exact. Override via ROUTER_SPEND_RESERVE_USD_MICROS.
const DefaultReserveAmountMicros int64 = 1_000_000

// DefaultReserveTTL is request timeout (600s) + 5m grace. Override via
// ROUTER_SPEND_RESERVE_TTL. Keep ≥ messagesTimeout / chatCompletionTimeout.
const DefaultReserveTTL = 15 * time.Minute

// ReserveSpendCapsParams is the input to Repo.ReserveSpendCaps.
type ReserveSpendCapsParams struct {
	OrganizationID  string
	APIKeyID        string // empty skips api-key scope
	RouterUserID    string // empty skips user-month scope
	RouterRequestID string
	AmountUsdMicros int64
	TTL             time.Duration
	// SkipOrg / SkipKey / SkipUser force-skip a scope (e.g. billing override).
	SkipOrg  bool
	SkipKey  bool
	SkipUser bool
}

// SpendHold tracks reservation ids for one request. MarkSettled after a
// successful DebitForInference so the handler defer's ReleaseAll no-ops.
type SpendHold struct {
	IDs     []uuid.UUID
	settled atomic.Bool
}

// MarkSettled records that reservations were consumed by settle (or need not
// be released). Safe to call more than once.
func (h *SpendHold) MarkSettled() {
	if h == nil {
		return
	}
	h.settled.Store(true)
}

// Settled reports whether MarkSettled has been called.
func (h *SpendHold) Settled() bool {
	return h != nil && h.settled.Load()
}

type spendHoldContextKeyT struct{}

// WithSpendHold stashes the request's reservation hold on ctx.
func WithSpendHold(ctx context.Context, h *SpendHold) context.Context {
	return context.WithValue(ctx, spendHoldContextKeyT{}, h)
}

// SpendHoldFrom returns the hold stashed by WithSpendHold, or nil.
func SpendHoldFrom(ctx context.Context) *SpendHold {
	h, _ := ctx.Value(spendHoldContextKeyT{}).(*SpendHold)
	return h
}
