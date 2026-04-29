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

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/decision"
)

// OrchestrationProfileReconciler reconciles a OrchestrationProfile object
type OrchestrationProfileReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Recorder       record.EventRecorder
	ContextBuilder *decision.DecisionContextBuilder
	DecisionClient *decision.DecisionClient
}

// Never Ever delete this comments as they are used by kubebuilder to generate RBAC permissions for the controller.
// If you need to change the permissions,
// modify the verbs and resources in the comments below and then run "make generate" to update the generated code.

// +kubebuilder:rbac:groups=orchestration.hiro.io,resources=orchestrationprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestration.hiro.io,resources=orchestrationprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.hiro.io,resources=orchestrationprofiles/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=deployments;replicasets;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="batch",resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the OrchestrationProfile object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/reconcile

// Reconciliation flow:
//  1. Fetch the OrchestrationProfile.
//  2. Validate the spec — mark Error if invalid (user must fix).
//  3. Check the referenced application exists — mark Pending if not yet deployed.
//  4. Resolve pods belonging to the application.
//  5. Build placement status and derive overall profile status.
func (r *OrchestrationProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	profile := &orchestrationv1alpha1.OrchestrationProfile{}
	if err := r.Get(ctx, req.NamespacedName, profile); err != nil {
		if apierrors.IsNotFound(err) {
			// Profile was deleted. Nothing to reconcile — GC handles cleanup.
			logger.Info("OrchestrationProfile not found, likely deleted", "name", req.Name)
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch OrchestrationProfile")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// err := r.Get(ctx, req.NamespacedName, profile)
	// if err != nil {
	// 	logger.Error(err, "failed to reconcile OrchestrationProfile")
	// 	return ctrl.Result{}, err
	// }

	logger.Info("Reconciling OrchestrationProfile",
		"name", profile.Name,
		"strategy", profile.Spec.Placement.Strategy,
		"awareness", profile.Spec.Placement.Awareness,
		"appRef", fmt.Sprintf("%s/%s/%s",
			profile.Spec.ApplicationRef.Kind,
			profile.Spec.ApplicationRef.Namespace,
			profile.Spec.ApplicationRef.Name,
		),
	)

	// profile.Status.Status = "Active"
	// profile.Status.LastUpdatedTime = metav1.Now()

	// if err := r.Status().Update(ctx, profile); err != nil {
	// 	logger.Error(err, "failed to update OrchestrationProfile status")
	// 	return ctrl.Result{}, err
	// }
	// return ctrl.Result{}, nil

	// -------------------------------------------------------------------------
	// Step 2: Validate the spec.
	// Validation failures are permanent (user must fix spec) — mark as Error.
	// -------------------------------------------------------------------------
	if validationErrs := r.validateProfile(profile); len(validationErrs) > 0 {
		msg := validationErrs.ToAggregate().Error()
		logger.Error(validationErrs.ToAggregate(), "OrchestrationProfile spec validation failed", "name", profile.Name)
		r.Recorder.Event(profile, corev1.EventTypeWarning, EventReasonValidationFailed, msg)
		return r.updateStatus(ctx, profile, func(s *orchestrationv1alpha1.OrchestrationProfileStatus) {
			s.Status = StatusError
			s.Reason = msg
			s.PlacementStatus = orchestrationv1alpha1.PlacementStatus{
				Strategy: profile.Spec.Placement.Strategy,
			}
		})
	}

	// -------------------------------------------------------------------------
	// Step 3: Check if the referenced application exists.
	// If it doesn't exist yet, set Pending and wait.
	// The Deployment/StatefulSet/etc. watcher will re-trigger reconcile when
	// the application appears — no polling needed.
	// -------------------------------------------------------------------------
	appExists, err := r.applicationExists(ctx, profile.Spec.ApplicationRef)
	if err != nil {
		msg := fmt.Sprintf("error checking %s %s/%s: %v",
			profile.Spec.ApplicationRef.Kind,
			profile.Spec.ApplicationRef.Namespace,
			profile.Spec.ApplicationRef.Name,
			err,
		)
		logger.Error(err, "Failed to check application existence",
			"app", profile.Spec.ApplicationRef.Name,
			"kind", profile.Spec.ApplicationRef.Kind,
		)
		r.Recorder.Event(profile, corev1.EventTypeWarning, EventReasonApplicationLookupError, msg)
		return r.updateStatus(ctx, profile, func(s *orchestrationv1alpha1.OrchestrationProfileStatus) {
			s.Status = StatusError
			s.Reason = msg
			s.PlacementStatus = orchestrationv1alpha1.PlacementStatus{
				Strategy: profile.Spec.Placement.Strategy,
			}
		})
	}

	if !appExists {
		logger.Info("Referenced application does not exist yet, waiting",
			"app", profile.Spec.ApplicationRef.Name,
			"kind", profile.Spec.ApplicationRef.Kind,
			"namespace", profile.Spec.ApplicationRef.Namespace,
		)
		r.Recorder.Eventf(profile, corev1.EventTypeNormal, EventReasonApplicationNotFound,
			"Referenced %s %s/%s not found, waiting for it to be created",
			profile.Spec.ApplicationRef.Kind,
			profile.Spec.ApplicationRef.Namespace,
			profile.Spec.ApplicationRef.Name,
		)
		return r.updateStatus(ctx, profile, func(s *orchestrationv1alpha1.OrchestrationProfileStatus) {
			s.Status = StatusNoPods
			s.PlacementStatus = orchestrationv1alpha1.PlacementStatus{
				Strategy:     profile.Spec.Placement.Strategy,
				ObservedPods: 0,
				ReadyPods:    0,
				PendingPods:  0,
				FailedPods:   0,
				PodStatuses:  nil,
			}
		})
	}

	// -------------------------------------------------------------------------
	// Step 4: Resolve pods belonging to the referenced application.
	// -------------------------------------------------------------------------
	pods, err := r.findPodsForApplication(ctx, profile)
	if err != nil {
		msg := fmt.Sprintf("error finding pods for %s %s/%s: %v",
			profile.Spec.ApplicationRef.Kind,
			profile.Spec.ApplicationRef.Namespace,
			profile.Spec.ApplicationRef.Name,
			err,
		)
		logger.Error(err, "Failed to find pods for application",
			"app", profile.Spec.ApplicationRef.Name,
			"namespace", profile.Spec.ApplicationRef.Namespace,
		)
		r.Recorder.Event(profile, corev1.EventTypeWarning, EventReasonPodDiscoveryFailed, msg)
		return r.updateStatus(ctx, profile, func(s *orchestrationv1alpha1.OrchestrationProfileStatus) {
			s.Status = StatusError
			s.Reason = msg
		})
	}

	// -------------------------------------------------------------------------
	// Step 5: Build placement status and derive overall profile status.
	//
	// Status transition table:
	//   0 pods              → NoPods   (app exists but no pods scheduled yet)
	//   all pods running    → Active
	//   any failed pods     → Degraded
	//   mix pending+running → Partial  (partial rollout)
	//   all pods pending    → Pending
	// -------------------------------------------------------------------------
	placementStatus := buildPlacementStatus(profile, pods)
	overallStatus := deriveProfileStatus(
		placementStatus.ObservedPods,
		placementStatus.ReadyPods,
		placementStatus.FailedPods,
		placementStatus.PendingPods,
	)

	logger.Info("updating OrchestrationProfile status",
		"name", profile.Name,
		"status", overallStatus,
		"observed", placementStatus.ObservedPods,
		"ready", placementStatus.ReadyPods,
		"pending", placementStatus.PendingPods,
		"failed", placementStatus.FailedPods,
	)

	return r.updateStatus(ctx, profile, func(s *orchestrationv1alpha1.OrchestrationProfileStatus) {
		s.Status = overallStatus
		s.PlacementStatus = placementStatus
		// NOTE: RebalancingStatus is intentionally not touched here.
		// It is owned exclusively by the rebalancing subsystem.
	})
}

// SetupWithManager sets up the controller with the Manager.
//
// Event sources:
//
//  1. OrchestrationProfile (primary) — any CRUD event on the profile itself
//     triggers reconciliation directly.
//
//  2. Pod — pod phase changes (Pending→Running, Running→Failed, etc.) trigger
//     reconciliation of all profiles whose appRef.Namespace matches the pod's
//     namespace. This keeps PlacementStatus up to date as pods come and go.
//
//  3. Deployment / StatefulSet / ReplicaSet / Job — workload events trigger
//     reconciliation of profiles that reference that exact workload. This ensures
//     profiles react immediately when their application is first created or deleted,
//     rather than waiting for pod events (which arrive later in the lifecycle).
func (r *OrchestrationProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orchestrationv1alpha1.OrchestrationProfile{}).
		Watches(&corev1.Pod{}, r.podToProfileMapper()).
		Watches(&appsv1.Deployment{}, r.appToProfileMapper()).
		Watches(&appsv1.StatefulSet{}, r.appToProfileMapper()).
		Watches(&appsv1.ReplicaSet{}, r.appToProfileMapper()).
		Watches(&batchv1.Job{}, r.appToProfileMapper()).
		Named("orchestrationprofile").
		Complete(r)
}
