#!/usr/bin/env bash
# scheduler-plugin/pin_k8s_version.sh <k8s-version>
#
# Pins scheduler-plugin/go.mod to a specific Kubernetes minor version by
# updating the k8s.io/kubernetes require and all staging-module replace
# directives in-place using `go mod edit`.
#
# No file is rewritten from scratch — only the affected lines change.
#
# After running this script, tidy the module to regenerate go.sum:
#   cd scheduler-plugin && go mod tidy
# Or use the Makefile:
#   make pin-k8s-version SCHED_K8S_VERSION=v1.36.0
#
# Usage:
#   scheduler-plugin/pin_k8s_version.sh v1.35.0
#   scheduler-plugin/pin_k8s_version.sh v1.36.0

set -euo pipefail

K8S_VERSION="${1:?Usage: $0 <k8s-version e.g. v1.35.0>}"

if ! [[ "${K8S_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "ERROR: version must be vMAJOR.MINOR.PATCH (e.g. v1.35.0)" >&2
  exit 1
fi

# Derive staging version: k8s v1.MINOR.PATCH -> staging v0.MINOR.PATCH
RAW="${K8S_VERSION#v}"
MINOR="${RAW#*.}"; MINOR="${MINOR%%.*}"
PATCH="${RAW##*.}"
STAGING_VERSION="v0.${MINOR}.${PATCH}"

# Always operate on the scheduler-plugin directory regardless of where
# the caller invoked this script from.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

STAGING_MODULES=(
  k8s.io/api
  k8s.io/apiextensions-apiserver
  k8s.io/apimachinery
  k8s.io/apiserver
  k8s.io/client-go
  k8s.io/cloud-provider
  k8s.io/component-base
  k8s.io/component-helpers
  k8s.io/controller-manager
  k8s.io/cri-api
  k8s.io/csi-translation-lib
  k8s.io/dynamic-resource-allocation
  k8s.io/kube-aggregator
  k8s.io/kube-scheduler
  k8s.io/kubelet
  k8s.io/metrics
  k8s.io/pod-security-admission
)

echo "Pinning scheduler-plugin → k8s=${K8S_VERSION}  staging=${STAGING_VERSION}"

# Update the kubernetes require entry.
go mod edit -require "k8s.io/kubernetes@${K8S_VERSION}"

# Update every staging module replace directive.
for mod in "${STAGING_MODULES[@]}"; do
  go mod edit -replace "${mod}=${mod}@${STAGING_VERSION}"
done

echo "go.mod updated."
echo "Next: cd scheduler-plugin && go mod tidy"
