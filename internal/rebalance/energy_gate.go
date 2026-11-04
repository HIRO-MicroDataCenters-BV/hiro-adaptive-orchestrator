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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// classifyScheduleTimeout labels a scheduling timeout Deferred if the energy gate holds right now, else Failed.
func classifyScheduleTimeout(
	ctx context.Context,
	c client.Client,
	eaoSettings placementserver.EAOSettings,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	timeoutReason string,
) (orchestrationv1alpha1.RebalanceOutcome, string) {
	// Read fresh: the gate can flip within the enactor's own timeout.
	hold := energyGateHold(ctx, c, eaoSettings, profile)
	if hold == "" {
		return OutcomeFailed, timeoutReason
	}
	return OutcomeDeferred, timeoutReason + " (" + hold + ")"
}

// energyGateHold says why the energy gate would hold the profile's new pods now, or "" if that can't be confirmed.
func energyGateHold(
	ctx context.Context,
	c client.Client,
	eaoSettings placementserver.EAOSettings,
	profile *orchestrationv1alpha1.OrchestrationProfile,
) string {
	// Same order as CheckEnergyGate, so a workload the gate never blocks isn't held by an unrelated EAO.
	if !profile.Spec.Placement.Awareness.Energy {
		return ""
	}

	eao, err := utils.FindEAOForApp(ctx, c, eaoSettings.GVK, profile.Spec.ApplicationRef)
	if err != nil || eao == nil {
		return ""
	}

	eaoCtx := placementserver.NewEAOContext(eao, eaoSettings.VerdictConfig, time.Now())
	if eaoCtx.Verdict != placement.EAOVerdictHold {
		return ""
	}
	return placementserver.HoldReason(eaoCtx)
}
