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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/metrics"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// =============================================================================
// DecisionContextBuilder
//
// Triggered by: kube-scheduler custom scoring plugin (step 4)
// Input:        PlacementContext  (pod + candidate nodes, sent by scheduler)
// Output:       DecisionRequest   (pod + nodes + AO profile + EAO profile)
//
// Assembly flow per pod:
//   1. Receive PlacementContext from kube-scheduler
//   2. Find the OrchestrationProfile governing this pod
//      (via ProfileByAppRefIndex — O(1) lookup)
//   3. Build AOProfileContext from the profile + its current placement status
//   4. Optionally fetch EAOProfileContext from the E.A.O CRD
//      (only when profile.Awareness.Energy == true)
//   5. Return the assembled DecisionRequest for the DecisionClient to send
// =============================================================================

// DecisionContextBuilder assembles a per-pod DecisionRequest from cluster state.
// It is called once per unscheduled pod by the kube-scheduler scoring plugin.
type DecisionContextBuilder struct {
	// client reads from the controller-runtime informer cache.
	// All profile reads are O(1) cache hits — no direct API server calls.
	client client.Client

	// profileIndexField is the field index key used to look up profiles by app.
	// Passed in so the builder does not depend on the controller package directly.
	profileIndexField string

	// eaoGVK is the GroupVersionKind used to list EnergyAwareOrchestration resources.
	// Configurable via EAO_GROUP / EAO_VERSION / EAO_KIND environment variables.
	eaoGVK schema.GroupVersionKind

	// eaoVerdict maps the EAO's decision.action values onto EAOVerdict.
	eaoVerdict EAOVerdictConfig
}

// WithEAOVerdict replaces the default EAO action mapping.
func (b *DecisionContextBuilder) WithEAOVerdict(cfg EAOVerdictConfig) *DecisionContextBuilder {
	b.eaoVerdict = cfg
	return b
}

// NewDecisionContextBuilder creates a new builder.
//
// client:             the same client.Client the reconciler uses (informer cache)
// profileIndexField:  the ProfileByAppRefIndex constant from op_index.go
// eaoGVK:             GVK for EnergyAwareOrchestration list queries
func NewDecisionContextBuilder(
	c client.Client,
	profileIndexField string,
	eaoGVK schema.GroupVersionKind,
) *DecisionContextBuilder {
	return &DecisionContextBuilder{
		client:            c,
		profileIndexField: profileIndexField,
		eaoGVK:            eaoGVK,
		eaoVerdict:        DefaultEAOVerdictConfig(),
	}
}

