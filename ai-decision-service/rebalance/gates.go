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
	gates   []gate
	filled  map[string]bool // nodes a recent Move put a pod on
	emptied map[string]bool // nodes a recent Move took a pod off
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
		why := "cooldown until " + rc.CooldownUntil.UTC().Format(time.RFC3339)
		for _, gt := range g.gates {
			g.block(gt.action, why)
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

	// 4. EAO: no energy now, so a Move's replacement pod would sit Pending.
	if why := eaoBlocksMove(req.EAOProfile, now, cfg); why != "" {
		g.block(orchestrationv1alpha1.RebalanceActionMove, why)
	}

	// 5. Recent relocations, for moveBlocked's churn and ping-pong guards. NewPodNode is set
	//    only when a replacement pod really appeared, so a wrong-node Failed Move counts too.
	g.filled, g.emptied = map[string]bool{}, map[string]bool{}
	cutoff := now.Add(-cfg.FlipMoveActionWindow)
	for _, h := range rc.History {
		if h.Action != orchestrationv1alpha1.RebalanceActionMove || h.NewPodNode == "" || !h.At.After(cutoff) {
			continue
		}
		g.filled[h.NewPodNode] = true
		if h.SourceNode != "" {
			g.emptied[h.SourceNode] = true
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

// reasonFor returns why action a was blocked, or "" if it wasn't.
func (g *gateResult) reasonFor(a orchestrationv1alpha1.RebalanceAction) string {
	for _, gt := range g.gates {
		if gt.action == a && !gt.allowed {
			return gt.reason
		}
	}
	return ""
}

// moveBlocked says why moving a pod from source to target isn't allowed, or "" when it is.
// Leaving a dead node skips cooldown, the PDB pre-check and both guards; the eviction API still enforces the PDB.
func (g *gateResult) moveBlocked(source, target string, classes map[string]nodeClass) string {
	if classes[source] == classDown {
		return ""
	}
	if why := g.reasonFor(orchestrationv1alpha1.RebalanceActionMove); why != "" {
		return why
	}
	// Churn: don't take from a node we just filled, unless it's now at risk.
	if g.filled[source] && classes[source] != classAtRisk {
		return fmt.Sprintf("a recent move put a pod on %s", source)
	}
	// Ping-pong: don't send back to a node we just emptied.
	if g.emptied[target] {
		return fmt.Sprintf("a recent move took a pod off %s", target)
	}
	return ""
}

// eaoBlocksMove says why the EAO's fresh verdict rules out a Move now, or "" when it doesn't.
func eaoBlocksMove(eao *placement.EAOProfileContext, now time.Time, cfg Config) string {
	if eao == nil || eao.Verdict != placement.EAOVerdictHold {
		return ""
	}
	lastUpdated, err := time.Parse(time.RFC3339, eao.LastUpdated)
	if err != nil || now.Sub(lastUpdated) > cfg.EAOStaleAfter {
		return ""
	}
	if eao.HoldUntil != nil {
		return "EAO holds until " + eao.HoldUntil.UTC().Format(time.RFC3339)
	}
	return "EAO holds the workload for lack of energy"
}
