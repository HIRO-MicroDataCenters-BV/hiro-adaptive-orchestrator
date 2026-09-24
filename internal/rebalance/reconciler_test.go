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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
	if err := autoscalingv2.AddToScheme(scheme); err != nil {
		t.Fatalf("adding autoscalingv2 scheme: %v", err)
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
		contextBuilder, decisionClient, 200*time.Millisecond, 0, decisionStore, 0, 0, 0, 0, 0,
		0, resource.Quantity{}, resource.Quantity{}, resource.Quantity{}, resource.Quantity{}, 0), c
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
	profile.Status.RebalancingStatus.LastTransitionAt = metav1.Now()
	r, c := newTestReconciler(t, testDeployment(), profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want DetectionInterval (30s) even when a cycle is already in flight", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	if got.Status.RebalancingStatus.State != StateEvaluating {
		t.Errorf("state = %q, want unchanged Evaluating (not stale yet)", got.Status.RebalancingStatus.State)
	}
}

// TestReconciler_StaleCycleRecoveredByWatchdog covers recoverStuckCycle: a
// profile left in a non-Watching state well past WatchdogStaleThreshold
// (simulating an operator crash mid-cycle) is force-recovered back to
// Watching with Outcome Failed and a cooldown, instead of being skipped
// forever.
func TestReconciler_StaleCycleRecoveredByWatchdog(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	profile.Spec.Rebalancing.CooldownSeconds = 60
	profile.Status.RebalancingStatus.State = StateEnacting
	profile.Status.RebalancingStatus.LastTransitionAt = metav1.NewTime(time.Now().Add(-1 * time.Hour))
	r, c := newTestReconciler(t, testDeployment(), profile)
	r.WatchdogStaleThreshold = 100 * time.Millisecond

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
		t.Fatalf("state = %q, want Watching after watchdog recovery", rs.State)
	}
	if len(rs.RecentDecisions) == 0 || rs.RecentDecisions[len(rs.RecentDecisions)-1].Outcome != OutcomeFailed {
		t.Errorf("recovery outcome = %+v, want the latest decision recorded as Failed", rs.RecentDecisions)
	}
	if rs.CooldownUntil.IsZero() {
		t.Error("CooldownUntil unset, want the watchdog recovery to apply the profile's cooldown")
	}
	if !strings.Contains(rs.Reason, "watchdog") {
		t.Errorf("reason = %q, want it to mention the watchdog recovery", rs.Reason)
	}
}

// TestReconciler_RecentStuckCycleLeftAlone is the negative counterpart to
// TestReconciler_StaleCycleRecoveredByWatchdog: a profile that's only
// recently entered a non-Watching state (still plausibly progressing) must
// not be force-recovered.
func TestReconciler_RecentStuckCycleLeftAlone(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	profile.Status.RebalancingStatus.State = StateDecided
	profile.Status.RebalancingStatus.LastTransitionAt = metav1.Now()
	r, c := newTestReconciler(t, testDeployment(), profile)
	r.WatchdogStaleThreshold = 1 * time.Hour

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want DetectionInterval (30s)", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	if got.Status.RebalancingStatus.State != StateDecided {
		t.Errorf("state = %q, want unchanged Decided", got.Status.RebalancingStatus.State)
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
// Action has no registered dispatcher at all (genuinely unrecognized, not
// one of the known RebalanceAction values): the cycle is treated as a
// processing error, not guessed at.
func TestReconciler_UnrecognizedActionFailsDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test",
			Action:    orchestrationv1alpha1.RebalanceAction("SomeFutureAction"),
			Reason:    "AI returned an action this build doesn't know about",
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

// TestReconciler_RejectDispatchedDirectly covers a Reject AI response: like
// NoOp, it exits straight from Evaluating to Watching, but records outcome
// Rejected — the AI actively chose not to act, which is not a processing
// error.
func TestReconciler_RejectDispatchedDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test",
			Action:    orchestrationv1alpha1.RebalanceActionReject,
			Reason:    "no viable target improves balance enough to justify the disruption",
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
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeRejected {
		t.Errorf("recentDecisions = %+v, want one Rejected-outcome entry", rs.RecentDecisions)
	}
}

