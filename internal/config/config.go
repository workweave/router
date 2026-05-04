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

// PostgresDSN returns DATABASE_URL when set, otherwise composes one from the
// POSTGRES_* env vars used by the rest of the platform.
//
// On Cloud Run with Cloud SQL, the Auth Proxy is mounted as a Unix socket at
// /cloudsql/<connection-name>. We connect through the socket so the proxy
// handles TLS + IAM to the upstream DB, and our local connection is plain —
// this bypasses pg_hba.conf's client-cert requirement that bites the
// VPC-private-IP path. Self-hosters don't set POSTGRES_CONNECTION_NAME, so
// they fall through to the standard TCP+sslmode branch and can wire any
// managed Postgres (RDS, Supabase, Neon, plain Docker) without certs.
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
	// Default to `require` so managed Postgres works out of the box.
	// Self-hosters running plain local Docker must set
	// POSTGRES_SSLMODE=disable explicitly.
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
