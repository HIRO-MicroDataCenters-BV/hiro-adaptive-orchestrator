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
	return NewStateWriter(c, recorder), recorder
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

	if err := writer.Transition(ctx, key, StateTriggered, "energy verdict flip", TransitionOptions{}); err != nil {
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
	err := writer.Transition(ctx, key, StateDecided, "bogus", TransitionOptions{})
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
		if err := writer.Transition(ctx, key, s, "progressing", TransitionOptions{}); err != nil {
			t.Fatalf("Transition to %s: %v", s, err)
		}
	}

	if err := writer.Transition(ctx, key, StateNoOp, "AI said no action warranted", TransitionOptions{
		Action:   "NoOp",
		Cooldown: 30 * time.Second,
	}); err != nil {
		t.Fatalf("Transition to NoOp: %v", err)
	}

	got := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := writer.client.Get(ctx, key, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	rs := got.Status.RebalancingStatus
	if rs.State != StateNoOp {
		t.Errorf("state = %q, want NoOp", rs.State)
	}
	if rs.CooldownUntil.IsZero() {
		t.Error("cooldownUntil not set on terminal transition")
	}
	if len(rs.RecentDecisions) != 1 {
		t.Fatalf("recentDecisions len = %d, want 1", len(rs.RecentDecisions))
	}
	if rs.RecentDecisions[0].State != StateNoOp || rs.RecentDecisions[0].DecisionID != rs.DecisionID {
		t.Errorf("recentDecisions[0] = %+v, want matching NoOp record", rs.RecentDecisions[0])
	}
}

func TestStateWriter_Transition_RecentDecisionsTrimmed(t *testing.T) {
	profile := testProfile("profile-d")
	writer, _ := newTestWriter(t, profile)
	ctx := context.Background()
	key := types.NamespacedName{Name: "profile-d"}

	for i := range MaxRecentDecisions + 3 {
		if err := writer.Transition(ctx, key, StateTriggered, "cycle", TransitionOptions{}); err != nil {
			t.Fatalf("cycle %d Triggered: %v", i, err)
		}
		if err := writer.Transition(ctx, key, StateEvaluating, "cycle", TransitionOptions{}); err != nil {
			t.Fatalf("cycle %d Evaluating: %v", i, err)
		}
		if err := writer.Transition(ctx, key, StateNoOp, "cycle", TransitionOptions{}); err != nil {
			t.Fatalf("cycle %d NoOp: %v", i, err)
		}
	}

	got := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := writer.client.Get(ctx, key, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Status.RebalancingStatus.RecentDecisions) != MaxRecentDecisions {
		t.Errorf("recentDecisions len = %d, want %d", len(got.Status.RebalancingStatus.RecentDecisions), MaxRecentDecisions)
	}
}
