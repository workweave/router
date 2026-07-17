package billing

import "context"

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
	DebitInference(ctx context.Context, p DebitParams) (balanceAfterMicros int64, err error)

	// GetAPIKeySpend reads a key's spend cap and spend-to-date fresh from
	// Postgres, bypassing the auth cache. found is false if the key was
	// deleted mid-request, treated as "no cap to enforce".
	GetAPIKeySpend(ctx context.Context, apiKeyID string) (spentMicros int64, capMicros *int64, found bool, err error)

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
}
