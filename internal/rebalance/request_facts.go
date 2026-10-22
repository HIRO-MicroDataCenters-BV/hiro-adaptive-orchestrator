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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// Never Ever delete this comments as they are used by kubebuilder to generate RBAC permissions for the controller.
// If you need to change the permissions,
// modify the verbs and resources in the comments below and then run "make generate" to update the generated code.

// +kubebuilder:rbac:groups="policy",resources=poddisruptionbudgets,verbs=get;list;watch

// rebalanceFacts resolves the trigger, workload, PDB and history sent with a rebalance
// request; a failed lookup leaves that fact nil.
func (r *Reconciler) rebalanceFacts(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	rs orchestrationv1alpha1.RebalancingStatus,
	result TriggerResult,
) placementserver.RebalanceFacts {
	logger := logf.FromContext(ctx)

	facts := placementserver.RebalanceFacts{
		Trigger: &placement.RebalanceTrigger{
			Type:      string(result.Condition),
			Node:      result.Detail.Node,
			Value:     result.Detail.Value,
			Threshold: result.Detail.Threshold,
		},
		Workload: r.workloadFacts(ctx, profile),
		State: rebalanceState(rs, r.Writer.HistoryConfig(),
			profile.Spec.Rebalancing.EscalationThreshold, time.Now()),
	}

	pods, err := utils.FindPodsForApplication(ctx, r.Client, profile.Spec.ApplicationRef)
	if err != nil {
		logger.V(1).Info("rebalance: could not list pods for PDB lookup, skipping", "profile", profile.Name, "err", err)
		return facts
	}
	facts.PDB, err = findPDB(ctx, r.Client, profile.Spec.ApplicationRef.Namespace, pods)
	if err != nil {
		logger.V(1).Info("rebalance: PDB lookup failed, skipping", "profile", profile.Name, "err", err)
	}
	return facts
}

// workloadFacts reports the workload's replicas and AdjustReplicas bounds.
func (r *Reconciler) workloadFacts(
	ctx context.Context,
	profile *orchestrationv1alpha1.OrchestrationProfile,
) *placement.RebalanceWorkload {
	ref := profile.Spec.ApplicationRef
	bounds := resolveReplicaBounds(ctx, r.Client, profile, r.MinReplicas, r.MaxReplicas)

	return &placement.RebalanceWorkload{
		Kind:        string(ref.Kind),
		Name:        ref.Name,
		Namespace:   ref.Namespace,
		Replicas:    workloadReplicas(ctx, r.Client, ref),
		MinReplicas: bounds.Min,
		MaxReplicas: bounds.Max,
	}
}

// workloadReplicas returns spec.replicas for scalable kinds; 0 otherwise or when unreadable.
func workloadReplicas(ctx context.Context, c client.Client, ref orchestrationv1alpha1.ApplicationReference) int32 {
	key := types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}
	var replicas *int32
	switch ref.Kind {
	case orchestrationv1alpha1.ApplicationKindDeployment:
		obj := &appsv1.Deployment{}
		if c.Get(ctx, key, obj) == nil {
			replicas = obj.Spec.Replicas
		}
	case orchestrationv1alpha1.ApplicationKindStatefulSet:
		obj := &appsv1.StatefulSet{}
		if c.Get(ctx, key, obj) == nil {
			replicas = obj.Spec.Replicas
		}
	case orchestrationv1alpha1.ApplicationKindReplicaSet:
		obj := &appsv1.ReplicaSet{}
		if c.Get(ctx, key, obj) == nil {
			replicas = obj.Spec.Replicas
		}
	}
	if replicas == nil {
		return 0
	}
	return *replicas
}

// findPDB returns the first PodDisruptionBudget in namespace matching any of pods, or nil.
func findPDB(ctx context.Context, c client.Client, namespace string, pods []corev1.Pod) (*placement.RebalancePDB, error) {
	if len(pods) == 0 {
		return nil, nil
	}
	list := &policyv1.PodDisruptionBudgetList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		pdb := &list.Items[i]
		// policy/v1: an empty selector matches every pod; nil matches none.
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			continue
		}
		for j := range pods {
			if selector.Matches(labels.Set(pods[j].Labels)) {
				return &placement.RebalancePDB{Name: pdb.Name, AllowedDisruptions: pdb.Status.DisruptionsAllowed}, nil
			}
		}
	}
	return nil, nil
}
