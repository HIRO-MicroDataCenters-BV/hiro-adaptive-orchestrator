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
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
)

// newMoveClient is a fake client where an eviction also creates a replacement pod on landOn,
// standing in for the Deployment controller. An empty landOn leaves the replacement unscheduled.
func newMoveClient(t *testing.T, landOn string, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, policyv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("adding scheme: %v", err)
		}
	}
	scheme.AddKnownTypeWithName(testEAOGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: testEAOGVK.Group, Version: testEAOGVK.Version, Kind: testEAOGVK.Kind + "List"},
		&unstructured.UnstructuredList{},
	)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj, subObj client.Object,
				opts ...client.SubResourceCreateOption) error {
				if err := c.SubResource(sub).Create(ctx, obj, subObj, opts...); err != nil {
					return err
				}
				return c.Create(ctx, testPod("app-a-new", landOn, corev1.PodRunning))
			},
		}).Build()
}

func TestMoveEnactor_RecordsReplacementFacts(t *testing.T) {
	cases := []struct {
		name    string
		landOn  string
		outcome orchestrationv1alpha1.RebalanceOutcome
		cause   HistoryReason
	}{
		{"lands on target", "node-b", OutcomeEnacted, ReasonMoved},
		{"lands on wrong node", "node-c", OutcomeFailed, ReasonWrongNode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newMoveClient(t, tc.landOn, testDeployment(), testPod("app-a-1", "node-a", corev1.PodRunning))
			res, err := moveEnactor(context.Background(), c, placementserver.NewDecisionStore(0),
				placementserver.NewEAOSettings(testEAOGVK),
				testProfileWithConditions(TriggerPeriodicReview), "app-a-1", "node-b", "better spread", "d-1", time.Second)
			if err != nil {
				t.Fatalf("moveEnactor: %v", err)
			}
			if res.outcome != tc.outcome || res.cause != tc.cause {
				t.Errorf("result = %s/%s, want %s/%s", res.outcome, res.cause, tc.outcome, tc.cause)
			}
			want := orchestrationv1alpha1.HistoryEntry{
				PodName: "app-a-1", SourceNode: "node-a", TargetNode: "node-b",
				NewPodName: "app-a-new", NewPodNode: tc.landOn,
			}
			if res.change != want {
				t.Errorf("change = %+v, want %+v", res.change, want)
			}
		})
	}
}

// TestMoveEnactor_EnergyGateClosedAfterDispatch: the gate closed after dispatchMove's check, so the
// eviction already happened. It explains a replacement stuck Pending, but not one on the wrong node.
func TestMoveEnactor_EnergyGateClosedAfterDispatch(t *testing.T) {
	cases := []struct {
		name    string
		landOn  string
		outcome orchestrationv1alpha1.RebalanceOutcome
		cause   HistoryReason
	}{
		{"replacement stays Pending", "", OutcomeDeferred, ReasonEnergyGateClosed},
		{"replacement lands on the wrong node", "node-c", OutcomeFailed, ReasonWrongNode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newMoveClient(t, tc.landOn, testDeployment(), testPod("app-a-1", "node-a", corev1.PodRunning),
				heldEAO("demo: energy window closed", time.Now().Add(time.Hour)))
			res, err := moveEnactor(context.Background(), c, placementserver.NewDecisionStore(0),
				placementserver.NewEAOSettings(testEAOGVK),
				testEnergyAwareProfile(), "app-a-1", "node-b", "better spread", "d-1", 50*time.Millisecond)
			if err != nil {
				t.Fatalf("moveEnactor: %v", err)
			}
			if res.outcome != tc.outcome || res.cause != tc.cause {
				t.Errorf("result = %s/%s, want %s/%s", res.outcome, res.cause, tc.outcome, tc.cause)
			}
		})
	}
}
