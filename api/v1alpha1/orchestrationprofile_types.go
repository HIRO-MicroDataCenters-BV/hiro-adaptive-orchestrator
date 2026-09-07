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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Application Reference
type ApplicationReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

// Placement Awareness
type Awareness struct {
	CPU    bool `json:"cpu,omitempty"`
	Memory bool `json:"memory,omitempty"`
	GPU    bool `json:"gpu,omitempty"`
	Energy bool `json:"energy,omitempty"`
}

// Placement Spec
type PlacementSpec struct {
	Strategy  string    `json:"strategy"`
	Awareness Awareness `json:"awareness,omitempty"`
}

// Rebalancing Spec
type RebalancingSpec struct {
	Enabled           bool     `json:"enabled"`
	TriggerConditions []string `json:"triggerConditions,omitempty"`
	CooldownSeconds   int      `json:"cooldownSeconds,omitempty"`
	DryRun            bool     `json:"dryRun,omitempty"`
}

// OrchestrationProfileSpec defines the desired state of OrchestrationProfile
type OrchestrationProfileSpec struct {
	// applicationRef references the application for which this orchestration profile is defined.
	// +required
	ApplicationRef ApplicationReference `json:"applicationRef"`

	// placement defines the placement strategy and awareness for the application.
	// +required
	Placement PlacementSpec `json:"placement"`

	// rebalancing defines the rebalancing strategy for the application.
	// +optional
	Rebalancing RebalancingSpec `json:"rebalancing,omitempty"`
}

// Pod Status
type PodStatus struct {
	Id        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	NodeName  string `json:"nodeName,omitempty"`
	Status    string `json:"status,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Placement Status
type PlacementStatus struct {
	Strategy     string      `json:"strategy,omitempty"`
	ObservedPods int         `json:"observedPods,omitempty"`
	ReadyPods    int         `json:"readyPods,omitempty"`
	PendingPods  int         `json:"pendingPods,omitempty"`
	FailedPods   int         `json:"failedPods,omitempty"`
	PodStatuses  []PodStatus `json:"podStatuses,omitempty"`
}

// RebalancingStateType enumerates the states of the rebalance decision
// lifecycle state machine (see the rebalance engine design).
type RebalancingStateType string

const (
	RebalancingStateWatching   RebalancingStateType = "Watching"
	RebalancingStateTriggered  RebalancingStateType = "Triggered"
	RebalancingStateEvaluating RebalancingStateType = "Evaluating"
	RebalancingStateDecided    RebalancingStateType = "Decided"
	RebalancingStateEnacting   RebalancingStateType = "Enacting"
)

type RebalanceOutcome string

const (
	RebalanceOutcomeEnacted  RebalanceOutcome = "Enacted"
	RebalanceOutcomeNoOp     RebalanceOutcome = "NoOp"
	RebalanceOutcomeRejected RebalanceOutcome = "Rejected"
	RebalanceOutcomeDeferred RebalanceOutcome = "Deferred"
	RebalanceOutcomeFailed   RebalanceOutcome = "Failed"
)

type RebalanceAction string

const (
	RebalanceActionMove           RebalanceAction = "Move"
	RebalanceActionNoOp           RebalanceAction = "NoOp"
	RebalanceActionReject         RebalanceAction = "Reject"
	RebalanceActionDefer          RebalanceAction = "Defer"
	RebalanceActionAdjustReplicas RebalanceAction = "AdjustReplicas"
)

// RebalanceDecision is a single terminal-outcome record kept in the profile's
// rolling decision history.
type RebalanceDecision struct {
	// decisionId correlates this record with engine logs and Kubernetes Events.
	DecisionID string `json:"decisionId"`

	// outcome is the terminal outcome of this decision cycle.
	// +kubebuilder:validation:Enum=Enacted;NoOp;Rejected;Deferred;Failed
	Outcome RebalanceOutcome `json:"outcome,omitempty"`

	// action is the AI-returned action that was processed (e.g. "Move", "NoOp").
	// Dont use kubebuilder:validation:Enum here because the AI may return new actions in
	// the future, and we don't want to break the CRD schema for that reason.
	Action RebalanceAction `json:"action,omitempty"`

	// reason explains why the decision ended in this state.
	Reason string `json:"reason,omitempty"`

	// details carries free-form, state-specific context (e.g. target node,
	// improvement score, dry-run marker).
	Details string `json:"details,omitempty"`

	// startedAt is when this decision's cycle entered Triggered.
	StartedAt metav1.Time `json:"startedAt,omitempty"`

	// lastTransitionAt is when this decision reached its terminal state.
	LastTransitionAt metav1.Time `json:"lastTransitionAt,omitempty"`
}

// Rebalancing Status
type RebalancingStatus struct {
	// state is the current position of this workload's decision lifecycle
	// state machine. Empty when no rebalance cycle has ever been triggered.
	// +kubebuilder:validation:Enum=Watching;Triggered;Evaluating;Decided;Enacting
	// +optional
	State RebalancingStateType `json:"state,omitempty"`

	// reason explains why the current state was entered.
	// +optional
	Reason string `json:"reason,omitempty"`

	// decisionId correlates the current cycle with engine logs and Kubernetes Events.
	// +optional
	DecisionID string `json:"decisionId,omitempty"`

	// action is the AI-returned action being processed for the current cycle
	// (e.g. "Move", "NoOp"). Empty before the AI has responded.
	// +optional
	Action RebalanceAction `json:"action,omitempty"`

	// details carries free-form, state-specific context for the current cycle
	// (e.g. target node, improvement score, dry-run marker).
	// +optional
	Details string `json:"details,omitempty"`

	// startedAt is when the current cycle entered Triggered.
	// +optional
	StartedAt metav1.Time `json:"startedAt,omitempty"`

	// lastTransitionAt is when state last changed.
	// +optional
	LastTransitionAt metav1.Time `json:"lastTransitionAt,omitempty"`

	// cooldownUntil blocks new Triggered transitions for this workload until
	// this time has passed.
	// +optional
	CooldownUntil metav1.Time `json:"cooldownUntil,omitempty"`

	// recentDecisions is a rolling window of the most recent terminal
	// outcomes, newest first, trimmed to a bounded length (default 10).
	// +optional
	RecentDecisions []RebalanceDecision `json:"recentDecisions,omitempty"`
}

// OrchestrationProfileStatus defines the observed state of OrchestrationProfile.
type OrchestrationProfileStatus struct {
	// status indicates the status of the orchestration profile (e.g., "Pending", "Active", "Error", "InUse").
	Status string `json:"status,omitempty"`

	// reason provides additional details when the status is not "Active" (e.g., error messages).
	Reason string `json:"reason,omitempty"`

	// placementStatus provides details about the current placement of the application.
	// +optional
	PlacementStatus PlacementStatus `json:"placementStatus,omitempty"`

	// rebalancingStatus provides details about the current rebalancing state of the application.
	// +optional
	RebalancingStatus RebalancingStatus `json:"rebalancingStatus,omitempty"`
	LastUpdatedTime   metav1.Time       `json:"lastUpdatedTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=op

// OrchestrationProfile is the Schema for the orchestrationprofiles API
type OrchestrationProfile struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of OrchestrationProfile
	// +required
	Spec OrchestrationProfileSpec `json:"spec"`

	// status defines the observed state of OrchestrationProfile
	// +optional
	Status OrchestrationProfileStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// OrchestrationProfileList contains a list of OrchestrationProfile
type OrchestrationProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []OrchestrationProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OrchestrationProfile{}, &OrchestrationProfileList{})
}
