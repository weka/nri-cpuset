#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

KUBECONFIG_FILE=""
SSH_USERNAME="root"
DEBUG=false
SINGLE_NODE_MODE=false
NODE_INDEX=0

usage() {
    cat << EOF
Usage: $0 --kubeconfig PATH [options]

Required:
  --kubeconfig PATH    Path to kubeconfig file

Options:
  --ssh-username USER  SSH username (default: root)
  --debug             Enable debug output
  --single-node IDX   Configure single node at index IDX (0-based)
  --help              Show this help

Examples:
  $0 --kubeconfig /path/to/kubeconfig.yaml
  $0 --kubeconfig ~/.kube/config --ssh-username ubuntu
  $0 --kubeconfig /path/to/kubeconfig.yaml --single-node 0
  $0 --kubeconfig /path/to/kubeconfig.yaml --single-node 2
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
            --single-node)
                SINGLE_NODE_MODE=true
                NODE_INDEX="$2"
                shift 2
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

    if [[ "$SINGLE_NODE_MODE" == "true" && -z "$NODE_INDEX" ]]; then
        error "Missing required node index for --single-node option"
    fi

    if [[ "$SINGLE_NODE_MODE" == "true" && ! "$NODE_INDEX" =~ ^[0-9]+$ ]]; then
        error "Node index must be a non-negative integer, got: $NODE_INDEX"
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
        error "Cannot SSH to $username@$node_ip"
    fi
    
    debug "SSH connection to $username@$node_ip successful"
}

cleanup_existing_plugin() {
    local node_ip="$1"
    local username="$2"
    
    log "Cleaning up existing plugin installation on $username@$node_ip..."
    
    ssh -o StrictHostKeyChecking=no "$username@$node_ip" << 'EOF'
        # Stop any running services
        sudo systemctl stop weka-nri.service || true
        sudo systemctl disable weka-nri.service || true
        
        # Remove plugin files
        sudo rm -f /opt/nri/plugins/weka-cpuset
        sudo rm -f /opt/nri/plugins/*weka-cpuset*
        sudo rm -f /usr/local/bin/weka-cpuset
        
        # Remove systemd service file
        sudo rm -f /etc/systemd/system/weka-nri.service
        sudo systemctl daemon-reload || true
        
        # Stop any running plugin processes
        sudo pkill -f weka-cpuset || true
        
        echo "Plugin cleanup completed"
EOF
    
    debug "Plugin cleanup completed on $node_ip"
}

configure_k3s_for_daemonset() {
    local node_ip="$1"
    local username="$2"
    
    log "Configuring K3s for NRI (daemonset mode) on $username@$node_ip..."
    
    ssh -o StrictHostKeyChecking=no "$username@$node_ip" << 'EOF'
        # Create NRI configuration directory
        sudo mkdir -p /etc/rancher/k3s
        
        # Backup the original K3s config if it exists
        if [ -f /etc/rancher/k3s/config.yaml ]; then
            sudo cp /etc/rancher/k3s/config.yaml /etc/rancher/k3s/config.yaml.backup.$(date +%s)
            echo "Original config backed up"
        fi
        
        # Read existing config to preserve it
        existing_config=""
        if [ -f /etc/rancher/k3s/config.yaml ]; then
            existing_config=$(cat /etc/rancher/k3s/config.yaml)
        fi
        
        # Create updated K3s config that disables static CPU manager and enables NRI
        sudo tee /etc/rancher/k3s/config.yaml > /dev/null << K3S_EOF
# Preserve existing configuration
${existing_config}

# NRI and CPU manager configuration for daemonset deployment
# Disable static CPU manager to allow NRI plugin to manage CPU assignments
kubelet-arg:
  - "cpu-manager-policy=none"
K3S_EOF

        # Remove static CPU manager state files to ensure clean restart
        echo "Cleaning up static CPU manager state..."
        sudo rm -f /var/lib/kubelet/cpu_manager_state
        sudo rm -rf /var/lib/kubelet/cpusets/
        
        # Check if there are kubelet args in the systemd service that override config
        if sudo systemctl cat k3s.service | grep -q "cpu-manager-policy=static"; then
            echo "WARNING: Found static CPU manager policy in systemd service"
            echo "Attempting to modify systemd service to disable static CPU manager..."
            
            # Backup original service file
            sudo cp /etc/systemd/system/k3s.service /etc/systemd/system/k3s.service.backup.$(date +%s)
            
            # Modify the service file to remove static CPU manager policy
            sudo sed -i 's/--kubelet-arg=--cpu-manager-policy=static//g' /etc/systemd/system/k3s.service
            sudo sed -i 's/--kubelet-arg=--reserved-cpus=[0-9]*//g' /etc/systemd/system/k3s.service
            
            # Clean up any double spaces
            sudo sed -i 's/  */ /g' /etc/systemd/system/k3s.service
            
            echo "Modified systemd service to remove static CPU manager policy"
            
            # Reload systemd to pick up service changes
            sudo systemctl daemon-reload
        fi

        # Remove any custom containerd config template that might be problematic
        sudo rm -f /var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl

        # Create NRI socket directory (daemonset will mount this)
        sudo mkdir -p /var/run/nri
        sudo chmod 755 /var/run/nri
        
        echo "K3s configuration updated for daemonset deployment"
EOF
    
    debug "K3s configured for daemonset on $node_ip"
}

