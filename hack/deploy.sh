#!/bin/bash
# hack/deploy.sh
#
# Full-stack deploy: HIRO Adaptive Orchestrator (operator) + HIRO Scheduler Plugin.
# Deploys operator first, waits for it to be healthy, then deploys the scheduler.
#
# This script is the single entry point for a complete cluster deployment.
# To deploy components individually:
#   Operator only  : hack/deploy_operator.sh
#   Scheduler only : hack/deploy_scheduler.sh
#   Extender only  : hack/deploy_extender.sh
#
# Required env:
#   GITHUB_PAT_TOKEN   GitHub PAT with write:packages scope
#
# Optional env:
#   APPLY_EXTENDER_CONFIG  Set to "true" to also deploy the legacy extender
#                          ConfigMap to kube-system (default: false).
#                          Use this on managed clusters where the control
#                          plane is locked and a custom scheduler pod cannot
#                          be deployed.
#
# All other variables are forwarded to the individual deploy scripts as-is.
# See hack/deploy_operator.sh and hack/deploy_scheduler.sh for
# the full list of overrides.
#
# Usage:
#   export GITHUB_PAT_TOKEN=<token>
#   hack/deploy.sh [kubeconfig-path]
#   APPLY_EXTENDER_CONFIG=true hack/deploy.sh [kubeconfig-path]

set -euo pipefail

# ---------------------------------------------------------------------------
# Paths and shared config
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
KUBECONFIG_PATH=${1:-~/.kube/config}

# Exported so every sub-script inherits the same values without re-parsing.
export GITHUB_PAT_TOKEN
export GITHUB_USERNAME=${GITHUB_USERNAME:-sskrishnav}
export NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}
export NAME_PREFIX=${NAME_PREFIX:-hiro-adaptive-orchestrator-}
export KUBECONFIG="$KUBECONFIG_PATH"

PLACEMENT_SERVER_PORT=${PLACEMENT_SERVER_PORT:-:8090}
APPLY_EXTENDER_CONFIG=${APPLY_EXTENDER_CONFIG:-false}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n\033[36m===>\033[0m %s\n' "$*"; }

print_config() {
  echo "================================================================"
  echo " HIRO Full-Stack Deploy"
  echo "================================================================"
  echo "Namespace             : $NAMESPACE"
  echo "Name Prefix           : $NAME_PREFIX"
  echo "Kubeconfig            : $KUBECONFIG"
  echo "Apply Extender Config : $APPLY_EXTENDER_CONFIG"
  echo "================================================================"
}

# ---------------------------------------------------------------------------
# Phases
# ---------------------------------------------------------------------------

deploy_operator() {
  step "Phase 1 — Deploying operator..."
  bash "$SCRIPT_DIR/deploy_operator.sh" "$KUBECONFIG_PATH"
}

wait_for_placement_server() {
  local placement_service="${NAME_PREFIX}controller-manager-placement-service"

  step "Phase 2 — Waiting for PlacementServer to be reachable..."
  echo "  Service : ${placement_service}.${NAMESPACE}.svc.cluster.local${PLACEMENT_SERVER_PORT}"
  echo "  Waiting for operator pod to be Ready..."

  kubectl wait --for=condition=Ready pod \
    -l app.kubernetes.io/name=hiro-adaptive-orchestrator \
    -n "$NAMESPACE" \
    --timeout=120s

  echo "  Operator is healthy."
}

deploy_scheduler() {
  step "Phase 3 — Deploying scheduler plugin..."
  bash "$SCRIPT_DIR/deploy_scheduler.sh" "$KUBECONFIG_PATH"
}

deploy_extender() {
  if [ "$APPLY_EXTENDER_CONFIG" = "true" ]; then
    step "Phase 4 — Deploying legacy extender ConfigMap..."
    bash "$SCRIPT_DIR/deploy_extender.sh" "$KUBECONFIG_PATH"
  else
    step "Phase 4 — Skipping extender (APPLY_EXTENDER_CONFIG=false)."
    echo "  To deploy the legacy extender, re-run with:"
    echo "    APPLY_EXTENDER_CONFIG=true hack/deploy.sh"
    echo "  Or run standalone: hack/deploy_extender.sh"
  fi
}

print_summary() {
  echo ""
  echo -e "\033[32m================================================================\033[0m"
  echo -e "\033[32m  Full-stack deployment complete.\033[0m"
  echo -e "\033[32m  Operator   : namespace/$NAMESPACE\033[0m"
  echo -e "\033[32m  Scheduler  : hiro-scheduler (pods labelled schedulerName=hiro-scheduler)\033[0m"
  if [ "$APPLY_EXTENDER_CONFIG" = "true" ]; then
    echo -e "\033[32m  Extender   : hiro-scheduler-config ConfigMap in kube-system\033[0m"
  fi
  echo -e "\033[32m================================================================\033[0m"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  print_config

  deploy_operator
  wait_for_placement_server
  #deploy_scheduler
  #deploy_extender

  print_summary
}

main "$@"
