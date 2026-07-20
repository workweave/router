package translate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/tidwall/gjson"
)

// ToolConfigurationSHA256 returns a privacy-safe hash of the inbound tool configuration; raw schema text is never persisted.
func (e *RequestEnvelope) ToolConfigurationSHA256() string {
	if e == nil {
		return sha256String("")
	}
	raw := gjson.GetBytes(e.body, "tools").Raw
	canonical, err := canonicalizeJSON(json.RawMessage(raw))
	if err != nil {
		return sha256String(raw)
	}
	return sha256Bytes(canonical)
}

// ReasoningConfigurationSHA256 returns a normalized hash; equivalent wire syntaxes (reasoning_effort vs reasoning.effort) produce the same value via ReasoningIntent.
func (e *RequestEnvelope) ReasoningConfigurationSHA256() string {
	if e == nil {
		return sha256String("")
	}
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

func canonicalizeJSON(raw json.RawMessage) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil {
		for key, value := range object {
			canonical, err := canonicalizeJSON(value)
			if err != nil {
				return nil, err
			}
			object[key] = canonical
		}
		return json.Marshal(object)
	}

	var array []json.RawMessage
	if err := json.Unmarshal(raw, &array); err == nil {
		for index, value := range array {
			canonical, err := canonicalizeJSON(value)
			if err != nil {
				return nil, err
			}
			array[index] = canonical
		}
		return json.Marshal(array)
	}

	var compact bytes.Buffer
	err := json.Compact(&compact, raw)
	if err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
