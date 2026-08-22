package observability

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafeGoPanicsRecovered(t *testing.T) {
	SafeGo(slog.Default(), 500*time.Millisecond, "panic-test", func(context.Context) {
		panic("boom")
	})
	// If the panic escaped, the goroutine would crash the test binary; just
	// giving the goroutine a chance to run is the assertion.
	time.Sleep(50 * time.Millisecond)
}

func TestTrackedGroupCancelAbortsInflight(t *testing.T) {
	g := NewTrackedGroup()
	started := make(chan struct{})
	exited := make(chan struct{})

	SafeGoTracked(g, slog.Default(), 5*time.Second, "blocker", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(exited)
	})

	require.Eventually(t, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}, 3*time.Second, 5*time.Millisecond, "goroutine should start")

	g.Cancel()
	select {
	case <-exited:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Cancel must abort the in-flight operation")
	}
}

// TestTrackedGroupWaitBounded ensures a group whose operations never finish on
// their own still returns from WaitWithContext when the shutdown budget
// expires, so a slow debit cannot hold the drain past SIGKILL.
func TestTrackedGroupWaitBounded(t *testing.T) {
	g := NewTrackedGroup()
	SafeGoTracked(g, slog.Default(), 5*time.Second, "forever", func(ctx context.Context) {
		<-ctx.Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	g.WaitWithContext(ctx)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 4*time.Second, "drain must not wait for an overrunning operation")

	// The overrunning operation is aborted by Cancel, so it does not leak past
	// shutdown.
	g.Cancel()
	done := make(chan struct{})
	go func() {
		g.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("operation not aborted by Cancel")
	}
}
