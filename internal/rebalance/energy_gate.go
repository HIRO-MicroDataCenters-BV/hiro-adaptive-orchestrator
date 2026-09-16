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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// classifyScheduleTimeout decides how an enactor should report a timeout
// waiting for a scheduling-dependent end-state (a replacement pod scheduled,
// a target ReadyReplicas count) to appear: outcome Failed (genuinely
// unexplained — needs investigation) or Deferred (very likely explained by
// this operator's own energy gate being closed right now, which will
// self-resolve once it opens). Only meaningful for a timeout that involves
// real pod scheduling — the energy gate is a scheduler extender, so it can
// never explain a timeout that doesn't involve the scheduler at all (e.g.
// an in-place resize, which never goes through scheduling).
//
// Checked fresh, right here, rather than reusing any energy snapshot taken
// earlier in the cycle (e.g. the one sent to the AI during Evaluating) — the
// gate can flip open/closed well within an enactor's own timeout window, so
// only a read taken at the moment of timeout is trustworthy.
//
// Mirrors CheckEnergyGate's own gating precisely (internal/placement-server/
// builder.go): checks profile.Spec.Placement.Awareness.Energy first, same as
// the gate itself does, so a workload the gate would never have blocked in
// the first place isn't misclassified just because some unrelated EAO
// happens to exist and happens to be insufficient. Every "can't confirm"
// case (awareness off, no matching EAO, lookup error, sufficient field not
// yet populated) falls back to the original Failed classification — this
// only ever reclassifies toward Deferred on a positive, direct signal, never
// guesses.
func classifyScheduleTimeout(
	ctx context.Context,
	c client.Client,
	eaoGVK schema.GroupVersionKind,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	timeoutReason string,
) (orchestrationv1alpha1.RebalanceOutcome, string) {
	if !profile.Spec.Placement.Awareness.Energy {
		return OutcomeFailed, timeoutReason
	}

	eao, err := utils.FindEAOForApp(ctx, c, eaoGVK, profile.Spec.ApplicationRef)
	if err != nil || eao == nil {
		return OutcomeFailed, timeoutReason
	}

	sufficient, found, _ := unstructured.NestedBool(eao.Object, "status", "energyMetrics", "sufficient")
	if !found || sufficient {
		return OutcomeFailed, timeoutReason
	}

	reason, _, _ := unstructured.NestedString(eao.Object, "status", "decision", "reason")
	if reason == "" {
		reason = "energy supply reported insufficient"
	}
	return OutcomeDeferred, timeoutReason + " (energy gate closed: " + reason + ")"
}
