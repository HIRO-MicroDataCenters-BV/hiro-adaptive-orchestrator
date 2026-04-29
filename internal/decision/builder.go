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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
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

	logger.Info("building decision request",
		"requestId", requestID,
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"candidateNodes", nodeNames,
	)

	// -------------------------------------------------------------------------
	// Step 1: Find the OrchestrationProfile governing this pod.
	// Use the app label from the pod to build the index key.
	// -------------------------------------------------------------------------
	profile, err := b.findProfileForPod(ctx, pod)
	if err != nil {
		return nil, fmt.Errorf("finding profile for pod %s/%s: %w",
			pod.Namespace, pod.Name, err)
	}
	if profile == nil {
		return nil, fmt.Errorf("no OrchestrationProfile found for pod %s/%s — "+
			"pod's application is not governed by any profile",
			pod.Namespace, pod.Name)
	}

	logger.Info("found governing profile for pod",
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
		if err != nil {
			// Log and continue — energy context is optional
			logger.Info("EAO profile unavailable, proceeding without energy data",
				"pod", pod.Name,
				"err", err,
			)
		}
	}

	// -------------------------------------------------------------------------
	// Step 4: Assemble the full DecisionRequest.
	// -------------------------------------------------------------------------
	req := &DecisionRequest{
		RequestID:      requestID,
		Timestamp:      metav1.Now(),
		Pod:            pod,
		CandidateNodes: nodes,
		AOProfile:      aoProfile,
		EAOProfile:     eaoProfile,
	}

	logger.Info("decision request assembled",
		"requestId", requestID,
		"pod", pod.Name,
		"profile", profile.Name,
		"strategy", aoProfile.Strategy,
		"candidateNodes", nodeNames,
		"currentPlacementNode", aoProfile.CurrentPlacement.NodeName,
		"energyEnabled", profile.Spec.Placement.Awareness.Energy,
		"eaoDataAttached", eaoProfile != nil,
	)

	return req, nil
}

// =============================================================================
// Profile lookup
// =============================================================================

