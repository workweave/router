package auth

import (
	"bytes"
	"fmt"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/tink"
)

// Encryptor encrypts/decrypts external API keys via AES-256-GCM.
// AAD binds each ciphertext to (externalID, provider).
type Encryptor interface {
	Encrypt(plaintext []byte, externalID, provider string) (ciphertext []byte, err error)
	Decrypt(ciphertext []byte, externalID, provider string) (plaintext []byte, err error)
}

type tinkEncryptor struct {
	aead tink.AEAD
}

// NewTinkEncryptor creates an Encryptor from a Tink keyset JSON.
func NewTinkEncryptor(keysetJSON string) (Encryptor, error) {
	reader := keyset.NewJSONReader(bytes.NewBufferString(keysetJSON))
	handle, err := insecurecleartextkeyset.Read(reader)
	if err != nil {
		return nil, fmt.Errorf("read keyset: %w", err)
	}
	primitive, err := aead.New(handle)
	if err != nil {
		return nil, fmt.Errorf("create AEAD primitive: %w", err)
	}
	return &tinkEncryptor{aead: primitive}, nil
}

func (e *tinkEncryptor) Encrypt(plaintext []byte, externalID, provider string) ([]byte, error) {
	return e.aead.Encrypt(plaintext, aadFor(externalID, provider))
}

func (e *tinkEncryptor) Decrypt(ciphertext []byte, externalID, provider string) ([]byte, error) {
	return e.aead.Decrypt(ciphertext, aadFor(externalID, provider))
}

// aadFor MUST stay byte-identical with the Weave-side helper at
// backend/internal/app/weaverouter/crypto.go.
func aadFor(externalID, provider string) []byte {
	return []byte(externalID + "\x00" + provider)
}

// NoOpEncryptor is a pass-through for development without encryption.
type NoOpEncryptor struct{}

func (NoOpEncryptor) Encrypt(plaintext []byte, _, _ string) ([]byte, error) {
	return plaintext, nil
}

func (NoOpEncryptor) Decrypt(ciphertext []byte, _, _ string) ([]byte, error) {
	return ciphertext, nil
}
