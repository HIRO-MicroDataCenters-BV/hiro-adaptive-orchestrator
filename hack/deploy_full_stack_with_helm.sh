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
# Local counterpart to .github/workflows/platform-verify-chart.yml, which runs
# the same deploy but with images actually pushed to Docker Hub first — this
# script skips that round-trip for speed.
#
# ─── Options ─────────────────────────────────────────────────────────────────
#   KIND_CLUSTER_NAME     Kind cluster to create/reuse        (default: hiro-platform-verify)
#   SCHED_K8S_VERSION     k8s version to build scheduler for  (default: v1.35.0)
#   HELM_NAMESPACE        Namespace for the Helm release      (default: hiro-adaptive-orchestrator-system)
#   HELM_PLATFORM_RELEASE Helm release name                   (default: hiro-adaptive-platform)
#   SCHED_NAME            schedulerName the webhook injects   (default: hiro-scheduler)
#
# ─── Optional components ─────────────────────────────────────────────────────
# Each drives both the Helm values and this script's final summary, so what's
# printed always matches what was deployed.
#
#   SCHEDULER_MODE        plugin | extender | none            (default: plugin)
#                         plugin   — standalone scheduler Deployment, plus the
#                                    webhook that sets schedulerName on pods.
#                         extender — patches the cluster's own kube-scheduler
#                                    to call the PlacementServer over HTTP; no
#                                    scheduler pod and no webhook.
#                         none     — operator only.
#   USE_MOCK_AGENT        Bundled mock decision agent         (default: true)
#   DEPLOY_SAMPLES        nginx + OrchestrationProfile samples (default: true)
#   DEPLOY_PROMETHEUS     Bundle kube-prometheus-stack and turn on both
#                         ServiceMonitors, so the hiro_* metrics are actually
#                         scraped instead of just exposed      (default: false)
#   OTLP_ENDPOINT         OTLP gRPC collector for traces, e.g.
#                         alloy.observability.svc:4317. Empty disables tracing
#                         entirely — nothing is exported       (default: empty)
#   METRICS_READER_SA     "namespace:serviceaccount" allowed to GET /metrics,
#                         passed to the chart as observability.metricsReaders.
#                         Defaults to the operator's own SA so the access URLs
#                         below work; with DEPLOY_PROMETHEUS=true it defaults
#                         to empty, since Prometheus grants itself already.
#
# No GITHUB_PAT_TOKEN/registry needed — images are built locally and loaded
# directly into Kind, unlike hack/deploy_full_stack.sh which pushes to GHCR.
#
# ─── Usage ───────────────────────────────────────────────────────────────────
#   hack/deploy_full_stack_with_helm.sh [deploy|clean|destroy]
#
#   deploy (default)   Build images, load into Kind, deploy, health-check.
#   clean              Uninstall the Helm release, keep the Kind cluster. In
#                      extender mode this is what reverts the kube-scheduler
#                      patch (a pre-delete hook), so prefer it over destroy.
#                      The CRD is annotated resource-policy: keep and stays,
#                      along with any OrchestrationProfiles you created.
#   destroy            Delete the whole Kind cluster.
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

USE_MOCK_AGENT=${USE_MOCK_AGENT:-true}
DEPLOY_SAMPLES=${DEPLOY_SAMPLES:-true}
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-false}
OTLP_ENDPOINT=${OTLP_ENDPOINT:-}
SCHEDULER_MODE=${SCHEDULER_MODE:-plugin}
KIND_KUBECONFIG_PATH="${REPO_ROOT}/.kind-kubeconfig-${KIND_CLUSTER_NAME}.yaml"

# These two Services each carry a unique app.kubernetes.io/component, so a
# positive label match identifies them outright. The operator's metrics
# Service does not — see operator_metrics_svc_name.
PLACEMENT_SERVER_LABEL_SELECTOR="app.kubernetes.io/component=placement-server"
SCHEDULER_METRICS_LABEL_SELECTOR="app.kubernetes.io/component=scheduler-metrics"
OPERATOR_LABEL_SELECTOR="control-plane=controller-manager"

# Both are sent to Helm below, so these are the single source of truth rather
# than a guess at what the chart defaulted to.
OPERATOR_METRICS_PORT=8443
SCHEDULER_METRICS_PORT=10259

METRICS_READER_SA=${METRICS_READER_SA:-}
METRICS_READER=""

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

