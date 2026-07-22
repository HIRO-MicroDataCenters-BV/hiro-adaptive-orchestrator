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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// DefaultDetectionInterval is how often each rebalancing-enabled profile is
// re-checked for slow-drifting trigger conditions (CPU/Memory/Scheduled)
// when no faster event has fired first.
const DefaultDetectionInterval = 30 * time.Second

// Never Ever delete this comments as they are used by kubebuilder to generate RBAC permissions for the controller.
// If you need to change the permissions,
// modify the verbs and resources in the comments below and then run "make generate" to update the generated code.

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=nodes,verbs=get

// Reconciler drives the Detection stage of the rebalance engine: it decides
// when a workload's decision lifecycle should enter Triggered. Hybrid by
// design (see the rebalance engine notes on Detection):
//
//   - Periodic — Reconcile always requeues itself after DetectionInterval,
//     which is the only path that can catch CPUThreshold, MemoryThreshold,
//     and Scheduled (there's no discrete Kubernetes event for "resource
//     requests drifted" or "the scheduled window arrived").
//   - Event-driven — Pod, Node, and (optionally) EnergyAwareOrchestration
//     watches cause an immediate reconcile instead of waiting for the next
//     tick, so EnergyThreshold and NodeFailure don't have to wait up to
//     DetectionInterval to be noticed. Profile status changes are already
//     covered for free by the primary For() watch below.
//
// Both paths call the exact same TriggerEvaluator.Evaluate — the watches
// and the ticker only decide *when* to check, not *what* currently holds.
//
// This Reconciler only drives the Triggered transition. Decision (Story
// 27/28) and Enaction (Story 29/30) extend Reconcile to also act once a
// profile is past Triggered — see the in-flight check below.
type Reconciler struct {
	client.Client
	Writer            *StateWriter
	Evaluator         *TriggerEvaluator
	ProfileIndexField string
	DetectionInterval time.Duration
}

// NewReconciler creates a Reconciler. interval <= 0 uses DefaultDetectionInterval.
func NewReconciler(
	c client.Client,
	writer *StateWriter,
	evaluator *TriggerEvaluator,
	profileIndexField string,
	interval time.Duration,
) *Reconciler {
	if interval <= 0 {
		interval = DefaultDetectionInterval
	}
	return &Reconciler{
		Client:            c,
		Writer:            writer,
		Evaluator:         evaluator,
		ProfileIndexField: profileIndexField,
		DetectionInterval: interval,
	}
}

// Reconcile checks one profile's cooldown and trigger conditions, and
// transitions it to Triggered if warranted.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	profile := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := r.Get(ctx, req.NamespacedName, profile); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !profile.Spec.Rebalancing.Enabled {
		return ctrl.Result{}, nil // opt-in — no Triggered ever fires
	}

	rs := profile.Status.RebalancingStatus

	if !rs.CooldownUntil.IsZero() {
		if remaining := time.Until(rs.CooldownUntil.Time); remaining > 0 {
			logger.V(1).Info("rebalance: still in cooldown, skipping",
				"profile", profile.Name, "remaining", remaining)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}

	if rs.State != "" && !IsTerminal(rs.State) {
		// A cycle is already past Triggered (Evaluating/Decided/Enacting) —
		// this Reconciler's job (Detection) is done for now; let it run to a
		// terminal state before considering a new one. Later stories extend
		// this method to act on these in-flight states rather than skip them.
		return ctrl.Result{}, nil
	}

	matched, result, err := r.Evaluator.Evaluate(ctx, profile)
	if err != nil {
		logger.Error(err, "rebalance: trigger evaluation failed", "profile", profile.Name)
		return ctrl.Result{RequeueAfter: r.DetectionInterval}, nil
	}

	if !matched {
		return ctrl.Result{RequeueAfter: r.DetectionInterval}, nil
	}

	if result.BypassAction != "" {
		// No AI consultation needed — the trigger already fully determined
		// the action. Drive the whole Triggered -> ... -> Enacted/Failed
		// sequence now instead of waiting on a future reconcile.
		r.enactBypassAction(ctx, req.NamespacedName, profile, result)
		return ctrl.Result{RequeueAfter: r.DetectionInterval}, nil
	}

	if err := r.Writer.Transition(ctx, req.NamespacedName, StateTriggered,
		fmt.Sprintf("%s: %s", result.Condition, result.Reason), TransitionOptions{}); err != nil {
		logger.Error(err, "rebalance: failed to transition to Triggered", "profile", profile.Name)
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: r.DetectionInterval}, nil
}

