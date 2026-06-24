package rl

import (
	"strings"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/catalog"
)

// rosterAliases maps a catalog model ID to the policy-artifact roster ID when
// the vendor-prefix rule below would produce the wrong slug. The RL policy was
// trained on OpenRouter-style slugs (ml_dev/agent_environment/roster.py's
// OPENROUTER_MODEL_ALIASES); Opus 4.x uses dash-separated minor versions but
// Sonnet/Haiku 4.x use dotted minor versions, so those two need an explicit
// entry. Everything else is derived (slash-form is already a slug; bare
// first-party IDs take their primary provider's vendor prefix).
var rosterAliases = map[string]string{
	"claude-sonnet-4-6": "anthropic/claude-sonnet-4.6",
	"claude-haiku-4-5":  "anthropic/claude-haiku-4.5",
}

// rosterIDFor returns the policy-artifact roster ID for a catalog model. The
// sidecar canonicalizes and intersects candidates against its own roster, so a
// best-effort slug is safe: a model the policy doesn't know is simply dropped.
func rosterIDFor(m catalog.Model) string {
	if alias, ok := rosterAliases[m.ID]; ok {
		return alias
	}
	if strings.Contains(m.ID, "/") {
		return m.ID
	}
	switch m.PrimaryProvider() {
	case providers.ProviderAnthropic:
		return "anthropic/" + m.ID
	case providers.ProviderOpenAI:
		return "openai/" + m.ID
	case providers.ProviderGoogle:
		return "google/" + m.ID
	}
	return m.ID
}
