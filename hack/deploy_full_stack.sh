#!/bin/bash
# hack/deploy_full_stack.sh
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
#   PLACEMENT_SCORE_PATH     decision endpoint path      (default: /api/v1/placement/score)
#   PLACEMENT_SERVER_HEALTH_PATH  health endpoint path    (default: /healthz)
#   PLACEMENT_TIMEOUT_SECS    scheduler→server timeout    (default: 8)
#
# ─── Operator — Decision Agent ───────────────────────────────────────────────
#   USE_MOCK_AGENT            true|false                  (default: true)
#                             When true, deploys hack/mock_decision_agent.yaml
#                             and sets DECISION_AGENT_URL automatically.
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
# ─── metrics-server (rebalance engine CPU/Memory triggers) ───────────────────
#   INSTALL_METRICS_SERVER     true|false                  (default: true)
#                              Idempotent — skips if already installed.
#                              See hack/install_metrics_server.sh.
#   METRICS_SERVER_VERSION     release tag to install       (default: latest)
#   METRICS_SERVER_INSECURE_TLS true|false                  (default: true)
#                              Needed on kind/minikube/k3d; set false on
#                              real clusters with valid kubelet certs.
#
# ─── Rebalance Engine ─────────────────────────────────────────────────────────
#   REBALANCE_MAX_RECENT_DECISIONS      decision-history length per profile (default: 10)
#   REBALANCE_DETECTION_INTERVAL        periodic detection tick, Go duration (default: 30s)
#   REBALANCE_DECISION_TIMEOUT          AI-consultation timeout, Go duration (default: 5s)
#   REBALANCE_NODE_PRESSURE_THRESHOLD   CPU/Memory pressure fraction         (default: 0.90)
#   REBALANCE_IMPROVEMENT_THRESHOLD     min Move improvement score to enact  (default: 20)
#   REBALANCE_DECISION_STORE_TTL        how long a Move decision biases scoring, Go duration (default: 60s)
#   REBALANCE_MOVE_ACTION_TIMEOUT       max wait for a Move's replacement pod, Go duration (default: 60s)
#
# ─── Scheduler ───────────────────────────────────────────────────────────────
#   SCHED_K8S_VERSION         k8s version to build for    (default: v1.35.0)
#   SCHED_VERSION             scheduler release version   (default: v0.1.0)
#   HIRO_SCHEDULER_NAME       name of the custom scheduler (default: hiro-scheduler)
#                             Injected by the webhook into spec.schedulerName.
#                             Must match the scheduler Deployment in config/scheduler/.
#
# ─── Webhook TLS (Phase 8 — pod scheduler MutatingAdmissionWebhook) ──────────
#   When DEPLOY_SCHEDULER_PLUGIN=true, Phase 8 deploys the webhook that
#   automatically sets spec.schedulerName = HIRO_SCHEDULER_NAME on pods governed
#   by an OrchestrationProfile. TLS is required for Kubernetes admission webhooks.
#
#   This project uses cert-manager (free, open-source — CNCF project).
#   Phase 2 installs cert-manager (idempotent) and waits for it to be ready.
#   Phase 4 (make deploy) applies Issuer + Certificate via config/default.
#   Phase 8 waits for the cert to be issued, enables the webhook, and restarts.
#
#   CERT_MANAGER_VERSION      cert-manager version to install (default: v1.17.2)
#   WEBHOOK_EXCLUDE_NAMESPACES  comma-separated namespaces the webhook will NOT intercept
#                               (default: kube-system,kube-public,kube-node-lease)
#                               Phase 8 patches the MWC with this list at deploy time.
#                               Always keep system namespaces in any custom list.
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
#   # Operator + mock agent (default)
#   hack/deploy_full_stack.sh [kubeconfig-path]
#
#   # Operator + custom scheduler plugin
#   DEPLOY_SCHEDULER_PLUGIN=true hack/deploy_full_stack.sh [kubeconfig-path]
#
#   # Operator + extender (patches default kube-scheduler)
#   DEPLOY_EXTENDER=true hack/deploy_full_stack.sh [kubeconfig-path]
#
#   # Real AI agent, custom namespace, scheduler plugin
#   USE_MOCK_AGENT=false DECISION_AGENT_URL=http://ai.example.com:8080 \
#     NAMESPACE=my-ns DEPLOY_SCHEDULER_PLUGIN=true \
#     hack/deploy_full_stack.sh [kubeconfig-path]

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
# PlacementServer — operator exposes it, scheduler and extender call it.
# PLACEMENT_SERVICE_NAME must be derived AFTER NAME_PREFIX is resolved.
# ---------------------------------------------------------------------------

