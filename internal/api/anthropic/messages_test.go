package anthropic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	anthropicapi "workweave/router/internal/api/anthropic"
	"workweave/router/internal/auth"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/policy"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRouter lets tests control exactly what routing decision (or error) the
// proxy.Service's scorer step returns, without needing a real cluster router.
// `got` records the last router.Request the Service forwarded, for tests that
// need to assert field-level forwarding (e.g. InstallationID handling).
type fakeRouter struct {
	decision router.Decision
	err      error
	got      *router.Request
}

type fakeRoutePreviewer struct {
	result policy.PreviewResult
	got    *router.Request
}

type countingUserRepository struct {
	upserts int
}

func (r *countingUserRepository) UpsertByEmail(context.Context, auth.UpsertUserParams) (*auth.User, error) {
	r.upserts++
	return &auth.User{ID: "unexpected"}, nil
}

func (r *countingUserRepository) UpsertByAccountUUID(context.Context, auth.UpsertUserByAccountUUIDParams) (*auth.User, error) {
	r.upserts++
	return &auth.User{ID: "unexpected"}, nil
}

func (*countingUserRepository) Get(context.Context, string) (*auth.User, error) {
	return nil, nil
}

func (*countingUserRepository) ListForInstallation(context.Context, string) ([]*auth.User, error) {
	return nil, nil
}

func (f *fakeRoutePreviewer) Route(context.Context, router.Request) (router.Decision, error) {
	return router.Decision{}, errors.New("serving route must not run")
}

func (f *fakeRoutePreviewer) PreviewRoute(_ context.Context, req router.Request) (policy.PreviewResult, error) {
	f.got = &req
	return f.result, nil
}

func (f *fakeRouter) Route(_ context.Context, req router.Request) (router.Decision, error) {
	if f.got != nil {
		*f.got = req
	}
	return f.decision, f.err
}

// fakeProviderClient is a minimal providers.Client double: Proxy/Passthrough
// return whatever the test wants, optionally writing a response first.
type fakeProviderClient struct {
	proxyErr       error
	proxyStatus    int
	proxyBody      string
	passthroughErr error
}

func (f *fakeProviderClient) Proxy(_ context.Context, _ router.Decision, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	if f.proxyErr != nil {
		return f.proxyErr
	}
	status := f.proxyStatus
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(f.proxyBody))
	return nil
}

func (f *fakeProviderClient) Passthrough(_ context.Context, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	if f.passthroughErr != nil {
		return f.passthroughErr
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(f.proxyBody))
	return nil
}

// max_tokens > 256 so DetectFromEnvelope treats this as a MainLoop turn, not a probe/classifier.
const validAnthropicBody = `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"max_tokens":4096}`

// newTestService wires a proxy.Service with a fake router and optional fake provider, no real I/O.
func newTestService(r router.Router, clientName string, client providers.Client) *proxy.Service {
	providerMap := map[string]providers.Client{}
	if clientName != "" && client != nil {
		providerMap[clientName] = client
	}
	return proxy.NewService(r, providerMap, nil, false, nil, nil, false, "", "", nil)
}

func errorEnvelope(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "error", got["type"])
	errObj, ok := got["error"].(map[string]any)
	require.True(t, ok, "expected error envelope to carry an \"error\" object")
	return errObj
}

func messagesEngine(svc *proxy.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/messages", anthropicapi.MessagesHandler(svc, nil))
	return engine
}

func postMessages(engine *gin.Engine, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))
	return rec
}

