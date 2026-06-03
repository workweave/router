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

// pinEvictionStrikeThreshold is the consecutive-non-retryable-4xx count
// that trips a sticky pin to expire. Two strikes (not one) tolerates a
// single transient 400 — e.g. a malformed prompt or a brief upstream
// schema-validation hiccup — without flushing a working pin's prompt
// cache. Hit two in a row and the model is wedged for this session;
// re-route via the cluster scorer rather than wait for the user to
// notice and manually /force-model out.
const pinEvictionStrikeThreshold = 2

// maybeEvictPinAfterUpstreamErr applies the two-strike pin-eviction
// policy on every turn that ran against a sticky session pin:
//
//   - Successful turn → reset the counter to 0 (any prior strikes are
//     forgiven the moment the model serves a real response).
//   - Non-retryable upstream 4xx → atomically increment the counter
//     on the existing pin row and, when the new count reaches
//     pinEvictionStrikeThreshold, expire the pin so the NEXT turn
//     re-routes via the cluster scorer.
//
// Skipped paths:
//   - !stickyHit: the pin row was just written this turn with
//     counter=0 (writeNewPin) or doesn't exist; no decision-history
//     yet.
//   - Zero session_key / installation_id: no addressable pin row.
//   - User-forced pins (ReasonUserForceModel or its tier_clamp
//     variant): user explicitly chose this model; auto-eviction would
//     silently override an explicit command. The user retains
//     /unforce-model as the escape hatch.
//   - Retryable upstream status (408 / 429 / 5xx): handled by
//     dispatchWithFallback's retry loop OR represent transient
//     upstream conditions that don't indict the model choice.
//
// Errors from the increment/reset/upsert paths are logged and
// swallowed — eviction is a recovery optimization on a turn that
// already succeeded or failed; failing it must not change the
// client-visible outcome.
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
	// Force-model pins are user commands; the router does not auto-evict
	// them. ReasonUserForceModel + the "+tier_clamp" suffix are both
	// surfaced verbatim in decision_reason; cover both with a prefix
	// check.
	if strings.HasPrefix(decisionReason, translate.ReasonUserForceModel) {
		return
	}

	log := observability.FromContext(ctx)
	pinCacheKey := sessionPinCacheKey(sessionKey, role)

	if proxyErr == nil {
		// Background ctx: the request ctx is canceled by the time the
		// response has finished streaming, but the success-reset is a
		// best-effort no-op on a missing pin so we don't want it
		// silently dropped by ctx cancellation.
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

	// Threshold reached. Expire the pin so the NEXT turn re-routes via
	// the cluster scorer. Mirrors the loop-break / no-progress / force
	// -model "expired pin" pattern: upsert a row with PinnedUntil in
	// the past so loadPin discards it, plus an in-proc cache evict so
	// the next turn doesn't serve the stale entry.
	expired := sessionpin.Pin{
		SessionKey:     sessionKey,
		Role:           role,
		InstallationID: installationID,
		Provider:       "",
		Model:          "",
		Reason:         "upstream_error_strike_threshold",
		TurnCount:      1,
		PinnedUntil:    time.Now().Add(-time.Second),
	}
	if err := s.pinStore.Upsert(context.Background(), expired); err != nil {
		log.Error("pin eviction upsert failed", "err", err, "role", role)
		return
	}
	if s.pinCache != nil {
		s.pinCache.Remove(pinCacheKey)
	}
	log.Info("session pin evicted after consecutive upstream errors",
		"role", role,
		"upstream_status", status,
		"consecutive_errors", count,
		"strike_threshold", pinEvictionStrikeThreshold,
	)
}
