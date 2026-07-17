package translate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

func TestUsageReducerTerminalValuesAreAuthoritative(t *testing.T) {
	input := 100
	output := 0
	terminalInput := 120
	terminalOutput := 25
	var reducer translate.UsageReducer

	reducer.Observe(translate.UsageObservation{
		Phase:       translate.UsagePhaseStart,
		Values:      translate.UsageValues{InputTokens: &input, OutputTokens: &output},
		Placeholder: true,
	})
	reducer.Observe(translate.UsageObservation{
		Phase:  translate.UsagePhaseTerminal,
		Values: translate.UsageValues{InputTokens: &terminalInput, OutputTokens: &terminalOutput},
	})

	snapshot := reducer.Snapshot()
	require.NotNil(t, snapshot.InputTokens)
	require.NotNil(t, snapshot.OutputTokens)
	assert.Equal(t, 120, *snapshot.InputTokens)
	assert.Equal(t, 25, *snapshot.OutputTokens)
	assert.Equal(t, translate.UsageAuthorityAuthoritative, snapshot.Authority)
}

func TestUsageReducerPlaceholderZeroDoesNotEraseKnownUsage(t *testing.T) {
	output := 19
	zero := 0
	var reducer translate.UsageReducer

	reducer.Observe(translate.UsageObservation{Phase: translate.UsagePhaseDelta, Values: translate.UsageValues{OutputTokens: &output}})
	reducer.Observe(translate.UsageObservation{Phase: translate.UsagePhaseDelta, Placeholder: true, Values: translate.UsageValues{OutputTokens: &zero}})

	snapshot := reducer.Snapshot()
	require.NotNil(t, snapshot.OutputTokens)
	assert.Equal(t, 19, *snapshot.OutputTokens)
	assert.Equal(t, translate.UsageAuthorityPartial, snapshot.Authority)
}

func TestUsageReducerTerminalZeroAfterPositiveIsContradictory(t *testing.T) {
	input := 10
	output := 8
	zero := 0
	var reducer translate.UsageReducer

	reducer.Observe(translate.UsageObservation{Phase: translate.UsagePhaseDelta, Values: translate.UsageValues{InputTokens: &input, OutputTokens: &output}})
	reducer.Observe(translate.UsageObservation{Phase: translate.UsagePhaseTerminal, Values: translate.UsageValues{InputTokens: &input, OutputTokens: &zero}})

	snapshot := reducer.Snapshot()
	require.NotNil(t, snapshot.OutputTokens)
	assert.Equal(t, 8, *snapshot.OutputTokens)
	assert.Equal(t, translate.UsageAuthorityContradictory, snapshot.Authority)
	assert.Equal(t, []translate.UsageContradiction{translate.UsageContradictionTerminalZero}, snapshot.Contradictions)
}

func TestUsageReducerFreshInputExcludesCacheTokens(t *testing.T) {
	input := 100
	cacheCreate := 25
	cacheRead := 40
	output := 5
	var reducer translate.UsageReducer

	reducer.Observe(translate.UsageObservation{Phase: translate.UsagePhaseTerminal, Values: translate.UsageValues{
		InputTokens:              &input,
		OutputTokens:             &output,
		CacheCreationInputTokens: &cacheCreate,
		CacheReadInputTokens:     &cacheRead,
	}})

	fresh := reducer.Snapshot().FreshInputTokens()
	require.NotNil(t, fresh)
	assert.Equal(t, 35, *fresh)
}
