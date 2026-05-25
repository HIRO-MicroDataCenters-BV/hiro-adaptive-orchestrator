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

import corev1 "k8s.io/api/core/v1"

// =============================================================================
// Kubernetes Scheduler Extender protocol types
//
// The default kube-scheduler calls the extender over HTTP when scheduling pods.
// Two endpoints are served by PlacementServer:
//
//   POST /extender/filter      -- energy gating (removes nodes when EAO says
//                                 energy is insufficient for this pod)
//   POST /extender/prioritize  -- AI scoring (maps DecisionResponse.NodeScores
//                                 onto the 0-10 integer scale the scheduler expects)
//
// Both endpoints receive ExtenderArgs and return either ExtenderFilterResult
// or HostPriorityList, matching the types defined in
// k8s.io/kube-scheduler/extender/v1 (reproduced here to avoid a heavy import).
// =============================================================================

// ExtenderArgs is the payload the Kubernetes scheduler sends to a custom
// extender's /filter and /prioritize endpoints.
//
// When nodeCacheCapable is false in KubeSchedulerConfiguration (our default),
// the scheduler always populates Nodes; NodeNames is nil.
type ExtenderArgs struct {
	Pod       *corev1.Pod      `json:"pod"`
	Nodes     *corev1.NodeList `json:"nodes,omitempty"`
	NodeNames *[]string        `json:"nodenames,omitempty"`
}

// ExtenderFilterResult is what the /filter endpoint returns.
// Nodes that may be scheduled on are in Nodes; nodes that were filtered out
// are in FailedNodes with the reason the scheduler will surface to the user.
type ExtenderFilterResult struct {
	Nodes       *corev1.NodeList  `json:"nodes,omitempty"`
	NodeNames   *[]string         `json:"nodenames,omitempty"`
	FailedNodes map[string]string `json:"failedNodes,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// HostPriority is the extender's score for a single node.
// Score must be in [0, 10]; higher means more preferred by the scheduler.
type HostPriority struct {
	Host  string `json:"host"`
	Score int64  `json:"score"`
}

// HostPriorityList is what the /prioritize endpoint returns.
// The scheduler merges these scores (weighted by extender.weight) with its own.
type HostPriorityList []HostPriority

// EnergyGateResult carries the outcome of a CheckEnergyGate call.
type EnergyGateResult struct {
	// Allowed is true when the pod may be scheduled normally.
	Allowed bool
	// Reason is a human-readable explanation when Allowed is false.
	Reason string
}