validate_config() {
  case "$SCHEDULER_MODE" in
    plugin|extender|none) ;;
    *)
      echo "ERROR: SCHEDULER_MODE='$SCHEDULER_MODE' — expected plugin, extender, or none." >&2
      exit 1
      ;;
  esac
}

# print_subhead <text> — underlined sub-heading inside a banner section.
print_subhead() {
  local text="$1" width=58
  printf '\n    %s\n' "$text"
  printf '    %s\n' "$(printf '─%.0s' $(seq 1 "$width"))"
}

print_config() {
  echo "========================================================"
  echo " hiro-adaptive-platform — Kind verification ($ACTION)"
  echo "========================================================"
  echo "Kind cluster        : $KIND_CLUSTER_NAME"
  echo "Namespace            : $HELM_NAMESPACE"
  echo "Helm release         : $HELM_PLATFORM_RELEASE"
  echo "Operator image       : ${OPERATOR_REPO}:${OPERATOR_TAG}"
  echo "Scheduler mode       : $SCHEDULER_MODE"
  if [ "$SCHEDULER_MODE" = "plugin" ]; then
    echo "Scheduler image      : ${SCHED_REPO}:${SCHED_TAG}"
  fi
  echo "Mock agent           : $USE_MOCK_AGENT"
  echo "Samples              : $DEPLOY_SAMPLES"
  echo "Prometheus           : $DEPLOY_PROMETHEUS"
  echo "Tracing endpoint     : ${OTLP_ENDPOINT:-<none>}"
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
# platform-verify-chart.yml's manifests-drift-check job for the same pattern.
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

# kind_cluster_config prints the Kind Cluster config — same topology/
# networking as hack/create_kind_cluster.sh.
kind_cluster_config() {
  cat <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: "0.0.0.0"
  podSubnet: "10.245.0.0/16"
  serviceSubnet: "10.97.0.0/12"
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30010
        hostPort: 30010
        protocol: TCP
      - containerPort: 30015
        hostPort: 30015
        protocol: TCP
      - containerPort: 30016
        hostPort: 30016
        protocol: TCP
      - containerPort: 30018
        hostPort: 30018
        protocol: TCP
  - role: worker
  - role: worker
EOF
}

ensure_kind_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER_NAME"; then
    step "Reusing existing Kind cluster: $KIND_CLUSTER_NAME"
  else
    step "Creating Kind cluster: $KIND_CLUSTER_NAME"
    kind create cluster --name "$KIND_CLUSTER_NAME" --config <(kind_cluster_config)
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
  if [ "$SCHEDULER_MODE" != "plugin" ]; then
    step "Skipping scheduler image (SCHEDULER_MODE=$SCHEDULER_MODE)"
    return
  fi
  step "Building scheduler image: ${SCHED_REPO}:${SCHED_TAG}"
  make -C "$REPO_ROOT" docker-build-scheduler \
    SCHED_IMG="$SCHED_REPO" \
    SCHED_VERSION="$OPERATOR_TAG" \
    SCHED_K8S_VERSION="$SCHED_K8S_VERSION"
}

load_images_into_kind() {
  step "Loading images into Kind (no registry needed)"
  kind load docker-image "${OPERATOR_REPO}:${OPERATOR_TAG}" --name "$KIND_CLUSTER_NAME"
  if [ "$SCHEDULER_MODE" = "plugin" ]; then
    kind load docker-image "${SCHED_REPO}:${SCHED_TAG}" --name "$KIND_CLUSTER_NAME"
  fi
}

