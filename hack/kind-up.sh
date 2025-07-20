#!/bin/bash

set -euo pipefail

# Configuration
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-test}
KIND_CONFIG_FILE=${KIND_CONFIG_FILE:-"$(dirname "$0")/kind-config.yaml"}
CONTAINERD_VERSION=${CONTAINERD_VERSION:-"1.7.13"}

echo "Setting up kind cluster: $KIND_CLUSTER_NAME"

# Check if kind is installed
if ! command -v kind &> /dev/null; then
    echo "Error: kind is not installed. Please install kind first."
    echo "See: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
    exit 1
fi

# Create kind config if it doesn't exist
if [[ ! -f "$KIND_CONFIG_FILE" ]]; then
    echo "Creating kind config at $KIND_CONFIG_FILE"
    cat > "$KIND_CONFIG_FILE" << EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.nri"]
    enable = true
    plugin_path = ["/opt/nri/plugins"]
    socket_path = "/run/containerd/nri/nri.sock"
    plugin_registration_timeout = "15s"
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /tmp/nri-plugins
    containerPath: /opt/nri/plugins
  - hostPath: /tmp/nri-run
    containerPath: /run/containerd/nri
- role: worker
  extraMounts:
  - hostPath: /tmp/nri-plugins
    containerPath: /opt/nri/plugins
  - hostPath: /tmp/nri-run
    containerPath: /run/containerd/nri
EOF
fi

# Create directories for NRI plugin
echo "Creating NRI plugin directories"
mkdir -p /tmp/nri-plugins
mkdir -p /tmp/nri-run

# Delete existing cluster if it exists
if kind get clusters | grep -q "^${KIND_CLUSTER_NAME}$"; then
    echo "Deleting existing cluster: $KIND_CLUSTER_NAME"
    kind delete cluster --name "$KIND_CLUSTER_NAME"
fi

# Create new cluster
echo "Creating kind cluster with containerd NRI support"
kind create cluster --name "$KIND_CLUSTER_NAME" --config "$KIND_CONFIG_FILE"

# Wait for cluster to be ready
echo "Waiting for cluster to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=300s

# Load plugin image into cluster if it exists
if [[ -n "${PLUGIN_IMAGE:-}" ]]; then
    echo "Loading plugin image: $PLUGIN_IMAGE"
    kind load docker-image "$PLUGIN_IMAGE" --name "$KIND_CLUSTER_NAME"
fi

echo "Kind cluster '$KIND_CLUSTER_NAME' is ready!"
echo "Use 'kubectl cluster-info --context kind-$KIND_CLUSTER_NAME' to access the cluster"