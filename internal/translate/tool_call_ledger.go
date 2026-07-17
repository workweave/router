package translate

import (
	"strconv"
	"strings"
)

// ToolCallEntry is one stable, client-visible tool call assembled from a
// potentially fragmented upstream stream. Source identifiers are aliases, not
// replacement IDs: a late identifier can never invalidate an emitted ID.
type ToolCallEntry struct {
	ExternalID  string
	SourceID    string
	SourceIndex int
	Name        string
	Arguments   strings.Builder
	OutputIndex int
	Open        bool
	Closed      bool
	aliases     map[string]struct{}
}

// ToolCallLedger centralizes stream-local tool identity and argument assembly.
// Entries are keyed first by source index because several providers omit IDs
// on initial deltas and some reuse an ID for parallel calls.
type ToolCallLedger struct {
	entries      map[int]*ToolCallEntry
	sourceOwners map[string]int
	nextID       uint64
}

// NewToolCallLedger creates an empty per-stream ledger.
func NewToolCallLedger() *ToolCallLedger {
	return &ToolCallLedger{entries: make(map[int]*ToolCallEntry), sourceOwners: make(map[string]int)}
}

// Upsert records source fields for index and returns its stable entry.
// The first delta's ID is kept as client-visible; late IDs alias it.
func (l *ToolCallLedger) Upsert(index int, sourceID, name string) *ToolCallEntry {
	entry, ok := l.entries[index]
	if !ok {
		l.nextID++
		externalID := sourceID
		if owner, claimed := l.sourceOwners[sourceID]; sourceID != "" && claimed && owner != index {
			externalID = ""
		}
		if externalID == "" {
			externalID = "call_router_" + strconv.FormatUint(l.nextID, 36)
		}
		entry = &ToolCallEntry{
			ExternalID:  externalID,
			SourceIndex: index,
			OutputIndex: index,
			Open:        true,
			aliases:     make(map[string]struct{}),
		}
		l.entries[index] = entry
	}
	if sourceID != "" {
		if entry.SourceID == "" {
			entry.SourceID = sourceID
		}
		if _, claimed := l.sourceOwners[sourceID]; !claimed {
			l.sourceOwners[sourceID] = index
		}
		entry.aliases[sourceID] = struct{}{}
	}
	if name != "" && entry.Name == "" {
		entry.Name = name
	}
	return entry
}

// AppendArguments appends a fragmented argument delta to index.
func (l *ToolCallLedger) AppendArguments(index int, sourceID, name, arguments string) *ToolCallEntry {
	entry := l.Upsert(index, sourceID, name)
	entry.Arguments.WriteString(arguments)
	return entry
}

// Close records a terminal item while preserving the existing external ID.
func (l *ToolCallLedger) Close(index int, sourceID, name, fallbackArguments string) *ToolCallEntry {
	entry := l.Upsert(index, sourceID, name)
	if entry.Arguments.Len() == 0 && fallbackArguments != "" {
		entry.Arguments.WriteString(fallbackArguments)
	}
	entry.Open = false
	entry.Closed = true
	return entry
}

// Entry returns the entry for source index, if it exists.
func (l *ToolCallLedger) Entry(index int) (*ToolCallEntry, bool) {
	entry, ok := l.entries[index]
	return entry, ok
}

// HasSourceID reports whether id was observed as an alias for index.
func (e *ToolCallEntry) HasSourceID(id string) bool {
	_, ok := e.aliases[id]
	return ok
}
