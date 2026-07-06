package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/translate"
)

// DefaultHandoverModel summarizes prior conversation before a mid-session
// model switch. Haiku-class is intentional: summarization is cheap.
const DefaultHandoverModel = "claude-haiku-4-5"

// DefaultHandoverTimeout bounds the summarizer call. Tuned for ~5-7s p95
// observed for haiku-class summarization of ~20k-token sessions.
const DefaultHandoverTimeout = 8 * time.Second

// DefaultHandoverMaxTokens caps the synthesized summary length.
const DefaultHandoverMaxTokens = 800

// handoverInstruction elicits the summary as a final user message, explicit
// about what must survive the switch (decisions, paths, latest intent).
const handoverInstruction = "Summarize the conversation so far in <= 800 tokens. " +
	"Preserve all decisions, file paths, code snippets, and the user's latest intent. " +
	"Output only the summary text — no preamble, no closing remark."

// DefaultCompactionMaxTokens caps the structured compaction summary. Larger
// than the switch handover cap because the compaction summary is the ONLY
// record of the elided history the model keeps — it must carry task state, not
// just a gist.
const DefaultCompactionMaxTokens = 4000

// compactionInstruction elicits Claude Code's 9-section structured summary used
// when a long session is compacted to fit a context window. Unlike the terse
// switch-handover instruction, this preserves enough task state (pending work,
// current file, next step) that the model can continue seamlessly, and quotes
// user-stated constraints verbatim so they keep applying after the elision.
const compactionInstruction = "The conversation is being compacted to fit the model's context window. " +
	"Produce a detailed structured summary of everything above under these numbered sections, " +
	"prioritizing technical accuracy and completeness:\n" +
	"1. Primary Request and Intent — every explicit user request, in detail.\n" +
	"2. Key Technical Concepts — technologies, frameworks, and patterns in play.\n" +
	"3. Files and Code Sections — files examined/modified, with key snippets and why they matter.\n" +
	"4. Errors and Fixes — problems hit, fixes applied, and user feedback received.\n" +
	"5. Problem Solving — approaches tried and reasoning, not just outcomes.\n" +
	"6. All User Messages (verbatim) — quote every non-tool user message exactly, especially any stated constraints or policies; do not paraphrase.\n" +
	"7. Pending Tasks — requested work not yet completed.\n" +
	"8. Current Work — precisely what was being done just before this summary, with filenames and state.\n" +
	"9. Next Step — the immediate next action, aligned to the user's most recent request.\n" +
	"Output only the summary text — no preamble, no closing remark."

// ProviderSummarizer adapts a providers.Client to handover.Summarizer by
// building a small Anthropic Messages request from the prior conversation.
type ProviderSummarizer struct {
	client    providers.Client
	model     string
	timeout   time.Duration
	maxTokens int
}

// NewProviderSummarizer constructs a summarizer adapter. Empty/zero args
// fall back to defaults.
func NewProviderSummarizer(client providers.Client, model string, timeout time.Duration) *ProviderSummarizer {
	if model == "" {
		model = DefaultHandoverModel
	}
	if timeout <= 0 {
		timeout = DefaultHandoverTimeout
	}
	return &ProviderSummarizer{
		client:    client,
		model:     model,
		timeout:   timeout,
		maxTokens: DefaultHandoverMaxTokens,
	}
}

// WithMaxTokens overrides the per-summary output cap. Zero/negative
// leaves the default.
func (s *ProviderSummarizer) WithMaxTokens(n int) *ProviderSummarizer {
	if n > 0 {
		s.maxTokens = n
	}
	return s
}

// Provider returns the upstream provider this summarizer dispatches to.
func (s *ProviderSummarizer) Provider() string {
	return providers.ProviderAnthropic
}

// ErrEmptySummary is returned when the upstream call succeeded but no
// assistant text was extractable.
var ErrEmptySummary = errors.New("handover: upstream returned no summary text")

// Summarize implements handover.Summarizer: builds and dispatches an
// Anthropic Messages call under a hard timeout, returning summary text plus
// usage for a separate ledger row. On failure returns ("", zero Usage, err)
// so the caller falls back to the full prior history.
func (s *ProviderSummarizer) Summarize(ctx context.Context, env *translate.RequestEnvelope) (string, handover.Usage, error) {
	return s.summarize(ctx, env, s.model, handoverInstruction, s.maxTokens, "handover")
}

