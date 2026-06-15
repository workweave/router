package feedback_test

import (
	"errors"
	"testing"
	"time"

	"workweave/router/internal/feedback"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSigner_MintVerifyRoundTrip(t *testing.T) {
	s := feedback.NewSigner("super-secret", time.Hour)
	require.NotNil(t, s)

	token := s.Mint("inst-1", "org-1", "req-1", "user-1")
	require.NotEmpty(t, token)

	claims, err := s.Verify(token)
	require.NoError(t, err)
	assert.Equal(t, "inst-1", claims.InstallationID)
	assert.Equal(t, "org-1", claims.ExternalID)
	assert.Equal(t, "req-1", claims.RequestID)
	assert.Equal(t, "user-1", claims.RouterUserID)
}

func TestSigner_RejectsTamperedToken(t *testing.T) {
	s := feedback.NewSigner("super-secret", time.Hour)
	token := s.Mint("inst-1", "org-1", "req-1", "")

	// Flip the last character of the signature.
	tampered := token[:len(token)-1] + flip(token[len(token)-1])
	_, err := s.Verify(tampered)
	assert.ErrorIs(t, err, feedback.ErrInvalidToken)
}

func TestSigner_RejectsForeignSecret(t *testing.T) {
	minted := feedback.NewSigner("secret-a", time.Hour).Mint("inst-1", "org-1", "req-1", "")
	_, err := feedback.NewSigner("secret-b", time.Hour).Verify(minted)
	assert.ErrorIs(t, err, feedback.ErrInvalidToken)
}

func TestSigner_RejectsExpiredToken(t *testing.T) {
	s := feedback.NewSigner("super-secret", time.Nanosecond)
	token := s.Mint("inst-1", "org-1", "req-1", "")
	time.Sleep(2 * time.Millisecond)
	_, err := s.Verify(token)
	assert.ErrorIs(t, err, feedback.ErrExpiredToken)
}

func TestSigner_NilIsDisabled(t *testing.T) {
	var s *feedback.Signer
	assert.Empty(t, s.Mint("a", "b", "c", ""))
	_, err := s.Verify("anything.anything")
	assert.True(t, errors.Is(err, feedback.ErrInvalidToken))
}

func TestNewSigner_EmptySecretReturnsNil(t *testing.T) {
	assert.Nil(t, feedback.NewSigner("", time.Hour))
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}
