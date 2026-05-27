#!/bin/bash
# hack/deploy_scheduler.sh
#
# Builds, pushes, and deploys the HIRO scheduler plugin.
# Expects the operator to already be running (PlacementServer must be reachable).
# For a full-stack deploy (operator + scheduler) use hack/deploy_all.sh.
#
# ─── Required ────────────────────────────────────────────────────────────────
#   GITHUB_PAT_TOKEN          GitHub PAT with write:packages scope
#
# ─── Identity ────────────────────────────────────────────────────────────────
#   GITHUB_USERNAME           ghcr.io login               (default: sskrishnav)
#   NAMESPACE                 k8s namespace               (default: hiro-adaptive-orchestrator-system)
#   NAME_PREFIX               kustomize namePrefix        (default: hiro-adaptive-orchestrator-)
#
# ─── Scheduler image ─────────────────────────────────────────────────────────
#   SCHED_K8S_VERSION         k8s version to build for    (default: v1.35.0)
#   SCHED_VERSION             scheduler release version   (default: v0.1.0)
#
# ─── PlacementServer (operator endpoint this scheduler calls) ────────────────
#   PLACEMENT_SERVICE_NAME    k8s Service name            (default: <NAME_PREFIX>controller-manager-placement-service)
#   PLACEMENT_SERVER_PORT     PlacementServer port        (default: :8090)
#   PLACEMENT_SERVER_PATH     decision endpoint path      (default: /api/v1/placement/decision)
#   PLACEMENT_TIMEOUT_SECS    plugin→server timeout (s)   (default: 8)
#
# These three values are injected into the HIROScore pluginConfig inside the
# KubeSchedulerConfiguration ConfigMap at deploy time (via kustomize + sed).
# The scheduler pod reads them from the mounted ConfigMap — no env vars needed.
#
# Usage (standalone):
#   export GITHUB_PAT_TOKEN=<token>
#   hack/deploy_scheduler.sh [kubeconfig-path]
#
# Usage (via hack/deploy_all.sh — all params inherited from parent):
#   hack/deploy_all.sh [kubeconfig-path]

set -euo pipefail

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
KUSTOMIZE="$REPO_ROOT/bin/kustomize"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

GITHUB_USERNAME=${GITHUB_USERNAME:-sskrishnav}
: "${GITHUB_PAT_TOKEN:?GITHUB_PAT_TOKEN must be set (export GITHUB_PAT_TOKEN=<your-ghcr-token>)}"

export KUBECONFIG=${1:-~/.kube/config}
export CR_PAT="$GITHUB_PAT_TOKEN"

export NAME_PREFIX=${NAME_PREFIX:-hiro-adaptive-orchestrator-}
export NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}

SCHED_K8S_VERSION=${SCHED_K8S_VERSION:-v1.35.0}
SCHED_VERSION=${SCHED_VERSION:-v0.1.0}

DOCKER_REGISTRY=ghcr.io/hiro-microdatacenters-bv/hiro-adaptive-orchestrator
SCHED_IMG="${DOCKER_REGISTRY}/hiro-scheduler:${SCHED_VERSION}-k8s${SCHED_K8S_VERSION}"

# PlacementServer config — injected into the HIROScore pluginConfig at deploy time.
PLACEMENT_SERVER_PORT=${PLACEMENT_SERVER_PORT:-:8090}
PLACEMENT_SERVER_PATH=${PLACEMENT_SERVER_PATH:-/api/v1/placement/decision}
PLACEMENT_TIMEOUT_SECS=${PLACEMENT_TIMEOUT_SECS:-8}
# Derived after NAME_PREFIX is set — override only if the operator service was renamed.
PLACEMENT_SERVICE_NAME=${PLACEMENT_SERVICE_NAME:-${NAME_PREFIX}controller-manager-placement-service}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n==> %s\n' "$*"; }

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

