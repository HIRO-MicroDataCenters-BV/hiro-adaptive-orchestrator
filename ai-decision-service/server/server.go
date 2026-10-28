package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

type AIDecisionService struct {
	cfg Config
	log *slog.Logger
}

func New(cfg Config, log *slog.Logger) *AIDecisionService {
	return &AIDecisionService{
		cfg: cfg,
		log: log,
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
	s.log.Debug("Received decision request",
		"requestId", req.RequestID,
		"inputHash", inputHash(req),
		"podName", req.Pod.Name,
		"request", req,
	)

	// 2. Pick the flow: rebalance requests carry rebalanceContext, placement ones don't.
	var resp any
	var flow, decidedBy string
	if req.RebalanceContext != nil {
		ans := rebalanceAnswer(req)
		resp, flow, decidedBy = ans, "rebalance", ans.DecidedBy
	} else {
		ans := s.placementAnswer(req)
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

func (s *AIDecisionService) placementAnswer(req placement.DecisionRequest) placement.DecisionResponse {
	scores, reason := scorer.Score(req, s.cfg.ScorerConfig)
	return placement.DecisionResponse{
		RequestID:  req.RequestID, // echo it back, so logs can join request and answer
		NodeScores: scores,
		Reason:     reason,
		DecidedBy:  placement.DecidedByScorer,
	}
}

func rebalanceAnswer(req placement.DecisionRequest) placement.RebalanceDecisionResponse {
	return placement.RebalanceDecisionResponse{
		RequestID: req.RequestID,
		Action:    orchestrationv1alpha1.RebalanceActionNoOp,
		Reason:    "skeleton: no rules yet",
		DecidedBy: placement.DecidedByRules,
	}
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
