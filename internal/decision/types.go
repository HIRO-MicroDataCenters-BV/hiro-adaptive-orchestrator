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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// =============================================================================
// DecisionRequest
//
// The complete per-pod decision payload assembled by DecisionContextBuilder.
// This is what gets sent to the External Decision/AI Agent (step 5/D).
//
// It is assembled from THREE sources:
//   1. Kube-Scheduler   → Pod + CandidateNodes  (scheduler sends these, step 4)
//   2. A.O operator     → AOProfile             (fetched via field index)
//   3. E.A.O operator   → EAOProfile            (fetched from E.A.O CRD, optional)
// =============================================================================

// DecisionRequest is the full context payload sent to the External AI Agent
// for a single unscheduled pod. Generated once per pod scheduling event.
type DecisionRequest struct {
	// RequestID is a unique identifier for this decision request.
	// Used to correlate the request with its response in logs.
	RequestID string `json:"requestId"`

	// Timestamp is when this request was assembled.
	Timestamp metav1.Time `json:"timestamp"`

	// Pod contains the details of the unscheduled pod needing placement.
	// Sourced from the kube-scheduler placement context (step 4).
	Pod *corev1.Pod `json:"pod"`

	// CandidateNodes is the list of nodes the kube-scheduler is considering
	// for this pod. Sourced from the scheduler's placement context (step 4).
	// The AI agent scores these nodes and returns them ranked.
	CandidateNodes []*corev1.Node `json:"candidateNodes"`

	// AOProfile is the OrchestrationProfile governing this pod's application.
	// Contains the placement strategy, resource awareness flags, and the
	// current placement snapshot of all existing pods of this application.
	// Sourced from the A.O operator via field index lookup.
	AOProfile *AOProfileContext `json:"aoProfile"`

	// EAOProfile contains per-node energy metrics from the Energy Aware
	// Orchestrator. Only populated when AOProfile.Awareness.Energy == true.
	// nil when energy awareness is disabled or E.A.O data is unavailable.
	EAOProfile *EAOProfileContext `json:"eaoProfile,omitempty"`
}

// // =============================================================================
// // Pod data — sourced from kube-scheduler (step 4)
// // =============================================================================

// // PodDetail carries all pod information relevant for a placement decision.
// type PodDetail struct {
// 	// Name is the pod name.
// 	Name string `json:"name"`

// 	// Namespace is the pod namespace.
// 	Namespace string `json:"namespace"`

// 	// UID is the pod's unique identifier, stable across restarts.
// 	UID string `json:"uid"`

// 	// ResourceRequests are the pod's declared resource requests.
// 	// Used by the AI agent for bin-packing and resource-aware scoring.
// 	ResourceRequests ResourceRequirements `json:"resourceRequests"`
// }

// ResourceRequirements captures CPU and memory requests for a pod.
// Values are in Kubernetes quantity string format (e.g. "500m", "128Mi").
type ResourceRequirements struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	GPU    string `json:"gpu,omitempty"` // populated when AOProfile.Awareness.GPU == true
}

// =============================================================================
// Node data — sourced from kube-scheduler (step 4)
// =============================================================================

// // NodeDetail describes a single candidate node sent by the kube-scheduler.
// // The AI agent uses this to score nodes relative to each other.
// type NodeDetail struct {
// 	// Name is the Kubernetes node name.
// 	Name string `json:"name"`

// 	// AllocatableResources is what remains available on the node
// 	// after accounting for existing pod requests.
// 	AllocatableResources ResourceRequirements `json:"allocatableResources"`

// 	// TotalResources is the node's total capacity.
// 	TotalResources ResourceRequirements `json:"totalResources"`

// 	// RunningPodCount is the number of pods currently running on this node.
// 	// Used by the AI agent for spread/pack decisions.
// 	RunningPodCount int `json:"runningPodCount"`
// }

// =============================================================================
// A.O Profile data — sourced from the A.O operator (fetched per pod)
// =============================================================================

