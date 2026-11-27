package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/model"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/rebalance"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// kube-scheduler's node score range
const (
	MinScore = 0
	MaxScore = 100
)

type AIDecisionService struct {
	cfg              Config
	placementDecider model.PlacementDecider // nil when the model is off, so the scorer answers.
	rebalanceDecider model.RebalanceDecider // nil when the model is off, so the rules answer.
	log              *slog.Logger
}

func New(cfg Config, placementDecider model.PlacementDecider, rebalanceDecider model.RebalanceDecider,
	log *slog.Logger) *AIDecisionService {
	return &AIDecisionService{
		cfg:              cfg,
		placementDecider: placementDecider,
		rebalanceDecider: rebalanceDecider,
		log:              log,
	}
}

func (s *AIDecisionService) Start() error {
	srv := &http.Server{
		Addr:        fmt.Sprintf(":%d", s.cfg.Port),
		Handler:     s.Handler(),
		ReadTimeout: s.cfg.ReadTimeout,
	}

	return srv.ListenAndServe()
}

func (s *AIDecisionService) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+s.cfg.HealthPath, s.handleHealth)
	mux.HandleFunc("POST "+s.cfg.DecisionPath, s.handleDecision)
	return mux
}

func (s *AIDecisionService) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (s *AIDecisionService) handleDecision(w http.ResponseWriter, r *http.Request) {
	// 1. Read the JSON body into the shared request type.
	start := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req placement.DecisionRequest
	err = json.Unmarshal(body, &req)
	if err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("Received decision request",
		"requestId", req.RequestID,
		"inputHash", inputHash(req),
	)
	s.log.Debug("Full request received",
		"requestId", req.RequestID,
		"body", string(body),
	)

	// 2. Pick the flow: rebalance requests carry rebalanceContext, placement ones don't.
	var resp any
	var flow, decidedBy string
	if req.RebalanceContext != nil {
		ans := s.rebalanceAnswer(r.Context(), req)
		resp, flow, decidedBy = ans, "rebalance", ans.DecidedBy
	} else {
		ans := s.placementAnswer(r.Context(), req)
		resp, flow, decidedBy = ans, "placement", ans.DecidedBy
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Flow", flow)
	w.Header().Set("X-Decided-By", decidedBy)
	_ = json.NewEncoder(w).Encode(resp)

	s.log.Info("Decision made",
		"requestId", req.RequestID,
		"flow", flow,
		"decidedBy", decidedBy,
		"latencyMilliSec", float64(time.Since(start).Microseconds())/1000,
		"inputHash", inputHash(req),
		"nodes", len(req.NodeStats),
		"response", resp,
	)
}

func (s *AIDecisionService) placementAnswer(ctx context.Context,
	req placement.DecisionRequest) placement.DecisionResponse {
	scores, reason := scorer.Score(req, s.cfg.ScorerConfig)
	scorerAnswer := placement.DecisionResponse{
		RequestID:  req.RequestID, // echo it back, so logs can join request and answer
		NodeScores: scores,
		Reason:     reason,
		DecidedBy:  placement.DecidedByScorer,
	}
	if s.placementDecider == nil {
		return scorerAnswer
	}
	s.log.Info("Scorer answer is in standby",
		"requestId", req.RequestID, "scoreAnswer", scorerAnswer)

	modelAnswer, err := s.placementDecider.DecidePlacement(ctx, req)
	if err == nil {
		err = checkPlacementAnswer(modelAnswer, req, s.cfg.ScorerConfig)
	}
	if err != nil {
		s.log.Warn("Placement model failed, scorer will be used instead",
			"requestId", req.RequestID, "error", err)
		scorerAnswer.DecidedBy = placement.DecidedByFallback
		return scorerAnswer
	}
	modelAnswer.DecidedBy = placement.DecidedByModel
	modelAnswer.RequestID = req.RequestID
	s.log.Info("Placement model answered successfully",
		"requestId", req.RequestID, "modelAnswer", modelAnswer)
	s.log.Debug("Comparing the model answer with the scorer answer",
		"requestId", req.RequestID, "modelReason",
		modelAnswer.Reason, "scoreReason", scorerAnswer.Reason)
	return modelAnswer
}

// rebalanceAnswer has the rules' answer ready first; it asks the rebalance model when one is set and a change
// is possible, and sends the rules' answer whenever the model fails or its answer fails the check.
func (s *AIDecisionService) rebalanceAnswer(ctx context.Context,
	req placement.DecisionRequest) placement.RebalanceDecisionResponse {
	assessment := rebalance.Assess(req, time.Now(), s.cfg.RebalanceConfig)
	rulesAnswer := assessment.RulesAnswer(s.cfg.ScorerConfig)
	if s.rebalanceDecider == nil || !assessment.AnyChangeAllowed() {
		return rulesAnswer
	}
	s.log.Info("Rules answer is in standby", "requestId", req.RequestID, "rulesAnswer", rulesAnswer)

	modelAnswer, err := s.rebalanceDecider.DecideRebalance(ctx, req, assessment)
	if err == nil {
		if why := assessment.Check(modelAnswer); why != "" {
			err = errors.New(why)
		}
	}
	if err != nil {
		s.log.Warn("Rebalance model failed, rules will be used instead",
			"requestId", req.RequestID, "error", err)
		rulesAnswer.DecidedBy = placement.DecidedByFallback
		return rulesAnswer
	}
	modelAnswer.RequestID = req.RequestID
	modelAnswer.DecidedBy = placement.DecidedByModel
	s.log.Info("Rebalance model answered successfully",
		"requestId", req.RequestID, "modelAnswer", modelAnswer)
	return modelAnswer
}

// inputHash fingerprints what was asked, ignoring the IDs and time that change on every call.
func inputHash(req placement.DecisionRequest) string {
	req.RequestID = ""            // exclude request ID from hash
	req.Timestamp = metav1.Time{} // exclude timestamp from hash
	if req.RebalanceContext != nil {
		rc := *req.RebalanceContext // make a copy
		rc.DecisionID = ""          // exclude decision ID from hash
		req.RebalanceContext = &rc
	}
	body, _ := json.Marshal(req)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}

// checkPlacementAnswer returns why a model answer can't be used, whichever model gave it:
// each candidate needs exactly one score from 0 to 100, and a node that isn't allowed must score 0.
func checkPlacementAnswer(answer placement.DecisionResponse, req placement.DecisionRequest,
	scorerCfg scorer.Config) error {
	strategy := scorer.Strategy(req.AOProfile)
	pod := scorer.PodRequests(req.Pod)
	resources := scorer.ScoredResources(req.AOProfile)
	unscored := map[string]placement.NodeStat{}
	for _, node := range req.NodeStats {
		if node.Candidate {
			unscored[node.Name] = node
		}
	}
	for _, nodeScore := range answer.NodeScores {
		nodeName := nodeScore.NodeName
		node, ok := unscored[nodeName]
		if !ok {
			return fmt.Errorf("node %s is not a possible node or scored twice", nodeName)
		}
		if nodeScore.Score < MinScore ||
			nodeScore.Score > MaxScore || math.IsNaN(nodeScore.Score) {
			return fmt.Errorf("node %s has invalid score %v", nodeName, nodeScore.Score)
		}
		// unknown usage can't be judged, so it passes, as the scorer gives it UnknownScore
		util, known := scorer.Utilization(node, pod, resources)
		if nodeScore.Score > 0 && known && !scorer.Allowed(util, strategy, scorerCfg) {
			return fmt.Errorf("node %s is not allowed (no fit, or over the Packed limit) but scored %v",
				nodeName, nodeScore.Score)
		}
		delete(unscored, nodeName)
	}
	if len(unscored) > 0 {
		return fmt.Errorf("nodes %v were left unscored", maps.Keys(unscored))
	}
	return nil
}
