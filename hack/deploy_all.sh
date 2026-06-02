#!/bin/bash
# hack/deploy_all.sh
#
# Full-stack deploy: HIRO Adaptive Orchestrator (operator) + optional
# scheduler integration (plugin or extender).
#
# This is the single top-level entry point — ALL parameters live here.
# Every sub-script also keeps its own :-defaults so it works standalone,
# but when called from here the exports below take precedence.
#
# To deploy components individually:
#   Operator only  : hack/deploy_operator.sh
#   Scheduler only : hack/deploy_scheduler.sh
#   Extender only  : hack/deploy_extender.sh
#
# ─── Required ────────────────────────────────────────────────────────────────
#   GITHUB_PAT_TOKEN          GitHub PAT with write:packages scope
#
# ─── Identity (shared) ───────────────────────────────────────────────────────
#   GITHUB_USERNAME           ghcr.io login               (default: sskrishnav)
#   NAMESPACE                 k8s namespace               (default: hiro-adaptive-orchestrator-system)
#   NAME_PREFIX               kustomize namePrefix        (default: hiro-adaptive-orchestrator-)
#
# ─── PlacementServer (operator exposes / scheduler calls) ────────────────────
#   PLACEMENT_SERVICE_NAME    k8s Service name            (default: <NAME_PREFIX>controller-manager-placement-service)
#   PLACEMENT_SERVER_PORT     listening port              (default: :8090)
#   PLACEMENT_SERVER_PATH     decision endpoint path      (default: /api/v1/placement/decision)
#   PLACEMENT_SERVER_HEALTH_PATH  health endpoint path    (default: /healthz)
#   PLACEMENT_TIMEOUT_SECS    scheduler→server timeout    (default: 8)
#
# ─── Operator — Decision Agent ───────────────────────────────────────────────
#   USE_MOCK_AGENT            true|false                  (default: true)
#   DECISION_AGENT_URL        required when USE_MOCK_AGENT=false
#   DECISION_AGENT_PATH       agent API path              (default: /api/v1/agent/placement/decision)
#
# ─── Operator — Extender paths ───────────────────────────────────────────────
#   EXTENDER_FILTER_PATH      extender filter path        (default: /extender/filter)
#   EXTENDER_PRIORITIZE_PATH  extender prioritize path    (default: /extender/prioritize)
#
# ─── Operator — EnergyAwareOrchestration CRD ─────────────────────────────────
#   EAO_GROUP                 CRD API group               (default: eas.hiro.io)
#   EAO_VERSION               CRD API version             (default: v1)
#   EAO_KIND                  CRD Kind                    (default: EnergyAwareOrchestration)
#
# ─── Scheduler ───────────────────────────────────────────────────────────────
#   SCHED_K8S_VERSION         k8s version to build for    (default: v1.35.0)
#   SCHED_VERSION             scheduler release version   (default: v0.1.0)
#
# ─── Deploy options ──────────────────────────────────────────────────────────
#   DEPLOY_SCHEDULER_PLUGIN   true|false                  (default: false)
#                             Deploys the HIRO custom scheduler pod.
#                             Pods opt in via spec.schedulerName: hiro-scheduler.
#                             Use on clusters that support custom scheduler pods.
#
#   DEPLOY_EXTENDER           true|false                  (default: false)
#                             Deploys extender ConfigMap and patches kube-scheduler.
#                             Affects ALL pods via the default scheduler.
#                             Use on clusters where a custom scheduler pod cannot run.
#
#   Note: DEPLOY_SCHEDULER_PLUGIN and DEPLOY_EXTENDER are mutually exclusive
#         in practice — choose one scheduler integration approach per cluster.
#
# ─── Usage ───────────────────────────────────────────────────────────────────
#   export GITHUB_PAT_TOKEN=<token>
#
#   # Operator only (default)
#   hack/deploy_all.sh [kubeconfig-path]
#
#   # Operator + custom scheduler plugin
#   DEPLOY_SCHEDULER_PLUGIN=true hack/deploy_all.sh [kubeconfig-path]
#
#   # Operator + extender (patches default kube-scheduler)
#   DEPLOY_EXTENDER=true hack/deploy_all.sh [kubeconfig-path]
#
#   # Real AI agent, custom namespace, scheduler plugin
#   USE_MOCK_AGENT=false DECISION_AGENT_URL=http://ai.example.com:8080 \
#     NAMESPACE=my-ns DEPLOY_SCHEDULER_PLUGIN=true \
#     hack/deploy_all.sh [kubeconfig-path]