// enactBypassAction drives Triggered -> Evaluating -> Decided -> Enacting ->
// Enacted/Failed for a trigger match whose action needs no AI consultation
// (result.BypassAction is already the answer). Evaluating and Decided are
// synthesized locally instead of calling the external AI — every transition
// still goes through StateWriter, so the sequence is fully visible in
// status/Events/recentDecisions, it just never makes an HTTP round trip.
//
// Errors are recorded via the terminal (Failed) transition, not returned —
// callers still get the standard periodic requeue either way.
func (r *Reconciler) enactBypassAction(
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	result TriggerResult,
) {
	logger := logf.FromContext(ctx)
	reason := fmt.Sprintf("%s: %s", result.Condition, result.Reason)

	if err := r.Writer.Transition(ctx, key, StateTriggered, reason, TransitionOptions{}); err != nil {
		logger.Error(err, "rebalance: bypass transition to Triggered failed", "profile", profile.Name)
		return
	}
	if err := r.Writer.Transition(ctx, key, StateEvaluating,
		"mechanical action determined by trigger, bypassing AI consultation", TransitionOptions{}); err != nil {
		logger.Error(err, "rebalance: bypass transition to Evaluating failed", "profile", profile.Name)
		return
	}
	if err := r.Writer.Transition(ctx, key, StateDecided, reason,
		TransitionOptions{Action: result.BypassAction}); err != nil {
		logger.Error(err, "rebalance: bypass transition to Decided failed", "profile", profile.Name)
		return
	}
	if err := r.Writer.Transition(ctx, key, StateEnacting,
		fmt.Sprintf("enacting %s", result.BypassAction), TransitionOptions{Action: result.BypassAction}); err != nil {
		logger.Error(err, "rebalance: bypass transition to Enacting failed", "profile", profile.Name)
		return
	}

	// CooldownSeconds is read directly here (rather than via a general
	// cooldown-computation helper, which doesn't exist yet for the AI-driven
	// path) specifically to prevent this mechanical action from retrying in
	// a tight loop every DetectionInterval if the delete doesn't actually fix
	// the underlying scheduling problem.
	cooldown := time.Duration(profile.Spec.Rebalancing.CooldownSeconds) * time.Second

	if err := r.enact(ctx, profile, result.BypassAction); err != nil {
		if tErr := r.Writer.Transition(ctx, key, StateFailed, err.Error(),
			TransitionOptions{Action: result.BypassAction, Cooldown: cooldown}); tErr != nil {
			logger.Error(tErr, "rebalance: bypass transition to Failed failed", "profile", profile.Name)
		}
		return
	}

	if err := r.Writer.Transition(ctx, key, StateEnacted, reason,
		TransitionOptions{Action: result.BypassAction, Cooldown: cooldown}); err != nil {
		logger.Error(err, "rebalance: bypass transition to Enacted failed", "profile", profile.Name)
	}
}

// enact dispatches a bypass action to its enactor. Unknown actions are a
// programmer error — TriggerEvaluator should never produce one that isn't
// handled here.
func (r *Reconciler) enact(ctx context.Context, profile *orchestrationv1alpha1.OrchestrationProfile, action string) error {
	switch action {
	case ActionRetryPendingSchedule:
		return retryPendingSchedule(ctx, r.Client, profile)
	default:
		return fmt.Errorf("unknown bypass action %q", action)
	}
}

