# HIRO Adaptive Orchestrator

A Kubernetes operator that provides intelligent, AI-driven pod placement and adaptive workload orchestration. It introduces the `OrchestrationProfile` custom resource to bind placement strategies to workloads, and integrates with the Kubernetes scheduler to score nodes using an external AI decision agent.

## Architecture

![Architecture](./K8S_%20adaptive_orchestrator_new-Integrate-EnergyAwareOrchestrator-1.gif)

The operator runs as two concurrent services inside a single binary:

| Service | Description |
|---------|-------------|
| **Reconciliation Controller** | Watches `OrchestrationProfile` CRDs and associated workloads (Deployments, StatefulSets, Jobs). Validates specs, discovers pods, and maintains profile status. |
| **Placement Server** (`HTTP :8090`) | Receives `PlacementContext` from a custom kube-scheduler plugin (pod + candidate nodes), enriches it with profile data via O(1) field-indexed cache lookups, delegates scoring to an external AI agent, and returns `NodeScores` to the scheduler. |

### Request Flow

```
kube-scheduler plugin
        │
        │  POST /placement  { pod, candidateNodes }
        ▼
PlacementServer (:8090)
        │
        │  Build DecisionRequest
        │  ├── OrchestrationProfile  (O(1) field index)
        │  ├── AOProfileContext      (strategy + awareness + current placement)
        │  └── EAOProfileContext     (energy data, if energy awareness enabled)
        ▼
External AI / Decision Agent  (DECISION_AGENT_URL)
        │
        │  { nodeScores }
        ▼
kube-scheduler plugin  →  schedules pod
```

---

## Features

- **Placement strategies** — `Balanced`, `Packed`, `Spread`
- **Multi-dimensional resource awareness** — CPU, Memory, GPU, Energy
- **Energy-aware orchestration** — optional integration with an Energy-Aware Orchestrator
- **Dynamic rebalancing** — trigger-based (energy threshold, CPU/memory threshold, node failure, scheduled)
- **AI-delegated scoring** — pluggable external decision agent via HTTP
- **Status observability** — rich per-profile status (`NoPods` → `Pending` → `Active` → `Partial` → `Degraded` → `Error`)
- **Production-ready** — leader election, HTTPS metrics (port 8443), Prometheus/ServiceMonitor support, restricted pod security

---

## Prerequisites

| Tool | Minimum version | Purpose |
|------|----------------|---------|
| Go | 1.25 | Build from source |
| Docker | any recent | Build container image |
| kubectl | v1.29+ | Interact with cluster |
| Kustomize | v5.8+ | Deploy via manifests |
| Helm | v3+ | Deploy via Helm chart |
| Kind | any recent | Local / E2E testing |
| kubebuilder | v4 | Scaffold / code generation |

A running Kubernetes cluster (v1.29+) with a `~/.kube/config` pointing to it is required for deployment.

