// Package policyclient implements the versioned HTTP contract shared by
// out-of-process policy routers.
package policyclient

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
	"workweave/router/internal/router/policy"
)

// DefaultTimeout bounds a single delegated policy decision.
const DefaultTimeout = 3 * time.Second

const (
	defaultRouteAttempts = 3
	routeRetryBackoff    = 50 * time.Millisecond
)

const (
	maxRouteMessages           = 96
	maxRouteMessageTextChars   = 3000
	maxRouteMessageTotalChars  = 48000
	maxRouteToolCallInputKeys  = 24
	maxRouteToolCallInputChars = 80
)

// Client calls a versioned policy sidecar.
type Client struct {
	baseURL string
	client  *http.Client
	timeout time.Duration
}

// New builds a policy sidecar client. A nil HTTP client uses a bounded default.
func New(baseURL string, client *http.Client, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), client: client, timeout: timeout}
}

// CheckHealth verifies that the policy sidecar is ready to serve traffic.
func (c *Client) CheckHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/readyz", nil)
	if err != nil {
		return fmt.Errorf("build policy readiness request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call policy readiness endpoint: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("policy readiness status %d", resp.StatusCode)
	}
	return nil
}

// ReportOutcome posts final dispatch usage/status to the policy sidecar.
func (c *Client) ReportOutcome(ctx context.Context, payload map[string]interface{}) error {
	return c.post(ctx, "/outcome", payload, "outcome")
}

// ReportFeedback posts explicit request/session feedback to the policy sidecar.
func (c *Client) ReportFeedback(ctx context.Context, payload map[string]interface{}) error {
	return c.post(ctx, "/feedback", payload, "feedback")
}

// Capabilities fetches the sidecar's optional behavior declaration.
func (c *Client) Capabilities(ctx context.Context) (policy.Capabilities, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/capabilities", nil)
	if err != nil {
		return policy.Capabilities{}, fmt.Errorf("build policy capabilities request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return policy.Capabilities{}, fmt.Errorf("call policy capabilities endpoint: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return policy.Capabilities{}, fmt.Errorf("read policy capabilities response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return policy.Capabilities{}, fmt.Errorf("policy capabilities status %d", resp.StatusCode)
	}
	var capabilities policy.Capabilities
	if err := json.Unmarshal(payload, &capabilities); err != nil {
		return policy.Capabilities{}, fmt.Errorf("decode policy capabilities response: %w", err)
	}
	if capabilities.SchemaVersion != policy.SchemaVersionV1 {
		return policy.Capabilities{}, fmt.Errorf("unsupported policy capabilities schema %q", capabilities.SchemaVersion)
	}
	return capabilities, nil
}

func (c *Client) post(ctx context.Context, path string, payload map[string]interface{}, label string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal policy %s request: %w", label, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build policy %s request: %w", label, err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call policy %s endpoint: %w", label, err)
	}
	defer resp.Body.Close()
	payloadBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("read policy %s response: %w", label, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payloadBytes, &parsed)
		if parsed.Error != "" {
			return fmt.Errorf("policy %s status %d: %s", label, resp.StatusCode, parsed.Error)
		}
		return fmt.Errorf("policy %s status %d", label, resp.StatusCode)
	}
	return nil
}

