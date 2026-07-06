package translate

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"workweave/router/internal/providers"
)

type openAIReasoningSignatureEnvelope struct {
	Version  int    `json:"v"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Enc      string `json:"enc"`
}

func encodeOpenAIReasoningSignature(id, enc string) string {
	if id == "" || enc == "" {
		return ""
	}
	b, err := json.Marshal(openAIReasoningSignatureEnvelope{
		Version:  1,
		Provider: providers.ProviderOpenAI,
		ID:       id,
		Enc:      enc,
	})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

func decodeOpenAIReasoningSignature(sig string) (id, enc string, ok bool) {
	if sig == "" {
		return "", "", false
	}
	b, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return "", "", false
	}
	var env openAIReasoningSignatureEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return "", "", false
	}
	if env.Version != 1 || env.Provider != providers.ProviderOpenAI || env.ID == "" || env.Enc == "" {
		return "", "", false
	}
	return env.ID, env.Enc, true
}

const openAIReasoningSignatureIDDelimiter = "__openai_reasoning__"

func embedOpenAIReasoningSignatureInID(id, sig string) string {
	if id == "" || sig == "" || strings.Contains(id, openAIReasoningSignatureIDDelimiter) {
		return id
	}
	return id + openAIReasoningSignatureIDDelimiter + base64.RawURLEncoding.EncodeToString([]byte(sig))
}

func extractOpenAIReasoningSignatureFromID(id string) (cleanID, sig string) {
	i := strings.Index(id, openAIReasoningSignatureIDDelimiter)
	if i < 0 {
		return id, ""
	}
	encoded := id[i+len(openAIReasoningSignatureIDDelimiter):]
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return id[:i], ""
	}
	return id[:i], string(b)
}
