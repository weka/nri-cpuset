# Weka NRI CPUSet Plugin - Design Document

## Overview

The Weka NRI CPUSet plugin is a Kubernetes Node Resource Interface (NRI) plugin that provides intelligent CPU and NUMA memory placement for containers. This document describes the plugin's architecture, design principles, and key implementation decisions.

## Architecture

### High-Level Components

```
┌─────────────────────────────────────────────────────────────────┐
│                    NRI Plugin Interface                         │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  │
│  │   Synchronize   │  │ CreateContainer │  │ RemoveContainer │  │
│  │                 │  │                 │  │                 │  │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│                     Plugin Core                                 │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐  │
│  │ State Manager   │  │ CPU Allocator   │  │ NUMA Manager    │  │
│  │                 │  │                 │  │                 │  │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│              Background Update System                           │
│  ┌─────────────────┐              ┌─────────────────────────────┐│
│  │ Update Queue    │──────────────▶│ Background Goroutine        ││
│  │ (Async Channel) │              │ (UpdateContainers Calls)   ││
│  └─────────────────┘              └─────────────────────────────┘│
└─────────────────────────────────────────────────────────────────┘
```

### Key Design Principles

#### 1. NRI Protocol Compliance

**Critical Requirement**: NRI callbacks must be lightweight and non-blocking.

- **Problem**: Direct `UpdateContainers()` calls from NRI callbacks violate the protocol
- **Solution**: Background update system with async processing
- **Benefit**: Prevents plugin crashes and maintains containerd compatibility

#### 2. Asynchronous Update Processing

All unsolicited container updates are processed asynchronously to avoid blocking NRI callbacks:

```go
// NRI Callback (fast, non-blocking)
func RemoveContainer() error {
    updates := state.RemoveContainer(containerID)
    plugin.queueBackgroundUpdate(updates, "container removal") // Async
    return nil // Return immediately
}

// Background Processing (separate goroutine)
func processBackgroundUpdates() {
    for req := range updateQueue {
        stub.UpdateContainers(req.updates) // Safe to block here
    }
}
```

#### 3. State-First Architecture

The plugin maintains authoritative state and uses it to drive container updates:

- **State Manager**: Tracks all container allocations and reservations
- **Conflict Resolution**: Uses state to detect and resolve CPU conflicts
- **Live Reallocation**: Optimistically updates state, then applies container changes

## Core Components

### State Manager (`pkg/state/`)

Manages container CPU allocations and enforces allocation policies.

**Key Responsibilities:**
- Track annotated, integer, and shared container allocations
- Detect CPU conflicts between container types
- Generate reallocation plans for live updates
- Compute shared CPU pools dynamically

**State Tracking:**
```go
type Manager struct {
    annotRef map[int]int        // CPU -> reference count (sharing allowed)
    intOwner map[int]string     // CPU -> container ID (exclusive)
    byCID    map[string]*ContainerInfo // Container lookup
}
```

### CPU Allocator (`pkg/allocator/`)

Handles CPU allocation logic with NUMA awareness and sibling core preferences.

**Key Features:**
- **Exclusive Allocation**: Integer containers get dedicated CPUs
- **Shared Pool Management**: Fractional containers share remaining CPUs
- **Sibling Core Preference**: Multi-CPU allocations prefer sibling cores
- **Conflict Detection**: Validates allocations against reserved CPUs

### NUMA Manager (`pkg/numa/`)

Provides NUMA topology discovery and CPU list management.

**Capabilities:**
- Parse `/proc/cpuinfo` and `/sys/devices/system/node/`
- CPU list parsing and formatting (`"0-3,8,12-15"`)
- NUMA node to CPU mapping
- Memory node selection for CPU affinity

### Background Update System

**Architecture Decision**: Separate unsolicited `UpdateContainers` calls from NRI callbacks.

