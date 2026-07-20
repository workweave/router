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

// ErrUserMonthlySpendLimitReached is returned when the engineer's current
// UTC-month spend meets their effective monthly limit. Mapped to HTTP 402.
var ErrUserMonthlySpendLimitReached = errors.New("billing: engineer monthly spend limit reached")

// ErrOrgMonthlySpendLimitReached is returned when the org's current UTC-month
// spend (including in-flight reservations) cannot fit another reserve slot.
// Mapped to HTTP 402.
var ErrOrgMonthlySpendLimitReached = errors.New("billing: organization monthly spend limit reached")

// ErrAPIKeySpendCapReached is returned when a key's lifetime spend (including
// in-flight reservations) cannot fit another reserve slot. Mapped to HTTP 402.
var ErrAPIKeySpendCapReached = errors.New("billing: api key spend cap reached")

// ErrSpendLimitCheckUnavailable is returned on a repo failure reading spend
// limits; gates fail closed (HTTP 503) to prevent unbounded-spend windows.
var ErrSpendLimitCheckUnavailable = errors.New("billing: spend limit check unavailable")
