# Rebalance Engine

The rebalance engine is the runtime loop that acts on the AI's guidance *after* initial
placement. It turns HIRO from scheduling-time intelligence into continuous, event-driven
orchestration: watching already-placed workloads, deciding when they should move, and
carrying that decision out safely.

It lives inside the same operator process as the `OrchestrationProfile` controller and the
`PlacementServer` — see [Wiring into the system](#wiring-into-the-system) below. There is no
separate binary, Deployment, or Service for it.

## The decision lifecycle

Every rebalance evaluation for a workload is an instance of a state machine, tracked in
`OrchestrationProfile.status.rebalancingStatus`. It is born when a trigger fires and dies in
one of several terminal states.

```
                        ┌──────────────┐
                        │   Triggered  │
                        └──────┬───────┘
                               │
                               ▼
                        ┌──────────────┐
                   ┌────│  Evaluating  │────┐
             NoOp  │    └──────┬───────┘    │  Rejected / Failed
                   │           │            │
                   │           ▼            │
                   │    ┌──────────────┐    │
                   │    │   Decided    │    │
                   │    └──────┬───────┘    │
                   │           │            │
                   │           ▼            │
                   │    ┌──────────────┐    │
                   │    │   Enacting   │─┐  │
                   │    └──────┬───────┘ │  │
                   │           │         │  │
                   ▼           ▼         ▼  ▼
                 NoOp       Enacted   Deferred / Failed
                   │           │         │      │
                   └───────────┴─────────┴──────┘
                               │
                        (cooldown elapses)
                               │
                               ▼
                          Triggered (next cycle)
```

Active states: `Triggered`, `Evaluating`, `Decided`, `Enacting`.
Terminal states: `Enacted`, `NoOp`, `Rejected`, `Deferred`, `Failed`.

The transition table lives in [`state.go`](state.go) (`validTransitions`,
`IsValidTransition`, `IsTerminal`). From any terminal state (or the unset initial state),
the only legal move is back into `Triggered` — starting a fresh cycle. Every other edge in
the diagram above is the only path the state machine allows; anything else is rejected by
the writer (see below).

## Package layout

| File | Role |
|---|---|
| [`state.go`](state.go) | The transition table. Pure, dependency-free — no I/O. |
| [`writer.go`](writer.go) | `StateWriter` — the **only** component allowed to mutate `status.rebalancingStatus`. |
| [`reconciler.go`](reconciler.go) | `Reconciler` — drives the `Triggered` transition (Detection stage). A real controller-runtime controller. |
| [`triggers.go`](triggers.go) | `TriggerEvaluator` — checks the five CRD-declared trigger conditions. |
| [`pressure.go`](pressure.go) | `NodePressureEvaluator` — CPU/Memory node pressure via metrics-server. Used by `TriggerEvaluator`. |
| [`retry_enactor.go`](retry_enactor.go) | `retryPendingSchedule` — the mechanical enactor for `ActionRetryPendingSchedule` (see [Bypassing the AI for mechanical actions](#bypassing-the-ai-for-mechanical-actions)). |
| [`engine.go`](engine.go) | `Engine` — a `manager.Runnable` used to register the rebalance engine's lifecycle with the operator's Manager. |

Every `.go` file above has a matching `_test.go`.

## Detection — how a workload becomes `Triggered`

Detection is hybrid, per the original design: **event-driven** for fast signals, **periodic**
for slow ones. Both paths call the *exact same* `TriggerEvaluator.Evaluate()` — the watches
and the ticker only decide *when* to check; `Evaluate()` decides *what* currently holds.

```
   ┌─────────────────────────┐        ┌──────────────────────────┐
   │  Periodic (every 30s)   │        │  Event-driven (watches)  │
   │  Reconcile requeues     │        │  Pod / Node / EAO change │
   │  itself unconditionally │        │  → immediate reconcile   │
   └────────────┬────────────┘        └─────────────┬────────────┘
                │                                    │
                └──────────────┬─────────────────────┘
                                ▼
                    Reconciler.Reconcile(profile)
                                │
                 cooldown active? ── yes ──► requeue at cooldown expiry
                                │ no
                 cycle already in flight? ── yes ──► no-op (let it finish)
                                │ no
                    TriggerEvaluator.Evaluate(profile)
                                │
                          any condition matched?
                        yes │           │ no
                            ▼           ▼
              StateWriter.Transition   requeue at DetectionInterval
              (-> Triggered)
```

### Why both paths exist

- **CPUThreshold, MemoryThreshold, Scheduled** can *only* be caught by the periodic path —
  there's no Kubernetes event for "resource usage drifted past a threshold" or "the
  scheduled window arrived." `Reconcile` requeues itself every `DetectionInterval` (default
  30s) forever, for every rebalancing-enabled profile.
- **EnergyThreshold, NodeFailure** are also checked on the periodic tick, but don't have to
  wait up to 30s — `Reconciler.SetupWithManager` wires watches on `Pod`, `Node`, and (if
  installed) `EnergyAwareOrchestration` that cause an *immediate* reconcile the moment
  something relevant changes.
- **Profile status changes** are covered for free: the primary `For(&OrchestrationProfile{})`
  watch fires on *any* update to the object, including status subresource updates — which is
  also how the Reconciler notices its own `StateWriter` transitions and how later stages of
  the state machine (Decision, Enaction) can drive themselves forward through the same
  `Reconcile` function once they're wired in.

### The `EnergyAwareOrchestration` watch is optional

EAO is a separate, optional CRD (from a different component, the Energy Aware Orchestrator).
`SetupWithManager` checks whether it's actually installed via the manager's `RESTMapper`
before adding a watch for it — if it's absent, the operator logs a note and continues
without that watch rather than failing to start. Without the CRD, `EnergyThreshold` is still
checked, just only on the periodic tick instead of instantly.

## Trigger conditions

`TriggerEvaluator.Evaluate` (in [`triggers.go`](triggers.go)) walks
`profile.Spec.Rebalancing.TriggerConditions` in the order the user declared them and returns
the **first** one that currently matches. If nothing matches, that's not an error — it's just
nothing to do this cycle.

| Condition | How it's checked |
|---|---|
| `EnergyThreshold` | Finds the `EnergyAwareOrchestration` matching the profile's `ApplicationRef` directly (no pod resolution needed, unlike the placement-server's pod-triggered EAO lookup). Three cases, in order: (1) energy reported insufficient, (2) decision is `Delayed`/`Waiting`, (3) decision says `DeployImmediately`/`Scheduled` **and** a pod for this app is still `Pending` — meaning it was likely blocked by the energy gate earlier and the window has since reopened. Case 3 deliberately reads the EAO's *current* status rather than parsing `nextEvaluationTime` itself — by the time this runs, the EAO's own status already reflects the new window. Case 3 also sets `TriggerResult.BypassAction` — see below. |
| `CPUThreshold` / `MemoryThreshold` | Delegates to `NodePressureEvaluator` for every distinct node the profile's pods are currently on. |
| `NodeFailure` | `NotReady` condition on any node hosting one of the profile's pods. |
| `Scheduled` | Always matches — the periodic tick itself *is* the schedule. |

EAO being unavailable, or a metrics-server call failing, is soft-failed per condition
(logged at V(1), evaluator moves on) — never a hard error that would derail the whole
`Evaluate()` call or the reconcile.

### Bypassing the AI for mechanical actions

`EnergyThreshold`'s case 3 (pending pod, window reopened) is different from the other two
cases in one important way: there's no judgment call to make. Cases 1/2 are genuine problem
signals that need the AI to decide *what* to do about them (once Evaluating actually calls
the AI — not built yet). Case 3's answer is already fully determined the moment it matches:
delete the pod, let it get scheduled again. Sending that through an AI round-trip would add
a network call and a timeout-failure surface for a question that has no real answer to give.

Why does the pod need any nudge at all, though — won't Kubernetes retry it? Eventually, yes:
`CheckEnergyGate` in `internal/placement-server` re-evaluates the EAO fresh on every
scheduling attempt, and kube-scheduler's own unschedulable-pod pool gets periodically
flushed and retried regardless of any specific event. But kube-scheduler has **no informer
on `EnergyAwareOrchestration`** — it doesn't watch that CRD, so an EAO status update by
itself pushes zero signal into the scheduling queue. The pod schedules only whenever
kube-scheduler's own independent backoff timer happens to roll around, which is decoupled
from the actual fix and could take a while. This mechanism exists purely to close that
latency gap, not to fix a correctness gap.

So instead of an annotation-patch nudge (unreliable — whether a generic pod update actually
causes kube-scheduler to requeue *that* pod depends on internal queueing-hint heuristics we
don't control), `retryPendingSchedule` (in [`retry_enactor.go`](retry_enactor.go)) **deletes**
the Pending, unscheduled pod outright. Its owning controller (Deployment/StatefulSet/Job)
notices the missing replica and creates a fresh Pod object — a `CREATE` event, which
unconditionally goes through full scheduling immediately, no heuristics involved. This is
safe specifically because the pod was never `Ready`: deleting it has no availability/PDB
implications the way evicting a running pod would (unlike the future Move enactor, which
will need the PDB-aware Eviction API for exactly that reason).

`Reconciler.enactBypassAction` (in `reconciler.go`) drives this end to end in a single
`Reconcile` call: `Triggered → Evaluating → Decided → Enacting → Enacted`/`Failed`, all
through `StateWriter` (so it's fully visible in status/Events/`recentDecisions`) — `Evaluating`
just records "no AI needed" instead of making an HTTP call, and `Enacting` calls
`retryPendingSchedule`. No new edges in the transition table: this still only ever reaches
`Enacted` via `Decided → Enacting`, same as every other action will.

One deliberate scope pull-forward: this terminal transition sets `cooldownUntil` from
`profile.Spec.Rebalancing.CooldownSeconds` even though general cooldown-duration computation
for the rest of the engine isn't built yet — without it, a delete that doesn't actually fix
the problem would retry every `DetectionInterval` in a tight loop.

## Node pressure (CPU/Memory)

[`pressure.go`](pressure.go)'s `NodePressureEvaluator` answers a node-level question — "is
the node this pod happens to be on under enough pressure that moving away would help" — not
a pod-level one. A node can be under pressure entirely because of *other* workloads; that's
exactly the case rebalancing should react to.

It uses **live usage from metrics-server** (`metrics.k8s.io`), not resource *requests* — a
real signal, not a static heuristic. `EvaluateCPUPressure` and `EvaluateMemoryPressure` are
independent, self-contained methods (each does its own Node-get + NodeMetrics-get); they're
not merged into one combined fetch because metrics-server always returns both resources in a
single response anyway, so there's no real fetch cost to save by combining — only coupling
to avoid in the trigger dispatch `switch`.

metrics-server is an optional cluster component (see
[deployment](../../hack/install_metrics_server.sh)). If it's not installed, both methods
return an error, which `TriggerEvaluator` catches and treats as "not currently evaluable" —
`CPUThreshold`/`MemoryThreshold` simply never fire until it's present.

Default threshold: 90% (`DefaultNodePressureThreshold`).

## `StateWriter` — the single-writer rule

This is a hard architectural rule, not a convention: **no code anywhere in this codebase may
mutate `status.rebalancingStatus` except through `StateWriter.Transition`.** Every stage of
the engine must go through it. This is what makes the engine's status trustworthy enough to
build a UI or alerting on top of later.

`Transition` (in [`writer.go`](writer.go)):

1. Re-fetches the profile (via an **uncached** reader, `mgr.GetAPIReader()` — not
   `mgr.GetClient()`) and validates the move against `IsValidTransition` — an illegal
   transition is rejected with an error, not silently coerced.
2. On entering `Triggered`: assigns a fresh `decisionId` (UUID) and resets the previous
   cycle's `action`/`details`.
3. On any terminal state: sets `cooldownUntil` (if a cooldown was passed in) and prepends a
   record to `recentDecisions`, trimmed to `MaxRecentDecisions` (10).
4. Persists via `Status().Update`, wrapped in `retry.RetryOnConflict` — callers never need
   their own conflict-retry loop, and re-applying the same transition after a crash is safe
   (it either no-ops because state already moved on, or succeeds idempotently).
5. Emits a Kubernetes Event (`RebalanceTransition` reason) on the `OrchestrationProfile`
   object — `Failed`/`Rejected`/`Deferred` as `Warning`, everything else as `Normal`. These
   show up in `kubectl describe orchestrationprofile <name>` interleaved with the existing
   `PlacementActive`/`PlacementDegraded`/etc. events from the main controller, distinguished
   by the `FROM` column (`rebalance-engine` vs `orchestrationprofile-controller`).

### Why the reader must be uncached

Found the hard way: `enactBypassAction` calls `Transition` five times back-to-back within one
`Reconcile` (`Triggered → Evaluating → Decided → Enacting → Enacted`/`Failed`). Each call
independently re-fetches the profile to determine the current state before validating the
move. `Status().Update` always writes straight to the API server — but `mgr.GetClient()`'s
`Get()` reads from the **local informer cache**, which only catches up after an asynchronous
watch round-trip. The write from step *N* had already landed on the server, but the cache
hadn't observed it yet by the time step *N+1* asked — so it read the state from *two steps
back*, and rejected an otherwise-valid transition with `invalid transition "X" -> "Y"`. Using
`mgr.GetAPIReader()` (a direct, uncached client) for the read side fixes this for every
chained-transition sequence, not just this one — Story 27/28/30's real Evaluating/Decided/
Enacting sequence will chain calls the same way.

This class of bug can't be caught by the fake-client unit tests in this package — the fake
client isn't cache-based, so writes are immediately visible to the next `Get()`, which is
exactly the behavior that masked this in testing. Catching it required a real cluster with
the real manager cache.

### A profile can get permanently stuck in a non-terminal state

`Reconcile` skips detection entirely whenever `state` is non-terminal (`Triggered`,
`Evaluating`, `Decided`, `Enacting`) — deliberately, so it doesn't re-trigger a cycle that's
still in flight. But today, nothing in this package ever gets a profile back out of
`Triggered` on its own: cases 1/2 of `EnergyThreshold` (and any future condition that needs a
real AI consultation) have no path forward yet, since `Evaluating`'s AI-calling logic isn't
built. Once a profile lands in `Triggered` for one of those, it's parked there permanently —
every subsequent Pod/Node/EAO event and periodic tick gets silently swallowed by the
in-flight guard, no matter what changes in the cluster afterward. The only way out today is a
manual status patch to a terminal state (e.g. `kubectl patch ... --subresource=status -p
'{"status":{"rebalancingStatus":{"state":"Failed"}}}'`), which itself immediately re-triggers
a reconcile via the primary watch. This needs a real fix once Story 27/28 exist (either a
"no path forward" case that itself transitions somewhere terminal instead of dead-ending, or
a staleness/timeout mechanism) — noting it here so it isn't rediscovered from scratch.

## Wiring into the system

Everything below runs inside the **same operator process** — one binary, one Deployment, one
pod. Wired in `cmd/main.go`:

```go
rebalanceWriter := rebalance.NewStateWriter(mgr.GetClient(), mgr.GetAPIReader(), mgr.GetEventRecorderFor("rebalance-engine"))
rebalanceEngine := rebalance.NewEngine(mgr.GetClient(), rebalanceWriter)
mgr.Add(rebalanceEngine)

metricsClient, _ := metricsclientset.NewForConfig(restConfig)
pressureEvaluator := rebalance.NewNodePressureEvaluator(mgr.GetClient(), metricsClient, 0)
triggerEvaluator := rebalance.NewTriggerEvaluator(mgr.GetClient(), eaoGVK, pressureEvaluator)

rebalanceDetector := rebalance.NewReconciler(
    mgr.GetClient(), rebalanceWriter, triggerEvaluator,
    controller.ProfileByAppRefIndex, 0, // 0 -> DefaultDetectionInterval
)
rebalanceDetector.SetupWithManager(mgr, eaoItemGVK)
```

- `controller.ProfileByAppRefIndex` — the same field index the `OrchestrationProfile`
  controller already registers (`internal/controller/op_index.go`), reused rather than
  duplicated. `internal/rebalance` does **not** import `internal/controller` directly; the
  index field name is passed in as a string constructor argument, mirroring the same
  decoupling convention `internal/placement-server` already uses.
- `eaoGVK` in `main.go` is the *List* kind (`...List`), used for `List()` calls in both the
  placement server and `TriggerEvaluator`. `Reconciler.SetupWithManager` needs the singular
  item kind instead (for `Watches()` and the `RESTMapper` check) — derived locally via
  `strings.TrimSuffix(eaoGVK.Kind, "List")` rather than carrying a second GVK variable
  through `main.go`.
- Pod discovery (`FindPodsForApplication`/`ResolveLabelSelector`) lives in
  `internal/utils/helpers.go`, shared between the `OrchestrationProfile` controller and
  `TriggerEvaluator` so both stay in sync on how a workload's pods are resolved, instead of
  two copies drifting apart.

### RBAC

Permissions added specifically for this package (`+kubebuilder:rbac` markers live in
[`reconciler.go`](reconciler.go) and [`retry_enactor.go`](retry_enactor.go)):

```
+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
+kubebuilder:rbac:groups=metrics.k8s.io,resources=nodes,verbs=get
+kubebuilder:rbac:groups="",resources=pods,verbs=delete
```

The `pods` `delete` verb merges into the same rule as the `get;list;watch` already granted
by the `OrchestrationProfile` controller's own markers — `make manifests` produces one
combined `pods` rule, not a duplicate.

Regenerate `config/rbac/role.yaml` with `make manifests` after changing these markers.

### CRD schema

`RebalancingStatus` (in `api/v1alpha1/orchestrationprofile_types.go`) carries: `state`
(OpenAPI-enum-validated against the 9 states), `reason`, `decisionId`, `action`, `details`,
`startedAt`, `lastTransitionAt`, `cooldownUntil`, and `recentDecisions` (rolling history,
each entry shaped like `RebalanceDecision`).

### Deployment

- `hack/install_metrics_server.sh` — idempotent metrics-server installer, wired into
  `hack/deploy_full_stack.sh` Phase 2 (default on, `INSTALL_METRICS_SERVER=false` to skip).
  Without it, `CPUThreshold`/`MemoryThreshold` simply never fire; nothing else is affected.
- `hack/deploy_operator.sh` / `hack/deploy_full_stack.sh` print a component breakdown at
  deploy time so it's clear the rebalance engine isn't a separate opt-in piece — it starts
  the moment the operator pod is `Ready`.

## Testing

No envtest suite in this package — everything is tested against `sigs.k8s.io/controller-
runtime/pkg/client/fake` (plus `k8s.io/metrics/.../fake` for metrics-server), which is fast
and sufficient for what's here: no CRD-schema-level behavior is being exercised (that's
covered separately by `internal/controller/op_rebalancing_status_test.go`, which does use
envtest to prove the OpenAPI enum validation actually works against a real API server).

Two non-obvious things worth knowing if you're adding tests here:

- `metricsfake.NewSimpleClientset(objects...)` silently drops seeded `NodeMetrics` objects —
  it infers the REST resource name via naive Kind pluralization ("nodemetrics"), but the
  real resource name is the irregular "nodes". Seed via `Tracker().Create()` with the
  explicit GVR instead (see `pressure_test.go`'s `newTestPressureEvaluator`).
- `record.NewFakeRecorder(n)` blocks (not drops) once its channel fills — a test that emits
  many transitions without draining events will hang, not fail loudly. Size the buffer
  generously in any test that loops over multiple transitions.
