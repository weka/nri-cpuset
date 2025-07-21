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

type ContainerMode int

const (
	ModeShared ContainerMode = iota
	ModeInteger
	ModeAnnotated
)

type ContainerInfo struct {
	ID     string
	Mode   ContainerMode
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
}

func (m *Manager) Synchronize(pods []*api.PodSandbox, containers []*api.Container, alloc Allocator) ([]*api.ContainerUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear existing state
	m.annotRef = make(map[int]int)
	m.intOwner = make(map[int]string)
	m.byCID = make(map[string]*ContainerInfo)

	updates := make([]*api.ContainerUpdate, 0)

	// Group containers by mode for proper priority processing
	var annotatedContainers, integerContainers, sharedContainers []*api.Container

	for _, container := range containers {
		pod := m.findPod(pods, container.PodSandboxId)
		if pod == nil {
			continue
		}

		mode := m.determineContainerMode(pod, container)
		switch mode {
		case ModeAnnotated:
			annotatedContainers = append(annotatedContainers, container)
		case ModeInteger:
			integerContainers = append(integerContainers, container)
		case ModeShared:
			sharedContainers = append(sharedContainers, container)
		}
	}

	fmt.Printf("=== Starting Synchronize: %d pods, %d containers ===\n", len(pods), len(containers))
	fmt.Printf("Container types: %d annotated, %d integer, %d shared\n", len(annotatedContainers), len(integerContainers), len(sharedContainers))

	// Process containers in priority order: annotated, then integer, then shared

	// Phase 1: Process annotated containers first (highest priority)
	for _, container := range annotatedContainers {
		pod := m.findPod(pods, container.PodSandboxId)
		if pod == nil {
			continue
		}

		// For annotated containers, we need to check only for conflicts with INTEGER containers
		// Sharing between annotated containers is allowed
		integerReserved := make([]int, 0)
		for cpu, _ := range m.intOwner {
			integerReserved = append(integerReserved, cpu)
		}

		result, err := alloc.HandleAnnotatedContainerWithIntegerConflictCheck(pod, integerReserved)
		if err != nil {
			fmt.Printf("Error allocating CPUs for annotated container %s: %v\n", container.Id, err)
			continue
		}

		fmt.Printf("Annotated container %s: allocated CPUs %v\n", container.Id, result.CPUs)

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

		fmt.Printf("Integer container %s: allocated CPUs %v\n", container.Id, cpus)

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
	if len(sharedContainers) > 0 {
		fmt.Printf("Shared containers: using CPU pool %v\n", sharedPool)
	}

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

	// Mark annotated CPUs as reserved
	for cpu := range m.annotRef {
		reserved[cpu] = struct{}{}
	}

	// Mark integer CPUs as reserved
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

func (m *Manager) findPod(pods []*api.PodSandbox, podID string) *api.PodSandbox {
	for _, pod := range pods {
		if pod.Id == podID {
			return pod
		}
	}
	return nil
}

func (m *Manager) determineContainerMode(pod *api.PodSandbox, container *api.Container) ContainerMode {
	// Check for annotation first
	if pod.Annotations != nil {
		if _, hasAnnotation := pod.Annotations[WekaAnnotation]; hasAnnotation {
			return ModeAnnotated
		}
	}

	// Check for integer semantics
	if m.hasIntegerSemantics(container) {
		return ModeInteger
	}

	return ModeShared
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

	// Check CPU quota and period are set for limits
	if cpu.Quota == nil || cpu.Period == nil || cpu.Quota.GetValue() <= 0 || cpu.Period.GetValue() <= 0 {
		return false
	}

	// Check memory limit is set
	if memory.Limit == nil || memory.Limit.GetValue() <= 0 {
		return false
	}

	// Check that limits.cpu is an integer
	quota := cpu.Quota.GetValue()
	period := int64(cpu.Period.GetValue())
	if quota%period != 0 {
		return false
	}

	cpuCores := quota / period
	if cpuCores <= 0 {
		return false
	}

	// CRITICAL: Check that requests == limits for both CPU and memory
	// This is required by the PRD for integer pod classification

	// Check CPU: requests == limits
	if cpu.Shares == nil {
		// If no shares (requests) are set but quota/period (limits) are set,
		// then requests != limits, so this is not an integer container
		return false
	}

	// Convert shares to CPU value: shares / 1024 should equal quota/period
	// Kubernetes uses 1024 shares per CPU core
	requestedCPUs := float64(cpu.Shares.GetValue()) / 1024.0
	limitCPUs := float64(quota) / float64(period)

	// Allow small floating point differences but they should be essentially equal
	if abs(requestedCPUs-limitCPUs) > 0.001 {
		return false
	}

	// Check Memory: requests == limits
	// Note: Memory requests are not directly available in the NRI Container object
	// In practice, Kubernetes QoS class "Guaranteed" requires requests == limits
	// For now, we'll accept any memory configuration since we can't easily verify
	// the memory request from the NRI interface. The CPU check is the main criterion.

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
