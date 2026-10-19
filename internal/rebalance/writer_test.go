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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

func newTestWriter(t *testing.T, profile *orchestrationv1alpha1.OrchestrationProfile) (*StateWriter, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&orchestrationv1alpha1.OrchestrationProfile{}).
		WithObjects(profile).
		Build()
	// Buffer generously — several tests emit many transitions without
	// draining the channel, and record.FakeRecorder.Eventf blocks (not
	// drops) once the buffer fills.
	recorder := record.NewFakeRecorder(256)
	return NewStateWriter(c, c, recorder, HistoryConfig{}), recorder
}

func testProfile(name string) *orchestrationv1alpha1.OrchestrationProfile {
	return &orchestrationv1alpha1.OrchestrationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: orchestrationv1alpha1.OrchestrationProfileSpec{
			ApplicationRef: orchestrationv1alpha1.ApplicationReference{
				Kind: "Deployment", Name: "app", Namespace: "default",
			},
			Placement: orchestrationv1alpha1.PlacementSpec{Strategy: "Balanced"},
		},
	}
}

func TestStateWriter_Transition_FirstCycle(t *testing.T) {
	profile := testProfile("profile-a")
	writer, recorder := newTestWriter(t, profile)
	ctx := context.Background()
	key := types.NamespacedName{Name: "profile-a"}

	if _, err := writer.Transition(ctx, key, StateTriggered, "energy verdict flip", TransitionOptions{}); err != nil {
		t.Fatalf("Transition to Triggered: %v", err)
	}

	got := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := writer.client.Get(ctx, key, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.RebalancingStatus.State != StateTriggered {
		t.Errorf("state = %q, want Triggered", got.Status.RebalancingStatus.State)
	}
	if got.Status.RebalancingStatus.DecisionID == "" {
		t.Error("decisionId not assigned on entering Triggered")
	}
	if got.Status.RebalancingStatus.StartedAt.IsZero() {
		t.Error("startedAt not set on entering Triggered")
	}

	select {
	case ev := <-recorder.Events:
		if ev == "" {
			t.Error("expected non-empty event")
		}
	default:
		t.Error("expected an event to be recorded")
	}
}

func TestStateWriter_Transition_InvalidRejected(t *testing.T) {
	profile := testProfile("profile-b")
	writer, _ := newTestWriter(t, profile)
	ctx := context.Background()
	key := types.NamespacedName{Name: "profile-b"}

	// Skipping straight to Decided without going through Triggered/Evaluating.
	_, err := writer.Transition(ctx, key, StateDecided, "bogus", TransitionOptions{})
	if err == nil {
		t.Fatal("expected error for invalid transition, got nil")
	}
}

func TestStateWriter_Transition_TerminalRecordsHistoryAndCooldown(t *testing.T) {
	profile := testProfile("profile-c")
	writer, _ := newTestWriter(t, profile)
	ctx := context.Background()
	key := types.NamespacedName{Name: "profile-c"}

	steps := []orchestrationv1alpha1.RebalancingStateType{StateTriggered, StateEvaluating}
	for _, s := range steps {
		if _, err := writer.Transition(ctx, key, s, "progressing", TransitionOptions{}); err != nil {
			t.Fatalf("Transition to %s: %v", s, err)
		}
	}

	if _, err := writer.Transition(ctx, key, StateWatching, "AI said no action warranted", TransitionOptions{
		Action:   orchestrationv1alpha1.RebalanceActionNoOp,
		Outcome:  OutcomeNoOp,
		Cooldown: 30 * time.Second,
	}); err != nil {
		t.Fatalf("Transition to Watching (NoOp): %v", err)
	}

	got := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := writer.client.Get(ctx, key, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	rs := got.Status.RebalancingStatus
	if rs.State != StateWatching {
		t.Errorf("state = %q, want Watching", rs.State)
	}
	if rs.CooldownUntil.IsZero() {
		t.Error("cooldownUntil not set on terminal transition")
	}
	if len(rs.History) != 1 {
		t.Fatalf("history len = %d, want 1", len(rs.History))
	}
	if e := rs.History[0]; e.Outcome != OutcomeNoOp || e.Count != 0 || !e.FirstAt.IsZero() || e.Reason != string(ReasonOther) {
		t.Errorf("history[0] = %+v, want one unmerged NoOp (no count/firstAt) with the Other reason", e)
	}
}

// runRebalanceCycle drives Triggered -> Evaluating -> Watching(outcome) for
// writer, returning the persisted status after the terminal write. Shared by
// the escalation tests below, which each need several full cycles in a row.
func runRebalanceCycle(t *testing.T, writer *StateWriter, key types.NamespacedName, outcome orchestrationv1alpha1.RebalanceOutcome) orchestrationv1alpha1.RebalancingStatus {
	t.Helper()
	ctx := context.Background()
	if _, err := writer.Transition(ctx, key, StateTriggered, "cycle", TransitionOptions{}); err != nil {
		t.Fatalf("Transition to Triggered: %v", err)
	}
	if _, err := writer.Transition(ctx, key, StateEvaluating, "cycle", TransitionOptions{}); err != nil {
		t.Fatalf("Transition to Evaluating: %v", err)
	}
	rs, err := writer.Transition(ctx, key, StateWatching, "cycle result", TransitionOptions{Outcome: outcome})
	if err != nil {
		t.Fatalf("Transition to Watching (%s): %v", outcome, err)
	}
	return rs
}

func TestStateWriter_Transition_EscalatesAfterConsecutiveFailures(t *testing.T) {
	profile := testProfile("profile-escalate-a")
	profile.Spec.Rebalancing.EscalationThreshold = 2
	writer, recorder := newTestWriter(t, profile)
	key := types.NamespacedName{Name: "profile-escalate-a"}

	rs := runRebalanceCycle(t, writer, key, OutcomeFailed)
	if rs.ConsecutiveFailures != 1 || rs.Escalated {
		t.Fatalf("after 1st Failed: consecutiveFailures=%d escalated=%v, want 1/false", rs.ConsecutiveFailures, rs.Escalated)
	}
	if rs.History[0].Outcome != OutcomeFailed {
		t.Errorf("history[0].Outcome = %q, want Failed (not yet promoted)", rs.History[0].Outcome)
	}

	rs = runRebalanceCycle(t, writer, key, OutcomeDeferred)
	if rs.ConsecutiveFailures != 2 || !rs.Escalated {
		t.Fatalf("after 2nd Failed/Deferred: consecutiveFailures=%d escalated=%v, want 2/true", rs.ConsecutiveFailures, rs.Escalated)
	}
	if rs.History[0].Outcome != OutcomeEscalated {
		t.Errorf("history[0].Outcome = %q, want Escalated once the threshold is crossed", rs.History[0].Outcome)
	}
	if rs.EscalatedReason == "" {
		t.Error("escalatedReason not set")
	}

	// Escalation must be loud — a Warning event, same as Failed/Rejected/Deferred.
	var sawWarning bool
	for {
		select {
		case ev := <-recorder.Events:
			if strings.HasPrefix(ev, "Warning") {
				sawWarning = true
			}
		default:
			if !sawWarning {
				t.Error("expected at least one Warning event across the two cycles")
			}
			return
		}
	}
}

func TestStateWriter_Transition_NonFailureOutcomeResetsConsecutiveFailures(t *testing.T) {
	profile := testProfile("profile-escalate-b")
	writer, _ := newTestWriter(t, profile)
	key := types.NamespacedName{Name: "profile-escalate-b"}

	rs := runRebalanceCycle(t, writer, key, OutcomeFailed)
	if rs.ConsecutiveFailures != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1", rs.ConsecutiveFailures)
	}

	rs = runRebalanceCycle(t, writer, key, OutcomeNoOp)
	if rs.ConsecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d after NoOp, want reset to 0", rs.ConsecutiveFailures)
	}
	if rs.Escalated {
		t.Error("escalated = true, want false — NoOp should never escalate")
	}
}

