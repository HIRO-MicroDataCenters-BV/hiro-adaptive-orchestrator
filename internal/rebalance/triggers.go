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

package rebalance

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// Trigger condition names — must match validTriggerConditions in
// internal/controller/op_constants.go.
const (
	TriggerEnergyThreshold = "EnergyThreshold"
	TriggerCPUThreshold    = "CPUThreshold"
	TriggerMemoryThreshold = "MemoryThreshold"
	TriggerNodeFailure     = "NodeFailure"
	TriggerScheduled       = "Scheduled"
)

// TriggerEvaluator evaluates an OrchestrationProfile's declared rebalancing
// trigger conditions against current cluster state.
//
// Used identically by both halves of the hybrid detection design (see
// reconciler.go): a periodic tick and an event-driven wake-up both call
// Evaluate the same way — the watches only decide *when* to check, this
// decides *what* currently holds.
type TriggerEvaluator struct {
	client   client.Client
	eaoGVK   schema.GroupVersionKind
	pressure *NodePressureEvaluator
}

// NewTriggerEvaluator creates a TriggerEvaluator.
func NewTriggerEvaluator(c client.Client, eaoGVK schema.GroupVersionKind, pressure *NodePressureEvaluator) *TriggerEvaluator {
	return &TriggerEvaluator{client: c, eaoGVK: eaoGVK, pressure: pressure}
}

// Evaluate checks every trigger condition declared on the profile, in
// order, and returns the first one that currently matches, along with a
// human-readable reason. matched=false means none of the declared
// conditions currently hold — not an error, just nothing to do this cycle.
func (e *TriggerEvaluator) Evaluate(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
) (matched bool, condition string, reason string, err error) {
	pods, err := utils.FindPodsForApplication(ctx, e.client, profile.Spec.ApplicationRef)
	if err != nil {
		return false, "", "", fmt.Errorf("finding pods for trigger evaluation: %w", err)
	}

	for _, cond := range profile.Spec.Rebalancing.TriggerConditions {
		var ok bool
		var r string

		switch cond {
		case TriggerEnergyThreshold:
			ok, r = e.evaluateEnergyThreshold(ctx, profile, pods)
		case TriggerCPUThreshold:
			ok, r = e.evaluateNodeResource(ctx, pods, e.pressure.EvaluateCPUPressure)
		case TriggerMemoryThreshold:
			ok, r = e.evaluateNodeResource(ctx, pods, e.pressure.EvaluateMemoryPressure)
		case TriggerNodeFailure:
			ok, r = e.evaluateNodeFailure(ctx, pods)
		case TriggerScheduled:
			ok, r = true, "scheduled rebalance window reached"
		default:
			continue // unrecognized condition — spec validation should have already caught this
		}

		if ok {
			return true, cond, r, nil
		}
	}
	return false, "", "", nil
}

// evaluateNodeResource applies a per-node pressure check (CPU or Memory,
// via NodePressureEvaluator) to every distinct node the profile's pods are
// currently placed on. metrics-server being unavailable is soft-failed per
// node (logged, not propagated) — the same graceful-degradation posture as
// the optional EnergyAwareOrchestration dependency.
func (e *TriggerEvaluator) evaluateNodeResource(
	ctx context.Context,
	pods []corev1.Pod,
	check func(ctx context.Context, nodeName string) (bool, string, error),
) (bool, string) {
	logger := logf.FromContext(ctx)

	seen := map[string]bool{}
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if nodeName == "" || seen[nodeName] {
			continue
		}
		seen[nodeName] = true

		ok, reason, err := check(ctx, nodeName)
		if err != nil {
			logger.V(1).Info("rebalance: node pressure check unavailable, skipping",
				"node", nodeName, "err", err)
			continue
		}
		if ok {
			return true, reason
		}
	}
	return false, ""
}

// evaluateNodeFailure reports whether any node hosting one of the
// profile's pods currently reports NotReady.
func (e *TriggerEvaluator) evaluateNodeFailure(ctx context.Context, pods []corev1.Pod) (bool, string) {
	logger := logf.FromContext(ctx)

	seen := map[string]bool{}
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if nodeName == "" || seen[nodeName] {
			continue
		}
		seen[nodeName] = true

		node := &corev1.Node{}
		if err := e.client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			logger.V(1).Info("rebalance: could not fetch node for NodeFailure check, skipping",
				"node", nodeName, "err", err)
			continue
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
				return true, fmt.Sprintf("node %s is not Ready (pod %s)", nodeName, pod.Name)
			}
		}
	}
	return false, ""
}

// evaluateEnergyThreshold finds the EnergyAwareOrchestration resource
// governing the profile's application (matched directly by ApplicationRef,
// no pod resolution needed — unlike internal/placement-server's
// pod-triggered EAO lookup) and checks three cases, in order:
//
//  1. Energy currently reported insufficient — a problem signal.
//  2. Decision explicitly Delayed/Waiting — also a problem signal.
//  3. The opposite signal: the decision now says DeployImmediately/Scheduled
//     (i.e. the EAO's own re-evaluation window has arrived and it already
//     refreshed its status to reflect that) AND a pod for this app is still
//     sitting Pending — meaning it was likely blocked by the energy gate
//     earlier and is worth retrying now. We deliberately don't parse
//     nextEvaluationTime ourselves; by the time we look, the EAO's own
//     status already reflects the new window.
func (e *TriggerEvaluator) evaluateEnergyThreshold(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	pods []corev1.Pod,
) (bool, string) {
	logger := logf.FromContext(ctx)
	appRef := profile.Spec.ApplicationRef

	eaoList := &unstructured.UnstructuredList{}
	eaoList.SetGroupVersionKind(e.eaoGVK)
	if err := e.client.List(ctx, eaoList); err != nil {
		// EnergyAwareOrchestration is an optional component — soft-fail like
		// internal/placement-server's DecisionContextBuilder.fetchEAOProfile.
		logger.V(1).Info("rebalance: EAO unavailable, skipping EnergyThreshold check", "err", err)
		return false, ""
	}

	for i := range eaoList.Items {
		eao := &eaoList.Items[i]

		refName, _, _ := unstructured.NestedString(eao.Object, "spec", "applicationRef", "name")
		refKind, _, _ := unstructured.NestedString(eao.Object, "spec", "applicationRef", "kind")
		refNamespace, _, _ := unstructured.NestedString(eao.Object, "spec", "applicationRef", "namespace")
		if refNamespace == "" {
			refNamespace = eao.GetNamespace()
		}
		if refName != appRef.Name || refNamespace != appRef.Namespace || refKind != appRef.Kind {
			continue
		}

		if sufficient, found, _ := unstructured.NestedBool(eao.Object, "status", "energyMetrics", "sufficient"); found && !sufficient {
			reason, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "reason")
			if reason == "" {
				reason = "energy supply reported insufficient"
			}
			return true, reason
		}

		action, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "action")

		if action == "Delayed" || action == "Waiting" {
			reason, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "reason")
			if reason == "" {
				reason = fmt.Sprintf("energy decision action=%s", action)
			}
			return true, reason
		}

		if (action == "DeployImmediately" || action == "Scheduled") && hasPendingPod(pods) {
			return true, "energy window reached — retrying previously deferred pod"
		}

		break // matched the EAO for this app — nothing more to check
	}
	return false, ""
}

// hasPendingPod reports whether any pod in the list is still Pending.
func hasPendingPod(pods []corev1.Pod) bool {
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodPending {
			return true
		}
	}
	return false
}
