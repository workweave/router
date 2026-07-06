package translate

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// AvailableToolNames returns provider-neutral names from the request's tools.
func (e *RequestEnvelope) AvailableToolNames() []string {
	if e == nil {
		return nil
	}
	tools := gjson.GetBytes(e.body, "tools")
	if !tools.IsArray() {
		return nil
	}
	seen := map[string]struct{}{}
	names := make([]string, 0)
	tools.ForEach(func(_, tool gjson.Result) bool {
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(tool.Get("function.name").String())
		}
		if name == "" {
			return true
		}
		if _, ok := seen[name]; ok {
			return true
		}
		seen[name] = struct{}{}
		names = append(names, name)
		return true
	})
	sort.Strings(names)
	return names
}
