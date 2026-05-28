/*
Copyright 2026 HIRO Adaptive Orchestrator.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package schedulerplugin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	schedulerplugin "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/scheduler-plugin"
)

func testPlacementCtx() *placement.PlacementContext {
	return &placement.PlacementContext{
		Pod: &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		},
		CandidateNodes: []*corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
		},
	}
}

func testDecisionResponse() placement.DecisionResponse {
	return placement.DecisionResponse{
		RequestID: "test-req-id",
		NodeScores: []placement.NodeScore{
			{NodeName: "node-a", Score: 90},
			{NodeName: "node-b", Score: 60},
		},
		Reason: "test reason",
	}
}

func TestDecide_HappyPath(t *testing.T) {
	want := testDecisionResponse()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}
		if r.Header.Get("X-Request-ID") == "" {
			t.Error("expected X-Request-ID header to be set")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	client := schedulerplugin.NewPlacementClient(srv.URL, "/placement", 5*time.Second)
	got, err := client.Decide(context.Background(), testPlacementCtx())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RequestID != want.RequestID {
		t.Errorf("RequestID: want %q, got %q", want.RequestID, got.RequestID)
	}
	if len(got.NodeScores) != len(want.NodeScores) {
		t.Fatalf("NodeScores length: want %d, got %d", len(want.NodeScores), len(got.NodeScores))
	}
	for i, ns := range want.NodeScores {
		if got.NodeScores[i].NodeName != ns.NodeName || got.NodeScores[i].Score != ns.Score {
			t.Errorf("NodeScores[%d]: want %+v, got %+v", i, ns, got.NodeScores[i])
		}
	}
}

func TestDecide_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := schedulerplugin.NewPlacementClient(srv.URL, "/placement", 5*time.Second)
	_, err := client.Decide(context.Background(), testPlacementCtx())
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

func TestDecide_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		http.Error(w, "slow response", http.StatusOK)
	}))
	defer srv.Close()

	client := schedulerplugin.NewPlacementClient(srv.URL, "/placement", 50*time.Millisecond)
	_, err := client.Decide(context.Background(), testPlacementCtx())
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestDecide_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not valid json {{{"))
	}))
	defer srv.Close()

	client := schedulerplugin.NewPlacementClient(srv.URL, "/placement", 5*time.Second)
	_, err := client.Decide(context.Background(), testPlacementCtx())
	if err == nil {
		t.Fatal("expected JSON decode error, got nil")
	}
}

func TestDecide_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := schedulerplugin.NewPlacementClient(srv.URL, "/placement", 5*time.Second)
	_, err := client.Decide(ctx, testPlacementCtx())
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestNewPlacementClientFromEnv_Defaults(t *testing.T) {
	os.Unsetenv("PLACEMENT_SERVER_URL")
	os.Unsetenv("PLACEMENT_SERVER_PATH")

	client := schedulerplugin.NewPlacementClientFromEnv(5 * time.Second)
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}
