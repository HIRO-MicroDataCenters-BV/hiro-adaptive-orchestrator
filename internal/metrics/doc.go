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

// Package metrics holds every custom Prometheus metric this operator
// exposes, across every subsystem (rebalance, placement, controller,
// webhook) — one file per subsystem, all registered against
// controller-runtime's own metrics.Registry, which the manager already
// serves on /metrics. No separate metrics server or endpoint is needed.
//
// This package is a leaf dependency: it must never import any package that
// imports it back (internal/rebalance, internal/placement-server,
// internal/controller, internal/webhook/v1 all import this package, not the
// other way round).
//
// Naming convention: every metric is prefixed hiro_<subsystem>_, where
// subsystem matches the file it's defined in (rebalance, placement,
// controller, webhook).
//
// Deliberately NOT covered here, because it's already provided elsewhere
// with no code needed:
//   - Both OrchestrationProfileReconciler and the rebalance engine's own
//     Reconciler are registered the standard controller-runtime way (see
//     their SetupWithManager), so controller_runtime_reconcile_total,
//     _reconcile_errors_total, and _reconcile_time_seconds — labeled by
//     controller name — already exist for both, per-controller, for free.
//   - The pod-defaulting webhook is registered via ctrl.NewWebhookManagedBy,
//     which auto-exposes controller_runtime_webhook_requests_total and
//     _webhook_latency_seconds the same way.
//   - The kube-scheduler HIROScore plugin (scheduler-plugin/) runs in
//     kube-scheduler's process, which never serves this package's registry.
//     Its metrics live in scheduler-plugin/metrics.go, registered against
//     component-base's legacyregistry instead — registering one here would
//     compile, increment, and never be exposed.
//
// What IS covered here is the domain-specific "why" that those generic
// request/error/duration metrics can't express — e.g. not just "how many
// rebalance reconciles happened" but "how many were rejected by which
// guardrail."
//
// docs/instrumentation.md is the full reference: every metric on both
// endpoints, every label value, and the free metrics this package relies on.
package metrics
