#!/usr/bin/env python3
# hack/patch_scheduler_static_pod.py
#
# Idempotently patches /etc/kubernetes/manifests/kube-scheduler.yaml to
# register the HIRO PlacementServer as a scheduler extender.
#
# Changes made to the manifest:
#   1. Appends --config=<SCHEDULER_CONFIG_PATH> to kube-scheduler command
#   2. Adds a volumeMount for the config file at SCHEDULER_CONFIG_PATH
#   3. Adds a hostPath volume pointing to SCHEDULER_CONFIG_PATH on the node
#
# Static pods may NOT reference ConfigMap volumes — kubelet rejects such
# manifests with "static pods may not reference configmaps". We therefore
# write the scheduler config file to the node filesystem beforehand (the
# deploy Job copies it via a separate hostPath mount) and reference it here
# as a hostPath volume, which kubelet accepts for static pods.
#
# Idempotent: exits 0 with no changes if already patched.
# Safe:       backs up the original manifest before writing.
#
# Environment variables (set by the Job via the shell script):
#   SCHEDULER_CONFIG_PATH  path inside scheduler pod        (required)
#   MANIFEST_PATH          path to kube-scheduler.yaml      (default: /host-manifests/kube-scheduler.yaml)
#   BACKUP_PATH            where to write the backup        (default: /host-etc-kubernetes/kube-scheduler.yaml.hiro-backup)
#
# IMPORTANT: the backup must NOT be placed inside the manifests directory.
# Kubelet reads every file there as a static pod spec — a backup of the
# original manifest would conflict with the patched one and override it.

import datetime
import os
import shutil
import sys
import yaml  # pyyaml


MANIFEST_PATH         = os.getenv("MANIFEST_PATH",  "/host-manifests/kube-scheduler.yaml")
BACKUP_PATH           = os.getenv("BACKUP_PATH",   "/host-etc-kubernetes/kube-scheduler.yaml.hiro-backup")
SCHEDULER_CONFIG_PATH = os.getenv("SCHEDULER_CONFIG_PATH")
VOLUME_NAME           = "hiro-scheduler-config"

if not SCHEDULER_CONFIG_PATH:
    print("ERROR: SCHEDULER_CONFIG_PATH environment variable is required", file=sys.stderr)
    sys.exit(1)

CONFIG_FLAG = f"--config={SCHEDULER_CONFIG_PATH}"


# ---------------------------------------------------------------------------
# Load manifest
# ---------------------------------------------------------------------------

print(f"Reading manifest: {MANIFEST_PATH}")
with open(MANIFEST_PATH) as f:
    doc = yaml.safe_load(f)

container = doc["spec"]["containers"][0]
commands  = container.get("command", [])


# ---------------------------------------------------------------------------
# Backup original (only on first run — do not overwrite an existing backup)
# ---------------------------------------------------------------------------

if not os.path.exists(BACKUP_PATH):
    shutil.copy2(MANIFEST_PATH, BACKUP_PATH)
    print(f"Backup written: {BACKUP_PATH}")
else:
    print(f"Backup already exists: {BACKUP_PATH} (skipping overwrite)")


# ---------------------------------------------------------------------------
# 1. Add --config flag (idempotent)
# ---------------------------------------------------------------------------

if any(c == CONFIG_FLAG for c in commands):
    print(f"Command flag already present: {CONFIG_FLAG}")
else:
    container["command"].append(CONFIG_FLAG)
    print(f"Added command flag: {CONFIG_FLAG}")


# ---------------------------------------------------------------------------
# 2. Add volumeMount (idempotent)
# ---------------------------------------------------------------------------

volume_mounts = container.setdefault("volumeMounts", [])

if not any(vm.get("name") == VOLUME_NAME for vm in volume_mounts):
    volume_mounts.append({
        "name":      VOLUME_NAME,
        "mountPath": SCHEDULER_CONFIG_PATH,
        "readOnly":  True,
    })
    print(f"Added volumeMount: {VOLUME_NAME} → {SCHEDULER_CONFIG_PATH}")
else:
    print(f"volumeMount already present: {VOLUME_NAME}")


# ---------------------------------------------------------------------------
# 3. Add hostPath volume (idempotent; static pods cannot reference ConfigMap volumes)
# ---------------------------------------------------------------------------

volumes = doc["spec"].setdefault("volumes", [])

if not any(v.get("name") == VOLUME_NAME for v in volumes):
    volumes.append({
        "name":     VOLUME_NAME,
        "hostPath": {"path": SCHEDULER_CONFIG_PATH, "type": "File"},
    })
    print(f"Added hostPath volume: {VOLUME_NAME} → {SCHEDULER_CONFIG_PATH}")
else:
    print(f"hostPath volume already present: {VOLUME_NAME}")


# ---------------------------------------------------------------------------
# 4. Always update hiro.io/last-updated annotation
#
# Kubelet only restarts the kube-scheduler container when it detects a change
# in the static pod manifest.  Since the --config patch above is idempotent
# (no diff on re-runs), kubelet would see no change and keep the old process
# running with its in-memory config.  Updating this annotation on every run
# guarantees a detectable manifest diff, causing kubelet to restart the
# container and pick up the updated config file from disk.
# ---------------------------------------------------------------------------

annotations = doc.setdefault("metadata", {}).setdefault("annotations", {})
annotations["hiro.io/last-updated"] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
print(f"Updated annotation: hiro.io/last-updated={annotations['hiro.io/last-updated']}")


# ---------------------------------------------------------------------------
# Write manifest (always, so the annotation change is persisted)
# ---------------------------------------------------------------------------

with open(MANIFEST_PATH, "w") as f:
    yaml.dump(doc, f, default_flow_style=False, allow_unicode=True)

print(f"Manifest written: {MANIFEST_PATH}")
print("Kubelet will detect the annotation change and restart kube-scheduler.")
