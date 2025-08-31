package state

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/weka/nri-cpuset/pkg/allocator"
	containerutil "github.com/weka/nri-cpuset/pkg/container"
	"github.com/weka/nri-cpuset/pkg/numa"
)

// safeShortID safely truncates a container ID to 12 characters for logging
func safeShortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

const (
	WekaAnnotation = "weka.io/cores-ids"
)

// Container mode constants
const (
	ModeShared    = "shared"
	ModeInteger   = "integer"
	ModeAnnotated = "annotated"
)

type ContainerInfo struct {
	ID        string
	Mode      string
	CPUs      []int
	PodID     string
	PodUID    string
	CreatedAt time.Time
}

type Manager struct {
	// GLOBAL ALLOCATION LOCK - all allocation operations must acquire this
	// This eliminates all race conditions between allocation, reallocation, and state updates
	allocationMu sync.Mutex

	// State access lock - separate from allocation lock to prevent deadlocks
	mu sync.RWMutex

	// Synchronization state tracking
	synchronized bool // True after initial Synchronize() completes
	syncCond     *sync.Cond // Condition variable to wait for synchronization

	// annotRef[cpu] -> refcount (annotated sharing)
	annotRef map[int]int

	// intOwner[cpu] -> containerID (integer exclusivity)
	intOwner map[int]string

	// byCID -> ContainerInfo (reverse lookup)
	byCID map[string]*ContainerInfo

	// pendingReallocationPlan stores the plan that needs to be applied after successful updates
	// This is now instance-based to prevent race conditions between concurrent allocations
	pendingReallocationPlan []ReallocationPlan

	// Debouncing for shared pool updates during rapid container removals
	sharedPoolUpdatePending bool
	sharedPoolDebounceTimer *time.Timer
	pendingUpdateCallback   func([]*api.ContainerUpdate)

	// Container query callback for state synchronization
	containerQueryCallback  func() ([]*api.Container, error)
	// Pod and container query callback for proper classification
	podContainerQueryCallback func() ([]*api.PodSandbox, []*api.Container, error)

	// Note: In true stateless architecture, no cleanup timers needed
	// State is always queried fresh from the runtime
}

// GetAllocationLock returns the global allocation lock for external synchronization
// This allows callers like Synchronize() to participate in the global locking protocol
func (m *Manager) GetAllocationLock() *sync.Mutex {
	return &m.allocationMu
}

// waitForSynchronization blocks until initial Synchronize() completes
// All allocation operations must call this first to ensure consistent state
func (m *Manager) waitForSynchronization() {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	for !m.synchronized {
		fmt.Printf("DEBUG: Waiting for initial synchronization to complete...\n")
		m.syncCond.Wait()
	}
	fmt.Printf("DEBUG: Initial synchronization confirmed - proceeding with operation\n")
}

func NewManager() *Manager {
	m := &Manager{
		annotRef:             make(map[int]int),
		intOwner:             make(map[int]string),
		byCID:                make(map[string]*ContainerInfo),
		synchronized:         false,
		// Note: We maintain state based on Synchronize() + incremental updates
	}
	m.syncCond = sync.NewCond(&m.mu)
	return m
}

// SetUpdateCallback sets the callback function for debounced shared pool updates
func (m *Manager) SetUpdateCallback(callback func([]*api.ContainerUpdate)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingUpdateCallback = callback
}

// SetContainerQueryCallback sets the callback function for querying actual running containers
func (m *Manager) SetContainerQueryCallback(callback func() ([]*api.Container, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.containerQueryCallback = callback
}

// SetPodContainerQueryCallback sets the callback function for querying both pods and containers for proper classification
func (m *Manager) SetPodContainerQueryCallback(callback func() ([]*api.PodSandbox, []*api.Container, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.podContainerQueryCallback = callback
}





// scheduleSharedPoolUpdate schedules a debounced shared pool update
func (m *Manager) scheduleSharedPoolUpdate(alloc Allocator) {
	const debounceDelay = 500 * time.Millisecond

	// Cancel existing timer if any
	if m.sharedPoolDebounceTimer != nil {
		m.sharedPoolDebounceTimer.Stop()
	}

	// Set the update as pending
	m.sharedPoolUpdatePending = true

	// Schedule new timer
	m.sharedPoolDebounceTimer = time.AfterFunc(debounceDelay, func() {
		fmt.Printf("DEBUG: scheduleSharedPoolUpdate timer fired, acquiring locks\n")
		// CRITICAL FIX: Acquire global allocation lock first to prevent races with allocations
		// This ensures shared pool updates don't interfere with ongoing container allocations
		m.allocationMu.Lock()
		defer m.allocationMu.Unlock()
		
		m.mu.Lock()
		fmt.Printf("DEBUG: scheduleSharedPoolUpdate acquired locks\n")

		if !m.sharedPoolUpdatePending {
			fmt.Printf("DEBUG: scheduleSharedPoolUpdate update not pending, releasing lock\n")
			m.mu.Unlock()
			return // Update was already processed or cancelled
		}

		// Generate updates for all shared containers
		updates := make([]*api.ContainerUpdate, 0)
		newSharedPool := m.computeSharedPool(alloc.GetOnlineCPUs())

		for _, containerInfo := range m.byCID {
			if containerInfo.Mode == ModeShared && len(newSharedPool) > 0 {
				update := &api.ContainerUpdate{
					ContainerId: containerInfo.ID,
					Linux: &api.LinuxContainerUpdate{
						Resources: &api.LinuxResources{
							Cpu: &api.LinuxCPU{
								Cpus: numa.FormatCPUList(newSharedPool),
							},
						},
					},
				}
				updates = append(updates, update)
				containerInfo.CPUs = newSharedPool
			}
		}

		m.sharedPoolUpdatePending = false

		// Extract callback and updates under lock, then release lock before calling
		var callback func([]*api.ContainerUpdate)
		if len(updates) > 0 && m.pendingUpdateCallback != nil {
			callback = m.pendingUpdateCallback
		}

		// Always unlock here - no defer needed since we handle all paths explicitly
		fmt.Printf("DEBUG: scheduleSharedPoolUpdate releasing lock before callback\n")
		m.mu.Unlock()

		// Call the callback outside the lock to prevent deadlock
		if callback != nil {
			fmt.Printf("DEBUG: scheduleSharedPoolUpdate calling callback with %d updates\n", len(updates))
			callback(updates)
			fmt.Printf("DEBUG: scheduleSharedPoolUpdate callback completed\n")
		} else {
			fmt.Printf("DEBUG: scheduleSharedPoolUpdate no callback to call\n")
		}
	})
}

// ContainerExists checks if a container exists in the state manager
func (m *Manager) ContainerExists(containerID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.byCID[containerID]
	return exists
}

type Allocator interface {
	GetOnlineCPUs() []int
	AllocateExclusiveCPUs(count int, reserved []int) ([]int, error)
	// Add the new unified allocation method
	AllocateContainerCPUs(pod *api.PodSandbox, container *api.Container, reserved []int) ([]int, string, error)
	// Add method for allocation with forbidden CPUs
	AllocateContainerCPUsWithForbidden(pod *api.PodSandbox, container *api.Container, reserved []int, forbidden []int) ([]int, string, error)
	// Add the specific method for annotated containers with integer conflict checking
	HandleAnnotatedContainerWithIntegerConflictCheck(pod *api.PodSandbox, integerReserved []int) (*allocator.AllocationResult, error)
	// Add method for checking reallocation feasibility
	CanReallocateInteger(currentCPUs []int, conflictCPUs []int, allReserved []int) ([]int, bool)
}

// ReallocationPlan represents a plan for reallocating integer containers
type ReallocationPlan struct {
	ContainerID string
	OldCPUs     []int
	NewCPUs     []int
	MemNodes    []int
}

// pendingReallocationPlan removed - now instance-based in Manager struct

// AllocateAnnotated performs allocation of annotated containers with proper synchronization
// DESIGN PRINCIPLE: Wait for initial Synchronize(), then use in-memory state with global locking
func (m *Manager) AllocateAnnotated(pod *api.PodSandbox, container *api.Container, alloc Allocator) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	// STEP 1: Wait for initial synchronization to complete
	m.waitForSynchronization()
	
	// STEP 2: GLOBAL ALLOCATION LOCK - prevents ALL race conditions
	m.allocationMu.Lock()
	defer m.allocationMu.Unlock()

	fmt.Printf("DEBUG: AllocateAnnotated acquired global lock for container %s\n", safeShortID(container.Id))

	// STEP 3: Get current state from memory (synchronized from Synchronize() + incremental updates)
	m.mu.RLock()
	currentIntegerReserved := m.getIntegerReservedCPUsUnsafe()
	currentAnnotatedReserved := m.getAnnotatedCPUsUnsafe()
	m.mu.RUnlock()

	fmt.Printf("DEBUG: Memory state - integer reserved: %v, annotated reserved: %v\n", currentIntegerReserved, currentAnnotatedReserved)

	// STEP 2: Try direct allocation (no conflicts)
	result, err := alloc.HandleAnnotatedContainerWithIntegerConflictCheck(pod, currentIntegerReserved)
	if err == nil {
		// SUCCESS: No conflicts, record atomically and return
		fmt.Printf("DEBUG: No conflicts for annotated container %s, recording atomically\n", safeShortID(container.Id))
		adjustment := m.createAnnotatedAdjustment(result)
		
		// Record state atomically
		m.mu.Lock()
		m.recordAnnotatedContainerUnsafe(container, pod, result.CPUs)
		m.mu.Unlock()
		
		return adjustment, nil, nil
	}

	// STEP 3: Check if error is a conflict (can be resolved) vs invalid annotation (cannot be resolved)
	if strings.Contains(err.Error(), "invalid CPU list") ||
		strings.Contains(err.Error(), "not online") ||
		strings.Contains(err.Error(), "missing") {
		fmt.Printf("DEBUG: Invalid annotation for annotated container %s: %v\n", safeShortID(container.Id), err)
		return nil, nil, err
	}

	// STEP 4: Handle conflicts with reallocation
	fmt.Printf("DEBUG: Conflicts detected for annotated container %s, attempting reallocation: %v\n", safeShortID(container.Id), err)
	
	cpuList, exists := pod.Annotations[WekaAnnotation]
	if !exists {
		return nil, nil, fmt.Errorf("missing %s annotation", WekaAnnotation)
	}

	requestedCPUs, err := numa.ParseCPUList(cpuList)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid CPU list in annotation: %w", err)
	}

	// STEP 5: Calculate reallocation plan atomically
	reallocationPlan, adjustment, err := m.calculateReallocationPlan(requestedCPUs, alloc)
	if err != nil {
		fmt.Printf("DEBUG: Reallocation planning failed: %v\n", err)
		return nil, nil, fmt.Errorf("cannot reallocate conflicting containers: %w", err)
	}

	// STEP 6: Execute reallocation and record new state atomically
	updates, err := m.executeReallocationPlanAtomic(reallocationPlan, container, pod, requestedCPUs)
	if err != nil {
		return nil, nil, fmt.Errorf("reallocation execution failed: %w", err)
	}

	fmt.Printf("DEBUG: Successfully planned reallocation of %d containers for annotated container %s\n", 
		len(reallocationPlan), safeShortID(container.Id))

	return adjustment, updates, nil
}

