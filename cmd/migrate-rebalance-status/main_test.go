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

package main

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

func profileWithHistory(name string, history any) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(orchestrationv1alpha1.GroupVersion.WithKind("OrchestrationProfile"))
	p.SetName(name)
	_ = unstructured.SetNestedField(p.Object, history, "status", "rebalancingStatus", "history")
	return p
}

// newUnstructuredClient registers profiles as unstructured only, so old-shape data is stored as-is.
func newUnstructuredClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	gv := orchestrationv1alpha1.GroupVersion
	scheme.AddKnownTypeWithName(gv.WithKind("OrchestrationProfile"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gv.WithKind("OrchestrationProfileList"), &unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(profileWithHistory("", nil)).WithObjects(objs...).Build()
}

func historyOf(t *testing.T, c client.Client, name string) (any, bool) {
	t.Helper()
	p := profileWithHistory(name, nil)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, p); err != nil {
		t.Fatalf("Get %s: %v", name, err)
	}
	h, found, _ := unstructured.NestedFieldNoCopy(p.Object, "status", "rebalancingStatus", "history")
	return h, found
}

func TestDropObjectHistory(t *testing.T) {
	oldShape := map[string]any{"recent": []any{map[string]any{"outcome": "NoOp"}}}
	newShape := []any{map[string]any{"outcome": "NoOp"}}
	c := newUnstructuredClient(profileWithHistory("old", oldShape), profileWithHistory("new", newShape))

	dropped, err := dropObjectHistory(context.Background(), c, false)
	if err != nil {
		t.Fatalf("dropObjectHistory: %v", err)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if h, found := historyOf(t, c, "old"); found {
		t.Errorf("old-shape history = %v, want it removed", h)
	}
	if _, found := historyOf(t, c, "new"); !found {
		t.Error("list-shaped history was removed, want it left alone")
	}
}

func TestDropObjectHistory_DryRunWritesNothing(t *testing.T) {
	c := newUnstructuredClient(profileWithHistory("old", map[string]any{"recent": []any{}}))

	dropped, err := dropObjectHistory(context.Background(), c, true)
	if err != nil {
		t.Fatalf("dropObjectHistory: %v", err)
	}
	if _, found := historyOf(t, c, "old"); dropped != 1 || !found {
		t.Errorf("dropped = %d, found = %v; want 1 reported and history kept in dry-run", dropped, found)
	}
}
