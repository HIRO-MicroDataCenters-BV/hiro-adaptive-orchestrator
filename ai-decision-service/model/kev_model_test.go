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

func TestBuildKevRequest(t *testing.T) {
	knownNode := nodeStat("sample-worker", 4000, 200, 8<<30, 1<<30, 1, true)
	knownNodeFacts := "cpu 5.0% requested, memory 12.5% requested, 1 pods of this app"
	unknownNode := placement.NodeStat{Name: "sample-worker2", Candidate: true}
	unknownNodeFacts := "usage unknown, unknown pods of this app"
	nonCandidateNode := placement.NodeStat{Name: "sample-worker3", Candidate: false}

	cases := []struct {
		name             string
		req              placement.DecisionRequest
		wantState        string
		wantInstructions string
		wantCriteria     map[string]string // exact, so an extra node fails too
	}{
		{
			name: "spread with known facts",
			req: placement.DecisionRequest{
				AOProfile: &placement.AOProfileContext{Strategy: "Spread"},
				Pod:       podRequesting("50m", "64Mi"),
				NodeStats: []placement.NodeStat{knownNode},
			},
			wantState:        "Placing a pod that requests 50m CPU and 64Mi memory. Strategy: Spread.",
			wantInstructions: "Pick the node with the fewest pods of this app, even if it is busier.",
			wantCriteria: map[string]string{
				"sample-worker": knownNodeFacts,
			},
		},
		{
			name: "spread with unknown facts",
			req: placement.DecisionRequest{
				AOProfile: &placement.AOProfileContext{Strategy: "Spread"},
				Pod:       podRequesting("50m", "64Mi"),
				NodeStats: []placement.NodeStat{knownNode, unknownNode},
			},
			wantState:        "Placing a pod that requests 50m CPU and 64Mi memory. Strategy: Spread.",
			wantInstructions: "Pick the node with the fewest pods of this app, even if it is busier.",
			wantCriteria: map[string]string{
				"sample-worker":  knownNodeFacts,
				"sample-worker2": unknownNodeFacts,
			},
		},
		{
			name: "Non candidate node",
			req: placement.DecisionRequest{
				AOProfile: &placement.AOProfileContext{Strategy: "Spread"},
				Pod:       podRequesting("50m", "64Mi"),
				NodeStats: []placement.NodeStat{knownNode, nonCandidateNode},
			},
			wantState:        "Placing a pod that requests 50m CPU and 64Mi memory. Strategy: Spread.",
			wantInstructions: "Pick the node with the fewest pods of this app, even if it is busier.",
			wantCriteria:     map[string]string{"sample-worker": knownNodeFacts},
		},
		{
			name: "Nil Profile",
			req: placement.DecisionRequest{
				AOProfile: nil,
				Pod:       podRequesting("1", "1Gi"),
				NodeStats: []placement.NodeStat{knownNode},
			},
			wantState:        "Placing a pod that requests 1000m CPU and 1024Mi memory. Strategy: Balanced.",
			wantInstructions: "Pick the node with the lowest CPU and memory usage, whatever its pod count.",
			wantCriteria:     map[string]string{"sample-worker": knownNodeFacts},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildKevRequest(c.req, "kev-latest")
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
	}, discardLog)
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
