package feedback_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	feedbackapi "workweave/router/internal/api/feedback"
	token "workweave/router/internal/feedback"
	"workweave/router/internal/postgres"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFeedbackLink_EndToEnd drives the real gin handlers through the real
// proxy.Service and the real Postgres FeedbackRepo against a live database.
// It is the full router-side path minus the LLM proxy: mint a token, load the
// routing context (GET), submit a rating (POST), and confirm the revision is
// read back. Gated behind ROUTER_TEST_DATABASE_URL (a DSN to a database with
// the router migrations applied) so it is a no-op in CI without a DB.
func TestFeedbackLink_EndToEnd(t *testing.T) {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ROUTER_TEST_DATABASE_URL not set; skipping live-DB integration test")
	}
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	// Seed an installation + one router.upstream telemetry row under a unique
	// request id so reruns don't collide, and clean both up afterward.
	externalID := "org_e2e_" + uuid.NewString()[:8]
	requestID := "req_e2e_" + uuid.NewString()[:8]
	var installID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO router.model_router_installations (external_id, name)
		 VALUES ($1, 'E2E feedback test') RETURNING id`, externalID,
	).Scan(&installID))
	t.Cleanup(func() {
		// request_feedback rows cascade on installation delete.
		_, _ = pool.Exec(ctx, `DELETE FROM router.model_router_request_telemetry WHERE installation_id = $1`, installID)
		_, _ = pool.Exec(ctx, `DELETE FROM router.model_router_installations WHERE id = $1`, installID)
	})
	_, err = pool.Exec(ctx,
		`INSERT INTO router.model_router_request_telemetry
		   (installation_id, request_id, span_type, trace_id, timestamp,
		    requested_model, decision_model, decision_provider, client_app)
		 VALUES ($1, $2, 'router.upstream', 'trace-e2e', NOW(),
		    'claude-sonnet-4', 'claude-haiku-4-5', 'anthropic', 'claude-code')`,
		installID, requestID)
	require.NoError(t, err)

	signer := token.NewSigner("e2e-secret", time.Hour)
	svc := proxy.NewService(nil, nil, nil, false, nil, nil, false, "", "", nil).
		WithFeedback(postgres.NewFeedbackRepo(pool), signer, "https://router.example.com")

	engine := gin.New()
	engine.GET("/v1/feedback/link/:token", feedbackapi.GetContextHandler(svc))
	engine.POST("/v1/feedback/link", feedbackapi.SubmitHandler(svc))

	tok := signer.Mint(installID, externalID, requestID, "")

	// 1) GET returns the routing context, no feedback yet.
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/link/"+tok, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ctxResp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ctxResp))
	assert.Equal(t, requestID, ctxResp["request_id"])
	assert.Equal(t, "claude-haiku-4-5", ctxResp["chosen_model"])
	assert.Equal(t, "anthropic", ctxResp["chosen_provider"])
	assert.Nil(t, ctxResp["feedback"])

	// 2) POST a thumbs-down with a comment.
	body := `{"token":"` + tok + `","rating":"down","comment":"  routed too low  "}`
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/feedback/link", strings.NewReader(body)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 3) GET again reflects the persisted, trimmed feedback.
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/link/"+tok, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ctxResp))
	fb, ok := ctxResp["feedback"].(map[string]any)
	require.True(t, ok, "feedback should be present after submit")
	assert.Equal(t, "down", fb["rating"])
	assert.Equal(t, "routed too low", fb["comment"])

	// 4) Revise to thumbs-up with a blank comment; the natural key keeps it to
	// one row and the comment collapses to NULL.
	body = `{"token":"` + tok + `","rating":"up","comment":"   "}`
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/feedback/link", strings.NewReader(body)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var rowCount int
	var rating string
	var comment *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM router.request_feedback WHERE installation_id = $1 AND request_id = $2`,
		installID, requestID).Scan(&rowCount))
	assert.Equal(t, 1, rowCount, "upsert must not duplicate rows")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT rating, comment FROM router.request_feedback WHERE installation_id = $1 AND request_id = $2`,
		installID, requestID).Scan(&rating, &comment))
	assert.Equal(t, "up", rating)
	assert.Nil(t, comment, "blank comment should collapse to NULL")
}

// TestFeedbackLink_FeedbackWithoutTelemetry guards the regression where a saved
// rating was hidden when no router.upstream telemetry row exists (telemetry
// disabled/pruned, or still in flight via async fireTelemetry). GET must still
// return the persisted rating, just with empty routing context.
func TestFeedbackLink_FeedbackWithoutTelemetry(t *testing.T) {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ROUTER_TEST_DATABASE_URL not set; skipping live-DB integration test")
	}
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	externalID := "org_nt_" + uuid.NewString()[:8]
	requestID := "req_nt_" + uuid.NewString()[:8]
	var installID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO router.model_router_installations (external_id, name)
		 VALUES ($1, 'E2E feedback no-telemetry') RETURNING id`, externalID,
	).Scan(&installID))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM router.model_router_installations WHERE id = $1`, installID)
	})

	// A saved rating with NO telemetry row for the request.
	_, err = pool.Exec(ctx,
		`INSERT INTO router.request_feedback (installation_id, external_id, request_id, rating, comment, source)
		 VALUES ($1, $2, $3, 'down', 'no telemetry yet', 'link')`,
		installID, externalID, requestID)
	require.NoError(t, err)

	signer := token.NewSigner("nt-secret", time.Hour)
	svc := proxy.NewService(nil, nil, nil, false, nil, nil, false, "", "", nil).
		WithFeedback(postgres.NewFeedbackRepo(pool), signer, "https://router.example.com")
	engine := gin.New()
	engine.GET("/v1/feedback/link/:token", feedbackapi.GetContextHandler(svc))

	tok := signer.Mint(installID, externalID, requestID, "")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/link/"+tok, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, requestID, resp["request_id"])
	assert.Nil(t, resp["chosen_model"], "no telemetry => empty routing context")
	fb, ok := resp["feedback"].(map[string]any)
	require.True(t, ok, "saved rating must be returned even without telemetry")
	assert.Equal(t, "down", fb["rating"])
	assert.Equal(t, "no telemetry yet", fb["comment"])
}
