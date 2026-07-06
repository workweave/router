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

	"workweave/router/internal/router"
)

// DefaultTimeout bounds a single delegated policy decision.
const DefaultTimeout = 3 * time.Second

const (
	maxRouteMessages           = 96
	maxRouteMessageTextChars   = 3000
	maxRouteMessageTotalChars  = 48000
	maxRouteToolCallInputKeys  = 24
	maxRouteToolCallInputChars = 80
)

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
	LatestUserText       string            `json:"latest_user_text,omitempty"`
	TurnIndex            int               `json:"turn_index"`
	ConversationMessages []routeMessage    `json:"conversation_messages,omitempty"`
	EstimatedInputTokens int               `json:"estimated_input_tokens"`
	HasTools             bool              `json:"has_tools"`
	HasImages            bool              `json:"has_images"`
	CandidateModels      []string          `json:"candidate_models"`
	CandidateProviders   map[string]string `json:"candidate_providers"`
}

type routeMessage struct {
	Role        string            `json:"role"`
	Text        string            `json:"text,omitempty"`
	ToolCalls   []routeToolCall   `json:"tool_calls,omitempty"`
	ToolResults []routeToolResult `json:"tool_results,omitempty"`
}

type routeToolCall struct {
	Name      string   `json:"name,omitempty"`
	InputKeys []string `json:"input_keys,omitempty"`
}

type routeToolResult struct {
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
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
	messages := routeMessages(q.ConversationMessages)
	body, err := json.Marshal(routeRequest{
		RouteID:              q.RouteID,
		PromptText:           q.PromptText,
		LatestUserText:       latestUserText(messages),
		TurnIndex:            turnIndex(messages),
		ConversationMessages: messages,
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

func routeMessages(messages []router.ConversationMessage) []routeMessage {
	if len(messages) == 0 {
		return nil
	}
	start := 0
	if len(messages) > maxRouteMessages {
		start = len(messages) - maxRouteMessages
	}
	reversed := make([]routeMessage, 0, len(messages)-start)
	totalText := 0
	for i := len(messages) - 1; i >= start; i-- {
		msg := messages[i]
		role := routeRole(msg.Role)
		if role == "" {
			continue
		}
		text := clipRouteText(msg.Text, maxRouteMessageTextChars)
		if totalText+len(text) > maxRouteMessageTotalChars {
			remaining := maxRouteMessageTotalChars - totalText
			if remaining <= 0 {
				text = ""
			} else {
				text = clipRouteText(text, remaining)
			}
		}
		totalText += len(text)
		calls := make([]routeToolCall, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			name := clipRouteText(call.Name, maxRouteToolCallInputChars)
			if name == "" {
				continue
			}
			keys := call.InputKeys
			if len(keys) > maxRouteToolCallInputKeys {
				keys = keys[:maxRouteToolCallInputKeys]
			}
			inputKeys := make([]string, 0, len(keys))
			for _, key := range keys {
				if clipped := clipRouteText(key, maxRouteToolCallInputChars); clipped != "" {
					inputKeys = append(inputKeys, clipped)
				}
			}
			calls = append(calls, routeToolCall{
				Name:      name,
				InputKeys: inputKeys,
			})
		}
		results := make([]routeToolResult, 0, len(msg.ToolResults))
		for _, result := range msg.ToolResults {
			results = append(results, routeToolResult{
				ToolUseID: clipRouteText(result.ToolUseID, maxRouteToolCallInputChars),
				IsError:   result.IsError,
			})
		}
		if text == "" && len(calls) == 0 && len(results) == 0 {
			continue
		}
		reversed = append(reversed, routeMessage{
			Role:        role,
			Text:        text,
			ToolCalls:   calls,
			ToolResults: results,
		})
	}
	out := make([]routeMessage, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		out = append(out, reversed[i])
	}
	return out
}

func routeRole(role string) string {
	switch strings.TrimSpace(strings.ToLower(role)) {
	case "system", "developer", "user", "assistant":
		return strings.TrimSpace(strings.ToLower(role))
	case "model":
		return "assistant"
	default:
		return ""
	}
}

func clipRouteText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return strings.TrimSpace(text[:limit])
}

func latestUserText(messages []routeMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if isPromptTextRole(messages[i].Role) && strings.TrimSpace(messages[i].Text) != "" {
			return strings.TrimSpace(messages[i].Text)
		}
	}
	return ""
}

func turnIndex(messages []routeMessage) int {
	count := 0
	for _, msg := range messages {
		if isPromptTextRole(msg.Role) && strings.TrimSpace(msg.Text) != "" {
			count++
		}
	}
	if count <= 1 {
		return 0
	}
	return count - 1
}

func isPromptTextRole(role string) bool {
	return role == "user" || role == "developer"
}

var _ Decider = (*HTTPDecider)(nil)
var _ OutcomeReporter = (*HTTPDecider)(nil)
