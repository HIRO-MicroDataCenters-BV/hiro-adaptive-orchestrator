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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
)

var testEAOGVK = schema.GroupVersionKind{Group: "eas.hiro.io", Version: "v1", Kind: "EnergyAwareOrchestration"}

func testAppRef() orchestrationv1alpha1.ApplicationReference {
	return orchestrationv1alpha1.ApplicationReference{
		Kind: "Deployment", Name: "app-a", Namespace: "default",
	}
}

func testProfileWithConditions(conditions ...orchestrationv1alpha1.TriggerCondition) *orchestrationv1alpha1.OrchestrationProfile {
	return &orchestrationv1alpha1.OrchestrationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-a"},
		Spec: orchestrationv1alpha1.OrchestrationProfileSpec{
			ApplicationRef: testAppRef(),
			Placement:      orchestrationv1alpha1.PlacementSpec{Strategy: "Balanced"},
			Rebalancing: orchestrationv1alpha1.RebalancingSpec{
				Enabled:           true,
				TriggerConditions: conditions,
			},
		},
	}
}

// testDeployment creates a Deployment matching testAppRef with a selector
// that testPod's labels satisfy.
func testDeployment() *appsv1.Deployment {
	labels := map[string]string{"app": "app-a"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app-a", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}

func testPod(name, nodeName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{"app": "app-a"},
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func testEAO(action, reason string, sufficient *bool) *unstructured.Unstructured {
	eao := &unstructured.Unstructured{}
	eao.SetGroupVersionKind(testEAOGVK)
	eao.SetName("eao-a")
	eao.SetNamespace("default")
	_ = unstructured.SetNestedMap(eao.Object, map[string]any{
		"name": "app-a", "namespace": "default", "kind": "Deployment",
	}, "spec", "applicationRef")
	decision := map[string]any{"action": action, "reason": reason}
	_ = unstructured.SetNestedMap(eao.Object, decision, "status", "decision")
	if sufficient != nil {
		_ = unstructured.SetNestedField(eao.Object, *sufficient, "status", "energyMetrics", "sufficient")
	}
	return eao
}

// heldEAO is a Scheduled EAO with no energy now whose slot starts at slotStart: a Hold while it's ahead.
func heldEAO(reason string, slotStart time.Time) *unstructured.Unstructured {
	eao := testEAO("Scheduled", reason, new(false))
	_ = unstructured.SetNestedField(eao.Object, slotStart.UTC().Format(time.RFC3339),
		"status", "decision", "scheduledSlot", "slotStart")
	return eao
}

// newTestTriggerEvaluator builds a TriggerEvaluator wired to a fake client
// seeded with the given objects and a fake metrics clientset with no
// NodeMetrics registered (CPU/Memory checks soft-fail to "no match" unless
// a test constructs its own pressure evaluator).
func newTestTriggerEvaluator(t *testing.T, objs ...client.Object) *TriggerEvaluator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding appsv1 scheme: %v", err)
	}
	if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding orchestration scheme: %v", err)
	}
	// Register the EAO GVK (and its List kind) so the fake client can serve
	// unstructured List calls for it — in a real cluster this is whatever
	// CRD is installed; here there's no CRD, so the scheme has to know it.
	scheme.AddKnownTypeWithName(testEAOGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: testEAOGVK.Group, Version: testEAOGVK.Version, Kind: testEAOGVK.Kind + "List"},
		&unstructured.UnstructuredList{},
	)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	c := builder.Build()

	metricsClient := metricsfake.NewSimpleClientset() //nolint:staticcheck // see pressure_test.go
	pressure := NewNodePressureEvaluator(c, metricsClient, 0.90)
	return NewTriggerEvaluator(c, placementserver.NewEAOSettings(testEAOGVK), pressure)
}

func TestTriggerEvaluator_EnergyHold(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	eao := heldEAO("optional: scheduled for a later slot", time.Now().Add(time.Hour))
	evaluator := newTestTriggerEvaluator(t, testDeployment(), eao)

	matched, result, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !matched || result.Condition != TriggerEnergyThreshold {
		t.Fatalf("matched=%v condition=%q, want true/%q", matched, result.Condition, TriggerEnergyThreshold)
	}
	if !strings.Contains(result.Reason, "optional: scheduled for a later slot; held until ") {
		t.Errorf("reason = %q, want the EAO's reason and when the hold ends", result.Reason)
	}
	if result.BypassAction != "" {
		t.Errorf("BypassAction = %q, want empty — a hold is a genuine problem signal, needs AI", result.BypassAction)
	}
}

