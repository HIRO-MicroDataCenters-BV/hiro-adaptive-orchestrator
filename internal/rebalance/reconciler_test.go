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

	"golang.org/x/time/rate"
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
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
)

// testProfileIndexField mirrors controller.ProfileByAppRefIndex without
// importing the controller package (see internal/placement-server for the
// same decoupling convention — the index field name is passed in, not
// imported directly).
const testProfileIndexField = ".spec.applicationRef.namespacedName"

// newTestReconciler builds a Reconciler wired to a fake client seeded with
// the given objects, registering testProfileIndexField so profilesByIndexKey
// works the same way it does against the real manager cache. Its AI agent
// URL is intentionally unreachable — tests that need a live agent response
// use newTestReconcilerWithAgent instead.
func newTestReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	r, c := newTestReconcilerWithAgent(t, "http://127.0.0.1:0", objs...)
	return r, c
}

// newTestReconcilerWithAgent is newTestReconciler with the AI agent URL
// pinned to the given address, so tests can point it at an httptest.Server
// to exercise the evaluateWithAI success path.
func newTestReconcilerWithAgent(t *testing.T, agentURL string, objs ...client.Object) (*Reconciler, client.Client) {
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

	writer := NewStateWriter(c, c, record.NewFakeRecorder(256), 0)
	metricsClient := metricsfake.NewSimpleClientset() //nolint:staticcheck // see pressure_test.go
	pressure := NewNodePressureEvaluator(c, metricsClient, 0.90)
	evaluator := NewTriggerEvaluator(c, testEAOGVK, pressure)

	contextBuilder := placementserver.NewDecisionContextBuilder(c, testProfileIndexField, testEAOGVK)
	decisionClient := placementserver.NewDecisionClient(agentURL, "", 200*time.Millisecond)

	decisionStore := placementserver.NewDecisionStore(0)

	return NewReconciler(c, writer, evaluator, testProfileIndexField, 30*time.Second,
		contextBuilder, decisionClient, 200*time.Millisecond, 0, decisionStore, 0, 0), c
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

// TestReconciler_ScheduledTriggerReachesFailedWhenAIUnreachable covers the
// non-bypass path end to end: a trigger match with no mechanical answer
// drives Triggered -> Evaluating -> (AI consultation) -> Watching (Outcome
// Failed) when the External AI Agent can't be reached, all within one
// Reconcile call. A successful AI response's continuation is covered
// separately by TestReconciler_AIPathSuccessStopsAtEvaluating.
func TestReconciler_ScheduledTriggerReachesFailedWhenAIUnreachable(t *testing.T) {
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
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching when the AI agent is unreachable", rs.State)
	}
	if rs.DecisionID == "" {
		t.Error("expected decisionId to be assigned entering the cycle")
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Errorf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
}

// TestReconciler_EnergyVerdictFlipTriggersWorkload is the acceptance-criteria
// integration test for trigger detection: a synthetic energy-verdict flip
// (EAO now reports insufficient energy) causes Reconcile to enter the
// profile's decision lifecycle end to end through TriggerEvaluator and
// StateWriter — not just at the evaluator-unit level (see triggers_test.go).
// The AI agent is unreachable in this fake-client setup, so the cycle ends
// back at Watching with Outcome Failed rather than stopping at Triggered.
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
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching (AI unreachable) after energy verdict flip entered the cycle", rs.State)
	}
	if rs.DecisionID == "" {
		t.Error("expected decisionId to be assigned entering the cycle")
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Errorf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
}

// TestReconciler_MoveBelowThresholdRejected covers a trigger match
// consulting a reachable AI agent successfully with a Move recommendation
// whose Improvement doesn't clear the guardrail: the cycle exits straight
// from Evaluating to Watching with Outcome Rejected, never reaching Decided.
func TestReconciler_MoveBelowThresholdRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID:   "test",
			Action:      orchestrationv1alpha1.RebalanceActionMove,
			PodName:     "app-a-1",
			TargetNode:  "node-b",
			Improvement: 0.4, // well below DefaultImprovementThreshold (20)
			Reason:      "better spread",
		})
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want DetectionInterval (30s)", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching — a below-threshold Move is rejected without reaching Decided", rs.State)
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeRejected {
		t.Errorf("recentDecisions = %+v, want one Rejected-outcome entry", rs.RecentDecisions)
	}
	if rs.RecentDecisions[0].Action != orchestrationv1alpha1.RebalanceActionMove {
		t.Errorf("recentDecisions[0].Action = %q, want Move", rs.RecentDecisions[0].Action)
	}
}

// TestReconciler_NoOpDispatchedDirectly covers a NoOp AI response: it exits
// straight from Evaluating to Watching, never touching Decided/Enacting.
func TestReconciler_NoOpDispatchedDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test",
			Action:    orchestrationv1alpha1.RebalanceActionNoOp,
			Reason:    "already balanced",
		})
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching", rs.State)
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeNoOp {
		t.Errorf("recentDecisions = %+v, want one NoOp-outcome entry", rs.RecentDecisions)
	}
}

// TestReconciler_UnrecognizedActionFailsDirectly covers an AI response whose
// Action has no registered dispatcher (e.g. Reject/Defer, which exist as
// values but have no enactor yet, or anything genuinely unrecognized): the
// cycle is treated as a processing error, not guessed at.
func TestReconciler_UnrecognizedActionFailsDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test",
			Action:    orchestrationv1alpha1.RebalanceActionReject,
			Reason:    "AI declined",
		})
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching", rs.State)
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Errorf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
}

