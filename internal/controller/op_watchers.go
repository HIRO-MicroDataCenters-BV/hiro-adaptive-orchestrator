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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Event Mappers
// -----------------------------------------------------------------------------

// podToProfileMapper returns a handler.EventHandler that maps Pod events
// to OrchestrationProfile reconcile requests. It uses podToProfileMapFunc
// to determine which profiles should be reconciled when a Pod changes.
func (r *OrchestrationProfileReconciler) podToProfileMapper() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(r.podToProfileMapFunc)
}

// appToProfileMapper returns a handler.EventHandler that maps workload events
// (Deployment, StatefulSet, ReplicaSet, Job) to OrchestrationProfile reconcile requests.
// It uses appToProfileMapFunc to determine which profiles reference the workload.
func (r *OrchestrationProfileReconciler) appToProfileMapper() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(r.appToProfileMapFunc)
}

// -----------------------------------------------------------------------------
// Mapping Functions
// -----------------------------------------------------------------------------

// podToProfileMapFunc maps a Pod event to OrchestrationProfile reconcile requests.
// It enqueues all profiles whose ApplicationRef.Namespace matches the Pod's namespace.
// This is intentionally broad; the reconciler will filter further as needed.
func (r *OrchestrationProfileReconciler) podToProfileMapFunc(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	return r.profilesReferencingNamespace(ctx, pod.Namespace, pod.Name, "Pod")
}

// appToProfileMapFunc maps a workload event (Deployment, StatefulSet, ReplicaSet, Job)
// to OrchestrationProfile reconcile requests. It enqueues profiles that reference
// the exact workload by name and namespace.
func (r *OrchestrationProfileReconciler) appToProfileMapFunc(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	return r.profilesReferencingApp(ctx, obj.GetNamespace(), obj.GetName())
}

// -----------------------------------------------------------------------------
// Shared Mapper Utilities
// -----------------------------------------------------------------------------

// profilesReferencingNamespace lists all OrchestrationProfiles and returns a
// reconcile.Request for each profile whose ApplicationRef.Namespace matches the given namespace.
// Used by podToProfileMapFunc to fan out Pod events to relevant profiles.
func (r *OrchestrationProfileReconciler) profilesReferencingNamespace(
	ctx context.Context,
	namespace string,
	triggerName string,
	triggerKind string,
) []reconcile.Request {
	logger := logf.FromContext(ctx)

	profileList := &orchestrationv1alpha1.OrchestrationProfileList{}
	if err := r.List(ctx, profileList); err != nil {
		logger.Error(err, "Failed to list OrchestrationProfiles during event mapping",
			"triggerKind", triggerKind,
			"triggerName", triggerName,
		)
		return nil
	}

	var requests []reconcile.Request
	for _, profile := range profileList.Items {
		if profile.Spec.ApplicationRef.Namespace == namespace {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: profile.Name,
				},
			})
		}
	}

	if len(requests) > 0 {
		logger.Info("Event triggered profile reconciliation",
			"triggerKind", triggerKind,
			"triggerName", triggerName,
			"namespace", namespace,
			"profilesEnqueued", len(requests),
		)
	}

	return requests
}

// profilesReferencingApp lists all OrchestrationProfiles and returns a
// reconcile.Request for each profile whose ApplicationRef matches the given namespace and name.
// Used by appToProfileMapFunc to map workload events to profiles that reference them.
func (r *OrchestrationProfileReconciler) profilesReferencingApp(
	ctx context.Context,
	namespace string,
	appName string,
) []reconcile.Request {
	logger := logf.FromContext(ctx)

	profileList := &orchestrationv1alpha1.OrchestrationProfileList{}
	if err := r.List(ctx, profileList); err != nil {
		logger.Error(err, "Failed to list OrchestrationProfiles during app event mapping",
			"app", appName,
			"namespace", namespace,
		)
		return nil
	}

	var requests []reconcile.Request
	for _, profile := range profileList.Items {
		appRef := profile.Spec.ApplicationRef
		if appRef.Name == appName && appRef.Namespace == namespace {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: profile.Name,
				},
			})
		}
	}

	if len(requests) > 0 {
		logger.Info("Workload event triggered profile reconciliation",
			"workload", appName,
			"namespace", namespace,
			"profilesEnqueued", len(requests),
		)
	}

	return requests
}
