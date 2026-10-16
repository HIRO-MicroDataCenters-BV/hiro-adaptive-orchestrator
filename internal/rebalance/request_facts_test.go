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

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
)

func testPDB(selector *metav1.LabelSelector, allowed int32) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "app-a-pdb", Namespace: "default"},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: selector},
		Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: allowed},
	}
}

func TestRebalanceFacts_TriggerWorkloadAndPDB(t *testing.T) {
	profile := testProfileWithConditions(TriggerCPUThreshold)
	profile.Spec.Rebalancing.ReplicasExternallyManaged = true
	pdb := testPDB(&metav1.LabelSelector{MatchLabels: map[string]string{"app": "app-a"}}, 0)
	r, _ := newTestReconciler(t, testScalableDeployment(1), testHPA("app-a-hpa", 2, 6),
		testPod("app-a-1", "node-a", corev1.PodRunning), pdb, profile)

	value, threshold := 0.95, 0.90
	facts := r.rebalanceFacts(context.Background(), profile, TriggerResult{
		Condition: TriggerCPUThreshold,
		Detail:    TriggerDetail{Node: "node-a", Value: &value, Threshold: &threshold},
	})

	tr := facts.Trigger
	if tr == nil || tr.Type != string(TriggerCPUThreshold) || tr.Node != "node-a" || *tr.Value != 0.95 || *tr.Threshold != 0.90 {
		t.Errorf("trigger = %+v, want CPUThreshold on node-a at 0.95/0.90", tr)
	}
	w := facts.Workload
	if w == nil || w.Replicas != 1 || w.MinReplicas != 2 || w.MaxReplicas != 6 {
		t.Fatalf("workload = %+v, want replicas 1 and HPA bounds [2, 6]", w)
	}
	if !w.ReplicasExternallyManaged || w.ResourcesExternallyManaged {
		t.Errorf("workload flags = replicas %v resources %v, want true/false from the profile",
			w.ReplicasExternallyManaged, w.ResourcesExternallyManaged)
	}
	if facts.PDB == nil || facts.PDB.Name != "app-a-pdb" || facts.PDB.AllowedDisruptions != 0 {
		t.Errorf("pdb = %+v, want app-a-pdb with 0 allowed disruptions", facts.PDB)
	}
}

func TestFindPDB_SelectorMatching(t *testing.T) {
	pods := []corev1.Pod{*testPod("app-a-1", "node-a", corev1.PodRunning)}
	cases := []struct {
		name     string
		selector *metav1.LabelSelector
		want     bool
	}{
		{"matching labels", &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app-a"}}, true},
		{"other app", &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}, false},
		{"empty selector matches every pod", &metav1.LabelSelector{}, true},
		{"nil selector matches none", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newTestReconciler(t, testPDB(tc.selector, 1))
			got, err := findPDB(context.Background(), c, "default", pods)
			if err != nil {
				t.Fatalf("findPDB: %v", err)
			}
			if (got != nil) != tc.want {
				t.Errorf("findPDB = %+v, want found=%v", got, tc.want)
			}
		})
	}
}

func TestReconciler_SendsRebalanceFactsToAgent(t *testing.T) {
	var got placementserver.DecisionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewDecoder(req.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placementserver.RebalanceDecisionResponse{
			RequestID: "test", Action: orchestrationv1alpha1.RebalanceActionNoOp,
		})
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	profile.Spec.Rebalancing.ResourcesExternallyManaged = true
	r, _ := newTestReconcilerWithAgent(t, server.URL, testScalableDeployment(1),
		testPod("app-a-1", "node-a", corev1.PodRunning), profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rc := got.RebalanceContext
	if rc == nil || rc.Trigger == nil || rc.Trigger.Type != string(TriggerScheduled) {
		t.Fatalf("rebalanceContext = %+v, want trigger Scheduled", rc)
	}
	if rc.Workload == nil || !rc.Workload.ResourcesExternallyManaged || rc.Workload.Replicas != 1 {
		t.Errorf("workload = %+v, want replicas 1 and resourcesExternallyManaged", rc.Workload)
	}
	if rc.PDB != nil {
		t.Errorf("pdb = %+v, want nil when no PodDisruptionBudget exists", rc.PDB)
	}
}

func TestReconciler_ResourceDefersWhenResourcesExternallyManaged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(adjustResourcesResponse(testResourceContainerName, "200m", "256Mi", 50))
	}))
	defer server.Close()

	profile := testProfileWithConditions(TriggerScheduled)
	profile.Spec.Rebalancing.ResourcesExternallyManaged = true
	r, c := newTestReconcilerWithAgent(t, server.URL, testResizableDeployment(true), testRunningPod("app-a-1"), profile)

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: profile.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	rs := getProfile(t, c, profile.Name).Status.RebalancingStatus
	if len(rs.RecentDecisions) != 1 || rs.RecentDecisions[0].Outcome != OutcomeDeferred {
		t.Fatalf("recentDecisions = %+v, want one Deferred-outcome entry", rs.RecentDecisions)
	}
	if !strings.Contains(rs.RecentDecisions[0].Reason, "resources are externally managed") {
		t.Errorf("reason = %q, want it to say resources are externally managed", rs.RecentDecisions[0].Reason)
	}
}
