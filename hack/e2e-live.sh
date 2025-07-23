#!/bin/bash

set -euo pipefail

# Configuration
KUBECONFIG=${KUBECONFIG:-"$HOME/.kube/config"}
TEST_NS=${TEST_NS:-wekaplugin-e2e}
PLUGIN_IMAGE=${PLUGIN_IMAGE:-""}  # Empty by default - will use build-and-deploy.sh to build if not specified
PLUGIN_REGISTRY=${PLUGIN_REGISTRY:-"images.scalar.dev.weka.io:5002"}
PLUGIN_NAME=${PLUGIN_NAME:-"weka-nri-cpuset"}
SKIP_BUILD=${SKIP_BUILD:-false}
TEST_TIMEOUT=${TEST_TIMEOUT:-30m}
TEST_PARALLEL=${TEST_PARALLEL:-8}
PRESERVE_ON_FAILURE=${PRESERVE_ON_FAILURE:-true}
CONTINUE_ON_FAILURE=${CONTINUE_ON_FAILURE:-false}

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
        # Parse the image format: [registry/]name:tag
        local image_registry=""
        local image_name=""
        local image_tag=""
        
        if [[ "$PLUGIN_IMAGE" =~ ^([^/]+/)?([^:]+):(.+)$ ]]; then
            image_registry="${BASH_REMATCH[1]%/}"  # Remove trailing slash
            image_name="${BASH_REMATCH[2]}"
            image_tag="${BASH_REMATCH[3]}"
        elif [[ "$PLUGIN_IMAGE" =~ ^([^/]+/)?([^:]+)$ ]]; then
            image_registry="${BASH_REMATCH[1]%/}"  # Remove trailing slash
            image_name="${BASH_REMATCH[2]}"
            image_tag="latest"
        else
            log_error "Invalid PLUGIN_IMAGE format: $PLUGIN_IMAGE"
            return 1
        fi
        
        # Override with parsed values if they exist
        if [[ -n "$image_registry" ]]; then
            deploy_args=("--kubeconfig" "$KUBECONFIG" "--registry" "$image_registry")
        fi
        if [[ -n "$image_name" ]]; then
            deploy_args+=("--image-name" "$image_name")
        fi
        
        # If we have a specific image, skip building
        deploy_args+=("--skip-build")
        log_info "Using existing plugin image: $PLUGIN_IMAGE"
    elif [[ "$SKIP_BUILD" == "true" ]]; then
        deploy_args+=("--skip-build")
        log_info "Skipping build, using existing image: ${PLUGIN_REGISTRY}/${PLUGIN_NAME}:latest"
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

# Monitor test pods in real-time
monitor_test_pods() {
    log_info "Monitoring test pods in namespace $TEST_NS..."
    
    # Start background monitoring
    {
        while true; do
            sleep 10
            local pod_count=$(kubectl get pods -n "$TEST_NS" --no-headers 2>/dev/null | wc -l || echo "0")
            if [[ "$pod_count" -gt 0 ]]; then
                echo ">>> Active test pods: $pod_count"
                kubectl get pods -n "$TEST_NS" --no-headers 2>/dev/null | while read line; do
                    echo "    $line"
                done
            fi
        done
    } &
    
    # Store PID for cleanup
    MONITOR_PID=$!
}

# Stop monitoring
stop_monitoring() {
    if [[ -n "${MONITOR_PID:-}" ]]; then
        kill "$MONITOR_PID" 2>/dev/null || true
        wait "$MONITOR_PID" 2>/dev/null || true
        unset MONITOR_PID
    fi
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
    
    # Additional verification: Check if NRI is available on nodes
    log_info "Verifying NRI socket availability on nodes..."
    local node_count=$(kubectl get nodes --no-headers | wc -l)
    log_info "Cluster has $node_count nodes"
}

