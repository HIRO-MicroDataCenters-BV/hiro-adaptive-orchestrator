package rebalance

import (
	"slices"
	"strings"
	"testing"
	"time"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var testNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// matches is true when got is empty for an empty want, or contains want.
func matches(got, want string) bool {
	if want == "" {
		return got == ""
	}
	return strings.Contains(got, want)
}

// at is a time relative to testNow.
func at(d time.Duration) *metav1.Time {
	t := metav1.NewTime(testNow.Add(d))
	return &t
}

// eao is an EAO context with this verdict, updated ago before testNow.
func eao(verdict placement.EAOVerdict, ago time.Duration, holdUntil *metav1.Time) *placement.EAOProfileContext {
	return &placement.EAOProfileContext{
		Verdict:     verdict,
		LastUpdated: testNow.Add(-ago).Format(time.RFC3339),
		HoldUntil:   holdUntil,
	}
}

// moved is a Move history entry from source to newNode, ago before testNow.
func moved(source, newNode string, ago time.Duration) orchestrationv1alpha1.HistoryEntry {
	return orchestrationv1alpha1.HistoryEntry{
		Action:     orchestrationv1alpha1.RebalanceActionMove,
		SourceNode: source,
		NewPodNode: newNode,
		At:         *at(-ago),
	}
}

// managed marks the app's replicas and/or resources as owned by something else.
func managed(replicas, resources bool) func(*placement.DecisionRequest) {
	return func(r *placement.DecisionRequest) {
		r.AOProfile = &placement.AOProfileContext{Rebalancing: placement.RebalancingConfig{
			ReplicasExternallyManaged:  replicas,
			ResourcesExternallyManaged: resources,
		}}
	}
}

// gatesFor applies the gates to req as Decide does: facts first.
func gatesFor(req placement.DecisionRequest) gates {
	cfg := DefaultConfig()
	return applyGates(req, testNow, cfg, classifyNodes(req, cfg))
}

// TestBlocks runs one answer of each kind through the gates (the request is validateRequest's: p1 on n1, 3 replicas).
func TestBlocks(t *testing.T) {
	type resp = placement.RebalanceDecisionResponse
	answers := map[string]resp{
		"move":       {Action: orchestrationv1alpha1.RebalanceActionMove, PodName: "p1", TargetNode: "n2"},
		"scale up":   {Action: orchestrationv1alpha1.RebalanceActionAdjustReplicas, TargetReplicas: 4},
		"scale down": {Action: orchestrationv1alpha1.RebalanceActionAdjustReplicas, TargetReplicas: 2},
		"resize":     {Action: orchestrationv1alpha1.RebalanceActionAdjustResources, ContainerName: "app", TargetCPU: "500m"},
		"noop":       {Action: orchestrationv1alpha1.RebalanceActionNoOp},
	}
	history := func(entries ...orchestrationv1alpha1.HistoryEntry) func(*placement.DecisionRequest) {
		return func(r *placement.DecisionRequest) { r.RebalanceContext.History = entries }
	}
	cases := []struct {
		name  string
		setup func(req *placement.DecisionRequest)
		want  map[string]string // answer → part of the reason; a missing answer must pass
	}{
		{name: "nothing set"},
		{
			name:  "cooldown ahead blocks every change, not NoOp",
			setup: func(r *placement.DecisionRequest) { r.RebalanceContext.CooldownUntil = at(5 * time.Minute) },
			want: map[string]string{
				"move": "cooldown until 2026-10-30T12:05:00Z", "scale up": "cooldown until",
				"scale down": "cooldown until", "resize": "cooldown until",
			},
		},
		{
			name:  "cooldown over",
			setup: func(r *placement.DecisionRequest) { r.RebalanceContext.CooldownUntil = at(-time.Minute) },
		},
		{
			name:  "replicas managed",
			setup: managed(true, false),
			want: map[string]string{
				"scale up": "replicas are externally managed", "scale down": "replicas are externally managed",
			},
		},
		{
			name:  "resources managed",
			setup: managed(false, true),
			want:  map[string]string{"resize": "resources are externally managed"},
		},
		{
			name:  "PDB allows no disruption",
			setup: func(r *placement.DecisionRequest) { r.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web"} },
			want:  map[string]string{"move": "PDB web does not allow disruptions"},
		},
		{
			name: "PDB allows one",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web", AllowedDisruptions: 1}
			},
		},
		{
			name: "cooldown wins over the PDB",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.CooldownUntil = at(5 * time.Minute)
				r.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web"}
			},
			want: map[string]string{
				"move": "cooldown until", "scale up": "cooldown until",
				"scale down": "cooldown until", "resize": "cooldown until",
			},
		},
		{
			name: "fresh EAO hold stops new pods, not a scale-down",
			setup: func(r *placement.DecisionRequest) {
				r.EAOProfile = eao(placement.EAOVerdictHold, time.Minute, at(10*time.Minute))
			},
			want: map[string]string{
				"move": "EAO holds until 2026-10-30T12:10:00Z", "scale up": "EAO holds until",
				"resize": "EAO holds until",
			},
		},
		{
			name:  "fresh EAO hold without a slot",
			setup: func(r *placement.DecisionRequest) { r.EAOProfile = eao(placement.EAOVerdictHold, time.Minute, nil) },
			want: map[string]string{
				"move": "EAO holds the workload for lack of energy", "scale up": "for lack of energy",
				"resize": "for lack of energy",
			},
		},
		{
			name: "PDB wins over the hold for a move",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web"}
				r.EAOProfile = eao(placement.EAOVerdictHold, time.Minute, nil)
			},
			want: map[string]string{"move": "PDB web", "scale up": "EAO holds", "resize": "EAO holds"},
		},
		{
			name:  "stale EAO hold",
			setup: func(r *placement.DecisionRequest) { r.EAOProfile = eao(placement.EAOVerdictHold, 10*time.Minute, nil) },
		},
		{
			name: "EAO hold with a bad lastUpdated",
			setup: func(r *placement.DecisionRequest) {
				r.EAOProfile = &placement.EAOProfileContext{Verdict: placement.EAOVerdictHold, LastUpdated: "yesterday"}
			},
		},
		{
			name:  "EAO run now",
			setup: func(r *placement.DecisionRequest) { r.EAOProfile = eao(placement.EAOVerdictRunNow, time.Minute, nil) },
		},
		{
			name:  "churn: a recent move filled the source",
			setup: history(moved("n0", "n1", 5*time.Minute)),
			want:  map[string]string{"move": "a recent move put a pod on n1"},
		},
		{
			name: "churn: an at-risk source may still move",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.History = []orchestrationv1alpha1.HistoryEntry{moved("n0", "n1", 5*time.Minute)}
				r.NodeStats[0].Unschedulable = true // n1 is now at risk
			},
		},
		{
			name:  "ping-pong: a recent move emptied the target",
			setup: history(moved("n2", "n3", 5*time.Minute)),
			want:  map[string]string{"move": "a recent move took a pod off n2"},
		},
		{name: "move older than the window", setup: history(moved("n2", "n1", 40*time.Minute))},
		{name: "move without a replacement pod", setup: history(moved("n2", "", 5*time.Minute))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validateRequest()
			if c.setup != nil {
				c.setup(&req)
			}
			g := gatesFor(req)
			for name, answer := range answers {
				if got := g.blocks(changeOf(answer, req)); !matches(got, c.want[name]) {
					t.Errorf("%s: got %q, want %q", name, got, c.want[name])
				}
			}
		})
	}
}

