package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/billing"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubBillingRepo is the minimum surface of billing.Repo needed to drive
// middleware.WithBalanceCheck through every branch. Each scenario flips
// the two fields; the debit and existence methods are unused here.
type stubBillingRepo struct {
	balance     int64
	override    bool
	balanceErr  error
	overrideErr error
}

func (r *stubBillingRepo) GetBalance(_ context.Context, _ string) (int64, error) {
	return r.balance, r.balanceErr
}
func (r *stubBillingRepo) HasActiveOverride(_ context.Context, _ string) (bool, error) {
	return r.override, r.overrideErr
}
func (r *stubBillingRepo) DebitInference(_ context.Context, _ billing.DebitParams) (int64, error) {
	return 0, nil
}
func (r *stubBillingRepo) BillingTablesExist(_ context.Context) (bool, error) {
	return true, nil
}

func withInstallation(c *gin.Context, externalID string) {
	c.Set("router_installation", &auth.Installation{
		ID:         "00000000-0000-0000-0000-000000000001",
		ExternalID: externalID,
	})
}

func runMiddleware(t *testing.T, repo billing.Repo, threshold int64, externalID string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := billing.NewService(repo)
	engine := gin.New()
	reached := false
	engine.GET("/probe", func(c *gin.Context) {
		// Synthetic auth: pre-stash the installation the way WithAuth would.
		withInstallation(c, externalID)
		middleware.WithBalanceCheck(svc, threshold)(c)
		if c.IsAborted() {
			return
		}
		reached = true
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	engine.ServeHTTP(w, req)
	return w, reached
}

func TestWithBalanceCheck_402WhenBelowThreshold(t *testing.T) {
	repo := &stubBillingRepo{balance: 999_999} // just under $1
	w, reached := runMiddleware(t, repo, 1_000_000, "org_x")
	assert.False(t, reached, "handler must not be reached when balance is below threshold")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)

	var body struct {
		Error            string `json:"error"`
		TopUpURL         string `json:"top_up_url"`
		BalanceUSDMicros int64  `json:"balance_usd_micros"`
		Message          string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "insufficient_credits", body.Error)
	assert.Equal(t, middleware.TopUpURL, body.TopUpURL)
	assert.Equal(t, int64(999_999), body.BalanceUSDMicros, "402 body echoes the actual balance for client UX")
	assert.NotEmpty(t, body.Message)
}

func TestWithBalanceCheck_402WhenBalanceRowMissing(t *testing.T) {
	repo := &stubBillingRepo{balanceErr: billing.ErrBalanceRowMissing}
	w, reached := runMiddleware(t, repo, 1_000_000, "org_x")
	assert.False(t, reached)
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestWithBalanceCheck_PassesWhenAboveThreshold(t *testing.T) {
	repo := &stubBillingRepo{balance: 50_000_000} // $50
	_, reached := runMiddleware(t, repo, 1_000_000, "org_x")
	assert.True(t, reached, "request must reach handler when balance is healthy")
}

func TestWithBalanceCheck_OverrideShortCircuitsAndFlagsContext(t *testing.T) {
	// Override path must (a) reach the handler, (b) plant the override
	// flag on the request context so the proxy's debit hook records a
	// delta=0 ledger row downstream.
	repo := &stubBillingRepo{override: true}
	gin.SetMode(gin.TestMode)
	svc := billing.NewService(repo)
	engine := gin.New()
	var hasOverride bool
	engine.GET("/probe", func(c *gin.Context) {
		withInstallation(c, "org_internal")
		middleware.WithBalanceCheck(svc, 1_000_000)(c)
		if c.IsAborted() {
			return
		}
		hasOverride = billing.HasOverrideFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, hasOverride, "override flag must reach the request context")
}

func TestWithBalanceCheck_FailOpenOnRepoError(t *testing.T) {
	// A transient DB error reading the balance must NOT blackhole inference.
	// We log Error and let the request through; the debit hook will likely
	// also fail and surface in metrics. Better than 500ing every customer
	// on a transient ClickHouse-style hiccup.
	repo := &stubBillingRepo{balanceErr: errors.New("conn refused")}
	w, reached := runMiddleware(t, repo, 1_000_000, "org_x")
	assert.True(t, reached, "transient DB error must not block inference")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithBalanceCheck_SkipsWhenInstallationMissing(t *testing.T) {
	// Synthetic / unauthed request — should never happen because WithAuth
	// runs first, but the middleware must not panic. Pass-through.
	gin.SetMode(gin.TestMode)
	repo := &stubBillingRepo{}
	svc := billing.NewService(repo)
	engine := gin.New()
	reached := false
	engine.GET("/probe", func(c *gin.Context) {
		// Note: no withInstallation() call.
		middleware.WithBalanceCheck(svc, 1_000_000)(c)
		if c.IsAborted() {
			return
		}
		reached = true
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	engine.ServeHTTP(w, req)
	assert.True(t, reached)
	assert.Equal(t, http.StatusOK, w.Code)
}
