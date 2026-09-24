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

// Package audit holds types shared between the initial-placement path
// (internal/placement-server) and the rebalance engine (internal/rebalance)
// for recording AI decisions.
//
// DecisionRecord is seeded now but not yet wired into either flow's logging.
// It exists so a future structured-log-based audit trail (see the rebalance
// engine design notes on decision traceability) has a stable shape to adopt
// without a schema change at that point. It is intentionally NOT persisted
// as a Kubernetes object — see the design rationale against using etcd as an
// event/log store.
package audit

import "time"

// Decision type tags for DecisionRecord.Type.
const (
	DecisionTypeInitialPlacement = "InitialPlacement"
	DecisionTypeRebalance        = "Rebalance"
)

// DecisionRecord is the common shape for a single AI placement decision,
// whether it came from initial scheduling or the rebalance engine.
type DecisionRecord struct {
	DecisionID  string    `json:"decisionId"`
	Type        string    `json:"type"` // DecisionTypeInitialPlacement | DecisionTypeRebalance
	Timestamp   time.Time `json:"timestamp"`
	PodName     string    `json:"podName"`
	Namespace   string    `json:"namespace"`
	ProfileName string    `json:"profileName"`
	SourceNode  string    `json:"sourceNode,omitempty"` // empty for initial placement
	TargetNode  string    `json:"targetNode"`
	Reason      string    `json:"reason,omitempty"`
	Improvement float64   `json:"improvement,omitempty"` // rebalance only
}
