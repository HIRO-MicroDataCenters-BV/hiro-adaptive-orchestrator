package scorer

import (
	"fmt"
	"math"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	corev1 "k8s.io/api/core/v1"
)

// Strategy returns the placement strategy from the profile, defaulting to Balanced.
func Strategy(profile *placement.AOProfileContext) string {
	strategy := string(orchestrationv1alpha1.PlacementStrategyBalanced)
	if profile != nil && profile.Strategy != "" {
		strategy = profile.Strategy
	}
	return strategy
}

// PodRequests sums the pod's container requests: CPU in millicores, memory in bytes.
func PodRequests(pod *corev1.Pod) map[string]int64 {
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

// ScoredResources picks the resources the profile cares about; none flagged means CPU and memory.
func ScoredResources(profile *placement.AOProfileContext) []string {
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

// Average returns the mean utilisation among the scored resources.
func Average(util map[string]float64) float64 {
	var sum float64
	for _, u := range util {
		sum += u
	}
	if len(util) == 0 {
		return 0
	}
	return sum / float64(len(util))
}

// Utilization is (requested + pod) / allocatable per resource; ok is false when any of it is unknown.
func Utilization(node placement.NodeStat,
	pod map[string]int64,
	resources []string) (util map[string]float64, ok bool) {
	util = make(map[string]float64)
	for _, r := range resources {
		nr, found := node.Resources[r]
		if !found || nr.Requested == nil || nr.Allocatable <= 0 {
			return nil, false
		}
		base := *nr.Requested
		if nr.Used != nil {
			base = max(base, *nr.Used) // a node busy by usage must not look empty
		}
		util[r] = float64(base+pod[r]) / float64(nr.Allocatable)
	}
	return util, true
}

// MaxUtil returns the highest utilisation among the scored resources.
func MaxUtil(util map[string]float64) float64 {
	var highest float64
	for _, u := range util {
		if u > highest {
			highest = u
		}
	}
	return highest
}

// Allowed reports whether a node may take the pod, for any decider: the pod must fit (util includes it),
// and under Packed the node must stay within PackedMaxUtil, as Packed fills nodes and needs headroom.
func Allowed(util map[string]float64, strategy string, cfg Config) bool {
	limit := 1.0
	if strategy == string(orchestrationv1alpha1.PlacementStrategyPacked) {
		limit = cfg.PackedMaxUtil
	}
	return MaxUtil(util) <= limit
}

// packedScore prefers fuller nodes; Score has already dropped nodes over the packed limit.
func packedScore(util map[string]float64) float64 {
	return 100 * Average(util)
}

// balancedScore prefers emptier nodes, minus a penalty when CPU and memory are uneven;
// Score has already dropped nodes the pod does not fit on.
func balancedScore(util map[string]float64, cfg Config) float64 {
	score := 100 * (1 - MaxUtil(util))
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

// SpreadCounts is how many of the app's pods each zone has, and the highest zone and node counts.
type SpreadCounts struct {
	ZoneCounts       map[string]int
	HighestZoneCount int
	HighestNodeCount int
}

// CountAppPodsPerZone counts the app's pods per zone;
// nodes with unknown appPods or no zone label are skipped for zones.
func CountAppPodsPerZone(nodes []placement.NodeStat, cfg Config) SpreadCounts {
	counts := SpreadCounts{
		ZoneCounts: make(map[string]int),
	}
	for _, node := range nodes {
		appPods := node.AppPods
		if appPods == nil {
			continue
		}
		counts.HighestNodeCount = max(counts.HighestNodeCount, *appPods)
		zone := node.Labels[cfg.ZoneLabel]
		if zone != "" {
			counts.ZoneCounts[zone] += *appPods
			counts.HighestZoneCount = max(counts.HighestZoneCount, counts.ZoneCounts[zone])
		}
	}
	return counts
}

// spreadScore prefers nodes and zones with fewer of the app's pods, with Balanced as a tie-break;
// Score has already dropped nodes the pod does not fit on.
func spreadScore(node placement.NodeStat, util map[string]float64, counts SpreadCounts, cfg Config) float64 {
	var nodePart float64
	if node.AppPods == nil {
		nodePart = cfg.UnknownScore
	} else if counts.HighestNodeCount == 0 {
		nodePart = 100
	} else {
		nodePart = 100 * (1 - float64(*node.AppPods)/float64(counts.HighestNodeCount))
	}

	var zonePart float64
	zone := node.Labels[cfg.ZoneLabel]
	if zone == "" {
		zonePart = nodePart
	} else if counts.HighestZoneCount == 0 {
		zonePart = 100
	} else {
		zonePart = 100 * (1 - float64(counts.ZoneCounts[zone])/float64(counts.HighestZoneCount))
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
	strategy := Strategy(req.AOProfile)
	pod := PodRequests(req.Pod)
	resources := ScoredResources(req.AOProfile)
	counts := CountAppPodsPerZone(req.NodeStats, cfg)

	nodeScores := make([]placement.NodeScore, 0, len(req.NodeStats))
	var bestNodeScore float64
	var bestNodeName string
	for _, nodeStat := range req.NodeStats {
		if !nodeStat.Candidate {
			continue // not a target; rebalance requests list it only so the decider sees every pod's node
		}
		var score float64
		util, ok := Utilization(nodeStat, pod, resources)
		if !ok {
			score = cfg.UnknownScore
		} else if !Allowed(util, strategy, cfg) {
			score = 0
		} else {
			switch strategy {
			case string(orchestrationv1alpha1.PlacementStrategyPacked):
				score = packedScore(util)
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