// AllocateAnnotatedWithReallocation - DEPRECATED, kept for backward compatibility
// Use StatelessAllocateAnnotated instead
func (m *Manager) AllocateAnnotatedWithReallocation(pod *api.PodSandbox, alloc Allocator) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	fmt.Printf("WARNING: Using deprecated AllocateAnnotatedWithReallocation, use StatelessAllocateAnnotated instead\n")
	
	// Create a temporary container for backward compatibility
	tempContainer := &api.Container{
		Id:           fmt.Sprintf("temp-%s", pod.Uid),
		PodSandboxId: pod.Id,
		Name:         "temp-container",
	}
	
	return m.StatelessAllocateAnnotated(pod, tempContainer, alloc)
}

// findConflictingIntegerContainers identifies integer containers using the requested CPUs
func (m *Manager) findConflictingIntegerContainers(requestedCPUs []int) map[string]*ContainerInfo {
	conflicting := make(map[string]*ContainerInfo)

	fmt.Printf("DEBUG: Finding conflicts for requested CPUs %v\n", requestedCPUs)
	fmt.Printf("DEBUG: Current intOwner map has %d entries\n", len(m.intOwner))
	for cpu, cid := range m.intOwner {
		fmt.Printf("DEBUG: intOwner[%d] = %s\n", cpu, safeShortID(cid))
	}

	for _, cpu := range requestedCPUs {
		if containerID, exists := m.intOwner[cpu]; exists {
			fmt.Printf("DEBUG: CPU %d is owned by container %s\n", cpu, safeShortID(containerID))
			if container := m.byCID[containerID]; container != nil && container.Mode == ModeInteger {
				fmt.Printf("DEBUG: Container %s has CPUs %v, adding to conflicts\n", safeShortID(containerID), container.CPUs)
				conflicting[containerID] = container
			} else {
				fmt.Printf("DEBUG: Container %s not found in byCID or not integer mode\n", safeShortID(containerID))
			}
		} else {
			fmt.Printf("DEBUG: CPU %d is not owned by any integer container\n", cpu)
		}
	}

	fmt.Printf("DEBUG: Found %d conflicting containers\n", len(conflicting))
	return conflicting
}

// planReallocation creates a reallocation plan for conflicting containers
func (m *Manager) planReallocation(conflictingContainers map[string]*ContainerInfo, annotatedCPUs []int, alloc Allocator) ([]ReallocationPlan, error) {
	// Pre-validate: Check if there are sufficient free CPUs for all reallocations
	if err := m.validateReallocationFeasibility(conflictingContainers, annotatedCPUs, alloc); err != nil {
		return nil, err
	}

	var plans []ReallocationPlan
	allReserved := m.getReservedCPUsUnsafe()

	for containerID, container := range conflictingContainers {
		newCPUs, canReallocate := alloc.CanReallocateInteger(container.CPUs, annotatedCPUs, allReserved)
		if !canReallocate {
			return nil, fmt.Errorf("cannot reallocate integer container %s", containerID)
		}

		// Integer pods keep flexible NUMA memory (no binding) per PRD 3.3
		// This prevents memory placement conflicts during live reallocation
		plans = append(plans, ReallocationPlan{
			ContainerID: containerID,
			OldCPUs:     container.CPUs,
			NewCPUs:     newCPUs,
			MemNodes:    nil, // No NUMA memory binding for integer pods
		})

		// Update allReserved for next iteration
		// Remove old CPUs and add new CPUs to reserved set
		allReserved = m.updateReservedSetForPlan(allReserved, container.CPUs, newCPUs)
	}

	return plans, nil
}

// updateReservedSetForPlan updates the reserved set by removing old CPUs and adding new ones
func (m *Manager) updateReservedSetForPlan(reserved []int, oldCPUs []int, newCPUs []int) []int {
	reservedSet := make(map[int]struct{})
	for _, cpu := range reserved {
		reservedSet[cpu] = struct{}{}
	}

	// Remove old CPUs
	for _, cpu := range oldCPUs {
		delete(reservedSet, cpu)
	}

	// Add new CPUs
	for _, cpu := range newCPUs {
		reservedSet[cpu] = struct{}{}
	}

	var result []int
	for cpu := range reservedSet {
		result = append(result, cpu)
	}

	return result
}

// validateReallocationFeasibility checks if there are enough free CPUs to reallocate all conflicting containers
func (m *Manager) validateReallocationFeasibility(conflictingContainers map[string]*ContainerInfo, annotatedCPUs []int, alloc Allocator) error {
	// Calculate total CPUs needed for reallocation
	totalCPUsNeeded := 0
	for _, container := range conflictingContainers {
		totalCPUsNeeded += len(container.CPUs)
	}

	// Calculate CPUs that will be freed by removing conflicts
	conflictCPUSet := make(map[int]struct{})
	for _, container := range conflictingContainers {
		for _, cpu := range container.CPUs {
			// Only count CPUs that are actually requested by annotated pod
			for _, annotatedCPU := range annotatedCPUs {
				if cpu == annotatedCPU {
					conflictCPUSet[cpu] = struct{}{}
					break
				}
			}
		}
	}
	freedCPUs := len(conflictCPUSet)

	// Get current reserved CPUs (excluding the conflicting ones)
	allReserved := m.getReservedCPUsUnsafe()
	effectiveReserved := make(map[int]struct{})
	for _, cpu := range allReserved {
		effectiveReserved[cpu] = struct{}{}
	}
	// Remove the CPUs that will be freed by reallocation
	for _, container := range conflictingContainers {
		for _, cpu := range container.CPUs {
			delete(effectiveReserved, cpu)
		}
	}
	// Add the CPUs that will be taken by the annotated pod
	for _, cpu := range annotatedCPUs {
		effectiveReserved[cpu] = struct{}{}
	}

	// Convert back to slice for allocator
	var reserved []int
	for cpu := range effectiveReserved {
		reserved = append(reserved, cpu)
	}

	// Check if we have enough free CPUs for reallocation
	// We need at least as many CPUs as we're trying to reallocate
	// The freed CPUs (from conflicts) should cover the reallocation needs
	if totalCPUsNeeded > freedCPUs {
		// Check if allocator can provide the additional CPUs needed
		additionalCPUsNeeded := totalCPUsNeeded - freedCPUs
		_, err := alloc.AllocateExclusiveCPUs(additionalCPUsNeeded, reserved)
		if err != nil {
			fmt.Printf("DEBUG: Reallocation feasibility check failed: need %d CPUs, freed %d CPUs, additional %d CPUs unavailable: %v\n",
				totalCPUsNeeded, freedCPUs, additionalCPUsNeeded, err)
			return fmt.Errorf("insufficient free CPUs for reallocation: need %d CPUs for %d containers but only %d CPUs freed by conflicts, and %d additional CPUs unavailable: %w",
				totalCPUsNeeded, len(conflictingContainers), freedCPUs, additionalCPUsNeeded, err)
		}
		fmt.Printf("DEBUG: Reallocation feasibility check passed: need %d CPUs, freed %d CPUs, additional %d CPUs available\n",
			totalCPUsNeeded, freedCPUs, additionalCPUsNeeded)
	} else {
		fmt.Printf("DEBUG: Reallocation feasibility check passed: need %d CPUs, freed %d CPUs (sufficient)\n",
			totalCPUsNeeded, freedCPUs)
	}
	return nil
}

// executeReallocationPlan creates the update requests but defers state changes
func (m *Manager) executeReallocationPlan(plans []ReallocationPlan, alloc Allocator) ([]*api.ContainerUpdate, error) {
	var updates []*api.ContainerUpdate

	// First validate all plans before any state changes
	for _, plan := range plans {
		container := m.byCID[plan.ContainerID]
		if container == nil {
			return nil, fmt.Errorf("container %s not found in state", plan.ContainerID)
		}
	}

	// Create all update requests
	for _, plan := range plans {
		update := &api.ContainerUpdate{
			ContainerId: plan.ContainerID,
			Linux: &api.LinuxContainerUpdate{
				Resources: &api.LinuxResources{
					Cpu: &api.LinuxCPU{
						Cpus: numa.FormatCPUList(plan.NewCPUs),
					},
				},
			},
		}

		// Set memory nodes if specified
		if len(plan.MemNodes) > 0 {
			update.Linux.Resources.Cpu.Mems = numa.FormatCPUList(plan.MemNodes)
		}

		updates = append(updates, update)
	}

	return updates, nil
}

// applyReallocationPlan updates internal state after successful container updates
func (m *Manager) applyReallocationPlan(plans []ReallocationPlan) {
	for _, plan := range plans {
		container := m.byCID[plan.ContainerID]
		if container == nil {
			continue
		}

		// Remove old CPU ownership
		for _, cpu := range plan.OldCPUs {
			delete(m.intOwner, cpu)
		}

		// Add new CPU ownership
		for _, cpu := range plan.NewCPUs {
			m.intOwner[cpu] = plan.ContainerID
		}

		// Update container info
		container.CPUs = plan.NewCPUs
	}
}

// ApplySuccessfulReallocation applies the pending reallocation plan after successful container updates
func (m *Manager) ApplySuccessfulReallocation() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.pendingReallocationPlan != nil {
		fmt.Printf("DEBUG: Applying pending reallocation plan with %d containers\n", len(m.pendingReallocationPlan))
		m.applyReallocationPlan(m.pendingReallocationPlan)
		m.pendingReallocationPlan = nil
		fmt.Printf("DEBUG: Successfully applied pending reallocation plan\n")
	}
}

// ClearPendingReallocation clears any pending reallocation plan (used for rollback on failures)
func (m *Manager) ClearPendingReallocation() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.pendingReallocationPlan != nil {
		fmt.Printf("DEBUG: Clearing pending reallocation plan due to failure\n")
		m.pendingReallocationPlan = nil
	}
}

// createAnnotatedAdjustment creates a container adjustment for an annotated container
func (m *Manager) createAnnotatedAdjustment(result *allocator.AllocationResult) *api.ContainerAdjustment {
	// Use comma-separated format instead of range format for annotated containers
	// This might fix NRI/containerd compatibility issues
	cpuList := formatCPUListCommaSeparated(result.CPUs)
	fmt.Printf("DEBUG: createAnnotatedAdjustment received CPUs: %v, formatted as: %s (comma-separated)\n", result.CPUs, cpuList)
	adjustment := &api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: cpuList,
				},
			},
		},
	}

	// Set memory nodes for exclusive containers
	if len(result.MemNodes) > 0 {
		adjustment.Linux.Resources.Cpu.Mems = numa.FormatCPUList(result.MemNodes)
	}

	return adjustment
}

