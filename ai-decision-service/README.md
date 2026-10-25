# AI Decision Service

The decision agent the operator calls, in place of the mock agent. One endpoint serves both flows:

- **Placement** (no `rebalanceContext`): scores every candidate node.
- **Rebalance** (`rebalanceContext` set): picks a rebalance action.

It only decides: it has no Kubernetes client and never calls the cluster. The operator sends all the data it needs.

It is a separate Go module (own `go.mod`), like `scheduler-plugin/`. It imports the request/response contract from the root module's `pkg/placement` (`replace => ../`).

## Current behaviour (skeleton)

| Flow | Answer | `decidedBy` |
|------|--------|-------------|
| Placement | Every node scores 50 | `scorer` |
| Rebalance | `NoOp` | `rules` |

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