// SummarizeForCompaction summarizes env with the structured 9-section
// compaction prompt, targeting an explicit model (the window-aware selection
// happens in the caller) and a larger output cap. Used by the context-window
// compaction cascade, not the switch-handover path. Same failure contract as
// Summarize.
func (s *ProviderSummarizer) SummarizeForCompaction(ctx context.Context, env *translate.RequestEnvelope, model string, maxTokens int) (string, handover.Usage, error) {
	if model == "" {
		model = s.model
	}
	if maxTokens <= 0 {
		maxTokens = DefaultCompactionMaxTokens
	}
	return s.summarize(ctx, env, model, compactionInstruction, maxTokens, "compaction")
}

// summarize builds an Anthropic Messages call from env with the given
// instruction/model/cap and dispatches it under a hard timeout. kind is a log
// label ("handover" or "compaction"). On any failure returns ("", zero, err)
// so callers fall back to the full history.
func (s *ProviderSummarizer) summarize(ctx context.Context, env *translate.RequestEnvelope, model, instruction string, maxTokens int, kind string) (string, handover.Usage, error) {
	log := observability.FromContext(ctx)
	if env == nil {
		return "", handover.Usage{}, errors.New("handover: nil envelope")
	}
	if s.client == nil {
		return "", handover.Usage{}, errors.New("handover: nil provider client")
	}

	body, err := buildSummaryRequestBody(env, model, instruction, maxTokens)
	if err != nil {
		return "", handover.Usage{}, fmt.Errorf("build %s request: %w", kind, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")

	prep := providers.PreparedRequest{
		Body:    body,
		Headers: make(http.Header),
	}
	prep.Headers.Set("anthropic-version", "2023-06-01")

	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    model,
		Reason:   kind + "_summary",
	}

	proxyErr := s.client.Proxy(callCtx, decision, prep, rec, req)
	if proxyErr != nil {
		log.Warn("Summarizer upstream call failed", "kind", kind, "err", proxyErr, "model", model)
		return "", handover.Usage{}, proxyErr
	}
	if callCtx.Err() != nil {
		log.Warn("Summarizer timed out", "kind", kind, "err", callCtx.Err(), "model", model)
		return "", handover.Usage{}, callCtx.Err()
	}
	if rec.Code >= 400 {
		err := fmt.Errorf("handover: upstream status %d", rec.Code)
		log.Warn("Summarizer non-2xx", "kind", kind, "status", rec.Code, "model", model)
		return "", handover.Usage{}, err
	}

	respBody, err := io.ReadAll(rec.Body)
	if err != nil {
		return "", handover.Usage{}, fmt.Errorf("read %s response: %w", kind, err)
	}
	text := extractAnthropicAssistantText(respBody)
	if text == "" {
		log.Warn("Summarizer extracted no text", "kind", kind, "model", model, "body_bytes", len(respBody))
		return "", handover.Usage{}, ErrEmptySummary
	}
	usage := extractAnthropicUsage(respBody)
	usage.Model = model
	usage.Provider = providers.ProviderAnthropic
	return text, usage, nil
}

// extractAnthropicUsage pulls the usage block from an Anthropic non-streaming
// Messages response. Missing fields are zero; we don't distinguish "absent".
func extractAnthropicUsage(body []byte) handover.Usage {
	if !gjson.ValidBytes(body) {
		return handover.Usage{}
	}
	usage := gjson.GetBytes(body, "usage")
	if !usage.IsObject() {
		return handover.Usage{}
	}
	return handover.Usage{
		InputTokens:   int(usage.Get("input_tokens").Int()),
		OutputTokens:  int(usage.Get("output_tokens").Int()),
		CacheCreation: int(usage.Get("cache_creation_input_tokens").Int()),
		CacheRead:     int(usage.Get("cache_read_input_tokens").Int()),
	}
}

// buildSummaryRequestBody builds a non-streaming Anthropic Messages request
// from the envelope's prior conversation, injecting the given summary
// instruction and overriding model/max_tokens/stream.
func buildSummaryRequestBody(env *translate.RequestEnvelope, model, instruction string, maxTokens int) ([]byte, error) {
	prep, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: model})
	if err != nil {
		return nil, fmt.Errorf("prepare anthropic body: %w", err)
	}
	body := prep.Body

	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, fmt.Errorf("set model: %w", err)
	}
	body, err = sjson.SetBytes(body, "stream", false)
	if err != nil {
		return nil, fmt.Errorf("set stream: %w", err)
	}
	body, err = sjson.SetBytes(body, "max_tokens", maxTokens)
	if err != nil {
		return nil, fmt.Errorf("set max_tokens: %w", err)
	}

	body, err = appendUserInstruction(body, instruction)
	if err != nil {
		return nil, fmt.Errorf("append instruction: %w", err)
	}

	for _, key := range []string{"tools", "tool_choice", "thinking", "context_management", "effort", "output_config", "metadata"} {
		body, _ = sjson.DeleteBytes(body, key)
	}

	return body, nil
}

