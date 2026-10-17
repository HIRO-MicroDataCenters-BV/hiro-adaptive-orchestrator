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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testGPU = "nvidia.com/gpu"

func withRequests(pod *corev1.Pod, uid, cpu, memory string) *corev1.Pod {
	pod.UID = types.UID(uid)
	pod.Spec.Containers = []corev1.Container{{
		Name:  "app",
		Image: "nginx",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}},
	}}
	return pod
}

func sizedNode(name string, labels map[string]string) *corev1.Node {
	node := testNode(name, true)
	node.Labels = labels
	node.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
		testGPU:               resource.MustParse("2"),
	}
	node.Status.NodeInfo = corev1.NodeSystemInfo{Architecture: "arm64", OperatingSystem: "linux", KubeletVersion: "v1.35.0"}
	return node
}

func requested(t *testing.T, s NodeStat, name string) int64 {
	t.Helper()
	r, ok := s.Resources[name]
	if !ok || r.Requested == nil {
		t.Fatalf("resource %q = %+v, want a known requested value", name, r)
	}
	return *r.Requested
}

func TestBuildNodeStats_DescribesNodeAndUsage(t *testing.T) {
	nodeA := sizedNode("node-a", map[string]string{"topology.kubernetes.io/zone": "zone-1"})
	nodeB := sizedNode("node-b", nil)

	appPod := withRequests(testAppPod("app-a-1", "node-a", corev1.PodRunning), "uid-1", "500m", "256Mi")
	otherPod := withRequests(testAppPod("other-1", "node-a", corev1.PodRunning), "uid-2", "250m", "128Mi")
	otherPod.Labels = map[string]string{"app": "other"}
	donePod := withRequests(testAppPod("app-a-done", "node-a", corev1.PodSucceeded), "uid-3", "1", "1Gi")

	b := newTestBuilder(t, testAppDeployment(), nodeA, nodeB, appPod, otherPod, donePod)
	stats := b.buildNodeStats(context.Background(), []*corev1.Node{nodeA, nodeB}, testAppRef(), nil)
	if len(stats) != 2 {
		t.Fatalf("len(stats) = %d, want 2", len(stats))
	}

	a := stats[0]
	if a.Name != "node-a" || a.Labels["topology.kubernetes.io/zone"] != "zone-1" {
		t.Errorf("node-a name/labels = %q %v", a.Name, a.Labels)
	}
	if a.Conditions["Ready"] != "True" || a.Platform.Arch != "arm64" || a.Platform.Kubelet != "v1.35.0" {
		t.Errorf("node-a conditions/platform = %v %+v", a.Conditions, a.Platform)
	}
	if got := requested(t, a, "cpu"); got != 750 {
		t.Errorf("cpu requested = %d, want 750 (succeeded pod excluded)", got)
	}
	if got := requested(t, a, "memory"); got != 384*1024*1024 {
		t.Errorf("memory requested = %d, want 384Mi", got)
	}
	if got := requested(t, a, "pods"); got != 2 {
		t.Errorf("pods requested = %d, want 2", got)
	}
	if a.Resources["cpu"].Allocatable != 4000 || a.Resources[testGPU].Allocatable != 2 {
		t.Errorf("allocatable = %+v, want 4000m CPU and 2 GPUs", a.Resources)
	}
	if a.AppPods == nil || *a.AppPods != 1 {
		t.Errorf("appPods = %v, want 1", a.AppPods)
	}
	if a.Energy != nil || a.Resources["cpu"].Used != nil {
		t.Errorf("energy/used should stay null until a source exists")
	}
	if got := requested(t, stats[1], "cpu"); got != 0 {
		t.Errorf("node-b cpu requested = %d, want 0", got)
	}
}

// failNodeListClient fails per-node pod lists, as an unsynced or missing index would.
type failNodeListClient struct{ client.Client }

func (c failNodeListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	for _, o := range opts {
		if _, ok := o.(client.MatchingFields); ok {
			return errors.New("index unavailable")
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func TestBuildNodeStats_KeepsNodeWhenUsageUnknown(t *testing.T) {
	node := sizedNode("node-a", nil)
	b := newTestBuilder(t, testAppDeployment(), node)
	b.client = failNodeListClient{b.client}

	stats := b.buildNodeStats(context.Background(), []*corev1.Node{node}, testAppRef(), nil)
	if len(stats) != 1 {
		t.Fatalf("len(stats) = %d, want the node kept", len(stats))
	}
	if r := stats[0].Resources["cpu"]; r.Allocatable != 4000 || r.Requested != nil {
		t.Errorf("cpu = %+v, want allocatable 4000 and requested null", r)
	}
	if stats[0].AppPods != nil {
		t.Errorf("appPods = %v, want null", *stats[0].AppPods)
	}
}

func TestBuildNodeStats_ImagesPresentForPlacedPod(t *testing.T) {
	node := sizedNode("node-a", nil)
	node.Status.Images = []corev1.ContainerImage{{Names: []string{"docker.io/library/nginx:latest"}}}
	pod := withRequests(testAppPod("new", "", corev1.PodPending), "uid-new", "100m", "64Mi")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "cache", Image: "redis:7"})

	b := newTestBuilder(t, testAppDeployment(), node)
	stats := b.buildNodeStats(context.Background(), []*corev1.Node{node}, testAppRef(), pod)
	if got := stats[0].ImagesPresent; len(got) != 1 || got[0] != "nginx" {
		t.Errorf("imagesPresent = %v, want [nginx] (redis:7 not pulled)", got)
	}
}

func TestWithDefaultTag(t *testing.T) {
	cases := map[string]string{
		"nginx":                     "nginx:latest",
		"nginx:1.25":                "nginx:1.25",
		"registry:5000/team/app":    "registry:5000/team/app:latest",
		"registry:5000/team/app:v2": "registry:5000/team/app:v2",
		"nginx@sha256:abc":          "nginx@sha256:abc",
	}
	for in, want := range cases {
		if got := withDefaultTag(in); got != want {
			t.Errorf("withDefaultTag(%q) = %q, want %q", in, got, want)
		}
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
