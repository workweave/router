package hmm

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

// DefaultTimeout bounds a single delegated policy decision.
const DefaultTimeout = 3 * time.Second

type HTTPDecider struct {
	baseURL string
	client  *http.Client
}

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

// ReportOutcome posts final dispatch usage/status back to the policy sidecar.
func (d *HTTPDecider) ReportOutcome(ctx context.Context, payload map[string]interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal outcome request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/outcome", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build outcome request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("call HMM outcome endpoint: %w", err)
	}
	defer resp.Body.Close()
	payloadBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("read HMM outcome response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payloadBytes, &parsed)
		if parsed.Error != "" {
			return fmt.Errorf("HMM outcome status %d: %s", resp.StatusCode, parsed.Error)
		}
		return fmt.Errorf("HMM outcome status %d", resp.StatusCode)
	}
	return nil
}

type routeRequest struct {
	RouteID              string            `json:"route_id"`
	PromptText           string            `json:"prompt_text"`
	TurnIndex            int               `json:"turn_index"`
	EstimatedInputTokens int               `json:"estimated_input_tokens"`
	HasTools             bool              `json:"has_tools"`
	HasImages            bool              `json:"has_images"`
	CandidateModels      []string          `json:"candidate_models"`
	CandidateProviders   map[string]string `json:"candidate_providers"`
}

type routeResponse struct {
	RouteID       string                 `json:"route_id"`
	Model         string                 `json:"model"`
	Score         float64                `json:"score"`
	ScoreKind     string                 `json:"score_kind"`
	Reason        string                 `json:"reason"`
	PolicyState   string                 `json:"policy_state"`
	PolicyGroup   string                 `json:"policy_group"`
	PolicyLabel   string                 `json:"policy_label"`
	Confidence    *float64               `json:"confidence"`
	Margin        *float64               `json:"margin"`
	Propensity    float64                `json:"propensity"`
	DisplayMarker string                 `json:"display_marker"`
	Debug         map[string]interface{} `json:"debug"`
	Error         string                 `json:"error"`
}

func (d *HTTPDecider) Decide(ctx context.Context, q Query) (Result, error) {
	models := make([]string, 0, len(q.Candidates))
	providers := make(map[string]string, len(q.Candidates))
	for _, c := range q.Candidates {
		models = append(models, c.RosterID)
		providers[c.RosterID] = c.Provider
	}
	body, err := json.Marshal(routeRequest{
		RouteID:              q.RouteID,
		PromptText:           q.PromptText,
		TurnIndex:            q.TurnIndex,
		EstimatedInputTokens: q.EstimatedInputTokens,
		HasTools:             q.HasTools,
		HasImages:            q.HasImages,
		CandidateModels:      models,
		CandidateProviders:   providers,
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
		return Result{}, fmt.Errorf("call HMM sidecar: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, fmt.Errorf("read HMM response: %w", err)
	}

	var parsed routeResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return Result{}, fmt.Errorf("decode HMM response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != "" {
			return Result{}, fmt.Errorf("HMM sidecar status %d: %s", resp.StatusCode, parsed.Error)
		}
		return Result{}, fmt.Errorf("HMM sidecar status %d", resp.StatusCode)
	}
	if parsed.Model == "" {
		return Result{}, fmt.Errorf("HMM sidecar returned empty model")
	}
	return Result{
		RouteID:       parsed.RouteID,
		Model:         parsed.Model,
		Score:         parsed.Score,
		ScoreKind:     parsed.ScoreKind,
		Reason:        parsed.Reason,
		PolicyState:   parsed.PolicyState,
		PolicyGroup:   parsed.PolicyGroup,
		PolicyLabel:   parsed.PolicyLabel,
		Confidence:    parsed.Confidence,
		Margin:        parsed.Margin,
		Propensity:    parsed.Propensity,
		DisplayMarker: parsed.DisplayMarker,
		Debug:         parsed.Debug,
	}, nil
}

var _ Decider = (*HTTPDecider)(nil)
var _ OutcomeReporter = (*HTTPDecider)(nil)
