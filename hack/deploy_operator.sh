#!/bin/bash
# hack/deploy_operator.sh
#
# Builds, pushes, and deploys the HIRO Adaptive Orchestrator (operator only).
# For a full-stack deploy (operator + scheduler) use hack/deploy_full_stack.sh.
#
# ─── What one operator Deployment bundles ────────────────────────────────────
#   This script deploys a SINGLE binary/Deployment/pod. There is nothing to
#   separately enable or deploy for the pieces below — they all start
#   together the moment the operator pod is Ready:
#
#     1. OrchestrationProfile controller
#          Reconciles OrchestrationProfile CRs, resolves their pods, and
#          computes PlacementStatus (Active/Pending/Degraded/...).
#     2. Placement Server (HTTP, :8090 by default)
#          AI-backed node scoring + energy gate filtering, called by the
#          scheduler plugin or the kube-scheduler extender per pod.
#     3. Rebalance Engine
#          Hybrid (periodic + event-driven) trigger detection feeding the
#          decision lifecycle state machine — see status.rebalancingStatus
#          on each OrchestrationProfile.
#     4. Pod scheduler admission webhook (opt-in via DEPLOY_SCHEDULER_PLUGIN)
#          Auto-sets spec.schedulerName on pods governed by a profile.
#
# ─── Required ────────────────────────────────────────────────────────────────
#   GITHUB_PAT_TOKEN          GitHub PAT with write:packages scope
#
# ─── Identity ────────────────────────────────────────────────────────────────
#   GITHUB_USERNAME           ghcr.io login               (default: sskrishnav)
#   NAMESPACE                 k8s namespace               (default: hiro-adaptive-orchestrator-system)
#   NAME_PREFIX               kustomize namePrefix        (default: hiro-adaptive-orchestrator-)
#
# ─── PlacementServer (served by this operator) ───────────────────────────────
#   PLACEMENT_SERVICE_NAME    k8s Service name            (default: <NAME_PREFIX>controller-manager-placement-service)
#   PLACEMENT_SERVER_PORT     listening port              (default: :8090)
#   PLACEMENT_SCORE_PATH     decision endpoint path      (default: /api/v1/placement/score)
#   PLACEMENT_SERVER_HEALTH_PATH  health endpoint path    (default: /healthz)
#
# ─── Decision Agent ──────────────────────────────────────────────────────────
#   USE_MOCK_AGENT            true|false                  (default: true)
#   DECISION_AGENT_URL        required when USE_MOCK_AGENT=false
#   DECISION_AGENT_PATH       agent API path              (default: /api/v1/agent/placement/decision)
#
# ─── Extender paths ──────────────────────────────────────────────────────────
#   EXTENDER_FILTER_PATH      extender filter path        (default: /extender/filter)
#   EXTENDER_PRIORITIZE_PATH  extender prioritize path    (default: /extender/prioritize)
#
# ─── Scheduler webhook (optional) ────────────────────────────────────────────
#   DEPLOY_SCHEDULER_PLUGIN   true → use config/default-with-webhook overlay
#                             false → use config/default (no webhook, no cert-manager)
#                             (default: false)
#   HIRO_SCHEDULER_NAME       scheduler name injected by the webhook (default: hiro-scheduler)
#
# ─── EnergyAwareOrchestration CRD ────────────────────────────────────────────
#   EAO_GROUP                 CRD API group               (default: eas.hiro.io)
#   EAO_VERSION               CRD API version             (default: v1)
#   EAO_KIND                  CRD Kind                    (default: EnergyAwareOrchestration)
#
# ─── Rebalance Engine ────────────────────────────────────────────────────────
#   REBALANCE_MAX_RECENT_DECISIONS      decision-history length per profile (default: 10)
#   REBALANCE_DETECTION_INTERVAL        periodic detection tick, Go duration (default: 30s)
#   REBALANCE_DECISION_TIMEOUT          AI-consultation timeout, Go duration (default: 5s)
#   REBALANCE_NODE_PRESSURE_THRESHOLD   CPU/Memory pressure fraction         (default: 0.90)
#   REBALANCE_IMPROVEMENT_THRESHOLD     min Move improvement score to enact  (default: 20)
#   REBALANCE_DECISION_STORE_TTL        how long a Move decision biases scoring, Go duration (default: 60s)
#   REBALANCE_MOVE_ACTION_TIMEOUT       max wait for a Move's replacement pod, Go duration (default: 60s)
#   REBALANCE_MOVE_RATE_LIMIT           cluster-wide Moves/minute across every profile (default: 5)
#
# Usage (standalone):
#   export GITHUB_PAT_TOKEN=<token>
#   hack/deploy_operator.sh [kubeconfig-path]
#   # If USE_MOCK_AGENT=true, also run after:
#   #   kubectl apply -f hack/mock_decision_agent.yaml
#
# Usage (via hack/deploy_full_stack.sh — all params inherited from parent):
#   hack/deploy_full_stack.sh [kubeconfig-path]
#   # deploy_full_stack.sh handles mock agent deployment automatically.