func TestTriggerEvaluator_EnergyDelayed(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	eao := testEAO("Delayed", "waiting for cheaper slot", nil)
	evaluator := newTestTriggerEvaluator(t, testDeployment(), eao)

	matched, result, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !matched || result.Condition != TriggerEnergyThreshold {
		t.Fatalf("matched=%v condition=%q, want true/%q", matched, result.Condition, TriggerEnergyThreshold)
	}
	if result.BypassAction != "" {
		t.Errorf("BypassAction = %q, want empty — Delayed is a genuine problem signal, needs AI", result.BypassAction)
	}
}

func TestTriggerEvaluator_EnergyVerdicts(t *testing.T) {
	pending := func() *corev1.Pod { return testPod("app-a-pending", "", corev1.PodPending) }
	cases := []struct {
		name        string
		eao         *unstructured.Unstructured
		holdActions []string // nil keeps the default mapping
		wantMatch   bool
		wantBypass  string
	}{
		{name: "slot ahead: the pending pod is not retried early",
			eao: heldEAO("", time.Now().Add(time.Hour)), wantMatch: true},
		{name: "slot started: the pending pod is retried",
			eao: heldEAO("", time.Now().Add(-time.Minute)), wantMatch: true, wantBypass: ActionRetryPendingSchedule},
		{name: "Waiting is an EAO failure, not an energy signal",
			eao: testEAO("Waiting", "eao failed", new(false))},
		{name: "DeployImmediately runs even when sufficient is false",
			eao: testEAO("DeployImmediately", "", new(false)), wantMatch: true, wantBypass: ActionRetryPendingSchedule},
		{name: "a renamed hold action is a setting change",
			eao: testEAO("Paused", "", nil), holdActions: []string{"Paused"}, wantMatch: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evaluator := newTestTriggerEvaluator(t, testDeployment(), tc.eao, pending())
			if tc.holdActions != nil {
				evaluator.eaoSettings.VerdictConfig.HoldActions = tc.holdActions
			}
			matched, result, err := evaluator.Evaluate(context.Background(), testProfileWithConditions(TriggerEnergyThreshold))
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if matched != tc.wantMatch || result.BypassAction != tc.wantBypass {
				t.Errorf("matched=%v bypass=%q, want %v/%q", matched, result.BypassAction, tc.wantMatch, tc.wantBypass)
			}
		})
	}
}

func TestTriggerEvaluator_EnergyWindowReachedWithPendingPod(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	eao := testEAO("DeployImmediately", "", new(true))
	pending := testPod("app-a-pending", "", corev1.PodPending)
	evaluator := newTestTriggerEvaluator(t, testDeployment(), eao, pending)

	matched, result, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !matched || result.Condition != TriggerEnergyThreshold {
		t.Fatalf("matched=%v condition=%q, want true/%q", matched, result.Condition, TriggerEnergyThreshold)
	}
	if result.Reason == "" {
		t.Error("expected non-empty reason")
	}
	if result.BypassAction != ActionRetryPendingSchedule {
		t.Errorf("BypassAction = %q, want %q — no AI consultation needed for a pending-pod retry",
			result.BypassAction, ActionRetryPendingSchedule)
	}
}

// TestTriggerEvaluator_FreshPendingPodNotYetRetryEligible covers the fix for
// a live-observed regression: once Reconcile stopped gating the
// RetryPendingSchedule bypass on cooldown, a retry's own deletion produced a
// brand-new replacement pod that was itself briefly Pending — which
// immediately re-matched this same case and retried again before the
// replacement ever got a real chance to schedule, live-locking in a tight
// loop (93 transitions observed in ~1-2s). A pod younger than
// MinPendingPodAge must not be considered retry-eligible yet.
func TestTriggerEvaluator_FreshPendingPodNotYetRetryEligible(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	eao := testEAO("DeployImmediately", "", new(true))
	pending := testPod("app-a-pending", "", corev1.PodPending)
	pending.CreationTimestamp = metav1.NewTime(time.Now())
	evaluator := newTestTriggerEvaluator(t, testDeployment(), eao, pending)

	matched, _, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if matched {
		t.Error("matched = true, want false — a pod pending for under MinPendingPodAge should not retry yet")
	}
}