export PLACEMENT_SERVICE_NAME=${PLACEMENT_SERVICE_NAME:-${NAME_PREFIX}controller-manager-placement-service}
export PLACEMENT_SERVER_PORT=${PLACEMENT_SERVER_PORT:-:8090}
export PLACEMENT_SCORE_PATH=${PLACEMENT_SCORE_PATH:-/api/v1/placement/score}
export PLACEMENT_FILTER_PATH=${PLACEMENT_FILTER_PATH:-/api/v1/placement/filter}
export PLACEMENT_SERVER_HEALTH_PATH=${PLACEMENT_SERVER_HEALTH_PATH:-/healthz}
export PLACEMENT_TIMEOUT_SECS=${PLACEMENT_TIMEOUT_SECS:-8}

# ---------------------------------------------------------------------------
# Decision Agent
# ---------------------------------------------------------------------------

export USE_MOCK_AGENT=${USE_MOCK_AGENT:-true}
# DECISION_AGENT_URL is set automatically by deploy_operator.sh when USE_MOCK_AGENT=true.
# Set it here only when USE_MOCK_AGENT=false.
if [ "$USE_MOCK_AGENT" = "false" ]; then
  : "${DECISION_AGENT_URL:?DECISION_AGENT_URL must be set when USE_MOCK_AGENT=false}"
  export DECISION_AGENT_URL
fi
export DECISION_AGENT_PATH=${DECISION_AGENT_PATH:-/api/v1/agent/placement/decision}

# ---------------------------------------------------------------------------
# Extender paths
# ---------------------------------------------------------------------------

export EXTENDER_FILTER_PATH=${EXTENDER_FILTER_PATH:-/extender/filter}
export EXTENDER_PRIORITIZE_PATH=${EXTENDER_PRIORITIZE_PATH:-/extender/prioritize}

# ---------------------------------------------------------------------------
# EnergyAwareOrchestration CRD coordinates
# ---------------------------------------------------------------------------

export EAO_GROUP=${EAO_GROUP:-eas.hiro.io}
export EAO_VERSION=${EAO_VERSION:-v1}
export EAO_KIND=${EAO_KIND:-EnergyAwareOrchestration}

# ---------------------------------------------------------------------------
# Scheduler
# ---------------------------------------------------------------------------

export SCHED_K8S_VERSION=${SCHED_K8S_VERSION:-v1.35.0}
export SCHED_VERSION=${SCHED_VERSION:-v0.1.0}
export HIRO_SCHEDULER_NAME=${HIRO_SCHEDULER_NAME:-hiro-scheduler}
export CERT_MANAGER_VERSION=${CERT_MANAGER_VERSION:-v1.17.2}
# Namespaces the webhook will NOT intercept (applied by Phase 8 kubectl patch).
# Kustomize applies the same defaults statically; this allows deploy-time override.
WEBHOOK_EXCLUDE_NAMESPACES=${WEBHOOK_EXCLUDE_NAMESPACES:-kube-system,kube-public,kube-node-lease}

# ---------------------------------------------------------------------------
# metrics-server — powers the rebalance engine's CPU/Memory triggers.
# See hack/install_metrics_server.sh.
# ---------------------------------------------------------------------------

export INSTALL_METRICS_SERVER=${INSTALL_METRICS_SERVER:-true}
export METRICS_SERVER_VERSION=${METRICS_SERVER_VERSION:-latest}
export METRICS_SERVER_INSECURE_TLS=${METRICS_SERVER_INSECURE_TLS:-true}

# ---------------------------------------------------------------------------
# Rebalance Engine — all optional, mirroring the operator's own package
# defaults (internal/rebalance). See internal/rebalance/README.md#configuration.
# ---------------------------------------------------------------------------

