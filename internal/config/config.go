package config

import (
	"fmt"
	"net/url"
	"os"
)

func MustGet(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("missing required env var: %s", key))
	}
	return v
}

func GetOr(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// PostgresDSN returns DATABASE_URL when set, otherwise composes one from POSTGRES_* env vars.
//
// On Cloud Run with Cloud SQL, POSTGRES_CONNECTION_NAME routes through the Auth Proxy Unix
// socket at /cloudsql/<connection-name>, which handles TLS+IAM upstream and bypasses
// pg_hba.conf's client-cert requirement on the VPC-private-IP path. Self-hosters omit it
// and fall through to TCP+sslmode for any managed Postgres without certs.
func PostgresDSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	user := MustGet("POSTGRES_USER")
	password := MustGet("POSTGRES_PASSWORD")
	db := MustGet("POSTGRES_DB")

	if conn := os.Getenv("POSTGRES_CONNECTION_NAME"); conn != "" {
		return fmt.Sprintf(
			"postgres://%s:%s@/%s?host=/cloudsql/%s",
			url.QueryEscape(user),
			url.QueryEscape(password),
			url.PathEscape(db),
			conn,
		)
	}

	host := MustGet("POSTGRES_HOST")
	port := GetOr("POSTGRES_PORT", "5432")
	// Default to require so managed Postgres works out of the box; local Docker must set POSTGRES_SSLMODE=disable.
	sslMode := GetOr("POSTGRES_SSLMODE", "require")
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=%s",
		url.QueryEscape(user),
		url.QueryEscape(password),
		host,
		port,
		url.PathEscape(db),
		sslMode,
	)
}
