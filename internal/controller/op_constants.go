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

// -----------------------------------------------------------------------------
// Profile Status Constants
// -----------------------------------------------------------------------------

const (
	// StatusActive indicates all pods of the referenced application are running and ready.
	StatusActive = "Active"

	// StatusPending indicates the application or its pods do not exist yet.
	StatusPending = "Pending"

	// StatusDegraded indicates the application is partially running (some pods failed or pending).
	StatusDegraded = "Degraded"

	// StatusError indicates the profile spec is invalid or an unrecoverable error occurred.
	StatusError = "Error"
)

// -----------------------------------------------------------------------------
// Validation Maps
// -----------------------------------------------------------------------------

// validStrategies defines the supported placement strategies.
var validStrategies = map[string]bool{
	"BestFit":     true,
	"MostFit":     true,
	"LeastFit":    true,
	"EnergyAware": true,
	"Spread":      true,
}

// validTriggerConditions defines the supported rebalancing trigger conditions.
var validTriggerConditions = map[string]bool{
	"CPUThreshold":    true,
	"MemoryThreshold": true,
	"EnergyThreshold": true,
	"NodeFailure":     true,
	"Scheduled":       true,
}

// supportedAppKinds defines the workload kinds the controller can resolve
// label selectors for.
var supportedAppKinds = map[string]bool{
	"Deployment":  true,
	"StatefulSet": true,
	"ReplicaSet":  true,
	"Job":         true,
}
