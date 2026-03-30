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
# The namespace where the operator will be deployed. 
# Check config/default/kustomization.yaml for the default namespace used in the manifests.
NAMESPACE=hiro-adaptive-orchestrator-system
# The service account name used by the operator. 
# Check config/default/manager_auth_proxy_patch.yaml for the default service account name.
SERVICE_ACCOUNT_NAME=hiro-adaptive-orchestrator-controller-manager

export KUBECONFIG=${2:-~/.kube/config}
# Export Name has to be IMG as it is used in the Makefile for docker-build and docker-push targets
export IMG=${DOCKER_REGISTRY}/${HIRO_OPERATOR_IMAGE}
export CR_PAT=$GITHUB_PAT_TOKEN

export DECISION_AGENT_URL="http://your-decision-agent-url"


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

# echo ""
# echo "Running operator locally..."
# make run

# Authenticate with GitHub Container Registry (GHCR) using the provided Personal Access Token (PAT)
echo ""
echo "Authenticating with GitHub Container Registry (GHCR)..."
echo $CR_PAT | docker login ghcr.io -u $GITHUB_USERNAME --password-stdin

# Deploy the operator pod in the Kubernetes cluster (instead of running locally)
echo ""
echo "Build the Operator image and push to the registry..."
make docker-build docker-push IMG=$IMG

# echo ""
# echo "Loading the Operator image into the kind cluster..."
# kind load docker-image $IMG --name $CLUSTER_NAME

echo ""
echo "Deploying operator..."
make deploy IMG=$IMG

echo ""
echo "Deployment completed successfully."

echo ""
echo "Creating image pull secret..."
kubectl create secret docker-registry ghcr-secret \
  --docker-server=ghcr.io \
  --docker-username=$GITHUB_USERNAME \
  --docker-password=$GITHUB_PAT_TOKEN \
  --namespace=$NAMESPACE \
  --dry-run=client -o yaml | kubectl apply -f -

echo ""
echo "Patching service account..."
kubectl patch serviceaccount $SERVICE_ACCOUNT_NAME \
  -p '{"imagePullSecrets": [{"name": "ghcr-secret"}]}' \
  --namespace=$NAMESPACE

echo ""
echo "Restarting operator pod to pick up new image..."
kubectl delete pod -l control-plane=controller-manager -n $NAMESPACE --ignore-not-found

echo ""
echo "Verifying operator deployment..."
kubectl get pods -n $NAMESPACE

echo ""
echo "Waiting for operator pod to be running (label: app.kubernetes.io/name=hiro-adaptive-orchestrator)..."
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=hiro-adaptive-orchestrator -n $NAMESPACE --timeout=180s

echo ""
echo "Applying sample OrchestrationProfile"
# kubectl apply -f config/samples/orchestration_v1alpha1_orchestrationprofile.yaml
kubectl apply -k config/samples/

echo ""
echo "Verify CRD resource is created..."
kubectl get orchestrationprofiles

echo ""
echo "Describe the created OrchestrationProfile resource..."
kubectl get orchestrationprofiles -o yaml

echo ""
# Print the line in orange color (ANSI escape code for orange is 33 for yellow, as true orange is not standard)
echo -e "\033[33m+++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++\033[0m"
echo -e "\033[33m++++ Deployment and sample applied.                                ++++\033[0m"
echo -e "\033[33m+++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++\033[0m"

echo ""
echo "Generate Corresponding Helm charts"
kubebuilder edit --plugins=helm/v2-alpha