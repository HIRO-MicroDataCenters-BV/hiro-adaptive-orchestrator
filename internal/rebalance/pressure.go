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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultNodePressureThreshold is the fraction of a node's allocatable
// capacity, measured against live usage from metrics-server, above which
// the node is considered under enough pressure to warrant rebalancing a pod
// away from it.
const DefaultNodePressureThreshold = 0.90

// NodePressureEvaluator computes node-level CPU/Memory pressure from
// metrics-server's live usage data (metrics.k8s.io).
//
// CPU and Memory are evaluated independently via EvaluateCPUPressure and
// EvaluateMemoryPressure — each is self-contained (fetches what it needs
// and returns a result), so Story 26's trigger dispatch can call whichever
// one applies to the declared condition without any shared-fetch
// bookkeeping between them.
//
// metrics-server is an optional cluster component. Both methods return a
// descriptive error when it's unavailable so callers can treat CPU/Memory
// conditions as "not currently evaluable" rather than crashing — the same
// soft-fail posture used for the optional EnergyAwareOrchestration CRD in
// internal/placement-server.
type NodePressureEvaluator struct {
	client        client.Client
	metricsClient metricsv.Interface
	threshold     float64
}

// NewNodePressureEvaluator creates a NodePressureEvaluator. threshold <= 0
// uses DefaultNodePressureThreshold.
func NewNodePressureEvaluator(c client.Client, metricsClient metricsv.Interface, threshold float64) *NodePressureEvaluator {
	if threshold <= 0 {
		threshold = DefaultNodePressureThreshold
	}
	return &NodePressureEvaluator{client: c, metricsClient: metricsClient, threshold: threshold}
}

// EvaluateCPUPressure reports whether the node's live CPU usage is at or
// above the configured threshold of its allocatable CPU capacity.
func (e *NodePressureEvaluator) EvaluateCPUPressure(ctx context.Context, nodeName string) (bool, string, error) {
	return e.evaluate(ctx, nodeName, corev1.ResourceCPU)
}

// EvaluateMemoryPressure reports whether the node's live Memory usage is at
// or above the configured threshold of its allocatable Memory capacity.
func (e *NodePressureEvaluator) EvaluateMemoryPressure(ctx context.Context, nodeName string) (bool, string, error) {
	return e.evaluate(ctx, nodeName, corev1.ResourceMemory)
}

// evaluate fetches the node's allocatable capacity and metrics-server's
// live usage, then compares the ratio for resourceName (the ResourceList
// map key — "cpu" or "memory" — identifying which entry of the node's data
// to check) against the configured threshold.
func (e *NodePressureEvaluator) evaluate(
	ctx context.Context,
	nodeName string,
	resourceName corev1.ResourceName,
) (bool, string, error) {
	node := &corev1.Node{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return false, "", fmt.Errorf("fetching node %s: %w", nodeName, err)
	}

	allocatable := node.Status.Allocatable[resourceName]
	if allocatable.IsZero() {
		return false, "", nil
	}

	nodeMetrics, err := e.metricsClient.MetricsV1beta1().NodeMetricses().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false, "", fmt.Errorf("fetching metrics-server usage for node %s: %w", nodeName, err)
	}

	usage := nodeMetrics.Usage[resourceName]
	ratio := float64(usage.MilliValue()) / float64(allocatable.MilliValue())
	if ratio >= e.threshold {
		return true, fmt.Sprintf(
			"node %s: %s usage at %.0f%% of allocatable (%.0f%% threshold)",
			nodeName, resourceName, ratio*100, e.threshold*100,
		), nil
	}
	return false, "", nil
}
