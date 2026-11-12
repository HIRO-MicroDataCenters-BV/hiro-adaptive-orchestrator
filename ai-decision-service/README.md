# AI Decision Service

The decision agent the operator calls, in place of the mock agent. One endpoint serves both flows:

- **Placement** (no `rebalanceContext`): scores every candidate node.
- **Rebalance** (`rebalanceContext` set): picks a rebalance action.

It only decides: it has no Kubernetes client and never calls the cluster. The operator sends all the data it needs.

It is a separate Go module (own `go.mod`), like `scheduler-plugin/`. It imports the request/response contract from the root module's `pkg/placement` (`replace => ../`).

## Current behaviour

| Flow | Answer | `decidedBy` |
|------|--------|-------------|
| Placement | The model scores the candidates when one is set; otherwise the scorer, with the profile's strategy (below) | `model`; `scorer` when the model is off; `fallback` when it fails |
| Rebalance | Moves a pod off a down node to the best healthy node; otherwise `NoOp`, listing what is blocked | `rules` (`fallback` if the answer fails validation) |

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

### Placement model (`model/`)

Off by default (`AI_DECISION_SERVICE_MODEL_PROVIDER` empty). With a provider set:

1. The scorer runs first, so its answer is ready if the model fails.
2. The model is asked, and given up on after `AI_DECISION_SERVICE_MODEL_TIMEOUT`.
3. **Check:** each candidate needs exactly one score from 0 to 100. A model error, a timeout or a failed check sends the scorer's answer with `decidedBy: fallback`.

The log also records the scorer's answer beside the model's, so the two can be compared.

**Kev** (`kev`, System One API): each candidate's facts (share of CPU and memory requested, the app's pods on it) become one option of a `choice` question, with the strategy's rule as the instruction. Each node's probability × 100 is its score, and the reason names Kev's pick, e.g. `Spread: node-2 best (probability 0.75)`.

**Adding a model:** a new file in `model/` that implements `PlacementDecider`, plus one `case` in `model.New`.

### Rebalance (`rebalance/`)

It decides from facts in the request (node conditions, history, PDB, EAO), not from the trigger.

1. **Gates** block what isn't safe now:
   - Cooldown blocks every change.
   - Externally managed replicas or resources block that action.
   - A PDB with no disruptions left, or a fresh EAO `Hold`, blocks Move.
   - A recent Move blocks taking a pod off the node it filled (unless that node is now at risk), and blocks moving one back to the node it emptied.
2. **Rule:** a pod on a down node (`Ready` not `True`) moves to the best healthy candidate. Leaving a down node skips the gates; the eviction API still enforces the PDB.
3. **Validate** checks the answer against the request and the gates. A failed check sends `NoOp` with `decidedBy: fallback`.

A node is at risk when it is cordoned or has a risk condition (below); it is never a Move target.

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
| `AI_DECISION_SERVICE_MODEL_PROVIDER` | `""` | Placement model: `""` = off (the scorer answers), `kev` = Kev's System One API |
| `AI_DECISION_SERVICE_MODEL_BASE_URL` | `""` | Model's address; the full URL is base URL + path. Required with a provider |
| `AI_DECISION_SERVICE_MODEL_PATH` | `""` | Model's API path, e.g. `/v1/systemone` for Kev. Required with a provider |
| `AI_DECISION_SERVICE_MODEL_NAME` | `""` | Model name sent with each request, e.g. `kev-latest`. Required with a provider |
| `AI_DECISION_SERVICE_MODEL_TIMEOUT` | `500ms` | Gives up on the model after this; the scorer answers instead |
| `AI_DECISION_SERVICE_REBALANCE_EAO_STALE_AFTER` | `5m` | EAO data older than this is ignored |
| `AI_DECISION_SERVICE_REBALANCE_FLIP_MOVE_ACTION_WINDOW` | `30m` | How long a Move blocks churn and ping-pong moves |
| `AI_DECISION_SERVICE_REBALANCE_RISK_CONDITIONS` | `MemoryPressure,DiskPressure,PIDPressure,NetworkUnavailable` | Node conditions that mark a node at risk when `True` |
| `AI_DECISION_SERVICE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; `debug` adds the full request body |

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
