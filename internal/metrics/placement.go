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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Path label values, shared by PlacementScoreRequestsTotal,
// PlacementScoreDuration, PlacementFilterRequestsTotal, and
// PlacementFilterErrorsTotal — which of the two equivalent protocol
// entry points (scheduler-plugin vs kube-scheduler extender) was used.
const (
	PathPlugin   = "plugin"
	PathExtender = "extender"
)

// Result label values for PlacementScoreRequestsTotal.
const (
	ScoreResultStoreHit  = "store_hit"
	ScoreResultAISuccess = "ai_success"
	ScoreResultAIError   = "ai_error"
)

// PlacementScoreRequestsTotal covers both PlacementServer entry points that
// score nodes for a pod — the scheduler-plugin path (POST
// /api/v1/placement/score) and the extender path (POST
// /extender/prioritize) — since both call the exact same shared score()
// method. path distinguishes which protocol was actually used; result is
// one of "store_hit" (DecisionStore answered, no AI call), "ai_success", or
// "ai_error".
var PlacementScoreRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_placement_score_requests_total",
	Help: "Total placement scoring requests, by protocol path and result.",
}, []string{"path", "result"})

// PlacementScoreDuration times the whole score() call, including a
// DecisionStore hit (near-zero) or the full AI round trip.
var PlacementScoreDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "hiro_placement_score_duration_seconds",
	Help:    "Duration of placement scoring, by protocol path.",
	Buckets: prometheus.DefBuckets,
}, []string{"path"})

// PlacementFilterRequestsTotal covers both energy-gate entry points — the
// plugin path (POST /api/v1/placement/filter) and the extender path (POST
// /extender/filter) — both backed by the shared filter() method.
var PlacementFilterRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_placement_filter_requests_total",
	Help: "Total energy-gate filter requests, by protocol path and whether the pod was allowed.",
}, []string{"path", "allowed"})

// PlacementFilterErrorsTotal counts a filter() error that was soft-failed
// open (the pod was allowed anyway, per filter's documented soft-fail
// behavior) — visibility into how often that safety net is actually used.
var PlacementFilterErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_placement_filter_errors_total",
	Help: "Total energy-gate filter errors, soft-failed open, by protocol path.",
}, []string{"path"})

// PlacementDecisionStoreHitsTotal and PlacementDecisionStoreMissesTotal
// cover DecisionStore.Lookup — raw counters rather than a computed ratio
// gauge, so any ratio/window can be derived in Prometheus/Grafana instead of
// being fixed by this process.
var PlacementDecisionStoreHitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "hiro_placement_decision_store_hits_total",
	Help: "Total DecisionStore lookups that found a live, unexpired entry.",
})

var PlacementDecisionStoreMissesTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "hiro_placement_decision_store_misses_total",
	Help: "Total DecisionStore lookups that found no entry, or an expired one.",
})

func init() {
	metrics.Registry.MustRegister(
		PlacementScoreRequestsTotal,
		PlacementScoreDuration,
		PlacementFilterRequestsTotal,
		PlacementFilterErrorsTotal,
		PlacementDecisionStoreHitsTotal,
		PlacementDecisionStoreMissesTotal,
	)
}
