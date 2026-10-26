# AI Decision Service

The decision agent the operator calls, in place of the mock agent. One endpoint serves both flows:

- **Placement** (no `rebalanceContext`): scores every candidate node.
- **Rebalance** (`rebalanceContext` set): picks a rebalance action.

It only decides: it has no Kubernetes client and never calls the cluster. The operator sends all the data it needs.

It is a separate Go module (own `go.mod`), like `scheduler-plugin/`. It imports the request/response contract from the root module's `pkg/placement` (`replace => ../`).

## Current behaviour

| Flow | Answer | `decidedBy` |
|------|--------|-------------|
| Placement | Scores each node 0–100 with the profile's strategy (below) | `scorer` |
| Rebalance | `NoOp` (rules not written yet) | `rules` |

### Placement scorer (`scorer/`)

Utilisation is worked out per resource **after** placing the pod: `(requested + pod request) / allocatable`.

| Strategy | Prefers | Score |
|----------|---------|-------|
| `Packed` | Fuller nodes, so others stay empty | `100 × average utilisation`; 0 when any resource is above the packed limit |
| `Balanced` (default) | Emptier nodes, no hotspots | `100 × (1 − highest utilisation)`, minus a penalty when CPU and memory are uneven |
| `Spread` | Fewer of the app's pods per zone, then per node | Weighted mix of the zone part, the node part and the Balanced score |

- A pod that doesn't fit (utilisation above 100%) scores 0.
- A node with unknown usage gets the neutral unknown score, and is never dropped.
- The awareness flags pick the resources (CPU, memory). None set means both.
- The reason names the best node, e.g. `Balanced: node-2 best (score 74)`.

Each answer writes one JSON log line: `requestId`, `flow`, `decidedBy`, `latencyMilliSec`, `inputHash` (same input, same hash), `nodes` and the response.

## Endpoints

| Method | Path | |
|--------|------|-|
| `POST` | `/api/v1/agent/placement/decision` | Placement or rebalance decision |
| `GET` | `/healthz` | Liveness/readiness, returns `OK` |

Bad JSON, an empty body or a body over the size limit returns `400`.

## Configuration

Every setting has a default (`server/config.go`) and an env var:

| Env var | Default | |
|---------|---------|-|
| `AI_DECISION_SERVICE_PORT` | `8080` | Listen port |
| `AI_DECISION_SERVICE_READ_TIMEOUT` | `5s` | Max time to read a request |
| `AI_DECISION_SERVICE_MAX_BODY_BYTES` | `5242880` (5 MiB) | Max request body |
| `AI_DECISION_SERVICE_HEALTH_PATH` | `/healthz` | Health path |
| `AI_DECISION_SERVICE_DECISION_PATH` | `/api/v1/agent/placement/decision` | Decision path |
| `AI_DECISION_SERVICE_SCORER_PACKED_MAX_UTIL` | `0.85` | Packed: a node above this utilisation scores 0 |
| `AI_DECISION_SERVICE_SCORER_IMBALANCE_PENALTY` | `0.2` | Balanced: how much a CPU/memory gap lowers the score |
| `AI_DECISION_SERVICE_SCORER_SPREAD_ZONE_WEIGHT` | `0.6` | Spread: weight of "fewer app pods in the zone" |
| `AI_DECISION_SERVICE_SCORER_SPREAD_NODE_WEIGHT` | `0.3` | Spread: weight of "fewer app pods on the node" |
| `AI_DECISION_SERVICE_SCORER_SPREAD_BALANCED_WEIGHT` | `0.1` | Spread: weight of the Balanced tie-break |
| `AI_DECISION_SERVICE_SCORER_ZONE_LABEL` | `topology.kubernetes.io/zone` | Node label that names its zone |
| `AI_DECISION_SERVICE_SCORER_UNKNOWN_SCORE` | `50` | Score for a node whose usage is unknown |

An invalid value stops the service at startup with a clear error.

In the umbrella chart, set them under `aiDecisionService.config`. The port comes from `aiDecisionService.port`.

## Build, test, run

```bash
make test-ai-decision-service           # go test in this module (root `go test ./...` skips it)
make build-ai-decision-service          # bin/ai-decision-service
make docker-build-ai-decision-service   # image, built from the repo root

(cd ai-decision-service && go run ./cmd)   # local, on :8080
curl localhost:8080/healthz
```

## Deploy

- **Kind:** `deploy/scripts/deploy_full_stack_with_helm.sh`. The AI Decision Service is on by default (`USE_AI_DECISION_SERVICE=true`).
- **Helm:** `--set aiDecisionService.enable=true`. The chart points the operator's `DECISION_AGENT_URL` at it. Enable only one of `aiDecisionService` and `mockAgent`.
- **Plain manifest:** `deploy/manifests/ai_decision_service.yaml`.
- **Release:** `release.yml` pushes `ghcr.io/<repo>/ai-decision-service:<version>`. `deploy.yml` turns it on with the `AI_DECISION_SERVICE_ENABLE` repo variable.
