# Weka CPU/NUMA Placement Component – Project Brief

*Node-Resource-Interface (NRI) plugin written in Go 1.22 that enforces CPU pinning and NUMA-aware memory placement in Kubernetes.*

---

## Tech Stack
- Go 1.24 - static builds (`CGO_ENABLED=0`)
- containerd 2.0+ with NRI v0.9
- Kubernetes ≥ 1.28
- Ginkgo 2 + Gomega for Go unit/integration tests
- Bats for bash-level smoke tests
- Kind for local e2e CI; real cluster e2e uses `$KUBECONFIG`

---

## Project Structure
| Path | Purpose |
|------|---------|
| `cmd/weka-cpuset/` | `main.go`, CLI flags, stub init |
| `pkg/allocator/` | core allocation logic (pure Go, unit-testable) |
| `pkg/numa/` | NUMA helpers (reads `/sys/devices/system/node`) |
| `pkg/state/` | in-memory maps + `Synchronize` rebuild |
| `test/e2e/` | Ginkgo tests that deploy the plugin to a *live* cluster |
| `hack/` | helper scripts (kind bootstrap, image build/push) |
| `docs/` | design notes, architecture diagrams |
| `deploy/` | K8s manifests (DS, RBAC), Helm chart |

> **Do Not Touch** `vendor/` (managed by `go mod vendor`).

---

## Commands
| Task | Command |
|------|---------|
| Local build | `make build` → `./bin/weka-cpuset` |
| Lint & static analysis | `make lint` (golangci-lint) |
| Unit tests | `make test` |
| Integration tests (no K8s) | `make test-integration` |
| **Kind e2e (RECOMMENDED)** | `make test-e2e-kind` |
| **Live cluster e2e** | `make test-e2e-live` |
| **Kind cluster setup** | `make kind-up` |
| **Kind cluster cleanup** | `make kind-down` |
| Image build | `make image` |
| Helm chart package | `make chart` |

---

## Coding Conventions
- Follow **Effective Go** plus `go fumpt`.
- Keep every file ≤300 LOC; highlight design issues if >600 LOC.
- Prefer functional options pattern for shared helpers.
- Public APIs need `godoc` comments.

---

## Placement Rules (executable spec)
1. **Annotated pod** (`weka.io/cores-ids` present)  
   - Use the list verbatim.  
   - If **integer semantics** (`requests=limits`, CPU whole number) ⇒ treat as *exclusive*; else *shared*.  
   - Pin memory (`cpuset.mems`) to NUMA node(s) of annotated CPUs.
2. **Pure-integer pod** (static CPU manager rules, no annotation)  
   - Allocate *N* free logical CPUs from `online − exclusive`.  
3. **Shared pod**  
   - Restrict to *shared pool* (`online − exclusive`).  
4. On every change to an exclusive set, recompute the shared pool and live-update running shared containers via `UpdateContainers`.

---

## End-to-End Testing Strategy

**⚠️ IMPORTANT: Always prefer Kind-based testing for development and CI.**

### Kind-based Testing (RECOMMENDED)
The kind-based approach automatically provisions a local Kubernetes cluster with:
- containerd 2.0+ with NRI v0.9 support enabled
- Static CPU manager policy configured 
- Proper NRI plugin directories mounted
- Plugin image pre-loaded

**Quick start:**
```bash
# Full automated e2e test with kind cluster
make test-e2e-kind

# Or step by step:
make kind-up        # Create kind cluster
make image          # Build plugin image  
make test-e2e-kind  # Run tests
make kind-down      # Cleanup
```

**Kind cluster features:**
- Multi-node setup (1 control-plane + 1 worker)
- NRI socket properly configured at `/run/containerd/nri/nri.sock`
- Plugin directory at `/opt/nri/plugins` 
- CPU manager with system/kube reserved resources
- Automatic image loading and plugin deployment

### Live Cluster Testing
For testing against existing clusters (CI/staging/prod):
```bash
# Prerequisites: 
# - Cluster must have containerd 2.0+ with NRI enabled
# - kubectl configured with proper RBAC permissions

export KUBECONFIG=~/.kube/config
export TEST_NS=wekaplugin-e2e
make test-e2e-live

# Or with custom settings:
KUBECONFIG=~/.kube/prod TEST_NS=weka-test make test-e2e-live
```

**Live cluster requirements:**
- containerd 2.0+ with NRI plugin support enabled
- Kubernetes ≥ 1.28 with static CPU manager policy
- RBAC permissions for DaemonSet deployment
- Nodes with sufficient CPU cores for testing

### Docs/PRD
- PRDs located in docs/prd.md 
- When implementing new functionality ensure it is covered in prd.md
