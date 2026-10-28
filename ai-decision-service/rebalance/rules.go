package rebalance

import (
	"time"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

const (
	// evacuationImprovement clears the engine's improvement threshold: leaving a dead node is the strongest reason to move.
	evacuationImprovement = 100
)

// Decide answers one rebalance request: gates first, then the first rule that matches.
func Decide(req placement.DecisionRequest,
	now time.Time, rebalanceCfg Config,
	scoreConfig scorer.Config) placement.RebalanceDecisionResponse {
	g := applyGates(req, now, rebalanceCfg)
	classes := classifyNodes(req, rebalanceCfg)
	if resp, ok := evacuateDownNode(req, g, classes, scorerCfg); ok {
		return resp
	}
	if resp, ok := deferForEAOSlot(req, now, cfg); ok {
		return resp
	}
	}
	return noOp(req, g)
}
