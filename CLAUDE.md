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

# Build Docker image (supports configurable registry)
make image REGISTRY=my-registry.com IMAGE_NAME=weka-nri-cpuset IMAGE_TAG=dev

# Build and push Docker image
make image-push REGISTRY=my-registry.com IMAGE_NAME=weka-nri-cpuset IMAGE_TAG=dev

# Build with timestamp (used by build-and-deploy script)
make image-with-timestamp REGISTRY=my-registry.com IMAGE_NAME=weka-nri-cpuset

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
# Automated build and deploy to live cluster (uses existing Dockerfile via Makefile)
./hack/build-and-deploy.sh --kubeconfig /path/to/kubeconfig

# With custom registry
./hack/build-and-deploy.sh --kubeconfig /path/to/kubeconfig --registry my-registry.com:5000

# Dry run (see what would happen)
./hack/build-and-deploy.sh --kubeconfig /path/to/kubeconfig --dry-run --debug

# Skip Docker build (use existing image)
./hack/build-and-deploy.sh --kubeconfig /path/to/kubeconfig --skip-build

# Alternative: Use Makefile directly for just building/pushing
make image-push REGISTRY=my-registry.com:5000 IMAGE_NAME=weka-nri-cpuset
```

#### E2E Testing

**Test Strategy Guidelines:**
- **Work on specific broken test** → Run only that test until it passes
- **Once specific test passes** → Run full test suite to ensure no regressions
- **Use focused tests for faster iteration during development**

##### Running All E2E Tests (Optimized for Parallel Execution)
```bash
# Run all E2E tests with optimized defaults (8 parallel workers, preserve failures)
KUBECONFIG=/path/to/kubeconfig make test-e2e-live

# Direct script execution with optimized defaults
KUBECONFIG=/path/to/kubeconfig ./hack/e2e-live.sh

# Default behavior (as of latest optimization):
# - TEST_PARALLEL=8 (8 parallel workers for faster execution)
# - PRESERVE_ON_FAILURE=true (preserve failed pods for debugging)
# - CONTINUE_ON_FAILURE=false (stop on parallel test failures)

# Customization examples:
KUBECONFIG=/path/to/kubeconfig TEST_PARALLEL=4 ./hack/e2e-live.sh  # Fewer workers
KUBECONFIG=/path/to/kubeconfig PRESERVE_ON_FAILURE=false ./hack/e2e-live.sh  # Fast cleanup
KUBECONFIG=/path/to/kubeconfig CONTINUE_ON_FAILURE=true ./hack/e2e-live.sh  # Run all tests

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

##### Live Reallocation Testing
```bash
# Test live reallocation functionality (implemented)
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="Live CPU Reallocation" ./test/e2e/

# Test specific live reallocation scenarios
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="should reallocate integer containers when annotated pod creates conflicts" ./test/e2e/

# Test conflict resolution scenarios
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="conflict" ./test/e2e/

# Run live reallocation tests with shorter timeout (faster feedback)
KUBECONFIG=/path/to/kubeconfig ginkgo -v --timeout=2m --focus="Live CPU Reallocation" ./test/e2e/
```

##### E2E Test Script (Unified with Build-Deploy)
```bash
# Run full E2E suite with fresh build and deployment (uses build-and-deploy.sh internally)
KUBECONFIG=/path/to/kubeconfig ./hack/e2e-live.sh

# Run E2E with custom registry (builds fresh image)
KUBECONFIG=/path/to/kubeconfig PLUGIN_REGISTRY=my-registry.com:5000 ./hack/e2e-live.sh

# Run E2E with existing image (skips building)
KUBECONFIG=/path/to/kubeconfig PLUGIN_IMAGE=my-registry.com/weka-nri-cpuset:v1.2.3 ./hack/e2e-live.sh

# Skip build and use existing image at default registry
KUBECONFIG=/path/to/kubeconfig SKIP_BUILD=true ./hack/e2e-live.sh

# Force plugin redeployment before testing
KUBECONFIG=/path/to/kubeconfig FORCE_DEPLOY=true ./hack/e2e-live.sh

# Run with custom configuration
KUBECONFIG=/path/to/kubeconfig TEST_PARALLEL=4 PRESERVE_ON_FAILURE=true ./hack/e2e-live.sh
```

