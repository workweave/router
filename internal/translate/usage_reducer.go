package translate

// UsagePhase identifies where in an upstream response usage was observed.
type UsagePhase string

const (
	// UsagePhaseStart is metadata emitted before response content.
	UsagePhaseStart UsagePhase = "start"
	// UsagePhaseDelta is metadata emitted while a response streams.
	UsagePhaseDelta UsagePhase = "delta"
	// UsagePhaseTerminal is metadata emitted with a completed response.
	UsagePhaseTerminal UsagePhase = "terminal"
	// UsagePhasePostTerminal is metadata received after a terminal response.
	UsagePhasePostTerminal UsagePhase = "post_terminal"
)

// UsageAuthority classifies whether usage may be used for billing.
type UsageAuthority string

const (
	// UsageAuthorityAuthoritative has complete, non-conflicting terminal usage.
	UsageAuthorityAuthoritative UsageAuthority = "authoritative"
	// UsageAuthorityPartial has some usage but lacks complete terminal values.
	UsageAuthorityPartial UsageAuthority = "partial"
	// UsageAuthorityMissing has no observed usage.
	UsageAuthorityMissing UsageAuthority = "missing"
	// UsageAuthorityContradictory has incompatible observed values.
	UsageAuthorityContradictory UsageAuthority = "contradictory"
)

// UsageValues carries presence-aware token counters. A nil field was absent on
// the wire; a pointer to zero was explicitly supplied by the provider.
type UsageValues struct {
	InputTokens              *int `json:"input_tokens,omitempty"`
	OutputTokens             *int `json:"output_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	ReasoningTokens          *int `json:"reasoning_tokens,omitempty"`
	AudioInputTokens         *int `json:"audio_input_tokens,omitempty"`
	AudioOutputTokens        *int `json:"audio_output_tokens,omitempty"`
	AcceptedPredictionTokens *int `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens *int `json:"rejected_prediction_tokens,omitempty"`
	ServerToolTokens         *int `json:"server_tool_tokens,omitempty"`
}

// UsageObservation is one provider usage report. Placeholder marks known
// non-terminal zero counters that must not erase an already-observed value.
type UsageObservation struct {
	Phase       UsagePhase
	Values      UsageValues
	Placeholder bool
}

// UsageContradiction is a stable, content-free code describing incompatible
// reports for one usage field.
type UsageContradiction string

const (
	// UsageContradictionTerminalZero follows a positive value with a terminal
	// explicit zero. The positive value is retained for reconciliation.
	UsageContradictionTerminalZero UsageContradiction = "terminal_zero_after_positive"
)

// UsageSnapshot is the canonical, presence-aware usage state for one request.
type UsageSnapshot struct {
	UsageValues
	Authority      UsageAuthority       `json:"authority_status"`
	Contradictions []UsageContradiction `json:"contradictions,omitempty"`
}

// FreshInputTokens returns input tokens minus cache creation and cache reads, clamped at zero to prevent double-counting.
func (s UsageSnapshot) FreshInputTokens() *int {
	if s.InputTokens == nil {
		return nil
	}
	fresh := *s.InputTokens
	if s.CacheCreationInputTokens != nil {
		fresh -= *s.CacheCreationInputTokens
	}
	if s.CacheReadInputTokens != nil {
		fresh -= *s.CacheReadInputTokens
	}
	if fresh < 0 {
		fresh = 0
	}
	return &fresh
}

// UsageReducer combines usage observations while preserving field presence and
// the provider's terminal authority semantics.
type UsageReducer struct {
	values         UsageValues
	terminalSeen   bool
	contradictions []UsageContradiction
}

