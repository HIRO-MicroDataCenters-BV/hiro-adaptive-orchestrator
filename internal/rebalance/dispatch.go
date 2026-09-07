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

	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
)

// actionDispatcher carries a successful AI response the rest of the way to a
// terminal transition (directly, or via Decided/Enacting for actions that
// need to do something first). Registered per orchestrationv1alpha1.RebalanceAction
// in actionDispatchers rather than switched on inline in dispatchDecision, so
// a future enactor (AdjustResources, AdjustReplicas, Defer, Escalate) is a
// new map entry, not a change to dispatchDecision itself.
type actionDispatcher func(
	r *Reconciler,
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	resp *placementserver.RebalanceDecisionResponse,
	cooldown time.Duration,
)

// actionDispatchers is the action -> dispatcher registry consulted by
// dispatchDecision. An action with no entry here is handled defensively —
// see dispatchDecision.
var actionDispatchers = map[orchestrationv1alpha1.RebalanceAction]actionDispatcher{
	orchestrationv1alpha1.RebalanceActionNoOp:           dispatchNoOp,
	orchestrationv1alpha1.RebalanceActionMove:           dispatchMove,
	orchestrationv1alpha1.RebalanceActionAdjustReplicas: dispatchScale,
}

// dispatchDecision applies actionDispatchers to a successful AI response.
// An action with no registered dispatcher (including today's Reject/Defer,
// which exist as values but have no enactor yet, and anything genuinely
// unrecognized) is treated as a processing error rather than guessed at —
// Watching + Outcome Failed, same as enact's default case for an unknown
// bypass action.
func (r *Reconciler) dispatchDecision(
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	resp *placementserver.RebalanceDecisionResponse,
) {
	logger := logf.FromContext(ctx)
	cooldown := time.Duration(profile.Spec.Rebalancing.CooldownSeconds) * time.Second

	dispatch, ok := actionDispatchers[resp.Action]
	if !ok {
		reason := fmt.Sprintf("no dispatcher for action %q: %s", resp.Action, resp.Reason)
		if _, err := r.Writer.Transition(ctx, key, StateWatching, reason,
			TransitionOptions{Outcome: OutcomeFailed, Cooldown: cooldown}); err != nil {
			logger.Error(err, "rebalance: dispatch transition to Watching (Failed, no dispatcher) failed", "profile", profile.Name)
		}
		return
	}
	dispatch(r, ctx, key, profile, resp, cooldown)
}

// dispatchNoOp exits Evaluating straight to Watching — NoOp needs no
// enactor, so it skips Decided/Enacting entirely (both are valid direct
// exits from Evaluating in validTransitions).
func dispatchNoOp(
	r *Reconciler,
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	resp *placementserver.RebalanceDecisionResponse,
	cooldown time.Duration,
) {
	logger := logf.FromContext(ctx)
	if _, err := r.Writer.Transition(ctx, key, StateWatching, resp.Reason,
		TransitionOptions{Action: resp.Action, Outcome: OutcomeNoOp, Cooldown: cooldown}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Watching (NoOp) failed", "profile", profile.Name)
	}
}