set -euo pipefail

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
KUSTOMIZE="$REPO_ROOT/bin/kustomize"

# ---------------------------------------------------------------------------
# Configuration (override via environment variables)
# ---------------------------------------------------------------------------

GITHUB_USERNAME=${GITHUB_USERNAME:-sskrishnav}
: "${GITHUB_PAT_TOKEN:?GITHUB_PAT_TOKEN must be set (export GITHUB_PAT_TOKEN=<your-ghcr-token>)}"

DOCKER_REGISTRY=ghcr.io/hiro-microdatacenters-bv/hiro-adaptive-orchestrator
OPERATOR_IMAGE=hiro-adaptive-orchestrator-controller:latest

export KUBECONFIG=${1:-~/.kube/config}
export IMG="${DOCKER_REGISTRY}/${OPERATOR_IMAGE}"
export CR_PAT="$GITHUB_PAT_TOKEN"

# Kustomize deployment identity
export NAME_PREFIX=${NAME_PREFIX:-hiro-adaptive-orchestrator-}
export NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}

# Scheduler webhook — set DEPLOY_SCHEDULER_PLUGIN=true to deploy with cert-manager TLS.
# PREREQUISITE when true: cert-manager must already be installed before this script runs.
DEPLOY_SCHEDULER_PLUGIN=${DEPLOY_SCHEDULER_PLUGIN:-false}
export HIRO_SCHEDULER_NAME=${HIRO_SCHEDULER_NAME:-hiro-scheduler}

# Derived names (kubebuilder convention: <NAME_PREFIX>controller-manager)
SA_NAME="${NAME_PREFIX}controller-manager"
DEPLOYMENT_NAME="${NAME_PREFIX}controller-manager"
# PlacementServer k8s Service name — override if you rename the service.
# Must match the name kustomize generates (namePrefix + "controller-manager-placement-service").
export PLACEMENT_SERVICE_NAME=${PLACEMENT_SERVICE_NAME:-${NAME_PREFIX}controller-manager-placement-service}

# Decision agent
USE_MOCK_AGENT=${USE_MOCK_AGENT:-true}
if [ "$USE_MOCK_AGENT" = "true" ]; then
  export DECISION_AGENT_URL="http://mock-decision-agent:8080"
else
  : "${DECISION_AGENT_URL:?DECISION_AGENT_URL must be set when USE_MOCK_AGENT=false}"
fi

# EnergyAwareOrchestration CRD coordinates
export EAO_GROUP="${EAO_GROUP:-eas.hiro.io}"
export EAO_VERSION="${EAO_VERSION:-v1}"
export EAO_KIND="${EAO_KIND:-EnergyAwareOrchestration}"

# Rebalance Engine — all optional, mirroring the operator's own package
# defaults (internal/rebalance). See internal/rebalance/README.md#configuration.
export REBALANCE_MAX_RECENT_DECISIONS="${REBALANCE_MAX_RECENT_DECISIONS:-10}"
export REBALANCE_DETECTION_INTERVAL="${REBALANCE_DETECTION_INTERVAL:-30s}"
export REBALANCE_DECISION_TIMEOUT="${REBALANCE_DECISION_TIMEOUT:-5s}"
export REBALANCE_NODE_PRESSURE_THRESHOLD="${REBALANCE_NODE_PRESSURE_THRESHOLD:-0.90}"
export REBALANCE_IMPROVEMENT_THRESHOLD="${REBALANCE_IMPROVEMENT_THRESHOLD:-20}"
export REBALANCE_DECISION_STORE_TTL="${REBALANCE_DECISION_STORE_TTL:-60s}"
export REBALANCE_MOVE_ACTION_TIMEOUT="${REBALANCE_MOVE_ACTION_TIMEOUT:-60s}"
export REBALANCE_MOVE_RATE_LIMIT="${REBALANCE_MOVE_RATE_LIMIT:-5}"

# PlacementServer paths (must match what the operator reads from env)
export PLACEMENT_SERVER_PORT=${PLACEMENT_SERVER_PORT:-:8090}
export PLACEMENT_SCORE_PATH=${PLACEMENT_SCORE_PATH:-/api/v1/placement/score}
export PLACEMENT_SERVER_HEALTH_PATH=${PLACEMENT_SERVER_HEALTH_PATH:-/healthz}
export DECISION_AGENT_PATH=${DECISION_AGENT_PATH:-/api/v1/agent/placement/decision}
export EXTENDER_FILTER_PATH=${EXTENDER_FILTER_PATH:-/extender/filter}
export EXTENDER_PRIORITIZE_PATH=${EXTENDER_PRIORITIZE_PATH:-/extender/prioritize}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n==> %s\n' "$*"; }

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