##### Alternative Ginkgo Installation Methods
```bash
# If ginkgo CLI is not available, install it
go install github.com/onsi/ginkgo/v2/ginkgo@latest

# Or use go run (ensures version compatibility with go.mod)
KUBECONFIG=/path/to/kubeconfig go run github.com/onsi/ginkgo/v2/ginkgo run -v --focus="test pattern" ./test/e2e/

# Check ginkgo version compatibility
ginkgo version
go run github.com/onsi/ginkgo/v2/ginkgo version
```

##### Debugging Plugin Issues
```bash
# View plugin logs for debugging
KUBECONFIG=/path/to/kubeconfig kubectl logs -n kube-system -l app=weka-nri-cpuset -f

# Check recent logs with grep for specific patterns
KUBECONFIG=/path/to/kubeconfig kubectl logs -n kube-system -l app=weka-nri-cpuset --since=2m | grep DEBUG
KUBECONFIG=/path/to/kubeconfig kubectl logs -n kube-system -l app=weka-nri-cpuset --since=5m | grep -E "(realloc|conflict|error)" -i

# Monitor plugin status during tests
KUBECONFIG=/path/to/kubeconfig kubectl get pods -n kube-system -l app=weka-nri-cpuset -w

# Check for errors in plugin deployment
KUBECONFIG=/path/to/kubeconfig kubectl describe pods -n kube-system -l app=weka-nri-cpuset
```

##### Test Environment Management
```bash
# Clean up leftover test pods (important for test reliability)
KUBECONFIG=/path/to/kubeconfig kubectl delete pods --all -n wekaplugin-e2e --grace-period=0 --force --ignore-not-found=true

# Check test namespace status
KUBECONFIG=/path/to/kubeconfig kubectl get pods -n wekaplugin-e2e -o wide

# Wait for daemonset rollout after deployment
KUBECONFIG=/path/to/kubeconfig kubectl rollout status daemonset/weka-nri-cpuset -n kube-system --timeout=120s

# Force restart plugin to clear state
KUBECONFIG=/path/to/kubeconfig kubectl rollout restart daemonset/weka-nri-cpuset -n kube-system
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

### Advanced Development Patterns

#### Implementing New Tests
When implementing tests for features like live reallocation:

```bash
# 1. First, implement the test logic in test/e2e/*_test.go
# 2. Use focused runs for rapid iteration
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="new test name" ./test/e2e/

# 3. Add debug logging to both test and implementation code for troubleshooting
# 4. Use shorter timeouts during development (30s instead of 5m)
# 5. Clean test environment between runs to avoid interference

# Example: General test implementation pattern
# - Create test resources and wait for ready state
# - Verify expected behavior through status checks
# - Clean up test resources
```

#### Debugging Plugin Issues
```bash
# Common debugging workflow:

# 1. Check for specific log patterns
KUBECONFIG=/path/to/kubeconfig kubectl logs -n kube-system -l app=weka-nri-cpuset --since=2m | grep "DEBUG"

# 2. Monitor plugin behavior during test execution
KUBECONFIG=/path/to/kubeconfig kubectl logs -n kube-system -l app=weka-nri-cpuset -f &
# Run your test
KUBECONFIG=/path/to/kubeconfig ginkgo -v --focus="test pattern" ./test/e2e/

# 3. Check for errors or unexpected behavior
KUBECONFIG=/path/to/kubeconfig kubectl logs -n kube-system -l app=weka-nri-cpuset --since=5m | grep -E "(error|fail)" -i
```

## Prerequisites
- Go 1.21+
- Docker with registry access
- kubectl configured for target cluster
- KUBECONFIG pointing to cluster with NRI support

## Test Artifact Collection & Analysis

### Automated Test Reporting

E2E tests automatically generate comprehensive failure reports in `test/e2e/.test-reports/` with unique execution IDs for systematic debugging.

#### Test Execution Structure
```
test/e2e/.test-reports/
└── 20240722-143052-a1b2c3d4/          # Execution ID (timestamp + UUID)
    ├── execution-summary.md            # Main overview and analysis workflow
    ├── test-failures-index.md          # List of all failed tests
    ├── failures/                       # Individual test failure reports
    │   └── [test-name]-[timestamp]/
    │       ├── summary.md              # Test failure analysis
    │       ├── pod-states.md           # All pod states across namespaces
    │       ├── plugin-logs.txt         # Plugin daemon logs
    │       ├── cluster-snapshot.md     # Node resources and DaemonSet status
    │       └── test-debug-output.txt   # Custom test debug info
    ├── logs/                          # Raw log files
    ├── pod-describes/                 # Detailed pod descriptions
    ├── plugin-logs/                   # Plugin logs by node
    ├── cluster-state/                 # Cluster snapshots
    └── test-debug/                    # Custom debug output by test
