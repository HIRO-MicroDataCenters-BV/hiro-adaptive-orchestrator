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

package placement

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
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

	// AOProfile is the OrchestrationProfile governing this pod's application.
	// Contains the placement strategy, resource awareness flags, and the
	// current placement snapshot of all existing pods of this application.
	// Sourced from the A.O operator via field index lookup.
	AOProfile *AOProfileContext `json:"aoProfile"`

	// EAOProfile contains per-node energy metrics from the Energy Aware
	// Orchestrator. Only populated when AOProfile.Awareness.Energy == true.
	// nil when energy awareness is disabled or E.A.O data is unavailable.
	EAOProfile *EAOProfileContext `json:"eaoProfile,omitempty"`

	// NodeStats lists every candidate node; rebalance also adds the app's other nodes
	// (e.g. NotReady) with candidate=false. Only candidates may be picked.
	NodeStats []NodeStat `json:"nodeStats"`

	// RebalanceContext is populated only for rebalance evaluations (the
	// rebalance engine's Evaluating stage), nil for initial-placement
	// requests. Its presence is how the AI agent distinguishes "score this
	// new pod" from "reconsider this already-placed workload" — same
	// endpoint, same conversation, richer payload. See the rebalance
	// engine design notes: "No new decision path. Same brain, richer
	// conversation."
	RebalanceContext *RebalanceContext `json:"rebalanceContext,omitempty"`
}

// =============================================================================
// Rebalance decision data — sourced from the rebalance engine's Evaluating
// stage (internal/rebalance), not from the kube-scheduler.
// =============================================================================

// RebalanceContext carries what the AI needs to reconsider an
// ALREADY-PLACED workload, as opposed to DecisionRequest's Pod field, which
// is a single unscheduled pod (initial placement only — left nil here).
type RebalanceContext struct {
	// Reason is why the rebalance engine is asking — the trigger condition
	// and detail that caused this workload to be reconsidered.
	Reason string `json:"reason"`

	// DecisionID correlates this request with the profile's decision
	// lifecycle status, engine logs, and Kubernetes Events.
	DecisionID string `json:"decisionId"`

	// CurrentPlacements is every pod of the application and which node it's
	// currently on. The AI needs the whole layout — not just one pod — to
	// judge imbalance and pick both which pod to move and where.
	CurrentPlacements []PodPlacement `json:"currentPlacements"`

	// RebalanceState is inlined: its fields sit directly in rebalanceContext.
	RebalanceState `json:",inline"`

	// Trigger is the structured form of Reason.
	Trigger *RebalanceTrigger `json:"trigger,omitempty"`

	// Workload is the profile's workload with its replica bounds and the
	// autoscalers that already own its sizing.
	Workload *RebalanceWorkload `json:"workload,omitempty"`

	// PDB is the PodDisruptionBudget covering the workload's pods; nil when
	// none exists.
	PDB *RebalancePDB `json:"pdb,omitempty"`
}

// RebalanceState is the workload's past decisions plus its current cooldown and failure streak,
// so the decider can avoid repeats and flip-flops without keeping state of its own.
type RebalanceState struct {
	// History is newest first, trimmed to HistoryWindow at request time.
	History       []orchestrationv1alpha1.HistoryEntry `json:"history"`
	HistoryWindow metav1.Duration                      `json:"historyWindow"`

	// CooldownUntil is nil when no cooldown is active.
	CooldownUntil       *metav1.Time `json:"cooldownUntil,omitempty"`
	ConsecutiveFailures int32        `json:"consecutiveFailures"`
	EscalationThreshold int32        `json:"escalationThreshold"`
}

// RebalanceTrigger is the trigger condition that started this evaluation.
type RebalanceTrigger struct {
	// Type is the profile's TriggerCondition that matched (CPUThreshold,
	// MemoryThreshold, EnergyThreshold, NodeFailure, PeriodicReview).
	Type string `json:"type"`

	// Node is the node the condition was observed on, for node-scoped triggers.
	Node string `json:"node,omitempty"`

	// Value and Threshold are fractions of allocatable (0–1) for
	// CPU/MemoryThreshold; nil for triggers that have no measured value.
	Value     *float64 `json:"value,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
}

// RebalanceWorkload describes the workload being reconsidered.
type RebalanceWorkload struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`

	// Replicas is the workload's desired replica count (spec.replicas).
	Replicas int32 `json:"replicas"`

	// MinReplicas and MaxReplicas are the bounds an AdjustReplicas target
	// must stay within: the HPA's when one exists, else operator defaults.
	MinReplicas int32 `json:"minReplicas"`
	MaxReplicas int32 `json:"maxReplicas"`

	// ImprovementThreshold is the smallest Improvement the operator enacts; a lower answer is Rejected.
	// 0 means unknown (an older operator).
	ImprovementThreshold float64 `json:"improvementThreshold"`
}

// RebalancePDB is the disruption budget that limits evictions for a Move.
type RebalancePDB struct {
	Name               string `json:"name"`
	AllowedDisruptions int32  `json:"allowedDisruptions"`
}

