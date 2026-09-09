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

func init() {
	metrics.Registry.MustRegister(
		RebalanceTransitionsTotal,
		RebalanceGuardrailRejectionsTotal,
		RebalanceRateLimitExhaustedTotal,
		RebalanceCooldownSkipsTotal,
		RebalanceImprovementScore,
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
	reader client.Reader
	desc   *prometheus.Desc
}

// NewRebalanceStateGaugeCollector creates a Collector reporting
// hiro_rebalance_profiles_by_state. The caller (cmd/main.go) must register
// it with metrics.Registry.MustRegister — not done automatically here, since
// it needs a reader that only exists once the manager is constructed.
func NewRebalanceStateGaugeCollector(reader client.Reader) prometheus.Collector {
	return &stateGaugeCollector{
		reader: reader,
		desc: prometheus.NewDesc(
			"hiro_rebalance_profiles_by_state",
			"Current number of OrchestrationProfiles in each rebalancing state.",
			[]string{"state"}, nil,
		),
	}
}

func (c *stateGaugeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect lists every OrchestrationProfile and counts them by state. An
// unset state (never triggered yet) is counted as Watching — the same
// resting/idle meaning, just prior to ever being touched.
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
	for i := range list.Items {
		state := list.Items[i].Status.RebalancingStatus.State
		if state == "" {
			state = orchestrationv1alpha1.RebalancingStateWatching
		}
		counts[state]++
	}

	for state, count := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, count, string(state))
	}
}
