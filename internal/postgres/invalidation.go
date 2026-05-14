package postgres

import (
	"context"
	"sync"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"

	"github.com/jackc/pgx/v5/pgxpool"
)

// InvalidationChannel is the Postgres LISTEN/NOTIFY channel that carries
// per-installation cache invalidation events between replicas.
const InvalidationChannel = "router_installation_invalidate"

// notifyTimeout bounds the publish call so a slow / unhealthy DB connection
// can't stall the request that triggered the settings change. The write has
// already committed at this point; missing the NOTIFY only delays peer
// replicas until their cache TTL fires.
const notifyTimeout = 2 * time.Second

// PgxInvalidationNotifier publishes installation invalidation events over
// Postgres LISTEN/NOTIFY so every replica sharing the same database drops
// the affected entries on the next request.
type PgxInvalidationNotifier struct {
	pool *pgxpool.Pool
}

// NewPgxInvalidationNotifier constructs a notifier backed by the supplied pool.
func NewPgxInvalidationNotifier(pool *pgxpool.Pool) *PgxInvalidationNotifier {
	return &PgxInvalidationNotifier{pool: pool}
}

// NotifyInstallationChanged publishes installationID on the invalidation
// channel. Fire-and-forget: errors are logged and dropped because the write
// has already committed and TTL is the cross-replica safety net.
func (n *PgxInvalidationNotifier) NotifyInstallationChanged(installationID string) {
	if installationID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		if _, err := n.pool.Exec(ctx, "SELECT pg_notify($1, $2)", InvalidationChannel, installationID); err != nil {
			observability.Get().Warn(
				"Failed to publish installation invalidation",
				"channel", InvalidationChannel,
				"installation_id", installationID,
				"err", err,
			)
		}
	}()
}

// InvalidationListener subscribes to the invalidation channel on a dedicated
// connection and forwards every payload to the local cache. Survives transient
// connection failures by reconnecting with backoff; under a sustained outage
// the cache's TTL acts as the safety net.
type InvalidationListener struct {
	pool  *pgxpool.Pool
	cache auth.APIKeyCache

	stopOnce sync.Once
	done     chan struct{}
}

// NewInvalidationListener wires a listener that drops entries from cache when
// a peer replica (or this replica's own writers) publishes on the channel.
func NewInvalidationListener(pool *pgxpool.Pool, cache auth.APIKeyCache) *InvalidationListener {
	return &InvalidationListener{
		pool:  pool,
		cache: cache,
		done:  make(chan struct{}),
	}
}

// Run blocks until ctx is canceled. Reconnects with bounded backoff on errors.
func (l *InvalidationListener) Run(ctx context.Context) {
	log := observability.Get()
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			close(l.done)
			return
		}
		if err := l.listenLoop(ctx); err != nil && ctx.Err() == nil {
			log.Warn("Invalidation listener disconnected; reconnecting", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				close(l.done)
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		// listenLoop returned cleanly because ctx was canceled.
		close(l.done)
		return
	}
}

// listenLoop holds a single connection for the lifetime of one LISTEN session.
func (l *InvalidationListener) listenLoop(ctx context.Context) error {
	log := observability.Get()
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+InvalidationChannel); err != nil {
		return err
	}
	log.Info("Invalidation listener active", "channel", InvalidationChannel)

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if notification == nil {
			continue
		}
		l.cache.InvalidateInstallation(notification.Payload)
	}
}

// Wait blocks until Run has returned. Intended for shutdown coordination.
func (l *InvalidationListener) Wait() { <-l.done }
