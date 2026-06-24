package rl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTimeout bounds a single policy decision. The sidecar embeds the
// prompt (a network call to the embedding provider) before scoring, so the
// budget is comparable to the cluster scorer's embed timeout.
const DefaultTimeout = 2 * time.Second

// HTTPDecider calls the policy sidecar's POST /route endpoint.
type HTTPDecider struct {
	baseURL string
	client  *http.Client
}

// NewHTTPDecider builds a Decider that posts to baseURL/route. A nil client
// uses a default client bounded by timeout; a non-nil client is used as-is.
func NewHTTPDecider(baseURL string, client *http.Client, timeout time.Duration) *HTTPDecider {
	if client == nil {
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		client = &http.Client{Timeout: timeout}
	}
	return &HTTPDecider{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
	}
}

type routeRequest struct {
	PromptText         string            `json:"prompt_text"`
	TurnIndex          int               `json:"turn_index"`
	CandidateModels    []string          `json:"candidate_models"`
	CandidateProviders map[string]string `json:"candidate_providers"`
}

type routeResponse struct {
	Model      string  `json:"model"`
	Score      float64 `json:"score"`
	ScoreLabel string  `json:"score_label"`
	Reason     string  `json:"reason"`
	StateLabel string  `json:"state_label"`
	Error      string  `json:"error"`
}

// Decide posts the candidate set to the sidecar and returns its selection.
func (d *HTTPDecider) Decide(ctx context.Context, q Query) (Result, error) {
	models := make([]string, 0, len(q.Candidates))
	providers := make(map[string]string, len(q.Candidates))
	for _, c := range q.Candidates {
		models = append(models, c.RosterID)
		providers[c.RosterID] = c.Provider
	}
	body, err := json.Marshal(routeRequest{
		PromptText:         q.PromptText,
		TurnIndex:          q.TurnIndex,
		CandidateModels:    models,
		CandidateProviders: providers,
	})
	if err != nil {
		return Result{}, fmt.Errorf("marshal route request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/route", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("build route request: %w", err)
	}
	req.Header.Set("content-type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("call policy sidecar: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read policy response: %w", err)
	}

	var parsed routeResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return Result{}, fmt.Errorf("decode policy response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != "" {
			return Result{}, fmt.Errorf("policy sidecar status %d: %s", resp.StatusCode, parsed.Error)
		}
		return Result{}, fmt.Errorf("policy sidecar status %d", resp.StatusCode)
	}
	if parsed.Model == "" {
		return Result{}, fmt.Errorf("policy sidecar returned empty model")
	}

	return Result{
		Model:      parsed.Model,
		Score:      parsed.Score,
		ScoreLabel: parsed.ScoreLabel,
		Reason:     parsed.Reason,
		StateLabel: parsed.StateLabel,
	}, nil
}

var _ Decider = (*HTTPDecider)(nil)
