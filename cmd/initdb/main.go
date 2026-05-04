// Command initdb bootstraps the router's Postgres database and schema.
// Idempotent: safe to run repeatedly on an already-initialized database.
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"workweave/router/internal/config"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dsn := config.PostgresDSN()

	u, err := url.Parse(dsn)
	if err != nil {
		fatal("Cannot parse DATABASE_URL: %v", err)
	}

	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" {
		fatal("DATABASE_URL has no database name")
	}

	ensureDatabase(ctx, u, dbName)
	ensureSchema(ctx, dsn)
}

// ensureDatabase connects to the "postgres" maintenance database and creates
// the target database if it doesn't already exist.
func ensureDatabase(ctx context.Context, u *url.URL, dbName string) {
	adminURL := *u
	adminURL.Path = "/postgres"

	conn, err := pgx.Connect(ctx, adminURL.String())
	if err != nil {
		fatal("Cannot connect to postgres maintenance database: %v", err)
	}
	defer conn.Close(ctx)

	var exists bool
	err = conn.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = @db_name)",
		pgx.NamedArgs{"db_name": dbName},
	).Scan(&exists)
	if err != nil {
		fatal("Cannot check database existence: %v", err)
	}

	if exists {
		fmt.Printf("Database %q already exists\n", dbName)
		return
	}

	quotedName := pgx.Identifier{dbName}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quotedName); err != nil {
		fatal("Cannot create database %s: %v", dbName, err)
	}
	fmt.Printf("Created database %q\n", dbName)
}

// ensureSchema connects to the target database and creates the router schema
// if it doesn't already exist.
func ensureSchema(ctx context.Context, dsn string) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fatal("Cannot connect to target database: %v", err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS router"); err != nil {
		fatal("Cannot create router schema: %v", err)
	}
	fmt.Println("Schema 'router' ready")
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