print_config() {
  echo "========================================================"
  echo " HIRO Adaptive Orchestrator — Operator Deploy"
  echo "========================================================"
  echo " One Deployment bundles all of the following:"
  echo "   1) OrchestrationProfile controller  — CR reconciliation, PlacementStatus"
  echo "   2) PlacementServer                  — AI node scoring + energy gate"
  echo "   3) Rebalance Engine                 — hybrid trigger detection"
  echo "   4) Pod scheduler webhook (opt-in)   — auto-sets schedulerName"
  echo "--------------------------------------------------------"
  echo "Namespace          : $NAMESPACE"
  echo "Name Prefix        : $NAME_PREFIX"
  echo "Service Account    : $SA_NAME  (derived)"
  echo "Deployment         : $DEPLOYMENT_NAME  (derived)"
  echo "Operator Image     : $IMG"
  echo "Kubeconfig         : $KUBECONFIG"
  echo "Scheduler Plugin   : $DEPLOY_SCHEDULER_PLUGIN"
  echo "  Overlay                : $([ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ] && echo config/default-with-webhook || echo config/default)"
  echo "EAO CRD            : $EAO_GROUP/$EAO_VERSION, Kind=$EAO_KIND"
  echo "Rebalance Engine   :"
  echo "  Max Recent Decisions   : $REBALANCE_MAX_RECENT_DECISIONS"
  echo "  Detection Interval     : $REBALANCE_DETECTION_INTERVAL"
  echo "  Decision Timeout       : $REBALANCE_DECISION_TIMEOUT"
  echo "  Node Pressure Threshold: $REBALANCE_NODE_PRESSURE_THRESHOLD"
  echo "  Improvement Threshold  : $REBALANCE_IMPROVEMENT_THRESHOLD"
  echo "  Decision Store TTL     : $REBALANCE_DECISION_STORE_TTL"
  echo "  Move Action Timeout    : $REBALANCE_MOVE_ACTION_TIMEOUT"
  echo "  Move Rate Limit        : $REBALANCE_MOVE_RATE_LIMIT"
  echo "PlacementServer    :"
  echo "  Service                : $PLACEMENT_SERVICE_NAME"
  echo "  Port                   : $PLACEMENT_SERVER_PORT"
  echo "  Path                   : $PLACEMENT_SCORE_PATH"
  echo "  Health                 : $PLACEMENT_SERVER_HEALTH_PATH"
  echo "Extender Filter    : $EXTENDER_FILTER_PATH"
  echo "Extender Prioritize: $EXTENDER_PRIORITIZE_PATH"
  if [ "$USE_MOCK_AGENT" = "true" ]; then
    echo "Decision Agent     : MOCK  ($DECISION_AGENT_URL)"
  else
    echo "Decision Agent     : REAL  ($DECISION_AGENT_URL)"
  fi
}

generate_code_and_manifests() {
  step "Linting..."
  make lint

  step "Generating code and manifests..."
  make generate
  make manifests

  step "Installing CRDs..."
  make install
  kubectl get crd orchestrationprofiles.orchestration.hiro.io

  step "Building operator binary..."
  make build

  step "Regenerating Helm charts..."
  rm -rf "$REPO_ROOT/dist"
  kubebuilder edit --plugins=helm/v2-alpha
}

build_and_push_operator_image() {
  step "Authenticating with GitHub Container Registry..."
  echo "$CR_PAT" | docker login ghcr.io -u "$GITHUB_USERNAME" --password-stdin

  step "Building and pushing operator image: $IMG"
  make docker-build docker-push IMG="$IMG"
}

configure_kustomize() {
  step "Configuring Kustomize (namespace=$NAMESPACE, namePrefix=$NAME_PREFIX)..."
  (cd "$REPO_ROOT/config/default" && "$KUSTOMIZE" edit set namespace "$NAMESPACE")
  (cd "$REPO_ROOT/config/default" && "$KUSTOMIZE" edit set nameprefix "$NAME_PREFIX")
  if [ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ]; then
    (cd "$REPO_ROOT/config/default-with-webhook" && "$KUSTOMIZE" edit set namespace "$NAMESPACE")
    (cd "$REPO_ROOT/config/default-with-webhook" && "$KUSTOMIZE" edit set nameprefix "$NAME_PREFIX")
  fi
}