// dispatchMove applies the improvement-threshold guardrail, and for an
// accepted recommendation drives Decided -> Enacting -> the Move enactor ->
// Watching. A recommendation below threshold, or missing the fields Move
// needs, exits straight to Watching without ever reaching Decided — only an
// accepted, well-formed Move goes through the enactor.
func dispatchMove(
	r *Reconciler,
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	resp *placementserver.RebalanceDecisionResponse,
	cooldown time.Duration,
) {
	logger := logf.FromContext(ctx)

	threshold := r.ImprovementThreshold
	if threshold <= 0 {
		threshold = DefaultImprovementThreshold
	}
	if resp.Improvement < threshold {
		reason := fmt.Sprintf("improvement %.2f below threshold %.2f: %s", resp.Improvement, threshold, resp.Reason)
		if _, err := r.Writer.Transition(ctx, key, StateWatching, reason,
			TransitionOptions{Action: resp.Action, Outcome: OutcomeRejected, Cooldown: cooldown}); err != nil {
			logger.Error(err, "rebalance: dispatch transition to Watching (Rejected) failed", "profile", profile.Name)
		}
		return
	}

	if resp.PodName == "" || resp.TargetNode == "" {
		reason := fmt.Sprintf("Move response missing podName/targetNode: %s", resp.Reason)
		if _, err := r.Writer.Transition(ctx, key, StateWatching, reason,
			TransitionOptions{Action: resp.Action, Outcome: OutcomeFailed, Cooldown: cooldown}); err != nil {
			logger.Error(err, "rebalance: dispatch transition to Watching (Failed, malformed Move) failed", "profile", profile.Name)
		}
		return
	}

	details := fmt.Sprintf("podName=%s targetNode=%s improvement=%.2f", resp.PodName, resp.TargetNode, resp.Improvement)
	decided, err := r.Writer.Transition(ctx, key, StateDecided, resp.Reason,
		TransitionOptions{Action: resp.Action, Details: details})
	if err != nil {
		logger.Error(err, "rebalance: dispatch transition to Decided failed", "profile", profile.Name)
		return
	}

	if profile.Spec.Rebalancing.DryRun {
		dispatchMoveDryRun(r, ctx, key, profile, resp, cooldown)
		return
	}

	// Cluster-wide throttle: an accepted Move sits in Decided — visible in
	// status, not yet disruptive — until a rate-limit token is available.
	// This is a single limiter shared by every profile (see MoveRateLimiter's
	// doc comment), so it bounds the fleet's total eviction rate regardless
	// of how many individually-cooled-down profiles want to act at once.
	rateWaitTimeout := r.MoveRateWaitTimeout
	if rateWaitTimeout <= 0 {
		rateWaitTimeout = DefaultMoveRateWaitTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, rateWaitTimeout)
	err = r.MoveRateLimiter.Wait(waitCtx)
	cancel()
	if err != nil {
		// Deliberately NOT applying cooldown here, unlike every other Failed
		// exit in this file: the profile didn't lose on its merits, it lost a
		// scheduling race against other profiles' Moves. cooldownSeconds can
		// be minutes; the limiter's own budget refills in seconds, so the
		// normal cooldown would leave a still-valid Move idle long after
		// capacity actually freed up. Safe to skip: the very next reconcile
		// this status write triggers calls Wait() again, and Wait() itself
		// blocks (up to MoveRateWaitTimeout) before failing — that blocking
		// IS the pacing, so there's no risk of the sub-second self-triggering
		// loop a missing cooldown caused elsewhere (see hasPendingPod's doc
		// comment in triggers.go). MoveRateWaitTimeout defaults generously
		// (see its doc comment) specifically so this path is rarely taken at
		// all — most waits succeed within the original call, without ever
		// needing a second AI consultation.
		reason := fmt.Sprintf("cluster-wide move rate limit: %v", err)
		if _, tErr := r.Writer.Transition(ctx, key, StateWatching, reason,
			TransitionOptions{Action: resp.Action, Outcome: OutcomeFailed}); tErr != nil {
			logger.Error(tErr, "rebalance: dispatch transition to Watching (Failed, rate limit) failed", "profile", profile.Name)
		}
		return
	}

	if _, err := r.Writer.Transition(ctx, key, StateEnacting,
		fmt.Sprintf("enacting move of %s to %s", resp.PodName, resp.TargetNode),
		TransitionOptions{Action: resp.Action}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Enacting failed", "profile", profile.Name)
		return
	}

	result, err := moveEnactor(ctx, r.Client, r.DecisionStore, profile,
		resp.PodName, resp.TargetNode, resp.Reason, decided.DecisionID, r.MoveActionTimeout)
	if err != nil {
		if _, tErr := r.Writer.Transition(ctx, key, StateWatching, err.Error(),
			TransitionOptions{Action: resp.Action, Outcome: OutcomeFailed, Cooldown: cooldown}); tErr != nil {
			logger.Error(tErr, "rebalance: dispatch transition to Watching (Failed, move enactor error) failed", "profile", profile.Name)
		}
		return
	}

	if _, err := r.Writer.Transition(ctx, key, StateWatching, result.reason,
		TransitionOptions{Action: resp.Action, Outcome: result.outcome, Cooldown: cooldown}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Watching failed",
			"profile", profile.Name, "outcome", result.outcome)
	}
}

// dispatchMoveDryRun completes a Move that already cleared every guardrail
// but is running with spec.rebalancing.dryRun: true: Decided -> Enacting ->
// Watching, without calling moveEnactor or waiting on the cluster-wide rate
// limiter — nothing is actually evicted, so nothing should compete for real
// eviction capacity, and there's no real outcome to poll for.
func dispatchMoveDryRun(
	r *Reconciler,
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	resp *placementserver.RebalanceDecisionResponse,
	cooldown time.Duration,
) {
	logger := logf.FromContext(ctx)

	if _, err := r.Writer.Transition(ctx, key, StateEnacting,
		fmt.Sprintf("dry-run: would enact move of %s to %s", resp.PodName, resp.TargetNode),
		TransitionOptions{Action: resp.Action}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Enacting (dry-run) failed", "profile", profile.Name)
		return
	}

	details := fmt.Sprintf("[dry-run] would move %s to %s (improvement=%.2f): %s",
		resp.PodName, resp.TargetNode, resp.Improvement, resp.Reason)
	if _, err := r.Writer.Transition(ctx, key, StateWatching, "dry-run: no side effects applied",
		TransitionOptions{Action: resp.Action, Outcome: OutcomeEnacted, Details: details, Cooldown: cooldown}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Watching (dry-run Enacted) failed", "profile", profile.Name)
	}
}

