package proxy

import (
	"encoding/json"

	"workweave/router/internal/translate"
)

// usageTelemetryDetails serializes canonical usage state; never retains prompt, tool, or media content.
func usageTelemetryDetails(snapshot translate.UsageSnapshot) []byte {
	details, err := json.Marshal(snapshot)
	if err != nil {
		return nil
	}
	return details
}
