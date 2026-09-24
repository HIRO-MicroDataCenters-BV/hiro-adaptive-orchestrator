#!/bin/bash
# hack/install_prometheus_operator.sh
#
# Installs the Prometheus Operator (via the kube-prometheus-stack Helm chart)
# as its own Helm release. Idempotent — does nothing if the release already
# exists. Safe to call on every deploy.
#
# Before installing, each sub-component (node-exporter, Grafana, Alertmanager)
# is checked against what's already running in the cluster — a component is
# only installed if nothing matching it is already present, so we don't
# double-install (or hostPort-conflict, in node-exporter's case) something
# that's already there.
#
# The Prometheus instance itself is configured with empty ServiceMonitor
# selectors, so it watches ServiceMonitors in any namespace with any labels —
# not just ones carrying this release's own Helm labels.
#
# ─── Configuration ────────────────────────────────────────────────────────
#   PROMETHEUS_OPERATOR_NAMESPACE      namespace for the release       (default: hiro-monitoring)
#   PROMETHEUS_OPERATOR_RELEASE        helm release name                (default: hiro-monitoring)
#   PROMETHEUS_OPERATOR_CHART_VERSION  kube-prometheus-stack chart version (default: latest)
#
# Usage (standalone):
#   hack/install_prometheus_operator.sh [kubeconfig-path]

set -euo pipefail

KUBECONFIG_PATH=${1:-~/.kube/config}
export KUBECONFIG="$KUBECONFIG_PATH"

PROMETHEUS_OPERATOR_NAMESPACE=${PROMETHEUS_OPERATOR_NAMESPACE:-hiro-monitoring}
PROMETHEUS_OPERATOR_RELEASE=${PROMETHEUS_OPERATOR_RELEASE:-hiro-monitoring}
PROMETHEUS_OPERATOR_CHART_VERSION=${PROMETHEUS_OPERATOR_CHART_VERSION:-latest}
# Empty → the Grafana subchart generates a random admin password into the
# <release>-grafana Secret.
GRAFANA_ADMIN_PASSWORD=${GRAFANA_ADMIN_PASSWORD:-}

CHART_REPO_NAME=prometheus-community
CHART_REPO_URL=https://prometheus-community.github.io/helm-charts
CHART_NAME=kube-prometheus-stack

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n==> %s\n' "$*"; }

# True if any DaemonSet/Deployment/StatefulSet, in any namespace, has a name
# matching $1 (case-insensitive) — a simple, chart-agnostic way to detect
# "something already provides this component" without assuming a specific
# release name or set of labels.
component_already_present() {
  local pattern="$1"
  for kind in daemonset deployment statefulset; do
    if kubectl get "$kind" -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
        | grep -qi "$pattern"; then
      return 0
    fi
  done
  return 1
}

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

# A Helm release record exists (in any state — deployed, failed, pending...).
release_exists() {
  helm status "$PROMETHEUS_OPERATOR_RELEASE" -n "$PROMETHEUS_OPERATOR_NAMESPACE" &>/dev/null
}

# The release exists AND its last operation succeeded. `helm status` alone
# returns success even for a failed release, so a failed first install would
# otherwise be mistaken for "already installed" and never retried.
release_is_deployed() {
  helm status "$PROMETHEUS_OPERATOR_RELEASE" -n "$PROMETHEUS_OPERATOR_NAMESPACE" 2>/dev/null \
    | grep -q '^STATUS: deployed'
}

remove_failed_release() {
  step "Removing the non-deployed '${PROMETHEUS_OPERATOR_RELEASE}' release before reinstalling..."
  helm uninstall "$PROMETHEUS_OPERATOR_RELEASE" -n "$PROMETHEUS_OPERATOR_NAMESPACE" --wait || true
}

ensure_helm_repo() {
  step "Ensuring Helm repo '${CHART_REPO_NAME}' is configured..."
  if helm repo list 2>/dev/null | grep -q "^${CHART_REPO_NAME}"; then
    echo "  Repo already added."
  else
    helm repo add "$CHART_REPO_NAME" "$CHART_REPO_URL"
  fi
  helm repo update "$CHART_REPO_NAME" &>/dev/null
}

ensure_namespace() {
  step "Ensuring namespace '${PROMETHEUS_OPERATOR_NAMESPACE}' exists..."
  kubectl create namespace "$PROMETHEUS_OPERATOR_NAMESPACE" \
    --dry-run=client -o yaml | kubectl apply -f -
}