type routeRequest struct {
	SchemaVersion             string             `json:"schema_version"`
	Strategy                  string             `json:"strategy"`
	ExecutionMode             string             `json:"execution_mode"`
	RouteID                   string             `json:"route_id"`
	OrganizationID            string             `json:"organization_id,omitempty"`
	InstallationID            string             `json:"installation_id,omitempty"`
	ClientApp                 string             `json:"client_app,omitempty"`
	Harness                   string             `json:"harness,omitempty"`
	RolloutID                 string             `json:"rollout_id,omitempty"`
	RequestedModel            string             `json:"requested_model,omitempty"`
	PromptText                string             `json:"prompt_text"`
	LatestUserText            string             `json:"latest_user_text,omitempty"`
	TurnIndex                 int                `json:"turn_index"`
	ConversationMessages      []routeMessage     `json:"conversation_messages,omitempty"`
	TrainingConversationDelta []routeMessage     `json:"training_conversation_delta,omitempty"`
	AvailableTools            []string           `json:"available_tools,omitempty"`
	FeedbackKey               string             `json:"feedback_key,omitempty"`
	FeedbackRole              string             `json:"feedback_role,omitempty"`
	ClientSessionID           string             `json:"client_session_id,omitempty"`
	EstimatedInputTokens      int                `json:"estimated_input_tokens"`
	HasTools                  bool               `json:"has_tools"`
	HasImages                 bool               `json:"has_images"`
	RoutingIntent             string             `json:"routing_intent,omitempty"`
	PreferredModels           []string           `json:"preferred_models,omitempty"`
	RoutingKnobs              *routingKnobs      `json:"routing_knobs,omitempty"`
	QualityBias               *float64           `json:"quality_bias,omitempty"`
	TrainingAllowed           bool               `json:"training_allowed"`
	CaptureMode               string             `json:"capture_mode,omitempty"`
	DebugEnabled              bool               `json:"debug_enabled"`
	Candidates                []policy.Candidate `json:"candidates"`
	CandidateModels           []string           `json:"candidate_models"`
	CandidateProviders        map[string]string  `json:"candidate_providers"`
}

type routingKnobs struct {
	QualityBias          *float64 `json:"quality_bias,omitempty"`
	SpeedWeight          *float64 `json:"speed_weight,omitempty"`
	OutputCostRatio      *float64 `json:"output_cost_ratio,omitempty"`
	ExpectedOutputTokens *int     `json:"expected_output_tokens,omitempty"`
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
	InputJSON string   `json:"input_json,omitempty"`
}

type routeToolResult struct {
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Text      string `json:"text,omitempty"`
}

type routeResponse struct {
	SchemaVersion        string                 `json:"schema_version"`
	RouteID              string                 `json:"route_id"`
	SelectedRosterID     string                 `json:"selected_roster_id"`
	SelectedProvider     string                 `json:"selected_provider"`
	Model                string                 `json:"model"`
	Score                float64                `json:"score"`
	ChosenScore          *float64               `json:"chosen_score"`
	CandidateScores      map[string]float32     `json:"candidate_scores"`
	ScoreKind            string                 `json:"score_kind"`
	ScoreLabel           string                 `json:"score_label"`
	Reason               string                 `json:"reason"`
	PolicyState          string                 `json:"policy_state"`
	StateLabel           string                 `json:"state_label"`
	PolicyGroup          string                 `json:"policy_group"`
	Cluster              string                 `json:"cluster"`
	PolicyLabel          string                 `json:"policy_label"`
	ComplexityLabel      string                 `json:"complexity_label"`
	PolicyRouteKey       string                 `json:"policy_route_key"`
	RoutingBucket        string                 `json:"routing_bucket"`
	Confidence           *float64               `json:"confidence"`
	ClassifierConfidence *float64               `json:"classifier_confidence"`
	Margin               *float64               `json:"margin"`
	ClassifierMargin     *float64               `json:"classifier_margin"`
	Propensity           float64                `json:"propensity"`
	DisplayMarker        string                 `json:"display_marker"`
	PolicyArtifactID     string                 `json:"policy_artifact_id"`
	PolicyModelID        string                 `json:"policy_model_id"`
	PolicyArtifactSHA256 string                 `json:"policy_artifact_sha256"`
	PolicySHA256         string                 `json:"policy_sha256"`
	RosterVersion        string                 `json:"roster_version"`
	DebugRef             string                 `json:"debug_ref"`
	Debug                map[string]interface{} `json:"debug"`
	Error                string                 `json:"error"`
}

