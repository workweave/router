// Package percallband is the per-API-call band head: given the trajectory so far
// (a per-session ring buffer of derived actions + timings) and, on MainLoop turns,
// the prompt embedding, it predicts whether the agent's NEXT call is a LARGE- or
// SMALL-band action and returns the routing band. Pure and I/O-free (mirrors the
// turntype/cluster packages); the model + threshold are compiled in via go:embed.
//
// The taxonomy, band map, and feature layout are the Go mirror of
// ml_dev/router_action_classifier/percall/{actions,features}.py. The Go<->Python
// parity is asserted by head_test.go / features_test.go against frozen fixtures, so
// train-time labels and serve-time features cannot silently drift.
//
// Layer-1 (intrinsic) is done and passed; this head runs SHADOW-ONLY until the
// Layer-2 extrinsic A-B clears. See
// router-internal/docs/plans/PLAN_percall_band_swap_layer2.md.
package percallband

// Action is the agent's action on a single LLM call, derived self-supervised from
// the call's response: the primary tool_use name -> category, else "reason" for a
// text-only response.
type Action string

const (
	Reason      Action = "reason"
	Explore     Action = "explore"
	Execute     Action = "execute"
	Edit        Action = "edit"
	Orchestrate Action = "orchestrate"
	Integrate   Action = "integrate"
)

// ActionTypes is the fixed taxonomy order. It MUST match actions.py ACTION_TYPES
// exactly: the one-hot / histogram feature columns are laid out in this order.
var ActionTypes = [...]Action{Reason, Explore, Execute, Edit, Orchestrate, Integrate}

// actionIndex maps an action to its column offset; noneIndex is the padding slot
// for "no prior action" in the lag one-hots.
var actionIndex = func() map[Action]int {
	m := make(map[Action]int, len(ActionTypes))
	for i, a := range ActionTypes {
		m[a] = i
	}
	return m
}()

const noneIndex = len(ActionTypes) // 6

// Band is the routing collapse: LARGE = serve the stronger pinned model, SMALL =
// serve the cheaper paired model.
type Band string

const (
	LargeBand Band = "large"
	SmallBand Band = "small"
)

// tool-name -> action sets (mirror actions.py).
var (
	exploreTools = set("Read", "Grep", "Glob", "LS", "ToolSearch", "NotebookRead", "WebFetch", "WebSearch")
	editTools    = set("Edit", "Write", "MultiEdit", "NotebookEdit", "ApplyPatch")
	executeTools = set("Bash", "PowerShell")
	// Planning / delegation / bookkeeping.
	orchestrateTools = set("Task", "Agent", "Skill", "TaskCreate", "TaskUpdate", "TaskList")
)

func set(names ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}

// ActionOfTool maps a primary tool_use name to its action category. Unknown named
// tools and mcp__* tools fall through to "integrate" (a concrete external
// capability), never "reason" (which is reserved for text-only responses).
func ActionOfTool(tool string) Action {
	switch {
	case has(exploreTools, tool):
		return Explore
	case has(editTools, tool):
		return Edit
	case has(executeTools, tool):
		return Execute
	case has(orchestrateTools, tool):
		return Orchestrate
	default:
		return Integrate
	}
}

func has(m map[string]struct{}, k string) bool {
	_, ok := m[k]
	return ok
}

// DeriveAction returns the agent's action on a call from its response: the primary
// tool_use name -> category, else "reason" when the response is text-only. Shared
// derivation with the offline labeler (actions.py derive_action).
func DeriveAction(hasTool bool, firstTool string) Action {
	if !hasTool || firstTool == "" {
		return Reason
	}
	return ActionOfTool(firstTool)
}