// Build assembles a DecisionRequest for a single unscheduled pod.
//
// Called by the kube-scheduler custom scoring plugin when it receives an
// unscheduled pod (step 4 → step 4.1 in the architecture).
//
// Steps:
//  1. Find the OrchestrationProfile governing this pod's application
//  2. Build the AOProfileContext (strategy + awareness + current placement)
//  3. Optionally fetch EAOProfileContext (energy data per node)
//  4. Return the complete DecisionRequest
func (b *DecisionContextBuilder) Build(
	ctx context.Context,
	placementCtx PlacementContext,
	requestID string,
) (*DecisionRequest, error) {
	logger := logf.FromContext(ctx)

	pod := placementCtx.Pod
	nodes := placementCtx.CandidateNodes
	nodeNames := utils.NodeNames(nodes)

	logger.Info("builder: building request",
		"requestId", requestID,
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"candidateNodes", nodeNames,
	)

	// -------------------------------------------------------------------------
	// Step 1: Find the OrchestrationProfile governing this pod.
	// Use the app label from the pod to build the index key.
	// -------------------------------------------------------------------------
	profile, err := b.FindProfileForPod(ctx, pod)
	if err != nil {
		metrics.PlacementContextBuildTotal.WithLabelValues(metrics.BuildResultProfileError).Inc()
		return nil, fmt.Errorf("finding profile for pod %s/%s: %w",
			pod.Namespace, pod.Name, err)
	}
	if profile == nil {
		metrics.PlacementContextBuildTotal.WithLabelValues(metrics.BuildResultProfileNotFound).Inc()
		return nil, fmt.Errorf("no OrchestrationProfile found for pod %s/%s — "+
			"pod's application is not governed by any profile",
			pod.Namespace, pod.Name)
	}

	logger.Info("builder: profile found for pod",
		"pod", pod.Name,
		"profile", profile.Name,
		"strategy", profile.Spec.Placement.Strategy,
	)

	// -------------------------------------------------------------------------
	// Step 2: Build AOProfileContext.
	// Includes strategy, awareness flags, and current placement snapshot.
	// -------------------------------------------------------------------------
	aoProfile := buildAOProfileContext(profile, pod)

	// -------------------------------------------------------------------------
	// Step 3: Optionally fetch EAOProfileContext.
	// Only when energy awareness is enabled in the profile.
	// Energy data is best-effort — a fetch failure does not fail the request.
	// -------------------------------------------------------------------------
	var eaoProfile *EAOProfileContext
	if profile.Spec.Placement.Awareness.Energy {
		eaoProfile, err = b.fetchEAOProfile(ctx, pod)
		observeEAOFetch(metrics.EAOCallerScore, eaoProfile, err)
		if err != nil {
			// Log and continue — energy context is optional
			logger.Info("builder: EAO unavailable, skipping energy data",
				"pod", pod.Name,
				"err", err,
			)
		}
	}

	nodeStats := b.buildNodeStats(ctx, nodes, profile.Spec.ApplicationRef, pod, true)

	// -------------------------------------------------------------------------
	// Step 4: Assemble the full DecisionRequest.
	// -------------------------------------------------------------------------
	req := &DecisionRequest{
		RequestID:  requestID,
		Timestamp:  metav1.Now(),
		Pod:        pod,
		AOProfile:  aoProfile,
		EAOProfile: eaoProfile,
		NodeStats:  nodeStats,
	}

	logValues := []any{
		"requestId", requestID,
		"pod", pod.Name,
		"profile", profile.Name,
		"aoStrategy", aoProfile.Strategy,
		"aoAwareness", aoProfile.Awareness,
		"rebalancingEnabled", aoProfile.Rebalancing.Enabled,
		"candidateNodes", nodeNames,
		"currentPlacementNode", aoProfile.CurrentPlacement.NodeName,
		"energyEnabled", profile.Spec.Placement.Awareness.Energy,
		"eaoDataAttached", eaoProfile != nil,
		"nodeStats", len(nodeStats),
	}
	// nil when energy awareness is off or no EAO object exists for the pod.
	if eaoProfile != nil {
		logValues = append(logValues,
			"eaoPriority", eaoProfile.Priority,
			"eaoEnergyConsumptionWatts", eaoProfile.EnergyConsumptionWatts,
		)
		// nil until the EnergyAwareOrchestrator has populated the EAO's status.
		if eaoProfile.Decision != nil {
			logValues = append(logValues,
				"eaoAction", eaoProfile.Decision.Action,
				"eaoReason", eaoProfile.Decision.Reason,
			)
		}
	}
	logger.Info("builder: request assembled", logValues...)

	metrics.PlacementContextBuildTotal.WithLabelValues(metrics.BuildResultSuccess).Inc()
	return req, nil
}

// =============================================================================
// Profile lookup
// =============================================================================

// FindProfileForPod finds the OrchestrationProfile governing the pod's
// application using the field index for O(1) lookup.
//
// Returns nil (no error) if no profile is found — caller decides how to handle.
// Used by the placement server and the pod scheduler MutatingAdmissionWebhook.
func (b *DecisionContextBuilder) FindProfileForPod(
	ctx context.Context,
	pod *corev1.Pod,
) (*orchestrationv1alpha1.OrchestrationProfile, error) {
	logger := logf.FromContext(ctx)
	appName, appNamespace, _ := utils.ResolveAppFromPod(ctx, b.client, pod)
	if appName == "" {
		// Pod has no recognized workload owner — not governed by any profile
		logger.V(1).Info("builder: no workload owner, skipping",
			"pod", pod.Name,
			"namespace", pod.Namespace,
		)
		return nil, nil
	}

	// Step 2: O(1) index lookup — find profiles referencing this app
	indexKey := appNamespace + "/" + appName
	profileList := &orchestrationv1alpha1.OrchestrationProfileList{}
	if err := b.client.List(ctx, profileList,
		client.MatchingFields{b.profileIndexField: indexKey},
	); err != nil {
		logger.Error(err, "builder: profile index lookup failed", "key", indexKey)
		return nil, fmt.Errorf("index lookup for key %q: %w", indexKey, err)
	}

	if len(profileList.Items) > 0 {
		// Return the first match. Multiple profiles per app is an edge case
		// that should be caught by admission validation.
		return &profileList.Items[0], nil
	}

	return nil, nil // no profile found — pod is not governed
}

