# Weka NRI CPUSet Component

A Kubernetes NRI (Node Resource Interface) plugin that provides intelligent CPU and NUMA memory placement for containers with support for live reassignment and sibling core allocation.

## Project Overview

- **Purpose**: Pin exact CPUs via annotations, automatic exclusive allocation for integer pods, shared pool management
- **Key Features**: Live CPU reassignment, sibling-aware allocation, NUMA memory placement, transactional updates
- **Architecture**: Node-resident NRI plugin with state management and conflict resolution

## Quick Navigation

### Local Development
```bash
# Build binary
make build

# Run unit tests
make test

# Run all verification (unit + lint + format)
make verify

# Build Docker image
make image IMAGE_TAG=dev

# Clean build artifacts
make clean
```

### Live Cluster Operations

#### Prerequisites & KUBECONFIG Setup
**Before running any cluster operations, ensure you have a valid KUBECONFIG:**

1. **Check if KUBECONFIG is already set:**
   ```bash
   echo $KUBECONFIG
   ```

2. **If not set, you must specify one of:**
   - Set environment variable: `export KUBECONFIG=/path/to/kubeconfig`
   - Or use inline for single command: `KUBECONFIG=/path/to/kubeconfig <command>`

3. **Test cluster connectivity:**
   ```bash
   kubectl cluster-info
   ```

#### Build & Deploy
```bash
# Automated build and deploy to live cluster
./hack/build-and-deploy.sh --kubeconfig /path/to/kubeconfig

# With custom registry
./hack/build-and-deploy.sh --kubeconfig /path/to/kubeconfig --registry my-registry.com:5000

# Dry run (see what would happen)
./hack/build-and-deploy.sh --kubeconfig /path/to/kubeconfig --dry-run --debug

# Skip Docker build (use existing image)
./hack/build-and-deploy.sh --kubeconfig /path/to/kubeconfig --skip-build

# If KUBECONFIG env var is set, you can omit --kubeconfig flag:
export KUBECONFIG=/path/to/kubeconfig
./hack/build-and-deploy.sh
```

#### E2E Testing

**Test Strategy Guidelines:**
- **Work on specific broken test** → Run only that test until it passes
- **Once specific test passes** → Run full test suite to ensure no regressions
- **Use focused tests for faster iteration during development**

##### Running All E2E Tests
```bash
# Run all E2E tests against live cluster (use when all tests should pass)
KUBECONFIG=/path/to/kubeconfig make test-e2e-live

# Direct script execution
KUBECONFIG=/path/to/kubeconfig ./hack/e2e-live.sh

# With extended timeout for slow clusters  
KUBECONFIG=/path/to/kubeconfig TEST_TIMEOUT=60m ./hack/e2e-live.sh
```

##### Running Specific E2E Tests (Recommended for Development)
```bash
# Run specific test suites using Ginkgo focus patterns:

# Test CPU conflict resolution (the test we've been working on)
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="CPU Conflict Resolution" ./test/e2e/

# Test just annotated pod functionality
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="Annotated Pod" ./test/e2e/

# Test live reallocation features
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="Live Reallocation" ./test/e2e/

# Test integer pod allocation
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="Integer Pod" ./test/e2e/

# Test recovery and synchronization
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="Recovery" ./test/e2e/

# Test shared pod functionality  
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="Shared Pod" ./test/e2e/

# Run specific test by exact description
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="should reallocate integer containers when annotated pod creates conflicts" ./test/e2e/

# Run tests that match any keyword
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="conflict" ./test/e2e/

# Run with different verbosity levels
KUBECONFIG=/path/to/kubeconfig ginkgo -vv --focus="CPU Conflict" ./test/e2e/  # Very verbose
KUBECONFIG=/path/to/kubeconfig ginkgo --focus="CPU Conflict" ./test/e2e/      # Normal

# Skip certain tests
KUBECONFIG=/path/to/kubeconfig ginkgo -v --skip="NUMA|Memory" ./test/e2e/

# Run with timeout control
KUBECONFIG=/path/to/kubeconfig ginkgo -v --timeout=10m --focus="Annotated Pod" ./test/e2e/
```

