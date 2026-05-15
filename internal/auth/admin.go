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

// AdminSessionCookieName is the HttpOnly cookie name for admin sessions.
const AdminSessionCookieName = "router_admin_session"

// DefaultAdminSessionTTL is the admin session validity.
const DefaultAdminSessionTTL = 7 * 24 * time.Hour

// ErrAdminSessionInvalid is returned when a session token fails to verify or the password is wrong.
var ErrAdminSessionInvalid = errors.New("admin session invalid")

// ErrAdminPasswordNotConfigured is returned when admin auth is attempted but no ROUTER_ADMIN_PASSWORD is set.
var ErrAdminPasswordNotConfigured = errors.New("admin password not configured")

// ErrAdminLoginRateLimited is returned when per-IP failures exceed the threshold.
var ErrAdminLoginRateLimited = errors.New("admin login rate limited")

// Per-IP brute-force lockout: 5 failures inside 5 minutes triggers the limiter; entries self-expire off the LRU.
const (
	adminLoginMaxFailures = 5
	adminLoginFailureTTL  = 5 * time.Minute
)

// adminSessionLabel is mixed into the signing key derivation so the admin password can never collide
// with a different signing context (e.g. future CSRF tokens or webhook signatures).
const adminSessionLabel = "router-admin-session-v1"

// AdminInstallationExternalID is the external ID for the admin-owned installation.
const AdminInstallationExternalID = "__router_admin__"

// AdminInstallationName is the display name for the auto-created admin installation.
const AdminInstallationName = "Dashboard"

// adminClaims is the signed session-token payload.
type adminClaims struct {
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// AdminPrincipal is returned by VerifyAdminSession. Carries no installation details —
// admin sessions are operator identities, not customer identities.
type AdminPrincipal struct {
	Subject   string
	ExpiresAt time.Time
}

// WithAdminPassword installs the operator password for minting and verifying admin sessions.
// Empty string disables admin login.
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

// AdminLoginEnabled reports whether an admin password is configured.
func (s *Service) AdminLoginEnabled() bool {
	return s.adminPassword != "" && len(s.adminSessionKey) == sha256.Size
}

// VerifyAdminPassword uses constant-time comparison to avoid timing oracles.
func (s *Service) VerifyAdminPassword(password string) error {
	if !s.AdminLoginEnabled() {
		return ErrAdminPasswordNotConfigured
	}
	if subtle.ConstantTimeCompare([]byte(s.adminPassword), []byte(password)) == 1 {
		return nil
	}
	return ErrAdminSessionInvalid
}

// VerifyAdminPasswordFromIP wraps VerifyAdminPassword with per-IP brute-force throttling.
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

// IssueAdminSession returns a signed session token. Format: `<payload>.<hmac>`.
// Rotating ROUTER_ADMIN_PASSWORD changes the signing key and invalidates all cookies.
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

// EnsureAdminInstallation returns the singleton admin installation, creating it on first call.
// On concurrent first-hit, the loser re-lists and returns the winner's row.
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
	// Lost a concurrent race.
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

// VerifyAdminSession parses and authenticates a session cookie token.
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
