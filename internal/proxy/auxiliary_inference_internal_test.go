package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/billing"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/handover"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auxTelemetryRepo captures the telemetry rows billAuxiliaryInference writes.
// fireTelemetry is async, so writes are mutex-guarded and read via waitForRows.
type auxTelemetryRepo struct {
	mu   sync.Mutex
	rows []InsertTelemetryParams
}

func (r *auxTelemetryRepo) InsertRequestTelemetry(_ context.Context, p InsertTelemetryParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, p)
	return nil
}

func (r *auxTelemetryRepo) snapshot() []InsertTelemetryParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]InsertTelemetryParams, len(r.rows))
	copy(out, r.rows)
	return out
}

// waitForRows polls until n rows have landed or the deadline passes, returning
// whatever arrived. Callers assert on the result so a missing row fails with a
// content diff rather than a timeout.
func (r *auxTelemetryRepo) waitForRows(n int) []InsertTelemetryParams {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rows := r.snapshot(); len(rows) >= n {
			return rows
		}
		time.Sleep(5 * time.Millisecond)
	}
	return r.snapshot()
}

func (r *auxTelemetryRepo) GetTelemetrySummary(context.Context, string, time.Time, time.Time) (TelemetrySummary, error) {
	return TelemetrySummary{}, nil
}

func (r *auxTelemetryRepo) GetTelemetryTimeseries(context.Context, string, time.Time, time.Time, string) ([]TelemetryBucket, error) {
	return nil, nil
}

func (r *auxTelemetryRepo) GetTelemetrySummaryAll(context.Context, time.Time, time.Time) (TelemetrySummary, error) {
	return TelemetrySummary{}, nil
}

func (r *auxTelemetryRepo) GetTelemetryTimeseriesAll(context.Context, time.Time, time.Time, string) ([]TelemetryBucket, error) {
	return nil, nil
}

func (r *auxTelemetryRepo) GetTelemetryRows(context.Context, string, time.Time, time.Time, int32) ([]TelemetryRow, error) {
	return nil, nil
}

func (r *auxTelemetryRepo) GetTelemetryRowsAll(context.Context, time.Time, time.Time, int32) ([]TelemetryRow, error) {
	return nil, nil
}

func (r *auxTelemetryRepo) GetTelemetryModelBreakdown(context.Context, string, time.Time, time.Time, string) ([]TelemetryModelBucket, error) {
	return nil, nil
}

func (r *auxTelemetryRepo) GetTelemetryModelBreakdownAll(context.Context, time.Time, time.Time, string) ([]TelemetryModelBucket, error) {
	return nil, nil
}

func (r *auxTelemetryRepo) GetTelemetryBySessionSequence(context.Context, uuid.UUID, []byte, string, int) (TelemetryTurnResult, error) {
	return TelemetryTurnResult{}, nil
}

// auxBillingRepo captures debit params so a test can assert the ledger row and
// the telemetry row describe the same call.
type auxBillingRepo struct {
	mu     sync.Mutex
	debits []billing.DebitParams
}

func (r *auxBillingRepo) GetBalance(context.Context, string) (int64, error) { return 0, nil }
func (r *auxBillingRepo) HasActiveOverride(context.Context, string) (bool, error) {
	return false, nil
}

func (r *auxBillingRepo) DebitInference(_ context.Context, p billing.DebitParams) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.debits = append(r.debits, p)
	return 0, nil
}

func (r *auxBillingRepo) snapshot() []billing.DebitParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]billing.DebitParams, len(r.debits))
	copy(out, r.debits)
	return out
}

func (r *auxBillingRepo) GetAPIKeySpend(context.Context, string) (int64, *int64, bool, error) {
	return 0, nil, false, nil
}

func (r *auxBillingRepo) GetUserMonthlySpendAndLimit(context.Context, string, string) (int64, *int64, error) {
	return 0, nil, nil
}

func (r *auxBillingRepo) GetOrgMonthlySpendAndLimit(context.Context, string) (int64, *int64, error) {
	return 0, nil, nil
}

func (r *auxBillingRepo) GetAutopayConfig(context.Context, string) (bool, int64, error) {
	return false, 0, nil
}

func (r *auxBillingRepo) BillingTablesExist(context.Context) (bool, error) { return true, nil }

const (
	auxTestSessionID = "1a3f5b7c-9d02-4e68-b1c4-7f0e2a6d9c53"
	auxTestOrgID     = "org_aux_test"
	auxTestRequestID = "req-aux-1"
	// auxTestModel must exist in the router catalog: the point of the
	// telemetry row is real cost, and an unpriced model would make the
	// assertions pass on zeros.
	auxTestModel = DefaultHandoverModel
)

