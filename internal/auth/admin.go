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
)

// AdminSessionCookieName is the HttpOnly cookie name used to carry signed
// admin sessions. Kept stable across versions for users who set
// ROUTER_ADMIN_PASSWORD and bookmark the dashboard.
const AdminSessionCookieName = "router_admin_session"

// DefaultAdminSessionTTL is how long a signed admin session is valid before
// the holder must re-authenticate. Rotate the env password to invalidate
// outstanding cookies sooner.
const DefaultAdminSessionTTL = 7 * 24 * time.Hour

// ErrAdminSessionInvalid is returned when a session token fails to verify
// (bad signature, malformed, or expired) or the supplied password does not
// match the configured admin password.
var ErrAdminSessionInvalid = errors.New("admin session invalid")

// ErrAdminPasswordNotConfigured is returned when an admin auth operation is
// attempted but no ROUTER_ADMIN_PASSWORD was provided to the service. The
// dashboard treats this as "admin login disabled".
var ErrAdminPasswordNotConfigured = errors.New("admin password not configured")

// adminSessionLabel is mixed into the signing key derivation so the same
// admin password can never collide with a different signing context (e.g.
// future CSRF tokens or webhook signatures).
const adminSessionLabel = "router-admin-session-v1"

// AdminInstallationExternalID is the well-known externalID for the
// installation owned by the dashboard admin. Self-hosted deploys typically
// have exactly one installation; this constant lets the admin handlers
// look it up (and create it on first use) without an additional config
// flag. rk_ keys issued from the dashboard belong to this installation.
const AdminInstallationExternalID = "__router_admin__"

// AdminInstallationName is the display name for the admin-owned
// installation when it is auto-created on first dashboard use.
const AdminInstallationName = "Dashboard"

// adminClaims is the payload encoded inside a signed session token. Kept
// minimal — the only thing that matters is "this cookie came from someone
// who knew the admin password at issue-time, and it hasn't expired".
type adminClaims struct {
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// AdminPrincipal is what VerifyAdminSession returns on success. Deliberately
// empty of installation details — admin sessions are "operator of this
// router" identities, not "customer" identities.
type AdminPrincipal struct {
	Subject   string
	ExpiresAt time.Time
}

// WithAdminPassword installs the operator password used to mint and verify
// admin session cookies. Pass an empty string to disable admin login.
func (s *Service) WithAdminPassword(password string) *Service {
	s.adminPassword = password
	if password == "" {
		s.adminSessionKey = nil
		return s
	}
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write([]byte(adminSessionLabel))
	s.adminSessionKey = mac.Sum(nil)
	return s
}

// AdminLoginEnabled reports whether the service was configured with an admin
// password. Handlers use this to surface a clean 503 instead of a misleading
// 401 when self-hosters forgot to set the env var.
func (s *Service) AdminLoginEnabled() bool {
	return s.adminPassword != "" && len(s.adminSessionKey) == sha256.Size
}

// VerifyAdminPassword returns nil iff password equals the configured admin
// password. Constant-time compare to avoid trivial timing oracles.
func (s *Service) VerifyAdminPassword(password string) error {
	if !s.AdminLoginEnabled() {
		return ErrAdminPasswordNotConfigured
	}
	if subtle.ConstantTimeCompare([]byte(s.adminPassword), []byte(password)) == 1 {
		return nil
	}
	return ErrAdminSessionInvalid
}

// IssueAdminSession returns a signed session token plus its expiry. Tokens
// are encoded as `<base64url(payload)>.<base64url(hmac)>` — no JWT
// dependency, no session row. Rotating ROUTER_ADMIN_PASSWORD changes the
// signing key and invalidates every outstanding cookie automatically.
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

// EnsureAdminInstallation returns the singleton admin-owned installation,
// creating it on first call. Self-hosted deploys land here through the
// dashboard's first interaction (issuing a key, saving a provider key,
// etc.) so operators don't have to seed an installation by hand before
// the UI works.
func (s *Service) EnsureAdminInstallation(ctx context.Context) (*Installation, error) {
	existing, err := s.installations.ListForExternalID(ctx, AdminInstallationExternalID)
	if err != nil {
		return nil, err
	}
	for _, inst := range existing {
		if inst != nil && inst.DeletedAt == nil {
			return inst, nil
		}
	}
	return s.installations.Create(ctx, CreateInstallationParams{
		ExternalID: AdminInstallationExternalID,
		Name:       AdminInstallationName,
	})
}

// VerifyAdminSession parses and authenticates a session cookie value.
// Returns ErrAdminSessionInvalid for anything malformed, badly signed, or
// expired so the middleware can map it cleanly to 401.
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