// formatCPUListCommaSeparated formats a CPU list as comma-separated values (e.g., "0,1,2")
// instead of using range notation (e.g., "0-2")
func formatCPUListCommaSeparated(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}

	sort.Ints(cpus)
	strs := make([]string, len(cpus))
	for i, cpu := range cpus {
		strs[i] = fmt.Sprintf("%d", cpu)
	}
	return strings.Join(strs, ",")
}

// GetContainerInfo returns container information by container ID
func (m *Manager) GetContainerInfo(containerID string) *ContainerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.byCID[containerID]
}

func (m *Manager) Synchronize(pods []*api.PodSandbox, containers []*api.Container, alloc Allocator) ([]*api.ContainerUpdate, error) {
	
	m.mu.Lock()
	defer m.mu.Unlock()

	// Count containers by state for debugging phantom container issues
	stoppedCount := 0
	for _, container := range containers {
		if container != nil && container.GetState() == api.ContainerState_CONTAINER_STOPPED {
			stoppedCount++
		}
	}
	if stoppedCount > 0 {
		fmt.Printf("Synchronizing: filtering out %d stopped containers from %d total\n", stoppedCount, len(containers))
	}


	// Clear existing state (including any invalid containers from previous runs)
	fmt.Printf("CRITICAL DEBUG: Synchronize() CLEARING STATE - before clear: intOwner=%d entries, annotRef=%d entries, byCID=%d entries\n", len(m.intOwner), len(m.annotRef), len(m.byCID))
	for cpu, owner := range m.intOwner {
		fmt.Printf("CRITICAL DEBUG: Synchronize() CLEARING intOwner - CPU %d was owned by %s\n", cpu, safeShortID(owner))
	}
	m.annotRef = make(map[int]int)
	m.intOwner = make(map[int]string)
	m.byCID = make(map[string]*ContainerInfo)
	fmt.Printf("CRITICAL DEBUG: Synchronize() STATE CLEARED - now: intOwner=%d entries, annotRef=%d entries, byCID=%d entries\n", len(m.intOwner), len(m.annotRef), len(m.byCID))
	// Also clear any pending reallocation plans from previous failed operations
	m.pendingReallocationPlan = nil


	// Separate containers by type for prioritized processing
	var annotatedContainers []*api.Container
	var integerContainers []*api.Container
	var sharedContainers []*api.Container

	for _, container := range containers {
		// Add safety checks to prevent panics
		if container == nil || container.PodSandboxId == "" {
			fmt.Printf("Warning: Skipping nil container or container with empty PodSandboxId\n")
			continue
		}

		// Filter out terminated containers to avoid managing phantom containers
		if container.GetState() == api.ContainerState_CONTAINER_STOPPED {
			fmt.Printf("Skipping stopped container %s\n", safeShortID(container.Id))
			continue
		}

		pod := m.findPod(pods, container.PodSandboxId)
		if pod == nil {
			continue
		}

		mode := m.determineContainerMode(pod, container)
		switch mode {
		case "annotated":
			annotatedContainers = append(annotatedContainers, container)
		case "integer":
			integerContainers = append(integerContainers, container)
		case "shared":
			sharedContainers = append(sharedContainers, container)
		default:
			fmt.Printf("Warning: Unknown container mode '%s' for container %s\n", mode, safeShortID(container.Id))
		}
	}

	updates := make([]*api.ContainerUpdate, 0)

	// Phase 1: Process annotated containers (highest priority)
	// Note: No need for live reallocation during sync since we're rebuilding state

	var skippedContainers []string
	for _, container := range annotatedContainers {
		pod := m.findPod(pods, container.PodSandboxId)
		if pod == nil {
			fmt.Printf("Warning: Pod not found for annotated container %s, skipping\n", container.Id)
			skippedContainers = append(skippedContainers, container.Id)
			continue
		}

		// For annotated containers, we need to check only for conflicts with INTEGER containers
		// Sharing between annotated containers is allowed
		integerReserved := make([]int, 0)
		for cpu := range m.intOwner {
			integerReserved = append(integerReserved, cpu)
		}

		result, err := alloc.HandleAnnotatedContainerWithIntegerConflictCheck(pod, integerReserved)
		if err != nil {
			// Enhanced error handling: Log detailed information and track the container
			cpuAnnotation := ""
			if pod.Annotations != nil {
				cpuAnnotation = pod.Annotations[WekaAnnotation]
			}
			fmt.Printf("Warning: Skipping annotated container %s (pod %s/%s) due to invalid CPU annotation '%s': %v\n",
				container.Id, pod.Namespace, pod.Name, cpuAnnotation, err)

			// Track this container as problematic for debugging purposes
			// These invalid containers won't interfere with normal allocation since they have no CPUs
			info := &ContainerInfo{
				ID:        container.Id,
				Mode:      "invalid-annotated", // Special mode for problematic containers
				CPUs:      nil,                 // No CPUs allocated
				PodID:     container.PodSandboxId,
				PodUID:    pod.Uid,
				CreatedAt: time.Now(),
			}
			m.byCID[container.Id] = info
			skippedContainers = append(skippedContainers, container.Id)
			continue
		}

		// Annotated container allocation successful
		info := &ContainerInfo{
			ID:        container.Id,
			Mode:      ModeAnnotated,
			CPUs:      result.CPUs,
			PodID:     container.PodSandboxId,
			PodUID:    pod.Uid,
			CreatedAt: time.Now(),
		}

		m.byCID[container.Id] = info

		// Update annotation reference counts
		for _, cpu := range result.CPUs {
			m.annotRef[cpu]++
		}

		// Create container update to apply CPU restrictions during synchronization
		// This ensures annotated containers discovered during sync get proper CPU assignments
		if len(result.CPUs) > 0 {
			update := &api.ContainerUpdate{
				ContainerId: container.Id,
				Linux: &api.LinuxContainerUpdate{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Cpus: numa.FormatCPUList(result.CPUs),
						},
					},
				},
			}

			// Set memory nodes if specified
			if len(result.MemNodes) > 0 {
				update.Linux.Resources.Cpu.Mems = numa.FormatCPUList(result.MemNodes)
			}

			updates = append(updates, update)
			fmt.Printf("DEBUG: Adding CPU restriction update for annotated container %s: %v\n", safeShortID(container.Id), result.CPUs)
		}
	}

	if len(skippedContainers) > 0 {
		fmt.Printf("State synchronization completed with %d skipped containers: %v\n", len(skippedContainers), skippedContainers)
	}

	// Phase 2: Process integer containers (medium priority)
	for _, container := range integerContainers {
		pod := m.findPod(pods, container.PodSandboxId)
		if pod == nil {
			fmt.Printf("Warning: Pod not found for integer container %s, skipping\n", container.Id)
			skippedContainers = append(skippedContainers, container.Id)
			continue
		}

		// CRITICAL FIX: During synchronization, we need to discover what CPUs the container
		// already has, NOT reallocate new ones. The container is already running with specific CPUs.

		// Get current CPU assignment from container's Linux resources
		var currentCPUs []int

		// DEBUG: Log what NRI data is available for this container
		if container.Linux != nil && container.Linux.Resources != nil && container.Linux.Resources.Cpu != nil {
			fmt.Printf("DEBUG: Container %s NRI CPU data - Cpus: '%s', Quota: %v, Period: %v\n",
				safeShortID(container.Id),
				container.Linux.Resources.Cpu.Cpus,
				container.Linux.Resources.Cpu.Quota,
				container.Linux.Resources.Cpu.Period)
		} else {
			fmt.Printf("DEBUG: Container %s has no NRI CPU resource data\n", safeShortID(container.Id))
		}

		if container.Linux != nil && container.Linux.Resources != nil &&
			container.Linux.Resources.Cpu != nil && container.Linux.Resources.Cpu.Cpus != "" {
			var err error
			currentCPUs, err = numa.ParseCPUList(container.Linux.Resources.Cpu.Cpus)
			if err != nil {
				fmt.Printf("Warning: Skipping integer container %s (pod %s/%s) due to invalid CPU assignment '%s': %v\n",
					container.Id, pod.Namespace, pod.Name, container.Linux.Resources.Cpu.Cpus, err)

				// During synchronization, don't track problematic containers to avoid state pollution
				// They will be handled during next container creation/update cycle
				fmt.Printf("DEBUG: Skipping problematic container %s during sync to avoid state corruption\n", container.Id)
				skippedContainers = append(skippedContainers, container.Id)
				continue
			}

			// Validate CPUs are actually online
			invalidCPUs := []int{}
			onlineCPUSet := make(map[int]struct{})
			for _, onlineCPU := range alloc.GetOnlineCPUs() {
				onlineCPUSet[onlineCPU] = struct{}{}
			}

			for _, cpu := range currentCPUs {
				if _, isOnline := onlineCPUSet[cpu]; !isOnline {
					invalidCPUs = append(invalidCPUs, cpu)
				}
			}

			if len(invalidCPUs) > 0 {
				fmt.Printf("Warning: Integer container %s has invalid CPU assignments %v (not online), skipping\n", container.Id, invalidCPUs)

				// Track this container as problematic for debugging purposes
				info := &ContainerInfo{
					ID:     container.Id,
					Mode:   "invalid-integer", // Special mode for problematic containers
					CPUs:   nil,               // No CPUs allocated
					PodID:  container.PodSandboxId,
					PodUID: pod.Uid,
				}
				m.byCID[container.Id] = info
				skippedContainers = append(skippedContainers, container.Id)
				continue
			}

			// CRITICAL FIX: Validate that the existing CPU assignment makes sense for an integer container
			// Calculate expected CPU count from container resource requirements
			expectedCPUCount := 0
			if container.Linux.Resources.Cpu != nil &&
				container.Linux.Resources.Cpu.Quota != nil &&
				container.Linux.Resources.Cpu.Period != nil {
				quota := container.Linux.Resources.Cpu.Quota.GetValue()
				period := int64(container.Linux.Resources.Cpu.Period.GetValue())
				if quota > 0 && period > 0 {
					expectedCPUCount = int(quota / period)
				}
			}

			// If the current CPU assignment is unreasonable (too many CPUs compared to quota),
			// treat this as a system container that shouldn't be managed as integer
			if expectedCPUCount > 0 && len(currentCPUs) > expectedCPUCount*4 {
				fmt.Printf("DEBUG: Skipping container %s: has %d CPUs but expected ~%d (likely system container)\n",
					container.Id, len(currentCPUs), expectedCPUCount)
				continue
			}

			fmt.Printf("DEBUG: Synchronizing integer container %s with existing CPUs %v (expected ~%d CPUs)\n",
				container.Id, currentCPUs, expectedCPUCount)
		} else {
			// Fallback: If we can't discover current CPUs, allocate new ones
			fmt.Printf("DEBUG: Cannot discover existing CPUs for integer container %s, fallback to allocation\n", container.Id)
			reserved := m.getReservedCPUsUnsafe()
			var err error
			currentCPUs, _, err = alloc.AllocateContainerCPUs(pod, container, reserved)
			if err != nil {
				fmt.Printf("Error allocating CPUs for integer container %s: %v\n", container.Id, err)
				continue
			}
		}

		// Integer container allocation successful

		info := &ContainerInfo{
			ID:     container.Id,
			Mode:   ModeInteger,
			CPUs:   currentCPUs,
			PodID:  container.PodSandboxId,
			PodUID: pod.Uid,
		}

		m.byCID[container.Id] = info

		// Check for conflicts with annotated containers and reallocate if needed
		var conflictFreeCPUs []int
		var hasConflicts bool

		for _, cpu := range currentCPUs {
			if _, hasAnnotated := m.annotRef[cpu]; hasAnnotated {
				hasConflicts = true
				fmt.Printf("WARNING: CPU %d conflict detected - integer container %s conflicts with annotated containers during sync\n", cpu, container.Id)
			} else {
				conflictFreeCPUs = append(conflictFreeCPUs, cpu)
			}
		}

		// If we have conflicts, REALLOCATE the integer container to available CPUs
		var finalCPUs []int
		if hasConflicts {
			fmt.Printf("DEBUG: Integer container %s has %d CPU conflicts, attempting reallocation during sync\n",
				container.Id, len(currentCPUs)-len(conflictFreeCPUs))

			// Build reserved CPU list for reallocation (annotated + other integer containers)
			reserved := make([]int, 0)
			for cpu := range m.annotRef {
				reserved = append(reserved, cpu)
			}
			for cpu := range m.intOwner {
				if m.intOwner[cpu] != container.Id { // Don't count our own CPUs as reserved
					reserved = append(reserved, cpu)
				}
			}

			// Attempt reallocation using the allocator
			newCPUs, _, err := alloc.AllocateContainerCPUs(pod, container, reserved)
			if err != nil {
				// Reallocation failed - this is a critical error during sync
				fmt.Printf("ERROR: Failed to reallocate integer container %s during sync: %v\n", container.Id, err)
				// Use only non-conflicting CPUs as fallback (may be empty)
				finalCPUs = conflictFreeCPUs
			} else {
				finalCPUs = newCPUs
				fmt.Printf("DEBUG: Successfully reallocated integer container %s from %v to %v during sync\n",
					container.Id, currentCPUs, finalCPUs)
			}
		} else {
			// No conflicts, use original CPUs
			finalCPUs = currentCPUs
		}

		// Update integer ownership mapping for the final CPU assignment
		for _, cpu := range finalCPUs {
			m.intOwner[cpu] = container.Id
		}

		// Update container info with final CPU assignment
		info.CPUs = finalCPUs
		currentCPUs = finalCPUs // Update for the container update below

		// Create container update to apply CPU restrictions during synchronization
		// This ensures containers discovered during sync get proper CPU assignments
		if len(currentCPUs) > 0 {
			update := &api.ContainerUpdate{
				ContainerId: container.Id,
				Linux: &api.LinuxContainerUpdate{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Cpus: numa.FormatCPUList(currentCPUs),
						},
					},
				},
			}
			updates = append(updates, update)
			fmt.Printf("DEBUG: Adding CPU restriction update for integer container %s: %v\n", safeShortID(container.Id), currentCPUs)
		}
	}

	// Phase 3: Process shared containers (lowest priority)
	sharedPool := m.computeSharedPool(alloc.GetOnlineCPUs())

	for _, container := range sharedContainers {
		pod := m.findPod(pods, container.PodSandboxId)
		if pod == nil {
			continue
		}

		info := &ContainerInfo{
			ID:        container.Id,
			Mode:      ModeShared,
			CPUs:      sharedPool,
			PodID:     container.PodSandboxId,
			PodUID:    pod.Uid,
			CreatedAt: time.Now(),
		}

		m.byCID[container.Id] = info

		// Create update for shared container to use current shared pool
		if len(sharedPool) > 0 {
			update := &api.ContainerUpdate{
				ContainerId: container.Id,
				Linux: &api.LinuxContainerUpdate{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Cpus: numa.FormatCPUList(sharedPool),
						},
					},
				},
			}
			updates = append(updates, update)
		}
	}

	// Mark synchronization as complete and notify waiting operations
	// NOTE: We already hold m.mu lock from the beginning of this function
	m.synchronized = true
	m.syncCond.Broadcast() // Wake up any operations waiting for synchronization
	
	fmt.Printf("DEBUG: Synchronize() completed - state manager now synchronized\n")
	return updates, nil
}