// auxTestUsage is a summarizer usage with tokens in every bucket, so a
// dropped cache field shows up as a cost mismatch rather than a silent zero.
func auxTestUsage() handover.Usage {
	return handover.Usage{
		InputTokens:   4000,
		OutputTokens:  700,
		CacheCreation: 1200,
		CacheRead:     9000,
		Model:         auxTestModel,
		Provider:      "anthropic",
	}
}

// auxTestService wires a Service with capturing billing + telemetry repos.
func auxTestService(t *testing.T) (*Service, *auxBillingRepo, *auxTelemetryRepo) {
	t.Helper()
	billingRepo := &auxBillingRepo{}
	telemetryRepo := &auxTelemetryRepo{}
	return &Service{
		billing:   billing.NewService(billingRepo).WithByokFeeRate(0.05),
		telemetry: telemetryRepo,
	}, billingRepo, telemetryRepo
}

// auxTestContext carries the installation + client identity the router's auth
// middleware would have stashed on a real request.
func auxTestContext(installationID uuid.UUID, sessionID string) context.Context {
	ctx := context.WithValue(context.Background(), InstallationIDContextKey{}, installationID.String())
	ctx = context.WithValue(ctx, ExternalIDContextKey{}, auxTestOrgID)
	return context.WithValue(ctx, ClientIdentityContextKey{}, ClientIdentity{
		SessionID: sessionID,
		DeviceID:  "device-aux-1",
		ClientApp: "claude_code",
	})
}

// TestBillAuxiliaryInferenceTagsSessionAndCost is the contract the public
// session-cost endpoint depends on: a billed summarizer call produces a
// telemetry row carrying the SAME client session id as the turn that
// triggered it, with actual cost populated.
func TestBillAuxiliaryInferenceTagsSessionAndCost(t *testing.T) {
	s, _, telemetryRepo := auxTestService(t)
	installationID := uuid.New()
	usage := auxTestUsage()

	s.billAuxiliaryInference(auxTestContext(installationID, auxTestSessionID),
		auxTestRequestID, auxSuffixHandoverSummary, auxTestOrgID, usage)

	rows := telemetryRepo.waitForRows(1)
	require.Len(t, rows, 1, "a billed auxiliary call must write exactly one telemetry row")
	row := rows[0]

	assert.Equal(t, auxTestSessionID, row.SessionID,
		"the auxiliary row must carry the client session id, else session cost undercounts it")
	assert.Equal(t, SpanTypeAuxiliaryInference, row.SpanType,
		"auxiliary calls must not masquerade as router.upstream served turns")
	assert.Equal(t, installationID.String(), row.InstallationID)
	assert.Equal(t, auxTestRequestID+auxSuffixHandoverSummary, row.RequestID,
		"the suffix must match the ledger's request-id convention so the two rows join")
	assert.Equal(t, auxTestRequestID, row.TraceID,
		"the trace id ties the auxiliary call back to the turn that triggered it")

	pricing, ok := catalog.PrimaryPriceFor(auxTestModel)
	require.True(t, ok, "the test model must be priced in the catalog")
	wantInput := catalog.EffectiveInputCost(usage.InputTokens, usage.CacheCreation, usage.CacheRead,
		pricing.InputUSDPer1M, pricing, usage.Provider)
	wantOutput := catalog.EffectiveOutputCost(usage.OutputTokens, pricing.OutputUSDPer1M)
	assert.Greater(t, wantInput+wantOutput, 0.0, "the fixture must produce a non-zero cost")
	assert.InDelta(t, wantInput, row.ActualInputCostUSD, 1e-12)
	assert.InDelta(t, wantOutput, row.ActualOutputCostUSD, 1e-12)
	assert.InDelta(t, wantInput, row.RequestedInputCostUSD, 1e-12,
		"requested mirrors actual: the router added this call, so it earns no savings credit")
	assert.InDelta(t, wantOutput, row.RequestedOutputCostUSD, 1e-12)

	assert.Equal(t, int32(usage.InputTokens), row.InputTokens)
	assert.Equal(t, int32(usage.OutputTokens), row.OutputTokens)
	require.NotNil(t, row.CacheCreationTokens)
	assert.Equal(t, int32(usage.CacheCreation), *row.CacheCreationTokens)
	require.NotNil(t, row.CacheReadTokens)
	assert.Equal(t, int32(usage.CacheRead), *row.CacheReadTokens)
}