```go
type plugin struct {
    updateQueue  chan updateRequest
    updateCtx    context.Context
    updateCancel context.CancelFunc
}

type updateRequest struct {
    updates   []*api.ContainerUpdate
    operation string
}
```

**Benefits:**
- **Protocol Compliance**: NRI callbacks return immediately
- **Error Isolation**: Background update failures don't crash callbacks
- **Serialization**: All updates processed sequentially
- **Debugging**: Clear separation between sync and async operations

## Container Classification

The plugin classifies containers into three types based on resource requirements:

### 1. Annotated Containers
- **Trigger**: `weka.io/cores-ids` annotation present
- **Behavior**: Pin to specific CPUs listed in annotation
- **Sharing**: Multiple annotated containers can share the same CPUs
- **Priority**: Highest (can trigger reallocation of integer containers)

### 2. Integer Containers  
- **Trigger**: CPU requests/limits are integers AND requests == limits
- **Behavior**: Get exclusive CPU allocation
- **Sharing**: No sharing allowed
- **Priority**: Medium (can be reallocated by annotated containers)

### 3. Shared Containers
- **Trigger**: All other containers (fractional CPU requests)
- **Behavior**: Share remaining CPU pool
- **Sharing**: All shared containers use the same CPU set
- **Priority**: Lowest (use whatever CPUs remain)

## Live Reallocation System

### Conflict Resolution Process

When an annotated container requests CPUs that conflict with integer containers:

1. **Conflict Detection**: State manager identifies conflicting integer containers
2. **Reallocation Planning**: Generate new CPU assignments for affected containers  
3. **State Update**: Optimistically update internal state
4. **Background Updates**: Queue container updates for async processing

```go
func AllocateAnnotatedWithReallocation(pod *api.PodSandbox, alloc Allocator) 
    (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
    
    // Try normal allocation first
    result, err := alloc.HandleAnnotatedContainer(pod, integerReserved)
    if err == nil {
        return result, nil, nil // No conflicts
    }
    
    // Handle conflicts with live reallocation
    reallocationPlan := planReallocation(conflictingContainers)
    updates := executeReallocationPlan(reallocationPlan)
    
    // Return adjustment + updates for background processing
    return adjustment, updates, nil
}
```

### Transactional Updates

The reallocation system uses optimistic state updates:

1. **Plan Generation**: Create reallocation plan without modifying state
2. **State Projection**: Calculate post-reallocation state for validation
3. **Optimistic Update**: Apply state changes assuming success
4. **Background Execution**: Queue container updates asynchronously

## Error Handling Philosophy

### No Panic Recovery

**Design Decision**: Remove all panic recovery to expose bugs directly.

- **Rationale**: Hidden panics mask real issues and make debugging difficult
- **Trade-off**: Plugin may crash on bugs, but bugs are immediately visible
- **Benefit**: Forces proper error handling and reveals race conditions

### Error Propagation

Errors are properly propagated through the call chain:

```go
func CreateContainer() (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
    adjustment, updates, err := handleAnnotatedContainer(pod, container)
    if err != nil {
        return nil, nil, fmt.Errorf("annotated container handling failed: %w", err)
    }
    // Queue updates asynchronously (no error blocking)
    queueBackgroundUpdate(updates, "live reallocation")
    return adjustment, nil, nil
}
```

### Background Update Failures

Background update failures are logged but don't affect NRI callback success:

```go
func processBackgroundUpdates() {
    _, err := p.stub.UpdateContainers(req.updates)
    if err != nil {
        fmt.Printf("ERROR: Background UpdateContainers failed for %s: %v\n", 
            req.operation, err)
        // Log but continue - don't crash the plugin
    }
}
```

## Threading and Concurrency

### Single-Threaded NRI Callbacks

NRI callbacks are processed sequentially by the NRI runtime, eliminating most race conditions in the main processing path.

### Background Update Serialization

All `UpdateContainers` calls are serialized through a single background goroutine:

