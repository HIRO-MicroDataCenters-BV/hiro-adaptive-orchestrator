#!/bin/bash

set -euo pipefail

# if [ $# -lt 1 ]; then
#   echo "Usage: $0 <kubeconfig-path>"
#   exit 1
# fi

CLUSTER_NAME=${1:-sample}
GITHUB_PAT_TOKEN=${GITHUB_PAT_TOKEN:-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}
GITHUB_USERNAME=${GITHUB_USERNAME:-sskrishnav}
# The image URL format is always ghcr.io/<org-or-user-lowercase>/<repo-name>:<tag>.
DOCKER_REGISTRY=ghcr.io/hiro-microdatacenters-bv/hiro-adaptive-orchestrator
HIRO_OPERATOR_IMAGE=hiro-adaptive-orchestrator-controller:latest
SOURCE_REPO_URL=https://github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator

export KUBECONFIG=${2:-~/.kube/config}
# Export Name has to be IMG as it is used in the Makefile for docker-build and docker-push targets
export IMG=${DOCKER_REGISTRY}/${HIRO_OPERATOR_IMAGE}
export CR_PAT=$GITHUB_PAT_TOKEN

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
echo "Building operator image..."
make build
    
echo ""
echo "Verifying deployment..."
kubectl get crd orchestrationprofiles.orchestration.hiro.io

echo ""
echo "Applying sample OrchestrationProfile"
kubectl apply -f config/samples/orchestration_v1alpha1_orchestrationprofile.yaml

echo ""
echo "Verify CRD resource is created..."
kubectl get orchestrationprofiles

# echo ""
# echo "Running operator locally..."
# make run

echo ""
echo "Describe the created OrchestrationProfile resource..."
kubectl get orchestrationprofiles -o yaml

# Authenticate with GitHub Container Registry (GHCR) using the provided Personal Access Token (PAT)
echo ""
echo "Authenticating with GitHub Container Registry (GHCR)..."
echo $CR_PAT | docker login ghcr.io -u $GITHUB_USERNAME --password-stdin

# Deploy the operator pod in the Kubernetes cluster (instead of running locally)
echo ""
echo "Build the Operator image and push to the registry..."
make docker-build docker-push IMG=$IMG

# Add label to the Image
# echo ""
# echo "Adding label to the Operator image..."
# docker container create --name temp-container $IMG
# docker container commit \
#   --change "LABEL org.opencontainers.image.source=$SOURCE_REPO_URL" \
#   temp-container \
#   $IMG
# docker container rm temp-container
# docker push $IMG

# echo ""
# echo "Loading the Operator image into the kind cluster..."
# kind load docker-image $IMG --name $CLUSTER_NAME

echo ""
echo "Deploying operator..."
make deploy IMG=$IMG

echo ""
echo "Deployment completed successfully."

echo ""
echo "Verifying operator deployment..."
kubectl get pods -n hiro-adaptive-orchestrator-system