package main

import "testing"

// TestFmtPriceRoundsAwayFloat64Artifacts pins fmtPrice to a clean decimal form
// for prices that round-trip through 0.<value>/1000 in float64. The naive
// FormatFloat(-1) representation leaks IEEE 754 noise (e.g. 0.071/1000
// renders as 0.00007099999999999999) into the generated install.sh /
// cc-statusline.sh prices block, which is checked into the repo and copied
// into WorkWeave verbatim by the bump-workweave-pin workflow.
//
// Inputs are taken from float64 variables so the test mirrors the runtime
// path genprices walks (struct field / 1000), not Go's untyped-constant
// arithmetic which silently uses higher-than-float64 precision.
func TestFmtPriceRoundsAwayFloat64Artifacts(t *testing.T) {
	cases := []struct {
		name          string
		inputUSDPer1M float64
		want          string
	}{
		// USD/1M values that, divided by 1000 at runtime, leak IEEE 754 noise.
		{"qwen3-235b input", 0.071, "0.000071"},
		{"qwen3-coder-next input", 0.070, "0.00007"},
		{"qwen3-next-80b input", 0.090, "0.00009"},
		{"qwen3.5-flash input", 0.065, "0.000065"},
		{"deepseek-v4-flash input", 0.140, "0.00014"},
		{"qwen3-235b output", 0.463, "0.000463"},
		{"deepseek-v4-flash output", 0.280, "0.00028"},
		{"qwen3.5-flash output", 0.260, "0.00026"},

		// Values that were already clean must stay clean.
		{"claude-opus input", 15.00, "0.015"},
		{"claude-haiku input", 0.80, "0.0008"},
		{"gemini-2.0-flash-lite input", 0.075, "0.000075"},
		{"gpt-5.5-pro input", 30.00, "0.03"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fmtPrice(tc.inputUSDPer1M / 1000)
			if got != tc.want {
				t.Fatalf("fmtPrice(%g/1000) = %q; want %q", tc.inputUSDPer1M, got, tc.want)
			}
		})
	}
}

func TestFmtPriceZero(t *testing.T) {
	if got := fmtPrice(0); got != "0" {
		t.Fatalf("fmtPrice(0) = %q; want %q", got, "0")
	}
}
