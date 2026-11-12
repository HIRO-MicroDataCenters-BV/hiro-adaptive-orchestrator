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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/metrics"
)

func delta(c prometheus.Collector, before float64) float64 {
	return testutil.ToFloat64(c) - before
}

func testPlacementCtx(pod *corev1.Pod) PlacementContext {
	return PlacementContext{Pod: pod, CandidateNodes: []*corev1.Node{testNode("node-a", true)}}
}

// "Not governed" and "lookup broke" reach the caller as one error; only the
// metric tells them apart.
func TestBuild_ContextBuildResultIsCounted(t *testing.T) {
	ctx := context.Background()
	success := metrics.PlacementContextBuildTotal.WithLabelValues(metrics.BuildResultSuccess)
	notFound := metrics.PlacementContextBuildTotal.WithLabelValues(metrics.BuildResultProfileNotFound)
	lookupErr := metrics.PlacementContextBuildTotal.WithLabelValues(metrics.BuildResultProfileError)

	pod, rs := testDeploymentPod()

	before := testutil.ToFloat64(success)
	b := newTestBuilder(t, testRebalanceProfile(false), testAppDeployment(), rs, pod)
	if _, err := b.Build(ctx, testPlacementCtx(pod), "req-ok"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := delta(success, before); got != 1 {
		t.Errorf("success delta = %v, want 1", got)
	}

	before = testutil.ToFloat64(notFound)
	b = newTestBuilder(t, testAppDeployment(), rs, pod) // no profile governs app-a
	if _, err := b.Build(ctx, testPlacementCtx(pod), "req-ungoverned"); err == nil {
		t.Fatal("Build: want an error for an ungoverned pod")
	}
	if got := delta(notFound, before); got != 1 {
		t.Errorf("profile_not_found delta = %v, want 1", got)
	}

	before = testutil.ToFloat64(lookupErr)
	b = newTestBuilder(t, testRebalanceProfile(false), testAppDeployment(), rs, pod)
	b.profileIndexField = "unregistered-index" // fake client rejects an unknown index
	if _, err := b.Build(ctx, testPlacementCtx(pod), "req-broken"); err == nil {
		t.Fatal("Build: want an error when the profile index lookup fails")
	}
	if got := delta(lookupErr, before); got != 1 {
		t.Errorf("profile_error delta = %v, want 1", got)
	}
}