set -euo pipefail

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
KUBECONFIG_PATH=${1:-~/.kube/config}

# ---------------------------------------------------------------------------
# Identity — shared by all sub-scripts
# ---------------------------------------------------------------------------

export GITHUB_PAT_TOKEN
export GITHUB_USERNAME=${GITHUB_USERNAME:-sskrishnav}
export NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}
export NAME_PREFIX=${NAME_PREFIX:-hiro-adaptive-orchestrator-}
export KUBECONFIG="$KUBECONFIG_PATH"

# ---------------------------------------------------------------------------
# PlacementServer — operator exposes it, scheduler and extender call it
# PLACEMENT_SERVICE_NAME must be derived AFTER NAME_PREFIX is resolved.
# ---------------------------------------------------------------------------

export PLACEMENT_SERVICE_NAME=${PLACEMENT_SERVICE_NAME:-${NAME_PREFIX}controller-manager-placement-service}
export PLACEMENT_SERVER_PORT=${PLACEMENT_SERVER_PORT:-:8090}
export PLACEMENT_SERVER_PATH=${PLACEMENT_SERVER_PATH:-/api/v1/placement/decision}
export PLACEMENT_SERVER_HEALTH_PATH=${PLACEMENT_SERVER_HEALTH_PATH:-/healthz}
export PLACEMENT_TIMEOUT_SECS=${PLACEMENT_TIMEOUT_SECS:-8}

# ---------------------------------------------------------------------------
# Operator — Decision Agent
# ---------------------------------------------------------------------------

export USE_MOCK_AGENT=${USE_MOCK_AGENT:-true}
# DECISION_AGENT_URL is set by deploy_operator.sh when USE_MOCK_AGENT=true.
# Set it here (and export) only when USE_MOCK_AGENT=false.
if [ "$USE_MOCK_AGENT" = "false" ]; then
  : "${DECISION_AGENT_URL:?DECISION_AGENT_URL must be set when USE_MOCK_AGENT=false}"
  export DECISION_AGENT_URL
fi
export DECISION_AGENT_PATH=${DECISION_AGENT_PATH:-/api/v1/agent/placement/decision}

# ---------------------------------------------------------------------------
# Operator — Extender paths
# ---------------------------------------------------------------------------

export EXTENDER_FILTER_PATH=${EXTENDER_FILTER_PATH:-/extender/filter}
export EXTENDER_PRIORITIZE_PATH=${EXTENDER_PRIORITIZE_PATH:-/extender/prioritize}

# ---------------------------------------------------------------------------
# Operator — EnergyAwareOrchestration CRD coordinates
# ---------------------------------------------------------------------------

export EAO_GROUP=${EAO_GROUP:-eas.hiro.io}
export EAO_VERSION=${EAO_VERSION:-v1}
export EAO_KIND=${EAO_KIND:-EnergyAwareOrchestration}

# ---------------------------------------------------------------------------
# Scheduler
# ---------------------------------------------------------------------------

export SCHED_K8S_VERSION=${SCHED_K8S_VERSION:-v1.35.0}
export SCHED_VERSION=${SCHED_VERSION:-v0.1.0}

# ---------------------------------------------------------------------------
# Deploy options — consumed by this script only, not forwarded as env vars
# ---------------------------------------------------------------------------

