// Package flags resolves per-organization overrides for the router's behavioral
// feature flags.
//
// Behavioral flags have deployment-wide defaults resolved at boot, or fixed
// defaults for settings that require explicit organization opt-in. This package
// layers a sparse per-organization override on top of that default, so a flag can
// be piloted (or disabled) for one installation without a global rollout.
//
// Precedence at a read site is header override > per-org override > deployment
// default. The header layer is owned by the individual override helpers that
// already exist in internal/proxy; this package supplies the middle layer via
// BoolOr and friends, each of which takes the deployment default as its last
// argument and returns it unchanged when the request carries no override.
//
// The package is I/O-free: it owns the value types, the registry, and the context
// key, so both internal/proxy (inner ring) and internal/server/middleware
// (adapter) can use it without an import cycle — the same shape as
// internal/router's WithRoutingKnobs.
package flags

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Key is the stable storage identifier for a flag. It is the JSON object key in
// the installation's flag_overrides column and the primary key in
// router.flag_definitions, so renaming one orphans existing overrides.
type Key string

// Kind is a flag's value type. A stored override whose JSON type disagrees with
// its registered Kind is rejected at parse time rather than coerced.
type Kind string

const (
	KindBool   Kind = "bool"
	KindInt    Kind = "int"
	KindFloat  Kind = "float"
	KindString Kind = "string"
)

// Registered flag keys. Each corresponds to exactly one entry in Registry.
const (
	KeyStruggleShadowEnabled        Key = "struggle_shadow_enabled"
	KeyStruggleEscalationEnabled    Key = "struggle_escalation_enabled"
	KeyStruggleEscalationHoldout    Key = "struggle_escalation_holdout_pct"
	KeyStruggleEvidenceArming       Key = "struggle_evidence_arming"
	KeySpiralShadowEnabled          Key = "spiral_shadow_enabled"
	KeyTurnSignalCapture            Key = "turn_signal_capture_enabled"
	KeyLoopEscalationEnabled        Key = "loop_escalation_enabled"
	KeyLoopEscalationHoldoutPct     Key = "loop_escalation_holdout_pct"
	KeyTextRepetitionBreak          Key = "text_repetition_break_enabled"
	KeyPlannerEnabled               Key = "planner_enabled"
	KeyScoreToolResultTurns         Key = "score_tool_result_turns"
	KeyPrefixTrimFreeSwitch         Key = "prefix_trim_free_switch"
	KeyAuthoritativeUpgradeGate     Key = "authoritative_upgrade_gate"
	KeyAuthorityCacheShadow         Key = "authority_cache_shadow"
	KeySiblingFailover              Key = "sibling_failover"
	KeyEffortEscalation             Key = "effort_escalation"
	KeyCyberRefusalRepin            Key = "cyber_refusal_repin"
	KeyCyberRefusalFallback         Key = "cyber_refusal_fallback_model"
	KeyAnthropicServerFallback      Key = "anthropic_server_side_fallback"
	KeyEmbedOnlyUserMessage         Key = "embed_only_user_message"
	KeyOpenAIResponsesBroad         Key = "openai_responses_broad"
	KeyAllowedModelsHeader          Key = "allowed_models_header"
	KeySubscriptionPlanAwareRouting Key = "subscription_plan_aware_routing_enabled"
)

// Definition describes one overridable flag. DeploymentDefault is not stored
// here: it is resolved at boot, then published to
// router.flag_definitions for the admin UI to display.
type Definition struct {
	Key Key
	// EnvVar is empty for settings controlled only by organization overrides.
	EnvVar      string
	Kind        Kind
	Description string
	// OrgOverridable gates whether a per-organization override may be written.
	// A registered-but-not-overridable flag still publishes its definition (so
	// the admin UI can show it read-only) but rejects writes.
	OrgOverridable bool
}

// RegistryVersion changes whenever Registry's membership changes. Publish uses
// it to make pruning safe during rolling deploys: a revision with an older
// registry version may not delete definitions published by a newer revision.
const RegistryVersion = 7

