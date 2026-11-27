package model

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/scorer"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// nodeStat builds a node with CPU and memory allocatable and requested, and the app's pod count.
func nodeStat(name string, cpuAlloc, cpuReq, memAlloc, memReq int64, appPods int, candidate bool) placement.NodeStat {
	return placement.NodeStat{
		Name:      name,
		Candidate: candidate,
		AppPods:   &appPods,
		Resources: map[string]placement.NodeResource{
			"cpu":    {Allocatable: cpuAlloc, Requested: &cpuReq},
			"memory": {Allocatable: memAlloc, Requested: &memReq},
		},
	}
}

// podRequesting builds a pod with one container requesting cpu and memory.
func podRequesting(cpu, memory string) *corev1.Pod {
	pod := &corev1.Pod{}
	pod.Spec.Containers = []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}},
	}}
	return pod
}

// inZone puts the node in zone, under the scorer's default zone label.
func inZone(n placement.NodeStat, zone string) placement.NodeStat {
	n.Labels = map[string]string{scorer.DefaultZoneLabel: zone}
	return n
}

func TestBuildKevRequest(t *testing.T) {
	const (
		smallPod = "Choosing a node for a new pod of this app, which requests 50m CPU and 64Mi memory. "
		ask      = "Which node is the best place for this pod? "
		balanced = "Strategy: Balanced. The app is latency sensitive, so no node should become a hotspot on any resource."
		packed   = "Strategy: Packed. To save energy, fill nodes that are already in use so other nodes can stay empty, " +
			"but no node may go over 85% on any resource."
		spread = "Strategy: Spread. For high availability, keep this app's pods apart, so that losing one node, " +
			"or one zone, takes down as few of them as possible."
		spreadRule = "Under Spread, fewer of this app's pods in the node's zone is better; if that is the same, " +
			"fewer of this app's pods on the node is better; only if both are the same, the less busy node is better."
		// a 50m / 64Mi pod on worker: cpu (200+50)/4000 = 6%, memory (1Gi+64Mi)/8Gi = 13%
		smallPodUsage = "its busiest resource, memory, would be at 13%; cpu would be at 6% and memory at 13%"
	)
	worker := nodeStat("sample-worker", 4000, 200, 8<<30, 1<<30, 1, true)
	unknown := placement.NodeStat{Name: "sample-worker2", Candidate: true}
	nonCandidate := nodeStat("sample-worker3", 4000, 200, 8<<30, 1<<30, 2, false)
	full := nodeStat("full", 4000, 3500, 8<<30, 1<<30, 0, true)    // a 50m pod takes cpu to 89%: over the Packed limit
	noFit := nodeStat("no-fit", 4000, 3990, 8<<30, 1<<30, 0, true) // a 50m pod needs 4040m of 4000m

	cases := []struct {
		name             string
		req              placement.DecisionRequest
		wantState        string
		wantInstructions string
		wantCriteria     map[string]string // exact, so an extra node fails too
	}{
		{
			name: "balanced by default, pod counted",
			req:  placement.DecisionRequest{Pod: podRequesting("1", "1Gi"), NodeStats: []placement.NodeStat{worker}},
			wantState: "Choosing a node for a new pod of this app, which requests 1000m CPU and 1024Mi memory. " +
				balanced,
			wantInstructions: ask + "Under Balanced, the node with lower CPU and memory use is the better place.",
			wantCriteria:     map[string]string{"sample-worker": "with this pod placed, cpu would be at 30% and memory at 25%"},
		},
		{
			name: "packed leads with the average",
			req: placement.DecisionRequest{AOProfile: &placement.AOProfileContext{Strategy: "Packed"},
				Pod: podRequesting("50m", "64Mi"), NodeStats: []placement.NodeStat{worker}},
			wantState:        smallPod + packed,
			wantInstructions: ask + "Under Packed, the fuller node is the better place.",
			wantCriteria: map[string]string{
				"sample-worker": "with this pod placed, it would be 10% full on average; cpu would be at 6% and memory at 13%"},
		},
		{
			name: "spread without zones skips non-candidates and marks unknown usage",
			req: placement.DecisionRequest{AOProfile: &placement.AOProfileContext{Strategy: "Spread"},
				Pod: podRequesting("50m", "64Mi"), NodeStats: []placement.NodeStat{worker, unknown, nonCandidate}},
			wantState:        smallPod + spread,
			wantInstructions: ask + spreadRule,
			wantCriteria: map[string]string{
				"sample-worker":  "this app already runs 1 pod on this node; with this pod placed, " + smallPodUsage,
				"sample-worker2": "usage unknown",
			},
		},
		{
			name: "spread counts zone pods on every node, candidate or not",
			req: placement.DecisionRequest{AOProfile: &placement.AOProfileContext{Strategy: "Spread"},
				Pod: podRequesting("50m", "64Mi"), NodeStats: []placement.NodeStat{
					inZone(nodeStat("w1", 4000, 200, 8<<30, 1<<30, 0, true), "a"),
					inZone(nodeStat("w2", 4000, 200, 8<<30, 1<<30, 1, true), "b"),
					inZone(nonCandidate, "a"),
				}},
			wantState:        smallPod + spread,
			wantInstructions: ask + spreadRule,
			wantCriteria: map[string]string{
				"w1": "this app already runs 2 pods in this node's zone, a, and 0 pods on this node; " +
					"with this pod placed, " + smallPodUsage,
				"w2": "this app already runs 1 pod in this node's zone, b, and 1 pod on this node; " +
					"with this pod placed, " + smallPodUsage,
			},
		},
		{
			name: "packed leaves out a node over the limit",
			req: placement.DecisionRequest{AOProfile: &placement.AOProfileContext{Strategy: "Packed"},
				Pod: podRequesting("50m", "64Mi"), NodeStats: []placement.NodeStat{worker, full}},
			wantState:        smallPod + packed,
			wantInstructions: ask + "Under Packed, the fuller node is the better place.",
			wantCriteria: map[string]string{
				"sample-worker": "with this pod placed, it would be 10% full on average; cpu would be at 6% and memory at 13%"},
		},
		{
			name: "balanced leaves out a node the pod does not fit on, but keeps one over the packed limit",
			req: placement.DecisionRequest{Pod: podRequesting("50m", "64Mi"),
				NodeStats: []placement.NodeStat{full, noFit}},
			wantState:        smallPod + balanced,
			wantInstructions: ask + "Under Balanced, the node with lower CPU and memory use is the better place.",
			wantCriteria:     map[string]string{"full": "with this pod placed, cpu would be at 89% and memory at 13%"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildKevRequest(c.req, "kev-latest", scorer.DefaultConfig())
			question, found := got.Questions[kevQuestionID]
			if !found {
				t.Fatalf("no question under %q", kevQuestionID)
			}
			if question.Type != kevQuestionTypeChoice {
				t.Errorf("got question type %q, want %q", question.Type, kevQuestionTypeChoice)
			}
			if got.Model != "kev-latest" {
				t.Errorf("got model %q, want %q", got.Model, "kev-latest")
			}
			if got.State != c.wantState {
				t.Errorf("got state %q, want %q", got.State, c.wantState)
			}
			if question.Instructions != c.wantInstructions {
				t.Errorf("got instruction %q, want %q", question.Instructions, c.wantInstructions)
			}
			if !maps.Equal(question.Criteria, c.wantCriteria) {
				t.Errorf("got criteria %v, want %v", question.Criteria, c.wantCriteria)
			}
		})
	}
}

