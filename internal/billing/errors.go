// Package billing implements the inner-ring credit-billing domain for the
// managed router: balance check, override resolution, and atomic
// debit-with-ledger. Pure orchestration over a Repo interface; the Postgres
// adapter lives in internal/postgres/billing_repo.go.
package billing

import "errors"

// ErrInsufficientCredits is returned by Service.CheckBalance when the org's
// balance is at or below the configured minimum threshold. Middleware maps
// this to HTTP 402.
var ErrInsufficientCredits = errors.New("billing: insufficient credits")

// ErrBalanceRowMissing is returned when the org has no balance row. Treated
// as 402 by middleware — same shape, different log message — so a missing
// row can be distinguished from a depleted balance in the operator log
// without changing the customer response.
var ErrBalanceRowMissing = errors.New("billing: balance row missing")