##### Running Tests from Specific Files
```bash
# Run all tests in a specific file
KUBECONFIG=/path/to/kubeconfig ginkgo -v ./test/e2e/annotated_pod_test.go
KUBECONFIG=/path/to/kubeconfig ginkgo -v ./test/e2e/integer_pod_test.go
KUBECONFIG=/path/to/kubeconfig ginkgo -v ./test/e2e/live_reallocation_test.go

# Run specific test file with focus pattern
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="should pin CPUs according to annotation" ./test/e2e/annotated_pod_test.go
```

##### Testing Workflow for Bug Fixes
```bash
# 1. Work on specific failing test first (faster iteration)
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="should reallocate integer containers when annotated pod creates conflicts" ./test/e2e/

# 2. Once that test passes, run related test group  
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="CPU Conflict Resolution" ./test/e2e/

# 3. Finally, run all tests to check for regressions
KUBECONFIG=/path/to/kubeconfig make test-e2e-live
```

### Kind Cluster (Isolated Testing)
```bash
# Create kind cluster, deploy, and test (self-contained)
make test-e2e-kind

# Step by step
make kind-up           # Create kind cluster
make test-e2e-kind     # Deploy and test  
make kind-down         # Cleanup
```

### Verification & Testing
```bash
# Unit tests only
make test

# Integration tests (no K8s required)
make test-integration

# All verification including E2E
make verify-all

# Specific package tests
go test -v ./pkg/numa/...
go test -v ./pkg/allocator/...
go test -v ./pkg/state/...
```

## Key Files & Directories

- **`cmd/weka-cpuset/main.go`** - Main plugin entry point and NRI interface implementation
- **`pkg/allocator/`** - CPU allocation logic with sibling awareness and conflict resolution
- **`pkg/state/`** - Container state management and live reallocation orchestration
- **`pkg/numa/`** - NUMA topology discovery and CPU list parsing
- **`hack/build-and-deploy.sh`** - Main automation script for build/deploy
- **`hack/e2e-live.sh`** - E2E test runner for live clusters
- **`test/e2e/`** - End-to-end test suites
- **`deploy/manifests/`** - Kubernetes deployment manifests
- **`docs/prd.md`** - Complete product requirements and technical specifications

## Common Commands for AI Assistance

### Debugging Deployment
```bash
# Check DaemonSet status
kubectl get daemonset -n kube-system weka-cpuset

# View plugin logs
kubectl logs -n kube-system -l app=weka-cpuset -f

# Verify NRI registration
kubectl exec -n kube-system <pod-name> -- ls -la /opt/nri/plugins/

# Test with sample annotated pod
kubectl apply -f docs/samples/annotated-daemonset.yaml
```

### Development Workflow
```bash
# Make changes to code
# Run unit tests
make test

# Build and deploy changes
./hack/build-and-deploy.sh --kubeconfig /path/to/kubeconfig

# Run specific E2E test to verify fix
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="specific test description" ./test/e2e/

# Once specific test passes, run full E2E suite
KUBECONFIG=/path/to/kubeconfig make test-e2e-live
```

## Prerequisites
- Go 1.21+
- Docker with registry access
- kubectl configured for target cluster
- KUBECONFIG pointing to cluster with NRI support

## Troubleshooting Quick Reference
1. **Build fails**: Check Go version and dependencies with `go mod tidy`
2. **Deploy fails**: Verify Docker registry access and kubectl connectivity
3. **Tests fail**: Check cluster has sufficient CPU resources and NRI support
4. **Plugin not loading**: Verify containerd NRI configuration and binary placement
5. **Malformed annotations**: Check CPU list syntax (fixed in recent updates)
6. **KUBECONFIG issues**: Verify path exists and kubectl can connect to cluster

For detailed implementation explanations, architecture decisions, and comprehensive requirements, see `docs/prd.md` and other documentation in the `docs/` directory.
