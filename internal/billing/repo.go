package billing

import "context"

// Repo is the adapter-boundary contract for credit billing reads and writes.
// Implementations live in internal/postgres/billing_repo.go.
type Repo interface {
	// GetBalance returns the org's current balance in USD micros. If no
	// balance row exists for the org, returns ErrBalanceRowMissing — callers
	// distinguish "row missing" from "balance == 0" at the boundary so the
	// log line is unambiguous.
	GetBalance(ctx context.Context, orgID string) (balanceMicros int64, err error)

	// HasActiveOverride reports whether the org currently has an unexpired
	// billing override. Used by middleware to short-circuit balance checks
	// and by the debit hook to pick the delta=0 path.
	HasActiveOverride(ctx context.Context, orgID string) (bool, error)

	// DebitInference performs an atomic UPDATE + INSERT in a single
	// statement: decrement the balance row and append the matching ledger
	// row. When override is active, delta is 0 and the ledger row still
	// records notional_cost_micros for the shadow billing trail. Returns
	// the post-debit balance.
	DebitInference(ctx context.Context, p DebitParams) (balanceAfterMicros int64, err error)

	// BillingTablesExist is a boot-time health check: returns true when
	// all three billing tables exist in the router schema. A missing
	// table means the migration hasn't run; the composition root then
	// disables billing rather than 500ing on every request.
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
}
