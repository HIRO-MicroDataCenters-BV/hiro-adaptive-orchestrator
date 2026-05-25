#!/bin/bash

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
HIRO_OPERATOR_IMAGE=hiro-adaptive-orchestrator-controller:latest

export KUBECONFIG=${2:-~/.kube/config}
export IMG="${DOCKER_REGISTRY}/${HIRO_OPERATOR_IMAGE}"
export CR_PAT="$GITHUB_PAT_TOKEN"

# Kustomize deployment identity
export NAME_PREFIX=${NAME_PREFIX:-hiro-adaptive-orchestrator-}
export NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}

# Derived names (kubebuilder convention: <NAME_PREFIX>controller-manager)
SA_NAME="${NAME_PREFIX}controller-manager"
DEPLOYMENT_NAME="${NAME_PREFIX}controller-manager"

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

# PlacementServer / Extender paths
export PLACEMENT_SERVER_PORT=${PLACEMENT_SERVER_PORT:-:8090}
export PLACEMENT_SERVER_PATH=${PLACEMENT_SERVER_PATH:-/api/v1/placement/decision}
export PLACEMENT_SERVER_HEALTH_PATH=${PLACEMENT_SERVER_HEALTH_PATH:-/healthz}
export DECISION_AGENT_PATH=${DECISION_AGENT_PATH:-/api/v1/agent/placement/decision}
export EXTENDER_FILTER_PATH=${EXTENDER_FILTER_PATH:-/extender/filter}
export EXTENDER_PRIORITIZE_PATH=${EXTENDER_PRIORITIZE_PATH:-/extender/prioritize}

# Set APPLY_SCHEDULER_CONFIG=true to create the hiro-scheduler-config ConfigMap in kube-system
APPLY_SCHEDULER_CONFIG=${APPLY_SCHEDULER_CONFIG:-false}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n==> %s\n' "$*"; }

# ---------------------------------------------------------------------------
# Functions
# ---------------------------------------------------------------------------

print_config() {
  echo "========================================"
  echo "HIRO Adaptive Orchestrator Deployment"
  echo "========================================"
  echo "Namespace          : $NAMESPACE"
  echo "Name Prefix        : $NAME_PREFIX"
  echo "Service Account    : $SA_NAME  (derived)"
  echo "Deployment         : $DEPLOYMENT_NAME  (derived)"
  echo "Operator Image     : $IMG"
  echo "Kubeconfig         : $KUBECONFIG"
  echo "EAO CRD            : $EAO_GROUP/$EAO_VERSION, Kind=$EAO_KIND"
  echo "Placement Server   : Port=$PLACEMENT_SERVER_PORT  Path=$PLACEMENT_SERVER_PATH  Health=$PLACEMENT_SERVER_HEALTH_PATH"
  echo "Extender Filter    : $EXTENDER_FILTER_PATH"
  echo "Extender Prioritize: $EXTENDER_PRIORITIZE_PATH"
  if [ "$USE_MOCK_AGENT" = "true" ]; then
    echo "Decision Agent     : MOCK  ($DECISION_AGENT_URL)"
  else
    echo "Decision Agent     : REAL  ($DECISION_AGENT_URL)"
  fi
  echo "Scheduler Config   : APPLY_SCHEDULER_CONFIG=$APPLY_SCHEDULER_CONFIG"
}

generate_code() {
  step "Running linter..."
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

build_and_push_image() {
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
  step "Deploying operator..."
  make deploy IMG="$IMG"
}

deploy_mock_agent() {
  step "Deploying mock decision agent into namespace '$NAMESPACE'..."
  sed "s/namespace: hiro-adaptive-orchestrator-system/namespace: $NAMESPACE/g" \
    hack/mock-decision-agent.yaml | kubectl apply -f -

  step "Waiting for mock decision agent pod to be ready..."
  kubectl wait --for=condition=Ready pod \
    -l app=decision-agent \
    -n "$NAMESPACE" \
    --timeout=120s
  echo "Mock decision agent is ready."
}

create_image_pull_secret() {
  step "Creating GHCR image pull secret..."
  kubectl create secret docker-registry ghcr-secret \
    --docker-server=ghcr.io \
    --docker-username="$GITHUB_USERNAME" \
    --docker-password="$GITHUB_PAT_TOKEN" \
    --namespace="$NAMESPACE" \
    --dry-run=client -o yaml | kubectl apply -f -
}

patch_service_account() {
  step "Patching service account '$SA_NAME' with image pull secret..."
  kubectl patch serviceaccount "$SA_NAME" \
    -p '{"imagePullSecrets": [{"name": "ghcr-secret"}]}' \
    --namespace="$NAMESPACE"
}

inject_env_vars() {
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

restart_and_wait_operator() {
  step "Restarting operator pod to pick up new image and env vars..."
  kubectl delete pod -l control-plane=controller-manager -n "$NAMESPACE" --ignore-not-found

  step "Waiting for operator pod to be ready..."
  kubectl wait --for=condition=Ready pod \
    -l app.kubernetes.io/name=hiro-adaptive-orchestrator \
    -n "$NAMESPACE" \
    --timeout=180s

  kubectl get pods -n "$NAMESPACE"
}

apply_extender_scheduler_config() {
  if [ "$APPLY_SCHEDULER_CONFIG" != "true" ]; then
    step "Skipping scheduler extender ConfigMap (APPLY_SCHEDULER_CONFIG=false)."
    echo "To apply later, re-run with: APPLY_SCHEDULER_CONFIG=true $0"
    return
  fi

  step "Applying scheduler extender ConfigMap to kube-system..."
  local placement_service="${NAME_PREFIX}controller-manager-placement-service"
  local tmp_config
  tmp_config=$(mktemp /tmp/hiro-scheduler-config-XXXXXX.yaml)

  sed \
    -e "s|hiro-adaptive-orchestrator-controller-manager-placement-service|${placement_service}|g" \
    -e "s|hiro-adaptive-orchestrator-system|${NAMESPACE}|g" \
    "$REPO_ROOT/config/extender/scheduler-config.yaml" > "$tmp_config"

  kubectl create configmap hiro-scheduler-config \
    --from-file=scheduler-config.yaml="$tmp_config" \
    -n kube-system \
    --dry-run=client -o yaml | kubectl apply -f -

  rm -f "$tmp_config"

  echo "ConfigMap 'hiro-scheduler-config' created/updated in kube-system."
  echo "NEXT STEP: mount this ConfigMap into the kube-scheduler pod and"
  echo "pass --config=/etc/kubernetes/scheduler-config.yaml."
  echo "See config/extender/scheduler-config.yaml for full mount instructions."
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

  generate_code
  build_and_push_image
  configure_kustomize
  deploy_operator

  if [ "$USE_MOCK_AGENT" = "true" ]; then
    deploy_mock_agent
  fi

  create_image_pull_secret
  patch_service_account
  inject_env_vars
  restart_and_wait_operator

  apply_extender_scheduler_config
  apply_sample_resources

  echo ""
  echo -e "\033[33m+++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++\033[0m"
  echo -e "\033[33m++++ Deployment and sample applied.                                ++++\033[0m"
  echo -e "\033[33m+++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++\033[0m"
}

main "$@"