# Phase 1: operator only. scheduler.mode stays "none" and webhook.enable
# stays "false" here on purpose — scheduler.plugin.placementServer.url has
# no default (the operator's Service name depends on Helm's own
# name-truncation logic, so it can't be known before the operator exists),
# and webhook.enable requires scheduler.mode=plugin. Both get turned on in
# phase 2, once the Service this chart needs to point at is actually live.
deploy_operator_phase() {
  step "Phase 1: deploying operator (scheduler + webhook off for now)"

  # envOverrides is a map the operator subchart appends after manager.env, so
  # this overrides values.yaml's empty default without touching that list.
  local tracing_args=""
  if [ -n "$OTLP_ENDPOINT" ]; then
    tracing_args="--set hiro-operator.manager.envOverrides.OTEL_EXPORTER_OTLP_ENDPOINT=${OTLP_ENDPOINT}"
  fi

  make -C "$REPO_ROOT" helm-platform-deploy \
    HELM_NAMESPACE="$HELM_NAMESPACE" \
    HELM_PLATFORM_RELEASE="$HELM_PLATFORM_RELEASE" \
    HELM_EXTRA_ARGS="\
      --set hiro-operator.manager.image.repository=${OPERATOR_REPO} \
      --set hiro-operator.manager.image.tag=${OPERATOR_TAG} \
      --set hiro-operator.manager.image.pullPolicy=IfNotPresent \
      --set hiro-operator.metrics.port=${OPERATOR_METRICS_PORT} \
      --set prerequisites.certManager.enable=true \
      --set prerequisites.prometheus.enable=${DEPLOY_PROMETHEUS} \
      --set hiro-operator.prometheus.enabled=${DEPLOY_PROMETHEUS} \
      --set mockAgent.enable=${USE_MOCK_AGENT} \
      --set samples.enabled=${DEPLOY_SAMPLES} \
      ${tracing_args}"
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
  if [ "$SCHEDULER_MODE" = "none" ]; then
    step "Phase 2: skipped (SCHEDULER_MODE=none — operator only)"
    return
  fi

  # Extender mode patches the cluster's own kube-scheduler via hook Jobs, so
  # there is no scheduler pod to point anywhere and no webhook to enable —
  # the chart's Jobs resolve the PlacementServer by label themselves.
  if [ "$SCHEDULER_MODE" = "extender" ]; then
    step "Phase 2: enabling scheduler extender"
    make -C "$REPO_ROOT" helm-platform-deploy \
      HELM_NAMESPACE="$HELM_NAMESPACE" \
      HELM_PLATFORM_RELEASE="$HELM_PLATFORM_RELEASE" \
      HELM_EXTRA_ARGS="\
        --reuse-values \
        --set scheduler.mode=extender"
    return
  fi

  step "Phase 2: enabling scheduler plugin + webhook"

  local tracing_args=""
  if [ -n "$OTLP_ENDPOINT" ]; then
    tracing_args="--set scheduler.plugin.tracing.endpoint=${OTLP_ENDPOINT} \
      --set scheduler.plugin.tracing.serviceName=${SCHED_NAME}"
  fi

  # {a,b} replaces the whole list, where [0]= would merge into index 0 and
  # leave any other entries behind. Quote-free on purpose: HELM_EXTRA_ARGS is
  # re-parsed by the shell in the Makefile recipe, which would eat the quotes
  # --set-json needs and leave invalid JSON.
  local reader_args=""
  if [ -n "$METRICS_READER" ]; then
    reader_args="--set observability.metricsReaders={${METRICS_READER}}"
  fi

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
      --set scheduler.plugin.metrics.port=${SCHEDULER_METRICS_PORT} \
      --set scheduler.plugin.metrics.serviceMonitor.enabled=${DEPLOY_PROMETHEUS} \
      --set webhook.enable=true \
      --set webhook.certManager.enable=true \
      ${tracing_args} ${reader_args}"
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

# Service names are resolved at runtime, never guessed — Helm truncates them
# based on release name and chart version.
svc_by_label() {
  kubectl -n "$HELM_NAMESPACE" get svc -l "$1" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true
}

# This Service carries no component label, only control-plane=controller-manager,
# which the placement Service shares — the port is what tells them apart.
operator_metrics_svc_name() {
  kubectl -n "$HELM_NAMESPACE" get svc -l "$OPERATOR_LABEL_SELECTOR" \
    -o jsonpath="{.items[?(@.spec.ports[0].port==${OPERATOR_METRICS_PORT})].metadata.name}" \
    2>/dev/null || true
}

operator_sa_name() {
  kubectl -n "$HELM_NAMESPACE" get deploy -l "$OPERATOR_LABEL_SELECTOR" \
    -o jsonpath='{.items[0].spec.template.spec.serviceAccountName}' 2>/dev/null || true
}

# Which ServiceAccount the chart should grant /metrics read access to. Bundled
# Prometheus grants itself, so nothing is needed there.
resolve_metrics_reader() {
  if [ "$DEPLOY_PROMETHEUS" = "true" ] && [ -z "$METRICS_READER_SA" ]; then
    METRICS_READER=""
    return
  fi

  if [ -n "$METRICS_READER_SA" ]; then
    METRICS_READER="$METRICS_READER_SA"
    local ns="${METRICS_READER_SA%%:*}" name="${METRICS_READER_SA##*:}"
    # A binding to a subject that doesn't exist yet is legal and inert, so this
    # warns rather than failing — the SA may be created after this release.
    if ! kubectl -n "$ns" get sa "$name" >/dev/null 2>&1; then
      echo "  WARNING: ServiceAccount $METRICS_READER_SA not found — the grant stays inert until it exists."
    fi
    return
  fi

  local sa
  sa="$(operator_sa_name)"
  METRICS_READER="${sa:+${HELM_NAMESPACE}:${sa}}"
}

print_summary() {
  echo ""
  print_banner "Verification complete." 32
  echo ""
  echo -e "\033[32m  ── Components ────────────────────────────────────────────\033[0m"
  echo -e "\033[32m    Operator          : deployed  (namespace/$HELM_NAMESPACE)\033[0m"
  echo -e "\033[32m      ├─ OrchestrationProfile controller\033[0m"
  echo -e "\033[32m      ├─ PlacementServer (AI node scoring, :8090)\033[0m"
  echo -e "\033[32m      └─ Rebalance Engine (hybrid trigger detection)\033[0m"
  case "$SCHEDULER_MODE" in
    plugin)
      echo -e "\033[32m    Scheduler plugin  : deployed  (schedulerName: $SCHED_NAME)\033[0m"
      echo -e "\033[32m    Webhook           : deployed  (TLS via cert-manager)\033[0m"
      ;;
    extender)
      echo -e "\033[32m    Scheduler extender: deployed  (patched kube-scheduler, all pods)\033[0m"
      echo -e "\033[33m    Webhook           : not deployed  (plugin mode only)\033[0m"
      ;;
    none)
      echo -e "\033[33m    Scheduler         : not deployed  (SCHEDULER_MODE=none)\033[0m"
      echo -e "\033[33m    Webhook           : not deployed  (plugin mode only)\033[0m"
      ;;
  esac
  echo -e "\033[32m    PlacementServer   : healthy\033[0m"
  if [ "$USE_MOCK_AGENT" = "true" ]; then
    echo -e "\033[32m    Mock agent        : deployed  (mock-decision-agent.$HELM_NAMESPACE)\033[0m"
  else
    echo -e "\033[33m    Mock agent        : not deployed  (using real agent)\033[0m"
  fi
  if [ "$DEPLOY_SAMPLES" = "true" ]; then
    echo -e "\033[32m    Samples           : deployed  (nginx + OrchestrationProfiles)\033[0m"
  else
    echo -e "\033[33m    Samples           : not deployed  (no workload to score)\033[0m"
  fi
  if [ "$DEPLOY_PROMETHEUS" = "true" ] && [ "$SCHEDULER_MODE" = "plugin" ]; then
    echo -e "\033[32m    Prometheus        : deployed  (scraping hiro_* via both ServiceMonitors)\033[0m"
  elif [ "$DEPLOY_PROMETHEUS" = "true" ]; then
    echo -e "\033[32m    Prometheus        : deployed  (scraping the operator; no scheduler pod)\033[0m"
  else
    echo -e "\033[33m    Prometheus        : not deployed  (hiro_* exposed but unscraped)\033[0m"
  fi
  if [ -n "$OTLP_ENDPOINT" ]; then
    echo -e "\033[32m    Tracing           : on  ($OTLP_ENDPOINT)\033[0m"
  else
    echo -e "\033[33m    Tracing           : off  (set OTLP_ENDPOINT to enable)\033[0m"
  fi
  echo ""
  echo -e "\033[32m    Kubeconfig        : $KIND_KUBECONFIG_PATH\033[0m"
  echo -e "\033[32m    (or: kubectl config use-context kind-${KIND_CLUSTER_NAME})\033[0m"
  echo ""

  step "Pods in $HELM_NAMESPACE"
  kubectl -n "$HELM_NAMESPACE" get pods
}