// TestReconciler_DeferDispatchedDirectly covers a Defer AI response: like
// NoOp, it exits straight from Evaluating to Watching, but records outcome
// Deferred and still applies cooldown — no enactor, no other side effect.
func TestReconciler_DeferDispatchedDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test",
			Action:    orchestrationv1alpha1.RebalanceActionDefer,
			Reason:    "conditions may improve shortly, revisit next cycle",
		})
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	profile.Spec.Rebalancing.CooldownSeconds = 60
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if rs.State != StateWatching {
		t.Fatalf("state = %q, want Watching", rs.State)
	}
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeDeferred {
		t.Errorf("recentDecisions = %+v, want one Deferred-outcome entry", rs.RecentDecisions)
	}
	if rs.CooldownUntil.IsZero() {
		t.Errorf("cooldownUntil = zero, want a cooldown to be applied after Defer")
	}
}

// TestReconciler_EscalateDispatchedDirectly covers an AI response that
// itself requests Escalate: like NoOp/Reject/Defer it exits straight from
// Evaluating to Watching, but also sets the persistent Escalated flag that
// pauses this profile's loop.
func TestReconciler_EscalateDispatchedDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test",
			Action:    orchestrationv1alpha1.RebalanceActionEscalate,
			Reason:    "repeated conflicting recommendations, needs a human to look",
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
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeEscalated {
		t.Errorf("recentDecisions = %+v, want one Escalated-outcome entry", rs.RecentDecisions)
	}
	if !rs.Escalated {
		t.Error("escalated = false, want true after a direct AI-requested Escalate")
	}
}

// TestReconciler_AutoEscalatesAfterConsecutiveFailures drives repeated
// AI-unreachable cycles (each ends Watching/Failed) against a low
// EscalationThreshold, and checks the threshold-crossing cycle gets promoted
// to Escalated with the loop paused — without the AI ever being asked to
// escalate itself.
func TestReconciler_AutoEscalatesAfterConsecutiveFailures(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	profile.Spec.Rebalancing.EscalationThreshold = 2
	r, c := newTestReconciler(t, testDeployment(), profile)

	for i := range 2 {
		if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
			t.Fatalf("Reconcile #%d: %v", i+1, err)
		}
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if !rs.Escalated {
		t.Fatalf("escalated = false after %d consecutive Failed cycles crossing threshold (2), want true", rs.ConsecutiveFailures)
	}
	if len(rs.RecentDecisions) != 2 || rs.RecentDecisions[0].Outcome != OutcomeEscalated {
		t.Errorf("recentDecisions = %+v, want the 2nd cycle promoted to Escalated", rs.RecentDecisions)
	}
}

// TestReconciler_EscalatedProfileSkipsDetection is the pause itself: once
// Escalated is set, Reconcile must not evaluate triggers or touch state at
// all, and must not keep self-requeuing (same posture as Enabled=false).
func TestReconciler_EscalatedProfileSkipsDetection(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	profile.Status.RebalancingStatus.Escalated = true
	profile.Status.RebalancingStatus.EscalatedReason = "3 consecutive Failed outcomes"
	r, c := newTestReconciler(t, testDeployment(), profile)

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 while escalated (watch-driven only)", res.RequeueAfter)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if len(rs.RecentDecisions) != 0 {
		t.Errorf("recentDecisions = %+v, want none — an escalated profile must not run any cycle", rs.RecentDecisions)
	}
	if !rs.Escalated {
		t.Error("escalated flipped to false unexpectedly")
	}
}

