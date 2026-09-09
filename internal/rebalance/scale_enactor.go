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

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// Never Ever delete this comments as they are used by kubebuilder to generate RBAC permissions for the controller.
// If you need to change the permissions,
// modify the verbs and resources in the comments below and then run "make generate" to update the generated code.

// +kubebuilder:rbac:groups="apps",resources=deployments;statefulsets,verbs=update
// +kubebuilder:rbac:groups="autoscaling",resources=horizontalpodautoscalers,verbs=get;list;watch

// DefaultScaleActionTimeout bounds how long scaleEnactor waits, after
// patching Spec.Replicas, for the workload to report that many ReadyReplicas.
const DefaultScaleActionTimeout = 60 * time.Second

// DefaultMinReplicas and DefaultMaxReplicas are the fallback guardrail bounds
// dispatchScale applies when the workload has no HorizontalPodAutoscaler of
// its own (see resolveReplicaBounds) — conservative defaults meant to stop a
// misbehaving AI response from scaling a workload to zero or to something
// absurd, not to express real capacity planning for any given workload.
const DefaultMinReplicas int32 = 1
const DefaultMaxReplicas int32 = 10

// scaleActionPollInterval mirrors moveActionPollInterval — how often
// awaitReadyReplicas re-reads the workload while waiting.
const scaleActionPollInterval = 2 * time.Second

// scaleResult is what scaleEnactor decides happened, for the caller
// (dispatchScale) to translate into a terminal StateWriter transition.
type scaleResult struct {
	outcome orchestrationv1alpha1.RebalanceOutcome
	reason  string
}

// replicaBounds is resolveReplicaBounds' result: the [Min, Max] range
// dispatchScale's guardrail enforces, plus whether the workload is already
// managed by KEDA. dispatchScale must defer entirely — bounds included, no
// patch attempted — whenever KEDAManaged is true, since KEDA already owns
// the target-replica decision for that workload.
type replicaBounds struct {
	Min, Max int32
	Source   string

	// KEDAManaged is true when the HPA behind Min/Max was created by a KEDA
	// ScaledObject (identified by its ownerReferences), not by a user
	// directly. See resolveReplicaBounds' doc comment for why this is
	// detectable from the HPA alone.
	KEDAManaged bool

	// KEDAOwner is the owning ScaledObject's name, for a readable Deferred
	// reason. Empty unless KEDAManaged.
	KEDAOwner string
}

// resolveReplicaBounds returns the [Min, Max] replica range dispatchScale's
// guardrail enforces for profile's workload. A HorizontalPodAutoscaler
// targeting the same workload (matched by kind + name, same namespace as the
// HPA itself — scaleTargetRef has no namespace field) is authoritative: it's
// the standard place a user already expresses "how far this workload may
// scale," so honour it instead of a second, competing bound. No matching HPA
// falls back to fallbackMin/fallbackMax (<= 0 uses the package defaults).
//
// KEDA doesn't scale a workload's replicas directly — it creates and drives
// a real HorizontalPodAutoscaler of its own, fed by its custom metrics
// adapter instead of CPU/memory. That generated HPA's ownerReferences point
// back to the ScaledObject that created it, so a found HPA is checked for
// KEDA ownership here — no new RBAC or CRD scheme registration needed, since
// it only inspects an HPA object already fetched for bounds.
func resolveReplicaBounds(
	ctx context.Context,
	c client.Client,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	fallbackMin, fallbackMax int32,
) replicaBounds {
	if fallbackMin <= 0 {
		fallbackMin = DefaultMinReplicas
	}
	if fallbackMax <= 0 {
		fallbackMax = DefaultMaxReplicas
	}

	ref := profile.Spec.ApplicationRef
	hpaList := &autoscalingv2.HorizontalPodAutoscalerList{}
	if err := c.List(ctx, hpaList, client.InNamespace(ref.Namespace)); err == nil {
		for i := range hpaList.Items {
			hpa := &hpaList.Items[i]
			target := hpa.Spec.ScaleTargetRef
			if target.Kind != ref.Kind || target.Name != ref.Name {
				continue
			}
			lo := int32(1)
			if hpa.Spec.MinReplicas != nil {
				lo = *hpa.Spec.MinReplicas
			}
			bounds := replicaBounds{
				Min: lo, Max: hpa.Spec.MaxReplicas,
				Source: fmt.Sprintf("HorizontalPodAutoscaler %s", hpa.Name),
			}
			if owner := kedaScaledObjectOwner(hpa.OwnerReferences); owner != "" {
				bounds.KEDAManaged = true
				bounds.KEDAOwner = owner
			}
			return bounds
		}
	}
	return replicaBounds{Min: fallbackMin, Max: fallbackMax, Source: "default bounds"}
}

