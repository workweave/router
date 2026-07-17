package translate

import "errors"

// StreamState describes a translated stream's terminal lifecycle state.
type StreamState uint8

const (
	// StreamIdle has not emitted a protocol start event.
	StreamIdle StreamState = iota
	// StreamStarted has emitted a protocol start event.
	StreamStarted
	// StreamTerminal observed one protocol-defined terminal event.
	StreamTerminal
	// StreamFailed emitted a protocol-defined failure event.
	StreamFailed
	// StreamCanceled records caller cancellation without attributing it to the provider.
	StreamCanceled
)

var (
	// ErrStreamIncomplete means an upstream stream reached EOF without a terminal event.
	ErrStreamIncomplete = errors.New("upstream stream ended before a terminal event")
	// ErrStreamEmpty means an upstream stream reached EOF before it started.
	ErrStreamEmpty = errors.New("upstream stream ended without output")
	// ErrStreamOrder means a writer attempted an invalid lifecycle transition.
	ErrStreamOrder = errors.New("invalid stream lifecycle transition")
)

// StreamLifecycle tracks protocol-neutral streaming invariants. Writers own
// wire encoding; this helper only decides whether their terminal outcome is
// valid and whether an incomplete EOF is retryable before output begins.
type StreamLifecycle struct {
	state         StreamState
	outputStarted bool
	hasOutputIdx  bool
	lastOutputIdx int
}

// NewStreamLifecycle creates an idle lifecycle.
func NewStreamLifecycle() *StreamLifecycle { return &StreamLifecycle{} }

// State returns the lifecycle state.
func (l *StreamLifecycle) State() StreamState { return l.state }

// OutputStarted reports whether the writer emitted protocol output sourced
// from upstream data. Eager router preludes deliberately do not count.
func (l *StreamLifecycle) OutputStarted() bool { return l.outputStarted }

// Start records the protocol's initial event.
func (l *StreamLifecycle) Start() error {
	if l.state != StreamIdle {
		return ErrStreamOrder
	}
	l.state = StreamStarted
	return nil
}

// Output records an upstream-derived output item. Output indexes must never
// go backwards; repeated deltas for one index are valid.
func (l *StreamLifecycle) Output(index int) error {
	if l.state != StreamStarted {
		return ErrStreamOrder
	}
	if l.hasOutputIdx && index < l.lastOutputIdx {
		return ErrStreamOrder
	}
	l.hasOutputIdx = true
	l.lastOutputIdx = index
	l.outputStarted = true
	return nil
}

// Terminal records exactly one protocol-defined success terminal.
func (l *StreamLifecycle) Terminal() error {
	if l.state != StreamStarted {
		return ErrStreamOrder
	}
	l.state = StreamTerminal
	return nil
}

// Fail records exactly one protocol-defined failure terminal.
func (l *StreamLifecycle) Fail() error {
	if l.state != StreamStarted {
		return ErrStreamOrder
	}
	l.state = StreamFailed
	return nil
}

// Cancel records caller cancellation separately from upstream failure.
func (l *StreamLifecycle) Cancel() error {
	if l.state != StreamStarted {
		return ErrStreamOrder
	}
	l.state = StreamCanceled
	return nil
}

// EOF classifies a completed read. A protocol terminal is required for
// success; pre-output EOF is distinguishable so proxy fallback can retry it.
func (l *StreamLifecycle) EOF() error {
	switch l.state {
	case StreamTerminal, StreamFailed, StreamCanceled:
		return nil
	case StreamIdle:
		return ErrStreamEmpty
	case StreamStarted:
		return ErrStreamIncomplete
	default:
		return ErrStreamOrder
	}
}
