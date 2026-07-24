// Command mitmproxy is a record/replay MITM forward proxy for the router smoke
// suite. Points the server container at this proxy via HTTPS_PROXY and trusts
// its CA via SSL_CERT_DIR, intercepting router→Anthropic TLS with zero router changes.
//
// SMOKE_PROXY_MODE: replay-only (PR CI default, no key needed) | record (nightly
// refresh) | replay-or-record (local default). Cache key: sha256(method+path+body).
// CA is ephemeral (in-memory); only the public cert touches disk — no key material
// or upstream credential is ever persisted to a cassette.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	cfg := configFromEnv()

	// Compose healthcheck: the image is distroless (no shell, no curl), so the
	// binary itself is the probe — dial the listen port and exit 0/1.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck(cfg.listen))
	}

	ca, err := newCA()
	if err != nil {
		log.Fatalf("mitmproxy: mint CA: %v", err)
	}
	if err := ca.writePublicCert(cfg.certDir); err != nil {
		log.Fatalf("mitmproxy: write CA cert: %v", err)
	}
	log.Printf("mitmproxy: CA public cert at %s/%s", cfg.certDir, caCertFilename)

	store, err := newStore(cfg.cacheDir)
	if err != nil {
		log.Fatalf("mitmproxy: open cassette store: %v", err)
	}

	p := &proxy{cfg: cfg, ca: ca, store: store, live: liveClient()}

	srv := &http.Server{
		Addr:         cfg.listen,
		Handler:      http.HandlerFunc(p.handle),
		ReadTimeout:  0, // CONNECT tunnels are long-lived; no server-side read deadline.
		WriteTimeout: 0,
	}
	log.Printf("mitmproxy: listening on %s mode=%s cache=%s", cfg.listen, cfg.mode, cfg.cacheDir)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("mitmproxy: serve: %v", err)
	}
}

type config struct {
	listen   string
	certDir  string
	cacheDir string
	mode     string
}

const (
	modeReplayOnly     = "replay-only"
	modeRecord         = "record"
	modeReplayOrRecord = "replay-or-record"
)

func configFromEnv() config {
	cfg := config{
		listen:   envOr("SMOKE_PROXY_LISTEN", ":8888"),
		certDir:  envOr("SMOKE_PROXY_CERT_DIR", "certs"),
		cacheDir: envOr("SMOKE_PROXY_CACHE_DIR", "cassettes"),
		mode:     envOr("SMOKE_PROXY_MODE", modeReplayOrRecord),
	}
	switch cfg.mode {
	case modeReplayOnly, modeRecord, modeReplayOrRecord:
	default:
		fmt.Fprintf(os.Stderr, "mitmproxy: invalid SMOKE_PROXY_MODE %q\n", cfg.mode)
		os.Exit(2)
	}
	if err := os.MkdirAll(cfg.certDir, 0o755); err != nil {
		log.Fatalf("mitmproxy: mkdir cert dir: %v", err)
	}
	if err := os.MkdirAll(cfg.cacheDir, 0o755); err != nil {
		log.Fatalf("mitmproxy: mkdir cache dir: %v", err)
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// liveClient is the client used to reach the real upstream when recording. It
// uses the host's real root store (this process runs on the host/runner, not in
// the distroless container), so it verifies api.anthropic.com normally.
func liveClient() *http.Client {
	return &http.Client{Timeout: 120 * time.Second}
}

// healthcheck dials the proxy's own listen address; used as the compose
// HEALTHCHECK since the distroless image has no shell/curl for CMD-SHELL.
func healthcheck(listen string) int {
	addr := listen
	if addr == "" || addr[0] == ':' {
		addr = "localhost" + addr
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mitmproxy: healthcheck dial %s: %v\n", addr, err)
		return 1
	}
	conn.Close()
	return 0
}
