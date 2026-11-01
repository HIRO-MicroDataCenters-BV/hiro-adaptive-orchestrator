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
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/metrics"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

func TestEAOVerdict(t *testing.T) {
	now := time.Date(2026, 10, 29, 13, 30, 0, 0, time.UTC)
	slot := func(start string) *EAOScheduledSlot { return &EAOScheduledSlot{SlotStart: start} }

	cases := []struct {
		name      string
		decision  *EAODecision
		want      placement.EAOVerdict
		wantUntil string // RFC 3339; empty means no HoldUntil
	}{
		{"no decision", nil, placement.EAOVerdictUnknown, ""},
		{"deploy immediately", &EAODecision{Action: "DeployImmediately"}, placement.EAOVerdictRunNow, ""},
		{"scheduled, slot ahead", &EAODecision{Action: "Scheduled", ScheduledSlot: slot("2026-10-29T14:00:00Z")},
			placement.EAOVerdictHold, "2026-10-29T14:00:00Z"},
		{"scheduled, slot started", &EAODecision{Action: "Scheduled", ScheduledSlot: slot("2026-10-29T13:00:00Z")},
			placement.EAOVerdictRunNow, ""},
		{"scheduled, no slot", &EAODecision{Action: "Scheduled"}, placement.EAOVerdictHold, ""},
		{"scheduled, bad slot", &EAODecision{Action: "Scheduled", ScheduledSlot: slot("soon")}, placement.EAOVerdictHold, ""},
		{"delayed", &EAODecision{Action: "Delayed"}, placement.EAOVerdictHold, ""},
		{"waiting is an EAO failure", &EAODecision{Action: "Waiting"}, placement.EAOVerdictUnknown, ""},
		{"unmapped value", &EAODecision{Action: "Throttled"}, placement.EAOVerdictUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, until := eaoVerdict(tc.decision, DefaultEAOVerdictConfig(), now)
			if got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
			gotUntil := ""
			if until != nil {
				gotUntil = until.UTC().Format(time.RFC3339)
			}
			if gotUntil != tc.wantUntil {
				t.Errorf("holdUntil = %q, want %q", gotUntil, tc.wantUntil)
			}
		})
	}
}

func TestEAOVerdict_RenamedActionIsASettingChange(t *testing.T) {
	cfg := EAOVerdictConfig{RunNowActions: []string{"RunNow"}, HoldActions: []string{"Planned"}}
	if got, _ := eaoVerdict(&EAODecision{Action: "Planned"}, cfg, time.Now()); got != placement.EAOVerdictHold {
		t.Errorf("verdict = %q, want Hold for a hold value renamed through config", got)
	}
}

func TestEAOVerdictConfig_Validate(t *testing.T) {
	if err := DefaultEAOVerdictConfig().Validate(); err != nil {
		t.Errorf("default config: %v", err)
	}
	both := EAOVerdictConfig{RunNowActions: []string{"Scheduled"}, HoldActions: []string{"Scheduled"}}
	if err := both.Validate(); err == nil {
		t.Error("a value in both lists: want an error")
	}
}

func testRebalanceEAO(priority string, watts int64, action, reason string, sufficient bool) *unstructured.Unstructured {
	eao := &unstructured.Unstructured{}
	eao.SetGroupVersionKind(testEAOGVK)
	eao.SetName("eao-a")
	eao.SetNamespace("default")
	_ = unstructured.SetNestedMap(eao.Object, map[string]any{
		"name": "app-a", "namespace": "default", "kind": "Deployment",
	}, "spec", "applicationRef")
	_ = unstructured.SetNestedField(eao.Object, priority, "spec", "priority")
	_ = unstructured.SetNestedField(eao.Object, watts, "spec", "energyConsumption")
	_ = unstructured.SetNestedMap(eao.Object, map[string]any{
		"action": action, "reason": reason,
	}, "status", "decision")
	_ = unstructured.SetNestedField(eao.Object, sufficient, "status", "energyMetrics", "sufficient")
	return eao
}

func TestBuildRebalanceContext_EnergyAwareAttachesEAOData(t *testing.T) {
	profile := testRebalanceProfile(true)
	b := newTestBuilder(t,
		testAppDeployment(),
		testAppPod("app-a-1", "node-a", corev1.PodRunning),
		testNode("node-a", true),
		testRebalanceEAO("Critical", 500, "DeployImmediately", "window open", false),
	)

	req, err := b.BuildRebalanceContext(context.Background(), profile, "decision-2", "EnergyThreshold: verdict flipped", RebalanceFacts{})
	if err != nil {
		t.Fatalf("BuildRebalanceContext: %v", err)
	}

	if req.EAOProfile == nil {
		t.Fatal("EAOProfile is nil, want data attached when energy awareness is enabled")
	}
	if req.EAOProfile.Priority != "Critical" || req.EAOProfile.EnergyConsumptionWatts != 500 {
		t.Errorf("EAOProfile spec fields = %+v, want Priority=Critical EnergyConsumptionWatts=500", req.EAOProfile)
	}
	if req.EAOProfile.Decision == nil || req.EAOProfile.Decision.Action != "DeployImmediately" {
		t.Errorf("EAOProfile.Decision = %+v, want Action=DeployImmediately", req.EAOProfile.Decision)
	}
	if req.EAOProfile.EnergyMetrics == nil || req.EAOProfile.EnergyMetrics.Sufficient {
		t.Errorf("EAOProfile.EnergyMetrics = %+v, want Sufficient=false", req.EAOProfile.EnergyMetrics)
	}

	if req.RebalanceContext == nil || len(req.RebalanceContext.History) != 0 {
		t.Errorf("RebalanceContext = %+v, want no history when none is passed", req.RebalanceContext)
	}
}