// Decide posts the supplied candidate set and returns the sidecar selection.
func (c *Client) Decide(ctx context.Context, query policy.Query) (policy.Result, error) {
	models := make([]string, 0, len(query.Candidates))
	providerMap := make(map[string]string, len(query.Candidates))
	for _, candidate := range query.Candidates {
		models = append(models, candidate.RosterID)
		providerMap[candidate.RosterID] = candidate.Provider
	}
	messages := routeMessages(query.ConversationMessages)
	var trainingDelta []routeMessage
	if query.Strategy == router.StrategyHMM && query.TrainingAllowed {
		trainingDelta = trainingRouteMessageDelta(query.ConversationMessages)
	}
	body, err := json.Marshal(routeRequest{
		SchemaVersion:             policy.SchemaVersionV1,
		Strategy:                  string(query.Strategy),
		ExecutionMode:             query.ExecutionMode,
		RouteID:                   query.RouteID,
		OrganizationID:            query.OrganizationID,
		InstallationID:            query.InstallationID,
		ClientApp:                 query.ClientApp,
		Harness:                   query.ClientApp,
		RolloutID:                 query.RolloutID,
		RequestedModel:            query.RequestedModel,
		PromptText:                query.PromptText,
		LatestUserText:            latestUserText(messages),
		TurnIndex:                 turnIndex(messages),
		ConversationMessages:      messages,
		TrainingConversationDelta: trainingDelta,
		AvailableTools:            clipRouteValues(query.AvailableTools, maxRouteToolCallInputKeys, maxRouteToolCallInputChars),
		FeedbackKey:               query.FeedbackKey,
		FeedbackRole:              query.FeedbackRole,
		ClientSessionID:           query.ClientSessionID,
		EstimatedInputTokens:      query.EstimatedInputTokens,
		HasTools:                  query.HasTools,
		HasImages:                 query.HasImages,
		RoutingIntent:             query.RoutingIntent,
		PreferredModels:           query.PreferredModels,
		RoutingKnobs:              wireRoutingKnobs(query.RoutingKnobs),
		QualityBias:               qualityBias(query.RoutingKnobs),
		TrainingAllowed:           query.TrainingAllowed,
		CaptureMode:               query.CaptureMode,
		DebugEnabled:              query.DebugEnabled,
		Candidates:                query.Candidates,
		CandidateModels:           models,
		CandidateProviders:        providerMap,
	})
	if err != nil {
		return policy.Result{}, fmt.Errorf("marshal policy route request: %w", err)
	}

	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, payload, err := c.doRoute(requestCtx, body)
	if err != nil {
		return policy.Result{}, err
	}

	var parsed routeResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return policy.Result{}, fmt.Errorf("decode policy route response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != "" {
			return policy.Result{}, fmt.Errorf("policy sidecar status %d: %s", resp.StatusCode, parsed.Error)
		}
		return policy.Result{}, fmt.Errorf("policy sidecar status %d", resp.StatusCode)
	}
	if parsed.SchemaVersion != "" && parsed.SchemaVersion != policy.SchemaVersionV1 {
		return policy.Result{}, fmt.Errorf("unsupported policy route schema %q", parsed.SchemaVersion)
	}
	selectedModel := firstNonEmpty(parsed.SelectedRosterID, parsed.Model)
	if selectedModel == "" {
		return policy.Result{}, fmt.Errorf("policy sidecar returned empty model")
	}
	score := parsed.Score
	if parsed.ChosenScore != nil {
		score = *parsed.ChosenScore
	}
	return policy.Result{
		SchemaVersion:        parsed.SchemaVersion,
		RouteID:              parsed.RouteID,
		Model:                selectedModel,
		Provider:             parsed.SelectedProvider,
		Score:                score,
		CandidateScores:      parsed.CandidateScores,
		ScoreKind:            firstNonEmpty(parsed.ScoreKind, parsed.ScoreLabel),
		Reason:               parsed.Reason,
		PolicyState:          firstNonEmpty(parsed.PolicyState, parsed.StateLabel),
		PolicyGroup:          firstNonEmpty(parsed.PolicyGroup, parsed.Cluster),
		PolicyLabel:          firstNonEmpty(parsed.PolicyLabel, parsed.ComplexityLabel),
		PolicyRouteKey:       firstNonEmpty(parsed.PolicyRouteKey, parsed.RoutingBucket),
		Confidence:           firstFloat(parsed.Confidence, parsed.ClassifierConfidence),
		Margin:               firstFloat(parsed.Margin, parsed.ClassifierMargin),
		Propensity:           parsed.Propensity,
		DisplayMarker:        parsed.DisplayMarker,
		PolicyArtifactID:     firstNonEmpty(parsed.PolicyArtifactID, parsed.PolicyModelID),
		PolicyArtifactSHA256: firstNonEmpty(parsed.PolicyArtifactSHA256, parsed.PolicySHA256),
		RosterVersion:        parsed.RosterVersion,
		DebugRef:             parsed.DebugRef,
		Debug:                parsed.Debug,
	}, nil
}

