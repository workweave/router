package policy_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
)

func TestMakeArmIDMatchesPythonTemporalQContract(t *testing.T) {
	identity := policy.ArmIdentity{
		CanonicalModel:               "candidate-a",
		Provider:                     "provider-a",
		UpstreamID:                   "candidate-a-upstream",
		Endpoint:                     "responses",
		ModelRevision:                "candidate-a-r1",
		ReasoningConfigurationSHA256: strings.Repeat("b", 64),
		ToolConfigurationSHA256:      strings.Repeat("c", 64),
	}

	assert.Equal(
		t,
		"tq_arm_0758a2ae1bc05e56a3866cf63665ab07f588662f6fc8d15ca208ead5f47d3fae",
		policy.MakeArmID(identity),
	)
}

func TestMakeArmIDBindsEveryConfigurationField(t *testing.T) {
	identity := policy.ArmIdentity{
		CanonicalModel:               "candidate-a",
		Provider:                     "provider-a",
		UpstreamID:                   "candidate-a-upstream",
		Endpoint:                     "responses",
		ModelRevision:                "candidate-a-r1",
		ReasoningConfigurationSHA256: strings.Repeat("b", 64),
		ToolConfigurationSHA256:      strings.Repeat("c", 64),
	}
	changed := identity
	changed.ToolConfigurationSHA256 = strings.Repeat("d", 64)

	assert.NotEqual(t, policy.MakeArmID(identity), policy.MakeArmID(changed))
}

func TestDeriveArmContextIncludesIngressAndForceEffort(t *testing.T) {
	base := router.Request{ReasoningConfigurationSHA256: strings.Repeat("a", 64)}
	forced := base
	forced.RoutingKnobs = &router.Overrides{ForceEffort: "high"}

	assert.NotEqual(
		t,
		policy.DeriveArmContext(base).ReasoningConfigurationSHA256,
		policy.DeriveArmContext(forced).ReasoningConfigurationSHA256,
	)
}
