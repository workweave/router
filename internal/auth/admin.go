package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// AdminSessionCookieName is the HttpOnly cookie name carrying signed admin sessions. Stable across versions.
const AdminSessionCookieName = "router_admin_session"

// DefaultAdminSessionTTL is the admin session validity. Rotate the env password to invalidate cookies sooner.
const DefaultAdminSessionTTL = 7 * 24 * time.Hour

// ErrAdminSessionInvalid is returned when a session token fails to verify or the password is wrong.
var ErrAdminSessionInvalid = errors.New("admin session invalid")

// ErrAdminPasswordNotConfigured is returned when admin auth is attempted but no ROUTER_ADMIN_PASSWORD is set.
var ErrAdminPasswordNotConfigured = errors.New("admin password not configured")

// ErrAdminLoginRateLimited is returned when per-IP failures exceed the threshold. Handlers map it to HTTP 429
// so a brute-force attempt against ROUTER_ADMIN_PASSWORD pays a real cost between guesses.
var ErrAdminLoginRateLimited = errors.New("admin login rate limited")

// Per-IP brute-force lockout: 5 failures inside 5 minutes triggers the limiter; entries self-expire off the LRU.
const (
	adminLoginMaxFailures = 5
	adminLoginFailureTTL  = 5 * time.Minute
)

// adminSessionLabel is mixed into the signing key derivation so the admin password can never collide
// with a different signing context (e.g. future CSRF tokens or webhook signatures).
const adminSessionLabel = "router-admin-session-v1"

// AdminInstallationExternalID is the well-known externalID for the dashboard-admin-owned installation.
// rk_ keys issued from the dashboard belong to this installation; the admin handlers create it on first use.
const AdminInstallationExternalID = "__router_admin__"

// AdminInstallationName is the display name for the auto-created admin installation.
const AdminInstallationName = "Dashboard"