// SetupWithManager registers the Reconciler with the Manager.
//
// Event sources:
//  1. OrchestrationProfile (primary) — any CRUD event, including status
//     updates, triggers reconciliation directly. This is also how the
//     Reconciler notices its own StateWriter transitions and how future
//     Decision/Enaction stages will drive themselves forward.
//  2. Pod — pod events (eviction, phase change) on a profile's own pods.
//  3. Node — NotReady wakes every rebalancing-enabled profile; Reconcile
//     determines whether the affected node actually hosts any of its pods.
//  4. EnergyAwareOrchestration (optional) — only watched if the CRD is
//     actually installed, checked via the RESTMapper. metrics-server-style
//     optional dependency: skip gracefully rather than fail manager startup
//     if it's absent.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, eaoGVK schema.GroupVersionKind) error {
	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&orchestrationv1alpha1.OrchestrationProfile{}).
		Watches(&corev1.Pod{}, r.podMapper()).
		Watches(&corev1.Node{}, r.nodeMapper())

	if r.eaoInstalled(mgr, eaoGVK) {
		eaoObj := &unstructured.Unstructured{}
		eaoObj.SetGroupVersionKind(eaoGVK)
		bldr = bldr.Watches(eaoObj, r.eaoMapper())
	} else {
		mgr.GetLogger().Info("rebalance: EnergyAwareOrchestration CRD not installed, "+
			"EnergyThreshold will only be checked on the periodic tick", "gvk", eaoGVK)
	}

	return bldr.Named("rebalance-detection").Complete(r)
}

// eaoInstalled reports whether the EnergyAwareOrchestration CRD is
// registered with the API server, via the Manager's RESTMapper.
func (r *Reconciler) eaoInstalled(mgr ctrl.Manager, eaoGVK schema.GroupVersionKind) bool {
	_, err := mgr.GetRESTMapper().RESTMapping(eaoGVK.GroupKind(), eaoGVK.Version)
	return err == nil
}

// podMapper maps Pod events to reconcile requests for the profile(s)
// governing that pod's application.
func (r *Reconciler) podMapper() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return nil
		}
		appName, appNamespace, _ := utils.ResolveAppFromPod(ctx, r.Client, pod)
		if appName == "" {
			return nil
		}
		return r.profilesByIndexKey(ctx, appNamespace+"/"+appName)
	})
}

// eaoMapper maps EnergyAwareOrchestration events to reconcile requests for
// the profile(s) referencing the same application.
func (r *Reconciler) eaoMapper() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return nil
		}
		name, _, _ := unstructured.NestedString(u.Object, "spec", "applicationRef", "name")
		if name == "" {
			return nil
		}
		namespace, _, _ := unstructured.NestedString(u.Object, "spec", "applicationRef", "namespace")
		if namespace == "" {
			namespace = u.GetNamespace()
		}
		return r.profilesByIndexKey(ctx, namespace+"/"+name)
	})
}

// nodeMapper wakes every rebalancing-enabled profile when a node goes
// NotReady. Reconcile itself determines whether that node actually hosts
// any of a given profile's pods — this is intentionally broad, matching
// the existing OrchestrationProfile controller's watcher convention.
func (r *Reconciler) nodeMapper() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		node, ok := obj.(*corev1.Node)
		if !ok {
			return nil
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
				return r.allEnabledProfiles(ctx)
			}
		}
		return nil
	})
}

// profilesByIndexKey looks up profiles by the ProfileByAppRefIndex-style
// field index, filtered to those with rebalancing enabled.
func (r *Reconciler) profilesByIndexKey(ctx context.Context, key string) []reconcile.Request {
	list := &orchestrationv1alpha1.OrchestrationProfileList{}
	if err := r.List(ctx, list, client.MatchingFields{r.ProfileIndexField: key}); err != nil {
		return nil
	}
	return enabledProfileRequests(list.Items)
}

// allEnabledProfiles lists every rebalancing-enabled profile cluster-wide.
func (r *Reconciler) allEnabledProfiles(ctx context.Context) []reconcile.Request {
	list := &orchestrationv1alpha1.OrchestrationProfileList{}
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	return enabledProfileRequests(list.Items)
}

// enabledProfileRequests converts profiles with rebalancing enabled into
// reconcile requests. OrchestrationProfile is cluster-scoped, so requests
// carry only a Name.
func enabledProfileRequests(items []orchestrationv1alpha1.OrchestrationProfile) []reconcile.Request {
	reqs := make([]reconcile.Request, 0, len(items))
	for _, p := range items {
		if !p.Spec.Rebalancing.Enabled {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
	}
	return reqs
}
