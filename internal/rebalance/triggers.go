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
	"maps"
	"slices"
	"strconv"
	"strings"
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
	TriggerPeriodicReview  = orchestrationv1alpha1.TriggerConditionPeriodicReview
)

// triggerPriority ranks the triggers Evaluate checks, 1 first; the profile only switches each one on.
var triggerPriority = map[int]orchestrationv1alpha1.TriggerCondition{
	1: TriggerNodeFailure,     // pods are down
	2: TriggerEnergyThreshold, // a pod held by the energy gate can start
	3: TriggerMemoryThreshold, // memory pressure: OOM kills, evictions
	4: TriggerCPUThreshold,    // CPU pressure: throttling only
	5: TriggerPeriodicReview,  // always matches, so it must be last
}

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

// nodeFailure is a node hosting one of the app's pods that isn't Ready, and since when.
type nodeFailure struct {
	node  string
	pod   string
	since time.Time
}

// TriggerEvaluator evaluates an OrchestrationProfile's declared rebalancing
// trigger conditions against current cluster state.
//
// Used identically by both halves of the hybrid detection design (see
// reconciler.go): a periodic tick and an event-driven wake-up both call
// Evaluate the same way — the watches only decide *when* to check, this
// decides *what* currently holds.
type TriggerEvaluator struct {
	client           client.Client
	eaoSettings      placementserver.EAOSettings
	pressure         *NodePressureEvaluator
	minPendingPodAge time.Duration
}

// NewTriggerEvaluator creates a TriggerEvaluator.
func NewTriggerEvaluator(
	c client.Client, eaoSettings placementserver.EAOSettings, pressure *NodePressureEvaluator, minPendingPodAge time.Duration,
) *TriggerEvaluator {
	return &TriggerEvaluator{client: c, eaoSettings: eaoSettings, pressure: pressure, minPendingPodAge: minPendingPodAge}
}

// Evaluate checks every trigger condition declared on the profile, in
// triggerPriority order (not the declared order), and returns the first one
// that currently matches. matched=false means none of the declared
// conditions currently hold — not an error, just nothing to do this cycle.
func (e *TriggerEvaluator) Evaluate(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
) (matched bool, result TriggerResult, err error) {
	pods, err := utils.FindPodsForApplication(ctx, e.client, profile.Spec.ApplicationRef)
	if err != nil {
		return false, TriggerResult{}, fmt.Errorf("finding pods for trigger evaluation: %w", err)
	}

	sortedTriggerPriority := slices.Sorted(maps.Keys(triggerPriority))
	for _, rank := range sortedTriggerPriority {
		cond := triggerPriority[rank]
		if !slices.Contains(profile.Spec.Rebalancing.TriggerConditions, cond) {
			continue // not switched on by this profile
		}
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
			ok, r, detail = e.evaluateNodeFailure(ctx, pods)
		case TriggerPeriodicReview:
			ok, r = true, "periodic review due"
		default:
			continue // unrecognized condition - a condition missing from triggerPriority is never reached
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

// failedNodes lists the nodes hosting the app's pods that report NotReady, in pod order.
func (e *TriggerEvaluator) failedNodes(ctx context.Context, pods []corev1.Pod) []nodeFailure {
	logger := logf.FromContext(ctx)

	var failed []nodeFailure
	seen := map[string]bool{}
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		if nodeName == "" || seen[nodeName] {
			continue
		}
		seen[nodeName] = true

		node := &corev1.Node{}
		if err := e.client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			metrics.RebalanceTriggerErrorsTotal.WithLabelValues(string(TriggerNodeFailure)).Inc()
			logger.V(1).Info("rebalance: could not fetch node for NodeFailure check, skipping",
				"node", nodeName, "err", err)
			continue
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
				failed = append(failed, nodeFailure{node: nodeName, pod: pod.Name, since: cond.LastTransitionTime.Time})
			}
		}
	}
	return failed
}

// evaluateNodeFailure reports whether any node hosting one of the
// profile's pods currently reports NotReady.
func (e *TriggerEvaluator) evaluateNodeFailure(ctx context.Context, pods []corev1.Pod) (bool, string, TriggerDetail) {
	failed := e.failedNodes(ctx, pods)
	if len(failed) == 0 {
		return false, "", TriggerDetail{}
	}
	f := failed[0]
	return true, fmt.Sprintf("node %s is not Ready (pod %s)", f.node, f.pod), TriggerDetail{Node: f.node}
}

// evaluateEnergyThreshold finds the EnergyAwareOrchestration resource
// governing the profile's application (matched directly by ApplicationRef,
// no pod resolution needed — unlike internal/placement-server's
// pod-triggered EAO lookup) and acts on its verdict, the same one the energy gate uses:
//
//  1. Hold (no energy until the slot) — a problem signal for the decider.
//  2. RunNow (DeployImmediately, or a hold whose slot has started) AND the
//     energy gate held a pod for this app that's still unscheduled — worth
//     retrying now (see heldByEnergyGate).
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

	// Same first check as CheckEnergyGate: with awareness off the gate holds nothing and the decider gets no EAO data.
	if !profile.Spec.Placement.Awareness.Energy {
		logger.V(1).Info("rebalance: energy awareness off, skipping EnergyThreshold check")
		return false, "", ""
	}

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
	for i := range pods {
		if heldByEnergyGate(&pods[i], e.minPendingPodAge) {
			return true, "energy window reached — retrying a pod the energy gate held", ActionRetryPendingSchedule
		}
	}

	return false, "", ""
}

// DefaultMinPendingPodAge backstops a tight delete/recreate loop: the retry isn't cooldown-gated.
const DefaultMinPendingPodAge = 10 * time.Second

// heldByEnergyGate reports whether the energy gate last turned this unscheduled pod away; other stuck pods are left alone.
func heldByEnergyGate(pod *corev1.Pod, minPendingPodAge time.Duration) bool {
	return pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" &&
		time.Since(pod.CreationTimestamp.Time) >= minPendingPodAge &&
		strings.Contains(utils.SchedulingFailureMessage(pod), placement.EnergyGateReasonPrefix)
}
