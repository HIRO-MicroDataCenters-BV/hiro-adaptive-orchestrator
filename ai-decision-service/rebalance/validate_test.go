package rebalance

import (
	"testing"
	"time"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// validateRequest has healthy n1 and n2, at-risk n3, non-candidate n4; p1 runs on n1, p2 is Pending.
func validateRequest() placement.DecisionRequest {
	atRisk := stat("n3", "True", true)
	atRisk.Unschedulable = true
	p1 := running("p1", "n1")
	p1.Containers = []placement.ContainerInfo{{Name: "app"}}
	req := request(
		[]placement.NodeStat{stat("n1", "True", true), stat("n2", "True", true), atRisk, stat("n4", "True", false)},
		p1, placement.PodPlacement{PodName: "p2", Phase: "Pending"},
	)
	req.RebalanceContext.Workload = &placement.RebalanceWorkload{Replicas: 3, MinReplicas: 1, MaxReplicas: 5}
	return req
}

func TestValidate(t *testing.T) {
	move := orchestrationv1alpha1.RebalanceActionMove
	replicas := orchestrationv1alpha1.RebalanceActionAdjustReplicas
	resources := orchestrationv1alpha1.RebalanceActionAdjustResources
	type resp = placement.RebalanceDecisionResponse
	mv := func(pod, target string) resp { return resp{Action: move, PodName: pod, TargetNode: target} }
	hold := eao(placement.EAOVerdictHold, time.Minute, nil)
	underHold := func(r *placement.DecisionRequest) { r.EAOProfile = hold }

	cases := []struct {
		name  string
		resp  resp
		setup func(req *placement.DecisionRequest)
		want  string // "" means valid, else part of the reason
	}{
		{name: "NoOp", resp: resp{Action: orchestrationv1alpha1.RebalanceActionNoOp}},
		{name: "Defer", resp: resp{Action: orchestrationv1alpha1.RebalanceActionDefer}},
		{name: "Reject", resp: resp{Action: orchestrationv1alpha1.RebalanceActionReject}},
		{name: "Escalate", resp: resp{Action: orchestrationv1alpha1.RebalanceActionEscalate}},
		{name: "unknown action", resp: resp{Action: "Teleport"}, want: `unknown action "Teleport"`},

		{name: "move", resp: mv("p1", "n2")},
		{name: "move unknown pod", resp: mv("p9", "n2"), want: `pod "p9" is not in the request`},
		{name: "move pending pod", resp: mv("p2", "n2"), want: `pod "p2" is not scheduled`},
		{name: "move to same node", resp: mv("p1", "n1"), want: `pod "p1" is already on n1`},
		{name: "move without target", resp: mv("p1", ""), want: `target "" is not a candidate`},
		{name: "move to non-candidate", resp: mv("p1", "n4"), want: `target "n4" is not a candidate`},
		{name: "move to at-risk node", resp: mv("p1", "n3"), want: `target "n3" is not healthy`},
		{
			name:  "move blocked by a gate",
			resp:  mv("p1", "n2"),
			setup: func(r *placement.DecisionRequest) { r.RebalanceContext.PDB = &placement.RebalancePDB{Name: "web"} },
			want:  "PDB web does not allow disruptions",
		},
		{
			name: "move off a down node skips the gates",
			resp: mv("p1", "n2"),
			setup: func(r *placement.DecisionRequest) {
				r.RebalanceContext.CooldownUntil = at(5 * time.Minute)
				r.EAOProfile = hold
				r.NodeStats[0] = stat("n1", "False", true)
			},
		},
		{name: "move under an EAO hold", resp: mv("p1", "n2"), setup: underHold, want: "EAO holds"},

		{name: "replicas", resp: resp{Action: replicas, TargetReplicas: 4}},
		{
			name:  "scale up under an EAO hold",
			resp:  resp{Action: replicas, TargetReplicas: 4},
			setup: underHold,
			want:  "EAO holds",
		},
		{name: "scale down under an EAO hold", resp: resp{Action: replicas, TargetReplicas: 2}, setup: underHold},
		{
			name:  "replicas managed",
			resp:  resp{Action: replicas, TargetReplicas: 4},
			setup: managed(true, false),
			want:  "replicas are externally managed",
		},
		{
			name:  "replicas without workload",
			resp:  resp{Action: replicas, TargetReplicas: 4},
			setup: func(r *placement.DecisionRequest) { r.RebalanceContext.Workload = nil },
			want:  "no workload in the request",
		},
		{name: "replicas below min", resp: resp{Action: replicas, TargetReplicas: 0}, want: "0 replicas is outside 1..5"},
		{name: "replicas above max", resp: resp{Action: replicas, TargetReplicas: 6}, want: "6 replicas is outside 1..5"},
		{name: "replicas unchanged", resp: resp{Action: replicas, TargetReplicas: 3}, want: "already at 3 replicas"},

		{name: "resources cpu", resp: resp{Action: resources, ContainerName: "app", TargetCPU: "500m"}},
		{name: "resources memory", resp: resp{Action: resources, ContainerName: "app", TargetMemory: "256Mi"}},
		{
			name:  "resources under an EAO hold",
			resp:  resp{Action: resources, ContainerName: "app", TargetCPU: "500m"},
			setup: underHold,
			want:  "EAO holds",
		},
		{
			name:  "resources managed",
			resp:  resp{Action: resources, ContainerName: "app", TargetCPU: "500m"},
			setup: managed(false, true),
			want:  "resources are externally managed",
		},
		{
			name: "resources unknown container",
			resp: resp{Action: resources, ContainerName: "sidecar", TargetCPU: "500m"},
			want: `container "sidecar" is not in the request`,
		},
		{name: "resources no target", resp: resp{Action: resources, ContainerName: "app"}, want: "no CPU or memory target"},
		{
			name: "resources bad cpu",
			resp: resp{Action: resources, ContainerName: "app", TargetCPU: "lots"},
			want: `cpu "lots" is not a positive quantity`,
		},
		{
			name: "resources zero memory",
			resp: resp{Action: resources, ContainerName: "app", TargetMemory: "0"},
			want: `memory "0" is not a positive quantity`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validateRequest()
			if c.setup != nil {
				c.setup(&req)
			}
			cfg := DefaultConfig()
			classes := classifyNodes(req, cfg)
			if got := validate(c.resp, req, applyGates(req, testNow, cfg, classes), classes); !matches(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
