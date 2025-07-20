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
| Kind e2e | `hack/kind-e2e.sh` |
| Real-cluster e2e | `hack/e2e-live.sh $KUBECONFIG` |
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
### Local (Kind)
1. Spin up kind cluster with matching containerd (`hack/kind-up.sh`).  
2. Load plugin image into cluster node.  
3. Apply CRDs & DaemonSet.  
4. Run Ginkgo:  
   - verifies admission decisions for annotated / integer / shared pods  
   - asserts cpuset & mems via `kubectl exec cat /proc/self/status`  
   - kills pods to ensure pool refresh logic.

### Real Cluster
`hack/e2e-live.sh` assumes:
```bash
export KUBECONFIG=~/.kube/config   # points to any alive cluster
export TEST_NS=wekaplugin-e2e
```

### Docs/PRD
- PRDs located in docs/prd.md 
- When implementing new functionality ensure it is covered in prd.md