func (m *Manager) RecordContainer(container *api.Container, adjustment *api.ContainerAdjustment) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if adjustment.Linux == nil || adjustment.Linux.Resources == nil || adjustment.Linux.Resources.Cpu == nil {
		return
	}

	cpuList := adjustment.Linux.Resources.Cpu.Cpus
	if cpuList == "" {
		return
	}

	cpus, err := numa.ParseCPUList(cpuList)
	if err != nil {
		fmt.Printf("Error parsing CPU list for container %s: %v\n", container.Id, err)
		return
	}

	// This will be set by the allocation logic
	info := m.byCID[container.Id]
	if info != nil {
		info.CPUs = cpus
	}
}

func (m *Manager) RemoveContainer(containerID string, alloc Allocator) ([]*api.ContainerUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	info := m.byCID[containerID]
	if info == nil {
		// ADD DEBUG LOG FOR MISSING CONTAINER
		fmt.Printf("DEBUG: RemoveContainer called for unknown container %s (not in state)\n", safeShortID(containerID))
		return nil, nil
	}

	// ADD DEBUG LOG FOR REMOVAL START
	fmt.Printf("DEBUG: RemoveContainer %s mode=%s CPUs=%v (before removal: intOwner=%d entries, annotRef=%d entries)\n", 
		safeShortID(containerID), info.Mode, info.CPUs, len(m.intOwner), len(m.annotRef))

	// Release reservations with enhanced validation
	switch info.Mode {
	case ModeAnnotated:
		for _, cpu := range info.CPUs {
			if count := m.annotRef[cpu]; count > 1 {
				m.annotRef[cpu] = count - 1
				fmt.Printf("DEBUG: Decremented annotation count for CPU %d to %d\n", cpu, count-1)
			} else {
				delete(m.annotRef, cpu)
				fmt.Printf("DEBUG: Released annotated CPU %d from container %s\n", cpu, safeShortID(containerID))
			}
		}
	case ModeInteger:
		// CRITICAL FIX: Validate CPU ownership before removal
		for _, cpu := range info.CPUs {
			if currentOwner := m.intOwner[cpu]; currentOwner != containerID {
				fmt.Printf("WARNING: CPU %d ownership mismatch during removal - expected %s, found %s\n", 
					cpu, safeShortID(containerID), safeShortID(currentOwner))
				// Still remove it to prevent state corruption, but log the issue
			}
			delete(m.intOwner, cpu)
			fmt.Printf("DEBUG: Released integer CPU %d from container %s\n", cpu, safeShortID(containerID))
		}
	case ModeShared:
		// No specific reservations to release for shared containers
	case "invalid-annotated", "invalid-integer":
		// Invalid containers don't have CPU reservations to release
		fmt.Printf("DEBUG: Removing invalid container %s (mode: %s)\n", safeShortID(containerID), info.Mode)
	}

	delete(m.byCID, containerID)

	// ADD DEBUG LOG FOR REMOVAL COMPLETE
	fmt.Printf("DEBUG: RemoveContainer %s complete (after removal: intOwner=%d entries, annotRef=%d entries)\n", 
		safeShortID(containerID), len(m.intOwner), len(m.annotRef))

	// CRITICAL FIX: Validate state consistency after removal
	m.validateStateConsistency()

	// If we have a callback set up, use debounced updates to prevent explosion during rapid deletions
	if m.pendingUpdateCallback != nil {
		m.scheduleSharedPoolUpdate(alloc)
		return nil, nil // Updates will be handled by debounced callback
	}

	// Fallback to immediate updates for backwards compatibility (tests, etc.)
	updates := make([]*api.ContainerUpdate, 0)
	newSharedPool := m.computeSharedPool(alloc.GetOnlineCPUs())

	for _, containerInfo := range m.byCID {
		if containerInfo.Mode == ModeShared && len(newSharedPool) > 0 {
			update := &api.ContainerUpdate{
				ContainerId: containerInfo.ID,
				Linux: &api.LinuxContainerUpdate{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Cpus: numa.FormatCPUList(newSharedPool),
						},
					},
				},
			}
			updates = append(updates, update)
			containerInfo.CPUs = newSharedPool
		}
	}

	return updates, nil
}

func (m *Manager) GetReservedCPUs() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getReservedCPUsUnsafe()
}

// AtomicAllocateAndRecordAnnotated performs atomic allocation and state recording for annotated containers
// This fixes the race condition where periodic cleanup removes annotation references before container is recorded
func (m *Manager) AtomicAllocateAndRecordAnnotated(pod *api.PodSandbox, container *api.Container, alloc Allocator) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Get current integer reservations under lock
	integerReserved := m.getIntegerReservedCPUsUnsafe()

	// Try allocation with current integer reservations
	result, err := alloc.HandleAnnotatedContainerWithIntegerConflictCheck(pod, integerReserved)
	if err != nil {
		return nil, nil, err
	}

	// Create adjustment immediately
	adjustment := m.createAnnotatedAdjustment(result)

	// Record container info and annotation references atomically within the same lock
	info := &ContainerInfo{
		ID:        container.Id,
		Mode:      ModeAnnotated,
		CPUs:      result.CPUs,
		PodID:     container.PodSandboxId,
		PodUID:    pod.Uid,
		CreatedAt: time.Now(),
	}

	m.byCID[container.Id] = info

	// Update annotation reference counts
	for _, cpu := range result.CPUs {
		m.annotRef[cpu]++
	}

	fmt.Printf("DEBUG: Atomically allocated and recorded annotated container %s with %d CPUs (annotRef now has %d entries)\n", 
		safeShortID(container.Id), len(result.CPUs), len(m.annotRef))

	return adjustment, nil, nil
}

