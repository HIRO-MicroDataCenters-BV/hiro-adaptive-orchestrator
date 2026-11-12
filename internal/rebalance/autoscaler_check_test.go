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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

var testVPAGVK = schema.GroupVersionKind{Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscaler"}

func testVPA(updateMode string) *unstructured.Unstructured {
	vpa := &unstructured.Unstructured{}
	vpa.SetGroupVersionKind(testVPAGVK)
	vpa.SetName("app-vpa")
	vpa.SetNamespace("default")
	_ = unstructured.SetNestedMap(vpa.Object, map[string]any{"kind": "Deployment", "name": "app-a"}, "spec", "targetRef")
	if updateMode != "" {
		_ = unstructured.SetNestedField(vpa.Object, updateMode, "spec", "updatePolicy", "updateMode")
	}
	return vpa
}

func TestReconciler_ReportsUndeclaredAutoscalers(t *testing.T) {
	otherHPA := testHPA("other-hpa", 1, 5)
	otherHPA.Spec.ScaleTargetRef.Name = "other-app"

	cases := []struct {
		name             string
		objs             []client.Object
		replicasManaged  bool
		resourcesManaged bool
		wantInMessage    []string // empty means no condition is written
	}{
		{name: "no autoscaler"},
		{name: "HPA for another workload", objs: []client.Object{otherHPA}},
		{name: "undeclared HPA", objs: []client.Object{testHPA("app-hpa", 1, 5)},
			wantInMessage: []string{"HorizontalPodAutoscaler app-hpa"}},
		{name: "declared HPA", objs: []client.Object{testHPA("app-hpa", 1, 5)}, replicasManaged: true},
		{name: "undeclared VPA", objs: []client.Object{testVPA("")},
			wantInMessage: []string{"VerticalPodAutoscaler app-vpa"}},
		{name: "declared VPA", objs: []client.Object{testVPA("")}, resourcesManaged: true},
		{name: "VPA that only recommends", objs: []client.Object{testVPA(vpaUpdateModeOff)}},
		{name: "both undeclared", objs: []client.Object{testHPA("app-hpa", 1, 5), testVPA("Recreate")},
			wantInMessage: []string{"HorizontalPodAutoscaler app-hpa", "VerticalPodAutoscaler app-vpa"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile := testProfileWithConditions()
			profile.Spec.Rebalancing.ReplicasExternallyManaged = tc.replicasManaged
			profile.Spec.Rebalancing.ResourcesExternallyManaged = tc.resourcesManaged
			r, c := newTestReconciler(t, append(tc.objs, testDeployment(), profile)...)

			if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			cond := apimeta.FindStatusCondition(getProfile(t, c, profile.Name).Status.Conditions,
				orchestrationv1alpha1.ConditionUndeclaredAutoscaler)
			if len(tc.wantInMessage) == 0 {
				if cond != nil {
					t.Fatalf("condition = %+v, want none", cond)
				}
				return
			}
			if cond == nil || cond.Status != metav1.ConditionTrue {
				t.Fatalf("condition = %+v, want True", cond)
			}
			for _, want := range tc.wantInMessage {
				if !strings.Contains(cond.Message, want) {
					t.Errorf("message = %q, want it to mention %q", cond.Message, want)
				}
			}
		})
	}
}

func TestReconciler_ClearsUndeclaredAutoscalerOnceGone(t *testing.T) {
	profile := testProfileWithConditions()
	apimeta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:    orchestrationv1alpha1.ConditionUndeclaredAutoscaler,
		Status:  metav1.ConditionTrue,
		Reason:  "UndeclaredAutoscaler",
		Message: "HorizontalPodAutoscaler app-hpa targets the app but replicasExternallyManaged is false",
	})
	r, c := newTestReconciler(t, testDeployment(), profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	cond := apimeta.FindStatusCondition(getProfile(t, c, profile.Name).Status.Conditions,
		orchestrationv1alpha1.ConditionUndeclaredAutoscaler)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Message != "" {
		t.Fatalf("condition = %+v, want False with no message", cond)
	}
}
