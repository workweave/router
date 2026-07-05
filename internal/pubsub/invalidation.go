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

// replicaSubscriptionTTL reclaims subscriptions leaked by replicas that
// crash before running their deferred cleanup.
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
// Fire-and-forget: the write already committed, so failures just log and rely on cache TTL.
func (n *InvalidationNotifier) NotifyInstallationChanged(installationID string) {
	if installationID == "" {
		return
	}
	log := observability.Get().With("installation_id", installationID)
	observability.SafeGo(log, notifyTimeout, "NotifyInstallationChanged", func(ctx context.Context) {
		result := n.publisher.Publish(ctx, &gcppubsub.Message{Data: []byte(installationID)})
		if _, err := result.Get(ctx); err != nil {
			log.Warn("Failed to publish installation invalidation", "err", err)
		}
	})
}

// Stop flushes buffered messages and shuts the publisher down. Must be
// called on graceful shutdown — Client.Close() does not stop publishers.
func (n *InvalidationNotifier) Stop() {
	n.publisher.Stop()
}

// InvalidationListener subscribes to the invalidation topic and forwards every
// payload to the local cache; cache TTL is the fallback under sustained outages.
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
// done is closed via defer so Wait() can't hang if a caller wraps Run with
// panic recovery (e.g. safeGo) — the defer still fires as the panic unwinds.
func (l *InvalidationListener) Run(ctx context.Context) {
	defer close(l.done)
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
}

// Wait blocks until Run has returned. Intended for shutdown coordination.
func (l *InvalidationListener) Wait() { <-l.done }

// CreateReplicaSubscription provisions a per-replica subscription on topicID so
// every replica sees every invalidation message — a shared subscription would
// load-balance and defeat the broadcast. Returns the subscription name and a
// cleanup func; cleanup uses context.Background() so it still runs if the
// caller's context is already canceled at shutdown.
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
