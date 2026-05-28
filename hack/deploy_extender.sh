#!/bin/bash
# hack/deploy_extender.sh
#
# Full extender deploy: applies the HIRO KubeSchedulerConfiguration ConfigMap
# to kube-system AND patches the kube-scheduler static pod manifest so the
# config is mounted and --config is passed automatically.
#
# The extender approach routes the DEFAULT kube-scheduler's filter and
# prioritize calls to the HIRO operator PlacementServer.
# Use this on clusters where a custom scheduler pod cannot be deployed
# (GKE Autopilot, EKS Fargate, managed AKS, etc.) or on kubeadm clusters
# for testing.
#
# ─── What this script does ────────────────────────────────────────────────────
#   1. Renders hiro-scheduler-config ConfigMap (substitutes PLACEMENT_SERVICE_NAME
#      and NAMESPACE into the urlPrefix) and applies it to kube-system.
#   2. Creates a hiro-patch-script ConfigMap from hack/patch_scheduler_static_pod.py.
#   3. Runs a privileged Kubernetes Job on the control-plane node that:
#        - mounts /etc/kubernetes/manifests from the node (hostPath, read/write)
#        - runs the Python patch script to add --config and the ConfigMap volume
#          to kube-scheduler.yaml
#   4. Kubelet detects the manifest change (inotify) and restarts kube-scheduler.
#   5. Waits for the new kube-scheduler pod to be Ready.
#   6. Verifies the --config flag is live in the running pod spec.
#
# ─── Prerequisites ────────────────────────────────────────────────────────────
#   - Operator must be running (PlacementServer must be reachable)
#   - kubectl access including kube-system namespace
#   - Control-plane node must allow privileged pods (kube-system uses baseline PSA)
#   - PATCHER_IMAGE must be pullable from the control-plane node
#     (default python:3-slim requires internet access; override for air-gapped clusters)
#
# ─── Configuration ────────────────────────────────────────────────────────────
#   NAMESPACE              operator namespace   (default: hiro-adaptive-orchestrator-system)
#   NAME_PREFIX            kustomize prefix     (default: hiro-adaptive-orchestrator-)
#   PLACEMENT_SERVICE_NAME k8s service name     (default: <NAME_PREFIX>controller-manager-placement-service)
#   SCHEDULER_CONFIG_PATH  mount path inside scheduler pod
#                                               (default: /etc/kubernetes/hiro-scheduler-config.yaml)
#   PATCH_JOB_NAMESPACE    namespace for patch Job (default: kube-system)
#   PATCHER_IMAGE          patcher container image (default: python:3-slim)
#
# ─── Usage ────────────────────────────────────────────────────────────────────
#   # Standalone
#   export GITHUB_PAT_TOKEN=<token>   # only needed if operator not yet deployed
#   hack/deploy_extender.sh [kubeconfig-path]
#
#   # Via deploy_all.sh
#   APPLY_EXTENDER_CONFIG=true hack/deploy_all.sh [kubeconfig-path]
#
#   # Custom namespace / service name
#   NAMESPACE=my-ns PLACEMENT_SERVICE_NAME=my-svc hack/deploy_extender.sh
#
#   # Air-gapped: use a local mirror image that has python3+pyyaml
#   PATCHER_IMAGE=my-registry/python3-pyyaml:latest hack/deploy_extender.sh

set -euo pipefail

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

export KUBECONFIG=${1:-~/.kube/config}
export NAME_PREFIX=${NAME_PREFIX:-hiro-adaptive-orchestrator-}
export NAMESPACE=${NAMESPACE:-hiro-adaptive-orchestrator-system}
export PLACEMENT_SERVICE_NAME=${PLACEMENT_SERVICE_NAME:-${NAME_PREFIX}controller-manager-placement-service}

SCHEDULER_CONFIG_PATH=${SCHEDULER_CONFIG_PATH:-/etc/kubernetes/hiro-scheduler-config.yaml}
PATCH_JOB_NAME="hiro-patch-scheduler"
PATCH_JOB_NAMESPACE=${PATCH_JOB_NAMESPACE:-kube-system}
PATCH_SCRIPT_CONFIGMAP="hiro-patch-script"
PATCHER_IMAGE=${PATCHER_IMAGE:-python:3-slim}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

step() { printf '\n==> %s\n' "$*"; }

# ---------------------------------------------------------------------------
# Steps
# ---------------------------------------------------------------------------

