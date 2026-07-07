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
	"workweave/router/internal/proxy"
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
	// Per-key spend-cap fields, exercised by WithAPIKeySpendCap.
	spendMicros int64
	capMicros   *int64
	spendFound  bool
	spendErr    error
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
func (r *stubBillingRepo) GetAPIKeySpend(_ context.Context, _ string) (int64, *int64, bool, error) {
	return r.spendMicros, r.capMicros, r.spendFound, r.spendErr
}
func (r *stubBillingRepo) BillingTablesExist(_ context.Context) (bool, error) {
	return true, nil
}
func (r *stubBillingRepo) GetAutopayConfig(_ context.Context, _ string) (bool, int64, error) {
	return false, 0, nil
}

func withInstallation(c *gin.Context, externalID string) {
	c.Set("router_installation", &auth.Installation{
		ID:         "00000000-0000-0000-0000-000000000001",
		ExternalID: externalID,
	})
}

func withUsageBypassInstallation(c *gin.Context, externalID string) {
	c.Set("router_installation", &auth.Installation{
		ID:                 "00000000-0000-0000-0000-000000000001",
		ExternalID:         externalID,
		UsageBypassEnabled: true,
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

// runMiddlewareWith drives WithBalanceCheck on routePath (the gin route the
// exemption's route-scoping keys off) with a caller-supplied installation setter
// and an optional Authorization header. authHeader is set verbatim when non-empty.
func runMiddlewareWith(t *testing.T, repo billing.Repo, threshold int64, routePath string, setInstall func(*gin.Context), authHeader string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := billing.NewService(repo)
	engine := gin.New()
	reached := false
	engine.GET(routePath, func(c *gin.Context) {
		setInstall(c)
		middleware.WithBalanceCheck(svc, threshold)(c)
		if c.IsAborted() {
			return
		}
		reached = true
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, routePath, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
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

func TestWithBalanceCheck_FailClosedOnRepoError(t *testing.T) {
	// A transient DB error reading the balance must fail closed. In a
	// prepaid credit system, letting the request through with an unknown
	// balance creates an unbilled-usage window where platform spend is
	// incurred but no debit is possible. 503 prompts the client to retry
	// rather than silently spend against an unknown balance.
	repo := &stubBillingRepo{balanceErr: errors.New("conn refused")}
	w, reached := runMiddleware(t, repo, 1_000_000, "org_x")
	assert.False(t, reached, "infra error must not allow request through")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "billing_unavailable", body.Error)
	assert.NotEmpty(t, body.Message)
}

func TestWithBalanceCheck_AllowsZeroThreshold(t *testing.T) {
	// minBalanceMicros=0 means "only 402 when the balance is actually
	// at or below zero". A small positive balance must pass through.
	repo := &stubBillingRepo{balance: 1}
	_, reached := runMiddleware(t, repo, 0, "org_x")
	assert.True(t, reached, "positive balance must pass when threshold is zero")
}

func TestWithBalanceCheck_BlocksAtZeroWhenThresholdIsZero(t *testing.T) {
	// At threshold=0, balance=0 still trips the ≤ check and returns 402.
	repo := &stubBillingRepo{balance: 0}
	w, reached := runMiddleware(t, repo, 0, "org_x")
	assert.False(t, reached)
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
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

func TestWithBalanceCheck_ExemptsSubscriptionUsageBypassRequest(t *testing.T) {
	// A usage-bypass org with a $0 balance must still pass when the request
	// carries a Claude subscription bearer — that turn is served on the
	// caller's own plan and debits $0, so prepaid credits don't apply.
	repo := &stubBillingRepo{balance: 0}
	setInstall := func(c *gin.Context) { withUsageBypassInstallation(c, "org_sub") }
	w, reached := runMiddlewareWith(t, repo, 0, "/v1/messages", setInstall, "Bearer sk-ant-oat-abc123")
	assert.True(t, reached, "subscription usage-bypass request must pass even at zero balance")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithBalanceCheck_402sUsageBypassOrgWithoutSubscription(t *testing.T) {
	// Usage-bypass gate on, but NO subscription credential on the request —
	// the turn routes to a paid model, so a depleted balance must still 402.
	repo := &stubBillingRepo{balance: 0}
	setInstall := func(c *gin.Context) { withUsageBypassInstallation(c, "org_sub") }
	w, reached := runMiddlewareWith(t, repo, 0, "/v1/messages", setInstall, "")
	assert.False(t, reached, "no subscription credential means the paid path is gated")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestWithBalanceCheck_402sSubscriptionWithoutUsageBypass(t *testing.T) {
	// Subscription bearer present, but the org has NOT enabled the usage-bypass
	// gate. The exemption is scoped to bypass orgs, so this must still 402 to
	// avoid opening an unbilled-usage window for regular prepaid orgs.
	repo := &stubBillingRepo{balance: 0}
	setInstall := func(c *gin.Context) { withInstallation(c, "org_prepaid") }
	w, reached := runMiddlewareWith(t, repo, 0, "/v1/messages", setInstall, "Bearer sk-ant-oat-abc123")
	assert.False(t, reached, "exemption must not apply without the usage-bypass gate")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestWithBalanceCheck_ExemptsSubscriptionWhenBalanceRowMissing(t *testing.T) {
	// The original 402 bug: a subscription org that never had a balance row.
	// Its turns are free, so a missing row must exempt, not 402.
	repo := &stubBillingRepo{balanceErr: billing.ErrBalanceRowMissing}
	setInstall := func(c *gin.Context) { withUsageBypassInstallation(c, "org_sub") }
	w, reached := runMiddlewareWith(t, repo, 0, "/v1/messages", setInstall, "Bearer sk-ant-oat-abc123")
	assert.True(t, reached, "subscription request with no balance row must pass")
	assert.Equal(t, http.StatusOK, w.Code)
}

// runMiddlewarePrep drives WithBalanceCheck with a caller-supplied prep hook
// that runs before the middleware (to stash installation + request-context
// values the way WithAuth would), returning the recorder, whether the handler
// was reached, and whether the override context flag was planted.
func runMiddlewarePrep(t *testing.T, repo billing.Repo, threshold int64, routePath string, prep func(*gin.Context)) (*httptest.ResponseRecorder, bool, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := billing.NewService(repo)
	engine := gin.New()
	reached := false
	hasOverride := false
	engine.GET(routePath, func(c *gin.Context) {
		prep(c)
		middleware.WithBalanceCheck(svc, threshold)(c)
		if c.IsAborted() {
			return
		}
		reached = true
		hasOverride = billing.HasOverrideFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, routePath, nil)
	engine.ServeHTTP(w, req)
	return w, reached, hasOverride
}

// stashAnthropicSub plants a dedicated X-Weave-Anthropic-Subscription value on
// the request context the way WithAuth does (raw, unvalidated).
func stashAnthropicSub(c *gin.Context, value string) {
	ctx := context.WithValue(c.Request.Context(), proxy.AnthropicSubscriptionContextKey{}, value)
	c.Request = c.Request.WithContext(ctx)
}

// stashCodexSub plants a dedicated Codex subscription (JWT + account id) on the
// request context the way WithAuth does. Both are required for a usable Codex sub.
func stashCodexSub(c *gin.Context, token, accountID string) {
	ctx := context.WithValue(c.Request.Context(), proxy.OpenAISubscriptionContextKey{}, token)
	ctx = context.WithValue(ctx, proxy.OpenAIAccountIDContextKey{}, accountID)
	c.Request = c.Request.WithContext(ctx)
}

func TestWithBalanceCheck_402sJunkDedicatedAnthropicSubHeader(t *testing.T) {
	// A junk X-Weave-Anthropic-Subscription value is never injected as a
	// subscription (injection requires sk-ant-oat), so the gate must NOT treat
	// it as one — otherwise a bypass org routes paid turns on deployment keys at
	// $0. Detection validates the dedicated header, not a bare presence check.
	repo := &stubBillingRepo{balance: 0}
	prep := func(c *gin.Context) {
		withUsageBypassInstallation(c, "org_sub")
		stashAnthropicSub(c, "not-a-real-token")
	}
	w, reached, _ := runMiddlewarePrep(t, repo, 0, "/v1/messages", prep)
	assert.False(t, reached, "junk subscription header must not exempt the gate")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestWithBalanceCheck_ExemptsValidDedicatedAnthropicSubHeader(t *testing.T) {
	// A valid sk-ant-oat token in the dedicated header (opencode / router-keyed
	// path) must exempt, mirroring credential injection's acceptance.
	repo := &stubBillingRepo{balance: 0}
	prep := func(c *gin.Context) {
		withUsageBypassInstallation(c, "org_sub")
		stashAnthropicSub(c, "sk-ant-oat01-valid-token")
	}
	w, reached, _ := runMiddlewarePrep(t, repo, 0, "/v1/messages", prep)
	assert.True(t, reached, "a valid dedicated-header subscription must exempt the gate")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithBalanceCheck_OverrideFlagSetEvenForSubscriptionRequest(t *testing.T) {
	// An override org that also presents a subscription credential must still get
	// the override context flag (so the debit hook writes delta=0 for ALL turns),
	// not be short-circuited by the subscription exemption before CheckBalance.
	repo := &stubBillingRepo{override: true}
	prep := func(c *gin.Context) {
		withUsageBypassInstallation(c, "org_override")
		stashAnthropicSub(c, "sk-ant-oat01-valid-token")
	}
	w, reached, hasOverride := runMiddlewarePrep(t, repo, 0, "/v1/messages", prep)
	assert.True(t, reached, "override org must reach the handler")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, hasOverride, "override flag must be planted even when a subscription is present")
}

func TestWithBalanceCheck_ExemptsCodexSubscriptionOnOpenAIRoute(t *testing.T) {
	// A Codex subscription covers the OpenAI chat/responses APIs, so a bypass
	// org presenting one on /v1/responses at $0 balance must be exempt.
	repo := &stubBillingRepo{balance: 0}
	prep := func(c *gin.Context) {
		withUsageBypassInstallation(c, "org_codex")
		stashCodexSub(c, "eyJhbGciOi.codex.jwt", "acct-abc-123")
	}
	w, reached, _ := runMiddlewarePrep(t, repo, 0, "/v1/responses", prep)
	assert.True(t, reached, "a Codex subscription must exempt an OpenAI-route request")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithBalanceCheck_402sCodexSubscriptionOnAnthropicRoute(t *testing.T) {
	// The greptile P1: a Codex subscription can't serve /v1/messages, so the turn
	// would route to a paid Anthropic model. The exemption is route-scoped, so a
	// Codex sub on the Anthropic route must NOT exempt — a depleted balance 402s.
	repo := &stubBillingRepo{balance: 0}
	prep := func(c *gin.Context) {
		withUsageBypassInstallation(c, "org_codex")
		stashCodexSub(c, "eyJhbGciOi.codex.jwt", "acct-abc-123")
	}
	w, reached, _ := runMiddlewarePrep(t, repo, 0, "/v1/messages", prep)
	assert.False(t, reached, "a Codex sub must not exempt an Anthropic-route request")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestWithBalanceCheck_SubscriptionOverdraftAllowsNegativeBalance(t *testing.T) {
	// A subscription-covered request stays optimistic: a small negative balance
	// (e.g. paid failover) within the overdraft floor still passes.
	repo := &stubBillingRepo{balance: billing.SubscriptionOverdraftFloorMicros / 2}
	setInstall := func(c *gin.Context) { withUsageBypassInstallation(c, "org_sub") }
	w, reached := runMiddlewareWith(t, repo, 0, "/v1/messages", setInstall, "Bearer sk-ant-oat-abc123")
	assert.True(t, reached, "subscription request within the overdraft floor must pass")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithBalanceCheck_SubscriptionGatedBelowOverdraftFloor(t *testing.T) {
	// Past the overdraft floor, even a subscription-covered request 402s — the
	// optimism is bounded.
	repo := &stubBillingRepo{balance: billing.SubscriptionOverdraftFloorMicros - 1}
	setInstall := func(c *gin.Context) { withUsageBypassInstallation(c, "org_sub") }
	w, reached := runMiddlewareWith(t, repo, 0, "/v1/messages", setInstall, "Bearer sk-ant-oat-abc123")
	assert.False(t, reached, "subscription request below the overdraft floor must gate")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}