export REBALANCE_MAX_RECENT_DECISIONS=${REBALANCE_MAX_RECENT_DECISIONS:-10}
export REBALANCE_DETECTION_INTERVAL=${REBALANCE_DETECTION_INTERVAL:-30s}
export REBALANCE_DECISION_TIMEOUT=${REBALANCE_DECISION_TIMEOUT:-5s}
export REBALANCE_NODE_PRESSURE_THRESHOLD=${REBALANCE_NODE_PRESSURE_THRESHOLD:-0.90}
export REBALANCE_IMPROVEMENT_THRESHOLD=${REBALANCE_IMPROVEMENT_THRESHOLD:-20}
export REBALANCE_DECISION_STORE_TTL=${REBALANCE_DECISION_STORE_TTL:-60s}
export REBALANCE_MOVE_ACTION_TIMEOUT=${REBALANCE_MOVE_ACTION_TIMEOUT:-60s}

# ---------------------------------------------------------------------------
# Deploy options — consumed by this script only
# ---------------------------------------------------------------------------

DEPLOY_SCHEDULER_PLUGIN=${DEPLOY_SCHEDULER_PLUGIN:-false}
DEPLOY_EXTENDER=${DEPLOY_EXTENDER:-false}

# ---------------------------------------------------------------------------
# Input validation
# ---------------------------------------------------------------------------

