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
