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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// PodNodeNameIndex indexes Pods by spec.nodeName so per-node usage is a cache
// read of only that node's pods.
const PodNodeNameIndex = "spec.nodeName"

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

// buildNodeStats builds one NodeStat per node. It never drops a node: usage it
// can't read is left null. pod is the pod being placed, nil for rebalance.
func (b *DecisionContextBuilder) buildNodeStats(
	ctx context.Context,
	nodes []*corev1.Node,
	appRef orchestrationv1alpha1.ApplicationReference,
	pod *corev1.Pod,
	candidate bool,
) []NodeStat {
	logger := logf.FromContext(ctx)

	appUIDs, err := b.appPodUIDs(ctx, appRef)
	if err != nil {
		logger.Info("builder: app pods unavailable, appPods left null", "err", err)
	}
	podImages := imagesOf(pod)

	now := metav1.Now()
	stats := make([]NodeStat, 0, len(nodes))
	for _, node := range nodes {
		stat := describeNode(node, now)
		stat.Candidate = candidate
		stat.ImagesPresent = imagesPresent(node, podImages)

		podList := &corev1.PodList{}
		if err := b.client.List(ctx, podList, client.MatchingFields{PodNodeNameIndex: node.Name}); err != nil {
			logger.Info("builder: pods on node unavailable, usage left null", "node", node.Name, "err", err)
		} else {
			addUsage(&stat, podList.Items, appUIDs)
		}
		stats = append(stats, stat)
	}
	return stats
}

// nonCandidateNodeStats lists, with candidate=false, the nodes hosting the app's pods that
// aren't candidates (NotReady, untolerated taint); a node that no longer exists is sent by name only.
func (b *DecisionContextBuilder) nonCandidateNodeStats(
	ctx context.Context,
	pods []corev1.Pod,
	candidates []*corev1.Node,
	appRef orchestrationv1alpha1.ApplicationReference,
) []NodeStat {
	logger := logf.FromContext(ctx)
	seen := make(map[string]bool, len(candidates))
	for _, n := range candidates {
		seen[n.Name] = true
	}

	var nodes []*corev1.Node
	var gone []NodeStat
	for i := range pods {
		name := pods[i].Spec.NodeName
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		node := &corev1.Node{}
		if err := b.client.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
			if !apierrors.IsNotFound(err) {
				logger.Info("builder: node unavailable, sent by name only", "node", name, "err", err)
			}
			gone = append(gone, NodeStat{Name: name, ObservedAt: metav1.Now()})
			continue
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return gone
	}
	return append(b.buildNodeStats(ctx, nodes, appRef, nil, false), gone...)
}

func (b *DecisionContextBuilder) appPodUIDs(
	ctx context.Context,
	appRef orchestrationv1alpha1.ApplicationReference,
) (map[types.UID]bool, error) {
	pods, err := utils.FindPodsForApplication(ctx, b.client, appRef)
	if err != nil {
		return nil, err
	}
	uids := make(map[types.UID]bool, len(pods))
	for i := range pods {
		uids[pods[i].UID] = true
	}
	return uids, nil
}

// describeNode copies the Node's own decision-relevant fields; usage is added separately.
func describeNode(node *corev1.Node, now metav1.Time) NodeStat {
	info := node.Status.NodeInfo
	stat := NodeStat{
		Name:          node.Name,
		Labels:        node.Labels,
		Taints:        node.Spec.Taints,
		Unschedulable: node.Spec.Unschedulable,
		Conditions:    make(map[string]string, len(node.Status.Conditions)),
		Platform: NodePlatform{
			Arch: info.Architecture, OS: info.OperatingSystem, Kernel: info.KernelVersion,
			Runtime: info.ContainerRuntimeVersion, Kubelet: info.KubeletVersion,
		},
		Resources:  make(map[string]NodeResource, len(node.Status.Allocatable)),
		ObservedAt: now,
	}
	for _, c := range node.Status.Conditions {
		stat.Conditions[string(c.Type)] = string(c.Status)
	}
	for name, q := range node.Status.Allocatable {
		stat.Resources[string(name)] = NodeResource{Allocatable: baseValue(name, q)}
	}
	return stat
}

// addUsage fills Requested for every allocatable resource, plus AppPods.
func addUsage(stat *NodeStat, pods []corev1.Pod, appUIDs map[types.UID]bool) {
	requested := map[corev1.ResourceName]int64{}
	appPods := 0
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		requested[corev1.ResourcePods]++
		for name, q := range podRequests(pod) {
			requested[name] += baseValue(name, q)
		}
		if appUIDs[pod.UID] {
			appPods++
		}
	}
	for name, r := range stat.Resources {
		v := requested[corev1.ResourceName(name)]
		r.Requested = &v
		stat.Resources[name] = r
	}
	if appUIDs != nil {
		stat.AppPods = &appPods
	}
}

// baseValue is CPU in millicores and every other resource in base units.
func baseValue(name corev1.ResourceName, q resource.Quantity) int64 {
	if name == corev1.ResourceCPU {
		return q.MilliValue()
	}
	return q.Value()
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

// imagesOf returns the pod's container and init-container images; nil for a nil pod.
func imagesOf(pod *corev1.Pod) []string {
	if pod == nil {
		return nil
	}
	images := make([]string, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.InitContainers {
		images = append(images, c.Image)
	}
	for _, c := range pod.Spec.Containers {
		images = append(images, c.Image)
	}
	return images
}

// imagesPresent returns which of images the node already has pulled. Best-effort
// match: the kubelet reports fully-qualified names (docker.io/library/nginx:latest).
func imagesPresent(node *corev1.Node, images []string) []string {
	var present []string
	for _, image := range images {
		ref := withDefaultTag(image)
		if nodeHasImage(node, ref) {
			present = append(present, image)
		}
	}
	return present
}

func nodeHasImage(node *corev1.Node, ref string) bool {
	for _, img := range node.Status.Images {
		for _, name := range img.Names {
			if name == ref || strings.HasSuffix(name, "/"+ref) {
				return true
			}
		}
	}
	return false
}

// withDefaultTag adds ":latest" to an image with neither tag nor digest, as the runtime does.
func withDefaultTag(image string) string {
	if strings.Contains(image, "@") {
		return image
	}
	last := image[strings.LastIndex(image, "/")+1:]
	if strings.Contains(last, ":") {
		return image
	}
	return image + ":latest"
}
