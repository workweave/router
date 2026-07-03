// Command genprices rewrites the BEGIN_GENERATED_PRICES block in
// install/cc-statusline.sh and install/install.sh from pricingTable in
// internal/observability/otel/pricing.go. Run via `make generate`.
package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"workweave/router/internal/observability/otel"
)

const (
	beginMarker = "# BEGIN_GENERATED_PRICES"
	endMarker   = "# END_GENERATED_PRICES"
)

// scriptPaths lists files containing a BEGIN/END_GENERATED_PRICES block.
// install.sh carries it in a heredoc, letting the installer write
// cc-statusline.sh without a URL fetch.
var scriptPaths = []string{
	"install/cc-statusline.sh",
	"install/install.sh",
}

func main() {
	block := buildBlock(otel.AllPricing())
	for _, path := range scriptPaths {
		if err := spliceFile(path, block); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Wrote %s\n", path)
	}
}

func spliceFile(path, block string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	updated, err := splice(path, string(raw), block)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(updated), 0o755)
}

// buildBlock produces the shell prices='{...}' assignment from the pricing
// table, in USD/1k (= USD/1M ÷ 1000) to match cc-statusline.sh's jq math.
func buildBlock(table map[string]otel.Pricing) string {
	models := make([]string, 0, len(table))
	for m := range table {
		models = append(models, m)
	}
	sort.Strings(models)

	pad := maxLen(models) + 3 // room for `"` + name + `":`

	var b strings.Builder
	b.WriteString("prices='{\n")
	b.WriteString("  \"input\": {\n")
	for i, m := range models {
		entry := fmt.Sprintf("%q:", m)
		comma := ","
		if i == len(models)-1 {
			comma = ""
		}
		fmt.Fprintf(&b, "    %-*s %s%s\n", pad, entry, fmtPrice(table[m].InputUSDPer1M/1000), comma)
	}
	b.WriteString("  },\n")
	b.WriteString("  \"output\": {\n")
	for i, m := range models {
		entry := fmt.Sprintf("%q:", m)
		comma := ","
		if i == len(models)-1 {
			comma = ""
		}
		fmt.Fprintf(&b, "    %-*s %s%s\n", pad, entry, fmtPrice(table[m].OutputUSDPer1M/1000), comma)
	}
	b.WriteString("  }\n")
	b.WriteString("}'")
	return b.String()
}

// splice replaces everything between the begin and end markers (exclusive) with block.
func splice(path, src, block string) (string, error) {
	start := strings.Index(src, beginMarker)
	end := strings.Index(src, endMarker)
	if start < 0 || end <= start {
		return "", fmt.Errorf("%s or %s marker missing in %s", beginMarker, endMarker, path)
	}
	afterBegin := start + len(beginMarker)
	return src[:afterBegin] + "\n" + block + "\n" + src[end:], nil
}

func maxLen(models []string) int {
	n := 0
	for _, m := range models {
		if len(m) > n {
			n = len(m)
		}
	}
	return n
}

// fmtPrice formats a USD/1k price for cc-statusline.sh's jq math. Rounds to
// 6 sig figs first so IEEE 754 artifacts (e.g. 0.071/1000 rendering as
// 0.00007099999999999999) don't leak into the generated block.
func fmtPrice(v float64) string {
	if v == 0 {
		return "0"
	}
	const sigFigs = 6
	scale := math.Pow10(sigFigs - 1 - int(math.Floor(math.Log10(math.Abs(v)))))
	rounded := math.Round(v*scale) / scale
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}
