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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// applicationExists checks whether the workload referenced by the profile
// currently exists in the API server.
//
// Returns:
//   - (true, nil)  — workload found and readable.
//   - (false, nil) — workload not found (NotFound); not an error, just not deployed yet.
//   - (false, err) — unexpected API error; caller should mark profile as Error.
func (r *OrchestrationProfileReconciler) applicationExists(
	ctx context.Context,
	appRef orchestrationv1alpha1.ApplicationReference,
) (bool, error) {
	key := types.NamespacedName{
		Name:      appRef.Name,
		Namespace: appRef.Namespace,
	}

	var obj client.Object
	switch appRef.Kind {
	case "Deployment":
		obj = &appsv1.Deployment{}
	case "StatefulSet":
		obj = &appsv1.StatefulSet{}
	case "Job":
		obj = &batchv1.Job{}
	case "ReplicaSet":
		obj = &appsv1.ReplicaSet{}
	default:
		// Unknown kind was already caught in validation.
		// Treat as non-existent rather than raising a hard error.
		return false, nil
	}

	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("checking existence of %s %s/%s: %w",
			appRef.Kind, appRef.Namespace, appRef.Name, err)
	}

	return true, nil
}

// findPodsForApplication resolves the workload's pod label selector and lists
// all pods that match it in the application's namespace.
//
// If the selector cannot be resolved (e.g. unknown kind), it falls back to the
// conventional {"app": appRef.Name} label to avoid returning zero pods silently.
func (r *OrchestrationProfileReconciler) findPodsForApplication(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
) ([]corev1.Pod, error) {
	logger := logf.FromContext(ctx)
	appRef := profile.Spec.ApplicationRef

	labelSelector, err := r.resolveLabelSelector(ctx, appRef)
	if err != nil {
		return nil, fmt.Errorf("resolving label selector for %s %s/%s: %w",
			appRef.Kind, appRef.Namespace, appRef.Name, err)
	}

	// Fallback: if selector resolution returned nothing, use the conventional app label.
	if len(labelSelector) == 0 {
		logger.Info("no label selector resolved, falling back to app label",
			"app", appRef.Name,
			"namespace", appRef.Namespace,
		)
		labelSelector = map[string]string{"app": appRef.Name}
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(appRef.Namespace),
		client.MatchingLabels(labelSelector),
	); err != nil {
		return nil, fmt.Errorf("listing pods for %s/%s with labels %v: %w",
			appRef.Namespace, appRef.Name, labelSelector, err)
	}

	logger.Info("resolved pods for application",
		"app", appRef.Name,
		"namespace", appRef.Namespace,
		"podCount", len(podList.Items),
	)

	return podList.Items, nil
}

// resolveLabelSelector fetches the referenced workload object and extracts its
// pod selector MatchLabels. Returns nil if the kind is not supported.
//
// Supported kinds: Deployment, StatefulSet, ReplicaSet, Job
func (r *OrchestrationProfileReconciler) resolveLabelSelector(
	ctx context.Context,
	appRef orchestrationv1alpha1.ApplicationReference,
) (map[string]string, error) {
	key := types.NamespacedName{
		Name:      appRef.Name,
		Namespace: appRef.Namespace,
	}

	switch appRef.Kind {
	case "Deployment":
		obj := &appsv1.Deployment{}
		if err := r.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj.Spec.Selector.MatchLabels, nil

	case "StatefulSet":
		obj := &appsv1.StatefulSet{}
		if err := r.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj.Spec.Selector.MatchLabels, nil

	case "Job":
		obj := &batchv1.Job{}
		if err := r.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj.Spec.Selector.MatchLabels, nil

	case "ReplicaSet":
		obj := &appsv1.ReplicaSet{}
		if err := r.Get(ctx, key, obj); err != nil {
			return nil, err
		}
		return obj.Spec.Selector.MatchLabels, nil

	default:
		return nil, nil
	}
}
