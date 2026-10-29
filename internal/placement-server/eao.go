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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/metrics"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// Defaults for EAOVerdictConfig, overridden by EAO_RUN_NOW_ACTIONS / EAO_HOLD_ACTIONS.
var (
	DefaultEAORunNowActions = []string{"DeployImmediately"}
	DefaultEAOHoldActions   = []string{"Scheduled", "Delayed"}
)

// EAOVerdictConfig lists which EAO decision.action values mean run now and which mean hold,
// so an EAO rename is a setting change, not a code change.
type EAOVerdictConfig struct {
	RunNowActions []string
	HoldActions   []string
}

// DefaultEAOVerdictConfig returns the mapping for today's EAO values.
func DefaultEAOVerdictConfig() EAOVerdictConfig {
	return EAOVerdictConfig{
		RunNowActions: slices.Clone(DefaultEAORunNowActions),
		HoldActions:   slices.Clone(DefaultEAOHoldActions),
	}
}

// Validate rejects a value listed as both run now and hold.
func (c EAOVerdictConfig) Validate() error {
	for _, a := range c.RunNowActions {
		if slices.Contains(c.HoldActions, a) {
			return fmt.Errorf("EAO action %q is in both the run-now and the hold lists", a)
		}
	}
	return nil
}

// eaoVerdict translates the EAO's decision; a hold whose slot has started is RunNow,
// and an unlisted value (e.g. Waiting, an EAO failure) is Unknown.
func eaoVerdict(d *EAODecision, cfg EAOVerdictConfig, now time.Time) (placement.EAOVerdict, *metav1.Time) {
	if d == nil {
		return placement.EAOVerdictUnknown, nil
	}
	switch {
	case slices.Contains(cfg.RunNowActions, d.Action):
		return placement.EAOVerdictRunNow, nil
	case slices.Contains(cfg.HoldActions, d.Action):
		if d.ScheduledSlot == nil {
			return placement.EAOVerdictHold, nil
		}
		start, err := time.Parse(time.RFC3339, d.ScheduledSlot.SlotStart)
		if err != nil {
			return placement.EAOVerdictHold, nil // hold, end unknown
		}
		if !now.Before(start) {
			return placement.EAOVerdictRunNow, nil
		}
		until := metav1.NewTime(start)
		return placement.EAOVerdictHold, &until
	default:
		return placement.EAOVerdictUnknown, nil
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

	logger.Info("builder: fetching EAO profile",
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

	return b.eaoContext(ctx, eao), nil
}

// fetchEAOForPod resolves the pod's root application via ResolveAppFromPod,
// then finds the EnergyAwareOrchestration CRD whose spec.applicationRef
// matches that application (utils.FindEAOForApp — shared with the rebalance
// engine's trigger evaluator and rebalance context builder, which already
// know their ApplicationReference directly and don't need this pod-based
// resolution step).
//
// Returns nil (no error) when no matching EAO exists.
func (b *DecisionContextBuilder) fetchEAOForPod(
	ctx context.Context,
	pod *corev1.Pod,
) (*unstructured.Unstructured, error) {
	logger := logf.FromContext(ctx)

	appName, appNamespace, appKind := utils.ResolveAppFromPod(ctx, b.client, pod)
	if appName == "" {
		logger.V(1).Info("builder: no workload owner, skipping EAO lookup",
			"pod", pod.Name,
			"namespace", pod.Namespace,
		)
		return nil, nil
	}

	appRef := orchestrationv1alpha1.ApplicationReference{
		Name: appName, Namespace: appNamespace, Kind: orchestrationv1alpha1.ApplicationKind(appKind),
	}
	eao, err := utils.FindEAOForApp(ctx, b.client, b.eaoGVK, appRef)
	if err != nil {
		return nil, err
	}
	if eao == nil {
		logger.V(1).Info("builder: no EAO found for application",
			"pod", pod.Name, "appName", appName, "appNamespace", appNamespace, "appKind", appKind,
		)
		return nil, nil
	}

	logger.V(1).Info("builder: EAO found for pod",
		"eao", eao.GetName(), "eaoNamespace", eao.GetNamespace(),
		"appName", appName, "appNamespace", appNamespace, "appKind", appKind,
	)
	return eao, nil
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
		slotStart, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "scheduledSlot", "slotStart")
		slotEnd, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "scheduledSlot", "slotEnd")
		if slotStart != "" || slotEnd != "" {
			ctx.Decision.ScheduledSlot = &EAOScheduledSlot{SlotStart: slotStart, SlotEnd: slotEnd}
		}
	}

	ctx.LastUpdated, _, _ = unstructured.NestedString(eao.Object, "status", "lastUpdated")

	// --- status.energyMetrics ---
	requiredWatts, foundRequired := nestedNumber(eao.Object, "status", "energyMetrics", "requiredWatts")
	sufficient, foundSufficient, _ := unstructured.NestedBool(eao.Object, "status", "energyMetrics", "sufficient")
	if foundRequired || foundSufficient {
		ctx.EnergyMetrics = &EAOEnergyMetrics{
			RequiredWatts: requiredWatts,
			Sufficient:    sufficient,
		}
		ctx.EnergyMetrics.MeasuredWatts, _ = nestedNumber(eao.Object, "status", "energyMetrics", "measuredWatts")
		ctx.EnergyMetrics.CurrentSlotAvailableWatts, _ = nestedNumber(
			eao.Object, "status", "energyMetrics", "currentSlotAvailableWatts",
		)
		ctx.EnergyMetrics.CurrentSlotConsumedWatts, _ = nestedNumber(
			eao.Object, "status", "energyMetrics", "currentSlotConsumedWatts",
		)
	}

	return ctx
}

