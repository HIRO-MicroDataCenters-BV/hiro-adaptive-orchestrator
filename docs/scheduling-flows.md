# Scheduling Flows — Function-Level Reference

This document traces every function call in both scheduling integration paths.
Both paths share the same core logic inside the operator — only the protocol
adapter layer differs.

---

## Shared Core (operator pod)

Both paths converge on these two methods in `PlacementServer` (`internal/placement-server/server.go`):

```
s.filter(ctx, pod)
  └── builder.CheckEnergyGate(ctx, pod)          [builder.go]
        ├── findProfileForPod()                   O(1) field-index lookup
        │     └── client.List(ProfileByAppRefIndex)
        ├── (if no profile or energy awareness off) → EnergyGateResponse{Allowed: true}
        └── fetchEAOProfile(ctx, pod)
              └── fetchEAOForPod()
                    ├── ResolveAppFromPod()        walk OwnerRef chain
                    └── client.List(EnergyAwareOrchestration)
              maps EAO spec/status → EnergyGateResponse{Allowed, Reason}

s.score(ctx, placementCtx, requestID)
  ├── builder.Build(ctx, placementCtx, requestID) [builder.go]
  │     ├── findProfileForPod()                   O(1) field-index lookup
  │     ├── buildAOProfileContext()               strategy + awareness + current placement
  │     └── fetchEAOProfile()                     (only if awareness.Energy == true)
  │           maps EAO CRD → EAOProfileContext
  │     → assembles DecisionRequest{pod, candidateNodes, AOProfile, EAOProfile}
  │
  └── client.RequestDecision(ctx, decisionReq)   [client.go]
        └── HTTP POST DECISION_AGENT_URL
              Body:     DecisionRequest (JSON)
              Response: DecisionResponse{nodeScores [{nodeName, score 0-100}], reason}
```

---

## Path A — Scheduler Plugin (hiro-scheduler)

Applies to pods with `spec.schedulerName: hiro-scheduler`.
The `HIROScore` plugin runs inside the `hiro-scheduler` pod.
The `PlacementServer` runs inside the operator pod.

```
Pod created
  spec.schedulerName: hiro-scheduler
          │
          ▼
┌─────────────────────────────────────────────────────────────────────┐
│ hiro-scheduler pod  [scheduler-plugin/plugin.go]                    │
│                                                                     │
│  ① PreFilter phase  (once per pod)                                  │
│    HIROScore.PreFilter(ctx, state, pod, nodes)                      │
│      └── client.CheckFilter(ctx, pod)     [client.go]              │
│            └── HTTP POST /api/v1/placement/filter                   │
│                  Body: EnergyGateRequest{Pod}                       │
│                          │                                          │
└──────────────────────────┼──────────────────────────────────────────┘
                           ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Operator pod  [server.go → builder.go]                              │
│                                                                     │
│    handleFilter()                                                   │
│      └── s.filter(ctx, pod)          ◄── shared core               │
│            └── builder.CheckEnergyGate()                           │
│                  ├── findProfileForPod()                            │
│                  └── fetchEAOProfile()                              │
│            → EnergyGateResponse{Allowed, Reason}                    │
└─────────────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────────┐
│ hiro-scheduler pod                                                  │
│                                                                     │
│    CheckFilter returns (allowed bool, reason string)                │
│    PreFilter stores → CycleState[energyGateStateKey]               │
│                                                                     │
│  ② Filter phase  (once per pod per node)                           │
│    HIROScore.Filter(ctx, state, pod, nodeInfo)                      │
│      └── reads CycleState[energyGateStateKey]                       │
│            ├── allowed  → nil  (node passes)                        │
│            └── blocked  → Status{Unschedulable, reason}             │
│                           all nodes blocked → pod deferred          │
│                                                                     │
│  ③ PreScore phase  (once per pod, after filter)                     │
│    HIROScore.PreScore(ctx, state, pod, nodes)                       │
│      └── client.Decide(ctx, placementCtx)  [client.go]             │
│            └── HTTP POST /api/v1/placement/score                    │
│                  Body: PlacementContext{pod, candidateNodes}         │
│                          │                                          │
└──────────────────────────┼──────────────────────────────────────────┘
                           ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Operator pod  [server.go → builder.go → client.go]                  │
│                                                                     │
│    handleScore()                                                    │
│      └── s.score(ctx, placementCtx, requestID)  ◄── shared core    │
│            ├── builder.Build()                                      │
│            │     ├── findProfileForPod()                            │
│            │     ├── buildAOProfileContext()                        │
│            │     └── fetchEAOProfile()  (if energy awareness on)   │
│            └── client.RequestDecision()                             │
│                  └── HTTP POST → External AI Agent                  │
│                        Response: DecisionResponse{nodeScores}       │
└─────────────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────────┐
│ hiro-scheduler pod                                                  │
│                                                                     │
│    Decide returns DecisionResponse{nodeScores [{name, score 0-100}]}│
│    PreScore stores nodeScoreMap → CycleState[cycleStateKey]         │
│                                                                     │
│  ④ Score phase  (once per pod per node)                             │
│    HIROScore.Score(ctx, state, pod, nodeInfo)                       │
│      └── reads CycleState[cycleStateKey]                            │
│            └── returns int64 score for this node (0–100)            │
│                                                                     │
│  hiro-scheduler selects highest-scored node → binds pod             │
└─────────────────────────────────────────────────────────────────────┘
```

