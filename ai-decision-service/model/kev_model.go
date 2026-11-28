package model

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	corev1 "k8s.io/api/core/v1"
)

// ProviderKev selects Kev's System One API.
const ProviderKev = "kev"

const (
	kevQuestionID         = "placement"
	kevQuestionTypeChoice = "choice"
	// kevQuestionTypeNoul   = "noul"
	// kevQuestionTypeScore  = "score"
)

// kevRequest represents the request sent to Kev, including the current state, model, and questions.
type kevRequest struct {
	State     string                 `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]kevQuestion `json:"questions"`
}

// kevQuestion represents a single question sent to Kev, including its type, instructions, and criteria.
type kevQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// kevResponse is Kev's reply; only the fields we use.
type kevResponse struct {
	Answers map[string]kevAnswer `json:"answers"`
}

// kevAnswer is Kev's answer to one choice question.
type kevAnswer struct {
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// KevDecider asks Kev, through its System One API, where to place a pod.
type KevDecider struct {
	cfg        Config
	scorerCfg  scorer.Config
	httpClient *http.Client
	log        *slog.Logger
}

var _ PlacementDecider = (*KevDecider)(nil) // the build fails if KevDecider stops matching the interface

// NewKevDecider returns a KevDecider whose calls stop after cfg.Timeout.
func NewKevDecider(cfg Config, scorerCfg scorer.Config, log *slog.Logger) *KevDecider {
	return &KevDecider{cfg: cfg, scorerCfg: scorerCfg, httpClient: &http.Client{Timeout: cfg.Timeout}, log: log}
}

// DecidePlacement asks Kev where to place a pod based on the current node statistics and AO profile.
func (kd *KevDecider) DecidePlacement(ctx context.Context,
	req placement.DecisionRequest) (placement.DecisionResponse, error) {
	kevReq := buildKevRequest(req, kd.cfg.ModelName, kd.scorerCfg)
	if len(kevReq.Questions[kevQuestionID].Criteria) == 0 {
		return placement.DecisionResponse{},
			fmt.Errorf("(requestId=%s) no candidate nodes to ask Kev about", req.RequestID)
	}
	kd.log.Debug("Sent request to Kev", "requestId", req.RequestID,
		"kevRequest", kevReq)

	var kevResp kevResponse
	err := postJSON(ctx, kd.httpClient, kd.cfg.BaseURL+kd.cfg.Path, kd.cfg.APIKey, kevReq, &kevResp)
	if err != nil {
		return placement.DecisionResponse{}, fmt.Errorf("(requestId=%s) Kev call failed: %w", req.RequestID, err)
	}

	answer, found := kevResp.Answers[kevQuestionID]
	if !found {
		return placement.DecisionResponse{},
			fmt.Errorf("(requestId=%s) Kev response missing answer for question ID %s",
				req.RequestID, kevQuestionID)
	}
	kd.log.Debug("Received answer from Kev", "requestId", req.RequestID,
		"kevResponse", answer)

	nodeScores := make([]placement.NodeScore, 0, len(req.NodeStats))
	for _, nodeStat := range req.NodeStats {
		if !nodeStat.Candidate {
			continue
		}
		probability, found := answer.Probabilities[nodeStat.Name]
		if !found {
			return placement.DecisionResponse{},
				fmt.Errorf("(requestId=%s) no probability from Kev for node %s",
					req.RequestID, nodeStat.Name)
		}
		nodeScores = append(nodeScores,
			placement.NodeScore{NodeName: nodeStat.Name, Score: 100 * probability})
	}

	placementResp := placement.DecisionResponse{
		NodeScores: nodeScores,
		Reason: fmt.Sprintf("%s: %s best (probability %.2f)",
			scorer.Strategy(req.AOProfile),
			answer.Choice,
			answer.Probabilities[answer.Choice]),

		Confidence: answer.Confidence,
	}
	return placementResp, nil
}

// strategyWording gives the strategy's purpose (for the state) and the one positive rule Kev ranks nodes by;
// negated rules made Kev pick worse. Default is Balanced, as in the scorer.
func strategyWording(strategy string, packedMaxUtil float64) (purpose, priority string) {
	switch strategy {
	case string(orchestrationv1alpha1.PlacementStrategyPacked):
		return fmt.Sprintf("Strategy: Packed. To save energy, fill nodes that are already in use so other nodes "+
				"can stay empty, but no node may go over %.0f%% on any resource.", 100*packedMaxUtil),
			"Under Packed, the fuller node is the better place."
	case string(orchestrationv1alpha1.PlacementStrategySpread):
		return "Strategy: Spread. For high availability, keep this app's pods apart, so that losing one node, " +
				"or one zone, takes down as few of them as possible.",
			"Under Spread, fewer of this app's pods in the node's zone is better; if that is the same, " +
				"fewer of this app's pods on the node is better; only if both are the same, the less busy node is better."
	default:
		return "Strategy: Balanced. The app is latency sensitive, so no node should become a hotspot on any resource.",
			"Under Balanced, the node with lower CPU and memory use is the better place."
	}
}

// nodeFacts describes one node with the pod placed, leading with what the strategy ranks by.
func nodeFacts(strategy string, util map[string]float64, resources []string,
	appPods *int, zone string, zonePods int) string {
	levels := make([]string, 0, len(resources))
	busiest := resources[0]
	for i, resource := range resources {
		verb := "at"
		if i == 0 {
			verb = "would be at"
		}
		levels = append(levels, fmt.Sprintf("%s %s %.0f%%", resource, verb, 100*util[resource]))
		if util[resource] > util[busiest] {
			busiest = resource
		}
	}
	usage := strings.Join(levels, " and ") // "cpu would be at 60% and memory at 58%"

	switch strategy {
	case string(orchestrationv1alpha1.PlacementStrategyPacked):
		return fmt.Sprintf("with this pod placed, it would be %.0f%% full on average; %s", 100*scorer.Average(util), usage)
	case string(orchestrationv1alpha1.PlacementStrategySpread):
		nodePods := "an unknown number of pods"
		if appPods != nil {
			nodePods = podCount(*appPods)
		}
		where := fmt.Sprintf("this app already runs %s on this node", nodePods)
		if zone != "" {
			where = fmt.Sprintf("this app already runs %s in this node's zone, %s, and %s on this node",
				podCount(zonePods), zone, nodePods)
		}
		return fmt.Sprintf("%s; with this pod placed, its busiest resource, %s, would be at %.0f%%; %s",
			where, busiest, 100*util[busiest], usage)
	default:
		return "with this pod placed, " + usage
	}
}

// podCount reads "1 pod" or "3 pods".
func podCount(n int) string {
	if n == 1 {
		return "1 pod"
	}
	return fmt.Sprintf("%d pods", n)
}

// buildKevRequest asks Kev one choice question over the candidate nodes, each described with the pod placed.
func buildKevRequest(req placement.DecisionRequest, modelName string, scorerCfg scorer.Config) kevRequest {
	strategy := scorer.Strategy(req.AOProfile)
	purpose, priority := strategyWording(strategy, scorerCfg.PackedMaxUtil)
	podSize := scorer.PodRequests(req.Pod)
	state := fmt.Sprintf("Choosing a node for a new pod of this app, which requests %dm CPU and %dMi memory. %s",
		podSize[corev1.ResourceCPU.String()], podSize[corev1.ResourceMemory.String()]/(1024*1024), purpose)

	resources := scorer.ScoredResources(req.AOProfile)
	zonePods := scorer.CountAppPodsPerZone(req.NodeStats, scorerCfg).ZoneCounts // all nodes, so non-candidates count too
	criteria := make(map[string]string)
	for _, nodeStat := range req.NodeStats {
		if !nodeStat.Candidate {
			continue // Kev may only choose among candidates
		}
		util, ok := scorer.Utilization(nodeStat, podSize, resources)
		if !ok {
			criteria[nodeStat.Name] = "usage unknown"
			continue
		}
		zone := nodeStat.Labels[scorerCfg.ZoneLabel]
		criteria[nodeStat.Name] = nodeFacts(strategy, util, resources, nodeStat.AppPods, zone, zonePods[zone])
	}

	return kevRequest{
		State: state,
		Model: modelName,
		Questions: map[string]kevQuestion{
			kevQuestionID: {
				Type:         kevQuestionTypeChoice,
				Instructions: "Which node is the best place for this pod? " + priority,
				Criteria:     criteria,
			},
		},
	}
}