// adminClaims is the payload inside a signed session token. Minimal: proves "knew admin password at issue-time and not expired".
type adminClaims struct {
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// AdminPrincipal is what VerifyAdminSession returns. Deliberately no installation details — admin
// sessions are operator-of-this-router identities, not customer identities.
type AdminPrincipal struct {
	Subject   string
	ExpiresAt time.Time
}

// WithAdminPassword installs the operator password used to mint and verify admin session cookies. Empty disables admin login.
func (s *Service) WithAdminPassword(password string) *Service {
	s.adminPassword = password
	if password == "" {
		s.adminSessionKey = nil
		return s
	}
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write([]byte(adminSessionLabel))
	s.adminSessionKey = mac.Sum(nil)
	s.adminLoginMu.Lock()
	if s.adminLoginFailures == nil {
		// 1024 unique IPs is plenty for one self-hosted router; LRU evicts oldest so a flood can't blow up memory.
		s.adminLoginFailures = expirable.NewLRU[string, int](1024, nil, adminLoginFailureTTL)
	}
	s.adminLoginMu.Unlock()
	return s
}

// AdminLoginEnabled reports whether the service has an admin password configured. Handlers use this
// to surface a clean 503 instead of a misleading 401 when self-hosters forgot to set the env var.
func (s *Service) AdminLoginEnabled() bool {
	return s.adminPassword != "" && len(s.adminSessionKey) == sha256.Size
}

// VerifyAdminPassword returns nil iff password matches. Constant-time compare to avoid timing oracles.
// HTTP callers should prefer VerifyAdminPasswordFromIP, which also throttles brute-force attempts.
func (s *Service) VerifyAdminPassword(password string) error {
	if !s.AdminLoginEnabled() {
		return ErrAdminPasswordNotConfigured
	}
	if subtle.ConstantTimeCompare([]byte(s.adminPassword), []byte(password)) == 1 {
		return nil
	}
	return ErrAdminSessionInvalid
}

// VerifyAdminPasswordFromIP wraps VerifyAdminPassword with a per-IP failure counter. After
// adminLoginMaxFailures unsuccessful attempts inside adminLoginFailureTTL, further attempts from
// that IP return ErrAdminLoginRateLimited until the entry expires. A successful login clears the
// counter so a legitimate user who mistyped is not punished indefinitely. Pass c.ClientIP().
func (s *Service) VerifyAdminPasswordFromIP(remoteIP, password string) error {
	if !s.AdminLoginEnabled() {
		return ErrAdminPasswordNotConfigured
	}
	if s.adminLoginFailures != nil && remoteIP != "" {
		if count, ok := s.adminLoginFailures.Get(remoteIP); ok && count >= adminLoginMaxFailures {
			return ErrAdminLoginRateLimited
		}
	}
	if err := s.VerifyAdminPassword(password); err != nil {
		if s.adminLoginFailures != nil && remoteIP != "" && errors.Is(err, ErrAdminSessionInvalid) {
			count, _ := s.adminLoginFailures.Get(remoteIP)
			s.adminLoginFailures.Add(remoteIP, count+1)
		}
		return err
	}
	if s.adminLoginFailures != nil && remoteIP != "" {
		s.adminLoginFailures.Remove(remoteIP)
	}
	return nil
}

// IssueAdminSession returns a signed session token plus expiry. Format `<base64url(payload)>.<base64url(hmac)>`
// — no JWT dep, no session row. Rotating ROUTER_ADMIN_PASSWORD changes the signing key and invalidates all cookies.
func (s *Service) IssueAdminSession() (token string, expiresAt time.Time, err error) {
	if !s.AdminLoginEnabled() {
		return "", time.Time{}, ErrAdminPasswordNotConfigured
	}
	now := s.now()
	expiresAt = now.Add(DefaultAdminSessionTTL)
	claims := adminClaims{
		Subject:   "admin",
		IssuedAt:  now.Unix(),
		ExpiresAt: expiresAt.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal admin claims: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.adminSessionKey)
	mac.Write([]byte(encodedPayload))
	encodedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encodedPayload + "." + encodedSig, expiresAt, nil
}

// EnsureAdminInstallation returns the singleton admin-owned installation, creating it on first call.
//
// Concurrent first-hit requests race: both see no row, both try Create; one wins, the other hits the
// unique constraint on (name, external_id). On Create failure we re-list so the loser returns the
// winner's row instead of bubbling a 500.
func (s *Service) EnsureAdminInstallation(ctx context.Context) (*Installation, error) {
	if inst, ok, err := s.findAdminInstallation(ctx); err != nil {
		return nil, err
	} else if ok {
		return inst, nil
	}
	created, createErr := s.installations.Create(ctx, CreateInstallationParams{
		ExternalID: AdminInstallationExternalID,
		Name:       AdminInstallationName,
	})
	if createErr == nil {
		return created, nil
	}
	// Lost a concurrent race — the row exists now. Re-list and return it.
	if inst, ok, err := s.findAdminInstallation(ctx); err == nil && ok {
		return inst, nil
	}
	return nil, createErr
}

func (s *Service) findAdminInstallation(ctx context.Context) (*Installation, bool, error) {
	existing, err := s.installations.ListForExternalID(ctx, AdminInstallationExternalID)
	if err != nil {
		return nil, false, err
	}
	for _, inst := range existing {
		if inst != nil && inst.DeletedAt == nil {
			return inst, true, nil
		}
	}
	return nil, false, nil
}

// VerifyAdminSession parses and authenticates a session cookie. Returns ErrAdminSessionInvalid for
// anything malformed, badly signed, or expired so middleware can map cleanly to 401.
func (s *Service) VerifyAdminSession(token string) (*AdminPrincipal, error) {
	if !s.AdminLoginEnabled() {
		return nil, ErrAdminPasswordNotConfigured
	}
	encodedPayload, encodedSig, ok := strings.Cut(token, ".")
	if !ok || encodedPayload == "" || encodedSig == "" {
		return nil, ErrAdminSessionInvalid
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(encodedSig)
	if err != nil {
		return nil, ErrAdminSessionInvalid
	}
	mac := hmac.New(sha256.New, s.adminSessionKey)
	mac.Write([]byte(encodedPayload))
	wantSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, wantSig) {
		return nil, ErrAdminSessionInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return nil, ErrAdminSessionInvalid
	}
	var claims adminClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, ErrAdminSessionInvalid
	}
	expiresAt := time.Unix(claims.ExpiresAt, 0)
	if !s.now().Before(expiresAt) {
		return nil, ErrAdminSessionInvalid
	}
	return &AdminPrincipal{Subject: claims.Subject, ExpiresAt: expiresAt}, nil
}
