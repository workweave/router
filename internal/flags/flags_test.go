package flags_test

import (
	"context"
	"encoding/json"
	"testing"

	"weave-os/router/internal/flags"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOverridesValid(t *testing.T) {
	o, err := flags.ParseOverrides([]byte(`{
		"struggle_shadow_enabled": false,
		"loop_escalation_holdout_pct": 25,
		"cyber_refusal_fallback_model": "claude-opus-5"
	}`))
	require.NoError(t, err)

	assert.False(t, o.Bools[flags.KeyStruggleShadowEnabled])
	assert.Equal(t, 25, o.Ints[flags.KeyLoopEscalationHoldoutPct])
	assert.Equal(t, "claude-opus-5", o.Strings[flags.KeyCyberRefusalFallback])
	assert.False(t, o.IsEmpty())
}

func TestParseOverridesEmptyIsNotAnError(t *testing.T) {
	for name, raw := range map[string][]byte{
		"nil":          nil,
		"empty":        {},
		"empty object": []byte(`{}`),
		"json null":    []byte(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			o, err := flags.ParseOverrides(raw)
			require.NoError(t, err)
			assert.True(t, o.IsEmpty())
		})
	}
}

func TestParseOverridesRejectsBadPayloads(t *testing.T) {
	for name, raw := range map[string]string{
		// A typo'd or retired key must not be silently dropped: a dropped
		// override reads at the call site as "the default applied".
		"unknown key":         `{"struggle_shadow_nabled": true}`,
		"bool given a string": `{"struggle_shadow_enabled": "true"}`,
		"bool given a number": `{"struggle_shadow_enabled": 1}`,
		"int given a string":  `{"loop_escalation_holdout_pct": "25"}`,
		"int given a float":   `{"loop_escalation_holdout_pct": 2.5}`,
		"string given a bool": `{"cyber_refusal_fallback_model": true}`,
		"not an object":       `[1, 2, 3]`,
		"malformed json":      `{"struggle_shadow_enabled":`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := flags.ParseOverrides([]byte(raw))
			require.Error(t, err)
		})
	}
}

func TestOverridesRoundTrip(t *testing.T) {
	original := []byte(`{"planner_enabled":false,"loop_escalation_holdout_pct":7,"cyber_refusal_fallback_model":"claude-sonnet-5"}`)
	parsed, err := flags.ParseOverrides(original)
	require.NoError(t, err)

	encoded, err := json.Marshal(parsed)
	require.NoError(t, err)

	reparsed, err := flags.ParseOverrides(encoded)
	require.NoError(t, err)
	assert.Equal(t, parsed, reparsed)
}

func TestEmptyOverridesMarshalsToEmptyObject(t *testing.T) {
	encoded, err := json.Marshal(flags.Overrides{})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(encoded))
}

func TestValidateOverridesRejectsWrongKindAndSemanticValues(t *testing.T) {
	for name, overrides := range map[string]flags.Overrides{
		"wrong typed map": {
			Ints: map[flags.Key]int{flags.KeyPlannerEnabled: 1},
		},
		"holdout above 100": {
			Ints: map[flags.Key]int{flags.KeyLoopEscalationHoldoutPct: 101},
		},
		"holdout below 0": {
			Ints: map[flags.Key]int{flags.KeyStruggleEscalationHoldout: -1},
		},
		"duplicate key across maps": {
			Bools: map[flags.Key]bool{flags.KeyPlannerEnabled: true},
			Ints:  map[flags.Key]int{flags.KeyPlannerEnabled: 1},
		},
		"empty fallback model": {
			Strings: map[flags.Key]string{flags.KeyCyberRefusalFallback: "  "},
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, flags.ValidateOverrides(overrides))
		})
	}
}

func TestValidateOverridesAcceptsTypedValues(t *testing.T) {
	o := flags.Overrides{
		Bools:   map[flags.Key]bool{flags.KeyStruggleEscalationEnabled: true},
		Ints:    map[flags.Key]int{flags.KeyStruggleEscalationHoldout: 50},
		Strings: map[flags.Key]string{flags.KeyCyberRefusalFallback: "claude-opus-5"},
	}
	require.NoError(t, flags.ValidateOverrides(o))
}

