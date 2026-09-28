#!/bin/bash
# hack/verify_helm_platform_deployment.sh
#
# Local verification for charts/hiro-adaptive-platform: creates (or reuses) a
# Kind cluster, builds the operator + scheduler-plugin images, loads them
# straight into the cluster (no registry needed — fine for local testing,
# see charts/hiro-adaptive-platform/README.md), deploys the full stack, and
# checks it came up healthy.
#
# Local counterpart to .github/workflows/test-chart-platform.yml, which runs
# the same deploy but with images actually pushed to Docker Hub first — this
# script skips that round-trip for speed.
#
# ─── Options ─────────────────────────────────────────────────────────────────
#   KIND_CLUSTER_NAME     Kind cluster to create/reuse        (default: hiro-platform-verify)
#   SCHED_K8S_VERSION      k8s version to build scheduler for  (default: v1.35.0)
#
# ─── Usage ───────────────────────────────────────────────────────────────────
#   hack/verify_helm_platform_deployment.sh [deploy|clean]
#
#   deploy (default)   Build images, load into Kind, deploy, health-check.
#   clean              Delete the Kind cluster created by "deploy".
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths / config
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

ACTION=${1:-deploy}
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-hiro-platform-verify}
SCHED_K8S_VERSION=${SCHED_K8S_VERSION:-v1.35.0}
HELM_NAMESPACE=${HELM_NAMESPACE:-hiro-adaptive-orchestrator-system}
HELM_PLATFORM_RELEASE=${HELM_PLATFORM_RELEASE:-hiro-adaptive-platform}

OPERATOR_REPO="hiro-operator"
OPERATOR_TAG="verify"
SCHED_REPO="hiro-scheduler"
SCHED_TAG="verify-k8s${SCHED_K8S_VERSION}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n==> %s\n' "$*"; }

