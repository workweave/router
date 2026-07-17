package translate_test

import (
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolCallLedger_IdlessCallKeepsGeneratedIDWhenSourceArrivesLate(t *testing.T) {
	l := translate.NewToolCallLedger()
	entry := l.AppendArguments(4, "", "", `{"path":`)
	generated := entry.ExternalID
	require.NotEmpty(t, generated)
	assert.Empty(t, entry.SourceID)

	entry = l.AppendArguments(4, "call_real", "Read", `"a.go"}`)
	assert.Equal(t, generated, entry.ExternalID)
	assert.Equal(t, "call_real", entry.SourceID)
	assert.Equal(t, "Read", entry.Name)
	assert.Equal(t, `{"path":"a.go"}`, entry.Arguments.String())
	assert.True(t, entry.HasSourceID("call_real"))
}

func TestToolCallLedger_ParallelAndDuplicateSourceIDsNeverCollide(t *testing.T) {
	l := translate.NewToolCallLedger()
	first := l.Upsert(0, "duplicate", "Read")
	second := l.Upsert(1, "duplicate", "Write")
	assert.NotEqual(t, first.ExternalID, second.ExternalID,
		"a reused source ID must remain scoped to its source index")

	closed := l.Close(1, "duplicate", "", `{"path":"b.go"}`)
	assert.True(t, closed.Closed)
	assert.False(t, closed.Open)
	assert.Equal(t, `{"path":"b.go"}`, closed.Arguments.String())
}
