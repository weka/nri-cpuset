# Weka NRI CPUSet Component - Product Requirements Document

## 1. Purpose

A node-resident component shall control CPU and NUMA-memory placement for every container so that:

- Pod owners can pin exact logical CPUs through the sandbox annotation `weka.io/cores-ids`
- Pods that qualify for static CPU-manager treatment obtain exclusive CPUs automatically
- All remaining pods share only the currently unreserved cores
- The placement rules remain valid across node reboots, crashes and live scale-up/down events
- Integer pods can be live-reassigned to different cores when annotated pods create conflicts

## 2. Definitions

| Term | Definition |
|------|------------|
| **Annotated pod** | A pod whose sandbox carries `weka.io/cores-ids: "<CPU-list>"`. The value is a comma/range-formatted list ("0,2-3,8"). The pod may request either fractional CPU shares or whole CPUs. It shall receive unrestricted access to every listed logical CPU. |
| **Integer pod** | A pod without the annotation whose every container meets static CPU-manager criteria: `requests.cpu == limits.cpu`, `requests.memory == limits.memory`, and `limits.cpu` is an integer (`quota % period == 0`). |
| **Shared pod** | Any pod that is neither annotated nor integer. |
| **Reserved core** | A logical CPU already allocated to an annotated or integer pod. |
| **Shared pool** | `online CPUs − reserved cores`; the dynamic set that shared pods are allowed to use. |

## 3. Functional Requirements

### 3.1 Admission-time Behaviour

| Pod type | Handling of cpuset.cpus | Admission errors |
|----------|------------------------|------------------|
| **Annotated** | Use the CPU list verbatim. Overlap with other annotated pods allowed (reference-count). If conflicts with integer pods exist and sufficient free CPUs are available, trigger live reallocation of conflicting integer pods to different exclusive CPU sets. | • CPU offline -or-<br>• CPU reserved by an integer pod with insufficient free CPUs for reallocation. |
| **Integer** | Allocate N exclusive CPUs (N = limits.cpu) from the free set `online − reserved`. Prefer sibling cores (hyperthreads) when available: for 2 cores prefer siblings, for 3 cores prefer full core + one sibling, for additional cores prefer completing partial cores before fragmenting new ones. Must avoid CPUs already allocated to annotated pods. | Not enough free CPUs after excluding annotated pod allocations. |
| **Shared** | Constrain to the current shared pool. | Shared pool would be empty after exclusion. |

### 3.2 Runtime Updates

Whenever the reserved set changes (creation or termination of annotated/integer pods) the component shall:

- Recompute the shared pool and live-update every running shared container's `cpuset.cpus` so that it never overlaps any reserved core
- For annotated pod admission with conflicts: live-reassign conflicting integer containers to new exclusive CPU sets when sufficient free cores exist
- Shared containers started before the component comes up (e.g., after node reboot) shall be corrected in the same way once the component registers
- All live updates maintain transactional semantics: either all affected containers are updated successfully or the operation fails atomically

### 3.3 Memory Placement

#### Annotated Pods (Fixed CPU Allocation)

1. Determine NUMA node(s) of all assigned CPUs
2. Set `cpuset.mems` to the union of NUMA nodes containing the assigned CPUs
   - If CPUs span 1 NUMA node → use that single node
   - If CPUs span 2 NUMA nodes → use both nodes  
   - If CPUs span 4 NUMA nodes → use all four nodes
3. This ensures memory placement is restricted only to NUMA nodes that contain the allocated CPUs
4. Since annotated pods have fixed CPU assignments, NUMA memory placement remains stable

#### Integer Pods (Dynamic CPU Allocation)

1. Leave `cpuset.mems` unchanged (inherit system default)
2. Integer pods may access memory from all NUMA nodes regardless of their CPU constraints
3. This provides maximum memory allocation flexibility since integer pods can be reallocated to different CPUs during conflict resolution
4. Avoiding NUMA memory binding prevents memory placement conflicts during live reassignment

#### Shared Pods