```go
func processBackgroundUpdates() {
    for {
        select {
        case <-p.updateCtx.Done():
            return
        case req := <-p.updateQueue:
            // Process updates sequentially
            p.stub.UpdateContainers(req.updates)
        }
    }
}
```

### State Manager Locking

The state manager uses read-write locks for thread safety:

```go
type Manager struct {
    mu sync.RWMutex
    // ... state maps
}

func (m *Manager) GetReservedCPUs() []int {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return m.getReservedCPUsUnsafe()
}
```

## Performance Considerations

### Memory Efficiency

- **Container Tracking**: Only active containers stored in memory
- **CPU Sets**: Use map[int]struct{} for efficient CPU set operations
- **State Cleanup**: Containers removed from state on deletion

### CPU Allocation Efficiency

- **Online CPU Detection**: Cache online CPUs to avoid repeated syscalls
- **NUMA Awareness**: Prefer local NUMA nodes for memory allocation
- **Sibling Core Logic**: Use topology information for optimal placement

### Update Batching

Background updates are processed individually but could be batched for efficiency:

```go
// Future optimization: batch multiple updates
func processBatchedUpdates(updates []updateRequest) {
    var allUpdates []*api.ContainerUpdate
    for _, req := range updates {
        allUpdates = append(allUpdates, req.updates...)
    }
    p.stub.UpdateContainers(allUpdates)
}
```

## Security Considerations

### Privileged Operations

The plugin requires privileged access for:
- Reading `/proc/cpuinfo` and `/sys/devices/system/node/`
- Making `UpdateContainers` calls through NRI
- Accessing container cgroup information

### Container Isolation

- **CPU Isolation**: Integer containers get exclusive CPU access
- **Memory Placement**: NUMA memory binding for optimal performance
- **No Cross-Container Access**: Plugin only modifies CPU/memory placement

## Debugging and Observability

### Structured Logging

The plugin uses structured logging with operation context:

```go
fmt.Printf("Processing %d background container updates for %s\n", 
    len(req.updates), req.operation)
fmt.Printf("DEBUG: Conflicts detected for annotated pod %s/%s, attempting live reallocation: %v\n", 
    pod.Namespace, pod.Name, err)
```

### Debug Information

Key debug points include:
- Container classification decisions
- CPU allocation choices
- Conflict detection and resolution
- Background update processing
- State synchronization

### Error Context

Errors include sufficient context for debugging:

```go
return fmt.Errorf("failed to allocate CPUs for container %s in pod %s/%s: %w", 
    container.Name, pod.Namespace, pod.Name, err)
```

## Future Enhancements

### Potential Improvements

1. **Update Batching**: Batch multiple background updates for efficiency
2. **Retry Logic**: Add retry logic for failed background updates
3. **Metrics Integration**: Add Prometheus metrics for allocation statistics
4. **Configuration**: Make allocation policies configurable
5. **CPU Topology**: Enhanced NUMA and cache topology awareness

### Scalability Considerations

- **Large Clusters**: State manager scales to hundreds of containers per node
- **High Churn**: Background update system handles rapid container creation/deletion
- **Memory Usage**: Linear growth with number of active containers

## Testing Strategy

### Unit Tests

- **State Manager**: Test allocation logic and conflict resolution
- **CPU Allocator**: Test NUMA awareness and sibling core preferences  
- **Background Updates**: Test async update processing and error handling

### Integration Tests

- **E2E Tests**: Full container lifecycle with real NRI interface
- **Live Reallocation**: Test complex conflict resolution scenarios
- **Error Injection**: Test plugin behavior under various failure conditions

### Performance Tests

- **Container Churn**: High-frequency container creation/deletion
- **Resource Pressure**: Test behavior with limited CPU resources
- **Concurrent Operations**: Multiple simultaneous container operations

This design provides a robust, NRI-compliant foundation for intelligent CPU management in Kubernetes clusters while maintaining high performance and reliability.