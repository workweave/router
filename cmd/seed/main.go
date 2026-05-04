// Command seed creates a local-dev installation and API key, then prints
// the raw token and paste-ready configuration for Claude Code and Cursor.
//
// Usage:
//
//	go run ./cmd/seed
//	# or via Makefile:
//	make seed
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/config"

	"github.com/jackc/pgx/v5"
)

const (
	defaultOrgID = "local-dev"
	defaultName  = "local-dev-seed"
	createdBy    = "make seed"
	baseURL      = "http://localhost:8082"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, config.PostgresDSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot connect to postgres: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "SET search_path TO router, public"); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot set search_path: %v\n", err)
		os.Exit(1)
	}

	rawToken := auth.GenerateID(auth.APIKeyPrefix)
	keyHash, keyPrefix, keySuffix := auth.APITokenFingerprint(rawToken)

	tx, err := conn.Begin(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot begin transaction: %v\n", err)
		os.Exit(1)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	externalID := defaultOrgID
	if v := os.Getenv("SEED_EXTERNAL_ID"); v != "" {
		externalID = v
	}

	var installationID string
	err = tx.QueryRow(ctx, `
		INSERT INTO model_router_installations
			(external_id, name, created_by)
		VALUES (@external_id, @name, @created_by)
		ON CONFLICT (external_id, name) WHERE deleted_at IS NULL DO NOTHING
		RETURNING id::text`,
		pgx.NamedArgs{
			"external_id": externalID,
			"name":        defaultName,
			"created_by":  createdBy,
		},
	).Scan(&installationID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			SELECT id::text FROM model_router_installations
			WHERE external_id = @external_id AND name = @name AND deleted_at IS NULL`,
			pgx.NamedArgs{"external_id": externalID, "name": defaultName},
		).Scan(&installationID)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create installation: %v\n", err)
		os.Exit(1)
	}

	var apiKeyID string
	err = tx.QueryRow(ctx, `
		INSERT INTO model_router_api_keys
			(installation_id, external_id, name,
			 key_prefix, key_hash, key_suffix, created_by)
		VALUES (@installation_id::uuid, @external_id, @name,
				@key_prefix, @key_hash, @key_suffix, @created_by)
		RETURNING id::text`,
		pgx.NamedArgs{
			"installation_id": installationID,
			"external_id":     externalID,
			"name":            defaultName,
			"key_prefix":      keyPrefix,
			"key_hash":        keyHash,
			"key_suffix":      keySuffix,
			"created_by":      createdBy,
		},
	).Scan(&apiKeyID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create api key: %v\n", err)
		os.Exit(1)
	}

	if err := tx.Commit(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot commit transaction: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created installation %s (external_id: %s)\n", installationID, externalID)
	fmt.Printf("Created api key      %s (suffix: ...%s)\n\n", apiKeyID, keySuffix)
	fmt.Printf("Weave Router key (shown once — store it now):\n")
	fmt.Printf("  %s\n\n", rawToken)
	fmt.Printf("This is a Weave Router auth token (rk_...) — it gates traffic to your\n")
	fmt.Printf("router. It is NOT an Anthropic key. Anthropic only sees the upstream\n")
	fmt.Printf("provider key (sk-ant-...) the router holds server-side.\n\n")

	fmt.Printf("Quick smoke test:\n")
	fmt.Printf("  curl -i -H 'Authorization: Bearer %s' %s/validate\n\n", rawToken, baseURL)

	fmt.Printf("=== Claude Code (recommended) ===\n")
	fmt.Printf("  export WEAVE_ROUTER_KEY=%s\n", rawToken)
	fmt.Printf("  ./install/install.sh --base-url %s\n", baseURL)
	fmt.Printf("  claude  # the installer wires settings.json — no shell exports needed afterwards\n\n")

	fmt.Printf("=== Claude Code (manual fallback) ===\n")
	fmt.Printf("  Claude Code reads its bearer from ANTHROPIC_AUTH_TOKEN (or\n")
	fmt.Printf("  ANTHROPIC_API_KEY); the value below is a Weave Router key, not\n")
	fmt.Printf("  an Anthropic key.\n")
	fmt.Printf("    export ANTHROPIC_BASE_URL=%s\n", baseURL)
	fmt.Printf("    export ANTHROPIC_AUTH_TOKEN=%s\n", rawToken)
	fmt.Printf("    claude\n\n")

	fmt.Printf("=== Cursor ===\n")
	fmt.Printf("  1. Open Cursor Settings > Models > Override OpenAI Base URL\n")
	fmt.Printf("     Set to: %s/v1\n", baseURL)
	fmt.Printf("  2. Paste the Weave Router key as the API key: %s\n", rawToken)
}