// TestTriggerEvaluator_ScheduledPendingPodNotRetryEligible covers a second
// live-observed regression: a pod that's Pending but already has a
// NodeName (scheduled, just hasn't started its container yet — ordinary
// startup latency, unrelated to the energy gate) was still matching
// hasPendingPod, but retryPendingSchedule's own selection (retry_enactor.go)
// requires NodeName == "" and finds nothing — so the bypass fired
// repeatedly and unproductively (cooldown-exempt) until the pod left
// Pending on its own. hasPendingPod must require the same "truly
// unscheduled" criteria retryPendingSchedule actually acts on.
func TestTriggerEvaluator_ScheduledPendingPodNotRetryEligible(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	eao := testEAO("DeployImmediately", "", new(true))
	pending := testPod("app-a-pending", "node-a", corev1.PodPending)
	evaluator := newTestTriggerEvaluator(t, testDeployment(), eao, pending)

	matched, _, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if matched {
		t.Error("matched = true, want false — a Pending pod that's already scheduled isn't retry-eligible")
	}
}

func TestTriggerEvaluator_EnergyOKNoPendingPod(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	eao := testEAO("DeployImmediately", "", new(true))
	running := testPod("app-a-1", "node-a", corev1.PodRunning)
	evaluator := newTestTriggerEvaluator(t, testDeployment(), eao, running)

	matched, _, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if matched {
		t.Error("matched = true, want false when energy is fine and nothing is pending")
	}
}

func TestTriggerEvaluator_EAOUnavailableSoftFails(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	evaluator := newTestTriggerEvaluator(t, testDeployment()) // no EAO seeded

	matched, _, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate should soft-fail, not error: %v", err)
	}
	if matched {
		t.Error("matched = true, want false when no EAO exists for the app")
	}
}

func TestTriggerEvaluator_NodeFailure(t *testing.T) {
	profile := testProfileWithConditions(TriggerNodeFailure)
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}
	evaluator := newTestTriggerEvaluator(t, testDeployment(), pod, node)

	matched, result, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !matched || result.Condition != TriggerNodeFailure {
		t.Fatalf("matched=%v condition=%q, want true/%q", matched, result.Condition, TriggerNodeFailure)
	}
	if result.Reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestTriggerEvaluator_NodeFailureIgnoresTerminatingPod(t *testing.T) {
	profile := testProfileWithConditions(TriggerNodeFailure)
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	pod.Finalizers = []string{"test/keep"} // the fake client needs one to hold a deleting object
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionUnknown},
			},
		},
	}
	evaluator := newTestTriggerEvaluator(t, testDeployment(), pod, node)

	matched, _, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if matched {
		t.Error("matched = true, want false: the only pod on the dead node is already being deleted")
	}
}

// notReadySince is a node that went NotReady at since.
func notReadySince(name string, since time.Time) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionUnknown, LastTransitionTime: metav1.NewTime(since),
		}}},
	}
}

func TestTriggerEvaluator_NodeHealthyNoMatch(t *testing.T) {
	profile := testProfileWithConditions(TriggerNodeFailure)
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	evaluator := newTestTriggerEvaluator(t, testDeployment(), pod, node)

	matched, _, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if matched {
		t.Error("matched = true, want false when node is Ready")
	}
}

func TestTriggerEvaluator_PeriodicReview(t *testing.T) {
	profile := testProfileWithConditions(TriggerPeriodicReview)
	evaluator := newTestTriggerEvaluator(t, testDeployment())

	matched, result, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !matched || result.Condition != TriggerPeriodicReview {
		t.Fatalf("matched=%v condition=%q, want true/%q", matched, result.Condition, TriggerPeriodicReview)
	}
}

func TestTriggerEvaluator_NoConditionsDeclaredNoMatch(t *testing.T) {
	profile := testProfileWithConditions() // none declared
	evaluator := newTestTriggerEvaluator(t, testDeployment())

	matched, _, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if matched {
		t.Error("matched = true, want false when no trigger conditions are declared")
	}
}

func TestTriggerEvaluator_FirstMatchWins(t *testing.T) {
	// NodeFailure declared before PeriodicReview; NodeFailure doesn't match
	// (node healthy) so PeriodicReview (always true) should be the one returned.
	profile := testProfileWithConditions(TriggerNodeFailure, TriggerPeriodicReview)
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	evaluator := newTestTriggerEvaluator(t, testDeployment(), pod, node)

	matched, result, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !matched || result.Condition != TriggerPeriodicReview {
		t.Fatalf("matched=%v condition=%q, want true/%q (NodeFailure shouldn't match, PeriodicReview should)",
			matched, result.Condition, TriggerPeriodicReview)
	}
}
