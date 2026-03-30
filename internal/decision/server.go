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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// PlacementDecisionPath is the endpoint the kube-scheduler plugin calls.
	PlacementDecisionPath = "/api/v1/placement/decision"

	// HealthPath is for liveness/readiness probes.
	HealthPath = "/healthz"

	// defaultServerPort is the default listening port.
	defaultServerPort = ":8090"

	// requestTimeout is applied per request.
	// Must be longer than DecisionClient timeout but shorter than the
	// scheduler's own scoring phase timeout.
	requestTimeout = 10 * time.Second
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
type PlacementServer struct {
	// Addr is the listening address (default ":8090").
	Addr string

	builder *DecisionContextBuilder
	client  *DecisionClient
	server  *http.Server
}

// NewPlacementServer creates a PlacementServer.
//
// addr: listening address e.g. ":8090". Pass "" to use the default.
//
// In cmd/main.go:
//
//	placementServer := decision.NewPlacementServer(
//	    contextBuilder,
//	    decisionClient,
//	    os.Getenv("PLACEMENT_SERVER_PORT"),
//	)
//	go placementServer.Start(ctx)
func NewPlacementServer(
	builder *DecisionContextBuilder,
	client *DecisionClient,
	port string,
) *PlacementServer {
	if port == "" {
		port = defaultServerPort
	}
	var addr string
	if len(port) > 0 && port[0] != ':' {
		addr = ":" + port
	} else {
		addr = port
	}
	return &PlacementServer{
		Addr:    addr,
		builder: builder,
		client:  client,
	}
}

// Start registers HTTP routes and begins serving.
// Blocks until ctx is cancelled, then shuts down gracefully.
func (s *PlacementServer) Start(ctx context.Context) error {
	logger := logf.FromContext(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc(PlacementDecisionPath, s.handlePlacementDecision)
	mux.HandleFunc(HealthPath, s.handleHealth)

	s.server = &http.Server{
		Addr:    s.Addr,
		Handler: mux,
	}

	logger.Info("placement server starting",
		"addr", s.Addr,
		"endpoint", PlacementDecisionPath,
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
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
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
