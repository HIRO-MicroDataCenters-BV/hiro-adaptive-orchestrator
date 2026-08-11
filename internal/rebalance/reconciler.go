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

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// DefaultDetectionInterval is how often each rebalancing-enabled profile is
// re-checked for slow-drifting trigger conditions (CPU/Memory/Scheduled)
// when no faster event has fired first.
const DefaultDetectionInterval = 30 * time.Second

// DefaultDecisionTimeout bounds how long evaluateWithAI waits for the
// External AI Agent to answer a rebalance decision request before treating
// it as failed.
const DefaultDecisionTimeout = 5 * time.Second

// DefaultImprovementThreshold is the minimum Improvement score (see
// placementserver.RebalanceDecisionResponse) a Move recommendation must
// clear to be enacted. Below this, the guardrail rejects the AI's own
// recommendation instead of enacting a marginal move. A single global value
// for now — per-trigger-reason thresholds are a possible future refinement,
// not implemented here.
const DefaultImprovementThreshold = 20.0

// DefaultMoveRateLimit is the default cluster-wide cap on how many Moves
// (across every profile, not per-profile — that's cooldown's job) may reach
// Enacting per minute. Bounds the fleet's total eviction rate regardless of
// how many individually-cooled-down profiles decide to act at once.
const DefaultMoveRateLimit = 5

// DefaultMoveRateWaitTimeout bounds how long dispatchMove will block waiting
// for a rate-limit token before giving up and failing the Move. Deliberately
// generous (not a short "fail fast" bound): the limiter's bucket always
// refills eventually, so a long wait almost always succeeds within this one
// call — meaning the already-obtained AI decision gets used, instead of
// being discarded and re-requested from the AI on a later reconcile. This
// only exists as a safety valve for a genuinely starved/misconfigured
// budget, not as the normal path. Pair with DefaultMaxConcurrentReconciles
// below — a worker blocked in Wait for minutes must not stall every other
// profile's reconciliation.
const DefaultMoveRateWaitTimeout = 5 * time.Minute

// DefaultMaxConcurrentReconciles is how many profiles' Reconcile calls this
// controller runs in parallel (see SetupWithManager). Raised above
// controller-runtime's default of 1 specifically because of dispatchMove's
// rate-limit Wait (DefaultMoveRateWaitTimeout): with only one worker, a
// single profile blocked for minutes waiting on a Move token would stall
// reconciliation for every other profile too, even ones with nothing to do
// with Move at all. Sized comfortably above DefaultMoveRateLimit's burst (5)
// rather than scaled to fleet size — the number of profiles that can
// simultaneously be blocked in Wait is bounded by the limiter's own
// throughput, not by how many profiles exist.
const DefaultMaxConcurrentReconciles = 10

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
// This Reconciler drives Detection (Triggered) and, for trigger matches with
// no mechanical bypass action, the AI consultation (Evaluating) too — see
// evaluateWithAI. Enaction on a successful AI response is not yet wired up
// here — see evaluateWithAI's doc comment.
type Reconciler struct {
	client.Client
	Writer            *StateWriter
	Evaluator         *TriggerEvaluator
	ProfileIndexField string
	DetectionInterval time.Duration

	// ContextBuilder and DecisionClient are the same instances the placement
	// server uses for initial-placement decisions (see cmd/main.go) — the
	// rebalance engine's AI consultation is a second use of the one
	// configured External AI Agent, not a parallel HTTP path.
	ContextBuilder *placementserver.DecisionContextBuilder
	DecisionClient *placementserver.DecisionClient

	// DecisionTimeout bounds evaluateWithAI's wait for the AI agent to
	// respond. <= 0 uses DefaultDecisionTimeout.
	DecisionTimeout time.Duration

	// ImprovementThreshold is the guardrail applied to a Move recommendation
	// in dispatchDecision. <= 0 uses DefaultImprovementThreshold.
	ImprovementThreshold float64

	// MoveActionTimeout bounds how long the Move enactor waits for a
	// replacement pod to be scheduled after eviction. <= 0 uses
	// DefaultMoveActionTimeout.
	MoveActionTimeout time.Duration

	// DecisionStore is the same instance PlacementServer.score reads —
	// dispatchDecision's Move enactor writes to it before eviction so the
	// scheduler can honor the decision when the replacement pod is scored.
	DecisionStore *placementserver.DecisionStore

	// MoveRateLimiter caps the cluster-wide rate of Decided -> Enacting
	// transitions for Move (see dispatchMove) — a single shared limiter
	// across every profile, since this bounds the fleet's total eviction
	// rate, not any one workload's own pace (that's cooldown). Constructed
	// by NewReconciler; RetryPendingSchedule is not gated by it (cheap,
	// deletes an already-unscheduled pod, no running workload disrupted).
	MoveRateLimiter *rate.Limiter

	// MoveRateWaitTimeout bounds how long dispatchMove blocks waiting on
	// MoveRateLimiter before failing the Move. <= 0 uses
	// DefaultMoveRateWaitTimeout. A settable field (not wired to an env var)
	// so tests can shrink it, same as MoveActionTimeout.
	MoveRateWaitTimeout time.Duration
}

