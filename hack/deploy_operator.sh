#!/bin/bash
# hack/deploy_operator.sh
#
# Builds, pushes, and deploys the HIRO Adaptive Orchestrator (operator only).
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
# ─── PlacementServer (served by this operator) ───────────────────────────────
#   PLACEMENT_SERVICE_NAME    k8s Service name            (default: <NAME_PREFIX>controller-manager-placement-service)
#   PLACEMENT_SERVER_PORT     listening port              (default: :8090)
#   PLACEMENT_SERVER_PATH     decision endpoint path      (default: /api/v1/placement/decision)
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
# ─── EnergyAwareOrchestration CRD ────────────────────────────────────────────
#   EAO_GROUP                 CRD API group               (default: eas.hiro.io)
#   EAO_VERSION               CRD API version             (default: v1)
#   EAO_KIND                  CRD Kind                    (default: EnergyAwareOrchestration)
#
# Usage (standalone):
#   export GITHUB_PAT_TOKEN=<token>
#   hack/deploy_operator.sh [kubeconfig-path]
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

# Derived names (kubebuilder convention: <NAME_PREFIX>controller-manager)
SA_NAME="${NAME_PREFIX}controller-manager"
DEPLOYMENT_NAME="${NAME_PREFIX}controller-manager"
# PlacementServer k8s Service name — override if you rename the service.
# Must match the name kustomize generates (namePrefix + "controller-manager-placement-service").
export PLACEMENT_SERVICE_NAME=${PLACEMENT_SERVICE_NAME:-${NAME_PREFIX}controller-manager-placement-service}

# Decision agent
USE_MOCK_AGENT=${USE_MOCK_AGENT:-true}
if [ "$USE_MOCK_AGENT" = "true" ]; then
  export DECISION_AGENT_URL="http://decision-agent:8080"
else
  : "${DECISION_AGENT_URL:?DECISION_AGENT_URL must be set when USE_MOCK_AGENT=false}"
fi

# EnergyAwareOrchestration CRD coordinates
export EAO_GROUP="${EAO_GROUP:-eas.hiro.io}"
export EAO_VERSION="${EAO_VERSION:-v1}"
export EAO_KIND="${EAO_KIND:-EnergyAwareOrchestration}"

# PlacementServer paths (must match what the operator reads from env)
export PLACEMENT_SERVER_PORT=${PLACEMENT_SERVER_PORT:-:8090}
export PLACEMENT_SERVER_PATH=${PLACEMENT_SERVER_PATH:-/api/v1/placement/decision}
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
  echo "Namespace          : $NAMESPACE"
  echo "Name Prefix        : $NAME_PREFIX"
  echo "Service Account    : $SA_NAME  (derived)"
  echo "Deployment         : $DEPLOYMENT_NAME  (derived)"
  echo "Operator Image     : $IMG"
  echo "Kubeconfig         : $KUBECONFIG"
  echo "EAO CRD            : $EAO_GROUP/$EAO_VERSION, Kind=$EAO_KIND"
  echo "PlacementServer    : Service=$PLACEMENT_SERVICE_NAME  Port=$PLACEMENT_SERVER_PORT  Path=$PLACEMENT_SERVER_PATH  Health=$PLACEMENT_SERVER_HEALTH_PATH"
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
}

deploy_operator() {
  step "Deploying operator via Kustomize..."
  # Strip NAME_PREFIX to get the base name kustomize will expand back with namePrefix.
  # e.g. PLACEMENT_SERVICE_NAME=hiro-adaptive-orchestrator-my-svc → base=my-svc
  #      kustomize namePrefix hiro-adaptive-orchestrator- + my-svc → hiro-adaptive-orchestrator-my-svc ✓
  local desired_base="${PLACEMENT_SERVICE_NAME#"$NAME_PREFIX"}"
  sed -i.bak "s/name: controller-manager-placement-service/name: ${desired_base}/" \
    "$REPO_ROOT/config/manager/placement_service_patch.yaml"
  rm -f "$REPO_ROOT/config/manager/placement_service_patch.yaml.bak"
  make deploy IMG="$IMG"
}

deploy_mock_agent() {
  step "Deploying mock decision agent into namespace '$NAMESPACE'..."
  sed "s/namespace: hiro-adaptive-orchestrator-system/namespace: $NAMESPACE/g" \
    hack/mock-decision-agent.yaml | kubectl apply -f -

  step "Waiting for mock decision agent to be ready..."
  kubectl wait --for=condition=Ready pod \
    -l app=decision-agent \
    -n "$NAMESPACE" \
    --timeout=120s
  echo "Mock decision agent is ready."
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
    PLACEMENT_SERVER_PATH="$PLACEMENT_SERVER_PATH" \
    PLACEMENT_SERVER_HEALTH_PATH="$PLACEMENT_SERVER_HEALTH_PATH" \
    EXTENDER_FILTER_PATH="$EXTENDER_FILTER_PATH" \
    EXTENDER_PRIORITIZE_PATH="$EXTENDER_PRIORITIZE_PATH" \
    EAO_GROUP="$EAO_GROUP" \
    EAO_VERSION="$EAO_VERSION" \
    EAO_KIND="$EAO_KIND"
}

restart_and_wait_for_operator() {
  # We use app.kubernetes.io/name=hiro-adaptive-orchestrator as the label selector to 
  # target the operator pod, which is set in config/default/kustomization.yaml.
  step "Restarting operator pod to pick up new image and env vars..."
  kubectl delete pod -l control-plane=controller-manager -n "$NAMESPACE" --ignore-not-found

  step "Waiting for operator pod to be ready..."
  kubectl wait --for=condition=Ready pod \
    -l app.kubernetes.io/name=hiro-adaptive-orchestrator \
    -n "$NAMESPACE" \
    --timeout=180s

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

  if [ "$USE_MOCK_AGENT" = "true" ]; then
    deploy_mock_agent
  fi

  create_image_pull_secret
  patch_operator_service_account
  inject_operator_env_vars
  restart_and_wait_for_operator
  apply_sample_resources

  echo ""
  echo -e "\033[32m========================================================\033[0m"
  echo -e "\033[32m  Operator deployed successfully.\033[0m"
  echo -e "\033[32m  Run hack/deploy_all.sh to also deploy the HIRO scheduler.\033[0m"
  echo -e "\033[32m========================================================\033[0m"
}

main "$@"
