package postgres

import (
	"context"
	"errors"

	"workweave/router/internal/billing"
	"workweave/router/internal/sqlc"

	"github.com/jackc/pgx/v5"
)

// BillingRepo implements billing.Repo against the router-schema credit
// tables via SQLC.
type BillingRepo struct {
	tx sqlc.DBTX
}

// NewBillingRepo constructs a BillingRepo backed by the given connection.
func NewBillingRepo(tx sqlc.DBTX) *BillingRepo {
	return &BillingRepo{tx: tx}
}

var _ billing.Repo = (*BillingRepo)(nil)

// GetBalance returns the org's current credit balance in USD micros.
// Maps pgx.ErrNoRows to billing.ErrBalanceRowMissing so middleware can
// distinguish "row missing" from "balance == 0".
func (r *BillingRepo) GetBalance(ctx context.Context, orgID string) (int64, error) {
	q := sqlc.New(r.tx)
	balance, err := q.GetOrgCreditBalance(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, billing.ErrBalanceRowMissing
		}
		return 0, err
	}
	return balance, nil
}

// HasActiveOverride reports whether the org has an unexpired billing
// override row. EXISTS-based query — true means the org bypasses billing.
func (r *BillingRepo) HasActiveOverride(ctx context.Context, orgID string) (bool, error) {
	q := sqlc.New(r.tx)
	override, err := q.GetActiveBillingOverride(ctx, orgID)
	if err != nil {
		return false, err
	}
	return override, nil
}

// DebitInference performs the atomic UPDATE + INSERT CTE. Returns the
// post-debit balance, or billing.ErrBalanceRowMissing if no balance row
// existed (the CTE returns zero rows in that case).
func (r *BillingRepo) DebitInference(ctx context.Context, p billing.DebitParams) (int64, error) {
	q := sqlc.New(r.tx)
	balanceAfter, err := q.DebitOrgCredits(ctx, sqlc.DebitOrgCreditsParams{
		OrganizationID:     p.OrganizationID,
		DeltaUsdMicros:     p.DeltaUsdMicros,
		NotionalCostMicros: p.NotionalCostMicros,
		EntryType:          p.EntryType,
		RouterRequestID:    stringPtrOrNil(p.RouterRequestID),
		RouterModel:        stringPtrOrNil(p.RouterModel),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, billing.ErrBalanceRowMissing
		}
		return 0, err
	}
	return balanceAfter, nil
}

// BillingTablesExist runs the boot-time health check. Returns true when
// all three billing tables exist in the router schema.
func (r *BillingRepo) BillingTablesExist(ctx context.Context) (bool, error) {
	q := sqlc.New(r.tx)
	return q.CheckBillingTablesExist(ctx)
}
