// Command genprices rewrites the BEGIN_GENERATED_PRICES block in both
// install/cc-statusline.sh and install/install.sh (the heredoc copy) from
// pricingTable in internal/observability/otel/pricing.go.
// Run via `make generate` or `go generate ./internal/observability/otel/`.
package main

import (
	"fmt"
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

// scriptPaths lists every file that contains a BEGIN/END_GENERATED_PRICES
// block. install.sh carries the block inside a heredoc so the installer writes
// cc-statusline.sh from its own content — no URL fetch required.
var scriptPaths = []string{
	"install/cc-statusline.sh",
	"install/install.sh",
}

func main() {
	block := buildBlock(otel.AllPricing())
	for _, path := range scriptPaths {
		if err := spliceFile(path, block); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s\n", path)
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

// buildBlock produces the shell prices='{...}' assignment from the pricing table.
// Values are in USD/1k (= USD/1M ÷ 1000) to match what the jq math in
// cc-statusline.sh expects.
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

// fmtPrice formats a USD/1k price with enough precision for the jq math in
// cc-statusline.sh. Uses the shortest decimal representation that round-trips.
func fmtPrice(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
