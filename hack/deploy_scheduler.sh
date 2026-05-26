#!/bin/bash
# hack/deploy_scheduler.sh
#
# Builds, pushes, and deploys the HIRO scheduler plugin.
# Expects the operator to already be running (PlacementServer must be reachable).
# Called directly or via hack/deploy.sh (full-stack deploy).
#
# Required env:
#   GITHUB_PAT_TOKEN   GitHub PAT with write:packages scope
#
# Key overrides (all have defaults):
#   GITHUB_USERNAME       ghcr.io login                    (default: sskrishnav)
#   NAMESPACE             operator namespace               (default: hiro-adaptive-orchestrator-system)
#   NAME_PREFIX           kustomize prefix                 (default: hiro-adaptive-orchestrator-)
#   SCHED_K8S_VERSION     K8s minor version to build for  (default: v1.35.0)
#   SCHED_VERSION         scheduler release version        (default: v0.1.0)
#
# Usage (standalone):
#   export GITHUB_PAT_TOKEN=<token>
#   hack/deploy_scheduler.sh [kubeconfig-path]

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

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n==> %s\n' "$*"; }

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

print_config() {
  echo "========================================================"
  echo " HIRO Scheduler Plugin — Deploy"
  echo "========================================================"
  echo "Namespace          : $NAMESPACE"
  echo "Name Prefix        : $NAME_PREFIX"
  echo "Scheduler Image    : $SCHED_IMG"
  echo "K8s Target Version : $SCHED_K8S_VERSION"
  echo "Kubeconfig         : $KUBECONFIG"
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

  step "Deploying scheduler k8s resources..."
  kubectl apply -k "$REPO_ROOT/config/scheduler/"

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