// Registry is the curated allowlist of flags that may carry a per-organization
// override. It is deliberately explicit rather than derived from the env var
// namespace: most ROUTER_* vars are infra (sidecar URLs, secrets, asset paths),
// are already per-installation columns on model_router_installations, or are
// consumed at construction time and have no per-request read site to override.
var Registry = []Definition{
	{
		Key:            KeySubscriptionPlanAwareRouting,
		Kind:           KindBool,
		Description:    "Avoid models covered only by exhausted Claude/Codex plans while another plan has headroom. Off by default; ignored when subscription routing is disabled.",
		OrgOverridable: true,
	},
	{
		Key:            KeyStruggleShadowEnabled,
		EnvVar:         "ROUTER_STRUGGLE_SHADOW_ENABLED",
		Kind:           KindBool,
		Description:    "Session-level struggle detector (log-only; writes struggle_shadow_events).",
		OrgOverridable: true,
	},
	{
		Key:            KeyStruggleEscalationEnabled,
		EnvVar:         "ROUTER_STRUGGLE_ESCALATION_ENABLED",
		Kind:           KindBool,
		Description:    "Early sideways escalation for sessions struggling in a repeated tool-call cycle.",
		OrgOverridable: true,
	},
	{
		Key:            KeyStruggleEscalationHoldout,
		EnvVar:         "ROUTER_STRUGGLE_ESCALATION_HOLDOUT_PCT",
		Kind:           KindInt,
		Description:    "Percent of struggle detections recorded without escalating, as a self-recovery baseline. 0-100.",
		OrgOverridable: true,
	},
	{
		Key:            KeyStruggleEvidenceArming,
		EnvVar:         "ROUTER_STRUGGLE_EVIDENCE_ARMING",
		Kind:           KindBool,
		Description:    "Let behavioral spiral evidence arm a struggle escalation before the 30-turn/10-minute thresholds.",
		OrgOverridable: true,
	},
	{
		Key:            KeySpiralShadowEnabled,
		EnvVar:         "ROUTER_SPIRAL_SHADOW_ENABLED",
		Kind:           KindBool,
		Description:    "Per-turn spiral detector (log-only).",
		OrgOverridable: true,
	},
	{
		Key:            KeyTurnSignalCapture,
		EnvVar:         "ROUTER_TURN_SIGNAL_CAPTURE_ENABLED",
		Kind:           KindBool,
		Description:    "Persist the per-turn behavioral signal snapshot onto telemetry rows. Skipped regardless for installations that opted out of AI training or set content capture to off.",
		OrgOverridable: true,
	},
	{
		Key:            KeyLoopEscalationEnabled,
		EnvVar:         "ROUTER_LOOP_ESCALATION_ENABLED",
		Kind:           KindBool,
		Description:    "Cyclic-loop escalate-to-opus action. Detection telemetry continues when off.",
		OrgOverridable: true,
	},
	{
		Key:            KeyLoopEscalationHoldoutPct,
		EnvVar:         "ROUTER_LOOP_ESCALATION_HOLDOUT_PCT",
		Kind:           KindInt,
		Description:    "Percent of loop detections recorded without escalating, as a self-recovery baseline. 0-100.",
		OrgOverridable: true,
	},
	{
		Key:            KeyTextRepetitionBreak,
		EnvVar:         "ROUTER_TEXT_REPETITION_BREAK_ENABLED",
		Kind:           KindBool,
		Description:    "Enforcing text-repetition loop break.",
		OrgOverridable: true,
	},
	{
		Key:            KeyPlannerEnabled,
		EnvVar:         "ROUTER_PLANNER_ENABLED",
		Kind:           KindBool,
		Description:    "Cache-aware EV planner for mid-session model switches.",
		OrgOverridable: true,
	},
	{
		Key:            KeyScoreToolResultTurns,
		EnvVar:         "ROUTER_SCORE_TOOL_RESULT_TURNS",
		Kind:           KindBool,
		Description:    "Re-score tool-result turns instead of following the session pin.",
		OrgOverridable: true,
	},
	{
		Key:            KeyPrefixTrimFreeSwitch,
		EnvVar:         "ROUTER_PREFIX_TRIM_FREE_SWITCH",
		Kind:           KindBool,
		Description:    "Treat a trimmed prompt prefix as a free switch point (cache already lost).",
		OrgOverridable: true,
	},
	{
		Key:            KeyAuthoritativeUpgradeGate,
		EnvVar:         "ROUTER_AUTHORITATIVE_UPGRADE_GATE",
		Kind:           KindBool,
		Description:    "Keep the confidence floor active for authoritative-per-turn policies.",
		OrgOverridable: true,
	},
	{
		Key:            KeyAuthorityCacheShadow,
		EnvVar:         "ROUTER_AUTHORITY_CACHE_SHADOW",
		Kind:           KindBool,
		Description:    "Record the HMM cache gate's counterfactual verdict on authoritative-per-turn turns. Observation only; never changes what is served.",
		OrgOverridable: true,
	},
	{
		Key:            KeySiblingFailover,
		EnvVar:         "ROUTER_SIBLING_FAILOVER",
		Kind:           KindBool,
		Description:    "Degrade to a same-cluster candidate when every binding for the routed model is exhausted.",
		OrgOverridable: true,
	},
	{
		Key:            KeyEffortEscalation,
		EnvVar:         "ROUTER_EFFORT_ESCALATION",
		Kind:           KindBool,
		Description:    "Apply policy-requested reasoning-effort escalation.",
		OrgOverridable: true,
	},
	{
		Key:            KeyCyberRefusalRepin,
		EnvVar:         "ROUTER_CYBER_REFUSAL_REPIN",
		Kind:           KindBool,
		Description:    "Re-pin a session off a model that returned a safety refusal (cyber, reasoning_extraction, ...).",
		OrgOverridable: true,
	},
	{
		Key:            KeyCyberRefusalFallback,
		EnvVar:         "ROUTER_CYBER_REFUSAL_FALLBACK_MODEL",
		Kind:           KindString,
		Description:    "Fallback model for a safety-refusal re-pin with no runner-up.",
		OrgOverridable: true,
	},
	{
		Key:            KeyAnthropicServerFallback,
		EnvVar:         "ROUTER_ANTHROPIC_SERVER_SIDE_FALLBACK",
		Kind:           KindBool,
		Description:    "Ask Anthropic to re-serve a safety-refused turn on a fallback model instead of returning the refusal.",
		OrgOverridable: true,
	},
	{
		Key:            KeyEmbedOnlyUserMessage,
		EnvVar:         "ROUTER_EMBED_ONLY_USER_MESSAGE",
		Kind:           KindBool,
		Description:    "Embed user-role text only, instead of the concatenated stream.",
		OrgOverridable: true,
	},
	{
		Key:            KeyOpenAIResponsesBroad,
		EnvVar:         "ROUTER_OPENAI_RESPONSES_BROAD",
		Kind:           KindBool,
		Description:    "Serve every direct-OpenAI turn on /v1/responses. Off, only the reasoning tool turn chat/completions rejects is promoted.",
		OrgOverridable: true,
	},
	{
		Key:            KeyAllowedModelsHeader,
		EnvVar:         "ROUTER_ALLOWED_MODELS_HEADER",
		Kind:           KindBool,
		Description:    "Honor the x-weave-allowed-models request header (per-request routing subset) for this organization even when the installation is not authorized for policy headers.",
		OrgOverridable: true,
	},
}

