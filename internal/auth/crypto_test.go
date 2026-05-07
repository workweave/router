package auth

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
)

// TestAADFor_GoldenBytes pins the AAD format. The matching test in
// backend/internal/app/weaverouter/crypto_test.go asserts the same expected
// bytes; drift on either side fails CI before any cross-repo decrypt breaks.
func TestAADFor_GoldenBytes(t *testing.T) {
	assert.Equal(t,
		[]byte("test-org\x00anthropic"),
		aadFor("test-org", "anthropic"),
	)
}

func TestTinkEncryptor_RoundTrip(t *testing.T) {
	enc := newTestEncryptor(t)
	plaintext := []byte("sk-ant-test-key-1234567890")
	const externalID = "org_test"
	const provider = "anthropic"

	ciphertext, err := enc.Encrypt(plaintext, externalID, provider)
	require.NoError(t, err)
	got, err := enc.Decrypt(ciphertext, externalID, provider)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

// TestTinkEncryptor_DecryptRejectsMismatchedAAD is the core security
// property: a ciphertext from one (org, provider) is unreadable elsewhere.
func TestTinkEncryptor_DecryptRejectsMismatchedAAD(t *testing.T) {
	enc := newTestEncryptor(t)
	ciphertext, err := enc.Encrypt([]byte("sk-secret"), "org_alice", "anthropic")
	require.NoError(t, err)

	cases := []struct {
		name       string
		externalID string
		provider   string
	}{
		{"different externalID", "org_bob", "anthropic"},
		{"different provider", "org_alice", "openai"},
		{"both different", "org_bob", "openai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := enc.Decrypt(ciphertext, tc.externalID, tc.provider)
			assert.Error(t, err)
		})
	}
}

func newTestEncryptor(t *testing.T) Encryptor {
	t.Helper()

	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buf)))

	enc, err := NewTinkEncryptor(buf.String())
	require.NoError(t, err)
	return enc
}
