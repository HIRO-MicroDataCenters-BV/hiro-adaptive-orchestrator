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
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/metrics"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// Trigger condition names, aliased from the canonical api/v1alpha1 enum.
const (
	TriggerEnergyThreshold = orchestrationv1alpha1.TriggerConditionEnergyThreshold
	TriggerCPUThreshold    = orchestrationv1alpha1.TriggerConditionCPUThreshold
	TriggerMemoryThreshold = orchestrationv1alpha1.TriggerConditionMemoryThreshold
	TriggerNodeFailure     = orchestrationv1alpha1.TriggerConditionNodeFailure
	TriggerScheduled       = orchestrationv1alpha1.TriggerConditionScheduled
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
	Condition orchestrationv1alpha1.TriggerCondition

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

	// Detail is the measurement behind the match, for node-scoped triggers.
	Detail TriggerDetail
}

// TriggerDetail is where and by how much a node-scoped trigger fired.
type TriggerDetail struct {
	// Node is the node the condition was observed on (pressure, NodeFailure).
	Node string

	// Value and Threshold are the usage ratio and its limit (fractions of
	// allocatable) for CPU/MemoryThreshold; nil otherwise.
	Value     *float64
	Threshold *float64
}

// TriggerEvaluator evaluates an OrchestrationProfile's declared rebalancing
// trigger conditions against current cluster state.
//
// Used identically by both halves of the hybrid detection design (see
// reconciler.go): a periodic tick and an event-driven wake-up both call
// Evaluate the same way — the watches only decide *when* to check, this
// decides *what* currently holds.
type TriggerEvaluator struct {
	client      client.Client
	eaoSettings placementserver.EAOSettings
	pressure    *NodePressureEvaluator
}

// NewTriggerEvaluator creates a TriggerEvaluator.
func NewTriggerEvaluator(
	c client.Client, eaoSettings placementserver.EAOSettings, pressure *NodePressureEvaluator,
) *TriggerEvaluator {
	return &TriggerEvaluator{client: c, eaoSettings: eaoSettings, pressure: pressure}
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
			detail TriggerDetail
		)

		switch cond {
		case TriggerEnergyThreshold:
			ok, r, bypass = e.evaluateEnergyThreshold(ctx, profile, pods)
		case TriggerCPUThreshold:
			ok, r, detail = e.evaluateNodeResource(ctx, pods, cond, e.pressure.readCPU)
		case TriggerMemoryThreshold:
			ok, r, detail = e.evaluateNodeResource(ctx, pods, cond, e.pressure.readMemory)
		case TriggerNodeFailure:
			ok, r, detail = e.evaluateNodeFailure(ctx, pods, cond)
		case TriggerScheduled:
			ok, r = true, "scheduled rebalance window reached"
		default:
			continue // unrecognized condition — spec validation should have already caught this
		}

		metrics.RebalanceTriggerEvaluationsTotal.
			WithLabelValues(string(cond), strconv.FormatBool(ok)).Inc()

		if ok {
			return true, TriggerResult{Condition: cond, Reason: r, BypassAction: bypass, Detail: detail}, nil
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
	cond orchestrationv1alpha1.TriggerCondition,
	check func(ctx context.Context, nodeName string) (pressureReading, error),
) (bool, string, TriggerDetail) {
	logger := logf.FromContext(ctx)

	seen := map[string]bool{}
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if nodeName == "" || seen[nodeName] {
			continue
		}
		seen[nodeName] = true

		reading, err := check(ctx, nodeName)
		if err != nil {
			metrics.RebalanceTriggerErrorsTotal.WithLabelValues(string(cond)).Inc()
			logger.V(1).Info("rebalance: node pressure check unavailable, skipping",
				"node", nodeName, "err", err)
			continue
		}
		if reading.exceeded {
			return true, reading.reason, TriggerDetail{
				Node: nodeName, Value: &reading.ratio, Threshold: &reading.threshold,
			}
		}
	}
	return false, "", TriggerDetail{}
}

// evaluateNodeFailure reports whether any node hosting one of the
// profile's pods currently reports NotReady.
func (e *TriggerEvaluator) evaluateNodeFailure(
	ctx context.Context,
	pods []corev1.Pod,
	cond orchestrationv1alpha1.TriggerCondition,
) (bool, string, TriggerDetail) {
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
			metrics.RebalanceTriggerErrorsTotal.WithLabelValues(string(cond)).Inc()
			logger.V(1).Info("rebalance: could not fetch node for NodeFailure check, skipping",
				"node", nodeName, "err", err)
			continue
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
				return true, fmt.Sprintf("node %s is not Ready (pod %s)", nodeName, pod.Name),
					TriggerDetail{Node: nodeName}
			}
		}
	}
	return false, "", TriggerDetail{}
}

// evaluateEnergyThreshold finds the EnergyAwareOrchestration resource
// governing the profile's application (matched directly by ApplicationRef,
// no pod resolution needed — unlike internal/placement-server's
// pod-triggered EAO lookup) and acts on its verdict, the same one the energy gate uses:
//
//  1. Hold (no energy until the slot) — a problem signal for the decider.
//  2. RunNow (DeployImmediately, or a hold whose slot has started) AND a pod
//     for this app is still sitting Pending — it was likely held by the energy
//     gate earlier and is worth retrying now.
//  3. Unknown (no decision, an EAO failure such as Waiting, an unmapped
//     action) — not an energy signal; the gate lets pods through.
//
// Returns (matched, reason, bypassAction). bypassAction is
// ActionRetryPendingSchedule for case 2 (no AI consultation needed) and
// empty for case 1 (the normal Evaluating flow decides what to do about it).
func (e *TriggerEvaluator) evaluateEnergyThreshold(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	pods []corev1.Pod,
) (bool, string, string) {
	logger := logf.FromContext(ctx)

	eao, err := utils.FindEAOForApp(ctx, e.client, e.eaoSettings.GVK, profile.Spec.ApplicationRef)
	if err != nil {
		// EnergyAwareOrchestration is an optional component — soft-fail like
		// internal/placement-server's DecisionContextBuilder.fetchEAOProfile.
		metrics.RebalanceTriggerErrorsTotal.WithLabelValues(string(TriggerEnergyThreshold)).Inc()
		logger.V(1).Info("rebalance: EAO unavailable, skipping EnergyThreshold check", "err", err)
		return false, "", ""
	}
	if eao == nil {
		return false, "", ""
	}

	eaoCtx := placementserver.NewEAOContext(eao, e.eaoSettings.VerdictConfig, time.Now())
	switch eaoCtx.Verdict {
	case placement.EAOVerdictHold:
		return true, placementserver.HoldReason(eaoCtx), ""
	case placement.EAOVerdictUnknown:
		logger.V(1).Info("rebalance: EAO verdict unknown, skipping EnergyThreshold check", "eao", eao.GetName())
		return false, "", ""
	}

	// RunNow; retry only now, so a pod held before the slot isn't retried early.
	if hasPendingPod(pods) {
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