# Run e2e tests
run_tests() {
    log_info "Running e2e tests against live cluster"
    
    log_info "Note: Test namespaces will be preserved for debugging - no cleanup performed"
    
    # Export required environment variables
    export KUBECONFIG="$KUBECONFIG"
    export TEST_NS="$TEST_NS"
    export PRESERVE_ON_FAILURE="$PRESERVE_ON_FAILURE"
    
    # Pre-test validation
    log_info "Performing pre-test validation..."
    local available_nodes=$(kubectl get nodes --no-headers | grep " Ready " | wc -l)
    log_info "Available nodes for testing: $available_nodes"
    
    # Start monitoring test pods
    monitor_test_pods
    
    log_info "Starting test execution with verbose output..."
    echo "==============================================================================="
    echo "Test Progress:"
    echo "- Use Ctrl+C to interrupt if needed"
    echo "- Test pod status will be shown periodically"
    echo "- Individual test progress will be displayed below"
    echo "- Using Ginkgo v2 with parallel execution (${TEST_PARALLEL} workers - default: 8)"
    echo "- Parallel tests: annotated pods, integer pods, shared pods"
    echo "- Sequential tests: recovery, live reallocation, conflict resolution"
    echo "- Resource preservation on failure: $PRESERVE_ON_FAILURE"
    echo "==============================================================================="
    
    # Optimize parallel execution based on available nodes
    local node_count
    node_count=$(kubectl get nodes --no-headers | wc -l)
    local optimal_parallel
    optimal_parallel=$(( node_count > TEST_PARALLEL ? TEST_PARALLEL : node_count ))
    
    log_info "Cluster has $node_count nodes, using $optimal_parallel parallel workers (max: $TEST_PARALLEL)"
    
    # Run tests with verbose and progress output
    local test_result=0
    cd "$(dirname "$0")/.."  # Ensure we're in the project root
    
    if command -v ginkgo &> /dev/null; then
        # Run parallel tests first (faster execution)
        log_info "Running parallel tests (annotated pods, integer pods, shared pods)..."
        ginkgo -r \
            --timeout="$TEST_TIMEOUT" \
            --label-filter="parallel" \
            --procs="$optimal_parallel" \
            -v \
            --show-node-events \
            --json-report=test-results-parallel.json \
            --junit-report=test-results-parallel.xml \
            ./test/e2e/ || test_result=$?
        
        # Run sequential tests if parallel tests passed or if we want to continue regardless
        if [[ "$test_result" -eq 0 ]] || [[ "${CONTINUE_ON_FAILURE:-false}" == "true" ]]; then
            log_info "Running sequential tests (recovery, live reallocation, conflicts)..."
            ginkgo -r \
                --timeout="$TEST_TIMEOUT" \
                --label-filter="sequential" \
                -v \
                --show-node-events \
                --json-report=test-results-sequential.json \
                --junit-report=test-results-sequential.xml \
                ./test/e2e/ || test_result=$?
        else
            log_warn "Parallel tests failed, skipping sequential tests (set CONTINUE_ON_FAILURE=true to run all)"
        fi
    else
        # Fallback to go test (less optimal)
        go test ./test/e2e \
            -timeout="$TEST_TIMEOUT" \
            -v \
            -ginkgo.label-filter="e2e" \
            -ginkgo.procs="$optimal_parallel" \
            -ginkgo.show-node-events \
            -ginkgo.json-report=test-results.json \
            -ginkgo.junit-report=test-results.xml || test_result=$?
    fi
    
    # Stop monitoring
    stop_monitoring
    
    echo "==============================================================================="
    log_info "Test execution completed"
    
    # Show final test pod status
    log_info "Final test namespace status:"
    kubectl get pods -n "$TEST_NS" -o wide 2>/dev/null || log_info "No pods found in test namespace"
    
    # Show test summary if reports were generated
    if [[ -f test-results-parallel.json ]] || [[ -f test-results-sequential.json ]] || [[ -f test-results.json ]]; then
        log_info "Test results summary available in test-results-*.json files"
        [[ -f test-results-parallel.json ]] && log_info "Parallel test results: test-results-parallel.json"
        [[ -f test-results-sequential.json ]] && log_info "Sequential test results: test-results-sequential.json"
        [[ -f test-results.json ]] && log_info "Combined test results: test-results.json"
    fi
    
    return $test_result
}

# Cleanup function - only show debug info if tests actually failed
cleanup() {
    # Stop monitoring if it's running
    stop_monitoring
    
    # Only show debugging info if tests failed (indicated by TEST_FAILED variable)
    if [[ "${TEST_FAILED:-false}" == "true" ]]; then
        log_info "Test execution completed with failures - preserving namespaces for debugging"
        log_info "Debug commands:"
        log_info "  kubectl get pods -n $TEST_NS* -o wide"
        log_info "  kubectl logs -n kube-system -l app=weka-nri-cpuset --since=5m"
        log_info "  kubectl get namespaces | grep $TEST_NS"
        log_info "Manual cleanup when ready: kubectl delete namespace \$(kubectl get ns | grep $TEST_NS | awk '{print \$1}')"
    else
        log_info "Test execution completed successfully"
    fi
    
    # Show available reports
    [[ -f test-results-parallel.json ]] && log_info "Parallel JSON report: test-results-parallel.json"
    [[ -f test-results-sequential.json ]] && log_info "Sequential JSON report: test-results-sequential.json"
    [[ -f test-results.json ]] && log_info "Combined JSON report: test-results.json"
    [[ -f test-results.xml ]] && log_info "Combined JUnit report: test-results.xml"
}

