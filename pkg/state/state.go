package state

import (
	"fmt"
	"sync"

	"github.com/containerd/nri/pkg/api"
	"github.com/weka/nri-cpuset/pkg/allocator"
	"github.com/weka/nri-cpuset/pkg/numa"
)

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
	ID     string
	Mode   string
	CPUs   []int
	PodID  string
	PodUID string
}

type Manager struct {
	mu sync.RWMutex

	// annotRef[cpu] -> refcount (annotated sharing)
	annotRef map[int]int

	// intOwner[cpu] -> containerID (integer exclusivity)
	intOwner map[int]string

	// byCID -> ContainerInfo (reverse lookup)
	byCID map[string]*ContainerInfo

	// pendingReallocationPlan stores the plan that needs to be applied after successful updates
	// This is now instance-based to prevent race conditions between concurrent allocations
	pendingReallocationPlan []ReallocationPlan
}

func NewManager() *Manager {
	return &Manager{
		annotRef: make(map[int]int),
		intOwner: make(map[int]string),
		byCID:    make(map[string]*ContainerInfo),
	}
}

type Allocator interface {
	GetOnlineCPUs() []int
	AllocateExclusiveCPUs(count int, reserved []int) ([]int, error)
	// Add the new unified allocation method
	AllocateContainerCPUs(pod *api.PodSandbox, container *api.Container, reserved []int) ([]int, string, error)
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

// AllocateAnnotatedWithReallocation attempts to allocate an annotated container with potential integer reallocation
func (m *Manager) AllocateAnnotatedWithReallocation(pod *api.PodSandbox, alloc Allocator) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// First, try normal allocation without reallocation
	integerReserved := m.getIntegerReservedCPUsUnsafe()
	fmt.Printf("DEBUG: Current integer reserved CPUs: %v\n", integerReserved)
	fmt.Printf("DEBUG: Current integer containers in state: %d\n", len(m.intOwner))
	for cpu, containerID := range m.intOwner {
		if container := m.byCID[containerID]; container != nil {
			fmt.Printf("DEBUG: Integer container %s owns CPU %d (has CPUs %v)\n", containerID[:12], cpu, container.CPUs)
		}
	}
	result, err := alloc.HandleAnnotatedContainerWithIntegerConflictCheck(pod, integerReserved)
	if err == nil {
		// No conflicts, proceed normally
		fmt.Printf("DEBUG: No conflicts detected for annotated pod %s/%s\n", pod.Namespace, pod.Name)
		return m.createAnnotatedAdjustment(result), nil, nil
	}

	// Conflicts detected, attempt live reallocation
	fmt.Printf("DEBUG: Conflicts detected for annotated pod %s/%s, attempting live reallocation: %v\n", pod.Namespace, pod.Name, err)
	cpuList, exists := pod.Annotations[WekaAnnotation]
	if !exists {
		return nil, nil, fmt.Errorf("missing %s annotation", WekaAnnotation)
	}

	requestedCPUs, err := numa.ParseCPUList(cpuList)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid CPU list in annotation: %w", err)
	}

	// Find conflicting integer containers
	conflictingContainers := m.findConflictingIntegerContainers(requestedCPUs)
	if len(conflictingContainers) == 0 {
		return nil, nil, fmt.Errorf("unknown conflict with annotated container allocation")
	}

	// Plan reallocation for conflicting containers
	fmt.Printf("DEBUG: Found %d conflicting integer containers\n", len(conflictingContainers))
	reallocationPlan, err := m.planReallocation(conflictingContainers, requestedCPUs, alloc)
	if err != nil {
		fmt.Printf("DEBUG: Reallocation planning failed: %v\n", err)
		return nil, nil, fmt.Errorf("cannot reallocate conflicting integer containers: %w", err)
	}

	// Execute reallocation plan (create updates but don't apply state changes yet)
	fmt.Printf("DEBUG: Executing reallocation plan with %d containers\n", len(reallocationPlan))
	updates, err := m.executeReallocationPlan(reallocationPlan, alloc)
	if err != nil {
		fmt.Printf("DEBUG: Reallocation execution failed: %v\n", err)
		return nil, nil, fmt.Errorf("failed to execute reallocation plan: %w", err)
	}
	fmt.Printf("DEBUG: Generated %d container updates for live reallocation\n", len(updates))

	// Store the plan for later application after successful container updates
	m.pendingReallocationPlan = reallocationPlan

	// Now allocate the annotated container using projected state (after reallocation)
	projectedIntegerReserved := m.getProjectedIntegerReservedCPUs(reallocationPlan)
	result, err = alloc.HandleAnnotatedContainerWithIntegerConflictCheck(pod, projectedIntegerReserved)
	if err != nil {
		// Clear pending plan on failure
		m.pendingReallocationPlan = nil
		return nil, nil, fmt.Errorf("annotated container allocation failed after reallocation: %w", err)
	}

	adjustment := m.createAnnotatedAdjustment(result)
	return adjustment, updates, nil
}

// findConflictingIntegerContainers identifies integer containers using the requested CPUs
func (m *Manager) findConflictingIntegerContainers(requestedCPUs []int) map[string]*ContainerInfo {
	conflicting := make(map[string]*ContainerInfo)

	for _, cpu := range requestedCPUs {
		if containerID, exists := m.intOwner[cpu]; exists {
			if container := m.byCID[containerID]; container != nil && container.Mode == ModeInteger {
				conflicting[containerID] = container
			}
		}
	}

	return conflicting
}

