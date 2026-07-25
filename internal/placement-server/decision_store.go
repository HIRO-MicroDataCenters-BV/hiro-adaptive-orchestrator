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
	"sync"
	"time"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// DefaultDecisionStoreTTL bounds how long a decision recorded by the
// rebalance engine remains valid for PlacementServer.score to take into
// account before it's treated as stale and a fresh AI call is made instead.
const DefaultDecisionStoreTTL = 60 * time.Second

// DecisionStoreEntry is one recorded rebalance decision the scheduler should
// take into account the next time it scores a pod for this workload.
//
// PodName is the pod the decision was made about (e.g. the one being
// evicted) — kept for traceability in logs/debugging even though lookups
// are keyed by workload, not by pod name: a Move enactor evicts PodName and
// its owning controller (Deployment/StatefulSet/etc.) creates a *new* Pod
// object with a different generated name, so there is no pod-name
// correlation available between PodName and whatever pod PlacementServer is
// actually scoring when it later consults this entry.
//
// TargetNode/Reason are populated for Action == Move (the only enactor that
// exists today and the only one that needs to bias node scoring); other,
// future action types (AdjustResources, AdjustReplicas, Defer, Escalate)
// don't reschedule a pod onto a specific node, so they'd leave TargetNode
// empty if they ever wrote here at all. The type isn't Move-specific so a
// later enactor can reuse it without a reshape, but PlacementServer.score
// today only special-cases Action == Move.
type DecisionStoreEntry struct {
	PodName    string
	Action     orchestrationv1alpha1.RebalanceAction
	TargetNode string
	Reason     string
	DecisionID string
	ExpiresAt  time.Time
}

// DecisionStore is a concurrency-safe, TTL-bounded in-memory map from
// workload identity to a pending rebalance decision.
//
// Keyed by workload identity (OrchestrationProfile namespace/name via
// WorkloadKey), not by any specific pod name — see DecisionStoreEntry's
// PodName doc comment for why pod-name correlation isn't available. Keying
// by workload instead means "the next pod scheduled for this workload
// should take this decision into account" — which is exactly the
// replacement, since only one rebalance cycle runs at a time per workload
// (cooldown prevents overlap).
//
// Written by an enactor (internal/rebalance) before it takes any
// user-visible action, read by PlacementServer.score on every scoring
// request. Lookup self-consumes: the entry is removed the moment any pod
// for that workload is scored, not left live for the rest of the TTL —
// otherwise a concurrent, unrelated pod for the same workload (e.g. a
// scale-up racing the rebalance cycle) could also pick up the same node
// hint. The owning enactor's own Delete call at its terminal transition is
// therefore a no-op safety net for the case where scoring never actually
// happens (e.g. the eviction never produces a schedulable replacement) —
// TTL expiry is the final backstop for that same case.
type DecisionStore struct {
	mu      sync.Mutex
	entries map[string]DecisionStoreEntry
	ttl     time.Duration
}

// NewDecisionStore creates a DecisionStore. ttl <= 0 uses DefaultDecisionStoreTTL.
func NewDecisionStore(ttl time.Duration) *DecisionStore {
	if ttl <= 0 {
		ttl = DefaultDecisionStoreTTL
	}
	return &DecisionStore{
		entries: make(map[string]DecisionStoreEntry),
		ttl:     ttl,
	}
}

// WorkloadKey builds the DecisionStore key for a workload identified by
// OrchestrationProfile namespace/name. Every caller (writing enactors,
// PlacementServer.score reading) must derive the key this same way.
func WorkloadKey(namespace, name string) string {
	return namespace + "/" + name
}

// Put records a decision for key, valid for the store's configured TTL from now.
func (s *DecisionStore) Put(key, podName string, action orchestrationv1alpha1.RebalanceAction, targetNode, reason, decisionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = DecisionStoreEntry{
		PodName:    podName,
		Action:     action,
		TargetNode: targetNode,
		Reason:     reason,
		DecisionID: decisionID,
		ExpiresAt:  time.Now().Add(s.ttl),
	}
}

// Lookup returns the live entry for key, if any, and consumes it — see the
// DecisionStore doc comment for why self-consuming on first hit matters.
// An expired entry is treated as a miss.
func (s *DecisionStore) Lookup(key string) (DecisionStoreEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return DecisionStoreEntry{}, false
	}
	delete(s.entries, key)
	if time.Now().After(entry.ExpiresAt) {
		return DecisionStoreEntry{}, false
	}
	return entry, true
}

// Delete removes any entry for key. Safe to call even if no entry exists —
// callers clear unconditionally on every terminal transition, even though
// Lookup's self-consuming behavior means there's usually nothing left to
// remove by then.
func (s *DecisionStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}
