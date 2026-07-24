package hmm

import (
	"strings"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/catalog"
)

var rosterAliases = map[string]string{
	"claude-sonnet-4-6":    "anthropic/claude-sonnet-4.6",
	"claude-haiku-4-5":     "anthropic/claude-haiku-4.5",
	"claude-sonnet-5":      "anthropic/claude-sonnet-5",
	"claude-opus-4-8":      "anthropic/claude-opus-4.8",
	"claude-fable-5":       "anthropic/claude-fable-5",
	"moonshotai/kimi-k2.7": "moonshotai/kimi-k2.7-code",
}

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
	return ""
}

// CatalogIDForRoster maps a roster arm ID back to its catalog model ID via the
// same forward mapping the resolver uses. Returns the arm ID unchanged when no
// catalog model maps to it.
func CatalogIDForRoster(rosterID string) string {
	for _, m := range catalog.Models {
		if rosterIDFor(m) == rosterID {
			return m.ID
		}
	}
	return rosterID
}
