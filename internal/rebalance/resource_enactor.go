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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// Never Ever delete this comments as they are used by kubebuilder to generate RBAC permissions for the controller.
// If you need to change the permissions,
// modify the verbs and resources in the comments below and then run "make generate" to update the generated code.

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=pods/resize,verbs=update

// DefaultResourceActionTimeout bounds how long resourceEnactor spends
// attempting in-place resize across a workload's running pods before giving
// up on the fast path and falling back to "applied to the template, takes
// effect on the next rollout" — the same outcome as if in-place resize
// weren't supported at all.
const DefaultResourceActionTimeout = 60 * time.Second

// DefaultMinCPU/MaxCPU/MinMemory/MaxMemory are dispatchResource's fallback
// guardrail bounds — conservative defaults meant to stop a misbehaving AI
// response from asking for something absurd, not to express real capacity
// planning for any given workload. Unlike replica bounds (Story 35), no
// existing cluster object (an HPA, say) is consulted for these — a
// per-workload autoscaler for CPU/memory (VPA) is a separate CRD this module
// doesn't already depend on, so these are env-var/default bounds only.
var (
	DefaultMinCPU    = resource.MustParse("50m")
	DefaultMaxCPU    = resource.MustParse("2")
	DefaultMinMemory = resource.MustParse("64Mi")
	DefaultMaxMemory = resource.MustParse("2Gi")
)

// resourceState is what resolveResourceState reports about a workload's
// existing container resource configuration — dispatchResource needs this
// before deciding whether AdjustResources applies at all.
type resourceState struct {
	// Found is false when containerName doesn't match any container in the
	// workload's pod template.
	Found bool

	// HasExistingLimits is true when the named container already declares at
	// least one of CPU/Memory, in requests or limits.
	HasExistingLimits bool
}

// resolveResourceState fetches profile's workload and reports whether the
// named container already declares any CPU/Memory requests or limits.
//
// A container with neither is deliberately left unconstrained by whoever
// wrote its spec — it already scales elastically within whatever the node
// has free, with no ceiling to adjust. Imposing brand-new requests/limits on
// such a workload wouldn't be an adjustment, it would be a first-time
// resource constraint — and would silently change its QoS class from
// BestEffort to Guaranteed, a bigger and different decision than this action
// is meant to make. dispatchResource defers instead of guessing (see
// OutcomeDeferred usage there).
func resolveResourceState(
	ctx context.Context,
	c client.Client,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	containerName string,
) (resourceState, error) {
	containers, err := fetchPodTemplateContainers(ctx, c, profile)
	if err != nil {
		return resourceState{}, err
	}

	container := findContainerByName(containers, containerName)
	if container == nil {
		return resourceState{}, nil
	}
	res := container.Resources
	return resourceState{
		Found:             true,
		HasExistingLimits: hasCPUOrMemory(res.Requests) || hasCPUOrMemory(res.Limits),
	}, nil
}

