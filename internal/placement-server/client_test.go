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

package placementserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

func testRebalanceDecisionRequest() *DecisionRequest {
	return &DecisionRequest{
		RequestID: testDecisionID,
		Timestamp: metav1.Now(),
		AOProfile: &AOProfileContext{ProfileName: "profile-a", Strategy: "Balanced"},
		RebalanceContext: &RebalanceContext{
			Reason:            "CPUThreshold: node-a at 92%",
			DecisionID:        testDecisionID,
			CurrentPlacements: []PodPlacement{{PodName: "app-a-1", NodeName: "node-a", Phase: "Running"}},
		},
	}
}

func TestRequestRebalanceDecision_Success(t *testing.T) {
	var gotRequestIDHeader string
	var gotReq DecisionRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestIDHeader = r.Header.Get("X-Request-ID")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RebalanceDecisionResponse{
			RequestID:   testDecisionID,
			Action:      orchestrationv1alpha1.RebalanceActionMove,
			PodName:     "app-a-1",
			TargetNode:  "node-b",
			Improvement: 0.42,
			Reason:      "better spread",
		})
	}))
	defer server.Close()

	c := NewDecisionClient(server.URL, "", 2*time.Second)
	req := testRebalanceDecisionRequest()

	resp, err := c.RequestRebalanceDecision(context.Background(), req)
	if err != nil {
		t.Fatalf("RequestRebalanceDecision: %v", err)
	}

	if resp.Action != orchestrationv1alpha1.RebalanceActionMove || resp.PodName != "app-a-1" || resp.TargetNode != "node-b" || resp.Improvement != 0.42 {
		t.Errorf("resp = %+v, unexpected fields", resp)
	}
	if gotRequestIDHeader != testDecisionID {
		t.Errorf("X-Request-ID header = %q, want %s", gotRequestIDHeader, testDecisionID)
	}
	if gotReq.RebalanceContext == nil || gotReq.RebalanceContext.DecisionID != testDecisionID {
		t.Errorf("server received RebalanceContext = %+v, want DecisionID %s", gotReq.RebalanceContext, testDecisionID)
	}
}

func TestRequestRebalanceDecision_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewDecisionClient(server.URL, "", 2*time.Second)
	_, err := c.RequestRebalanceDecision(context.Background(), testRebalanceDecisionRequest())
	if err == nil {
		t.Fatal("expected an error for a non-200 response, got nil")
	}
}

func TestRequestRebalanceDecision_MalformedResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	c := NewDecisionClient(server.URL, "", 2*time.Second)
	_, err := c.RequestRebalanceDecision(context.Background(), testRebalanceDecisionRequest())
	if err == nil {
		t.Fatal("expected a decode error for a malformed response body, got nil")
	}
}

func TestRequestRebalanceDecision_ContextTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewDecisionClient(server.URL, "", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := c.RequestRebalanceDecision(ctx, testRebalanceDecisionRequest())
	if err == nil {
		t.Fatal("expected an error when the context deadline is exceeded, got nil")
	}
}

func testPlacementDecisionRequest() *DecisionRequest {
	return &DecisionRequest{
		RequestID: testDecisionID,
		Timestamp: metav1.Now(),
		Pod:       testAppPod("app-a-1", "", corev1.PodPending),
		AOProfile: &AOProfileContext{ProfileName: "profile-a", Strategy: "Balanced"},
	}
}

// slowAgent answers every call with an empty JSON body after delay.
func slowAgent(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRequestDecision_StopsAtPlacementTimeout(t *testing.T) {
	c := NewDecisionClient(slowAgent(t, 200*time.Millisecond).URL, "", 20*time.Millisecond)

	_, err := c.RequestDecision(context.Background(), testPlacementDecisionRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestRequestRebalanceDecision_OutlastsPlacementTimeout(t *testing.T) {
	c := NewDecisionClient(slowAgent(t, 100*time.Millisecond).URL, "", 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := c.RequestRebalanceDecision(ctx, testRebalanceDecisionRequest()); err != nil {
		t.Fatalf("RequestRebalanceDecision: %v, want success past the placement timeout", err)
	}
}