func TestStateWriter_Transition_DirectEscalateBypassesThreshold(t *testing.T) {
	profile := testProfile("profile-escalate-c")
	profile.Spec.Rebalancing.EscalationThreshold = 5 // high — only a direct request should trigger it here
	writer, _ := newTestWriter(t, profile)
	key := types.NamespacedName{Name: "profile-escalate-c"}
	ctx := context.Background()

	if _, err := writer.Transition(ctx, key, StateTriggered, "cycle", TransitionOptions{}); err != nil {
		t.Fatalf("Transition to Triggered: %v", err)
	}
	if _, err := writer.Transition(ctx, key, StateEvaluating, "cycle", TransitionOptions{}); err != nil {
		t.Fatalf("Transition to Evaluating: %v", err)
	}
	rs, err := writer.Transition(ctx, key, StateWatching, "AI said escalate", TransitionOptions{
		Action: orchestrationv1alpha1.RebalanceActionEscalate, Outcome: OutcomeEscalated,
	})
	if err != nil {
		t.Fatalf("Transition to Watching (Escalated): %v", err)
	}
	if !rs.Escalated {
		t.Error("escalated = false, want true after a direct AI-requested Escalate")
	}
	if rs.ConsecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d, want 0 — a direct escalate isn't a failure streak", rs.ConsecutiveFailures)
	}
	if rs.History[0].Outcome != OutcomeEscalated {
		t.Errorf("history[0].Outcome = %q, want Escalated", rs.History[0].Outcome)
	}
}

