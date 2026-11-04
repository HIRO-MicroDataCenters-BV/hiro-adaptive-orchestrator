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
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/metrics"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// -----------------------------------------------------------------------------
// Status Update Helper
// -----------------------------------------------------------------------------

// updateStatus applies a mutator function to the profile's Status, stamps
// LastUpdatedTime, and persists the change via the status subresource.
//
// Skips the write entirely if the mutation didn't change anything observable —
// avoids a self-triggered reconcile loop on every periodic tick.
// RebalancingStatus is excluded from the comparison: this reconciler never
// touches it (see the NOTE in Reconcile), it's owned by the rebalance engine.
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

	previous := profile.Status.DeepCopy()

	mutate(&profile.Status)

	unchanged := profile.Status.Status == previous.Status &&
		profile.Status.Reason == previous.Reason &&
		reflect.DeepEqual(profile.Status.PlacementStatus, previous.PlacementStatus) &&
		profile.Generation == previous.ObservedGeneration
	if unchanged {
		return ctrl.Result{}, nil
	}

	profile.Status.LastUpdatedTime = metav1.Now()
	profile.Status.ObservedGeneration = profile.Generation
	setReadyCondition(profile)

	// Emit an event whenever the status transitions to a new value.
	if profile.Status.Status != previous.Status {
		r.emitStatusTransitionEvent(profile)
		metrics.ControllerProfileStatusTransitionsTotal.WithLabelValues(string(previous.Status), string(profile.Status.Status)).Inc()
	}

	if err := r.Status().Update(ctx, profile); err != nil {
		if apierrors.IsConflict(err) {
			logger.Info("reconciler: status update conflict, requeuing", "name", profile.Name)
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Error(err, "reconciler: failed to update status", "name", profile.Name)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// placementStatusMessage renders a human-readable summary of status, shared
// by the recorded Event and the Ready condition so they never drift apart.
func placementStatusMessage(
	status orchestrationv1alpha1.ProfileStatus,
	reason string,
	ps orchestrationv1alpha1.PlacementStatus,
) string {
	switch status {
	case StatusActive:
		return fmt.Sprintf("All %d pod(s) are running and ready (%s strategy)", ps.ReadyPods, ps.Strategy)
	case StatusPending:
		return fmt.Sprintf("%d pod(s) are still being scheduled (%s strategy)", ps.PendingPods, ps.Strategy)
	case StatusNoPods:
		return fmt.Sprintf("Application has no pods yet (%s strategy)", ps.Strategy)
	case StatusPartial:
		return fmt.Sprintf("%d/%d pod(s) ready, %d pending, %d failed (%s strategy)",
			ps.ReadyPods, ps.ObservedPods, ps.PendingPods, ps.FailedPods, ps.Strategy)
	case StatusDegraded:
		return fmt.Sprintf("%d pod(s) failed out of %d observed (%s strategy)", ps.FailedPods, ps.ObservedPods, ps.Strategy)
	case StatusError:
		return reason
	default:
		return "status not yet observed"
	}
}

// emitStatusTransitionEvent fires a Kubernetes event describing the new status.
// Called only when the status value actually changes to avoid event spam.
func (r *OrchestrationProfileReconciler) emitStatusTransitionEvent(
	profile *orchestrationv1alpha1.OrchestrationProfile,
) {
	msg := placementStatusMessage(profile.Status.Status, profile.Status.Reason, profile.Status.PlacementStatus)
	switch profile.Status.Status {
	case StatusActive:
		r.Recorder.Event(profile, corev1.EventTypeNormal, EventReasonPlacementActive, msg)
	case StatusPending:
		r.Recorder.Event(profile, corev1.EventTypeNormal, EventReasonPlacementPending, msg)
	case StatusNoPods:
		r.Recorder.Event(profile, corev1.EventTypeNormal, EventReasonApplicationNoPods, msg)
	case StatusPartial:
		r.Recorder.Event(profile, corev1.EventTypeWarning, EventReasonPlacementPartial, msg)
	case StatusDegraded:
		r.Recorder.Event(profile, corev1.EventTypeWarning, EventReasonPlacementDegraded, msg)
	case StatusError:
		r.Recorder.Event(profile, corev1.EventTypeWarning, EventReasonPlacementError, "Profile entered error state: "+msg)
	}
}

// setReadyCondition derives the Ready condition from the just-computed
// Status — True only when Status is Active.
func setReadyCondition(profile *orchestrationv1alpha1.OrchestrationProfile) {
	status := profile.Status.Status

	condStatus := metav1.ConditionFalse
	reason := string(status)
	switch status {
	case "":
		condStatus = metav1.ConditionUnknown
		reason = "Unknown"
	case StatusActive:
		condStatus = metav1.ConditionTrue
	}

	apimeta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:    orchestrationv1alpha1.ConditionReady,
		Status:  condStatus,
		Reason:  reason,
		Message: placementStatusMessage(status, profile.Status.Reason, profile.Status.PlacementStatus),
	})
}

// -----------------------------------------------------------------------------
// Fetching Observed Pod Statuses
// -----------------------------------------------------------------------------

// fetchPodStatuses processes the list of pods and counts how many are ready, pending, or failed.
// It also constructs a detailed list of PodStatus for each observed pod.
func fetchPodStatuses(
	pods []corev1.Pod,
) (ready, pending, failed int, podStatuses []orchestrationv1alpha1.PodStatus) {
	podStatuses = make([]orchestrationv1alpha1.PodStatus, 0, len(pods))
	for _, pod := range pods {
		ps := orchestrationv1alpha1.PodStatus{
			Id:        string(pod.UID),
			Name:      pod.Name,
			Namespace: pod.Namespace,
			NodeName:  pod.Spec.NodeName,
			Status:    string(pod.Status.Phase),
			Reason:    pod.Status.Reason,
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			if isPodReady(pod) {
				ready++
			}
		case corev1.PodPending:
			pending++
			// Surface the scheduling failure message when the pod is Unschedulable.
			if msg := utils.SchedulingFailureMessage(&pod); msg != "" {
				ps.Reason = msg
			}
		case corev1.PodFailed:
			failed++
			// Prefer the detailed message over the short reason field.
			if pod.Status.Message != "" {
				ps.Reason = pod.Status.Message
			}
		}
		podStatuses = append(podStatuses, ps)
	}
	return ready, pending, failed, podStatuses
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
	readyCount, pendingCount, failedCount, podStatuses := fetchPodStatuses(pods)

	return orchestrationv1alpha1.PlacementStatus{
		Strategy:     string(profile.Spec.Placement.Strategy),
		ObservedPods: len(pods),
		ReadyPods:    readyCount,
		PendingPods:  pendingCount,
		FailedPods:   failedCount,
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
func deriveProfileStatus(total, running, failed, pending int) orchestrationv1alpha1.ProfileStatus {
	if total == 0 {
		return StatusNoPods
	}
	switch {
	case failed > 0 && running == 0:
		return StatusDegraded // all pods failed
	case failed > 0 || (pending > 0 && running > 0):
		return StatusPartial // mix of states
	case pending > 0 && running == 0:
		return StatusPending // all still scheduling
	case running == total:
		return StatusActive // all ready
	default:
		return StatusPartial // running but not all ready yet
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
