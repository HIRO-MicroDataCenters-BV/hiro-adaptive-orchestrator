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

// Package placement contains the wire-format types shared between the operator's
// PlacementServer and the scheduler's PlacementClient.
//
// These types must live outside internal/ so the scheduler-plugin module
// (scheduler-plugin/go.mod) can import them without violating Go's internal
// package visibility rules.
package placement

import (
	corev1 "k8s.io/api/core/v1"
)

// PlacementContext is the inbound payload sent by the kube-scheduler plugin
// to the operator's PlacementServer.
type PlacementContext struct {
	// Pod is the unscheduled pod that needs a placement decision.
	Pod *corev1.Pod `json:"pod"`

	// CandidateNodes is the list of nodes the scheduler has pre-filtered
	// as feasible for this pod. The AI agent scores only these nodes.
	CandidateNodes []*corev1.Node `json:"candidateNodes"`
}

// DecisionResponse is returned by the PlacementServer to the scheduler plugin.
type DecisionResponse struct {
	// RequestID echoes the request ID for correlation.
	RequestID string `json:"requestId"`

	// NodeScores is the list of candidate nodes ranked by the AI agent.
	// Higher score = more preferred for this pod.
	NodeScores []NodeScore `json:"nodeScores"`

	// Reason is a human-readable explanation of the decision.
	// Surfaced in profile status and operator logs.
	Reason string `json:"reason,omitempty"`
}

// NodeScore is the AI agent's score for a single candidate node.
type NodeScore struct {
	// NodeName is the Kubernetes node name.
	NodeName string `json:"nodeName"`

	// Score is the AI agent's score for this node (higher = more preferred).
	// Typically in range 0–100 to align with Kubernetes scoring conventions.
	Score float64 `json:"score"`
}
