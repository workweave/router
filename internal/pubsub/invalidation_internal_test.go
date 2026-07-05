package pubsub

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/auth"

	gcppubsub "cloud.google.com/go/pubsub/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePublisher is an in-memory stand-in for the publisher interface, letting
// tests exercise NotifyInstallationChanged's guard/goroutine logic without a
// real Pub/Sub connection.
type fakePublisher struct {
	mu        sync.Mutex
	published []string
	err       error
	done      chan struct{}
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{done: make(chan struct{}, 8)}
}

func (f *fakePublisher) Publish(_ context.Context, installationID string) error {
	f.mu.Lock()
	f.published = append(f.published, installationID)
	err := f.err
	f.mu.Unlock()
	f.done <- struct{}{}
	return err
}

func (f *fakePublisher) Published() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.published))
	copy(out, f.published)
	return out
}

func (f *fakePublisher) waitForPublish(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Publish to be called")
	}
}

func TestNotifyInstallationChanged_EmptyIDNeverPublishes(t *testing.T) {
	fp := newFakePublisher()
	n := &InvalidationNotifier{publisher: fp, stop: func() {}}

	n.NotifyInstallationChanged("")

	select {
	case <-fp.done:
		t.Fatal("Publish must not be called for an empty installation ID")
	case <-time.After(50 * time.Millisecond):
	}
	assert.Empty(t, fp.Published())
}

func TestNotifyInstallationChanged_PublishesInstallationID(t *testing.T) {
	fp := newFakePublisher()
	n := &InvalidationNotifier{publisher: fp, stop: func() {}}

	n.NotifyInstallationChanged("install-123")

	fp.waitForPublish(t)
	assert.Equal(t, []string{"install-123"}, fp.Published())
}

func TestNotifyInstallationChanged_PublishErrorDoesNotPanic(t *testing.T) {
	fp := newFakePublisher()
	fp.err = errors.New("boom")
	n := &InvalidationNotifier{publisher: fp, stop: func() {}}

	assert.NotPanics(t, func() {
		n.NotifyInstallationChanged("install-456")
		fp.waitForPublish(t)
	})
	assert.Equal(t, []string{"install-456"}, fp.Published())
}

func TestInvalidationNotifier_StopCallsUnderlyingStop(t *testing.T) {
	called := false
	n := &InvalidationNotifier{publisher: newFakePublisher(), stop: func() { called = true }}

	n.Stop()

	assert.True(t, called, "Stop must delegate to the wrapped Publisher.Stop")
}

// fakeSubscriber is an in-memory stand-in for subscriberReceiver, letting
// tests drive Run's message-handling loop without a real subscription.
type fakeSubscriber struct {
	messages []*gcppubsub.Message
}

func (f *fakeSubscriber) String() string { return "fake-subscription" }

func (f *fakeSubscriber) Receive(ctx context.Context, handler func(context.Context, *gcppubsub.Message)) error {
	for _, msg := range f.messages {
		handler(ctx, msg)
	}
	<-ctx.Done()
	return ctx.Err()
}

// fakeAPIKeyCache records InvalidateInstallation calls.
type fakeAPIKeyCache struct {
	auth.NoOpAPIKeyCache
	mu          sync.Mutex
	invalidated []string
}

func (f *fakeAPIKeyCache) InvalidateInstallation(installationID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, installationID)
}

func (f *fakeAPIKeyCache) Invalidated() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.invalidated))
	copy(out, f.invalidated)
	return out
}

func TestInvalidationListener_RunForwardsNonEmptyMessagesToCache(t *testing.T) {
	sub := &fakeSubscriber{messages: []*gcppubsub.Message{
		{Data: []byte("install-1")},
		{Data: []byte("")},
		{Data: []byte("install-2")},
	}}
	cache := &fakeAPIKeyCache{}
	l := NewInvalidationListener(nil, cache)
	l.subscriber = sub

	ctx, cancel := context.WithCancel(context.Background())
	go l.Run(ctx)

	require.Eventually(t, func() bool {
		return len(cache.Invalidated()) == 2
	}, time.Second, 5*time.Millisecond, "Run must invalidate the cache for every non-empty message")
	assert.Equal(t, []string{"install-1", "install-2"}, cache.Invalidated())

	cancel()
	l.Wait()
}

func TestInvalidationListener_WaitBlocksUntilRunReturns(t *testing.T) {
	sub := &fakeSubscriber{}
	l := NewInvalidationListener(nil, &fakeAPIKeyCache{})
	l.subscriber = sub

	ctx, cancel := context.WithCancel(context.Background())
	waitReturned := make(chan struct{})
	go func() {
		l.Wait()
		close(waitReturned)
	}()

	go l.Run(ctx)

	select {
	case <-waitReturned:
		t.Fatal("Wait must not return before Run has completed")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case <-waitReturned:
	case <-time.After(time.Second):
		t.Fatal("Wait must return once Run has returned")
	}
}