// Build used to dereference the EAO context and its Decision in its own log
// line, so a pod panicked unless its profile had energy on, an EAO existed,
// and that EAO's status was already populated.
func TestBuild_EAOContextStates(t *testing.T) {
	cases := []struct {
		name    string
		energy  bool
		eao     *unstructured.Unstructured
		wantEAO bool
	}{
		{name: "energy off", energy: false},
		{name: "energy on, no EAO object", energy: true},
		{name: "energy on, EAO status not yet populated", energy: true,
			eao: testRebalanceEAO("Preferred", 250, "", "", true), wantEAO: true},
		{name: "energy on, EAO with decision", energy: true,
			eao: testRebalanceEAO("Critical", 500, "DeployImmediately", "window open", true), wantEAO: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod, rs := testDeploymentPod()
			objs := []client.Object{testRebalanceProfile(tc.energy), testAppDeployment(), rs, pod}
			if tc.eao != nil {
				objs = append(objs, tc.eao)
			}
			b := newTestBuilder(t, objs...)

			req, err := b.Build(context.Background(),
				PlacementContext{Pod: pod, CandidateNodes: []*corev1.Node{testNode("node-a", true)}}, "req-1")
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := req.EAOProfile != nil; got != tc.wantEAO {
				t.Errorf("EAO context attached = %v, want %v", got, tc.wantEAO)
			}
		})
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

// The gate follows the EAO's verdict, never energyMetrics.sufficient.
func TestCheckEnergyGate_FollowsTheVerdict(t *testing.T) {
	withSlot := func(eao *unstructured.Unstructured, start time.Time) *unstructured.Unstructured {
		_ = unstructured.SetNestedField(eao.Object, start.UTC().Format(time.RFC3339),
			"status", "decision", "scheduledSlot", "slotStart")
		return eao
	}
	ahead := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	started := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		eao         *unstructured.Unstructured
		holdActions []string // nil keeps the default mapping
		wantAllowed bool
		wantReason  string
	}{
		{name: "slot ahead holds the pod",
			eao:        withSlot(testRebalanceEAO("Optional", 200, "Scheduled", "optional: later slot", false), ahead),
			wantReason: "optional: later slot; held until 2099-01-01T00:00:00Z"},
		{name: "slot started lets it run even if the EAO lags",
			eao: withSlot(testRebalanceEAO("Optional", 200, "Scheduled", "", false), started), wantAllowed: true},
		{name: "Waiting is an EAO failure, not a hold",
			eao: testRebalanceEAO("Optional", 200, "Waiting", "", false), wantAllowed: true},
		{name: "DeployImmediately runs even when sufficient is false",
			eao: testRebalanceEAO("Critical", 500, "DeployImmediately", "", false), wantAllowed: true},
		{name: "a renamed hold action holds even when sufficient is true",
			eao: testRebalanceEAO("Optional", 200, "Paused", "", true), holdActions: []string{"Paused"},
			wantReason: "EAO holds the workload"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod, rs := testDeploymentPod()
			b := newTestBuilder(t, testRebalanceProfile(true), testAppDeployment(), rs, pod, tc.eao)
			if tc.holdActions != nil {
				b.eaoSettings.VerdictConfig.HoldActions = tc.holdActions
			}
			resp, err := b.CheckEnergyGate(context.Background(), pod)
			if err != nil || resp.Allowed != tc.wantAllowed || resp.Reason != tc.wantReason {
				t.Errorf("CheckEnergyGate = (%+v, %v), want allowed=%v reason=%q", resp, err, tc.wantAllowed, tc.wantReason)
			}
		})
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

func TestMapEAOToProfileContext_SlotFreshnessAndWholeWatts(t *testing.T) {
	eao := testRebalanceEAO("Optional", 200, "Scheduled", "waiting for solar", false)
	_ = unstructured.SetNestedMap(eao.Object, map[string]any{
		"slotStart": "2026-10-16T12:00:00Z", "slotEnd": "2026-10-16T18:00:00Z",
	}, "status", "decision", "scheduledSlot")
	_ = unstructured.SetNestedField(eao.Object, "2026-10-16T11:58:00Z", "status", "lastUpdated")
	_ = unstructured.SetNestedField(eao.Object, int64(200), "status", "energyMetrics", "requiredWatts")
	_ = unstructured.SetNestedField(eao.Object, 187.5, "status", "energyMetrics", "measuredWatts")

	got := mapEAOToProfileContext(eao)

	if got.Decision == nil || got.Decision.ScheduledSlot == nil ||
		got.Decision.ScheduledSlot.SlotStart != "2026-10-16T12:00:00Z" ||
		got.Decision.ScheduledSlot.SlotEnd != "2026-10-16T18:00:00Z" {
		t.Errorf("decision = %+v, want scheduledSlot 12:00–18:00", got.Decision)
	}
	if got.LastUpdated != "2026-10-16T11:58:00Z" {
		t.Errorf("lastUpdated = %q", got.LastUpdated)
	}
	if got.EnergyMetrics == nil || got.EnergyMetrics.RequiredWatts != 200 || got.EnergyMetrics.MeasuredWatts != 187.5 {
		t.Errorf("energyMetrics = %+v, want requiredWatts 200 (whole number) and measuredWatts 187.5", got.EnergyMetrics)
	}
}
