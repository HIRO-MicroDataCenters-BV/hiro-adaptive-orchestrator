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

package v1

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
)

// nolint:unused
var podlog = logf.Log.WithName("pod-resource")

// DefaultSchedulerName is the fallback when HIRO_SCHEDULER_NAME is not set.
const DefaultSchedulerName = "hiro-scheduler"

// SetupPodWebhookWithManager registers the MutatingAdmissionWebhook for Pod.
//
// The webhook sets spec.schedulerName on pods whose application is governed
// by an OrchestrationProfile, so users do not have to set it manually.
//
// schedulerName is read from env HIRO_SCHEDULER_NAME (default: "hiro-scheduler").
//
// Requires ENABLE_WEBHOOKS != "false" (checked in main.go).
// Requires TLS certs mounted at /tmp/k8s-webhook-server/serving-certs/
// (provisioned by hack/deploy_webhook.sh).
func SetupPodWebhookWithManager(
	mgr ctrl.Manager,
	builder *placementserver.DecisionContextBuilder,
	schedulerName string,
) error {
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(&PodCustomDefaulter{
			builder:       builder,
			schedulerName: schedulerName,
		}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=mpod-v1.kb.io,admissionReviewVersions=v1

// PodCustomDefaulter sets spec.schedulerName on pods whose application is
// governed by an OrchestrationProfile.
//
// Soft-fail: any lookup error results in the pod being allowed unchanged —
// pod creation is never blocked by this webhook.
type PodCustomDefaulter struct {
	builder       *placementserver.DecisionContextBuilder
	schedulerName string
}

// Default implements webhook.CustomDefaulter.
func (d *PodCustomDefaulter) Default(ctx context.Context, obj *corev1.Pod) error {
	if d.builder == nil {
		return nil
	}

	logger := logf.FromContext(ctx)

	if obj.Spec.SchedulerName == d.schedulerName {
		return nil // already targeting the right scheduler
	}

	profile, err := d.builder.FindProfileForPod(ctx, obj)
	if err != nil {
		// Soft-fail: never block pod creation due to infrastructure errors.
		logger.Error(err, "webhook: profile lookup failed, allowing pod unchanged",
			"pod", obj.Name, "namespace", obj.Namespace)
		return nil
	}

	if profile == nil {
		return nil // no OrchestrationProfile governs this pod
	}

	obj.Spec.SchedulerName = d.schedulerName
	logger.Info("webhook: set schedulerName",
		"pod", obj.Name,
		"namespace", obj.Namespace,
		"profile", profile.Name,
		"schedulerName", d.schedulerName,
	)
	return nil
}
