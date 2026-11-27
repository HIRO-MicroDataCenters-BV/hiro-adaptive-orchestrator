package rebalance

import (
	"strings"
	"testing"
	"time"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// sized is a healthy candidate with this share of its CPU and memory requested, so the scorer can rank it.
func sized(name string, usedPercent int64) placement.NodeStat {
	n := stat(name, "True", true)
	used := usedPercent * 10
	n.Resources = map[string]placement.NodeResource{
		"cpu":    {Allocatable: 1000, Requested: &used},
		"memory": {Allocatable: 1000, Requested: &used},
	}
	return n
}

func TestRulesAnswer(t *testing.T) {
	down := stat("n1", "False", false)
	atRisk := stat("n2", "True", true)
	atRisk.Unschedulable = true
	cooldown := func(r *placement.DecisionRequest) { r.RebalanceContext.CooldownUntil = at(5 * time.Minute) }

	cases := []struct {
		name       string
		nodes      []placement.NodeStat
		setup      func(req *placement.DecisionRequest)
		wantAction orchestrationv1alpha1.RebalanceAction
		wantTarget string
		wantReason string // part of the reason; "" skips the check
	}{
		{
			name:       "all healthy",
			nodes:      []placement.NodeStat{sized("n1", 50), sized("n2", 20)},
			wantAction: orchestrationv1alpha1.RebalanceActionNoOp,
			wantReason: "no rule matched",
		},
		{
			name:       "NoOp lists what is blocked",
			nodes:      []placement.NodeStat{sized("n1", 50), sized("n2", 20)},
			setup:      cooldown,
			wantAction: orchestrationv1alpha1.RebalanceActionNoOp,
			wantReason: "blocked: cooldown until",
		},
		{
			name:       "down node moves to the best healthy node",
			nodes:      []placement.NodeStat{down, sized("n3", 70), sized("n2", 20)},
			wantAction: orchestrationv1alpha1.RebalanceActionMove,
			wantTarget: "n2",
			wantReason: "node n1 is down: move p1 to n2",
		},
		{
			name:       "down node skips cooldown",
			nodes:      []placement.NodeStat{down, sized("n2", 20)},
			setup:      cooldown,
			wantAction: orchestrationv1alpha1.RebalanceActionMove,
			wantTarget: "n2",
		},
		{
			name:       "down node skips the PDB pre-check",
			nodes:      []placement.NodeStat{down, sized("n2", 20)},
			setup:      func(r *placement.DecisionRequest) { r.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web"} },
			wantAction: orchestrationv1alpha1.RebalanceActionMove,
			wantTarget: "n2",
		},
		{
			name:       "down node with only at-risk targets",
			nodes:      []placement.NodeStat{down, atRisk},
			wantAction: orchestrationv1alpha1.RebalanceActionNoOp,
			wantReason: "node n1 is down but no healthy node can take pod p1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := request(c.nodes, running("p1", "n1"))
			if c.setup != nil {
				c.setup(&req)
			}
			got := Assess(req, testNow, DefaultConfig()).RulesAnswer(scorer.DefaultConfig())
			if got.Action != c.wantAction || got.TargetNode != c.wantTarget || !strings.Contains(got.Reason, c.wantReason) {
				t.Errorf("got %s to %q (%q), want %s to %q (%q)",
					got.Action, got.TargetNode, got.Reason, c.wantAction, c.wantTarget, c.wantReason)
			}
			if got.RequestID != "r1" || got.DecidedBy != placement.DecidedByRules {
				t.Errorf("requestId/decidedBy = %q/%q, want r1/rules", got.RequestID, got.DecidedBy)
			}
			isMove := got.Action == orchestrationv1alpha1.RebalanceActionMove
			if isMove && (got.PodName != "p1" || got.Improvement != evacuationImprovement) {
				t.Errorf("move = %+v, want p1 with improvement %d", got, evacuationImprovement)
			}
		})
	}
}

func TestCheckMustMove(t *testing.T) {
	down := stat("n1", "False", false)
	atRisk := stat("n2", "True", true)
	atRisk.Unschedulable = true
	noOp := placement.RebalanceDecisionResponse{Action: orchestrationv1alpha1.RebalanceActionNoOp}
	move := func(pod, target string) placement.RebalanceDecisionResponse {
		return placement.RebalanceDecisionResponse{
			Action: orchestrationv1alpha1.RebalanceActionMove, PodName: pod, TargetNode: target}
	}

	cases := []struct {
		name    string
		req     placement.DecisionRequest
		answer  placement.RebalanceDecisionResponse
		wantWhy string // part of the reason; "" means the answer may be sent
	}{
		{
			name:    "NoOp while a pod is on a down node",
			req:     request([]placement.NodeStat{down, sized("n2", 20)}, running("p1", "n1")),
			answer:  noOp,
			wantWhy: "pod p1 is on down node n1 and must move first",
		},
		{
			name: "moving another pod while one is on a down node",
			req: request([]placement.NodeStat{down, sized("n2", 20), sized("n3", 20)},
				running("p1", "n1"), running("p2", "n2")),
			answer:  move("p2", "n3"),
			wantWhy: "must move first",
		},
		{
			name:   "moving the pod off the down node",
			req:    request([]placement.NodeStat{down, sized("n2", 20)}, running("p1", "n1")),
			answer: move("p1", "n2"),
		},
		{
			name:   "NoOp when no healthy node can take the pod",
			req:    request([]placement.NodeStat{down, atRisk}, running("p1", "n1")),
			answer: noOp,
		},
		{
			name:   "NoOp when no pod is on a down node",
			req:    request([]placement.NodeStat{sized("n1", 50), sized("n2", 20)}, running("p1", "n1")),
			answer: noOp,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why := Assess(c.req, testNow, DefaultConfig()).Check(c.answer)
			if c.wantWhy == "" && why != "" {
				t.Errorf("got rejected (%q), want allowed", why)
			}
			if c.wantWhy != "" && !strings.Contains(why, c.wantWhy) {
				t.Errorf("got %q, want it to contain %q", why, c.wantWhy)
			}
		})
	}
}

func TestAnyChangeAllowed(t *testing.T) {
	healthy := []placement.NodeStat{sized("n1", 50), sized("n2", 20)}
	withDown := []placement.NodeStat{stat("n1", "False", false), sized("n2", 20)}
	cases := []struct {
		name     string
		nodes    []placement.NodeStat
		cooldown bool
		want     bool
	}{
		{name: "no cooldown", nodes: healthy, want: true},
		{name: "cooldown, nothing on a down node", nodes: healthy, cooldown: true, want: false},
		{name: "cooldown, a pod on a down node", nodes: withDown, cooldown: true, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := request(c.nodes, running("p1", "n1"))
			if c.cooldown {
				req.RebalanceContext.CooldownUntil = at(5 * time.Minute)
			}
			if got := Assess(req, testNow, DefaultConfig()).AnyChangeAllowed(); got != c.want {
				t.Errorf("AnyChangeAllowed() = %v, want %v", got, c.want)
			}
		})
	}
}