// dispatchScale applies the same improvement-threshold guardrail as Move,
// plus a replica-bounds guardrail (see resolveReplicaBounds — an existing
// HorizontalPodAutoscaler on the workload wins over the package/env
// defaults), and for an accepted recommendation drives Decided -> Enacting ->
// scaleEnactor -> Watching. Unlike Move, AdjustReplicas isn't gated by any
// cluster-wide rate limiter or DecisionStore — scaling a Deployment/
// StatefulSet's replica count doesn't disrupt a specific running pod the way
// an eviction does.
func dispatchScale(
	r *Reconciler,
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	resp *placementserver.RebalanceDecisionResponse,
	cooldown time.Duration,
) {
	logger := logf.FromContext(ctx)

	threshold := r.ImprovementThreshold
	if threshold <= 0 {
		threshold = DefaultImprovementThreshold
	}
	if resp.Improvement < threshold {
		reason := fmt.Sprintf("improvement %.2f below threshold %.2f: %s", resp.Improvement, threshold, resp.Reason)
		if _, err := r.Writer.Transition(ctx, key, StateWatching, reason,
			TransitionOptions{Action: resp.Action, Outcome: OutcomeRejected, Cooldown: cooldown}); err != nil {
			logger.Error(err, "rebalance: dispatch transition to Watching (Rejected) failed", "profile", profile.Name)
		}
		return
	}

	if resp.TargetReplicas <= 0 {
		reason := fmt.Sprintf("AdjustReplicas response missing/invalid targetReplicas: %s", resp.Reason)
		if _, err := r.Writer.Transition(ctx, key, StateWatching, reason,
			TransitionOptions{Action: resp.Action, Outcome: OutcomeFailed, Cooldown: cooldown}); err != nil {
			logger.Error(err, "rebalance: dispatch transition to Watching (Failed, malformed AdjustReplicas) failed", "profile", profile.Name)
		}
		return
	}

	min, max, boundsSource := resolveReplicaBounds(ctx, r.Client, profile, r.MinReplicas, r.MaxReplicas)
	if resp.TargetReplicas < min || resp.TargetReplicas > max {
		reason := fmt.Sprintf("targetReplicas %d outside [%d, %d] (%s): %s",
			resp.TargetReplicas, min, max, boundsSource, resp.Reason)
		if _, err := r.Writer.Transition(ctx, key, StateWatching, reason,
			TransitionOptions{Action: resp.Action, Outcome: OutcomeRejected, Cooldown: cooldown}); err != nil {
			logger.Error(err, "rebalance: dispatch transition to Watching (Rejected, out of bounds) failed", "profile", profile.Name)
		}
		return
	}

	details := fmt.Sprintf("targetReplicas=%d improvement=%.2f bounds=[%d,%d] (%s)",
		resp.TargetReplicas, resp.Improvement, min, max, boundsSource)
	if _, err := r.Writer.Transition(ctx, key, StateDecided, resp.Reason,
		TransitionOptions{Action: resp.Action, Details: details}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Decided failed", "profile", profile.Name)
		return
	}

	if profile.Spec.Rebalancing.DryRun {
		dispatchScaleDryRun(r, ctx, key, profile, resp, cooldown)
		return
	}

	if _, err := r.Writer.Transition(ctx, key, StateEnacting,
		fmt.Sprintf("enacting scale to %d replicas", resp.TargetReplicas),
		TransitionOptions{Action: resp.Action}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Enacting failed", "profile", profile.Name)
		return
	}

	result, err := scaleEnactor(ctx, r.Client, profile, resp.TargetReplicas, resp.Reason, r.ScaleActionTimeout)
	if err != nil {
		if _, tErr := r.Writer.Transition(ctx, key, StateWatching, err.Error(),
			TransitionOptions{Action: resp.Action, Outcome: OutcomeFailed, Cooldown: cooldown}); tErr != nil {
			logger.Error(tErr, "rebalance: dispatch transition to Watching (Failed, scale enactor error) failed", "profile", profile.Name)
		}
		return
	}

	if _, err := r.Writer.Transition(ctx, key, StateWatching, result.reason,
		TransitionOptions{Action: resp.Action, Outcome: result.outcome, Cooldown: cooldown}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Watching failed",
			"profile", profile.Name, "outcome", result.outcome)
	}
}

// dispatchScaleDryRun mirrors dispatchMoveDryRun: Decided -> Enacting ->
// Watching(Enacted), without ever calling scaleEnactor — no Spec.Replicas
// patch, so nothing to poll for either.
func dispatchScaleDryRun(
	r *Reconciler,
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	resp *placementserver.RebalanceDecisionResponse,
	cooldown time.Duration,
) {
	logger := logf.FromContext(ctx)

	if _, err := r.Writer.Transition(ctx, key, StateEnacting,
		fmt.Sprintf("dry-run: would enact scale to %d replicas", resp.TargetReplicas),
		TransitionOptions{Action: resp.Action}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Enacting (dry-run) failed", "profile", profile.Name)
		return
	}

	details := fmt.Sprintf("[dry-run] would scale to %d replicas (improvement=%.2f): %s",
		resp.TargetReplicas, resp.Improvement, resp.Reason)
	if _, err := r.Writer.Transition(ctx, key, StateWatching, "dry-run: no side effects applied",
		TransitionOptions{Action: resp.Action, Outcome: OutcomeEnacted, Details: details, Cooldown: cooldown}); err != nil {
		logger.Error(err, "rebalance: dispatch transition to Watching (dry-run Enacted) failed", "profile", profile.Name)
	}
}
