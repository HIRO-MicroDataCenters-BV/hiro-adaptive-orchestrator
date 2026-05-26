/*
Copyright 2026 HIRO Adaptive Orchestrator.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// =============================================================================
// PlacementServer
//
// An HTTP server running inside the A.O operator process.
// It is the entry point for per-pod placement decisions.
//
// Request flow:
//   Kube-scheduler plugin  →  POST PlacementContext{*corev1.Pod, []*corev1.Node}
//   PlacementServer        →  DecisionContextBuilder.Build()
//                          →  DecisionClient.RequestDecision()
//   Kube-scheduler plugin  ←  DecisionResponse{NodeScores}
// =============================================================================

// PlacementServer receives PlacementContext from the kube-scheduler scoring
// plugin, assembles a DecisionRequest, and returns NodeScores.
//
// It also serves the Kubernetes Scheduler Extender protocol on two additional
// endpoints:
//   - extenderFilterPath   -- energy gating (POST ExtenderArgs -> ExtenderFilterResult)
//   - extenderPrioritizePath -- AI scoring  (POST ExtenderArgs -> HostPriorityList)
type PlacementServer struct {
	// Addr is the listening address (default ":8090").
	Addr string

	// placementPath is the HTTP path for placement decisions.
	placementPath string

	// healthPath is the HTTP path for liveness/readiness probes.
	healthPath string

	// extenderFilterPath is the HTTP path for the scheduler extender filter endpoint.
	extenderFilterPath string

	// extenderPrioritizePath is the HTTP path for the scheduler extender prioritize endpoint.
	extenderPrioritizePath string

	builder *DecisionContextBuilder
	client  *DecisionClient
	server  *http.Server

	// requestTimeout is applied per request.
	// Must be longer than DecisionClient timeout but shorter than the
	// scheduler's own scoring phase timeout.
	requestTimeout time.Duration
}

// NewPlacementServer creates a PlacementServer.
//
// port:                   listening address e.g. ":8090".
// placementPath:          HTTP path for placement decisions.
// healthPath:             HTTP path for health probes.
// extenderFilterPath:     HTTP path for the scheduler extender filter endpoint.
// extenderPrioritizePath: HTTP path for the scheduler extender prioritize endpoint.
func NewPlacementServer(
	builder *DecisionContextBuilder,
	client *DecisionClient,
	port string,
	placementPath string,
	healthPath string,
	extenderFilterPath string,
	extenderPrioritizePath string,
	timeout time.Duration,
) *PlacementServer {
	var addr string
	if len(port) > 0 && port[0] != ':' {
		addr = ":" + port
	} else {
		addr = port
	}
	return &PlacementServer{
		Addr:                   addr,
		placementPath:          placementPath,
		healthPath:             healthPath,
		extenderFilterPath:     extenderFilterPath,
		extenderPrioritizePath: extenderPrioritizePath,
		builder:                builder,
		client:                 client,
		requestTimeout:         timeout,
	}
}

// Start registers HTTP routes and begins serving.
// Blocks until ctx is cancelled, then shuts down gracefully.
func (s *PlacementServer) Start(ctx context.Context) error {
	logger := logf.FromContext(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc(s.placementPath, s.handlePlacementDecision)
	mux.HandleFunc(s.healthPath, s.handleHealth)
	mux.HandleFunc(s.extenderFilterPath, s.handleExtenderFilter)
	mux.HandleFunc(s.extenderPrioritizePath, s.handleExtenderPrioritize)

	s.server = &http.Server{
		Addr:    s.Addr,
		Handler: mux,
	}

	logger.Info("placement server starting",
		"addr", s.Addr,
		"endpoint", s.placementPath,
	)

	// Graceful shutdown when manager context is cancelled
	go func() {
		<-ctx.Done()
		logger.Info("placement server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			logger.Error(err, "placement server shutdown error")
		}
	}()

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("placement server: %w", err)
	}
	return nil
}

// =============================================================================
// POST /api/v1/placement/decision
//
// Request:  PlacementContext { *corev1.Pod, []*corev1.Node }
// Response: DecisionResponse { NodeScores, Reason }
// =============================================================================

func (s *PlacementServer) handlePlacementDecision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Per-request timeout — prevents slow AI agent from blocking scheduler
	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	logger := logf.FromContext(ctx)

	// Assign or carry forward the request ID for end-to-end log correlation
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = uuid.NewString()
	}

	logger.Info("placement decision request received",
		"requestId", requestID,
		"remoteAddr", r.RemoteAddr,
	)

	// ------------------------------------------------------------------
	// 1. Decode PlacementContext from the scheduler plugin
	// ------------------------------------------------------------------
	var placementCtx PlacementContext
	if err := json.NewDecoder(r.Body).Decode(&placementCtx); err != nil {
		logger.Error(err, "failed to decode PlacementContext", "requestId", requestID)
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	if placementCtx.Pod == nil {
		http.Error(w, "pod is required in PlacementContext", http.StatusBadRequest)
		return
	}

	logger.Info("placement context decoded",
		"requestId", requestID,
		"pod", placementCtx.Pod.Name,
		"namespace", placementCtx.Pod.Namespace,
		"candidateNodes", len(placementCtx.CandidateNodes),
	)

	// ------------------------------------------------------------------
	// 2. Build DecisionRequest
	//    Enriches PlacementContext with AOProfile + EAOProfile
	// ------------------------------------------------------------------
	decisionReq, err := s.builder.Build(ctx, placementCtx, requestID)
	if err != nil {
		logger.Error(err, "failed to build decision request",
			"requestId", requestID,
			"pod", placementCtx.Pod.Name,
		)
		http.Error(w,
			fmt.Sprintf("failed to build decision context: %v", err),
			http.StatusInternalServerError,
		)
		return
	}

	// ------------------------------------------------------------------
	// 3. Send DecisionRequest to External AI Agent
	//    Returns NodeScores
	// ------------------------------------------------------------------
	decisionResp, err := s.client.RequestDecision(ctx, decisionReq)
	if err != nil {
		logger.Error(err, "failed to get decision from AI agent",
			"requestId", requestID,
			"pod", placementCtx.Pod.Name,
		)
		http.Error(w,
			fmt.Sprintf("failed to get placement decision: %v", err),
			http.StatusBadGateway,
		)
		return
	}

	// ------------------------------------------------------------------
	// 4. Return DecisionResponse to scheduler plugin
	//    Scheduler plugin translates NodeScores → ValidPlacementDecision
	// ------------------------------------------------------------------
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(decisionResp); err != nil {
		logger.Error(err, "failed to encode decision response", "requestId", requestID)
		return
	}

	logger.Info("placement decision response sent",
		"requestId", requestID,
		"pod", placementCtx.Pod.Name,
		"nodeScores", len(decisionResp.NodeScores),
		"topNode", topNodeName(decisionResp.NodeScores),
		"reason", decisionResp.Reason,
	)
}

// handleHealth responds to liveness/readiness probes from Kubernetes.
func (s *PlacementServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// =============================================================================
// POST /extender/filter
//
// Kubernetes Scheduler Extender -- energy gate.
//
// Request:  ExtenderArgs  { Pod, Nodes }
// Response: ExtenderFilterResult { Nodes (allowed) | FailedNodes (blocked) }
//
// When the pod's OrchestrationProfile has energy awareness enabled and the EAO
// reports insufficient energy, ALL candidate nodes are returned in FailedNodes
// so the scheduler defers the pod. For unmanaged pods or when energy data is
// unavailable the full node list is passed through unchanged.
// =============================================================================

func (s *PlacementServer) handleExtenderFilter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()
	logger := logf.FromContext(ctx)

	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = uuid.NewString()
	}

	var args ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if args.Pod == nil || args.Nodes == nil {
		http.Error(w, "pod and nodes are required", http.StatusBadRequest)
		return
	}

	gate, err := s.builder.CheckEnergyGate(ctx, args.Pod)
	if err != nil {
		logger.Error(err, "energy gate check failed, allowing scheduling",
			"requestId", requestID,
			"pod", args.Pod.Name,
		)
		gate = EnergyGateResult{Allowed: true}
	}

	var result ExtenderFilterResult
	if gate.Allowed {
		result.Nodes = args.Nodes
	} else {
		failed := make(map[string]string, len(args.Nodes.Items))
		for _, node := range args.Nodes.Items {
			failed[node.Name] = gate.Reason
		}
		result.FailedNodes = failed
		result.Nodes = &corev1.NodeList{}
		logger.Info("extender filter: energy gate blocked scheduling",
			"requestId", requestID,
			"pod", args.Pod.Name,
			"namespace", args.Pod.Namespace,
			"reason", gate.Reason,
			"blockedNodes", len(failed),
		)
	}

	writeExtenderJSON(w, requestID, result)
}

// =============================================================================
// POST /extender/prioritize
//
// Kubernetes Scheduler Extender -- AI-based node scoring.
//
// Request:  ExtenderArgs     { Pod, Nodes }
// Response: HostPriorityList [ { host, score 0-10 } ]
//
// Builds a full DecisionRequest (profile + EAO context), sends it to the
// External AI Agent, and maps the returned float64 scores to int64 [0, 10].
// On any error (profile not found, AI agent unreachable) all nodes receive
// score 5 so the scheduler's built-in priorities still apply.
// =============================================================================

func (s *PlacementServer) handleExtenderPrioritize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()
	logger := logf.FromContext(ctx)

	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = uuid.NewString()
	}

	var args ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if args.Pod == nil || args.Nodes == nil {
		http.Error(w, "pod and nodes are required", http.StatusBadRequest)
		return
	}

	candidateNodes := make([]*corev1.Node, len(args.Nodes.Items))
	for i := range args.Nodes.Items {
		candidateNodes[i] = &args.Nodes.Items[i]
	}

	placementCtx := PlacementContext{
		Pod:            args.Pod,
		CandidateNodes: candidateNodes,
	}

	decisionReq, err := s.builder.Build(ctx, placementCtx, requestID)
	if err != nil {
		logger.Error(err, "extender prioritize: build failed, returning equal scores",
			"requestId", requestID,
			"pod", args.Pod.Name,
		)
		writeExtenderJSON(w, requestID, equalPriorities(args.Nodes.Items))
		return
	}

	decisionResp, err := s.client.RequestDecision(ctx, decisionReq)
	if err != nil {
		logger.Error(err, "extender prioritize: AI agent unreachable, returning equal scores",
			"requestId", requestID,
			"pod", args.Pod.Name,
		)
		writeExtenderJSON(w, requestID, equalPriorities(args.Nodes.Items))
		return
	}

	priorities := nodeScoresToHostPriorities(decisionResp.NodeScores, args.Nodes.Items)

	logger.Info("extender prioritize response sent",
		"requestId", requestID,
		"pod", args.Pod.Name,
		"namespace", args.Pod.Namespace,
		"nodeCount", len(priorities),
		"topNode", topNodeName(decisionResp.NodeScores),
	)

	writeExtenderJSON(w, requestID, priorities)
}

// =============================================================================
// Extender helpers
// =============================================================================

// normalizeScore maps a float64 AI agent score (expected range 0-100) to an
// int64 in [0, 10] as required by the Kubernetes scheduler extender protocol.
func normalizeScore(score float64) int64 {
	if score <= 0 {
		return 0
	}
	if score >= 100 {
		return 10
	}
	return int64(score / 10)
}

// nodeScoresToHostPriorities converts AI agent NodeScores to HostPriorityList.
// Nodes absent from the AI response receive score 5 (neutral).
func nodeScoresToHostPriorities(scores []NodeScore, nodes []corev1.Node) HostPriorityList {
	scoreMap := make(map[string]float64, len(scores))
	for _, s := range scores {
		scoreMap[s.NodeName] = s.Score
	}
	priorities := make(HostPriorityList, len(nodes))
	for i, node := range nodes {
		score, found := scoreMap[node.Name]
		if !found {
			score = 50
		}
		priorities[i] = HostPriority{
			Host:  node.Name,
			Score: normalizeScore(score),
		}
	}
	return priorities
}

// equalPriorities returns a HostPriorityList with score 5 for every node.
// Used as a safe fallback when the AI agent is unreachable or a profile
// cannot be found, so the scheduler's own priorities still decide placement.
func equalPriorities(nodes []corev1.Node) HostPriorityList {
	priorities := make(HostPriorityList, len(nodes))
	for i, node := range nodes {
		priorities[i] = HostPriority{Host: node.Name, Score: 5}
	}
	return priorities
}

// writeExtenderJSON writes v as JSON with the X-Request-ID header set.
func writeExtenderJSON(w http.ResponseWriter, requestID string, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	_ = json.NewEncoder(w).Encode(v)
}
