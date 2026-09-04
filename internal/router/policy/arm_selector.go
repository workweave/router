package policy

import (
	"context"
	"errors"

	"weave-os/router/internal/router"
)

// ErrNoEligibleArm is returned when deterministic selection exhausts every
// eligible arm reported by the policy sidecar.
var ErrNoEligibleArm = errors.New("no eligible arm in any ranked group")

// SelectionInput is the content-free classification the router selects an arm from.
type SelectionInput struct {
	Strategy           router.Strategy
	ExecutionMode      string
	RouteID            string
	Harness            string
	ClassifierGroup    string
	RankedFallback     []PreviewGroup
	CandidateRosterIDs []string
	QualityBias        *float64
}

// SelectionPick is the router's selected arm.
type SelectionPick struct {
	Group            string
	Arm              string
	ArmScoresByGroup map[string]map[string]float32
}

// ArmSelector picks the served arm from a sidecar classification. An error
// fails the turn: with a classifier-only sidecar there is no arm to fall back to.
type ArmSelector func(ctx context.Context, input SelectionInput) (SelectionPick, error)

// selectionInputFor snapshots the sidecar's classification for the arm selector.
func selectionInputFor(strategy router.Strategy, executionMode string, req router.Request, res Result, resolved ResolvedCandidates) SelectionInput {
	candidateRosterIDs := make([]string, 0, len(resolved.Candidates))
	for _, candidate := range resolved.Candidates {
		candidateRosterIDs = append(candidateRosterIDs, candidate.RosterID)
	}
	input := SelectionInput{
		Strategy:           strategy,
		ExecutionMode:      executionMode,
		RouteID:            res.RouteID,
		Harness:            req.ClientApp,
		ClassifierGroup:    res.PolicyGroup,
		RankedFallback:     res.RankedFallback,
		CandidateRosterIDs: candidateRosterIDs,
	}
	if req.RoutingKnobs != nil && req.RoutingKnobs.QualityBias != nil {
		qualityBias := *req.RoutingKnobs.QualityBias
		input.QualityBias = &qualityBias
	}
	if req.ForceCluster != "" {
		if _, hasOverride := req.ClusterArmOverrides[req.ForceCluster]; !hasOverride {
			for _, group := range res.RankedFallback {
				if group.Group == req.ForceCluster && len(group.EligibleArms) > 0 {
					input.ClassifierGroup = req.ForceCluster
					input.RankedFallback = []PreviewGroup{group}
					break
				}
			}
		}
	}
	return input
}
