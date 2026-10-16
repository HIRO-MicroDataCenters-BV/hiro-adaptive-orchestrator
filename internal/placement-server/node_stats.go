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

package placementserver

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// PodNodeNameIndex indexes Pods by spec.nodeName so per-node usage is a cache
// read of only that node's pods.
const PodNodeNameIndex = "spec.nodeName"

// Defaults for NodeStat's zone label and GPU resource; override with
// NODE_STATS_ZONE_LABEL / NODE_STATS_GPU_RESOURCE.
const (
	DefaultZoneLabel   = "topology.kubernetes.io/zone"
	DefaultGPUResource = "nvidia.com/gpu"
)

// BuilderOption configures a DecisionContextBuilder.
type BuilderOption func(*DecisionContextBuilder)

// WithNodeStatsLabels sets the zone label and GPU resource NodeStats reads; empty keeps the default.
func WithNodeStatsLabels(zoneLabel, gpuResource string) BuilderOption {
	return func(b *DecisionContextBuilder) {
		if zoneLabel != "" {
			b.zoneLabel = zoneLabel
		}
		if gpuResource != "" {
			b.gpuResource = corev1.ResourceName(gpuResource)
		}
	}
}

// RegisterPodIndexes registers PodNodeNameIndex. Must run before mgr.Start.
func RegisterPodIndexes(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &corev1.Pod{}, PodNodeNameIndex, PodNodeNameIndexFunc,
	); err != nil {
		return fmt.Errorf("registering index %q: %w", PodNodeNameIndex, err)
	}
	return nil
}

// PodNodeNameIndexFunc is the indexer for PodNodeNameIndex; unscheduled pods
// are not indexed.
func PodNodeNameIndexFunc(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}
	return []string{pod.Spec.NodeName}
}

// buildNodeStats snapshots allocatable, requested and this app's pod count
// for each node. Energy stays nil until a per-node energy source exists.
func (b *DecisionContextBuilder) buildNodeStats(
	ctx context.Context,
	nodes []*corev1.Node,
	appRef orchestrationv1alpha1.ApplicationReference,
) ([]NodeStat, error) {
	appPods, err := utils.FindPodsForApplication(ctx, b.client, appRef)
	if err != nil {
		return nil, fmt.Errorf("listing application pods: %w", err)
	}
	appUIDs := make(map[types.UID]bool, len(appPods))
	for i := range appPods {
		appUIDs[appPods[i].UID] = true
	}

	now := metav1.Now()
	stats := make([]NodeStat, 0, len(nodes))
	for _, node := range nodes {
		podList := &corev1.PodList{}
		if err := b.client.List(ctx, podList, client.MatchingFields{PodNodeNameIndex: node.Name}); err != nil {
			return nil, fmt.Errorf("listing pods on node %s: %w", node.Name, err)
		}

		stat := NodeStat{
			NodeName:    node.Name,
			Zone:        node.Labels[b.zoneLabel],
			Allocatable: b.amountsOf(node.Status.Allocatable),
			ObservedAt:  now,
		}
		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			req := b.amountsOf(podRequests(pod))
			stat.Requested.CPUMilli += req.CPUMilli
			stat.Requested.MemoryBytes += req.MemoryBytes
			stat.Requested.GPU += req.GPU
			if appUIDs[pod.UID] {
				stat.AppPods++
			}
		}
		stats = append(stats, stat)
	}
	return stats, nil
}

// podRequests is the pod's effective request, as the scheduler counts it:
// max(sum of containers, largest init container) plus pod overhead.
func podRequests(pod *corev1.Pod) corev1.ResourceList {
	total := corev1.ResourceList{}
	for _, c := range pod.Spec.Containers {
		addResources(total, c.Resources.Requests)
	}
	for _, c := range pod.Spec.InitContainers {
		for name, q := range c.Resources.Requests {
			if cur, ok := total[name]; !ok || q.Cmp(cur) > 0 {
				total[name] = q.DeepCopy()
			}
		}
	}
	addResources(total, pod.Spec.Overhead)
	return total
}

func addResources(total, add corev1.ResourceList) {
	for name, q := range add {
		cur := total[name]
		cur.Add(q)
		total[name] = cur
	}
}

func (b *DecisionContextBuilder) amountsOf(list corev1.ResourceList) ResourceAmounts {
	gpu := list[b.gpuResource]
	return ResourceAmounts{
		CPUMilli:    list.Cpu().MilliValue(),
		MemoryBytes: list.Memory().Value(),
		GPU:         gpu.Value(),
	}
}
