package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/billing"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// runSpendCap drives WithAPIKeySpendCap with a synthetic api key pre-stashed
// the way WithAuth would, and a billing service backed by the given stub repo.
// Returns the response and whether the handler ran.
func runSpendCap(t *testing.T, keyID string, repo *stubBillingRepo) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := billing.NewService(repo)
	engine := gin.New()
	reached := false
	engine.GET("/probe", func(c *gin.Context) {
		if keyID != "" {
			c.Set("router_api_key", &auth.APIKey{ID: keyID})
		}
		middleware.WithAPIKeySpendCap(svc)(c)
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

func capPtr(v int64) *int64 { return &v }

func TestAPIKeySpendCap_UncappedPassesThrough(t *testing.T) {
	// Found key, nil cap → never blocked regardless of spend.
	w, reached := runSpendCap(t, "k1", &stubBillingRepo{spendFound: true, capMicros: nil, spendMicros: 999_999_999})
	assert.True(t, reached, "a key with no cap is never blocked")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIKeySpendCap_UnderCapPassesThrough(t *testing.T) {
	w, reached := runSpendCap(t, "k2", &stubBillingRepo{spendFound: true, capMicros: capPtr(1_000_000), spendMicros: 999_999})
	assert.True(t, reached, "spend just under the cap still routes")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIKeySpendCap_AtCapRejected(t *testing.T) {
	w, reached := runSpendCap(t, "k3", &stubBillingRepo{spendFound: true, capMicros: capPtr(1_000_000), spendMicros: 1_000_000})
	assert.False(t, reached, "spend equal to the cap blocks the request")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
	assert.Contains(t, w.Body.String(), "key_spend_cap_reached")
}

func TestAPIKeySpendCap_OverCapRejected(t *testing.T) {
	w, reached := runSpendCap(t, "k4", &stubBillingRepo{spendFound: true, capMicros: capPtr(1_000_000), spendMicros: 2_500_000})
	assert.False(t, reached)
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestAPIKeySpendCap_KeyNotFoundPassesThrough(t *testing.T) {
	// Key deleted mid-request (found=false) → no cap to enforce, allow through.
	w, reached := runSpendCap(t, "k5", &stubBillingRepo{spendFound: false})
	assert.True(t, reached)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIKeySpendCap_ReadErrorFailsClosed(t *testing.T) {
	// A repo read error must fail closed (503), like the balance gate — a cap
	// that lets requests through on read errors is an unbilled-usage hole.
	w, reached := runSpendCap(t, "k6", &stubBillingRepo{spendErr: errors.New("conn refused")})
	assert.False(t, reached)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestAPIKeySpendCap_NoKeyPassesThrough(t *testing.T) {
	// Admin/cookie paths carry no api key on context; the check is a no-op.
	w, reached := runSpendCap(t, "", &stubBillingRepo{})
	assert.True(t, reached)
	assert.Equal(t, http.StatusOK, w.Code)
}

// runSpendCapSub drives WithAPIKeySpendCap on routePath with a caller-supplied
// installation setter, an api key, and an optional Authorization header,
// returning the recorder, the reached flag, and whether the request context was
// flagged subscription-only. The gate keys off the api key but reads the
// installation from context for the subscription exemption.
func runSpendCapSub(t *testing.T, routePath, keyID string, setInstall func(*gin.Context), authHeader string, repo *stubBillingRepo) (*httptest.ResponseRecorder, bool, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := billing.NewService(repo)
	engine := gin.New()
	reached := false
	subOnly := false
	engine.GET(routePath, func(c *gin.Context) {
		setInstall(c)
		c.Set("router_api_key", &auth.APIKey{ID: keyID})
		middleware.WithAPIKeySpendCap(svc)(c)
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

func TestAPIKeySpendCap_CapReachedCoveringSubscriptionServesSubscriptionOnly(t *testing.T) {
	// Cap reached + a usage-bypass org presenting a Claude sub on /v1/messages:
	// pass through flagged subscription-only, not 402. The cap bounds paid spend.
	repo := &stubBillingRepo{spendFound: true, capMicros: capPtr(1_000_000), spendMicros: 1_000_000}
	setInstall := func(c *gin.Context) { withUsageBypassInstallation(c, "org_sub") }
	w, reached, subOnly := runSpendCapSub(t, "/v1/messages", "k7", setInstall, "Bearer sk-ant-oat-abc123", repo)
	assert.True(t, reached, "a covered turn must pass even when the cap is reached")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, subOnly, "the request must be flagged subscription-only")
}

func TestAPIKeySpendCap_CapReachedNoSubscriptionStillRejected(t *testing.T) {
	repo := &stubBillingRepo{spendFound: true, capMicros: capPtr(1_000_000), spendMicros: 1_000_000}
	setInstall := func(c *gin.Context) { withUsageBypassInstallation(c, "org_sub") }
	w, reached, _ := runSpendCapSub(t, "/v1/messages", "k8", setInstall, "", repo)
	assert.False(t, reached, "no subscription credential means the paid path is gated")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestAPIKeySpendCap_CapReachedSubscriptionWithoutBypassRejected(t *testing.T) {
	repo := &stubBillingRepo{spendFound: true, capMicros: capPtr(1_000_000), spendMicros: 1_000_000}
	setInstall := func(c *gin.Context) { withInstallation(c, "org_prepaid") }
	w, reached, _ := runSpendCapSub(t, "/v1/messages", "k9", setInstall, "Bearer sk-ant-oat-abc123", repo)
	assert.False(t, reached, "exemption must not apply without the usage-bypass gate")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}
