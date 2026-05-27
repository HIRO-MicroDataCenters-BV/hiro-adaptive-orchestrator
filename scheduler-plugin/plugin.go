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

package schedulerplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	fwk "k8s.io/kube-scheduler/framework"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// HIROScoreArgs holds the plugin configuration supplied via KubeSchedulerConfiguration
// pluginConfig. All fields are optional — unset fields fall back to the
// DefaultPlacementServer* constants defined in client.go.
//
// Example KubeSchedulerConfiguration snippet:
//
//	pluginConfig:
//	  - name: HIROScore
//	    args:
//	      placementServerURL: "http://my-svc.my-ns.svc.cluster.local:8090"
//	      placementServerPath: "/api/v1/placement/decision"
//	      timeoutSeconds: 8
type HIROScoreArgs struct {
	PlacementServerURL  string `json:"placementServerURL,omitempty"`
	PlacementServerPath string `json:"placementServerPath,omitempty"`
	TimeoutSeconds      int    `json:"timeoutSeconds,omitempty"`
}

// PluginName is the name registered with the scheduler framework.
// Referenced by KubeSchedulerConfiguration to enable the plugin.
const PluginName = "HIROScore"

// cycleStateKey is the CycleState storage key for per-pod AI scores.
const cycleStateKey fwk.StateKey = "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/HIROScore/nodeScores"

// nodeScoreMap maps node name → AI score (0–100).
// Implements fwk.StateData via Clone().
type nodeScoreMap map[string]int64

func (m nodeScoreMap) Clone() fwk.StateData {
	clone := make(nodeScoreMap, len(m))
	for k, v := range m {
		clone[k] = v
	}
	return clone
}

// HIROScore implements FilterPlugin, PreScorePlugin, and ScorePlugin.
//
// Filter:   blocks pods when OrchestrationProfile.status.decision.action == "Delay"
//           AND energy awareness is enabled. Soft-fails open on any client error.
// PreScore: calls PlacementServer, stashes AI NodeScores in CycleState.
// Score:    reads AI score from CycleState and returns it for this node.
type HIROScore struct {
	client *PlacementClient
}

// Enforce interface compliance at compile time.
var (
	_ fwk.FilterPlugin   = (*HIROScore)(nil)
	_ fwk.PreScorePlugin = (*HIROScore)(nil)
	_ fwk.ScorePlugin    = (*HIROScore)(nil)
)

// New is the factory function registered with the scheduler framework.
// Signature must match framework/runtime.PluginFactory:
//
//	func(ctx, runtime.Object, fwk.Handle) (fwk.Plugin, error)
//
// Args are read from the pluginConfig section of KubeSchedulerConfiguration.
// Any field left unset falls back to DefaultPlacementServer* constants.
func New(_ context.Context, obj apiruntime.Object, _ fwk.Handle) (fwk.Plugin, error) {
	args := &HIROScoreArgs{
		PlacementServerURL:  DefaultPlacementServerURL,
		PlacementServerPath: DefaultPlacementServerPath,
		TimeoutSeconds:      8,
	}

	if unknown, ok := obj.(*apiruntime.Unknown); ok && unknown != nil && len(unknown.Raw) > 0 {
		if err := json.Unmarshal(unknown.Raw, args); err != nil {
			return nil, fmt.Errorf("HIROScore: parsing pluginConfig args: %w", err)
		}
		// Re-apply defaults for any fields left as zero value after unmarshalling.
		if args.PlacementServerURL == "" {
			args.PlacementServerURL = DefaultPlacementServerURL
		}
		if args.PlacementServerPath == "" {
			args.PlacementServerPath = DefaultPlacementServerPath
		}
		if args.TimeoutSeconds <= 0 {
			args.TimeoutSeconds = 8
		}
	}

	client := NewPlacementClient(
		args.PlacementServerURL,
		args.PlacementServerPath,
		time.Duration(args.TimeoutSeconds)*time.Second,
	)
	return &HIROScore{client: client}, nil
}

// Name returns the plugin name used in KubeSchedulerConfiguration.
func (h *HIROScore) Name() string { return PluginName }

// =============================================================================
// FilterPlugin
// =============================================================================

// Filter is called once per (pod, node) pair after feasibility filters.
//
// It returns Unschedulable only when ALL of the following hold:
//   - The pod belongs to an OrchestrationProfile
//   - The profile's decision.action is "Delay"
//   - Energy awareness is enabled on the profile
//
// On any error (PlacementServer unreachable, profile not found, etc.) it
// returns nil (allow) — never block a pod because of a plugin failure.
func (h *HIROScore) Filter(
	_ context.Context,
	_ fwk.CycleState,
	_ *corev1.Pod,
	_ fwk.NodeInfo,
) *fwk.Status {
	// TODO: look up OrchestrationProfile for this pod.
	// TODO: if profile.status.decision.action == "Delay" && energy gate enabled
	//       return fwk.NewStatus(fwk.Unschedulable, "HIROScore: energy gate active")
	return nil // soft-fail open
}

// =============================================================================
// PreScorePlugin
// =============================================================================

// PreScore is called once per pod after all nodes have passed feasibility.
// It calls the PlacementServer, collects AI scores for all candidate nodes,
// and stashes them in CycleState for Score to read.
//
// On any error it stores an empty map so Score returns neutral 0 for all nodes.
func (h *HIROScore) PreScore(
	ctx context.Context,
	state fwk.CycleState,
	pod *corev1.Pod,
	nodes []fwk.NodeInfo,
) *fwk.Status {
	candidateNodes := make([]*corev1.Node, 0, len(nodes))
	for _, ni := range nodes {
		if ni.Node() != nil {
			candidateNodes = append(candidateNodes, ni.Node())
		}
	}

	placementCtx := placement.PlacementContext{
		Pod:            pod,
		CandidateNodes: candidateNodes,
	}
	resp, err := h.client.Decide(ctx, &placementCtx)
	if err != nil {
		state.Write(cycleStateKey, nodeScoreMap{})
		return nil
	}

	scores := make(nodeScoreMap, len(resp.NodeScores))
	for _, ns := range resp.NodeScores {
		s := int64(ns.Score)
		if s < fwk.MinNodeScore {
			s = fwk.MinNodeScore
		}
		if s > fwk.MaxNodeScore {
			s = fwk.MaxNodeScore
		}
		scores[ns.NodeName] = s
	}
	state.Write(cycleStateKey, scores)
	return nil
}

// =============================================================================
// ScorePlugin
// =============================================================================

// Score returns the AI score for the given node that was stashed in PreScore.
// Returns 0 if the node was not scored (soft-fail).
func (h *HIROScore) Score(
	_ context.Context,
	state fwk.CycleState,
	_ *corev1.Pod,
	nodeInfo fwk.NodeInfo,
) (int64, *fwk.Status) {
	nodeName := nodeInfo.Node().Name
	data, err := state.Read(cycleStateKey)
	if err != nil {
		return 0, nil
	}
	scores, ok := data.(nodeScoreMap)
	if !ok {
		return 0, fwk.AsStatus(fmt.Errorf("HIROScore: unexpected CycleState type %T", data))
	}
	return scores[nodeName], nil
}

// ScoreExtensions returns nil; HIROScore does not implement NormalizeScore.
func (h *HIROScore) ScoreExtensions() fwk.ScoreExtensions { return nil }
