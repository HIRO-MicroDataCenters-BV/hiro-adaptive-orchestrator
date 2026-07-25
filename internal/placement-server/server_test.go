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

package placementserver

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// newTestPlacementServer builds a PlacementServer wired to a fake client
// seeded with the given objects, sharing newTestBuilder's fixtures. client
// is nil — decisionStoreResponse never uses it, and no test here exercises
// the AI-call fallback path.
func newTestPlacementServer(t *testing.T, store *DecisionStore, objs ...client.Object) *PlacementServer {
	t.Helper()
	builder := newTestBuilder(t, objs...)
	return NewPlacementServer(builder, nil, store,
		":0", "/score", "/filter", "/healthz", "/extfilter", "/extprioritize", time.Second)
}

// testOwnedPod returns a ReplicaSet (owned by the "app-a" Deployment) and a
// Pod owned by that ReplicaSet — utils.ResolveAppFromPod (used by
// FindProfileForPod) walks Pod -> ReplicaSet -> Deployment owner references,
// not labels, so decisionStoreResponse tests need this chain to resolve a
// profile for the pod being scored.
func testOwnedPod() (*appsv1.ReplicaSet, *corev1.Pod) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-a-rs", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "Deployment", Name: "app-a", UID: "dep-uid"},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-a-2", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "app-a-rs", UID: "rs-uid"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	return rs, pod
}

func TestDecisionStoreResponse_Miss(t *testing.T) {
	rs, pod := testOwnedPod()
	s := newTestPlacementServer(t, NewDecisionStore(time.Minute), testAppDeployment(), rs, testRebalanceProfile(false))

	placementCtx := PlacementContext{
		Pod:            pod,
		CandidateNodes: []*corev1.Node{testNode("node-a", true), testNode("node-b", true)},
	}

	if resp := s.decisionStoreResponse(context.Background(), placementCtx, "req-1"); resp != nil {
		t.Errorf("decisionStoreResponse() = %+v, want nil on a store miss", resp)
	}
}

func TestDecisionStoreResponse_MoveHit(t *testing.T) {
	profile := testRebalanceProfile(false)
	store := NewDecisionStore(time.Minute)
	rs, pod := testOwnedPod() // the replacement being scored now
	s := newTestPlacementServer(t, store, testAppDeployment(), rs, profile)

	key := WorkloadKey(profile.Namespace, profile.Name)
	store.Put(key, "app-a-1", orchestrationv1alpha1.RebalanceActionMove, "node-b", "energy window", "decision-1")

	placementCtx := PlacementContext{
		Pod:            pod,
		CandidateNodes: []*corev1.Node{testNode("node-a", true), testNode("node-b", true)},
	}

	resp := s.decisionStoreResponse(context.Background(), placementCtx, "req-1")
	if resp == nil {
		t.Fatal("decisionStoreResponse() = nil, want a synthesized response on a Move hit")
	}
	if len(resp.NodeScores) != 2 {
		t.Fatalf("NodeScores = %+v, want 2 entries", resp.NodeScores)
	}
	got := map[string]float64{}
	for _, ns := range resp.NodeScores {
		got[ns.NodeName] = ns.Score
	}
	if got["node-b"] != 100.0 {
		t.Errorf("node-b score = %v, want 100", got["node-b"])
	}
	if got["node-a"] != 0.0 {
		t.Errorf("node-a score = %v, want 0", got["node-a"])
	}

	// Lookup self-consumes — a second call must miss.
	if resp := s.decisionStoreResponse(context.Background(), placementCtx, "req-2"); resp != nil {
		t.Errorf("second decisionStoreResponse() = %+v, want nil — entry should self-consume", resp)
	}
}

func TestDecisionStoreResponse_TargetNodeNotACandidate(t *testing.T) {
	profile := testRebalanceProfile(false)
	store := NewDecisionStore(time.Minute)
	rs, pod := testOwnedPod()
	s := newTestPlacementServer(t, store, testAppDeployment(), rs, profile)

	key := WorkloadKey(profile.Namespace, profile.Name)
	store.Put(key, "app-a-1", orchestrationv1alpha1.RebalanceActionMove, "node-z", "reason", "decision-1")

	placementCtx := PlacementContext{
		Pod:            pod,
		CandidateNodes: []*corev1.Node{testNode("node-a", true), testNode("node-b", true)},
	}

	if resp := s.decisionStoreResponse(context.Background(), placementCtx, "req-1"); resp != nil {
		t.Errorf("decisionStoreResponse() = %+v, want nil when TargetNode isn't a candidate", resp)
	}
}

func TestDecisionStoreResponse_NonMoveActionIgnored(t *testing.T) {
	profile := testRebalanceProfile(false)
	store := NewDecisionStore(time.Minute)
	rs, pod := testOwnedPod()
	s := newTestPlacementServer(t, store, testAppDeployment(), rs, profile)

	key := WorkloadKey(profile.Namespace, profile.Name)
	store.Put(key, "app-a-1", orchestrationv1alpha1.RebalanceActionNoOp, "", "reason", "decision-1")

	placementCtx := PlacementContext{
		Pod:            pod,
		CandidateNodes: []*corev1.Node{testNode("node-a", true)},
	}

	if resp := s.decisionStoreResponse(context.Background(), placementCtx, "req-1"); resp != nil {
		t.Errorf("decisionStoreResponse() = %+v, want nil for a non-Move entry", resp)
	}
}

func TestDecisionStoreResponse_NoProfileForPod(t *testing.T) {
	s := newTestPlacementServer(t, NewDecisionStore(time.Minute))

	pod := testAppPod("orphan-1", "", corev1.PodPending) // no OwnerReferences — can't resolve to any profile
	placementCtx := PlacementContext{
		Pod:            pod,
		CandidateNodes: []*corev1.Node{testNode("node-a", true)},
	}

	if resp := s.decisionStoreResponse(context.Background(), placementCtx, "req-1"); resp != nil {
		t.Errorf("decisionStoreResponse() = %+v, want nil when no profile governs the pod", resp)
	}
}
