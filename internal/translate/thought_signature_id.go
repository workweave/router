package translate

import (
	"encoding/base64"
	"strings"
)

// thoughtSignatureIDDelimiter separates the tool-use id from a base64-encoded
// Gemini thoughtSignature smuggled inside it. The id field survives all
// Anthropic/OpenAI client SDKs (typed string, tool_use/tool_result correlation),
// making it a stateless round-trip channel for opaque signatures that typed SDKs
// would otherwise drop as unknown fields.
//
// Prior art: BerriAI/litellm PR #16895.
const thoughtSignatureIDDelimiter = "__thought__"

// embedSignatureInID returns id with sig base64-encoded and appended.
func embedSignatureInID(id, sig string) string {
	if sig == "" {
		return id
	}
	return id + thoughtSignatureIDDelimiter + base64.RawURLEncoding.EncodeToString([]byte(sig))
}

// extractSignatureFromID is the inverse of embedSignatureInID.
func extractSignatureFromID(id string) (cleanID, signature string) {
	i := strings.Index(id, thoughtSignatureIDDelimiter)
	if i < 0 {
		return id, ""
	}
	encoded := id[i+len(thoughtSignatureIDDelimiter):]
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return id, ""
	}
	return id[:i], string(decoded)
}
