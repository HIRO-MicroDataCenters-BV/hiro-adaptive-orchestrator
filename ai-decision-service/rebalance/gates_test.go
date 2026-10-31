package rebalance

import (
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

func TestApplyGates(t *testing.T) {
	cases := []struct {
		name                      string
		setup                     func(req *placement.DecisionRequest)
		move, replicas, resources string // "" means allowed, else part of the reason
	}{
		{name: "nothing set"},
		{
			name:      "cooldown ahead blocks everything",
			setup:     func(r *placement.DecisionRequest) { r.RebalanceContext.CooldownUntil = at(5 * time.Minute) },
			move:      "cooldown until 2026-10-30T12:05:00Z",
			replicas:  "cooldown until",
			resources: "cooldown until",
		},
		{
			name:  "cooldown over",
			setup: func(r *placement.DecisionRequest) { r.RebalanceContext.CooldownUntil = at(-time.Minute) },
		},
		{name: "replicas managed", setup: managed(true, false), replicas: "replicas are externally managed"},
		{name: "resources managed", setup: managed(false, true), resources: "resources are externally managed"},
		{
			name:  "PDB allows no disruption",
			setup: func(r *placement.DecisionRequest) { r.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web"} },
			move:  "PDB web does not allow disruptions",
		},
		{
			name: "PDB allows one",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web", AllowedDisruptions: 1}
			},
		},
		{
			name: "first reason is kept",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.CooldownUntil = at(5 * time.Minute)
				r.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web"}
			},
			move:      "cooldown until",
			replicas:  "cooldown until",
			resources: "cooldown until",
		},
		{
			name: "fresh EAO hold with a slot",
			setup: func(r *placement.DecisionRequest) {
				r.EAOProfile = eao(placement.EAOVerdictHold, time.Minute, at(10*time.Minute))
			},
			move: "EAO holds until 2026-10-30T12:10:00Z",
		},
		{
			name:  "fresh EAO hold without a slot",
			setup: func(r *placement.DecisionRequest) { r.EAOProfile = eao(placement.EAOVerdictHold, time.Minute, nil) },
			move:  "EAO holds the workload for lack of energy",
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := request(nil)
			if c.setup != nil {
				c.setup(&req)
			}
			g := applyGates(req, testNow, DefaultConfig())
			want := map[orchestrationv1alpha1.RebalanceAction]string{
				orchestrationv1alpha1.RebalanceActionMove:            c.move,
				orchestrationv1alpha1.RebalanceActionAdjustReplicas:  c.replicas,
				orchestrationv1alpha1.RebalanceActionAdjustResources: c.resources,
			}
			for a, w := range want {
				if got := g.reasonFor(a); !matches(got, w) {
					t.Errorf("%s: got %q, want %q", a, got, w)
				}
			}
		})
	}
}

func TestIsAllowed(t *testing.T) {
	g := applyGates(request(nil), testNow, DefaultConfig())
	g.block(orchestrationv1alpha1.RebalanceActionMove, "test")
	cases := map[orchestrationv1alpha1.RebalanceAction]bool{
		orchestrationv1alpha1.RebalanceActionNoOp:           true,
		orchestrationv1alpha1.RebalanceActionDefer:          true,
		orchestrationv1alpha1.RebalanceActionReject:         true,
		orchestrationv1alpha1.RebalanceActionEscalate:       true,
		orchestrationv1alpha1.RebalanceActionAdjustReplicas: true,
		orchestrationv1alpha1.RebalanceActionMove:           false,
		"Teleport": false,
	}
	for a, want := range cases {
		if got := g.isAllowed(a); got != want {
			t.Errorf("isAllowed(%s) = %v, want %v", a, got, want)
		}
	}
}

func TestMoveBlocked(t *testing.T) {
	healthy := map[string]nodeClass{"n1": classHealthy, "n2": classHealthy}
	cases := []struct {
		name    string
		setup   func(req *placement.DecisionRequest)
		classes map[string]nodeClass
		want    string // "" means allowed, else part of the reason
	}{
		{name: "free move", classes: healthy},
		{
			name:    "cooldown blocks a healthy source",
			setup:   func(r *placement.DecisionRequest) { r.RebalanceContext.CooldownUntil = at(5 * time.Minute) },
			classes: healthy,
			want:    "cooldown until",
		},
		{
			name:    "down source skips cooldown",
			setup:   func(r *placement.DecisionRequest) { r.RebalanceContext.CooldownUntil = at(5 * time.Minute) },
			classes: map[string]nodeClass{"n1": classDown, "n2": classHealthy},
		},
		{
			name: "churn: a recent move filled the source",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.History = []orchestrationv1alpha1.HistoryEntry{moved("n0", "n1", 5*time.Minute)}
			},
			classes: healthy,
			want:    "a recent move put a pod on n1",
		},
		{
			name: "churn: an at-risk source may still move",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.History = []orchestrationv1alpha1.HistoryEntry{moved("n0", "n1", 5*time.Minute)}
			},
			classes: map[string]nodeClass{"n1": classAtRisk, "n2": classHealthy},
		},
		{
			name: "ping-pong: a recent move emptied the target",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.History = []orchestrationv1alpha1.HistoryEntry{moved("n2", "n3", 5*time.Minute)}
			},
			classes: healthy,
			want:    "a recent move took a pod off n2",
		},
		{
			name: "move older than the window",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.History = []orchestrationv1alpha1.HistoryEntry{moved("n2", "n1", 40*time.Minute)}
			},
			classes: healthy,
		},
		{
			name: "move without a replacement pod",
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.History = []orchestrationv1alpha1.HistoryEntry{moved("n2", "", 5*time.Minute)}
			},
			classes: healthy,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := request(nil)
			if c.setup != nil {
				c.setup(&req)
			}
			g := applyGates(req, testNow, DefaultConfig())
			if got := g.moveBlocked("n1", "n2", c.classes); !matches(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
