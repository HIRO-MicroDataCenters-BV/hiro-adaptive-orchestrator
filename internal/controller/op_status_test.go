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

// Pure unit tests for the status-computation helpers in op_status.go.
// These tests do not start envtest and have no external dependencies.
package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// podFixture builds a minimal corev1.Pod with the given phase.
// Conditions are set only when needed by individual tests.
func podFixture(name string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// readyRunningPod returns a Running pod whose PodReady condition is True.
func readyRunningPod(name string) corev1.Pod {
	pod := podFixture(name, corev1.PodRunning)
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	return pod
}

var _ = Describe("deriveProfileStatus", func() {
	// Parameters: (total, ready, failed, pending) → expected status string.
	// "ready" maps to ReadyPods (the running+ready count), not all Running pods.
	DescribeTable("maps pod counts to the correct status",
		func(total, ready, failed, pending int, expected string) {
			Expect(deriveProfileStatus(total, ready, failed, pending)).To(Equal(expected))
		},
		Entry("no pods at all → NoPods", 0, 0, 0, 0, StatusNoPods),
		Entry("all pods ready → Active", 3, 3, 0, 0, StatusActive),
		Entry("all pods pending → Pending", 3, 0, 0, 3, StatusPending),
		Entry("all pods failed, none running → Degraded", 3, 0, 3, 0, StatusDegraded),
		Entry("some failed, some running → Partial", 3, 1, 2, 0, StatusPartial),
		Entry("some pending, some running → Partial", 3, 1, 0, 2, StatusPartial),
		Entry("failed + pending + running → Partial", 5, 2, 1, 2, StatusPartial),
		Entry("running but not all ready (default) → Partial", 2, 1, 0, 0, StatusPartial),
	)
})

var _ = Describe("fetchPodStatuses", func() {
	Context("with no pods", func() {
		It("returns zero counts and empty slice", func() {
			ready, pending, failed, statuses := fetchPodStatuses(nil)
			Expect(ready).To(Equal(0))
			Expect(pending).To(Equal(0))
			Expect(failed).To(Equal(0))
			Expect(statuses).To(BeEmpty())
		})
	})

	Context("with ready running pods", func() {
		It("counts only Running+Ready pods as ready", func() {
			pods := []corev1.Pod{
				readyRunningPod("pod-1"),
				readyRunningPod("pod-2"),
			}
			ready, pending, failed, statuses := fetchPodStatuses(pods)
			Expect(ready).To(Equal(2))
			Expect(pending).To(Equal(0))
			Expect(failed).To(Equal(0))
			Expect(statuses).To(HaveLen(2))
		})

		It("does not count Running pod that is not Ready", func() {
			pod := podFixture("pod-not-ready", corev1.PodRunning)
			// PodReady condition absent — not ready
			ready, _, _, _ := fetchPodStatuses([]corev1.Pod{pod})
			Expect(ready).To(Equal(0))
		})
	})

	Context("with pending pods", func() {
		It("counts pending pods correctly", func() {
			pods := []corev1.Pod{
				podFixture("pod-1", corev1.PodPending),
				podFixture("pod-2", corev1.PodPending),
			}
			_, pending, _, _ := fetchPodStatuses(pods)
			Expect(pending).To(Equal(2))
		})

		It("captures the scheduling failure message for an Unschedulable pod", func() {
			pod := podFixture("unschedulable", corev1.PodPending)
			pod.Status.Conditions = []corev1.PodCondition{
				{
					Type:    corev1.PodScheduled,
					Status:  corev1.ConditionFalse,
					Message: "0/3 nodes available: insufficient memory",
				},
			}
			_, _, _, statuses := fetchPodStatuses([]corev1.Pod{pod})
			Expect(statuses[0].Reason).To(Equal("0/3 nodes available: insufficient memory"))
		})
	})

	Context("with failed pods", func() {
		It("counts failed pods correctly", func() {
			pods := []corev1.Pod{podFixture("pod-1", corev1.PodFailed)}
			_, _, failed, _ := fetchPodStatuses(pods)
			Expect(failed).To(Equal(1))
		})

		It("prefers the detailed message over the short reason field", func() {
			pod := podFixture("oom-pod", corev1.PodFailed)
			pod.Status.Reason = "OOMKilled"
			pod.Status.Message = "Container exceeded memory limit and was killed"
			_, _, _, statuses := fetchPodStatuses([]corev1.Pod{pod})
			Expect(statuses[0].Reason).To(Equal("Container exceeded memory limit and was killed"))
		})
	})

	Context("with mixed pod phases", func() {
		It("counts each phase independently", func() {
			pods := []corev1.Pod{
				readyRunningPod("ready-pod"),
				podFixture("pending-pod", corev1.PodPending),
				podFixture("failed-pod", corev1.PodFailed),
			}
			ready, pending, failed, statuses := fetchPodStatuses(pods)
			Expect(ready).To(Equal(1))
			Expect(pending).To(Equal(1))
			Expect(failed).To(Equal(1))
			Expect(statuses).To(HaveLen(3))
		})
	})

	Context("PodStatus fields", func() {
		It("populates Name, Namespace, and Status for each pod", func() {
			pod := readyRunningPod("my-pod")
			pod.Namespace = "staging"
			pod.Status.Phase = corev1.PodRunning

			_, _, _, statuses := fetchPodStatuses([]corev1.Pod{pod})
			Expect(statuses).To(HaveLen(1))
			Expect(statuses[0].Name).To(Equal("my-pod"))
			Expect(statuses[0].Namespace).To(Equal("staging"))
			Expect(statuses[0].Status).To(Equal(string(corev1.PodRunning)))
		})
	})
})

var _ = Describe("buildPlacementStatus", func() {
	It("propagates the strategy from the profile spec", func() {
		profile := &orchestrationv1alpha1.OrchestrationProfile{
			Spec: orchestrationv1alpha1.OrchestrationProfileSpec{
				Placement: orchestrationv1alpha1.PlacementSpec{Strategy: "Spread"},
			},
		}
		status := buildPlacementStatus(profile, nil)
		Expect(status.Strategy).To(Equal("Spread"))
	})

	It("sets ObservedPods to the total number of pods passed in", func() {
		profile := &orchestrationv1alpha1.OrchestrationProfile{
			Spec: orchestrationv1alpha1.OrchestrationProfileSpec{
				Placement: orchestrationv1alpha1.PlacementSpec{Strategy: "Balanced"},
			},
		}
		pods := []corev1.Pod{
			readyRunningPod("pod-1"),
			readyRunningPod("pod-2"),
			podFixture("pod-3", corev1.PodPending),
		}
		status := buildPlacementStatus(profile, pods)
		Expect(status.ObservedPods).To(Equal(3))
		Expect(status.ReadyPods).To(Equal(2))
		Expect(status.PendingPods).To(Equal(1))
		Expect(status.FailedPods).To(Equal(0))
		Expect(status.PodStatuses).To(HaveLen(3))
	})

	It("returns zero counts and empty PodStatuses when pods slice is nil", func() {
		profile := &orchestrationv1alpha1.OrchestrationProfile{
			Spec: orchestrationv1alpha1.OrchestrationProfileSpec{
				Placement: orchestrationv1alpha1.PlacementSpec{Strategy: "Packed"},
			},
		}
		status := buildPlacementStatus(profile, nil)
		Expect(status.ObservedPods).To(Equal(0))
		Expect(status.PodStatuses).To(BeEmpty())
	})
})
