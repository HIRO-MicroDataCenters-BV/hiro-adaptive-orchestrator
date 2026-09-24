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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newTestNode returns a Node with a fixed allocatable capacity (4 CPU, 8Gi
// memory) — every test below only varies usage, not allocatable.
func newTestNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
}

func newTestNodeMetrics(name, cpuUsage, memUsage string) *metricsv1beta1.NodeMetrics {
	return &metricsv1beta1.NodeMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Usage: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuUsage),
			corev1.ResourceMemory: resource.MustParse(memUsage),
		},
	}
}

func newTestPressureEvaluator(t *testing.T, node *corev1.Node, metrics *metricsv1beta1.NodeMetrics, threshold float64) *NodePressureEvaluator {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()

	// NewSimpleClientset is deprecated in favor of NewClientset, but the
	// latter is only generated with --with-applyconfig, which this package
	// isn't built with — NewSimpleClientset is the only constructor
	// available here.
	metricsClient := metricsfake.NewSimpleClientset() //nolint:staticcheck
	// NewSimpleClientset(objects...) seeds via the tracker's naive Kind->Resource
	// pluralization ("nodemetrics"), but NodeMetrics' actual REST resource name
	// is the irregular "nodes" — so objects passed to the constructor are
	// stored under a bucket the typed client never queries. Seed explicitly
	// against the real GVR instead.
	gvr := metricsv1beta1.SchemeGroupVersion.WithResource("nodes")
	if err := metricsClient.Tracker().Create(gvr, metrics, ""); err != nil {
		t.Fatalf("seeding node metrics: %v", err)
	}
	return NewNodePressureEvaluator(c, metricsClient, threshold)
}

func TestNodePressureEvaluator_UnderThreshold(t *testing.T) {
	node := newTestNode("node-a")
	metrics := newTestNodeMetrics("node-a", "1", "2Gi") // 25% cpu, 25% mem
	evaluator := newTestPressureEvaluator(t, node, metrics, 0.90)

	cpuExceeded, _, err := evaluator.EvaluateCPUPressure(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("EvaluateCPUPressure: %v", err)
	}
	if cpuExceeded {
		t.Error("CPU pressure exceeded = true, want false at 25% usage")
	}

	memExceeded, _, err := evaluator.EvaluateMemoryPressure(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("EvaluateMemoryPressure: %v", err)
	}
	if memExceeded {
		t.Error("Memory pressure exceeded = true, want false at 25% usage")
	}
}

func TestNodePressureEvaluator_AtOrOverThreshold(t *testing.T) {
	node := newTestNode("node-b")
	metrics := newTestNodeMetrics("node-b", "3800m", "7.6Gi") // 95% cpu, 95% mem
	evaluator := newTestPressureEvaluator(t, node, metrics, 0.90)

	cpuExceeded, reason, err := evaluator.EvaluateCPUPressure(context.Background(), "node-b")
	if err != nil {
		t.Fatalf("EvaluateCPUPressure: %v", err)
	}
	if !cpuExceeded {
		t.Error("CPU pressure exceeded = false, want true at 95% usage")
	}
	if reason == "" {
		t.Error("expected non-empty reason when threshold exceeded")
	}

	memExceeded, reason, err := evaluator.EvaluateMemoryPressure(context.Background(), "node-b")
	if err != nil {
		t.Fatalf("EvaluateMemoryPressure: %v", err)
	}
	if !memExceeded {
		t.Error("Memory pressure exceeded = false, want true at 95% usage")
	}
	if reason == "" {
		t.Error("expected non-empty reason when threshold exceeded")
	}
}

func TestNodePressureEvaluator_ZeroAllocatable(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-c"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{}, // no cpu/memory reported
		},
	}
	metrics := newTestNodeMetrics("node-c", "1", "1Gi")
	evaluator := newTestPressureEvaluator(t, node, metrics, 0.90)

	exceeded, reason, err := evaluator.EvaluateCPUPressure(context.Background(), "node-c")
	if err != nil {
		t.Fatalf("EvaluateCPUPressure: %v", err)
	}
	if exceeded {
		t.Error("exceeded = true, want false when allocatable is zero/unreported")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestNodePressureEvaluator_MetricsServerUnavailable(t *testing.T) {
	node := newTestNode("node-d")
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	metricsClient := metricsfake.NewSimpleClientset() //nolint:staticcheck // no NodeMetrics registered — Get() will 404

	evaluator := NewNodePressureEvaluator(c, metricsClient, 0.90)
	_, _, err := evaluator.EvaluateCPUPressure(context.Background(), "node-d")
	if err == nil {
		t.Fatal("expected error when metrics-server has no data for the node, got nil")
	}
}

func TestNewNodePressureEvaluator_DefaultThreshold(t *testing.T) {
	node := newTestNode("node-e")
	metrics := newTestNodeMetrics("node-e", "3900m", "7.9Gi")  // ~97%, above 90% default
	evaluator := newTestPressureEvaluator(t, node, metrics, 0) // threshold<=0 -> default

	exceeded, _, err := evaluator.EvaluateCPUPressure(context.Background(), "node-e")
	if err != nil {
		t.Fatalf("EvaluateCPUPressure: %v", err)
	}
	if !exceeded {
		t.Error("expected default threshold (90%) to be exceeded at ~97% usage")
	}
}