func TestMessagesHandler_RequestTooLarge(t *testing.T) {
	svc := newTestService(&fakeRouter{}, "", nil)
	engine := messagesEngine(svc)

	oversized := bytes.Repeat([]byte("a"), 10*1024*1024+1)
	rec := postMessages(engine, oversized)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestMessagesHandler_MalformedBodyReturns400(t *testing.T) {
	svc := newTestService(&fakeRouter{}, "", nil)
	engine := messagesEngine(svc)

	// A JSON array is valid JSON but not a JSON object, so ParseAnthropic
	// returns translate.ErrNotJSONObject.
	rec := postMessages(engine, []byte(`[]`))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestMessagesHandler_NoEligibleProviderReturns400(t *testing.T) {
	svc := newTestService(&fakeRouter{err: cluster.ErrNoEligibleProvider}, "", nil)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestMessagesHandler_InvalidRoutingKnobsReturns400(t *testing.T) {
	svc := newTestService(&fakeRouter{err: cluster.ErrInvalidRoutingKnobs}, "", nil)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestMessagesHandler_ClusterUnavailableReturns503WithRetryAfter(t *testing.T) {
	svc := newTestService(&fakeRouter{err: cluster.ErrClusterUnavailable}, "", nil)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestMessagesHandler_RLPolicyUnavailableReturns503(t *testing.T) {
	// No RL router wired — strategy fails closed rather than falling back to cluster scorer.
	svc := newTestService(&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5"}}, "", nil)
	engine := messagesEngine(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(validAnthropicBody)))
	req = req.WithContext(router.WithStrategy(req.Context(), router.StrategyRL))
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
	assert.Contains(t, errObj["message"], "RL policy router")
}

func TestMessagesHandler_BanditUnavailableReturns503(t *testing.T) {
	// No bandit router wired, mirroring the RL case above.
	svc := newTestService(&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5"}}, "", nil)
	engine := messagesEngine(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(validAnthropicBody)))
	req = req.WithContext(router.WithStrategy(req.Context(), router.StrategyBandit))
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
	assert.Contains(t, errObj["message"], "bandit router")
}

// TestMessagesHandler_ProviderNotConfiguredReturns502 guards the fix for the
// anthropic handler previously falling through to the generic 502
// "Upstream call failed." branch (with no dedicated log line) whenever
// ProxyMessages returned proxy.ErrProviderNotConfigured — the exact error the
// dispatch switch in service.go returns for a decision naming a provider with
// no known translation family. openai and gemini already special-cased this
// sentinel; anthropic was missing it before the shared proxy.ClassifyDispatchError.
func TestMessagesHandler_ProviderNotConfiguredReturns502(t *testing.T) {
	svc := newTestService(&fakeRouter{decision: router.Decision{Provider: "not-a-real-provider", Model: "claude-sonnet-4-5"}}, "", nil)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
	assert.Equal(t, "Provider not configured.", errObj["message"])
}

func TestMessagesHandler_UpstreamStatusErrorPassesThroughStatus(t *testing.T) {
	client := &fakeProviderClient{proxyErr: &providers.UpstreamStatusError{Status: http.StatusTooManyRequests}}
	svc := newTestService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5", Reason: "test"}},
		providers.ProviderAnthropic, client,
	)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestMessagesHandler_UnknownErrorReturns502(t *testing.T) {
	client := &fakeProviderClient{proxyErr: errors.New("boom: transport exploded")}
	svc := newTestService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5", Reason: "test"}},
		providers.ProviderAnthropic, client,
	)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestMessagesHandler_HappyPathServesUpstreamResponse(t *testing.T) {
	client := &fakeProviderClient{proxyStatus: http.StatusOK, proxyBody: `{"id":"msg_1","type":"message"}`}
	svc := newTestService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5", Reason: "test_reason"}},
		providers.ProviderAnthropic, client,
	)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"id":"msg_1","type":"message"}`, rec.Body.String())
	assert.Equal(t, "claude-sonnet-4-5", rec.Header().Get("x-router-model"))
	assert.Equal(t, providers.ProviderAnthropic, rec.Header().Get("x-router-provider"))
	assert.Equal(t, "test_reason", rec.Header().Get("x-router-decision"))
}

func TestMessagesHandler_AgentShadowDoesNotUpsertRouterUser(t *testing.T) {
	users := &countingUserRepository{}
	authSvc := auth.NewService(nil, nil, nil, users, nil, nil, nil)
	provider := &fakeProviderClient{proxyBody: `{"id":"msg_eval","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":3}}`}
	svc := newTestService(&fakeRouter{}, providers.ProviderAnthropic, provider)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("router_installation", &auth.Installation{ID: uuid.NewString()})
		ctx := context.WithValue(c.Request.Context(), proxy.AgentShadowEvalContextKey{}, proxy.AgentShadowEvaluation{
			Model: "claude-opus-4-8", RolloutID: "pilot-1", StateID: "state-1",
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	engine.POST("/v1/messages", anthropicapi.MessagesHandler(svc, authSvc))
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"{\"account_uuid\":\"real-account\",\"email\":\"person@example.com\"}"},"max_tokens":4096}`)

	recorder := postMessages(engine, body)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Zero(t, users.upserts, "eval traffic must not mutate router-user identity state")
}

// --- RouteHandler (/v1/route) ---

func routeEngine(svc *proxy.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/route", anthropicapi.RouteHandler(svc))
	return engine
}

func TestRouteHandler_InvalidRoutingKnobsReturns400(t *testing.T) {
	svc := newTestService(&fakeRouter{err: cluster.ErrInvalidRoutingKnobs}, "", nil)
	engine := routeEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestRouteHandler_GenericRoutingErrorReturns502(t *testing.T) {
	svc := newTestService(&fakeRouter{err: errors.New("scorer exploded")}, "", nil)
	engine := routeEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestRouteHandler_RequestTooLarge(t *testing.T) {
	svc := newTestService(&fakeRouter{}, "", nil)
	engine := routeEngine(svc)

	oversized := bytes.Repeat([]byte("a"), 10*1024*1024+1)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewReader(oversized)))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestRouteHandler_HappyPathReturnsDecision(t *testing.T) {
	decision := router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cheap_and_cheerful"}
	svc := newTestService(&fakeRouter{decision: decision}, "", nil)
	engine := routeEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "claude-haiku-4-5", got["model"])
	assert.Equal(t, providers.ProviderAnthropic, got["provider"])
	assert.Equal(t, "cheap_and_cheerful", got["reason"])
}

func TestRouteHandler_ForwardsAuthorizationForProviderEligibility(t *testing.T) {
	decision := router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}
	var got router.Request
	svc := newTestService(&fakeRouter{decision: decision, got: &got}, providers.ProviderAnthropic, &fakeProviderClient{}).
		WithByokOnly(true)
	engine := routeEngine(svc)
	req := httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewReader([]byte(validAnthropicBody)))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-claude-code-token")
	rec := httptest.NewRecorder()

	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, got.EnabledProviders, providers.ProviderAnthropic)
}