# Local ports differ from remote ones so a local process already bound to the
# remote port's number doesn't collide.
print_port_forward_guide() {
  local placement_svc operator_metrics_svc scheduler_metrics_svc
  placement_svc="$(svc_by_label "$PLACEMENT_SERVER_LABEL_SELECTOR")"
  operator_metrics_svc="$(operator_metrics_svc_name)"
  scheduler_metrics_svc="$(svc_by_label "$SCHEDULER_METRICS_LABEL_SELECTOR")"

  echo ""
  print_banner "Port-forwarding (run manually)" 36
  echo ""
  echo "  # 1. Kill any existing port-forwards for these services:"
  [ -n "$placement_svc" ] && echo "  pkill -f 'port-forward.*svc/$placement_svc' || true"
  [ -n "$operator_metrics_svc" ] && echo "  pkill -f 'port-forward.*svc/$operator_metrics_svc' || true"
  [ -n "$scheduler_metrics_svc" ] && echo "  pkill -f 'port-forward.*svc/$scheduler_metrics_svc' || true"
  [ "$USE_MOCK_AGENT" = "true" ] && echo "  pkill -f 'port-forward.*svc/mock-decision-agent' || true"
  echo ""
  echo "  # 2. Start the port-forwards:"
  [ -n "$placement_svc" ] && echo "  kubectl port-forward -n $HELM_NAMESPACE svc/$placement_svc 18090:8090 &"
  [ -n "$operator_metrics_svc" ] && echo "  kubectl port-forward -n $HELM_NAMESPACE svc/$operator_metrics_svc 18443:${OPERATOR_METRICS_PORT} &"
  [ -n "$scheduler_metrics_svc" ] && echo "  kubectl port-forward -n $HELM_NAMESPACE svc/$scheduler_metrics_svc 20259:${SCHEDULER_METRICS_PORT} &"
  [ "$USE_MOCK_AGENT" = "true" ] && echo "  kubectl port-forward -n $HELM_NAMESPACE svc/mock-decision-agent 18080:8080 &"
  echo ""
}

