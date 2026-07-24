//go:build smoke

package smoke

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestBoot asserts the stack is up and the public, unauthenticated endpoints
// respond with well-formed payloads — a fast fail if the binary didn't boot
// (bad assets dir, missing pubsub topic, migration failure, etc.).
func TestBoot(t *testing.T) {
	t.Run("health", func(t *testing.T) {
		resp, err := http.Get(cfg.BaseURL + "/health")
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /health: want 200, got %d", resp.StatusCode)
		}
	})

	t.Run("version", func(t *testing.T) {
		body := getJSON(t, "/v1/version")
		// Build metadata is injected via -ldflags; assert the field exists even
		// if empty in a local `go run` build, so the shape contract holds.
		if _, ok := body["commit"]; !ok {
			if _, ok := body["git_commit"]; !ok {
				t.Fatalf("GET /v1/version missing commit field; got keys %v", keys(body))
			}
		}
	})

	t.Run("router models non-empty", func(t *testing.T) {
		body := getJSON(t, "/v1/router/models")
		models, ok := body["models"].([]any)
		if !ok {
			t.Fatalf("GET /v1/router/models: want a models array, got keys %v", keys(body))
		}
		if len(models) == 0 {
			t.Fatalf("GET /v1/router/models returned an empty catalog")
		}
	})
}

func getJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	resp, err := http.Get(cfg.BaseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d", path, resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("GET %s: body not a JSON object: %s", path, truncate(raw, 200))
	}
	return m
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
