package postgres

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPgxErrNoRowsUnwrapsSql pins the pgx->sql error-chain that the proxy's
// no-rows detection relies on. pgx.ErrNoRows wraps sql.ErrNoRows; if a pgx
// upgrade changes that, this test fails before prod does.
func TestPgxErrNoRowsUnwrapsSql(t *testing.T) {
	if !errors.Is(pgx.ErrNoRows, sql.ErrNoRows) {
		t.Fatal("pgx.ErrNoRows must be errors.Is-comparable to sql.ErrNoRows so handleRouterFeedbackCommand's no-rows branch fires for real Postgres no-rows errors")
	}
}
