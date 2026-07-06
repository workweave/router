package pubsub

import (
	"context"

	"workweave/router/internal/observability"

	gcppubsub "cloud.google.com/go/pubsub/v2"
)

// AutopayNotifier publishes an "org needs a recharge" signal over GCP Pub/Sub
// when the router's debit hook detects a balance crossing below the org's
// autopay threshold. The Weave control-plane subscriber picks it up and charges
// the saved card; a reconciliation sweep backstops any dropped signal.
type AutopayNotifier struct {
	publisher *gcppubsub.Publisher
}

// NewAutopayNotifier constructs a notifier backed by the supplied Publisher.
func NewAutopayNotifier(publisher *gcppubsub.Publisher) *AutopayNotifier {
	return &AutopayNotifier{publisher: publisher}
}

// NotifyRechargeNeeded publishes organizationID on the autopay topic.
// Fire-and-forget: the balance debit has already committed and the
// reconciliation sweep is the safety net, so a publish error is logged and
// dropped rather than propagated onto the already-served request.
func (n *AutopayNotifier) NotifyRechargeNeeded(organizationID string) {
	if organizationID == "" {
		return
	}
	log := observability.Get().With("organization_id", organizationID)
	observability.SafeGo(log, notifyTimeout, "NotifyRechargeNeeded", func(ctx context.Context) {
		result := n.publisher.Publish(ctx, &gcppubsub.Message{Data: []byte(organizationID)})
		if _, err := result.Get(ctx); err != nil {
			log.Warn("Failed to publish autopay recharge signal", "err", err)
		}
	})
}

// Stop flushes buffered messages and shuts the publisher's background
// goroutines down. Must be called during graceful shutdown — Client.Close()
// does not stop publishers.
func (n *AutopayNotifier) Stop() {
	n.publisher.Stop()
}
