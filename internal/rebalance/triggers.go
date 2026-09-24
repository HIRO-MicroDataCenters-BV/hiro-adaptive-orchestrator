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
	"time"

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
// internal/controller/op_constants.go. These are the only values a user can
// declare in spec.rebalancing.triggerConditions.
const (
	TriggerEnergyThreshold = "EnergyThreshold"
	TriggerCPUThreshold    = "CPUThreshold"
	TriggerMemoryThreshold = "MemoryThreshold"
	TriggerNodeFailure     = "NodeFailure"
	TriggerScheduled       = "Scheduled"
)

// ActionRetryPendingSchedule is NOT a trigger condition — it never appears
// in spec.rebalancing.triggerConditions and is never validated against
// validTriggerConditions. It's an internal action, produced only by
// evaluateEnergyThreshold's pending-pod case, and consumed only via
// TriggerResult.BypassAction: delete a Pending, unscheduled pod so its
// owning controller creates a replacement, forcing an immediate fresh
// scheduling attempt instead of waiting on kube-scheduler's own backoff.
// Kept in its own block, deliberately separate from the CRD-enum constants
// above, so it isn't mistaken for one of them.
const ActionRetryPendingSchedule = "RetryPendingSchedule"

// TriggerResult describes a matched trigger condition and how it should be
// handled downstream.
type TriggerResult struct {
	// Condition is the CRD-declared trigger condition that matched (one of
	// the Trigger* constants above).
	Condition string

	// Reason is a human-readable explanation, surfaced in status and events.
	Reason string

	// BypassAction, when non-empty (ActionRetryPendingSchedule today), names
	// an action whose enactment doesn't need an AI consultation — the
	// trigger's own logic already fully determined it (no judgement call for
	// the AI to make: no target node to pick, no improvement threshold to
	// weigh). The Reconciler still drives the full Triggered -> Evaluating ->
	// Decided -> Enacting -> Enacted/Failed sequence for traceability;
	// Evaluating just synthesizes the decision internally instead of calling
	// the external AI.
	//
	// Empty means the normal flow applies: Evaluating calls the AI, which may
	// return Move/NoOp/etc.
	BypassAction string
}

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
// order, and returns the first one that currently matches. matched=false
// means none of the declared conditions currently hold — not an error, just
// nothing to do this cycle.
func (e *TriggerEvaluator) Evaluate(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
) (matched bool, result TriggerResult, err error) {
	pods, err := utils.FindPodsForApplication(ctx, e.client, profile.Spec.ApplicationRef)
	if err != nil {
		return false, TriggerResult{}, fmt.Errorf("finding pods for trigger evaluation: %w", err)
	}

	for _, cond := range profile.Spec.Rebalancing.TriggerConditions {
		var (
			ok     bool
			r      string
			bypass string
		)

		switch cond {
		case TriggerEnergyThreshold:
			ok, r, bypass = e.evaluateEnergyThreshold(ctx, profile, pods)
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
			return true, TriggerResult{Condition: cond, Reason: r, BypassAction: bypass}, nil
		}
	}
	return false, TriggerResult{}, nil
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
//
// Returns (matched, reason, bypassAction). bypassAction is
// ActionRetryPendingSchedule for case 3 (no AI consultation needed) and
// empty for cases 1/2 (genuine problem signals — the normal Evaluating flow,
// once built, decides what to do about them).
func (e *TriggerEvaluator) evaluateEnergyThreshold(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	pods []corev1.Pod,
) (bool, string, string) {
	logger := logf.FromContext(ctx)

	eao, err := utils.FindEAOForApp(ctx, e.client, e.eaoGVK, profile.Spec.ApplicationRef)
	if err != nil {
		// EnergyAwareOrchestration is an optional component — soft-fail like
		// internal/placement-server's DecisionContextBuilder.fetchEAOProfile.
		logger.V(1).Info("rebalance: EAO unavailable, skipping EnergyThreshold check", "err", err)
		return false, "", ""
	}
	if eao == nil {
		return false, "", ""
	}

	if sufficient, found, _ := unstructured.NestedBool(eao.Object, "status", "energyMetrics", "sufficient"); found && !sufficient {
		reason, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "reason")
		if reason == "" {
			reason = "energy supply reported insufficient"
		}
		return true, reason, ""
	}

	action, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "action")

	if action == "Delayed" || action == "Waiting" {
		reason, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "reason")
		if reason == "" {
			reason = fmt.Sprintf("energy decision action=%s", action)
		}
		return true, reason, ""
	}

	if (action == "DeployImmediately" || action == "Scheduled") && hasPendingPod(pods) {
		return true, "energy window reached — retrying previously deferred pod", ActionRetryPendingSchedule
	}

	return false, "", ""
}

// MinPendingPodAge is how long a pod must have been Pending before
// hasPendingPod considers it retry-eligible. Reconcile no longer gates the
// RetryPendingSchedule bypass on cooldown (see reconciler.go), so without
// this floor a retry's own deletion produces a brand-new replacement that is
// itself briefly Pending — and since every StateWriter transition is a
// status write the primary watch reacts to, that immediately re-queues
// another Reconcile which would see "still Pending" and retry again before
// the replacement ever got a real chance to schedule, live-locking in a
// tight loop with no backstop at all. This floor gives normal scheduling a
// fair window first; a pod genuinely stuck behind the energy gate stays
// Pending well past it regardless, so real retries are barely delayed.
const MinPendingPodAge = 10 * time.Second

// hasPendingPod reports whether any pod in the list is Pending, unscheduled
// (NodeName == ""), and has been so for at least MinPendingPodAge. Must
// match retryPendingSchedule's own selection criteria (retry_enactor.go)
// exactly: a pod that's already scheduled but simply hasn't started its
// container yet is still Phase: Pending for a few seconds — normal startup
// latency, nothing to do with the energy gate. Without the NodeName check,
// hasPendingPod would keep matching that pod on every reconcile while
// retryPendingSchedule (correctly) finds nothing to act on, and since the
// bypass path isn't cooldown-gated, that mismatch alone is enough to drive
// a tight, unproductive Triggered->...->Watching loop until the pod
// naturally leaves Pending.
func hasPendingPod(pods []corev1.Pod) bool {
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" &&
			time.Since(pod.CreationTimestamp.Time) >= MinPendingPodAge {
			return true
		}
	}
	return false
}
