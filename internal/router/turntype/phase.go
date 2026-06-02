package turntype

import (
	"strings"

	"workweave/router/internal/translate"
)

// Phase classifies the coding-agent workflow phase of a turn. It is orthogonal
// to TurnType: a MainLoop turn may be a planning phase, and a SubAgentDispatch
// turn is the research phase. PhaseNone means the turn is not part of a
// detectable research/plan flow and routes normally.
type Phase string

const (
	PhaseNone     Phase = ""
	PhaseResearch Phase = "research"
	PhasePlanning Phase = "planning"
)

// DetectPhase classifies the coding-agent workflow phase. tt is the
// already-computed turn type so sub-agent detection is not re-run.
//
// Conservative: PhaseNone is the safe default and preserves normal routing.
// Only the two reliably-detectable phases are classified — research (the
// Explore sub-agent) and planning (Claude Code plan mode). Implementation is
// intentionally not detected: Claude Code sends the full tool registry every
// turn, so a deliberate implementation turn is indistinguishable from a normal
// coding turn.
func DetectPhase(env *translate.RequestEnvelope, tt TurnType) Phase {
	if tt == SubAgentDispatch {
		return PhaseResearch
	}
	if isPlanMode(env) {
		return PhasePlanning
	}
	return PhaseNone
}

// isPlanMode reports whether the request is a Claude Code plan-mode turn.
// Mirrors isCompaction: Anthropic-format-gated (plan mode is Claude-Code-only,
// and Claude Code always talks Anthropic wire format), keyed off the
// "Plan mode is active" system-prompt reminder. The ExitPlanMode tool — offered
// only while plan mode is active — is a second independent signal.
func isPlanMode(env *translate.RequestEnvelope) bool {
	if env == nil || env.SourceFormat() != translate.FormatAnthropic {
		return false
	}
	if strings.Contains(strings.ToLower(env.SystemText()), "plan mode is active") {
		return true
	}
	return env.HasToolNamed("ExitPlanMode")
}
