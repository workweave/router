package pubsub

import (
	"context"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"

	gcppubsub "cloud.google.com/go/pubsub/v2"
)

// notifyTimeout bounds the publish call so a slow Pub/Sub connection can't
// stall the request that triggered the settings change.
const notifyTimeout = 2 * time.Second

// InvalidationNotifier publishes installation invalidation events over GCP Pub/Sub.
type InvalidationNotifier struct {
	publisher *gcppubsub.Publisher
}

// NewInvalidationNotifier constructs a notifier backed by the supplied Publisher.
func NewInvalidationNotifier(publisher *gcppubsub.Publisher) *InvalidationNotifier {
	return &InvalidationNotifier{publisher: publisher}
}

// NotifyInstallationChanged publishes installationID on the invalidation topic.
// Fire-and-forget: errors are logged and dropped because the write has already
// committed and TTL is the cross-replica safety net.
func (n *InvalidationNotifier) NotifyInstallationChanged(installationID string) {
	if installationID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		result := n.publisher.Publish(ctx, &gcppubsub.Message{Data: []byte(installationID)})
		if _, err := result.Get(ctx); err != nil {
			observability.Get().Warn(
				"Failed to publish installation invalidation",
				"installation_id", installationID,
				"err", err,
			)
		}
	}()
}

// InvalidationListener subscribes to the invalidation topic and forwards every
// payload to the local cache. Pub/Sub's built-in retry handles transient failures;
// under a sustained outage the cache TTL acts as the safety net.
type InvalidationListener struct {
	subscriber *gcppubsub.Subscriber
	cache      auth.APIKeyCache
	done       chan struct{}
}

// NewInvalidationListener wires a listener that drops entries from cache when
// any replica (including this one's own writers) publishes on the topic.
func NewInvalidationListener(subscriber *gcppubsub.Subscriber, cache auth.APIKeyCache) *InvalidationListener {
	return &InvalidationListener{
		subscriber: subscriber,
		cache:      cache,
		done:       make(chan struct{}),
	}
}

// Run blocks until ctx is canceled, forwarding invalidation messages to cache.
func (l *InvalidationListener) Run(ctx context.Context) {
	log := observability.Get()
	log.Info("Invalidation listener active", "subscription", l.subscriber.String())
	err := l.subscriber.Receive(ctx, func(_ context.Context, msg *gcppubsub.Message) {
		installationID := string(msg.Data)
		if installationID != "" {
			l.cache.InvalidateInstallation(installationID)
		}
		msg.Ack()
	})
	if err != nil && ctx.Err() == nil {
		log.Warn("Invalidation listener receive loop ended unexpectedly", "err", err)
	}
	close(l.done)
}

// Wait blocks until Run has returned. Intended for shutdown coordination.
func (l *InvalidationListener) Wait() { <-l.done }
