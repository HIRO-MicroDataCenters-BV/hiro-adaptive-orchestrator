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

	corev1 "k8s.io/api/core/v1"
)

const controlPlaneTaint = "node-role.kubernetes.io/control-plane"

func taintedNode(name string, effect corev1.TaintEffect) *corev1.Node {
	node := testNode(name, true)
	node.Spec.Taints = []corev1.Taint{{Key: controlPlaneTaint, Effect: effect}}
	return node
}

func TestFilterTolerated(t *testing.T) {
	plain := testNode("worker", true)
	noSchedule := taintedNode("control-plane", corev1.TaintEffectNoSchedule)
	noExecute := taintedNode("draining", corev1.TaintEffectNoExecute)
	prefer := taintedNode("soft", corev1.TaintEffectPreferNoSchedule)
	nodes := []*corev1.Node{plain, noSchedule, noExecute, prefer}

	tolerating := *testAppPod("app-a-1", "worker", corev1.PodRunning)
	tolerating.Spec.Tolerations = []corev1.Toleration{{
		Key: controlPlaneTaint, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	}}

	cases := []struct {
		name string
		pods []corev1.Pod
		want []string
	}{
		{"no tolerations keeps only untainted and soft-tainted", []corev1.Pod{*testAppPod("app-a-1", "worker", corev1.PodRunning)},
			[]string{"worker", "soft"}},
		{"no pods behaves like no tolerations", nil, []string{"worker", "soft"}},
		{"tolerated NoSchedule taint is kept", []corev1.Pod{tolerating}, []string{"worker", "control-plane", "soft"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterTolerated(context.Background(), nodes, tc.pods)
			names := make([]string, 0, len(got))
			for _, n := range got {
				names = append(names, n.Name)
			}
			if len(names) != len(tc.want) {
				t.Fatalf("kept %v, want %v", names, tc.want)
			}
			for i := range names {
				if names[i] != tc.want[i] {
					t.Fatalf("kept %v, want %v", names, tc.want)
				}
			}
		})
	}
}
