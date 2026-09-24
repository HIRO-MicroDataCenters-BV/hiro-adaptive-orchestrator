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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestContainerResourcesConverged(t *testing.T) {
	target := func(cpu, mem string) (resource.Quantity, resource.Quantity) {
		var c, m resource.Quantity
		if cpu != "" {
			c = resource.MustParse(cpu)
		}
		if mem != "" {
			m = resource.MustParse(mem)
		}
		return c, m
	}

	tests := []struct {
		name          string
		pod           *corev1.Pod
		targetCPU     string
		targetMemory  string
		wantConverged bool
	}{
		{
			name:          "no container statuses at all",
			pod:           &corev1.Pod{},
			targetCPU:     "200m",
			wantConverged: false,
		},
		{
			name: "container status present but no Resources field yet",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: testResourceContainerName},
			}}},
			targetCPU:     "200m",
			wantConverged: false,
		},
		{
			name: "cpu target matches, memory target left unspecified",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: testResourceContainerName, Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
				}},
			}}},
			targetCPU:     "200m",
			wantConverged: true,
		},
		{
			name: "cpu target does not match",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: testResourceContainerName, Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}},
			}}},
			targetCPU:     "200m",
			wantConverged: false,
		},
		{
			name: "both cpu and memory targets set, both match",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: testResourceContainerName, Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				}},
			}}},
			targetCPU:     "200m",
			targetMemory:  "256Mi",
			wantConverged: true,
		},
		{
			name: "cpu matches but memory target not yet reflected",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: testResourceContainerName, Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				}},
			}}},
			targetCPU:     "200m",
			targetMemory:  "256Mi",
			wantConverged: false,
		},
		{
			name: "matching container name required — different container's status ignored",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "some-other-container", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
				}},
			}}},
			targetCPU:     "200m",
			wantConverged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, mem := target(tt.targetCPU, tt.targetMemory)
			got := containerResourcesConverged(tt.pod, testResourceContainerName, cpu, mem)
			if got != tt.wantConverged {
				t.Errorf("containerResourcesConverged() = %v, want %v", got, tt.wantConverged)
			}
		})
	}
}

func TestResizeRejectedReason(t *testing.T) {
	tests := []struct {
		name         string
		conditions   []corev1.PodCondition
		wantRejected bool
	}{
		{
			name:         "no conditions",
			wantRejected: false,
		},
		{
			name: "PodResizePending/Infeasible is a definitive rejection",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodResizePending, Reason: corev1.PodReasonInfeasible, Message: "won't fit"},
			},
			wantRejected: true,
		},
		{
			name: "PodResizeInProgress/Error is a definitive rejection",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodResizeInProgress, Reason: corev1.PodReasonError, Message: "actuation failed"},
			},
			wantRejected: true,
		},
		{
			name: "PodResizePending/Deferred is NOT a rejection — may still succeed",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodResizePending, Reason: corev1.PodReasonDeferred, Message: "not enough headroom right now"},
			},
			wantRejected: false,
		},
		{
			name: "PodResizeInProgress with no Error reason is NOT a rejection — still working on it",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodResizeInProgress, Reason: "", Message: "actuating"},
			},
			wantRejected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: tt.conditions}}
			reason, rejected := resizeRejectedReason(pod)
			if rejected != tt.wantRejected {
				t.Errorf("rejected = %v, want %v", rejected, tt.wantRejected)
			}
			if rejected && reason == "" {
				t.Errorf("rejected but reason is empty, want the condition's Message")
			}
			if !rejected && reason != "" {
				t.Errorf("reason = %q, want empty when not rejected", reason)
			}
		})
	}
}
