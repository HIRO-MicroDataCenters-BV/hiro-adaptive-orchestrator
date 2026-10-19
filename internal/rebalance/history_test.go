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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

func entryAt(at time.Time, reason HistoryReason) orchestrationv1alpha1.HistoryEntry {
	return orchestrationv1alpha1.HistoryEntry{Outcome: OutcomeNoOp, Reason: string(reason), At: metav1.NewTime(at)}
}

func TestRebalanceState_TrimsByWindowAtReadTime(t *testing.T) {
	now := time.Now()
	rs := orchestrationv1alpha1.RebalancingStatus{
		History: []orchestrationv1alpha1.HistoryEntry{
			entryAt(now.Add(-10*time.Minute), ReasonNoChange),
			entryAt(now.Add(-2*time.Hour), ReasonBelowThreshold),
		},
		ConsecutiveFailures: 2,
	}

	s := rebalanceState(rs, HistoryConfig{}, 5, now)
	if len(s.History) != 1 || s.History[0].Reason != string(ReasonNoChange) {
		t.Errorf("history = %+v, want only the entry inside the 1h window", s.History)
	}
	if s.HistoryWindow.Duration != DefaultHistoryWindow || s.ConsecutiveFailures != 2 || s.EscalationThreshold != 5 {
		t.Errorf("state = %+v, want default window, 2 failures, threshold 5", s)
	}
}

func TestRebalanceState_Cooldown(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		until metav1.Time
		want  bool
	}{
		{"active cooldown is sent", metav1.NewTime(now.Add(time.Minute)), true},
		{"past cooldown is nil", metav1.NewTime(now.Add(-time.Minute)), false},
		{"unset cooldown is nil", metav1.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := rebalanceState(orchestrationv1alpha1.RebalancingStatus{CooldownUntil: tc.until}, HistoryConfig{}, 0, now)
			if (h.CooldownUntil != nil) != tc.want {
				t.Errorf("cooldownUntil = %v, want set=%v", h.CooldownUntil, tc.want)
			}
		})
	}
}

func TestRebalanceState_UnsetThresholdUsesDefault(t *testing.T) {
	h := rebalanceState(orchestrationv1alpha1.RebalancingStatus{}, HistoryConfig{}, 0, time.Now())
	if h.EscalationThreshold != DefaultEscalationThreshold {
		t.Errorf("escalationThreshold = %d, want default %d", h.EscalationThreshold, DefaultEscalationThreshold)
	}
}
