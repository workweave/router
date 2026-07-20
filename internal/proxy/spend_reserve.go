package proxy

import (
	"context"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"

	"github.com/google/uuid"
)

// ArmSpendReservations reserves all applicable spend caps in one transaction
// after identity is on ctx and before Proxy*. Returns a release func for
// defer (no-ops after DebitForInference settles). Nil billing is a no-op.
// Agent-shadow evaluation traffic skips entirely (no reservation, no gate),
// matching the billing middleware skips from #787 — synthetic eval must not
// consume production spend budget.
func (s *Service) ArmSpendReservations(ctx context.Context) (context.Context, func(), error) {
	if s == nil || s.billing == nil {
		return ctx, func() {}, nil
	}
	if _, ok := AgentShadowEvalFromContext(ctx); ok {
		return ctx, func() {}, nil
	}
	orgID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	apiKeyID, _ := ctx.Value(APIKeyIDContextKey{}).(string)
	userID := auth.UserIDFrom(ctx)
	requestID := uuid.New().String()
	ctx, release, err := s.billing.ArmSpendReservations(ctx, orgID, apiKeyID, userID, requestID)
	if err != nil {
		observability.FromContext(ctx).Info("Spend reservation refused",
			"err", err,
			"organization_id", orgID,
			"router_user_id", userID,
		)
		return ctx, nil, err
	}
	return ctx, release, nil
}
