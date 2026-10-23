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
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/pkg/placement"
)

// HistoryReason is a short code for where a decision cycle ended. Deliberately not
// a CRD enum: adding a code is a code change only, never a CRD change.
type HistoryReason string

const (
	ReasonOther             HistoryReason = "Other"
	ReasonNoChange          HistoryReason = "NoChange"
	ReasonRejectedByAI      HistoryReason = "RejectedByAI"
	ReasonDeferredByAI      HistoryReason = "DeferredByAI"
	ReasonEscalatedByAI     HistoryReason = "EscalatedByAI"
	ReasonFailureStreak     HistoryReason = "FailureStreak"
	ReasonUnknownAction     HistoryReason = "UnknownAction"
	ReasonMalformedResponse HistoryReason = "MalformedResponse"
	ReasonAIUnavailable     HistoryReason = "AIUnavailable"
	ReasonContextError      HistoryReason = "ContextError"
	ReasonBelowThreshold    HistoryReason = "BelowThreshold"
	ReasonOutOfBounds       HistoryReason = "OutOfBounds"
	ReasonExternallyManaged HistoryReason = "ExternallyManaged"
	ReasonRateLimited       HistoryReason = "RateLimited"
	ReasonPDBBlocked        HistoryReason = "PDBBlocked"
	ReasonEnergyGateClosed  HistoryReason = "EnergyGateClosed"
	ReasonTimeout           HistoryReason = "Timeout"
	ReasonWrongNode         HistoryReason = "WrongNode"
	ReasonEnactFailed       HistoryReason = "EnactFailed"
	ReasonUnsupported       HistoryReason = "Unsupported"
	ReasonWatchdog          HistoryReason = "Watchdog"
	ReasonMoved             HistoryReason = "Moved"
	ReasonScaled            HistoryReason = "Scaled"
	ReasonResized           HistoryReason = "Resized"
	ReasonRetried           HistoryReason = "Retried"
)

// History defaults; override with REBALANCE_HISTORY_WINDOW / _MAX_ENTRIES / _MESSAGE_MAX.
const (
	DefaultHistoryWindow     = time.Hour
	DefaultHistoryMaxEntries = 20
	DefaultHistoryMessageMax = 200
)

// HistoryConfig bounds the history kept in profile status; zero fields use the defaults.
type HistoryConfig struct {
	Window     time.Duration
	MaxEntries int
	MessageMax int
}

func (c HistoryConfig) WithDefaults() HistoryConfig {
	if c.Window <= 0 {
		c.Window = DefaultHistoryWindow
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = DefaultHistoryMaxEntries
	}
	if c.MessageMax <= 0 {
		c.MessageMax = DefaultHistoryMessageMax
	}
	return c
}

// recordHistory adds entry at the front, merging it into the newest entry when it
// repeats it exactly, then drops entries outside the window and beyond the cap.
// Enacted never merges: each one is a real change that gates must count.
// Other never merges either: it's a catch-all, so two may be different problems.
func recordHistory(
	recent []orchestrationv1alpha1.HistoryEntry,
	entry orchestrationv1alpha1.HistoryEntry,
	cfg HistoryConfig,
	now time.Time,
) []orchestrationv1alpha1.HistoryEntry {
	entry.Message = truncate(entry.Message, cfg.MessageMax)
	entry.DecidedBy = truncate(entry.DecidedBy, cfg.MessageMax) // agent-supplied, so bounded too
	mergeable := entry.Outcome != OutcomeEnacted && entry.Reason != string(ReasonOther)
	if len(recent) > 0 && mergeable && startedInWindow(recent[0], cfg, now) && sameDecision(recent[0], entry) {
		merged := recent[0]
		if merged.Count == 0 {
			merged.Count, merged.FirstAt = 1, merged.At // first repeat: start counting
		}
		merged.Count++
		merged.Message = entry.Message
		merged.At = entry.At
		recent = append([]orchestrationv1alpha1.HistoryEntry{merged}, recent[1:]...)
	} else {
		recent = append([]orchestrationv1alpha1.HistoryEntry{entry}, recent...)
	}
	return trimHistory(recent, cfg, now)
}

// trimHistory keeps entries whose latest occurrence is inside the window, up to the cap.
func trimHistory(
	recent []orchestrationv1alpha1.HistoryEntry,
	cfg HistoryConfig,
	now time.Time,
) []orchestrationv1alpha1.HistoryEntry {
	cutoff := now.Add(-cfg.Window)
	kept := make([]orchestrationv1alpha1.HistoryEntry, 0, len(recent))
	for _, e := range recent {
		if e.At.Time.Before(cutoff) {
			continue
		}
		kept = append(kept, e)
		if len(kept) == cfg.MaxEntries {
			break
		}
	}
	return kept
}

// startedInWindow keeps count to "within the window": an older run starts a new entry instead.
func startedInWindow(e orchestrationv1alpha1.HistoryEntry, cfg HistoryConfig, now time.Time) bool {
	start := e.At
	if e.Count > 0 {
		start = e.FirstAt
	}
	return !start.Time.Before(now.Add(-cfg.Window))
}

// sameDecision is true when two entries differ only in timing and wording, so merging
// them loses nothing: same action, outcome, reason and change facts.
func sameDecision(a, b orchestrationv1alpha1.HistoryEntry) bool {
	a.Message, b.Message = "", ""
	a.Count, b.Count = 0, 0
	a.At, b.At = metav1.Time{}, metav1.Time{}
	a.FirstAt, b.FirstAt = metav1.Time{}, metav1.Time{}
	return a == b
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}

// rebalanceState builds the request's history and streak facts from the profile status,
// re-trimming by the window so entries don't outlive it when nothing has been written for a while.
func rebalanceState(
	rs orchestrationv1alpha1.RebalancingStatus,
	cfg HistoryConfig,
	escalationThreshold int32,
	now time.Time,
) placement.RebalanceState {
	cfg = cfg.WithDefaults()
	if escalationThreshold <= 0 {
		escalationThreshold = DefaultEscalationThreshold
	}
	h := placement.RebalanceState{
		History:             trimHistory(rs.History, cfg, now),
		HistoryWindow:       metav1.Duration{Duration: cfg.Window},
		ConsecutiveFailures: rs.ConsecutiveFailures,
		EscalationThreshold: escalationThreshold,
	}
	if rs.CooldownUntil.After(now) {
		until := rs.CooldownUntil
		h.CooldownUntil = &until
	}
	return h
}
