package rebalance

import (
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	corev1 "k8s.io/api/core/v1"
)

const conditionTrue = string(corev1.ConditionTrue)

// nodeClass is how healthy a node is, judged only from facts in the request.
type nodeClass int

const (
	classUnknown nodeClass = iota // no data (e.g. the node is gone): no shortcuts
	classHealthy
	classAtRisk // Ready, but under pressure or cordoned
	classDown   // Ready is False or Unknown
)

// classifyNode sorts one node from its Ready condition, risk conditions and cordon.
func classifyNode(n placement.NodeStat, cfg Config) nodeClass {
	ready, ok := n.Conditions["Ready"]
	if !ok {
		return classUnknown
	}
	if ready != conditionTrue {
		return classDown
	}
	if n.Unschedulable {
		return classAtRisk
	}
	for _, c := range cfg.RiskConditions {
		if n.Conditions[c] == conditionTrue {
			return classAtRisk
		}
	}
	return classHealthy
}

// classifyNodes maps every node in the request to its class; a node not in the map is classUnknown.
func classifyNodes(req placement.DecisionRequest, cfg Config) map[string]nodeClass {
	classes := make(map[string]nodeClass, len(req.NodeStats))
	for _, n := range req.NodeStats {
		classes[n.Name] = classifyNode(n, cfg)
	}
	return classes
}

// firstPodOnDownNode returns the first of the app's pods on a down node, if any.
func firstPodOnDownNode(req placement.DecisionRequest, classes map[string]nodeClass) (placement.PodPlacement, bool) {
	for _, p := range req.RebalanceContext.CurrentPlacements {
		if classes[p.NodeName] == classDown {
			return p, true
		}
	}
	return placement.PodPlacement{}, false
}

// hasHealthyCandidate reports whether any candidate node is healthy, so a pod on a down node has somewhere to go.
func hasHealthyCandidate(req placement.DecisionRequest, classes map[string]nodeClass) bool {
	for _, n := range req.NodeStats {
		if n.Candidate && classes[n.Name] == classHealthy {
			return true
		}
	}
	return false
}
