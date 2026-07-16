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

import "testing"

func TestTopScoreNodeName(t *testing.T) {
	tests := []struct {
		name   string
		scores []NodeScore
		want   string
	}{
		{"empty", nil, ""},
		{"single", []NodeScore{{NodeName: "node-a", Score: 42}}, "node-a"},
		{
			"picks highest",
			[]NodeScore{
				{NodeName: "node-a", Score: 10},
				{NodeName: "node-b", Score: 90},
				{NodeName: "node-c", Score: 50},
			},
			"node-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := topScoreNodeName(tt.scores); got != tt.want {
				t.Errorf("topScoreNodeName() = %q, want %q", got, tt.want)
			}
		})
	}
}
