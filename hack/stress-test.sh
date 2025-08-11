#!/bin/bash

set -euo pipefail

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

# Configuration
KUBECONFIG=${KUBECONFIG:-"$HOME/.kube/config"}
TEST_NS=${TEST_NS:-wekaplugin-stress}
PLUGIN_IMAGE=${PLUGIN_IMAGE:-""}  # Empty by default - will use build-and-deploy.sh to build if not specified
PLUGIN_REGISTRY=${PLUGIN_REGISTRY:-"images.scalar.dev.weka.io:5002"}
PLUGIN_NAME=${PLUGIN_NAME:-"weka-nri-cpuset"}
TEST_TIMEOUT=${TEST_TIMEOUT:-60m}  # Longer timeout for stress tests
TEST_PARALLEL=${TEST_PARALLEL:-1}  # Stress tests are resource-intensive, use fewer parallel workers
PRESERVE_ON_FAILURE=${PRESERVE_ON_FAILURE:-true}
CONTINUE_ON_FAILURE=${CONTINUE_ON_FAILURE:-false}

# Verify prerequisites
check_prerequisites() {
    log_info "Checking prerequisites for stress testing..."
    
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
    
    # Check node resources
    local node_count
    node_count=$(kubectl get nodes --no-headers | wc -l)
    log_info "Cluster has $node_count nodes for stress testing"
    
    if [[ "$node_count" -lt 2 ]]; then
        log_warn "Stress testing with fewer than 2 nodes may not fully exercise the plugin"
    fi
    
    # Check available CPU resources
    log_info "Checking node CPU resources for stress testing..."
    kubectl get nodes -o custom-columns="NAME:.metadata.name,CPU:.status.capacity.cpu" --no-headers | while read -r name cpu; do
        log_info "Node $name: $cpu CPUs"
    done
}

# Deploy plugin if not already deployed (reuse logic from e2e-live.sh)
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
    
    log_info "Deploying Weka NRI CPUSet plugin using build-and-deploy.sh..."
    
    # Prepare arguments for build-and-deploy.sh
    local deploy_args=("--kubeconfig" "$KUBECONFIG")
    
    # Add registry if specified
    if [[ -n "$PLUGIN_REGISTRY" ]]; then
        deploy_args+=("--registry" "$PLUGIN_REGISTRY")
    fi
    
    # Add image name
    deploy_args+=("--image-name" "$PLUGIN_NAME")
    
    # If PLUGIN_IMAGE is specified, parse it to get registry, name, and tag
    if [[ -n "$PLUGIN_IMAGE" ]]; then
        log_info "Using existing plugin image: $PLUGIN_IMAGE"
        # Parse and use existing image logic from e2e-live.sh if needed
    else
        log_info "Building and deploying new plugin image with registry: $PLUGIN_REGISTRY"
    fi
    
    # Call build-and-deploy.sh with the prepared arguments
    local script_dir="$(dirname "$0")"
    if ! "$script_dir/build-and-deploy.sh" "${deploy_args[@]}"; then
        log_error "Failed to deploy plugin using build-and-deploy.sh"
        return 1
    fi
    
    log_info "Plugin deployed successfully"
}

# Verify plugin is working
verify_plugin() {
    log_info "Verifying plugin is working..."
    
    # Check pod status
    local ready_pods
    ready_pods=$(kubectl get daemonset weka-nri-cpuset -n kube-system -o jsonpath='{.status.numberReady}')
    local desired_pods
    desired_pods=$(kubectl get daemonset weka-nri-cpuset -n kube-system -o jsonpath='{.status.desiredNumberScheduled}')
    
    if [[ "$ready_pods" -eq "$desired_pods" ]] && [[ "$ready_pods" -gt 0 ]]; then
        log_info "Plugin is running on $ready_pods/$desired_pods nodes"
    else
        log_error "Plugin is not ready: $ready_pods/$desired_pods pods ready"
        return 1
    fi
    
    # Check logs for errors
    log_info "Checking plugin logs for errors..."
    local error_count
    error_count=$(kubectl logs -n kube-system -l app=weka-nri-cpuset --tail=100 | grep -i error | wc -l || true)
    if [[ "$error_count" -gt 0 ]]; then
        log_warn "Found $error_count error messages in logs"
        kubectl logs -n kube-system -l app=weka-nri-cpuset --tail=20
    else
        log_info "No errors found in plugin logs"
    fi
    
    log_info "Plugin verification complete"
}

