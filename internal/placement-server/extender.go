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

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

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

	var args ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if args.Pod == nil || args.Nodes == nil {
		http.Error(w, "pod and nodes are required", http.StatusBadRequest)
		return
	}

	requestID := string(args.Pod.UID)

	logger.Info("extender: filter request received",
		"requestId", requestID,
		"pod", args.Pod.Name,
		"namespace", args.Pod.Namespace,
		"nodeCount", len(args.Nodes.Items),
	)

	gate, err := s.builder.CheckEnergyGate(ctx, args.Pod)
	if err != nil {
		logger.Error(err, "extender: energy gate check failed, allowing scheduling",
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
		logger.Info("extender: filter blocked by energy gate",
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

	var args ExtenderArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if args.Pod == nil || args.Nodes == nil {
		http.Error(w, "pod and nodes are required", http.StatusBadRequest)
		return
	}

	requestID := string(args.Pod.UID)

	logger.Info("extender: prioritize request received",
		"requestId", requestID,
		"pod", args.Pod.Name,
		"namespace", args.Pod.Namespace,
		"nodeCount", len(args.Nodes.Items),
	)

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
		logger.Error(err, "extender: prioritize build failed, returning equal scores",
			"requestId", requestID,
			"pod", args.Pod.Name,
		)
		writeExtenderJSON(w, requestID, equalPriorities(args.Nodes.Items))
		return
	}

	decisionResp, err := s.client.RequestDecision(ctx, decisionReq)
	if err != nil {
		logger.Error(err, "extender: prioritize agent unreachable, returning equal scores",
			"requestId", requestID,
			"pod", args.Pod.Name,
		)
		writeExtenderJSON(w, requestID, equalPriorities(args.Nodes.Items))
		return
	}

	priorities := nodeScoresToHostPriorities(decisionResp.NodeScores, args.Nodes.Items)

	logger.Info("extender: prioritize response sent",
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
