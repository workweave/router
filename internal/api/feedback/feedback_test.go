package feedback_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	feedbackapi "workweave/router/internal/api/feedback"
	token "workweave/router/internal/feedback"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeFeedbackRepo struct {
	ctxResult proxy.FeedbackContext
	ctxErr    error
	upserts   []proxy.UpsertFeedbackParams
}

func (f *fakeFeedbackRepo) Upsert(_ context.Context, p proxy.UpsertFeedbackParams) error {
	f.upserts = append(f.upserts, p)
	return nil
}

func (f *fakeFeedbackRepo) GetContext(_ context.Context, _, _ string) (proxy.FeedbackContext, error) {
	return f.ctxResult, f.ctxErr
}

func newService(repo proxy.FeedbackRepository, signer *token.Signer) *proxy.Service {
	return proxy.NewService(nil, nil, nil, false, nil, nil, false, "", "", nil).
		WithFeedback(repo, signer, "https://router.example.com")
}

func TestGetContextHandler_ValidTokenReturnsContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	signer := token.NewSigner("secret", time.Hour)
	repo := &fakeFeedbackRepo{ctxResult: proxy.FeedbackContext{
		RequestID:      "req-1",
		ChosenModel:    "claude-haiku-4-5",
		ChosenProvider: "anthropic",
		ClientApp:      "claude-code",
		RoutedAt:       time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Rating:         "up",
	}}
	engine := gin.New()
	engine.GET("/v1/feedback/link/:token", feedbackapi.GetContextHandler(newService(repo, signer)))

	tok := signer.Mint("inst-1", "org-1", "req-1", "user-1")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/link/"+tok, nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "req-1", got["request_id"])
	assert.Equal(t, "claude-haiku-4-5", got["chosen_model"])
	assert.Equal(t, "anthropic", got["chosen_provider"])
	fb, ok := got["feedback"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "up", fb["rating"])
}

func TestGetContextHandler_UnknownRequestStillRenders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	signer := token.NewSigner("secret", time.Hour)
	repo := &fakeFeedbackRepo{ctxErr: pgx.ErrNoRows}
	engine := gin.New()
	engine.GET("/v1/feedback/link/:token", feedbackapi.GetContextHandler(newService(repo, signer)))

	tok := signer.Mint("inst-1", "org-1", "req-1", "")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/link/"+tok, nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "req-1", got["request_id"])
	assert.Nil(t, got["feedback"])
}

func TestGetContextHandler_InvalidTokenReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	signer := token.NewSigner("secret", time.Hour)
	engine := gin.New()
	engine.GET("/v1/feedback/link/:token", feedbackapi.GetContextHandler(newService(&fakeFeedbackRepo{}, signer)))

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/link/bogus.token", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetContextHandler_ExpiredTokenReturns410(t *testing.T) {
	gin.SetMode(gin.TestMode)
	signer := token.NewSigner("secret", time.Nanosecond)
	engine := gin.New()
	engine.GET("/v1/feedback/link/:token", feedbackapi.GetContextHandler(newService(&fakeFeedbackRepo{}, signer)))

	tok := signer.Mint("inst-1", "org-1", "req-1", "")
	time.Sleep(2 * time.Millisecond)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/link/"+tok, nil))

	assert.Equal(t, http.StatusGone, rec.Code)
}

func TestSubmitHandler_ValidSubmissionPersists(t *testing.T) {
	gin.SetMode(gin.TestMode)
	signer := token.NewSigner("secret", time.Hour)
	repo := &fakeFeedbackRepo{}
	engine := gin.New()
	engine.POST("/v1/feedback/link", feedbackapi.SubmitHandler(newService(repo, signer)))

	tok := signer.Mint("inst-1", "org-1", "req-1", "user-1")
	body := `{"token":"` + tok + `","rating":"down","comment":"  routed to a weak model  "}`
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/feedback/link", strings.NewReader(body)))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.upserts, 1)
	up := repo.upserts[0]
	assert.Equal(t, "inst-1", up.InstallationID)
	assert.Equal(t, "org-1", up.ExternalID)
	assert.Equal(t, "req-1", up.RequestID)
	assert.Equal(t, "down", up.Rating)
	assert.Equal(t, "user-1", up.RouterUserID)
	require.NotNil(t, up.Comment)
	assert.Equal(t, "routed to a weak model", *up.Comment)
}

