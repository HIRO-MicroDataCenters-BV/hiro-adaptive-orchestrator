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

package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// =============================================================================
// DecisionClient
//
// Triggered by: DecisionContextBuilder returning a completed DecisionRequest
// Input:        DecisionRequest  (assembled per pod by DecisionContextBuilder)
// Output:       DecisionResponse (node scores returned by External AI Agent)
//
// Flow (steps 5/D → 6/E in architecture):
//   1. Receive DecisionRequest from DecisionContextBuilder
//   2. POST to External Decision/AI Agent endpoint
//   3. Receive NodeScores in response
//   4. Return DecisionResponse to caller (Rebalance Component or scheduler plugin)
//      which translates it into a ValidPlacementDecision (step 7)
// =============================================================================

const (
	// decisionEndpoint is the path on the External AI Agent that receives
	// per-pod DecisionRequests.
	decisionEndpoint = "/api/v1/placement/decision"
)

// DecisionClient sends a per-pod DecisionRequest to the External Decision/AI
// Agent and returns the scored node list.
type DecisionClient struct {
	// agentURL is the base URL of the External Decision/AI Agent.
	// Example: "http://ai-decision-agent.hiro-system.svc.cluster.local:8080"
	agentURL string

	// httpClient is the underlying HTTP client.
	// Timeout is set at construction — it applies per-request.
	httpClient *http.Client
}

// NewDecisionClient creates a new DecisionClient.
//
// agentURL: base URL of the External Decision/AI Agent.
// timeout:  per-request timeout. Recommended: 5–30s depending on AI agent SLA.
//
// Example:
//
//	client := decision.NewDecisionClient(
//	    os.Getenv("DECISION_AGENT_URL"),
//	    15 * time.Second,
//	)
func NewDecisionClient(agentURL string, timeout time.Duration) *DecisionClient {
	return &DecisionClient{
		agentURL: agentURL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// RequestDecision sends a DecisionRequest to the External AI Agent and
// returns the DecisionResponse containing ranked NodeScores.
//
// This implements step 5/D → 6/E in the architecture:
//
//	Caller builds DecisionRequest (via DecisionContextBuilder.Build)
//	→ RequestDecision POST to AI Agent
//	→ AI Agent returns NodeScores
//	→ Caller translates NodeScores to ValidPlacementDecision (step 7)
//
// The caller's context is forwarded so cancellation propagates to the
// HTTP call — if the scheduler times out, the request is cancelled.
func (c *DecisionClient) RequestDecision(
	ctx context.Context,
	req *DecisionRequest,
) (*DecisionResponse, error) {
	logger := logf.FromContext(ctx)

	// Assign a request ID if not already set by the builder
	if req.RequestID == "" {
		req.RequestID = uuid.NewString()
	}

	logger.Info("sending decision request to external AI agent",
		"requestId", req.RequestID,
		"agentURL", c.agentURL,
		"pod", req.Pod.Name,
		"namespace", req.Pod.Namespace,
		"profile", req.AOProfile.ProfileName,
		"strategy", req.AOProfile.Strategy,
		"candidateNodes", len(req.CandidateNodes),
		"currentPlacementNode", req.AOProfile.CurrentPlacement.NodeName,
		"energyDataAttached", req.EAOProfile != nil,
	)

	// -------------------------------------------------------------------------
	// Serialise the DecisionRequest to JSON
	// -------------------------------------------------------------------------
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshalling DecisionRequest for pod %s/%s: %w",
			req.Pod.Namespace, req.Pod.Name, err)
	}

	// -------------------------------------------------------------------------
	// Build and send the HTTP POST request
	// The caller's ctx is attached so cancellation propagates cleanly
	// -------------------------------------------------------------------------
	url := c.agentURL + decisionEndpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building HTTP request for pod %s/%s: %w",
			req.Pod.Namespace, req.Pod.Name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Request-ID", req.RequestID)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling external decision agent at %s for pod %s/%s: %w",
			url, req.Pod.Namespace, req.Pod.Name, err)
	}
	defer httpResp.Body.Close()

	// -------------------------------------------------------------------------
	// Handle non-200 responses
	// -------------------------------------------------------------------------
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"external decision agent returned HTTP %d for pod %s/%s (requestId=%s)",
			httpResp.StatusCode, req.Pod.Namespace, req.Pod.Name, req.RequestID,
		)
	}

	// -------------------------------------------------------------------------
	// Parse the DecisionResponse
	// -------------------------------------------------------------------------
	var resp DecisionResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decoding DecisionResponse for pod %s/%s: %w",
			req.Pod.Namespace, req.Pod.Name, err)
	}

	logger.Info("received decision response from external AI agent",
		"requestId", req.RequestID,
		"pod", req.Pod.Name,
		"nodeScoresCount", len(resp.NodeScores),
		"topNode", topNodeName(resp.NodeScores),
		"reason", resp.Reason,
	)

	return &resp, nil
}

// =============================================================================
// Helper
// =============================================================================

// topNodeName returns the name of the highest-scored node from the response,
// or "none" if the list is empty. Used for logging only.
func topNodeName(scores []NodeScore) string {
	if len(scores) == 0 {
		return "none"
	}
	top := scores[0]
	for _, s := range scores[1:] {
		if s.Score > top.Score {
			top = s
		}
	}
	return fmt.Sprintf("%s (score=%.2f)", top.NodeName, top.Score)
}