deploy_operator() {
  # Strip NAME_PREFIX to get the base name kustomize will expand back with namePrefix.
  # e.g. PLACEMENT_SERVICE_NAME=hiro-adaptive-orchestrator-my-svc → base=my-svc
  #      kustomize namePrefix hiro-adaptive-orchestrator- + my-svc → hiro-adaptive-orchestrator-my-svc ✓
  local desired_base="${PLACEMENT_SERVICE_NAME#"$NAME_PREFIX"}"
  sed -i.bak "s/name: controller-manager-placement-service/name: ${desired_base}/" \
    "$REPO_ROOT/config/manager/placement_service_patch.yaml"
  rm -f "$REPO_ROOT/config/manager/placement_service_patch.yaml.bak"

  if [ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ]; then
    step "Deploying operator + webhook overlay via Kustomize (config/default-with-webhook)..."
    make deploy-scheduler IMG="$IMG"
  else
    step "Deploying operator via Kustomize (config/default)..."
    make deploy IMG="$IMG"
  fi
}

create_image_pull_secret() {
  step "Creating GHCR image pull secret in namespace '$NAMESPACE'..."
  kubectl create secret docker-registry ghcr-secret \
    --docker-server=ghcr.io \
    --docker-username="$GITHUB_USERNAME" \
    --docker-password="$GITHUB_PAT_TOKEN" \
    --namespace="$NAMESPACE" \
    --dry-run=client -o yaml | kubectl apply -f -
}

patch_operator_service_account() {
  step "Patching service account '$SA_NAME' with image pull secret..."
  kubectl patch serviceaccount "$SA_NAME" \
    -p '{"imagePullSecrets": [{"name": "ghcr-secret"}]}' \
    --namespace="$NAMESPACE"
}

inject_operator_env_vars() {
  step "Injecting environment variables into operator deployment..."
  kubectl set env deployment/"$DEPLOYMENT_NAME" \
    -n "$NAMESPACE" \
    DECISION_AGENT_URL="$DECISION_AGENT_URL" \
    DECISION_AGENT_PATH="$DECISION_AGENT_PATH" \
    PLACEMENT_SERVER_PORT="$PLACEMENT_SERVER_PORT" \
    PLACEMENT_SCORE_PATH="$PLACEMENT_SCORE_PATH" \
    PLACEMENT_SERVER_HEALTH_PATH="$PLACEMENT_SERVER_HEALTH_PATH" \
    EXTENDER_FILTER_PATH="$EXTENDER_FILTER_PATH" \
    EXTENDER_PRIORITIZE_PATH="$EXTENDER_PRIORITIZE_PATH" \
    EAO_GROUP="$EAO_GROUP" \
    EAO_VERSION="$EAO_VERSION" \
    EAO_KIND="$EAO_KIND" \
    REBALANCE_MAX_RECENT_DECISIONS="$REBALANCE_MAX_RECENT_DECISIONS" \
    REBALANCE_DETECTION_INTERVAL="$REBALANCE_DETECTION_INTERVAL" \
    REBALANCE_DECISION_TIMEOUT="$REBALANCE_DECISION_TIMEOUT" \
    REBALANCE_NODE_PRESSURE_THRESHOLD="$REBALANCE_NODE_PRESSURE_THRESHOLD" \
    REBALANCE_IMPROVEMENT_THRESHOLD="$REBALANCE_IMPROVEMENT_THRESHOLD" \
    REBALANCE_DECISION_STORE_TTL="$REBALANCE_DECISION_STORE_TTL" \
    REBALANCE_MOVE_ACTION_TIMEOUT="$REBALANCE_MOVE_ACTION_TIMEOUT" \
    REBALANCE_MOVE_RATE_LIMIT="$REBALANCE_MOVE_RATE_LIMIT"
}

restart_and_wait_for_operator() {
  # inject_operator_env_vars already triggered a rolling update via kubectl set env.
  # Just wait for that rollout to fully settle — no need to delete pods manually,
  # which would race with the in-progress ReplicaSet transition.
  step "Waiting for operator rollout to complete..."
  kubectl rollout status deployment/"$DEPLOYMENT_NAME" \
    -n "$NAMESPACE" \
    --timeout=300s

  kubectl get pods -n "$NAMESPACE"
}

apply_sample_resources() {
  step "Applying sample OrchestrationProfile..."
  kubectl apply -k config/samples/

  step "Verifying OrchestrationProfile resources..."
  kubectl get orchestrationprofiles
  kubectl get orchestrationprofiles -o yaml
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  print_config

  generate_code_and_manifests
  build_and_push_operator_image
  configure_kustomize
  deploy_operator
  create_image_pull_secret
  patch_operator_service_account
  inject_operator_env_vars
  restart_and_wait_for_operator
  apply_sample_resources

  echo ""
  echo -e "\033[32m========================================================\033[0m"
  echo -e "\033[32m  Operator deployed successfully.\033[0m"
  echo -e "\033[32m  Run hack/deploy_full_stack.sh to also deploy the HIRO scheduler.\033[0m"
  echo -e "\033[32m========================================================\033[0m"
}

main "$@"
