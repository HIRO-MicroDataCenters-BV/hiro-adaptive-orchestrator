#!/bin/bash
# hack/install_metrics_server.sh
#
# Installs metrics-server (metrics.k8s.io) if it isn't already present in
# the cluster. Idempotent — safe to call on every deploy; does nothing if
# a metrics-server Deployment already exists in kube-system.
#
# Why: the rebalance engine's CPUThreshold/MemoryThreshold trigger
# conditions (internal/rebalance/pressure.go) read live node usage from
# metrics-server. Without it, those two conditions simply never fire
# (soft-fail, logged) — everything else in the operator works the same
# either way. This script is what makes them actually functional.
#
# ─── Configuration ────────────────────────────────────────────────────────
#   METRICS_SERVER_VERSION       release tag to install      (default: latest)
#   METRICS_SERVER_INSECURE_TLS  true|false                  (default: true)
#                                 Passes --kubelet-insecure-tls to the
#                                 container args. Needed on kind/minikube/k3d,
#                                 where kubelet serving certs aren't signed by
#                                 a CA metrics-server trusts. Set to false on
#                                 real clusters with valid kubelet certs.
#
# Usage (standalone):
#   hack/install_metrics_server.sh [kubeconfig-path]
#
# Usage (via hack/deploy_full_stack.sh):
#   INSTALL_METRICS_SERVER=false hack/deploy_full_stack.sh   # to skip it

set -euo pipefail

KUBECONFIG_PATH=${1:-~/.kube/config}
export KUBECONFIG="$KUBECONFIG_PATH"

METRICS_SERVER_VERSION=${METRICS_SERVER_VERSION:-latest}
METRICS_SERVER_INSECURE_TLS=${METRICS_SERVER_INSECURE_TLS:-true}

DEPLOYMENT_NAME=metrics-server
DEPLOYMENT_NAMESPACE=kube-system

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n==> %s\n' "$*"; }

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

is_already_installed() {
  kubectl get deployment "$DEPLOYMENT_NAME" -n "$DEPLOYMENT_NAMESPACE" &>/dev/null
}

manifest_url() {
  if [ "$METRICS_SERVER_VERSION" = "latest" ]; then
    echo "https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml"
  else
    echo "https://github.com/kubernetes-sigs/metrics-server/releases/download/${METRICS_SERVER_VERSION}/components.yaml"
  fi
}

apply_manifest() {
  step "Installing metrics-server (${METRICS_SERVER_VERSION})..."
  kubectl apply -f "$(manifest_url)"
}

patch_insecure_kubelet_tls() {
  if [ "$METRICS_SERVER_INSECURE_TLS" != "true" ]; then
    return
  fi
  step "Patching metrics-server for insecure kubelet TLS (dev clusters: kind/minikube/k3d)..."
  kubectl patch deployment "$DEPLOYMENT_NAME" -n "$DEPLOYMENT_NAMESPACE" --type=json \
    -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
}

wait_for_ready() {
  step "Waiting for metrics-server to be Available..."
  kubectl wait deployment/"$DEPLOYMENT_NAME" -n "$DEPLOYMENT_NAMESPACE" \
    --for=condition=Available --timeout=120s
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  step "Checking for an existing metrics-server installation..."
  if is_already_installed; then
    echo "  metrics-server already installed — skipping install."
    kubectl get deployment "$DEPLOYMENT_NAME" -n "$DEPLOYMENT_NAMESPACE"
    return
  fi

  apply_manifest
  patch_insecure_kubelet_tls
  wait_for_ready

  step "metrics-server installed and ready."
}

main "$@"
