#!/bin/bash
# hack/deploy_full_stack_with_helm.sh
#
# Local verification for charts/hiro-adaptive-platform: regenerates
# manifests/code (same as hack/deploy_operator.sh), creates (or reuses) a
# Kind cluster, builds the operator + scheduler-plugin images, loads them
# straight into the cluster (no registry needed — fine for local testing,
# see charts/hiro-adaptive-platform/README.md), deploys the full stack, and
# checks it came up healthy.
#
# Local counterpart to .github/workflows/verify-chart-platform.yml, which runs
# the same deploy but with images actually pushed to Docker Hub first — this
# script skips that round-trip for speed.
#
# ─── Options ─────────────────────────────────────────────────────────────────
#   KIND_CLUSTER_NAME     Kind cluster to create/reuse        (default: hiro-platform-verify)
#   SCHED_K8S_VERSION      k8s version to build scheduler for  (default: v1.35.0)
#
# ─── Usage ───────────────────────────────────────────────────────────────────
#   hack/deploy_full_stack_with_helm.sh [deploy|clean]
#
#   deploy (default)   Build images, load into Kind, deploy, health-check.
#   clean              Delete the Kind cluster created by "deploy".
set -euo pipefail

# ---------------------------------------------------------------------------
# Paths / config
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

# Pin to go.mod's own version rather than whatever's linked as the system
# `go` — golangci-lint's vendored export-data reader lags behind very recent
# Go releases (e.g. a Homebrew `go` bump from an unrelated `brew upgrade`),
# causing "export data version N is greater than maximum supported version"
# on `make lint`. Setting this makes `go` auto-download/use the exact
# go.mod-declared version for every command this script runs, regardless of
# the system default.
export GOTOOLCHAIN="go$(awk '/^go /{print $2}' "$REPO_ROOT/go.mod")"

ACTION=${1:-deploy}
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-hiro-platform-verify}
SCHED_K8S_VERSION=${SCHED_K8S_VERSION:-v1.35.0}
HELM_NAMESPACE=${HELM_NAMESPACE:-hiro-adaptive-orchestrator-system}
HELM_PLATFORM_RELEASE=${HELM_PLATFORM_RELEASE:-hiro-adaptive-platform}

OPERATOR_REPO="hiro-operator"
OPERATOR_TAG="verify"
SCHED_REPO="hiro-scheduler"
SCHED_TAG="verify-k8s${SCHED_K8S_VERSION}"
SCHED_NAME=${SCHED_NAME:-hiro-scheduler}
KIND_KUBECONFIG_PATH="${REPO_ROOT}/.kind-kubeconfig-${KIND_CLUSTER_NAME}.yaml"
PLACEMENT_SERVER_LABEL_SELECTOR="app.kubernetes.io/component=placement-server"

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
  for cmd in kind kubectl docker helm kubebuilder; do
    command -v "$cmd" >/dev/null || { echo "ERROR: '$cmd' not found in PATH." >&2; exit 1; }
    echo "  [ok] $cmd -> $(command -v "$cmd")"
  done
}

# Same as hack/deploy_operator.sh's generate_code_and_manifests(), minus
# `make install`/`make build` (CRDs are installed via the Helm chart itself;
# the operator is built as a Docker image, not a local binary here). IMG is
# read from config/manager/kustomization.yaml's own committed default rather
# than hardcoded, so this can't clobber it — see .github/workflows/
# verify-chart-platform.yml's manifests-drift-check job for the same pattern.
regenerate_manifests() {
  step "Linting..."
  make -C "$REPO_ROOT" lint

  step "Generating code and manifests..."
  make -C "$REPO_ROOT" generate
  make -C "$REPO_ROOT" manifests

  step "Regenerating dist/chart..."
  local img_name img_tag
  img_name=$(awk '/newName:/{print $2}' "$REPO_ROOT/config/manager/kustomization.yaml")
  img_tag=$(awk '/newTag:/{print $2}' "$REPO_ROOT/config/manager/kustomization.yaml")
  ( cd "$REPO_ROOT" && rm -rf dist && IMG="${img_name}:${img_tag}" kubebuilder edit --plugins=helm/v2-alpha --force )
  echo "  Regenerated with IMG=${img_name}:${img_tag}"
}

ensure_kind_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER_NAME"; then
    step "Reusing existing Kind cluster: $KIND_CLUSTER_NAME"
  else
    step "Creating Kind cluster: $KIND_CLUSTER_NAME"
    kind create cluster --name "$KIND_CLUSTER_NAME"
  fi
  kubectl config use-context "kind-${KIND_CLUSTER_NAME}"

  kind get kubeconfig --name "$KIND_CLUSTER_NAME" > "$KIND_KUBECONFIG_PATH"
  echo "  Kubeconfig: $KIND_KUBECONFIG_PATH"
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