// TestReconciler_ClearingEscalatedResumesDetection: once a human clears the
// flag (a plain status patch, per the documented recovery), the very next
// Reconcile must resume normal detection instead of staying paused forever.
func TestReconciler_ClearingEscalatedResumesDetection(t *testing.T) {
	profile := testProfileWithConditions(TriggerScheduled)
	profile.Status.RebalancingStatus.Escalated = true
	r, c := newTestReconciler(t, testDeployment(), profile)

	got := getProfile(t, c, profile.Name)
	got.Status.RebalancingStatus.Escalated = false
	got.Status.RebalancingStatus.ConsecutiveFailures = 0
	if err := c.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("clearing escalated: %v", err)
	}

	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want DetectionInterval (30s) once resumed", res.RequeueAfter)
	}

	got = getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 {
		t.Errorf("recentDecisions = %+v, want one entry — detection should have run normally", rs.RecentDecisions)
	}
}

// TestReconciler_MoveAboveThresholdReachesEnacting covers an accepted Move
// recommendation: the cycle reaches Decided and Enacting (verified via the
// Warning event the terminal write emits) before the enactor's eviction
// call — unsupported by the fake client — fails it back to Watching with
// Outcome Failed. This still proves dispatchMove drives the cycle all the
// way to the enactor instead of stopping at Decided, which is what the
// guardrail chain itself is responsible for; actual eviction success is
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

// TestReconciler_MoveTimeoutWithClosedEnergyGateIsDeferred: awaitReplacement
// times out (no replacement pod will ever appear against a fake client)
// against an energy-aware workload whose EAO reports insufficient energy
// right now — classifyScheduleTimeout should reclassify the timeout
// Deferred instead of Failed.
func TestReconciler_MoveTimeoutWithClosedEnergyGateIsDeferred(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test", Action: orchestrationv1alpha1.RebalanceActionMove,
			PodName: "app-a-1", TargetNode: "node-b", Improvement: 50, Reason: "better spread",
		})
	}))
	defer server.Close()

	profile := testEnergyAwareProfile()
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	eao := testEAO("Waiting", "demo: energy window closed", boolPtr(false))
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), pod, profile, eao)
	r.MoveActionTimeout = 50 * time.Millisecond

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeDeferred {
		t.Fatalf("recentDecisions = %+v, want one Deferred-outcome entry", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Reason, "energy gate closed") {
		t.Errorf("reason = %q, want it to mention the energy gate", rs.RecentDecisions[0].Reason)
	}
}

// TestReconciler_MoveTimeoutWithoutEnergyAwarenessStaysFailed covers the
// negative case: the same stuck-scheduling timeout, but the workload isn't
// energy-aware at all (Awareness.Energy: false) — classifyScheduleTimeout
// must not reclassify it, even though a (misleadingly) closed EAO exists.
func TestReconciler_MoveTimeoutWithoutEnergyAwarenessStaysFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test", Action: orchestrationv1alpha1.RebalanceActionMove,
			PodName: "app-a-1", TargetNode: "node-b", Improvement: 50, Reason: "better spread",
		})
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled) // Awareness.Energy: false
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	eao := testEAO("Waiting", "demo: energy window closed", boolPtr(false))
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), pod, profile, eao)
	r.MoveActionTimeout = 50 * time.Millisecond

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Fatalf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
}

// TestReconciler_MoveTimeoutWithOpenEnergyGateStaysFailed covers the other
// negative case: energy awareness is on and an EAO exists, but it reports
// sufficient energy — the timeout is genuinely unexplained, so it must stay
// Failed.
func TestReconciler_MoveTimeoutWithOpenEnergyGateStaysFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test", Action: orchestrationv1alpha1.RebalanceActionMove,
			PodName: "app-a-1", TargetNode: "node-b", Improvement: 50, Reason: "better spread",
		})
	}))
	defer server.Close()

	profile := testEnergyAwareProfile()
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	eao := testEAO("DeployImmediately", "window open", boolPtr(true))
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), pod, profile, eao)
	r.MoveActionTimeout = 50 * time.Millisecond

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Fatalf("recentDecisions = %+v, want one Failed-outcome entry", rs.RecentDecisions)
	}
}

