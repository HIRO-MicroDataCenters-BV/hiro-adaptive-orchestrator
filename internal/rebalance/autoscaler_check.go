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
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list

// vpaListGVK is the standard VerticalPodAutoscaler API; its CRD is optional.
var vpaListGVK = schema.GroupVersionKind{Group: "autoscaling.k8s.io", Version: "v1", Kind: "VerticalPodAutoscalerList"}

// vpaUpdateModeOff only recommends, so it can't fight the operator.
const vpaUpdateModeOff = "Off"

// reportUndeclaredAutoscalers updates the status condition; a failure is only logged, as it never affects a decision.
func (r *Reconciler) reportUndeclaredAutoscalers(
	ctx context.Context,
	key types.NamespacedName,
	profile *orchestrationv1alpha1.OrchestrationProfile,
) {
	message := undeclaredAutoscalers(ctx, r.Client, profile)
	existing := apimeta.FindStatusCondition(profile.Status.Conditions, orchestrationv1alpha1.ConditionUndeclaredAutoscaler)
	if (existing == nil && message == "") || (existing != nil && existing.Message == message) {
		return
	}
	if err := r.Writer.SetUndeclaredAutoscaler(ctx, key, message); err != nil {
		logf.FromContext(ctx).Error(err, "rebalance: could not record the autoscaler check", "profile", profile.Name)
	}
}

// undeclaredAutoscalers describes each autoscaler acting on a dimension the profile doesn't mark externally managed.
func undeclaredAutoscalers(
	ctx context.Context,
	c client.Client,
	profile *orchestrationv1alpha1.OrchestrationProfile,
) string {
	var found []string
	if !profile.Spec.Rebalancing.ReplicasExternallyManaged {
		if hpa := findHPA(ctx, c, profile.Spec.ApplicationRef); hpa != nil {
			found = append(found, fmt.Sprintf(
				"HorizontalPodAutoscaler %s targets the app but replicasExternallyManaged is false", hpa.Name))
		}
	}
	if !profile.Spec.Rebalancing.ResourcesExternallyManaged {
		if vpa := findActiveVPA(ctx, c, profile.Spec.ApplicationRef); vpa != "" {
			found = append(found, fmt.Sprintf(
				"VerticalPodAutoscaler %s targets the app but resourcesExternallyManaged is false", vpa))
		}
	}
	return strings.Join(found, "; ")
}

// findActiveVPA names the VPA changing the workload's resources, or "" if there's none or its CRD isn't installed.
func findActiveVPA(ctx context.Context, c client.Client, ref orchestrationv1alpha1.ApplicationReference) string {
	vpaList := &unstructured.UnstructuredList{}
	vpaList.SetGroupVersionKind(vpaListGVK)
	if err := c.List(ctx, vpaList, client.InNamespace(ref.Namespace)); err != nil {
		return ""
	}
	for i := range vpaList.Items {
		vpa := vpaList.Items[i].Object
		kind, _, _ := unstructured.NestedString(vpa, "spec", "targetRef", "kind")
		name, _, _ := unstructured.NestedString(vpa, "spec", "targetRef", "name")
		mode, _, _ := unstructured.NestedString(vpa, "spec", "updatePolicy", "updateMode")
		if kind == string(ref.Kind) && name == ref.Name && mode != vpaUpdateModeOff {
			return vpaList.Items[i].GetName()
		}
	}
	return ""
}
