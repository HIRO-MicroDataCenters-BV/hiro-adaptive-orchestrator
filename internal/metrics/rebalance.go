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
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// Guardrail label values for RebalanceGuardrailRejectionsTotal.
const (
	GuardrailThreshold = "threshold"
	GuardrailBounds    = "bounds"
)

// Result label values for RebalanceAIRequestsTotal.
const (
	AIResultSuccess      = "success"
	AIResultContextError = "context_error"
	AIResultAIError      = "ai_error"
)

// Result label values for RebalancePressureQueriesTotal.
const (
	PressureResultSuccess     = "success"
	PressureResultUnavailable = "unavailable"
)

// Stage label values for RebalanceEnactmentFailuresTotal — which step of
// carrying out an accepted decision failed.
const (
	EnactStageEvict           = "evict"
	EnactStagePatch           = "patch"
	EnactStageWaitReplacement = "wait_replacement"
)

// Result label values for RebalanceResizeOutcomesTotal.
const (
	ResizeResultAll     = "all_resized"
	ResizeResultPartial = "partial"
	ResizeResultNone    = "none"
)

// RebalanceTransitionsTotal is deliberately NOT labeled by the transition's
// free-text reason: reason is a full sentence (e.g. a specific node's
// wattage), so as a label it would be unbounded cardinality. Action/outcome
// give the same "why" diagnostic value from already-bounded enums instead.
var RebalanceTransitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_rebalance_transitions_total",
	Help: "Total rebalance state transitions, by from-state, to-state, action, and outcome (outcome is empty for non-terminal transitions).",
}, []string{"from", "to", "action", "outcome"})

// RebalanceGuardrailRejectionsTotal counts AI recommendations a dispatch
// guardrail refused to act on, before ever reaching Decided.
var RebalanceGuardrailRejectionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_rebalance_guardrail_rejections_total",
	Help: "Total AI recommendations rejected by a dispatch guardrail, by action and which guardrail (threshold, bounds).",
}, []string{"action", "guardrail"})

// RebalanceRateLimitExhaustedTotal counts an accepted recommendation that
// still failed to clear the cluster-wide rate limit before Enacting.
var RebalanceRateLimitExhaustedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_rebalance_rate_limit_exhausted_total",
	Help: "Total accepted recommendations that failed the cluster-wide rate-limit wait, by action.",
}, []string{"action"})

// RebalanceCooldownSkipsTotal counts a reconcile that skipped AI
// consultation because the profile is still in cooldown.
var RebalanceCooldownSkipsTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "hiro_rebalance_cooldown_skips_total",
	Help: "Total reconciles that skipped AI consultation because the profile is still in cooldown.",
})

// RebalanceImprovementScore is the distribution of Improvement scores
// returned by the AI agent, regardless of whether a guardrail later accepted
// or rejected the recommendation.
var RebalanceImprovementScore = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "hiro_rebalance_improvement_score",
	Help:    "Distribution of Improvement scores returned by the AI agent, by action.",
	Buckets: prometheus.LinearBuckets(0, 10, 11), // 0,10,...,100
}, []string{"action"})

// RebalanceAIRequestsTotal separates the two ways consulting the AI can fail
// — assembling the request (context_error) vs the round trip itself (ai_error)
// — which RebalanceTransitionsTotal collapses into a single outcome=Failed
// alongside the AI simply declining to act.
var RebalanceAIRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_rebalance_ai_requests_total",
	Help: "Total rebalance AI consultations, by result (success, context_error, ai_error).",
}, []string{"result"})

// RebalanceAIDuration mirrors hiro_placement_score_duration_seconds so the
// two AI call paths are directly comparable.
var RebalanceAIDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "hiro_rebalance_ai_duration_seconds",
	Help:    "Duration of rebalance AI consultations, including context assembly.",
	Buckets: prometheus.DefBuckets,
})

// Covers detection, which RebalanceTransitionsTotal only sees once something fired.
var RebalanceTriggerEvaluationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_rebalance_trigger_evaluations_total",
	Help: "Total trigger condition evaluations, by condition and whether it fired.",
}, []string{"condition", "fired"})

// Those paths soft-fail to "did not fire", so a condition that can never match
// otherwise looks like one that simply didn't.
var RebalanceTriggerErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_rebalance_trigger_errors_total",
	Help: "Total trigger evaluations skipped because a data source was unavailable, by condition.",
}, []string{"condition"})

// A missing metrics-server degrades CPU/Memory triggers silently otherwise.
var RebalancePressureQueriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_rebalance_pressure_queries_total",
	Help: "Total node pressure queries against metrics-server, by result.",
}, []string{"result"})

// Buckets run to 300s: enactment waits on a replacement pod becoming ready.
var RebalanceEnactmentDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "hiro_rebalance_enactment_duration_seconds",
	Help:    "Duration of enacting an accepted rebalance decision, by action.",
	Buckets: []float64{1, 5, 10, 30, 60, 120, 300},
}, []string{"action"})

