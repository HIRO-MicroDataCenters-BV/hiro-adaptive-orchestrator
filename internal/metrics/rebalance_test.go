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

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

func testProfile(name string, state orchestrationv1alpha1.RebalancingStateType) *orchestrationv1alpha1.OrchestrationProfile {
	return &orchestrationv1alpha1.OrchestrationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: orchestrationv1alpha1.OrchestrationProfileStatus{
			RebalancingStatus: orchestrationv1alpha1.RebalancingStatus{State: state},
		},
	}
}

// TestStateGaugeCollector_CountsByStateIncludingUnset covers both the basic
// per-state counting and the one piece of real logic in Collect: a profile
// that has never been triggered (State == "") counts as Watching, the same
// resting/idle meaning it already has everywhere else in this codebase.
func TestStateGaugeCollector_CountsByStateIncludingUnset(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testProfile("never-triggered", ""),
		testProfile("watching-1", orchestrationv1alpha1.RebalancingStateWatching),
		testProfile("watching-2", orchestrationv1alpha1.RebalancingStateWatching),
		testProfile("triggered-1", orchestrationv1alpha1.RebalancingStateTriggered),
		testProfile("enacting-1", orchestrationv1alpha1.RebalancingStateEnacting),
	).Build()

	collector := NewRebalanceStateGaugeCollector(c)

	want := `
		# HELP hiro_rebalance_profiles_by_state Current number of OrchestrationProfiles in each rebalancing state.
		# TYPE hiro_rebalance_profiles_by_state gauge
		hiro_rebalance_profiles_by_state{state="Watching"} 3
		hiro_rebalance_profiles_by_state{state="Triggered"} 1
		hiro_rebalance_profiles_by_state{state="Evaluating"} 0
		hiro_rebalance_profiles_by_state{state="Decided"} 0
		hiro_rebalance_profiles_by_state{state="Enacting"} 1
	`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(want), "hiro_rebalance_profiles_by_state"); err != nil {
		t.Errorf("unexpected metric state: %v", err)
	}
}

// TestStateGaugeCollector_NoProfiles proves an empty cluster reports every
// state at zero rather than omitting them.
func TestStateGaugeCollector_NoProfiles(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	collector := NewRebalanceStateGaugeCollector(c)

	count := testutil.CollectAndCount(collector, "hiro_rebalance_profiles_by_state")
	if count != 5 {
		t.Errorf("metric count = %d, want 5 (one per state, all zero)", count)
	}
}
