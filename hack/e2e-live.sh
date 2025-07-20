#!/bin/bash

set -euo pipefail

# Configuration
KUBECONFIG=${KUBECONFIG:-"$HOME/.kube/config"}
TEST_NS=${TEST_NS:-wekaplugin-e2e}
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

# Verify prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."
    
    # Check kubectl
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl is not installed"
        exit 1
    fi
    
    # Check kubeconfig
    if [[ ! -f "$KUBECONFIG" ]]; then
        log_error "Kubeconfig file not found: $KUBECONFIG"
        exit 1
    fi
    
    # Test cluster connectivity
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Cannot connect to Kubernetes cluster"
        log_error "Check your KUBECONFIG: $KUBECONFIG"
        exit 1
    fi
    
    log_info "Connected to cluster: $(kubectl config current-context)"
}

# Deploy plugin if not already deployed
deploy_plugin() {
    log_info "Checking if Weka NRI CPUSet plugin is deployed..."
    
    if kubectl get daemonset weka-nri-cpuset -n kube-system &> /dev/null; then
        log_warn "Plugin already deployed. Use FORCE_DEPLOY=true to redeploy."
        if [[ "${FORCE_DEPLOY:-}" == "true" ]]; then
            log_info "Force redeploying plugin..."
            kubectl delete -f deploy/manifests/ || true
            sleep 5
        else
            return 0
        fi
    fi
    
    log_info "Deploying Weka NRI CPUSet plugin..."
    
    # Apply manifests
    kubectl apply -f deploy/manifests/rbac.yaml
    kubectl apply -f deploy/manifests/configmap.yaml
    
    # Update image in DaemonSet if specified
    if [[ -n "${PLUGIN_IMAGE:-}" ]]; then
        log_info "Using plugin image: $PLUGIN_IMAGE"
        cat deploy/manifests/daemonset.yaml | \
          sed "s|image: weka/nri-cpuset:latest|image: $PLUGIN_IMAGE|" | \
          kubectl apply -f -
    else
        kubectl apply -f deploy/manifests/daemonset.yaml
    fi
    
    # Wait for rollout
    log_info "Waiting for plugin DaemonSet to be ready..."
    kubectl rollout status daemonset/weka-nri-cpuset -n kube-system --timeout=300s
    
    log_info "Plugin deployed successfully"
}

# Verify plugin is working
verify_plugin() {
    log_info "Verifying plugin is working..."
    
    # Check pod status
    local ready_pods=$(kubectl get daemonset weka-nri-cpuset -n kube-system -o jsonpath='{.status.numberReady}')
    local desired_pods=$(kubectl get daemonset weka-nri-cpuset -n kube-system -o jsonpath='{.status.desiredNumberScheduled}')
    
    if [[ "$ready_pods" -eq "$desired_pods" ]] && [[ "$ready_pods" -gt 0 ]]; then
        log_info "Plugin is running on $ready_pods/$desired_pods nodes"
    else
        log_error "Plugin is not ready: $ready_pods/$desired_pods pods ready"
        return 1
    fi
    
    # Check logs for errors
    log_info "Checking plugin logs for errors..."
    local error_count=$(kubectl logs -n kube-system -l app=weka-nri-cpuset --tail=100 | grep -i error | wc -l || true)
    if [[ "$error_count" -gt 0 ]]; then
        log_warn "Found $error_count error messages in logs"
        kubectl logs -n kube-system -l app=weka-nri-cpuset --tail=20
    else
        log_info "No errors found in plugin logs"
    fi
}

# Run e2e tests
run_tests() {
    log_info "Running e2e tests against live cluster"
    
    # Create test namespace
    kubectl create namespace "$TEST_NS" --dry-run=client -o yaml | kubectl apply -f -
    
    # Export required environment variables
    export KUBECONFIG="$KUBECONFIG"
    export TEST_NS="$TEST_NS"
    
    # Run tests
    if command -v ginkgo &> /dev/null; then
        ginkgo -r --timeout="$TEST_TIMEOUT" --label-filter="e2e" ./test/e2e/
    else
        go run github.com/onsi/ginkgo/v2/ginkgo -r --timeout="$TEST_TIMEOUT" --label-filter="e2e" ./test/e2e/
    fi
    
    return $?
}

# Cleanup function
cleanup() {
    if [[ "${SKIP_CLEANUP:-}" != "true" ]]; then
        log_info "Cleaning up test namespace..."
        kubectl delete namespace "$TEST_NS" --ignore-not-found=true
    else
        log_info "Skipping cleanup (SKIP_CLEANUP=true)"
    fi
}

# Main execution
main() {
    # Change to script directory
    cd "$(dirname "$0")/.."
    
    log_info "Starting e2e test on live cluster"
    log_info "Cluster: $(kubectl config current-context)"
    log_info "Test namespace: $TEST_NS"
    
    # Check prerequisites
    check_prerequisites
    
    # Deploy plugin
    deploy_plugin
    
    # Verify plugin
    verify_plugin
    
    # Set up cleanup
    trap cleanup EXIT
    
    # Run tests
    if run_tests; then
        log_info "All e2e tests passed!"
        exit 0
    else
        log_error "Some e2e tests failed"
        
        # Print debug information
        log_warn "Debug information:"
        echo "=== Plugin Pod Status ==="
        kubectl get pods -n kube-system -l app=weka-nri-cpuset -o wide
        
        echo "=== Plugin Logs ==="
        kubectl logs -n kube-system -l app=weka-nri-cpuset --tail=50
        
        echo "=== Test Pods Status ==="
        kubectl get pods -n "$TEST_NS" -o wide || true
        
        echo "=== Node Information ==="
        kubectl get nodes -o wide
        
        exit 1
    fi
}

# Help message
if [[ "${1:-}" == "--help" ]] || [[ "${1:-}" == "-h" ]]; then
    cat << EOF
Usage: $0 [options]

Run e2e tests against a live Kubernetes cluster.

Environment Variables:
  KUBECONFIG      Path to kubeconfig file (default: \$HOME/.kube/config)
  TEST_NS         Test namespace (default: wekaplugin-e2e)
  PLUGIN_IMAGE    Plugin container image (default: weka/nri-cpuset:latest)
  TEST_TIMEOUT    Test timeout (default: 30m)
  FORCE_DEPLOY    Force plugin redeployment (default: false)
  SKIP_CLEANUP    Skip test namespace cleanup (default: false)

Examples:
  # Basic usage
  $0

  # Use specific kubeconfig and namespace
  KUBECONFIG=~/.kube/prod TEST_NS=my-test $0

  # Force plugin redeployment
  FORCE_DEPLOY=true $0

  # Skip cleanup for debugging
  SKIP_CLEANUP=true $0
EOF
    exit 0
fi

# Run main function
main "$@"