print_config() {
  echo "========================================================"
  echo " HIRO Scheduler Extender — Full Deploy"
  echo "========================================================"
  echo "Namespace            : $NAMESPACE"
  echo "Name Prefix          : $NAME_PREFIX"
  echo "Placement Svc Name   : $PLACEMENT_SERVICE_NAME"
  echo "Scheduler Config Path: $SCHEDULER_CONFIG_PATH"
  echo "Patch Job Namespace  : $PATCH_JOB_NAMESPACE"
  echo "Patcher Image        : $PATCHER_IMAGE"
  echo "Kubeconfig           : $KUBECONFIG"
  echo "========================================================"
}

# Step 1 — Render and apply the KubeSchedulerConfiguration ConfigMap.
# Substitutes PLACEMENT_SERVICE_NAME and NAMESPACE into the urlPrefix so
# the default kube-scheduler calls the correct PlacementServer endpoint.
apply_extender_configmap() {
  step "Rendering and applying hiro-scheduler-config to kube-system..."

  local tmp_config
  tmp_config=$(mktemp /tmp/hiro-extender-config-XXXXXX.yaml)

  sed \
    -e "s|hiro-adaptive-orchestrator-controller-manager-placement-service|${PLACEMENT_SERVICE_NAME}|g" \
    -e "s|hiro-adaptive-orchestrator-system|${NAMESPACE}|g" \
    "$REPO_ROOT/config/extender/scheduler-config.yaml" > "$tmp_config"

  kubectl create configmap hiro-scheduler-config \
    --from-file=scheduler-config.yaml="$tmp_config" \
    -n kube-system \
    --dry-run=client -o yaml | kubectl apply -f -

  rm -f "$tmp_config"
  echo "  ConfigMap hiro-scheduler-config applied to kube-system."
}

# Step 2 — Package the Python patch script as a ConfigMap so the Job can
# mount and run it without requiring a custom image.
create_patch_script_configmap() {
  step "Creating patch-script ConfigMap from hack/patch_scheduler_static_pod.py..."

  kubectl create configmap "$PATCH_SCRIPT_CONFIGMAP" \
    --from-file=patch.py="$SCRIPT_DIR/patch_scheduler_static_pod.py" \
    -n "$PATCH_JOB_NAMESPACE" \
    --dry-run=client -o yaml | kubectl apply -f -

  echo "  ConfigMap '$PATCH_SCRIPT_CONFIGMAP' applied to $PATCH_JOB_NAMESPACE."
}

# Step 3 — Run the privileged Job on the control-plane node.
#
# Volume layout inside the patcher container:
#   /host-manifests/          ← hostPath /etc/kubernetes/manifests  (read/write)
#   /hiro-config/             ← ConfigMap hiro-scheduler-config      (read)
#   /scripts/patch.py         ← ConfigMap hiro-patch-script          (executable)
#
# The patch script:
#   - Backs up kube-scheduler.yaml to kube-scheduler.yaml.hiro-backup
#   - Appends --config=SCHEDULER_CONFIG_PATH to the command
#   - Adds a ConfigMap volumeMount and volume entry
#   - Writes the patched manifest back in place
#   - Is idempotent: skips if --config is already present
#
# Kubelet detects the manifest change via inotify and restarts kube-scheduler.
run_patch_job() {
  step "Running privileged patch Job on the control-plane node..."

  # Remove any leftover Job from a previous run so logs are always fresh.
  kubectl delete job "$PATCH_JOB_NAME" \
    -n "$PATCH_JOB_NAMESPACE" \
    --ignore-not-found

  kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${PATCH_JOB_NAME}
  namespace: ${PATCH_JOB_NAMESPACE}
  labels:
    app.kubernetes.io/name: hiro-adaptive-orchestrator
    app.kubernetes.io/component: scheduler-patcher
spec:
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: Never

      # ── Target the control-plane node ───────────────────────────────────
      # Static pod manifests live on the control-plane node's filesystem.
      nodeSelector:
        node-role.kubernetes.io/control-plane: ""
      tolerations:
      - key: node-role.kubernetes.io/control-plane
        effect: NoSchedule

      volumes:
      # ── Host /etc/kubernetes/manifests ──────────────────────────────────
      # The Job writes kube-scheduler.yaml here directly on the node disk.
      # Kubelet watches this directory (inotify) and restarts kube-scheduler
      # when the file changes.
      - name: host-manifests
        hostPath:
          path: /etc/kubernetes/manifests
          type: Directory

      # ── HIRO extender KubeSchedulerConfiguration ───────────────────────
      # The ConfigMap we applied in step 1. The patch script adds this as a
      # volume in the kube-scheduler manifest so kubelet can mount it.
      - name: hiro-scheduler-config
        configMap:
          name: hiro-scheduler-config

      # ── Python patch script ─────────────────────────────────────────────
      - name: patch-script
        configMap:
          name: ${PATCH_SCRIPT_CONFIGMAP}
          defaultMode: 0755

      containers:
      - name: patcher
        image: ${PATCHER_IMAGE}
        command: ["/bin/sh", "-c"]
        args:
        - |
          set -e
          echo "Installing pyyaml..."
          pip install -q pyyaml
          echo "Running patch script..."
          python3 /scripts/patch.py
        env:
        - name: MANIFEST_PATH
          value: /host-manifests/kube-scheduler.yaml
        - name: SCHEDULER_CONFIG_PATH
          value: ${SCHEDULER_CONFIG_PATH}
        - name: CONFIGMAP_NAME
          value: hiro-scheduler-config
        - name: CONFIGMAP_KEY
          value: scheduler-config.yaml
        securityContext:
          privileged: true
          runAsUser: 0
        volumeMounts:
        - name: host-manifests
          mountPath: /host-manifests
        - name: hiro-scheduler-config
          mountPath: /hiro-config
        - name: patch-script
          mountPath: /scripts
EOF

  step "Waiting for patch Job to complete (timeout 120s)..."
  kubectl wait job/"$PATCH_JOB_NAME" \
    -n "$PATCH_JOB_NAMESPACE" \
    --for=condition=Complete \
    --timeout=120s

  echo ""
  echo "  Job logs:"
  echo "  ──────────────────────────────────────"
  kubectl logs job/"$PATCH_JOB_NAME" -n "$PATCH_JOB_NAMESPACE" | sed 's/^/  /'
  echo "  ──────────────────────────────────────"
}

