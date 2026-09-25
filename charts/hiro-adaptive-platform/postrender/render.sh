#!/bin/bash
# charts/hiro-adaptive-platform/postrender/render.sh
#
# Helm post-renderer. When webhook.enable=true caused this chart to render a
# MutatingWebhookConfiguration, patches the operator Deployment (from the
# hiro-operator subchart) with the volume mount / port / arg it needs to
# serve webhook TLS.
#
# ENABLE_WEBHOOKS itself is NOT set here — that's an ordinary values
# override (hiro-operator.manager.env must include it; see values.yaml and
# the webhook-enabled example values file), since manager.env is a plain
# list dist/chart already renders verbatim from values, no patch needed.
#
# dist/chart (the hiro-operator subchart) is never modified for this —
# it's fully kubebuilder-generated and gets wiped on every regen. This
# script patches Helm's already-rendered YAML instead, after the fact,
# reusing the SAME kustomize patch file the non-Helm deploy path
# (config/default-with-webhook) already applies to the same Deployment —
# one canonical definition of "how the manager Deployment gets patched for
# webhook support," used by both deploy paths.
#
# Wired in automatically by `make helm-platform-deploy` via --post-renderer.
# If you run `helm install/upgrade` directly instead, pass
# `--post-renderer <path-to-this-script>` yourself, or webhook mode will
# render without its Deployment patch.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
WEBHOOK_PATCH="$REPO_ROOT/config/default-with-webhook/manager_webhook_patch.yaml"

# Prefer the repo-vendored kustomize (same version everything else here
# uses, no separate install needed) over whatever's on PATH.
KUSTOMIZE="$REPO_ROOT/bin/kustomize"
if [ ! -x "$KUSTOMIZE" ]; then
  KUSTOMIZE="kustomize"
fi

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

cat > "$WORK_DIR/all.yaml"

if ! grep -q 'kind: MutatingWebhookConfiguration' "$WORK_DIR/all.yaml"; then
  # webhook.enable=false (or unset) — nothing to patch, pass through as-is.
  cat "$WORK_DIR/all.yaml"
  exit 0
fi

if [ ! -f "$WEBHOOK_PATCH" ]; then
  echo "postrender/render.sh: expected patch file not found: $WEBHOOK_PATCH" >&2
  exit 1
fi

cat > "$WORK_DIR/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- all.yaml
patches:
- path: ${WEBHOOK_PATCH}
  target:
    kind: Deployment
EOF

"$KUSTOMIZE" build --load-restrictor LoadRestrictionsNone "$WORK_DIR"
