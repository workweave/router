//go:build smoke

package smoke

import "testing"

// TestCaching is the core of the suite: it exercises the Anthropic prompt-cache
// path end to end against the real API — the exact surface #820/#821 broke.
//
// #820 turned on router cache_control breakpoint injection for the
// Anthropic→Anthropic path; it could emit invalid combinations (a 5th
// breakpoint past Anthropic's 4-cap, or a router 5m breakpoint ordered before a
// client ttl=1h one), which the real Anthropic API rejects with a 400. #821
// fixed it by re-validating after injection and undoing bad breakpoints. These
// scenarios assert the observable outcomes of a correct implementation.
func TestCaching(t *testing.T) {
	// Router-injected caching works with NO client breakpoints: the router adds
	// its own on the large stable prefix. First call warms the cache
	// (cache_creation > 0); an identical-prefix second call reads it
	// (cache_read > 0). If injection is silently broken or the prefix is
	// re-keyed per turn, cache_read stays 0 — the #820-class canary.
	t.Run("router-injected caching warms then reads", func(t *testing.T) {
		uid := "smoke-cache-injected"
		warm := call(t, newRequest(uid).tokens(32).text("Say: one").build(t))
		requireOKMessage(t, warm)
		if warm.message.Usage.CacheCreationInputTokens <= 0 {
			t.Errorf("first call: want cache_creation_input_tokens > 0, got %d (router injection not engaging?)",
				warm.message.Usage.CacheCreationInputTokens)
		}

		// Same session + identical large prefix, different trailing user text.
		read := call(t, newRequest(uid).tokens(32).text("Say: two").build(t))
		requireOKMessage(t, read)
		if read.message.Usage.CacheReadInputTokens <= 0 {
			t.Errorf("second call: want cache_read_input_tokens > 0, got %d (prefix not cached/read)",
				read.message.Usage.CacheReadInputTokens)
		}
	})

	// Client already at Anthropic's 4-breakpoint capacity — three explicit
	// breakpoints spread across tools[] and system, matching native Claude Code.
	// The router must NOT inject a 5th and 400. Pre-#821 the tools[] breakpoints
	// were undercounted and this over-injected. Assert a clean 200.
	t.Run("client at capacity does not over-inject", func(t *testing.T) {
		// 2 extra cached tools + toolCache on the Edit tool + sysCache = 4 total
		// explicit breakpoints, exactly at capacity.
		body := newRequest("smoke-cache-capacity").tokens(32).
			cachedTools(2).toolCache("5m").sysCache("5m").
			text("Say: ok").build(t)
		r := call(t, body)
		requireOKMessage(t, r)
	})

	// Client pins a ttl=1h breakpoint on the final message block and nothing
	// else. The router must not inject an earlier implicit-5m breakpoint that
	// would order 5m-before-1h and trip Anthropic's TTL rule. Assert 200 and
	// that caching still engages.
	t.Run("ttl=1h message breakpoint is not poisoned", func(t *testing.T) {
		uid := "smoke-cache-ttl-order"
		warm := call(t, newRequest(uid).tokens(32).msgCache("1h").text("Say: alpha").build(t))
		requireOKMessage(t, warm)
		// A 1h breakpoint on the trailing block caches the whole prefix; a
		// same-prefix follow-up should read it back.
		read := call(t, newRequest(uid).tokens(32).msgCache("1h").text("Say: beta").build(t))
		requireOKMessage(t, read)
		if read.message.Usage.CacheReadInputTokens <= 0 {
			t.Errorf("ttl=1h follow-up: want cache_read_input_tokens > 0, got %d", read.message.Usage.CacheReadInputTokens)
		}
	})

	// Five explicit client breakpoints exceed Anthropic's 4-cap. The router's
	// own validator should reject this cleanly as a 4xx with an Anthropic-shaped
	// error body — not pass it upstream to fail confusingly, and not 5xx.
	t.Run("overflow rejected cleanly by router", func(t *testing.T) {
		body := newRequest("smoke-cache-overflow").tokens(32).
			cachedTools(4).toolCache("5m").sysCache("5m").
			text("Say: ok").build(t)
		r := call(t, body)
		if r.status < 400 || r.status >= 500 {
			t.Fatalf("overflow: want a 4xx rejection, got %d; body: %s", r.status, truncate(r.body, 400))
		}
		if r.message == nil || r.message.Error == nil {
			t.Fatalf("overflow: want an Anthropic-shaped error body, got: %s", truncate(r.body, 400))
		}
	})
}
