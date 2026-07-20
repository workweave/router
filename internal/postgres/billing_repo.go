package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"workweave/router/internal/billing"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BillingRepo implements billing.Repo against the router-schema credit
// tables via SQLC.
type BillingRepo struct {
	pool *pgxpool.Pool
}

// NewBillingRepo constructs a BillingRepo backed by the given pool.
func NewBillingRepo(pool *pgxpool.Pool) *BillingRepo {
	return &BillingRepo{pool: pool}
}

var _ billing.Repo = (*BillingRepo)(nil)

func (r *BillingRepo) q() *sqlc.Queries { return sqlc.New(r.pool) }

// GetBalance returns the org's current credit balance in USD micros.
// Maps pgx.ErrNoRows to billing.ErrBalanceRowMissing so middleware can
// distinguish "row missing" from "balance == 0".
func (r *BillingRepo) GetBalance(ctx context.Context, orgID string) (int64, error) {
	balance, err := r.q().GetOrgCreditBalance(ctx, orgID)
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
	return r.q().GetActiveBillingOverride(ctx, orgID)
}

// DebitInference performs the atomic UPDATE + INSERT CTE, then settles any
// reservation ids in the same transaction.
func (r *BillingRepo) DebitInference(ctx context.Context, p billing.DebitParams) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlc.New(tx)
	balanceAfter, err := q.DebitOrgCredits(ctx, sqlc.DebitOrgCreditsParams{
		OrganizationID:     p.OrganizationID,
		DeltaUsdMicros:     p.DeltaUsdMicros,
		NotionalCostMicros: p.NotionalCostMicros,
		EntryType:          p.EntryType,
		RouterRequestID:    stringPtrOrNil(p.RouterRequestID),
		RouterModel:        stringPtrOrNil(p.RouterModel),
		APIKeyID:           uuidOrNil(p.APIKeyID),
		RouterUserID:       uuidOrNil(p.RouterUserID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, billing.ErrBalanceRowMissing
		}
		return 0, err
	}
	for _, id := range p.ReservationIDs {
		if err := consumeSpendReservation(ctx, q, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return balanceAfter, nil
}

// GetAPIKeySpend reads a key's cap and spend-to-date fresh from Postgres.
// Returns found=false (nil error) when no active key matches the id.
func (r *BillingRepo) GetAPIKeySpend(ctx context.Context, apiKeyID string) (int64, int64, *int64, bool, error) {
	parsed, err := uuid.Parse(apiKeyID)
	if err != nil {
		return 0, 0, nil, false, nil
	}
	row, err := r.q().GetModelRouterAPIKeySpend(ctx, parsed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, nil, false, nil
		}
		return 0, 0, nil, false, err
	}
	return row.SpentUsdMicros, row.ReservedUsdMicros, row.SpendCapUsdMicros, true, nil
}

// GetUserMonthlySpendAndLimit resolves the effective monthly limit
// (per-user override > org default; NULL override = explicitly uncapped).
func (r *BillingRepo) GetUserMonthlySpendAndLimit(ctx context.Context, organizationID, routerUserID string) (int64, int64, *int64, error) {
	parsed, err := uuid.Parse(routerUserID)
	if err != nil {
		return 0, 0, nil, nil
	}
	row, err := r.q().GetUserMonthlySpendAndLimit(ctx, sqlc.GetUserMonthlySpendAndLimitParams{
		RouterUserID:   parsed,
		OrganizationID: organizationID,
	})
	if err != nil {
		return 0, 0, nil, err
	}
	limit := row.OrgDefaultLimitUsdMicros
	if row.HasOverride {
		limit = row.OverrideLimitUsdMicros
	}
	return row.SpentUsdMicros, row.ReservedUsdMicros, limit, nil
}

// GetOrgMonthlySpendAndLimit reads the org's current UTC-month spend and cap.
func (r *BillingRepo) GetOrgMonthlySpendAndLimit(ctx context.Context, organizationID string) (int64, int64, *int64, error) {
	row, err := r.q().GetOrgMonthlySpendAndLimit(ctx, organizationID)
	if err != nil {
		return 0, 0, nil, err
	}
	return row.SpentUsdMicros, row.ReservedUsdMicros, row.OrgLimitUsdMicros, nil
}

// GetAutopayConfig reads the org's autopay enabled flag and recharge threshold.
func (r *BillingRepo) GetAutopayConfig(ctx context.Context, orgID string) (bool, int64, error) {
	row, err := r.q().GetAutopayConfig(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, nil
		}
		return false, 0, err
	}
	return row.Enabled, row.ThresholdUsdMicros, nil
}

