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
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("_")
	for i := 0; i < idLength; i++ {
		b.WriteByte(alphaNumericCharacters[randInt(len(alphaNumericCharacters))])
	}
	return b.String()
}

func randInt(max int) int {
	if max <= 0 {
		panic("max must be positive")
	}
	for {
		var buf [1]byte
		if _, err := rand.Read(buf[:]); err != nil {
			panic(err)
		}
		v := int(buf[0])
		if v < 256-(256%max) {
			return v % max
		}
	}
}

func HasAPIKeyPrefix(token string) bool {
	return strings.HasPrefix(token, APIKeyPrefix+"_")
}