package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// runSpendCap drives WithAPIKeySpendCap with a synthetic api key pre-stashed
// the way WithAuth would, returning the response and whether the handler ran.
func runSpendCap(t *testing.T, key *auth.APIKey) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	reached := false
	engine.GET("/probe", func(c *gin.Context) {
		if key != nil {
			c.Set("router_api_key", key)
		}
		middleware.WithAPIKeySpendCap()(c)
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
	w, reached := runSpendCap(t, &auth.APIKey{ID: "k1", SpendCapUsdMicros: nil, SpentUsdMicros: 999_999_999})
	assert.True(t, reached, "a key with no cap is never blocked, regardless of spend")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIKeySpendCap_UnderCapPassesThrough(t *testing.T) {
	w, reached := runSpendCap(t, &auth.APIKey{ID: "k2", SpendCapUsdMicros: capPtr(1_000_000), SpentUsdMicros: 999_999})
	assert.True(t, reached, "spend just under the cap still routes")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAPIKeySpendCap_AtCapRejected(t *testing.T) {
	w, reached := runSpendCap(t, &auth.APIKey{ID: "k3", SpendCapUsdMicros: capPtr(1_000_000), SpentUsdMicros: 1_000_000})
	assert.False(t, reached, "spend equal to the cap blocks the request")
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
	assert.Contains(t, w.Body.String(), "key_spend_cap_reached")
}

func TestAPIKeySpendCap_OverCapRejected(t *testing.T) {
	w, reached := runSpendCap(t, &auth.APIKey{ID: "k4", SpendCapUsdMicros: capPtr(1_000_000), SpentUsdMicros: 2_500_000})
	assert.False(t, reached)
	assert.Equal(t, http.StatusPaymentRequired, w.Code)
}

func TestAPIKeySpendCap_NoKeyPassesThrough(t *testing.T) {
	// Admin/cookie paths carry no api key on context; the cap check must be a
	// no-op rather than block them.
	w, reached := runSpendCap(t, nil)
	assert.True(t, reached)
	assert.Equal(t, http.StatusOK, w.Code)
}
