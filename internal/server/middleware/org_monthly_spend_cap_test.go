package middleware_test

import (
	"context"
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

// runOrgMonthlyCapSub drives WithOrgMonthlySpendCap on routePath with a
// caller-supplied installation setter and optional Authorization header,
// returning the recorder, the reached flag, and whether the request context
// was flagged subscription-only.
func runOrgMonthlyCapSub(t *testing.T, routePath string, setInstall func(*gin.Context), authHeader string, repo *stubBillingRepo) (*httptest.ResponseRecorder, bool, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := billing.NewService(repo)
	engine := gin.New()
	reached := false
	subOnly := false
	engine.GET(routePath, func(c *gin.Context) {
		setInstall(c)
		middleware.WithOrgMonthlySpendCap(svc)(c)
		if c.IsAborted() {
			return
		}
		reached = true
		subOnly = billing.SubscriptionOnlyFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, routePath, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	engine.ServeHTTP(w, req)
	return w, reached, subOnly
}

func TestOrgMonthlySpendCap_CapReachedCoveringSubscriptionServesSubscriptionOnly(t *testing.T) {
	// Cap reached + a usage-bypass org presenting a Claude sub on /v1/messages:
	// pass through flagged subscription-only, not 402. The cap bounds paid spend.
	repo := &stubBillingRepo{orgMonthSpent: 1_000_000, orgMonthLimit: capPtr(1_000_000)}
	setInstall := func(c *gin.Context) { withUsageBypassInstallation(c, "org_sub") }
	w, reached, subOnly := runOrgMonthlyCapSub(t, "/v1/messages", setInstall, "Bearer sk-ant-oat-abc123", repo)
	assert.True(t, reached, "a covered turn must pass even when the cap is reached")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, subOnly, "the request must be flagged subscription-only")
}

func TestOrgMonthlySpendCap_CapReachedNoSubscriptionStillRejected(t *testing.T) {
	repo := &stubBillingRepo{orgMonthSpent: 1_000_000, orgMonthLimit: capPtr(1_000_000)}
	setInstall := func(c *gin.Context) { withUsageBypassInstallation(c, "org_sub") }
	w, reached, _ := runOrgMonthlyCapSub(t, "/v1/messages", setInstall, "", repo)
	assert.False(t, reached, "no subscription credential means the paid path is gated")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestOrgMonthlySpendCap_CapReachedSubscriptionWithoutBypassRejected(t *testing.T) {
	repo := &stubBillingRepo{orgMonthSpent: 1_000_000, orgMonthLimit: capPtr(1_000_000)}
	setInstall := func(c *gin.Context) { withInstallation(c, "org_prepaid") }
	w, reached, _ := runOrgMonthlyCapSub(t, "/v1/messages", setInstall, "Bearer sk-ant-oat-abc123", repo)
	assert.False(t, reached, "exemption must not apply without the usage-bypass gate")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestOrgMonthlySpendCap_OverridePassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Over cap AND a repo that would error: an override org must bypass both the
	// 402 and the 503 fail-closed, matching the balance gate's escape hatch.
	svc := billing.NewService(&stubBillingRepo{orgMonthErr: errors.New("pg down")})
	engine := gin.New()
	reached := false
	engine.GET("/probe", func(c *gin.Context) {
		withInstallation(c, "org-override")
		ctx := context.WithValue(c.Request.Context(), billing.HasOverrideContextKey, true)
		c.Request = c.Request.WithContext(ctx)
		middleware.WithOrgMonthlySpendCap(svc)(c)
		if c.IsAborted() {
			return
		}
		reached = true
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	assert.True(t, reached, "billing-override orgs bypass the monthly cap")
	assert.Equal(t, http.StatusOK, w.Code)
}
