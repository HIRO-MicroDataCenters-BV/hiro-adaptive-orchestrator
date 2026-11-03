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

package rebalance

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/metrics"
)

// The whole point of RebalanceTriggerErrorsTotal: a condition that could not
// be evaluated must be distinguishable from one that was evaluated and did
// not fire. Both report matched=false, so only the metric tells them apart.
// The node exists with allocatable capacity, so evaluation reaches
// metrics-server, which has no NodeMetrics registered and so errors.
func TestEvaluate_UnevaluableConditionIsCounted(t *testing.T) {
	profile := testProfileWithConditions(TriggerCPUThreshold)
	evaluator := newTestTriggerEvaluator(t,
		testDeployment(), testPod("pod-a", "node-a", corev1.PodRunning), newTestNode("node-a"))

	errCounter := metrics.RebalanceTriggerErrorsTotal.WithLabelValues(string(TriggerCPUThreshold))
	notFired := metrics.RebalanceTriggerEvaluationsTotal.
		WithLabelValues(string(TriggerCPUThreshold), "false")
	unavailable := metrics.RebalancePressureQueriesTotal.
		WithLabelValues(metrics.PressureResultUnavailable)

	beforeErr := testutil.ToFloat64(errCounter)
	beforeNotFired := testutil.ToFloat64(notFired)
	beforeUnavailable := testutil.ToFloat64(unavailable)

	matched, _, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if matched {
		t.Fatal("expected no match when metrics-server is unavailable")
	}

	if got := testutil.ToFloat64(errCounter) - beforeErr; got != 1 {
		t.Errorf("trigger error counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(notFired) - beforeNotFired; got != 1 {
		t.Errorf("evaluation(fired=false) delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(unavailable) - beforeUnavailable; got != 1 {
		t.Errorf("pressure unavailable delta = %v, want 1", got)
	}
}

// A condition that fires must be recorded as fired, and must not be counted
// as an error.
func TestEvaluate_FiredConditionIsCounted(t *testing.T) {
	profile := testProfileWithConditions(TriggerPeriodicReview)
	evaluator := newTestTriggerEvaluator(t, testDeployment())

	fired := metrics.RebalanceTriggerEvaluationsTotal.
		WithLabelValues(string(TriggerPeriodicReview), "true")
	errCounter := metrics.RebalanceTriggerErrorsTotal.WithLabelValues(string(TriggerPeriodicReview))

	beforeFired := testutil.ToFloat64(fired)
	beforeErr := testutil.ToFloat64(errCounter)

	matched, _, err := evaluator.Evaluate(context.Background(), profile)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !matched {
		t.Fatal("expected PeriodicReview to match")
	}

	if got := testutil.ToFloat64(fired) - beforeFired; got != 1 {
		t.Errorf("evaluation(fired=true) delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(errCounter) - beforeErr; got != 0 {
		t.Errorf("trigger error counter delta = %v, want 0", got)
	}
}
