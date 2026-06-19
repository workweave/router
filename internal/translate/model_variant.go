package translate

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// contextWindowVariantTag is the suffix Claude Code appends to a model id to
// mark its 1M-context variant in the picker and status line (e.g.
// "claude-opus-4-8[1m]"). It is a client-side display convention, not a real
// Anthropic model id: forwarding it verbatim to the native Anthropic API
// returns 404 not_found_error ("the selected model may not exist"). The router
// strips it back to the canonical id before dispatch; the 1M window itself is
// enabled via the context-1m-2025-08-07 beta header (size-triggered in the
// proxy), never via the model name.
const contextWindowVariantTag = "[1m]"

// CanonicalModel strips a Claude Code context-window variant tag from model
// and reports whether one was present. Models without the tag pass through
// unchanged.
func CanonicalModel(model string) (canonical string, hadVariantTag bool) {
	if strings.HasSuffix(model, contextWindowVariantTag) {
		return model[:len(model)-len(contextWindowVariantTag)], true
	}
	return model, false
}

// CanonicalizeModelInBody rewrites the request body's top-level "model" field
// to its canonical form (stripping a Claude Code context-window variant tag)
// and reports whether the tag was present. A body whose model carries no tag —
// or has no model field at all — is returned unchanged with hadVariantTag
// false. This is the single normalization seam for inbound requests so the
// tag never reaches a native Anthropic upstream, and so routing, pins, and
// telemetry all key off the canonical id.
func CanonicalizeModelInBody(body []byte) (out []byte, hadVariantTag bool, err error) {
	canonical, had := CanonicalModel(gjson.GetBytes(body, "model").String())
	if !had {
		return body, false, nil
	}
	out, err = sjson.SetBytes(body, "model", canonical)
	if err != nil {
		return body, true, err
	}
	return out, true, nil
}