// definitions indexes Registry by key for O(1) validation.
var definitions = func() map[Key]Definition {
	m := make(map[Key]Definition, len(Registry))
	for _, d := range Registry {
		m[d.Key] = d
	}
	return m
}()

// Lookup returns the definition for key.
func Lookup(key Key) (def Definition, ok bool) {
	def, ok = definitions[key]
	return def, ok
}

// PublishedDefinition pairs a registry entry with the deployment default this
// process actually resolved from the environment at boot.
type PublishedDefinition struct {
	Definition
	// DeploymentDefault is the resolved default rendered as text. It is display
	// metadata for the control plane's admin UI, never read back by routing.
	DeploymentDefault string
}

// DefinitionRepository publishes the registry so the Weave control plane can
// render and validate the per-org override UI without importing router code.
// Implemented by internal/postgres.
type DefinitionRepository interface {
	// Publish upserts every supplied definition and removes rows for flags no
	// longer in the registry.
	Publish(ctx context.Context, defs []PublishedDefinition) error
}

// Overrides is a sparse set of per-organization flag values. Values are held in
// per-Kind maps rather than a single map of a sum type so that reads are
// type-safe without assertions and an empty Overrides costs no allocation.
type Overrides struct {
	Bools   map[Key]bool
	Ints    map[Key]int
	Floats  map[Key]float64
	Strings map[Key]string
}

// ErrUnknownKey means an override refers to a flag absent from Registry.
var ErrUnknownKey = fmt.Errorf("flags: unknown flag key")

