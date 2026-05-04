package auth

import (
	"crypto/rand"
	"strings"
)

// APIKeyPrefix fronts every router-issued bearer token: "<prefix>_<24 random chars>".
const APIKeyPrefix = "rk"

const (
	idLength               = 24
	alphaNumericCharacters = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

func GenerateID(prefix string) string {
	if len(prefix) > 10 {
		panic("prefix must be 10 characters or less")
	}
	bytes := make([]byte, idLength)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("_")
	for _, x := range bytes {
		b.WriteByte(alphaNumericCharacters[int(x)%len(alphaNumericCharacters)])
	}
	return b.String()
}

func HasAPIKeyPrefix(token string) bool {
	return strings.HasPrefix(token, APIKeyPrefix+"_")
}
