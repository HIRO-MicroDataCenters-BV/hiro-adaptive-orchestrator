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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ControllerProfileStatusTransitionsTotal counts a change to
// OrchestrationProfileStatus.Status — generic reconcile success/error/
// duration already exists for free via controller-runtime's own metrics
// (see internal/metrics's doc comment); this is the domain-specific "what
// did it actually settle on" that those don't express. Only fired when the
// status value actually changes, same gating updateStatus already applies
// to its own event emission — a counter incrementing on every reconcile
// regardless of change would just track reconcile frequency again.
var ControllerProfileStatusTransitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "hiro_controller_profile_status_transitions_total",
	Help: "Total OrchestrationProfile status transitions, by from-status and to-status.",
}, []string{"from", "to"})

func init() {
	metrics.Registry.MustRegister(ControllerProfileStatusTransitionsTotal)
}