// =============================================================================
// AOProfileContext builder
// =============================================================================

// buildAOProfileContext assembles the AOProfileContext from the profile's
// spec and its current status (PlacementStatus).
func buildAOProfileContext(
	profile *orchestrationv1alpha1.OrchestrationProfile,
	pod *corev1.Pod,
) *AOProfileContext {
	triggerConditions := make([]string, len(profile.Spec.Rebalancing.TriggerConditions))
	for i, c := range profile.Spec.Rebalancing.TriggerConditions {
		triggerConditions[i] = string(c)
	}

	return &AOProfileContext{
		ProfileName: profile.Name,
		Strategy:    string(profile.Spec.Placement.Strategy),
		Awareness: AwarenessFlags{
			CPU:    profile.Spec.Placement.Awareness.CPU,
			Memory: profile.Spec.Placement.Awareness.Memory,
			GPU:    profile.Spec.Placement.Awareness.GPU,
			Energy: profile.Spec.Placement.Awareness.Energy,
		},
		CurrentPlacement: buildCurrentPlacement(pod),
		Rebalancing: RebalancingConfig{
			Enabled:                    profile.Spec.Rebalancing.Enabled,
			CooldownSeconds:            profile.Spec.Rebalancing.CooldownSeconds,
			TriggerConditions:          triggerConditions,
			ReplicasExternallyManaged:  profile.Spec.Rebalancing.ReplicasExternallyManaged,
			ResourcesExternallyManaged: profile.Spec.Rebalancing.ResourcesExternallyManaged,
		},
	}
}

// buildCurrentPlacement extracts the existing pod placements from the
// profile's PlacementStatus. The AI agent uses this to make spread/
// balance decisions for the new pod.
//
// pod is nil for rebalance requests (BuildRebalanceContext) — there's no
// single "the pod" being placed; the full current layout goes in
// RebalanceContext.CurrentPlacements instead, and this field is left at its
// zero value.
func buildCurrentPlacement(
	pod *corev1.Pod,
) PodPlacement {
	if pod == nil {
		return PodPlacement{}
	}
	// TODO: extract current placement from pod annotations or status
	return PodPlacement{
		PodName:  pod.Name,
		NodeName: pod.Spec.NodeName, // empty if unscheduled
		Phase:    string(pod.Status.Phase),
	}
}

// =============================================================================
// Rebalance context builder
//
// Triggered by: the rebalance engine's Reconciler, on entering Evaluating
// (internal/rebalance) — not by the kube-scheduler.
// Input:        an OrchestrationProfile whose workload is being
//               reconsidered, plus the trigger reason and decision history.
// Output:       DecisionRequest with RebalanceContext populated, Pod nil.
//
// Unlike Build (one unscheduled pod, scheduler-proposed candidates), this
// assembles context for a whole already-placed workload: every pod's
// current node (the AI needs the full layout to judge imbalance and pick
// both which pod to move and where) and every Ready cluster node as a
// candidate (there's no scheduling cycle proposing a filtered subset).
// =============================================================================

// RebalanceFacts are rebalance inputs the engine already resolved (it owns
// trigger evaluation and HPA/KEDA/PDB knowledge); each pointer may be nil.
type RebalanceFacts struct {
	Trigger  *RebalanceTrigger
	Workload *RebalanceWorkload
	PDB      *RebalancePDB
	State    RebalanceState
}

