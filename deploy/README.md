# deploy/

Everything needed to put HIRO on a cluster. Full detail lives in [docs/deployment.md](../docs/deployment.md).

```
deploy/
  scripts/     Shell entry points (run from anywhere; they resolve the repo root themselves)
  manifests/   Plain manifests the scripts apply (mock decision agent)
  helm/        Umbrella chart hiro-adaptive-platform (operator + webhook + scheduler + extras)
```

Kubebuilder-owned inputs stay where kubebuilder expects them and are **not** in this folder:
`config/` (Kustomize bases/overlays) and `dist/` (generated operator chart + `install.yaml`).

## Which path?

| Goal | Command |
|------|---------|
| Local Kind, whole stack, one command (recommended) | `deploy/scripts/deploy_full_stack_with_helm.sh [deploy\|clean\|destroy]` |
| Any cluster, whole stack via Helm | `make helm-platform-deploy HELM_EXTRA_ARGS="--set ..."` (chart: `deploy/helm/hiro-adaptive-platform`) |
| Released version from GHCR | `.github/workflows/deploy.yml`, or `helm install … oci://ghcr.io/hiro-microdatacenters-bv/hiro-adaptive-orchestrator/charts/hiro-adaptive-platform` |
| Scripted Kustomize full stack (pushes images to GHCR) | `deploy/scripts/deploy_full_stack.sh [kubeconfig]` |
| Single component | `deploy/scripts/deploy_operator.sh`, `deploy_scheduler.sh`, `deploy_extender.sh` / `undeploy_extender.sh` |
| Operator only, Kustomize | `make deploy IMG=…` (or `make deploy-scheduler` for the webhook overlay) |
| Operator only, Helm | `make helm-deploy IMG=…` (chart: `dist/chart`) |
| Single YAML bundle | `make build-installer IMG=…` → `kubectl apply -f dist/install.yaml` |
| Prerequisites only | `deploy/scripts/install_metrics_server.sh`, `install_prometheus_operator.sh` |
| Mock AI agent only | `kubectl apply -f deploy/manifests/mock_decision_agent.yaml` |

## Keep in sync by hand

Helm's `.Files.Get` can't read outside the chart, so two files are copied into it:

- `scripts/patch_scheduler_static_pod.py` ↔ `helm/hiro-adaptive-platform/files/patch_scheduler_static_pod.py`
- the `server.py` embedded in `manifests/mock_decision_agent.yaml` ↔ `helm/hiro-adaptive-platform/files/mock_decision_agent_server.py`
