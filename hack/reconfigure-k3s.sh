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
  $0 --kubeconfig ~/kc/operator-demo
  $0 --kubeconfig ~/.kube/config --ssh-username ubuntu
  $0 --kubeconfig ~/kc/operator-demo --single-node 0
  $0 --kubeconfig ~/kc/operator-demo --single-node 2
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

build_binary() {
    log "Building weka-cpuset binary for x86_64..."
    
    cd "$PROJECT_ROOT"
    
    if ! command -v go >/dev/null 2>&1; then
        error "Go not found in PATH"
    fi
    
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/weka-cpuset-linux-amd64 ./cmd/weka-cpuset/
    
    if [[ ! -f "bin/weka-cpuset-linux-amd64" ]]; then
        error "Failed to build binary"
    fi
    
    log "Binary built successfully: bin/weka-cpuset-linux-amd64"
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

deploy_binary_to_node() {
    local node_ip="$1"
    local username="$2"
    local binary_path="$3"
    
    log "Deploying binary and wrapper script to $username@$node_ip..."
    
    scp -o StrictHostKeyChecking=no "$binary_path" "$username@$node_ip:/tmp/weka-cpuset"
    
    ssh -o StrictHostKeyChecking=no "$username@$node_ip" << 'EOF'
        # Stop any running services to prevent conflicts during re-deploy
        sudo systemctl stop weka-nri.service || true
        sudo systemctl disable weka-nri.service || true
        
        # Clean up existing plugin installations
        sudo rm -f /opt/nri/plugins/weka-cpuset
        sudo rm -f /opt/nri/plugins/*weka-cpuset*
        
        # Create directories
        sudo mkdir -p /opt/nri/plugins
        sudo mkdir -p /var/log
        
        # Move binary to /usr/local/bin
        sudo mv /tmp/weka-cpuset /usr/local/bin/weka-cpuset
        sudo chmod +x /usr/local/bin/weka-cpuset
        sudo chown root:root /usr/local/bin/weka-cpuset
        
        # Create wrapper script in /opt/nri/plugins
        sudo tee /opt/nri/plugins/99-weka-cpuset > /dev/null << 'WRAPPER_EOF'
#!/bin/bash
# Weka NRI Plugin Wrapper Script
# This script wraps the actual binary and handles logging

BINARY="/usr/local/bin/weka-cpuset"
LOGFILE="/var/log/weka-cpuset.log"

# Ensure log file exists and has proper permissions
touch "$LOGFILE" 2>/dev/null || sudo touch "$LOGFILE"
sudo chown root:root "$LOGFILE" 2>/dev/null || true
sudo chmod 644 "$LOGFILE" 2>/dev/null || true

# Function to log with timestamp
log_with_timestamp() {
    while IFS= read -r line; do
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] $line" >> "$LOGFILE"
    done
}

# Log startup
echo "[$(date '+%Y-%m-%d %H:%M:%S')] Starting Weka NRI Plugin (PID: $$)" >> "$LOGFILE"
echo "[$(date '+%Y-%m-%d %H:%M:%S')] Command: $BINARY $*" >> "$LOGFILE"

# Execute the binary and capture both stdout and stderr, with timestamps
exec "$BINARY" "$@" 2>&1 | log_with_timestamp
WRAPPER_EOF

        sudo chmod +x /opt/nri/plugins/99-weka-cpuset
        sudo chown root:root /opt/nri/plugins/99-weka-cpuset
        
        # Remove systemd service file if it exists
        sudo rm -f /etc/systemd/system/weka-nri.service
        sudo systemctl daemon-reload || true
EOF
    
    debug "Binary and wrapper script deployed successfully to $node_ip"
}


configure_k3s_nri() {
    local node_ip="$1"
    local username="$2"
    
    log "Configuring K3s for NRI on $username@$node_ip..."
    
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
        
        # Create updated K3s config that preserves existing settings and adds NRI
        sudo tee /etc/rancher/k3s/config.yaml > /dev/null << K3S_EOF
# Preserve existing configuration
${existing_config}

# NRI and CPU manager configuration added by weka-nri setup
# Note: NRI is enabled by default in containerd, no feature gate needed
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

        # Create NRI plugin directories
        sudo mkdir -p /etc/nri/plugins
        sudo mkdir -p /opt/nri/plugins
        sudo mkdir -p /run/containerd/nri
        
        echo "K3s configuration updated to enable NRI while preserving existing settings"
EOF
    
    debug "K3s configured for NRI on $node_ip"
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
            if sudo systemctl is-active --quiet k3s && [ -S /run/nri/nri.sock ]; then
                # Additional check that containerd is listening on NRI socket
                if sudo lsof /run/nri/nri.sock >/dev/null 2>&1; then
                    echo "K3s and NRI socket ready"
                    break
                fi
            fi
            sleep 2
            timeout=$((timeout - 2))
        done
        
        if [ $timeout -le 0 ]; then
            echo "K3s/NRI failed to be ready within 300 seconds"
            exit 1
        fi
        
        # Wait for plugin auto-discovery and startup
        sleep 10
        
        echo "K3s restarted successfully"
EOF
    
    debug "K3s restarted on $node_ip"
}

verify_configuration() {
    local node_ip="$1"
    local username="$2"
    
    log "Verifying configuration on $username@$node_ip..."
    
    ssh -o StrictHostKeyChecking=no "$username@$node_ip" << 'EOF'
        echo "=== Service Status ==="
        sudo systemctl is-active k3s.service || echo "k3s.service: FAILED"
        
        echo "=== NRI Socket ==="
        if [ -S /run/nri/nri.sock ]; then
            echo "NRI socket exists: /run/nri/nri.sock"
        else
            echo "NRI socket missing!"
        fi
        
        echo "=== Plugin Files ==="
        if [ -x /opt/nri/plugins/99-weka-cpuset ]; then
            echo "Plugin wrapper script exists and is executable"
        else
            echo "Plugin wrapper script missing or not executable!"
        fi
        
        if [ -x /usr/local/bin/weka-cpuset ]; then
            echo "Plugin binary exists in /usr/local/bin"
        else
            echo "Plugin binary missing from /usr/local/bin!"
        fi
        
        echo "=== Plugin Process Status ==="
        if pgrep -f "weka-cpuset" >/dev/null 2>&1; then
            echo "Weka plugin process is running:"
            ps aux | grep weka-cpuset | grep -v grep
        else
            echo "No weka-cpuset process found"
        fi
        
        echo "=== Plugin Logs ==="
        if [ -f /var/log/weka-cpuset.log ]; then
            echo "--- Recent plugin logs (last 10 lines) ---"
            tail -n 10 /var/log/weka-cpuset.log
        else
            echo "Plugin log file not found at /var/log/weka-cpuset.log"
        fi
        
        echo "=== K3s/Containerd Logs ==="
        echo "--- k3s logs (last 5 lines) ---"
        sudo journalctl -u k3s.service --since "5 minutes ago" --no-pager -n 5 | grep -i nri || echo "No NRI-related logs found"
EOF
}

configure_node() {
    local node_ip="$1"
    local username="$2"
    local binary_path="$3"
    
    log "Configuring node: $username@$node_ip"
    
    test_ssh_connection "$node_ip" "$username"
    deploy_binary_to_node "$node_ip" "$username" "$binary_path"
    configure_k3s_nri "$node_ip" "$username"
    restart_k3s "$node_ip" "$username"
    verify_configuration "$node_ip" "$username"
    
    log "Node $node_ip configured successfully"
}

main() {
    parse_args "$@"
    
    log "Starting K3s NRI reconfiguration..."
    log "Kubeconfig: $KUBECONFIG_FILE"
    log "SSH Username: $SSH_USERNAME"
    if [[ "$SINGLE_NODE_MODE" == "true" ]]; then
        log "Mode: Single node (index: $NODE_INDEX)"
    else
        log "Mode: All nodes"
    fi
    
    # Build binary
    build_binary
    
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
        configure_node "${node_ips[$NODE_INDEX]}" "$SSH_USERNAME" "$PROJECT_ROOT/bin/weka-cpuset-linux-amd64"
    else
        log "Configuring all nodes..."
        for node_ip in "${node_ips[@]}"; do
            configure_node "$node_ip" "$SSH_USERNAME" "$PROJECT_ROOT/bin/weka-cpuset-linux-amd64"
        done
    fi
    
    log "K3s NRI reconfiguration completed successfully!"
    log ""
    log "Next steps:"
    log "1. Test with a sample pod that has CPU annotations"
    if [[ "$SINGLE_NODE_MODE" == "true" ]]; then
        log "2. If successful, run without --single-node to configure all nodes"
    fi
    log "3. Monitor logs: kubectl --kubeconfig=$KUBECONFIG_FILE logs -l app=weka-nri -f"
}

main "$@"