restart_k3s() {
    local node_ip="$1"
    local username="$2"
    
    log "Restarting K3s on $username@$node_ip..."
    
    ssh -o StrictHostKeyChecking=no "$username@$node_ip" << 'EOF'
        # Stop K3s services
        sudo systemctl stop k3s || true
        sudo systemctl stop k3s-agent || true
        
        # Start K3s
        sudo systemctl start k3s
        
        # Wait for K3s to be fully ready and NRI socket available
        timeout=300
        while [ $timeout -gt 0 ]; do
            if sudo systemctl is-active --quiet k3s; then
                # Check if NRI socket directory is available
                if [ -d /var/run/nri ]; then
                    echo "K3s ready and NRI socket directory available"
                    break
                fi
            fi
            sleep 2
            timeout=$((timeout - 2))
        done
        
        if [ $timeout -le 0 ]; then
            echo "K3s failed to be ready within 300 seconds"
            exit 1
        fi
        
        echo "K3s restarted successfully"
EOF
    
    debug "K3s restarted on $node_ip"
}

verify_node_configuration() {
    local node_ip="$1"
    local username="$2"
    
    log "Verifying node configuration on $username@$node_ip..."
    
    ssh -o StrictHostKeyChecking=no "$username@$node_ip" << 'EOF'
        echo "=== Service Status ==="
        sudo systemctl is-active k3s.service || echo "k3s.service: FAILED"
        
        echo "=== NRI Socket Directory ==="
        if [ -d /var/run/nri ]; then
            echo "NRI socket directory exists: /var/run/nri"
            ls -la /var/run/nri/ || echo "Directory empty (expected before daemonset deployment)"
        else
            echo "NRI socket directory missing!"
        fi
        
        echo "=== CPU Manager Configuration ==="
        if [ -f /var/lib/kubelet/cpu_manager_state ]; then
            echo "WARNING: CPU manager state file still exists"
        else
            echo "CPU manager state file removed (good)"
        fi
        
        echo "=== Plugin Cleanup Status ==="
        if pgrep -f "weka-cpuset" >/dev/null 2>&1; then
            echo "WARNING: Old plugin process still running"
            ps aux | grep weka-cpuset | grep -v grep
        else
            echo "No old plugin processes running (good)"
        fi
        
        if [ -f /opt/nri/plugins/99-weka-cpuset ] || [ -f /usr/local/bin/weka-cpuset ]; then
            echo "WARNING: Old plugin files still exist"
        else
            echo "Old plugin files cleaned up (good)"
        fi
EOF
}

configure_node() {
    local node_ip="$1"
    local username="$2"
    
    log "Configuring node: $username@$node_ip"
    
    test_ssh_connection "$node_ip" "$username"
    cleanup_existing_plugin "$node_ip" "$username"
    configure_k3s_for_daemonset "$node_ip" "$username"
    restart_k3s "$node_ip" "$username"
    verify_node_configuration "$node_ip" "$username"
    
    log "Node $node_ip configured successfully"
}

main() {
    parse_args "$@"
    
    log "Starting K3s NRI reconfiguration for daemonset deployment..."
    log "Kubeconfig: $KUBECONFIG_FILE"
    log "SSH Username: $SSH_USERNAME"
    if [[ "$SINGLE_NODE_MODE" == "true" ]]; then
        log "Mode: Single node (index: $NODE_INDEX)"
    else
        log "Mode: All nodes"
    fi
    
    # Get node IPs
    node_ips=($(get_node_ips "$KUBECONFIG_FILE"))
    
    if [[ ${#node_ips[@]} -eq 0 ]]; then
        error "No nodes found in cluster"
    fi
    
    log "Found ${#node_ips[@]} nodes: ${node_ips[*]}"
    
    if [[ "$SINGLE_NODE_MODE" == "true" ]]; then
        if [[ $NODE_INDEX -ge ${#node_ips[@]} ]]; then
            error "Node index $NODE_INDEX is out of range. Available nodes: ${#node_ips[@]} (0-$((${#node_ips[@]}-1)))"
        fi
        log "Single node mode - configuring node at index $NODE_INDEX: ${node_ips[$NODE_INDEX]}"
        configure_node "${node_ips[$NODE_INDEX]}" "$SSH_USERNAME"
    else
        log "Configuring all nodes..."
        for node_ip in "${node_ips[@]}"; do
            configure_node "$node_ip" "$SSH_USERNAME"
        done
    fi
    
    log "K3s NRI reconfiguration for daemonset deployment completed successfully!"
    log ""
    log "Next steps:"
    log "1. Use the build-and-deploy.sh script to build and deploy the daemonset"
    log "2. Monitor deployment: kubectl --kubeconfig=$KUBECONFIG_FILE get pods -n kube-system -l app=weka-nri-cpuset"
    log "3. Check logs: kubectl --kubeconfig=$KUBECONFIG_FILE logs -n kube-system -l app=weka-nri-cpuset -f"
}

main "$@"