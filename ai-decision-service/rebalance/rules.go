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
	// evacuationImprovement clears the engine's improvement threshold: leaving a dead node matters most.
	evacuationImprovement = 100
)

// Assessment is one rebalance request with its node classes and gates worked out;
// the rules' answer and every model's answer are judged against it.
type Assessment struct {
	req     placement.DecisionRequest
	classes map[string]nodeClass
	gates   gates
}

// Assess works out the facts and gates for one rebalance request.
func Assess(req placement.DecisionRequest, now time.Time, cfg Config) Assessment {
	classes := classifyNodes(req, cfg)
	return Assessment{req: req, classes: classes, gates: applyGates(req, now, cfg, classes)}
}

// RulesAnswer is the first rule that matches, checked like any model's answer; it is every model's fallback.
func (a Assessment) RulesAnswer(scorerCfg scorer.Config) placement.RebalanceDecisionResponse {
	answer, matched := evacuateDownNode(a.req, a.classes, scorerCfg)
	if !matched {
		answer = noOp(a.req, a.gates)
	}
	if why := a.Check(answer); why != "" {
		return rejectedAnswer(a.req, why)
	}
	return answer
}

// Check says why an answer can't be sent, or "" when it can: the gates and limits, then the must-do
// that a pod on a down node moves first whenever a healthy node can take it.
func (a Assessment) Check(resp placement.RebalanceDecisionResponse) string {
	if why := validate(resp, a.req, a.gates, a.classes); why != "" {
		return why
	}
	stranded, found := firstPodOnDownNode(a.req, a.classes)
	if !found || !hasHealthyCandidate(a.req, a.classes) {
		return ""
	}
	if resp.Action == orchestrationv1alpha1.RebalanceActionMove {
		if pod, ok := findPod(a.req.RebalanceContext.CurrentPlacements, resp.PodName); ok &&
			a.classes[pod.NodeName] == classDown {
			return ""
		}
	}
	return fmt.Sprintf("pod %s is on down node %s and must move first", stranded.PodName, stranded.NodeName)
}

// Blocked lists what the gates block right now, in words, for a model's question or prompt.
func (a Assessment) Blocked() []string {
	return a.gates.blockedReasons()
}

// AnyChangeAllowed is false only in a cooldown with no pod on a down node, when no model could change anything.
func (a Assessment) AnyChangeAllowed() bool {
	_, stranded := firstPodOnDownNode(a.req, a.classes)
	return a.gates.cooldown == "" || stranded
}

// evacuateDownNode moves one pod off a dead node to the scorer's best healthy candidate.
func evacuateDownNode(req placement.DecisionRequest, classes map[string]nodeClass,
	scorerCfg scorer.Config) (placement.RebalanceDecisionResponse, bool) {
	// 1. The first pod on a dead node.
	pod, found := firstPodOnDownNode(req, classes)
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
	resp.Reason = fmt.Sprintf("node %s is down: move %s to %s (score %.0f)",
		pod.NodeName, pod.PodName, best.NodeName, best.Score)
	return resp, true
}

// noOp is the safe answer when no rule matched; it lists what the gates blocked.
func noOp(req placement.DecisionRequest, g gates) placement.RebalanceDecisionResponse {
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

// rejectedAnswer is the NoOp sent when an answer fails validate.
func rejectedAnswer(req placement.DecisionRequest, why string) placement.RebalanceDecisionResponse {
	return placement.RebalanceDecisionResponse{
		RequestID: req.RequestID,
		Action:    orchestrationv1alpha1.RebalanceActionNoOp,
		Reason:    "answer rejected: " + why,
		DecidedBy: placement.DecidedByFallback,
	}
}