func TestChangeOf(t *testing.T) {
	req := validateRequest()
	type resp = placement.RebalanceDecisionResponse
	cases := []struct {
		name string
		resp resp
		want change
	}{
		{
			name: "move",
			resp: resp{Action: orchestrationv1alpha1.RebalanceActionMove, PodName: "p1", TargetNode: "n2"},
			want: change{action: orchestrationv1alpha1.RebalanceActionMove, addsPods: true, source: "n1", target: "n2"},
		},
		{
			name: "scale up",
			resp: resp{Action: orchestrationv1alpha1.RebalanceActionAdjustReplicas, TargetReplicas: 4},
			want: change{action: orchestrationv1alpha1.RebalanceActionAdjustReplicas, addsPods: true},
		},
		{
			name: "scale down",
			resp: resp{Action: orchestrationv1alpha1.RebalanceActionAdjustReplicas, TargetReplicas: 2},
			want: change{action: orchestrationv1alpha1.RebalanceActionAdjustReplicas},
		},
		{
			name: "resize",
			resp: resp{Action: orchestrationv1alpha1.RebalanceActionAdjustResources, ContainerName: "app"},
			want: change{action: orchestrationv1alpha1.RebalanceActionAdjustResources, addsPods: true},
		},
		{
			name: "NoOp",
			resp: resp{Action: orchestrationv1alpha1.RebalanceActionNoOp},
			want: change{action: orchestrationv1alpha1.RebalanceActionNoOp},
		},
		{
			name: "unknown action is assumed to add pods",
			resp: resp{Action: "Teleport"},
			want: change{action: "Teleport", addsPods: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := changeOf(c.resp, req); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestBlockedReasons(t *testing.T) {
	req := validateRequest()
	req.RebalanceContext.CooldownUntil = at(5 * time.Minute)
	req.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web"}
	req.EAOProfile = eao(placement.EAOVerdictHold, time.Minute, nil)
	managed(true, false)(&req)

	want := []string{
		"cooldown until 2026-10-30T12:05:00Z",
		"AdjustReplicas: replicas are externally managed",
		"Move: PDB web does not allow disruptions",
		"new pods: EAO holds the workload for lack of energy",
	}
	if got := gatesFor(req).blockedReasons(); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := gatesFor(validateRequest()).blockedReasons(); len(got) != 0 {
		t.Errorf("got %q, want nothing blocked", got)
	}
}
