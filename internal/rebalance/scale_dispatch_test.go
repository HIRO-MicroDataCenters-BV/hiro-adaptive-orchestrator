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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
)

// testScalableDeployment builds the "app-a" Deployment testAppRef points at,
// starting at 1 replica, with a pre-set ReadyReplicas. Pre-setting
// ReadyReplicas to whatever a test's mock AI response will recommend lets
// scaleEnactor's awaitReadyReplicas poll succeed on its first read against
// the fake client — which never runs a real rollout — without needing any
// timing dependency, the same trick TestReconciler_MoveAboveThresholdReachesEnacting
// avoids needing by not asserting past Enacting at all.
func testScalableDeployment(readyReplicas int32) *appsv1.Deployment {
	labels := map[string]string{"app": "app-a"}
	specReplicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app-a", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &specReplicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: readyReplicas},
	}
}

func testHPA(name string, minReplicas, maxReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment", Name: "app-a", APIVersion: "apps/v1",
			},
			MinReplicas: &minReplicas,
			MaxReplicas: maxReplicas,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: "cpu",
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: ptrInt32(80),
					},
				},
			}},
		},
	}
}

// testKEDAOwnedHPA is testHPA with an ownerReference back to a ScaledObject,
// the same way KEDA's own generated HPA carries one — see
// resolveReplicaBounds' doc comment on why that's what dispatchScale checks.
func testKEDAOwnedHPA(name, scaledObjectName string, minReplicas, maxReplicas int32) *autoscalingv2.HorizontalPodAutoscaler {
	hpa := testHPA(name, minReplicas, maxReplicas)
	hpa.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "keda.sh/v1alpha1",
		Kind:       "ScaledObject",
		Name:       scaledObjectName,
		UID:        "test-uid",
	}}
	return hpa
}

func ptrInt32(v int32) *int32 { return &v }

func adjustReplicasResponse(targetReplicas int32, improvement float64) placementserver.RebalanceDecisionResponse {
	return placementserver.RebalanceDecisionResponse{
		RequestID:      "test",
		Action:         orchestrationv1alpha1.RebalanceActionAdjustReplicas,
		TargetReplicas: targetReplicas,
		Improvement:    improvement,
		Reason:         "mock: demand increased",
	}
}

func TestReconciler_ScaleAboveThresholdReachesEnacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustReplicasResponse(3, 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testScalableDeployment(3) // already reports 3 ready — fake client, no real rollout
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching", rs.State)
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeEnacted {
		t.Fatalf("recentDecisions = %+v, want one Enacted-outcome entry", rs.RecentDecisions)
	}
	if rs.RecentDecisions[0].Action != orchestrationv1alpha1.RebalanceActionAdjustReplicas {
		t.Errorf("recentDecisions[0].Action = %q, want AdjustReplicas", rs.RecentDecisions[0].Action)
	}

	updated := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a", Namespace: "default"}, updated); err != nil {
		t.Fatalf("Get deployment: %v", err)
	}
	if updated.Spec.Replicas == nil || *updated.Spec.Replicas != 3 {
		t.Errorf("Spec.Replicas = %v, want 3", updated.Spec.Replicas)
	}
}

func TestReconciler_ScaleBelowThresholdRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustReplicasResponse(3, 5)) // below DefaultImprovementThreshold (20)
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testScalableDeployment(1)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeRejected {
		t.Fatalf("recentDecisions = %+v, want one Rejected-outcome entry", rs.RecentDecisions)
	}
}

func TestReconciler_ScaleMalformedTargetReplicasFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustReplicasResponse(0, 50)) // missing/invalid targetReplicas
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testScalableDeployment(1)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Fatalf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
}

// TestReconciler_ScaleOutsideDefaultBoundsRejected covers the fallback
// guardrail: no HPA exists for app-a, so DefaultMaxReplicas (10) applies, and
// a recommendation above it must be rejected rather than enacted.
func TestReconciler_ScaleOutsideDefaultBoundsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustReplicasResponse(50, 50)) // above DefaultMaxReplicas (10)
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testScalableDeployment(1)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeRejected {
		t.Fatalf("recentDecisions = %+v, want one Rejected-outcome entry", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Reason, "default bounds") {
		t.Errorf("reason = %q, want it to mention default bounds", rs.RecentDecisions[0].Reason)
	}
}

// TestReconciler_ScaleHonoursHPABounds proves an HPA targeting app-a wins
// over the package/env default bounds: 15 is above DefaultMaxReplicas (10)
// but within this HPA's [2, 20], so it must be enacted, not rejected.
func TestReconciler_ScaleHonoursHPABounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustReplicasResponse(15, 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testScalableDeployment(15)
	hpa := testHPA("app-a-hpa", 2, 20)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, hpa, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeEnacted {
		t.Fatalf("recentDecisions = %+v, want one Enacted-outcome entry (HPA bounds should have allowed 15)", rs.RecentDecisions)
	}
}

// TestReconciler_ScaleDefersToKEDAManagedWorkload covers Story 36: a workload
// whose HPA was created by a KEDA ScaledObject (identified by ownerReferences)
// must never be patched directly — the cycle ends Deferred, and Spec.Replicas
// is left untouched, regardless of how well the recommendation would
// otherwise have cleared every other guardrail.
func TestReconciler_ScaleDefersToKEDAManagedWorkload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustReplicasResponse(3, 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testScalableDeployment(1)
	hpa := testKEDAOwnedHPA("keda-hpa-my-scaledobject", "my-scaledobject", 1, 10)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, hpa, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeDeferred {
		t.Fatalf("recentDecisions = %+v, want one Deferred-outcome entry", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Reason, "KEDA") || !strings.Contains(rs.RecentDecisions[0].Reason, "my-scaledobject") {
		t.Errorf("reason = %q, want it to mention KEDA and the ScaledObject name", rs.RecentDecisions[0].Reason)
	}

	stillOne := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a", Namespace: "default"}, stillOne); err != nil {
		t.Fatalf("Get deployment: %v", err)
	}
	if stillOne.Spec.Replicas == nil || *stillOne.Spec.Replicas != 1 {
		t.Errorf("Spec.Replicas = %v, want unchanged at 1 (KEDA-managed workload must never be patched)", stillOne.Spec.Replicas)
	}
}

// TestReconciler_ScaleDryRunSkipsSideEffects covers dry-run for AdjustReplicas:
// the cycle must still end Enacted with a [dry-run] marker, but Spec.Replicas
// on the actual Deployment must never change.
func TestReconciler_ScaleDryRunSkipsSideEffects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustReplicasResponse(5, 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	profile.Spec.Rebalancing.DryRun = true
	deploy := testScalableDeployment(1)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeEnacted {
		t.Fatalf("recentDecisions = %+v, want one Enacted-outcome entry", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Details, "[dry-run]") {
		t.Errorf("details = %q, want it to carry the dry-run marker", rs.RecentDecisions[0].Details)
	}

	stillOne := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a", Namespace: "default"}, stillOne); err != nil {
		t.Fatalf("Get deployment: %v", err)
	}
	if stillOne.Spec.Replicas == nil || *stillOne.Spec.Replicas != 1 {
		t.Errorf("Spec.Replicas = %v, want unchanged at 1 (dry-run must not patch)", stillOne.Spec.Replicas)
	}
}
