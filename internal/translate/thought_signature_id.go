package translate

import (
	"encoding/base64"
	"strings"
)

// thoughtSignatureIDDelimiter separates a synthesized tool-use id from a
// base64-encoded Gemini thoughtSignature smuggled inside it. The id field is
// structurally preserved by every Anthropic and OpenAI client (it's a typed
// string used to correlate tool_use ↔ tool_result), so this is a stateless
// round-trip channel that works even when a client's typed SDK drops unknown
// fields like the off-spec thought_signature field on a tool_use block.
//
// Prior art: BerriAI/litellm PR #16895 uses the same approach on OpenAI
// tool_call.id.
const thoughtSignatureIDDelimiter = "__thought__"

// embedSignatureInID returns id with sig encoded and appended via the
// delimiter. Returns id unchanged when sig is empty.
func embedSignatureInID(id, sig string) string {
	if sig == "" {
		return id
	}
	return id + thoughtSignatureIDDelimiter + base64.RawURLEncoding.EncodeToString([]byte(sig))
}

// extractSignatureFromID is the inverse of embedSignatureInID. If id contains
// the delimiter and the suffix decodes as base64, it returns the leading
// portion and the decoded signature. Otherwise it returns id unchanged and an
// empty signature.
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