// planReallocation creates a reallocation plan for conflicting containers
func (m *Manager) planReallocation(conflictingContainers map[string]*ContainerInfo, annotatedCPUs []int, alloc Allocator) ([]ReallocationPlan, error) {
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
	adjustment := &api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: numa.FormatCPUList(result.CPUs),
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

func (m *Manager) Synchronize(pods []*api.PodSandbox, containers []*api.Container, alloc Allocator) ([]*api.ContainerUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear existing state (including any invalid containers from previous runs)
	m.annotRef = make(map[int]int)
	m.intOwner = make(map[int]string)
	m.byCID = make(map[string]*ContainerInfo)
	// Also clear any pending reallocation plans from previous failed operations
	m.pendingReallocationPlan = nil

	// Separate containers by type for prioritized processing
	var annotatedContainers []*api.Container
	var integerContainers []*api.Container
	var sharedContainers []*api.Container

	for _, container := range containers {
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
				ID:     container.Id,
				Mode:   "invalid-annotated", // Special mode for problematic containers
				CPUs:   nil,                 // No CPUs allocated
				PodID:  container.PodSandboxId,
				PodUID: pod.Uid,
			}
			m.byCID[container.Id] = info
			skippedContainers = append(skippedContainers, container.Id)
			continue
		}

		// Annotated container allocation successful
		info := &ContainerInfo{
			ID:     container.Id,
			Mode:   ModeAnnotated,
			CPUs:   result.CPUs,
			PodID:  container.PodSandboxId,
			PodUID: pod.Uid,
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
			containerShortID := container.Id
			if len(containerShortID) > 12 {
				containerShortID = containerShortID[:12]
			}
			fmt.Printf("DEBUG: Adding CPU restriction update for annotated container %s: %v\n", containerShortID, result.CPUs)
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

		// Update integer ownership - resolve conflicts with annotated containers
		var conflictFreeCPUs []int
		for _, cpu := range currentCPUs {
			if _, hasAnnotated := m.annotRef[cpu]; hasAnnotated {
				fmt.Printf("WARNING: CPU %d conflict detected - assigned to both integer container %s and annotated containers during sync\n", cpu, container.Id)
				// Per PRD: annotated containers have priority, integer containers should be reallocated
				// During sync, we'll exclude this CPU from integer container assignment
			} else {
				m.intOwner[cpu] = container.Id
				conflictFreeCPUs = append(conflictFreeCPUs, cpu)
			}
		}

		// If we have conflicts, update the container info to reflect only non-conflicting CPUs
		if len(conflictFreeCPUs) != len(currentCPUs) {
			fmt.Printf("DEBUG: Integer container %s had %d CPU conflicts, now assigned %d CPUs: %v\n", 
				container.Id, len(currentCPUs)-len(conflictFreeCPUs), len(conflictFreeCPUs), conflictFreeCPUs)
			info.CPUs = conflictFreeCPUs
			currentCPUs = conflictFreeCPUs // Update for the container update below
		}

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
			containerShortID := container.Id
			if len(containerShortID) > 12 {
				containerShortID = containerShortID[:12]
			}
			fmt.Printf("DEBUG: Adding CPU restriction update for integer container %s: %v\n", containerShortID, currentCPUs)
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
			ID:     container.Id,
			Mode:   ModeShared,
			CPUs:   sharedPool,
			PodID:  container.PodSandboxId,
			PodUID: pod.Uid,
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
		return nil, nil
	}

	// Release reservations
	switch info.Mode {
	case ModeAnnotated:
		for _, cpu := range info.CPUs {
			if count := m.annotRef[cpu]; count > 1 {
				m.annotRef[cpu] = count - 1
			} else {
				delete(m.annotRef, cpu)
			}
		}
	case ModeInteger:
		for _, cpu := range info.CPUs {
			delete(m.intOwner, cpu)
		}
	case ModeShared:
		// No specific reservations to release for shared containers
	case "invalid-annotated", "invalid-integer":
		// Invalid containers don't have CPU reservations to release
		fmt.Printf("DEBUG: Removing invalid container %s (mode: %s)\n", containerID, info.Mode)
	}

	delete(m.byCID, containerID)

	// Recompute shared pool and update shared containers
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
	if m.hasIntegerSemantics(container) {
		return "integer"
	}

	return "shared"
}

func (m *Manager) hasIntegerSemantics(container *api.Container) bool {
	if container.Linux == nil || container.Linux.Resources == nil {
		return false
	}

	cpu := container.Linux.Resources.Cpu
	memory := container.Linux.Resources.Memory

	if cpu == nil || memory == nil {
		return false
	}

	// Check CPU requirements
	if cpu.Quota == nil || cpu.Period == nil || cpu.Quota.GetValue() <= 0 || cpu.Period.GetValue() <= 0 {
		return false
	}

	// Check if CPU limit is an integer
	if cpu.Quota.GetValue()%int64(cpu.Period.GetValue()) != 0 {
		return false
	}

	// Check memory requirements
	if memory.Limit == nil || memory.Limit.GetValue() <= 0 {
		return false
	}

	return true
}

// Helper function for floating point comparison
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
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

	// Create container info
	info := &ContainerInfo{
		ID:     container.Id,
		Mode:   ModeAnnotated,
		CPUs:   cpus,
		PodID:  container.PodSandboxId,
		PodUID: pod.Uid,
	}

	m.byCID[container.Id] = info

	// Update annotation reference counts
	for _, cpu := range cpus {
		m.annotRef[cpu]++
	}

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

	// Create container info
	info := &ContainerInfo{
		ID:     container.Id,
		Mode:   ModeInteger,
		CPUs:   cpus,
		PodID:  container.PodSandboxId,
		PodUID: pod.Uid,
	}

	m.byCID[container.Id] = info

	// Update integer ownership map
	for _, cpu := range cpus {
		m.intOwner[cpu] = container.Id
	}

	fmt.Printf("DEBUG: Recorded integer container %s with CPUs %v\n", container.Id, cpus)
	return nil
}