// TestReconciler_MoveWrongNodeStaysFailedEvenWithClosedEnergyGate proves
// classifyScheduleTimeout is only ever applied to the timeout branch, not
// the "replacement landed on the wrong node" branch: a replacement pod
// appears (so awaitReplacement doesn't time out at all) but on the wrong
// node, with the energy gate closed — must still be Failed, not Deferred.
// The replacement is created 300ms after Reconcile starts (well under
// moveActionPollInterval's 2s cadence, so it's reliably missed by the
// immediate first poll and picked up by the second) to deterministically
// exercise the "replacement found" path rather than the timeout path.
func TestReconciler_MoveWrongNodeStaysFailedEvenWithClosedEnergyGate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test", Action: orchestrationv1alpha1.RebalanceActionMove,
			PodName: "app-a-1", TargetNode: "node-b", Improvement: 50, Reason: "better spread",
		})
	}))
	defer server.Close()

	profile := testEnergyAwareProfile()
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	eao := testEAO("Waiting", "demo: energy window closed", boolPtr(false))
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), pod, profile, eao)
	r.MoveActionTimeout = 5 * time.Second

	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = c.Create(context.Background(), testPod("app-a-2", "node-c", corev1.PodRunning)) // wrong node
	}()

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeFailed {
		t.Fatalf("recentDecisions = %+v, want one Failed-outcome entry (wrong node, not a timeout)", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Reason, "intent not honoured") {
		t.Errorf("reason = %q, want the wrong-node reason, not a timeout/energy-gate one", rs.RecentDecisions[0].Reason)
	}
}

// TestReconciler_MoveRateLimitedFailsCleanly covers dispatchMove's cluster-wide
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

// TestReconciler_MoveDryRunSkipsSideEffectsAndRateLimit covers dry-run: an
// accepted, above-threshold Move on a spec.rebalancing.dryRun: true profile
// must still reach Enacting and resolve Watching+Enacted, but without
// actually evicting the pod and without waiting on MoveRateLimiter (exhausted
// here with burst 0, which would fail a real Move instantly — proving the
// dry-run path never calls Wait at all).
func TestReconciler_MoveDryRunSkipsSideEffectsAndRateLimit(t *testing.T) {
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
	profile.Spec.Rebalancing.DryRun = true
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), pod, profile)
	r.MoveRateLimiter = rate.NewLimiter(rate.Limit(0), 0) // burst 0: a real Wait would fail instantly

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
	if !strings.Contains(rs.RecentDecisions[0].Details, "[dry-run]") {
		t.Errorf("details = %q, want it to carry the dry-run marker", rs.RecentDecisions[0].Details)
	}

	stillThere := &corev1.Pod{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a-1", Namespace: "default"}, stillThere); err != nil {
		t.Fatalf("pod app-a-1 should not have been evicted in dry-run: %v", err)
	}
}

// TestReconciler_MoveDryRunBelowThresholdStillRejected proves dry-run doesn't
// bypass guardrails — only Enacting's real side effects are skipped, so a
// below-threshold recommendation must still end Rejected, same as a live run.
func TestReconciler_MoveDryRunBelowThresholdStillRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID:   "test",
			Action:      orchestrationv1alpha1.RebalanceActionMove,
			PodName:     "app-a-1",
			TargetNode:  "node-b",
			Improvement: 5, // below DefaultImprovementThreshold (20)
			Reason:      "marginal",
		})
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	profile.Spec.Rebalancing.DryRun = true
	pod := testPod("app-a-1", "node-a", corev1.PodRunning)
	r, c := newTestReconcilerWithAgent(t, server.URL, testDeployment(), pod, profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getProfile(t, c, profile.Name)
	rs := got.Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeRejected {
		t.Fatalf("recentDecisions = %+v, want one Rejected-outcome entry", rs.RecentDecisions)
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
