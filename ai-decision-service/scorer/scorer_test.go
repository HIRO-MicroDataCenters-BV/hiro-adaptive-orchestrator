package scorer

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// near compares floats with a small tolerance, since float math isn't exact.
func near(a, b float64) bool { return math.Abs(a-b) < 0.001 }

// node builds a candidate with cpu/memory allocatable and requested, and the app's pod count.
func node(name, zone string, cpuAlloc, cpuReq, memAlloc, memReq int64, appPods int) placement.NodeStat {
	var labels map[string]string
	if zone != "" {
		labels = map[string]string{DefaultZoneLabel: zone}
	} else {
		labels = nil
	}
	return placement.NodeStat{
		Name:   name,
		Labels: labels,
		Resources: map[string]placement.NodeResource{
			"cpu": {
				Allocatable: cpuAlloc,
				Requested:   &cpuReq,
			},
			"memory": {
				Allocatable: memAlloc,
				Requested:   &memReq,
			},
		},
		AppPods: &appPods,
	}
}

func TestFormulas(t *testing.T) {
	// Test cases for the formula function can be added here.
	cfg := DefaultConfig()
	util1 := map[string]float64{"cpu": 0.5, "memory": 0.7}
	util2 := map[string]float64{"cpu": 0.9, "memory": 0.5}
	util3 := map[string]float64{"cpu": 1.2, "memory": 0.5}
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{name: "packed average", got: packedScore(util1, cfg), want: 60},
		{name: "packed above limit", got: packedScore(util2, cfg), want: 0},
		{name: "balanced", got: balancedScore(util1, cfg), want: 26},
		{name: "balanced does not fit", got: balancedScore(util3, cfg), want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !near(c.got, c.want) {
				t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		change  func(c *Config) // breaks one field
		wantErr bool
	}{
		{"default is valid", func(c *Config) {}, false},
		{"packed limit 0", func(c *Config) { c.PackedMaxUtil = 0 }, true},
		{"packed limit above 1", func(c *Config) { c.PackedMaxUtil = 1.5 }, true},
		{"negative penalty", func(c *Config) { c.ImbalancePenalty = -1 }, true},
		{"all spread weights 0", func(c *Config) {
			c.SpreadZoneWeight, c.SpreadNodeWeight, c.SpreadBalancedWeight = 0, 0, 0
		}, true},
		{"empty zone label", func(c *Config) { c.ZoneLabel = "" }, true},
		{"unknown score above 100", func(c *Config) { c.UnknownScore = 150 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.change(&c)
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestScore(t *testing.T) {
	profile := func(strategy string) *placement.AOProfileContext {
		return &placement.AOProfileContext{Strategy: strategy}
	}
	cpuOnlyProfile := profile("Balanced")
	cpuOnlyProfile.Awareness.CPU = true
	// n1 is 20% full, n2 is 60% full (cpu and memory); the pod requests nothing.
	lowHighNodeStats := []placement.NodeStat{
		node("n1", "", 1000, 200, 1000, 200, 0),
		node("n2", "", 1000, 600, 1000, 600, 0),
	}
	// The Spread example: zone A has 2 app pods, zone B has 1.
	zonesNodeStats := []placement.NodeStat{
		node("n1", "A", 1000, 0, 1000, 0, 2),
		node("n2", "A", 1000, 0, 1000, 0, 0),
		node("n3", "B", 1000, 0, 1000, 0, 1),
	}
	unknownNodeStats := node("n1", "", 1000, 0, 1000, 0, 0)
	unknownNodeStats.Resources["cpu"] = placement.NodeResource{Allocatable: 1000} // Requested nil
	podFitNodeStats := []placement.NodeStat{
		node("n1", "", 1000, 800, 1000, 500, 0),
		node("n2", "", 1000, 0, 1000, 500, 0),
	}
	podFitPod := corev1.Pod{}
	podFitPod.Spec.Containers = []corev1.Container{
		{
			Name:  "c1",
			Image: "nginx",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{"cpu": resource.MustParse("500m")},
			},
		},
	}

	// CPU-only: equal CPU, very different memory; memory is ignored, so both score the same.
	cpuOnlyNodeStats := []placement.NodeStat{
		node("n1", "", 1000, 200, 1000, 900, 0),
		node("n2", "", 1000, 200, 1000, 100, 0),
	}

	cases := []struct {
		name       string
		req        placement.DecisionRequest
		wantReason string             // substring of the reason; empty skips the check
		wantScores map[string]float64 // node name → score; nil skips the check
	}{
		{
			name:       "packed picks fuller",
			req:        placement.DecisionRequest{AOProfile: profile("Packed"), NodeStats: lowHighNodeStats},
			wantReason: "Packed: n2 best",
		},
		{
			name:       "balanced picks emptier",
			req:        placement.DecisionRequest{AOProfile: profile("Balanced"), NodeStats: lowHighNodeStats},
			wantReason: "Balanced: n1 best",
		},
		{
			name:       "spread picks emptier zone",
			req:        placement.DecisionRequest{AOProfile: profile("Spread"), NodeStats: zonesNodeStats},
			wantReason: "Spread: n3 best",
		},
		{
			name:       "nil profile uses balanced",
			req:        placement.DecisionRequest{NodeStats: lowHighNodeStats},
			wantReason: "Balanced: n1 best",
		},
		{
			name:       "no nodes",
			req:        placement.DecisionRequest{AOProfile: profile("Packed")},
			wantReason: "no candidate nodes",
		},
		{
			name:       "pod fits on one node only",
			req:        placement.DecisionRequest{AOProfile: profile("Packed"), NodeStats: podFitNodeStats, Pod: &podFitPod},
			wantReason: "Packed: n2 best",
			wantScores: map[string]float64{"n1": 0, "n2": 50},
		},
		{
			name:       "unknown data gets the neutral score",
			req:        placement.DecisionRequest{NodeStats: []placement.NodeStat{unknownNodeStats}},
			wantScores: map[string]float64{"n1": DefaultUnknownScore},
		},
		{
			name:       "cpu-only awareness ignores memory",
			req:        placement.DecisionRequest{AOProfile: cpuOnlyProfile, NodeStats: cpuOnlyNodeStats},
			wantScores: map[string]float64{"n1": 80, "n2": 80},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scores, reason := Score(c.req, DefaultConfig())
			if !strings.Contains(reason, c.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", reason, c.wantReason)
			}
			for _, s := range scores {
				if want, ok := c.wantScores[s.NodeName]; ok && !near(s.Score, want) {
					t.Errorf("score of %s = %v, want %v", s.NodeName, s.Score, want)
				}
			}
		})
	}
}

func BenchmarkScore(b *testing.B) {
	nodes := make([]placement.NodeStat, 0, 500)
	for i := range 500 {
		nodes = append(nodes, node(fmt.Sprintf("n%d", i), fmt.Sprintf("z%d", i%3), 4000, 1000, 8000, 2000, i%5))
	}
	req := placement.DecisionRequest{AOProfile: &placement.AOProfileContext{Strategy: "Spread"}, NodeStats: nodes}
	cfg := DefaultConfig()
	for b.Loop() {
		Score(req, cfg)
	}
}
