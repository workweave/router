package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"workweave/router/internal/api/admin"
)

// newSidecarHealthChecker returns a HealthChecker that probes the HMM
// sidecar's /readyz endpoint. When the sidecar returns non-200, the
// router's /health endpoint reports unhealthy, so Cloud Run defers
// traffic until the sidecar has finished loading its policy artifacts.
func newSidecarHealthChecker(sidecarURL string, logger *slog.Logger) admin.HealthChecker {
	base := strings.TrimRight(sidecarURL, "/")
	readyURL := base + "/readyz"
	client := &http.Client{Timeout: 2 * time.Second}
	return &sidecarHealthChecker{readyURL: readyURL, client: client, logger: logger}
}

type sidecarHealthChecker struct {
	readyURL string
	client   *http.Client
	logger   *slog.Logger
}

func (c *sidecarHealthChecker) CheckHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.readyURL, nil)
	if err != nil {
		return fmt.Errorf("hmm sidecar readiness probe: build request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("hmm sidecar readiness probe: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hmm sidecar not ready (status %d)", resp.StatusCode)
	}
	return nil
}
