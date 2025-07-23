#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

KUBECONFIG_FILE=""
REGISTRY="images.scalar.dev.weka.io:5002"
IMAGE_NAME="weka-nri-cpuset"
DEBUG=false
DRY_RUN=false
SKIP_BUILD=false

usage() {
    cat << EOF
Usage: $0 --kubeconfig PATH [options]

Required:
  --kubeconfig PATH    Path to kubeconfig file

Options:
  --registry URL       Docker registry URL (default: images.scalar.dev.weka.io:5002)
  --image-name NAME    Image name (default: weka-nri-cpuset)
  --debug             Enable debug output
  --dry-run           Show what would be done without executing
  --skip-build        Skip building Docker image (use existing)
  --help              Show this help

Description:
  Build Docker image using existing Dockerfile and deploy weka-cpuset as a DaemonSet.
  Uses unix timestamp as image version for simplicity.

Examples:
  $0 --kubeconfig ~/kc/operator-demo
  $0 --kubeconfig ~/.kube/config --registry my-registry.com:5000
  $0 --kubeconfig ~/kc/operator-demo --dry-run --debug
EOF
    exit 1
}

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >&2
}

debug() {
    if [[ "$DEBUG" == "true" ]]; then
        log "DEBUG: $*"
    fi
}

error() {
    log "ERROR: $*"
    exit 1
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --kubeconfig)
                KUBECONFIG_FILE="$2"
                shift 2
                ;;
            --registry)
                REGISTRY="$2"
                shift 2
                ;;
            --image-name)
                IMAGE_NAME="$2"
                shift 2
                ;;
            --debug)
                DEBUG=true
                shift
                ;;
            --dry-run)
                DRY_RUN=true
                shift
                ;;
            --skip-build)
                SKIP_BUILD=true
                shift
                ;;
            --help)
                usage
                ;;
            *)
                error "Unknown option: $1"
                ;;
        esac
    done

    if [[ -z "$KUBECONFIG_FILE" ]]; then
        error "Missing required --kubeconfig argument"
    fi

    if [[ ! -f "$KUBECONFIG_FILE" ]]; then
        error "Kubeconfig file not found: $KUBECONFIG_FILE"
    fi
}

run_command() {
    local cmd="$1"
    local description="$2"
    
    log "$description"
    debug "Command: $cmd"
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log "DRY RUN: Would execute: $cmd"
        return 0
    fi
    
    if ! eval "$cmd"; then
        error "Failed to execute: $description"
    fi
}

check_dependencies() {
    log "Checking dependencies..."
    
    if ! command -v docker >/dev/null 2>&1; then
        error "Docker not found in PATH"
    fi
    
    if ! command -v kubectl >/dev/null 2>&1; then
        error "kubectl not found in PATH"
    fi
    
    if ! command -v make >/dev/null 2>&1; then
        error "make not found in PATH"
    fi
    
    # Test kubectl access
    if ! kubectl --kubeconfig="$KUBECONFIG_FILE" cluster-info >/dev/null 2>&1; then
        error "Cannot access Kubernetes cluster with provided kubeconfig"
    fi
    
    # Test docker access
    if ! docker info >/dev/null 2>&1; then
        error "Cannot access Docker daemon"
    fi
    
    log "Dependencies check passed"
}


update_daemonset_manifest() {
    local image_tag="$1"
    local temp_manifest="$PROJECT_ROOT/deploy/manifests/daemonset-temp.yaml"
    local original_manifest="$PROJECT_ROOT/deploy/manifests/daemonset.yaml"
    
    log "Updating daemonset manifest with image: $image_tag"
    
    # Create temporary manifest with updated image
    if ! sed "s#image: PLACEHOLDER_IMAGE_TAG#image: $image_tag#g" "$original_manifest" > "$temp_manifest"; then
        error "Failed to create temporary manifest"
    fi
    
    # Verify that the file was created and has content
    if [[ ! -s "$temp_manifest" ]]; then
        error "Temporary manifest is empty or was not created"
    fi
    
    debug "Temporary manifest created at $temp_manifest"
    echo "$temp_manifest" # Return the path to the temporary manifest
}

deploy_daemonset() {
    local manifest_path="$1"
    local image_tag="$2"
    
    log "Deploying daemonset with image: $image_tag"
    
    # Apply RBAC first
    run_command "kubectl --kubeconfig='$KUBECONFIG_FILE' apply -f '$PROJECT_ROOT/deploy/manifests/rbac.yaml'" \
        "Applying RBAC configuration"
    
    # Apply ConfigMap if it exists
    if [[ -f "$PROJECT_ROOT/deploy/manifests/configmap.yaml" ]]; then
        run_command "kubectl --kubeconfig='$KUBECONFIG_FILE' apply -f '$PROJECT_ROOT/deploy/manifests/configmap.yaml'" \
            "Applying ConfigMap"
    fi
    
    # Apply DaemonSet
    run_command "kubectl --kubeconfig='$KUBECONFIG_FILE' apply -f '$manifest_path'" \
        "Deploying DaemonSet"
}