// ErrNotOverridable means an override refers to a registered flag that is not
// allowed to vary per organization.
var ErrNotOverridable = fmt.Errorf("flags: flag is not overridable per organization")

// ErrWrongKind means a typed override was put in the map for a different Kind.
var ErrWrongKind = fmt.Errorf("flags: override value has the wrong kind")

// ErrInvalidValue means a typed override violates a registered semantic
// constraint, such as a percentage outside [0, 100].
var ErrInvalidValue = fmt.Errorf("flags: invalid override value")

// IsEmpty reports whether o carries no overrides at all.
func (o Overrides) IsEmpty() bool {
	return len(o.Bools) == 0 && len(o.Ints) == 0 && len(o.Floats) == 0 && len(o.Strings) == 0
}

// ValidateOverrides checks a typed override set before it is persisted. The
// JSON parser performs the same checks after decoding, but callers that already
// have typed maps (notably auth.Service) must not be able to bypass them. A key
// appearing in two maps is rejected because JSON would otherwise silently pick
// whichever map is marshaled last.
func ValidateOverrides(o Overrides) error {
	seen := make(map[Key]struct{}, len(o.Bools)+len(o.Ints)+len(o.Floats)+len(o.Strings))
	check := func(key Key, kind Kind) error {
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: %q appears in multiple typed maps", ErrWrongKind, key)
		}
		seen[key] = struct{}{}
		def, ok := definitions[key]
		if !ok {
			return fmt.Errorf("%w: %q", ErrUnknownKey, key)
		}
		if !def.OrgOverridable {
			return fmt.Errorf("%w: %q", ErrNotOverridable, key)
		}
		if def.Kind != kind {
			return fmt.Errorf("%w: %q is %s, got %s", ErrWrongKind, key, def.Kind, kind)
		}
		return nil
	}
	for key := range o.Bools {
		if err := check(key, KindBool); err != nil {
			return err
		}
	}
	for key, value := range o.Ints {
		if err := check(key, KindInt); err != nil {
			return err
		}
		if key == KeyLoopEscalationHoldoutPct || key == KeyStruggleEscalationHoldout {
			if value < 0 || value > 100 {
				return fmt.Errorf("%w: %q must be between 0 and 100, got %d", ErrInvalidValue, key, value)
			}
		}
	}
	for key := range o.Floats {
		if err := check(key, KindFloat); err != nil {
			return err
		}
	}
	for key, value := range o.Strings {
		if err := check(key, KindString); err != nil {
			return err
		}
		if key == KeyCyberRefusalFallback && strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %q cannot be empty", ErrInvalidValue, key)
		}
	}
	return nil
}

