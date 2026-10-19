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

import "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"

// Wire-format types shared by the PlacementServer, the scheduler plugin and
// the Decision Service. Canonical definitions live in pkg/placement so modules
// with their own go.mod (scheduler-plugin, ai-decision-service) can import
// them without violating Go's internal/ visibility rules. These aliases keep
// existing operator code unchanged.
type (
	DecisionRequest           = placement.DecisionRequest
	RebalanceContext          = placement.RebalanceContext
	RebalanceHistory          = placement.RebalanceHistory
	RebalanceTrigger          = placement.RebalanceTrigger
	RebalanceWorkload         = placement.RebalanceWorkload
	RebalancePDB              = placement.RebalancePDB
	RebalanceDecisionResponse = placement.RebalanceDecisionResponse
	ResourceRequirements      = placement.ResourceRequirements
	AOProfileContext          = placement.AOProfileContext
	AwarenessFlags            = placement.AwarenessFlags
	PodPlacement              = placement.PodPlacement
	ContainerInfo             = placement.ContainerInfo
	RebalancingConfig         = placement.RebalancingConfig
	NodeStat                  = placement.NodeStat
	NodeResource              = placement.NodeResource
	NodePlatform              = placement.NodePlatform
	EAOProfileContext         = placement.EAOProfileContext
	EAODecision               = placement.EAODecision
	EAOScheduledSlot          = placement.EAOScheduledSlot
	EAOEnergyMetrics          = placement.EAOEnergyMetrics
	NodeEnergyData            = placement.NodeEnergyData
	DecisionResponse          = placement.DecisionResponse
	NodeScore                 = placement.NodeScore
	PlacementContext          = placement.PlacementContext
)