# Run stress/chaos tests
run_stress_tests() {
    log_info "Running stress/chaos tests against live cluster"
    
    # Export required environment variables
    export KUBECONFIG="$KUBECONFIG"
    export TEST_NS="$TEST_NS"
    export PRESERVE_ON_FAILURE="$PRESERVE_ON_FAILURE"
    
    # Pre-test validation
    log_info "Performing pre-test validation for stress testing..."
    local available_nodes
    available_nodes=$(kubectl get nodes --no-headers | grep " Ready " | wc -l)
    log_info "Available nodes for stress testing: $available_nodes"
    
    if [[ "$available_nodes" -lt 1 ]]; then
        log_error "No ready nodes available for stress testing"
        return 1
    fi
    
    log_info "Starting stress test execution..."
    echo "==============================================================================="
    echo "Stress Test Configuration:"
    echo "- Cluster: $(kubectl config current-context)"
    echo "- Test namespace: $TEST_NS"
    echo "- Test timeout: $TEST_TIMEOUT"
    echo "- Parallel workers: $TEST_PARALLEL (stress tests use fewer workers)"
    echo "- Resource preservation on failure: $PRESERVE_ON_FAILURE"
    echo "- Available nodes: $available_nodes"
    echo "==============================================================================="
    
    # Run stress tests with verbose and progress output
    local test_result=0
    cd "$(dirname "$0")/.."  # Ensure we're in the project root
    
    if command -v ginkgo &> /dev/null; then
        # Run stress/chaos tests specifically
        log_info "Running stress/chaos tests..."
        ginkgo -r \
            --timeout="$TEST_TIMEOUT" \
            --label-filter="stress || chaos" \
            --procs="$TEST_PARALLEL" \
            -v \
            --show-node-events \
            --json-report=test-results-stress.json \
            --junit-report=test-results-stress.xml \
            ./test/e2e/ || test_result=$?
    else
        # Fallback to go test
        go test ./test/e2e \
            -timeout="$TEST_TIMEOUT" \
            -v \
            -ginkgo.label-filter="stress || chaos" \
            -ginkgo.procs="$TEST_PARALLEL" \
            -ginkgo.show-node-events \
            -ginkgo.json-report=test-results-stress.json \
            -ginkgo.junit-report=test-results-stress.xml || test_result=$?
    fi
    
    echo "==============================================================================="
    log_info "Stress test execution completed"
    
    # Show test summary
    if [[ -f test-results-stress.json ]]; then
        log_info "Stress test results available: test-results-stress.json"
    fi
    
    return $test_result
}

# Cleanup function
cleanup() {
    log_info "Stress test execution completed"
    
    # Show available reports
    [[ -f test-results-stress.json ]] && log_info "Stress test JSON report: test-results-stress.json"
    [[ -f test-results-stress.xml ]] && log_info "Stress test JUnit report: test-results-stress.xml"
    
    # Show namespace cleanup info if tests failed
    if [[ "${TEST_FAILED:-false}" == "true" ]]; then
        log_info "Stress test namespaces preserved for debugging"
        log_info "Debug commands:"
        log_info "  kubectl get pods -n $TEST_NS* -o wide"
        log_info "  kubectl logs -n kube-system -l app=weka-nri-cpuset --since=10m"
        log_info "  kubectl get namespaces | grep $TEST_NS"
        log_info "Manual cleanup: kubectl delete namespace \$(kubectl get ns | grep $TEST_NS | awk '{print \$1}')"
    fi
}