validate_inputs() {
  local errors=0

  if [ -z "${GITHUB_PAT_TOKEN:-}" ]; then
    echo "ERROR: GITHUB_PAT_TOKEN is required but not set." >&2
    echo "       export GITHUB_PAT_TOKEN=<github-pat-with-write:packages-scope>" >&2
    errors=1
  fi

  if [ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ] && [ "$DEPLOY_EXTENDER" = "true" ]; then
    echo "ERROR: DEPLOY_SCHEDULER_PLUGIN and DEPLOY_EXTENDER cannot both be true." >&2
    echo "       Choose one scheduler integration approach:" >&2
    echo "         DEPLOY_SCHEDULER_PLUGIN=true  — custom scheduler pod (opt-in per pod)" >&2
    echo "         DEPLOY_EXTENDER=true          — patches default kube-scheduler (cluster-wide)" >&2
    errors=1
  fi

  if [ "$errors" -ne 0 ]; then
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n\033[36m===>\033[0m %s\n' "$*"; }

sep()  { echo "  ──────────────────────────────────────────────────────────"; }

# ---------------------------------------------------------------------------
# Config summary
# ---------------------------------------------------------------------------

print_config() {
  echo ""
  echo "  ╔══════════════════════════════════════════════════════════╗"
  echo "  ║           HIRO Full-Stack Deploy — Configuration         ║"
  echo "  ╚══════════════════════════════════════════════════════════╝"
  echo ""
  echo "  ── Operator (one Deployment bundles all four) ───────────"
  echo "    1. OrchestrationProfile controller — CR reconciliation, PlacementStatus"
  echo "    2. PlacementServer                 — AI node scoring energy gate (:8090)"
  echo "    3. Rebalance Engine                — hybrid trigger detection → decision lifecycle state machine"
  echo "    4. Pod scheduler webhook           — opt-in, auto-sets schedulerName (DEPLOY_SCHEDULER_PLUGIN=true)"
  echo ""
  echo "  ── Identity ──────────────────────────────────────────────"
  echo "    Namespace              : $NAMESPACE"
  echo "    Name Prefix            : $NAME_PREFIX"
  echo "    Kubeconfig             : $KUBECONFIG"
  echo ""
  echo "  ── PlacementServer ───────────────────────────────────────"
  echo "    Service Name           : $PLACEMENT_SERVICE_NAME"
  echo "    Port                   : $PLACEMENT_SERVER_PORT"
  echo "    Decision Path          : $PLACEMENT_SCORE_PATH"
  echo "    Health Path            : $PLACEMENT_SERVER_HEALTH_PATH"
  echo "    Scheduler Timeout (s)  : $PLACEMENT_TIMEOUT_SECS"
  echo ""
  echo "  ── Decision Agent ────────────────────────────────────────"
  if [ "$USE_MOCK_AGENT" = "true" ]; then
  echo "    Mode                   : MOCK  (hack/mock_decision_agent.yaml)"
  else
  echo "    Mode                   : REAL"
  echo "    URL                    : $DECISION_AGENT_URL"
  fi
  echo "    Agent Path             : $DECISION_AGENT_PATH"
  echo ""
  echo "  ── Extender Paths ────────────────────────────────────────"
  echo "    Filter                 : $EXTENDER_FILTER_PATH"
  echo "    Prioritize             : $EXTENDER_PRIORITIZE_PATH"
  echo ""
  echo "  ── EAO CRD ───────────────────────────────────────────────"
  echo "    Group/Version          : $EAO_GROUP/$EAO_VERSION"
  echo "    Kind                   : $EAO_KIND"
  echo ""
  echo "  ── metrics-server ────────────────────────────────────────"
  echo "    Install                : $INSTALL_METRICS_SERVER  (version: $METRICS_SERVER_VERSION)"
  echo "    Insecure kubelet TLS   : $METRICS_SERVER_INSECURE_TLS"
  echo ""
  echo "  ── Rebalance Engine ──────────────────────────────────────"
  echo "    Max Recent Decisions   : $REBALANCE_MAX_RECENT_DECISIONS"
  echo "    Detection Interval     : $REBALANCE_DETECTION_INTERVAL"
  echo "    Decision Timeout       : $REBALANCE_DECISION_TIMEOUT"
  echo "    Node Pressure Threshold: $REBALANCE_NODE_PRESSURE_THRESHOLD"
  echo "    Improvement Threshold  : $REBALANCE_IMPROVEMENT_THRESHOLD"
  echo "    Decision Store TTL     : $REBALANCE_DECISION_STORE_TTL"
  echo "    Move Action Timeout    : $REBALANCE_MOVE_ACTION_TIMEOUT"
  echo ""
  echo "  ── Scheduler ─────────────────────────────────────────────"
  echo "    K8s Target Version     : $SCHED_K8S_VERSION"
  echo "    Scheduler Version      : $SCHED_VERSION"
  echo "    Scheduler Name         : $HIRO_SCHEDULER_NAME"
  echo ""
  echo "  ── Webhook TLS ───────────────────────────────────────────"
  echo "    TLS Provider           : cert-manager ${CERT_MANAGER_VERSION}"
  echo "    Webhook enabled        : $DEPLOY_SCHEDULER_PLUGIN  (Phase 8)"
  echo "    Excluded Namespaces    : ${WEBHOOK_EXCLUDE_NAMESPACES}"
  echo ""
  echo "  ── Deploy Options ────────────────────────────────────────"
  echo "    Mock Agent             : $USE_MOCK_AGENT"
  echo "    Scheduler Plugin       : $DEPLOY_SCHEDULER_PLUGIN"
  echo "    Extender               : $DEPLOY_EXTENDER"
  echo ""
}

# ---------------------------------------------------------------------------
# Phase 3 — Cleanup
#
# Detects and tears down the opposing scheduler integration so the cluster is
# never in a mixed state.  At most one of (plugin, extender) is active at a time.
# ---------------------------------------------------------------------------

uninstall_extender_if_running() {
  if ! kubectl get configmap hiro-scheduler-config -n kube-system &>/dev/null 2>&1; then
    echo "  No running extender detected — nothing to remove."
    return
  fi
  echo "  Detected running extender — removing it..."
  bash "$SCRIPT_DIR/undeploy_extender.sh" "$KUBECONFIG_PATH"
  echo "  Extender removed."
}

uninstall_scheduler_plugin_if_running() {
  if ! kubectl get deployment "${NAME_PREFIX}hiro-scheduler" -n "$NAMESPACE" &>/dev/null 2>&1; then
    echo "  No running scheduler plugin detected — nothing to remove."
    return
  fi
  echo "  Detected running scheduler plugin — removing it..."

  local kustomize_bin="$REPO_ROOT/bin/kustomize"

  # Configure kustomize with current namespace/prefix so delete targets the right names.
  (cd "$REPO_ROOT/config/scheduler" && "$kustomize_bin" edit set namespace "$NAMESPACE")
  (cd "$REPO_ROOT/config/scheduler" && "$kustomize_bin" edit set nameprefix "$NAME_PREFIX")

  # Delete all scheduler plugin resources (Deployment, SA, ClusterRole, ConfigMap …).
  "$kustomize_bin" build "$REPO_ROOT/config/scheduler/" \
    | kubectl delete --ignore-not-found=true -f - || true

  # MWC is cluster-scoped and lives in the webhook overlay — delete separately.
  kubectl delete mutatingwebhookconfiguration \
    "${NAME_PREFIX}mutating-webhook-configuration" --ignore-not-found || true

  # Flip ENABLE_WEBHOOKS=false on the operator and wait for the rollout.
  local operator="${NAME_PREFIX}controller-manager"
  if kubectl get deployment "$operator" -n "$NAMESPACE" &>/dev/null 2>&1; then
    kubectl set env deployment/"$operator" -n "$NAMESPACE" ENABLE_WEBHOOKS=false || true
    kubectl rollout restart deployment/"$operator" -n "$NAMESPACE" || true
    kubectl rollout status deployment/"$operator" -n "$NAMESPACE" --timeout=120s || true
  fi

  echo "  Scheduler plugin removed."
}

cleanup_conflicting_integration() {
  step "Phase 3 — Cleanup: removing conflicting scheduler integration if any..."
  if [ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ]; then
    uninstall_extender_if_running
  elif [ "$DEPLOY_EXTENDER" = "true" ]; then
    uninstall_scheduler_plugin_if_running
  else
    echo "  No scheduler integration active — nothing to clean up."
  fi
}

# ---------------------------------------------------------------------------
# Phase 1 — Pre-flight checks
#
# Verifies the local environment and cluster are ready before any deployment
# work begins.  Fails fast with a clear message so nothing is deployed into
# a broken environment.
#
# Checks (in order):
#   1. Required binaries (kubectl, docker) are in PATH
#   2. Docker daemon is running
#   3. Kubeconfig file exists on disk
#   4. Kubernetes cluster is reachable
# ---------------------------------------------------------------------------

check_prerequisites() {
  step "Phase 1 — Pre-flight checks..."

  local errors=0

  # ── Required binaries ──────────────────────────────────────────────────
  for cmd in kubectl docker; do
    if ! command -v "$cmd" &>/dev/null; then
      echo "  ERROR: '$cmd' not found in PATH." >&2
      errors=1
    else
      echo "  [ok] $cmd  →  $(command -v "$cmd")"
    fi
  done

  # ── Docker daemon ──────────────────────────────────────────────────────
  if command -v docker &>/dev/null; then
    if ! docker info &>/dev/null 2>&1; then
      echo "  ERROR: Docker daemon is not running. Start Docker Desktop or the Docker service." >&2
      errors=1
    else
      echo "  [ok] Docker daemon is running"
    fi
  fi

  # ── Kubeconfig ─────────────────────────────────────────────────────────
  if [ ! -f "$KUBECONFIG_PATH" ]; then
    echo "  ERROR: Kubeconfig not found: $KUBECONFIG_PATH" >&2
    errors=1
  else
    echo "  [ok] Kubeconfig found: $KUBECONFIG_PATH"
  fi

  # ── Cluster reachability ───────────────────────────────────────────────
  if [ "$errors" -eq 0 ]; then
    echo "  Checking cluster reachability..."
    if ! kubectl cluster-info --request-timeout=10s &>/dev/null 2>&1; then
      echo "  ERROR: Cannot reach the Kubernetes cluster." >&2
      echo "         Verify your kubeconfig and that the cluster is running:" >&2
      echo "           kubectl cluster-info --kubeconfig=$KUBECONFIG_PATH" >&2
      errors=1
    else
      local server ctx
      server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo "unknown")
      ctx=$(kubectl config current-context 2>/dev/null || echo "unknown")
      echo "  [ok] Cluster reachable: $server"
      echo "  [ok] Current context  : $ctx"
    fi
  fi

  if [ "$errors" -ne 0 ]; then
    echo "" >&2
    echo "  Pre-flight checks failed. Fix the errors above and retry." >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Phase 2 — Install prerequisites
#
# Installs cluster-level tools required before the main kustomize apply.
# Add new prerequisites here as the stack grows.
#
# Current prerequisites:
#   cert-manager    (conditional) — required when DEPLOY_SCHEDULER_PLUGIN=true
#                   because config/default-with-webhook/kustomization.yaml
#                   includes ../certmanager (Issuer + Certificate CRDs) and
#                   they must exist before kustomize build | kubectl apply runs.
#   metrics-server  (default on)  — required for the rebalance engine's
#                   CPUThreshold/MemoryThreshold trigger conditions
#                   (internal/rebalance/pressure.go). Everything else works
#                   fine without it; those two conditions just never fire.
#                   See hack/install_metrics_server.sh.
# ---------------------------------------------------------------------------

install_prerequisites() {
  step "Phase 2 — Installing prerequisites..."

  # ── cert-manager ────────────────────────────────────────────────────────
  # Required when the scheduler webhook is enabled (includes cert-manager
  # Issuer + Certificate resources in the main kustomize deploy).
  if [ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ]; then
    echo "  [cert-manager] DEPLOY_SCHEDULER_PLUGIN=true — installing cert-manager ${CERT_MANAGER_VERSION}..."

    local cm_url="https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"

    if kubectl get deployment cert-manager -n cert-manager &>/dev/null; then
      local installed
      installed=$(kubectl get deployment cert-manager -n cert-manager \
        -o jsonpath='{.metadata.labels.app\.kubernetes\.io/version}' 2>/dev/null || echo "unknown")
      echo "  [cert-manager] Already installed (version: ${installed}). Skipping."
    else
      kubectl apply -f "${cm_url}"
      echo "  [cert-manager] Applied."
    fi

    kubectl wait deployment/cert-manager \
      -n cert-manager --for=condition=Available --timeout=120s
    kubectl wait deployment/cert-manager-webhook \
      -n cert-manager --for=condition=Available --timeout=120s
    kubectl wait deployment/cert-manager-cainjector \
      -n cert-manager --for=condition=Available --timeout=120s
    echo "  [cert-manager] Ready."
  else
    echo "  [cert-manager] Skipped (DEPLOY_SCHEDULER_PLUGIN=false)."
  fi

  # ── metrics-server ──────────────────────────────────────────────────────
  # Powers the rebalance engine's CPUThreshold/MemoryThreshold triggers.
  # Idempotent — hack/install_metrics_server.sh skips if already installed.
  if [ "$INSTALL_METRICS_SERVER" = "true" ]; then
    echo "  [metrics-server] INSTALL_METRICS_SERVER=true — checking/installing..."
    bash "$SCRIPT_DIR/install_metrics_server.sh" "$KUBECONFIG_PATH"
  else
    echo "  [metrics-server] Skipped (INSTALL_METRICS_SERVER=false) — CPU/Memory"
    echo "                   rebalance triggers will never fire until it's installed."
  fi

  # ── Add future prerequisites here ───────────────────────────────────────
}

# ---------------------------------------------------------------------------
# Phase 4 — Operator
# ---------------------------------------------------------------------------

deploy_oprator_with_samples() {
  step "Phase 4 — Deploying operator..."
  bash "$SCRIPT_DIR/deploy_operator.sh" "$KUBECONFIG_PATH"
}

# ---------------------------------------------------------------------------
# Phase 5 — Mock Decision Agent (when USE_MOCK_AGENT=true)
#
# The mock agent is a lightweight Python HTTP server that scores candidate
# nodes randomly for initial placement, and recommends Move/NoOp randomly for
# rebalance evaluations (see hack/mock_decision_agent.yaml for exact
# behaviour). It is deployed in the same namespace as the operator so the
# short DNS name "mock-decision-agent" resolves from the operator pod.
#
# This phase lives in deploy_full_stack.sh (not deploy_operator.sh) so that
# standalone operator deployments are not coupled to the mock agent lifecycle.
# ---------------------------------------------------------------------------

deploy_mock_agent() {
  if [ "$USE_MOCK_AGENT" = "true" ]; then
    step "Phase 5 — Deploying mock decision agent..."
    # Stamping a fresh timestamp into the pod template's redeployed-at
    # annotation on every apply forces a new ReplicaSet even when nothing
    # else changed, so the pod always picks up the ConfigMap's latest script
    # content instead of an already-running process keeping the old one.
    sed -e "s/namespace: hiro-adaptive-orchestrator-system/namespace: $NAMESPACE/g" \
        -e "s/REPLACE_DEPLOY_TIMESTAMP/$(date -u +%Y-%m-%dT%H:%M:%SZ)/g" \
      "$SCRIPT_DIR/mock_decision_agent.yaml" | kubectl apply -f -

    echo "  Waiting for mock decision agent rollout to complete..."
    kubectl rollout status deployment/mock-decision-agent -n "$NAMESPACE" --timeout=120s
    echo "  Mock decision agent is ready."
  else
    step "Phase 5 — Skipping mock agent              (USE_MOCK_AGENT=false)."
    echo "  Decision agent URL : $DECISION_AGENT_URL"
  fi
}

# ---------------------------------------------------------------------------
# Phase 6 — PlacementServer health gate
# ---------------------------------------------------------------------------

wait_for_placement_server() {
  local operator="${NAME_PREFIX}controller-manager"
  step "Phase 6 — Waiting for PlacementServer to be reachable..."
  echo "  Service : ${PLACEMENT_SERVICE_NAME}.${NAMESPACE}.svc.cluster.local${PLACEMENT_SERVER_PORT}"
  echo "  Waiting for operator deployment rollout..."

  kubectl rollout status deployment/"${operator}" \
    -n "$NAMESPACE" \
    --timeout=300s

  echo "  PlacementServer is healthy."
}

# ---------------------------------------------------------------------------
# Phase 7 — Scheduler plugin (opt-in: DEPLOY_SCHEDULER_PLUGIN=true)
# ---------------------------------------------------------------------------

deploy_scheduler_plugin() {
  if [ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ]; then
    step "Phase 7 — Deploying HIRO scheduler plugin..."
    bash "$SCRIPT_DIR/deploy_scheduler.sh" "$KUBECONFIG_PATH"
  else
    step "Phase 7 — Skipping scheduler plugin        (DEPLOY_SCHEDULER_PLUGIN=false)."
    echo "  To deploy: DEPLOY_SCHEDULER_PLUGIN=true hack/deploy_full_stack.sh"
    echo "  Standalone: hack/deploy_scheduler.sh"
  fi
}

# ---------------------------------------------------------------------------
# Webhook namespace exclusions — patches the MutatingWebhookConfiguration so
# the webhook never intercepts pods in the specified namespaces.
#
# Called at the end of Phase 8 AFTER the MWC is live in the cluster.
# The kustomize overlay (mwc_namespace_patch.yaml) applies the same defaults
# statically; this call lets WEBHOOK_EXCLUDE_NAMESPACES override them.
# ---------------------------------------------------------------------------

configure_webhook_namespace_exclusions() {
  local mwc="${NAME_PREFIX}mutating-webhook-configuration"

  # Build a JSON array from the comma-separated namespace list.
  local json_values="["
  IFS=',' read -ra ns_array <<< "$WEBHOOK_EXCLUDE_NAMESPACES"
  for ns in "${ns_array[@]}"; do
    ns="${ns#"${ns%%[![:space:]]*}"}"   # trim leading whitespace
    ns="${ns%"${ns##*[![:space:]]}"}"   # trim trailing whitespace
    [[ -n "$ns" ]] && json_values+="\"$ns\","
  done
  json_values="${json_values%,}]"

  echo "  Configuring webhook namespace exclusions: ${WEBHOOK_EXCLUDE_NAMESPACES}"
  kubectl patch mutatingwebhookconfiguration "$mwc" \
    --type=json \
    -p "[{\"op\":\"replace\",\"path\":\"/webhooks/0/namespaceSelector\",\"value\":{\"matchExpressions\":[{\"key\":\"kubernetes.io/metadata.name\",\"operator\":\"NotIn\",\"values\":${json_values}}]}}]"
  echo "  Webhook will not intercept pods in: ${WEBHOOK_EXCLUDE_NAMESPACES}"
}

# ---------------------------------------------------------------------------
# Phase 8 — Enable pod scheduler webhook (DEPLOY_SCHEDULER_PLUGIN=true only)
#
# By this point Phase 2 has installed cert-manager and Phase 4 (make deploy)
# has applied everything in config/default-with-webhook:
#   - self-signed Issuer + webhook Certificate  (config/certmanager)
#   - webhook Service + MutatingWebhookConfiguration  (config/webhook)
#   - operator Deployment with cert volume (optional) + port 9443
#
# cert-manager starts issuing the TLS cert after Phase 4 applies the
# Certificate resource.  We must wait for the cert Secret before flipping
# ENABLE_WEBHOOKS=true — if the operator starts without certs it crashes.
# ---------------------------------------------------------------------------

deploy_scheduler_webhook() {
  if [ "$DEPLOY_SCHEDULER_PLUGIN" != "true" ]; then
    step "Phase 8 — Skipping webhook                (DEPLOY_SCHEDULER_PLUGIN=false)."
    echo "  Webhook is only deployed with the scheduler plugin."
    return
  fi

  local operator="${NAME_PREFIX}controller-manager"
  local cert="${NAME_PREFIX}serving-cert"

  step "Phase 8 — Enabling pod scheduler webhook..."

  # Wait for cert-manager to issue the TLS certificate before enabling the
  # webhook server — the operator crashes if ENABLE_WEBHOOKS=true and the
  # cert Secret does not exist yet.
  echo "  Waiting for cert-manager to issue certificate '${cert}'..."
  sleep 5
  kubectl wait certificate "${cert}" \
    -n "${NAMESPACE}" \
    --for=condition=Ready \
    --timeout=120s
  echo "  Certificate is Ready."

  # Flip ENABLE_WEBHOOKS=true now that the cert Secret exists.
  # HIRO_SCHEDULER_NAME is already set in config/manager/manager.yaml;
  # passing it here lets the user override it via the env var.
  kubectl set env deployment/"${operator}" \
    -n "${NAMESPACE}" \
    ENABLE_WEBHOOKS=true \
    HIRO_SCHEDULER_NAME="${HIRO_SCHEDULER_NAME}"
  echo "  ENABLE_WEBHOOKS=true set (schedulerName=${HIRO_SCHEDULER_NAME})."

  # Restart so the operator picks up the new env and mounts the cert Secret.
  kubectl rollout restart deployment/"${operator}" -n "${NAMESPACE}"
  kubectl rollout status deployment/"${operator}" \
    -n "${NAMESPACE}" \
    --timeout=120s
  echo "  Webhook enabled and operator is ready."

  configure_webhook_namespace_exclusions
}

# ---------------------------------------------------------------------------
# Phase 9 — Extender (opt-in: DEPLOY_EXTENDER=true)
# ---------------------------------------------------------------------------

deploy_extender() {
  if [ "$DEPLOY_EXTENDER" = "true" ]; then
    step "Phase 9 — Deploying extender (patches kube-scheduler)..."
    bash "$SCRIPT_DIR/deploy_extender.sh" "$KUBECONFIG_PATH"
  else
    step "Phase 9 — Skipping extender                (DEPLOY_EXTENDER=false)."
    echo "  To deploy: DEPLOY_EXTENDER=true hack/deploy_full_stack.sh"
    echo "  Standalone: hack/deploy_extender.sh"
  fi
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

print_summary() {
  echo ""
  echo -e "\033[32m  ╔══════════════════════════════════════════════════════════╗\033[0m"
  echo -e "\033[32m  ║              Full-stack deployment complete.             ║\033[0m"
  echo -e "\033[32m  ╚══════════════════════════════════════════════════════════╝\033[0m"
  echo ""
  echo -e "\033[32m  ── Components ────────────────────────────────────────────\033[0m"
  echo -e "\033[32m    Operator        : deployed  (namespace/$NAMESPACE)\033[0m"
  echo -e "\033[32m      ├─ OrchestrationProfile controller\033[0m"
  echo -e "\033[32m      ├─ PlacementServer (AI node scoring, :8090)\033[0m"
  echo -e "\033[32m      └─ Rebalance Engine (hybrid trigger detection)\033[0m"
  if [ "$USE_MOCK_AGENT" = "true" ]; then
    echo -e "\033[32m    Mock Agent      : deployed  (mock-decision-agent.$NAMESPACE)\033[0m"
  else
    echo -e "\033[33m    Mock Agent      : not deployed  (using real agent)\033[0m"
  fi
  if [ "$INSTALL_METRICS_SERVER" = "true" ]; then
    echo -e "\033[32m    metrics-server  : installed  (powers CPU/Memory rebalance triggers)\033[0m"
  else
    echo -e "\033[33m    metrics-server  : not installed  (CPU/Memory triggers won't fire)\033[0m"
  fi
  if [ "$DEPLOY_SCHEDULER_PLUGIN" = "true" ]; then
    echo -e "\033[32m    Scheduler Plugin: deployed  (opt-in: schedulerName=${HIRO_SCHEDULER_NAME})\033[0m"
    echo -e "\033[32m    Webhook (mutator): deployed  (auto-sets schedulerName, TLS via cert-manager)\033[0m"
  else
    echo -e "\033[33m    Scheduler Plugin: not deployed\033[0m"
    echo -e "\033[33m    Webhook (mutator): not deployed\033[0m"
  fi
  if [ "$DEPLOY_EXTENDER" = "true" ]; then
    echo -e "\033[32m    Extender        : deployed  (all pods via default scheduler)\033[0m"
  else
    echo -e "\033[33m    Extender        : not deployed\033[0m"
  fi
  echo ""
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  validate_inputs
  print_config

  check_prerequisites
  install_prerequisites
  cleanup_conflicting_integration
  deploy_oprator_with_samples
  deploy_mock_agent
  wait_for_placement_server
  deploy_scheduler_plugin
  deploy_scheduler_webhook
  deploy_extender

  print_summary
}

main "$@"
