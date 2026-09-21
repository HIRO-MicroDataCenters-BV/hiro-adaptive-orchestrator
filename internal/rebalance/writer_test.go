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
	return NewStateWriter(c, c, recorder, 10), recorder
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
	if len(rs.RecentDecisions) != 1 {
		t.Fatalf("recentDecisions len = %d, want 1", len(rs.RecentDecisions))
	}
	if rs.RecentDecisions[0].Outcome != OutcomeNoOp || rs.RecentDecisions[0].DecisionID != rs.DecisionID {
		t.Errorf("recentDecisions[0] = %+v, want matching NoOp record", rs.RecentDecisions[0])
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
	if rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Errorf("recentDecisions[0].Outcome = %q, want Failed (not yet promoted)", rs.RecentDecisions[0].Outcome)
	}

	rs = runRebalanceCycle(t, writer, key, OutcomeDeferred)
	if rs.ConsecutiveFailures != 2 || !rs.Escalated {
		t.Fatalf("after 2nd Failed/Deferred: consecutiveFailures=%d escalated=%v, want 2/true", rs.ConsecutiveFailures, rs.Escalated)
	}
	if rs.RecentDecisions[0].Outcome != OutcomeEscalated {
		t.Errorf("recentDecisions[0].Outcome = %q, want Escalated once the threshold is crossed", rs.RecentDecisions[0].Outcome)
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
	if rs.RecentDecisions[0].Outcome != OutcomeEscalated {
		t.Errorf("recentDecisions[0].Outcome = %q, want Escalated", rs.RecentDecisions[0].Outcome)
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

func TestStateWriter_Transition_RecentDecisionsTrimmed(t *testing.T) {
	profile := testProfile("profile-d")
	writer, _ := newTestWriter(t, profile)
	ctx := context.Background()
	key := types.NamespacedName{Name: "profile-d"}

	for i := 0; i < writer.maxRecentDecisions+3; i++ {
		if _, err := writer.Transition(ctx, key, StateTriggered, "cycle", TransitionOptions{}); err != nil {
			t.Fatalf("cycle %d Triggered: %v", i, err)
		}
		if _, err := writer.Transition(ctx, key, StateEvaluating, "cycle", TransitionOptions{}); err != nil {
			t.Fatalf("cycle %d Evaluating: %v", i, err)
		}
		if _, err := writer.Transition(ctx, key, StateWatching, "cycle",
			TransitionOptions{Outcome: OutcomeNoOp}); err != nil {
			t.Fatalf("cycle %d Watching (NoOp): %v", i, err)
		}
	}

	got := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := writer.client.Get(ctx, key, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Status.RebalancingStatus.RecentDecisions) != writer.maxRecentDecisions {
		t.Errorf("recentDecisions len = %d, want %d", len(got.Status.RebalancingStatus.RecentDecisions), writer.maxRecentDecisions)
	}
}