// appendUserInstruction appends a role=user text message to the messages array.
func appendUserInstruction(body []byte, text string) ([]byte, error) {
	msg := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": text},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "messages.-1", raw)
}

// extractAnthropicAssistantText pulls concatenated text from an Anthropic
// Messages non-streaming response. All text blocks are joined with newlines.
func extractAnthropicAssistantText(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return ""
	}
	var out bytes.Buffer
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "text" {
			return true
		}
		text := block.Get("text").String()
		if text == "" {
			return true
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(text)
		return true
	})
	return out.String()
}

var _ handover.Summarizer = (*ProviderSummarizer)(nil)

// runCompactionHandover rewrites env in-place with a handover summary when
// Claude Code context compaction is detected on a non-Anthropic route —
// compaction already dropped old turns, so without this the non-Anthropic
// model loses awareness of edits/decisions from those elided turns.
//
// On any failure it logs and passes the compacted body through unchanged
// (never trims it further, which would discard Claude Code's own compaction
// summary). Returns a handoverOutcome so the caller can bill the summary call.
func (s *Service) runCompactionHandover(ctx context.Context, env *translate.RequestEnvelope, reqHeaders http.Header, decisionModel string) handoverOutcome {
	log := observability.FromContext(ctx)
	var out handoverOutcome
	out.Invoked = true

	var (
		sumProvider       string
		sumCreds          *Credentials
		canCallSummarizer bool
	)
	if s.summarizer != nil {
		sumProvider = s.summarizer.Provider()
		sumCreds = resolveSummarizerCreds(ctx, sumProvider, reqHeaders)
		nonDepCreds := s.requestUsesNonDeploymentCreds(ctx, reqHeaders)
		canCallSummarizer = sumCreds != nil || !nonDepCreds
	}

	switch {
	case s.summarizer == nil:
		out.FallbackToFullHistory = true
		log.Info("Compaction handover: summarizer not wired; preserved compacted body instead", "decision_model", decisionModel)
	case !canCallSummarizer:
		out.FallbackToFullHistory = true
		log.Info("Compaction handover: summarizer skipped (tenant boundary); preserved compacted body instead", "decision_model", decisionModel)
	default:
		summCtx := ctx
		if sumCreds != nil {
			summCtx = context.WithValue(ctx, CredentialsContextKey{}, sumCreds)
		} else {
			// Strip any request credential (e.g. subscription OAuth token) so
			// this synthetic call runs on the deployment key instead of
			// inheriting one that could 401 or cross a tenant boundary.
			summCtx = clearCredentials(ctx)
		}
		start := time.Now()
		summary, summaryUsage, sumErr := s.summarizer.Summarize(summCtx, env)
		out.LatencyMS = time.Since(start).Milliseconds()
		switch {
		case sumErr != nil:
			out.FallbackToFullHistory = true
			log.Warn("Compaction handover: summarizer failed; preserved compacted body instead", "err", sumErr, "decision_model", decisionModel)
		case summary == "":
			out.FallbackToFullHistory = true
			log.Warn("Compaction handover: summarizer returned empty; preserved compacted body instead", "decision_model", decisionModel)
		default:
			handover.RewriteEnvelope(env, summary)
			out.SummaryTokens = estimateSummaryTokens(summary)
			out.SummaryUsage = summaryUsage
			log.Info("Compaction handover: context rewritten with summary", "summary_len", len(summary), "decision_model", decisionModel)
		}
	}
	return out
}
