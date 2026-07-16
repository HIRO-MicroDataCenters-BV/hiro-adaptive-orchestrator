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
	"testing"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

func TestIsValidTransition(t *testing.T) {
	tests := []struct {
		name string
		from orchestrationv1alpha1.RebalancingStateType
		to   orchestrationv1alpha1.RebalancingStateType
		want bool
	}{
		{"none to Triggered", stateNone, StateTriggered, true},
		{"none to Evaluating rejected", stateNone, StateEvaluating, false},
		{"Triggered to Evaluating", StateTriggered, StateEvaluating, true},
		{"Triggered to Decided rejected", StateTriggered, StateDecided, false},
		{"Evaluating to Decided", StateEvaluating, StateDecided, true},
		{"Evaluating to NoOp", StateEvaluating, StateNoOp, true},
		{"Evaluating to Rejected", StateEvaluating, StateRejected, true},
		{"Evaluating to Failed", StateEvaluating, StateFailed, true},
		{"Evaluating to Enacting rejected", StateEvaluating, StateEnacting, false},
		{"Decided to Enacting", StateDecided, StateEnacting, true},
		{"Decided to Enacted rejected", StateDecided, StateEnacted, false},
		{"Enacting to Enacted", StateEnacting, StateEnacted, true},
		{"Enacting to Deferred", StateEnacting, StateDeferred, true},
		{"Enacting to Failed", StateEnacting, StateFailed, true},
		{"Enacting to Triggered rejected", StateEnacting, StateTriggered, false},
		{"Enacted to Triggered", StateEnacted, StateTriggered, true},
		{"NoOp to Triggered", StateNoOp, StateTriggered, true},
		{"Rejected to Triggered", StateRejected, StateTriggered, true},
		{"Deferred to Triggered", StateDeferred, StateTriggered, true},
		{"Failed to Triggered", StateFailed, StateTriggered, true},
		{"Enacted to Evaluating rejected", StateEnacted, StateEvaluating, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("IsValidTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []orchestrationv1alpha1.RebalancingStateType{StateEnacted, StateNoOp, StateRejected, StateDeferred, StateFailed}
	for _, s := range terminal {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = false, want true", s)
		}
	}

	active := []orchestrationv1alpha1.RebalancingStateType{stateNone, StateTriggered, StateEvaluating, StateDecided, StateEnacting}
	for _, s := range active {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = true, want false", s)
		}
	}
}