# Main execution
main() {
    # Change to script directory
    cd "$(dirname "$0")/.."
    
    log_info "Starting stress/chaos tests on live cluster"
    log_info "Cluster: $(kubectl config current-context)"
    log_info "Test namespace: $TEST_NS"
    log_info "Test timeout: $TEST_TIMEOUT"
    log_info "Parallel workers: $TEST_PARALLEL"
    log_info "Preserve on failure: $PRESERVE_ON_FAILURE"
    
    # Check prerequisites
    check_prerequisites
    
    # Deploy plugin
    deploy_plugin
    
    # Verify plugin
    verify_plugin
    
    # Set up cleanup and interrupt handling
    trap cleanup EXIT
    trap 'log_warn "Interrupted by user, cleaning up..."; cleanup; exit 130' INT TERM
    
    # Run stress tests
    if run_stress_tests; then
        log_info "All stress/chaos tests passed!"
        export TEST_FAILED=false
        exit 0
    else
        log_error "Some stress/chaos tests failed"
        export TEST_FAILED=true
        
        # Print debug information
        log_warn "Debug information:"
        echo "=== Plugin Pod Status ==="
        kubectl get pods -n kube-system -l app=weka-nri-cpuset -o wide
        
        echo "=== Plugin Logs (last 50 lines) ==="
        kubectl logs -n kube-system -l app=weka-nri-cpuset --tail=50
        
        echo "=== Stress Test Pods Status ==="
        kubectl get pods -n "$TEST_NS" -o wide || true
        
        echo "=== Node Information ==="
        kubectl get nodes -o wide
        
        echo "=== Node Resources ==="
        kubectl top nodes 2>/dev/null || log_warn "Could not get node resource usage (metrics-server may not be available)"
        
        exit 1
    fi
}

# Help message
if [[ "${1:-}" == "--help" ]] || [[ "${1:-}" == "-h" ]]; then
    cat << EOF
Usage: $0 [options]

Run stress/chaos tests against a live Kubernetes cluster.

These are intensive tests that create significant load on the cluster and plugin.
They are designed to test system behavior under extreme conditions and should
be run separately from regular e2e tests.

Environment Variables:
  KUBECONFIG      Path to kubeconfig file (default: \$HOME/.kube/config)
  TEST_NS         Test namespace prefix (default: wekaplugin-stress)
  PLUGIN_IMAGE    Plugin container image - if specified, will use existing image and skip build
  PLUGIN_REGISTRY Docker registry for building images (default: images.scalar.dev.weka.io:5002)
  PLUGIN_NAME     Plugin image name (default: weka-nri-cpuset)
  TEST_TIMEOUT    Test timeout (default: 60m - stress tests take longer)
  TEST_PARALLEL   Number of parallel workers (default: 1 - stress tests are resource-intensive)
  PRESERVE_ON_FAILURE Preserve failed test resources for debugging (default: true)
  FORCE_DEPLOY    Force plugin redeployment (default: false)

Examples:
  # Basic stress test usage
  $0

  # Use specific kubeconfig and custom timeout
  KUBECONFIG=~/.kube/prod TEST_TIMEOUT=90m $0

  # Use existing image and preserve resources
  PLUGIN_IMAGE=my-registry/nri-cpuset:v1.2.3 PRESERVE_ON_FAILURE=true $0

Prerequisites:
  - At least 8 CPU cores per node recommended for meaningful stress testing
  - Multiple nodes recommended for full testing coverage
  - Sufficient memory and disk space for test pod creation
  - Plugin should be deployed and working on the cluster

Warning: These tests create significant load and may impact cluster performance.
Run during maintenance windows or on dedicated test clusters.
EOF
    exit 0
fi

# Run main function
main "$@"