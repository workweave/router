package translate

import (
	"encoding/base64"
	"encoding/json"
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
		Provider: "openai",
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
	if env.Version != 1 || env.Provider != "openai" || env.ID == "" || env.Enc == "" {
		return "", "", false
	}
	return env.ID, env.Enc, true
}
