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

	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// testEnergyAwareProfile mirrors testProfileWithConditions but with energy
// awareness enabled, since classifyScheduleTimeout checks that first.
func testEnergyAwareProfile() *orchestrationv1alpha1.OrchestrationProfile {
	p := testProfileWithConditions(TriggerScheduled)
	p.Spec.Placement.Awareness.Energy = true
	return p
}

func TestClassifyScheduleTimeout(t *testing.T) {
	const timeoutReason = "no scheduled replacement observed within 1m0s"

	tests := []struct {
		name        string
		profile     *orchestrationv1alpha1.OrchestrationProfile
		objs        []client.Object
		wantOutcome orchestrationv1alpha1.RebalanceOutcome
		wantReason  string
	}{
		{
			name:        "energy awareness disabled stays Failed",
			profile:     testProfileWithConditions(TriggerScheduled), // Awareness.Energy: false
			objs:        []client.Object{testEAO("Waiting", "demo: closed", boolPtr(false))},
			wantOutcome: OutcomeFailed,
			wantReason:  timeoutReason,
		},
		{
			name:        "no matching EAO stays Failed",
			profile:     testEnergyAwareProfile(),
			objs:        nil,
			wantOutcome: OutcomeFailed,
			wantReason:  timeoutReason,
		},
		{
			name:        "sufficient field not populated stays Failed",
			profile:     testEnergyAwareProfile(),
			objs:        []client.Object{testEAO("Waiting", "demo: closed", nil)},
			wantOutcome: OutcomeFailed,
			wantReason:  timeoutReason,
		},
		{
			name:        "sufficient true stays Failed",
			profile:     testEnergyAwareProfile(),
			objs:        []client.Object{testEAO("DeployImmediately", "window open", boolPtr(true))},
			wantOutcome: OutcomeFailed,
			wantReason:  timeoutReason,
		},
		{
			name:        "sufficient false with a decision reason becomes Deferred",
			profile:     testEnergyAwareProfile(),
			objs:        []client.Object{testEAO("Waiting", "demo: energy window closed", boolPtr(false))},
			wantOutcome: OutcomeDeferred,
			wantReason:  timeoutReason + " (energy gate closed: demo: energy window closed)",
		},
		{
			name:        "sufficient false with no decision reason falls back to generic text",
			profile:     testEnergyAwareProfile(),
			objs:        []client.Object{testEAO("Waiting", "", boolPtr(false))},
			wantOutcome: OutcomeDeferred,
			wantReason:  timeoutReason + " (energy gate closed: energy supply reported insufficient)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator := newTestTriggerEvaluator(t, tt.objs...)
			outcome, reason := classifyScheduleTimeout(context.Background(), evaluator.client, testEAOGVK, tt.profile, timeoutReason)
			if outcome != tt.wantOutcome {
				t.Errorf("outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}
