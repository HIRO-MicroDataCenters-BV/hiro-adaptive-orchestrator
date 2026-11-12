package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/ai-decision-service/model"
	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

const (
	InitialPlacementBody = `{"requestId":"r0",
		"nodeStats": [{"name":"node1", "candidate": true}, {"name":"node2", "candidate": true}]}`
	InitialRebalanceBody = `{"requestId":"r2",
		"nodeStats": [{"name":"node1", "candidate": false, "conditions": {"Ready":"False"}},
			{"name":"node2", "candidate": true, "conditions": {"Ready":"True"}}],
		"rebalanceContext": {"reason":"test",
			"currentPlacements": [{"podName":"p1", "nodeName":"node1", "phase":"Running"}]}}`
	stubModelReason = "stub model: node2 best"
)

// stubDecider stands in for a model: it returns answer and err for every placement.
type stubDecider struct {
	answer placement.DecisionResponse
	err    error
}

func (d stubDecider) DecidePlacement(context.Context,
	placement.DecisionRequest) (placement.DecisionResponse, error) {
	return d.answer, d.err
}

// do sends one request through the service's routes and returns the recorded response; a nil decider means model off.
func do(t *testing.T, cfg Config, placementDecider model.PlacementDecider,
	method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	srv := New(cfg, placementDecider, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestHealth(t *testing.T) {
	rec := do(t, DefaultConfig(), nil, http.MethodGet, DefaultHealthPath, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
		t.Errorf("expected status %d and body %q, got %d and body %q", http.StatusOK, "OK", rec.Code, rec.Body.String())
	}
}

func TestPlacementUsesScorer(t *testing.T) {
	rec := do(t, DefaultConfig(), nil, http.MethodPost, DefaultDecisionPath, InitialPlacementBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var resp placement.DecisionResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.RequestID != "r0" ||
		len(resp.NodeScores) != 2 ||
		resp.DecidedBy != placement.DecidedByScorer ||
		!strings.HasPrefix(resp.Reason, "Balanced") {
		t.Errorf("expected request ID 'r0', 2 node scores, and decided by scorer, got response: %+v", resp)
	}
}

func TestPlacementUsesModel(t *testing.T) {
	cases := []struct {
		name          string
		modelScores   []placement.NodeScore
		modelErr      error
		wantDecidedBy string
	}{
		{
			name:          "good answer",
			modelScores:   []placement.NodeScore{{NodeName: "node1", Score: 30}, {NodeName: "node2", Score: 70}},
			wantDecidedBy: placement.DecidedByModel,
		},
		{
			name:          "model error",
			modelErr:      errors.New("model unreachable"),
			wantDecidedBy: placement.DecidedByFallback,
		},
		{
			name: "non-candidate scored",
			modelScores: []placement.NodeScore{
				{NodeName: "node1", Score: 30}, {NodeName: "node2", Score: 70}, {NodeName: "node3", Score: 50},
			},
			wantDecidedBy: placement.DecidedByFallback,
		},
		{
			name:          "candidate left unscored",
			modelScores:   []placement.NodeScore{{NodeName: "node1", Score: 30}},
			wantDecidedBy: placement.DecidedByFallback,
		},
		{
			name: "scored twice",
			modelScores: []placement.NodeScore{
				{NodeName: "node1", Score: 30}, {NodeName: "node1", Score: 30}, {NodeName: "node2", Score: 70},
			},
			wantDecidedBy: placement.DecidedByFallback,
		},
		{
			name:          "score out of range",
			modelScores:   []placement.NodeScore{{NodeName: "node1", Score: 30}, {NodeName: "node2", Score: 150}},
			wantDecidedBy: placement.DecidedByFallback,
		},
		{
			// JSON can't encode NaN, so a missed check would also break the response.
			name:          "NaN score",
			modelScores:   []placement.NodeScore{{NodeName: "node1", Score: 30}, {NodeName: "node2", Score: math.NaN()}},
			wantDecidedBy: placement.DecidedByFallback,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decider := stubDecider{
				answer: placement.DecisionResponse{NodeScores: c.modelScores, Reason: stubModelReason},
				err:    c.modelErr,
			}
			rec := do(t, DefaultConfig(), decider, http.MethodPost, DefaultDecisionPath, InitialPlacementBody)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
			}
			var resp placement.DecisionResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}
			// The stub never sets these, so the server must.
			if resp.RequestID != "r0" || resp.DecidedBy != c.wantDecidedBy {
				t.Errorf("expected request ID 'r0' and decided by %s, got response: %+v", c.wantDecidedBy, resp)
			}
			if c.wantDecidedBy == placement.DecidedByModel {
				if !slices.Equal(resp.NodeScores, c.modelScores) || resp.Reason != stubModelReason {
					t.Errorf("expected the model's scores %v and reason, got response: %+v", c.modelScores, resp)
				}
			} else if len(resp.NodeScores) != 2 || !strings.HasPrefix(resp.Reason, "Balanced") {
				t.Errorf("expected the scorer's answer, got response: %+v", resp)
			}
		})
	}
}

func TestRebalanceUsesRules(t *testing.T) {
	rec := do(t, DefaultConfig(), nil, http.MethodPost, DefaultDecisionPath, InitialRebalanceBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var resp placement.RebalanceDecisionResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.RequestID != "r2" || resp.Action != orchestrationv1alpha1.RebalanceActionMove ||
		resp.PodName != "p1" || resp.TargetNode != "node2" || resp.DecidedBy != placement.DecidedByRules {
		t.Errorf("expected r2 to move p1 off down node1 to node2, decided by rules, got response: %+v", resp)
	}
}

func TestRejectsBadRequests(t *testing.T) {
	small := DefaultConfig()
	small.MaxBodyBytes = 10
	cases := []struct {
		name           string
		config         Config
		method         string
		body           string
		expectedStatus int
	}{
		{"bad json", DefaultConfig(), http.MethodPost, "not json", http.StatusBadRequest},
		{"too large body", small, http.MethodPost, `{"requestId":"r3", "nodeStats": []}`, http.StatusBadRequest},
		{"empty body", DefaultConfig(), http.MethodPost, "", http.StatusBadRequest},
		{"wrong method", DefaultConfig(), http.MethodGet, "", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, c.config, nil, c.method, DefaultDecisionPath, c.body)
			if rec.Code != c.expectedStatus {
				t.Fatalf("expected status %d, got %d", c.expectedStatus, rec.Code)
			}
		})
	}
}

func TestInputHash(t *testing.T) {
	a := placement.DecisionRequest{
		RequestID: "r4",
		NodeStats: []placement.NodeStat{{Name: "node1"}},
	}
	b := a
	b.RequestID = "r9"
	b.Timestamp = metav1.Now()
	if inputHash(a) != inputHash(b) {
		t.Errorf("expected input hashes to be equal, got %s and %s", inputHash(a), inputHash(b))
	}
	c := a
	c.NodeStats = []placement.NodeStat{{Name: "node2"}}
	if inputHash(a) == inputHash(c) {
		t.Errorf("expected input hashes to be different, got %s and %s", inputHash(a), inputHash(c))
	}
}
