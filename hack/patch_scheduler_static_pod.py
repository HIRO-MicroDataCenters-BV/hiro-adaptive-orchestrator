#!/usr/bin/env python3
# hack/patch_scheduler_static_pod.py
#
# Idempotently patches /etc/kubernetes/manifests/kube-scheduler.yaml to
# register the HIRO PlacementServer as a scheduler extender.
#
# Changes made to the manifest:
#   1. Appends --config=<SCHEDULER_CONFIG_PATH> to kube-scheduler command
#   2. Adds a volumeMount for the HIRO ConfigMap at SCHEDULER_CONFIG_PATH
#   3. Adds a ConfigMap volume reference (name: hiro-scheduler-config)
#
# Idempotent: exits 0 with no changes if already patched.
# Safe:       backs up the original manifest before writing.
#
# Environment variables (set by the Job via the shell script):
#   SCHEDULER_CONFIG_PATH  path inside scheduler pod   (required)
#   MANIFEST_PATH          path to kube-scheduler.yaml (default: /host-manifests/kube-scheduler.yaml)
#   CONFIGMAP_NAME         ConfigMap name              (default: hiro-scheduler-config)
#   CONFIGMAP_KEY          key inside ConfigMap        (default: scheduler-config.yaml)

import os
import shutil
import sys
import yaml  # pyyaml


MANIFEST_PATH       = os.getenv("MANIFEST_PATH",       "/host-manifests/kube-scheduler.yaml")
SCHEDULER_CONFIG_PATH = os.getenv("SCHEDULER_CONFIG_PATH")
CONFIGMAP_NAME      = os.getenv("CONFIGMAP_NAME",      "hiro-scheduler-config")
CONFIGMAP_KEY       = os.getenv("CONFIGMAP_KEY",       "scheduler-config.yaml")
VOLUME_NAME         = "hiro-scheduler-config"

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
# Idempotency check
# ---------------------------------------------------------------------------

if any(c == CONFIG_FLAG for c in commands):
    print(f"Already patched ({CONFIG_FLAG} present) — nothing to do.")
    sys.exit(0)


# ---------------------------------------------------------------------------
# Backup original
# ---------------------------------------------------------------------------

backup_path = MANIFEST_PATH + ".hiro-backup"
if not os.path.exists(backup_path):
    shutil.copy2(MANIFEST_PATH, backup_path)
    print(f"Backup written: {backup_path}")
else:
    print(f"Backup already exists: {backup_path} (skipping overwrite)")


# ---------------------------------------------------------------------------
# 1. Add --config flag
# ---------------------------------------------------------------------------

container["command"].append(CONFIG_FLAG)
print(f"Added command flag: {CONFIG_FLAG}")


# ---------------------------------------------------------------------------
# 2. Add volumeMount
# ---------------------------------------------------------------------------

volume_mounts = container.setdefault("volumeMounts", [])

# Guard: don't add a duplicate volumeMount
if not any(vm.get("name") == VOLUME_NAME for vm in volume_mounts):
    volume_mounts.append({
        "name":      VOLUME_NAME,
        "mountPath": SCHEDULER_CONFIG_PATH,
        "subPath":   CONFIGMAP_KEY,
        "readOnly":  True,
    })
    print(f"Added volumeMount: {VOLUME_NAME} → {SCHEDULER_CONFIG_PATH}")


# ---------------------------------------------------------------------------
# 3. Add ConfigMap volume
# ---------------------------------------------------------------------------

volumes = doc["spec"].setdefault("volumes", [])

# Guard: don't add a duplicate volume
if not any(v.get("name") == VOLUME_NAME for v in volumes):
    volumes.append({
        "name":      VOLUME_NAME,
        "configMap": {"name": CONFIGMAP_NAME},
    })
    print(f"Added volume: {VOLUME_NAME} (ConfigMap: {CONFIGMAP_NAME})")


# ---------------------------------------------------------------------------
# Write patched manifest
# ---------------------------------------------------------------------------

with open(MANIFEST_PATH, "w") as f:
    yaml.dump(doc, f, default_flow_style=False, allow_unicode=True)

print(f"Manifest patched successfully: {MANIFEST_PATH}")
print("Kubelet will detect the change and restart kube-scheduler automatically.")
