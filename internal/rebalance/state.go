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

// Package rebalance implements the rebalance engine's decision lifecycle
// state machine: Detection (Triggered) → Decision (Evaluating → Decided /
// NoOp / Rejected / Failed) → Enaction (Enacting → Enacted / Deferred /
// Failed).
//
// Every transition MUST go through StateWriter (writer.go) — no other code
// in this codebase is permitted to mutate status.rebalancingStatus directly.
package rebalance

import (
	"slices"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// State type aliases for readability at call sites.
const (
	StateTriggered  = orchestrationv1alpha1.RebalancingStateTriggered
	StateEvaluating = orchestrationv1alpha1.RebalancingStateEvaluating
	StateDecided    = orchestrationv1alpha1.RebalancingStateDecided
	StateEnacting   = orchestrationv1alpha1.RebalancingStateEnacting
	StateEnacted    = orchestrationv1alpha1.RebalancingStateEnacted
	StateNoOp       = orchestrationv1alpha1.RebalancingStateNoOp
	StateRejected   = orchestrationv1alpha1.RebalancingStateRejected
	StateDeferred   = orchestrationv1alpha1.RebalancingStateDeferred
	StateFailed     = orchestrationv1alpha1.RebalancingStateFailed

	// stateNone represents a profile that has never entered the state
	// machine (status.rebalancingStatus.state is empty).
	stateNone = orchestrationv1alpha1.RebalancingStateType("")
)

// validTransitions is the authoritative transition table for the decision
// lifecycle. Every terminal state (and the unset initial state) can only
// move forward into Triggered — starting a fresh cycle after cooldown.
var validTransitions = map[orchestrationv1alpha1.RebalancingStateType][]orchestrationv1alpha1.RebalancingStateType{
	stateNone:       {StateTriggered},
	StateTriggered:  {StateEvaluating},
	StateEvaluating: {StateDecided, StateNoOp, StateRejected, StateFailed},
	StateDecided:    {StateEnacting},
	StateEnacting:   {StateEnacted, StateDeferred, StateFailed},
	StateEnacted:    {StateTriggered},
	StateNoOp:       {StateTriggered},
	StateRejected:   {StateTriggered},
	StateDeferred:   {StateTriggered},
	StateFailed:     {StateTriggered},
}

// terminalStates are the states where a decision cycle ends. Each one
// starts the workload's cooldown and appends a record to RecentDecisions.
var terminalStates = map[orchestrationv1alpha1.RebalancingStateType]bool{
	StateEnacted:  true,
	StateNoOp:     true,
	StateRejected: true,
	StateDeferred: true,
	StateFailed:   true,
}

// IsValidTransition reports whether moving from `from` to `to` is allowed by
// the decision lifecycle state machine.
func IsValidTransition(from, to orchestrationv1alpha1.RebalancingStateType) bool {
	return slices.Contains(validTransitions[from], to)
}

// IsTerminal reports whether the given state ends a decision cycle.
func IsTerminal(s orchestrationv1alpha1.RebalancingStateType) bool {
	return terminalStates[s]
}
