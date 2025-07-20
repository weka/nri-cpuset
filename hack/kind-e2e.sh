#!/bin/bash

set -euo pipefail

# Configuration
KIND_CLUSTER_NAME=${KIND_CLUSTER_NAME:-test}
PLUGIN_IMAGE=${PLUGIN_IMAGE:-weka/nri-cpuset:latest}
TEST_TIMEOUT=${TEST_TIMEOUT:-30m}

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

cleanup() {
    if [[ "${SKIP_CLEANUP:-}" != "true" ]]; then
        log_info "Cleaning up..."
        kind delete cluster --name "$KIND_CLUSTER_NAME" || true
    else
        log_info "Skipping cleanup (SKIP_CLEANUP=true)"
    fi
}

# Set up cleanup trap
trap cleanup EXIT

# Change to script directory
cd "$(dirname "$0")/.."

log_info "Starting kind e2e test for Weka NRI CPUSet plugin"

# Build plugin image
log_info "Building plugin image: $PLUGIN_IMAGE"
make image IMAGE_TAG=latest

# Set up kind cluster
log_info "Setting up kind cluster"
PLUGIN_IMAGE=$PLUGIN_IMAGE ./hack/kind-up.sh

# Set kubectl context - use context instead of KUBECONFIG due to macOS path length limits
KUBECTL_CONTEXT="kind-$KIND_CLUSTER_NAME"

# Load plugin image into cluster
log_info "Loading plugin image into cluster"
kind load docker-image "$PLUGIN_IMAGE" --name "$KIND_CLUSTER_NAME"

# Deploy plugin
log_info "Deploying Weka NRI CPUSet plugin"

# Create namespace if it doesn't exist
kubectl --context="$KUBECTL_CONTEXT" create namespace kube-system --dry-run=client -o yaml | kubectl --context="$KUBECTL_CONTEXT" apply -f -

# Apply RBAC
kubectl --context="$KUBECTL_CONTEXT" apply -f deploy/manifests/rbac.yaml

# Apply ConfigMap
kubectl --context="$KUBECTL_CONTEXT" apply -f deploy/manifests/configmap.yaml

# Apply DaemonSet with correct image
cat deploy/manifests/daemonset.yaml | \
  sed "s|image: weka/nri-cpuset:latest|image: $PLUGIN_IMAGE|" | \
  kubectl --context="$KUBECTL_CONTEXT" apply -f -

# Wait for plugin to be ready
log_info "Waiting for plugin DaemonSet to be ready..."
kubectl --context="$KUBECTL_CONTEXT" rollout status daemonset/weka-nri-cpuset -n kube-system --timeout=300s

# Verify plugin is running
log_info "Verifying plugin pods are running"
kubectl --context="$KUBECTL_CONTEXT" get pods -n kube-system -l app=weka-nri-cpuset

# Check plugin logs
log_info "Plugin logs:"
kubectl --context="$KUBECTL_CONTEXT" logs -n kube-system -l app=weka-nri-cpuset --tail=20

# Run e2e tests
log_info "Running e2e tests"
export TEST_NS=wekaplugin-e2e

# Create test namespace
kubectl --context="$KUBECTL_CONTEXT" create namespace "$TEST_NS" --dry-run=client -o yaml | kubectl --context="$KUBECTL_CONTEXT" apply -f -

# Set KUBECONFIG environment for Ginkgo tests
export KUBECONFIG="$(mktemp)"
kind get kubeconfig --name="$KIND_CLUSTER_NAME" > "$KUBECONFIG"

# Run Ginkgo tests
if command -v ginkgo &> /dev/null; then
    ginkgo -r --timeout="$TEST_TIMEOUT" --label-filter="e2e" ./test/e2e/
else
    go run github.com/onsi/ginkgo/v2/ginkgo -r --timeout="$TEST_TIMEOUT" --label-filter="e2e" ./test/e2e/
fi

test_exit_code=$?

if [[ $test_exit_code -eq 0 ]]; then
    log_info "All e2e tests passed!"
else
    log_error "Some e2e tests failed (exit code: $test_exit_code)"
    
    # Print debug information
    log_warn "Debug information:"
    echo "=== Plugin Pod Status ==="
    kubectl --context="$KUBECTL_CONTEXT" get pods -n kube-system -l app=weka-nri-cpuset -o wide
    
    echo "=== Plugin Logs ==="
    kubectl --context="$KUBECTL_CONTEXT" logs -n kube-system -l app=weka-nri-cpuset --tail=50
    
    echo "=== Test Pods Status ==="
    kubectl --context="$KUBECTL_CONTEXT" get pods -n "$TEST_NS" -o wide
    
    echo "=== Node Information ==="
    kubectl --context="$KUBECTL_CONTEXT" get nodes -o wide
    kubectl --context="$KUBECTL_CONTEXT" describe nodes
fi

exit $test_exit_code