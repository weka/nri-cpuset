#!/bin/bash

# Unified parallel deployment script for weka-nri-cpuset
# Builds once, then deploys to all cluster nodes in parallel

set -e

# Validate KUBECONFIG is set
if [ -z "$KUBECONFIG" ]; then
    echo "Error: KUBECONFIG environment variable must be set"
    echo "Example: export KUBECONFIG=/path/to/kubeconfig"
    exit 1
fi

# Configuration
MAX_PARALLEL=${MAX_PARALLEL:-4}
USERNAME=${USERNAME:-root}

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log() {
    echo -e "${BLUE}[$(date '+%H:%M:%S')]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[$(date '+%H:%M:%S')] ✅${NC} $*"
}

log_error() {
    echo -e "${RED}[$(date '+%H:%M:%S')] ❌${NC} $*"
}

log_warning() {
    echo -e "${YELLOW}[$(date '+%H:%M:%S')] ⚠️${NC} $*"
}

# Function to deploy to a single node
deploy_to_node() {
    local node="$1"
    local target="${USERNAME}@${node}"
    local binary_path="$2"
    
    log "Deploying to $node..."
    
    # Create timestamp for versioning
    local timestamp=$(date +%Y%m%d-%H%M%S)
    local binary_name_versioned="weka-cpuset-dev-$timestamp"
    
    # Use a temporary file to capture output and check actual exit code
    local temp_output="/tmp/deploy_${node}_$$"
    
    # Deploy steps
    {
        echo "[$node] Creating directory structure..."
        ssh -o ConnectTimeout=10 -o StrictHostKeyChecking=no "$target" "mkdir -p /opt/weka-nri-cpuset"
        
        echo "[$node] Uploading binary as $binary_name_versioned..."
        scp -o ConnectTimeout=10 -o StrictHostKeyChecking=no "$binary_path" "$target:/opt/weka-nri-cpuset/$binary_name_versioned"
        
        echo "[$node] Creating symlink..."
        ssh -o ConnectTimeout=10 -o StrictHostKeyChecking=no "$target" "rm -f /usr/local/bin/weka-cpuset && ln -sf /opt/weka-nri-cpuset/$binary_name_versioned /usr/local/bin/weka-cpuset"
        
        echo "[$node] Restarting k3s..."
        ssh -o ConnectTimeout=10 -o StrictHostKeyChecking=no "$target" "systemctl restart k3s"
        
        echo "[$node] Waiting for k3s to be ready..."
        ssh -o ConnectTimeout=10 -o StrictHostKeyChecking=no "$target" "timeout 60 bash -c 'while ! systemctl is-active --quiet k3s; do sleep 2; done'"
        
        echo "[$node] ✅ Deployment complete: $binary_name_versioned"
        
    } > "$temp_output" 2>&1
    
    if [ $? -eq 0 ]; then
        # Success - show output with node prefix
        sed "s/^//" "$temp_output"
        rm -f "$temp_output"
        log_success "Deployment to $node completed successfully"
        return 0
    else
        # Failure - show output with node prefix and error
        sed "s/^//" "$temp_output"
        rm -f "$temp_output"
        log_error "Deployment to $node failed"
        return 1
    fi
}

# Export function for xargs
export -f deploy_to_node log log_success log_error log_warning
export USERNAME RED GREEN YELLOW BLUE NC