// TestBillAuxiliaryInferenceMatchesLedgerAmount proves the telemetry row's
// actual cost and the credit-ledger notional cost carry the same economic
// meaning, so summing either representation of a session yields the same total.
func TestBillAuxiliaryInferenceMatchesLedgerAmount(t *testing.T) {
	s, billingRepo, telemetryRepo := auxTestService(t)
	usage := auxTestUsage()

	s.billAuxiliaryInference(auxTestContext(uuid.New(), auxTestSessionID),
		auxTestRequestID, auxSuffixPrecompactionSummary, auxTestOrgID, usage)

	rows := telemetryRepo.waitForRows(1)
	require.Len(t, rows, 1)
	// fireBilling is async (SafeGo), so poll until the debit lands instead of
	// snapshotting once.
	var debits []billing.DebitParams
	require.Eventually(t, func() bool {
		debits = billingRepo.snapshot()
		return len(debits) >= 1
	}, 2*time.Second, 5*time.Millisecond, "billing debit must be recorded")

	telemetryMicros := catalog.USDToMicros(rows[0].ActualInputCostUSD) +
		catalog.USDToMicros(rows[0].ActualOutputCostUSD)
	assert.Equal(t, debits[0].NotionalCostMicros, telemetryMicros,
		"ledger notional cost and telemetry actual cost must describe the same charge")
	assert.Equal(t, auxTestRequestID+auxSuffixPrecompactionSummary, debits[0].RouterRequestID,
		"the ledger and telemetry rows must share a request id")
}

// TestBillAuxiliaryInferenceSkipsNonCalls proves a skipped or failed
// summarizer writes nothing: a zero-token row would inflate a session's
// request_count with a call that never happened.
func TestBillAuxiliaryInferenceSkipsNonCalls(t *testing.T) {
	cases := []struct {
		name  string
		usage handover.Usage
	}{
		{name: "summarizer never ran", usage: handover.Usage{}},
		{name: "no model reported", usage: handover.Usage{InputTokens: 100, OutputTokens: 10}},
		{name: "no tokens consumed", usage: handover.Usage{Model: auxTestModel, Provider: "anthropic"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, billingRepo, telemetryRepo := auxTestService(t)

			s.billAuxiliaryInference(auxTestContext(uuid.New(), auxTestSessionID),
				auxTestRequestID, auxSuffixHandoverSummary, auxTestOrgID, tc.usage)

			// Give the async telemetry write a chance to land if it were fired.
			time.Sleep(50 * time.Millisecond)
			assert.Empty(t, telemetryRepo.snapshot(), "no upstream call means no telemetry row")
			assert.Empty(t, billingRepo.snapshot(), "no upstream call means no debit")
		})
	}
}

// TestBillAuxiliaryInferenceBillsWithoutInstallation proves an unauthenticated
// / selfhosted path still debits (billing is keyed on external id) but writes
// no telemetry: the telemetry table's installation_id is NOT NULL.
func TestBillAuxiliaryInferenceBillsWithoutInstallation(t *testing.T) {
	s, billingRepo, telemetryRepo := auxTestService(t)

	ctx := context.WithValue(context.Background(), ClientIdentityContextKey{},
		ClientIdentity{SessionID: auxTestSessionID})
	s.billAuxiliaryInference(ctx, auxTestRequestID, auxSuffixHandoverSummary, auxTestOrgID, auxTestUsage())

	// fireBilling is async (SafeGo), so poll rather than sleep.
	require.Eventually(t, func() bool {
		return len(billingRepo.snapshot()) >= 1
	}, 2*time.Second, 5*time.Millisecond, "the customer is still charged for the call")
	assert.Empty(t, telemetryRepo.snapshot(), "no installation means no row to attribute")
}

// TestBillAuxiliaryInferenceUsesSummarizerProviderForBYOK proves BYOK is keyed
// off the summarizer's own provider, not the turn's resolved credential — the
// summarizer dispatches on its own credential context.
func TestBillAuxiliaryInferenceUsesSummarizerProviderForBYOK(t *testing.T) {
	s, billingRepo, _ := auxTestService(t)

	ctx := auxTestContext(uuid.New(), auxTestSessionID)
	ctx = context.WithValue(ctx, ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: "anthropic", Plaintext: []byte("sk-ant-byok")},
	})
	s.billAuxiliaryInference(ctx, auxTestRequestID, auxSuffixHandoverSummary, auxTestOrgID, auxTestUsage())

	// fireBilling is async (SafeGo), so poll until the debit lands instead of
	// sleeping a fixed duration that can race under slow scheduling.
	var debits []billing.DebitParams
	require.Eventually(t, func() bool {
		debits = billingRepo.snapshot()
		return len(debits) >= 1
	}, 2*time.Second, 5*time.Millisecond, "billing debit must be recorded")
	require.Len(t, debits, 1)
	assert.Zero(t, debits[0].DeltaUsdMicros,
		"a BYOK-served summary debits no inference cost — the customer paid their own upstream")
	assert.NotZero(t, debits[0].FeeUsdMicros, "Weave still charges its platform fee")
}
