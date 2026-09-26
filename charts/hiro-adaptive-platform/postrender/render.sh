#!/bin/bash
# charts/hiro-adaptive-platform/postrender/render.sh
#
# Helm post-renderer. Patches the operator Deployment (from the
# hiro-operator subchart) for two features that can't be expressed as plain
# values on a subchart whose manager.env is a flat list Helm can't merge
# into by key:
#
#   1. webhook.enable=true → adds the cert volume/mount/port/arg (reusing
#      the same static patch the non-Helm deploy path already applies via
#      config/default-with-webhook) and appends ENABLE_WEBHOOKS=true.
#   2. scheduler.mode=plugin → appends HIRO_SCHEDULER_NAME matching
#      scheduler.plugin.schedulerName, read back out of the scheduler
#      plugin's own already-rendered ConfigMap so the two can never drift.
#
# Both rely on Kubernetes allowing duplicate env-var names in a PodSpec and
# using the last one — appending an override is enough, no need to find and
# replace manager.env's static default.
#
# dist/chart (the hiro-operator subchart) is never modified for this —
# it's fully kubebuilder-generated and gets wiped on every regen. This
# script patches Helm's already-rendered YAML instead, after the fact.
#
# Wired in automatically by `make helm-platform-deploy`/`helm-platform-template`.
# Helm v4 requires --post-renderer to name an installed plugin (type
# postrenderer/v1), not a raw script path (see plugin.yaml next to this
# file, and https://github.com/helm/helm/issues/31340) — the Makefile
# targets install that plugin and pass
# `--post-renderer hiro-adaptive-platform-postrenderer`. If you run `helm
# install/upgrade`/`helm template` directly instead:
#   helm plugin install charts/hiro-adaptive-platform/postrender
#   helm ... --post-renderer hiro-adaptive-platform-postrenderer
# or these patches won't apply.
set -euo pipefail

# -P resolves symlinks — needed because `helm plugin install` in local-dev
# mode symlinks the plugin dir (e.g. ~/Library/helm/plugins/postrender ->
# this directory); plain `pwd` would keep the symlink's path instead of
# this file's real location, breaking REPO_ROOT below.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
WEBHOOK_CERT_PATCH="$REPO_ROOT/config/default-with-webhook/manager_webhook_patch.yaml"
ENABLE_WEBHOOKS_PATCH="$SCRIPT_DIR/manager_enable_webhooks_patch.yaml"

# Prefer the repo-vendored kustomize (same version everything else here
# uses, no separate install needed) over whatever's on PATH.
KUSTOMIZE="$REPO_ROOT/bin/kustomize"
if [ ! -x "$KUSTOMIZE" ]; then
  KUSTOMIZE="kustomize"
fi

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

cat > "$WORK_DIR/all.yaml"

webhook_active=false
if grep -q 'kind: MutatingWebhookConfiguration' "$WORK_DIR/all.yaml"; then
  webhook_active=true
fi

# The scheduler plugin's ConfigMap always renders this exact line (6-space
# indent, preserved verbatim from templates/scheduler-plugin/configmap.yaml's
# literal block) when scheduler.mode=plugin — extract the resolved
# schedulerName straight out of Helm's own rendered output instead of
# needing this script to know scheduler.plugin.schedulerName's value itself.
scheduler_name=""
scheduler_name="$(grep -m1 '^      - schedulerName: ' "$WORK_DIR/all.yaml" | sed 's/^      - schedulerName: //' || true)"

if [ "$webhook_active" = "false" ] && [ -z "$scheduler_name" ]; then
  # Neither feature active — nothing to patch, pass through as-is.
  cat "$WORK_DIR/all.yaml"
  exit 0
fi

# Scopes every patch below to the operator's own Deployment only — plain
# "kind: Deployment" also matches the scheduler-plugin Deployment
# (templates/scheduler-plugin/deployment.yaml) when scheduler.mode=plugin,
# and these patches (ports/-, env/-, volumeMounts/-) don't apply cleanly to
# that one, since it has none of those arrays pre-populated the same way.
OPERATOR_DEPLOYMENT_TARGET='    kind: Deployment
    labelSelector: "app.kubernetes.io/name=hiro-operator,control-plane=controller-manager"'

patches=""

if [ "$webhook_active" = "true" ]; then
  if [ ! -f "$WEBHOOK_CERT_PATCH" ]; then
    echo "postrender/render.sh: expected patch file not found: $WEBHOOK_CERT_PATCH" >&2
    exit 1
  fi
  patches+=$'- path: '"${WEBHOOK_CERT_PATCH}"$'\n  target:\n'"${OPERATOR_DEPLOYMENT_TARGET}"$'\n'
  patches+=$'- path: '"${ENABLE_WEBHOOKS_PATCH}"$'\n  target:\n'"${OPERATOR_DEPLOYMENT_TARGET}"$'\n'
fi

if [ -n "$scheduler_name" ]; then
  cat > "$WORK_DIR/manager_scheduler_name_patch.yaml" <<EOF
- op: add
  path: /spec/template/spec/containers/0/env/-
  value:
    name: HIRO_SCHEDULER_NAME
    value: "${scheduler_name}"
EOF
  patches+=$'- path: '"$WORK_DIR/manager_scheduler_name_patch.yaml"$'\n  target:\n'"${OPERATOR_DEPLOYMENT_TARGET}"$'\n'
fi

cat > "$WORK_DIR/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- all.yaml
patches:
${patches}
EOF

"$KUSTOMIZE" build --load-restrictor LoadRestrictionsNone "$WORK_DIR"
