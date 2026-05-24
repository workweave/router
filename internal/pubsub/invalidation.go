package pubsub

import (
	"context"
	"fmt"
	"strings"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"

	gcppubsub "cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/durationpb"
)

// notifyTimeout bounds the publish call so a slow Pub/Sub connection can't
// stall the request that triggered the settings change.
const notifyTimeout = 2 * time.Second

// replicaSubscriptionTTL is the expiration policy applied to per-replica
// subscriptions. If a replica crashes without running its deferred cleanup,
// the subscription is reclaimed by GCP after this idle window so leaked
// subscriptions can't accumulate forever.
const replicaSubscriptionTTL = 24 * time.Hour

// subscriptionDeleteTimeout bounds the cleanup call during shutdown so a slow
// Pub/Sub admin API can't hold up termination indefinitely.
const subscriptionDeleteTimeout = 10 * time.Second

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

// Stop flushes any buffered messages and shuts the publisher's background
// goroutines down. Must be called during graceful shutdown — Client.Close()
// does not stop publishers.
func (n *InvalidationNotifier) Stop() {
	n.publisher.Stop()
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

// CreateReplicaSubscription provisions a per-replica subscription on topicID so
// every replica receives every invalidation message. A shared subscription
// would load-balance — only one replica would see each message — defeating
// the cross-fleet broadcast we need for cache invalidation.
//
// The subscription name is "<prefix>-<uuid>" so concurrent replicas don't
// collide. An expiration policy is set so subscriptions leaked by crashed
// replicas are reclaimed automatically.
//
// Returns the fully-qualified subscription name and a cleanup func that
// deletes the subscription. Cleanup uses context.Background() internally so
// shutdown completes even when the parent context is already canceled.
func CreateReplicaSubscription(
	ctx context.Context,
	client *gcppubsub.Client,
	projectID string,
	topicID string,
	prefix string,
) (subscriptionName string, cleanup func(), err error) {
	if prefix == "" {
		return "", nil, fmt.Errorf("subscription prefix is required")
	}
	subID := fmt.Sprintf("%s-%s", strings.TrimRight(prefix, "-"), uuid.NewString())
	subscriptionName = fmt.Sprintf("projects/%s/subscriptions/%s", projectID, subID)
	topicName := fmt.Sprintf("projects/%s/topics/%s", projectID, topicID)

	_, err = client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name:  subscriptionName,
		Topic: topicName,
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{
			Ttl: durationpb.New(replicaSubscriptionTTL),
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("create per-replica subscription: %w", err)
	}

	cleanup = func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), subscriptionDeleteTimeout)
		defer cancel()
		delErr := client.SubscriptionAdminClient.DeleteSubscription(cleanupCtx, &pubsubpb.DeleteSubscriptionRequest{
			Subscription: subscriptionName,
		})
		if delErr != nil {
			observability.Get().Warn(
				"Failed to delete per-replica invalidation subscription; relying on expiration policy",
				"subscription", subscriptionName,
				"err", delErr,
			)
		}
	}
	return subscriptionName, cleanup, nil
}
