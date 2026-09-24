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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
)

// testResourceContainerName is the container name used by every test in
// this file — dispatchResource looks it up by name, not position.
const testResourceContainerName = "app"

// testResizableDeployment builds the "app-a" Deployment testAppRef points
// at, with a single container named testResourceContainerName. withLimits
// controls whether that container starts with existing CPU/Memory requests —
// TestReconciler_ResourceNoExistingLimitsDeferred needs one with none.
func testResizableDeployment(withLimits bool) *appsv1.Deployment {
	labels := map[string]string{"app": "app-a"}
	container := corev1.Container{Name: testResourceContainerName}
	if withLimits {
		container.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app-a", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
			},
		},
	}
}

// testRunningPod builds one running pod matching testResizableDeployment's
// selector, so tests can exercise resourceEnactor's in-place-resize path
// (resizeRunningPods) in addition to the template patch.
func testRunningPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"app": "app-a"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: testResourceContainerName,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func adjustResourcesResponse(containerName, targetCPU, targetMemory string, improvement float64) placementserver.RebalanceDecisionResponse {
	return placementserver.RebalanceDecisionResponse{
		RequestID:     "test",
		Action:        orchestrationv1alpha1.RebalanceActionAdjustResources,
		ContainerName: containerName,
		TargetCPU:     targetCPU,
		TargetMemory:  targetMemory,
		Improvement:   improvement,
		Reason:        "mock: usage drifted",
	}
}

func TestReconciler_ResourceAboveThresholdReachesEnacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse(testResourceContainerName, "200m", "256Mi", 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testResizableDeployment(true)
	pod := testRunningPod("app-a-1")
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, pod, profile)
	// Short-circuit awaitResizeConvergence's poll loop — the fake client
	// never populates ContainerStatus.Resources, so it would otherwise
	// burn the full DefaultResourceActionTimeout (60s) waiting for a
	// convergence signal that will never come. This test only cares that
	// the cycle reaches Enacted with the template patched, not whether the
	// in-place resize itself is confirmed — see
	// TestReconciler_ResourceInPlaceResizeConfirmedConverged for that.
	r.ResourceActionTimeout = 50 * time.Millisecond

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeEnacted {
		t.Fatalf("recentDecisions = %+v, want one Enacted-outcome entry", rs.RecentDecisions)
	}
	if rs.RecentDecisions[0].Action != orchestrationv1alpha1.RebalanceActionAdjustResources {
		t.Errorf("recentDecisions[0].Action = %q, want AdjustResources", rs.RecentDecisions[0].Action)
	}

	updated := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a", Namespace: "default"}, updated); err != nil {
		t.Fatalf("Get deployment: %v", err)
	}
	got := updated.Spec.Template.Spec.Containers[0].Resources.Requests
	if got.Cpu().String() != "200m" || got.Memory().String() != "256Mi" {
		t.Errorf("template resources = %v, want cpu=200m memory=256Mi", got)
	}
}

// TestReconciler_ResourceInPlaceResizeConfirmedConverged: the running pod's
// status already shows the target resources actually enacted (as a real
// kubelet would report after applying the resize) — awaitResizeConvergence
// should confirm this on its first check (PollUntilContextCancel's
// immediate=true), no waiting needed, and the reason should say so.
func TestReconciler_ResourceInPlaceResizeConfirmedConverged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse(testResourceContainerName, "200m", "256Mi", 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testResizableDeployment(true)
	pod := testRunningPod("app-a-1")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: testResourceContainerName,
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
	}}
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, pod, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeEnacted {
		t.Fatalf("recentDecisions = %+v, want one Enacted-outcome entry", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Reason, "resized 1 running pod(s) in place") {
		t.Errorf("reason = %q, want it to confirm the in-place resize, not just the template patch", rs.RecentDecisions[0].Reason)
	}
}

