package rebalance

import (
	"fmt"
	"strings"
	"time"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
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
	if resp, ok := evacuateDownNode(req, classes, scoreConfig); ok {
		return resp
	}
	return noOp(req, g)
}

// evacuateDownNode moves one pod off a dead node to the scorer's best healthy candidate.
func evacuateDownNode(req placement.DecisionRequest, classes map[string]nodeClass,
	scorerCfg scorer.Config) (placement.RebalanceDecisionResponse, bool) {
	// 1. The first pod on a dead node.
	var pod placement.PodPlacement
	found := false
	for _, p := range req.RebalanceContext.CurrentPlacements {
		if classes[p.NodeName] == classDown {
			pod = p
			found = true
			break
		}
	}
	if !found {
		return placement.RebalanceDecisionResponse{}, false
	}

	// 2. The best healthy target; the scorer already skips non-candidates.
	scores, _ := scorer.Score(req, scorerCfg)
	var best placement.NodeScore
	for _, s := range scores {
		if classes[s.NodeName] != classHealthy {
			continue // never onto an at-risk node
		}
		if best.NodeName == "" || s.Score > best.Score {
			best = s
		}
	}

	// 3. The answer.
	resp := placement.RebalanceDecisionResponse{RequestID: req.RequestID, DecidedBy: placement.DecidedByRules}
	if best.NodeName == "" {
		resp.Action = orchestrationv1alpha1.RebalanceActionNoOp
		resp.Reason = fmt.Sprintf("node %s is down but no healthy node can take pod %s", pod.NodeName, pod.PodName)
		return resp, true
	}
	resp.Action = orchestrationv1alpha1.RebalanceActionMove
	resp.PodName = pod.PodName
	resp.TargetNode = best.NodeName
	resp.Improvement = evacuationImprovement
	resp.Reason = fmt.Sprintf("node %s is down: move %s to %s (score %.0f)", pod.NodeName, pod.PodName, best.NodeName, best.Score)
	return resp, true
}

// noOp is the safe answer when no rule matched; it lists what the gates blocked.
func noOp(req placement.DecisionRequest, g gateResult) placement.RebalanceDecisionResponse {
	reason := "no rule matched"
	if reasons := g.blockedReasons(); len(reasons) > 0 {
		reason += "; blocked: " + strings.Join(reasons, "; ")
	}

	return placement.RebalanceDecisionResponse{
		RequestID: req.RequestID,
		Action:    orchestrationv1alpha1.RebalanceActionNoOp,
		Reason:    reason,
		DecidedBy: placement.DecidedByRules,
	}
}
