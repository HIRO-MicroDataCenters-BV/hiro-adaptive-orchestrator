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
	Status    string `json:"status,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Placement Status
type PlacementStatus struct {
	Strategy     string      `json:"strategy,omitempty"`
	ObservedPods int         `json:"observedPods,omitempty"`
	ReadyPods    int         `json:"readyPods,omitempty"`
	PendingPods  int         `json:"pendingPods,omitempty"`
	PodStatuses  []PodStatus `json:"podStatuses,omitempty"`
}

// Rebalancing Status
type RebalancingStatus struct {
	LastTriggeredAt string `json:"lastTriggeredAt,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// OrchestrationProfileStatus defines the observed state of OrchestrationProfile.
type OrchestrationProfileStatus struct {

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
