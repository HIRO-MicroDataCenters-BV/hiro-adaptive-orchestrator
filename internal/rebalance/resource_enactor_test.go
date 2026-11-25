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

func TestApplyTargetResources(t *testing.T) {
	list := func(cpu, mem string) corev1.ResourceList {
		l := corev1.ResourceList{}
		if cpu != "" {
			l[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if mem != "" {
			l[corev1.ResourceMemory] = resource.MustParse(mem)
		}
		return l
	}

	tests := []struct {
		name                     string
		requests, limits         corev1.ResourceList
		targetCPU, targetMemory  string
		wantRequests, wantLimits corev1.ResourceList
	}{
		{
			name:     "limit twice the request stays twice the request",
			requests: list("50m", "64Mi"), limits: list("100m", "128Mi"),
			targetCPU: "75m", targetMemory: "96Mi",
			wantRequests: list("75m", "96Mi"), wantLimits: list("150m", "192Mi"),
		},
		{
			name:     "limit equal to the request stays equal (Guaranteed)",
			requests: list("100m", "128Mi"), limits: list("100m", "128Mi"),
			targetCPU: "200m", targetMemory: "256Mi",
			wantRequests: list("200m", "256Mi"), wantLimits: list("200m", "256Mi"),
		},
		{
			name:     "no limit stays no limit",
			requests: list("50m", "64Mi"), limits: nil,
			targetCPU: "75m", targetMemory: "96Mi",
			wantRequests: list("75m", "96Mi"), wantLimits: nil,
		},
		{
			name:     "limit without a request is treated as request equal to limit",
			requests: nil, limits: list("100m", ""),
			targetCPU:    "150m",
			wantRequests: list("150m", ""), wantLimits: list("150m", ""),
		},
		{
			name:     "only cpu targeted, memory untouched",
			requests: list("50m", "64Mi"), limits: list("100m", "128Mi"),
			targetCPU:    "75m",
			wantRequests: list("75m", "64Mi"), wantLimits: list("150m", "128Mi"),
		},
		{
			name:     "uneven ratio rounds the limit up, never below the request",
			requests: list("30m", ""), limits: list("100m", ""),
			targetCPU:    "40m",
			wantRequests: list("40m", ""), wantLimits: list("134m", ""),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container := &corev1.Container{Resources: corev1.ResourceRequirements{Requests: tt.requests, Limits: tt.limits}}
			var cpu, mem resource.Quantity
			if tt.targetCPU != "" {
				cpu = resource.MustParse(tt.targetCPU)
			}
			if tt.targetMemory != "" {
				mem = resource.MustParse(tt.targetMemory)
			}

			applyTargetResources(container, cpu, mem)

			for _, check := range []struct {
				what      string
				got, want corev1.ResourceList
			}{
				{"requests", container.Resources.Requests, tt.wantRequests},
				{"limits", container.Resources.Limits, tt.wantLimits},
			} {
				if len(check.got) != len(check.want) {
					t.Fatalf("%s = %v, want %v", check.what, check.got, check.want)
				}
				for name, want := range check.want {
					if got := check.got[name]; got.Cmp(want) != 0 {
						t.Errorf("%s[%s] = %s, want %s", check.what, name, got.String(), want.String())
					}
				}
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