print_access_urls() {
  local operator_metrics_svc scheduler_metrics_svc sa
  operator_metrics_svc="$(operator_metrics_svc_name)"
  scheduler_metrics_svc="$(svc_by_label "$SCHEDULER_METRICS_LABEL_SELECTOR")"
  sa="$(operator_sa_name)"

  print_banner "Access URLs (after port-forwarding)" 36

  print_subhead "Open directly"
  printf "      %-24s %s\n" "PlacementServer health" "http://localhost:18090/healthz"
  if [ "$USE_MOCK_AGENT" = "true" ]; then
    printf "      %-24s %s\n" "Mock decision agent" "http://localhost:18080"
  fi

  print_subhead "Metrics — HTTPS, bearer token required"
  [ -n "$operator_metrics_svc" ] && \
    printf "      %-24s %s\n" "Operator" "https://localhost:18443/metrics"
  [ -n "$scheduler_metrics_svc" ] && \
    printf "      %-24s %s\n" "Scheduler plugin" "https://localhost:20259/metrics"
  echo ""

  # No reader was granted, so a curl would 403 — print how to get one rather
  # than a command that fails.
  if [ -z "$METRICS_READER" ]; then
    echo "      Prometheus scrapes both already. To curl them by hand, grant"
    echo "      yourself a reader first:"
    echo ""
    echo "        helm upgrade $HELM_PLATFORM_RELEASE charts/hiro-adaptive-platform \\"
    echo "          -n $HELM_NAMESPACE --reuse-values \\"
    echo "          --set observability.metricsReaders={${HELM_NAMESPACE}:${sa}}"
    echo ""
    return
  fi

  # Token minted once into a variable, so each curl stays a single
  # copy-pasteable line instead of wrapping.
  if [ -n "$sa" ]; then
    echo "        TOKEN=\$(kubectl create token $sa -n $HELM_NAMESPACE)"
    [ -n "$operator_metrics_svc" ] && \
      echo "        curl -sk -H \"Authorization: Bearer \$TOKEN\" https://localhost:18443/metrics | grep '^hiro_'"
    [ -n "$scheduler_metrics_svc" ] && \
      echo "        curl -sk -H \"Authorization: Bearer \$TOKEN\" https://localhost:20259/metrics | grep '^hiro_scheduler_plugin_'"
  fi

  print_subhead "Note"
  echo "      Counters only appear once something exercises them."
  if [ "$DEPLOY_SAMPLES" = "true" ]; then
    echo "      Samples are deployed, so placement and controller metrics"
    echo "      populate on their own; rebalance needs a trigger to fire."
  else
    echo "      Nothing is scheduled yet (DEPLOY_SAMPLES=false), so expect"
    echo "      empty output until you apply a workload with a profile."
  fi
  echo "      See internal/metrics/README.md."
  echo ""
}

