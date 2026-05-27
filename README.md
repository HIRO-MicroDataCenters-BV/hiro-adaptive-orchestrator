# HIRO Adaptive Orchestrator

A Kubernetes operator that provides intelligent, AI-driven pod placement and adaptive workload orchestration. It introduces the `OrchestrationProfile` custom resource to bind placement strategies to workloads, and integrates with the Kubernetes scheduler to score nodes using an external AI decision agent.

---

## Table of Contents

- [Architecture](#architecture)
  - [Components](#components)
  - [Flow 1 — Reconciliation Controller](#flow-1--reconciliation-controller)
  - [Flow 2 — Placement Server](#flow-2--placement-server-ai-driven-scheduling)
  - [Flow 3 — HIRO Scheduler Plugin](#flow-3--hiro-scheduler-plugin)
  - [Parameter Flow](#parameter-flow)
- [Features](#features)
- [Prerequisites](#prerequisites)
- [Quick Start](#quick-start)
- [Deployment](#deployment)
  - [Full-Stack (operator + scheduler)](#full-stack-operator--scheduler)
  - [Operator Only](#operator-only)
  - [Scheduler Only](#scheduler-only)
  - [Legacy Extender Only](#legacy-extender-only)
  - [Manual Kustomize](#manual-kustomize)
  - [Helm](#helm)
  - [YAML Bundle](#yaml-bundle)
- [Configuration Reference](#configuration-reference)
  - [All Parameters](#all-parameters)
  - [Operator Environment Variables](#operator-environment-variables)
  - [Scheduler Plugin Config](#scheduler-plugin-config)
  - [OrchestrationProfile CRD](#orchestrationprofile-crd)
- [Scheduler Integration Approaches](#scheduler-integration-approaches)
  - [Plugin Approach (recommended)](#plugin-approach-recommended)
  - [Extender Approach (legacy)](#extender-approach-legacy)
- [Mock Decision Agent](#mock-decision-agent)
- [Testing](#testing)
- [Development Workflow](#development-workflow)
- [Project Structure](#project-structure)
- [Upgrading Go or Kubernetes Version](#upgrading-go-or-kubernetes-version)
- [Contributing](#contributing)
- [License](#license)

---

## Architecture

![Architecture](./K8S_adaptive_orchestrator_new-Integrate-EnergyAwareOrchestrator-1.gif)

### Components

The system consists of two independently deployed binaries:

| Component | Binary | Description |
|-----------|--------|-------------|
| **Operator** | `cmd/main.go` | Reconciliation controller + PlacementServer HTTP service |
| **Scheduler Plugin** | `scheduler-plugin/cmd/main.go` | Custom `kube-scheduler` binary with `HIROScore` plugin registered |

```
┌─────────────────────────────────────────────────────────────────────┐
│  Kubernetes API Server                                              │
└──────────┬──────────────────────────────────────┬───────────────────┘
           │ watch OrchestrationProfiles           │ schedule pods
           │ watch Deployments/StatefulSets/Jobs   │ (schedulerName: hiro-scheduler)
           ▼                                       ▼
┌──────────────────────┐              ┌────────────────────────┐
│  HIRO Operator       │              │  HIRO Scheduler        │
│  ─────────────────   │              │  (hiro-scheduler pod)  │
│  Reconciler          │              │                        │
│  PlacementServer     │◄─────POST────│  HIROScore plugin      │
│  :8090               │   PlacCtx    │  PreScore / Score      │
└──────────┬───────────┘              └────────────────────────┘
           │ POST DecisionRequest
           ▼
┌──────────────────────┐
│  Decision Agent      │
│  (AI / Mock)         │
│  :8080               │
└──────────────────────┘
```

### Flow 1 — Reconciliation Controller

```
Kubernetes API server
        │  OrchestrationProfile created / updated
        │  Deployment / StatefulSet / Job updated
        │  Pod phase changed
        ▼
Reconciliation Controller
        ├── Validate spec (applicationRef, strategy, awareness)
        │       on failure → status=Error + Kubernetes Event
        ├── Resolve workload (Deployment / StatefulSet / Job)
        │       not found → status=Error + Kubernetes Event
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

### Flow 2 — Placement Server (AI-driven scheduling)

Called by the HIROScore scheduler plugin for every pending pod with `schedulerName: hiro-scheduler`:

```
HIROScore.PreScore (inside hiro-scheduler pod)
        │  POST /api/v1/placement/decision  { pod, candidateNodes }
        ▼
PlacementServer (:8090, inside operator pod)
        ├── Look up OrchestrationProfile for the pod (O(1) field index)
        ├── Build DecisionRequest
        │   ├── AOProfileContext   (strategy + awareness + current placement)
        │   ├── EAOProfileContext  (energy data, if energy awareness enabled)
        │   └── CandidateNodes     (full Node objects from scheduler)
        ▼
External Decision Agent  (DECISION_AGENT_URL)
        │  { nodeScores: [{nodeName, score}, ...] }
        ▼
HIROScore.Score → returns per-node AI score → scheduler selects highest-scored node
```

### Flow 3 — HIRO Scheduler Plugin

`hiro-scheduler` is a standard `kube-scheduler` binary extended with the `HIROScore` plugin. Only pods with `spec.schedulerName: hiro-scheduler` are routed here; all other pods continue to use the default scheduler.

```
Pod created with spec.schedulerName: hiro-scheduler
        ▼
hiro-scheduler framework
        ├── Filter phase  →  HIROScore.Filter  (soft-fail open — never blocks scheduling)
        ├── PreScore phase →  HIROScore.PreScore
        │     POST PlacementContext{pod, candidateNodes} to operator PlacementServer
        │     Stash NodeScores in CycleState
        └── Score phase   →  HIROScore.Score
              Read NodeScore from CycleState → return to framework
```

Plugin configuration (URL, path, timeout) is read from `KubeSchedulerConfiguration` `pluginConfig` — see [Scheduler Plugin Config](#scheduler-plugin-config).

### Parameter Flow

```
deploy_all.sh  (master parameter sheet — all vars defined here)
        │  exports every variable
        │
        ├──► deploy_operator.sh
        │         │
        │         ├─ sed placement_service_patch.yaml base name
        │         │       → kustomize applies NAME_PREFIX
        │         │       → Kubernetes Service named PLACEMENT_SERVICE_NAME  ✓
        │         │
        │         └─ kubectl set env deployment/<NAME_PREFIX>controller-manager
        │                 ▼
        │           Operator pod  (cmd/main.go reads os.Getenv)
        │                 ▼
        │           PlacementServer / DecisionClient constructed with those values
        │
        ├──► deploy_scheduler.sh
        │         kustomize build config/scheduler/ | sed (patches pluginConfig)
        │                 ▼
        │         ConfigMap hiro-scheduler-config  (KubeSchedulerConfiguration)
        │                 ▼
        │         hiro-scheduler pod mounts ConfigMap
        │                 ▼
        │         HIROScore.New() reads pluginConfig.args → NewPlacementClient(url, path, timeout)
        │
        └──► deploy_extender.sh  (optional, APPLY_EXTENDER_CONFIG=true)
                  sed → ConfigMap hiro-scheduler-config in kube-system
                  (for use with default kube-scheduler as extender)
```

`PLACEMENT_SERVICE_NAME` is applied at **deploy time** to set the Kubernetes Service name. It is not injected into the operator pod (the operator only listens on a port; routing is handled by Kubernetes). The scheduler and extender scripts use `PLACEMENT_SERVICE_NAME` to construct the URL they call.

Each sub-script also works **standalone** — it carries its own `:-` defaults for every variable. When called from `deploy_all.sh`, the parent exports override the defaults.

---

## Features

- **Placement strategies** — `Balanced`, `Packed`, `Spread`
- **Multi-dimensional resource awareness** — CPU, Memory, GPU, Energy
- **Energy-aware orchestration** — optional integration with an `EnergyAwareOrchestration` CRD
- **Dynamic rebalancing** — trigger-based (energy threshold, CPU/memory threshold, node failure, scheduled)
- **AI-delegated scoring** — pluggable external decision agent via HTTP
- **Custom scheduler plugin** — `HIROScore` runs as a separate `hiro-scheduler` binary; pods opt in via `schedulerName: hiro-scheduler`
- **Legacy extender support** — for managed clusters where a custom scheduler pod cannot be deployed
- **Status observability** — `NoPods` → `Pending` → `Active` → `Partial` → `Degraded` → `Error`
- **Kubernetes events** — status transitions and errors recorded as events on `OrchestrationProfile`
- **Production-ready** — leader election, HTTPS metrics (:8443), Prometheus/ServiceMonitor support, restricted pod security

---

## Prerequisites

| Tool | Minimum version | Purpose |
|------|----------------|---------|
| Go | 1.25 | Build operator from source |
| Docker | any recent | Build container images |
| kubectl | v1.29+ | Interact with cluster |
| Kustomize | v5.8+ | Deploy via manifests |
| Helm | v3+ | Deploy via Helm chart |
| Kind | any recent | Local / E2E testing |
| kubebuilder | v4 | Scaffold / code generation |

A running Kubernetes cluster (v1.29+) with `~/.kube/config` pointing to it is required for deployment.

An **external Decision Agent** reachable at a URL you control is required for the placement server to function. For local testing, a mock agent is deployed automatically (see [Mock Decision Agent](#mock-decision-agent)).

---

## Quick Start

```bash
export GITHUB_PAT_TOKEN=<your-ghcr-token>

# Full-stack: operator + scheduler plugin (mock agent, default namespace)
hack/deploy_all.sh

# Full-stack with a real AI agent
USE_MOCK_AGENT=false DECISION_AGENT_URL=http://ai.example.com:8080 \
  hack/deploy_all.sh

# Custom namespace + name prefix
NAMESPACE=my-ns NAME_PREFIX=my-org- hack/deploy_all.sh

# Also deploy the legacy extender ConfigMap (for managed clusters)
APPLY_EXTENDER_CONFIG=true hack/deploy_all.sh
```

---

## Deployment

All deploy scripts share the same parameter model: every variable has a default and can be overridden via environment. See [All Parameters](#all-parameters) for the full reference.

### Full-Stack (operator + scheduler)

```bash
export GITHUB_PAT_TOKEN=<token>
hack/deploy_all.sh [kubeconfig-path]
```

Runs four phases in order:
1. **deploy_operator** — build, push, and deploy the operator
2. **wait_for_placement_server** — wait for the operator pod to be Ready
3. **deploy_scheduler** — build, push, and deploy the HIRO scheduler
4. **deploy_extender** — (skipped unless `APPLY_EXTENDER_CONFIG=true`)

### Operator Only

```bash
export GITHUB_PAT_TOKEN=<token>
hack/deploy_operator.sh [kubeconfig-path]
```

Steps performed:
1. Lint, generate code, install CRDs, build binary
2. Build and push operator Docker image to GHCR
3. Configure Kustomize (namespace + namePrefix)
4. Patch `placement_service_patch.yaml` with the derived base name so kustomize produces `PLACEMENT_SERVICE_NAME` as the Service name
5. Deploy operator via `make deploy`
6. Deploy mock decision agent (if `USE_MOCK_AGENT=true`)
7. Create GHCR image pull secret
8. Patch ServiceAccount with pull secret
9. Inject environment variables into the operator Deployment (including `PLACEMENT_SERVICE_NAME`)
10. Restart operator pod and wait for Ready

### Scheduler Only

The operator must already be running before deploying the scheduler.

```bash
export GITHUB_PAT_TOKEN=<token>
hack/deploy_scheduler.sh [kubeconfig-path]
```

Steps performed:
1. Build and push scheduler Docker image (`hiro-scheduler:<version>-k8s<K8S_VERSION>`)
2. Configure Kustomize for `config/scheduler/` (namespace, namePrefix, image)
3. Patch `KubeSchedulerConfiguration` ConfigMap with placement server URL/path/timeout
4. Apply manifests (ServiceAccount, ClusterRole, ClusterRoleBinding, ConfigMap, Deployment)
5. Wait for scheduler deployment rollout

### Legacy Extender Only

For managed Kubernetes clusters (GKE Autopilot, EKS Fargate, etc.) where a custom scheduler pod cannot run. This configures the **default** `kube-scheduler` to use the operator's PlacementServer as an extender.

```bash
hack/deploy_extender.sh [kubeconfig-path]
```

This applies a `KubeSchedulerConfiguration` ConfigMap to `kube-system`. After applying, you must manually mount it into the `kube-scheduler` static pod. See `config/extender/scheduler-config.yaml` for full instructions.

### Manual Kustomize

```bash
export IMG=<registry>/<image>:<tag>
make docker-build docker-push IMG=$IMG
make deploy IMG=$IMG

# Tear down
make undeploy && make uninstall
```

### Helm

The Helm chart in `dist/chart/` is auto-generated from `config/`. To regenerate:

```bash
rm -rf dist/
kubebuilder edit --plugins=helm/v2-alpha
```

Deploy:

```bash
make helm-deploy IMG=$IMG
make helm-deploy IMG=$IMG HELM_NAMESPACE=my-namespace
make helm-deploy IMG=$IMG HELM_EXTRA_ARGS="--set manager.replicas=2"

make helm-status
make helm-rollback
make helm-uninstall
```

### YAML Bundle

```bash
make build-installer IMG=<registry>/<image>:<tag>
kubectl apply -f dist/install.yaml
```

---

## Configuration Reference

### All Parameters

Every parameter can be set as an environment variable before calling any deploy script. `deploy_all.sh` exports all of them; each sub-script carries its own `:-` default for standalone use.

#### Identity (shared by all scripts)

| Variable | Default | Description |
|----------|---------|-------------|
| `GITHUB_PAT_TOKEN` | — | **Required.** GitHub PAT with `write:packages` scope |
| `GITHUB_USERNAME` | `sskrishnav` | ghcr.io login |
| `NAMESPACE` | `hiro-adaptive-orchestrator-system` | Kubernetes namespace for all resources |
| `NAME_PREFIX` | `hiro-adaptive-orchestrator-` | Kustomize namePrefix, prepended to all resource names |

#### PlacementServer (operator exposes it; scheduler and extender call it)

| Variable | Default | Description |
|----------|---------|-------------|
| `PLACEMENT_SERVICE_NAME` | `<NAME_PREFIX>controller-manager-placement-service` | k8s Service name for the PlacementServer. The deploy script patches the manifest so kustomize produces this exact name. Used by the scheduler and extender to build the URL they call. |
| `PLACEMENT_SERVER_PORT` | `:8090` | Port the PlacementServer listens on |
| `PLACEMENT_SERVER_PATH` | `/api/v1/placement/decision` | HTTP path for placement decisions |
| `PLACEMENT_SERVER_HEALTH_PATH` | `/healthz` | HTTP path for health probes |
| `PLACEMENT_TIMEOUT_SECS` | `8` | Scheduler plugin → PlacementServer request timeout (seconds) |

#### Operator — Decision Agent

| Variable | Default | Description |
|----------|---------|-------------|
| `USE_MOCK_AGENT` | `true` | Deploy and use the in-cluster mock agent |
| `DECISION_AGENT_URL` | `http://decision-agent:8080` (mock) | Base URL of the AI agent. **Required when `USE_MOCK_AGENT=false`.** |
| `DECISION_AGENT_PATH` | `/api/v1/agent/placement/decision` | HTTP path on the AI agent |

#### Operator — Extender Paths

| Variable | Default | Description |
|----------|---------|-------------|
| `EXTENDER_FILTER_PATH` | `/extender/filter` | Path for legacy extender filter calls |
| `EXTENDER_PRIORITIZE_PATH` | `/extender/prioritize` | Path for legacy extender prioritize calls |

#### Operator — EnergyAwareOrchestration CRD

| Variable | Default | Description |
|----------|---------|-------------|
| `EAO_GROUP` | `eas.hiro.io` | API group of the `EnergyAwareOrchestration` CRD |
| `EAO_VERSION` | `v1` | API version |
| `EAO_KIND` | `EnergyAwareOrchestration` | Kind name |

#### Scheduler

| Variable | Default | Description |
|----------|---------|-------------|
| `SCHED_K8S_VERSION` | `v1.35.0` | Kubernetes minor version the scheduler binary is compiled against. Must match the target cluster. |
| `SCHED_VERSION` | `v0.1.0` | Scheduler release version (used in the Docker image tag) |

#### Deploy Options

| Variable | Default | Description |
|----------|---------|-------------|
| `APPLY_EXTENDER_CONFIG` | `false` | Set to `true` to also run `deploy_extender.sh` as Phase 4 |

---

### Operator Environment Variables

The operator pod reads configuration exclusively from environment variables. These are injected into the Deployment by `inject_operator_env_vars()` in `deploy_operator.sh` via `kubectl set env`.

| Variable | Default | Read by |
|----------|---------|---------|
| `DECISION_AGENT_URL` | `http://decision-agent:8080` | `decision.NewDecisionClient` |
| `DECISION_AGENT_PATH` | `/api/v1/agent/placement/decision` | `decision.NewDecisionClient` |
| `PLACEMENT_SERVER_PORT` | `:8090` | `decision.NewPlacementServer` |
| `PLACEMENT_SERVER_PATH` | `/api/v1/placement/decision` | `decision.NewPlacementServer` |
| `PLACEMENT_SERVER_HEALTH_PATH` | `/healthz` | `decision.NewPlacementServer` |
| `EXTENDER_FILTER_PATH` | `/extender/filter` | `decision.NewPlacementServer` |
| `EXTENDER_PRIORITIZE_PATH` | `/extender/prioritize` | `decision.NewPlacementServer` |
| `EAO_GROUP` | `eas.hiro.io` | `decision.NewDecisionContextBuilder` |
| `EAO_VERSION` | `v1` | `decision.NewDecisionContextBuilder` |
| `EAO_KIND` | `EnergyAwareOrchestration` | `decision.NewDecisionContextBuilder` |

---

### Scheduler Plugin Config

The `HIROScore` plugin is configured via `pluginConfig` inside the `KubeSchedulerConfiguration` ConfigMap (`config/scheduler/configmap.yaml`). The deploy script patches these values from shell variables before applying.

```yaml
pluginConfig:
  - name: HIROScore
    args:
      placementServerURL:  "http://<PLACEMENT_SERVICE_NAME>.<NAMESPACE>.svc.cluster.local<PLACEMENT_SERVER_PORT>"
      placementServerPath: "/api/v1/placement/decision"   # PLACEMENT_SERVER_PATH
      timeoutSeconds:      8                              # PLACEMENT_TIMEOUT_SECS
```

To update a running scheduler's config without redeployment:

```bash
kubectl edit configmap <NAME_PREFIX>hiro-scheduler-config -n <NAMESPACE>
# Then restart the scheduler:
kubectl rollout restart deployment/<NAME_PREFIX>hiro-scheduler -n <NAMESPACE>
```

---

### OrchestrationProfile CRD

`OrchestrationProfile` is a cluster-scoped resource.

```yaml
apiVersion: orchestration.hiro.io/v1alpha1
kind: OrchestrationProfile
metadata:
  name: my-app-profile
spec:
  applicationRef:
    apiVersion: apps/v1
    kind: Deployment          # Deployment | StatefulSet | Job
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
                              # | NodeFailure | Scheduled
```

For pods to be handled by the HIRO scheduler, add to the pod template:

```yaml
spec:
  schedulerName: hiro-scheduler
```

#### Placement Strategies

| Strategy | Behaviour |
|----------|-----------|
| `Balanced` | Distributes pods evenly across nodes |
| `Packed` | Concentrates pods on the fewest nodes (bin-packing) |
| `Spread` | Maximises pod spread across failure domains |

#### Profile Status

| Status | Meaning |
|--------|---------|
| `NoPods` | Referenced application has no pods yet |
| `Pending` | Pods exist but none are ready |
| `Active` | All observed pods are ready |
| `Partial` | Some pods are ready; others are pending |
| `Degraded` | One or more pods failed |
| `Error` | Spec validation failed or referenced workload not found |

```bash
kubectl describe orchestrationprofile <name>
```

---

## Scheduler Integration Approaches

### Plugin Approach (recommended)

Deploys `hiro-scheduler` as a standalone scheduler pod. Pods explicitly opt in by setting `spec.schedulerName: hiro-scheduler`. The default `kube-scheduler` continues to handle all other pods.

**Pros:** Clean separation, no changes to existing workloads, per-pod opt-in, HA-ready (2 replicas with leader election).

```bash
# Deploy
hack/deploy_scheduler.sh

# Verify
kubectl get pods -n hiro-adaptive-orchestrator-system -l app=hiro-scheduler
kubectl logs -n hiro-adaptive-orchestrator-system deployment/hiro-adaptive-orchestrator-hiro-scheduler

# Pin a workload to the HIRO scheduler
kubectl patch deployment my-app -p '{"spec":{"template":{"spec":{"schedulerName":"hiro-scheduler"}}}}'
```

To build the scheduler for a different Kubernetes version:

```bash
# Update go.mod and rebuild
make pin-k8s-version SCHED_K8S_VERSION=v1.36.0
make build-scheduler
make docker-build-scheduler SCHED_K8S_VERSION=v1.36.0
```

### Extender Approach (legacy)

Routes the **default** `kube-scheduler`'s filter and prioritize calls to the operator's PlacementServer. Use this on managed clusters (GKE Autopilot, EKS Fargate) where deploying a custom scheduler pod is not possible.

**Cons:** Affects all pods (not opt-in), requires patching the static `kube-scheduler` pod, higher coupling to cluster internals.

```bash
# Deploy extender ConfigMap
hack/deploy_extender.sh

# Or as part of full-stack deploy:
APPLY_EXTENDER_CONFIG=true hack/deploy_all.sh
```

After applying, mount the ConfigMap into the `kube-scheduler` pod and pass `--config=/etc/kubernetes/scheduler-config.yaml`. See `config/extender/scheduler-config.yaml` for full instructions.

---

## Mock Decision Agent

For local and CI testing a mock agent is included. It responds to every placement request with all candidate nodes scored equally at `50`.

```bash
# Deployed automatically when USE_MOCK_AGENT=true (the default)
hack/deploy_operator.sh

# Deploy manually
kubectl apply -f hack/mock-decision-agent.yaml

# Check it is running
kubectl get pods -n hiro-adaptive-orchestrator-system -l app=decision-agent
```

The mock agent listens at `http://decision-agent:8080` inside the operator namespace, matching the default `DECISION_AGENT_URL`.

---

## Testing

### Unit & Integration Tests

```bash
make test
```

Uses **Ginkgo v2 + Gomega** with `controller-runtime/envtest` (real Kubernetes API server + etcd, no cluster needed). Coverage profile written to `cover.out`.

### End-to-End Tests

```bash
make test-e2e
```

Runs against an isolated **Kind** cluster (created and torn down automatically). Never run against a development or production cluster.

### Lint

```bash
make lint        # report issues
make lint-fix    # auto-fix where possible
```

---

## Development Workflow

```
Edit *_types.go or markers
        ├── make manifests   (regenerate CRDs / RBAC)
        └── make generate    (regenerate DeepCopy)

Edit *.go files
        ├── make lint-fix    (auto-fix style)
        └── make test        (unit + integration tests)

Deploy operator only
        └── hack/deploy_operator.sh

Deploy scheduler only (operator must be running)
        └── hack/deploy_scheduler.sh

Deploy everything
        └── hack/deploy_all.sh
```

> **Never manually edit** auto-generated files: `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, `zz_generated.*.go`, `dist/chart/`, `dist/install.yaml`.

---

## Project Structure

```
cmd/
  main.go                              # Operator entry point: manager + PlacementServer
api/v1alpha1/
  orchestrationprofile_types.go        # CRD schema
  zz_generated.deepcopy.go             # Auto-generated — DO NOT EDIT
pkg/
  placement/
    types.go                           # Shared wire types (PlacementContext, DecisionResponse,
                                       # NodeScore) — importable by both operator and scheduler
internal/
  controller/
    orchestrationprofile_controller.go # Main reconciler
    op_index.go                        # O(1) field index (ProfileByAppRefIndex)
    op_watchers.go                     # Pod/Workload → Profile event mapping
    op_validation.go                   # Spec validation
    op_status.go                       # Status computation + event recording
    op_pod_discovery.go                # Pod resolution (OwnerReference walk)
    op_constants.go                    # Status enum + event reason constants
  decision/
    server.go                          # PlacementServer HTTP service (:8090)
                                       #   POST /api/v1/placement/decision  (plugin)
                                       #   POST /extender/filter            (legacy)
                                       #   POST /extender/prioritize        (legacy)
    builder.go                         # Assembles DecisionRequest (incl. EAO profile fetch)
    client.go                          # HTTP client to external AI agent
    types.go                           # Type aliases → pkg/placement (zero churn)
  utils/
    helpers.go                         # ResolveAppFromPod, KeysOf, NodeNames
scheduler-plugin/                      # Separate Go module (own go.mod)
  plugin.go                            # HIROScore: FilterPlugin + PreScorePlugin + ScorePlugin
  client.go                            # PlacementClient (HTTP POST to operator)
  client_test.go                       # httptest-based unit tests
  cmd/
    main.go                            # hiro-scheduler binary entry point
  Dockerfile                           # Build context: repo root
  go.mod                               # k8s.io/kubernetes v1.35.0 + staging replace block
  pin_k8s_version.sh                   # Pin go.mod to a specific K8s minor version
config/
  crd/bases/                           # Generated CRDs — DO NOT EDIT
  rbac/                                # Generated RBAC — DO NOT EDIT
  manager/
    manager.yaml                       # Operator Deployment (env vars, ports, resources)
    placement_service_patch.yaml       # ClusterIP Service for PlacementServer (:8090)
  scheduler/
    kustomization.yaml                 # Kustomize base for scheduler manifests
    serviceaccount.yaml                # Scheduler ServiceAccount
    clusterrole.yaml                   # Scheduler RBAC (nodes, pods, bindings, leases…)
    clusterrolebinding.yaml
    configmap.yaml                     # KubeSchedulerConfiguration + HIROScore pluginConfig
    deployment.yaml                    # hiro-scheduler Deployment (2 replicas, HA)
  extender/
    scheduler-config.yaml              # Legacy KubeSchedulerConfiguration for extender mode
  samples/                             # Example OrchestrationProfile + nginx Deployment
  default/                             # Kustomize overlay (namespace, namePrefix)
hack/
  deploy_all.sh                        # Full-stack entry point — all parameters defined here
  deploy_operator.sh                   # Operator-only deploy
  deploy_scheduler.sh                  # Scheduler-only deploy
  deploy_extender.sh                   # Legacy extender ConfigMap deploy
  mock-decision-agent.yaml             # In-cluster mock AI agent
dist/
  chart/                               # Generated Helm chart — DO NOT EDIT
  install.yaml                         # Generated single-file install bundle
test/e2e/                              # End-to-end tests (Kind)
```

---

## Upgrading Go or Kubernetes Version

### Go Version

Three files must be kept in sync (not updated automatically by kubebuilder):

| File | Field |
|------|-------|
| `go.mod` | `go X.Y.Z` |
| `Makefile` | `GOLANGCI_LINT_VERSION` |
| `.custom-gcl.yml` | `version:` |

```bash
# 1. Update go.mod
go mod edit -go=X.Y && go mod tidy

# 2. Find a compatible golangci-lint version
curl -s "https://proxy.golang.org/github.com/golangci/golangci-lint/v2/@v/vX.Y.Z.mod" | grep "^go "

# 3. Update Makefile: GOLANGCI_LINT_VERSION ?= vX.Y.Z
# 4. Update .custom-gcl.yml: version: vX.Y.Z (must match)

# 5. Verify
rm -f bin/golangci-lint*
make lint
```

### Kubernetes Version (scheduler plugin)

The scheduler binary must be compiled against the same Kubernetes minor version as the target cluster.

```bash
# Update scheduler-plugin/go.mod and rebuild
make pin-k8s-version SCHED_K8S_VERSION=v1.36.0
cd scheduler-plugin && go mod tidy

# Rebuild and push image
make docker-build-scheduler SCHED_K8S_VERSION=v1.36.0

# Redeploy scheduler with the new version
SCHED_K8S_VERSION=v1.36.0 hack/deploy_scheduler.sh
```

The `pin_k8s_version.sh` script uses `go mod edit` to surgically update only the `k8s.io/kubernetes` require entry and the staging module replace directives — no full file rewrite.

---

## Project Initialization

Scaffolded using [Kubebuilder](https://book.kubebuilder.io/):

```bash
kubebuilder init \
  --domain orchestration.hiro.io \
  --repo github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator \
  --owner "HIRO Adaptive Orchestrator"

kubebuilder create api \
  --group orchestration --version v1alpha1 --kind OrchestrationProfile \
  --resource=true --controller=true

kubebuilder edit --plugins=helm/v2-alpha
```

> Do **not** re-run these on an existing checkout — they modify project scaffolding.

---

## Contributing

1. Fork the repository and create a feature branch.
2. Run `make manifests generate` after editing types.
3. Run `make lint-fix test` before opening a pull request.
4. E2E tests are validated in CI via GitHub Actions against a Kind cluster.

For detailed development guidelines, Kubebuilder CLI cheat sheet, API design conventions, and logging standards, see [AGENTS.md](./AGENTS.md).

---

## License

Licensed under the [Apache License, Version 2.0](https://www.apache.org/licenses/LICENSE-2.0).