# Reset test environment to clean state
reset_test_environment() {
    log_info "Resetting test environment to clean state..."
    
    # Clean up any existing test namespaces (including per-worker namespaces)
    log_info "Cleaning up existing test namespaces..."
    kubectl delete namespace "$TEST_NS" --ignore-not-found=true --timeout=60s
    # Clean up any worker-specific namespaces from previous runs
    kubectl get namespaces -o name | grep "namespace/${TEST_NS}-w" | xargs -r kubectl delete --timeout=60s
    
    # Wait for namespaces to be fully deleted
    log_info "Waiting for namespace deletion to complete..."
    while kubectl get namespace "$TEST_NS" &> /dev/null || kubectl get namespaces -o name | grep -q "namespace/${TEST_NS}-w"; do
        sleep 2
    done
    
    # Force restart plugin pods to clear their internal state
    log_info "Restarting plugin to clear internal state..."
    kubectl rollout restart daemonset/weka-nri-cpuset -n kube-system
    kubectl rollout status daemonset/weka-nri-cpuset -n kube-system --timeout=120s
    
    # Give the plugin a moment to resynchronize with clean state
    log_info "Allowing plugin to resynchronize..."
    sleep 10
    
    # Verify plugin is healthy after restart
    local ready_pods=$(kubectl get daemonset weka-nri-cpuset -n kube-system -o jsonpath='{.status.numberReady}')
    local desired_pods=$(kubectl get daemonset weka-nri-cpuset -n kube-system -o jsonpath='{.status.desiredNumberScheduled}')
    
    if [[ "$ready_pods" -eq "$desired_pods" ]] && [[ "$ready_pods" -gt 0 ]]; then
        log_info "Plugin successfully restarted: $ready_pods/$desired_pods pods ready"
    else
        log_error "Plugin restart failed: $ready_pods/$desired_pods pods ready"
        return 1
    fi
    
    log_info "Test environment reset complete"
}

# Main execution
main() {
    # Change to script directory
    cd "$(dirname "$0")/.."
    
    log_info "Starting e2e test on live cluster"
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
    
    # Run tests
    if run_tests; then
        log_info "All e2e tests passed!"
        export TEST_FAILED=false
        exit 0
    else
        log_error "Some e2e tests failed"
        export TEST_FAILED=true
        
        # Print debug information
        log_warn "Debug information:"
        echo "=== Plugin Pod Status ==="
        kubectl get pods -n kube-system -l app=weka-nri-cpuset -o wide
        
        echo "=== Plugin Logs (last 50 lines) ==="
        kubectl logs -n kube-system -l app=weka-nri-cpuset --tail=50
        
        echo "=== Test Pods Status ==="
        kubectl get pods -n "$TEST_NS" -o wide || true
        
        echo "=== Test Pod Logs (if any) ==="
        kubectl get pods -n "$TEST_NS" --no-headers 2>/dev/null | while read name rest; do
            echo "--- Logs for $name ---"
            kubectl logs -n "$TEST_NS" "$name" || true
        done
        
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

Run e2e tests against a live Kubernetes cluster with real-time monitoring.

Features:
  - Real-time test progress output
  - Background monitoring of test pods
  - Detailed test reports (JSON/JUnit)
  - Comprehensive debug information on failure
  - Proper cleanup on interruption (Ctrl+C)

Environment Variables:
  KUBECONFIG      Path to kubeconfig file (default: \$HOME/.kube/config)
  TEST_NS         Test namespace (default: wekaplugin-e2e)
  PLUGIN_IMAGE    Plugin container image - if specified, will use existing image and skip build
  PLUGIN_REGISTRY Docker registry for building images (default: images.scalar.dev.weka.io:5002)
  PLUGIN_NAME     Plugin image name (default: weka-nri-cpuset)
  SKIP_BUILD      Skip building and use existing image at registry (default: false)
  TEST_TIMEOUT    Test timeout (default: 30m)
  TEST_PARALLEL   Number of parallel workers (default: 8)
  PRESERVE_ON_FAILURE Preserve failed test resources for debugging (default: true)
  CONTINUE_ON_FAILURE Continue with sequential tests even if parallel tests fail (default: false)
  FORCE_DEPLOY    Force plugin redeployment (default: false)
  SKIP_CLEANUP    Skip test namespace cleanup (default: false)
  PRESERVE_REPORTS Keep test report files after completion (default: false)

Examples:
  # Basic usage (builds and deploys fresh image)
  $0

  # Use specific kubeconfig and namespace  
  KUBECONFIG=~/.kube/prod TEST_NS=my-test $0

  # Build with custom registry
  PLUGIN_REGISTRY=my-registry.com:5000 $0

  # Use existing image (skip building)
  PLUGIN_IMAGE=my-registry/nri-cpuset:v1.2.3 $0
  
  # Skip build and use latest at default registry  
  SKIP_BUILD=true $0

  # Force plugin redeployment (will rebuild if no PLUGIN_IMAGE specified)
  FORCE_DEPLOY=true $0

  # Run with custom registry and debugging
  PLUGIN_REGISTRY=my-registry.com PRESERVE_ON_FAILURE=true $0
  
  # Run with fewer parallel workers (reduce from default 8)
  TEST_PARALLEL=4 $0
  
  # Continue all tests even if some fail (preserve is already default)
  CONTINUE_ON_FAILURE=true $0
  
  # Disable failure preservation (faster cleanup)
  PRESERVE_ON_FAILURE=false $0
EOF
    exit 0
fi

# Run main function
main "$@"