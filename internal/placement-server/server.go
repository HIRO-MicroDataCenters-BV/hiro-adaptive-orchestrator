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

package placementserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// =============================================================================
// PlacementServer
//
// Single HTTP listener inside the operator pod serving four routes:
//
//   Plugin path (HIROScore scheduler plugin):
//     POST /api/v1/placement/score   — AI node scoring   → handleScore
//     POST /api/v1/placement/filter  — energy gate check → handleFilter
//
//   Extender path (default kube-scheduler extender protocol):
//     POST /extender/prioritize      — AI node scoring   → handleExtenderPrioritize
//     POST /extender/filter          — energy gate check → handleExtenderFilter
//
//   Health:
//     GET  /healthz                  — liveness/readiness probe
//
// The AI scoring and energy gate LOGIC is shared via two private methods:
//   score()  — builder.Build + client.RequestDecision (used by plugin + extender)
//   filter() — builder.CheckEnergyGate               (used by plugin + extender)
//
// Handlers are thin protocol adapters: decode input → call shared method →
// encode output in the format the caller expects.
// =============================================================================

// PlacementServer is the HTTP server that handles both the scheduler plugin
// and the kube-scheduler extender protocols.
type PlacementServer struct {
	// Addr is the listening address (default ":8090").
	Addr string

	// Plugin-path endpoints
	scorePath  string // POST /api/v1/placement/score  — AI scoring
	filterPath string // POST /api/v1/placement/filter — energy gate

	// Health endpoint
	healthPath string

	// Extender-path endpoints (kube-scheduler extender protocol)
	extenderFilterPath     string
	extenderPrioritizePath string

	builder *DecisionContextBuilder
	client  *DecisionClient
	server  *http.Server

	// requestTimeout is applied per request.
	requestTimeout time.Duration
}

// NewPlacementServer creates a PlacementServer.
func NewPlacementServer(
	builder *DecisionContextBuilder,
	client *DecisionClient,
	port string,
	scorePath string,
	filterPath string,
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
		scorePath:              scorePath,
		filterPath:             filterPath,
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
	mux.HandleFunc(s.scorePath, s.handleScore)
	mux.HandleFunc(s.filterPath, s.handleFilter)
	mux.HandleFunc(s.healthPath, s.handleHealth)
	mux.HandleFunc(s.extenderFilterPath, s.handleExtenderFilter)
	mux.HandleFunc(s.extenderPrioritizePath, s.handleExtenderPrioritize)

	s.server = &http.Server{
		Addr:    s.Addr,
		Handler: mux,
	}

	logger.Info("placement: server starting",
		"addr", s.Addr,
		"scorePath", s.scorePath,
		"filterPath", s.filterPath,
	)

	go func() {
		<-ctx.Done()
		logger.Info("placement: server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			logger.Error(err, "placement: server shutdown error")
		}
	}()

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("placement server: %w", err)
	}
	return nil
}

// =============================================================================
// Shared core — single implementation used by BOTH plugin and extender paths
// =============================================================================

// score runs the full AI decision pipeline: build DecisionRequest via the
// builder, send it to the External AI Agent, return the DecisionResponse.
//
// Used by:
//   - handleScore             (plugin path: POST /api/v1/placement/score)
//   - handleExtenderPrioritize (extender path: POST /extender/prioritize)
func (s *PlacementServer) score(
	ctx context.Context,
	placementCtx PlacementContext,
	requestID string,
) (*DecisionResponse, error) {
	req, err := s.builder.Build(ctx, placementCtx, requestID)
	if err != nil {
		return nil, err
	}
	return s.client.RequestDecision(ctx, req)
}

// filter runs the energy gate check for a single pod.
//
// Used by:
//   - handleFilter        (plugin path: POST /api/v1/placement/filter)
//   - handleExtenderFilter (extender path: POST /extender/filter)
func (s *PlacementServer) filter(ctx context.Context, pod *corev1.Pod) (EnergyGateResponse, error) {
	return s.builder.CheckEnergyGate(ctx, pod)
}

// =============================================================================
// POST /api/v1/placement/score  (plugin path — AI scoring)
//
// Request:  PlacementContext { *corev1.Pod, []*corev1.Node }
// Response: DecisionResponse { NodeScores, Reason }
// =============================================================================

func (s *PlacementServer) handleScore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()
	logger := logf.FromContext(ctx)

	var placementCtx PlacementContext
	if err := json.NewDecoder(r.Body).Decode(&placementCtx); err != nil {
		logger.Error(err, "placement: decode request failed", "remoteAddr", r.RemoteAddr)
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if placementCtx.Pod == nil {
		http.Error(w, "pod is required in PlacementContext", http.StatusBadRequest)
		return
	}

	requestID := string(placementCtx.Pod.UID)
	logger.Info("placement: score request received",
		"requestId", requestID,
		"remoteAddr", r.RemoteAddr,
		"pod", placementCtx.Pod.Name,
		"namespace", placementCtx.Pod.Namespace,
		"candidateNodes", len(placementCtx.CandidateNodes),
	)

	decisionResp, err := s.score(ctx, placementCtx, requestID)
	if err != nil {
		logger.Error(err, "placement: score request failed",
			"requestId", requestID,
			"pod", placementCtx.Pod.Name,
		)
		http.Error(w, fmt.Sprintf("placement score failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(decisionResp); err != nil {
		logger.Error(err, "placement: encode response failed", "requestId", requestID)
		return
	}

	logger.Info("placement: score response sent",
		"requestId", requestID,
		"pod", placementCtx.Pod.Name,
		"nodeScores", len(decisionResp.NodeScores),
		"topNode", topNodeName(decisionResp.NodeScores),
		"reason", decisionResp.Reason,
	)
}

// =============================================================================
// POST /api/v1/placement/filter  (plugin path — energy gate)
//
// Request:  EnergyGateRequest  { *corev1.Pod }
// Response: EnergyGateResponse { Allowed bool, Reason string }
//
// Soft-fail open: on any error the response is Allowed=true so the pod is
// never blocked by infrastructure failures.
// =============================================================================

func (s *PlacementServer) handleFilter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()
	logger := logf.FromContext(ctx)

	var req EnergyGateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if req.Pod == nil {
		http.Error(w, "pod is required", http.StatusBadRequest)
		return
	}

	requestID := string(req.Pod.UID)
	logger.Info("placement: filter request received",
		"requestId", requestID,
		"pod", req.Pod.Name,
		"namespace", req.Pod.Namespace,
	)

	gate, err := s.filter(ctx, req.Pod)
	if err != nil {
		logger.Error(err, "placement: filter check failed, allowing scheduling",
			"requestId", requestID,
			"pod", req.Pod.Name,
		)
		gate = EnergyGateResponse{Allowed: true}
	}

	if !gate.Allowed {
		logger.Info("placement: filter blocked pod",
			"requestId", requestID,
			"pod", req.Pod.Name,
			"namespace", req.Pod.Namespace,
			"reason", gate.Reason,
		)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	_ = json.NewEncoder(w).Encode(gate)
}

// handleHealth responds to liveness/readiness probes from Kubernetes.
func (s *PlacementServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