// AllocateInteger performs allocation of integer containers with proper synchronization
// DESIGN PRINCIPLE: Wait for initial Synchronize(), then use in-memory state with global locking
func (m *Manager) AllocateInteger(pod *api.PodSandbox, container *api.Container, alloc Allocator, forbidden []int) (*api.ContainerAdjustment, error) {
	// STEP 1: Wait for initial synchronization to complete
	m.waitForSynchronization()
	
	// STEP 2: GLOBAL ALLOCATION LOCK - prevents ALL race conditions
	m.allocationMu.Lock()
	defer m.allocationMu.Unlock()

	fmt.Printf("DEBUG: AllocateInteger acquired global lock for container %s\n", safeShortID(container.Id))
	
	// CRITICAL DEBUG: Log current state when entering allocation
	m.mu.RLock()
	fmt.Printf("DEBUG: AllocateInteger ENTRY STATE - intOwner has %d entries, annotRef has %d entries, byCID has %d entries\n", len(m.intOwner), len(m.annotRef), len(m.byCID))
	for cpu, owner := range m.intOwner {
		fmt.Printf("DEBUG: AllocateInteger ENTRY STATE - CPU %d owned by %s\n", cpu, safeShortID(owner))
	}
	m.mu.RUnlock()

	// STEP 3: Get current state from memory (synchronized from Synchronize() + incremental updates)
	m.mu.RLock()
	currentReserved := m.getReservedCPUsUnsafe()
	currentAnnotated := m.getAnnotatedCPUsUnsafe()
	m.mu.RUnlock()

	fmt.Printf("DEBUG: Memory state - reserved: %v, annotated: %v, forbidden: %v\n", currentReserved, currentAnnotated, forbidden)

	// STEP 2: Check for conflicts with annotated containers (REJECT if conflict)
	annotatedSet := make(map[int]struct{})
	for _, cpu := range currentAnnotated {
		annotatedSet[cpu] = struct{}{}
	}

	// STEP 3: Combine all unavailable CPUs
	allUnavailable := make(map[int]struct{})
	for _, cpu := range currentReserved {
		allUnavailable[cpu] = struct{}{}
	}
	for _, cpu := range currentAnnotated {
		allUnavailable[cpu] = struct{}{}
	}
	for _, cpu := range forbidden {
		allUnavailable[cpu] = struct{}{}
	}

	var unavailableSlice []int
	for cpu := range allUnavailable {
		unavailableSlice = append(unavailableSlice, cpu)
	}

	// STEP 4: Attempt allocation
	cpus, _, err := alloc.AllocateContainerCPUsWithForbidden(pod, container, unavailableSlice, nil)
	if err != nil {
		// Check if this is due to annotated container conflict
		if len(currentAnnotated) > 0 {
			conflictCPUs := make([]int, 0)
			for _, cpu := range forbidden {
				if _, isAnnotated := annotatedSet[cpu]; isAnnotated {
					conflictCPUs = append(conflictCPUs, cpu)
				}
			}
			if len(conflictCPUs) > 0 {
				return nil, fmt.Errorf("integer container allocation rejected: conflicts with annotated containers on CPUs %v", conflictCPUs)
			}
		}
		
		fmt.Printf("DEBUG: Integer allocation failed for container %s: %v\n", safeShortID(container.Id), err)
		return nil, fmt.Errorf("failed to allocate CPUs: %w", err)
	}

	// STEP 5: Validate no CPU ownership conflicts (double-check)
	m.mu.RLock()
	var conflicts []int
	var conflictOwners []string
	
	for _, cpu := range cpus {
		if existingOwner := m.intOwner[cpu]; existingOwner != "" {
			conflicts = append(conflicts, cpu)
			conflictOwners = append(conflictOwners, safeShortID(existingOwner))
		}
		// Also check annotated conflicts (should not happen but safety check)
		if _, hasAnnotated := m.annotRef[cpu]; hasAnnotated {
			fmt.Printf("ERROR: Integer container %s attempted to allocate annotated CPU %d\n", safeShortID(container.Id), cpu)
			conflicts = append(conflicts, cpu)
		}
	}
	m.mu.RUnlock()

	if len(conflicts) > 0 {
		return nil, fmt.Errorf("CPU conflict detected during integer allocation: CPUs %v already owned by %v", conflicts, conflictOwners)
	}

	// STEP 6: Record allocation in internal state for consistency
	// Even in "stateless" architecture, we need to track containers for RemoveContainer() calls
	// The "stateless" part refers to querying fresh reserved CPUs, not avoiding all state
	m.mu.Lock()
	
	info := &ContainerInfo{
		ID:        container.Id,
		Mode:      ModeInteger,
		CPUs:      cpus,
		PodID:     container.PodSandboxId,
		PodUID:    pod.Uid,
		CreatedAt: time.Now(),
	}
	
	m.byCID[container.Id] = info
	
	// Record integer CPU ownership
	for _, cpu := range cpus {
		m.intOwner[cpu] = container.Id
		fmt.Printf("DEBUG: StatelessAllocateInteger - assigned CPU %d to container %s\n", cpu, safeShortID(container.Id))
	}
	
	m.mu.Unlock()
	
	fmt.Printf("DEBUG: StatelessAllocateInteger - recorded container %s in state with CPUs %v\n", safeShortID(container.Id), cpus)

	// STEP 7: Create adjustment
	adjustment := &api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: numa.FormatCPUList(cpus),
				},
			},
		},
	}

	fmt.Printf("DEBUG: Successfully allocated integer container %s with CPUs %v\n", safeShortID(container.Id), cpus)
	fmt.Printf("DEBUG: TRUE STATELESS - allocation recorded by NRI, will be discovered in next query\n")
	return adjustment, nil
}

// StatelessAllocateAnnotated - compatibility wrapper for old name
func (m *Manager) StatelessAllocateAnnotated(pod *api.PodSandbox, container *api.Container, alloc Allocator) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	return m.AllocateAnnotated(pod, container, alloc)
}

// StatelessAllocateInteger - compatibility wrapper for old name
func (m *Manager) StatelessAllocateInteger(pod *api.PodSandbox, container *api.Container, alloc Allocator, forbidden []int) (*api.ContainerAdjustment, error) {
	return m.AllocateInteger(pod, container, alloc, forbidden)
}

// AtomicAllocateAndRecordInteger - DEPRECATED, kept for backward compatibility
func (m *Manager) AtomicAllocateAndRecordInteger(pod *api.PodSandbox, container *api.Container, alloc Allocator, forbidden []int) (*api.ContainerAdjustment, error) {
	return m.AllocateInteger(pod, container, alloc, forbidden)
}

func (m *Manager) GetIntegerReservedCPUs() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getIntegerReservedCPUsUnsafe()
}

func (m *Manager) getIntegerReservedCPUsUnsafe() []int {
	var reserved []int
	for cpu := range m.intOwner {
		reserved = append(reserved, cpu)
	}
	fmt.Printf("DEBUG: getIntegerReservedCPUsUnsafe() returning %v, intOwner map has %d entries\n", reserved, len(m.intOwner))
	return reserved
}

// NEW STATELESS HELPER METHODS - these fetch fresh state every time

// getFreshReservedCPUs returns all currently reserved CPUs (both annotated and integer)
// TRUE STATELESS IMPLEMENTATION: Query live container state instead of in-memory maps
// If pods and containers are provided, uses them directly; otherwise queries runtime
func (m *Manager) getFreshReservedCPUs() []int {
	return m.getFreshReservedCPUsWithContext(nil, nil)
}

// getFreshReservedCPUsWithContext allows providing pod/container context for better classification
func (m *Manager) getFreshReservedCPUsWithContext(pods []*api.PodSandbox, containers []*api.Container) []int {
	// APPROACH 5: Query container runtime state directly from cgroups
	// This is the most direct and stateless approach - query what CPUs are actually assigned right now
	return m.queryReservedCPUsFromCgroups()
}

// queryReservedCPUsFromCgroups directly queries the cgroup filesystem to see what CPUs are currently assigned
func (m *Manager) queryReservedCPUsFromCgroups() []int {
	fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - querying cgroup filesystem for current CPU assignments\n")
	
	// For now, fall back to the original callback-based approach
	// TODO: Implement direct cgroup querying in future iteration
	pods, containers, err := m.queryRunningPodsAndContainers()
	if err != nil {
		fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - pod-container query failed: %v, trying containers only\n", err)
		
		// Fallback to container-only query
		containers, err := m.queryRunningContainers()
		if err != nil {
			fmt.Printf("ERROR: queryReservedCPUsFromCgroups() failed to query running containers: %v, falling back to in-memory state\n", err)
			return m.getFreshReservedCPUsFromMemory() // Fallback to old approach
		}
		pods = nil // No pod information available for runtime queries
		fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - queried %d containers from runtime (no pod context)\n", len(containers))
	} else {
		fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - using pod-container context: %d pods, %d containers\n", len(pods), len(containers))
	}

	reserved := make(map[int]struct{})

	// Extract CPU assignments from live container data  
	for _, container := range containers {
		if container == nil {
			continue
		}

		// Extract CPUs from container resources
		var cpus []int
		if container.Linux != nil && container.Linux.Resources != nil &&
			container.Linux.Resources.Cpu != nil && container.Linux.Resources.Cpu.Cpus != "" {
			var err error
			cpus, err = numa.ParseCPUList(container.Linux.Resources.Cpu.Cpus)
			if err != nil {
				fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - failed to parse CPU list '%s' for container %s: %v\n", 
					container.Linux.Resources.Cpu.Cpus, safeShortID(container.Id), err)
				continue
			}

			// Find the pod for this container for proper classification
			var pod *api.PodSandbox
			if pods != nil && container.PodSandboxId != "" {
				pod = m.findPod(pods, container.PodSandboxId)
			}

			// IMPROVED FIX: Additional system container filtering based on CPU assignment pattern
			// System containers often have access to all CPUs, which indicates they're not exclusive
			if len(cpus) > 50 { // If container has access to more than 50 CPUs, it's likely a system container
				fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - skipping container %s with broad CPU access (%d CPUs) - likely system container\n", 
					safeShortID(container.Id), len(cpus))
				continue
			}

			// IMPROVED FIX: Check if this container has exclusive CPU allocation semantics using pod-based classification
			if !m.hasExclusiveCPUSemantics(container, pod) {
				if pod != nil {
					fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - skipping non-exclusive container %s (pod %s/%s)\n", 
						safeShortID(container.Id), pod.Namespace, pod.Name)
				} else {
					fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - skipping non-exclusive container %s (no pod info)\n", 
						safeShortID(container.Id))
				}
				continue
			}

			if pod != nil {
				fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - exclusive container %s (pod %s/%s) has CPU assignment %v (%d CPUs)\n", 
					safeShortID(container.Id), pod.Namespace, pod.Name, cpus, len(cpus))
			} else {
				fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - exclusive container %s (no pod info) has CPU assignment %v (%d CPUs)\n", 
					safeShortID(container.Id), cpus, len(cpus))
			}
			for _, cpu := range cpus {
				reserved[cpu] = struct{}{}
			}
		}
	}

	var result []int
	for cpu := range reserved {
		result = append(result, cpu)
	}

	fmt.Printf("DEBUG: queryReservedCPUsFromCgroups() - STATELESS QUERY returned %d total reserved CPUs: %v\n", len(result), result)
	return result
}

