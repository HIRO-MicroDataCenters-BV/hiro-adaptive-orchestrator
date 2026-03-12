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

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Status Update Helper
// -----------------------------------------------------------------------------

// updateStatus applies a mutator function to the profile's Status, stamps
// LastUpdatedTime, and persists the change via the status subresource.
//
// Optimistic concurrency conflicts (another writer updated the resource between
// our Get and Update) are handled by requeuing rather than hard-failing, so
// the next reconcile picks up the latest version automatically.
func (r *OrchestrationProfileReconciler) updateStatus(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	mutate func(*orchestrationv1alpha1.OrchestrationProfileStatus),
) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	mutate(&profile.Status)
	profile.Status.LastUpdatedTime = metav1.Now()

	if err := r.Status().Update(ctx, profile); err != nil {
		if apierrors.IsConflict(err) {
			// Concurrent writer (e.g. rebalancer) updated the status.
			// Requeue so we re-read the latest version and retry.
			logger.Info("status update conflict, requeuing", "name", profile.Name)
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "failed to update OrchestrationProfile status", "name", profile.Name)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// -----------------------------------------------------------------------------
// Placement Status Builder
// -----------------------------------------------------------------------------

// buildPlacementStatus constructs a full PlacementStatus from the observed pod list.
//
// For each pod it captures:
//   - Id        — pod UID (stable across restarts, useful for external systems)
//   - Name      — pod name
//   - Namespace — pod namespace
//   - Status    — Kubernetes phase (Running, Pending, Failed, Succeeded, Unknown)
//   - Reason    — scheduling failure message (Pending pods) or exit message (Failed pods)
func buildPlacementStatus(
	profile *orchestrationv1alpha1.OrchestrationProfile,
	pods []corev1.Pod,
) orchestrationv1alpha1.PlacementStatus {
	podStatuses := make([]orchestrationv1alpha1.PodStatus, 0, len(pods))
	readyCount := 0
	pendingCount := 0

	for _, pod := range pods {
		ps := orchestrationv1alpha1.PodStatus{
			Id:        string(pod.UID),
			Name:      pod.Name,
			Namespace: pod.Namespace,
			Status:    string(pod.Status.Phase),
			Reason:    pod.Status.Reason,
		}

		switch pod.Status.Phase {
		case corev1.PodRunning:
			if isPodReady(pod) {
				readyCount++
			}

		case corev1.PodPending:
			pendingCount++
			// Surface the scheduling failure message when the pod is Unschedulable.
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled &&
					cond.Status == corev1.ConditionFalse &&
					cond.Message != "" {
					ps.Reason = cond.Message
					break
				}
			}

		case corev1.PodFailed:
			// Prefer the detailed message over the short reason field.
			if pod.Status.Message != "" {
				ps.Reason = pod.Status.Message
			}
		}

		podStatuses = append(podStatuses, ps)
	}

	return orchestrationv1alpha1.PlacementStatus{
		Strategy:     profile.Spec.Placement.Strategy,
		ObservedPods: len(pods),
		ReadyPods:    readyCount,
		PendingPods:  pendingCount,
		PodStatuses:  podStatuses,
	}
}

// -----------------------------------------------------------------------------
// Profile Status Derivation
// -----------------------------------------------------------------------------

// deriveProfileStatus maps the observed pod states to a single profile-level
// status string, following this transition table:
//
//	0 pods              → NoPods   (app exists but no pods scheduled yet)
//	all pods running    → Active
//	any failed pods     → Degraded
//	mix pending+running → Partial  (partial rollout or mixed health)
//	all pods pending    → Pending   (still scheduling)
func deriveProfileStatus(pods []corev1.Pod) string {
	if len(pods) == 0 {
		return StatusNoPods
	}

	running, failed, pending := 0, 0, 0
	for _, pod := range pods {
		switch pod.Status.Phase {
		case corev1.PodRunning:
			running++
		case corev1.PodFailed:
			failed++
		case corev1.PodPending:
			pending++
		}
	}

	switch {
	case failed > 0:
		return StatusDegraded
	case pending > 0 && running > 0:
		return StatusPartial
	case running > 0 && pending == 0:
		return StatusActive
	case pending > len(pods):
		return StatusPending
	default:
		return StatusError
	}
}

// isPodReady returns true if the pod has the PodReady condition set to True.
func isPodReady(pod corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
