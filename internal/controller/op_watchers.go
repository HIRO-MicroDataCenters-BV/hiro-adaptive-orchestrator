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
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
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
	// return r.profilesReferencingNamespace(ctx, pod.Namespace, pod.Name, "Pod")
	return r.podToProfileMapViaAppToProfile(ctx, pod)
}

// appToProfileMapFunc maps a workload event (Deployment, StatefulSet, ReplicaSet, Job)
// to OrchestrationProfile reconcile requests. It enqueues profiles that reference
// the exact workload by name and namespace.
func (r *OrchestrationProfileReconciler) appToProfileMapFunc(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	return r.profilesReferencingApp(ctx, obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName())
}

// -----------------------------------------------------------------------------
// Shared Mapper Utilities
// -----------------------------------------------------------------------------

func (r *OrchestrationProfileReconciler) profilesReferencingApp(
	ctx context.Context,
	kind string,
	namespace string,
	appName string,
) []reconcile.Request {
	indexKey := namespace + "/" + appName
	return r.profilesByAppRefIndex(ctx, indexKey, kind, appName)
}

// =============================================================================
// Pod → Profile mapping
//
// Problem: given a Pod event, which OrchestrationProfile(s) govern it?
//
// Two-step solution:
//   Step 1 — resolveAppFromPod: walk OwnerRef chain to find the top-level
//             workload name that the profile's applicationRef points to.
//             Pod → ReplicaSet → Deployment  (most common)
//             Pod → StatefulSet
//             Pod → Job
//
//   Step 2 — field index lookup: use ProfileByAppRefIndex to find profiles
//             whose spec.applicationRef matches that workload. O(1) cache hit.
//
// This is the explicit pod→profile mapping the user story requires.
// =============================================================================

func (r *OrchestrationProfileReconciler) podToProfileMapViaAppToProfile(
	ctx context.Context,
	pod *corev1.Pod,
) []reconcile.Request {
	logger := logf.FromContext(ctx)

	// Step 1: resolve the top-level workload this pod belongs to
	appName, appNamespace := r.resolveAppFromPod(ctx, pod)
	if appName == "" {
		// Pod has no recognized workload owner — not governed by any profile
		logger.V(1).Info("pod has no recognized workload owner, skipping",
			"pod", pod.Name, "namespace", pod.Namespace)
		return nil
	}

	// Step 2: O(1) index lookup — find profiles referencing this app
	indexKey := appNamespace + "/" + appName
	return r.profilesByAppRefIndex(ctx, indexKey, "Pod", pod.Name)
}

// resolveAppFromPod walks the pod's OwnerReferences to find the name of the
// top-level workload (Deployment, StatefulSet, Job, or standalone ReplicaSet).
//
// Walk logic:
//
//	Pod.OwnerRef → ReplicaSet → check RS.OwnerRef → Deployment?
//	                                               → standalone RS if not
//	Pod.OwnerRef → StatefulSet  (direct)
//	Pod.OwnerRef → Job          (direct)
//
// Returns ("", "") if no recognized workload owner is found.
func (r *OrchestrationProfileReconciler) resolveAppFromPod(
	ctx context.Context,
	pod *corev1.Pod,
) (appName, appNamespace string) {
	appName, appNamespace, _ = utils.ResolveAppFromPod(ctx, r.Client, pod)
	return
}

// =============================================================================
// Shared index lookup — used by BOTH mappers
// =============================================================================

// profilesByAppRefIndex queries the ProfileByAppRefIndex field index with the
// given key and returns reconcile requests for all matching profiles.
//
// This replaces profilesReferencingNamespace and profilesReferencingApp —
// both were O(n) list-and-scan. This is O(1).
func (r *OrchestrationProfileReconciler) profilesByAppRefIndex(
	ctx context.Context,
	indexKey string,
	triggerKind string,
	triggerName string,
) []reconcile.Request {
	logger := logf.FromContext(ctx)

	profileList := &orchestrationv1alpha1.OrchestrationProfileList{}
	if err := r.List(ctx, profileList,
		client.MatchingFields{ProfileByAppRefIndex: indexKey},
	); err != nil {
		logger.Error(err, "failed to look up profiles by app index",
			"indexKey", indexKey,
			"triggerKind", triggerKind,
			"triggerName", triggerName,
		)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(profileList.Items))
	for _, profile := range profileList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name: profile.Name, // cluster-scoped: no namespace
			},
		})
	}

	if len(requests) > 0 {
		logger.Info("index lookup triggered profile reconciliation",
			"triggerKind", triggerKind,
			"triggerName", triggerName,
			"indexKey", indexKey,
			"profilesEnqueued", len(requests),
		)
	}

	return requests
}
