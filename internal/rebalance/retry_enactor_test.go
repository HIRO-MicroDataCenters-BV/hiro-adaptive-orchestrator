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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newRetryTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding appsv1 scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestRetryPendingSchedule_DeletesUnscheduledPendingPod(t *testing.T) {
	pending := testPod("app-a-pending", "", corev1.PodPending)
	c := newRetryTestClient(t, testDeployment(), pending)
	profile := testProfileWithConditions(TriggerEnergyThreshold)

	if err := retryPendingSchedule(context.Background(), c, profile); err != nil {
		t.Fatalf("retryPendingSchedule: %v", err)
	}

	got := &corev1.Pod{}
	err := c.Get(context.Background(), types.NamespacedName{Name: "app-a-pending", Namespace: "default"}, got)
	if err == nil {
		t.Error("expected pending pod to be deleted, but it still exists")
	}
}

func TestRetryPendingSchedule_LeavesScheduledPendingPodAlone(t *testing.T) {
	// Pending but already assigned a node (e.g. still ContainerCreating) —
	// not the energy-gate case, must not be touched.
	scheduledPending := testPod("app-a-scheduled-pending", "node-a", corev1.PodPending)
	c := newRetryTestClient(t, testDeployment(), scheduledPending)
	profile := testProfileWithConditions(TriggerEnergyThreshold)

	err := retryPendingSchedule(context.Background(), c, profile)
	if err == nil {
		t.Fatal("expected an error (no unscheduled pending pod found), got nil")
	}

	got := &corev1.Pod{}
	if getErr := c.Get(context.Background(), types.NamespacedName{Name: "app-a-scheduled-pending", Namespace: "default"}, got); getErr != nil {
		t.Errorf("expected scheduled-but-pending pod to remain untouched, Get failed: %v", getErr)
	}
}

func TestRetryPendingSchedule_LeavesRunningPodAlone(t *testing.T) {
	running := testPod("app-a-running", "node-a", corev1.PodRunning)
	c := newRetryTestClient(t, testDeployment(), running)
	profile := testProfileWithConditions(TriggerEnergyThreshold)

	err := retryPendingSchedule(context.Background(), c, profile)
	if err == nil {
		t.Fatal("expected an error (no pending pod found), got nil")
	}

	got := &corev1.Pod{}
	if getErr := c.Get(context.Background(), types.NamespacedName{Name: "app-a-running", Namespace: "default"}, got); getErr != nil {
		t.Errorf("expected running pod to remain untouched, Get failed: %v", getErr)
	}
}

func TestRetryPendingSchedule_NoPendingPodReturnsError(t *testing.T) {
	c := newRetryTestClient(t, testDeployment())
	profile := testProfileWithConditions(TriggerEnergyThreshold)

	if err := retryPendingSchedule(context.Background(), c, profile); err == nil {
		t.Fatal("expected an error when there's nothing to retry, got nil")
	}
}

func TestRetryPendingSchedule_DeletesOnlyMatchingCandidates(t *testing.T) {
	pending := testPod("app-a-pending", "", corev1.PodPending)
	running := testPod("app-a-running", "node-a", corev1.PodRunning)
	scheduledPending := testPod("app-a-scheduled-pending", "node-b", corev1.PodPending)
	c := newRetryTestClient(t, testDeployment(), pending, running, scheduledPending)
	profile := testProfileWithConditions(TriggerEnergyThreshold)

	if err := retryPendingSchedule(context.Background(), c, profile); err != nil {
		t.Fatalf("retryPendingSchedule: %v", err)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a-pending", Namespace: "default"}, &corev1.Pod{}); err == nil {
		t.Error("expected the unscheduled pending pod to be deleted")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a-running", Namespace: "default"}, &corev1.Pod{}); err != nil {
		t.Errorf("running pod should remain untouched: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app-a-scheduled-pending", Namespace: "default"}, &corev1.Pod{}); err != nil {
		t.Errorf("scheduled-but-pending pod should remain untouched: %v", err)
	}
}
