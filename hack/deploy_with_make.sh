#!/bin/bash

set -euo pipefail

# if [ $# -lt 1 ]; then
#   echo "Usage: $0 <kubeconfig-path>"
#   exit 1
# fi

export KUBECONFIG=${1:-~/.kube/config}

echo "========================================"
echo "HIRO Adaptive Orchestrator Deployment"
echo "========================================"

echo "Using kubeconfig: $KUBECONFIG"

echo ""
echo "Generating code..."
make generate

echo ""
echo "Generating manifests..."
make manifests

echo ""
echo "Installing CRDs..."
make install

echo ""
echo "Deploying operator..."
make deploy

echo ""
echo "Verifying deployment..."
kubectl get crd orchestrationprofiles.orchestration.hiro.io

# echo ""
# echo "Applying sample OrchestrationProfile"
# kubectl apply -f config/samples/orchestration_v1alpha1_orchestrationprofile.yaml

# echo ""
# echo "Verify CRD resource is created..."
# kubectl get orchestrationprofiles

echo ""
echo "Deployment completed successfully."