// BuildRebalanceContext assembles a DecisionRequest for the rebalance
// engine's Evaluating stage. Reuses the same AOProfileContext/EAOProfileContext
// assembly as Build so the AI sees the same profile shape either way — only
// RebalanceContext and the current-placement snapshot differ.
func (b *DecisionContextBuilder) BuildRebalanceContext(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	decisionID string,
	reason string,
	facts RebalanceFacts,
) (*DecisionRequest, error) {
	logger := logf.FromContext(ctx)

	pods, err := utils.FindPodsForApplication(ctx, b.client, profile.Spec.ApplicationRef)
	if err != nil {
		return nil, fmt.Errorf("finding pods for rebalance context: %w", err)
	}

	nodes, err := b.listReadyNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing candidate nodes for rebalance context: %w", err)
	}
	nodes = filterTolerated(ctx, nodes, pods)

	aoProfile := buildAOProfileContext(profile, nil)

	var eaoProfile *EAOProfileContext
	if profile.Spec.Placement.Awareness.Energy {
		eaoProfile, err = b.fetchEAOProfileForApp(ctx, profile.Spec.ApplicationRef)
		observeEAOFetch(metrics.EAOCallerRebalance, eaoProfile, err)
		if err != nil {
			// Energy context is optional — log and continue, matching Build's posture.
			logger.Info("builder: EAO unavailable for rebalance context, skipping",
				"profile", profile.Name, "err", err)
		}
	}

	nodeStats := b.buildNodeStats(ctx, nodes, profile.Spec.ApplicationRef, nil, true)
	nodeStats = append(nodeStats, b.nonCandidateNodeStats(ctx, pods, nodes, profile.Spec.ApplicationRef)...)

	req := &DecisionRequest{
		RequestID:  decisionID,
		Timestamp:  metav1.Now(),
		AOProfile:  aoProfile,
		EAOProfile: eaoProfile,
		NodeStats:  nodeStats,
		RebalanceContext: &RebalanceContext{
			Reason:            reason,
			DecisionID:        decisionID,
			CurrentPlacements: buildCurrentPlacements(pods),
			RebalanceState:    facts.State,
			Trigger:           facts.Trigger,
			Workload:          facts.Workload,
			PDB:               facts.PDB,
		},
	}

	logger.Info("builder: rebalance context assembled",
		"profile", profile.Name,
		"decisionId", decisionID,
		"reason", reason,
		"podCount", len(pods),
		"candidateNodes", len(nodes),
		"nodeStats", len(nodeStats),
		"energyDataAttached", eaoProfile != nil,
		"workloadAttached", facts.Workload != nil,
		"pdbAttached", facts.PDB != nil,
	)

	return req, nil
}

// listReadyNodes lists every cluster node reporting NodeReady == True — the
// candidate set for a rebalance Move, since there's no scheduling cycle
// proposing a pre-filtered subset the way there is for initial placement.
func (b *DecisionContextBuilder) listReadyNodes(ctx context.Context) ([]*corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := b.client.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	ready := make([]*corev1.Node, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready = append(ready, node)
				break
			}
		}
	}
	return ready, nil
}

// filterTolerated drops nodes with a NoSchedule/NoExecute taint the app's pods don't tolerate,
// mirroring the scheduler's TaintToleration filter that rebalance candidates otherwise skip.
func filterTolerated(ctx context.Context, nodes []*corev1.Node, pods []corev1.Pod) []*corev1.Node {
	var tolerations []corev1.Toleration
	if len(pods) > 0 {
		tolerations = pods[0].Spec.Tolerations
	}
	kept := make([]*corev1.Node, 0, len(nodes))
	for _, node := range nodes {
		if toleratesAll(ctx, node.Spec.Taints, tolerations) {
			kept = append(kept, node)
		}
	}
	return kept
}