func TestStateWriter_Transition_UnsetThresholdUsesDefault(t *testing.T) {
	profile := testProfile("profile-escalate-d") // EscalationThreshold left unset
	writer, _ := newTestWriter(t, profile)
	key := types.NamespacedName{Name: "profile-escalate-d"}

	var rs orchestrationv1alpha1.RebalancingStatus
	for i := range DefaultEscalationThreshold - 1 {
		rs = runRebalanceCycle(t, writer, key, OutcomeFailed)
		if rs.Escalated {
			t.Fatalf("escalated too early, on cycle %d of %d", i+1, DefaultEscalationThreshold)
		}
	}
	rs = runRebalanceCycle(t, writer, key, OutcomeFailed)
	if !rs.Escalated {
		t.Errorf("expected escalation once consecutiveFailures reached DefaultEscalationThreshold (%d)", DefaultEscalationThreshold)
	}
}

// endCycle runs one Triggered -> Evaluating -> Watching cycle with opts.
func endCycle(t *testing.T, writer *StateWriter, key types.NamespacedName, reason string, opts TransitionOptions) {
	t.Helper()
	ctx := context.Background()
	if _, err := writer.Transition(ctx, key, StateTriggered, "cycle", TransitionOptions{}); err != nil {
		t.Fatalf("Triggered: %v", err)
	}
	if _, err := writer.Transition(ctx, key, StateEvaluating, "cycle", TransitionOptions{}); err != nil {
		t.Fatalf("Evaluating: %v", err)
	}
	if _, err := writer.Transition(ctx, key, StateWatching, reason, opts); err != nil {
		t.Fatalf("Watching: %v", err)
	}
}

