#!/bin/bash
# hack/undeploy_extender.sh
#
# Restores the kube-scheduler static pod manifest from the backup written by
# deploy_extender.sh, then removes the HIRO extender ConfigMaps.
#
# What this script does:
#   1. Runs a privileged Job on the control-plane node that restores
#      /etc/kubernetes/manifests/kube-scheduler.yaml from its .hiro-backup copy.
#   2. Kubelet detects the change and restarts kube-scheduler (original config).
#   3. Deletes ConfigMaps: hiro-scheduler-config, hiro-patch-script (kube-system).
#   4. Waits for kube-scheduler to be Ready without the extender.
#
# Usage:
#   hack/undeploy_extender.sh [kubeconfig-path]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export KUBECONFIG=${1:-~/.kube/config}
PATCH_JOB_NAMESPACE=${PATCH_JOB_NAMESPACE:-kube-system}
RESTORE_JOB_NAME="hiro-restore-scheduler"
PATCHER_IMAGE=${PATCHER_IMAGE:-python:3-slim}

step() { printf '\n==> %s\n' "$*"; }

run_restore_job() {
  step "Running restore Job on control-plane node..."

  kubectl delete job "$RESTORE_JOB_NAME" \
    -n "$PATCH_JOB_NAMESPACE" \
    --ignore-not-found

  kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${RESTORE_JOB_NAME}
  namespace: ${PATCH_JOB_NAMESPACE}
spec:
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: Never
      nodeSelector:
        node-role.kubernetes.io/control-plane: ""
      tolerations:
      - key: node-role.kubernetes.io/control-plane
        effect: NoSchedule
      volumes:
      - name: host-manifests
        hostPath:
          path: /etc/kubernetes/manifests
          type: Directory
      - name: host-etc-kubernetes
        hostPath:
          path: /etc/kubernetes
          type: Directory
      containers:
      - name: restorer
        image: ${PATCHER_IMAGE}
        command: ["/bin/sh", "-c"]
        args:
        - |
          MANIFEST=/host-manifests/kube-scheduler.yaml
          BACKUP=/host-etc-kubernetes/kube-scheduler.yaml.hiro-backup
          if [ ! -f "\$BACKUP" ]; then
            echo "No backup found at \$BACKUP — nothing to restore."
          else
            cp "\$BACKUP" "\$MANIFEST"
            rm -f "\$BACKUP"
            echo "Restored \$MANIFEST from backup."
          fi
          CONFIG=/host-etc-kubernetes/hiro-scheduler-config.yaml
          if [ -f "\$CONFIG" ]; then
            rm -f "\$CONFIG"
            echo "Removed \$CONFIG from node filesystem."
          else
            echo "No config file at \$CONFIG — already clean."
          fi
        securityContext:
          privileged: true
          runAsUser: 0
        volumeMounts:
        - name: host-manifests
          mountPath: /host-manifests
        - name: host-etc-kubernetes
          mountPath: /host-etc-kubernetes
EOF

  kubectl wait job/"$RESTORE_JOB_NAME" \
    -n "$PATCH_JOB_NAMESPACE" \
    --for=condition=Complete \
    --timeout=120s

  kubectl logs job/"$RESTORE_JOB_NAME" -n "$PATCH_JOB_NAMESPACE" | sed 's/^/  /'
}

cleanup_configmaps() {
  step "Removing HIRO extender ConfigMaps from kube-system..."
  kubectl delete configmap hiro-scheduler-config hiro-patch-script \
    -n kube-system --ignore-not-found
}

wait_for_scheduler() {
  step "Forcing kube-scheduler pod restart to pick up restored manifest..."
  kubectl delete pod \
    -l component=kube-scheduler \
    -n kube-system \
    --grace-period=0 \
    --ignore-not-found

  step "Waiting for kube-scheduler to come back Ready..."
  kubectl wait pod \
    -l component=kube-scheduler \
    -n kube-system \
    --for=condition=Ready \
    --timeout=120s
  echo "  kube-scheduler is Ready."
}

main() {
  run_restore_job
  cleanup_configmaps
  wait_for_scheduler

  echo ""
  echo -e "\033[32m========================================================\033[0m"
  echo -e "\033[32m  kube-scheduler restored. HIRO extender removed.\033[0m"
  echo -e "\033[32m========================================================\033[0m"
}

main "$@"
