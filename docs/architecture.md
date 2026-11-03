# Architecture

Referenced from the main [README](../README.md#architecture), which only covers the component
table — this file has the full detail: component wiring, the reconciliation flow, the Plugin and
Extender request paths, and the `OrchestrationProfile` CRD spec.

See also [internal/rebalance/README.md](../internal/rebalance/README.md) for the rebalance
engine's decision lifecycle, and [docs/scheduling-flows.md](scheduling-flows.md) for
function-level call traces of both scheduling paths.

## Component Wiring

```
┌───────────────────────────────────────────────────────────────────────┐
│  Kubernetes API Server                                                │
└──────────┬──────────────────────────────────────────┬──────────────────┘
           │ watch OrchestrationProfile / EAO          │ schedule pods
           │ watch Deployments/StatefulSets/Jobs       │
           │ watch Pod/Node events + periodic tick     │
           ▼                                           │
┌────────────────────────────────────────┐   ┌─────────┴───────────────────┐
│  HIRO Operator Pod                      │   │  Scheduler (choose one)     │
│  ──────────────────────────────────    │   │  ─────────────────────────  │
│  Reconciler (placement status)          │   │  [A] hiro-scheduler(plugin) │
│                                          │   │      per-pod opt-in        │
│  Rebalance Engine                       │   │  [B] default kube-scheduler │
│    Watching → Triggered → Evaluating    │   │      extender, cluster-wide │
│    → Decided → Enacting                 │   │                             │
│    (Move / RetryPendingSchedule)        │◄──┤  [A] → /placement/filter    │
│    │ writes on Move                     │   │       /placement/score      │
│    ▼                                    │   │  [B] → /extender/filter     │
│  DecisionStore (shared, TTL-bounded)    │   │       /extender/prioritize  │
│    │ read before AI call                │   │                             │
│    ▼                                    │   │                             │
│  PlacementServer (:8090)                │   │                             │
│  ──────────────────────────────────    │   │                             │
│  /api/v1/placement/score                │   │                             │
│  /api/v1/placement/filter               │   │                             │
│  /extender/filter                       │   │                             │
│  /extender/prioritize                   │   │                             │
│  /healthz                               │   │                             │
└──────────────────┬───────────────────────┘   └─────────────────────────────┘
                   │ POST DecisionRequest / RebalanceContext
                   ▼
         ┌──────────────────────┐
         │  Decision Agent      │
         │  (AI or Mock) :8080  │
         └──────────────────────┘
```

```mermaid
flowchart TB
    API["Kubernetes API Server"]

    subgraph Operator["HIRO Operator Pod"]
        direction TB
        Reconciler["Reconciler<br/>(placement status)"]
        RE["Rebalance Engine<br/>Watching → Triggered → Evaluating<br/>→ Decided → Enacting<br/>(Move / RetryPendingSchedule)"]
        Store[("DecisionStore<br/>shared, TTL-bounded")]
        PS["PlacementServer (:8090)<br/>─────────────────────<br/>/api/v1/placement/score<br/>/api/v1/placement/filter<br/>/extender/filter<br/>/extender/prioritize<br/>/healthz"]

        RE -->|"writes on Move"| Store
        Store -->|"read before AI call"| PS
    end

    subgraph SchedChoice["Scheduler (choose one)"]
        direction TB
        A["[A] hiro-scheduler (plugin)<br/>per-pod opt-in"]
        B["[B] default kube-scheduler<br/>extender, cluster-wide"]
    end

    Agent["Decision Agent<br/>(AI or Mock) :8080"]

    API -->|"watch OrchestrationProfile / EAO<br/>watch Deployments/StatefulSets/Jobs"| Reconciler
    API -->|"watch Pod/Node events<br/>+ periodic tick"| RE
    API -->|"schedule pods"| SchedChoice
    A -->|"[A] → /placement/filter<br/>/placement/score"| PS
    B -->|"[B] → /extender/filter<br/>/extender/prioritize"| PS
    PS -->|"POST DecisionRequest"| Agent
    RE -->|"POST RebalanceContext"| Agent
```

<a id="reconciliation-flow"></a>
## Reconciliation Flow

```
Kubernetes API server
        │  OrchestrationProfile created / updated
        │  Deployment / StatefulSet / ReplicaSet / Job updated
        │  Pod phase changed
        ▼
Reconciliation Controller
        ├── Validate spec (applicationRef, strategy, awareness)
        │       on failure → status=Error + Kubernetes Event
        ├── Resolve workload (Deployment / StatefulSet / ReplicaSet / Job)
        │       not found → status=NoPods (waits for the watch to re-trigger)
        ├── Discover pods via OwnerReference walk
        │       no pods   → status=NoPods
        ├── Compute pod health (ready / pending / failed counts)
        │       all ready  → status=Active
        │       some ready → status=Partial
        │       none ready → status=Pending
        │       any failed → status=Degraded
        ▼
OrchestrationProfile.status updated
        └── Status transition → Kubernetes Event emitted
```

```mermaid
flowchart TD
    API["Kubernetes API server<br/>OrchestrationProfile created/updated<br/>Deployment/StatefulSet/ReplicaSet/Job updated<br/>Pod phase changed"] --> RC["Reconciliation Controller"]

    RC --> V["Validate spec<br/>(applicationRef, strategy, awareness)"]
    V -->|"on failure"| ErrV["status = Error<br/>+ Kubernetes Event"]
    V -->|"ok"| RW["Resolve workload<br/>(Deployment / StatefulSet / ReplicaSet / Job)"]

    RW -->|"not found"| NoPodsRW["status = NoPods<br/>(waits for the watch)"]
    RW -->|"found"| DP["Discover pods via<br/>OwnerReference walk"]

    DP -->|"no pods"| NoPods["status = NoPods"]
    DP -->|"pods found"| CH["Compute pod health<br/>(ready / pending / failed counts)"]

    CH -->|"all ready"| Active["status = Active"]
    CH -->|"some ready"| Partial["status = Partial"]
    CH -->|"none ready"| Pending["status = Pending"]
    CH -->|"any failed"| Degraded["status = Degraded"]

    ErrV --> Upd["OrchestrationProfile.status updated"]
    NoPodsRW --> Upd
    NoPods --> Upd
    Active --> Upd
    Partial --> Upd
    Pending --> Upd
    Degraded --> Upd

    Upd --> Evt["Status transition →<br/>Kubernetes Event emitted"]
```

<a id="placementserver"></a>
## PlacementServer

The `PlacementServer` is a **single HTTP listener on `:8090`** inside the operator pod. Both scheduler integration approaches — plugin and extender — call the same server on different routes. There is no separate binary or process: it is one `net/http` mux started by `cmd/main.go` alongside the reconciler.

```mermaid
flowchart LR
    Plugin["hiro-scheduler<br/>(plugin)"]
    Extender["default scheduler<br/>(extender)"]
    Probes["Kubernetes<br/>probes"]

    subgraph PS["PlacementServer :8090 — HIRO Operator Pod"]
        direction TB
        R0["POST /api/v1/placement/filter<br/>1. look up OrchestrationProfile for pod (O(1) field index)<br/>2. CheckEnergyGate via EAO CRD<br/>&nbsp;&nbsp;allowed → Allowed: true<br/>&nbsp;&nbsp;blocked → Allowed: false (plugin marks pod Unschedulable)"]
        R1["POST /api/v1/placement/score<br/>1. look up OrchestrationProfile (O(1) field index)<br/>2. build DecisionRequest (AOProfile + EAOProfile + nodes)<br/>3. call External AI Agent → return NodeScores to plugin"]
        R2["POST /extender/filter<br/>1. look up OrchestrationProfile for pod<br/>2. CheckEnergyGate via EAO CRD<br/>&nbsp;&nbsp;allowed → pass all nodes through unchanged<br/>&nbsp;&nbsp;blocked → FailedNodes: all (scheduler defers the pod)"]
        R3["POST /extender/prioritize<br/>1. build DecisionRequest (same pipeline as plugin path)<br/>2. call External AI Agent → map scores [0-100] → [0-10]<br/>3. missing nodes → score 5 (neutral fallback)"]
        R4["GET /healthz → 200 ok"]
    end

    Agent["Decision<br/>Agent :8080"]

    Plugin --> R0
    Plugin --> R1
    Extender --> R2
    Extender --> R3
    Probes --> R4
    R1 -->|"POST DecisionRequest"| Agent
    R3 -->|"POST DecisionRequest"| Agent
```

> The plugin's `/placement/filter` + `/placement/score` and the extender's `/extender/filter` + `/extender/prioritize` all share the same underlying `s.filter()` / `s.score()` core (`builder.CheckEnergyGate()` / `DecisionContextBuilder.Build()` + `DecisionClient.RequestDecision()`) — see [docs/scheduling-flows.md](scheduling-flows.md#shared-vs-protocol-specific). Each path's filter step runs `CheckEnergyGate` first; each path's score/prioritize step calls the AI agent, differing only in response shape (plugin: raw 0–100 `NodeScores`; extender: normalised 0–10 `HostPriorityList`).

<a id="plugin-path-hiro-scheduler--placementserver"></a>
## Plugin Path: hiro-scheduler + PlacementServer

End-to-end flow for pods with `spec.schedulerName: hiro-scheduler`. The `HIROScore` plugin runs inside the `hiro-scheduler` pod; the `PlacementServer` runs inside the operator pod. They communicate via HTTP on two routes — `/api/v1/placement/filter` (energy gate) and `/api/v1/placement/score` (AI scoring).


```mermaid
sequenceDiagram
    participant Sched as hiro-scheduler
    participant PS as PlacementServer<br/>(:8090, operator pod)
    participant Agent as External Decision<br/>Agent (:8080)

    Note over Sched: Pod created with<br/>spec.schedulerName: hiro-scheduler
    Sched->>PS: PreFilter — POST /api/v1/placement/filter<br/>Body: EnergyGateRequest{pod}
    activate PS
    PS->>PS: builder.CheckEnergyGate() — findProfileForPod()<br/>+ fetchEAOProfile() (if awareness.Energy=true)
    PS-->>Sched: {allowed, reason}
    deactivate PS
    Sched->>Sched: PreFilter stashes energy gate result in CycleState<br/>(soft-fail open on error — Allowed: true)
    Sched->>Sched: Filter phase — HIROScore.Filter reads<br/>CycleState → Unschedulable if blocked, else passes
    Sched->>PS: PreScore — POST /api/v1/placement/score<br/>Body: PlacementContext{pod, candidateNodes}
    activate PS
    PS->>PS: Decode PlacementContext
    PS->>PS: builder.Build() — assemble DecisionRequest<br/>findProfileForPod() (O(1) field index)<br/>buildAOProfileContext() (strategy + awareness + placement)<br/>fetchEAOProfile() (if awareness.Energy=true)
    PS->>Agent: client.RequestDecision()<br/>POST DECISION_AGENT_URL<br/>Body: DecisionRequest{pod, candidateNodes, AOProfile, EAOProfile}
    Agent-->>PS: {nodeScores: [{nodeName, score 0-100}], reason}
    PS-->>Sched: 200 OK {nodeScores, reason}
    deactivate PS
    Sched->>Sched: PreScore stashes NodeScores in CycleState
    Sched->>Sched: Score phase — HIROScore.Score reads<br/>NodeScore from CycleState → returns score to framework
    Sched->>Sched: Selects highest-scored node → binds pod
```

Plugin configuration (URL, path, timeout) is injected into the `KubeSchedulerConfiguration` ConfigMap at deploy time — see [Scheduler Plugin Config](deployment.md#scheduler-plugin-config).

For a detailed function-level trace of every call in this path see [docs/scheduling-flows.md — Path A](scheduling-flows.md#path-a--scheduler-plugin-hiro-scheduler).

<a id="extender-path-default-kube-scheduler--placementserver"></a>
## Extender Path: default kube-scheduler + PlacementServer

End-to-end flow when the extender is deployed. The default `kube-scheduler` calls the PlacementServer during its filter and prioritize phases for **all pods** cluster-wide. No custom scheduler pod is needed.

> **Why ClusterIP, not DNS?** `kube-scheduler` runs with `hostNetwork: true` and uses the node's `/etc/resolv.conf` — not CoreDNS — so `svc.cluster.local` names are unresolvable. kube-proxy iptables/eBPF rules make ClusterIPs reachable from the node network namespace on all cluster types. `deploy_extender.sh` resolves the ClusterIP at deploy time and embeds it in `urlPrefix`.

```
Pod created (any pod — default scheduler handles all)
        ▼
default kube-scheduler
        │  KubeSchedulerConfiguration from /etc/kubernetes/hiro-scheduler-config.yaml
        │  extender urlPrefix: http://<ClusterIP>:8090   ← resolved at deploy time
        │
        ├── Filter phase
        │     POST /extender/filter
        │     Body: ExtenderArgs { Pod *corev1.Pod, Nodes *corev1.NodeList }
        │             ▼
        │     PlacementServer.handleExtenderFilter (operator pod)
        │             ├── CheckEnergyGate(pod)
        │             │     ├── findProfileForPod()   O(1) field index
        │             │     └── fetchEAOProfile()     energy data from EAO CRD
        │             │
        │             ├── allowed  → ExtenderFilterResult { Nodes: all candidates }
        │             └── blocked  → ExtenderFilterResult { FailedNodes: all + reason }
        │                           scheduler defers pod — no node selected this cycle
        │
        └── Score phase (prioritize)
              POST /extender/prioritize
              Body: ExtenderArgs { Pod *corev1.Pod, Nodes *corev1.NodeList }
                      ▼
              PlacementServer.handleExtenderPrioritize (operator pod)
                      ├── builder.Build() — assemble DecisionRequest
                      │     ├── findProfileForPod()
                      │     ├── buildAOProfileContext()
                      │     └── fetchEAOProfile()
                      │
                      ├── client.RequestDecision()
                      │     POST DECISION_AGENT_URL
                      │     Response: { nodeScores: [{nodeName, score 0-100}, ...] }
                      │
                      ├── nodeScoresToHostPriorities()
                      │     score / 10  →  int64 [0-10]  (extender protocol)
                      │     missing node → score 5       (neutral fallback)
                      │
                      └── HostPriorityList [ {host, score 0-10}, ... ]
                              ▼
              kube-scheduler merges extender scores with built-in priorities
              selects highest-scored node → binds pod

On any error (profile not found, AI agent unreachable):
        handleExtenderPrioritize → equalPriorities() → score 5 for all nodes
        scheduling continues using built-in priorities only

If extender is unreachable (ignorable: true in ConfigMap):
        kube-scheduler logs "Skipping extender as it returned error and has ignorable flag set"
        scheduling continues normally — extender never blocks the cluster
```

```mermaid
flowchart TD
    Pod["Pod created<br/>(any pod — default scheduler handles all)"] --> KS["default kube-scheduler<br/>KubeSchedulerConfiguration from<br/>/etc/kubernetes/hiro-scheduler-config.yaml<br/>extender urlPrefix: http://ClusterIP:8090<br/>(resolved at deploy time)"]

    KS --> F["Filter phase<br/>POST /extender/filter<br/>Body: ExtenderArgs{Pod, Nodes}"]
    F --> HF["PlacementServer.handleExtenderFilter<br/>(operator pod)"]
    HF --> CEG["CheckEnergyGate(pod)<br/>findProfileForPod() (O(1) field index)<br/>fetchEAOProfile() (energy data from EAO CRD)"]
    CEG -->|"allowed"| FA["ExtenderFilterResult{Nodes: all candidates}"]
    CEG -->|"blocked"| FB["ExtenderFilterResult{FailedNodes: all + reason}<br/>scheduler defers pod — no node selected this cycle"]

    FA --> S["Score phase (prioritize)<br/>POST /extender/prioritize<br/>Body: ExtenderArgs{Pod, Nodes}"]
    S --> HP["PlacementServer.handleExtenderPrioritize<br/>(operator pod)"]
    HP --> B["builder.Build() — assemble DecisionRequest<br/>findProfileForPod()<br/>buildAOProfileContext()<br/>fetchEAOProfile()"]
    B --> RD["client.RequestDecision()<br/>POST DECISION_AGENT_URL<br/>Response: {nodeScores: [{nodeName, score 0-100}]}"]
    RD --> NS["nodeScoresToHostPriorities()<br/>score/10 → int64 [0-10] (extender protocol)<br/>missing node → score 5 (neutral fallback)"]
    NS --> HPL["HostPriorityList [{host, score 0-10}, ...]"]
    HPL --> Merge["kube-scheduler merges extender scores<br/>with built-in priorities<br/>selects highest-scored node → binds pod"]

    RD -.->|"on error: profile not found,<br/>AI agent unreachable"| EQ["equalPriorities()<br/>→ score 5 for all nodes<br/>scheduling continues using built-in priorities only"]
    EQ -.-> Merge

    KS -.->|"if extender unreachable<br/>(ignorable: true in ConfigMap)"| Ignore["kube-scheduler logs 'Skipping extender...'<br/>scheduling continues normally —<br/>extender never blocks the cluster"]
```

`ignorable: true` is set in `config/extender/scheduler-config.yaml`. The extender is never a hard dependency for scheduling.

For a detailed function-level trace of every call in this path see [docs/scheduling-flows.md — Path B](scheduling-flows.md#path-b--extender-default-kube-scheduler).

<a id="operator-logs-extender-side"></a>
### Operator logs (extender side)

| Event | Log message |
|-------|-------------|
| Filter request received | `"extender filter request received"` with pod/namespace/nodeCount |
| Energy gate blocked | `"extender filter: energy gate blocked scheduling"` with reason/blockedNodes |
| Prioritize request received | `"extender prioritize request received"` with pod/namespace/nodeCount |
| Prioritize response sent | `"extender prioritize response sent"` with topNode/nodeCount |

<a id="orchestrationprofile-crd"></a>
## OrchestrationProfile CRD

`OrchestrationProfile` is a cluster-scoped resource.

```yaml
apiVersion: orchestration.hiro.io/v1alpha1
kind: OrchestrationProfile
metadata:
  name: my-app-profile
spec:
  applicationRef:             # immutable after creation
    apiVersion: apps/v1
    kind: Deployment          # Deployment | StatefulSet | ReplicaSet | Job
    name: my-app
    namespace: default

  placement:
    strategy: Spread          # Balanced | Packed | Spread
    awareness:
      cpu: false
      memory: false
      gpu: false
      energy: true            # Enables EAOProfileContext enrichment

  rebalancing:
    enabled: true
    cooldownSeconds: 300
    triggerConditions:
      - "EnergyThreshold"     # EnergyThreshold | CPUThreshold | MemoryThreshold
                              # | NodeFailure | PeriodicReview
    replicasExternallyManaged: false   # true: an HPA/KEDA/GitOps owns replicas — never changed
    resourcesExternallyManaged: false  # true: a VPA or similar owns CPU/memory — never changed
```

`kind`, `strategy`, and `triggerConditions` are CRD-enforced enums — an unsupported value is rejected by the API server at `kubectl apply`/`helm upgrade` time, not accepted and surfaced later as `status.Error`.

For pods to be handled by the HIRO scheduler plugin, add to the pod template:

```yaml
spec:
  schedulerName: hiro-scheduler
```

**When `DEPLOY_SCHEDULER_PLUGIN=true`, you do not need to set `schedulerName` manually.** The `MutatingAdmissionWebhook` automatically sets `spec.schedulerName: hiro-scheduler` on any pod whose owning workload is referenced by an `OrchestrationProfile`. The webhook runs at pod CREATE time with `failurePolicy: Ignore` — pods are never blocked if the webhook is unavailable.

Pods without `schedulerName` (and not covered by a profile) use the default scheduler. When the extender approach is deployed, the default scheduler calls HIRO automatically for all pods.

<a id="placement-strategies"></a>
### Placement Strategies

The AI Decision Service's code scorer turns the strategy into a 0–100 score per candidate node, using utilisation after placing the pod. See [`ai-decision-service/README.md`](../ai-decision-service/README.md) for the formulas and settings.

| Strategy | Use when | Behaviour |
|----------|----------|-----------|
| `Balanced` (default) | General, latency-sensitive services | Prefers the least-full node, so no node gets hot; small penalty when CPU and memory use are uneven |
| `Packed` | Saving energy and cost | Prefers fuller nodes so others stay empty, but never above the packed limit (default 85%) |
| `Spread` | High availability | Prefers the zone, then the node, with the fewest of this app's pods; without zone labels, nodes only |

<a id="profile-status"></a>
### Profile Status

| Status | Meaning |
|--------|---------|
| `NoPods` | Referenced application has no pods yet |
| `Pending` | Pods exist but none are ready |
| `Active` | All observed pods are ready |
| `Partial` | Some pods are ready; others are pending |
| `Degraded` | One or more pods failed |
| `Error` | Spec validation failed, or an API error occurred while checking/discovering the referenced workload (a workload that simply doesn't exist *yet* is `NoPods`, not `Error`) |

<a id="status-conditions"></a>
### Status Conditions

`status.conditions` and `status.observedGeneration` follow the standard Kubernetes convention, so generic tooling (`kubectl wait --for=condition=...`, GitOps health checks) works without knowing anything `OrchestrationProfile`-specific:

| Condition | Set by | Meaning |
|---|---|---|
| `Ready` | Placement controller | Mirrors `status.status` — `True` only when `Active`. |
| `RebalancingEscalated` | Rebalance engine | `True` when the rebalance loop has paused itself after repeated failures (see [internal/rebalance/README.md](../internal/rebalance/README.md#escalation--pausing-on-repeated-failures)) — independent of `Ready`, since pods can be perfectly healthy while rebalancing is stuck needing a human. Only present once `spec.rebalancing.enabled: true` and at least one rebalance cycle has run. |

`status.observedGeneration` tells you whether the status above actually reflects your latest edit (compare it to `metadata.generation`) — useful right after a spec change, before the controller has had a chance to reconcile it.

```bash
kubectl describe orchestrationprofile <name>
```