const (
	kevTestPath    = "/v1/systemone"
	kevTestModel   = "kev-latest"
	kevTestTimeout = time.Second
)

// fakeKev starts a stand-in for Kev that checks the call, waits delay, then replies with status and body.
func fakeKev(t *testing.T, status int, body string, delay time.Duration) *httptest.Server {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Checked here rather than after the call, so the race detector sees no shared variables.
		if r.Method != http.MethodPost || r.URL.Path != kevTestPath {
			t.Errorf("got %s %s, want POST %s", r.Method, r.URL.Path, kevTestPath)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
			t.Errorf("got Content-Type %q, want application/json", contentType)
		}
		var sent kevRequest
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("could not decode the request sent to Kev: %v", err)
		}
		if sent.Model != kevTestModel {
			t.Errorf("got model %q, want %q", sent.Model, kevTestModel)
		}
		time.Sleep(delay)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // the client may already be gone after a timeout
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// kevTestDecider returns a KevDecider that calls baseURL and gives up after timeout.
func kevTestDecider(baseURL string, timeout time.Duration) *KevDecider {
	return NewKevDecider(Config{
		Provider:  ProviderKev,
		BaseURL:   baseURL,
		Path:      kevTestPath,
		ModelName: kevTestModel,
		Timeout:   timeout,
	}, scorer.DefaultConfig(), discardLog)
}

// kevTestRequest builds a Spread request for a small pod over nodes.
func kevTestRequest(nodes ...placement.NodeStat) placement.DecisionRequest {
	return placement.DecisionRequest{
		RequestID: "test-pod-uid",
		AOProfile: &placement.AOProfileContext{Strategy: "Spread"},
		Pod:       podRequesting("50m", "64Mi"),
		NodeStats: nodes,
	}
}