```

#### Analysis Workflow for LLMs

1. **Start with `execution-summary.md`**: Overview of test run and directory structure
2. **Check `test-failures-index.md`**: Quick list of all failures with links
3. **For each failure**: Navigate to `failures/[test-name]/summary.md` for detailed analysis
4. **Review artifacts**: Pod states, plugin logs, and cluster state for root cause

#### Key Analysis Files (LLM-Optimized)

**Quick Assessment:**
- `test-failures-index.md` - Overview of all failures
- `execution-summary.md` - Test run context and artifact navigation

**Per-Test Deep Dive:**
- `failures/[test]/summary.md` - Comprehensive failure report with analysis tips
- `failures/[test]/pod-states.md` - Pod states across all test namespaces
- `failures/[test]/plugin-logs.txt` - Plugin behavior during test execution

#### Exploration Commands

```bash
# List all test executions
ls -la test/e2e/.test-reports/

# Quick failure overview for specific execution
cat test/e2e/.test-reports/[execution-id]/test-failures-index.md

# Detailed analysis of specific failure
cat test/e2e/.test-reports/[execution-id]/failures/[test-name]/summary.md

# Review plugin behavior for failure
cat test/e2e/.test-reports/[execution-id]/failures/[test-name]/plugin-logs.txt
```

#### LLM Analysis Patterns

When analyzing test failures, follow this pattern:

1. **Identify execution context** from `execution-summary.md`
2. **Categorize failures** from `test-failures-index.md`
3. **Analyze failure patterns**:
   - Pod creation issues → Check `pod-states.md`
   - Resource conflicts → Review `plugin-logs.txt`
   - Plugin behavior → Examine `cluster-snapshot.md`
4. **Root cause analysis** using test-specific debug output

### Manual Debugging (Fallback)

If artifact collection is not available, use manual debugging:

```bash
# Check test namespace status
KUBECONFIG=/path/to/kubeconfig kubectl get pods -n wekaplugin-e2e-w* -o wide

# Get plugin logs for recent failures
KUBECONFIG=/path/to/kubeconfig kubectl logs -n kube-system -l app=weka-nri-cpuset --since=10m

# Describe failing pods
KUBECONFIG=/path/to/kubeconfig kubectl describe pods -n wekaplugin-e2e-w1

# Check for CreateContainerError or scheduling issues
KUBECONFIG=/path/to/kubeconfig kubectl get events -n wekaplugin-e2e-w1 --sort-by='.lastTimestamp'
```

## Troubleshooting Quick Reference
1. **Build fails**: Check Go version and dependencies with `go mod tidy`
2. **Deploy fails**: Verify Docker registry access and kubectl connectivity
3. **Tests fail**: Check cluster has sufficient CPU resources and NRI support; review `test/e2e/.test-reports/` for detailed failure analysis
4. **Plugin not loading**: Verify containerd NRI configuration and binary placement
5. **Malformed annotations**: Check CPU list syntax (fixed in recent updates)
6. **KUBECONFIG issues**: Verify path exists and kubectl can connect to cluster
7. **Plugin behavior issues**: Use debug logging and plugin logs to diagnose (see debugging commands above)
8. **Test timing issues**: Clean test namespace between runs and add explicit waits where needed
9. **Sporadic failures**: Use test artifact reports in `test/e2e/.test-reports/` for systematic analysis

## Implementation Notes

### General Development Infrastructure
Key patterns for reliable plugin development and testing:

- **State Management**: Plugin maintains internal state that must be properly synchronized
- **Debug Logging**: Strategic logging helps diagnose complex plugin behavior
- **Test Environment**: Clean environment between test runs prevents interference
- **Container Lifecycle**: Proper handling of container creation, updates, and removal events

### Common Development Patterns
```go
// Example: Adding debug logging for troubleshooting
fmt.Printf("DEBUG: Processing container %s in pod %s/%s\n", container.Id, pod.Namespace, pod.Name)
```

For detailed implementation explanations, architecture decisions, and comprehensive requirements, see `docs/prd.md` and other documentation in the `docs/` directory.