// NewReconciler creates a Reconciler. interval <= 0 uses
// DefaultDetectionInterval; decisionTimeout <= 0 uses DefaultDecisionTimeout;
// improvementThreshold <= 0 uses DefaultImprovementThreshold; moveActionTimeout
// <= 0 uses DefaultMoveActionTimeout; moveRateLimit <= 0 uses
// DefaultMoveRateLimit.
func NewReconciler(
	c client.Client,
	writer *StateWriter,
	evaluator *TriggerEvaluator,
	profileIndexField string,
	interval time.Duration,
	contextBuilder *placementserver.DecisionContextBuilder,
	decisionClient *placementserver.DecisionClient,
	decisionTimeout time.Duration,
	improvementThreshold float64,
	decisionStore *placementserver.DecisionStore,
	moveActionTimeout time.Duration,
	moveRateLimit int,
) *Reconciler {
	if interval <= 0 {
		interval = DefaultDetectionInterval
	}
	if decisionTimeout <= 0 {
		decisionTimeout = DefaultDecisionTimeout
	}
	if improvementThreshold <= 0 {
		improvementThreshold = DefaultImprovementThreshold
	}
	if moveActionTimeout <= 0 {
		moveActionTimeout = DefaultMoveActionTimeout
	}
	if moveRateLimit <= 0 {
		moveRateLimit = DefaultMoveRateLimit
	}
	// Burst equals the per-minute limit itself: a quiet fleet can absorb a
	// full minute's budget worth of Moves immediately, then throttles to a
	// steady trickle (one token every 60/moveRateLimit seconds) after that.
	moveRateLimiter := rate.NewLimiter(rate.Limit(float64(moveRateLimit)/60.0), moveRateLimit)
	return &Reconciler{
		Client:               c,
		Writer:               writer,
		Evaluator:            evaluator,
		ProfileIndexField:    profileIndexField,
		DetectionInterval:    interval,
		ContextBuilder:       contextBuilder,
		DecisionClient:       decisionClient,
		DecisionTimeout:      decisionTimeout,
		ImprovementThreshold: improvementThreshold,
		DecisionStore:        decisionStore,
		MoveRateLimiter:      moveRateLimiter,
		MoveRateWaitTimeout:  DefaultMoveRateWaitTimeout,
		MoveActionTimeout:    moveActionTimeout,
	}
}

// Reconcile checks one profile's trigger conditions and transitions it to
// Triggered if warranted. Cooldown only gates the AI-consultation path, not
// the mechanical bypass path — see the BypassAction branch below for why.
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

	if rs.State != "" && rs.State != StateWatching {
		// A cycle is already past Triggered (Evaluating/Decided/Enacting) —
		// this Reconciler's job (Detection) is done for now; let it run back
		// to Watching before considering a new one. Later stories extend
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
		// the action, and it's cheap/mechanical (RetryPendingSchedule today:
		// deleting an already-Pending, never-Ready pod). Deliberately NOT
		// gated by cooldown: cooldown exists to rate-limit AI consultation,
		// not to delay reacting to a still-unresolved condition (e.g. a pod
		// still Pending after an unrelated NoOp/Move armed cooldown) once
		// any reconcile — periodic or event-driven — observes it's now
		// actionable. Drive the whole Triggered -> ... -> Enacted/Failed
		// sequence now instead of waiting on a future reconcile.
		r.enactBypassAction(ctx, req.NamespacedName, profile, result)
		return ctrl.Result{RequeueAfter: r.DetectionInterval}, nil
	}

	if !rs.CooldownUntil.IsZero() {
		if remaining := time.Until(rs.CooldownUntil.Time); remaining > 0 {
			logger.V(1).Info("rebalance: still in cooldown, skipping AI consultation",
				"profile", profile.Name, "remaining", remaining)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}

	r.evaluateWithAI(ctx, req.NamespacedName, profile, result)
	return ctrl.Result{RequeueAfter: r.DetectionInterval}, nil
}