// NodeStat is a compact, decision-relevant view of one node.
// Null pointer fields mean unknown, never zero.
type NodeStat struct {
	Name string `json:"name"`

	// Candidate means the node may be picked; its health is in Conditions.
	Candidate bool `json:"candidate"`

	Labels map[string]string `json:"labels,omitempty"`

	Taints        []corev1.Taint    `json:"taints,omitempty"`
	Unschedulable bool              `json:"unschedulable"`
	Conditions    map[string]string `json:"conditions,omitempty"` // condition type → status

	Platform NodePlatform `json:"platform"`

	// Resources covers every resource the node reports, keyed by resource name.
	Resources map[string]NodeResource `json:"resources"`

	// AppPods counts this application's non-terminated pods on the node.
	AppPods *int `json:"appPods"`

	// ImagesPresent lists the placed pod's images already on the node (placement only).
	ImagesPresent []string `json:"imagesPresent,omitempty"`

	// Energy stays null until per-node energy data exists.
	Energy *float64 `json:"energy"`

	ObservedAt metav1.Time `json:"observedAt"`
}

// NodeResource is one resource in base units (CPU in millicores).
type NodeResource struct {
	Allocatable int64 `json:"allocatable"`

	// Requested sums non-terminated pods' requests, excluding the pod being placed.
	Requested *int64 `json:"requested"`

	// Used is live usage; null until a metrics source is wired in.
	Used *int64 `json:"used"`
}

