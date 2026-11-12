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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// Never Ever delete this comments as they are used by kubebuilder to generate RBAC permissions for the controller.
// If you need to change the permissions,
// modify the verbs and resources in the comments below and then run "make generate" to update the generated code.

// +kubebuilder:rbac:groups="",resources=pods,verbs=delete

// retryPendingSchedule deletes the pods the gate held, so their replacements schedule now instead of after kube-scheduler's backoff.
// A plain Delete is safe: these pods were never Ready.
func retryPendingSchedule(
	ctx context.Context,
	c client.Client,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	minPendingPodAge time.Duration,
) error {
	pods, err := utils.FindPodsForApplication(ctx, c, profile.Spec.ApplicationRef)
	if err != nil {
		return fmt.Errorf("finding pods to retry: %w", err)
	}

	deleted := 0
	for i := range pods {
		pod := &pods[i]
		if !heldByEnergyGate(pod, minPendingPodAge) {
			continue
		}
		if err := c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pending pod %s: %w", pod.Name, err)
		}
		deleted++
	}

	if deleted == 0 {
		return fmt.Errorf("no pod held by the energy gate found to retry for %s/%s",
			profile.Spec.ApplicationRef.Namespace, profile.Spec.ApplicationRef.Name)
	}
	return nil
}
