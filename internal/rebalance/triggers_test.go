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
)

var testEAOGVK = schema.GroupVersionKind{Group: "eas.hiro.io", Version: "v1", Kind: "EnergyAwareOrchestration"}

func testAppRef() orchestrationv1alpha1.ApplicationReference {
	return orchestrationv1alpha1.ApplicationReference{
		Kind: "Deployment", Name: "app-a", Namespace: "default",
	}
}

func testProfileWithConditions(conditions ...string) *orchestrationv1alpha1.OrchestrationProfile {
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

func boolPtr(b bool) *bool { return &b }

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
	return NewTriggerEvaluator(c, testEAOGVK, pressure)
}

func TestTriggerEvaluator_EnergyInsufficient(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	eao := testEAO("DeployImmediately", "", boolPtr(false))
	evaluator := newTestTriggerEvaluator(t, testDeployment(), eao)

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
	if result.BypassAction != "" {
		t.Errorf("BypassAction = %q, want empty — insufficient energy is a genuine problem signal, needs AI", result.BypassAction)
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

func TestTriggerEvaluator_EnergyWindowReachedWithPendingPod(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	eao := testEAO("DeployImmediately", "", boolPtr(true))
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

func TestTriggerEvaluator_EnergyOKNoPendingPod(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	eao := testEAO("DeployImmediately", "", boolPtr(true))
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

func TestTriggerEvaluator_Scheduled(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	evaluator := newTestTriggerEvaluator(t, testDeployment())

	matched, result, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !matched || result.Condition != TriggerScheduled {
		t.Fatalf("matched=%v condition=%q, want true/%q", matched, result.Condition, TriggerScheduled)
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
	// NodeFailure declared before Scheduled; NodeFailure doesn't match
	// (node healthy) so Scheduled (always true) should be the one returned.
	profile := testProfileWithConditions(TriggerNodeFailure, TriggerScheduled)
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
	if !matched || result.Condition != TriggerScheduled {
		t.Fatalf("matched=%v condition=%q, want true/%q (NodeFailure shouldn't match, Scheduled should)",
			matched, result.Condition, TriggerScheduled)
	}
}
