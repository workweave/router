package catalog_test

import (
	"math"
	"testing"

	"workweave/router/internal/router/catalog"

	"github.com/stretchr/testify/assert"
)

// legacyUsdToMicros is byte-for-byte the pre-consolidation
// internal/postgres/telemetry.go implementation (NaN/Inf guard only, no
// negative guard). Kept here as a golden reference so the shared
// catalog.USDToMicros can be proven to reproduce it exactly.
func legacyUsdToMicros(f float64) int64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int64(math.Round(f * 1_000_000))
}

// legacyComputeNotionalMicros is byte-for-byte the pre-consolidation
// internal/billing/service.go computeNotionalMicros rounding step (NaN/Inf/
// negative guard), applied directly to a precomputed total rather than
// DebitInferenceParams so it can be table-tested against arbitrary floats.
func legacyComputeNotionalMicros(total float64) int64 {
	if math.IsNaN(total) || math.IsInf(total, 0) || total < 0 {
		return 0
	}
	return int64(math.Round(total * 1_000_000))
}

func TestUSDToMicros_MatchesBothLegacyImplementations(t *testing.T) {
	cases := []float64{
		0,
		6.75,
		0.0000005,  // rounds up to 1 micro
		0.00000049, // rounds down to 0 micros
		12.3456785, // exercises round-half behavior on a real fraction-of-cent
		999_999.999999,
		1e-12,
		0.1 + 0.2, // classic float64 imprecision case
	}
	for _, f := range cases {
		got := catalog.USDToMicros(f)
		assert.Equal(t, legacyUsdToMicros(f), got, "diverges from legacy postgres.usdToMicros for %v", f)
		assert.Equal(t, legacyComputeNotionalMicros(f), got, "diverges from legacy billing.computeNotionalMicros for %v", f)
	}
}

func TestUSDToMicros_NaNAndInfCollapseToZero(t *testing.T) {
	assert.Equal(t, int64(0), catalog.USDToMicros(math.NaN()))
	assert.Equal(t, int64(0), catalog.USDToMicros(math.Inf(1)))
	assert.Equal(t, int64(0), catalog.USDToMicros(math.Inf(-1)))
}

func TestUSDToMicros_NegativeCollapsesToZero(t *testing.T) {
	// billing.computeNotionalMicros always guarded negative; postgres.usdToMicros
	// never received a negative input in practice, so extending the guard here
	// is a safe superset, not a behavior change for real traffic.
	assert.Equal(t, int64(0), catalog.USDToMicros(-0.01))
	assert.Equal(t, legacyComputeNotionalMicros(-5), catalog.USDToMicros(-5))
}

func TestUSDToMicros_RoundsHalfAwayFromZero(t *testing.T) {
	// 6.7500005 USD = 6,750,000.5 micros -> rounds to 6,750,001 (math.Round
	// rounds half away from zero, matching both legacy implementations).
	assert.Equal(t, int64(6_750_001), catalog.USDToMicros(6.7500005))
}