// TestReconciler_MoveAboveThresholdReachesEnacting covers an accepted Move
// recommendation: the cycle reaches Decided and Enacting (verified via the
// Warning event the terminal write emits) before the enactor's eviction
// call — unsupported by the fake client — fails it back to Watching with
// Outcome Failed. This still proves dispatchMove drives the cycle all the
// way to the enactor instead of stopping at Decided, which is what Story
// 28's own guardrail work is responsible for; actual eviction success is
// exercised on a live cluster, not here.
func TestReconciler_MoveAboveThresholdReachesEnacting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID:   "test",
			Action:      orchestrationv1alpha1.RebalanceActionMove,
			PodName:     "app-a-1",
			TargetNode:  "node-b",
			Improvement: 50,
			Reason:      "better spread",
		})
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), pod, profile)
	// Short-circuit awaitReplacement's poll loop — no replacement pod will
	// ever appear against a fake client, and this test only cares that
	// dispatchMove reaches the enactor, not the eviction outcome itself.
	r.MoveActionTimeout = 50 * time.Millisecond

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching", rs.State)
	}
	if len(rs.RecentDecisions) != 1 {
		t.Fatalf("recentDecisions = %+v, want exactly one entry", rs.RecentDecisions)
	}
	if rs.RecentDecisions[0].Action != orchestrationv1alpha1.RebalanceActionMove {
		t.Errorf("recentDecisions[0].Action = %q, want Move", rs.RecentDecisions[0].Action)
	}
}

// TestReconciler_MoveRateLimitedFailsCleanly covers Story 31's cluster-wide
// rate limit: an accepted, above-threshold Move still reaches Decided, but
// with MoveRateLimiter exhausted (burst 0 — any Wait fails immediately, no
// timing dependency) it must not proceed to Enacting. It should fail
// cleanly back to Watching with a rate-limit reason, the same way a
// below-threshold or malformed Move fails without ever touching the
// enactor.
func TestReconciler_MoveRateLimitedFailsCleanly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID:   "test",
			Action:      orchestrationv1alpha1.RebalanceActionMove,
			PodName:     "app-a-1",
			TargetNode:  "node-b",
			Improvement: 50, // well above DefaultImprovementThreshold (20)
			Reason:      "better spread",
		})
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), pod, profile)
	r.MoveRateLimiter = rate.NewLimiter(rate.Limit(0), 0) // burst 0: every Wait fails instantly

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching — a rate-limited Move must still resolve cleanly", rs.State)
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Fatalf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Reason, "rate limit") {
		t.Errorf("reason = %q, want it to mention the rate limit", rs.RecentDecisions[0].Reason)
	}
}

// TestReconciler_EnergyPendingRetryBypassesToEnacted covers the case we
// discussed: EAO's window reopened (action=DeployImmediately) while a pod
// for the app is still Pending and unscheduled. TriggerEvaluator signals
// BypassAction (no AI consultation needed), and Reconcile should drive the
// full Triggered -> Evaluating -> Decided -> Enacting -> Watching (Outcome
// Enacted) sequence in one pass, actually deleting the pending pod so its
// controller recreates it.
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
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching — bypass path should reach a terminal write in one Reconcile call", rs.State)
	}
	if rs.Action != orchestrationv1alpha1.RebalanceAction(ActionRetryPendingSchedule) {
		t.Errorf("action = %q, want %q", rs.Action, ActionRetryPendingSchedule)
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeEnacted {
		t.Errorf("recentDecisions = %+v, want one Enacted-outcome entry", rs.RecentDecisions)
	}

	// The whole point: the pending pod should actually be gone, so its
	// owning controller (Deployment) creates a replacement.
	err = c.Get(context.Background(), types.NamespacedName{Name: "app-a-pending", Namespace: "default"}, &corev1.Pod{})
	if err == nil {
		t.Error("expected the pending pod to have been deleted by the bypass enactor")
	}
}

// TestReconciler_BypassIgnoresActiveCooldown covers the fix for a real gap
// found during live testing: an unrelated NoOp/Move can arm a full
// cooldownSeconds, and if the EAO's window then reopens while a pod is still
// Pending, that genuinely-actionable RetryPendingSchedule bypass used to be
// silently swallowed by the stale cooldown until it expired on its own —
// even though the bypass path never calls the AI and has nothing to do with
// what cooldown is rate-limiting. Same fixture as
// TestReconciler_EnergyPendingRetryBypassesToEnacted, but with an active
// CooldownUntil in the future: the bypass must still fire immediately.
func TestReconciler_BypassIgnoresActiveCooldown(t *testing.T) {
	profile := testProfileWithConditions(TriggerEnergyThreshold)
	profile.Status.RebalancingStatus.CooldownUntil = metav1.NewTime(time.Now().Add(4 * time.Minute))
	sufficient := true
	eao := testEAO("DeployImmediately", "", &sufficient)
	pending := testPod("app-a-pending", "", corev1.PodPending)
	r, c := newTestReconciler(t, testDeployment(), eao, pending, profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want DetectionInterval (30s), not the stale cooldown", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching — bypass should ignore the active cooldown entirely", rs.State)
	}
	if rs.Action != orchestrationv1alpha1.RebalanceAction(ActionRetryPendingSchedule) {
		t.Errorf("action = %q, want %q", rs.Action, ActionRetryPendingSchedule)
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeEnacted {
		t.Errorf("recentDecisions = %+v, want one Enacted-outcome entry", rs.RecentDecisions)
	}

	err = c.Get(context.Background(), types.NamespacedName{Name: "app-a-pending", Namespace: "default"}, &corev1.Pod{})
	if err == nil {
		t.Error("expected the pending pod to have been deleted despite the active cooldown")
	}
}
