#!/bin/bash
# hack/deploy_extender.sh
#
# Applies the legacy KubeSchedulerConfiguration ConfigMap for the HIRO
# scheduler extender approach.
#
# The extender approach routes the DEFAULT kube-scheduler's filter and
# prioritize calls to the operator's PlacementServer.  Use this instead of
# (or alongside) the scheduler plugin when you cannot deploy a custom
# scheduler pod — for example on managed Kubernetes clusters where the
# control plane is locked down (GKE Autopilot, EKS Fargate, etc.).
#
# After applying the ConfigMap you must mount it into the kube-scheduler pod
# and pass --config=/etc/kubernetes/scheduler-config.yaml.
# See config/extender/scheduler-config.yaml for full instructions.
#
# Prerequisites:
#   - Operator must be running (PlacementServer must be reachable)
#   - kubectl access to kube-system namespace
#
# Key overrides (all have defaults):
#   NAMESPACE    operator namespace  (default: hiro-adaptive-orchestrator-system)
#   NAME_PREFIX  kustomize prefix    (default: hiro-adaptive-orchestrator-)
#
# Usage (standalone):
#   hack/deploy_extender.sh [kubeconfig-path]
#
# Usage (via hack/deploy.sh):
#   APPLY_EXTENDER_CONFIG=true hack/deploy.sh [kubeconfig-path]

set -euo pipefail

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

export KUBECONFIG=${1:-~/.kube/config}

export NAME_PREFIX=${NAME_PREFIX:-hiro-adaptive-orchestrator-}
export NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}
# Derived after NAME_PREFIX is set — override only if the operator service was renamed.
export PLACEMENT_SERVICE_NAME=${PLACEMENT_SERVICE_NAME:-${NAME_PREFIX}controller-manager-placement-service}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n==> %s\n' "$*"; }

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

print_config() {
  echo "========================================================"
  echo " HIRO Scheduler Extender — Deploy"
  echo "========================================================"
  echo "Namespace            : $NAMESPACE"
  echo "NamePrefix           : $NAME_PREFIX"
  echo "Placement Svc Name   : $PLACEMENT_SERVICE_NAME"
  echo "Kubeconfig           : $KUBECONFIG"
}

main() {
  print_config

  local tmp_config
  tmp_config=$(mktemp /tmp/hiro-extender-config-XXXXXX.yaml)

  step "Rendering extender ConfigMap (service=${PLACEMENT_SERVICE_NAME}.${NAMESPACE})..."
  sed \
    -e "s|hiro-adaptive-orchestrator-controller-manager-placement-service|${PLACEMENT_SERVICE_NAME}|g" \
    -e "s|hiro-adaptive-orchestrator-system|${NAMESPACE}|g" \
    "$REPO_ROOT/config/extender/scheduler-config.yaml" > "$tmp_config"

  step "Applying extender ConfigMap to kube-system..."
  kubectl create configmap hiro-scheduler-config \
    --from-file=scheduler-config.yaml="$tmp_config" \
    -n kube-system \
    --dry-run=client -o yaml | kubectl apply -f -

  rm -f "$tmp_config"

  echo ""
  echo -e "\033[32m========================================================\033[0m"
  echo -e "\033[32m  Extender ConfigMap applied to kube-system.\033[0m"
  echo -e "\033[32m========================================================\033[0m"
  echo ""
  echo "  Next steps:"
  echo "  1. Mount the ConfigMap into the kube-scheduler pod."
  echo "  2. Pass --config=/etc/kubernetes/scheduler-config.yaml."
  echo "  See config/extender/scheduler-config.yaml for full instructions."
}

main "$@"
