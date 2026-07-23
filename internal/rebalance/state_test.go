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
		{"Watching to Triggered", StateWatching, StateTriggered, true},
		{"Watching to Evaluating rejected", StateWatching, StateEvaluating, false},
		{"Triggered to Evaluating", StateTriggered, StateEvaluating, true},
		{"Triggered to Decided rejected", StateTriggered, StateDecided, false},
		{"Evaluating to Decided", StateEvaluating, StateDecided, true},
		{"Evaluating to Watching", StateEvaluating, StateWatching, true},
		{"Evaluating to Enacting rejected", StateEvaluating, StateEnacting, false},
		{"Decided to Enacting", StateDecided, StateEnacting, true},
		{"Decided to Watching rejected", StateDecided, StateWatching, false},
		{"Enacting to Watching", StateEnacting, StateWatching, true},
		{"Enacting to Triggered rejected", StateEnacting, StateTriggered, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsValidTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("IsValidTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}
