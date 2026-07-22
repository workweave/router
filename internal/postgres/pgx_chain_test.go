package postgres

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPgxErrNoRowsUnwrapsSql pins the cross-package error chain the proxy
// layer relies on: postgres returns pgx.ErrNoRows; the proxy checks
// errors.Is(err, sql.ErrNoRows). pgx.ErrNoRows is a newProxyErr wrapping
// sql.ErrNoRows, so errors.Is unwraps correctly today — this test fails
// the moment a pgx upgrade changes the wrap behavior.
func TestPgxErrNoRowsUnwrapsSql(t *testing.T) {
	if !errors.Is(pgx.ErrNoRows, sql.ErrNoRows) {
		t.Fatal("pgx.ErrNoRows must be errors.Is-comparable to sql.ErrNoRows so handleRouterFeedbackCommand's no-rows branch fires for real Postgres no-rows errors")
	}
}
