package rebalance

import (
	"fmt"
	"time"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// gate represents a single rebalance action and whether it is allowed, along with the reason if blocked.
type gate struct {
	action  orchestrationv1alpha1.RebalanceAction
	allowed bool
	reason  string
}

// gateResult represents a collection of rebalance actions and their corresponding gate results.
type gateResult struct {
	gates []gate
}

// block removes an action once, keeping the first reason; later gates don't repeat it.
func (g *gateResult) block(a orchestrationv1alpha1.RebalanceAction, why string) {
	for i, gt := range g.gates {
		if gt.action == a && gt.allowed {
			g.gates[i].allowed = false
			g.gates[i].reason = why
			break
		}
	}
}

// applyGates removes the actions that aren't safe or allowed right now.
func applyGates(req placement.DecisionRequest, now time.Time, cfg Config) gateResult {
	g := gateResult{
		gates: []gate{
			{
				action:  orchestrationv1alpha1.RebalanceActionMove,
				allowed: true,
			},
			{
				action:  orchestrationv1alpha1.RebalanceActionAdjustReplicas,
				allowed: true,
			},
			{
				action:  orchestrationv1alpha1.RebalanceActionAdjustResources,
				allowed: true,
			},
		},
	}

	rc := req.RebalanceContext

	// 1. Cooldown: nothing may change until it ends.
	if rc.CooldownUntil != nil && now.Before(rc.CooldownUntil.Time) {
		for _, gt := range g.gates {
			if gt.allowed {
				g.block(gt.action, fmt.Sprintf("cooldown in effect until %v", rc.CooldownUntil.Time))
			}
		}
	}

	// 2. Check if replicas are externally managed and block the corresponding action if so.
	if req.AOProfile != nil {
		if req.AOProfile.Rebalancing.ReplicasExternallyManaged {
			g.block(orchestrationv1alpha1.RebalanceActionAdjustReplicas,
				"replicas are externally managed")
		}
		if req.AOProfile.Rebalancing.ResourcesExternallyManaged {
			g.block(orchestrationv1alpha1.RebalanceActionAdjustResources,
				"resources are externally managed")
		}
	}

	// 3. PDB: a Move evicts a pod, which the budget doesn't allow right now.
	if rc.PDB != nil && rc.PDB.AllowedDisruptions == 0 {
		g.block(orchestrationv1alpha1.RebalanceActionMove,
			fmt.Sprintf("PDB %s does not allow disruptions", rc.PDB.Name))
	}

	// 4. Flip-flop: don't move off a node a recent move just put a pod on.
	//    Skipped for NodeFailure: a dead node must be left even if we just moved there.
	if rc.Trigger != nil && rc.Trigger.Node != "" &&
		rc.Trigger.Type != string(orchestrationv1alpha1.TriggerConditionNodeFailure) {
		for _, h := range rc.History {
			if h.Action == orchestrationv1alpha1.RebalanceActionMove &&
				h.Outcome == orchestrationv1alpha1.RebalanceOutcomeEnacted &&
				h.NewPodNode == rc.Trigger.Node &&
				h.At.Time.After(now.Add(-cfg.FlipMoveActionWindow)) {
				g.block(orchestrationv1alpha1.RebalanceActionMove,
					fmt.Sprintf("recent move placed pod %s on node %s", h.NewPodName, rc.Trigger.Node))
				break
			}
		}
	}

	return g
}

// isAllowed reports whether action a passed every gate.
func (g *gateResult) isAllowed(a orchestrationv1alpha1.RebalanceAction) bool {
	if a == orchestrationv1alpha1.RebalanceActionNoOp ||
		a == orchestrationv1alpha1.RebalanceActionDefer ||
		a == orchestrationv1alpha1.RebalanceActionEscalate ||
		a == orchestrationv1alpha1.RebalanceActionReject {
		return true
	}
	for _, gt := range g.gates {
		if gt.action == a {
			return gt.allowed
		}
	}
	return false
}

// blockedReasons lists "<action>: <reason>" for each blocked action, for the NoOp reason.
func (g *gateResult) blockedReasons() []string {
	var reasons []string
	for _, gt := range g.gates {
		if !gt.allowed {
			reasons = append(reasons, fmt.Sprintf("%s: %s", gt.action, gt.reason))
		}
	}
	return reasons
}
