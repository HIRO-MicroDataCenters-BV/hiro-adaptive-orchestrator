package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

const (
	InitialPlacementBody = `{"requestId":"r0", "nodeStats": [{"name":"node1"}, {"name":"node2"}]}`
	InitialRebalanceBody = `{"requestId":"r2", "nodeStats": [{"name":"node1"}, {"name":"node2"}], "rebalanceContext":{"reason":"test"}}`
)

// do sends one request through the service's routes and returns the recorded response.
func do(t *testing.T, cfg Config, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	srv := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestHealth(t *testing.T) {
	rec := do(t, DefaultConfig(), http.MethodGet, DefaultHealthPath, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "OK" {
		t.Errorf("expected status %d and body %q, got %d and body %q", http.StatusOK, "OK", rec.Code, rec.Body.String())
	}
}

func TestPlacementGivesEqualScores(t *testing.T) {
	rec := do(t, DefaultConfig(), http.MethodPost, DefaultDecisionPath, InitialPlacementBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var resp placement.DecisionResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.RequestID != "r0" || len(resp.NodeScores) != 2 || resp.DecidedBy != placement.DecidedByScorer {
		t.Errorf("expected request ID 'r0', 2 node scores, and decided by scorer, got response: %+v", resp)
	}
}

func TestRebalanceAnswersNoOp(t *testing.T) {
	rec := do(t, DefaultConfig(), http.MethodPost, DefaultDecisionPath, InitialRebalanceBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var resp placement.RebalanceDecisionResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.RequestID != "r2" || resp.Action != orchestrationv1alpha1.RebalanceActionNoOp || resp.DecidedBy != placement.DecidedByRules {
		t.Errorf("expected request ID 'r2', action 'NoOp', and decided by rules, got response: %+v", resp)
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
			rec := do(t, c.config, c.method, DefaultDecisionPath, c.body)
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
