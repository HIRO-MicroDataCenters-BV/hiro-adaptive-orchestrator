package rebalance

import "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"

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
	if ready != "True" {
		return classDown
	}
	if n.Unschedulable {
		return classAtRisk
	}
	for _, c := range cfg.RiskConditions {
		if n.Conditions[c] == "True" {
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
