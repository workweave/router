// Package feedback defines the signed, no-login token that authorizes a single
// router request's feedback link (`/f/<token>`). The token is the sole
// credential for the public feedback page: it is minted by the router when a
// request is routed and verified by the router when the feedback page loads or
// a rating is submitted. The Weave backend never sees the secret.
//
// The package is pure (HMAC + base64 only, no I/O) so it can be shared by the
// proxy (mint), the API handlers (verify), and the composition root (Signer
// construction) without violating the layering rules.
package feedback

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrInvalidToken is returned when a token is malformed or its signature does
// not verify. Handlers map this to HTTP 404 (the link is bogus).
var ErrInvalidToken = errors.New("feedback: invalid token")

// ErrExpiredToken is returned when a token's signature verifies but its
// expiry has passed. Handlers map this to HTTP 410 (the link is stale).
var ErrExpiredToken = errors.New("feedback: expired token")

// Claims is the verified payload carried by a feedback-link token. All fields
// are opaque to this package; the router uses InstallationID + RequestID to
// look up routing context and ExternalID + RouterUserID to attribute the
// emitted router.feedback span to a Weave organization and user.
type Claims struct {
	InstallationID string `json:"iid"`
	ExternalID     string `json:"eid"`
	RequestID      string `json:"rid"`
	RouterUserID   string `json:"uid,omitempty"`
	// ExpiresAt is a Unix timestamp (seconds). Zero means "never expires".
	ExpiresAt int64 `json:"exp"`
}

// Signer mints and verifies feedback-link tokens with a single HMAC-SHA256
// secret. A nil *Signer means the feature is disabled: Mint returns "" and
// Verify returns ErrInvalidToken, so callers can treat "no signer" and "bad
// token" identically. Safe for concurrent use.
type Signer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewSigner returns a Signer over the given secret with the given link TTL.
// Returns nil when secret is empty so the composition root can leave the
// feature unwired without a sentinel. A non-positive ttl mints tokens that
// never expire.
func NewSigner(secret string, ttl time.Duration) *Signer {
	if secret == "" {
		return nil
	}
	return &Signer{secret: []byte(secret), ttl: ttl, now: time.Now}
}

// Mint returns a signed, URL-safe token encoding the claims for one request.
// The expiry is computed from the Signer's TTL at mint time. Nil receiver
// returns "" so disabled deployments simply omit the feedback link.
func (s *Signer) Mint(installationID, externalID, requestID, routerUserID string) string {
	if s == nil {
		return ""
	}
	var exp int64
	if s.ttl > 0 {
		exp = s.now().Add(s.ttl).Unix()
	}
	claims := Claims{
		InstallationID: installationID,
		ExternalID:     externalID,
		RequestID:      requestID,
		RouterUserID:   routerUserID,
		ExpiresAt:      exp,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		// Claims is a fixed struct of strings/int — marshaling cannot fail in
		// practice. Returning "" degrades to "no link" rather than panicking
		// on the request path.
		return ""
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + s.sign(body)
}

// Verify checks a token's signature and expiry and returns its claims.
// Returns ErrInvalidToken for malformed input or a signature mismatch and
// ErrExpiredToken when the signature is valid but the link has expired.
func (s *Signer) Verify(token string) (Claims, error) {
	if s == nil {
		return Claims{}, ErrInvalidToken
	}
	body, sig, ok := strings.Cut(token, ".")
	if !ok || body == "" || sig == "" {
		return Claims{}, ErrInvalidToken
	}
	expected := s.sign(body)
	// Constant-time compare so a timing side-channel can't be used to forge a
	// signature byte by byte.
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return Claims{}, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, ErrInvalidToken
	}
	if claims.ExpiresAt != 0 && s.now().Unix() >= claims.ExpiresAt {
		return Claims{}, ErrExpiredToken
	}
	return claims, nil
}

// sign returns the URL-safe base64 HMAC-SHA256 of body under the secret.
func (s *Signer) sign(body string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
