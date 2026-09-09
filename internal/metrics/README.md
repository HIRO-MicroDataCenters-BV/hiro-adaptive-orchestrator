# Metrics

Every custom Prometheus metric this operator exposes, across every subsystem — one file per
subsystem (`rebalance.go`, `placement.go`, `controller.go`, `webhook.go`), all registered
against controller-runtime's own `metrics.Registry`, which the manager already serves on
`/metrics`. No separate metrics server or endpoint needed.

This package is a **leaf dependency**: `internal/rebalance`, `internal/placement-server`,
`internal/controller`, and `internal/webhook/v1` all import it — it never imports any of them
back.

## Already free, not duplicated here

- **Both reconcilers** (`OrchestrationProfileReconciler` and the rebalance engine's own
  `Reconciler`) are registered the standard controller-runtime way, so
  `controller_runtime_reconcile_total`, `_reconcile_errors_total`, and
  `_reconcile_time_seconds` — labeled by controller name (`orchestrationprofile` /
  `rebalance-detection`) — already exist for both.
- **The pod-defaulting webhook** is registered via `ctrl.NewWebhookManagedBy`, which
  auto-exposes `controller_runtime_webhook_requests_total` and `_webhook_latency_seconds`
  the same way.
- **The `HIROScore` kube-scheduler plugin** (`scheduler-plugin/`) runs inside kube-scheduler's
  own process, not this operator's — a separate binary with its own `/metrics` endpoint, and
  the scheduling framework itself already instruments every plugin's Filter/Score/PreScore
  extension-point latency and outcome. Out of scope for this package.

What's covered below is the domain-specific "why" those generic request/error/duration metrics
can't express.

## Rebalance (`rebalance.go`)

| Metric | Type | Labels | What it means |
|---|---|---|---|
| `hiro_rebalance_transitions_total` | Counter | `from`, `to`, `action`, `outcome` | Every state transition. Not labeled by the free-text `reason` — unbounded cardinality. |
| `hiro_rebalance_guardrail_rejections_total` | Counter | `action`, `guardrail` (`threshold`\|`bounds`) | A recommendation a dispatch guardrail refused, before reaching `Decided`. |
| `hiro_rebalance_rate_limit_exhausted_total` | Counter | `action` | An accepted Move that still failed the cluster-wide rate-limit wait. |
| `hiro_rebalance_cooldown_skips_total` | Counter | — | A reconcile that skipped AI consultation because the profile is still in cooldown. |
| `hiro_rebalance_improvement_score` | Histogram | `action` | Distribution of `Improvement` scores the AI returns, regardless of guardrail outcome. |
| `hiro_rebalance_profiles_by_state` | Gauge | `state` | Live count of profiles per rebalancing state, recomputed on every scrape (not incrementally maintained — see `rebalance.go`'s doc comment on why). |

## Placement (`placement.go`)

Covers both the scheduler-plugin path (`/api/v1/placement/*`) and the kube-scheduler extender
path (`/extender/*`) in one place — both protocols share the same underlying `score()`/
`filter()` methods, distinguished only by the `path` label (`plugin`\|`extender`).

| Metric | Type | Labels | What it means |
|---|---|---|---|
| `hiro_placement_score_requests_total` | Counter | `path`, `result` (`store_hit`\|`ai_success`\|`ai_error`) | Every scoring request. |
| `hiro_placement_score_duration_seconds` | Histogram | `path` | Duration of scoring, including a DecisionStore hit (near-zero) or the full AI round trip. |
| `hiro_placement_filter_requests_total` | Counter | `path`, `allowed` | Every energy-gate check that completed without error. |
| `hiro_placement_filter_errors_total` | Counter | `path` | An energy-gate error that was soft-failed open (the pod was allowed anyway). |
| `hiro_placement_decision_store_hits_total` / `_misses_total` | Counter | — | Raw `DecisionStore.Lookup` counts — a ratio is a PromQL/Grafana concern, not computed here. |

## Controller (`controller.go`)

| Metric | Type | Labels | What it means |
|---|---|---|---|
| `hiro_controller_profile_status_transitions_total` | Counter | `from`, `to` | An `OrchestrationProfileStatus.Status` change. Only fires on an actual change, same gating `updateStatus` already applies to its own event emission. |

## Webhook (`webhook.go`)

| Metric | Type | Labels | What it means |
|---|---|---|---|
| `hiro_webhook_pod_default_outcomes_total` | Counter | `outcome` (`mutated`\|`already_set`\|`no_profile`\|`lookup_error`) | What the pod-defaulting webhook actually did. |
