package translate_test

import (
	"errors"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamLifecycle_RequiresOneOrderedTerminal(t *testing.T) {
	l := translate.NewStreamLifecycle()
	require.ErrorIs(t, l.Output(0), translate.ErrStreamOrder)
	require.NoError(t, l.Start())
	require.NoError(t, l.Output(2))
	require.NoError(t, l.Output(2), "multiple deltas share an output index")
	require.ErrorIs(t, l.Output(1), translate.ErrStreamOrder)
	require.NoError(t, l.Terminal())
	require.NoError(t, l.EOF())
	require.ErrorIs(t, l.Output(2), translate.ErrStreamOrder)
	require.ErrorIs(t, l.Terminal(), translate.ErrStreamOrder)
}

func TestStreamLifecycle_IncompleteAndCancellationRemainDistinct(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		assert.ErrorIs(t, translate.NewStreamLifecycle().EOF(), translate.ErrStreamEmpty)
	})
	t.Run("incomplete", func(t *testing.T) {
		l := translate.NewStreamLifecycle()
		require.NoError(t, l.Start())
		assert.ErrorIs(t, l.EOF(), translate.ErrStreamIncomplete)
	})
	t.Run("canceled", func(t *testing.T) {
		l := translate.NewStreamLifecycle()
		require.NoError(t, l.Start())
		require.NoError(t, l.Cancel())
		assert.NoError(t, l.EOF())
		assert.False(t, errors.Is(l.EOF(), translate.ErrStreamIncomplete))
	})
}
