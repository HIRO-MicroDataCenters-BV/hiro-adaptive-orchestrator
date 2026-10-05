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

package controller

import (
	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Profile Status Constants — aliases of the canonical api/v1alpha1 enum values.
// -----------------------------------------------------------------------------

const (
	StatusNoPods   = orchestrationv1alpha1.ProfileStatusNoPods
	StatusPartial  = orchestrationv1alpha1.ProfileStatusPartial
	StatusActive   = orchestrationv1alpha1.ProfileStatusActive
	StatusPending  = orchestrationv1alpha1.ProfileStatusPending
	StatusDegraded = orchestrationv1alpha1.ProfileStatusDegraded
	StatusError    = orchestrationv1alpha1.ProfileStatusError
)

// ProfileByAppRefIndex is the field index key for OrchestrationProfile applicationRef lookups
// Index value: "<appNamespace>/<appName>" derived from spec.applicationRef
const ProfileByAppRefIndex = ".spec.applicationRef.namespacedName"

// -----------------------------------------------------------------------------
// Event Reasons
// -----------------------------------------------------------------------------

const (
	// EventReasonValidationFailed is emitted when the OrchestrationProfile spec fails validation.
	EventReasonValidationFailed = "ValidationFailed"

	// EventReasonApplicationNotFound is emitted when the referenced application does not exist yet.
	EventReasonApplicationNotFound = "ApplicationNotFound"

	// EventReasonApplicationLookupError is emitted when the existence check for the referenced app fails.
	EventReasonApplicationLookupError = "ApplicationLookupError"

	// EventReasonPodDiscoveryFailed is emitted when pod discovery for the application fails.
	EventReasonPodDiscoveryFailed = "PodDiscoveryFailed"

	// EventReasonPlacementActive is emitted when all pods are running and ready.
	EventReasonPlacementActive = "PlacementActive"

	// EventReasonPlacementPending is emitted when all pods are still being scheduled.
	EventReasonPlacementPending = "PlacementPending"

	// EventReasonApplicationNoPods is emitted when the application exists but has no pods yet.
	EventReasonApplicationNoPods = "ApplicationNoPods"

	// EventReasonPlacementPartial is emitted when some pods are ready and others are not.
	EventReasonPlacementPartial = "PlacementPartial"

	// EventReasonPlacementDegraded is emitted when one or more pods have failed.
	EventReasonPlacementDegraded = "PlacementDegraded"

	// EventReasonPlacementError is emitted when an unrecoverable error occurs.
	EventReasonPlacementError = "PlacementError"
)

// -----------------------------------------------------------------------------
// Validation Maps
// -----------------------------------------------------------------------------

// validStrategies defines the supported placement strategies.
var validStrategies = map[orchestrationv1alpha1.PlacementStrategy]bool{
	orchestrationv1alpha1.PlacementStrategyBalanced: true,
	orchestrationv1alpha1.PlacementStrategyPacked:   true,
	orchestrationv1alpha1.PlacementStrategySpread:   true,
}

// validTriggerConditions defines the supported rebalancing trigger conditions.
var validTriggerConditions = map[orchestrationv1alpha1.TriggerCondition]bool{
	orchestrationv1alpha1.TriggerConditionCPUThreshold:    true,
	orchestrationv1alpha1.TriggerConditionMemoryThreshold: true,
	orchestrationv1alpha1.TriggerConditionEnergyThreshold: true,
	orchestrationv1alpha1.TriggerConditionNodeFailure:     true,
	orchestrationv1alpha1.TriggerConditionScheduled:       true,
}

// supportedAppKinds defines the workload kinds the controller can resolve
// label selectors for.
var supportedAppKinds = map[orchestrationv1alpha1.ApplicationKind]bool{
	orchestrationv1alpha1.ApplicationKindDeployment:  true,
	orchestrationv1alpha1.ApplicationKindStatefulSet: true,
	orchestrationv1alpha1.ApplicationKindReplicaSet:  true,
	orchestrationv1alpha1.ApplicationKindJob:         true,
}