func previewRouteEngine(svc *proxy.Service, authorized bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if authorized {
			c.Set("router_installation", &auth.Installation{ID: "eval-installation", PolicyHeaderOverridesEnabled: true})
		}
		c.Request = c.Request.WithContext(router.WithStrategy(c.Request.Context(), router.StrategyHMM))
		c.Next()
	})
	engine.POST("/v1/route/preview", anthropicapi.PreviewRouteHandler(svc))
	return engine
}

func TestPreviewRouteHandler_RejectsInstallationWithoutEvalAuthorization(t *testing.T) {
	engine := previewRouteEngine(newTestService(&fakeRouter{}, "", nil), false)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/route/preview", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusForbidden, recorder.Code)
	errObj := errorEnvelope(t, recorder.Body.Bytes())
	assert.Equal(t, "permission_error", errObj["type"])
}

func TestPreviewRouteHandler_ReturnsFrozenPolicyPlan(t *testing.T) {
	previewer := &fakeRoutePreviewer{result: policy.PreviewResult{
		SchemaVersion:        policy.SchemaVersionV1,
		PolicyArtifactID:     "hmm-prod",
		PolicyArtifactSHA256: "sha256:artifact",
		RosterSHA256:         "sha256:roster",
		EligibleRosterIDs:    []string{"anthropic/claude-opus-4-8", "openai/gpt-5.5"},
	}}
	svc := newTestService(&fakeRouter{}, "", nil).WithPolicyStrategy(policy.StrategySpec{
		Strategy: router.StrategyHMM,
		Router:   previewer,
	})
	engine := previewRouteEngine(svc, true)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/route/preview", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"policy_artifact_sha256":"sha256:artifact"`)
	assert.Contains(t, recorder.Body.String(), `"eligible_roster_ids":["anthropic/claude-opus-4-8","openai/gpt-5.5"]`)
	require.NotNil(t, previewer.got)
	assert.False(t, previewer.got.TrainingAllowed)
}

