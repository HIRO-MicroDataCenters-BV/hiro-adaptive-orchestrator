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

	corev1 "k8s.io/api/core/v1"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// =============================================================================
// PlacementClient
//
// Used by: HIROScore scheduler plugin (PreFilter + PreScore phases)
// Calls:   PlacementServer running in the operator pod (:8090)
//
// Two endpoints — both share the same server URL:
//
//   CheckFilter  →  POST /api/v1/placement/filter   — energy gate (PreFilter)
//   Decide       →  POST /api/v1/placement/score    — AI scoring  (PreScore)
// =============================================================================

const (
	envPlacementServerURL  = "PLACEMENT_SERVER_URL"
	envPlacementScorePath  = "PLACEMENT_SCORE_PATH"
	envPlacementFilterPath = "PLACEMENT_FILTER_PATH"
)

const (
	// DefaultPlacementServerURL is the in-cluster DNS name of the PlacementServer.
	DefaultPlacementServerURL = "http://hiro-adaptive-orchestrator-controller-manager-placement-service" +
		".hiro-adaptive-orchestrator-system.svc.cluster.local:8090"

	// DefaultPlacementServerPath must match PLACEMENT_SCORE_PATH on the operator.
	DefaultPlacementServerPath = "/api/v1/placement/score"

	// DefaultFilterPath must match PLACEMENT_FILTER_PATH on the operator.
	DefaultFilterPath = "/api/v1/placement/filter"
)

// PlacementClient is a concurrency-safe HTTP client for the PlacementServer.
type PlacementClient struct {
	serverURL  string
	scorePath  string
	filterPath string
	httpClient *http.Client
}

// NewPlacementClient creates a PlacementClient with explicit parameters.
// Prefer NewPlacementClientFromEnv in production; use this constructor in tests.
func NewPlacementClient(serverURL, scorePath, filterPath string, timeout time.Duration) *PlacementClient {
	return &PlacementClient{
		serverURL:  serverURL,
		scorePath:  scorePath,
		filterPath: filterPath,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// NewPlacementClientFromEnv creates a PlacementClient from environment variables:
//
//	PLACEMENT_SERVER_URL    — base URL of the PlacementServer
//	PLACEMENT_SCORE_PATH    — HTTP path for AI scoring
//	PLACEMENT_FILTER_PATH   — HTTP path for energy gate filter
func NewPlacementClientFromEnv(timeout time.Duration) *PlacementClient {
	serverURL := os.Getenv(envPlacementServerURL)
	if serverURL == "" {
		serverURL = DefaultPlacementServerURL
	}
	scorePath := os.Getenv(envPlacementScorePath)
	if scorePath == "" {
		scorePath = DefaultPlacementServerPath
	}
	filterPath := os.Getenv(envPlacementFilterPath)
	if filterPath == "" {
		filterPath = DefaultFilterPath
	}
	return NewPlacementClient(serverURL, scorePath, filterPath, timeout)
}

// =============================================================================
// CheckFilter — PreFilter phase (energy gate)
// =============================================================================

// CheckFilter calls POST /api/v1/placement/filter on the PlacementServer to
// determine whether energy constraints allow this pod to be scheduled at all.
//
// Returns (true, "", nil)  — pod is allowed.
// Returns (false, reason, nil) — pod is blocked by the energy gate.
// Returns (true, "", err)  — soft-fail open on any communication error.
func (c *PlacementClient) CheckFilter(
	ctx context.Context,
	pod *corev1.Pod,
) (allowed bool, reason string, err error) {
	reqBody := placement.EnergyGateRequest{Pod: pod}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return true, "", fmt.Errorf("marshalling EnergyGateRequest: %w", err)
	}

	url := c.serverURL + c.filterPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return true, "", fmt.Errorf("building filter request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", string(pod.UID))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return true, "", fmt.Errorf("calling placement filter at %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return true, "", fmt.Errorf("placement filter returned HTTP %d", resp.StatusCode)
	}

	var result placement.EnergyGateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return true, "", fmt.Errorf("decoding EnergyGateResponse: %w", err)
	}

	return result.Allowed, result.Reason, nil
}

// =============================================================================
// Decide — PreScore phase (AI scoring)
// =============================================================================

// Decide sends a PlacementContext to POST /api/v1/placement/score on the
// PlacementServer and returns the AI-scored NodeScores.
//
// Callers must soft-fail on error — never return Unschedulable because of
// PlacementClient failures.
func (c *PlacementClient) Decide(
	ctx context.Context,
	placementCtx *placement.PlacementContext,
) (*placement.DecisionResponse, error) {
	requestID := string(placementCtx.Pod.UID)

	body, err := json.Marshal(placementCtx)
	if err != nil {
		return nil, fmt.Errorf("marshalling PlacementContext (requestId=%s): %w", requestID, err)
	}

	url := c.serverURL + c.scorePath
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