func toleratesAll(ctx context.Context, taints []corev1.Taint, tolerations []corev1.Toleration) bool {
	logger := logf.FromContext(ctx)
	for i := range taints {
		taint := &taints[i]
		if taint.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		tolerated := false
		for j := range tolerations {
			// false: the Gt/Lt operators are behind an alpha gate that's off by default.
			if tolerations[j].ToleratesTaint(logger, taint, false) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

// buildCurrentPlacements converts every pod of the application into a
// PodPlacement — the full layout snapshot a rebalance decision needs,
// unlike buildCurrentPlacement's single-pod use for initial placement.
func buildCurrentPlacements(pods []corev1.Pod) []PodPlacement {
	placements := make([]PodPlacement, 0, len(pods))
	for i := range pods {
		placements = append(placements, PodPlacement{
			PodName:    pods[i].Name,
			NodeName:   pods[i].Spec.NodeName,
			Phase:      string(pods[i].Status.Phase),
			Containers: buildContainerInfo(pods[i].Spec.Containers),
		})
	}
	return placements
}

// buildContainerInfo maps a pod's containers into the wire-format shown to
// the AI — see ContainerInfo's doc comment on why (AdjustResources needs to
// name a specific container and see its current values).
func buildContainerInfo(containers []corev1.Container) []ContainerInfo {
	if len(containers) == 0 {
		return nil
	}
	infos := make([]ContainerInfo, 0, len(containers))
	for _, c := range containers {
		infos = append(infos, ContainerInfo{
			Name:          c.Name,
			CPURequest:    quantityString(c.Resources.Requests, corev1.ResourceCPU),
			MemoryRequest: quantityString(c.Resources.Requests, corev1.ResourceMemory),
			CPULimit:      quantityString(c.Resources.Limits, corev1.ResourceCPU),
			MemoryLimit:   quantityString(c.Resources.Limits, corev1.ResourceMemory),
		})
	}
	return infos
}

// quantityString returns list[name]'s string form, or "" if list is nil or
// doesn't have that resource — a container commonly leaves CPU or Memory (or
// both) unset.
func quantityString(list corev1.ResourceList, name corev1.ResourceName) string {
	q, ok := list[name]
	if !ok {
		return ""
	}
	return q.String()
}

// =============================================================================
// PlacementContext builder helper
//
// Used by the kube-scheduler scoring plugin to construct the PlacementContext
// from the raw Kubernetes objects before calling Build().
// =============================================================================

// BuildPlacementContext constructs the PlacementContext from the raw
// Kubernetes pod and node objects provided by the scheduler.
//
// Called by the kube-scheduler custom scoring plugin before calling Build().
func BuildPlacementContext(pod *corev1.Pod, nodes []*corev1.Node) PlacementContext {
	return PlacementContext{
		Pod:            pod,
		CandidateNodes: nodes,
	}
}

// // podDetailFromK8s converts a Kubernetes Pod object to a PodDetail.
// func podDetailFromK8s(pod *corev1.Pod) PodDetail {
// 	detail := PodDetail{
// 		Name:      pod.Name,
// 		Namespace: pod.Namespace,
// 		UID:       string(pod.UID),
// 	}

// 	// Extract resource requests (use first container as representative)
// 	for _, c := range pod.Spec.Containers {
// 		if cpu := c.Resources.Requests.Cpu(); cpu != nil {
// 			detail.ResourceRequests.CPU = cpu.String()
// 		}
// 		if mem := c.Resources.Requests.Memory(); mem != nil {
// 			detail.ResourceRequests.Memory = mem.String()
// 		}
// 		break
// 	}

// 	return detail
// }

// // nodeDetailsFromK8s converts a list of Kubernetes Node objects to NodeDetails.
// func nodeDetailsFromK8s(nodes []*corev1.Node) []NodeDetail {
// 	details := make([]NodeDetail, 0, len(nodes))
// 	for _, node := range nodes {
// 		nd := NodeDetail{
// 			Name: node.Name,
// 		}

// 		// Total capacity
// 		if cpu := node.Status.Capacity.Cpu(); cpu != nil {
// 			nd.TotalResources.CPU = cpu.String()
// 		}
// 		if mem := node.Status.Capacity.Memory(); mem != nil {
// 			nd.TotalResources.Memory = mem.String()
// 		}

// 		// Allocatable (remaining after system/existing pods)
// 		if cpu := node.Status.Allocatable.Cpu(); cpu != nil {
// 			nd.AllocatableResources.CPU = cpu.String()
// 		}
// 		if mem := node.Status.Allocatable.Memory(); mem != nil {
// 			nd.AllocatableResources.Memory = mem.String()
// 		}

// 		details = append(details, nd)
// 	}
// 	return details
// }

// // nodeNameSet returns a set of node names for fast lookup.
// func nodeNameSet(nodes []NodeDetail) map[string]bool {
// 	m := make(map[string]bool, len(nodes))
// 	for _, n := range nodes {
// 		m[n.Name] = true
// 	}
// 	return m
// }

// // Ensure corev1 and types imports are used.
// var _ = types.NamespacedName{}
// var _ *corev1.Pod
