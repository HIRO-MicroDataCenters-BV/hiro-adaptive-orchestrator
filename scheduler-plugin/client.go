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

package schedulerplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// =============================================================================
// PlacementClient
//
// Used by: HIROScore scheduler plugin (PreScore phase)
// Calls:   PlacementServer running in the operator pod (:8090)
//
// Flow:
//   HIROScore.PreScore  →  PlacementClient.Decide(PlacementContext)
//                       →  POST PLACEMENT_SERVER_URL + PLACEMENT_SERVER_PATH
//                       ←  DecisionResponse{NodeScores}
//   HIROScore.Score     →  reads per-node score from CycleState
// =============================================================================

const (
	envPlacementServerURL  = "PLACEMENT_SERVER_URL"
	envPlacementServerPath = "PLACEMENT_SERVER_PATH"
)

const (
	// DefaultPlacementServerURL is the in-cluster DNS name of the PlacementServer.
	DefaultPlacementServerURL = "http://hiro-adaptive-orchestrator-controller-manager-placement-service" +
		".hiro-adaptive-orchestrator-system.svc.cluster.local:8090"

	// DefaultPlacementServerPath must match PLACEMENT_SERVER_PATH on the operator.
	DefaultPlacementServerPath = "/api/v1/placement/decision"
)

// PlacementClient is a concurrency-safe HTTP client that sends a PlacementContext
// to the PlacementServer and returns the AI-scored NodeScores.
type PlacementClient struct {
	serverURL  string
	path       string
	httpClient *http.Client
}

// NewPlacementClient creates a PlacementClient with explicit parameters.
// Prefer NewPlacementClientFromEnv in production; use this constructor in tests.
func NewPlacementClient(serverURL, path string, timeout time.Duration) *PlacementClient {
	return &PlacementClient{
		serverURL:  serverURL,
		path:       path,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// NewPlacementClientFromEnv creates a PlacementClient from environment variables:
//
//	PLACEMENT_SERVER_URL  — base URL of the PlacementServer
//	PLACEMENT_SERVER_PATH — HTTP path for decisions
func NewPlacementClientFromEnv(timeout time.Duration) *PlacementClient {
	serverURL := os.Getenv(envPlacementServerURL)
	if serverURL == "" {
		serverURL = DefaultPlacementServerURL
	}
	path := os.Getenv(envPlacementServerPath)
	if path == "" {
		path = DefaultPlacementServerPath
	}
	return NewPlacementClient(serverURL, path, timeout)
}

// Decide sends a PlacementContext to the PlacementServer and returns
// DecisionResponse containing AI-scored NodeScores.
//
// Callers must soft-fail on error — never return Unschedulable because of
// PlacementClient failures.
func (c *PlacementClient) Decide(
	ctx context.Context,
	placementCtx *placement.PlacementContext,
) (*placement.DecisionResponse, error) {
	requestID := uuid.NewString()

	body, err := json.Marshal(placementCtx)
	if err != nil {
		return nil, fmt.Errorf("marshalling PlacementContext (requestId=%s): %w", requestID, err)
	}

	url := c.serverURL + c.path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request to placement server (requestId=%s): %w", requestID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", requestID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling placement server at %s (requestId=%s): %w",
			url, requestID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("placement server returned HTTP %d (requestId=%s)",
			resp.StatusCode, requestID)
	}

	var decisionResp placement.DecisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&decisionResp); err != nil {
		return nil, fmt.Errorf("decoding DecisionResponse (requestId=%s): %w", requestID, err)
	}

	return &decisionResp, nil
}