// BillingTablesExist runs the boot-time health check.
func (r *BillingRepo) BillingTablesExist(ctx context.Context) (bool, error) {
	return r.q().CheckBillingTablesExist(ctx)
}

// ReserveSpendCaps reserves all applicable scopes in one transaction.
func (r *BillingRepo) ReserveSpendCaps(ctx context.Context, p billing.ReserveSpendCapsParams) ([]uuid.UUID, error) {
	if p.AmountUsdMicros <= 0 {
		return nil, fmt.Errorf("billing: reserve amount must be positive")
	}
	if p.TTL <= 0 {
		return nil, fmt.Errorf("billing: reserve TTL must be positive")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlc.New(tx)

	expires := time.Now().UTC().Add(p.TTL)
	expiresTS := pgtype.Timestamptz{Time: expires, Valid: true}
	month := utcMonthDate()
	var ids []uuid.UUID

	if !p.SkipOrg && p.OrganizationID != "" {
		_, _, limit, err := r.getOrgMonthlyInTx(ctx, q, p.OrganizationID)
		if err != nil {
			return nil, err
		}
		if limit != nil {
			if err := q.EnsureOrgMonthlySpendRow(ctx, sqlc.EnsureOrgMonthlySpendRowParams{
				OrganizationID: p.OrganizationID,
				Month:          month,
			}); err != nil {
				return nil, err
			}
			_, err := q.TryBumpOrgMonthReserved(ctx, sqlc.TryBumpOrgMonthReservedParams{
				AmountUsdMicros: p.AmountUsdMicros,
				OrganizationID:  p.OrganizationID,
				Month:           month,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, billing.ErrOrgMonthlySpendLimitReached
				}
				return nil, err
			}
			row, err := q.InsertSpendReservation(ctx, sqlc.InsertSpendReservationParams{
				ScopeKind:       billing.ScopeOrgMonth,
				ScopeID:         p.OrganizationID,
				Month:           month,
				AmountUsdMicros: p.AmountUsdMicros,
				ExpiresAt:       expiresTS,
				RouterRequestID: stringPtrOrNil(p.RouterRequestID),
			})
			if err != nil {
				return nil, err
			}
			ids = append(ids, row.ID)
		}
	}

	if !p.SkipKey && p.APIKeyID != "" {
		keyUUID, err := uuid.Parse(p.APIKeyID)
		if err != nil {
			return nil, fmt.Errorf("billing: invalid api_key_id: %w", err)
		}
		spentRow, err := q.GetModelRouterAPIKeySpend(ctx, keyUUID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil && spentRow.SpendCapUsdMicros != nil {
			_, err := q.TryBumpAPIKeyReserved(ctx, sqlc.TryBumpAPIKeyReservedParams{
				AmountUsdMicros: p.AmountUsdMicros,
				APIKeyID:        keyUUID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, billing.ErrAPIKeySpendCapReached
				}
				return nil, err
			}
			row, err := q.InsertSpendReservation(ctx, sqlc.InsertSpendReservationParams{
				ScopeKind:       billing.ScopeAPIKey,
				ScopeID:         p.APIKeyID,
				Month:           pgtype.Date{}, // NULL for lifetime
				AmountUsdMicros: p.AmountUsdMicros,
				ExpiresAt:       expiresTS,
				RouterRequestID: stringPtrOrNil(p.RouterRequestID),
			})
			if err != nil {
				return nil, err
			}
			ids = append(ids, row.ID)
		}
	}

	if !p.SkipUser && p.RouterUserID != "" && p.OrganizationID != "" {
		userUUID, err := uuid.Parse(p.RouterUserID)
		if err != nil {
			return nil, fmt.Errorf("billing: invalid router_user_id: %w", err)
		}
		_, _, limit, err := r.getUserMonthlyInTx(ctx, q, p.OrganizationID, userUUID)
		if err != nil {
			return nil, err
		}
		if limit != nil {
			if err := q.EnsureUserMonthlySpendRow(ctx, sqlc.EnsureUserMonthlySpendRowParams{
				RouterUserID: userUUID,
				Month:        month,
			}); err != nil {
				return nil, err
			}
			_, err := q.TryBumpUserMonthReserved(ctx, sqlc.TryBumpUserMonthReservedParams{
				AmountUsdMicros: p.AmountUsdMicros,
				RouterUserID:    userUUID,
				Month:           month,
				LimitUsdMicros:  *limit,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, billing.ErrUserMonthlySpendLimitReached
				}
				return nil, err
			}
			row, err := q.InsertSpendReservation(ctx, sqlc.InsertSpendReservationParams{
				ScopeKind:       billing.ScopeUserMonth,
				ScopeID:         p.RouterUserID,
				Month:           month,
				AmountUsdMicros: p.AmountUsdMicros,
				ExpiresAt:       expiresTS,
				RouterRequestID: stringPtrOrNil(p.RouterRequestID),
			})
			if err != nil {
				return nil, err
			}
			ids = append(ids, row.ID)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *BillingRepo) getOrgMonthlyInTx(ctx context.Context, q *sqlc.Queries, orgID string) (int64, int64, *int64, error) {
	row, err := q.GetOrgMonthlySpendAndLimit(ctx, orgID)
	if err != nil {
		return 0, 0, nil, err
	}
	return row.SpentUsdMicros, row.ReservedUsdMicros, row.OrgLimitUsdMicros, nil
}

func (r *BillingRepo) getUserMonthlyInTx(ctx context.Context, q *sqlc.Queries, orgID string, userID uuid.UUID) (int64, int64, *int64, error) {
	row, err := q.GetUserMonthlySpendAndLimit(ctx, sqlc.GetUserMonthlySpendAndLimitParams{
		RouterUserID:   userID,
		OrganizationID: orgID,
	})
	if err != nil {
		return 0, 0, nil, err
	}
	limit := row.OrgDefaultLimitUsdMicros
	if row.HasOverride {
		limit = row.OverrideLimitUsdMicros
	}
	return row.SpentUsdMicros, row.ReservedUsdMicros, limit, nil
}

// ReleaseSpendReservations consumes ids via DELETE … RETURNING.
func (r *BillingRepo) ReleaseSpendReservations(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlc.New(tx)
	for _, id := range ids {
		if err := consumeSpendReservation(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SweepExpiredSpendReservations deletes expired rows and decrements reserved.
func (r *BillingRepo) SweepExpiredSpendReservations(ctx context.Context, now time.Time) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlc.New(tx)
	rows, err := q.DeleteExpiredSpendReservations(ctx, pgtype.Timestamptz{Time: now.UTC(), Valid: true})
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if err := decrementReservedForScope(ctx, q, row.ScopeKind, row.ScopeID, row.Month, row.AmountUsdMicros); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// consumeSpendReservation is the shared DELETE … RETURNING + conditional
// reserved decrement used by settle, release, and (per-id) callers.
func consumeSpendReservation(ctx context.Context, q *sqlc.Queries, id uuid.UUID) error {
	row, err := q.DeleteSpendReservation(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	return decrementReservedForScope(ctx, q, row.ScopeKind, row.ScopeID, row.Month, row.AmountUsdMicros)
}

func decrementReservedForScope(ctx context.Context, q *sqlc.Queries, scopeKind, scopeID string, month pgtype.Date, amount int64) error {
	switch scopeKind {
	case billing.ScopeOrgMonth:
		return q.DecrementOrgMonthReserved(ctx, sqlc.DecrementOrgMonthReservedParams{
			AmountUsdMicros: amount,
			OrganizationID:  scopeID,
			Month:           month,
		})
	case billing.ScopeUserMonth:
		uid, err := uuid.Parse(scopeID)
		if err != nil {
			return fmt.Errorf("billing: sweep user scope_id: %w", err)
		}
		return q.DecrementUserMonthReserved(ctx, sqlc.DecrementUserMonthReservedParams{
			AmountUsdMicros: amount,
			RouterUserID:    uid,
			Month:           month,
		})
	case billing.ScopeAPIKey:
		uid, err := uuid.Parse(scopeID)
		if err != nil {
			return fmt.Errorf("billing: sweep api_key scope_id: %w", err)
		}
		return q.DecrementAPIKeyReserved(ctx, sqlc.DecrementAPIKeyReservedParams{
			AmountUsdMicros: amount,
			APIKeyID:        uid,
		})
	default:
		return fmt.Errorf("billing: unknown spend reservation scope_kind %q", scopeKind)
	}
}

// utcNow is the clock for reservation month bucketing. Tests override it to
// prove Ensure/Bump/Insert share one Go-computed month even when Postgres
// NOW() would land in a different UTC month.
var utcNow = time.Now

func utcMonthDate() pgtype.Date {
	now := utcNow().UTC()
	return pgtype.Date{Time: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), Valid: true}
}