// enactBypassAction drives Triggered -> Evaluating -> Decided -> Enacting ->
// Watching (Outcome Enacted/Failed) for a trigger match whose action needs
// no AI consultation (result.BypassAction is already the answer). Evaluating
// and Decided are synthesized locally instead of calling the external AI —
// every transition still goes through StateWriter, so the sequence is fully
// visible in status/Events/recentDecisions, it just never makes an HTTP
// round trip.
//
// Errors are recorded via the terminal (Watching, Outcome Failed) transition,
// not returned — callers still get the standard periodic requeue either way.
func (r *Reconciler) enactBypassAction(
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	result TriggerResult,
) {
	logger := logf.FromContext(ctx)
	reason := fmt.Sprintf("%s: %s", result.Condition, result.Reason)
	bypassAction := orchestrationv1alpha1.RebalanceAction(result.BypassAction)

	if _, err := r.Writer.Transition(ctx, key, StateTriggered, reason, TransitionOptions{}); err != nil {
		logger.Error(err, "rebalance: bypass transition to Triggered failed", "profile", profile.Name)
		return
	}
	if _, err := r.Writer.Transition(ctx, key, StateEvaluating,
		"mechanical action determined by trigger, bypassing AI consultation", TransitionOptions{}); err != nil {
		logger.Error(err, "rebalance: bypass transition to Evaluating failed", "profile", profile.Name)
		return
	}
	if _, err := r.Writer.Transition(ctx, key, StateDecided, reason,
		TransitionOptions{Action: bypassAction}); err != nil {
		logger.Error(err, "rebalance: bypass transition to Decided failed", "profile", profile.Name)
		return
	}
	if _, err := r.Writer.Transition(ctx, key, StateEnacting,
		fmt.Sprintf("enacting %s", result.BypassAction), TransitionOptions{Action: bypassAction}); err != nil {
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
		if _, tErr := r.Writer.Transition(ctx, key, StateWatching, err.Error(),
			TransitionOptions{Action: bypassAction, Outcome: OutcomeFailed, Cooldown: cooldown}); tErr != nil {
			logger.Error(tErr, "rebalance: bypass transition to Watching (Failed) failed", "profile", profile.Name)
		}
		return
	}

	if _, err := r.Writer.Transition(ctx, key, StateWatching, reason,
		TransitionOptions{Action: bypassAction, Outcome: OutcomeEnacted, Cooldown: cooldown}); err != nil {
		logger.Error(err, "rebalance: bypass transition to Watching (Enacted) failed", "profile", profile.Name)
	}
}

// evaluateWithAI drives Triggered -> Evaluating and consults the External AI
// Agent for a trigger match that has no mechanical answer (result.BypassAction
// is empty). A successful response is handed to dispatchDecision, which
// applies the improvement-threshold guardrail and drives the rest of the
// cycle; the timeout/error paths return straight to Watching (Outcome
// Failed) via failEvaluation.
func (r *Reconciler) evaluateWithAI(
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	result TriggerResult,
) {
	logger := logf.FromContext(ctx)
	reason := fmt.Sprintf("%s: %s", result.Condition, result.Reason)

	if _, err := r.Writer.Transition(ctx, key, StateTriggered, reason, TransitionOptions{}); err != nil {
		logger.Error(err, "rebalance: AI-path transition to Triggered failed", "profile", profile.Name)
		return
	}

	rs, err := r.Writer.Transition(ctx, key, StateEvaluating,
		"requesting AI rebalance decision", TransitionOptions{})
	if err != nil {
		logger.Error(err, "rebalance: AI-path transition to Evaluating failed", "profile", profile.Name)
		return
	}

	timeout := r.DecisionTimeout
	if timeout <= 0 {
		timeout = DefaultDecisionTimeout
	}
	aiCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := r.ContextBuilder.BuildRebalanceContext(aiCtx, profile, rs.DecisionID, reason, rs.RecentDecisions)
	if err != nil {
		r.failEvaluation(ctx, key, profile, fmt.Sprintf("building AI decision context: %v", err))
		return
	}

	resp, err := r.DecisionClient.RequestRebalanceDecision(aiCtx, req)
	if err != nil {
		r.failEvaluation(ctx, key, profile, fmt.Sprintf("AI unavailable: %v", err))
		return
	}

	logger.Info("rebalance: AI rebalance decision received",
		"profile", profile.Name,
		"action", resp.Action,
		"podName", resp.PodName,
		"targetNode", resp.TargetNode,
		"improvement", resp.Improvement,
		"reason", resp.Reason,
	)

	r.dispatchDecision(ctx, key, profile, resp)
}

// failEvaluation returns a profile to Watching with Outcome Failed when the
// AI consultation itself could not be completed (context assembly or the
// HTTP round trip), as opposed to the AI successfully responding with an
// unfavorable decision.
//
// Applies the profile's own cooldown here too — otherwise an unreachable or
// slow AI agent gets re-consulted on every single DetectionInterval tick with
// no backoff at all, hammering it in a tight loop instead of waiting like
// every other terminal outcome does.
func (r *Reconciler) failEvaluation(
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	reason string,
) {
	logger := logf.FromContext(ctx)
	cooldown := time.Duration(profile.Spec.Rebalancing.CooldownSeconds) * time.Second
	if _, err := r.Writer.Transition(ctx, key, StateWatching, reason,
		TransitionOptions{Outcome: OutcomeFailed, Cooldown: cooldown}); err != nil {
		logger.Error(err, "rebalance: AI-path transition to Watching (Failed) failed", "profile", profile.Name)
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
		Watches(&corev1.Node{}, r.nodeMapper()).
		WithOptions(controller.Options{MaxConcurrentReconciles: DefaultMaxConcurrentReconciles})

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
