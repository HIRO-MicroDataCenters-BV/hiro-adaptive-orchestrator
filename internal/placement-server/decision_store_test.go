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
	"testing"
	"time"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

func TestWorkloadKey(t *testing.T) {
	if got, want := WorkloadKey("default", "my-app"), "default/my-app"; got != want {
		t.Errorf("WorkloadKey() = %q, want %q", got, want)
	}
}

func TestDecisionStore_PutLookup_Hit(t *testing.T) {
	s := NewDecisionStore(time.Minute)
	key := WorkloadKey("default", "my-app")
	s.Put(key, "pod-a", orchestrationv1alpha1.RebalanceActionMove, "node-2", "energy window", "decision-1")

	entry, ok := s.Lookup(key)
	if !ok {
		t.Fatal("Lookup() ok = false, want true")
	}
	if entry.PodName != "pod-a" || entry.TargetNode != "node-2" || entry.DecisionID != "decision-1" {
		t.Errorf("Lookup() entry = %+v, unexpected fields", entry)
	}
	if entry.Action != orchestrationv1alpha1.RebalanceActionMove {
		t.Errorf("Lookup() Action = %q, want Move", entry.Action)
	}
}

func TestDecisionStore_Lookup_Miss(t *testing.T) {
	s := NewDecisionStore(time.Minute)
	if _, ok := s.Lookup(WorkloadKey("default", "nonexistent")); ok {
		t.Error("Lookup() ok = true for a key that was never Put, want false")
	}
}

func TestDecisionStore_Lookup_SelfConsumes(t *testing.T) {
	s := NewDecisionStore(time.Minute)
	key := WorkloadKey("default", "my-app")
	s.Put(key, "pod-a", orchestrationv1alpha1.RebalanceActionMove, "node-2", "reason", "decision-1")

	if _, ok := s.Lookup(key); !ok {
		t.Fatal("first Lookup() ok = false, want true")
	}
	if _, ok := s.Lookup(key); ok {
		t.Error("second Lookup() ok = true, want false — entry should self-consume on first hit")
	}
}

func TestDecisionStore_Lookup_ExpiredTreatedAsMiss(t *testing.T) {
	s := NewDecisionStore(1 * time.Millisecond)
	key := WorkloadKey("default", "my-app")
	s.Put(key, "pod-a", orchestrationv1alpha1.RebalanceActionMove, "node-2", "reason", "decision-1")

	time.Sleep(5 * time.Millisecond)

	if _, ok := s.Lookup(key); ok {
		t.Error("Lookup() ok = true for an expired entry, want false")
	}
}

func TestDecisionStore_Delete(t *testing.T) {
	s := NewDecisionStore(time.Minute)
	key := WorkloadKey("default", "my-app")
	s.Put(key, "pod-a", orchestrationv1alpha1.RebalanceActionMove, "node-2", "reason", "decision-1")

	s.Delete(key)

	if _, ok := s.Lookup(key); ok {
		t.Error("Lookup() ok = true after Delete(), want false")
	}
}

func TestDecisionStore_Delete_NonexistentKeyIsNoop(t *testing.T) {
	s := NewDecisionStore(time.Minute)
	s.Delete(WorkloadKey("default", "nonexistent")) // must not panic
}

func TestNewDecisionStore_NonPositiveTTLUsesDefault(t *testing.T) {
	s := NewDecisionStore(0)
	if s.ttl != DefaultDecisionStoreTTL {
		t.Errorf("ttl = %v, want DefaultDecisionStoreTTL (%v)", s.ttl, DefaultDecisionStoreTTL)
	}
}
