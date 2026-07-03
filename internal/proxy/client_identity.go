package proxy

import (
	"context"
	"encoding/json"
	"strings"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
)

// ClientIdentity holds per-request user identification signals, persisted to
// router.model_router_users (Email, DisplayName).
type ClientIdentity struct {
	DeviceID    string
	AccountID   string
	SessionID   string
	Email       string
	DisplayName string
	UserAgent   string
	ClientApp   string
	// RolloutID is the x-weave-rollout-id eval/training-harness correlation
	// id; joins a sandbox rollout's graded reward to its routing decisions.
	RolloutID string
}

// ClientIdentityContextKey is the request-context key for client identity.
type ClientIdentityContextKey struct{}

// ClientIdentityFrom reads the ClientIdentity stashed on ctx.
func ClientIdentityFrom(ctx context.Context) ClientIdentity {
	id, _ := ctx.Value(ClientIdentityContextKey{}).(ClientIdentity)
	return id
}

// ResolveUserFromContext dispatches identity signals from ctx to
// auth.Service.ResolveAndStashUser. No-op if deps are missing or both email
// and account_uuid are empty — Claude CLI v2.1.x sends account_uuid only, so
// gating on email alone would break that path.
func ResolveUserFromContext(ctx context.Context, authSvc *auth.Service, installation *auth.Installation) context.Context {
	log := observability.Get()
	if authSvc == nil || installation == nil {
		log.Info("ResolveUserFromContext bailout",
			"reason", "nil_dep",
			"authsvc_nil", authSvc == nil,
			"installation_nil", installation == nil,
		)
		return ctx
	}
	id := ClientIdentityFrom(ctx)
	if id.Email == "" && id.AccountID == "" {
		log.Info("ResolveUserFromContext bailout",
			"reason", "no_identity_signal",
			"installation_id", installation.ID,
		)
		return ctx
	}
	log.Debug("ResolveUserFromContext dispatch",
		"installation_id", installation.ID,
		"email_present", id.Email != "",
		"account_present", id.AccountID != "",
		"name_present", id.DisplayName != "",
	)
	return authSvc.ResolveAndStashUser(ctx, installation.ID, id.Email, id.AccountID, id.DisplayName)
}

// ClaudeCodeMetadata mirrors the JSON Claude Code encodes into
// metadata.user_id. Email is promoted to router.model_router_users.
type ClaudeCodeMetadata struct {
	DeviceID  string `json:"device_id"`
	AccountID string `json:"account_uuid"`
	SessionID string `json:"session_id"`
	Email     string `json:"email"`
}

// ParseClaudeCodeMetadata extracts identity fields from the JSON in
// metadata.user_id. Best-effort: returns zero on parse failure.
func ParseClaudeCodeMetadata(raw string) ClaudeCodeMetadata {
	if raw == "" {
		return ClaudeCodeMetadata{}
	}
	var meta ClaudeCodeMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return ClaudeCodeMetadata{}
	}
	return meta
}

// MaxEmailLen caps email length per RFC 5321 §4.5.3.1.3 (256 bytes).
const MaxEmailLen = 254

// MaxClientIdentifierLen bounds caller-controlled opaque identifiers.
// Claude Code emits ~36-char UUIDs; 128 leaves flood-protection headroom.
const MaxClientIdentifierLen = 128

// NormalizeClientIdentifier returns s unchanged if within bounds, else "".
// Rejects rather than truncates: a truncated id looks valid but no longer correlates.
func NormalizeClientIdentifier(s string) string {
	if len(s) > MaxClientIdentifierLen {
		return ""
	}
	return s
}

// RolloutIDHeader carries the eval/training-harness rollout correlation id.
const RolloutIDHeader = "X-Weave-Rollout-Id"

// MaxRolloutIDLen bounds the rollout correlation id. Harness ids compose
// run_id/condition/seed/instance_id, exceeding the 128-byte client id cap.
const MaxRolloutIDLen = 256

// NormalizeRolloutID trims and bounds a rollout id, rejecting (not
// truncating) oversized values — same reasoning as NormalizeClientIdentifier.
func NormalizeRolloutID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > MaxRolloutIDLen {
		return ""
	}
	return s
}

// NormalizeEmail trims, lower-cases, and structurally validates an email to
// match the case-sensitive unique index on (installation_id, email).
// Returns "" when empty, oversized, or malformed. Deliverability isn't
// checked — email is treated as an opaque identifier.
func NormalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > MaxEmailLen {
		return ""
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return ""
	}
	if strings.IndexByte(s[at+1:], '@') >= 0 {
		return ""
	}
	return s
}

// Canonical client_app values. Kept in one place so telemetry, dashboards,
// and tests don't drift on capitalization.
const (
	ClientAppClaudeCode = "claude-code"
	ClientAppCodex      = "codex"
	ClientAppCursor     = "cursor"
	ClientAppGeminiCLI  = "gemini-cli"
	ClientAppOpencode   = "opencode"
)

// clientAppAliases maps the raw X-App values some clients send to their
// canonical client_app. Claude Code sends "cli", which would otherwise store
// verbatim and miss the dashboard's label map.
var clientAppAliases = map[string]string{
	"cli":    ClientAppClaudeCode,
	"cli-bg": ClientAppClaudeCode,
}

// MaxClientAppLen bounds the X-App header. Canonical values are short
// (claude-code, codex, cursor); longer values suggest header smuggling.
const MaxClientAppLen = 32

// NormalizeClientApp trusts an explicit, in-bounds X-App header (lower-cased);
// otherwise falls back to a coarse User-Agent classifier for older installs
// that don't send X-App. Returns "" when neither signal is recognized.
func NormalizeClientApp(xApp, userAgent string) string {
	xApp = strings.ToLower(strings.TrimSpace(xApp))
	if xApp != "" && len(xApp) <= MaxClientAppLen {
		if canonical, ok := clientAppAliases[xApp]; ok {
			return canonical
		}
		return xApp
	}
	ua := strings.ToLower(userAgent)
	switch {
	case ua == "":
		return ""
	case strings.Contains(ua, "claude-cli"):
		return ClientAppClaudeCode
	case strings.Contains(ua, "codex_cli") || strings.Contains(ua, "codex-cli") || strings.HasPrefix(ua, "codex/"):
		return ClientAppCodex
	case strings.Contains(ua, "cursor"):
		return ClientAppCursor
	case strings.Contains(ua, "gemini-cli") || strings.Contains(ua, "google-genai"):
		return ClientAppGeminiCLI
	case strings.Contains(ua, "opencode"):
		return ClientAppOpencode
	}
	return ""
}

// MaxDisplayNameLen bounds the free-form display name, mirroring the
// installer-side cap; longer values suggest header smuggling, not a real name.
const MaxDisplayNameLen = 128

// NormalizeDisplayName trims and strips control characters (so a malicious
// header can't smuggle bytes) from a display name. Returns "" if empty or
// oversized. Not case-folded — names are free-form, not lookup keys.
func NormalizeDisplayName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxDisplayNameLen {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// Drop ASCII control chars (incl. CR/LF) and the C1 block. Printable
		// Unicode passes through so non-ASCII names render correctly.
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return ""
	}
	return out
}
