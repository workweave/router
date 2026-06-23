package proxy

import (
	"context"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingPinStore captures the last Upsert so the written PinnedUntil can be
// asserted. Get returns nothing — setForceModelPin only reads to carry forward
// LastServedModel.
type recordingPinStore struct {
	upserts []sessionpin.Pin
}

func (s *recordingPinStore) Get(context.Context, [sessionpin.SessionKeyLen]byte, string) (sessionpin.Pin, bool, error) {
	return sessionpin.Pin{}, false, nil
}
func (s *recordingPinStore) Upsert(_ context.Context, p sessionpin.Pin) error {
	s.upserts = append(s.upserts, p)
	return nil
}
func (s *recordingPinStore) UpdateUsage(context.Context, [sessionpin.SessionKeyLen]byte, string, sessionpin.Usage) error {
	return nil
}
func (s *recordingPinStore) IncrementUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) (int, error) {
	return 0, nil
}
func (s *recordingPinStore) ResetUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) error {
	return nil
}
func (s *recordingPinStore) SweepExpired(context.Context) error { return nil }

// TestPinExpiry_UserForcedNeverExpires is the regression for the debug-session
// bug: a /force-model opus pin lapsed on the 1h session TTL during a ~87-minute
// idle gap, so the next turn missed the pin and silently re-routed to a cheaper
// model. User-forced pins must carry the never-expires sentinel; every other
// reason keeps the sliding session TTL.
func TestPinExpiry_UserForcedNeverExpires(t *testing.T) {
	assert.Equal(t, pinNeverExpires, pinExpiry(translate.ReasonUserForceModel),
		"user-forced pins must never expire")
	// The "+tier_clamp" suffix is written when a forced pin is clamped for a
	// turn; the underlying directive is still user-forced and must not expire.
	assert.Equal(t, pinNeverExpires, pinExpiry(translate.ReasonUserForceModel+"+tier_clamp"),
		"a tier-clamped user-forced pin must still never expire")

	got := pinExpiry("cluster:v0.67 top_p=[2,4]")
	assert.True(t, got.After(time.Now()), "cluster pin keeps a live TTL")
	assert.True(t, got.Before(time.Now().Add(pinSessionTTL+time.Minute)),
		"cluster pin keeps the bounded one-hour session TTL, not the sentinel")
}

// TestSetForceModelPin_WritesNeverExpiresSentinel guards the write path: the
// /force-model upsert must persist the never-expires PinnedUntil so an idle gap
// can never silently drop the user's directive.
func TestSetForceModelPin_WritesNeverExpiresSentinel(t *testing.T) {
	store := &recordingPinStore{}
	svc := NewService(nil, nil, nil, false, nil, store, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	var key [sessionpin.SessionKeyLen]byte
	require.NoError(t, svc.setForceModelPin(
		context.Background(), key, roleForTier(0), uuid.New(),
		"claude-opus-4-8", providers.ProviderAnthropic))

	require.Len(t, store.upserts, 1)
	assert.Equal(t, translate.ReasonUserForceModel, store.upserts[0].Reason)
	assert.Equal(t, pinNeverExpires, store.upserts[0].PinnedUntil,
		"a /force-model pin must be written with the never-expires sentinel")
}