# print_banner <text> <ansi-color-code> — boxed heading, same style as
# hack/deploy_full_stack.sh uses for its own summary/access-URL sections.
print_banner() {
  local text="$1" color="$2" width=58
  local border pad left right
  border=$(printf '═%.0s' $(seq 1 "$width"))
  pad=$(( width - ${#text} ))
  left=$(( (pad + 1) / 2 ))
  right=$(( pad - left ))
  echo -e "\033[${color}m  ╔${border}╗\033[0m"
  printf "\033[%sm  ║%*s%s%*s║\033[0m\n" "$color" "$left" "" "$text" "$right" ""
  echo -e "\033[${color}m  ╚${border}╝\033[0m"
}

print_config() {
  echo "========================================================"
  echo " hiro-adaptive-platform — Kind verification ($ACTION)"
  echo "========================================================"
  echo "Kind cluster        : $KIND_CLUSTER_NAME"
  echo "Namespace            : $HELM_NAMESPACE"
  echo "Helm release         : $HELM_PLATFORM_RELEASE"
  echo "Operator image       : ${OPERATOR_REPO}:${OPERATOR_TAG}"
  echo "Scheduler image      : ${SCHED_REPO}:${SCHED_TAG}"
  echo "========================================================"
}

# ---------------------------------------------------------------------------
# Steps — deploy
# ---------------------------------------------------------------------------

check_prerequisites() {
  step "Checking required binaries"
  for cmd in kind kubectl docker helm; do
    command -v "$cmd" >/dev/null || { echo "ERROR: '$cmd' not found in PATH." >&2; exit 1; }
    echo "  [ok] $cmd -> $(command -v "$cmd")"
  done
}

ensure_kind_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER_NAME"; then
    step "Reusing existing Kind cluster: $KIND_CLUSTER_NAME"
  else
    step "Creating Kind cluster: $KIND_CLUSTER_NAME"
    kind create cluster --name "$KIND_CLUSTER_NAME"
  fi
  kubectl config use-context "kind-${KIND_CLUSTER_NAME}"
}

build_operator_image() {
  step "Building operator image: ${OPERATOR_REPO}:${OPERATOR_TAG}"
  make -C "$REPO_ROOT" docker-build IMG="${OPERATOR_REPO}:${OPERATOR_TAG}"
}

build_scheduler_image() {
  step "Building scheduler image: ${SCHED_REPO}:${SCHED_TAG}"
  make -C "$REPO_ROOT" docker-build-scheduler \
    SCHED_IMG="$SCHED_REPO" \
    SCHED_VERSION="$OPERATOR_TAG" \
    SCHED_K8S_VERSION="$SCHED_K8S_VERSION"
}

load_images_into_kind() {
  step "Loading images into Kind (no registry needed)"
  kind load docker-image "${OPERATOR_REPO}:${OPERATOR_TAG}" --name "$KIND_CLUSTER_NAME"
  kind load docker-image "${SCHED_REPO}:${SCHED_TAG}" --name "$KIND_CLUSTER_NAME"
}

deploy_platform() {
  step "Deploying charts/hiro-adaptive-platform"
  make -C "$REPO_ROOT" helm-platform-deploy \
    HELM_NAMESPACE="$HELM_NAMESPACE" \
    HELM_PLATFORM_RELEASE="$HELM_PLATFORM_RELEASE" \
    HELM_EXTRA_ARGS="\
      --set hiro-operator.manager.image.repository=${OPERATOR_REPO} \
      --set hiro-operator.manager.image.tag=${OPERATOR_TAG} \
      --set hiro-operator.manager.image.pullPolicy=IfNotPresent \
      --set scheduler.mode=plugin \
      --set scheduler.plugin.image.repository=${SCHED_REPO} \
      --set scheduler.plugin.image.tag=${SCHED_TAG} \
      --set scheduler.plugin.image.pullPolicy=IfNotPresent \
      --set webhook.enable=true \
      --set webhook.certManager.enable=true \
      --set prerequisites.certManager.enable=true \
      --set mockAgent.enable=true"
}

check_placement_server_health() {
  step "Checking PlacementServer health"
  local svc="${HELM_PLATFORM_RELEASE}-hiro-operator-controller-manager-placement-service"
  kubectl -n "$HELM_NAMESPACE" run verify-curl --rm -i --restart=Never \
    --image=curlimages/curl:latest -- \
    curl -sf "http://${svc}.${HELM_NAMESPACE}.svc.cluster.local:8090/healthz"
  echo "  PlacementServer is healthy."
}

print_summary() {
  local svc="${HELM_PLATFORM_RELEASE}-hiro-operator-controller-manager-placement-service"

  echo ""
  print_banner "Verification complete." 32
  echo ""
  echo -e "\033[32m  ── Components ────────────────────────────────────────────\033[0m"
  echo -e "\033[32m    Operator          : deployed  (namespace/$HELM_NAMESPACE)\033[0m"
  echo -e "\033[32m    Scheduler plugin  : deployed  (schedulerName: hiro-scheduler)\033[0m"
  echo -e "\033[32m    Webhook           : deployed  (TLS via cert-manager)\033[0m"
  echo -e "\033[32m    Mock decision agent: deployed  (mock-decision-agent.$HELM_NAMESPACE)\033[0m"
  echo -e "\033[32m    PlacementServer   : healthy\033[0m"
  echo ""

  step "Pods in $HELM_NAMESPACE"
  kubectl -n "$HELM_NAMESPACE" get pods

  echo ""
  print_banner "Port-forwarding (run manually)" 36
  echo ""
  echo "  kubectl port-forward -n $HELM_NAMESPACE svc/$svc 18090:8090 &"
  echo "  kubectl port-forward -n $HELM_NAMESPACE svc/mock-decision-agent 18080:8080 &"
  echo ""

  print_banner "Access URLs (after port-forwarding)" 36
  echo ""
  printf "    %-26s %s\n" "PlacementServer health" "http://localhost:18090/healthz"
  printf "    %-26s %s\n" "Mock decision agent" "http://localhost:18080"
  echo ""

  echo "  Teardown: hack/verify_helm_platform_deployment.sh clean"
  echo ""
}

run_deploy() {
  check_prerequisites
  ensure_kind_cluster
  build_operator_image
  build_scheduler_image
  load_images_into_kind
  deploy_platform
  check_placement_server_health
  print_summary
}

# ---------------------------------------------------------------------------
# Steps — clean
# ---------------------------------------------------------------------------

run_clean() {
  step "Deleting Kind cluster: $KIND_CLUSTER_NAME"
  kind delete cluster --name "$KIND_CLUSTER_NAME"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  print_config
  case "$ACTION" in
    deploy) run_deploy ;;
    clean)  run_clean ;;
    *)
      echo "ERROR: unknown action '$ACTION' — expected 'deploy' or 'clean'." >&2
      exit 1
      ;;
  esac
}

main
