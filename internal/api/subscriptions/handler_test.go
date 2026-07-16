package subscriptions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/providers"
	"workweave/router/internal/subscriptions"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeEnroller struct {
	enrolled  *subscriptions.EnrollParams
	listed    []*subscriptions.Credential
	removeErr error
	removed   string
}

func (f *fakeEnroller) Enroll(_ context.Context, p subscriptions.EnrollParams) (*subscriptions.Credential, error) {
	f.enrolled = &p
	return &subscriptions.Credential{ID: "cred-1", Provider: p.Provider, AccountLabel: p.AccountLabel}, nil
}

func (f *fakeEnroller) List(context.Context, string, string) ([]*subscriptions.Credential, error) {
	return f.listed, nil
}

func (f *fakeEnroller) Remove(_ context.Context, _, _, id string) error {
	f.removed = id
	return f.removeErr
}

func init() { gin.SetMode(gin.TestMode) }

// ctxKeyInstallation mirrors the unexported gin context key WithAuth sets; the
// test seeds it directly to stand in for a valid rk_ key. If middleware renames
// the key, InstallationFrom stops resolving and these tests fail loudly.
const ctxKeyInstallation = "router_installation"

// newAuthedContext builds a gin context with an installation set, as WithAuth
// would after a valid rk_ key.
func newAuthedContext(w http.ResponseWriter, req *http.Request) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Set(ctxKeyInstallation, &auth.Installation{ID: "inst-1"})
	return c
}

func postJSON(body map[string]any) *http.Request { return postJSONAs("a@b.com", body) }

// postJSONAs builds an enrollment request whose authenticated identity is the
// X-Weave-User-Email header (empty email omits the header). The server binds to
// this header, not the body's user_email.
func postJSONAs(email string, body map[string]any) *http.Request {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/subscriptions", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if email != "" {
		req.Header.Set("X-Weave-User-Email", email)
	}
	return req
}

func TestEnroll_Success(t *testing.T) {
	fake := &fakeEnroller{}
	w := httptest.NewRecorder()
	c := newAuthedContext(w, postJSONAs("Dev@Example.com", map[string]any{
		"provider":          "claude",
		"user_email":        "Dev@Example.com",
		"access_token":      "sk-ant-oat01-token",
		"refresh_token":     "refresh-1",
		"claude_account_id": "claude-acct-1",
		"account_label":     "Max plan",
	}))
	EnrollHandler(fake)(c)

	require.Equal(t, http.StatusCreated, w.Code)
	require.NotNil(t, fake.enrolled)
	assert.Equal(t, providers.ProviderAnthropic, fake.enrolled.Provider)
	assert.Equal(t, "dev@example.com", fake.enrolled.UserEmail, "identity must come from the header, normalized")
}

func TestEnroll_RequiresIdentityHeader(t *testing.T) {
	fake := &fakeEnroller{}
	w := httptest.NewRecorder()
	c := newAuthedContext(w, postJSONAs("", map[string]any{
		"provider": "claude", "access_token": "sk-ant-oat01-x", "refresh_token": "r",
	}))
	EnrollHandler(fake)(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Nil(t, fake.enrolled, "an unidentified caller must not reach the service")
}

func TestEnroll_RejectsBodyEmailImpersonation(t *testing.T) {
	fake := &fakeEnroller{}
	w := httptest.NewRecorder()
	c := newAuthedContext(w, postJSONAs("me@b.com", map[string]any{
		"provider": "claude", "user_email": "victim@b.com",
		"access_token": "sk-ant-oat01-x", "refresh_token": "r",
	}))
	EnrollHandler(fake)(c)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Nil(t, fake.enrolled, "a body user_email other than the caller must be rejected")
}

func TestEnroll_RequiresClaudeAccountID(t *testing.T) {
	// Claude enrollment must carry the stable account id (like OpenAI's
	// chatgpt_account_id); without it the fingerprint would fall back to the
	// rotating refresh token and duplicate the pool on every fresh login.
	fake := &fakeEnroller{}
	w := httptest.NewRecorder()
	c := newAuthedContext(w, postJSONAs("a@b.com", map[string]any{
		"provider": "claude", "access_token": "sk-ant-oat01-x", "refresh_token": "r",
	}))
	EnrollHandler(fake)(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Nil(t, fake.enrolled, "a Claude enrollment without claude_account_id must be rejected")
}

func TestEnroll_RejectsBadTokenShapes(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"router key as access token", map[string]any{
			"provider": "claude", "user_email": "a@b.com", "access_token": "rk_abc", "refresh_token": "r",
		}},
		{"claude non-oat token", map[string]any{
			"provider": "claude", "user_email": "a@b.com", "access_token": "sk-ant-api-key", "refresh_token": "r", "claude_account_id": "claude-acct-1",
		}},
		{"openai missing account id", map[string]any{
			"provider": "chatgpt", "user_email": "a@b.com", "access_token": "jwt", "refresh_token": "r",
		}},
		{"missing refresh token", map[string]any{
			"provider": "claude", "user_email": "a@b.com", "access_token": "sk-ant-oat01-x",
		}},
		{"unknown provider", map[string]any{
			"provider": "gemini", "user_email": "a@b.com", "access_token": "x", "refresh_token": "r",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeEnroller{}
			w := httptest.NewRecorder()
			c := newAuthedContext(w, postJSON(tc.body))
			EnrollHandler(fake)(c)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Nil(t, fake.enrolled, "invalid enrollment must not reach the service")
		})
	}
}

func TestEnroll_Unauthenticated(t *testing.T) {
	fake := &fakeEnroller{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = postJSON(map[string]any{"provider": "claude"})
	EnrollHandler(fake)(c)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// removeEngine wires RemoveHandler behind a route that seeds the installation,
// so the DELETE's no-body status flushes exactly as it would in production.
func removeEngine(fake Enroller) *gin.Engine {
	e := gin.New()
	e.DELETE("/v1/subscriptions/:id", func(c *gin.Context) {
		c.Set(ctxKeyInstallation, &auth.Installation{ID: "inst-1"})
		RemoveHandler(fake)(c)
	})
	return e
}

func TestRemove_NotFound(t *testing.T) {
	fake := &fakeEnroller{removeErr: subscriptions.ErrCredentialNotFound}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/subscriptions/cred-x?user_email=a@b.com", nil)
	req.Header.Set("X-Weave-User-Email", "a@b.com")
	removeEngine(fake).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRemove_Success(t *testing.T) {
	fake := &fakeEnroller{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/subscriptions/cred-1?user_email=a@b.com", nil)
	req.Header.Set("X-Weave-User-Email", "a@b.com")
	removeEngine(fake).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "cred-1", fake.removed)
}

func TestRemove_RejectsQueryEmailImpersonation(t *testing.T) {
	fake := &fakeEnroller{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/v1/subscriptions/cred-1?user_email=victim@b.com", nil)
	req.Header.Set("X-Weave-User-Email", "me@b.com")
	removeEngine(fake).ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, fake.removed, "a query user_email other than the caller must be rejected")
}
