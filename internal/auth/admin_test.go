package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adminTestPassword is the operator password used across the admin session tests.
const adminTestPassword = "correct-horse-battery-staple"

// mutableClock is a Clock whose returned time can be advanced mid-test, needed for the
// expired-token case where IssueAdminSession and VerifyAdminSession must observe different "now"s.
type mutableClock struct {
	t time.Time
}

func (c *mutableClock) now() time.Time {
	return c.t
}

func newAdminTestService(t *testing.T) (*Service, *mutableClock) {
	t.Helper()
	clock := &mutableClock{t: time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)}
	svc := NewService(nil, nil, nil, nil, NoOpAPIKeyCache{}, nil, clock.now).
		WithAdminPassword(adminTestPassword)
	return svc, clock
}

func TestAdminSession_IssueAndVerifyRoundTrip(t *testing.T) {
	svc, _ := newAdminTestService(t)

	token, expiresAt, err := svc.IssueAdminSession()
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	principal, err := svc.VerifyAdminSession(token)
	require.NoError(t, err, "a freshly-issued session must verify successfully")
	require.NotNil(t, principal)
	assert.Equal(t, "admin", principal.Subject)
	assert.WithinDuration(t, expiresAt, principal.ExpiresAt, time.Second)
}

func TestAdminSession_ExpiredTokenIsRejected(t *testing.T) {
	svc, clock := newAdminTestService(t)

	token, _, err := svc.IssueAdminSession()
	require.NoError(t, err)

	// Move the clock past the session TTL.
	clock.t = clock.t.Add(DefaultAdminSessionTTL + time.Minute)

	_, err = svc.VerifyAdminSession(token)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAdminSessionInvalid, "an expired session token must be rejected")
}

func TestAdminSession_TamperedSignatureIsRejected(t *testing.T) {
	svc, _ := newAdminTestService(t)

	token, _, err := svc.IssueAdminSession()
	require.NoError(t, err)

	payload, sig, ok := strings.Cut(token, ".")
	require.True(t, ok)

	t.Run("flipped signature byte", func(t *testing.T) {
		sigBytes, decodeErr := base64.RawURLEncoding.DecodeString(sig)
		require.NoError(t, decodeErr)
		require.NotEmpty(t, sigBytes)
		tampered := append([]byte{}, sigBytes...)
		tampered[0] ^= 0xFF
		tamperedSig := base64.RawURLEncoding.EncodeToString(tampered)

		_, verifyErr := svc.VerifyAdminSession(payload + "." + tamperedSig)
		require.Error(t, verifyErr)
		assert.ErrorIs(t, verifyErr, ErrAdminSessionInvalid)
	})

	t.Run("re-signed with wrong key", func(t *testing.T) {
		otherSvc, _ := newAdminTestService(t)
		otherSvc.WithAdminPassword("a-completely-different-password")

		forgedToken, _, issueErr := otherSvc.IssueAdminSession()
		require.NoError(t, issueErr)
		forgedPayload, forgedSig, cutOK := strings.Cut(forgedToken, ".")
		require.True(t, cutOK)

		// Splice the original payload with a signature produced under a different key.
		_, verifyErr := svc.VerifyAdminSession(payload + "." + forgedSig)
		require.Error(t, verifyErr)
		assert.ErrorIs(t, verifyErr, ErrAdminSessionInvalid)

		// Sanity: the forged token doesn't verify against its own issuer's mismatched payload either
		// when checked by svc (different key), proving the signature really is key-bound.
		_, verifyErr2 := svc.VerifyAdminSession(forgedPayload + "." + forgedSig)
		require.Error(t, verifyErr2)
		assert.ErrorIs(t, verifyErr2, ErrAdminSessionInvalid)
	})
}

func TestAdminSession_MalformedTokensAreRejected(t *testing.T) {
	svc, _ := newAdminTestService(t)

	cases := []string{
		"",
		"no-dot-in-here",
		".missing-payload",
		"missing-signature.",
		"not-base64!!.not-base64!!",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			_, err := svc.VerifyAdminSession(tc)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrAdminSessionInvalid)
		})
	}
}

func TestVerifyAdminPassword_ConstantTimeCompare(t *testing.T) {
	svc, _ := newAdminTestService(t)

	require.NoError(t, svc.VerifyAdminPassword(adminTestPassword))

	err := svc.VerifyAdminPassword("wrong-password")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAdminSessionInvalid)
}

