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
`OrchestrationProfile.status.rebalancingStatus`. `state` only ever holds one of five active
values — it never encodes *how* a cycle ended. The result of a finished cycle is recorded
separately, as an `outcome` on the `recentDecisions` entry written in the same transition that
returns `state` to `Watching`.

```
                        ┌──────────────┐
                        │   Watching   │◀────────────────────────┐
                        └──────┬───────┘                         │
                               │ trigger fires                   │
                               ▼                                 │
                        ┌──────────────┐                         │
                        │   Triggered  │                         │
                        └──────┬───────┘                         │
                               │ AI call                         │
                               ▼                                 │
                        ┌──────────────┐                         │
                        │  Evaluating  │──── NoOp / Rejected ────┤
                        └──────┬───────┘        / Failed         │
                               │ Move accepted                   │
                               ▼                                 │
                        ┌──────────────┐                         │
                        │   Decided    │                         │
                        └──────┬───────┘                         │
                               │                                 │
                               ▼                                 │
                        ┌──────────────┐                         │
                        │   Enacting   │─── Enacted / Deferred ──┘
                        └──────────────┘        / Failed
```

Active states (`state`): `Watching`, `Triggered`, `Evaluating`, `Decided`, `Enacting`.
Outcomes (`recentDecisions[].outcome`, never a `state` value): `Enacted`, `NoOp`, `Rejected`,
`Deferred`, `Failed`.

The transition table lives in [`state.go`](state.go) (`validTransitions`,
`IsValidTransition`). From `Watching` (or the unset initial state — the two are equivalent),
the only legal move is into `Triggered` — starting a fresh cycle. Every other edge in the
diagram above is the only path the state machine allows; anything else is rejected by the
writer (see below). `Evaluating` and `Enacting` are the only states that can transition
straight back to `Watching` — every such write must carry an outcome (`TransitionOptions.Outcome`),
enforced by `StateWriter.Transition` itself.

## Package layout

