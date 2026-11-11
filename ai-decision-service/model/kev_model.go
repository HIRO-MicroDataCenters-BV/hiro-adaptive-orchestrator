package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	corev1 "k8s.io/api/core/v1"
)

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
	httpClient *http.Client
	log        *slog.Logger
}

var _ PlacementDecider = (*KevDecider)(nil) // the build fails if KevDecider stops matching the interface

// NewKevDecider returns a KevDecider whose calls stop after cfg.Timeout.
func NewKevDecider(cfg Config, log *slog.Logger) *KevDecider {
	return &KevDecider{cfg: cfg, httpClient: &http.Client{Timeout: cfg.Timeout}, log: log}
}

// DecidePlacement asks Kev where to place a pod based on the current node statistics and AO profile.
func (kd *KevDecider) DecidePlacement(ctx context.Context,
	req placement.DecisionRequest) (placement.DecisionResponse, error) {
	kevReq := buildKevRequest(req, kd.cfg.ModelName)
	if len(kevReq.Questions[kevQuestionID].Criteria) == 0 {
		return placement.DecisionResponse{},
			fmt.Errorf("(requestId=%s) no candidate nodes to ask Kev about", req.RequestID)
	}
	kd.log.Debug("Sent request to Kev", "requestId", req.RequestID,
		"state", kevReq.State, "criteria", kevReq.Questions[kevQuestionID].Criteria)

	body, err := json.Marshal(kevReq)
	if err != nil {
		return placement.DecisionResponse{},
			fmt.Errorf("failed to marshal Kev request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		kd.cfg.BaseURL+kd.cfg.Path, bytes.NewReader(body))
	if err != nil {
		return placement.DecisionResponse{},
			fmt.Errorf("(requestId=%s) failed to create request to Kev: %w", req.RequestID, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := kd.httpClient.Do(httpReq)
	if err != nil {
		return placement.DecisionResponse{},
			fmt.Errorf("(requestId=%s) failed to send request to Kev: %w",
				req.RequestID, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return placement.DecisionResponse{},
			fmt.Errorf("(requestId=%s) Kev responded with error status: %s",
				req.RequestID, resp.Status)
	}

	var kevResp kevResponse
	err = json.NewDecoder(resp.Body).Decode(&kevResp)
	if err != nil {
		return placement.DecisionResponse{},
			fmt.Errorf("(requestId=%s) failed to decode Kev response: %w",
				req.RequestID, err)
	}

	answer, found := kevResp.Answers[kevQuestionID]
	if !found {
		return placement.DecisionResponse{},
			fmt.Errorf("(requestId=%s) Kev response missing answer for question ID %s",
				req.RequestID, kevQuestionID)
	}
	kd.log.Debug("Received answer from Kev", "requestId", req.RequestID,
		"choice", answer.Choice, "confidence", answer.Confidence, "probabilities", answer.Probabilities,
		"latencyMilliSec", float64(time.Since(start).Microseconds())/1000)

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

// strategyRule spells out the strategy, as Kev picks poorly without it; default is Balanced, as in the scorer.
func strategyRule(strategy string) string {
	switch strategy {
	case string(orchestrationv1alpha1.PlacementStrategyPacked):
		return "Pick the node with the highest CPU and memory usage, whatever its pod count."
	case string(orchestrationv1alpha1.PlacementStrategySpread):
		return "Pick the node with the fewest pods of this app, even if it is busier."
	default:
		return "Pick the node with the lowest CPU and memory usage, whatever its pod count."
	}
}

// buildKevRequest constructs the request to be sent to Kev, including the current state, model, and questions.
func buildKevRequest(req placement.DecisionRequest, modelName string) kevRequest {
	criteria := make(map[string]string)

	strategy := scorer.Strategy(req.AOProfile)
	podSize := scorer.PodRequests(req.Pod)
	cpu := podSize[corev1.ResourceCPU.String()]
	memBytes := podSize[corev1.ResourceMemory.String()]
	mem := memBytes / (1024 * 1024)
	stateStr := "Placing a pod that requests " +
		fmt.Sprintf("%dm", cpu) + " CPU and " +
		fmt.Sprintf("%dMi", mem) + " memory. Strategy: " +
		strategy + "."
	instrStr := strategyRule(strategy)
	resources := scorer.ScoredResources(req.AOProfile)
	for _, nodeStat := range req.NodeStats {
		if !nodeStat.Candidate {
			continue // Kev may only choose among candidates
		}
		usage := "usage unknown"
		if util, ok := scorer.RequestedUtilization(nodeStat, nil, resources); ok {
			parts := make([]string, 0, len(resources))
			for _, resource := range resources {
				parts = append(parts, fmt.Sprintf("%s %.1f%% requested", resource, 100*util[resource]))
			}
			usage = strings.Join(parts, ", ")
		}
		pods := "unknown"
		if nodeStat.AppPods != nil {
			pods = strconv.Itoa(*nodeStat.AppPods)
		}
		criteria[nodeStat.Name] = fmt.Sprintf("%s, %s pods of this app", usage, pods)
	}

	return kevRequest{
		State: stateStr,
		Model: modelName,
		Questions: map[string]kevQuestion{
			kevQuestionID: {
				Type:         kevQuestionTypeChoice,
				Instructions: instrStr,
				Criteria:     criteria,
			},
		},
	}
}
