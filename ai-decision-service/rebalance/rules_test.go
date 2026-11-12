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

func TestDecide(t *testing.T) {
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
			wantReason: "blocked: Move: cooldown until",
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
			got := Decide(req, testNow, DefaultConfig(), scorer.DefaultConfig())
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
