package toolcheck

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/stretchr/testify/assert"
)

func TestRepairJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" = no repair applied
	}{
		{"truncated mid number", `{"a":1,"b":2`, `{"a":1,"b":2}`},
		{"truncated mid string", `{"a":"hel`, `{"a":"hel"}`},
		{"dangling key", `{"a":1,"b":`, `{"a":1}`},
		{"trailing comma", `{"a":1,`, `{"a":1}`},
		{"nested truncation", `{"a":{"b":[1,2`, `{"a":{"b":[1,2]}}`},
		{"markdown fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"bare fence", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"trailing garbage", `{"a":1} and then some prose`, `{"a":1}`},
		{"not json", `read the file please`, ""},
		{"mismatched closer", `{"a":1]`, ""},
		{"valid input untouched", `{"a":1}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, actions := repairJSON(tc.in)
			if tc.want == "" {
				if got != "" {
					// A repair that yields invalid JSON is also a non-repair
					// from the caller's perspective.
					assert.False(t, gjson.Valid(got), "expected no usable repair, got %q", got)
				}
				return
			}
			assert.JSONEq(t, tc.want, got)
			assert.NotEmpty(t, actions)
		})
	}
}
