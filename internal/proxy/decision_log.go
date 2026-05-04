package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DecisionLog appends one JSON line per routed request to a sidecar file
// so out-of-process consumers (the cc-statusline script) can match
// transcript entries to their original requested_model — information
// that is otherwise lost once the router rewrites the model field
// before forwarding upstream.
//
// The file is the only authoritative source for "what did the client
// originally ask for?" — Anthropic's response, which CC stores in its
// transcript, only carries the *routed* model.
type DecisionLog struct {
	path string
	mu   sync.Mutex
}

// DecisionLogEntry is a single sidecar record. RequestID is Anthropic's
// `request-id` response header (also stored as `requestId` in CC's
// transcript JSONL) — the join key that lets a consumer correlate the
// two streams.
type DecisionLogEntry struct {
	Timestamp        string `json:"ts"`
	RequestID        string `json:"request_id"`
	RequestedModel   string `json:"requested_model"`
	DecisionModel    string `json:"decision_model"`
	DecisionReason   string `json:"decision_reason"`
	DecisionProvider string `json:"decision_provider"`
	DeviceID         string `json:"device_id,omitempty"`
	SessionID        string `json:"session_id,omitempty"`
}

// NewDecisionLog returns a *DecisionLog when path is non-empty (and the
// parent directory is creatable); returns nil otherwise so the proxy
// service treats logging as disabled. nil is the off switch — production
// deployments that don't set ROUTER_DECISIONS_LOG_PATH get no file.
func NewDecisionLog(path string) *DecisionLog {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil
	}
	return &DecisionLog{path: path}
}

// Append writes one JSON line to the log. nil-safe so callers don't have
// to guard. Errors are swallowed: this is a best-effort observability
// channel, not a request-path correctness concern. A separate goroutine
// could write while we hold the mutex; that's fine — POSIX guarantees
// each O_APPEND write up to PIPE_BUF (≥4KB) is atomic, but the mutex is
// belt-and-suspenders against partial-line races.
func (d *DecisionLog) Append(entry DecisionLogEntry) {
	if d == nil || entry.RequestID == "" {
		return
	}
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	f, err := os.OpenFile(d.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
