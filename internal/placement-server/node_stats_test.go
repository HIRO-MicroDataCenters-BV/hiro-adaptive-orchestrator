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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func withRequests(pod *corev1.Pod, uid, cpu, memory string) *corev1.Pod {
	pod.UID = types.UID(uid)
	pod.Spec.Containers = []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}},
	}}
	return pod
}

func sizedNode(name string, labels map[string]string, gpuResource string) *corev1.Node {
	node := testNode(name, true)
	node.Labels = labels
	node.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:               resource.MustParse("4"),
		corev1.ResourceMemory:            resource.MustParse("8Gi"),
		corev1.ResourceName(gpuResource): resource.MustParse("2"),
	}
	return node
}

func TestBuildNodeStats_SumsRequestsAndCountsAppPods(t *testing.T) {
	nodeA := sizedNode("node-a", map[string]string{DefaultZoneLabel: "zone-1"}, DefaultGPUResource)
	nodeB := sizedNode("node-b", nil, DefaultGPUResource)

	appPod := withRequests(testAppPod("app-a-1", "node-a", corev1.PodRunning), "uid-1", "500m", "256Mi")
	otherPod := withRequests(testAppPod("other-1", "node-a", corev1.PodRunning), "uid-2", "250m", "128Mi")
	otherPod.Labels = map[string]string{"app": "other"}
	donePod := withRequests(testAppPod("app-a-done", "node-a", corev1.PodSucceeded), "uid-3", "1", "1Gi")

	b := newTestBuilder(t, testAppDeployment(), nodeA, nodeB, appPod, otherPod, donePod)
	stats, err := b.buildNodeStats(context.Background(), []*corev1.Node{nodeA, nodeB}, testAppRef())
	if err != nil {
		t.Fatalf("buildNodeStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("len(stats) = %d, want 2", len(stats))
	}

	a := stats[0]
	if a.Zone != "zone-1" || a.AppPods != 1 {
		t.Errorf("node-a zone=%q appPods=%d, want zone-1 and 1", a.Zone, a.AppPods)
	}
	if a.Requested.CPUMilli != 750 || a.Requested.MemoryBytes != 384*1024*1024 {
		t.Errorf("node-a requested = %+v, want 750m CPU and 384Mi (succeeded pod excluded)", a.Requested)
	}
	if a.Allocatable.CPUMilli != 4000 || a.Allocatable.GPU != 2 {
		t.Errorf("node-a allocatable = %+v, want 4000m CPU and 2 GPU", a.Allocatable)
	}
	if a.Energy != nil {
		t.Errorf("energy = %v, want nil until a per-node energy source exists", *a.Energy)
	}
	if b := stats[1]; b.AppPods != 0 || b.Requested.CPUMilli != 0 || b.Zone != "" {
		t.Errorf("node-b = %+v, want empty usage and no zone", b)
	}
}

func TestBuildNodeStats_HonoursConfiguredLabels(t *testing.T) {
	node := sizedNode("node-a", map[string]string{"example.com/rack": "rack-7"}, "amd.com/gpu")

	b := newTestBuilder(t, testAppDeployment(), node)
	WithNodeStatsLabels("example.com/rack", "amd.com/gpu")(b)

	stats, err := b.buildNodeStats(context.Background(), []*corev1.Node{node}, testAppRef())
	if err != nil {
		t.Fatalf("buildNodeStats: %v", err)
	}
	if stats[0].Zone != "rack-7" || stats[0].Allocatable.GPU != 2 {
		t.Errorf("stats = %+v, want zone rack-7 and 2 amd.com/gpu", stats[0])
	}
}

func TestPodRequests_InitContainerAndOverhead(t *testing.T) {
	pod := withRequests(testAppPod("p", "node-a", corev1.PodRunning), "uid", "100m", "64Mi")
	pod.Spec.InitContainers = []corev1.Container{{
		Name: "init",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("300m"),
		}},
	}}
	pod.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")}

	got := podRequests(pod)
	if cpu := got.Cpu().MilliValue(); cpu != 310 {
		t.Errorf("cpu = %dm, want 310m (max(100m, init 300m) + 10m overhead)", cpu)
	}
	if mem := got.Memory().Value(); mem != 64*1024*1024 {
		t.Errorf("memory = %d, want 64Mi", mem)
	}
}

func TestMapEAOToProfileContext_SlotFreshnessAndWholeWatts(t *testing.T) {
	eao := testRebalanceEAO("Optional", 200, "Scheduled", "waiting for solar", false)
	_ = unstructured.SetNestedMap(eao.Object, map[string]any{
		"slotStart": "2026-10-16T12:00:00Z", "slotEnd": "2026-10-16T18:00:00Z",
	}, "status", "decision", "scheduledSlot")
	_ = unstructured.SetNestedField(eao.Object, "2026-10-16T11:58:00Z", "status", "lastUpdated")
	_ = unstructured.SetNestedField(eao.Object, int64(200), "status", "energyMetrics", "requiredWatts")
	_ = unstructured.SetNestedField(eao.Object, 187.5, "status", "energyMetrics", "measuredWatts")

	got := mapEAOToProfileContext(eao)

	if got.Decision == nil || got.Decision.ScheduledSlot == nil ||
		got.Decision.ScheduledSlot.SlotStart != "2026-10-16T12:00:00Z" ||
		got.Decision.ScheduledSlot.SlotEnd != "2026-10-16T18:00:00Z" {
		t.Errorf("decision = %+v, want scheduledSlot 12:00–18:00", got.Decision)
	}
	if got.LastUpdated != "2026-10-16T11:58:00Z" {
		t.Errorf("lastUpdated = %q", got.LastUpdated)
	}
	if got.EnergyMetrics == nil || got.EnergyMetrics.RequiredWatts != 200 || got.EnergyMetrics.MeasuredWatts != 187.5 {
		t.Errorf("energyMetrics = %+v, want requiredWatts 200 (whole number) and measuredWatts 187.5", got.EnergyMetrics)
	}
}