// AOProfileContext is the subset of OrchestrationProfile data that is
// relevant for making a per-pod placement decision.
type AOProfileContext struct {
	// ProfileName is the name of the governing OrchestrationProfile.
	ProfileName string `json:"profileName"`

	// Strategy is the placement strategy declared in the profile.
	// One of: Balanced, Packed, Spread.
	Strategy string `json:"strategy"`

	// Awareness flags tell the AI agent which dimensions to optimise for.
	Awareness AwarenessFlags `json:"awareness"`

	// CurrentPlacement is a snapshot of where the pod of this
	// application is currently placed. The AI agent uses this to make
	// decisions for the new pod.
	CurrentPlacement PodPlacement `json:"currentPlacement"`

	// Rebalancing carries the rebalancing configuration from the profile.
	Rebalancing RebalancingConfig `json:"rebalancing"`
}

// AwarenessFlags mirrors the profile's Awareness spec, telling the AI agent
// which resource dimensions to consider when scoring nodes.
type AwarenessFlags struct {
	CPU    bool `json:"cpu"`
	Memory bool `json:"memory"`
	GPU    bool `json:"gpu"`
	Energy bool `json:"energy"`
}

// PodPlacement records where a single existing pod of the application
// is currently placed. Used by the AI agent for spread decisions.
type PodPlacement struct {
	// PodName is the name of the existing pod.
	PodName string `json:"podName"`

	// NodeName is the node this pod is currently running on.
	// Empty string means the pod is not yet scheduled.
	NodeName string `json:"nodeName,omitempty"`

	// Phase is the pod's current lifecycle phase (Running, Pending, Failed).
	Phase string `json:"phase"`
}

// RebalancingConfig carries the rebalancing configuration from the profile.
type RebalancingConfig struct {
	Enabled           bool     `json:"enabled"`
	CooldownSeconds   int      `json:"cooldownSeconds,omitempty"`
	TriggerConditions []string `json:"triggerConditions,omitempty"`
}

// =============================================================================
// E.A.O Profile data — sourced from the Energy Aware Orchestrator
// =============================================================================

// EAOProfileContext carries per-node energy metrics from the E.A.O.
// Only assembled when AOProfile.Awareness.Energy == true.
type EAOProfileContext struct {
	// NodeEnergyData is the list of per-node energy metrics.
	NodeEnergyData []NodeEnergyData `json:"nodeEnergyData"`
}

// NodeEnergyData holds energy metrics for a single node.
type NodeEnergyData struct {
	// NodeName is the Kubernetes node name.
	NodeName string `json:"nodeName"`

	// EnergyScore is a normalised score (0.0–1.0) where lower means
	// more energy efficient. Used by the AI agent when Energy awareness
	// is enabled.
	EnergyScore float64 `json:"energyScore"`

	// PowerWatts is the current power draw of the node in watts.
	PowerWatts float64 `json:"powerWatts,omitempty"`
}

// =============================================================================
// DecisionResponse — returned by the External AI Agent (step 6/E)
// =============================================================================

// DecisionResponse is what the External Decision/AI Agent returns
// after scoring the candidate nodes for the unscheduled pod.
// The Decision Context Builder translates this into a ValidPlacementDecision
// that is returned to the Kube-Scheduler (step 7).
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

// =============================================================================
// PlacementContext — what the kube-scheduler sends to A.O (step 4)
//
// This is the inbound payload received from the scheduler's custom scoring
// plugin. The DecisionContextBuilder takes this as its primary input and
// enriches it with AOProfile and EAOProfile data to produce a DecisionRequest.
// =============================================================================

// PlacementContext is the inbound payload from the kube-scheduler.
// Received at step 4 in the architecture.
type PlacementContext struct {
	// Pod is the unscheduled pod that needs a placement decision.
	Pod *corev1.Pod `json:"pod"`

	// CandidateNodes is the list of nodes the scheduler has pre-filtered
	// as feasible for this pod. The AI agent scores only these nodes.
	CandidateNodes []*corev1.Node `json:"candidateNodes"`
}