func TestDecidePlacement(t *testing.T) {
	reply := `{"answers":{"placement":{"choice":"sample-worker2","confidence":0.77,` +
		`"probabilities":{"sample-worker":0.25,"sample-worker2":0.75}}}}`
	srv := fakeKev(t, http.StatusOK, reply, 0)
	req := kevTestRequest(
		nodeStat("sample-worker", 4000, 200, 8<<30, 1<<30, 1, true),
		nodeStat("sample-worker2", 8000, 1000, 16<<30, 2<<30, 0, true),
	)

	got, err := kevTestDecider(srv.URL, kevTestTimeout).DecidePlacement(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantScores := []placement.NodeScore{
		{NodeName: "sample-worker", Score: 25},
		{NodeName: "sample-worker2", Score: 75},
	}
	if !slices.Equal(got.NodeScores, wantScores) {
		t.Errorf("got scores %v, want %v", got.NodeScores, wantScores)
	}
	if got.Confidence != 0.77 {
		t.Errorf("got confidence %v, want 0.77", got.Confidence)
	}
	if wantReason := "Spread: sample-worker2 best (probability 0.75)"; got.Reason != wantReason {
		t.Errorf("got reason %q, want %q", got.Reason, wantReason)
	}
}

func TestDecidePlacementScoresNodesNotAllowedZero(t *testing.T) {
	// Kev is asked only about sample-worker, so its reply has no probability for no-fit.
	reply := `{"answers":{"placement":{"choice":"sample-worker","confidence":0.9,` +
		`"probabilities":{"sample-worker":1}}}}`
	srv := fakeKev(t, http.StatusOK, reply, 0)
	req := kevTestRequest(
		nodeStat("sample-worker", 4000, 200, 8<<30, 1<<30, 1, true),
		nodeStat("no-fit", 4000, 3990, 8<<30, 1<<30, 0, true), // a 50m pod needs 4040m of 4000m
	)

	got, err := kevTestDecider(srv.URL, kevTestTimeout).DecidePlacement(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantScores := []placement.NodeScore{
		{NodeName: "sample-worker", Score: 100},
		{NodeName: "no-fit", Score: 0},
	}
	if !slices.Equal(got.NodeScores, wantScores) {
		t.Errorf("got scores %v, want %v", got.NodeScores, wantScores)
	}
}

func TestDecidePlacementErrors(t *testing.T) {
	worker := nodeStat("sample-worker", 4000, 200, 8<<30, 1<<30, 1, true)
	worker2 := nodeStat("sample-worker2", 8000, 1000, 16<<30, 2<<30, 0, true)
	nonCandidate := nodeStat("sample-worker3", 4000, 0, 8<<30, 0, 0, false)
	// A good reply, so the down, slow and no-candidate cases can only fail for their own reason.
	validReply := `{"answers":{"placement":{"choice":"sample-worker","confidence":0.9,` +
		`"probabilities":{"sample-worker":0.6,"sample-worker2":0.4}}}}`
	missingProbability := `{"answers":{"placement":{"choice":"sample-worker","confidence":0.9,` +
		`"probabilities":{"sample-worker":1}}}}`

	cases := []struct {
		name    string
		req     placement.DecisionRequest
		status  int
		body    string
		delay   time.Duration
		timeout time.Duration // zero means kevTestTimeout
		kevDown bool
	}{
		{
			name:    "Kev is down",
			req:     kevTestRequest(worker, worker2),
			status:  http.StatusOK,
			body:    validReply,
			kevDown: true,
		},
		{
			name:   "HTTP 500",
			req:    kevTestRequest(worker, worker2),
			status: http.StatusInternalServerError,
			body:   "internal error",
		},
		{
			name:   "invalid JSON",
			req:    kevTestRequest(worker, worker2),
			status: http.StatusOK,
			body:   "not json",
		},
		{
			name:   "no answer",
			req:    kevTestRequest(worker, worker2),
			status: http.StatusOK,
			body:   `{"answers":{}}`,
		},
		{
			name:   "missing probability",
			req:    kevTestRequest(worker, worker2),
			status: http.StatusOK,
			body:   missingProbability,
		},
		{
			name:    "too slow",
			req:     kevTestRequest(worker, worker2),
			status:  http.StatusOK,
			body:    validReply,
			delay:   200 * time.Millisecond,
			timeout: 50 * time.Millisecond,
		},
		{
			name:   "no candidates",
			req:    kevTestRequest(nonCandidate),
			status: http.StatusOK,
			body:   validReply,
		},
		{
			name:   "no allowed candidates",
			req:    kevTestRequest(nodeStat("no-fit", 4000, 3990, 8<<30, 1<<30, 0, true)), // 50m needs 4040m of 4000m
			status: http.StatusOK,
			body:   validReply,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := fakeKev(t, c.status, c.body, c.delay)
			if c.kevDown {
				srv.Close()
			}
			decider := kevTestDecider(srv.URL, cmp.Or(c.timeout, kevTestTimeout))
			_, err := decider.DecidePlacement(context.Background(), c.req)
			if err == nil {
				t.Fatal("got no error, want one")
			}
			if !strings.Contains(err.Error(), c.req.RequestID) {
				t.Errorf("error %q does not name requestId %q", err, c.req.RequestID)
			}
		})
	}
}
