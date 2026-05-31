package proxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSuppressMarkerIfRequested pins the opt-out decision: the routing marker is
// dropped only for the recognized disable values, and preserved otherwise.
func TestSuppressMarkerIfRequested(t *testing.T) {
	const marker = "✦ **Weave Router** → deepseek/deepseek-v4-flash · best pick for this turn\n\n"
	cases := []struct {
		name      string
		setHeader bool
		value     string
		want      string
	}{
		{name: "absent header keeps marker", setHeader: false, want: marker},
		{name: "off suppresses", setHeader: true, value: "off", want: ""},
		{name: "OFF is case-insensitive", setHeader: true, value: "OFF", want: ""},
		{name: "surrounding whitespace tolerated", setHeader: true, value: "  off  ", want: ""},
		{name: "false suppresses", setHeader: true, value: "false", want: ""},
		{name: "0 suppresses", setHeader: true, value: "0", want: ""},
		{name: "none suppresses", setHeader: true, value: "none", want: ""},
		{name: "on keeps marker", setHeader: true, value: "on", want: marker},
		{name: "unrecognized value keeps marker", setHeader: true, value: "yes", want: marker},
		{name: "empty value keeps marker", setHeader: true, value: "", want: marker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.setHeader {
				h.Set(routingMarkerHeader, tc.value)
			}
			assert.Equal(t, tc.want, suppressMarkerIfRequested(h, marker))
		})
	}
}