// RebalanceTransitionsTotal only records that a cycle ended Failed, not where.
var RebalanceEnactmentFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_rebalance_enactment_failures_total",
	Help: "Total enactment failures, by action and which step failed.",
}, []string{"action", "stage"})

// A shortfall is reported as Enacted (the patch lands on next rollout), so
// without this a resize that reached no running pods looks fully applied.
var RebalanceResizeOutcomesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_rebalance_resize_outcomes_total",
	Help: "Total AdjustResources enactments, by how much of the in-place resize took effect.",
}, []string{"result"})

// Otherwise indistinguishable from a normal transition back to Watching, so
// "cycles are getting stuck" cannot be alerted on.
var RebalanceWatchdogRecoveriesTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "hiro_rebalance_watchdog_recoveries_total",
	Help: "Total rebalance cycles force-recovered by the watchdog after going stale.",
})

func init() {
	metrics.Registry.MustRegister(
		RebalanceTransitionsTotal,
		RebalanceGuardrailRejectionsTotal,
		RebalanceRateLimitExhaustedTotal,
		RebalanceCooldownSkipsTotal,
		RebalanceImprovementScore,
		RebalanceAIRequestsTotal,
		RebalanceAIDuration,
		RebalanceTriggerEvaluationsTotal,
		RebalanceTriggerErrorsTotal,
		RebalancePressureQueriesTotal,
		RebalanceEnactmentDuration,
		RebalanceEnactmentFailuresTotal,
		RebalanceResizeOutcomesTotal,
		RebalanceWatchdogRecoveriesTotal,
	)
}

// stateGaugeCollector reports how many OrchestrationProfiles currently sit
// in each rebalancing state, live, on every scrape — rather than an
// incrementally Inc/Dec'd gauge, which would drift after an operator crash
// or restart (the in-memory counter resets to 0; real cluster state
// doesn't). A List per scrape is cheap at typical Prometheus scrape
// intervals (15-30s) and keeps this always correct instead of eventually
// correct.
type stateGaugeCollector struct {
	reader        client.Reader
	desc          *prometheus.Desc
	escalatedDesc *prometheus.Desc
}

// NewRebalanceStateGaugeCollector creates a Collector reporting
// hiro_rebalance_profiles_by_state and hiro_rebalance_profiles_escalated.
// The caller (cmd/main.go) must register it with
// metrics.Registry.MustRegister — not done automatically here, since it
// needs a reader that only exists once the manager is constructed.
func NewRebalanceStateGaugeCollector(reader client.Reader) prometheus.Collector {
	return &stateGaugeCollector{
		reader: reader,
		desc: prometheus.NewDesc(
			"hiro_rebalance_profiles_by_state",
			"Current number of OrchestrationProfiles in each rebalancing state.",
			[]string{"state"}, nil,
		),
		escalatedDesc: prometheus.NewDesc(
			"hiro_rebalance_profiles_escalated",
			"Current number of OrchestrationProfiles with the rebalance loop paused by escalation.",
			nil, nil,
		),
	}
}

func (c *stateGaugeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
	ch <- c.escalatedDesc
}

// Collect lists every OrchestrationProfile once and reports two live counts
// off that single List: by state (an unset state, never triggered yet, is
// counted as Watching — the same resting/idle meaning, just prior to ever
// being touched), and currently escalated (a status flag orthogonal to
// state — an escalated profile still shows state Watching, since the pause
// happens only after its terminal write back there).
func (c *stateGaugeCollector) Collect(ch chan<- prometheus.Metric) {
	logger := logf.Log.WithName("rebalance-metrics")

	list := &orchestrationv1alpha1.OrchestrationProfileList{}
	if err := c.reader.List(context.Background(), list); err != nil {
		logger.Error(err, "rebalance: listing profiles for state gauge failed")
		return
	}

	counts := map[orchestrationv1alpha1.RebalancingStateType]float64{
		orchestrationv1alpha1.RebalancingStateWatching:   0,
		orchestrationv1alpha1.RebalancingStateTriggered:  0,
		orchestrationv1alpha1.RebalancingStateEvaluating: 0,
		orchestrationv1alpha1.RebalancingStateDecided:    0,
		orchestrationv1alpha1.RebalancingStateEnacting:   0,
	}
	var escalated float64
	for i := range list.Items {
		rs := list.Items[i].Status.RebalancingStatus
		state := rs.State
		if state == "" {
			state = orchestrationv1alpha1.RebalancingStateWatching
		}
		counts[state]++
		if rs.Escalated {
			escalated++
		}
	}

	for state, count := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, count, string(state))
	}
	ch <- prometheus.MustNewConstMetric(c.escalatedDesc, prometheus.GaugeValue, escalated)
}
