package proxy

import (
	"encoding/json"

	"workweave/router/internal/translate"
)

// usageTelemetryDetails serializes the canonical usage state for durable
// telemetry. UsageSnapshot contains token counters and stable codes only, so
// this cannot retain prompt, tool, or media content.
func usageTelemetryDetails(snapshot translate.UsageSnapshot) []byte {
	details, err := json.Marshal(snapshot)
	if err != nil {
		return nil
	}
	return details
}