# Populates HELM_COMPONENT_FLAGS with --set overrides for any sub-component
# that's skipped because something matching it already exists in the cluster.
detect_component_conflicts() {
  step "Checking for existing node-exporter / Grafana / Alertmanager..."
  HELM_COMPONENT_FLAGS=()

  if component_already_present "node-exporter"; then
    echo "  [nodeExporter] already present in cluster — skipping (would otherwise hostPort-conflict)."
    HELM_COMPONENT_FLAGS+=(--set nodeExporter.enabled=false)
  else
    echo "  [nodeExporter] not found — installing."
  fi

  if component_already_present "grafana"; then
    echo "  [grafana] already present in cluster — skipping."
    HELM_COMPONENT_FLAGS+=(--set grafana.enabled=false)
  else
    echo "  [grafana] not found — installing."
    if [ -n "$GRAFANA_ADMIN_PASSWORD" ]; then
      echo "  [grafana] using the admin password from GRAFANA_ADMIN_PASSWORD."
      HELM_COMPONENT_FLAGS+=(--set-string "grafana.adminPassword=$GRAFANA_ADMIN_PASSWORD")
    fi
  fi

  if component_already_present "alertmanager"; then
    echo "  [alertmanager] already present in cluster — skipping."
    HELM_COMPONENT_FLAGS+=(--set alertmanager.enabled=false)
  else
    echo "  [alertmanager] not found — installing."
  fi
}

install_chart() {
  step "Installing ${CHART_NAME} as release '${PROMETHEUS_OPERATOR_RELEASE}'..."

  local version_flag=()
  if [ "$PROMETHEUS_OPERATOR_CHART_VERSION" != "latest" ]; then
    version_flag=(--version "$PROMETHEUS_OPERATOR_CHART_VERSION")
  fi

  # "${arr[@]+"${arr[@]}"}" instead of a plain "${arr[@]}": with `set -u`
  # active, some bash versions treat expanding a zero-element array as an
  # unbound variable even though it's legitimately empty. This form expands
  # to nothing in that case, on every bash version, rather than erroring.
  #
  # --set-json (not --set) for the two selectors below: Helm's --set
  # mini-language parses `{}` as an empty list, not an empty map, and the
  # Prometheus CRD rejects a list where it expects a map. --set-json passes
  # real JSON, so `{}` is unambiguously an empty object.
  helm install "$PROMETHEUS_OPERATOR_RELEASE" "${CHART_REPO_NAME}/${CHART_NAME}" \
    -n "$PROMETHEUS_OPERATOR_NAMESPACE" \
    "${version_flag[@]+"${version_flag[@]}"}" \
    "${HELM_COMPONENT_FLAGS[@]+"${HELM_COMPONENT_FLAGS[@]}"}" \
    --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
    --set-json prometheus.prometheusSpec.serviceMonitorSelector={} \
    --set-json prometheus.prometheusSpec.serviceMonitorNamespaceSelector={} \
    --wait --timeout 5m
}

print_prometheus_service_account() {
  step "Locating the Prometheus pod's service account (needed for RBAC binding)..."
  local sa
  sa=$(kubectl get pod -n "$PROMETHEUS_OPERATOR_NAMESPACE" \
    -l app.kubernetes.io/name=prometheus \
    -o jsonpath='{.items[0].spec.serviceAccountName}' 2>/dev/null || true)
  if [ -n "$sa" ]; then
    echo "  Service account: ${PROMETHEUS_OPERATOR_NAMESPACE}:${sa}"
  else
    echo "  Could not auto-detect it yet (pod may still be starting)."
    echo "  Once ready, check with:"
    echo "    kubectl get pod -n ${PROMETHEUS_OPERATOR_NAMESPACE} -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].spec.serviceAccountName}'"
  fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  step "Checking for an existing '${PROMETHEUS_OPERATOR_RELEASE}' release..."
  if release_is_deployed; then
    echo "  '${PROMETHEUS_OPERATOR_RELEASE}' already installed and deployed in namespace '${PROMETHEUS_OPERATOR_NAMESPACE}' — skipping install."
    print_prometheus_service_account
    return
  fi

  if release_exists; then
    echo "  a '${PROMETHEUS_OPERATOR_RELEASE}' release exists but is not in 'deployed' state (likely a failed install)."
    remove_failed_release
  fi

  ensure_helm_repo
  ensure_namespace
  detect_component_conflicts
  install_chart
  print_prometheus_service_account

  step "Prometheus Operator installed and ready."
}

main "$@"