func historyOf(t *testing.T, writer *StateWriter, key types.NamespacedName) []orchestrationv1alpha1.HistoryEntry {
	t.Helper()
	got := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := writer.client.Get(context.Background(), key, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got.Status.RebalancingStatus.History
}

func TestStateWriter_History_MergesRepeats(t *testing.T) {
	writer, _ := newTestWriter(t, testProfile("profile-d"))
	key := types.NamespacedName{Name: "profile-d"}

	for range 5 {
		endCycle(t, writer, key, "nothing to do", TransitionOptions{Outcome: OutcomeNoOp, Reason: ReasonNoChange})
	}
	endCycle(t, writer, key, "below threshold", TransitionOptions{
		Action: orchestrationv1alpha1.RebalanceActionMove, Outcome: OutcomeRejected, Reason: ReasonBelowThreshold,
		Change: orchestrationv1alpha1.HistoryEntry{PodName: "p1", TargetNode: "n2"},
	})

	h := historyOf(t, writer, key)
	if len(h) != 2 {
		t.Fatalf("history = %+v, want 2 entries (5 NoOps merged + 1 Move)", h)
	}
	if h[0].Action != orchestrationv1alpha1.RebalanceActionMove || h[0].TargetNode != "n2" {
		t.Errorf("history[0] = %+v, want the newest (Move) first with its facts", h[0])
	}
	if h[1].Count != 5 || h[1].FirstAt.After(h[1].At.Time) {
		t.Errorf("history[1] = %+v, want 5 merged NoOps with firstAt <= at", h[1])
	}
}

func TestStateWriter_History_DifferentFactsDoNotMerge(t *testing.T) {
	writer, _ := newTestWriter(t, testProfile("profile-f"))
	key := types.NamespacedName{Name: "profile-f"}

	for _, node := range []string{"n1", "n2"} {
		endCycle(t, writer, key, "blocked by PDB", TransitionOptions{
			Action: orchestrationv1alpha1.RebalanceActionMove, Outcome: OutcomeDeferred, Reason: ReasonPDBBlocked,
			Change: orchestrationv1alpha1.HistoryEntry{SourceNode: "n0", TargetNode: node},
		})
	}
	if h := historyOf(t, writer, key); len(h) != 2 {
		t.Errorf("history = %+v, want two separate Moves: different targets must not merge", h)
	}
}

func TestStateWriter_History_EnactedNeverMerges(t *testing.T) {
	writer, _ := newTestWriter(t, testProfile("profile-j"))
	key := types.NamespacedName{Name: "profile-j"}

	for range 2 {
		endCycle(t, writer, key, "scaled", TransitionOptions{
			Action: orchestrationv1alpha1.RebalanceActionAdjustReplicas, Outcome: OutcomeEnacted, Reason: ReasonScaled,
			Change: orchestrationv1alpha1.HistoryEntry{FromReplicas: 1, ToReplicas: 2},
		})
	}
	h := historyOf(t, writer, key)
	if len(h) != 2 {
		t.Fatalf("history = %+v, want two separate Enacted entries", h)
	}
	if h[0].Count != 0 || !h[0].FirstAt.IsZero() {
		t.Errorf("history[0] = %+v, want no count/firstAt on an unmerged entry", h[0])
	}
}

func TestStateWriter_History_OtherReasonNeverMerges(t *testing.T) {
	writer, _ := newTestWriter(t, testProfile("profile-k"))
	key := types.NamespacedName{Name: "profile-k"}

	for _, msg := range []string{"first problem", "second problem"} {
		endCycle(t, writer, key, msg, TransitionOptions{Outcome: OutcomeFailed}) // no code → Other
	}
	h := historyOf(t, writer, key)
	if len(h) != 2 || h[1].Message != "first problem" {
		t.Errorf("history = %+v, want two separate Other entries, keeping the first message", h)
	}
}

func TestStateWriter_History_CappedAndTruncated(t *testing.T) {
	profile := testProfile("profile-g")
	writer, _ := newTestWriter(t, profile)
	writer.history = HistoryConfig{MaxEntries: 3, MessageMax: 10}.WithDefaults()
	key := types.NamespacedName{Name: "profile-g"}

	for i := range 5 {
		endCycle(t, writer, key, "a long reason that will be cut", TransitionOptions{
			Action: orchestrationv1alpha1.RebalanceActionAdjustReplicas, Outcome: OutcomeEnacted, Reason: ReasonScaled,
			Change: orchestrationv1alpha1.HistoryEntry{ToReplicas: int32(i + 1)},
		})
	}
	h := historyOf(t, writer, key)
	if len(h) != 3 || h[0].ToReplicas != 5 {
		t.Fatalf("history = %+v, want the newest 3 entries", h)
	}
	if got := []rune(h[0].Message); len(got) != 10 {
		t.Errorf("message = %q, want it truncated to 10 characters", h[0].Message)
	}
}

func TestStateWriter_History_SkipHistory(t *testing.T) {
	writer, _ := newTestWriter(t, testProfile("profile-h"))
	key := types.NamespacedName{Name: "profile-h"}

	endCycle(t, writer, key, "dry-run", TransitionOptions{Outcome: OutcomeEnacted, SkipHistory: true})
	if h := historyOf(t, writer, key); len(h) != 0 {
		t.Errorf("history = %+v, want none for a skipped (dry-run) cycle", h)
	}
}

func TestStateWriter_Transition_ClearedEscalationDropsReason(t *testing.T) {
	profile := testProfile("profile-i")
	profile.Status.RebalancingStatus.EscalatedReason = "old: AI unavailable"
	writer, _ := newTestWriter(t, profile)
	key := types.NamespacedName{Name: "profile-i"}

	endCycle(t, writer, key, "nothing to do", TransitionOptions{Outcome: OutcomeNoOp, Reason: ReasonNoChange})

	got := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := writer.client.Get(context.Background(), key, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r := got.Status.RebalancingStatus.EscalatedReason; r != "" {
		t.Errorf("escalatedReason = %q, want it cleared when not escalated", r)
	}
}
