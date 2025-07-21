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

// AllocateAnnotatedWithReallocation attempts to allocate an annotated container with potential integer reallocation
func (m *Manager) AllocateAnnotatedWithReallocation(pod *api.PodSandbox, alloc Allocator) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// First, try normal allocation without reallocation
	integerReserved := m.getIntegerReservedCPUsUnsafe()
	fmt.Printf("DEBUG: Current integer reserved CPUs: %v\n", integerReserved)
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

	// Execute reallocation plan
	fmt.Printf("DEBUG: Executing reallocation plan with %d containers\n", len(reallocationPlan))
	updates, err := m.executeReallocationPlan(reallocationPlan, alloc)
	if err != nil {
		fmt.Printf("DEBUG: Reallocation execution failed: %v\n", err)
		return nil, nil, fmt.Errorf("failed to execute reallocation plan: %w", err)
	}
	fmt.Printf("DEBUG: Generated %d container updates for live reallocation\n", len(updates))

	// Now allocate the annotated container (should succeed)
	result, err = alloc.HandleAnnotatedContainerWithIntegerConflictCheck(pod, m.getIntegerReservedCPUsUnsafe())
	if err != nil {
		// Rollback the reallocation changes
		m.rollbackReallocation(reallocationPlan)
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

		// Calculate NUMA nodes for new CPU assignment
		memNodes := m.calculateMemNodes(newCPUs, alloc)

		plans = append(plans, ReallocationPlan{
			ContainerID: containerID,
			OldCPUs:     container.CPUs,
			NewCPUs:     newCPUs,
			MemNodes:    memNodes,
		})

		// Update allReserved for next iteration
		// Remove old CPUs and add new CPUs to reserved set
		allReserved = m.updateReservedSetForPlan(allReserved, container.CPUs, newCPUs)
	}

	return plans, nil
}

// calculateMemNodes calculates NUMA memory nodes for given CPUs
func (m *Manager) calculateMemNodes(cpus []int, alloc Allocator) []int {
	// This is a simplified implementation - in a real scenario we'd access the NUMA manager
	// For now, assume we can calculate this from the allocator
	return []int{0} // Simplified
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

// executeReallocationPlan executes the reallocation plan and updates internal state
func (m *Manager) executeReallocationPlan(plans []ReallocationPlan, alloc Allocator) ([]*api.ContainerUpdate, error) {
	var updates []*api.ContainerUpdate

	for _, plan := range plans {
		// Create container update
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

		// Update internal state
		container := m.byCID[plan.ContainerID]
		if container == nil {
			return nil, fmt.Errorf("container %s not found in state", plan.ContainerID)
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

	return updates, nil
}

// rollbackReallocation reverts the reallocation changes in case of failure
func (m *Manager) rollbackReallocation(plans []ReallocationPlan) {
	for _, plan := range plans {
		container := m.byCID[plan.ContainerID]
		if container == nil {
			continue
		}

		// Remove new CPU ownership
		for _, cpu := range plan.NewCPUs {
			delete(m.intOwner, cpu)
		}

		// Restore old CPU ownership
		for _, cpu := range plan.OldCPUs {
			m.intOwner[cpu] = plan.ContainerID
		}

		// Restore container info
		container.CPUs = plan.OldCPUs
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

	// Clear existing state
	m.annotRef = make(map[int]int)
	m.intOwner = make(map[int]string)
	m.byCID = make(map[string]*ContainerInfo)

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

	for _, container := range annotatedContainers {
		pod := m.findPod(pods, container.PodSandboxId)
		if pod == nil {
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
			fmt.Printf("Error allocating CPUs for annotated container %s: %v\n", container.Id, err)
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
	}

	// Phase 2: Process integer containers (medium priority)
	for _, container := range integerContainers {
		pod := m.findPod(pods, container.PodSandboxId)
		if pod == nil {
			continue
		}

		reserved := m.getReservedCPUsUnsafe()

		cpus, _, err := alloc.AllocateContainerCPUs(pod, container, reserved)
		if err != nil {
			fmt.Printf("Error allocating CPUs for integer container %s: %v\n", container.Id, err)
			continue
		}

		// Integer container allocation successful

		info := &ContainerInfo{
			ID:     container.Id,
			Mode:   ModeInteger,
			CPUs:   cpus,
			PodID:  container.PodSandboxId,
			PodUID: pod.Uid,
		}

		m.byCID[container.Id] = info

		// Update integer ownership
		for _, cpu := range cpus {
			m.intOwner[cpu] = container.Id
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
	return reserved
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
