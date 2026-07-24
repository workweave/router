package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// cassette is a recorded HTTP interaction: enough to replay the response
// without ever touching the network again.
type cassette struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	// Body holds the raw response bytes. Stored as a plain JSON string (not
	// []byte's base64 default) so cassettes stay human-diffable and secret-scannable.
	Body rawTextBody `json:"body"`
}

// rawTextBody is []byte that marshals as a plain JSON string instead of
// base64 (encoding/json's default for []byte). Only valid for UTF-8 text —
// callers must decompress/never persist binary payloads here.
type rawTextBody []byte

func (b rawTextBody) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(b))
}

func (b *rawTextBody) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*b = rawTextBody(s)
	return nil
}

// store is a disk-backed cassette cache keyed by a hash of the request. An
// in-memory mutex serializes writes (test parallelism is low; simplicity wins
// over a fancier per-key lock).
type store struct {
	dir string
	mu  sync.Mutex
}

func newStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &store{dir: dir}, nil
}

// requestKey hashes method + path + body. The smoke fixtures are
// byte-deterministic (stable system prompt, fixed user text per scenario), so
// identical scenarios hash identically across runs and across machines.
func requestKey(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *store) path(key string) string {
	return filepath.Join(s.dir, key+".json")
}

func (s *store) load(key string) (*cassette, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path(key))
	if err != nil {
		return nil, false
	}
	var c cassette
	if json.Unmarshal(raw, &c) != nil {
		return nil, false
	}
	return &c, true
}

// save writes a cassette atomically (temp file + rename) so a crash mid-write
// never leaves a corrupt cassette that a later replay would fail to parse.
func (s *store) save(key string, c *cassette) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "cassette-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, s.path(key))
}

// sanitizeHeaders drops auth credentials, org identifiers, rate-limit counters,
// and request IDs before persisting — cassettes are committed to the repo so
// a leaked key or org ID would be a real incident (see CLAUDE.md).
func sanitizeHeaders(h http.Header) map[string]string {
	drop := map[string]struct{}{
		"authorization":                          {},
		"x-api-key":                              {},
		"proxy-authorization":                    {},
		"cookie":                                 {},
		"set-cookie":                             {},
		"anthropic-organization-id":              {},
		"request-id":                             {},
		"cf-ray":                                 {},
		"anthropic-ratelimit-input-tokens-limit": {},
		"anthropic-ratelimit-input-tokens-remaining":  {},
		"anthropic-ratelimit-input-tokens-reset":      {},
		"anthropic-ratelimit-output-tokens-limit":     {},
		"anthropic-ratelimit-output-tokens-remaining": {},
		"anthropic-ratelimit-output-tokens-reset":     {},
		"anthropic-ratelimit-requests-limit":          {},
		"anthropic-ratelimit-requests-remaining":      {},
		"anthropic-ratelimit-requests-reset":          {},
		"anthropic-ratelimit-tokens-limit":            {},
		"anthropic-ratelimit-tokens-remaining":        {},
		"anthropic-ratelimit-tokens-reset":            {},
		// OpenAI's own org/project identifiers + request-id + rate-limit headers.
		"openai-organization":                  {},
		"openai-project":                       {},
		"openai-processing-ms":                 {},
		"x-request-id":                         {},
		"x-ratelimit-limit-requests":           {},
		"x-ratelimit-remaining-requests":       {},
		"x-ratelimit-reset-requests":           {},
		"x-ratelimit-limit-tokens":             {},
		"x-ratelimit-remaining-tokens":         {},
		"x-ratelimit-reset-tokens":             {},
		"x-ratelimit-limit-project-tokens":     {},
		"x-ratelimit-remaining-project-tokens": {},
		"x-ratelimit-reset-project-tokens":     {},
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if _, skip := drop[strings.ToLower(k)]; skip {
			continue
		}
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

var errCacheMiss = fmt.Errorf("cassette: cache miss")
