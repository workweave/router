package translate_test

import (
	"testing"

	"workweave/router/internal/translate"
)

func FuzzUsageReducer(f *testing.F) {
	f.Add(10, 2, 4, 1)
	f.Add(100, 0, 0, 7)
	f.Add(0, 0, 0, 0)

	f.Fuzz(func(t *testing.T, input, output, cacheRead, terminalOutput int) {
		input = boundedUsageFuzzInt(input)
		output = boundedUsageFuzzInt(output)
		cacheRead = boundedUsageFuzzInt(cacheRead)
		terminalOutput = boundedUsageFuzzInt(terminalOutput)

		var reducer translate.UsageReducer
		reducer.Observe(translate.UsageObservation{
			Phase: translate.UsagePhaseStart,
			Values: translate.UsageValues{
				InputTokens:          &input,
				CacheReadInputTokens: &cacheRead,
			},
			Placeholder: true,
		})
		reducer.Observe(translate.UsageObservation{
			Phase:  translate.UsagePhaseDelta,
			Values: translate.UsageValues{OutputTokens: &output},
		})
		reducer.Observe(translate.UsageObservation{
			Phase:  translate.UsagePhaseTerminal,
			Values: translate.UsageValues{OutputTokens: &terminalOutput},
		})

		snapshot := reducer.Snapshot()
		if fresh := snapshot.FreshInputTokens(); fresh != nil && *fresh < 0 {
			t.Fatalf("fresh input tokens must be non-negative: %d", *fresh)
		}
	})
}

func boundedUsageFuzzInt(value int) int {
	if value < 0 {
		return int((uint64(-(value + 1)) + 1) % 1_000_000)
	}
	return value % 1_000_000
}
