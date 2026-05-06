package auth

import (
	"bytes"
	"fmt"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/tink"
)

// Encryptor handles AES-256-GCM encryption and decryption using Google Tink.
type Encryptor interface {
	Encrypt(plaintext []byte) (ciphertext []byte, err error)
	Decrypt(ciphertext []byte) (plaintext []byte, err error)
}

type tinkEncryptor struct {
	aead tink.AEAD
}

// NewTinkEncryptor creates an Encryptor from a Tink keyset JSON string.
// Generate a keyset with: tinkey create-keyset --key-template AES256_GCM --out-format json
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

// Encrypt encrypts plaintext using AES-256-GCM. Tink handles nonce generation.
func (e *tinkEncryptor) Encrypt(plaintext []byte) ([]byte, error) {
	return e.aead.Encrypt(plaintext, nil)
}

// Decrypt decrypts ciphertext using AES-256-GCM.
func (e *tinkEncryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	return e.aead.Decrypt(ciphertext, nil)
}

// NoOpEncryptor is a passthrough for local development without encryption.
type NoOpEncryptor struct{}

// Encrypt returns plaintext unchanged.
func (NoOpEncryptor) Encrypt(plaintext []byte) ([]byte, error) { return plaintext, nil }

// Decrypt returns ciphertext unchanged.
func (NoOpEncryptor) Decrypt(ciphertext []byte) ([]byte, error) { return ciphertext, nil }