# Phase 1: operator only. scheduler.mode stays "none" and webhook.enable
# stays "false" here on purpose — scheduler.plugin.placementServer.url has
# no default (the operator's Service name depends on Helm's own
# name-truncation logic, so it can't be known before the operator exists),
# and webhook.enable requires scheduler.mode=plugin. Both get turned on in
# phase 2, once the Service this chart needs to point at is actually live.
deploy_operator_phase() {
  step "Phase 1: deploying operator (scheduler + webhook off for now)"
  make -C "$REPO_ROOT" helm-platform-deploy \
    HELM_NAMESPACE="$HELM_NAMESPACE" \
    HELM_PLATFORM_RELEASE="$HELM_PLATFORM_RELEASE" \
    HELM_EXTRA_ARGS="\
      --set hiro-operator.manager.image.repository=${OPERATOR_REPO} \
      --set hiro-operator.manager.image.tag=${OPERATOR_TAG} \
      --set hiro-operator.manager.image.pullPolicy=IfNotPresent \
      --set prerequisites.certManager.enable=true \
      --set mockAgent.enable=true"
}

# Same lookup charts/hiro-adaptive-platform/README.md documents doing by
# hand: find the PlacementServer Service by label, now that it exists.
resolve_placement_server_url() {
  step "Resolving PlacementServer Service"
  local svc
  svc=$(kubectl -n "$HELM_NAMESPACE" get svc \
    -l "$PLACEMENT_SERVER_LABEL_SELECTOR" \
    -o jsonpath='{.items[0].metadata.name}')
  if [ -z "$svc" ]; then
    echo "ERROR: no Service labeled app.kubernetes.io/component=placement-server in namespace $HELM_NAMESPACE." >&2
    exit 1
  fi
  PLACEMENT_SERVER_URL="http://${svc}.${HELM_NAMESPACE}.svc.cluster.local:8090"
  echo "  Service : $svc"
  echo "  URL     : $PLACEMENT_SERVER_URL"
}

# Phase 2: now that PLACEMENT_SERVER_URL is known, turn on the scheduler
# plugin and the webhook. --reuse-values keeps phase 1's settings (operator
# image, mockAgent, cert-manager) — without it this `helm upgrade` would
# reset everything not repeated below back to values.yaml's defaults.
deploy_scheduler_phase() {
  step "Phase 2: enabling scheduler plugin + webhook"
  make -C "$REPO_ROOT" helm-platform-deploy \
    HELM_NAMESPACE="$HELM_NAMESPACE" \
    HELM_PLATFORM_RELEASE="$HELM_PLATFORM_RELEASE" \
    HELM_EXTRA_ARGS="\
      --reuse-values \
      --set scheduler.mode=plugin \
      --set scheduler.plugin.schedulerName=${SCHED_NAME} \
      --set scheduler.plugin.image.repository=${SCHED_REPO} \
      --set scheduler.plugin.image.tag=${SCHED_TAG} \
      --set scheduler.plugin.image.pullPolicy=IfNotPresent \
      --set scheduler.plugin.placementServer.url=${PLACEMENT_SERVER_URL} \
      --set webhook.enable=true \
      --set webhook.certManager.enable=true"
}

check_placement_server_health() {
  step "Checking PlacementServer health"
  local url="${PLACEMENT_SERVER_URL}/healthz"

  # Deployment being Ready (what helm --wait already guarantees) doesn't
  # mean the Service's endpoints have propagated yet — retry instead of a
  # single shot.
  local attempt
  for attempt in $(seq 1 10); do
    if kubectl -n "$HELM_NAMESPACE" run "verify-curl-${attempt}" --rm -i --restart=Never \
      --image=curlimages/curl:latest -- curl -sf "$url" >/dev/null 2>&1; then
      echo "  PlacementServer is healthy."
      return 0
    fi
    sleep 3
  done

  echo "ERROR: PlacementServer at $url did not become healthy after 10 attempts." >&2
  exit 1
}

print_summary() {
  local svc
  svc=$(kubectl -n "$HELM_NAMESPACE" get svc -l "$PLACEMENT_SERVER_LABEL_SELECTOR" -o jsonpath='{.items[0].metadata.name}')

  echo ""
  print_banner "Verification complete." 32
  echo ""
  echo -e "\033[32m  ── Components ────────────────────────────────────────────\033[0m"
  echo -e "\033[32m    Operator          : deployed  (namespace/$HELM_NAMESPACE)\033[0m"
  echo -e "\033[32m    Scheduler plugin  : deployed  (schedulerName: $SCHED_NAME)\033[0m"
  echo -e "\033[32m    Webhook           : deployed  (TLS via cert-manager)\033[0m"
  echo -e "\033[32m    Mock decision agent: deployed  (mock-decision-agent.$HELM_NAMESPACE)\033[0m"
  echo -e "\033[32m    PlacementServer   : healthy\033[0m"
  echo ""
  echo -e "\033[32m    Kubeconfig        : $KIND_KUBECONFIG_PATH\033[0m"
  echo -e "\033[32m    (or: kubectl config use-context kind-${KIND_CLUSTER_NAME})\033[0m"
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

  echo "  Teardown: hack/deploy_full_stack_with_helm.sh clean"
  echo ""
}

run_deploy() {
  check_prerequisites
  regenerate_manifests
  ensure_kind_cluster
  build_operator_image
  build_scheduler_image
  load_images_into_kind
  deploy_operator_phase
  resolve_placement_server_url
  deploy_scheduler_phase
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
