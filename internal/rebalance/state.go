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
// state machine: Detection (Triggered) → Decision (Evaluating → Decided) →
// Enaction (Enacting), always returning to Watching. The outcome of a cycle
// (Enacted, NoOp, Rejected, Deferred, Failed) is not a `state` value — it is
// recorded in RecentDecisions alongside the Watching transition that ends
// the cycle.
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
	StateWatching   = orchestrationv1alpha1.RebalancingStateWatching
	StateTriggered  = orchestrationv1alpha1.RebalancingStateTriggered
	StateEvaluating = orchestrationv1alpha1.RebalancingStateEvaluating
	StateDecided    = orchestrationv1alpha1.RebalancingStateDecided
	StateEnacting   = orchestrationv1alpha1.RebalancingStateEnacting

	// stateNone represents a profile that has never entered the state
	// machine (status.rebalancingStatus.state is empty). Treated the same
	// as StateWatching everywhere below.
	stateNone = orchestrationv1alpha1.RebalancingStateType("")
)

// Outcome type aliases for readability at call sites.
const (
	OutcomeEnacted  = orchestrationv1alpha1.RebalanceOutcomeEnacted
	OutcomeNoOp     = orchestrationv1alpha1.RebalanceOutcomeNoOp
	OutcomeRejected = orchestrationv1alpha1.RebalanceOutcomeRejected
	OutcomeDeferred = orchestrationv1alpha1.RebalanceOutcomeDeferred
	OutcomeFailed   = orchestrationv1alpha1.RebalanceOutcomeFailed
)

// validTransitions is the authoritative transition table for the decision
// lifecycle. A cycle always ends by returning to Watching — the outcome of
// that cycle (Enacted/NoOp/Rejected/Deferred/Failed) is recorded separately
// in RecentDecisions, not as a `state` value. stateNone and StateWatching
// are equivalent resting positions.
var validTransitions = map[orchestrationv1alpha1.RebalancingStateType][]orchestrationv1alpha1.RebalancingStateType{
	stateNone:       {StateTriggered},
	StateWatching:   {StateTriggered},
	StateTriggered:  {StateEvaluating},
	StateEvaluating: {StateDecided, StateWatching},
	// Decided -> Watching (in addition to the normal Decided -> Enacting)
	// covers dispatchMove's cluster-wide rate-limit wait (Story 31): an
	// accepted Move is recorded in Decided, but if MoveRateLimiter can't
	// grant a token before MoveRateWaitTimeout, the cycle never actually
	// attempts enactment — it fails straight back to Watching (Outcome
	// Failed), the same way Evaluating already exits straight to Watching
	// for NoOp/threshold-rejected without ever claiming to have Decided.
	StateDecided:  {StateEnacting, StateWatching},
	StateEnacting: {StateWatching},
}

// IsValidTransition reports whether moving from `from` to `to` is allowed by
// the decision lifecycle state machine.
func IsValidTransition(from, to orchestrationv1alpha1.RebalancingStateType) bool {
	return slices.Contains(validTransitions[from], to)
}
