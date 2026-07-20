package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/tidwall/gjson"
)

// ToolConfigurationSHA256 returns a content-retention-safe identity for the
// complete inbound tool configuration. It is never persisted with raw schema
// or description text.
func (e *RequestEnvelope) ToolConfigurationSHA256() string {
	if e == nil {
		return sha256String("")
	}
	return sha256String(gjson.GetBytes(e.body, "tools").Raw)
}

// ReasoningConfigurationSHA256 returns a normalized identity for the inbound
// reasoning configuration. Equivalent wire syntaxes normalize through
// ReasoningIntent before hashing.
func (e *RequestEnvelope) ReasoningConfigurationSHA256() string {
	intent := e.ReasoningIntent()
	payload := struct {
		BudgetTokens int64  `json:"budget_tokens"`
		Explicit     bool   `json:"explicit"`
		Kind         string `json:"kind"`
		Level        string `json:"level"`
	}{
		BudgetTokens: intent.BudgetTokens,
		Explicit:     intent.Explicit,
		Kind:         string(intent.Kind),
		Level:        intent.Level,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic("marshal reasoning configuration: " + err.Error())
	}
	return sha256Bytes(encoded)
}

func sha256String(value string) string {
	return sha256Bytes([]byte(value))
}

func sha256Bytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