func TestAccessorsFallBackToDeploymentDefault(t *testing.T) {
	// A bare context carries no overrides at all.
	assert.True(t, flags.BoolOr(context.Background(), flags.KeyPlannerEnabled, true))
	assert.False(t, flags.BoolOr(context.Background(), flags.KeyPlannerEnabled, false))
	assert.Equal(t, 10, flags.IntOr(context.Background(), flags.KeyLoopEscalationHoldoutPct, 10))
	assert.Equal(t, "a", flags.StringOr(context.Background(), flags.KeyCyberRefusalFallback, "a"))

	// A context carrying *some* overrides still falls back for the others.
	ctx := flags.WithOverrides(context.Background(), flags.Overrides{
		Bools: map[flags.Key]bool{flags.KeyPlannerEnabled: false},
	})
	assert.False(t, flags.BoolOr(ctx, flags.KeyPlannerEnabled, true), "override should win")
	assert.True(t, flags.BoolOr(ctx, flags.KeySiblingFailover, true), "unset flag keeps its default")
	assert.Equal(t, 10, flags.IntOr(ctx, flags.KeyLoopEscalationHoldoutPct, 10))
}

func TestOverrideWinsOverDeploymentDefaultInBothDirections(t *testing.T) {
	// The point of the mechanism: an org can turn a flag ON when the deployment
	// default is off, and OFF when the deployment default is on.
	on := flags.WithOverrides(context.Background(), flags.Overrides{
		Bools: map[flags.Key]bool{flags.KeyEffortEscalation: true},
	})
	assert.True(t, flags.BoolOr(on, flags.KeyEffortEscalation, false))

	off := flags.WithOverrides(context.Background(), flags.Overrides{
		Bools: map[flags.Key]bool{flags.KeyEffortEscalation: false},
	})
	assert.False(t, flags.BoolOr(off, flags.KeyEffortEscalation, true))
}

func TestWithOverridesIgnoresEmptySet(t *testing.T) {
	ctx := flags.WithOverrides(context.Background(), flags.Overrides{})
	_, ok := flags.OverridesFromContext(ctx)
	assert.False(t, ok, "an empty set should not be stashed at all")
}

func TestRegistryIsWellFormed(t *testing.T) {
	seenKeys := map[flags.Key]struct{}{}
	seenEnv := map[string]struct{}{}
	for _, def := range flags.Registry {
		assert.NotEmpty(t, def.Key, "every definition needs a key")
		assert.NotEmpty(t, def.Description, "flag %q needs a description for the admin UI", def.Key)
		assert.Contains(t,
			[]flags.Kind{flags.KindBool, flags.KindInt, flags.KindFloat, flags.KindString},
			def.Kind, "flag %q has an unsupported kind", def.Key)

		_, dupKey := seenKeys[def.Key]
		assert.False(t, dupKey, "duplicate flag key %q", def.Key)
		seenKeys[def.Key] = struct{}{}

		// Two flags sharing an env var would make the published deployment
		// default ambiguous in the admin UI.
		if def.EnvVar != "" {
			_, dupEnv := seenEnv[def.EnvVar]
			assert.False(t, dupEnv, "duplicate env var %q", def.EnvVar)
			seenEnv[def.EnvVar] = struct{}{}
		}

		def, ok := flags.Lookup(def.Key)
		require.True(t, ok, "flag %q must be resolvable via Lookup", def.Key)
	}
}

func TestLookupUnknownKey(t *testing.T) {
	_, ok := flags.Lookup(flags.Key("not_a_flag"))
	assert.False(t, ok)
}

func TestSubscriptionPlanAwareRoutingIsAnOrganizationOnlyBoolean(t *testing.T) {
	def, ok := flags.Lookup(flags.KeySubscriptionPlanAwareRouting)
	require.True(t, ok)
	assert.Equal(t, flags.KindBool, def.Kind)
	assert.True(t, def.OrgOverridable)
	assert.Empty(t, def.EnvVar)
	for _, enabled := range []bool{false, true} {
		overrides := flags.Overrides{Bools: map[flags.Key]bool{flags.KeySubscriptionPlanAwareRouting: enabled}}
		require.NoError(t, flags.ValidateOverrides(overrides))
		stored, err := json.Marshal(overrides)
		require.NoError(t, err)
		loaded, err := flags.ParseOverrides(stored)
		require.NoError(t, err)
		assert.Equal(t, overrides, loaded)
	}
}

func TestKeysIsSortedAcrossKinds(t *testing.T) {
	o, err := flags.ParseOverrides([]byte(`{
		"planner_enabled": true,
		"cyber_refusal_fallback_model": "x",
		"loop_escalation_holdout_pct": 1
	}`))
	require.NoError(t, err)
	assert.Equal(t, []flags.Key{
		flags.KeyCyberRefusalFallback,
		flags.KeyLoopEscalationHoldoutPct,
		flags.KeyPlannerEnabled,
	}, o.Keys())
}