| File | Role |
|---|---|
| [`state.go`](state.go) | The transition table. Pure, dependency-free — no I/O. |
| [`writer.go`](writer.go) | `StateWriter` — the **only** component allowed to mutate `status.rebalancingStatus`. |
| [`reconciler.go`](reconciler.go) | `Reconciler` — drives the `Triggered` transition (Detection) and, for trigger matches with no mechanical bypass, the AI consultation (`Evaluating`) too. A real controller-runtime controller. |
| [`triggers.go`](triggers.go) | `TriggerEvaluator` — checks the five CRD-declared trigger conditions. |
| [`pressure.go`](pressure.go) | `NodePressureEvaluator` — CPU/Memory node pressure via metrics-server. Used by `TriggerEvaluator`. |
| [`retry_enactor.go`](retry_enactor.go) | `retryPendingSchedule` — the mechanical enactor for `ActionRetryPendingSchedule` (see [Bypassing the AI for mechanical actions](#bypassing-the-ai-for-mechanical-actions)). |
| [`dispatch.go`](dispatch.go) | `dispatchDecision` and the `actionDispatchers` registry — carries a successful AI response to a terminal transition (see [Dispatch](#dispatch--acting-on-the-ais-decision)). |
| [`move_enactor.go`](move_enactor.go) | `moveEnactor` — evicts a pod and waits for its replacement, driving `Decided → Enacting → Watching` for an accepted `Move`. |
| [`engine.go`](engine.go) | `Engine` — a `manager.Runnable` used to register the rebalance engine's lifecycle with the operator's Manager. |

`internal/placement-server/decision_store.go` (not in this package) holds `DecisionStore`,
shared between `moveEnactor` (writes) and `PlacementServer.score` (reads) — see
[The decision store](#the-decision-store--how-move-actually-lands-a-pod-on-a-specific-node).

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
signals that need the AI to decide *what* to do about them (see
[Decision — how `Evaluating` calls the AI](#decision--how-evaluating-calls-the-ai) below).
Case 3's answer is already fully determined the moment it matches:
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
`Reconcile` call: `Triggered → Evaluating → Decided → Enacting → Watching` (outcome `Enacted`
or `Failed`), all through `StateWriter` (so it's fully visible in status/Events/
`recentDecisions`) — `Evaluating` just records "no AI needed" instead of making an HTTP call,
and `Enacting` calls `retryPendingSchedule`. No new edges in the transition table: this still
only ever reaches `Watching` via `Decided → Enacting`, same as every other action will.

One deliberate scope pull-forward: this terminal transition sets `cooldownUntil` from
`profile.Spec.Rebalancing.CooldownSeconds` even though general cooldown-duration computation
for the rest of the engine isn't built yet — without it, a delete that doesn't actually fix
the problem would retry every `DetectionInterval` in a tight loop.

## Decision — how `Evaluating` calls the AI

For every trigger match that has no mechanical bypass (i.e. `TriggerResult.BypassAction` is
empty), `Reconciler.evaluateWithAI` (in `reconciler.go`) drives `Triggered → Evaluating` and
consults the External AI Agent — the same agent the placement server already uses for
initial-placement scoring, not a second HTTP path:

1. `StateWriter.Transition` moves the profile `Triggered → Evaluating`, capturing the fresh
   `decisionId` and the profile's `recentDecisions` history from the write result — no extra
   `Get()` needed, avoiding the cache-staleness bug class described above.
2. `DecisionContextBuilder.BuildRebalanceContext` (in `internal/placement-server`) assembles a
   `DecisionRequest`: every pod of the application and its current node (`CurrentPlacements` —
   the AI needs the whole layout, not just one pod, to judge imbalance and pick both which pod
   to move and where), every `Ready` cluster node as a candidate, the profile's strategy/
   awareness/rebalancing config, optional EAO energy data, the trigger reason, and recent
   decision history.
3. `DecisionClient.RequestRebalanceDecision` POSTs that request under a configurable timeout
   (`Reconciler.DecisionTimeout`, default 5s) and decodes a `RebalanceDecisionResponse`:
   `Action` (`orchestrationv1alpha1.RebalanceAction` — `Move`, `NoOp`, and other values defined
   there for later stories; a named type so Go call sites get compile-time-checked comparisons
   even though the wire format is a plain JSON string), `PodName` (which pod from
   `CurrentPlacements` the action applies to — required for `Move`, since `TargetNode` alone
   can't say which pod is going there when more than one could), `TargetNode`, `Improvement`,
   and `Reason`.
4. A context-build error or a request timeout/error returns the profile to `Watching` with
   outcome `Failed` via `failEvaluation` (with the profile's own cooldown applied, so an
   unreachable AI agent is retried on the normal cooldown cadence instead of every single
   `DetectionInterval` tick), with a reason describing which step failed. A successful response
   is handed to `Reconciler.dispatchDecision` (in `dispatch.go`) — see
   [Dispatch — acting on the AI's decision](#dispatch--acting-on-the-ais-decision) below.

## Dispatch — acting on the AI's decision

[`dispatch.go`](dispatch.go) carries a successful `RebalanceDecisionResponse` the rest of the
way to a terminal `Watching` write. It's a registry (`actionDispatchers`, keyed by
`orchestrationv1alpha1.RebalanceAction`), not a hardcoded switch, so a future enactor
(`AdjustResources`, `AdjustReplicas`, `Defer`, `Escalate`) is a new map entry — `dispatchDecision`
itself never changes. An action with no registered dispatcher (today: anything other than
`Move`/`NoOp`, including `Reject`/`Defer`, which exist as values but have no enactor yet) is
treated as a processing error — `Watching` + outcome `Failed` — rather than guessed at.

- **`NoOp`** (`dispatchNoOp`) exits straight from `Evaluating` to `Watching` with outcome `NoOp`.
  No `Decided`/`Enacting` hop — both are valid direct exits from `Evaluating` in
  `validTransitions`.
- **`Move`** (`dispatchMove`) first applies the improvement-threshold guardrail
  (`Reconciler.ImprovementThreshold`, default `DefaultImprovementThreshold` — a single global
  value for now, not per-trigger-reason). Below threshold, or missing `PodName`/`TargetNode`,
  exits straight to `Watching` (`Rejected` / `Failed`) without ever reaching `Decided`. An
  accepted, well-formed `Move` transitions `Decided → Enacting` and calls `moveEnactor`
  ([`move_enactor.go`](move_enactor.go)):
  1. Records a [`DecisionStore`](decision_store.go) entry — see below — *before* evicting.
  2. Evicts the pod via the PDB-aware Eviction API (`c.SubResource("eviction").Create`, not a
     raw delete — the pod being moved is `Ready`, unlike `retryPendingSchedule`'s target).
     A `429` (PDB refused) maps to outcome `Deferred`.
  3. Polls (`Reconciler.MoveActionTimeout`, default `DefaultMoveActionTimeout` = 60s) for a
     replacement pod — one not present before eviction, now scheduled to a node. Landing on
     `TargetNode` is outcome `Enacted`; landing elsewhere or timing out is outcome `Failed`.
  4. The `DecisionStore` entry is cleared on every path out, regardless of outcome.

### The decision store — how `Move` actually lands a pod on a specific node

Kubernetes gives no direct way to pin a specific pod to a specific node through the normal
scheduling path — evicting a pod only tells its owning controller to create a replacement;
*where* that replacement lands is still kube-scheduler's own decision. [`decision_store.go`](decision_store.go)
(`internal/placement-server`) is how the rebalance engine influences that decision instead of
fighting it: a concurrency-safe, TTL-bounded (`DefaultDecisionStoreTTL` = 60s) in-memory map,
keyed by **workload identity** (`WorkloadKey(profile.Namespace, profile.Name)`), not by pod
name — the replacement pod has a different generated name than the one evicted, so there's no
pod-name correlation available at the moment it's scored.

`PlacementServer.score` (`server.go`) checks the store first, via `decisionStoreResponse`: a
live entry with `Action == Move` synthesizes a `DecisionResponse` that scores `TargetNode`
decisively (100) above every other candidate (0) — the same shape the AI agent would return —
instead of making a fresh AI call. A miss (the common case) falls through unchanged.

Lookup **self-consumes**: the entry is removed the moment any pod for that workload is scored,
not left live for the rest of the TTL. Without that, a concurrent, unrelated pod for the same
workload (e.g. a scale-up racing the rebalance cycle) could pick up the same node hint. The
enactor's own `Delete` at its terminal transition is therefore a no-op safety net for whichever
case reaches a terminal outcome without `score` ever having been called (e.g. the eviction never
produced a schedulable replacement) — TTL expiry is the final backstop for that same case.

One design consequence worth calling out: the AI sees the *entire* current placement every
call but the response only ever describes one action. Multi-pod convergence (e.g. two pods
need to move to actually balance the workload) is expected to happen over multiple Triggered
cycles — the AI recommends its single highest-value move this cycle, cooldown elapses, and if
the workload is still imbalanced the next cycle's request reflects the post-move layout and the
AI can recommend the next one. `recentDecisions` accumulates the history across cycles. Nothing
in the schema currently supports batching several decisions into one response, and the state
machine (one active decision per profile at a time) isn't shaped for concurrent per-pod cycles
either.

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
3. On transitioning to `Watching`: requires `TransitionOptions.Outcome` (rejects the call
   otherwise), sets `cooldownUntil` (if a cooldown was passed in), and prepends a record to
   `recentDecisions` carrying that outcome, trimmed to `MaxRecentDecisions` (10).
4. Persists via `Status().Update`, wrapped in `retry.RetryOnConflict` — callers never need
   their own conflict-retry loop, and re-applying the same transition after a crash is safe
   (it either no-ops because state already moved on, or succeeds idempotently).
5. Emits a Kubernetes Event (`RebalanceTransition` reason) on the `OrchestrationProfile`
   object — a transition to `Watching` with outcome `Failed`/`Rejected`/`Deferred` is a
   `Warning`, everything else is `Normal`. These show up in `kubectl describe
   orchestrationprofile <name>` interleaved with the existing `PlacementActive`/
   `PlacementDegraded`/etc. events from the main controller, distinguished by the `FROM`
   column (`rebalance-engine` vs `orchestrationprofile-controller`).

### Why the reader must be uncached

Found the hard way: `enactBypassAction` calls `Transition` five times back-to-back within one
`Reconcile` (`Triggered → Evaluating → Decided → Enacting → Watching`, outcome `Enacted` or
`Failed`). Each call
independently re-fetches the profile to determine the current state before validating the
move. `Status().Update` always writes straight to the API server — but `mgr.GetClient()`'s
`Get()` reads from the **local informer cache**, which only catches up after an asynchronous
watch round-trip. The write from step *N* had already landed on the server, but the cache
hadn't observed it yet by the time step *N+1* asked — so it read the state from *two steps
back*, and rejected an otherwise-valid transition with `invalid transition "X" -> "Y"`. Using
`mgr.GetAPIReader()` (a direct, uncached client) for the read side fixes this for every
chained-transition sequence, not just this one — `evaluateWithAI`'s `Triggered → Evaluating`
sequence, and the future `Decided → Enacting` sequence once an AI-recommended action actually
gets dispatched, chain calls the same way.

This class of bug can't be caught by the fake-client unit tests in this package — the fake
client isn't cache-based, so writes are immediately visible to the next `Get()`, which is
exactly the behavior that masked this in testing. Catching it required a real cluster with
the real manager cache.

### A profile can get permanently stuck in a non-Watching state

`Reconcile` skips detection entirely whenever `state != Watching` (`Triggered`, `Evaluating`,
`Decided`, `Enacting`) — deliberately, so it doesn't re-trigger a cycle that's still in flight.
Every path that can enter the state machine now also has a defined way back to `Watching` —
AI-unreachable (`failEvaluation`), guardrail-rejected/unrecognized-action/malformed-Move
(`dispatchDecision`), and a `Move`'s eviction/replacement outcome (`moveEnactor`), each ending in
a terminal `Watching` write with an outcome.

What isn't fully closed: `enactBypassAction` and `dispatchMove` each drive several `Transition`
calls sequentially *within a single `Reconcile` call*. If the operator process dies exactly
between two of those calls (e.g. mid-rollout), the profile is abandoned wherever it was —
`Reconcile`'s in-flight guard then skips it forever, since nothing else ever revisits a non-
`Watching` state on its own. The only way out today is a manual status patch back to `Watching`
(e.g. `kubectl patch ... --subresource=status -p
'{"status":{"rebalancingStatus":{"state":"Watching"}}}'`), which itself immediately re-triggers
a reconcile via the primary watch. Closing this fully would need a stuck-state watchdog (revert
anything stuck past some max cycle duration) — not built, noted here so it isn't rediscovered
from scratch.

## Wiring into the system

Everything below runs inside the **same operator process** — one binary, one Deployment, one
pod. Wired in `cmd/main.go`:

```go
// Each of these is parsed from an environment variable and resolved to the
// package default (rebalance.DefaultX) right here in main.go if unset or
// <= 0 — so every log line and downstream constructor call sees the value
// actually in effect, never the raw "0 means unset" sentinel.
rebalanceMaxRecentDecisions := parseIntEnv("REBALANCE_MAX_RECENT_DECISIONS")      // -> rebalance.DefaultMaxRecentDecisions
rebalanceDetectionInterval := parseDurationEnv("REBALANCE_DETECTION_INTERVAL")    // -> rebalance.DefaultDetectionInterval
rebalanceDecisionTimeout := parseDurationEnv("REBALANCE_DECISION_TIMEOUT")        // -> rebalance.DefaultDecisionTimeout
rebalanceNodePressureThreshold := parseFloatEnv("REBALANCE_NODE_PRESSURE_THRESHOLD") // -> rebalance.DefaultNodePressureThreshold
rebalanceImprovementThreshold := parseFloatEnv("REBALANCE_IMPROVEMENT_THRESHOLD") // -> rebalance.DefaultImprovementThreshold
rebalanceDecisionStoreTTL := parseDurationEnv("REBALANCE_DECISION_STORE_TTL")     // -> placementserver.DefaultDecisionStoreTTL
rebalanceMoveActionTimeout := parseDurationEnv("REBALANCE_MOVE_ACTION_TIMEOUT")   // -> rebalance.DefaultMoveActionTimeout

rebalanceWriter := rebalance.NewStateWriter(
    mgr.GetClient(), mgr.GetAPIReader(), mgr.GetEventRecorderFor("rebalance-engine"),
    rebalanceMaxRecentDecisions,
)
rebalanceEngine := rebalance.NewEngine(mgr.GetClient(), rebalanceWriter)
mgr.Add(rebalanceEngine)

// decisionStore is shared between PlacementServer (reads it in score) and the
// rebalance engine's Move enactor (writes to it before eviction) — both live
// in this same operator binary.
decisionStore := placementserver.NewDecisionStore(rebalanceDecisionStoreTTL)

metricsClient, _ := metricsclientset.NewForConfig(restConfig)
pressureEvaluator := rebalance.NewNodePressureEvaluator(mgr.GetClient(), metricsClient, rebalanceNodePressureThreshold)
triggerEvaluator := rebalance.NewTriggerEvaluator(mgr.GetClient(), eaoGVK, pressureEvaluator)

// contextBuilder and decisionClient are the same instances the placement server
// uses for initial-placement scoring — the rebalance engine's AI consultation is
// a second use of the one configured External AI Agent, not a parallel HTTP path.
rebalanceDetector := rebalance.NewReconciler(
    mgr.GetClient(), rebalanceWriter, triggerEvaluator,
    controller.ProfileByAppRefIndex, rebalanceDetectionInterval,
    contextBuilder, decisionClient, rebalanceDecisionTimeout,
    rebalanceImprovementThreshold, decisionStore, rebalanceMoveActionTimeout,
)
rebalanceDetector.SetupWithManager(mgr, eaoItemGVK)

// decisionStore is also passed to NewPlacementServer so score() can read it.
```

### Configuration

Every numeric default in this package is overridable at deploy time — none of it needs a code
change or rebuild to tune for a given cluster:

| Environment variable | Overrides | Default |
|---|---|---|
| `REBALANCE_MAX_RECENT_DECISIONS` | `StateWriter`'s rolling decision-history length | `DefaultMaxRecentDecisions` (10) |
| `REBALANCE_DETECTION_INTERVAL` | `Reconciler`'s periodic detection tick (Go duration, e.g. `30s`) | `DefaultDetectionInterval` (30s) |
| `REBALANCE_DECISION_TIMEOUT` | `Reconciler`'s AI-consultation timeout (Go duration, e.g. `5s`) | `DefaultDecisionTimeout` (5s) |
| `REBALANCE_NODE_PRESSURE_THRESHOLD` | `NodePressureEvaluator`'s CPU/Memory pressure fraction (e.g. `0.90`) | `DefaultNodePressureThreshold` (0.90) |
| `REBALANCE_IMPROVEMENT_THRESHOLD` | `dispatchMove`'s guardrail — minimum `Improvement` to enact a `Move` | `DefaultImprovementThreshold` (20) |
| `REBALANCE_DECISION_STORE_TTL` | How long a `Move` decision biases `PlacementServer.score` (Go duration) | `placementserver.DefaultDecisionStoreTTL` (60s) |
| `REBALANCE_MOVE_ACTION_TIMEOUT` | Max wait for a `Move`'s replacement pod to be scheduled (Go duration) | `DefaultMoveActionTimeout` (60s) |

Each default lives once, as a constant next to the type it configures — `main.go` doesn't
duplicate the numbers, it just resolves "unset" to that constant before constructing anything,
so the startup log always shows the value actually in effect rather than a misleading `0`. An
unparseable value (bad duration/float/int) fails the operator at startup rather than silently
falling back, since a typo here should be caught at deploy time, not discovered later as
unexplained behavior.

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
(OpenAPI-enum-validated against the 5 active states), `reason`, `decisionId`, `action`,
`details`, `startedAt`, `lastTransitionAt`, `cooldownUntil`, and `recentDecisions` (rolling
history, each entry shaped like `RebalanceDecision` — `outcome` is its own OpenAPI enum,
separate from and never overlapping with `state`'s).

An operator upgrading from the older 9-value `state` enum needs
`cmd/migrate-rebalance-status` run once against the cluster first — see that command's own
doc comment for the exact order (apply the new CRD, then run the migration, then roll out the
new operator build).

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