// kedaScaledObjectOwner returns the name of the owning KEDA ScaledObject, if
// owners contains one — see resolveReplicaBounds' doc comment.
func kedaScaledObjectOwner(owners []metav1.OwnerReference) string {
	for _, owner := range owners {
		if owner.Kind == "ScaledObject" && owner.APIVersion == "keda.sh/v1alpha1" {
			return owner.Name
		}
	}
	return ""
}

// scaleEnactor patches profile's workload (Deployment or StatefulSet) to
// targetReplicas and waits for that many ReadyReplicas to be observed.
//
// Returns a scaleResult describing the terminal outcome to record — never an
// error for anything that happened *after* the patch succeeded, mirroring
// moveEnactor: a timeout waiting for ready replicas is a defined outcome
// (Failed) the caller writes to status, not a Go error. A non-nil error means
// the workload couldn't even be fetched, or its kind isn't one this enactor
// knows how to scale — the patch was never attempted.
func scaleEnactor(
	ctx context.Context,
	c client.Client,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	targetReplicas int32,
	reason string,
	timeout time.Duration,
) (scaleResult, error) {
	if timeout <= 0 {
		timeout = DefaultScaleActionTimeout
	}

	ref := profile.Spec.ApplicationRef
	key := types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}

	switch ref.Kind {
	case "Deployment":
		obj := &appsv1.Deployment{}
		if err := c.Get(ctx, key, obj); err != nil {
			return scaleResult{}, fmt.Errorf("fetching Deployment %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		obj.Spec.Replicas = &targetReplicas
		if err := c.Update(ctx, obj); err != nil {
			return scaleResult{}, fmt.Errorf("patching Deployment %s/%s to %d replicas: %w",
				ref.Namespace, ref.Name, targetReplicas, err)
		}

	case "StatefulSet":
		obj := &appsv1.StatefulSet{}
		if err := c.Get(ctx, key, obj); err != nil {
			return scaleResult{}, fmt.Errorf("fetching StatefulSet %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		obj.Spec.Replicas = &targetReplicas
		if err := c.Update(ctx, obj); err != nil {
			return scaleResult{}, fmt.Errorf("patching StatefulSet %s/%s to %d replicas: %w",
				ref.Namespace, ref.Name, targetReplicas, err)
		}

	default:
		return scaleResult{}, fmt.Errorf("AdjustReplicas does not support workload kind %q", ref.Kind)
	}

	if err := awaitReadyReplicas(ctx, c, ref.Kind, key, targetReplicas, timeout); err != nil {
		return scaleResult{outcome: OutcomeFailed, reason: err.Error()}, nil
	}
	return scaleResult{
		outcome: OutcomeEnacted,
		reason:  fmt.Sprintf("scaled to %d replicas: %s", targetReplicas, reason),
	}, nil
}

// awaitReadyReplicas polls the workload until its Status.ReadyReplicas
// matches target, up to timeout.
func awaitReadyReplicas(
	ctx context.Context,
	c client.Client,
	kind string,
	key types.NamespacedName,
	target int32,
	timeout time.Duration,
) error {
	var lastReady int32 = -1
	pollErr := wait.PollUntilContextTimeout(ctx, scaleActionPollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			switch kind {
			case "Deployment":
				obj := &appsv1.Deployment{}
				if err := c.Get(ctx, key, obj); err != nil {
					return false, nil // transient list error — keep polling until timeout
				}
				lastReady = obj.Status.ReadyReplicas
			case "StatefulSet":
				obj := &appsv1.StatefulSet{}
				if err := c.Get(ctx, key, obj); err != nil {
					return false, nil
				}
				lastReady = obj.Status.ReadyReplicas
			}
			return lastReady == target, nil
		})
	if pollErr != nil {
		return fmt.Errorf("timed out waiting for %d ready replicas (last observed %d) within %s",
			target, lastReady, timeout)
	}
	return nil
}
