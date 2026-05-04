package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	APITokenPrefixLen = 8
	APITokenSuffixLen = 4
)

func HashAPIKeySHA256(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func APITokenFingerprint(token string) (hashHex, prefix, suffix string) {
	hashHex = HashAPIKeySHA256(token)
	if len(token) <= APITokenPrefixLen+APITokenSuffixLen {
		return hashHex, token, ""
	}
	return hashHex, token[:APITokenPrefixLen], token[len(token)-APITokenSuffixLen:]
}
