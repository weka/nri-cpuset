# Weka NRI CPUSet Component

A Kubernetes NRI (Node Resource Interface) plugin that provides intelligent CPU and NUMA memory placement for containers with support for live reassignment and sibling core allocation.

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/wekafs/weka-nri-cpuset)](https://github.com/wekafs/weka-nri-cpuset/releases)

## Overview

The Weka NRI CPUSet Component enables fine-grained CPU and NUMA memory management for containerized workloads in Kubernetes. It supports three types of workloads:

- **Annotated pods**: Pin exact CPUs via annotations (`weka.io/cores-ids`)
- **Integer pods**: Automatic exclusive CPU allocation for static CPU-manager eligible pods
- **Shared pods**: Dynamic shared pool management for remaining workloads

## Key Features

- 🎯 **Precise CPU Pinning**: Pin containers to specific logical CPUs using annotations
- 🔄 **Live Reassignment**: Dynamically reallocate integer containers when conflicts arise
- 🧠 **NUMA-Aware**: Intelligent NUMA memory placement based on CPU assignments
- 🔧 **Sibling Core Optimization**: Prefer hyperthreaded siblings for better cache locality
- ⚡ **Conflict Resolution**: Automatic resolution when annotated pods conflict with integer pods
- 🛡️ **Transactional Updates**: Atomic operations with rollback on failure
- 🏗️ **Cold-boot Guarantee**: Works from the first pod on node restart

## Quick Start

### Prerequisites

- Kubernetes cluster with NRI-enabled container runtime (containerd or CRI-O)
- Go 1.21+ (for building from source)
- `kubectl` configured for your cluster

### Deployment Methods

#### Method 1: DaemonSet Deployment (Recommended)

Deploy using Kubernetes manifests for easy management and updates:

```bash
# Deploy the NRI plugin as a DaemonSet
kubectl apply -f https://raw.githubusercontent.com/wekafs/weka-nri-cpuset/main/deploy/manifests/rbac.yaml
kubectl apply -f https://raw.githubusercontent.com/wekafs/weka-nri-cpuset/main/deploy/manifests/configmap.yaml
kubectl apply -f https://raw.githubusercontent.com/wekafs/weka-nri-cpuset/main/deploy/manifests/daemonset.yaml
```

Or with Helm:

```bash
helm install weka-nri-cpuset ./deploy/helm/weka-nri-cpuset
```

**Note**: When using the DaemonSet method, you'll need to specify the image tag from our [releases page](https://github.com/wekafs/weka-nri-cpuset/releases) in the manifest, as we don't currently publish a `:latest` tag.

#### Method 2: Direct Host Installation

For production deployments requiring cold-boot guarantees, install directly on cluster nodes:

##### For standard Kubernetes clusters:

```bash
# Download the latest release
wget https://github.com/wekafs/weka-nri-cpuset/releases/download/v0.1.0/weka-cpuset-linux-amd64

# Install on each node
sudo cp weka-cpuset-linux-amd64 /opt/nri/plugins/99-weka-cpuset
sudo chmod +x /opt/nri/plugins/99-weka-cpuset

# Restart containerd to register the plugin
sudo systemctl restart containerd
```

##### For K3s clusters:

Use our automated configuration script that handles K3s-specific setup:

```bash
# Configure all nodes in the cluster
./hack/reconfigure-k3s.sh --kubeconfig ~/.kube/config

# Or configure a single node (useful for testing)
./hack/reconfigure-k3s.sh --kubeconfig ~/.kube/config --single-node 0
```

The K3s script:
- Builds and deploys the plugin binary to `/opt/nri/plugins/`
- Configures K3s for NRI support
- Disables static CPU manager (incompatible with NRI)
- Restarts K3s with proper settings
- Verifies installation

## Configuration

### Container Runtime Configuration

#### Containerd Configuration

Ensure NRI is enabled in your containerd configuration (`/etc/containerd/config.toml`):

```toml
[plugins."io.containerd.grpc.v1.nri"]
  enable      = true
  plugin_path = ["/opt/nri/plugins"]
  socket_path = "/run/containerd/nri/nri.sock"
  plugin_registration_timeout = "15s"
```

#### K3s Configuration

For K3s, add to `/etc/rancher/k3s/config.yaml`:

```yaml
kubelet-arg:
  - "cpu-manager-policy=none"  # Disable static CPU manager
```

**Important**: The static CPU manager must be disabled as it conflicts with NRI-based CPU management.

### Plugin Configuration

The plugin supports configuration via environment variables or ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: weka-nri-config
  namespace: kube-system
data:
  debug: "true"
  numa-memory-policy: "strict"  # strict|preferred|none
```

## Usage Examples

### Annotated Pods

Pin containers to specific CPUs using the `weka.io/cores-ids` annotation:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: high-performance-app
  annotations:
    weka.io/cores-ids: "0,2-3,8"  # Pin to CPUs 0, 2, 3, and 8
spec:
  containers:
  - name: app
    image: my-app:latest
    resources:
      requests:
        cpu: "2"
        memory: "4Gi"
      limits:
        cpu: "4"
        memory: "4Gi"
```

### Integer Pods (Automatic Exclusive Allocation)

Pods with integer CPU requests get exclusive CPUs automatically:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: compute-workload
spec:
  containers:
  - name: compute
    image: compute-app:latest
    resources:
      requests:
        cpu: "4"      # Integer CPU request
        memory: "8Gi"
      limits:
        cpu: "4"      # Must equal requests for exclusivity
        memory: "8Gi"
```

### Shared Pods

Regular pods share the remaining CPU pool:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: web-server
spec:
  containers:
  - name: web
    image: nginx:latest
    resources:
      requests:
        cpu: "100m"   # Fractional CPU - uses shared pool
        memory: "256Mi"
```

### Real-World Example: Mixed Workload

```yaml
# High-priority database pinned to specific cores
apiVersion: v1
kind: Pod
metadata:
  name: database
  annotations:
    weka.io/cores-ids: "0-3"  # Dedicated to cores 0-3
spec:
  containers:
  - name: db
    image: postgres:14
    resources:
      limits: { cpu: "4", memory: "8Gi" }

---
# ML training job gets exclusive cores automatically  
apiVersion: v1
kind: Pod
metadata:
  name: ml-training
spec:
  containers:
  - name: trainer
    image: tensorflow:latest
    resources:
      requests: { cpu: "8", memory: "16Gi" }
      limits: { cpu: "8", memory: "16Gi" }  # Integer = exclusive

---
# Web services share remaining cores
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web-tier
spec:
  replicas: 3
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
      - name: web
        image: nginx:latest
        resources:
          requests: { cpu: "200m", memory: "512Mi" }  # Shared pool
```

## CPU Allocation Strategy

### Sibling Core Preference

The plugin optimizes CPU allocation by preferring sibling cores (hyperthreads):

- **2 cores**: Prefer two siblings from the same physical core
- **3 cores**: Prefer one complete physical core (2 siblings) + one additional core
- **Additional cores**: Complete partially allocated cores before fragmenting new ones

### Live Reassignment

When annotated pods conflict with existing integer pod allocations:

1. **Conflict Detection**: Identify integer containers using conflicting CPUs
2. **Feasibility Check**: Verify sufficient free CPUs exist for reassignment
3. **Sibling-Aware Reallocation**: Apply sibling preference during reassignment
4. **Atomic Updates**: Execute all container updates atomically or fail entirely

### NUMA Memory Placement

- **Annotated pods**: Memory restricted to NUMA nodes containing assigned CPUs
- **Integer pods**: Unrestricted memory access for maximum flexibility during reassignment
- **Shared pods**: Unrestricted memory access

## Monitoring and Troubleshooting

### Check Plugin Status

```bash
# Verify plugin is loaded
kubectl get pods -n kube-system -l app=weka-nri-cpuset

# Check plugin logs
kubectl logs -n kube-system -l app=weka-nri-cpuset -f

# Verify NRI registration (on cluster nodes)
ls -la /opt/nri/plugins/
sudo lsof /run/nri/nri.sock
```

### Debug Container CPU Assignments

```bash
# Check container CPU assignments
cat /sys/fs/cgroup/cpuset/kubepods/*/cpuset.cpus

# View NUMA memory assignments  
cat /sys/fs/cgroup/cpuset/kubepods/*/cpuset.mems

# Monitor CPU usage by core
htop  # or similar tool
```

### Common Issues

**Plugin not loading**:
- Verify NRI is enabled in container runtime configuration
- Check plugin binary permissions and location
- Restart container runtime after configuration changes

**CPU conflicts**:
- Check for annotation syntax errors (`weka.io/cores-ids`)
- Verify sufficient free CPUs for integer pod allocation
- Review plugin logs for conflict resolution details

**Performance issues**:
- Monitor CPU usage distribution across cores
- Check for NUMA memory allocation mismatches
- Verify sibling core allocation is working as expected

## Development and Building

### Build from Source

```bash
# Clone the repository
git clone https://github.com/wekafs/weka-nri-cpuset.git
cd weka-nri-cpuset

# Build for local testing
make build

# Build Docker image
make image IMAGE_TAG=dev

# Run unit tests
make test

# Run end-to-end tests (requires cluster)
make test-e2e-live KUBECONFIG=/path/to/kubeconfig
```

### Contributing

We welcome contributions! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

### Testing

The project includes comprehensive test suites:

- **Unit tests**: Test individual components and allocation logic
- **Integration tests**: Test plugin integration with container runtime
- **End-to-end tests**: Test complete workflows in live Kubernetes clusters

## Architecture

### Components

- **`cmd/weka-cpuset/`**: Main plugin entry point and NRI interface
- **`pkg/allocator/`**: CPU allocation logic with sibling awareness
- **`pkg/state/`**: Container state management and live reallocation
- **`pkg/numa/`**: NUMA topology discovery and CPU parsing

### Ordering and Conflicts

The plugin registers with `index = 99` to execute after topology-aware plugins. NRI's transactional semantics ensure that conflicting changes abort container creation safely.

## Compatibility

### Kubernetes Versions

| Kubernetes | Containerd | Status |
|------------|------------|--------|
| 1.24+ | 1.6.0+ | ✅ Supported |
| 1.20-1.23 | 1.5.0+ | ✅ Supported |

### Container Runtimes

| Runtime | Version | NRI Support | Status |
|---------|---------|-------------|--------|
| containerd | 1.7.0+ | Native | ✅ Fully supported |
| containerd | 1.6.0+ | Native | ✅ Fully supported |
| CRI-O | 1.23+ | Native | ✅ Supported |

### Operating Systems

| OS | Architecture | Status |
|----|--------------|--------|
| Linux | x86_64 | ✅ Fully supported |
| Linux | ARM64 | ✅ Supported |
| Linux | s390x | ✅ Supported |

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Support

- 📖 **Documentation**: [docs/](docs/) directory contains detailed technical documentation
- 🐛 **Issues**: Report bugs via [GitHub Issues](https://github.com/wekafs/weka-nri-cpuset/issues)
- 💬 **Discussions**: Join conversations in [GitHub Discussions](https://github.com/wekafs/weka-nri-cpuset/discussions)

## Related Projects

- [containerd/nri](https://github.com/containerd/nri) - Node Resource Interface specification
- [containers/nri-plugins](https://github.com/containers/nri-plugins) - Community NRI plugins
- [kubernetes/node-feature-discovery](https://github.com/kubernetes-sigs/node-feature-discovery) - Node feature detection

---

**Weka NRI CPUSet Component** - Intelligent CPU and NUMA management for Kubernetes workloads. 