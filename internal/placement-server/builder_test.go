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
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

var testEAOGVK = schema.GroupVersionKind{Group: "eas.hiro.io", Version: "v1", Kind: "EnergyAwareOrchestration"}

const testProfileIndexField = ".spec.applicationRef.namespacedName"

const testDecisionID = "decision-1"

func testAppRef() orchestrationv1alpha1.ApplicationReference {
	return orchestrationv1alpha1.ApplicationReference{
		Kind: "Deployment", Name: "app-a", Namespace: "default",
	}
}

func testRebalanceProfile(energyAware bool) *orchestrationv1alpha1.OrchestrationProfile {
	return &orchestrationv1alpha1.OrchestrationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-a"},
		Spec: orchestrationv1alpha1.OrchestrationProfileSpec{
			ApplicationRef: testAppRef(),
			Placement: orchestrationv1alpha1.PlacementSpec{
				Strategy:  "Balanced",
				Awareness: orchestrationv1alpha1.Awareness{Energy: energyAware},
			},
			Rebalancing: orchestrationv1alpha1.RebalancingSpec{Enabled: true},
		},
	}
}

func testAppDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app-a", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app-a"}},
		},
	}
}

func testAppPod(name, nodeName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{"app": "app-a"},
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func testNode(name string, ready bool) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

// newTestBuilder builds a DecisionContextBuilder wired to a fake client
// seeded with the given objects, registering the EAO GVK the same way the
// real manager does via RESTMapper-backed scheme registration.
func newTestBuilder(t *testing.T, objs ...client.Object) *DecisionContextBuilder {
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
		WithIndex(&corev1.Pod{}, PodNodeNameIndex, PodNodeNameIndexFunc).
		Build()

	return NewDecisionContextBuilder(c, testProfileIndexField, NewEAOSettings(testEAOGVK))
}

func TestBuildRebalanceContext_AssemblesPlacementsAndCandidateNodes(t *testing.T) {
	profile := testRebalanceProfile(false)
	b := newTestBuilder(t,
		testAppDeployment(),
		testAppPod("app-a-1", "node-a", corev1.PodRunning),
		testAppPod("app-a-2", "node-b", corev1.PodRunning),
		testNode("node-a", true),
		testNode("node-b", true),
		testNode("node-c", false), // NotReady — must be excluded
	)

	state := RebalanceState{
		History: []orchestrationv1alpha1.HistoryEntry{{
			Action:  orchestrationv1alpha1.RebalanceActionNoOp,
			Outcome: orchestrationv1alpha1.RebalanceOutcomeNoOp,
			Reason:  "NoChange",
			Count:   3,
			At:      metav1.Now(),
		}},
		ConsecutiveFailures: 1,
		EscalationThreshold: 3,
	}

	req, err := b.BuildRebalanceContext(context.Background(), profile, testDecisionID, "CPUThreshold: node-a at 92%",
		RebalanceFacts{State: state})
	if err != nil {
		t.Fatalf("BuildRebalanceContext: %v", err)
	}

	if req.RequestID != testDecisionID {
		t.Errorf("RequestID = %q, want %s", req.RequestID, testDecisionID)
	}
	if req.Pod != nil {
		t.Errorf("Pod = %+v, want nil for a rebalance request", req.Pod)
	}
	if len(req.NodeStats) != 2 {
		t.Fatalf("NodeStats len = %d, want 2 (NotReady node excluded)", len(req.NodeStats))
	}

	if req.AOProfile == nil || req.AOProfile.ProfileName != profile.Name {
		t.Fatalf("AOProfile = %+v, want ProfileName %q", req.AOProfile, profile.Name)
	}
	if !req.AOProfile.Rebalancing.Enabled {
		t.Error("AOProfile.Rebalancing.Enabled = false, want true")
	}

	if req.EAOProfile != nil {
		t.Errorf("EAOProfile = %+v, want nil when energy awareness is disabled", req.EAOProfile)
	}

	if req.RebalanceContext == nil {
		t.Fatal("RebalanceContext is nil")
	}
	rc := req.RebalanceContext
	if rc.DecisionID != testDecisionID || rc.Reason != "CPUThreshold: node-a at 92%" {
		t.Errorf("RebalanceContext = %+v, unexpected DecisionID/Reason", rc)
	}
	if len(rc.CurrentPlacements) != 2 {
		t.Fatalf("CurrentPlacements len = %d, want 2", len(rc.CurrentPlacements))
	}
	byName := map[string]PodPlacement{}
	for _, p := range rc.CurrentPlacements {
		byName[p.PodName] = p
	}
	if byName["app-a-1"].NodeName != "node-a" || byName["app-a-2"].NodeName != "node-b" {
		t.Errorf("CurrentPlacements = %+v, want app-a-1 on node-a and app-a-2 on node-b", rc.CurrentPlacements)
	}

	if len(rc.History) != 1 || rc.ConsecutiveFailures != 1 || rc.EscalationThreshold != 3 {
		t.Errorf("RebalanceState = %+v, want the facts' state passed through unchanged", rc.RebalanceState)
	}

	// The state is inlined: its fields must sit directly in rebalanceContext on the wire.
	raw, err := json.Marshal(rc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"history", "historyWindow", "consecutiveFailures", "escalationThreshold"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("rebalanceContext JSON has no top-level %q: %s", key, raw)
		}
	}
}

// testDeploymentPod is owned via ReplicaSet by testAppDeployment, the path
// ResolveAppFromPod takes for a real Deployment-managed pod.
func testDeploymentPod() (*corev1.Pod, *appsv1.ReplicaSet) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-a-rs", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "app-a"}},
		},
	}
	pod := testAppPod("pod-x", "", corev1.PodPending)
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name}}
	return pod, rs
}