// fetchPodTemplateContainers returns profile's workload's pod template
// containers, regardless of Kind (Deployment/StatefulSet share the same
// PodTemplateSpec shape).
func fetchPodTemplateContainers(
	ctx context.Context,
	c client.Client,
	profile *orchestrationv1alpha1.OrchestrationProfile,
) ([]corev1.Container, error) {
	ref := profile.Spec.ApplicationRef
	key := types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}

	switch ref.Kind {
	case kindDeployment:
		obj := &appsv1.Deployment{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil, fmt.Errorf("fetching Deployment %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		return obj.Spec.Template.Spec.Containers, nil
	case kindStatefulSet:
		obj := &appsv1.StatefulSet{}
		if err := c.Get(ctx, key, obj); err != nil {
			return nil, fmt.Errorf("fetching StatefulSet %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		return obj.Spec.Template.Spec.Containers, nil
	default:
		return nil, fmt.Errorf("AdjustResources does not support workload kind %q", ref.Kind)
	}
}

// findContainerByName returns a pointer into containers for the entry named
// name, or nil if none matches.
func findContainerByName(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func hasCPUOrMemory(list corev1.ResourceList) bool {
	if list == nil {
		return false
	}
	_, hasCPU := list[corev1.ResourceCPU]
	_, hasMemory := list[corev1.ResourceMemory]
	return hasCPU || hasMemory
}

// resourceBounds is the [Min, Max] range dispatchResource's guardrail
// enforces for CPU and Memory independently.
type resourceBounds struct {
	MinCPU, MaxCPU       resource.Quantity
	MinMemory, MaxMemory resource.Quantity
}

// resourceBounds resolves dispatchResource's guardrail bounds from the
// Reconciler's configured fields, falling back to the package defaults for
// any left unset (zero value).
func (r *Reconciler) resourceBounds() resourceBounds {
	b := resourceBounds{MinCPU: r.MinCPU, MaxCPU: r.MaxCPU, MinMemory: r.MinMemory, MaxMemory: r.MaxMemory}
	if b.MinCPU.IsZero() {
		b.MinCPU = DefaultMinCPU
	}
	if b.MaxCPU.IsZero() {
		b.MaxCPU = DefaultMaxCPU
	}
	if b.MinMemory.IsZero() {
		b.MinMemory = DefaultMinMemory
	}
	if b.MaxMemory.IsZero() {
		b.MaxMemory = DefaultMaxMemory
	}
	return b
}

// check reports whether targetCPU/targetMemory (either may be the zero
// Quantity, meaning "unspecified — leave unchanged", per parseTargetResources)
// fall within b. A non-ok result carries a human-readable reason naming which
// bound was violated.
func (b resourceBounds) check(targetCPU, targetMemory resource.Quantity) (reason string, ok bool) {
	if !targetCPU.IsZero() && (targetCPU.Cmp(b.MinCPU) < 0 || targetCPU.Cmp(b.MaxCPU) > 0) {
		return fmt.Sprintf("targetCpu %s outside [%s, %s]", targetCPU.String(), b.MinCPU.String(), b.MaxCPU.String()), false
	}
	if !targetMemory.IsZero() && (targetMemory.Cmp(b.MinMemory) < 0 || targetMemory.Cmp(b.MaxMemory) > 0) {
		return fmt.Sprintf("targetMemory %s outside [%s, %s]", targetMemory.String(), b.MinMemory.String(), b.MaxMemory.String()), false
	}
	return "", true
}

// parseTargetResources parses cpu/memory's non-empty resource.Quantity
// strings. Whichever input is empty comes back as the zero Quantity
// (IsZero() true) — dispatchResource/resourceEnactor's signal to leave that
// resource unchanged.
func parseTargetResources(cpu, memory string) (targetCPU, targetMemory resource.Quantity, err error) {
	if cpu != "" {
		if targetCPU, err = resource.ParseQuantity(cpu); err != nil {
			return resource.Quantity{}, resource.Quantity{}, fmt.Errorf("targetCpu %q: %w", cpu, err)
		}
	}
	if memory != "" {
		if targetMemory, err = resource.ParseQuantity(memory); err != nil {
			return resource.Quantity{}, resource.Quantity{}, fmt.Errorf("targetMemory %q: %w", memory, err)
		}
	}
	return targetCPU, targetMemory, nil
}

// resourceEnactor patches profile's workload (Deployment or StatefulSet) so
// its named container's CPU and/or Memory requests+limits match
// targetCPU/targetMemory (either may be zero to leave that resource
// unchanged), then best-effort resizes any already-running pods in place.
// containerName is matched by name, not position — the AI names the exact
// container in its response (see RebalanceDecisionResponse.ContainerName),
// so this works for a workload with any number of containers.
//
// Returns a scaleResult (shared with scaleEnactor — same shape, same
// caller contract: a non-nil error means nothing was attempted, no
// side effect). Unlike scaleEnactor, an in-place-resize failure is not a
// Failed outcome by itself: the template patch has already durably applied,
// so the workload gets the target resources on its next rollout either way.
// This enactor does not poll for the resize to actually converge on running
// pods — the in-place request being accepted is treated as sufficient
// confirmation for this story's scope; verifying convergence is a possible
// future refinement, not built here.
func resourceEnactor(
	ctx context.Context,
	c client.Client,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	containerName string,
	targetCPU, targetMemory resource.Quantity,
	reason string,
	timeout time.Duration,
) (scaleResult, error) {
	if timeout <= 0 {
		timeout = DefaultResourceActionTimeout
	}

	ref := profile.Spec.ApplicationRef
	key := types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}

	var selector *metav1.LabelSelector

	switch ref.Kind {
	case kindDeployment:
		obj := &appsv1.Deployment{}
		if err := c.Get(ctx, key, obj); err != nil {
			return scaleResult{}, fmt.Errorf("fetching Deployment %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		container := findContainerByName(obj.Spec.Template.Spec.Containers, containerName)
		if container == nil {
			return scaleResult{}, fmt.Errorf("container %q not found in Deployment %s/%s", containerName, ref.Namespace, ref.Name)
		}
		applyTargetResources(container, targetCPU, targetMemory)
		if err := c.Update(ctx, obj); err != nil {
			return scaleResult{}, fmt.Errorf("patching Deployment %s/%s resources: %w", ref.Namespace, ref.Name, err)
		}
		selector = obj.Spec.Selector

	case kindStatefulSet:
		obj := &appsv1.StatefulSet{}
		if err := c.Get(ctx, key, obj); err != nil {
			return scaleResult{}, fmt.Errorf("fetching StatefulSet %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		container := findContainerByName(obj.Spec.Template.Spec.Containers, containerName)
		if container == nil {
			return scaleResult{}, fmt.Errorf("container %q not found in StatefulSet %s/%s", containerName, ref.Namespace, ref.Name)
		}
		applyTargetResources(container, targetCPU, targetMemory)
		if err := c.Update(ctx, obj); err != nil {
			return scaleResult{}, fmt.Errorf("patching StatefulSet %s/%s resources: %w", ref.Namespace, ref.Name, err)
		}
		selector = obj.Spec.Selector

	default:
		return scaleResult{}, fmt.Errorf("AdjustResources does not support workload kind %q", ref.Kind)
	}

	resized, attempted, err := resizeRunningPods(ctx, c, ref.Namespace, selector, containerName, targetCPU, targetMemory, timeout)
	if err != nil || attempted == 0 {
		// No running pods matched, or listing them failed — the template
		// patch still applies on the next rollout, so this is not a failure.
		reason := fmt.Sprintf("template patched: %s (no running pods resized in place)", reason)
		return scaleResult{outcome: OutcomeEnacted, reason: reason}, nil
	}
	if resized < attempted {
		reason := fmt.Sprintf("template patched: %s (in-place resize unavailable on %d/%d running pods; effective on next rollout)",
			reason, attempted-resized, attempted)
		return scaleResult{outcome: OutcomeEnacted, reason: reason}, nil
	}
	return scaleResult{
		outcome: OutcomeEnacted,
		reason:  fmt.Sprintf("resized %d running pod(s) in place: %s", resized, reason),
	}, nil
}

// applyTargetResources sets container's CPU and/or Memory requests+limits to
// whichever of targetCPU/targetMemory is non-zero, leaving the other
// resource's existing values untouched.
func applyTargetResources(container *corev1.Container, targetCPU, targetMemory resource.Quantity) {
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	if !targetCPU.IsZero() {
		container.Resources.Requests[corev1.ResourceCPU] = targetCPU
		container.Resources.Limits[corev1.ResourceCPU] = targetCPU
	}
	if !targetMemory.IsZero() {
		container.Resources.Requests[corev1.ResourceMemory] = targetMemory
		container.Resources.Limits[corev1.ResourceMemory] = targetMemory
	}
}

// resizeRunningPods lists the pods currently matching selector in namespace
// and attempts an in-place resize (the "resize" subresource) on each,
// applying the same target resources the template was just patched with.
// Returns how many succeeded out of how many were attempted. A pod-level
// resize error (most commonly: the cluster doesn't support in-place resize
// at all) is not returned as an error here — it's reflected in the
// resized-vs-attempted counts for the caller to turn into a reason string.
func resizeRunningPods(
	ctx context.Context,
	c client.Client,
	namespace string,
	selector *metav1.LabelSelector,
	containerName string,
	targetCPU, targetMemory resource.Quantity,
	timeout time.Duration,
) (resized, attempted int, err error) {
	if selector == nil {
		return 0, 0, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return 0, 0, fmt.Errorf("converting selector: %w", err)
	}

	podList := &corev1.PodList{}
	if err := c.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return 0, 0, fmt.Errorf("listing pods: %w", err)
	}

	resizeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for i := range podList.Items {
		pod := &podList.Items[i]
		container := findContainerByName(pod.Spec.Containers, containerName)
		if container == nil {
			continue // shouldn't happen for a pod from this workload's own template, but don't guess
		}
		attempted++
		applyTargetResources(container, targetCPU, targetMemory)
		if err := c.SubResource("resize").Update(resizeCtx, pod); err == nil {
			resized++
		}
		if resizeCtx.Err() != nil {
			break // timed out partway through — remaining pods fall back to the template's next-rollout path
		}
	}
	return resized, attempted, nil
}
