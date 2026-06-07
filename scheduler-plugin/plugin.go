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
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	fwk "k8s.io/kube-scheduler/framework"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// HIROScoreArgs holds the plugin configuration supplied via KubeSchedulerConfiguration
// pluginConfig. All fields are optional — unset fields fall back to the
// Default* constants defined in client.go.
//
// Example KubeSchedulerConfiguration snippet:
//
//	pluginConfig:
//	  - name: HIROScore
//	    args:
//	      placementServerURL: "http://my-svc.my-ns.svc.cluster.local:8090"
//	      placementServerPath: "/api/v1/placement/score"
//	      filterPath: "/api/v1/placement/filter"
//	      timeoutSeconds: 8
type HIROScoreArgs struct {
	PlacementServerURL  string `json:"placementServerURL,omitempty"`
	PlacementServerPath string `json:"placementServerPath,omitempty"`
	FilterPath          string `json:"filterPath,omitempty"`
	TimeoutSeconds      int    `json:"timeoutSeconds,omitempty"`
}

// PluginName is the name registered with the scheduler framework.
const PluginName = "HIROScore"

// CycleState keys
const (
	// cycleStateKey stores per-pod AI node scores (set in PreScore, read in Score).
	cycleStateKey fwk.StateKey = "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/HIROScore/nodeScores"

	// energyGateStateKey stores the energy gate decision (set in PreFilter, read in Filter).
	energyGateStateKey fwk.StateKey = "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/HIROScore/energyGate"
)

// nodeScoreMap maps node name → AI score (0–100).
type nodeScoreMap map[string]int64

func (m nodeScoreMap) Clone() fwk.StateData {
	clone := make(nodeScoreMap, len(m))
	maps.Copy(clone, m)
	return clone
}

// energyGateDecision holds the outcome of the PreFilter energy gate check.
type energyGateDecision struct {
	allowed bool
	reason  string
}

func (d energyGateDecision) Clone() fwk.StateData { return d }

// HIROScore implements PreFilterPlugin, FilterPlugin, PreScorePlugin, and ScorePlugin.
//
// PreFilter: calls PlacementServer /filter once per pod — checks energy gate.
// Filter:    reads energy gate result from CycleState — returns Unschedulable if blocked.
// PreScore:  calls PlacementServer /score — collects AI NodeScores for all candidate nodes.
// Score:     reads AI score from CycleState for the given node.
type HIROScore struct {
	client *PlacementClient
}

// Enforce interface compliance at compile time.
var (
	_ fwk.PreFilterPlugin = (*HIROScore)(nil)
	_ fwk.FilterPlugin    = (*HIROScore)(nil)
	_ fwk.PreScorePlugin  = (*HIROScore)(nil)
	_ fwk.ScorePlugin     = (*HIROScore)(nil)
)

// New is the factory function registered with the scheduler framework.
func New(_ context.Context, obj apiruntime.Object, _ fwk.Handle) (fwk.Plugin, error) {
	args := &HIROScoreArgs{
		PlacementServerURL:  DefaultPlacementServerURL,
		PlacementServerPath: DefaultPlacementServerPath,
		FilterPath:          DefaultFilterPath,
		TimeoutSeconds:      8,
	}

	if unknown, ok := obj.(*apiruntime.Unknown); ok && unknown != nil && len(unknown.Raw) > 0 {
		if err := json.Unmarshal(unknown.Raw, args); err != nil {
			return nil, fmt.Errorf("HIROScore: parsing pluginConfig args: %w", err)
		}
		if args.PlacementServerURL == "" {
			args.PlacementServerURL = DefaultPlacementServerURL
		}
		if args.PlacementServerPath == "" {
			args.PlacementServerPath = DefaultPlacementServerPath
		}
		if args.FilterPath == "" {
			args.FilterPath = DefaultFilterPath
		}
		if args.TimeoutSeconds <= 0 {
			args.TimeoutSeconds = 8
		}
	}

	client := NewPlacementClient(
		args.PlacementServerURL,
		args.PlacementServerPath,
		args.FilterPath,
		time.Duration(args.TimeoutSeconds)*time.Second,
	)
	return &HIROScore{client: client}, nil
}

// Name returns the plugin name used in KubeSchedulerConfiguration.
func (h *HIROScore) Name() string { return PluginName }

// =============================================================================
// PreFilterPlugin — energy gate (once per pod, before Filter)
// =============================================================================

// PreFilter calls the PlacementServer's /filter endpoint once per pod.
// The result is stored in CycleState for Filter to read per node.
// Soft-fail open: any error is treated as Allowed=true.
func (h *HIROScore) PreFilter(
	ctx context.Context,
	state fwk.CycleState,
	pod *corev1.Pod,
	_ []fwk.NodeInfo,
) (*fwk.PreFilterResult, *fwk.Status) {
	allowed, reason, err := h.client.CheckFilter(ctx, pod)
	if err != nil {
		// Infrastructure failure — never block a pod; let the scheduler proceed.
		state.Write(energyGateStateKey, energyGateDecision{allowed: true})
		return nil, nil
	}
	state.Write(energyGateStateKey, energyGateDecision{allowed: allowed, reason: reason})
	return nil, nil
}

// PreFilterExtensions returns nil; HIROScore does not add/remove nodes in PreFilter.
func (h *HIROScore) PreFilterExtensions() fwk.PreFilterExtensions { return nil }

// =============================================================================
// FilterPlugin — energy gate (per pod/node pair)
// =============================================================================

// Filter reads the energy gate decision stored by PreFilter.
// Returns Unschedulable for every node when the energy gate blocked the pod.
// Soft-fail open: if the CycleState key is missing, the pod is allowed.
func (h *HIROScore) Filter(
	_ context.Context,
	state fwk.CycleState,
	_ *corev1.Pod,
	_ fwk.NodeInfo,
) *fwk.Status {
	data, err := state.Read(energyGateStateKey)
	if err != nil {
		return nil // gate not checked — allow
	}
	gate, ok := data.(energyGateDecision)
	if !ok || gate.allowed {
		return nil
	}
	return fwk.NewStatus(fwk.Unschedulable, "HIROScore: "+gate.reason)
}

// =============================================================================
// PreScorePlugin — AI scoring (once per pod, after Filter)
// =============================================================================

// PreScore calls the PlacementServer's /score endpoint, collects AI scores for
// all candidate nodes, and stashes them in CycleState for Score to read.
// Soft-fail open: on any error an empty map is stored so Score returns 0.
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
		scores[ns.NodeName] = max(fwk.MinNodeScore, min(fwk.MaxNodeScore, int64(ns.Score)))
	}
	state.Write(cycleStateKey, scores)
	return nil
}

// =============================================================================
// ScorePlugin — returns per-node AI score (per pod/node pair)
// =============================================================================

// Score returns the AI score for the given node that was stashed in PreScore.
// Returns 0 (neutral) if the node was not scored — soft-fail.
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