// getFreshReservedCPUsFromMemory - fallback to in-memory maps (old approach)
func (m *Manager) getFreshReservedCPUsFromMemory() []int {
	reserved := make(map[int]struct{})
	
	fmt.Printf("DEBUG: getFreshReservedCPUsFromMemory() - FALLBACK - intOwner map has %d entries, annotRef map has %d entries\n", len(m.intOwner), len(m.annotRef))
	
	// Add annotated CPUs
	for cpu := range m.annotRef {
		reserved[cpu] = struct{}{}
	}
	
	// Add integer CPUs
	for cpu := range m.intOwner {
		reserved[cpu] = struct{}{}
	}
	
	var result []int
	for cpu := range reserved {
		result = append(result, cpu)
	}
	
	fmt.Printf("DEBUG: getFreshReservedCPUsFromMemory() - FALLBACK returned %d CPUs: %v\n", len(result), result)
	return result
}

// getFreshIntegerReservedCPUs returns CPUs reserved by integer containers
// TRUE STATELESS IMPLEMENTATION: Query live integer container state
func (m *Manager) getFreshIntegerReservedCPUs() []int {
	// STATELESS APPROACH: Get live container data from runtime
	runningContainers, err := m.queryRunningContainers()
	if err != nil {
		fmt.Printf("ERROR: getFreshIntegerReservedCPUs() failed to query running containers: %v, falling back to in-memory state\n", err)
		return m.getFreshIntegerReservedCPUsFromMemory() // Fallback
	}

	var reserved []int
	fmt.Printf("DEBUG: getFreshIntegerReservedCPUs() - querying %d running containers for integer CPUs\n", len(runningContainers))

	// Find integer containers and extract their CPU assignments
	for _, container := range runningContainers {
		if container == nil {
			continue
		}

		// Check if this is an integer container by checking CPU quota
		if !m.isIntegerContainer(container) {
			continue
		}

		// Extract CPUs from container resources  
		if container.Linux != nil && container.Linux.Resources != nil &&
			container.Linux.Resources.Cpu != nil && container.Linux.Resources.Cpu.Cpus != "" {
			cpus, err := numa.ParseCPUList(container.Linux.Resources.Cpu.Cpus)
			if err != nil {
				fmt.Printf("DEBUG: getFreshIntegerReservedCPUs() - failed to parse CPU list for integer container %s: %v\n", 
					safeShortID(container.Id), err)
				continue
			}

			fmt.Printf("DEBUG: getFreshIntegerReservedCPUs() - integer container %s has CPUs %v\n", safeShortID(container.Id), cpus)
			reserved = append(reserved, cpus...)
		}
	}

	fmt.Printf("DEBUG: getFreshIntegerReservedCPUs() - STATELESS QUERY returned %d integer CPUs: %v\n", len(reserved), reserved)
	return reserved
}

// getFreshIntegerReservedCPUsFromMemory - fallback to in-memory maps (old approach)
func (m *Manager) getFreshIntegerReservedCPUsFromMemory() []int {
	var reserved []int
	fmt.Printf("DEBUG: getFreshIntegerReservedCPUsFromMemory() - FALLBACK - intOwner map has %d entries\n", len(m.intOwner))
	for cpu := range m.intOwner {
		reserved = append(reserved, cpu)
		fmt.Printf("DEBUG: getFreshIntegerReservedCPUsFromMemory() - found CPU %d owned by %s\n", cpu, safeShortID(m.intOwner[cpu]))
	}
	fmt.Printf("DEBUG: getFreshIntegerReservedCPUsFromMemory() - FALLBACK returned %d CPUs: %v\n", len(reserved), reserved)
	return reserved
}

// getAnnotatedCPUsUnsafe returns CPUs reserved by annotated containers from memory (caller must hold lock)
func (m *Manager) getAnnotatedCPUsUnsafe() []int {
	var annotated []int
	for cpu := range m.annotRef {
		annotated = append(annotated, cpu)
	}
	return annotated
}

// queryRunningContainers gets the current list of running containers from the runtime
func (m *Manager) queryRunningContainers() ([]*api.Container, error) {
	m.mu.RLock()
	callback := m.containerQueryCallback
	m.mu.RUnlock()

	if callback != nil {
		containers, err := callback()
		if err != nil {
			return nil, fmt.Errorf("container query callback failed: %w", err)
		}
		fmt.Printf("DEBUG: queryRunningContainers() - callback returned %d containers\n", len(containers))
		return containers, nil
	}


	return nil, fmt.Errorf("no container query method available")
}

// queryRunningPodsAndContainers gets both pods and containers for proper classification
func (m *Manager) queryRunningPodsAndContainers() ([]*api.PodSandbox, []*api.Container, error) {
	m.mu.RLock()
	callback := m.podContainerQueryCallback
	m.mu.RUnlock()

	if callback != nil {
		pods, containers, err := callback()
		if err != nil {
			return nil, nil, fmt.Errorf("pod-container query callback failed: %w", err)
		}
		fmt.Printf("DEBUG: queryRunningPodsAndContainers() - callback returned %d pods, %d containers\n", len(pods), len(containers))
		return pods, containers, nil
	}

	return nil, nil, fmt.Errorf("no pod-container query method available")
}


// isIntegerContainer checks if a container should be treated as an integer container
func (m *Manager) isIntegerContainer(container *api.Container) bool {
	// Check if container has integer CPU semantics (whole number of CPUs)
	if container.Linux == nil || container.Linux.Resources == nil || 
	   container.Linux.Resources.Cpu == nil {
		return false
	}

	cpu := container.Linux.Resources.Cpu
	if cpu.Quota == nil || cpu.Period == nil {
		return false
	}

	quota := cpu.Quota.GetValue()
	period := int64(cpu.Period.GetValue())
	if quota <= 0 || period <= 0 {
		return false
	}

	// Check if it's requesting whole CPUs (integer semantics)
	cpuCount := float64(quota) / float64(period)
	isInteger := cpuCount == float64(int(cpuCount)) && cpuCount >= 1.0

	if isInteger {
		fmt.Printf("DEBUG: isIntegerContainer() - container %s is integer (%.1f CPUs)\n", safeShortID(container.Id), cpuCount)
	}

	return isInteger
}

// hasExclusiveCPUSemantics checks if a container requires exclusive CPU allocation
// Uses proper pod-based classification instead of heuristics
func (m *Manager) hasExclusiveCPUSemantics(container *api.Container, pod *api.PodSandbox) bool {
	if pod == nil {
		// Fallback to simple integer check if pod is not available
		fmt.Printf("DEBUG: hasExclusiveCPUSemantics() - no pod available for container %s, using integer check\n", safeShortID(container.Id))
		return m.isIntegerContainer(container)
	}

	// Use proper container classification based on pod information
	mode := m.determineContainerMode(pod, container)
	
	switch mode {
	case "annotated", "integer":
		fmt.Printf("DEBUG: hasExclusiveCPUSemantics() - container %s (pod %s/%s) has mode %s - requires exclusive CPUs\n", 
			safeShortID(container.Id), pod.Namespace, pod.Name, mode)
		return true
	case "shared":
		fmt.Printf("DEBUG: hasExclusiveCPUSemantics() - container %s (pod %s/%s) has mode %s - does not require exclusive CPUs\n", 
			safeShortID(container.Id), pod.Namespace, pod.Name, mode)
		return false
	default:
		fmt.Printf("DEBUG: hasExclusiveCPUSemantics() - container %s (pod %s/%s) has unknown mode %s - defaulting to no exclusivity\n", 
			safeShortID(container.Id), pod.Namespace, pod.Name, mode)
		return false
	}
}

// getFreshAnnotatedCPUs returns CPUs reserved by annotated containers  
// TRUE STATELESS IMPLEMENTATION: Query live annotated container state
func (m *Manager) getFreshAnnotatedCPUs() []int {
	// STATELESS APPROACH: Get live container data from runtime
	runningContainers, err := m.queryRunningContainers()
	if err != nil {
		fmt.Printf("ERROR: getFreshAnnotatedCPUs() failed to query running containers: %v, falling back to in-memory state\n", err)
		return m.getFreshAnnotatedCPUsFromMemory() // Fallback
	}

	var annotated []int
	fmt.Printf("DEBUG: getFreshAnnotatedCPUs() - querying %d running containers for annotated CPUs\n", len(runningContainers))

	// Find annotated containers by looking for containers that have CPU annotations
	for _, container := range runningContainers {
		if container == nil {
			continue
		}

		// Check if this container is managed by an annotated pod
		if !m.isAnnotatedContainer(container) {
			continue
		}

		// Extract CPUs from container resources
		if container.Linux != nil && container.Linux.Resources != nil &&
			container.Linux.Resources.Cpu != nil && container.Linux.Resources.Cpu.Cpus != "" {
			cpus, err := numa.ParseCPUList(container.Linux.Resources.Cpu.Cpus)
			if err != nil {
				fmt.Printf("DEBUG: getFreshAnnotatedCPUs() - failed to parse CPU list for annotated container %s: %v\n", 
					safeShortID(container.Id), err)
				continue
			}

			fmt.Printf("DEBUG: getFreshAnnotatedCPUs() - annotated container %s has CPUs %v\n", safeShortID(container.Id), cpus)
			annotated = append(annotated, cpus...)
		}
	}

	fmt.Printf("DEBUG: getFreshAnnotatedCPUs() - STATELESS QUERY returned %d annotated CPUs: %v\n", len(annotated), annotated)
	return annotated
}

// getFreshAnnotatedCPUsFromMemory - fallback to in-memory maps (old approach)  
func (m *Manager) getFreshAnnotatedCPUsFromMemory() []int {
	var annotated []int
	fmt.Printf("DEBUG: getFreshAnnotatedCPUsFromMemory() - FALLBACK - annotRef map has %d entries\n", len(m.annotRef))
	for cpu := range m.annotRef {
		annotated = append(annotated, cpu)
	}
	fmt.Printf("DEBUG: getFreshAnnotatedCPUsFromMemory() - FALLBACK returned %d CPUs: %v\n", len(annotated), annotated)
	return annotated
}

// isContainerManagedByNRI checks if a container was allocated by our NRI plugin
func (m *Manager) isContainerManagedByNRI(container *api.Container) bool {
	// Strategy: Only count containers that have specific, exclusive CPU assignments
	// System containers typically have broad CPU access (0-63) and should be ignored
	
	if container.Linux == nil || container.Linux.Resources == nil ||
		container.Linux.Resources.Cpu == nil || container.Linux.Resources.Cpu.Cpus == "" {
		return false
	}

	cpus, err := numa.ParseCPUList(container.Linux.Resources.Cpu.Cpus)
	if err != nil {
		return false
	}

	// Heuristic: Containers managed by NRI plugin typically have:
	// - Specific, non-overlapping CPU assignments (not 0-63)
	// - Reasonable CPU counts (not all cores)
	// - Integer CPU semantics for exclusive allocation
	
	// If container has most/all CPUs, it's likely a system container
	if len(cpus) > 50 { // More than 50 CPUs suggests system container with broad access
		fmt.Printf("DEBUG: isContainerManagedByNRI() - container %s has %d CPUs (system container)\n", safeShortID(container.Id), len(cpus))
		return false
	}

	// Check if it's an integer container (which we manage)
	if m.isIntegerContainer(container) {
		fmt.Printf("DEBUG: isContainerManagedByNRI() - container %s is NRI-managed integer container\n", safeShortID(container.Id))
		return true
	}

	// TODO: Add logic for annotated containers when implemented
	// For now, assume non-integer containers are not managed by us
	fmt.Printf("DEBUG: isContainerManagedByNRI() - container %s is not managed by NRI\n", safeShortID(container.Id))
	return false
}