func (c *Client) doRoute(ctx context.Context, body []byte) (*http.Response, []byte, error) {
	var lastErr error
	for attempt := 1; attempt <= defaultRouteAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/route", bytes.NewReader(body))
		if err != nil {
			return nil, nil, fmt.Errorf("build policy route request: %w", err)
		}
		req.Header.Set("content-type", "application/json")
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("call policy sidecar: %w", err)
		} else {
			payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("read policy route response: %w", readErr)
			} else if !isTransientPolicyStatus(resp.StatusCode) {
				return resp, payload, nil
			} else {
				lastErr = policyStatusError(resp.StatusCode, payload)
			}
		}
		if attempt == defaultRouteAttempts {
			break
		}
		timer := time.NewTimer(time.Duration(attempt) * routeRetryBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, fmt.Errorf("policy sidecar retries exhausted: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return nil, nil, fmt.Errorf("policy sidecar retries exhausted: %w", lastErr)
}

func isTransientPolicyStatus(status int) bool {
	return status == http.StatusInternalServerError ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

func policyStatusError(status int, payload []byte) error {
	var parsed struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(payload, &parsed)
	if parsed.Error != "" {
		return fmt.Errorf("policy sidecar status %d: %s", status, parsed.Error)
	}
	return fmt.Errorf("policy sidecar status %d", status)
}

func wireRoutingKnobs(knobs *router.Overrides) *routingKnobs {
	if knobs == nil {
		return nil
	}
	return &routingKnobs{
		QualityBias:          knobs.QualityBias,
		SpeedWeight:          knobs.SpeedWeight,
		OutputCostRatio:      knobs.OutputCostRatio,
		ExpectedOutputTokens: knobs.ExpectedOutputTokens,
	}
}

func qualityBias(knobs *router.Overrides) *float64 {
	if knobs == nil {
		return nil
	}
	return knobs.QualityBias
}

type routeMessageLimits struct {
	maxMessages           int
	maxTextChars          int
	maxTotalTextChars     int
	maxToolCallInputKeys  int
	maxToolCallInputChars int
	includeToolCallInput  bool
	includeToolResultText bool
}

func routeMessages(messages []router.ConversationMessage) []routeMessage {
	return convertRouteMessages(messages, routeMessageLimits{
		maxMessages:           maxRouteMessages,
		maxTextChars:          maxRouteMessageTextChars,
		maxTotalTextChars:     maxRouteMessageTotalChars,
		maxToolCallInputKeys:  maxRouteToolCallInputKeys,
		maxToolCallInputChars: maxRouteToolCallInputChars,
	})
}

func trainingRouteMessageDelta(messages []router.ConversationMessage) []routeMessage {
	if len(messages) == 0 {
		return nil
	}

	// Each route happens before its next assistant response. Preserve the new
	// exchange since the last response so training can reconstruct the turn.
	start := 0
	latestUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if routeRole(messages[i].Role) == "user" {
			latestUser = i
			break
		}
	}
	if latestUser >= 0 {
		for i := latestUser - 1; i >= 0; i-- {
			if routeRole(messages[i].Role) == "assistant" {
				start = i
				break
			}
		}
	}

	return convertRouteMessages(messages[start:], routeMessageLimits{
		includeToolCallInput:  true,
		includeToolResultText: true,
	})
}

func convertRouteMessages(messages []router.ConversationMessage, limits routeMessageLimits) []routeMessage {
	if len(messages) == 0 {
		return nil
	}
	start := 0
	if limits.maxMessages > 0 && len(messages) > limits.maxMessages {
		start = len(messages) - limits.maxMessages
	}
	reversed := make([]routeMessage, 0, len(messages)-start)
	totalText := 0
	for i := len(messages) - 1; i >= start; i-- {
		message := messages[i]
		role := routeRole(message.Role)
		if role == "" {
			continue
		}
		text := clipRouteText(message.Text, limits.maxTextChars)
		if limits.maxTotalTextChars > 0 && totalText+len(text) > limits.maxTotalTextChars {
			remaining := limits.maxTotalTextChars - totalText
			if remaining <= 0 {
				text = ""
			} else {
				text = clipRouteText(text, remaining)
			}
		}
		totalText += len(text)
		calls := make([]routeToolCall, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			name := clipRouteText(call.Name, limits.maxToolCallInputChars)
			if name == "" {
				continue
			}
			keys := call.InputKeys
			if limits.maxToolCallInputKeys > 0 && len(keys) > limits.maxToolCallInputKeys {
				keys = keys[:limits.maxToolCallInputKeys]
			}
			inputKeys := make([]string, 0, len(keys))
			for _, key := range keys {
				if clipped := clipRouteText(key, limits.maxToolCallInputChars); clipped != "" {
					inputKeys = append(inputKeys, clipped)
				}
			}
			routeCall := routeToolCall{Name: name, InputKeys: inputKeys}
			if limits.includeToolCallInput {
				routeCall.InputJSON = clipRouteText(call.InputJSON, limits.maxTextChars)
			}
			calls = append(calls, routeCall)
		}
		results := make([]routeToolResult, 0, len(message.ToolResults))
		for _, result := range message.ToolResults {
			routeResult := routeToolResult{
				ToolUseID: clipRouteText(result.ToolUseID, limits.maxToolCallInputChars),
				IsError:   result.IsError,
			}
			if limits.includeToolResultText {
				routeResult.Text = clipRouteText(result.Text, limits.maxTextChars)
			}
			results = append(results, routeResult)
		}
		if text == "" && len(calls) == 0 && len(results) == 0 {
			continue
		}
		reversed = append(reversed, routeMessage{Role: role, Text: text, ToolCalls: calls, ToolResults: results})
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
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Text) != "" {
			return strings.TrimSpace(messages[i].Text)
		}
	}
	return ""
}

func turnIndex(messages []routeMessage) int {
	count := 0
	for _, message := range messages {
		if message.Role == "user" && strings.TrimSpace(message.Text) != "" {
			count++
		}
	}
	if count <= 1 {
		return 0
	}
	return count - 1
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstFloat(values ...*float64) *float64 {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func clipRouteValues(values []string, maxValues, maxChars int) []string {
	if len(values) == 0 {
		return nil
	}
	if maxValues > 0 && len(values) > maxValues {
		values = values[:maxValues]
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		clipped := clipRouteText(value, maxChars)
		if clipped == "" {
			continue
		}
		if _, ok := seen[clipped]; ok {
			continue
		}
		seen[clipped] = struct{}{}
		out = append(out, clipped)
	}
	return out
}

var _ policy.Decider = (*Client)(nil)
var _ policy.OutcomeReporter = (*Client)(nil)
var _ policy.FeedbackReporter = (*Client)(nil)