wait_for_deployment() {
    log "Waiting for daemonset pods to be ready..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log "DRY RUN: Would wait for daemonset rollout"
        return 0
    fi
    
    # Wait for rollout to complete
    if kubectl --kubeconfig="$KUBECONFIG_FILE" rollout status daemonset/weka-nri-cpuset -n kube-system --timeout=300s; then
        log "Daemonset rollout completed successfully"
    else
        log "WARNING: Daemonset rollout did not complete within timeout"
        return 1
    fi
}

show_deployment_status() {
    log "Showing deployment status..."
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log "DRY RUN: Would show deployment status"
        return 0
    fi
    
    echo ""
    echo "=== DaemonSet Status ==="
    kubectl --kubeconfig="$KUBECONFIG_FILE" get daemonset weka-nri-cpuset -n kube-system -o wide
    
    echo ""
    echo "=== Pod Status ==="
    kubectl --kubeconfig="$KUBECONFIG_FILE" get pods -n kube-system -l app=weka-nri-cpuset -o wide
    
    echo ""
    echo "=== Recent Events ==="
    kubectl --kubeconfig="$KUBECONFIG_FILE" get events -n kube-system --field-selector involvedObject.name=weka-nri-cpuset --sort-by='.lastTimestamp' | tail -10
}

cleanup_temp_files() {
    log "Cleaning up temporary files..."
    
    if [[ -f "$PROJECT_ROOT/deploy/manifests/daemonset-temp.yaml" ]]; then
        rm -f "$PROJECT_ROOT/deploy/manifests/daemonset-temp.yaml"
        debug "Removed temporary manifest file"
    fi
}

main() {
    parse_args "$@"
    
    log "Starting build and deploy process..."
    log "Kubeconfig: $KUBECONFIG_FILE"
    log "Registry: $REGISTRY"
    log "Image name: $IMAGE_NAME"
    log "Dry run: $DRY_RUN"
    log "Skip build: $SKIP_BUILD"
    
    # Ensure cleanup happens even if script fails
    trap cleanup_temp_files EXIT
    
    check_dependencies
    
    image_tag=""
    if [[ "$SKIP_BUILD" == "true" ]]; then
        log "Skipping Docker build (--skip-build specified)"
        # Use latest tag or ask user to specify
        image_tag="${REGISTRY}/${IMAGE_NAME}:latest"
        log "Using existing image: $image_tag"
        else
        # Build and push image using Makefile
        local timestamp=$(date +%s)
        local full_image_tag="${REGISTRY}/${IMAGE_NAME}:${timestamp}"
        
        log "Building Docker image using existing Dockerfile: $full_image_tag"
        
        # Use the existing Makefile to build and push the image
        run_command "cd '$PROJECT_ROOT' && make image-push REGISTRY='$REGISTRY' IMAGE_NAME='$IMAGE_NAME' IMAGE_TAG='$timestamp'" \
            "Building and pushing Docker image via Makefile"
        
        image_tag="$full_image_tag"
    fi
    
    # Update manifest with new image tag
    temp_manifest=$(update_daemonset_manifest "$image_tag")
    
    # Deploy to cluster
    deploy_daemonset "$temp_manifest" "$image_tag"
    
    # Wait for deployment
    if wait_for_deployment; then
        # Show status
        show_deployment_status
        
        log ""
        log "=== DEPLOYMENT SUCCESSFUL ==="
        log "Image deployed: $image_tag"
        log ""
        log "Useful commands:"
        log "  Check pod status:  kubectl --kubeconfig=$KUBECONFIG_FILE get pods -n kube-system -l app=weka-nri-cpuset"
        log "  View logs:         kubectl --kubeconfig=$KUBECONFIG_FILE logs -n kube-system -l app=weka-nri-cpuset -f"
        log "  Describe pods:     kubectl --kubeconfig=$KUBECONFIG_FILE describe pods -n kube-system -l app=weka-nri-cpuset"
        log "  Delete daemonset:  kubectl --kubeconfig=$KUBECONFIG_FILE delete daemonset weka-nri-cpuset -n kube-system"
    else
        error "Deployment failed or timed out"
    fi
}

main "$@"