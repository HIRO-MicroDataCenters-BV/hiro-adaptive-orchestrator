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
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Registered against legacyregistry — the registry kube-scheduler already
// serves on :10259/metrics. The operator's internal/metrics package can't be
// reused: it registers into controller-runtime's registry, which nothing
// serves in this process. Namespace+Subsystem reproduce its hiro_<subsystem>_*
// naming. No label carries a pod, node, or profile name.
const (
	metricsNamespace = "hiro"
	metricsSubsystem = "scheduler_plugin"
)

// Endpoint label values.
const (
	endpointScore  = "score"
	endpointFilter = "filter"
)

// Result label values for PlacementRequestsTotal.
const (
	resultSuccess = "success"
	resultError   = "error"
)

// Reason label values for PlacementErrorsTotal — the five distinguishable
// failure points in PlacementClient's request path.
const (
	reasonMarshal      = "marshal"
	reasonRequestBuild = "request_build"
	reasonTransport    = "transport"
	reasonHTTPStatus   = "http_status"
	reasonDecode       = "decode"
)

// Phase label values for SoftFailTotal.
const (
	phasePreFilter = "prefilter"
	phasePreScore  = "prescore"
	phaseScore     = "score"
)

// Latency buckets cover the default 8s client timeout; node-count buckets
// span a single node up to a large cluster's candidate set.
var (
	latencyBuckets   = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}
	nodeCountBuckets = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500}
)

func counterOpts(name, help string) *metrics.CounterOpts {
	return &metrics.CounterOpts{
		Namespace:      metricsNamespace,
		Subsystem:      metricsSubsystem,
		Name:           name,
		Help:           help,
		StabilityLevel: metrics.ALPHA,
	}
}

func histogramOpts(name, help string, buckets []float64) *metrics.HistogramOpts {
	return &metrics.HistogramOpts{
		Namespace:      metricsNamespace,
		Subsystem:      metricsSubsystem,
		Name:           name,
		Help:           help,
		Buckets:        buckets,
		StabilityLevel: metrics.ALPHA,
	}
}

var (
	PlacementRequestsTotal = metrics.NewCounterVec(counterOpts(
		"placement_requests_total",
		"Total requests to the PlacementServer, by endpoint and result.",
	), []string{"endpoint", "result"})

	PlacementRequestDuration = metrics.NewHistogramVec(histogramOpts(
		"placement_request_duration_seconds",
		"Duration of PlacementServer requests, by endpoint.",
		latencyBuckets,
	), []string{"endpoint"})

	PlacementErrorsTotal = metrics.NewCounterVec(counterOpts(
		"placement_errors_total",
		"Total PlacementServer request errors, by endpoint and cause.",
	), []string{"endpoint", "reason"})

	// Each event is a pod scheduled with no AI input. Without this, a broken
	// PlacementServer looks identical to a healthy one from outside.
	SoftFailTotal = metrics.NewCounterVec(counterOpts(
		"soft_fail_total",
		"Total soft-fail-open events, by extension point phase.",
	), []string{"phase"})

	EnergyGateDecisionsTotal = metrics.NewCounterVec(counterOpts(
		"energy_gate_decisions_total",
		"Total completed energy-gate checks, by whether the pod was allowed.",
	), []string{"allowed"})

	// Drift toward zero means the AI responds but scores nothing — which
	// SoftFailTotal misses, since the call itself succeeded.
	NodesScored = metrics.NewHistogram(histogramOpts(
		"nodes_scored",
		"Number of nodes the AI returned scores for, per scheduling cycle.",
		nodeCountBuckets,
	))

	// Separates "AI never scored this node" (neutral 0) from "AI scored it 0".
	UnscoredNodesTotal = metrics.NewCounter(counterOpts(
		"unscored_nodes_total",
		"Total Score calls for a node absent from the AI's response, scored neutral.",
	))
)

func init() {
	legacyregistry.MustRegister(
		PlacementRequestsTotal,
		PlacementRequestDuration,
		PlacementErrorsTotal,
		SoftFailTotal,
		EnergyGateDecisionsTotal,
		NodesScored,
		UnscoredNodesTotal,
	)
}