func TestVerifyAdminPassword_NotConfigured(t *testing.T) {
	clock := &mutableClock{t: time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)}
	svc := NewService(nil, nil, nil, nil, NoOpAPIKeyCache{}, nil, clock.now)

	assert.False(t, svc.AdminLoginEnabled())

	err := svc.VerifyAdminPassword(adminTestPassword)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAdminPasswordNotConfigured)

	_, _, issueErr := svc.IssueAdminSession()
	require.Error(t, issueErr)
	assert.ErrorIs(t, issueErr, ErrAdminPasswordNotConfigured)

	_, verifyErr := svc.VerifyAdminSession("anything.anything")
	require.Error(t, verifyErr)
	assert.ErrorIs(t, verifyErr, ErrAdminPasswordNotConfigured)
}

func TestVerifyAdminPasswordFromIP_LockoutAfterFiveFailures(t *testing.T) {
	svc, _ := newAdminTestService(t)
	const ip = "203.0.113.7"

	// Four failures must not yet trip the lockout.
	for i := 0; i < adminLoginMaxFailures-1; i++ {
		err := svc.VerifyAdminPasswordFromIP(ip, "wrong-password")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrAdminSessionInvalid,
			"failures under the threshold must report invalid credentials, not rate limiting")
	}

	// The 5th failure trips the lockout counter, but the failing attempt itself still
	// reports invalid credentials (the limiter blocks *subsequent* attempts).
	err := svc.VerifyAdminPasswordFromIP(ip, "wrong-password")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAdminSessionInvalid)

	// A 6th attempt, even with the correct password, must now be rate limited.
	err = svc.VerifyAdminPasswordFromIP(ip, adminTestPassword)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAdminLoginRateLimited,
		"the 6th attempt within the window must be rejected by the per-IP lockout, not evaluated")
}

func TestVerifyAdminPasswordFromIP_SuccessResetsFailureCount(t *testing.T) {
	svc, _ := newAdminTestService(t)
	const ip = "203.0.113.8"

	for i := 0; i < adminLoginMaxFailures-1; i++ {
		err := svc.VerifyAdminPasswordFromIP(ip, "wrong-password")
		require.Error(t, err)
	}

	// A successful login before hitting the threshold must reset the counter.
	require.NoError(t, svc.VerifyAdminPasswordFromIP(ip, adminTestPassword))

	// Failures should now start counting from zero again, so adminLoginMaxFailures-1
	// more failures must still not trigger the lockout.
	for i := 0; i < adminLoginMaxFailures-1; i++ {
		err := svc.VerifyAdminPasswordFromIP(ip, "wrong-password")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrAdminSessionInvalid,
			"the failure counter must have been reset by the earlier success")
	}
}

func TestVerifyAdminPasswordFromIP_DifferentIPsAreIndependent(t *testing.T) {
	svc, _ := newAdminTestService(t)
	const ipA = "203.0.113.9"
	const ipB = "203.0.113.10"

	for i := 0; i < adminLoginMaxFailures; i++ {
		_ = svc.VerifyAdminPasswordFromIP(ipA, "wrong-password")
	}

	err := svc.VerifyAdminPasswordFromIP(ipA, adminTestPassword)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAdminLoginRateLimited, "ipA must be locked out")

	// A different IP must be unaffected by ipA's lockout.
	err = svc.VerifyAdminPasswordFromIP(ipB, adminTestPassword)
	require.NoError(t, err, "ipB has never failed and must not be rate limited")
}

func TestVerifyAdminPasswordFromIP_NotConfigured(t *testing.T) {
	clock := &mutableClock{t: time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)}
	svc := NewService(nil, nil, nil, nil, NoOpAPIKeyCache{}, nil, clock.now)

	err := svc.VerifyAdminPasswordFromIP("203.0.113.11", adminTestPassword)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAdminPasswordNotConfigured)
}

// TestAdminSession_SigningKeyDerivedFromPassword pins down that WithAdminPassword derives a
// distinct signing key per password so that rotating the password invalidates existing sessions,
// per the documented contract on IssueAdminSession.
func TestAdminSession_RotatingPasswordInvalidatesExistingSessions(t *testing.T) {
	svc, _ := newAdminTestService(t)

	token, _, err := svc.IssueAdminSession()
	require.NoError(t, err)

	svc.WithAdminPassword("a-brand-new-password")

	_, err = svc.VerifyAdminSession(token)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAdminSessionInvalid))
}
