#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

KUBECONFIG_FILE=""
SSH_USERNAME="root"
DEBUG=false

usage() {
    cat << EOF
Usage: $0 --kubeconfig PATH [options]

Required:
  --kubeconfig PATH    Path to kubeconfig file

Options:
  --ssh-username USER  SSH username (default: root)
  --debug             Enable debug output
  --help              Show this help

Description:
  One-time cleanup script to remove the weka-cpuset plugin from all nodes
  in the k8s cluster. This will:
  - Stop any running plugin processes
  - Remove plugin binaries and wrapper scripts
  - Clean up systemd services
  - Remove plugin directories

Examples:
  $0 --kubeconfig /path/to/kubeconfig.yaml
  $0 --kubeconfig ~/.kube/config --ssh-username ubuntu
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
            --ssh-username)
                SSH_USERNAME="$2"
                shift 2
                ;;
            --debug)
                DEBUG=true
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

get_node_ips() {
    local kubeconfig="$1"
    debug "Discovering node IPs from kubeconfig: $kubeconfig"
    
    kubectl --kubeconfig="$kubeconfig" get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}' | tr ' ' '\n' | grep -v '^$'
}

test_ssh_connection() {
    local node_ip="$1"
    local username="$2"
    
    debug "Testing SSH connection to $username@$node_ip"
    
    if ! ssh -o ConnectTimeout=10 -o StrictHostKeyChecking=no "$username@$node_ip" "echo 'SSH connection successful'" >/dev/null 2>&1; then
        log "WARNING: Cannot SSH to $username@$node_ip - skipping this node"
        return 1
    fi
    
    debug "SSH connection to $username@$node_ip successful"
    return 0
}

cleanup_plugin_from_node() {
    local node_ip="$1"
    local username="$2"
    
    log "Cleaning up weka-cpuset plugin from $username@$node_ip..."
    
    ssh -o StrictHostKeyChecking=no "$username@$node_ip" << 'EOF'
        echo "=== Starting Plugin Cleanup ==="
        
        # Stop and disable systemd service
        echo "Stopping and disabling systemd services..."
        sudo systemctl stop weka-nri.service 2>/dev/null || true
        sudo systemctl disable weka-nri.service 2>/dev/null || true
        
        # Kill any running plugin processes
        echo "Killing running plugin processes..."
        if pgrep -f "weka-cpuset" >/dev/null 2>&1; then
            echo "Found running weka-cpuset processes:"
            ps aux | grep weka-cpuset | grep -v grep
            sudo pkill -f "weka-cpuset" || true
            sleep 2
            # Force kill if still running
            sudo pkill -9 -f "weka-cpuset" 2>/dev/null || true
            echo "Plugin processes terminated"
        else
            echo "No running weka-cpuset processes found"
        fi
        
        # Remove plugin files
        echo "Removing plugin files..."
        sudo rm -f /opt/nri/plugins/weka-cpuset
        sudo rm -f /opt/nri/plugins/*weka-cpuset*
        sudo rm -f /opt/nri/plugins/99-weka-cpuset
        sudo rm -f /usr/local/bin/weka-cpuset
        
        # Remove systemd service file
        echo "Removing systemd service file..."
        sudo rm -f /etc/systemd/system/weka-nri.service
        sudo systemctl daemon-reload 2>/dev/null || true
        
        # Remove log files
        echo "Removing log files..."
        sudo rm -f /var/log/weka-cpuset.log*
        
        # Clean up any backup files from previous configurations
        echo "Removing backup files..."
        sudo rm -f /etc/rancher/k3s/config.yaml.backup.*
        sudo rm -f /etc/systemd/system/k3s.service.backup.*
        
        # Remove empty plugin directories if they exist
        if [ -d /opt/nri/plugins ] && [ -z "$(ls -A /opt/nri/plugins)" ]; then
            echo "Removing empty plugin directory..."
            sudo rmdir /opt/nri/plugins 2>/dev/null || true
        fi
        
        if [ -d /opt/nri ] && [ -z "$(ls -A /opt/nri)" ]; then
            echo "Removing empty NRI directory..."
            sudo rmdir /opt/nri 2>/dev/null || true
        fi
        
        echo "=== Plugin Cleanup Summary ==="
        echo "✓ Systemd service stopped and disabled"
        echo "✓ Plugin processes terminated"
        echo "✓ Plugin files removed"
        echo "✓ Service files removed"
        echo "✓ Log files removed"
        echo "✓ Backup files cleaned up"
        
        # Verify cleanup
        echo ""
        echo "=== Verification ==="
        if pgrep -f "weka-cpuset" >/dev/null 2>&1; then
            echo "❌ WARNING: Plugin processes still running!"
            ps aux | grep weka-cpuset | grep -v grep
        else
            echo "✓ No plugin processes running"
        fi
        
        plugin_files_found=false
        for file in /opt/nri/plugins/*weka-cpuset* /usr/local/bin/weka-cpuset; do
            if [ -e "$file" ]; then
                echo "❌ WARNING: Plugin file still exists: $file"
                plugin_files_found=true
            fi
        done
        
        if [ "$plugin_files_found" = false ]; then
            echo "✓ No plugin files found"
        fi
        
        if [ -f /etc/systemd/system/weka-nri.service ]; then
            echo "❌ WARNING: Systemd service file still exists"
        else
            echo "✓ Systemd service file removed"
        fi
        
        echo "=== Cleanup completed for node $(hostname) ==="
EOF
    
    if [ $? -eq 0 ]; then
        log "Plugin cleanup completed successfully on $node_ip"
    else
        log "WARNING: Plugin cleanup encountered errors on $node_ip"
    fi
}

main() {
    parse_args "$@"
    
    log "Starting one-time plugin cleanup for all nodes..."
    log "Kubeconfig: $KUBECONFIG_FILE"
    log "SSH Username: $SSH_USERNAME"
    
    # Get node IPs
    node_ips=($(get_node_ips "$KUBECONFIG_FILE"))
    
    if [[ ${#node_ips[@]} -eq 0 ]]; then
        error "No nodes found in cluster"
    fi
    
    log "Found ${#node_ips[@]} nodes: ${node_ips[*]}"
    
    successful_cleanups=0
    failed_cleanups=0
    
    for node_ip in "${node_ips[@]}"; do
        log "Processing node: $node_ip"
        
        if test_ssh_connection "$node_ip" "$SSH_USERNAME"; then
            cleanup_plugin_from_node "$node_ip" "$SSH_USERNAME"
            ((successful_cleanups++))
        else
            log "Skipping node $node_ip due to SSH connection failure"
            ((failed_cleanups++))
        fi
        
        echo "" # Add spacing between nodes
    done
    
    log "=== CLEANUP SUMMARY ==="
    log "Successfully cleaned: $successful_cleanups nodes"
    log "Failed/Skipped: $failed_cleanups nodes"
    log "Total nodes: ${#node_ips[@]}"
    
    if [ $failed_cleanups -eq 0 ]; then
        log "✓ All nodes cleaned up successfully!"
        log ""
        log "Next steps:"
        log "1. Run reconfigure-k3s-daemonset.sh to prepare nodes for daemonset deployment"
        log "2. Use build-and-deploy.sh to deploy the plugin as a daemonset"
    else
        log "⚠ Some nodes could not be cleaned up. Check SSH connectivity and re-run if needed."
    fi
}

main "$@"