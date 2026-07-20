package billing

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Repo is the adapter-boundary contract for credit billing reads and writes.
// Implementations live in internal/postgres/billing_repo.go.
type Repo interface {
	// GetBalance returns the org's balance in USD micros, or
	// ErrBalanceRowMissing if no row exists (distinct from balance == 0).
	GetBalance(ctx context.Context, orgID string) (balanceMicros int64, err error)

	// HasActiveOverride reports whether the org has an unexpired billing
	// override, used to short-circuit balance checks and pick the delta=0
	// debit path.
	HasActiveOverride(ctx context.Context, orgID string) (bool, error)

	// DebitInference atomically decrements the balance and appends the
	// ledger row. Under an active override, delta is 0 but the ledger row
	// still records notional_cost_micros for the shadow billing trail.
	// When ReservationIDs is non-empty, those reservations are settled
	// (DELETE … RETURNING + reserved decrement) in the same transaction.
	DebitInference(ctx context.Context, p DebitParams) (balanceAfterMicros int64, err error)

	// GetAPIKeySpend reads a key's spend cap and spend-to-date fresh from
	// Postgres, bypassing the auth cache. found is false if the key was
	// deleted mid-request, treated as "no cap to enforce".
	GetAPIKeySpend(ctx context.Context, apiKeyID string) (spentMicros, reservedMicros int64, capMicros *int64, found bool, err error)

	// GetUserMonthlySpendAndLimit returns the engineer's current UTC-month spend
	// (and in-flight reserved) and effective monthly limit: per-user override
	// when set (NULL = explicitly uncapped), else org default.
	GetUserMonthlySpendAndLimit(ctx context.Context, organizationID, routerUserID string) (spentMicros, reservedMicros int64, limitMicros *int64, err error)

	// GetOrgMonthlySpendAndLimit returns the org's current UTC-month spend
	// (and in-flight reserved) and its configured monthly cap. nil limitMicros
	// means no cap is set.
	GetOrgMonthlySpendAndLimit(ctx context.Context, organizationID string) (spentMicros, reservedMicros int64, limitMicros *int64, err error)

	// ReserveSpendCaps atomically reserves every applicable scope in one TX;
	// any scope failure rolls back the whole TX (no partial holds).
	ReserveSpendCaps(ctx context.Context, p ReserveSpendCapsParams) ([]uuid.UUID, error)

	// ReleaseSpendReservations deletes reservation ids and decrements denormalized
	// reserved. Idempotent: already-gone ids are no-ops.
	ReleaseSpendReservations(ctx context.Context, ids []uuid.UUID) error

	// SweepExpiredSpendReservations deletes expired reservation rows and
	// decrements reserved counters for each DELETE … RETURNING hit.
	SweepExpiredSpendReservations(ctx context.Context, now time.Time) (released int, err error)

	// GetAutopayConfig reports the org's autopay state and recharge
	// threshold. A missing config row returns enabled=false, nil error
	// ("autopay off"), not an error.
	GetAutopayConfig(ctx context.Context, orgID string) (enabled bool, thresholdMicros int64, err error)

	// BillingTablesExist is a boot-time check for the three billing tables.
	// A missing table means the migration hasn't run yet.
	BillingTablesExist(ctx context.Context) (bool, error)
}

// DebitParams is the input to Repo.DebitInference. Money is in USD micros.
type DebitParams struct {
	OrganizationID     string
	DeltaUsdMicros     int64  // signed: negative for real debit, 0 for override pass-through
	NotionalCostMicros int64  // always the would-be charge, populated regardless of override
	EntryType          string // 'inference', 'adjustment', etc.
	RouterRequestID    string // upstream call id; suffix ('_summary','_main') used for handover rows
	RouterModel        string
	// APIKeyID, if non-empty, attributes the debit to that key, bumping its
	// lifetime spent_usd_micros in the same transaction.
	APIKeyID string
	// RouterUserID, if non-empty, attributes the debit to that engineer and bumps
	// the org's monthly counter; org counter is always bumped regardless.
	RouterUserID string
	// ReservationIDs, when non-empty, are settled in the same transaction as
	// the debit (DELETE … RETURNING + reserved decrement).
	ReservationIDs []uuid.UUID
}