// findProfileForPod finds the OrchestrationProfile governing the pod's
// application using the field index for O(1) lookup.
//
// It tries two strategies in order:
//  1. app label:              pod.Labels["app"]
//  2. app.kubernetes.io/name: pod.Labels["app.kubernetes.io/name"]
//
// Returns nil (no error) if no profile is found — caller decides how to handle.
func (b *DecisionContextBuilder) findProfileForPod(
	ctx context.Context,
	pod *corev1.Pod,
) (*orchestrationv1alpha1.OrchestrationProfile, error) {
	logger := logf.FromContext(ctx)
	appName, appNamespace, _ := utils.ResolveAppFromPod(ctx, b.client, pod)
	if appName == "" {
		// Pod has no recognized workload owner — not governed by any profile
		logger.V(1).Info("pod has no recognised workload owner, skipping",
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
		logger.Error(err, "index lookup for key", "key", indexKey)
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
	return &AOProfileContext{
		ProfileName: profile.Name,
		Strategy:    profile.Spec.Placement.Strategy,
		Awareness: AwarenessFlags{
			CPU:    profile.Spec.Placement.Awareness.CPU,
			Memory: profile.Spec.Placement.Awareness.Memory,
			GPU:    profile.Spec.Placement.Awareness.GPU,
			Energy: profile.Spec.Placement.Awareness.Energy,
		},
		CurrentPlacement: buildCurrentPlacement(pod),
		Rebalancing: RebalancingConfig{
			Enabled:           profile.Spec.Rebalancing.Enabled,
			CooldownSeconds:   profile.Spec.Rebalancing.CooldownSeconds,
			TriggerConditions: profile.Spec.Rebalancing.TriggerConditions,
		},
	}
}

// buildCurrentPlacement extracts the existing pod placements from the
// profile's PlacementStatus. The AI agent uses this to make spread/
// balance decisions for the new pod.
func buildCurrentPlacement(
	pod *corev1.Pod,
) PodPlacement {
	// TODO: extract current placement from pod annotations or status
	return PodPlacement{
		PodName:  pod.Name,
		NodeName: pod.Spec.NodeName, // empty if unscheduled
		Phase:    string(pod.Status.Phase),
	}
}

// =============================================================================
// EAOProfileContext fetcher
// =============================================================================

// fetchEAOProfile finds the EnergyAwareOrchestration CRD that governs the
// pod's application and maps its spec/status into an EAOProfileContext.
//
// Only called when profile.Awareness.Energy == true.
// Returns nil (no error) when no matching EAO is found.
func (b *DecisionContextBuilder) fetchEAOProfile(
	ctx context.Context,
	pod *corev1.Pod,
) (*EAOProfileContext, error) {
	logger := logf.FromContext(ctx)

	logger.Info("fetching EAO profile for pod",
		"pod", pod.Name,
		"namespace", pod.Namespace,
	)

	eao, err := b.fetchEAOForPod(ctx, pod)
	if err != nil {
		return nil, err
	}
	if eao == nil {
		return nil, nil
	}

	return mapEAOToProfileContext(eao), nil
}

// fetchEAOForPod resolves the pod's root application via ResolveAppFromPod,
// then lists all EnergyAwareOrchestration CRDs and returns the one whose
// spec.applicationRef matches that application.
//
// Returns nil (no error) when no matching EAO exists.
//
// +kubebuilder:rbac:groups=eas.hiro.io,resources=energyawareorchestrations,verbs=get;list;watch
func (b *DecisionContextBuilder) fetchEAOForPod(
	ctx context.Context,
	pod *corev1.Pod,
) (*unstructured.Unstructured, error) {
	logger := logf.FromContext(ctx)

	// Step 1: walk OwnerReferences to find the top-level workload name, namespace, and kind.
	appName, appNamespace, appKind := utils.ResolveAppFromPod(ctx, b.client, pod)
	if appName == "" {
		logger.V(1).Info("pod has no recognised workload owner, skipping EAO lookup",
			"pod", pod.Name,
			"namespace", pod.Namespace,
		)
		return nil, nil
	}

	// Step 2: list all EnergyAwareOrchestration resources across all namespaces.
	eaoList := &unstructured.UnstructuredList{}
	eaoList.SetGroupVersionKind(b.eaoGVK)
	if err := b.client.List(ctx, eaoList); err != nil {
		return nil, fmt.Errorf("listing EnergyAwareOrchestration resources: %w", err)
	}

	// Step 3: find the EAO whose spec.applicationRef matches the resolved app
	// by name, namespace, and kind.
	for i := range eaoList.Items {
		eao := &eaoList.Items[i]

		refName, _, _ := unstructured.NestedString(eao.Object, "spec", "applicationRef", "name")
		refKind, _, _ := unstructured.NestedString(eao.Object, "spec", "applicationRef", "kind")
		refNamespace, _, _ := unstructured.NestedString(eao.Object, "spec", "applicationRef", "namespace")
		if refNamespace == "" {
			refNamespace = eao.GetNamespace()
		}

		if refName == appName && refNamespace == appNamespace && refKind == appKind {
			logger.V(1).Info("found matching EAO for pod",
				"eao", eao.GetName(),
				"eaoNamespace", eao.GetNamespace(),
				"appName", appName,
				"appNamespace", appNamespace,
				"appKind", appKind,
			)
			return eao, nil
		}
	}

	logger.V(1).Info("no EAO found for pod application",
		"pod", pod.Name,
		"appName", appName,
		"appNamespace", appNamespace,
		"appKind", appKind,
	)
	return nil, nil
}

// mapEAOToProfileContext maps the relevant spec and status fields of an
// EnergyAwareOrchestration resource into an EAOProfileContext.
func mapEAOToProfileContext(eao *unstructured.Unstructured) *EAOProfileContext {
	ctx := &EAOProfileContext{}

	// --- spec fields ---
	ctx.Priority, _, _ = unstructured.NestedString(eao.Object, "spec", "priority")

	if watts, found, _ := unstructured.NestedInt64(eao.Object, "spec", "energyConsumption"); found {
		ctx.EnergyConsumptionWatts = watts
	}

	// --- status.decision ---
	action, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "action")
	reason, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "reason")
	nextEval, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "nextEvaluationTime")
	if action != "" {
		ctx.Decision = &EAODecision{
			Action:             action,
			Reason:             reason,
			NextEvaluationTime: nextEval,
		}
	}

	// --- status.energyMetrics ---
	requiredWatts, foundRequired, _ := unstructured.NestedFloat64(eao.Object, "status", "energyMetrics", "requiredWatts")
	sufficient, foundSufficient, _ := unstructured.NestedBool(eao.Object, "status", "energyMetrics", "sufficient")
	if foundRequired || foundSufficient {
		ctx.EnergyMetrics = &EAOEnergyMetrics{
			RequiredWatts: requiredWatts,
			Sufficient:    sufficient,
		}
		ctx.EnergyMetrics.CurrentSlotAvailableWatts, _, _ = unstructured.NestedFloat64(
			eao.Object, "status", "energyMetrics", "currentSlotAvailableWatts",
		)
		ctx.EnergyMetrics.CurrentSlotConsumedWatts, _, _ = unstructured.NestedFloat64(
			eao.Object, "status", "energyMetrics", "currentSlotConsumedWatts",
		)
	}

	return ctx
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