print_config() {
  local placement_svc="$PLACEMENT_SERVICE_NAME"
  echo "========================================================"
  echo " HIRO Scheduler Plugin — Deploy"
  echo "========================================================"
  echo "Namespace              : $NAMESPACE"
  echo "Name Prefix            : $NAME_PREFIX"
  echo "Scheduler Image        : $SCHED_IMG"
  echo "K8s Target Version     : $SCHED_K8S_VERSION"
  echo "Kubeconfig             : $KUBECONFIG"
  echo "PlacementServer Svc    : ${placement_svc}.${NAMESPACE}.svc.cluster.local${PLACEMENT_SERVER_PORT}"
  echo "PlacementServer Path   : $PLACEMENT_SERVER_PATH"
  echo "Placement Timeout (s)  : $PLACEMENT_TIMEOUT_SECS"
  echo "========================================================"
}

build_and_push_scheduler_image() {
  step "Authenticating with GitHub Container Registry..."
  echo "$CR_PAT" | docker login ghcr.io -u "$GITHUB_USERNAME" --password-stdin

  step "Building scheduler image: $SCHED_IMG"
  docker build \
    --build-arg K8S_VERSION="$SCHED_K8S_VERSION" \
    -t "$SCHED_IMG" \
    -f "$REPO_ROOT/scheduler-plugin/Dockerfile" \
    "$REPO_ROOT"

  step "Pushing scheduler image: $SCHED_IMG"
  docker push "$SCHED_IMG"
}

deploy_scheduler_resources() {
  step "Configuring Kustomize for scheduler..."
  echo "  Namespace  : $NAMESPACE"
  echo "  NamePrefix : $NAME_PREFIX"
  echo "  Image      : $SCHED_IMG"

  # Pin namespace, namePrefix, and image in config/scheduler/kustomization.yaml.
  # These edits are idempotent — safe to re-run.
  (cd "$REPO_ROOT/config/scheduler" && "$KUSTOMIZE" edit set namespace "$NAMESPACE")
  (cd "$REPO_ROOT/config/scheduler" && "$KUSTOMIZE" edit set nameprefix "$NAME_PREFIX")
  (cd "$REPO_ROOT/config/scheduler" && "$KUSTOMIZE" edit set image hiro-scheduler="$SCHED_IMG")

  # Build the placement server URL from user-supplied (or default) values.
  # kustomize cannot substitute values inside ConfigMap data, so we patch the
  # rendered YAML before applying.
  local placement_svc="$PLACEMENT_SERVICE_NAME"
  local placement_url="http://${placement_svc}.${NAMESPACE}.svc.cluster.local${PLACEMENT_SERVER_PORT}"

  step "Deploying scheduler k8s resources..."
  echo "  PlacementServer URL  : $placement_url"
  echo "  PlacementServer Path : $PLACEMENT_SERVER_PATH"
  echo "  Timeout              : ${PLACEMENT_TIMEOUT_SECS}s"

  "$KUSTOMIZE" build "$REPO_ROOT/config/scheduler/" \
    | sed \
        -e "s|placementServerURL:.*|placementServerURL: \"${placement_url}\"|" \
        -e "s|placementServerPath:.*|placementServerPath: \"${PLACEMENT_SERVER_PATH}\"|" \
        -e "s|timeoutSeconds:.*|timeoutSeconds: ${PLACEMENT_TIMEOUT_SECS}|" \
    | kubectl apply -f -

  step "Waiting for scheduler deployment to be ready..."
  kubectl rollout status deployment/"${NAME_PREFIX}hiro-scheduler" \
    -n "$NAMESPACE" \
    --timeout=120s
  echo "Scheduler deployment is ready."
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  print_config

  build_and_push_scheduler_image
  deploy_scheduler_resources

  echo ""
  echo -e "\033[32m========================================================\033[0m"
  echo -e "\033[32m  Scheduler plugin deployed successfully.\033[0m"
  echo -e "\033[32m========================================================\033[0m"
}

main "$@"