DEPLOY_SCHEDULER_PLUGIN=${DEPLOY_SCHEDULER_PLUGIN:-false}
DEPLOY_EXTENDER=${DEPLOY_EXTENDER:-false}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n\033[36m===>\033[0m %s\n' "$*"; }

# ---------------------------------------------------------------------------
# Phases
# ---------------------------------------------------------------------------

print_config() {
  echo "================================================================"
  echo " HIRO Full-Stack Deploy"
  echo "================================================================"
  echo "── Identity ──────────────────────────────────────────────────"
  echo "  Namespace              : $NAMESPACE"
  echo "  Name Prefix            : $NAME_PREFIX"
  echo "  Kubeconfig             : $KUBECONFIG"
  echo "── PlacementServer ───────────────────────────────────────────"
  echo "  Service Name           : $PLACEMENT_SERVICE_NAME"
  echo "  Port                   : $PLACEMENT_SERVER_PORT"
  echo "  Decision Path          : $PLACEMENT_SERVER_PATH"
  echo "  Health Path            : $PLACEMENT_SERVER_HEALTH_PATH"
  echo "  Scheduler Timeout (s)  : $PLACEMENT_TIMEOUT_SECS"
  echo "── Operator ──────────────────────────────────────────────────"
  if [ "$USE_MOCK_AGENT" = "true" ]; then
  echo "  Decision Agent         : MOCK"
  else
  echo "  Decision Agent         : REAL  ($DECISION_AGENT_URL)"
  fi
  echo "  Agent Path             : $DECISION_AGENT_PATH"
  echo "  Extender Filter        : $EXTENDER_FILTER_PATH"
  echo "  Extender Prioritize    : $EXTENDER_PRIORITIZE_PATH"
  echo "  EAO CRD                : $EAO_GROUP/$EAO_VERSION  Kind=$EAO_KIND"
  echo "── Scheduler ─────────────────────────────────────────────────"
  echo "  K8s Target Version     : $SCHED_K8S_VERSION"
  echo "  Scheduler Version      : $SCHED_VERSION"
  echo "── Deploy Options ────────────────────────────────────────────"
  echo "  Deploy Scheduler Plugin: $DEPLOY_SCHEDULER_PLUGIN"
  echo "  Deploy Extender        : $DEPLOY_EXTENDER"
  echo "================================================================"
}

deploy_operator() {
  step "Phase 1 — Deploying operator..."
  bash "$SCRIPT_DIR/deploy_operator.sh" "$KUBECONFIG_PATH"
}

wait_for_placement_server() {
  step "Phase 2 — Waiting for PlacementServer to be reachable..."
  echo "  Service : ${PLACEMENT_SERVICE_NAME}.${NAMESPACE}.svc.cluster.local${PLACEMENT_SERVER_PORT}"
  echo "  Waiting for operator pod to be Ready..."

  kubectl wait --for=condition=Ready pod \
    -l app.kubernetes.io/name=hiro-adaptive-orchestrator \
    -n "$NAMESPACE" \
    --timeout=120s

  echo "  Operator is healthy."
}

deploy_scheduler_plugin() {
  if [ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ]; then
    step "Phase 3 — Deploying HIRO scheduler plugin..."
    bash "$SCRIPT_DIR/deploy_scheduler.sh" "$KUBECONFIG_PATH"
  else
    step "Phase 3 — Skipping scheduler plugin  (DEPLOY_SCHEDULER_PLUGIN=false)."
    echo "  To deploy: DEPLOY_SCHEDULER_PLUGIN=true hack/deploy_all.sh"
    echo "  Standalone: hack/deploy_scheduler.sh"
  fi
}

deploy_extender() {
  if [ "$DEPLOY_EXTENDER" = "true" ]; then
    step "Phase 4 — Deploying extender (patches kube-scheduler)..."
    bash "$SCRIPT_DIR/deploy_extender.sh" "$KUBECONFIG_PATH"
  else
    step "Phase 4 — Skipping extender              (DEPLOY_EXTENDER=false)."
    echo "  To deploy: DEPLOY_EXTENDER=true hack/deploy_all.sh"
    echo "  Standalone: hack/deploy_extender.sh"
  fi
}

print_summary() {
  echo ""
  echo -e "\033[32m================================================================\033[0m"
  echo -e "\033[32m  Full-stack deployment complete.\033[0m"
  echo -e "\033[32m  Operator        : namespace/$NAMESPACE\033[0m"
  if [ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ]; then
    echo -e "\033[32m  Scheduler Plugin: deployed  (opt-in: schedulerName=hiro-scheduler)\033[0m"
  else
    echo -e "\033[33m  Scheduler Plugin: not deployed\033[0m"
  fi
  if [ "$DEPLOY_EXTENDER" = "true" ]; then
    echo -e "\033[32m  Extender        : deployed  (all pods via default scheduler)\033[0m"
  else
    echo -e "\033[33m  Extender        : not deployed\033[0m"
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
  deploy_scheduler_plugin
  deploy_extender

  print_summary
}

main "$@"
