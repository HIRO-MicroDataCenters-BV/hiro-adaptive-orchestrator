package rebalance

import (
	"fmt"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	"k8s.io/apimachinery/pkg/api/resource"
)

// validate says why an answer breaks a limit, or "" when it can be sent; it checks every answer.
func validate(resp placement.RebalanceDecisionResponse, req placement.DecisionRequest,
	g gateResult, classes map[string]nodeClass) string {
	rc := req.RebalanceContext
	switch resp.Action {
	case orchestrationv1alpha1.RebalanceActionNoOp,
		orchestrationv1alpha1.RebalanceActionDefer,
		orchestrationv1alpha1.RebalanceActionReject,
		orchestrationv1alpha1.RebalanceActionEscalate:
		return ""
	case orchestrationv1alpha1.RebalanceActionMove:
		return validateMove(resp, req, g, classes)
	case orchestrationv1alpha1.RebalanceActionAdjustReplicas:
		return validateReplicas(resp, rc.Workload, g)
	case orchestrationv1alpha1.RebalanceActionAdjustResources:
		return validateResources(resp, rc.CurrentPlacements, g)
	}
	return fmt.Sprintf("unknown action %q", resp.Action)
}

// validateMove checks the pod, the target and the move guards.
func validateMove(resp placement.RebalanceDecisionResponse, req placement.DecisionRequest,
	g gateResult, classes map[string]nodeClass) string {
	pod, ok := findPod(req.RebalanceContext.CurrentPlacements, resp.PodName)
	if !ok {
		return fmt.Sprintf("pod %q is not in the request", resp.PodName)
	}
	if pod.NodeName == "" {
		return fmt.Sprintf("pod %q is not scheduled", resp.PodName)
	}
	if resp.TargetNode == pod.NodeName {
		return fmt.Sprintf("pod %q is already on %s", resp.PodName, pod.NodeName)
	}
	if node, ok := findNode(req.NodeStats, resp.TargetNode); !ok || !node.Candidate {
		return fmt.Sprintf("target %q is not a candidate", resp.TargetNode)
	}
	if classes[resp.TargetNode] != classHealthy {
		return fmt.Sprintf("target %q is not healthy", resp.TargetNode)
	}
	return g.moveBlocked(pod.NodeName, resp.TargetNode, classes)
}

// validateReplicas checks the gate and that the target is a real change within the bounds.
func validateReplicas(resp placement.RebalanceDecisionResponse, w *placement.RebalanceWorkload, g gateResult) string {
	if a := orchestrationv1alpha1.RebalanceActionAdjustReplicas; !g.isAllowed(a) {
		return g.reasonFor(a)
	}
	if w == nil {
		return "no workload in the request"
	}
	if resp.TargetReplicas < w.MinReplicas || resp.TargetReplicas > w.MaxReplicas {
		return fmt.Sprintf("%d replicas is outside %d..%d", resp.TargetReplicas, w.MinReplicas, w.MaxReplicas)
	}
	if resp.TargetReplicas == w.Replicas {
		return fmt.Sprintf("already at %d replicas", w.Replicas)
	}
	return ""
}

// validateResources checks the gate, the container and that each target is a positive quantity.
func validateResources(resp placement.RebalanceDecisionResponse, pods []placement.PodPlacement, g gateResult) string {
	if a := orchestrationv1alpha1.RebalanceActionAdjustResources; !g.isAllowed(a) {
		return g.reasonFor(a)
	}
	if !hasContainer(pods, resp.ContainerName) {
		return fmt.Sprintf("container %q is not in the request", resp.ContainerName)
	}
	if resp.TargetCPU == "" && resp.TargetMemory == "" {
		return "no CPU or memory target"
	}
	for _, t := range [...]struct{ name, value string }{{"cpu", resp.TargetCPU}, {"memory", resp.TargetMemory}} {
		if t.value == "" {
			continue
		}
		if q, err := resource.ParseQuantity(t.value); err != nil || q.Sign() <= 0 {
			return fmt.Sprintf("%s %q is not a positive quantity", t.name, t.value)
		}
	}
	return ""
}

// findPod returns the pod with this name from the request's placements.
func findPod(pods []placement.PodPlacement, name string) (placement.PodPlacement, bool) {
	for _, p := range pods {
		if p.PodName == name {
			return p, true
		}
	}
	return placement.PodPlacement{}, false
}

// findNode returns the node with this name from the request's node stats.
func findNode(nodes []placement.NodeStat, name string) (placement.NodeStat, bool) {
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return placement.NodeStat{}, false
}

// hasContainer reports whether any pod in the request has a container with this name.
func hasContainer(pods []placement.PodPlacement, name string) bool {
	for _, p := range pods {
		for _, c := range p.Containers {
			if c.Name == name {
				return true
			}
		}
	}
	return false
}
