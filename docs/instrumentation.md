# Instrumentation

Metrics, traces, and logs across both processes — what exists, how to turn it on, and how to
read it.

- [At a glance](#at-a-glance)
- [Two processes, two endpoints](#two-processes-two-endpoints)
- [Metrics](#metrics)
- [Reading the metrics](#reading-the-metrics)
- [Scraping](#scraping)
- [Tracing](#tracing)
- [Logs](#logs)
- [Grafana stack](#grafana-stack)
- [Adding a metric](#adding-a-metric)
- [Known gaps](#known-gaps)

---

## At a glance

| Signal | Default | Turn on with |
|---|---|---|
| **Metrics** | always exposed | nothing — but nothing *reads* them until you grant a reader and a scraper |
| **Traces** | off, nothing exported | `OTEL_EXPORTER_OTLP_ENDPOINT` |
| **Logs** | always on | nothing; they carry `trace_id` once tracing is on |

Metrics being "always exposed" is not the same as being usable. Out of the box both endpoints
require a bearer token that nothing is authorized to mint, and no Prometheus is watching. See
[Reading the metrics](#reading-the-metrics) and [Scraping](#scraping).

---

## Two processes, two endpoints

This is the single most important fact about the instrumentation here: **the operator and the
scheduler plugin are separate binaries with separate metric registries, and one does not see
the other.**

| | Operator | Scheduler plugin |
|---|---|---|
| Process | `cmd/main.go` | inside **kube-scheduler** |
| Endpoint | `:8443/metrics` | `:10259/metrics` |
| Registry | controller-runtime `metrics.Registry` | `component-base/metrics/legacyregistry` |
| Defined in | [internal/metrics/](../internal/metrics/) | [scheduler-plugin/metrics.go](../scheduler-plugin/metrics.go) |
| Prefix | `hiro_<subsystem>_*` | `hiro_scheduler_plugin_*` |

The plugin **cannot** reuse `internal/metrics`. That package registers into controller-runtime's
registry in `init()`, and nothing serves that registry inside the kube-scheduler process.
`legacyregistry` is the one kube-scheduler already serves. Registering in the wrong one is a
silent failure: the code compiles, the counters increment, and nothing is ever exposed.

Two registries means **two scrape targets** — the operator's ServiceMonitor selects
`control-plane=controller-manager` and will never match the scheduler pod. Both must be enabled
separately.

---

## Metrics

### Operator (`:8443`)

Defined in [internal/metrics/](../internal/metrics/), one file per subsystem.

**Rebalance engine** — `rebalance.go`

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `hiro_rebalance_transitions_total` | counter | `from`, `to`, `action`, `outcome` | Every state transition that actually landed. Deliberately not labelled by the free-text reason |
| `hiro_rebalance_trigger_evaluations_total` | counter | `condition`, `fired` | Detection — every trigger condition checked |
| `hiro_rebalance_trigger_errors_total` | counter | `condition` | A condition that couldn't be evaluated. Those paths report "did not fire", so without this a condition that can never match looks like one that simply didn't |
| `hiro_rebalance_pressure_queries_total` | counter | `result` | metrics-server lookups. A missing metrics-server otherwise silently disables the CPU/Memory triggers |
| `hiro_rebalance_ai_requests_total` | counter | `result` | Rebalance AI consultations, separating a context-assembly failure from a failed round trip |
| `hiro_rebalance_ai_duration_seconds` | histogram | — | AI consultation latency, including context assembly. Default buckets |
| `hiro_rebalance_improvement_score` | histogram | `action` | Improvement scores the AI returns, whether or not a guardrail accepts them. Buckets `0, 10, … 100` |
| `hiro_rebalance_guardrail_rejections_total` | counter | `action`, `guardrail` | A recommendation a guardrail refused before reaching `Decided` |
| `hiro_rebalance_cooldown_skips_total` | counter | — | A reconcile that skipped the AI because the profile is in cooldown |
| `hiro_rebalance_rate_limit_exhausted_total` | counter | `action` | An accepted Move that still failed the cluster-wide rate-limit wait |
| `hiro_rebalance_enactment_duration_seconds` | histogram | `action` | Time to carry out a decision. Buckets run to 300s, since enactment waits on a replacement pod |
| `hiro_rebalance_enactment_failures_total` | counter | `action`, `stage` | Which step of enactment failed — transitions alone only record that the cycle failed |
| `hiro_rebalance_resize_outcomes_total` | counter | `result` | How much of an in-place resize took effect. A shortfall is still reported as Enacted |
| `hiro_rebalance_watchdog_recoveries_total` | counter | — | A cycle force-recovered after going stale — otherwise indistinguishable from a normal return to Watching |
| `hiro_rebalance_profiles_by_state` | gauge | `state` | Live count per state, recomputed on every scrape rather than incremented, so it can't drift after a restart |
| `hiro_rebalance_profiles_escalated` | gauge | — | Profiles currently paused by escalation. A status flag orthogonal to `state` |

**PlacementServer** — `placement.go`. Plugin (`/api/v1/placement/*`) and extender (`/extender/*`)
share the same `score()` / `filter()` methods, so one set of metrics covers both, split by `path`.

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `hiro_placement_score_requests_total` | counter | `path`, `result` | Every scoring request |
| `hiro_placement_score_duration_seconds` | histogram | `path` | A DecisionStore hit (near zero) or the full AI round trip. Default buckets |
| `hiro_placement_filter_requests_total` | counter | `path`, `allowed` | Every energy-gate check that returned without error |
| `hiro_placement_filter_errors_total` | counter | `path` | An energy-gate error soft-failed open |
| `hiro_placement_decision_store_hits_total` | counter | — | DecisionStore lookups that found a live entry |
| `hiro_placement_decision_store_misses_total` | counter | — | Lookups that found nothing or an expired entry. Derive a ratio in PromQL |
| `hiro_placement_context_build_total` | counter | `result` | Scoring-context assembly. "Pod isn't governed" and "profile lookup broke" reach the caller as one error |
| `hiro_placement_eao_fetch_total` | counter | `caller`, `result` | EnergyAwareOrchestration lookups. Every caller soft-fails a missing EAO — see below |

**OrchestrationProfile controller** — `controller.go`

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `hiro_controller_profile_status_transitions_total` | counter | `from`, `to` | A change to `status.status`. Fires only on an actual change |

**Pod-defaulting webhook** — `webhook.go`

| Metric | Type | Labels | What it tells you |
|---|---|---|---|
| `hiro_webhook_pod_default_outcomes_total` | counter | `outcome` | What the webhook did on each admission |

#### Energy awareness degrades silently

All three placement paths treat a missing or unreachable EAO as "carry on without energy data",
and none of them report an error:

| `caller` | What happens without the EAO |
|---|---|
| `score` | Energy data is dropped from the AI request |
| `filter` | The gate allows the pod — and `filter_requests_total` counts it as `allowed="true"`, identical to "energy checked and sufficient" |
| `rebalance` | Energy data is dropped from the rebalance AI request |

`hiro_placement_eao_fetch_total{result="not_found"|"error"}` is the only signal that energy-aware
placement is running blind. It isn't counted at all when a profile has energy awareness off.

#### Label values

| Label | Metric | Values |
|---|---|---|
| `from` / `to` / `state` | rebalance | `Watching`, `Triggered`, `Evaluating`, `Decided`, `Enacting` |
| `action` | `transitions_total` | `Move`, `AdjustReplicas`, `AdjustResources`, `NoOp`, `Defer`, `Reject`, `Escalate`, or `""` before a decision |
| `action` | `improvement_score`, `guardrail_rejections_total`, `enactment_*` | `Move`, `AdjustReplicas`, `AdjustResources` |
| `action` | `rate_limit_exhausted_total` | `Move` — the rate limiter applies to Moves only |
| `outcome` | `transitions_total` | `Enacted`, `NoOp`, `Rejected`, `Deferred`, `Failed`, `Escalated`, or `""` while non-terminal |
| `condition` | `trigger_*` | `CPUThreshold`, `MemoryThreshold`, `EnergyThreshold`, `NodeFailure`, `Scheduled` |
| `fired` | `trigger_evaluations_total` | `true`, `false` |
| `guardrail` | `guardrail_rejections_total` | `threshold`, `bounds` |
| `stage` | `enactment_failures_total` | `evict`, `patch`, `wait_replacement` |
| `result` | `ai_requests_total` | `success`, `context_error`, `ai_error` |
| `result` | `pressure_queries_total` | `success`, `unavailable` |
| `result` | `resize_outcomes_total` | `all_resized`, `partial`, `none` |
| `path` | placement | `plugin`, `extender` |
| `result` | `score_requests_total` | `store_hit`, `ai_success`, `ai_error` |
| `allowed` | `filter_requests_total` | `true`, `false` |
| `result` | `context_build_total` | `success`, `profile_not_found`, `profile_error` |
| `caller` | `eao_fetch_total` | `score`, `filter`, `rebalance` |
| `result` | `eao_fetch_total` | `found`, `not_found`, `error` |
| `from` / `to` | `profile_status_transitions_total` | `NoPods`, `Pending`, `Active`, `Partial`, `Degraded`, `Error` |
| `outcome` | `pod_default_outcomes_total` | `mutated`, `already_set`, `no_profile`, `lookup_error` |

#### Free metrics

controller-runtime, client-go and the Go runtime expose these with no code from us. Check here
before adding a `hiro_*` metric — reconcile latency, for one, already exists.

| Source | Metrics |
|---|---|
| Reconcilers, labelled `controller=` (`orchestrationprofile`, `rebalance-detection`) | `controller_runtime_reconcile_total{result}`, `_reconcile_errors_total`, `_reconcile_panics_total`, `_reconcile_time_seconds`, `_active_workers`, `_max_concurrent_reconciles` |
| Workqueue, labelled `name=` | `workqueue_depth`, `_adds_total`, `_queue_duration_seconds`, `_work_duration_seconds`, `_retries_total`, `_longest_running_processor_seconds`, `_unfinished_work_seconds` |
| Webhook, labelled `webhook=` — only with `webhook.enable=true` | `controller_runtime_webhook_requests_total{code}`, `_requests_in_flight`, `_latency_seconds` |
| client-go | `rest_client_requests_total{code,method,host}`, `_request_duration_seconds`, `_request_size_bytes`, `_response_size_bytes` |
| Leader election | `leader_election_master_status{name}` |
| Cert reload | `certwatcher_read_certificate_total`, `_errors_total` — metrics cert, and the webhook cert when enabled |
| Go runtime / process | `go_goroutines`, `go_threads`, `go_gc_duration_seconds`, `go_memstats_*`, `process_cpu_seconds_total`, `process_resident_memory_bytes`, `process_open_fds`, … |

kube-scheduler similarly exposes its own `scheduler_*` metrics on `:10259`, including per-plugin
extension-point latency for `HIROScore`.

### Scheduler plugin (`:10259`)

| Metric | Type | Labels |
|---|---|---|
| `hiro_scheduler_plugin_placement_requests_total` | counter | `endpoint`, `result` |
| `hiro_scheduler_plugin_placement_request_duration_seconds` | histogram | `endpoint` |
| `hiro_scheduler_plugin_placement_errors_total` | counter | `endpoint`, `reason` |
| `hiro_scheduler_plugin_soft_fail_total` | counter | `phase` |
| `hiro_scheduler_plugin_energy_gate_decisions_total` | counter | `allowed` |
| `hiro_scheduler_plugin_nodes_scored` | histogram | — |
| `hiro_scheduler_plugin_unscored_nodes_total` | counter | — |

| Label | Values |
|---|---|
| `endpoint` | `score`, `filter` |
| `result` | `success`, `error` |
| `reason` | `marshal`, `request_build`, `transport`, `http_status`, `decode` |
| `phase` | `prefilter`, `prescore`, `score` |
| `allowed` | `true`, `false` |

Two of these exist specifically because the plugin **fails open**, and a failing-open plugin is
invisible from outside:

- **`soft_fail_total`** — each event is a pod scheduled with no AI input at all. The three
  branches are an energy-gate error (PreFilter), a scoring error (PreScore, every node then
  scores 0), and a CycleState miss (Score).
- **`nodes_scored` / `unscored_nodes_total`** — the AI decides how many nodes it returns, and
  nothing checks that it covered every candidate. A node absent from the response gets a
  neutral **0**, indistinguishable from one the AI rated 0. If the AI returns scores for 3 of
  50 candidates, the HTTP call still succeeds, `placement_requests_total{result="success"}`
  still increments, and 47 nodes silently tie at zero. `nodes_scored` drifting toward zero is
  the only signal.

### Why a metric family can be missing from `/metrics`

A labelled `*Vec` emits nothing until some label combination is first used, so a family you
expect may simply be absent rather than broken. Unlabelled metrics appear immediately at 0.

Check `_count` to tell the two apart: `hiro_scheduler_plugin_nodes_scored_count 0` means the
metric is registered and nothing has been observed yet.

---

## Reading the metrics

Both endpoints are HTTPS with bearer-token authn, delegating authorization to the API server via
a `SubjectAccessReview` on the `/metrics` nonResourceURL. **Nothing is authorized by default** —
the chart ships the reader role unbound so you choose who gets it.

Grant a reader:

```bash
helm upgrade <release> charts/hiro-adaptive-platform -n <namespace> --reuse-values \
  --set observability.metricsReaders={<namespace>:<serviceaccount>}
```

The same single grant covers **both** endpoints — the operator and kube-scheduler check the same
nonResourceURL. `hack/deploy_full_stack_with_helm.sh` sets this to the operator's own
ServiceAccount automatically (unless `DEPLOY_PROMETHEUS=true`, where the bundled Prometheus
already grants itself) and prints ready-to-run commands.

Then port-forward and scrape by hand:

```bash
kubectl port-forward -n <namespace> svc/<operator-metrics-svc> 18443:8443 &
kubectl port-forward -n <namespace> svc/<scheduler-metrics-svc> 20259:10259 &

TOKEN=$(kubectl create token <serviceaccount> -n <namespace>)
curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:18443/metrics | grep '^hiro_'
curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:20259/metrics | grep '^hiro_scheduler_plugin_'
```

A **403** here means the grant is missing, not that the endpoint is down.

Service names are truncated by Helm and must be looked up, never guessed — the operator's
placement Service really does render as `...-hir-controller-manager-placement-service`. Find
them by label:

```bash
kubectl get svc -n <namespace> -l app.kubernetes.io/component=placement-server
kubectl get svc -n <namespace> -l app.kubernetes.io/component=scheduler-metrics
```

The operator's **metrics** Service has no unique component label — it shares
`control-plane=controller-manager` with the placement Service — so select it by port instead:

```bash
kubectl get svc -n <namespace> -l control-plane=controller-manager \
  -o jsonpath='{.items[?(@.spec.ports[0].port==8443)].metadata.name}'
```

---

## Scraping

Two ServiceMonitors, because two scrape targets:

| Target | Enabled by |
|---|---|
| Operator | `hiro-operator.prometheus.enabled=true` |
| Scheduler plugin | `scheduler.plugin.metrics.serviceMonitor.enabled=true` |

Both default to **false**, and installing Prometheus does not switch either on. Miss one and
half the `hiro_*` metrics are exposed but never collected, with nothing to indicate it.

With the bundled stack:

```bash
DEPLOY_PROMETHEUS=true hack/deploy_full_stack_with_helm.sh deploy
```

With your own Prometheus: grant its ServiceAccount via `observability.metricsReaders`, point its
`serviceMonitorNamespaceSelector` at the release namespace, and turn **both** ServiceMonitors on.

---

## Tracing

OpenTelemetry, OTLP over gRPC, W3C TraceContext propagation. **Off unless an endpoint is set** —
with `OTEL_EXPORTER_OTLP_ENDPOINT` empty, no exporter is constructed and behaviour is
byte-for-byte what it was before tracing existed.

```bash
OTLP_ENDPOINT=alloy.observability.svc:4317 hack/deploy_full_stack_with_helm.sh deploy
```

| Variable | Default | Meaning |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(empty)* | OTLP gRPC collector. Empty = tracing off |
| `OTEL_SERVICE_NAME` | `hiro-adaptive-orchestrator` / `hiro-scheduler` | `service.name` on spans |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | Sample ratio, 0.0–1.0, parent-based |
| `OTEL_RESOURCE_ATTRIBUTES` | *(empty)* | Extra attributes, comma-separated `k=v` |

Set it on **both** processes to get one connected trace. In the chart that means
`hiro-operator.manager.env`'s `OTEL_*` entries and `scheduler.plugin.tracing.*`; the deploy
script sets both from one `OTLP_ENDPOINT`.

What a connected trace spans:

```
kube-scheduler PreScore  →  PlacementServer /score  →  AI decision agent
RebalanceEngine          →  AI decision agent
```

Client spans and `traceparent` injection come from an `otelhttp` transport on both HTTP clients;
the server side is an `otelhttp` handler wrapping the PlacementServer mux.

**kube-scheduler has no tracing configuration of its own** — there is no `Tracing` field in
`KubeSchedulerConfiguration`. So the plugin owns its own `TracerProvider` lifecycle in
`scheduler-plugin/cmd/main.go`, and the setup is duplicated rather than shared, because that
module must not import controller-runtime.

---

## Logs

Structured, via `logf.FromContext(ctx)` in the operator and `klog.InfoS` in the plugin.

When tracing is on, log lines on instrumented paths carry `trace_id` and `span_id`, so a log
line in Loki links to its trace in Tempo. Both helpers return nothing when tracing is off, so
nothing changes in that case.

Notable lines: the plugin logs every AI node score and the recommended node once per scheduling
cycle (`HIROScore: AI node scores`), and each soft-fail branch logs why it fell back.

---

## Grafana stack

Prometheus-compatible throughout, so moving to Alloy / Loki / Mimir / Tempo needs no code
changes:

| | What changes |
|---|---|
| **Metrics → Mimir** | Nothing in this repo. Either keep the ServiceMonitors and add remote-write, or point Alloy at the same two endpoints with the same bearer token and reader grant |
| **Traces → Tempo** | Set `OTEL_EXPORTER_OTLP_ENDPOINT` at Alloy or Tempo's OTLP gRPC port. Already the supported path |
| **Logs → Loki** | Collect pod stdout with Alloy. Already structured; `trace_id` is already present when tracing is on, which is what makes log↔trace linking work |

The only thing worth doing deliberately is keeping `OTEL_SERVICE_NAME` distinct per release, so
two deployments in one backend stay distinguishable.

---

## Adding a metric

Conventions, in both processes:

1. **Name** `hiro_<subsystem>_*`, one file per subsystem.
2. **No unbounded labels.** Never a pod, node, or profile name. Never a free-text reason —
   `hiro_rebalance_transitions_total` deliberately drops the transition's reason because it's a
   full sentence.
3. **Every label value is a named Go constant** in the metrics package, never a string literal
   at the call site.
4. **Pick the right registry** — see [Two processes, two endpoints](#two-processes-two-endpoints).
   Getting this wrong fails silently.
5. **Register a new scrape target** if you add a new process. Exposing without scraping is the
   gap that left the scheduler plugin's metrics invisible when they were first added.
6. **Test with `testutil`** — `prometheus/client_golang/prometheus/testutil` for the operator,
   `component-base/metrics/testutil` for the plugin. Note a non-vec `component-base` Histogram
   has no `Desc`, so read it from `legacyregistry.DefaultGatherer` rather than
   `GetHistogramMetricCount`.

The bar for a new metric is that it distinguishes states the existing ones collapse. Three were
dropped during this work for failing that bar, rather than being shoehorned in — among them an
audit-write counter: `internal/placement-server/decision_audit.go` is a log line that can't fail,
and is documented as deliberately not a metric, so the counter could only ever read `success`.

---

## Known gaps

- **The rebalance engine evicts before checking the energy gate.** The gate that decides whether
  a pod may proceed is `CheckEnergyGate` in `internal/placement-server/builder.go`;
  `internal/rebalance/energy_gate.go` only classifies schedule timeouts despite its name. A
  rebalance-side gate metric was deliberately *not* added, because adding one would have implied
  a check that doesn't happen there. Revisit when the ordering is fixed.
- **Some chart features need the Helm post-renderer** to take effect — the webhook's cert
  wiring, `HIRO_SCHEDULER_NAME`, and propagating a non-default `PLACEMENT_SERVER_PORT`. Metrics
  and tracing do not. See
  [charts/hiro-adaptive-platform/README.md](../charts/hiro-adaptive-platform/README.md#post-renderer-plugin-helm-v4).