1. Leave `cpuset.mems` unchanged (inherit system default)
2. Shared pods may access memory from all NUMA nodes regardless of their CPU constraints
3. This provides maximum memory allocation flexibility for non-exclusive workloads

### 3.4 Book-keeping and Recovery

Maintain in-memory structures:

- `annotRef[cpu] → refcount` (annotated sharing)
- `intOwner[cpu] → containerID` (integer exclusivity)
- `byCID → {mode, cpu[]}` (reverse lookup)

On every Synchronize event (initial connect, crash recovery) rebuild the above from the runtime-supplied list of live containers and pod annotations.

On container exit release reservations and trigger shared-pool refresh.

For live reassignment operations, maintain transactional integrity by tracking pending changes and rolling back on failure.

### 3.5 CPU Allocation Strategy

#### Sibling Core Preference for Integer Pods

When allocating CPUs for integer containers, prefer sibling cores (hyperthreads) to optimize cache locality and minimize fragmentation:

1. **Complete partial cores first**: Prefer orphan cores where the sibling is already allocated to the same container purpose
2. **For each pair of cores**: Select full physical cores (both siblings) when possible  
3. **For the last partial core**: When requesting odd number of cores, prefer orphan cores where the sibling is already allocated
4. **Fallback**: If optimal sibling allocation is not possible, fall back to any available cores while maintaining exclusivity

This best-effort strategy optimizes for completing physical cores rather than fragmenting new ones.

This strategy optimizes for:
- Better cache locality between related threads
- Reduced memory bandwidth contention  
- Minimized core fragmentation for future allocations
- Improved overall system performance

#### Live Reassignment Priority

During annotated pod admission with integer pod conflicts:

1. **Conflict Detection**: Identify integer containers using CPUs requested by annotated pod
2. **Reallocation Feasibility**: Verify sufficient free CPUs exist for reassignment
3. **Sibling-Aware Reassignment**: Apply sibling preference when reallocating integer containers
4. **Atomic Updates**: Execute all container updates atomically or fail the entire operation
5. **NUMA Consistency**: Maintain optimal NUMA placement during reassignment

## 4. Ordering and Conflicts

Register the component with `index = 99` so it executes after the reference topology-aware

If any later component writes a conflicting value to the same cgroup field, NRI shall abort container creation (transactional safety)t

## 5. Deployment & Cold-boot Guarantee

containerd/k3s configuration must include:

```toml
[plugins."io.containerd.grpc.v1.nri"]
  enable      = true
  plugin_path = ["/opt/nri/plugins"]   # pre-installed binary
  socket_path = "/run/containerd/nri/nri.sock"
  plugin_registration_timeout = "15s"  # or another safe value
```

Place the statically linked binary in `/opt/nri/plugins` on every node before kubelet starts, or provide a systemd unit ordered `Before=kubelet.service`.

A privileged DaemonSet may still be used for upgrades and logging; the host binary guarantees that all workload pods pass through the component even on the very first boot.

## 6. Error Semantics

| Condition | Result |
|-----------|--------|
| Invalid annotation syntax | Pod scheduled with FailedScheduling and explicit error. |
| Insufficient CPUs for integer pod | Same. |
| Insufficient CPUs for integer pod after attempted live reallocation | Same. |
| Live reassignment transaction failure | Pod creation fails, existing containers remain unchanged. |
| Shared pool exhausted | Pod scheduled with FailedScheduling and explicit error. |
| NUMA resolution failure for annotated pod | Same. |

## 7. Feasibility Statement

- `cpuset.cpus` and `cpuset.mems` are writable through NRI ContainerAdjustment and UpdateContainers interfaces
- Live cpuset changes are accepted by containerd and honoured by the kernel; only already-faulted memory pages may remain on their original NUMA nodes
- NUMA node discovery is available via `/sys/devices/system/node/`
- Sibling core topology is discoverable through `/sys/devices/system/cpu/cpu*/topology/`
- Transactional container updates are achievable through NRI's batched UpdateContainers interface
- All stated requirements are implementable with the current NRI and kernel capabilities