func TestSubmitHandler_RejectsInvalidRating(t *testing.T) {
	gin.SetMode(gin.TestMode)
	signer := token.NewSigner("secret", time.Hour)
	repo := &fakeFeedbackRepo{}
	engine := gin.New()
	engine.POST("/v1/feedback/link", feedbackapi.SubmitHandler(newService(repo, signer)))

	tok := signer.Mint("inst-1", "org-1", "req-1", "")
	body := `{"token":"` + tok + `","rating":"meh"}`
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/feedback/link", strings.NewReader(body)))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, repo.upserts)
}

func TestSubmitHandler_InvalidTokenReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	signer := token.NewSigner("secret", time.Hour)
	repo := &fakeFeedbackRepo{}
	engine := gin.New()
	engine.POST("/v1/feedback/link", feedbackapi.SubmitHandler(newService(repo, signer)))

	body := `{"token":"bogus.token","rating":"up"}`
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/feedback/link", strings.NewReader(body)))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, repo.upserts)
}

func rateEngine(repo proxy.FeedbackRepository, signer *token.Signer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/feedback/rate", feedbackapi.RateHandler(newService(repo, signer)))
	return engine
}

func TestRateHandler_OneClickUpPersists(t *testing.T) {
	signer := token.NewSigner("secret", time.Hour)
	repo := &fakeFeedbackRepo{}
	tok := signer.Mint("inst-1", "org-1", "req-1", "user-1")

	rec := httptest.NewRecorder()
	rateEngine(repo, signer).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/rate?t="+tok+"&r=up", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.upserts, 1)
	up := repo.upserts[0]
	assert.Equal(t, "req-1", up.RequestID)
	assert.Equal(t, "up", up.Rating)
	assert.Equal(t, "user-1", up.RouterUserID)
	assert.Nil(t, up.Comment, "one-click ratings carry no comment")
	assert.Contains(t, rec.Body.String(), "Thank you for your feedback!")
	assert.Contains(t, rec.Body.String(), "/v1/feedback/assets/wooly-wave.png")
	assert.Contains(t, rec.Body.String(), "/v1/feedback/assets/weave.svg")
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
}

func TestRateHandler_OneClickDownPersists(t *testing.T) {
	signer := token.NewSigner("secret", time.Hour)
	repo := &fakeFeedbackRepo{}
	tok := signer.Mint("inst-1", "org-1", "req-2", "")

	rec := httptest.NewRecorder()
	rateEngine(repo, signer).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/rate?t="+tok+"&r=down", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, repo.upserts, 1)
	assert.Equal(t, "down", repo.upserts[0].Rating)
}

func TestRateHandler_RejectsBadRating(t *testing.T) {
	signer := token.NewSigner("secret", time.Hour)
	repo := &fakeFeedbackRepo{}
	tok := signer.Mint("inst-1", "org-1", "req-1", "")

	rec := httptest.NewRecorder()
	rateEngine(repo, signer).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/rate?t="+tok+"&r=meh", nil))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, repo.upserts)
}

func TestRateHandler_InvalidTokenReturns404(t *testing.T) {
	signer := token.NewSigner("secret", time.Hour)
	repo := &fakeFeedbackRepo{}

	rec := httptest.NewRecorder()
	rateEngine(repo, signer).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/rate?t=bogus.token&r=up", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, repo.upserts)
}

func TestRateHandler_ExpiredTokenReturns410(t *testing.T) {
	signer := token.NewSigner("secret", time.Nanosecond)
	repo := &fakeFeedbackRepo{}
	tok := signer.Mint("inst-1", "org-1", "req-1", "")
	time.Sleep(2 * time.Millisecond)

	rec := httptest.NewRecorder()
	rateEngine(repo, signer).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/rate?t="+tok+"&r=up", nil))

	assert.Equal(t, http.StatusGone, rec.Code)
	assert.Empty(t, repo.upserts)
}