# Step 4 — Wait for kubelet to detect the file change and restart
# kube-scheduler with the new manifest.
#
# Kubelet's inotify watch fires within a few seconds of the file write.
# After receiving the event it terminates the old pod and starts a new one.
# The full cycle (terminate + start + ready) takes ~15-30s.
wait_for_scheduler_restart() {
  step "Waiting for kube-scheduler to restart with HIRO extender config..."
  echo "  Pausing 10s for kubelet inotify to fire..."
  sleep 10

  kubectl wait pod \
    -l component=kube-scheduler \
    -n kube-system \
    --for=condition=Ready \
    --timeout=120s

  echo "  kube-scheduler pod is Ready."
}

# Step 5 — Confirm the patch is visible in the live pod spec.
verify_patch() {
  step "Verifying patch..."

  local sched_pod
  sched_pod=$(kubectl get pod \
    -l component=kube-scheduler \
    -n kube-system \
    -o jsonpath='{.items[0].metadata.name}')

  echo "  Running pod: $sched_pod"

  local cmd_json
  cmd_json=$(kubectl get pod "$sched_pod" \
    -n kube-system \
    -o jsonpath='{.spec.containers[0].command}')

  if echo "$cmd_json" | grep -q "hiro-scheduler-config"; then
    echo -e "  \033[32m✓ --config flag confirmed in the running pod spec.\033[0m"
  else
    echo -e "  \033[31m✗ --config flag NOT found. Kubelet may still be restarting — retry in 15s.\033[0m"
    echo "    Full command: $cmd_json"
    exit 1
  fi

  echo ""
  echo "  Volume mounts on kube-scheduler:"
  kubectl get pod "$sched_pod" -n kube-system \
    -o jsonpath='{range .spec.containers[0].volumeMounts[*]}  {.name}: {.mountPath}{"\n"}{end}'
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  print_config

  apply_extender_configmap
  create_patch_script_configmap
  run_patch_job
  wait_for_scheduler_restart
  verify_patch

  echo ""
  echo -e "\033[32m========================================================\033[0m"
  echo -e "\033[32m  Extender deploy complete.\033[0m"
  echo -e "\033[32m  Filter     : POST /extender/filter\033[0m"
  echo -e "\033[32m  Prioritize : POST /extender/prioritize\033[0m"
  echo -e "\033[32m  Server     : ${PLACEMENT_SERVICE_NAME}.${NAMESPACE}.svc.cluster.local\033[0m"
  echo -e "\033[32m========================================================\033[0m"
  echo ""
  echo "  To verify extender calls:"
  echo "    kubectl logs -l component=kube-scheduler -n kube-system | grep -i extender"
  echo ""
  echo "  To undo (restore original manifest):"
  echo "    hack/undeploy_extender.sh"
}

main "$@"
