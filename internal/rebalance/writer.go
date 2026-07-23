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

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// MaxRecentDecisions bounds the rolling history kept on profile status.
const MaxRecentDecisions = 10

// EventReasonRebalanceTransition is the Kubernetes Event reason emitted for
// every rebalance state transition.
const EventReasonRebalanceTransition = "RebalanceTransition"

// TransitionOptions carries the extra fields a transition may set. All are
// optional — zero values are simply not written.
type TransitionOptions struct {
	// Action is the AI-returned action being processed (e.g. "Move", "NoOp").
	Action orchestrationv1alpha1.RebalanceAction

	// Details carries free-form, state-specific context (e.g. target node,
	// improvement score, dry-run marker).
	Details string

	// Outcome is required whenever to == StateWatching — it is how the
	// cycle that just ended is recorded in RecentDecisions. Ignored on
	// every other transition.
	Outcome orchestrationv1alpha1.RebalanceOutcome

	// Cooldown, when non-zero, sets CooldownUntil = now + Cooldown. Only
	// meaningful when to == StateWatching.
	Cooldown time.Duration
}

// StateWriter is the single component permitted to mutate
// status.rebalancingStatus. Every stage of the rebalance engine — detection,
// decision, enaction — must transition state through this writer so that
// every change is validated against the state machine, persisted
// consistently, and surfaced as a Kubernetes Event.
//
// StateWriter is safe for concurrent use.
type StateWriter struct {
	client   client.Client
	reader   client.Reader
	recorder record.EventRecorder
}

// NewStateWriter creates a StateWriter.
//
// reader MUST be a non-cached reader (mgr.GetAPIReader(), not mgr.GetClient())
// when multiple Transition calls are chained back-to-back within the same
// Reconcile invocation (as enactBypassAction in reconciler.go does, and as
// the future Evaluating/Decided/Enacting sequence will too): the manager's
// cached client only reflects a write after an asynchronous watch round-trip
// from the API server, so a Transition immediately following another would
// read stale state from the cache — even though the prior write already
// landed on the server — and reject a transition that's actually valid.
// Status().Update always goes straight to the API server regardless, so
// only the read side needs the uncached reader.
func NewStateWriter(c client.Client, reader client.Reader, recorder record.EventRecorder) *StateWriter {
	return &StateWriter{client: c, reader: reader, recorder: recorder}
}

// Transition moves the OrchestrationProfile identified by key from its
// current rebalancing state to `to`, validating the move against the state
// machine, persisting it via the status subresource, and emitting a
// Kubernetes Event. It returns the freshly-persisted RebalancingStatus so
// callers chaining multiple transitions in the same Reconcile (e.g.
// enactBypassAction, evaluateWithAI) can carry state forward without another
// Get — avoiding the same cache-staleness bug class the uncached reader on
// this writer already exists to prevent.
//
// Re-fetches the profile and retries on write conflicts, so callers don't
// need their own retry loop — this also makes the writer idempotent across
// operator restarts: re-applying the same transition after a crash either
// succeeds (state hadn't changed) or is rejected as invalid (state already
// moved on), never double-applies a side effect.
func (w *StateWriter) Transition(
	ctx context.Context,
	key types.NamespacedName,
	to orchestrationv1alpha1.RebalancingStateType,
	reason string,
	opts TransitionOptions,
) (orchestrationv1alpha1.RebalancingStatus, error) {
	logger := logf.FromContext(ctx).WithName("rebalance-writer")

	var result orchestrationv1alpha1.RebalancingStatus

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		profile := &orchestrationv1alpha1.OrchestrationProfile{}
		if err := w.reader.Get(ctx, key, profile); err != nil {
			return fmt.Errorf("rebalance writer: fetching profile %s: %w", key.Name, err)
		}

		from := profile.Status.RebalancingStatus.State
		if !IsValidTransition(from, to) {
			return fmt.Errorf("rebalance writer: invalid transition %q -> %q for profile %s",
				emptyAsNone(from), to, key.Name)
		}

		now := metav1.Now()
		rs := &profile.Status.RebalancingStatus

		if to == StateTriggered {
			// Starting a fresh cycle — assign a new correlation ID and reset
			// the per-cycle fields left over from the previous one.
			rs.DecisionID = uuid.NewString()
			rs.StartedAt = now
			rs.Action = ""
			rs.Details = ""
		}

		rs.State = to
		rs.Reason = reason
		rs.LastTransitionAt = now
		if opts.Action != "" {
			rs.Action = opts.Action
		}
		if opts.Details != "" {
			rs.Details = opts.Details
		}

		if to == StateWatching {
			if opts.Outcome == "" {
				return fmt.Errorf("rebalance writer: transition to Watching for profile %s requires an Outcome",
					key.Name)
			}
			if opts.Cooldown > 0 {
				rs.CooldownUntil = metav1.NewTime(now.Add(opts.Cooldown))
			}
			rs.RecentDecisions = prependDecision(rs.RecentDecisions, orchestrationv1alpha1.RebalanceDecision{
				DecisionID:       rs.DecisionID,
				Outcome:          opts.Outcome,
				Action:           rs.Action,
				Reason:           reason,
				Details:          rs.Details,
				StartedAt:        rs.StartedAt,
				LastTransitionAt: now,
			})
		}

		if err := w.client.Status().Update(ctx, profile); err != nil {
			return err // conflict errors are retried by RetryOnConflict; others bubble up
		}

		logger.Info("rebalance: state transition",
			"profile", key.Name, "from", emptyAsNone(from), "to", to,
			"reason", reason, "decisionId", rs.DecisionID,
		)
		w.emitEvent(profile, from, to, opts.Outcome, reason, rs.DecisionID)
		result = profile.Status.RebalancingStatus
		return nil
	})

	return result, err
}

// emitEvent records a Kubernetes Event describing the transition. A terminal
// write (to == StateWatching) surfaces as Warning when the outcome is
// Failed, Rejected, or Deferred, so it stands out in `kubectl describe`;
// every other transition is Normal.
func (w *StateWriter) emitEvent(
	profile *orchestrationv1alpha1.OrchestrationProfile,
	from, to orchestrationv1alpha1.RebalancingStateType,
	outcome orchestrationv1alpha1.RebalanceOutcome,
	reason, decisionID string,
) {
	if w.recorder == nil {
		return
	}
	eventType := corev1.EventTypeNormal
	switch outcome {
	case OutcomeFailed, OutcomeRejected, OutcomeDeferred:
		eventType = corev1.EventTypeWarning
	}
	w.recorder.Eventf(profile, eventType, EventReasonRebalanceTransition,
		"%s -> %s: %s (decisionId=%s)", emptyAsNone(from), to, reason, decisionID,
	)
}

// prependDecision inserts a new decision at the front of the rolling window
// and trims it to MaxRecentDecisions.
func prependDecision(
	existing []orchestrationv1alpha1.RebalanceDecision,
	next orchestrationv1alpha1.RebalanceDecision,
) []orchestrationv1alpha1.RebalanceDecision {
	updated := append([]orchestrationv1alpha1.RebalanceDecision{next}, existing...)
	if len(updated) > MaxRecentDecisions {
		updated = updated[:MaxRecentDecisions]
	}
	return updated
}

// emptyAsNone renders the unset state as "None" for readable log/event output.
func emptyAsNone(s orchestrationv1alpha1.RebalancingStateType) orchestrationv1alpha1.RebalancingStateType {
	if s == stateNone {
		return "None"
	}
	return s
}