// Observe merges one usage report into the reducer.
func (r *UsageReducer) Observe(observation UsageObservation) {
	terminal := observation.Phase == UsagePhaseTerminal
	if terminal && !observation.Placeholder {
		r.terminalSeen = true
	}
	r.mergeField(&r.values.InputTokens, observation.Values.InputTokens, terminal, observation.Placeholder)
	r.mergeField(&r.values.OutputTokens, observation.Values.OutputTokens, terminal, observation.Placeholder)
	r.mergeField(&r.values.CacheCreationInputTokens, observation.Values.CacheCreationInputTokens, terminal, observation.Placeholder)
	r.mergeField(&r.values.CacheReadInputTokens, observation.Values.CacheReadInputTokens, terminal, observation.Placeholder)
	r.mergeField(&r.values.ReasoningTokens, observation.Values.ReasoningTokens, terminal, observation.Placeholder)
	r.mergeField(&r.values.AudioInputTokens, observation.Values.AudioInputTokens, terminal, observation.Placeholder)
	r.mergeField(&r.values.AudioOutputTokens, observation.Values.AudioOutputTokens, terminal, observation.Placeholder)
	r.mergeField(&r.values.AcceptedPredictionTokens, observation.Values.AcceptedPredictionTokens, terminal, observation.Placeholder)
	r.mergeField(&r.values.RejectedPredictionTokens, observation.Values.RejectedPredictionTokens, terminal, observation.Placeholder)
	r.mergeField(&r.values.ServerToolTokens, observation.Values.ServerToolTokens, terminal, observation.Placeholder)
}

func (r *UsageReducer) mergeField(current **int, observed *int, terminal, placeholder bool) {
	if observed == nil {
		return
	}
	if placeholder && *observed == 0 && *current != nil {
		return
	}
	if terminal && *observed == 0 && *current != nil && **current > 0 {
		r.addContradiction(UsageContradictionTerminalZero)
		return
	}
	value := *observed
	*current = &value
}

func (r *UsageReducer) addContradiction(code UsageContradiction) {
	for _, current := range r.contradictions {
		if current == code {
			return
		}
	}
	r.contradictions = append(r.contradictions, code)
}

// Snapshot returns the reducer's current usage state and billing authority.
func (r *UsageReducer) Snapshot() UsageSnapshot {
	if r == nil {
		return UsageSnapshot{Authority: UsageAuthorityMissing}
	}
	snapshot := UsageSnapshot{
		UsageValues:    cloneUsageValues(r.values),
		Contradictions: append([]UsageContradiction(nil), r.contradictions...),
	}
	switch {
	case len(snapshot.Contradictions) > 0:
		snapshot.Authority = UsageAuthorityContradictory
	case r.terminalSeen && snapshot.InputTokens != nil && snapshot.OutputTokens != nil:
		snapshot.Authority = UsageAuthorityAuthoritative
	case hasUsageValues(snapshot.UsageValues):
		snapshot.Authority = UsageAuthorityPartial
	default:
		snapshot.Authority = UsageAuthorityMissing
	}
	return snapshot
}

func hasUsageValues(values UsageValues) bool {
	return values.InputTokens != nil || values.OutputTokens != nil || values.CacheCreationInputTokens != nil || values.CacheReadInputTokens != nil || values.ReasoningTokens != nil || values.AudioInputTokens != nil || values.AudioOutputTokens != nil || values.AcceptedPredictionTokens != nil || values.RejectedPredictionTokens != nil || values.ServerToolTokens != nil
}

func cloneUsageValues(values UsageValues) UsageValues {
	return UsageValues{
		InputTokens:              cloneUsageInt(values.InputTokens),
		OutputTokens:             cloneUsageInt(values.OutputTokens),
		CacheCreationInputTokens: cloneUsageInt(values.CacheCreationInputTokens),
		CacheReadInputTokens:     cloneUsageInt(values.CacheReadInputTokens),
		ReasoningTokens:          cloneUsageInt(values.ReasoningTokens),
		AudioInputTokens:         cloneUsageInt(values.AudioInputTokens),
		AudioOutputTokens:        cloneUsageInt(values.AudioOutputTokens),
		AcceptedPredictionTokens: cloneUsageInt(values.AcceptedPredictionTokens),
		RejectedPredictionTokens: cloneUsageInt(values.RejectedPredictionTokens),
		ServerToolTokens:         cloneUsageInt(values.ServerToolTokens),
	}
}

func cloneUsageInt(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
