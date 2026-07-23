//go:build smoke

package smoke

import (
	"net/http"
	"strings"
	"testing"
)

// TestBasic exercises the core request path: the force-model command surface,
// a non-stream turn, and a streamed turn — all pinned to the cheap model and
// served by Anthropic.
func TestBasic(t *testing.T) {
	t.Run("force-model command turn", func(t *testing.T) {
		// A /force-model command returns a synthetic 200 acknowledgement handled
		// entirely inside the router (no upstream call).
		body := forceModelBody(t, "smoke-basic-cmd", cfg.PinModel)
		r := call(t, body)
		if r.status != http.StatusOK {
			t.Fatalf("force-model command: want 200, got %d; body: %s", r.status, truncate(r.body, 400))
		}
		if r.message == nil || r.message.Type != "message" {
			t.Fatalf("force-model command: want a message body, got: %s", truncate(r.body, 400))
		}
	})

	t.Run("non-stream turn served by pinned model", func(t *testing.T) {
		body := newRequest("smoke-basic-nonstream").tokens(64).
			text("Reply with exactly the word: ok").build(t)
		r := call(t, body)
		requireOKMessage(t, r)

		if r.message.Usage.InputTokens <= 0 {
			t.Errorf("want input_tokens > 0, got %d", r.message.Usage.InputTokens)
		}
		if r.message.Usage.OutputTokens <= 0 {
			t.Errorf("want output_tokens > 0, got %d", r.message.Usage.OutputTokens)
		}
		if len(r.message.Content) == 0 {
			t.Errorf("want non-empty content")
		}
		assertServedByPin(t, r)
	})

	t.Run("streamed turn is well-ordered", func(t *testing.T) {
		body := newRequest("smoke-basic-stream").tokens(64).streaming().
			text("Reply with exactly the word: ok").build(t)
		r := call(t, body)
		if r.status != http.StatusOK {
			t.Fatalf("stream: want 200, got %d; body: %s", r.status, truncate(r.body, 400))
		}
		assertStreamWellFormed(t, r)
		if r.message == nil || r.message.Usage.InputTokens <= 0 {
			t.Errorf("stream: want input_tokens > 0 in message_start usage")
		}
		assertServedByPin(t, r)
	})
}

// assertServedByPin checks the x-router-model / x-router-provider decision
// headers name the pinned model on Anthropic. The pin resolves the requested
// model verbatim, so decision_model must equal PinModel.
func assertServedByPin(t *testing.T, r response) {
	t.Helper()
	assertServedByModel(t, r, cfg.PinModel, "anthropic")
}

// assertServedByModel is assertServedByPin generalized to an arbitrary
// model/provider pair, for scenarios that pin something other than the
// suite-wide default (e.g. a gpt-5.x model on the direct OpenAI provider).
func assertServedByModel(t *testing.T, r response, wantModel, wantProvider string) {
	t.Helper()
	gotModel := r.headers.Get(headerRouterModel)
	if gotModel == "" {
		t.Errorf("missing %s header", headerRouterModel)
	} else if !strings.Contains(gotModel, wantModel) && gotModel != wantModel {
		t.Errorf("want %s header = %q (pinned), got %q", headerRouterModel, wantModel, gotModel)
	}
	if gotProvider := r.headers.Get(headerRouterProvider); gotProvider != "" && gotProvider != wantProvider {
		t.Errorf("want %s = %s, got %q", headerRouterProvider, wantProvider, gotProvider)
	}
}

// Response-header names; kept in sync with internal/proxy/headers.go.
const (
	headerRouterModel    = "x-router-model"
	headerRouterProvider = "x-router-provider"
	headerRouterDecision = "x-router-decision"
)
