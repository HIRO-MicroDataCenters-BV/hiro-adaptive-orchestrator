#!/bin/bash
# hack/deploy_scheduler.sh
#
# Builds, pushes, and deploys the HIRO scheduler plugin.
# Expects the operator to already be running (PlacementServer must be reachable).
# For a full-stack deploy (operator + scheduler) use hack/deploy_full_stack.sh.
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
#   PLACEMENT_SCORE_PATH      decision endpoint path      (default: /api/v1/placement/score)
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
# Usage (via hack/deploy_full_stack.sh — all params inherited from parent):
#   hack/deploy_full_stack.sh [kubeconfig-path]

set -euo pipefail

# ---------------------------------------------------------------------------
# Paths / config
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
KUSTOMIZE="$REPO_ROOT/bin/kustomize"

GITHUB_USERNAME=${GITHUB_USERNAME:-sskrishnav}
: "${GITHUB_PAT_TOKEN:?GITHUB_PAT_TOKEN must be set}"

export KUBECONFIG=${1:-~/.kube/config}
export CR_PAT="$GITHUB_PAT_TOKEN"
export NAME_PREFIX=${NAME_PREFIX:-hiro-adaptive-orchestrator-}
export NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}

SCHED_K8S_VERSION=${SCHED_K8S_VERSION:-v1.35.0}
SCHED_VERSION=${SCHED_VERSION:-v0.1.0}
GIT_SHA=$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo "unknown")
SCHED_IMG="ghcr.io/hiro-microdatacenters-bv/hiro-adaptive-orchestrator/hiro-scheduler:${SCHED_VERSION}-k8s${SCHED_K8S_VERSION}-${GIT_SHA}"

PLACEMENT_SERVER_PORT=${PLACEMENT_SERVER_PORT:-:8090}
PLACEMENT_SCORE_PATH=${PLACEMENT_SCORE_PATH:-/api/v1/placement/score}
PLACEMENT_TIMEOUT_SECS=${PLACEMENT_TIMEOUT_SECS:-8}
PLACEMENT_SERVICE_NAME=${PLACEMENT_SERVICE_NAME:-${NAME_PREFIX}controller-manager-placement-service}

SCHED_DEPLOYMENT="${NAME_PREFIX}hiro-scheduler"
SCHED_SA="${NAME_PREFIX}hiro-scheduler"

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
  echo "Namespace              : $NAMESPACE"
  echo "Name Prefix            : $NAME_PREFIX"
  echo "Scheduler Image        : $SCHED_IMG"
  echo "K8s Target Version     : $SCHED_K8S_VERSION"
  echo "Kubeconfig             : $KUBECONFIG"
  echo "PlacementServer Svc    : ${PLACEMENT_SERVICE_NAME}.${NAMESPACE}.svc.cluster.local${PLACEMENT_SERVER_PORT}"
  echo "PlacementServer Path   : $PLACEMENT_SCORE_PATH"
  echo "Placement Timeout (s)  : $PLACEMENT_TIMEOUT_SECS"
  echo "========================================================"
}

build_scheduler_image() {
  step "Building scheduler image: $SCHED_IMG"
  docker build \
    --build-arg K8S_VERSION="$SCHED_K8S_VERSION" \
    -t "$SCHED_IMG" \
    -f "$REPO_ROOT/scheduler-plugin/Dockerfile" \
    "$REPO_ROOT"
}

push_scheduler_image() {
  step "Authenticating and pushing scheduler image..."
  echo "$CR_PAT" | docker login ghcr.io -u "$GITHUB_USERNAME" --password-stdin
  docker push "$SCHED_IMG"
}

configure_kustomize() {
  step "Configuring Kustomize for scheduler..."
  (cd "$REPO_ROOT/config/scheduler" && "$KUSTOMIZE" edit set namespace "$NAMESPACE")
  (cd "$REPO_ROOT/config/scheduler" && "$KUSTOMIZE" edit set nameprefix "$NAME_PREFIX")
  (cd "$REPO_ROOT/config/scheduler" && "$KUSTOMIZE" edit set image hiro-scheduler="$SCHED_IMG")
}

apply_scheduler_resources() {
  local placement_url="http://${PLACEMENT_SERVICE_NAME}.${NAMESPACE}.svc.cluster.local${PLACEMENT_SERVER_PORT}"

  step "Applying scheduler k8s resources..."
  echo "  PlacementServer URL  : $placement_url"
  echo "  PlacementServer Path : $PLACEMENT_SCORE_PATH"
  echo "  Timeout              : ${PLACEMENT_TIMEOUT_SECS}s"

  "$KUSTOMIZE" build "$REPO_ROOT/config/scheduler/" \
    | sed \
        -e "s|placementServerURL:.*|placementServerURL: \"${placement_url}\"|" \
        -e "s|placementServerPath:.*|placementServerPath: \"${PLACEMENT_SCORE_PATH}\"|" \
        -e "s|timeoutSeconds:.*|timeoutSeconds: ${PLACEMENT_TIMEOUT_SECS}|" \
    | kubectl apply -f -
}

attach_image_pull_secret() {
  step "Attaching GHCR pull secret to scheduler service account..."
  kubectl create secret docker-registry ghcr-secret \
    --docker-server=ghcr.io \
    --docker-username="$GITHUB_USERNAME" \
    --docker-password="$GITHUB_PAT_TOKEN" \
    --namespace="$NAMESPACE" \
    --dry-run=client -o yaml | kubectl apply -f -

  kubectl patch serviceaccount "$SCHED_SA" \
    -n "$NAMESPACE" \
    -p '{"imagePullSecrets": [{"name": "ghcr-secret"}]}'
}

wait_for_scheduler() {
  # Restart so pods pick up the updated SA (imagePullSecrets are resolved at
  # pod creation time — ImagePullBackOff pods won't recover on their own).
  step "Restarting and waiting for scheduler rollout..."
  kubectl rollout restart deployment/"$SCHED_DEPLOYMENT" -n "$NAMESPACE"
  kubectl rollout status deployment/"$SCHED_DEPLOYMENT" -n "$NAMESPACE" --timeout=120s
  echo "Scheduler is ready."
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  print_config

  build_scheduler_image
  push_scheduler_image
  configure_kustomize
  apply_scheduler_resources
  attach_image_pull_secret
  wait_for_scheduler

  echo ""
  echo -e "\033[32m========================================================\033[0m"
  echo -e "\033[32m  Scheduler plugin deployed successfully.\033[0m"
  echo -e "\033[32m========================================================\033[0m"
}

main "$@"
