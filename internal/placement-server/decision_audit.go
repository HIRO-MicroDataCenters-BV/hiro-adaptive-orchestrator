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
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/audit"
)

// logInitialPlacementDecision records a successful initial-placement AI
// decision as a structured log line, using the same audit.DecisionRecord
// shape the rebalance engine uses for its own decisions (see
// internal/audit and internal/rebalance/writer.go).
//
// This is deliberately just a log line, not a CRD/status write or a metric:
// individual decision history is intended to live in whatever log
// aggregator this operator's stdout is shipped to, not in etcd or
// Prometheus — see the rebalance engine design notes on decision
// traceability for the reasoning.
func logInitialPlacementDecision(ctx context.Context, req *DecisionRequest, resp *DecisionResponse) {
	logger := logf.FromContext(ctx)

	record := audit.DecisionRecord{
		DecisionID:  resp.RequestID,
		Type:        audit.DecisionTypeInitialPlacement,
		Timestamp:   time.Now(),
		PodName:     req.Pod.Name,
		Namespace:   req.Pod.Namespace,
		ProfileName: req.AOProfile.ProfileName,
		TargetNode:  topScoreNodeName(resp.NodeScores),
		Reason:      resp.Reason,
	}

	logger.Info("audit: decision recorded", "record", record)
}

// topScoreNodeName returns the plain name of the highest-scored node, or ""
// if there are no scores. Unlike topNodeName in client.go (which formats a
// human-readable "name (score=X)" string for log messages), this returns
// the bare name for the DecisionRecord.TargetNode field.
func topScoreNodeName(scores []NodeScore) string {
	if len(scores) == 0 {
		return ""
	}
	top := scores[0]
	for _, s := range scores[1:] {
		if s.Score > top.Score {
			top = s
		}
	}
	return top.NodeName
}