// Keys returns every overridden key, sorted, for logging and tests.
func (o Overrides) Keys() (keys []Key) {
	keys = make([]Key, 0, len(o.Bools)+len(o.Ints)+len(o.Floats)+len(o.Strings))
	for k := range o.Bools {
		keys = append(keys, k)
	}
	for k := range o.Ints {
		keys = append(keys, k)
	}
	for k := range o.Floats {
		keys = append(keys, k)
	}
	for k := range o.Strings {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ParseOverrides decodes a flag_overrides JSONB payload. Empty or JSON null
// yields an empty Overrides and no error. Every key must be registered and
// overridable, and every value must match its registered Kind; a violation
// is returned as an error rather than silently dropped.
func ParseOverrides(raw []byte) (o Overrides, err error) {
	if len(raw) == 0 {
		return Overrides{}, nil
	}
	var decoded map[string]json.RawMessage
	err = json.Unmarshal(raw, &decoded)
	if err != nil {
		return Overrides{}, fmt.Errorf("flags: decode overrides: %w", err)
	}
	for name, rawValue := range decoded {
		key := Key(name)
		def, ok := definitions[key]
		if !ok {
			return Overrides{}, fmt.Errorf("flags: unknown flag %q", name)
		}
		if !def.OrgOverridable {
			return Overrides{}, fmt.Errorf("flags: flag %q is not overridable per organization", name)
		}
		err = o.set(def, rawValue)
		if err != nil {
			return Overrides{}, err
		}
	}
	if err := ValidateOverrides(o); err != nil {
		return Overrides{}, err
	}
	return o, nil
}

// set decodes one value into the map matching def.Kind.
func (o *Overrides) set(def Definition, rawValue []byte) (err error) {
	switch def.Kind {
	case KindBool:
		var v bool
		err = json.Unmarshal(rawValue, &v)
		if err != nil {
			return fmt.Errorf("flags: flag %q expects a boolean: %w", def.Key, err)
		}
		if o.Bools == nil {
			o.Bools = make(map[Key]bool)
		}
		o.Bools[def.Key] = v
	case KindInt:
		var v int
		err = json.Unmarshal(rawValue, &v)
		if err != nil {
			return fmt.Errorf("flags: flag %q expects an integer: %w", def.Key, err)
		}
		if o.Ints == nil {
			o.Ints = make(map[Key]int)
		}
		o.Ints[def.Key] = v
	case KindFloat:
		var v float64
		err = json.Unmarshal(rawValue, &v)
		if err != nil {
			return fmt.Errorf("flags: flag %q expects a number: %w", def.Key, err)
		}
		if o.Floats == nil {
			o.Floats = make(map[Key]float64)
		}
		o.Floats[def.Key] = v
	case KindString:
		var v string
		err = json.Unmarshal(rawValue, &v)
		if err != nil {
			return fmt.Errorf("flags: flag %q expects a string: %w", def.Key, err)
		}
		if o.Strings == nil {
			o.Strings = make(map[Key]string)
		}
		o.Strings[def.Key] = v
	default:
		return fmt.Errorf("flags: flag %q has unsupported kind %q", def.Key, def.Kind)
	}
	return nil
}

// MarshalJSON renders the override set as the flat object stored in the
// flag_overrides column, so ParseOverrides(json.Marshal(o)) round-trips. An
// empty set marshals to "{}" rather than null, matching the column default.
func (o Overrides) MarshalJSON() (data []byte, err error) {
	flat := make(map[string]any, len(o.Bools)+len(o.Ints)+len(o.Floats)+len(o.Strings))
	for k, v := range o.Bools {
		flat[string(k)] = v
	}
	for k, v := range o.Ints {
		flat[string(k)] = v
	}
	for k, v := range o.Floats {
		flat[string(k)] = v
	}
	for k, v := range o.Strings {
		flat[string(k)] = v
	}
	return json.Marshal(flat)
}

type overridesContextKey struct{}

// WithOverrides stashes per-organization overrides on ctx. An empty set leaves
// ctx unchanged so the accessors take their cheap path.
func WithOverrides(ctx context.Context, o Overrides) context.Context {
	if o.IsEmpty() {
		return ctx
	}
	return context.WithValue(ctx, overridesContextKey{}, o)
}

// OverridesFromContext returns the per-organization overrides carried by ctx.
func OverridesFromContext(ctx context.Context) (o Overrides, ok bool) {
	o, ok = ctx.Value(overridesContextKey{}).(Overrides)
	return o, ok
}

// BoolOr returns the per-organization override for key, or deploymentDefault
// when the request carries none.
func BoolOr(ctx context.Context, key Key, deploymentDefault bool) (value bool) {
	o, ok := OverridesFromContext(ctx)
	if !ok {
		return deploymentDefault
	}
	if v, found := o.Bools[key]; found {
		return v
	}
	return deploymentDefault
}

// IntOr returns the per-organization override for key, or deploymentDefault
// when the request carries none.
func IntOr(ctx context.Context, key Key, deploymentDefault int) (value int) {
	o, ok := OverridesFromContext(ctx)
	if !ok {
		return deploymentDefault
	}
	if v, found := o.Ints[key]; found {
		return v
	}
	return deploymentDefault
}

// FloatOr returns the per-organization override for key, or deploymentDefault
// when the request carries none.
func FloatOr(ctx context.Context, key Key, deploymentDefault float64) (value float64) {
	o, ok := OverridesFromContext(ctx)
	if !ok {
		return deploymentDefault
	}
	if v, found := o.Floats[key]; found {
		return v
	}
	return deploymentDefault
}

// StringOr returns the per-organization override for key, or deploymentDefault
// when the request carries none.
func StringOr(ctx context.Context, key Key, deploymentDefault string) (value string) {
	o, ok := OverridesFromContext(ctx)
	if !ok {
		return deploymentDefault
	}
	if v, found := o.Strings[key]; found {
		return v
	}
	return deploymentDefault
}