// nestedNumber reads a JSON number that unstructured may have decoded as
// int64 (whole values) or float64; NestedFloat64 alone rejects the former.
func nestedNumber(obj map[string]any, fields ...string) (float64, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// fetchEAOProfileForApp is fetchEAOProfile's rebalance-side counterpart: the
// caller already knows the ApplicationReference directly (from the profile
// being rebalanced), so no pod-based resolution is needed.
func (b *DecisionContextBuilder) fetchEAOProfileForApp(
	ctx context.Context,
	appRef orchestrationv1alpha1.ApplicationReference,
) (*EAOProfileContext, error) {
	eao, err := utils.FindEAOForApp(ctx, b.client, b.eaoGVK, appRef)
	if err != nil {
		return nil, err
	}
	if eao == nil {
		return nil, nil
	}
	return b.eaoContext(ctx, eao), nil
}

// eaoContext maps the EAO and adds its verdict; an unmapped action is logged, so an EAO change shows up.
func (b *DecisionContextBuilder) eaoContext(ctx context.Context, eao *unstructured.Unstructured) *EAOProfileContext {
	eaoCtx := mapEAOToProfileContext(eao)
	eaoCtx.Verdict, eaoCtx.HoldUntil = eaoVerdict(eaoCtx.Decision, b.eaoVerdict, time.Now())
	if eaoCtx.Verdict == placement.EAOVerdictUnknown && eaoCtx.Decision != nil {
		logf.FromContext(ctx).Info("builder: EAO action not mapped to a verdict",
			"eao", eao.GetName(), "action", eaoCtx.Decision.Action)
	}
	return eaoCtx
}

// observeEAOFetch records one EAO lookup; nil with no error means none exists.
func observeEAOFetch(caller string, eao *EAOProfileContext, err error) {
	result := metrics.EAOResultFound
	switch {
	case err != nil:
		result = metrics.EAOResultError
	case eao == nil:
		result = metrics.EAOResultNotFound
	}
	metrics.PlacementEAOFetchTotal.WithLabelValues(caller, result).Inc()
}

// =============================================================================
// Energy gate check
// =============================================================================

// CheckEnergyGate checks whether the pod's governing OrchestrationProfile has
// energy awareness enabled and, if so, whether the EAO reports sufficient
// energy for scheduling.
//
// Called by the scheduler extender /filter handler once per pod scheduling event.
// Returns Allowed=true when:
//   - no profile governs the pod (unmanaged pods are never gated)
//   - energy awareness is disabled in the profile
//   - EAO data is unavailable (best-effort; scheduling is not blocked)
//   - EAO energy metrics indicate sufficient energy
func (b *DecisionContextBuilder) CheckEnergyGate(
	ctx context.Context,
	pod *corev1.Pod,
) (EnergyGateResponse, error) {
	profile, err := b.FindProfileForPod(ctx, pod)
	if err != nil {
		// Allow on lookup error -- do not block scheduling
		return EnergyGateResponse{Allowed: true}, err
	}
	if profile == nil || !profile.Spec.Placement.Awareness.Energy {
		return EnergyGateResponse{Allowed: true}, nil
	}

	eaoProfile, err := b.fetchEAOProfile(ctx, pod)
	observeEAOFetch(metrics.EAOCallerFilter, eaoProfile, err)
	if err != nil || eaoProfile == nil {
		// EAO data unavailable -- allow scheduling (best-effort energy gating)
		return EnergyGateResponse{Allowed: true}, nil
	}

	if eaoProfile.EnergyMetrics != nil && !eaoProfile.EnergyMetrics.Sufficient {
		reason := "insufficient energy available"
		if eaoProfile.Decision != nil && eaoProfile.Decision.Reason != "" {
			reason = eaoProfile.Decision.Reason
		}
		return EnergyGateResponse{Allowed: false, Reason: reason}, nil
	}

	return EnergyGateResponse{Allowed: true}, nil
}
