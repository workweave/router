package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// familyVersionPattern matches "<family><major>[.-<minor>][-<suffix>]" —
// e.g. claude-sonnet-4-6, gpt-5.4-mini, kimi-k2.7. IDs with no generation
// number (gpt-4o) don't match and are treated as singleton families.
var familyVersionPattern = regexp.MustCompile(`^(.+?)(\d+)(?:[.-](\d+))?(-[a-z][a-z0-9\-]*)?$`)

// FamilyAndVersion parses id into a family key (generation stripped, suffix
// kept, e.g. "gpt-5.4-mini" → family "gpt-mini") and a (major, minor) version
// tuple. ok=false means id has no generation number; treat as singleton family.
func FamilyAndVersion(id string) (family string, version [2]int, ok bool) {
	m := familyVersionPattern.FindStringSubmatch(id)
	if m == nil {
		return "", [2]int{}, false
	}
	major, err := strconv.Atoi(m[2])
	if err != nil {
		return "", [2]int{}, false
	}
	minor := 0
	if m[3] != "" {
		minor, err = strconv.Atoi(m[3])
		if err != nil {
			return "", [2]int{}, false
		}
	}
	return strings.TrimSuffix(m[1], "-") + m[4], [2]int{major, minor}, true
}

// versionLess reports whether a is an earlier generation than b.
func versionLess(a, b [2]int) bool {
	if a[0] != b[0] {
		return a[0] < b[0]
	}
	return a[1] < b[1]
}

// FamilyDuplicates returns, for each family with 2+ members, the non-newest
// ids paired with the newest superseder. IDs with ok=false are exempt.
func FamilyDuplicates(ids []string) []Duplicate {
	type member struct {
		id      string
		version [2]int
	}
	byFamily := make(map[string][]member)
	for _, id := range ids {
		family, version, ok := FamilyAndVersion(id)
		if !ok {
			continue
		}
		byFamily[family] = append(byFamily[family], member{id: id, version: version})
	}

	var out []Duplicate
	for family, members := range byFamily {
		if len(members) < 2 {
			continue
		}
		newest := members[0]
		for _, m := range members[1:] {
			if versionLess(newest.version, m.version) {
				newest = m
			}
		}
		for _, m := range members {
			if m.id == newest.id {
				continue
			}
			out = append(out, Duplicate{Family: family, Superseded: m.id, SupersededBy: newest.id})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Family != out[j].Family {
			return out[i].Family < out[j].Family
		}
		return out[i].Superseded < out[j].Superseded
	})
	return out
}

// Duplicate is one FamilyDuplicates finding: Superseded should be dropped in
// favor of the already-present, strictly newer SupersededBy.
type Duplicate struct {
	Family       string
	Superseded   string
	SupersededBy string
}

// String renders a human-readable line for test failure output.
func (d Duplicate) String() string {
	return fmt.Sprintf("family %q: %q is superseded by %q (already deployed) — drop it from deployed_models", d.Family, d.Superseded, d.SupersededBy)
}