// TestReconciler_ResourceInPlaceResizeRejectedFallsBackToNextRollout: the
// running pod's status carries a PodResizePending/Infeasible condition —
// resizeRejectedReason must recognize this as a definitive rejection, not
// count the pod as resized, and the cycle still ends Enacted via the
// template's next-rollout guarantee (a rejected in-place resize is not a
// Failed outcome — see resourceEnactor's own doc comment).
func TestReconciler_ResourceInPlaceResizeRejectedFallsBackToNextRollout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse(testResourceContainerName, "200m", "256Mi", 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testResizableDeployment(true)
	pod := testRunningPod("app-a-1")
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodResizePending, Reason: corev1.PodReasonInfeasible, Message: "node has insufficient cpu",
	}}
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, pod, profile)
	r.ResourceActionTimeout = 50 * time.Millisecond // no convergence coming — same short-circuit as the above-threshold test

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeEnacted {
		t.Fatalf("recentDecisions = %+v, want one Enacted-outcome entry (rejected resize is not Failed)", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Reason, "in-place resize unavailable") {
		t.Errorf("reason = %q, want it to note the in-place resize didn't take, not claim success", rs.RecentDecisions[0].Reason)
	}
}

func TestReconciler_ResourceBelowThresholdRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse(testResourceContainerName, "200m", "256Mi", 5)) // below DefaultImprovementThreshold (20)
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testResizableDeployment(true)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeRejected {
		t.Fatalf("recentDecisions = %+v, want one Rejected-outcome entry", rs.RecentDecisions)
	}
}

func TestReconciler_ResourceMalformedMissingFieldsFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse("", "", "", 50)) // missing containerName and targets
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testResizableDeployment(true)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Fatalf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
}

func TestReconciler_ResourceInvalidQuantityFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse(testResourceContainerName, "not-a-quantity", "", 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testResizableDeployment(true)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Fatalf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
}

func TestReconciler_ResourceContainerNotFoundFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse("no-such-container", "200m", "", 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testResizableDeployment(true)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Fatalf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Reason, "no-such-container") {
		t.Errorf("reason = %q, want it to mention the missing container name", rs.RecentDecisions[0].Reason)
	}
}

// TestReconciler_ResourceNoExistingLimitsDeferred covers the guardrail this
// story added beyond the original AC: a container with no CPU/Memory
// requests or limits at all is deliberately left alone rather than having
// new constraints imposed on it.
func TestReconciler_ResourceNoExistingLimitsDeferred(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse(testResourceContainerName, "200m", "256Mi", 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testResizableDeployment(false) // no existing requests/limits
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeDeferred {
		t.Fatalf("recentDecisions = %+v, want one Deferred-outcome entry", rs.RecentDecisions)
	}

	stillUnset := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a", Namespace: "default"}, stillUnset); err != nil {
		t.Fatalf("Get deployment: %v", err)
	}
	if len(stillUnset.Spec.Template.Spec.Containers[0].Resources.Requests) != 0 {
		t.Errorf("Resources.Requests = %v, want still unset (must not impose new constraints)",
			stillUnset.Spec.Template.Spec.Containers[0].Resources.Requests)
	}
}

// TestReconciler_ResourceOutsideDefaultBoundsRejected covers the fallback
// guardrail: no MinCPU/MaxCPU configured on the test Reconciler, so
// DefaultMaxCPU (2) applies, and a recommendation above it must be rejected.
func TestReconciler_ResourceOutsideDefaultBoundsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse(testResourceContainerName, "4", "", 50)) // above DefaultMaxCPU (2)
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	deploy := testResizableDeployment(true)
	r, c := newTestReconcilerWithAgent(t, server.URL, deploy, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeRejected {
		t.Fatalf("recentDecisions = %+v, want one Rejected-outcome entry", rs.RecentDecisions)
	}
}

// TestReconciler_ResourceDryRunSkipsSideEffects covers dry-run for
// AdjustResources: the cycle must still end Enacted with a [dry-run] marker,
// but the actual Deployment template must never change.
func TestReconciler_ResourceDryRunSkipsSideEffects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse(testResourceContainerName, "200m", "256Mi", 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	profile.Spec.Rebalancing.DryRun = true
	deploy := testResizableDeployment(true)
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

	stillOriginal := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a", Namespace: "default"}, stillOriginal); err != nil {
		t.Fatalf("Get deployment: %v", err)
	}
	got := stillOriginal.Spec.Template.Spec.Containers[0].Resources.Requests
	if got.Cpu().String() != "100m" || got.Memory().String() != "128Mi" {
		t.Errorf("template resources = %v, want unchanged at cpu=100m memory=128Mi (dry-run must not patch)", got)
	}
}
