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
	"testing"
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
	schedulerplugin "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/scheduler-plugin"
)

// Label values are asserted as literals rather than via the package's
// constants: these strings are the wire contract dashboards query by, so the
// test should fail if they ever change.
func counterValue(t *testing.T, m metrics.CounterMetric) float64 {
	t.Helper()
	v, err := testutil.GetCounterMetricValue(m)
	if err != nil {
		t.Fatalf("reading counter: %v", err)
	}
	return v
}

// Read from the registry rather than the metric: a non-vec Histogram has no
// Desc, so testutil's per-metric helpers don't apply to it.
func histogramCount(t *testing.T, name string) uint64 {
	t.Helper()
	families, err := legacyregistry.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		var total uint64
		for _, m := range f.GetMetric() {
			total += m.GetHistogram().GetSampleCount()
		}
		return total
	}
	return 0
}

// Metrics are process-global, so every assertion is a delta — the test must
// not depend on which other tests ran first.
func TestDecide_RecordsSuccessMetrics(t *testing.T) {
	okCounter := schedulerplugin.PlacementRequestsTotal.WithLabelValues("score", "success")
	beforeOK := counterValue(t, okCounter)
	beforeScored := histogramCount(t, "hiro_scheduler_plugin_nodes_scored")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(testDecisionResponse())
	}))
	defer srv.Close()

	client := schedulerplugin.NewPlacementClient(srv.URL, "/score", "/filter", 2*time.Second)
	if _, err := client.Decide(context.Background(), testPlacementCtx()); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if got := counterValue(t, okCounter) - beforeOK; got != 1 {
		t.Errorf("success counter delta = %v, want 1", got)
	}
	if got := histogramCount(t, "hiro_scheduler_plugin_nodes_scored") - beforeScored; got != 1 {
		t.Errorf("nodes_scored observation delta = %d, want 1", got)
	}
}

// An HTTP error must be attributed to http_status specifically, not folded
// into a generic failure — telling "server rejected it" apart from
// "server unreachable" is the whole point of the reason label.
func TestDecide_RecordsHTTPStatusError(t *testing.T) {
	errCounter := schedulerplugin.PlacementRequestsTotal.WithLabelValues("score", "error")
	statusCounter := schedulerplugin.PlacementErrorsTotal.WithLabelValues("score", "http_status")
	beforeErr := counterValue(t, errCounter)
	beforeStatus := counterValue(t, statusCounter)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := schedulerplugin.NewPlacementClient(srv.URL, "/score", "/filter", 2*time.Second)
	if _, err := client.Decide(context.Background(), testPlacementCtx()); err == nil {
		t.Fatal("expected an error from a 500 response")
	}

	if got := counterValue(t, errCounter) - beforeErr; got != 1 {
		t.Errorf("error counter delta = %v, want 1", got)
	}
	if got := counterValue(t, statusCounter) - beforeStatus; got != 1 {
		t.Errorf("http_status counter delta = %v, want 1", got)
	}
}

// A transport failure (unreachable server) must land on reason=transport.
func TestDecide_RecordsTransportError(t *testing.T) {
	transportCounter := schedulerplugin.PlacementErrorsTotal.WithLabelValues("score", "transport")
	before := counterValue(t, transportCounter)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	client := schedulerplugin.NewPlacementClient(url, "/score", "/filter", time.Second)
	if _, err := client.Decide(context.Background(), testPlacementCtx()); err == nil {
		t.Fatal("expected an error from an unreachable server")
	}

	if got := counterValue(t, transportCounter) - before; got != 1 {
		t.Errorf("transport counter delta = %v, want 1", got)
	}
}

// A blocked pod must be recorded as a real gate decision, not an error —
// the gate working as designed is not a failure.
func TestCheckFilter_RecordsGateDecision(t *testing.T) {
	blocked := schedulerplugin.EnergyGateDecisionsTotal.WithLabelValues("false")
	before := counterValue(t, blocked)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(placement.EnergyGateResponse{
			Allowed: false,
			Reason:  "energy budget exceeded",
		})
	}))
	defer srv.Close()

	client := schedulerplugin.NewPlacementClient(srv.URL, "/score", "/filter", 2*time.Second)
	allowed, _, err := client.CheckFilter(context.Background(), testPlacementCtx().Pod)
	if err != nil {
		t.Fatalf("CheckFilter: %v", err)
	}
	if allowed {
		t.Error("expected the pod to be blocked")
	}

	if got := counterValue(t, blocked) - before; got != 1 {
		t.Errorf("gate decision counter delta = %v, want 1", got)
	}
}
