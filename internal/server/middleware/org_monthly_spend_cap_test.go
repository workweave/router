package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/billing"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// runOrgMonthlyCap drives WithOrgMonthlySpendCap with a synthetic installation
// pre-stashed the way WithAuth would. Returns the recorder and a reached flag.
func runOrgMonthlyCap(t *testing.T, externalID string, repo *stubBillingRepo) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := billing.NewService(repo)
	engine := gin.New()
	reached := false
	engine.GET("/probe", func(c *gin.Context) {
		if externalID != "" {
			withInstallation(c, externalID)
		}
		middleware.WithOrgMonthlySpendCap(svc)(c)
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

func TestOrgMonthlySpendCap_NoCapPassesThrough(t *testing.T) {
	w, reached := runOrgMonthlyCap(t, "org-1", &stubBillingRepo{orgMonthSpent: 999_999_999, orgMonthLimit: nil})
	assert.True(t, reached, "an org with no monthly cap is never blocked")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestOrgMonthlySpendCap_UnderCapPassesThrough(t *testing.T) {
	w, reached := runOrgMonthlyCap(t, "org-2", &stubBillingRepo{orgMonthSpent: 999_999, orgMonthLimit: capPtr(1_000_000)})
	assert.True(t, reached, "spend just under the cap still routes")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestOrgMonthlySpendCap_AtCapRejected(t *testing.T) {
	w, reached := runOrgMonthlyCap(t, "org-3", &stubBillingRepo{orgMonthSpent: 1_000_000, orgMonthLimit: capPtr(1_000_000)})
	assert.False(t, reached, "spend equal to the cap blocks the request")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
	assert.Contains(t, w.Body.String(), "org_monthly_spend_limit_reached")
}

func TestOrgMonthlySpendCap_OverCapRejected(t *testing.T) {
	w, reached := runOrgMonthlyCap(t, "org-4", &stubBillingRepo{orgMonthSpent: 2_500_000, orgMonthLimit: capPtr(1_000_000)})
	assert.False(t, reached)
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestOrgMonthlySpendCap_ReadErrorFailsClosed(t *testing.T) {
	// A repo read error must fail closed (503), like the balance gate — a cap
	// that lets requests through on read errors is an unbounded-spend hole.
	w, reached := runOrgMonthlyCap(t, "org-5", &stubBillingRepo{orgMonthErr: errors.New("pg down")})
	assert.False(t, reached)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "billing_unavailable")
}

func TestOrgMonthlySpendCap_NoInstallationPassesThrough(t *testing.T) {
	w, reached := runOrgMonthlyCap(t, "", &stubBillingRepo{orgMonthSpent: 2_500_000, orgMonthLimit: capPtr(1_000_000)})
	assert.True(t, reached, "requests without an installation skip the gate")
	assert.Equal(t, http.StatusOK, w.Code)
}
