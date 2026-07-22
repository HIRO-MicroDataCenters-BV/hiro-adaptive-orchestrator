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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// testProfileIndexField mirrors controller.ProfileByAppRefIndex without
// importing the controller package (see internal/placement-server for the
// same decoupling convention — the index field name is passed in, not
// imported directly).
const testProfileIndexField = ".spec.applicationRef.namespacedName"

// newTestReconciler builds a Reconciler wired to a fake client seeded with
// the given objects, registering testProfileIndexField so profilesByIndexKey
// works the same way it does against the real manager cache.
func newTestReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
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
	scheme.AddKnownTypeWithName(testEAOGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: testEAOGVK.Group, Version: testEAOGVK.Version, Kind: testEAOGVK.Kind + "List"},
		&unstructured.UnstructuredList{},
	)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&orchestrationv1alpha1.OrchestrationProfile{}).
		WithObjects(objs...).
		WithIndex(&orchestrationv1alpha1.OrchestrationProfile{}, testProfileIndexField,
			func(obj client.Object) []string {
				p := obj.(*orchestrationv1alpha1.OrchestrationProfile) //nolint:forcetypeassert
				ref := p.Spec.ApplicationRef
				if ref.Name == "" || ref.Namespace == "" {
					return nil
				}
				return []string{ref.Namespace + "/" + ref.Name}
			},
		).
		Build()

	writer := NewStateWriter(c, c, record.NewFakeRecorder(256))
	metricsClient := metricsfake.NewSimpleClientset() //nolint:staticcheck // see pressure_test.go
	pressure := NewNodePressureEvaluator(c, metricsClient, 0.90)
	evaluator := NewTriggerEvaluator(c, testEAOGVK, pressure)

	return NewReconciler(c, writer, evaluator, testProfileIndexField, 30*time.Second), c
}

func getProfile(t *testing.T, c client.Client, name string) *orchestrationv1alpha1.OrchestrationProfile {
	t.Helper()
	p := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, p); err != nil {
		t.Fatalf("Get profile %s: %v", name, err)
	}
	return p
}

func TestReconciler_ProfileNotFound(t *testing.T) {
	r, _ := newTestReconciler(t)
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "missing"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 for a missing profile", res.RequeueAfter)
	}
}

func TestReconciler_RebalancingDisabledNoOp(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	profile.Spec.Rebalancing.Enabled = false
	r, c := newTestReconciler(t, testDeployment(), profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 when rebalancing disabled", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	if got.Status.RebalancingStatus.State != "" {
		t.Errorf("state = %q, want empty when rebalancing disabled", got.Status.RebalancingStatus.State)
	}
}

func TestReconciler_InCooldownSkipsAndRequeuesAtExpiry(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	profile.Status.RebalancingStatus.CooldownUntil = metav1.NewTime(time.Now().Add(10 * time.Second))
	r, _ := newTestReconciler(t, testDeployment(), profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > 10*time.Second {
		t.Errorf("RequeueAfter = %v, want a positive duration <= 10s (remaining cooldown)", res.RequeueAfter)
	}
}

func TestReconciler_CycleInFlightSkipsDetection(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	profile.Status.RebalancingStatus.State = StateEvaluating // already past Triggered
	r, c := newTestReconciler(t, testDeployment(), profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 when a cycle is already in flight", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	if got.Status.RebalancingStatus.State != StateEvaluating {
		t.Errorf("state = %q, want unchanged Evaluating", got.Status.RebalancingStatus.State)
	}
}

func TestReconciler_NoTriggerMatchRequeuesWithoutTransition(t *testing.T) {
	profile := testProfileWithConditions(TriggerNodeFailure) // no pods/nodes seeded to match
	r, c := newTestReconciler(t, testDeployment(), profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want DetectionInterval (30s)", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	if got.Status.RebalancingStatus.State != "" {
		t.Errorf("state = %q, want empty when no trigger condition matches", got.Status.RebalancingStatus.State)
	}
}

func TestReconciler_ScheduledTriggerTransitionsToTriggered(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	r, c := newTestReconciler(t, testDeployment(), profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want DetectionInterval (30s)", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateTriggered {
		t.Fatalf("state = %q, want Triggered", rs.State)
	}
	if rs.DecisionID == "" {
		t.Error("expected decisionId to be assigned")
	}
	if rs.Reason == "" {
		t.Error("expected a non-empty reason")
	}
}

// TestReconciler_EnergyVerdictFlipTriggersWorkload is the Story 26
// acceptance-criteria integration test: a synthetic energy-verdict flip
// (EAO now reports insufficient energy) causes Reconcile to transition the
// profile to Triggered, end to end through TriggerEvaluator and StateWriter
// — not just at the evaluator-unit level (see triggers_test.go).
func TestReconciler_EnergyVerdictFlipTriggersWorkload(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	insufficient := false
	eao := testEAO("DeployImmediately", "", &insufficient)
	r, c := newTestReconciler(t, testDeployment(), eao, profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want DetectionInterval (30s)", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateTriggered {
		t.Fatalf("state = %q, want Triggered after energy verdict flip", rs.State)
	}
	if rs.Reason == "" {
		t.Error("expected a non-empty reason describing the energy trigger")
	}
}

// TestReconciler_EnergyPendingRetryBypassesToEnacted covers the case we
// discussed: EAO's window reopened (action=DeployImmediately) while a pod
// for the app is still Pending and unscheduled. TriggerEvaluator signals
// BypassAction (no AI consultation needed), and Reconcile should drive the
// full Triggered -> Evaluating -> Decided -> Enacting -> Enacted sequence in
// one pass, actually deleting the pending pod so its controller recreates it.
func TestReconciler_EnergyPendingRetryBypassesToEnacted(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	sufficient := true
	eao := testEAO("DeployImmediately", "", &sufficient)
	pending := testPod("app-a-pending", "", corev1.PodPending)
	r, c := newTestReconciler(t, testDeployment(), eao, pending, profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want DetectionInterval (30s)", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateEnacted {
		t.Fatalf("state = %q, want Enacted — bypass path should reach a terminal state in one Reconcile call", rs.State)
	}
	if rs.Action != ActionRetryPendingSchedule {
		t.Errorf("action = %q, want %q", rs.Action, ActionRetryPendingSchedule)
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].State != StateEnacted {
		t.Errorf("recentDecisions = %+v, want one Enacted entry", rs.RecentDecisions)
	}

	// The whole point: the pending pod should actually be gone, so its
	// owning controller (Deployment) creates a replacement.
	err = c.Get(context.Background(), types.NamespacedName{Name: "app-a-pending", Namespace: "default"}, &corev1.Pod{})
	if err == nil {
		t.Error("expected the pending pod to have been deleted by the bypass enactor")
	}
}