print_teardown_hint() {
  print_subhead "Teardown"
  echo "      Release only, keeps the cluster:"
  echo "        hack/deploy_full_stack_with_helm.sh clean"
  echo ""
  echo "      Delete the Kind cluster:"
  echo "        KIND_CLUSTER_NAME=$KIND_CLUSTER_NAME hack/deploy_full_stack_with_helm.sh destroy"
  echo ""
}

# Only prints what is actually off — nothing to act on when both are already on.
print_scrape_and_tracing_guide() {
  if [ "$DEPLOY_PROMETHEUS" = "true" ] && [ -n "$OTLP_ENDPOINT" ]; then
    print_teardown_hint
    return
  fi

  print_banner "Turning on what's currently off" 36

  if [ "$DEPLOY_PROMETHEUS" != "true" ]; then
    print_subhead "Scraping — metrics are exposed but nothing collects them"
    echo "      Bundled kube-prometheus-stack:"
    echo "        DEPLOY_PROMETHEUS=true hack/deploy_full_stack_with_helm.sh deploy"
    echo ""
    echo "      Or your own Prometheus:"
    echo "        METRICS_READER_SA=<ns>:<prometheus-sa> \\"
    echo "          hack/deploy_full_stack_with_helm.sh deploy"
    echo ""
    echo "        then point its serviceMonitorNamespaceSelector at '$HELM_NAMESPACE'."
    if [ "$SCHEDULER_MODE" = "plugin" ]; then
      echo ""
      echo "      The operator's ServiceMonitor selects control-plane=controller-manager,"
      echo "      which misses the scheduler pod — turn its own one on too:"
      echo "        helm upgrade $HELM_PLATFORM_RELEASE charts/hiro-adaptive-platform \\"
      echo "          -n $HELM_NAMESPACE --reuse-values \\"
      echo "          --set scheduler.plugin.metrics.serviceMonitor.enabled=true"
    fi
    echo ""
  fi

  if [ -z "$OTLP_ENDPOINT" ]; then
    print_subhead "Tracing — no spans are exported"
    echo "      Point at an OTLP gRPC collector (Alloy or Tempo):"
    echo "        OTLP_ENDPOINT=alloy.observability.svc:4317 \\"
    echo "          hack/deploy_full_stack_with_helm.sh deploy"
    echo ""
    if [ "$SCHEDULER_MODE" = "plugin" ]; then
      echo "      Sets it on both the operator and the scheduler, so one trace"
      echo "      spans scheduler -> PlacementServer -> AI agent."
    else
      echo "      Sets it on the operator. Only plugin mode has a scheduler"
      echo "      process of its own to trace."
    fi
    echo ""
  fi

  print_teardown_hint
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
  resolve_metrics_reader
  deploy_scheduler_phase
  check_placement_server_health
  print_summary
  print_port_forward_guide
  print_access_urls
  print_scrape_and_tracing_guide
}

# ---------------------------------------------------------------------------
# Steps — clean / destroy
# ---------------------------------------------------------------------------

run_clean() {
  if ! helm status "$HELM_PLATFORM_RELEASE" -n "$HELM_NAMESPACE" >/dev/null 2>&1; then
    step "No release '$HELM_PLATFORM_RELEASE' in $HELM_NAMESPACE — nothing to uninstall"
    return
  fi

  step "Uninstalling Helm release: $HELM_PLATFORM_RELEASE"
  make -C "$REPO_ROOT" helm-platform-uninstall \
    HELM_NAMESPACE="$HELM_NAMESPACE" \
    HELM_PLATFORM_RELEASE="$HELM_PLATFORM_RELEASE"

  echo ""
  echo "  Kind cluster '$KIND_CLUSTER_NAME' is still running — redeploy with:"
  echo "       KIND_CLUSTER_NAME=$KIND_CLUSTER_NAME hack/deploy_full_stack_with_helm.sh deploy"
  echo "  To delete the cluster too: hack/deploy_full_stack_with_helm.sh destroy"
  echo ""
}

run_destroy() {
  step "Deleting Kind cluster: $KIND_CLUSTER_NAME"
  kind delete cluster --name "$KIND_CLUSTER_NAME"
  rm -f "$KIND_KUBECONFIG_PATH"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  validate_config
  print_config
  case "$ACTION" in
    deploy)  run_deploy ;;
    clean)   run_clean ;;
    destroy) run_destroy ;;
    *)
      echo "ERROR: unknown action '$ACTION' — expected 'deploy', 'clean', or 'destroy'." >&2
      exit 1
      ;;
  esac
}

main
