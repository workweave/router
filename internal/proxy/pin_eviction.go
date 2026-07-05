package proxy

import (
	"context"
	"strings"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
)

// pinEvictionStrikeThreshold is the consecutive-non-retryable-4xx count that
// expires a sticky pin. Two (not one) tolerates a single transient 400
// without flushing a working pin's prompt cache.
const pinEvictionStrikeThreshold = 2

// expireSessionPin writes an already-expired sessionpin.Pin so the next
// turn's loadPin discards it and the session re-routes via the cluster
// scorer. Shared by force-model clear, loop-break/no-progress/
// degenerate-response eviction, and the upstream-error strike threshold —
// call sites differ only in the Reason string recorded for observability.
//
// context.Background(): callers invoke this once the response has already
// streamed or is about to be written, so the request ctx may already be
// canceled; the eviction write must still land or the next turn inherits
// the stale pin.
func (s *Service) expireSessionPin(
	ctx context.Context,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
	reason string,
) error {
	expired := sessionpin.Pin{
		SessionKey:     sessionKey,
		Role:           role,
		InstallationID: installationID,
		Provider:       "",
		Model:          "",
		Reason:         reason,
		TurnCount:      1,
		PinnedUntil:    time.Now().Add(-time.Second),
	}
	return s.pinStore.Upsert(context.Background(), expired)
}

// evictPinAfterDegenerateResponse expires the session pin after a degenerate
// response (end_turn, no tool calls, too few output tokens). The current
// turn already streamed and can't be retried, but evicting ensures the next
// turn re-scores instead of repeating the same misbehaving model.
//
// No-ops when there's no decision history yet (!stickyHit), no addressable
// pin row (zero session_key/installation_id), or the pin was user-forced
// (auto-eviction shouldn't override an explicit /force-model). Upsert errors
// are logged and swallowed since eviction is best-effort.
func (s *Service) evictPinAfterDegenerateResponse(
	ctx context.Context,
	stickyHit bool,
	decisionReason string,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
) {
	if !stickyHit || s.pinStore == nil || installationID == uuid.Nil {
		return
	}
	if sessionKey == ([sessionpin.SessionKeyLen]byte{}) {
		return
	}
	if strings.HasPrefix(decisionReason, translate.ReasonUserForceModel) {
		return
	}

	log := observability.FromContext(ctx)

	if err := s.expireSessionPin(ctx, installationID, sessionKey, role, "degenerate_response"); err != nil {
		log.Error("pin eviction after degenerate response failed", "err", err, "role", role)
		return
	}
	log.Info("session pin evicted after degenerate response",
		"role", role,
	)
}

// maybeEvictPinAfterUpstreamErr applies the two-strike eviction policy for a
// turn run against a sticky pin: a successful turn resets the strike counter,
// a non-retryable upstream 4xx increments it, and hitting
// pinEvictionStrikeThreshold expires the pin so the next turn re-routes via
// the cluster scorer.
//
// No-ops when there's no decision history yet (!stickyHit), no addressable
// pin row (zero session_key/installation_id), the pin was user-forced (user
// keeps /unforce-model as the escape hatch), or the status is retryable
// (408/429/5xx — handled by dispatchWithFallback's retry loop). Errors from
// the increment/reset/upsert are logged and swallowed since eviction is
// best-effort and must not change the client-visible outcome.
func (s *Service) maybeEvictPinAfterUpstreamErr(
	ctx context.Context,
	stickyHit bool,
	proxyErr error,
	decisionReason string,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
) {
	if !stickyHit || s.pinStore == nil || installationID == uuid.Nil {
		return
	}
	if sessionKey == ([sessionpin.SessionKeyLen]byte{}) {
		return
	}
	// Prefix check covers both ReasonUserForceModel and its tier_clamp suffix.
	if strings.HasPrefix(decisionReason, translate.ReasonUserForceModel) {
		return
	}

	log := observability.FromContext(ctx)

	if proxyErr == nil {
		// context.Background(): the request ctx is already canceled by the
		// time streaming finishes, but this reset must still go through.
		if err := s.pinStore.ResetUpstreamErrors(context.Background(), sessionKey, role); err != nil {
			log.Error("pin error-counter reset failed", "err", err, "role", role)
		}
		return
	}

	status := upstreamStatus(proxyErr)
	if status == 0 {
		// Non-upstream error (transport blowup, deadline, etc.) — not a
		// model-quality signal; leave the counter alone.
		return
	}
	if providers.IsRetryableStatus(status) {
		return
	}

	count, err := s.pinStore.IncrementUpstreamErrors(context.Background(), sessionKey, role)
	if err != nil {
		log.Error("pin error-counter increment failed", "err", err, "role", role, "upstream_status", status)
		return
	}
	if count < pinEvictionStrikeThreshold {
		log.Debug("pin error-counter incremented",
			"role", role,
			"upstream_status", status,
			"consecutive_errors", count,
			"strike_threshold", pinEvictionStrikeThreshold,
		)
		return
	}

	// Expire via a PinnedUntil in the past, same pattern as loop-break /
	// no-progress / force-model, so loadPin discards it next turn.
	if err := s.expireSessionPin(ctx, installationID, sessionKey, role, "upstream_error_strike_threshold"); err != nil {
		log.Error("pin eviction upsert failed", "err", err, "role", role)
		return
	}
	log.Info("session pin evicted after consecutive upstream errors",
		"role", role,
		"upstream_status", status,
		"consecutive_errors", count,
		"strike_threshold", pinEvictionStrikeThreshold,
	)
}
