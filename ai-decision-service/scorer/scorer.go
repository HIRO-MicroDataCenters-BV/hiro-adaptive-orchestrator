package scorer

import (
	"fmt"
	"math"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	corev1 "k8s.io/api/core/v1"
)

// average returns the mean utilisation among the scored resources.
func average(util map[string]float64) float64 {
	var sum float64
	for _, u := range util {
		sum += u
	}
	if len(util) == 0 {
		return 0
	}
	return sum / float64(len(util))
}

// podRequests sums the pod's container requests: CPU in millicores, memory in bytes.
func podRequests(pod *corev1.Pod) map[string]int64 {
	resources := make(map[string]int64)
	if pod == nil {
		return resources
	}
	for _, container := range pod.Spec.Containers {
		resources[corev1.ResourceCPU.String()] += container.Resources.Requests.Cpu().MilliValue()
		resources[corev1.ResourceMemory.String()] += container.Resources.Requests.Memory().Value()
	}
	return resources
}

// scoredResources picks the resources the profile cares about; none flagged means CPU and memory.
func scoredResources(profile *placement.AOProfileContext) []string {
	var resources []string
	if profile != nil && profile.Awareness.CPU {
		resources = append(resources, corev1.ResourceCPU.String())
	}
	if profile != nil && profile.Awareness.Memory {
		resources = append(resources, corev1.ResourceMemory.String())
	}
	if len(resources) == 0 {
		return []string{
			corev1.ResourceCPU.String(),
			corev1.ResourceMemory.String(),
		}
	}
	return resources
}

// utilAfter is (requested + pod) / allocatable per resource; ok is false when any of it is unknown.
func utilAfter(node placement.NodeStat, pod map[string]int64, resources []string) (util map[string]float64, ok bool) {
	util = make(map[string]float64)
	for _, r := range resources {
		nr, found := node.Resources[r]
		if !found || nr.Requested == nil || nr.Allocatable <= 0 {
			return nil, false
		}
		util[r] = float64(*nr.Requested+pod[r]) / float64(nr.Allocatable)
	}
	return util, true
}

// maxUtil returns the highest utilisation among the scored resources.
func maxUtil(util map[string]float64) float64 {
	var highest float64
	for _, u := range util {
		if u > highest {
			highest = u
		}
	}
	return highest
}

// packedScore prefers fuller nodes, but scores 0 above the packed limit.
func packedScore(util map[string]float64, cfg Config) float64 {
	highest := maxUtil(util)
	if highest > cfg.PackedMaxUtil {
		return 0
	}
	return 100 * average(util)
}

// balancedScore prefers emptier nodes, minus a penalty when CPU and memory are uneven.
func balancedScore(util map[string]float64, cfg Config) float64 {
	highest := maxUtil(util)
	if highest > 1 {
		return 0
	}
	score := 100 * (1 - highest)
	cpu, cpuFound := util[corev1.ResourceCPU.String()]
	mem, memFound := util[corev1.ResourceMemory.String()]
	if cpuFound && memFound {
		penalty := math.Abs(cpu - mem)
		score -= 100 * cfg.ImbalancePenalty * penalty
	}
	if score < 0 {
		score = 0
	}
	return score
}

// spreadCounts is how many of the app's pods each zone has, and the highest zone and node counts.
type spreadCounts struct {
	zoneCounts       map[string]int
	highestZoneCount int
	highestNodeCount int
}

// countAppPods counts the app's pods per zone; nodes with unknown appPods or no zone label are skipped for zones.
func countAppPods(nodes []placement.NodeStat, cfg Config) spreadCounts {
	counts := spreadCounts{
		zoneCounts: make(map[string]int),
	}
	for _, node := range nodes {
		appPods := node.AppPods
		if appPods == nil {
			continue
		}
		counts.highestNodeCount = max(counts.highestNodeCount, *appPods)
		zone := node.Labels[cfg.ZoneLabel]
		if zone != "" {
			counts.zoneCounts[zone] += *appPods
			counts.highestZoneCount = max(counts.highestZoneCount, counts.zoneCounts[zone])
		}
	}
	return counts
}

// spreadScore prefers nodes and zones with fewer of the app's pods, with Balanced as a tie-break.
func spreadScore(node placement.NodeStat, util map[string]float64, counts spreadCounts, cfg Config) float64 {
	if maxUtil(util) > 1 {
		return 0
	}

	var nodePart float64
	if node.AppPods == nil {
		nodePart = cfg.UnknownScore
	} else if counts.highestNodeCount == 0 {
		nodePart = 100
	} else {
		nodePart = 100 * (1 - float64(*node.AppPods)/float64(counts.highestNodeCount))
	}

	var zonePart float64
	zone := node.Labels[cfg.ZoneLabel]
	if zone == "" {
		zonePart = nodePart
	} else if counts.highestZoneCount == 0 {
		zonePart = 100
	} else {
		zonePart = 100 * (1 - float64(counts.zoneCounts[zone])/float64(counts.highestZoneCount))
	}

	zoneWeight := cfg.SpreadZoneWeight
	nodeWeight := cfg.SpreadNodeWeight
	balancedWeight := cfg.SpreadBalancedWeight
	score := (zoneWeight*zonePart + nodeWeight*nodePart + balancedWeight*balancedScore(util, cfg)) /
		(zoneWeight + nodeWeight + balancedWeight)
	return score
}

// Score rates every candidate node 0–100 for the request's strategy; the reason names the best node.
func Score(req placement.DecisionRequest, cfg Config) ([]placement.NodeScore, string) {
	strategy := string(orchestrationv1alpha1.PlacementStrategyBalanced)
	if req.AOProfile != nil && req.AOProfile.Strategy != "" {
		strategy = req.AOProfile.Strategy
	}
	pod := podRequests(req.Pod)
	resources := scoredResources(req.AOProfile)
	counts := countAppPods(req.NodeStats, cfg)

	nodeScores := make([]placement.NodeScore, 0, len(req.NodeStats))
	var bestNodeScore float64
	var bestNodeName string
	for _, nodeStat := range req.NodeStats {
		var score float64
		util, ok := utilAfter(nodeStat, pod, resources)
		if !ok {
			score = cfg.UnknownScore
		} else {
			switch strategy {
			case string(orchestrationv1alpha1.PlacementStrategyPacked):
				score = packedScore(util, cfg)
			case string(orchestrationv1alpha1.PlacementStrategySpread):
				score = spreadScore(nodeStat, util, counts, cfg)
			default:
				score = balancedScore(util, cfg)
			}
		}
		nodeScores = append(nodeScores, placement.NodeScore{NodeName: nodeStat.Name, Score: score})
		if bestNodeName == "" || score > bestNodeScore {
			bestNodeScore = score
			bestNodeName = nodeStat.Name
		}
	}
	if bestNodeName == "" {
		return nodeScores, fmt.Sprintf("%s: no candidate nodes", strategy)
	}
	reason := fmt.Sprintf("%s: %s best (score %.0f)", strategy, bestNodeName, bestNodeScore)

	return nodeScores, reason
}
