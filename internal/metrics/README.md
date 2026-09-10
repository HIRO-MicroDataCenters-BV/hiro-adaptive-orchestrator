# Metrics

Everything this operator exposes on its Prometheus `/metrics` endpoint, in two parts:

1. **[Free metrics](#free-metrics-not-defined-here)** — emitted automatically by
   controller-runtime, client-go, and the Go runtime. We wrote none of these; they exist
   because the operator uses the standard `ctrl.NewControllerManagedBy(mgr)` wiring.
2. **[Custom metrics](#custom-metrics-hiro_)** — defined in this package (`hiro_*` prefix),
   one file per subsystem (`rebalance.go`, `placement.go`, `controller.go`, `webhook.go`).

All of it is served on the manager's existing endpoint (`:8443/metrics`, HTTPS + bearer-token
auth) and picked up by `config/prometheus/monitor.yaml` (the ServiceMonitor). No separate
metrics server.

This package is a **leaf dependency**: `internal/rebalance`, `internal/placement-server`,
`internal/controller`, and `internal/webhook/v1` all import it — it never imports any of them
back. Custom metrics register against controller-runtime's own `metrics.Registry`, so they
land on the same endpoint as the free ones.

---

## Free metrics (not defined here)

### Per reconciler — labeled `controller=`

Two controllers: `controller="orchestrationprofile"` (the `OrchestrationProfileReconciler`)
and `controller="rebalance-detection"` (the rebalance engine's `Reconciler`, named via
`.Named("rebalance-detection")`).

| Metric | Type |
|---|---|
| `controller_runtime_reconcile_total{controller,result}` | counter |
| `controller_runtime_reconcile_errors_total{controller}` | counter |
| `controller_runtime_reconcile_panics_total{controller}` | counter |
| `controller_runtime_reconcile_time_seconds{controller}` | histogram |
| `controller_runtime_active_workers{controller}` | gauge |
| `controller_runtime_max_concurrent_reconciles{controller}` | gauge |

### Workqueue — labeled `name=` (= controller name)

`workqueue_depth`, `workqueue_adds_total`, `workqueue_queue_duration_seconds`,
`workqueue_work_duration_seconds`, `workqueue_retries_total`,
`workqueue_longest_running_processor_seconds`, `workqueue_unfinished_work_seconds`.

### Webhook — labeled `webhook=` (only when `DEPLOY_SCHEDULER_PLUGIN=true`)

| Metric | Type |
|---|---|
| `controller_runtime_webhook_requests_total{webhook,code}` | counter |
| `controller_runtime_webhook_requests_in_flight{webhook}` | gauge |
| `controller_runtime_webhook_latency_seconds{webhook}` | histogram |

### Kubernetes API client (client-go)

`rest_client_requests_total{code,method,host}`, `rest_client_request_duration_seconds`,
`rest_client_request_size_bytes`, `rest_client_response_size_bytes`.

### Leader election

`leader_election_master_status{name}`.

### TLS cert reload (certwatcher)

`certwatcher_read_certificate_total`, `certwatcher_read_certificate_errors_total` — for the
metrics-server cert and, when enabled, the webhook serving cert.

### Go runtime / process

`go_goroutines`, `go_threads`, `go_gc_duration_seconds`, `go_memstats_*`,
`process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds`,
`process_start_time_seconds`, …

### Out of scope: the kube-scheduler plugin

The `HIROScore` plugin (`scheduler-plugin/`) runs inside **kube-scheduler's own process**, not
this operator's — a separate binary with its own `/metrics`. The scheduling framework there
already instruments every plugin's Filter/Score/PreScore extension-point latency and outcome.
Nothing to add or scrape from this side.

---

## Custom metrics (`hiro_*`)

What the free request/error/duration metrics above can't express — the domain-specific "why".

### Rebalance engine (`rebalance.go`)

| Metric | Type | Labels | What it means |
|---|---|---|---|
| `hiro_rebalance_transitions_total` | Counter | `from`, `to`, `action`, `outcome` | Every state transition. Not labeled by the free-text `reason` — unbounded cardinality. `outcome` is empty for non-terminal transitions. |
| `hiro_rebalance_guardrail_rejections_total` | Counter | `action`, `guardrail` | A recommendation a dispatch guardrail refused, before reaching `Decided`. |
| `hiro_rebalance_rate_limit_exhausted_total` | Counter | `action` | An accepted Move that still failed the cluster-wide rate-limit wait. |
| `hiro_rebalance_cooldown_skips_total` | Counter | — | A reconcile that skipped AI consultation because the profile is still in cooldown. |
| `hiro_rebalance_improvement_score` | Histogram | `action` | Distribution of `Improvement` scores the AI returns, regardless of guardrail outcome. Buckets: `0, 10, …, 100`. |
| `hiro_rebalance_profiles_by_state` | Gauge | `state` | Live count of profiles per rebalancing state, recomputed on every scrape by a custom collector (not incrementally maintained — see `rebalance.go`'s doc comment on why). |

### Placement server (`placement.go`)

Covers both the scheduler-plugin path (`/api/v1/placement/*`) and the kube-scheduler extender
path (`/extender/*`) in one place — both protocols share the same underlying `score()` /
`filter()` methods, distinguished only by the `path` label.

| Metric | Type | Labels | What it means |
|---|---|---|---|
| `hiro_placement_score_requests_total` | Counter | `path`, `result` | Every scoring request. |
| `hiro_placement_score_duration_seconds` | Histogram | `path` | Duration of scoring — a DecisionStore hit (near-zero) or the full AI round trip. Default buckets. |
| `hiro_placement_filter_requests_total` | Counter | `path`, `allowed` | Every energy-gate check that completed without error. |
| `hiro_placement_filter_errors_total` | Counter | `path` | An energy-gate error that was soft-failed open (the pod was allowed anyway). |
| `hiro_placement_decision_store_hits_total` | Counter | — | `DecisionStore.Lookup` calls that found a live, unexpired entry. |
| `hiro_placement_decision_store_misses_total` | Counter | — | `DecisionStore.Lookup` calls that found nothing, or an expired entry. A ratio is a PromQL/Grafana concern, not computed here. |

### OrchestrationProfile controller (`controller.go`)

| Metric | Type | Labels | What it means |
|---|---|---|---|
| `hiro_controller_profile_status_transitions_total` | Counter | `from`, `to` | An `OrchestrationProfileStatus.Status` change. Only fires on an actual change — same gating `updateStatus` applies to its own event emission. |

### Pod-defaulting webhook (`webhook.go`)

| Metric | Type | Labels | What it means |
|---|---|---|---|
| `hiro_webhook_pod_default_outcomes_total` | Counter | `outcome` | What the pod-defaulting webhook actually did on each admission. |

---

## Label value reference

| Label | Where | Values |
|---|---|---|
| `action` | `hiro_rebalance_improvement_score`, `_guardrail_rejections_total`, `_rate_limit_exhausted_total` | `Move`, `AdjustReplicas` |
| `action` | `hiro_rebalance_transitions_total` | same, plus `""` before a decision is made, and `NoOp` / `Defer` / `Reject` when that's the recorded decision |
| `outcome` | `hiro_rebalance_transitions_total` | `Enacted`, `NoOp`, `Rejected`, `Deferred`, `Failed`, or `""` (non-terminal) |
| `from` / `to` / `state` | rebalance metrics | `Watching`, `Triggered`, `Evaluating`, `Decided`, `Enacting` |
| `guardrail` | `hiro_rebalance_guardrail_rejections_total` | `threshold`, `bounds` |
| `path` | placement metrics | `plugin`, `extender` |
| `result` | `hiro_placement_score_requests_total` | `store_hit`, `ai_success`, `ai_error` |
| `allowed` | `hiro_placement_filter_requests_total` | `true`, `false` |
| `from` / `to` | `hiro_controller_profile_status_transitions_total` | `Pending`, `Active`, `Degraded`, `Error`, `InUse` |
| `outcome` | `hiro_webhook_pod_default_outcomes_total` | `mutated`, `already_set`, `no_profile`, `lookup_error` |

All label *values* are named Go constants (`GuardrailThreshold`, `PathPlugin`,
`ScoreResultStoreHit`, `PodDefaultMutated`, …) in this package — never raw string literals at
the call sites.