// --- RouteAnthropicRequest InstallationID forwarding ---
//
// RouteAnthropicRequest is the dry-run /v1/route entry. It must forward
// InstallationID through the same uuid.Parse gate as ProxyMessages: any invalid
// string in the auth-stashed context value collapses to "" (so the HMM
// sidecar's `omitempty` drops the field), and a valid UUID round-trips.

func TestRouteAnthropicRequest_ForwardsValidInstallationIDUUIDVerbatim(t *testing.T) {
	installID := uuid.New()
	decision := router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}
	var got router.Request
	svc := newTestService(&fakeRouter{decision: decision, got: &got}, "", nil)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, installID.String())
	_, err := svc.RouteAnthropicRequest(ctx, []byte(validAnthropicBody), nil)
	require.NoError(t, err)
	assert.Equal(t, installID.String(), got.InstallationID,
		"valid installation UUID must round-trip to the HMM sidecar unchanged")
}

func TestRouteAnthropicRequest_DropsMalformedInstallationIDInsteadOfForwardingRaw(t *testing.T) {
	decision := router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}
	var got router.Request
	svc := newTestService(&fakeRouter{decision: decision, got: &got}, "", nil)

	// "not-a-uuid" is the regression case: pre-fix leaked through verbatim,
	// diverging tenant attribution from ProxyMessages (which drops invalid IDs).
	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, "not-a-uuid")
	_, err := svc.RouteAnthropicRequest(ctx, []byte(validAnthropicBody), nil)
	require.NoError(t, err)
	assert.Equal(t, "", got.InstallationID,
		"malformed installation ID must collapse to empty string so the HMM "+
			"sidecar omits the field, matching ProxyMessages's runTurnLoop behavior")
}

func TestRouteAnthropicRequest_DropsEmptyInstallationIDInsteadOfForwardingRaw(t *testing.T) {
	decision := router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}
	var got router.Request
	svc := newTestService(&fakeRouter{decision: decision, got: &got}, "", nil)

	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, "")
	_, err := svc.RouteAnthropicRequest(ctx, []byte(validAnthropicBody), nil)
	require.NoError(t, err)
	assert.Equal(t, "", got.InstallationID,
		"empty installation ID must collapse to empty string")
}

func TestRouteAnthropicRequest_DropsMissingInstallationIDInsteadOfForwardingRaw(t *testing.T) {
	decision := router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}
	var got router.Request
	svc := newTestService(&fakeRouter{decision: decision, got: &got}, "", nil)

	// No WithValue — request reached RouteAnthropicRequest with no
	// InstallationIDContextKey on the context at all.
	_, err := svc.RouteAnthropicRequest(context.Background(), []byte(validAnthropicBody), nil)
	require.NoError(t, err)
	assert.Equal(t, "", got.InstallationID,
		"missing installation ID context value must collapse to empty string")
}

// --- PassthroughHandler ---

func passthroughEngine(svc *proxy.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/messages/count_tokens", anthropicapi.PassthroughHandler(svc))
	return engine
}

func TestPassthroughHandler_NotImplementedReturns501(t *testing.T) {
	client := &fakeProviderClient{passthroughErr: providers.ErrNotImplemented}
	svc := newTestService(&fakeRouter{}, providers.ProviderAnthropic, client)
	engine := passthroughEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusNotImplemented, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestPassthroughHandler_RequestTooLarge(t *testing.T) {
	svc := newTestService(&fakeRouter{}, "", nil)
	engine := passthroughEngine(svc)

	oversized := bytes.Repeat([]byte("a"), 10*1024*1024+1)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(oversized)))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestPassthroughHandler_HappyPath(t *testing.T) {
	client := &fakeProviderClient{proxyBody: `{"input_tokens":42}`}
	svc := newTestService(&fakeRouter{}, providers.ProviderAnthropic, client)
	engine := passthroughEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"input_tokens":42}`, rec.Body.String())
}
