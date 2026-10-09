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

// The filter path returns Allowed=true with a nil error when the EAO is
// missing, so PlacementFilterRequestsTotal counts it as a genuine allow. The
// EAO metric is the only thing that tells the two apart.
func TestCheckEnergyGate_MissingEAOIsDistinguishableFromAllow(t *testing.T) {
	ctx := context.Background()
	notFound := metrics.PlacementEAOFetchTotal.WithLabelValues(metrics.EAOCallerFilter, metrics.EAOResultNotFound)
	found := metrics.PlacementEAOFetchTotal.WithLabelValues(metrics.EAOCallerFilter, metrics.EAOResultFound)

	pod, rs := testDeploymentPod()

	beforeNotFound := testutil.ToFloat64(notFound)
	b := newTestBuilder(t, testRebalanceProfile(true), testAppDeployment(), rs, pod)
	resp, err := b.CheckEnergyGate(ctx, pod)
	if err != nil || !resp.Allowed {
		t.Fatalf("CheckEnergyGate with no EAO = (%+v, %v), want allowed with no error", resp, err)
	}
	if got := delta(notFound, beforeNotFound); got != 1 {
		t.Errorf("filter not_found delta = %v, want 1", got)
	}

	beforeFound := testutil.ToFloat64(found)
	b = newTestBuilder(t, testRebalanceProfile(true), testAppDeployment(), rs, pod,
		testRebalanceEAO("Critical", 500, "DeployImmediately", "window open", true))
	resp, err = b.CheckEnergyGate(ctx, pod)
	if err != nil || !resp.Allowed {
		t.Fatalf("CheckEnergyGate with sufficient EAO = (%+v, %v), want allowed with no error", resp, err)
	}
	if got := delta(found, beforeFound); got != 1 {
		t.Errorf("filter found delta = %v, want 1", got)
	}
}

// Energy awareness off means no EAO is consulted at all, so nothing is counted.
func TestBuild_EAOFetchSkippedWhenEnergyOff(t *testing.T) {
	pod, rs := testDeploymentPod()
	b := newTestBuilder(t, testRebalanceProfile(false), testAppDeployment(), rs, pod)

	scoreNotFound := metrics.PlacementEAOFetchTotal.WithLabelValues(metrics.EAOCallerScore, metrics.EAOResultNotFound)
	before := testutil.ToFloat64(scoreNotFound)

	if _, err := b.Build(context.Background(), testPlacementCtx(pod), "req-1"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := delta(scoreNotFound, before); got != 0 {
		t.Errorf("score not_found delta = %v, want 0 with energy awareness off", got)
	}
}