# Main script
main() {
    log "Starting unified parallel deployment of weka-nri-cpuset"
    log "Configuration: KUBECONFIG=$KUBECONFIG, MAX_PARALLEL=$MAX_PARALLEL, USERNAME=$USERNAME"
    
    # Check if we can connect to the cluster
    if ! kubectl --kubeconfig="$KUBECONFIG" cluster-info >/dev/null 2>&1; then
        log_error "Cannot connect to Kubernetes cluster with KUBECONFIG=$KUBECONFIG"
        log_error "Please verify:"
        log_error "  1. KUBECONFIG path exists and is readable"
        log_error "  2. kubectl can connect: kubectl --kubeconfig=\"$KUBECONFIG\" cluster-info"
        exit 1
    fi
    
    # Get all node names
    log "Discovering cluster nodes..."
    local nodes
    if ! nodes=$(kubectl --kubeconfig="$KUBECONFIG" get nodes -o jsonpath='{.items[*].metadata.name}'); then
        log_error "Failed to get cluster nodes"
        exit 1
    fi
    
    # Convert to array
    read -ra node_array <<< "$nodes"
    local node_count=${#node_array[@]}
    
    if [ $node_count -eq 0 ]; then
        log_error "No nodes found in cluster"
        exit 1
    fi
    
    log "Found $node_count nodes: ${node_array[*]}"
    
    # Clean and build the binary ONCE
    log "Cleaning previous builds..."
    make clean >/dev/null 2>&1 || true
    
    log "Building weka-cpuset binary..."
    if ! make build; then
        log_error "Failed to build binary"
        exit 1
    fi
    
    # Check for binary in common locations
    local binary_path=""
    if [ -f ".temp/weka-cpuset" ]; then
        binary_path=".temp/weka-cpuset"
    elif [ -f "bin/weka-cpuset" ]; then
        binary_path="bin/weka-cpuset"
    elif [ -f "weka-cpuset" ]; then
        binary_path="weka-cpuset"
    else
        log_error "Binary not found. Checked locations: .temp/weka-cpuset, bin/weka-cpuset, weka-cpuset"
        log_error "Please ensure 'make build' produces a binary in one of these locations"
        exit 1
    fi
    
    log_success "Binary built successfully at $binary_path"
    
    # Deploy to all nodes in parallel
    log "Starting parallel deployment to $node_count nodes with $MAX_PARALLEL parallel jobs..."
    
    local start_time=$(date +%s)
    
    # Create a function wrapper that passes the binary path
    deploy_wrapper() {
        deploy_to_node "$1" "$binary_path"
    }
    export -f deploy_wrapper
    export binary_path
    
    # Use printf to ensure each node is on a new line for xargs
    printf '%s\n' "${node_array[@]}" | xargs -I {} -P "$MAX_PARALLEL" bash -c 'deploy_wrapper "$@"' _ {}
    
    local exit_code=$?
    local end_time=$(date +%s)
    local duration=$((end_time - start_time))
    
    echo
    log "Parallel deployment completed in ${duration}s"
    
    # Summary
    if [ $exit_code -eq 0 ]; then
        log_success "All deployments completed successfully!"
        log ""
        log "Plugin deployment completed successfully"
    else
        log_error "Some deployments failed. Check the output above for details."
    fi
    
    return $exit_code
}

# Help function
show_help() {
    cat << EOF
Unified Parallel Deployment Script for weka-nri-cpuset

This script builds the weka-nri-cpuset binary once and then deploys it to all 
cluster nodes in parallel for faster deployment.

Usage: $0 [OPTIONS]

REQUIRED:
  KUBECONFIG      Must be set to path of kubeconfig file

Environment Variables:
  MAX_PARALLEL    Number of parallel deployments (default: 4)
  USERNAME        SSH username for nodes (default: root)

Examples:
  # Deploy with KUBECONFIG set
  export KUBECONFIG=/path/to/kubeconfig
  ./deploy-plugin.sh

  # Deploy with custom settings
  export KUBECONFIG=/path/to/kubeconfig
  MAX_PARALLEL=8 USERNAME=ubuntu ./deploy-plugin.sh

  # One-liner deployment
  KUBECONFIG=/path/to/kubeconfig ./deploy-plugin.sh

Requirements:
  - kubectl must be available and able to connect to cluster
  - SSH access to all cluster nodes with the specified USERNAME
  - make build must produce a weka-cpuset binary

The script will:
  1. Validate KUBECONFIG is set and cluster is accessible
  2. Build the weka-cpuset binary once (make clean && make build)
  3. Deploy to all cluster nodes in parallel
  4. Restart k3s service on each node
  5. Verify k3s starts successfully
  6. Provide deployment summary and next steps

Each deployment includes:
  - Binary upload with timestamp versioning
  - Symlink creation to /usr/local/bin/weka-cpuset
  - k3s service restart with readiness check
  - Verification that k3s starts successfully

EOF
}

# Parse command line arguments
case "${1:-}" in
    -h|--help)
        show_help
        exit 0
        ;;
    *)
        main "$@"
        ;;
esac