An **external Decision Agent** reachable at a URL you control is required for the placement server to function (see [Configuration](#configuration)).

---

## Getting Started — Local Development

### 1. Clone the repository

```bash
git clone https://github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator.git
cd hiro-adaptive-orchestrator
```

### 2. Install dependencies

All Go dependencies are managed via `go.mod`. Build tools (controller-gen, kustomize, envtest, golangci-lint) are downloaded automatically by `make` targets into `./bin/`.

```bash
go mod download
```

### 3. Run code generation

After cloning, or any time you edit `*_types.go` or kubebuilder markers, regenerate the manifests and DeepCopy methods:

```bash
make manifests   # Regenerate CRDs and RBAC from markers
make generate    # Regenerate DeepCopy methods
```

### 4. Install CRDs into your cluster

```bash
make install
```

Verify the CRD was installed:

```bash
kubectl get crd orchestrationprofiles.orchestration.hiro.io
```

### 5. Run the operator locally

Running locally uses your current `kubeconfig` context. Set the required environment variable first:

```bash
export DECISION_AGENT_URL="http://your-decision-agent:8080"
make run
```

The controller starts and the placement server listens on `:8090` by default.

### 6. Apply a sample resource

```bash
kubectl apply -k config/samples/
kubectl get orchestrationprofiles
kubectl get orchestrationprofiles -o yaml
```

---

## Quick Start (automated)

A shell script automates the full local development cycle — code generation, CRD installation, image build, registry push, and deployment:

```bash
export GITHUB_PAT_TOKEN=<your-ghcr-token>
export GITHUB_USERNAME=<your-github-username>
export DECISION_AGENT_URL="http://your-decision-agent:8080"

./hack/deploy_with_make.sh
```

> **Note:** The script builds and pushes to `ghcr.io/hiro-microdatacenters-bv/hiro-adaptive-orchestrator`. Adjust the `DOCKER_REGISTRY` variable inside the script if you use a different registry.

---

## Configuration

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DECISION_AGENT_URL` | **Yes** | — | Base URL of the external AI/decision agent (e.g. `http://decision-agent:8080`) |
| `PLACEMENT_SERVER_PORT` | No | `:8090` | Listening address for the placement server |

### OrchestrationProfile CRD Reference

`OrchestrationProfile` is a cluster-scoped resource (short name: `op`).

```yaml
apiVersion: orchestration.hiro.io/v1alpha1
kind: OrchestrationProfile
metadata:
  name: my-app-profile
spec:
  # Reference to the workload this profile governs
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
    triggerConditions:        # EnergyThreshold | CPUThreshold | MemoryThreshold
      - "EnergyThreshold"     # | NodeFailure | Scheduled
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

---

## Deployment

### Option A — Kustomize (recommended for development)

```bash
# Build image and push to your registry
export IMG=<registry>/<image>:<tag>
make docker-build docker-push IMG=$IMG

# Deploy to the cluster
make deploy IMG=$IMG

# Verify
kubectl get pods -n hiro-adaptive-orchestrator-system
kubectl logs -n hiro-adaptive-orchestrator-system \
  deployment/hiro-adaptive-orchestrator-controller-manager -c manager -f
```

To tear down:

```bash
make undeploy
make uninstall   # removes CRDs
```

### Option B — Helm

```bash
# Generate the Helm chart (if not already present)
kubebuilder edit --plugins=helm/v2-alpha

# Deploy
export IMG=<registry>/<image>:<tag>
make helm-deploy IMG=$IMG

# Check status
make helm-status

# Upgrade with custom values
make helm-deploy IMG=$IMG HELM_EXTRA_ARGS="--set manager.replicas=2"

# Rollback
make helm-rollback

# Uninstall
make helm-uninstall
```

#### Helm values (key options from `dist/chart/values.yaml`)

| Key | Default | Description |
|-----|---------|-------------|
| `manager.image.repository` | `controller` | Image repository |
| `manager.image.tag` | `latest` | Image tag |
| `manager.replicas` | `1` | Controller replica count |
| `manager.resources.limits.cpu` | `500m` | CPU limit |
| `manager.resources.limits.memory` | `128Mi` | Memory limit |

### Option C — YAML bundle (single file install)

```bash
make build-installer IMG=<registry>/<image>:<tag>
kubectl apply -f dist/install.yaml
```

---

## Testing

### Unit & Integration Tests

Tests use **Ginkgo v2 + Gomega** with `controller-runtime/envtest` (real Kubernetes API server + etcd, no cluster needed):

```bash
make test
```

A coverage profile is written to `cover.out`.

### End-to-End Tests

E2E tests run against an isolated **Kind** cluster (created and torn down automatically):

```bash
make test-e2e
```

> Always run E2E tests against a dedicated Kind cluster — not your development or production cluster.

### Lint

```bash
make lint        # report issues
make lint-fix    # auto-fix where possible
```

---

## Development Workflow

```
Edit *_types.go or markers
        │
        ├── make manifests   (regenerate CRDs / RBAC)
        └── make generate    (regenerate DeepCopy)

Edit *.go files
        │
        ├── make lint-fix    (auto-fix style)
        └── make test        (unit tests)

Ready to deploy
        │
        ├── make docker-build docker-push IMG=...
        └── make deploy IMG=...  OR  make helm-deploy IMG=...
```

> **Never manually edit** auto-generated files: `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, `zz_generated.*.go`.
> Always use `kubebuilder create api` / `kubebuilder create webhook` to scaffold new resources.

---

## Project Structure

```
cmd/main.go                          # Entry point: manager + PlacementServer startup
api/v1alpha1/
  orchestrationprofile_types.go      # CRD schema (OrchestrationProfile)
  zz_generated.deepcopy.go           # Auto-generated — DO NOT EDIT
internal/
  controller/
    orchestrationprofile_controller.go  # Main reconciler
    op_index.go                         # O(1) field index registration
    op_watchers.go                      # Pod/Workload → Profile event mapping
    op_validation.go                    # Spec validation
    op_status.go                        # Status computation
    op_pod_discovery.go                 # Pod resolution (OwnerReference walk)
    op_constants.go                     # Status enum values
  decision/
    server.go                        # HTTP server for kube-scheduler plugin (:8090)
    builder.go                       # Assembles DecisionRequest from cluster state
    client.go                        # HTTP client to external AI agent (8s timeout)
    types.go                         # DecisionRequest, DecisionResponse, PlacementContext
  utils/
    helpers.go                       # ResolveAppFromPod, KeysOf, NodeNames
config/
  crd/bases/                         # Generated CRDs — DO NOT EDIT
  rbac/                              # Generated RBAC — DO NOT EDIT
  samples/                           # Example OrchestrationProfile + nginx Deployment
  default/                           # Kustomize base
dist/chart/                          # Generated Helm chart
test/e2e/                            # End-to-end tests (Kind)
hack/
  deploy_with_make.sh                # Automated local deployment script
```

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