// isAnnotatedContainer checks if a container belongs to a pod with CPU annotations
func (m *Manager) isAnnotatedContainer(container *api.Container) bool {
	// For now, we can't easily determine this without pod information in the stateless context
	// This would require querying pod annotations from the API server
	// As a heuristic, assume containers with very specific CPU assignments (non-sequential) 
	// might be annotated containers
	
	// TODO: Implement proper pod annotation checking via Kubernetes API
	// For now, return false to avoid false positives
	fmt.Printf("DEBUG: isAnnotatedContainer() - container %s annotation check not implemented (assuming false)\n", safeShortID(container.Id))
	return false
}

// recordAnnotatedContainerUnsafe records an annotated container without locking (caller must hold lock)
func (m *Manager) recordAnnotatedContainerUnsafe(container *api.Container, pod *api.PodSandbox, cpus []int) {
	info := &ContainerInfo{
		ID:        container.Id,
		Mode:      ModeAnnotated,
		CPUs:      cpus,
		PodID:     container.PodSandboxId,
		PodUID:    pod.Uid,
		CreatedAt: time.Now(),
	}
	
	m.byCID[container.Id] = info
	
	// Update annotation reference counts
	for _, cpu := range cpus {
		m.annotRef[cpu]++
	}
	
	fmt.Printf("DEBUG: Recorded annotated container %s with CPUs %v\n", safeShortID(container.Id), cpus)
}

// findFreshConflictingContainers finds containers that conflict with requested CPUs by querying runtime state
func (m *Manager) findFreshConflictingContainers(requestedCPUs []int) (map[string]*ContainerInfo, error) {
	// Query fresh container state from runtime
	pods, containers, err := m.queryRunningPodsAndContainers()
	if err != nil {
		fmt.Printf("DEBUG: findFreshConflictingContainers() - pod-container query failed: %v, trying containers only\n", err)
		// Fallback to container-only query
		containers, err = m.queryRunningContainers()
		if err != nil {
			fmt.Printf("ERROR: findFreshConflictingContainers() failed to query running containers: %v\n", err)
			return nil, fmt.Errorf("failed to query containers: %w", err)
		}
		pods = nil // No pod information available
	}

	conflictingContainers := make(map[string]*ContainerInfo)
	requestedCPUSet := make(map[int]struct{})
	for _, cpu := range requestedCPUs {
		requestedCPUSet[cpu] = struct{}{}
	}

	fmt.Printf("DEBUG: findFreshConflictingContainers() - checking %d containers for conflicts with CPUs %v\n", 
		len(containers), requestedCPUs)

	// Check each container for CPU conflicts
	for _, container := range containers {
		if container == nil {
			continue
		}

		// Extract CPUs from container resources
		if container.Linux == nil || container.Linux.Resources == nil ||
			container.Linux.Resources.Cpu == nil || container.Linux.Resources.Cpu.Cpus == "" {
			continue
		}

		cpus, err := numa.ParseCPUList(container.Linux.Resources.Cpu.Cpus)
		if err != nil {
			fmt.Printf("DEBUG: findFreshConflictingContainers() - failed to parse CPU list for container %s: %v\n", 
				safeShortID(container.Id), err)
			continue
		}

		// Find the pod for proper classification
		var pod *api.PodSandbox
		if pods != nil && container.PodSandboxId != "" {
			pod = m.findPod(pods, container.PodSandboxId)
		}

		// Check if this container has exclusive CPU semantics (integer or annotated)
		if !m.hasExclusiveCPUSemantics(container, pod) {
			continue
		}

		// Check for conflicts with requested CPUs
		var conflictCPUs []int
		for _, cpu := range cpus {
			if _, isRequested := requestedCPUSet[cpu]; isRequested {
				conflictCPUs = append(conflictCPUs, cpu)
			}
		}

		if len(conflictCPUs) > 0 {
			// Create ContainerInfo for the conflicting container
			mode := "unknown"
			if pod != nil {
				mode = m.determineContainerMode(pod, container)
			}

			// Only count integer containers for reallocation (annotated containers can share)
			if mode == "integer" || (pod == nil && m.isIntegerContainer(container)) {
				info := &ContainerInfo{
					ID:     container.Id,
					Mode:   ModeInteger,
					CPUs:   cpus,
					PodID:  container.PodSandboxId,
					PodUID: "",
				}
				if pod != nil {
					info.PodUID = pod.Uid
				}

				conflictingContainers[container.Id] = info
				fmt.Printf("DEBUG: findFreshConflictingContainers() - found conflicting integer container %s with CPUs %v (conflicts: %v)\n", 
					safeShortID(container.Id), cpus, conflictCPUs)
			}
		}
	}

	fmt.Printf("DEBUG: findFreshConflictingContainers() - found %d conflicting containers\n", len(conflictingContainers))
	return conflictingContainers, nil
}

// calculateReallocationPlan calculates what integer containers need to be moved
func (m *Manager) calculateReallocationPlan(requestedCPUs []int, alloc Allocator) ([]ReallocationPlan, *api.ContainerAdjustment, error) {
	// Find conflicting integer containers from in-memory state (already synchronized)
	m.mu.RLock()
	conflictingContainers := make(map[string]*ContainerInfo)
	for _, cpu := range requestedCPUs {
		if containerID, exists := m.intOwner[cpu]; exists {
			if container := m.byCID[containerID]; container != nil && container.Mode == ModeInteger {
				conflictingContainers[containerID] = container
				fmt.Printf("DEBUG: Found conflicting integer container %s on CPU %d\n", safeShortID(containerID), cpu)
			}
		}
	}
	m.mu.RUnlock()
	
	if len(conflictingContainers) == 0 {
		// No conflicts found - this means the CPUs are available for direct allocation
		fmt.Printf("DEBUG: No conflicting containers found for requested CPUs %v - direct allocation possible\n", requestedCPUs)
		
		// Create adjustment for direct allocation
		adjustment := &api.ContainerAdjustment{
			Linux: &api.LinuxContainerAdjustment{
				Resources: &api.LinuxResources{
					Cpu: &api.LinuxCPU{
						Cpus: numa.FormatCPUList(requestedCPUs),
					},
				},
			},
		}
		
		// Return empty reallocation plan and valid adjustment
		return []ReallocationPlan{}, adjustment, nil
	}
	
	fmt.Printf("DEBUG: Found %d conflicting containers for reallocation\n", len(conflictingContainers))
	
	// Plan reallocation for each conflicting container
	var plans []ReallocationPlan
	allReserved := m.getReservedCPUsUnsafe()
	
	for containerID, container := range conflictingContainers {
		newCPUs, canReallocate := alloc.CanReallocateInteger(container.CPUs, requestedCPUs, allReserved)
		if !canReallocate {
			return nil, nil, fmt.Errorf("cannot reallocate integer container %s", containerID)
		}
		
		plans = append(plans, ReallocationPlan{
			ContainerID: containerID,
			OldCPUs:     container.CPUs,
			NewCPUs:     newCPUs,
			MemNodes:    nil, // No NUMA binding for integer containers
		})
		
		// Update allReserved for next iteration
		allReserved = m.updateReservedSetForPlan(allReserved, container.CPUs, newCPUs)
	}
	
	// Create adjustment for the annotated container
	adjustment := &api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: formatCPUListCommaSeparated(requestedCPUs),
				},
			},
		},
	}
	
	return plans, adjustment, nil
}

// executeReallocationPlanAtomic applies the reallocation plan and records the new annotated container
func (m *Manager) executeReallocationPlanAtomic(plans []ReallocationPlan, annotatedContainer *api.Container, pod *api.PodSandbox, requestedCPUs []int) ([]*api.ContainerUpdate, error) {
	// Create container updates for reallocation
	var updates []*api.ContainerUpdate
	
	for _, plan := range plans {
		update := &api.ContainerUpdate{
			ContainerId: plan.ContainerID,
			Linux: &api.LinuxContainerUpdate{
				Resources: &api.LinuxResources{
					Cpu: &api.LinuxCPU{
						Cpus: numa.FormatCPUList(plan.NewCPUs),
					},
				},
			},
		}
		updates = append(updates, update)
	}
	
	// Apply state changes atomically
	m.mu.Lock()
	
	// Remove old integer container assignments
	for _, plan := range plans {
		for _, cpu := range plan.OldCPUs {
			if m.intOwner[cpu] == plan.ContainerID {
				delete(m.intOwner, cpu)
			}
		}
	}
	
	// Add new integer container assignments
	for _, plan := range plans {
		container := m.byCID[plan.ContainerID]
		if container != nil {
			container.CPUs = plan.NewCPUs
			for _, cpu := range plan.NewCPUs {
				m.intOwner[cpu] = plan.ContainerID
			}
		}
	}
	
	// Record the annotated container
	m.recordAnnotatedContainerUnsafe(annotatedContainer, pod, requestedCPUs)
	
	m.mu.Unlock()
	
	return updates, nil
}

// StatelessUpdateContainer handles UpdateContainer calls with global locking
// This prevents race conditions with unsolicited UpdateContainer calls
func (m *Manager) StatelessUpdateContainer(containerID string) *api.ContainerUpdate {
	// GLOBAL ALLOCATION LOCK - prevents race conditions with ongoing allocations
	m.allocationMu.Lock()
	defer m.allocationMu.Unlock()

	// Get fresh container info
	m.mu.RLock()
	containerInfo := m.byCID[containerID]
	m.mu.RUnlock()

	if containerInfo == nil {
		fmt.Printf("DEBUG: StatelessUpdateContainer called for unknown container %s\n", safeShortID(containerID))
		return nil
	}

	// Return current CPU assignment to maintain consistency
	update := &api.ContainerUpdate{
		ContainerId: containerID,
		Linux: &api.LinuxContainerUpdate{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: m.formatCPUListForMode(containerInfo.CPUs, containerInfo.Mode),
				},
			},
		},
	}

	fmt.Printf("DEBUG: StatelessUpdateContainer returning CPU assignment %s for container %s (mode: %s)\n",
		update.Linux.Resources.Cpu.Cpus, safeShortID(containerID), containerInfo.Mode)

	return update
}

