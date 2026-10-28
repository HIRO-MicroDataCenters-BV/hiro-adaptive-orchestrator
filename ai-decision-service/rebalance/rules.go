package rebalance

import (
	"time"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// Decide answers one rebalance request: gates first, then the first rule that matches.
func Decide(req placement.DecisionRequest,
	now time.Time, rebalanceCfg Config,
	scoreConfig scorer.Config) placement.RebalanceDecisionResponse {
	g := applyGates(req, now, rebalanceCfg)
	if resp, ok := deferForEAOSlot(req, g, now, rebalanceCfg); ok {
		return resp
	}
	if resp, ok := moveOffFailedNode(req, g, now, scoreConfig); ok {
		return resp
	}
	return noOp(req, g)
}