// NodePlatform is the node's OS and runtime stack.
type NodePlatform struct {
	Arch    string `json:"arch,omitempty"`
	OS      string `json:"os,omitempty"`
	Kernel  string `json:"kernel,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	Kubelet string `json:"kubelet,omitempty"`
}

// RebalanceDecisionResponse is the AI's answer to a rebalance evaluation —
// broader than DecisionResponse's NodeScores, since the AI can recommend
// actions beyond placement scoring (Move today; NoOp; future actions per
// the design doc's enactor table: AdjustResources, AdjustReplicas, Defer,
// Escalate).
type RebalanceDecisionResponse struct {
	// RequestID echoes the request ID for correlation.
	RequestID string `json:"requestId"`

	// Action is the AI's recommended action. An action this operator
	// doesn't recognize is handled defensively by the caller, not by this
	// type.
	Action orchestrationv1alpha1.RebalanceAction `json:"action"`

	// PodName identifies which pod from the request's
	// RebalanceContext.CurrentPlacements the action applies to. Required
	// when Action == Move — the AI is shown the full current placement
	// (every pod of the application), so it must say which one it means;
	// TargetNode alone is ambiguous whenever more than one pod could move
	// to the same node. For AdjustResources it's informational only (which
	// pod the AI was looking at when it read ContainerName's current
	// values) — the patch itself applies to the whole workload's template,
	// not just this one pod instance. Empty for actions that aren't
	// pod-specific (NoOp).
	PodName string `json:"podName,omitempty"`

	// TargetNode is populated when Action == Move — the node the AI
	// recommends moving PodName to.
	TargetNode string `json:"targetNode,omitempty"`

	// TargetReplicas is populated when Action == AdjustReplicas — the
	// absolute replica count the AI recommends (not a delta), same
	// convention as TargetNode for Move.
	TargetReplicas int32 `json:"targetReplicas,omitempty"`

	// ContainerName is populated when Action == AdjustResources — the name
	// of the container (within PodName, and so within the workload's pod
	// template) the AI's TargetCPU/TargetMemory apply to. Required: with a
	// workload free to have any number of containers, there's no default to
	// guess — the AI is shown each pod's container names (and current
	// resources) via RebalanceContext.CurrentPlacements[].Containers
	// specifically so it can name one back.
	ContainerName string `json:"containerName,omitempty"`

	// TargetCPU and TargetMemory are populated when Action ==
	// AdjustResources — resource.Quantity strings (e.g. "500m", "512Mi"),
	// same absolute-value convention as TargetReplicas. At least one must be
	// set; either may be left empty to leave that resource unchanged. Applied
	// to both requests and limits of the named container.
	TargetCPU    string `json:"targetCpu,omitempty"`
	TargetMemory string `json:"targetMemory,omitempty"`

	// Improvement is the AI's estimate of how much this action improves the
	// placement, used by the improvement-threshold guardrail .
	Improvement float64 `json:"improvement,omitempty"`

	// Reason is a human-readable explanation, surfaced in status and events.
	Reason string `json:"reason,omitempty"`

	// Confidence and DecidedBy: see DecisionResponse.
	Confidence float64 `json:"confidence,omitempty"`
	DecidedBy  string  `json:"decidedBy,omitempty"`
}

// ResourceRequirements captures CPU and memory requests for a pod.
// Values are in Kubernetes quantity string format (e.g. "500m", "128Mi").
type ResourceRequirements struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	GPU    string `json:"gpu,omitempty"` // populated when AOProfile.Awareness.GPU == true
}

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

	// Containers lists this pod's containers with their current CPU/Memory
	// configuration — populated for rebalance requests (buildCurrentPlacements)
	// so the AI can recommend AdjustResources against a named container
	// instead of guessing blind. Empty for initial-placement requests
	// (buildCurrentPlacement), which have no use for it.
	Containers []ContainerInfo `json:"containers,omitempty"`
}

// ContainerInfo is one container's name and current CPU/Memory requests and
// limits (each omitted if unset on the container), shown to the AI so an
// AdjustResources recommendation can name a specific container and reason
// about its current values rather than proposing a change blind.
type ContainerInfo struct {
	Name          string `json:"name"`
	CPURequest    string `json:"cpuRequest,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// RebalancingConfig carries the rebalancing configuration from the profile.
type RebalancingConfig struct {
	Enabled           bool     `json:"enabled"`
	CooldownSeconds   int      `json:"cooldownSeconds,omitempty"`
	TriggerConditions []string `json:"triggerConditions,omitempty"`

	// True means the profile says something else owns that dimension; never change it.
	ReplicasExternallyManaged  bool `json:"replicasExternallyManaged"`
	ResourcesExternallyManaged bool `json:"resourcesExternallyManaged"`
}

// =============================================================================
// E.A.O Profile data — sourced from the Energy Aware Orchestrator
// =============================================================================

// EAOProfileContext carries energy data from the EnergyAwareOrchestration CRD
// (eas.hiro.io/v1) for the pod's governing application.
// Only assembled when AOProfile.Awareness.Energy == true.
type EAOProfileContext struct {
	// NodeEnergyData is optional per-node energy scoring (populated by future sources).
	NodeEnergyData []NodeEnergyData `json:"nodeEnergyData,omitempty"`

	// Priority is the scheduling priority from the EAO spec (Critical | Preferred | Optional).
	Priority string `json:"priority,omitempty"`

	// EnergyConsumptionWatts is the estimated energy requirement declared in the EAO spec.
	EnergyConsumptionWatts int64 `json:"energyConsumptionWatts,omitempty"`

	// Decision is the scheduling decision produced by the EnergyAwareOrchestrator.
	// nil when the EAO status has not yet been populated.
	Decision *EAODecision `json:"decision,omitempty"`

	// EnergyMetrics holds the energy availability snapshot from the EAO status.
	// nil when the EAO status has not yet been populated.
	EnergyMetrics *EAOEnergyMetrics `json:"energyMetrics,omitempty"`

	// LastUpdated is the EAO's last reconcile time (RFC 3339) — the field to
	// judge freshness by, not Decision.NextEvaluationTime.
	LastUpdated string `json:"lastUpdated,omitempty"`

	// Verdict is Decision.Action in this repo's terms.
	Verdict EAOVerdict `json:"verdict"`

	// HoldUntil is when a Hold ends (the slot start); nil when unknown.
	HoldUntil *metav1.Time `json:"holdUntil,omitempty"`
}

// EAOVerdict is what the EAO's decision means for running the workload now.
type EAOVerdict string

const (
	EAOVerdictRunNow  EAOVerdict = "RunNow"  // may run now
	EAOVerdictHold    EAOVerdict = "Hold"    // no energy now; wait until HoldUntil
	EAOVerdictUnknown EAOVerdict = "Unknown" // no usable decision: missing, an EAO failure, or an unmapped value
)

// EAODecision is the scheduling decision from the EnergyAwareOrchestrator.
type EAODecision struct {
	// Action is one of: DeployImmediately | Scheduled | Delayed | Waiting.
	// It is the run-now-or-wait verdict; never recompute it from the watts.
	Action string `json:"action"`

	// Reason is a human-readable explanation for the decision.
	Reason string `json:"reason"`

	// NextEvaluationTime equals ScheduledSlot.SlotStart and is absent for
	// DeployImmediately (ISO 8601).
	NextEvaluationTime string `json:"nextEvaluationTime,omitempty"`

	// ScheduledSlot is when the workload should run; hold until SlotStart.
	ScheduledSlot *EAOScheduledSlot `json:"scheduledSlot,omitempty"`
}

// EAOScheduledSlot is the energy window the EAO assigned to the workload.
type EAOScheduledSlot struct {
	SlotStart string `json:"slotStart,omitempty"`
	SlotEnd   string `json:"slotEnd,omitempty"`
}

// EAOEnergyMetrics is the energy availability snapshot from the EAO status.
type EAOEnergyMetrics struct {
	// RequiredWatts is spec.energyConsumption copied over — the user's
	// estimate. Informational: the EAO decision already accounts for it.
	RequiredWatts float64 `json:"requiredWatts"`

	// MeasuredWatts is the workload's real or predicted draw. Informational.
	MeasuredWatts float64 `json:"measuredWatts,omitempty"`

	// Sufficient indicates whether current energy supply meets the requirement.
	Sufficient bool `json:"sufficient"`

	// CurrentSlotAvailableWatts is the energy available in the current 6-hour slot.
	CurrentSlotAvailableWatts float64 `json:"currentSlotAvailableWatts,omitempty"`

	// CurrentSlotConsumedWatts is the energy currently being consumed.
	CurrentSlotConsumedWatts float64 `json:"currentSlotConsumedWatts,omitempty"`
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