// formatCPUListForMode formats CPU list appropriately for the container mode
func (m *Manager) formatCPUListForMode(cpus []int, mode string) string {
	if len(cpus) == 0 {
		return ""
	}

	// Use comma-separated format for annotated containers for better NRI compatibility
	if mode == ModeAnnotated {
		return formatCPUListCommaSeparated(cpus)
	}

	// Use standard format for other containers
	return numa.FormatCPUList(cpus)
}

// getProjectedIntegerReservedCPUs computes integer-reserved CPUs after applying the reallocation plan
// This prevents temporal inconsistency in conflict checking during live reallocation
func (m *Manager) getProjectedIntegerReservedCPUs(reallocationPlan []ReallocationPlan) []int {
	// Start with current integer reservations
	projected := make(map[int]string)
	for cpu, containerID := range m.intOwner {
		projected[cpu] = containerID
	}

	// Apply reallocation plan to compute projected state
	for _, plan := range reallocationPlan {
		// Remove old CPUs from this container
		for _, cpu := range plan.OldCPUs {
			if projected[cpu] == plan.ContainerID {
				delete(projected, cpu)
			}
		}
		// Add new CPUs for this container
		for _, cpu := range plan.NewCPUs {
			projected[cpu] = plan.ContainerID
		}
	}

	// Convert to slice
	var result []int
	for cpu := range projected {
		result = append(result, cpu)
	}

	fmt.Printf("DEBUG: Projected integer reserved CPUs after reallocation: %v (current: %v)\n", result, m.getIntegerReservedCPUsUnsafe())
	return result
}

func (m *Manager) getReservedCPUsUnsafe() []int {
	reserved := make(map[int]struct{})

	// Add annotated CPUs
	for cpu := range m.annotRef {
		reserved[cpu] = struct{}{}
	}

	// Add integer CPUs
	for cpu := range m.intOwner {
		reserved[cpu] = struct{}{}
	}

	var result []int
	for cpu := range reserved {
		result = append(result, cpu)
	}

	return result
}

func (m *Manager) computeSharedPool(onlineCPUs []int) []int {
	reserved := make(map[int]struct{})

	// Add annotated CPUs
	for cpu := range m.annotRef {
		reserved[cpu] = struct{}{}
	}

	// Add integer CPUs
	for cpu := range m.intOwner {
		reserved[cpu] = struct{}{}
	}

	var sharedPool []int
	for _, cpu := range onlineCPUs {
		if _, isReserved := reserved[cpu]; !isReserved {
			sharedPool = append(sharedPool, cpu)
		}
	}

	return sharedPool
}

func (m *Manager) findPod(pods []*api.PodSandbox, sandboxID string) *api.PodSandbox {
	for _, pod := range pods {
		if pod.Id == sandboxID {
			return pod
		}
	}
	return nil
}

func (m *Manager) determineContainerMode(pod *api.PodSandbox, container *api.Container) string {
	// Check for annotation first
	if pod.Annotations != nil {
		if _, hasAnnotation := pod.Annotations[WekaAnnotation]; hasAnnotation {
			return "annotated"
		}
	}

	// Check for integer semantics
	if containerutil.HasIntegerSemantics(container) {
		return "integer"
	}

	return "shared"
}

func (m *Manager) IsReserved(cpu int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, isAnnotated := m.annotRef[cpu]
	_, isInteger := m.intOwner[cpu]

	return isAnnotated || isInteger
}

func (m *Manager) IsExclusive(cpu int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, isInteger := m.intOwner[cpu]
	return isInteger
}

// RecordAnnotatedContainer specifically handles recording annotated containers with proper reference counting
func (m *Manager) RecordAnnotatedContainer(container *api.Container, pod *api.PodSandbox, adjustment *api.ContainerAdjustment) error {
	// CRITICAL FIX: This function should not be used directly for new allocations due to race conditions
	// Use AtomicAllocateAndRecordAnnotated instead for new container allocations
	// This function is kept for backward compatibility and recovery scenarios
	return m.AtomicRecordAnnotatedContainer(container, pod, adjustment)
}

// AtomicRecordAnnotatedContainer records an annotated container with proper atomicity
// This fixes the race condition where periodic cleanup removes annotation references
func (m *Manager) AtomicRecordAnnotatedContainer(container *api.Container, pod *api.PodSandbox, adjustment *api.ContainerAdjustment) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if adjustment.Linux == nil || adjustment.Linux.Resources == nil || adjustment.Linux.Resources.Cpu == nil {
		return fmt.Errorf("invalid adjustment for annotated container")
	}

	cpuList := adjustment.Linux.Resources.Cpu.Cpus
	if cpuList == "" {
		return fmt.Errorf("no CPU list in adjustment")
	}

	cpus, err := numa.ParseCPUList(cpuList)
	if err != nil {
		return fmt.Errorf("error parsing CPU list for container %s: %w", container.Id, err)
	}

	// Remove any temporary container info that might have been created
	tempContainerID := fmt.Sprintf("pending-annotated-%s", pod.Uid)
	if tempInfo, exists := m.byCID[tempContainerID]; exists {
		// Remove the old annotation references that were added for the temporary container
		for _, cpu := range tempInfo.CPUs {
			if count := m.annotRef[cpu]; count > 1 {
				m.annotRef[cpu] = count - 1
			} else {
				delete(m.annotRef, cpu)
			}
		}
		delete(m.byCID, tempContainerID)
		fmt.Printf("DEBUG: Removed temporary annotated container %s\n", tempContainerID)
	}

	// Create container info
	info := &ContainerInfo{
		ID:        container.Id,
		Mode:      ModeAnnotated,
		CPUs:      cpus,
		PodID:     container.PodSandboxId,
		PodUID:    pod.Uid,
		CreatedAt: time.Now(),
	}

	m.byCID[container.Id] = info

	// Update annotation reference counts atomically
	for _, cpu := range cpus {
		m.annotRef[cpu]++
	}

	fmt.Printf("DEBUG: Atomically recorded annotated container %s with %d CPUs (annotRef now has %d entries)\n", 
		safeShortID(container.Id), len(cpus), len(m.annotRef))

	return nil
}

func (m *Manager) RecordIntegerContainer(container *api.Container, pod *api.PodSandbox, adjustment *api.ContainerAdjustment) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if adjustment.Linux == nil || adjustment.Linux.Resources == nil || adjustment.Linux.Resources.Cpu == nil {
		return fmt.Errorf("invalid adjustment for integer container")
	}

	cpuList := adjustment.Linux.Resources.Cpu.Cpus
	if cpuList == "" {
		return fmt.Errorf("no CPU list in adjustment")
	}

	cpus, err := numa.ParseCPUList(cpuList)
	if err != nil {
		return fmt.Errorf("error parsing CPU list for container %s: %w", container.Id, err)
	}

	// CRITICAL FIX: Atomic conflict detection and rejection
	// Check for CPU conflicts BEFORE recording and REJECT conflicting allocations
	var conflicts []int
	var conflictOwners []string
	
	for _, cpu := range cpus {
		if existingOwner := m.intOwner[cpu]; existingOwner != "" {
			conflicts = append(conflicts, cpu)
			conflictOwners = append(conflictOwners, safeShortID(existingOwner))
			fmt.Printf("ERROR: CPU %d ownership conflict - new container %s conflicts with existing %s\n", 
				cpu, safeShortID(container.Id), safeShortID(existingOwner))
		}
	}

	// REJECT allocation if any conflicts detected
	if len(conflicts) > 0 {
		return fmt.Errorf("CPU exclusivity violation: container %s attempted to use CPUs %v already owned by containers %v - allocation rejected", 
			safeShortID(container.Id), conflicts, conflictOwners)
	}

	// ENHANCED DEBUG LOG FOR RECORDING
	fmt.Printf("DEBUG: Recording integer container %s with CPUs %v (before: intOwner=%d entries)\n", 
		safeShortID(container.Id), cpus, len(m.intOwner))

	// Create container info
	info := &ContainerInfo{
		ID:        container.Id,
		Mode:      ModeInteger,
		CPUs:      cpus,
		PodID:     container.PodSandboxId,
		PodUID:    pod.Uid,
		CreatedAt: time.Now(),
	}

	m.byCID[container.Id] = info

	// Update integer ownership map (now safe since conflicts were checked)
	for _, cpu := range cpus {
		m.intOwner[cpu] = container.Id
		fmt.Printf("DEBUG: Assigned CPU %d to integer container %s\n", cpu, safeShortID(container.Id))
	}

	fmt.Printf("DEBUG: Recorded integer container %s with CPUs %v (after: intOwner=%d entries)\n", 
		safeShortID(container.Id), cpus, len(m.intOwner))
	
	// CRITICAL FIX: Validate state consistency after recording
	m.validateStateConsistency()
	
	return nil
}

// validateStateConsistency detects and reports state corruption issues
func (m *Manager) validateStateConsistency() {
	// Check for orphaned intOwner entries
	orphanedOwners := 0
	orphanedCPUs := make([]int, 0)
	for cpu, containerID := range m.intOwner {
		if info := m.byCID[containerID]; info == nil {
			fmt.Printf("WARNING: State corruption - CPU %d owned by missing container %s\n", 
				cpu, safeShortID(containerID))
			orphanedOwners++
			orphanedCPUs = append(orphanedCPUs, cpu)
		} else if info.Mode != ModeInteger {
			fmt.Printf("WARNING: State corruption - CPU %d owned by non-integer container %s (mode=%s)\n", 
				cpu, safeShortID(containerID), info.Mode)
		}
	}

	// Check for CPU ownership conflicts within integer containers
	cpuOwnershipCount := make(map[int]int)
	for _, info := range m.byCID {
		if info.Mode == ModeInteger {
			for _, cpu := range info.CPUs {
				cpuOwnershipCount[cpu]++
				if cpuOwnershipCount[cpu] > 1 {
					fmt.Printf("ERROR: State corruption - CPU %d assigned to multiple integer containers (conflict detected)\n", cpu)
				}
			}
		}
	}

	// Check for orphaned annotRef entries  
	orphanedAnnotations := 0
	for cpu, count := range m.annotRef {
		hasAnnotatedOwner := false
		for _, info := range m.byCID {
			if info.Mode == ModeAnnotated {
				for _, infoCPU := range info.CPUs {
					if infoCPU == cpu {
						hasAnnotatedOwner = true
						break
					}
				}
			}
		}
		if !hasAnnotatedOwner {
			fmt.Printf("WARNING: State corruption - CPU %d has annotation count %d but no annotated container\n", 
				cpu, count)
			orphanedAnnotations++
		}
	}

	if orphanedOwners > 0 || orphanedAnnotations > 0 {
		fmt.Printf("ERROR: State corruption detected - %d orphaned owners, %d orphaned annotations\n", 
			orphanedOwners, orphanedAnnotations)
	}
}

