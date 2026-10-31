package rebalance

import (
	"testing"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// stat builds a node with its Ready status ("" for none) and the candidate flag.
func stat(name, ready string, candidate bool) placement.NodeStat {
	n := placement.NodeStat{Name: name, Candidate: candidate}
	if ready != "" {
		n.Conditions = map[string]string{"Ready": ready}
	}
	return n
}

// running is a pod of the app on node.
func running(name, node string) placement.PodPlacement {
	return placement.PodPlacement{PodName: name, NodeName: node, Phase: "Running"}
}

// request builds a rebalance request with these nodes and pods.
func request(nodes []placement.NodeStat, pods ...placement.PodPlacement) placement.DecisionRequest {
	return placement.DecisionRequest{
		RequestID:        "r1",
		NodeStats:        nodes,
		RebalanceContext: &placement.RebalanceContext{CurrentPlacements: pods},
	}
}

func TestClassifyNode(t *testing.T) {
	cordoned := stat("n", "True", true)
	cordoned.Unschedulable = true
	pressure := stat("n", "True", true)
	pressure.Conditions["MemoryPressure"] = "True"
	calm := stat("n", "True", true)
	calm.Conditions["MemoryPressure"] = "False"

	cases := []struct {
		name string
		node placement.NodeStat
		want nodeClass
	}{
		{"no Ready condition", stat("n", "", true), classUnknown},
		{"Ready False", stat("n", "False", true), classDown},
		{"Ready Unknown", stat("n", "Unknown", true), classDown},
		{"cordoned", cordoned, classAtRisk},
		{"memory pressure", pressure, classAtRisk},
		{"risk condition False", calm, classHealthy},
		{"healthy", stat("n", "True", true), classHealthy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyNode(c.node, DefaultConfig()); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
