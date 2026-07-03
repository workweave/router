// Package feedback defines the signed, no-login token that authorizes a
// single router request's feedback link (`/f/<token>`). It's the sole
// credential for the public feedback page — minted at routing time, verified
// on page load/rating submit — and the Weave backend never sees the secret.
// Pure (HMAC + base64, no I/O) so it can be shared across layers.
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

// Claims is the verified payload of a feedback-link token. InstallationID +
// RequestID look up routing context; ExternalID + RouterUserID attribute the
// emitted router.feedback span to a Weave org and user.
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
// Verify returns ErrInvalidToken, so "no signer" and "bad token" are
// indistinguishable to callers. Safe for concurrent use.
type Signer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewSigner returns a Signer over the given secret and link TTL. Returns nil
// when secret is empty, letting the composition root leave the feature
// unwired without a sentinel. Non-positive ttl mints tokens that never expire.
func NewSigner(secret string, ttl time.Duration) *Signer {
	if secret == "" {
		return nil
	}
	return &Signer{secret: []byte(secret), ttl: ttl, now: time.Now}
}

// Mint returns a signed, URL-safe token encoding the claims for one request,
// with expiry computed from the Signer's TTL. Nil receiver returns "" so
// disabled deployments simply omit the feedback link.
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
		// Marshaling a fixed string/int struct can't practically fail; degrade
		// to "no link" rather than panic on the request path.
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
