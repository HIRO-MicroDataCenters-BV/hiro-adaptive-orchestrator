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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/metrics"
)

// EventReasonRebalanceTransition is the Kubernetes Event reason emitted for
// every rebalance state transition.
const EventReasonRebalanceTransition = "RebalanceTransition"

// DefaultEscalationThreshold is how many consecutive Failed/Deferred
// terminal outcomes a profile may accumulate before Transition promotes the
// current cycle's outcome to Escalated and pauses the rebalance loop for it.
const DefaultEscalationThreshold = 3

// TransitionOptions carries the extra fields a transition may set. All are
// optional — zero values are simply not written.
type TransitionOptions struct {
	// Action is the AI-returned action being processed (e.g. "Move", "NoOp").
	Action orchestrationv1alpha1.RebalanceAction

	// Details carries free-form, state-specific context (e.g. target node,
	// improvement score, dry-run marker).
	Details string

	// Trigger is the condition that started the cycle; read only when to == StateTriggered.
	Trigger orchestrationv1alpha1.TriggerCondition

	// Outcome is required whenever to == StateWatching — it is how the
	// cycle that just ended is recorded in history. Ignored on
	// every other transition.
	Outcome orchestrationv1alpha1.RebalanceOutcome

	// Cooldown, when non-zero, sets CooldownUntil = now + Cooldown. Only
	// meaningful when to == StateWatching.
	Cooldown time.Duration

	// Reason is the history code for a terminal transition; empty records ReasonOther.
	Reason HistoryReason

	// DecidedBy is who answered the decision request; empty when no agent was asked.
	DecidedBy string

	// Change carries the facts of what the cycle changed (Move/AdjustReplicas/AdjustResources
	// fields); only those fields are read, the writer fills the rest of the history entry.
	Change orchestrationv1alpha1.HistoryEntry

	// SkipHistory leaves the history untouched, for cycles that changed nothing real (dry-run).
	SkipHistory bool
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
	history  HistoryConfig
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
//
// Zero fields of history use the History defaults.
func NewStateWriter(c client.Client, reader client.Reader, recorder record.EventRecorder, history HistoryConfig) *StateWriter {
	return &StateWriter{client: c, reader: reader, recorder: recorder, history: history.WithDefaults()}
}

// HistoryConfig returns the effective history bounds, for building request summaries.
func (w *StateWriter) HistoryConfig() HistoryConfig {
	return w.history
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
			rs.Trigger = opts.Trigger
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

		effectiveOutcome := opts.Outcome

		if to == StateWatching {
			if opts.Outcome == "" {
				return fmt.Errorf("rebalance writer: transition to Watching for profile %s requires an Outcome",
					key.Name)
			}

			// Track the Failed/Deferred streak, and escalate — pausing the
			// rebalance loop for this profile until a human clears it — once
			// it crosses the profile's threshold. A direct AI-requested
			// Escalate (opts.Outcome already Escalated) bypasses the streak
			// check entirely: the AI's own judgment is honored immediately,
			// not gated on a repeat-failure count.
			switch opts.Outcome {
			case OutcomeFailed, OutcomeDeferred:
				rs.ConsecutiveFailures++
			default:
				rs.ConsecutiveFailures = 0
			}
			threshold := profile.Spec.Rebalancing.EscalationThreshold
			if threshold <= 0 {
				threshold = DefaultEscalationThreshold
			}
			historyReason := opts.Reason
			if opts.Outcome != OutcomeEscalated && rs.ConsecutiveFailures >= threshold {
				effectiveOutcome = OutcomeEscalated
				historyReason = ReasonFailureStreak
				rs.Escalated = true
				rs.EscalatedReason = fmt.Sprintf("%d consecutive Failed/Deferred outcomes, latest: %s",
					rs.ConsecutiveFailures, reason)
			} else if opts.Outcome == OutcomeEscalated {
				rs.Escalated = true
				rs.EscalatedReason = reason
			}

			if opts.Cooldown > 0 {
				rs.CooldownUntil = metav1.NewTime(now.Add(opts.Cooldown))
			}
			if !opts.SkipHistory {
				if historyReason == "" {
					historyReason = ReasonOther
				}
				entry := opts.Change
				entry.Action = rs.Action
				entry.Trigger = rs.Trigger
				entry.Outcome = effectiveOutcome
				entry.Reason = string(historyReason)
				entry.Message = reason
				entry.DecidedBy = opts.DecidedBy
				entry.At = now
				rs.History = recordHistory(rs.History, entry, w.history, now.Time)
			}

			escalatedStatus := metav1.ConditionFalse
			escalatedReason := "NotEscalated"
			escalatedMessage := "rebalance loop is running normally"
			if rs.Escalated {
				escalatedStatus = metav1.ConditionTrue
				escalatedReason = "Escalated"
				escalatedMessage = rs.EscalatedReason
			} else {
				rs.EscalatedReason = "" // stale once escalation was cleared
			}
			apimeta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
				Type:    orchestrationv1alpha1.ConditionRebalancingEscalated,
				Status:  escalatedStatus,
				Reason:  escalatedReason,
				Message: escalatedMessage,
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
		metrics.RebalanceTransitionsTotal.WithLabelValues(
			string(emptyAsNone(from)), string(to), string(rs.Action), string(opts.Outcome),
		).Inc()
		result = profile.Status.RebalancingStatus
		return nil
	})

	return result, err
}

// emitEvent records a Kubernetes Event describing the transition. A terminal
// write (to == StateWatching) surfaces as Warning when the outcome is
// Failed, Rejected, Deferred, or Escalated, so it stands out in `kubectl
// describe`; every other transition is Normal.
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
	case OutcomeFailed, OutcomeRejected, OutcomeDeferred, OutcomeEscalated:
		eventType = corev1.EventTypeWarning
	}
	w.recorder.Eventf(profile, eventType, EventReasonRebalanceTransition,
		"%s -> %s: %s (decisionId=%s)", emptyAsNone(from), to, reason, decisionID,
	)
}

// emptyAsNone renders the unset state as "None" for readable log/event output.
func emptyAsNone(s orchestrationv1alpha1.RebalancingStateType) orchestrationv1alpha1.RebalancingStateType {
	if s == stateNone {
		return "None"
	}
	return s
}
