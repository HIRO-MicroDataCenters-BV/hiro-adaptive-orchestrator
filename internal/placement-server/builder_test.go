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

func testRebalanceEAO(priority string, watts int64, action, reason string, sufficient bool) *unstructured.Unstructured {
	eao := &unstructured.Unstructured{}
	eao.SetGroupVersionKind(testEAOGVK)
	eao.SetName("eao-a")
	eao.SetNamespace("default")
	_ = unstructured.SetNestedMap(eao.Object, map[string]any{
		"name": "app-a", "namespace": "default", "kind": "Deployment",
	}, "spec", "applicationRef")
	_ = unstructured.SetNestedField(eao.Object, priority, "spec", "priority")
	_ = unstructured.SetNestedField(eao.Object, watts, "spec", "energyConsumption")
	_ = unstructured.SetNestedMap(eao.Object, map[string]any{
		"action": action, "reason": reason,
	}, "status", "decision")
	_ = unstructured.SetNestedField(eao.Object, sufficient, "status", "energyMetrics", "sufficient")
	return eao
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
		Build()

	return NewDecisionContextBuilder(c, testProfileIndexField, testEAOGVK)
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

	recentDecisions := []orchestrationv1alpha1.RebalanceDecision{
		{
			Outcome:          orchestrationv1alpha1.RebalanceOutcomeNoOp,
			Action:           orchestrationv1alpha1.RebalanceActionNoOp,
			Reason:           "previously balanced",
			LastTransitionAt: metav1.Now(),
		},
	}

	req, err := b.BuildRebalanceContext(context.Background(), profile, testDecisionID, "CPUThreshold: node-a at 92%", recentDecisions)
	if err != nil {
		t.Fatalf("BuildRebalanceContext: %v", err)
	}

	if req.RequestID != testDecisionID {
		t.Errorf("RequestID = %q, want %s", req.RequestID, testDecisionID)
	}
	if req.Pod != nil {
		t.Errorf("Pod = %+v, want nil for a rebalance request", req.Pod)
	}
	if len(req.CandidateNodes) != 2 {
		t.Fatalf("CandidateNodes len = %d, want 2 (NotReady node excluded)", len(req.CandidateNodes))
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

	if len(rc.RecentDecisions) != 1 || rc.RecentDecisions[0].Action != "NoOp" {
		t.Errorf("RecentDecisions = %+v, want one converted NoOp entry", rc.RecentDecisions)
	}
}

func TestBuildRebalanceContext_EnergyAwareAttachesEAOData(t *testing.T) {
	profile := testRebalanceProfile(true)
	b := newTestBuilder(t,
		testAppDeployment(),
		testAppPod("app-a-1", "node-a", corev1.PodRunning),
		testNode("node-a", true),
		testRebalanceEAO("Critical", 500, "DeployImmediately", "window open", false),
	)

	req, err := b.BuildRebalanceContext(context.Background(), profile, "decision-2", "EnergyThreshold: verdict flipped", nil)
	if err != nil {
		t.Fatalf("BuildRebalanceContext: %v", err)
	}

	if req.EAOProfile == nil {
		t.Fatal("EAOProfile is nil, want data attached when energy awareness is enabled")
	}
	if req.EAOProfile.Priority != "Critical" || req.EAOProfile.EnergyConsumptionWatts != 500 {
		t.Errorf("EAOProfile spec fields = %+v, want Priority=Critical EnergyConsumptionWatts=500", req.EAOProfile)
	}
	if req.EAOProfile.Decision == nil || req.EAOProfile.Decision.Action != "DeployImmediately" {
		t.Errorf("EAOProfile.Decision = %+v, want Action=DeployImmediately", req.EAOProfile.Decision)
	}
	if req.EAOProfile.EnergyMetrics == nil || req.EAOProfile.EnergyMetrics.Sufficient {
		t.Errorf("EAOProfile.EnergyMetrics = %+v, want Sufficient=false", req.EAOProfile.EnergyMetrics)
	}

	if req.RebalanceContext == nil || len(req.RebalanceContext.RecentDecisions) != 0 {
		t.Errorf("RecentDecisions = %+v, want empty for nil input", req.RebalanceContext.RecentDecisions)
	}
}
