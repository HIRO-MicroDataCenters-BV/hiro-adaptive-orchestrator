package rebalance

import (
	"fmt"
	"maps"
	"slices"
	"time"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// change is what an answer would do, so the gates can judge it.
type change struct {
	action         orchestrationv1alpha1.RebalanceAction
	addsPods       bool   // a move's replacement, a resize rollout or a scale-up
	source, target string // the nodes a moved pod leaves and lands on
}

// gates says what is blocked right now and why; "" or a missing key means allowed.
type gates struct {
	cooldown       string                                           // no change at all
	blockedActions map[orchestrationv1alpha1.RebalanceAction]string // managed elsewhere, or the PDB
	newPodsBlocked string                                           // the EAO holds new pods
	blockedSources map[string]string                                // a recent move filled this node
	blockedTargets map[string]string                                // a recent move emptied this node
}

// applyGates works out what isn't safe or allowed right now; classes exempts at-risk nodes from the churn guard.
func applyGates(req placement.DecisionRequest, now time.Time, cfg Config, classes map[string]nodeClass) gates {
	g := gates{
		blockedActions: map[orchestrationv1alpha1.RebalanceAction]string{},
		blockedSources: map[string]string{},
		blockedTargets: map[string]string{},
	}
	rc := req.RebalanceContext

	// 1. Cooldown: nothing may change until it ends.
	if rc.CooldownUntil != nil && now.Before(rc.CooldownUntil.Time) {
		g.cooldown = "cooldown until " + rc.CooldownUntil.UTC().Format(time.RFC3339)
	}

	// 2. Replicas or resources another controller manages.
	if req.AOProfile != nil {
		if req.AOProfile.Rebalancing.ReplicasExternallyManaged {
			g.blockedActions[orchestrationv1alpha1.RebalanceActionAdjustReplicas] = "replicas are externally managed"
		}
		if req.AOProfile.Rebalancing.ResourcesExternallyManaged {
			g.blockedActions[orchestrationv1alpha1.RebalanceActionAdjustResources] = "resources are externally managed"
		}
	}

	// 3. PDB: a Move evicts a pod, which the budget doesn't allow right now.
	if rc.PDB != nil && rc.PDB.AllowedDisruptions == 0 {
		g.blockedActions[orchestrationv1alpha1.RebalanceActionMove] =
			fmt.Sprintf("PDB %s does not allow disruptions", rc.PDB.Name)
	}

	// 4. EAO: no energy now, so any new pod would sit Pending.
	g.newPodsBlocked = eaoHoldReason(req.EAOProfile, now, cfg)

	// 5. Recent relocations. NewPodNode is set only when a replacement pod really appeared,
	//    so a wrong-node Failed Move counts too.
	cutoff := now.Add(-cfg.FlipMoveActionWindow)
	for _, h := range rc.History {
		if h.Action != orchestrationv1alpha1.RebalanceActionMove || h.NewPodNode == "" || !h.At.After(cutoff) {
			continue
		}
		// Churn: don't take from a node we just filled, unless it's now at risk.
		if classes[h.NewPodNode] != classAtRisk {
			g.blockedSources[h.NewPodNode] = fmt.Sprintf("a recent move put a pod on %s", h.NewPodNode)
		}
		// Ping-pong: don't send back to a node we just emptied.
		if h.SourceNode != "" {
			g.blockedTargets[h.SourceNode] = fmt.Sprintf("a recent move took a pod off %s", h.SourceNode)
		}
	}

	return g
}

// blocks says why c isn't allowed now, or "" when it is; the first reason wins.
func (g gates) blocks(c change) string {
	if isNoChange(c.action) {
		return ""
	}
	if g.cooldown != "" {
		return g.cooldown
	}
	if why := g.blockedActions[c.action]; why != "" {
		return why
	}
	if c.addsPods && g.newPodsBlocked != "" {
		return g.newPodsBlocked
	}
	if why := g.blockedSources[c.source]; why != "" {
		return why
	}
	return g.blockedTargets[c.target]
}

// blockedReasons lists what is blocked right now, for the NoOp reason.
func (g gates) blockedReasons() []string {
	var reasons []string
	if g.cooldown != "" {
		reasons = append(reasons, g.cooldown)
	}
	for _, a := range slices.Sorted(maps.Keys(g.blockedActions)) {
		reasons = append(reasons, fmt.Sprintf("%s: %s", a, g.blockedActions[a]))
	}
	if g.newPodsBlocked != "" {
		reasons = append(reasons, "new pods: "+g.newPodsBlocked)
	}
	return reasons
}

// changeOf describes what response would do; an action it doesn't know is assumed to add pods.
func changeOf(resp placement.RebalanceDecisionResponse, req placement.DecisionRequest) change {
	c := change{action: resp.Action}
	switch resp.Action {
	case orchestrationv1alpha1.RebalanceActionMove:
		pod, _ := findPod(req.RebalanceContext.CurrentPlacements, resp.PodName)
		c.addsPods, c.source, c.target = true, pod.NodeName, resp.TargetNode
	case orchestrationv1alpha1.RebalanceActionAdjustReplicas:
		w := req.RebalanceContext.Workload
		c.addsPods = w != nil && resp.TargetReplicas > w.Replicas
	default:
		c.addsPods = !isNoChange(resp.Action) // AdjustResources rolls out new pods
	}
	return c
}

// isNoChange reports whether action a leaves the workload as it is.
func isNoChange(a orchestrationv1alpha1.RebalanceAction) bool {
	switch a {
	case orchestrationv1alpha1.RebalanceActionNoOp,
		orchestrationv1alpha1.RebalanceActionDefer,
		orchestrationv1alpha1.RebalanceActionReject,
		orchestrationv1alpha1.RebalanceActionEscalate:
		return true
	}
	return false
}

// eaoHoldReason says why the EAO's fresh verdict holds new pods now, or "" when it doesn't.
func eaoHoldReason(eao *placement.EAOProfileContext, now time.Time, cfg Config) string {
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