**Soft-fail rules:**
- `PreFilter` error → stores `{allowed: true}` → pod is never blocked by infra failure
- `PreScore` error → stores empty map → `Score` returns 0 for all nodes → scheduler uses built-in priorities

---

## Path B — Extender (default kube-scheduler)

Applies to **all pods** cluster-wide. No custom scheduler pod needed.
The default `kube-scheduler` calls PlacementServer as a configured extender.

> `kube-scheduler` runs `hostNetwork: true` and uses the node's DNS — not CoreDNS.
> `deploy_extender.sh` resolves the PlacementServer ClusterIP at deploy time and
> embeds it in `urlPrefix` so kube-scheduler can reach it.

```
Pod created  (any pod — default scheduler)
          │
          ▼
┌─────────────────────────────────────────────────────────────────────┐
│ default kube-scheduler  (reads /etc/kubernetes/hiro-extender.yaml)  │
│                                                                     │
│  ① Filter phase  (once per pod)                                     │
│    HTTP POST /extender/filter                                       │
│    Body: ExtenderArgs{Pod, Nodes *NodeList}                         │
│                    │                                                │
└────────────────────┼────────────────────────────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Operator pod  [extender.go → server.go → builder.go]                │
│                                                                     │
│    handleExtenderFilter()                                           │
│      └── s.filter(ctx, args.Pod)     ◄── shared core               │
│            └── builder.CheckEnergyGate()                           │
│                  ├── findProfileForPod()                            │
│                  └── fetchEAOProfile()                              │
│            → EnergyGateResponse{Allowed, Reason}                    │
│                                                                     │
│      ├── Allowed  → ExtenderFilterResult{Nodes: all candidates}     │
│      └── Blocked  → ExtenderFilterResult{FailedNodes: all + reason} │
└─────────────────────────────────────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────────────────┐
│ default kube-scheduler                                              │
│                                                                     │
│    ├── Allowed  → continues to Score phase                          │
│    └── Blocked  → pod deferred this cycle (retried later)           │
│                                                                     │
│  ② Score phase / prioritize  (once per pod)                         │
│    HTTP POST /extender/prioritize                                   │
│    Body: ExtenderArgs{Pod, Nodes *NodeList}                         │
│                    │                                                │
└────────────────────┼────────────────────────────────────────────────┘
                     ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Operator pod  [extender.go → server.go → builder.go → client.go]   │
│                                                                     │
│    handleExtenderPrioritize()                                       │
│      └── s.score(ctx, placementCtx, requestID)  ◄── shared core    │
│            ├── builder.Build()                                      │
│            │     ├── findProfileForPod()                            │
│            │     ├── buildAOProfileContext()                        │
│            │     └── fetchEAOProfile()  (if energy awareness on)   │
│            └── client.RequestDecision()                             │
│                  └── HTTP POST → External AI Agent                  │
│                        Response: DecisionResponse{nodeScores}       │
│                                                                     │
│      └── nodeScoresToHostPriorities()                               │
│            score / 10  →  int64 [0–10]  (extender protocol)        │
│            missing node → score 5  (neutral fallback)               │
│            → HostPriorityList [{host, score 0-10}, ...]             │
└─────────────────────────────────────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────────────────┐
│ default kube-scheduler                                              │
│                                                                     │
│    merges extender scores with built-in priorities                  │
│    selects highest-scored node → binds pod                          │
└─────────────────────────────────────────────────────────────────────┘
```

**Soft-fail rules:**
- `handleExtenderFilter` error → `EnergyGateResponse{Allowed: true}` → scheduling continues
- `handleExtenderPrioritize` error → `equalPriorities()` → score 5 for all nodes
- Extender unreachable (`ignorable: true`) → kube-scheduler logs and continues normally

---

## Shared vs Protocol-Specific

```
                        ┌─────────────────────────────────┐
                        │         PlacementServer          │
                        │                                  │
  Plugin  →  /filter    │  handleFilter()    ─┐            │
  Extender → /extender/ │  handleExtenderFilter() ─┘ s.filter() │
  filter                │                    └► builder.CheckEnergyGate() │
                        │                                  │
  Plugin  →  /score     │  handleScore()     ─┐            │
  Extender → /extender/ │  handleExtenderPrioritize() ─┘ s.score() │
  prioritize            │                    └► builder.Build()     │
                        │                       + client.RequestDecision() │
                        └─────────────────────────────────┘
                                      ▲
                              shared core — runs once
                              regardless of which path
                              called it
```

The protocol-specific layers (encoding/decoding `ExtenderArgs`, `HostPriorityList`,
`PlacementContext`, `EnergyGateRequest`) are thin adapters; the business logic
runs exactly once in `s.filter()` and `s.score()`.
