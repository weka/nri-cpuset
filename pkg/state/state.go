package state

import (
	"fmt"
	"sync"

	"github.com/containerd/nri/pkg/api"
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
}

func (m *Manager) Synchronize(pods []*api.PodSandbox, containers []*api.Container, alloc Allocator) ([]*api.ContainerUpdate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear existing state
	m.annotRef = make(map[int]int)
	m.intOwner = make(map[int]string)
	m.byCID = make(map[string]*ContainerInfo)

	updates := make([]*api.ContainerUpdate, 0)

	// First pass: process annotated and integer containers to build reserved set
	for _, container := range containers {
		pod := m.findPod(pods, container.PodSandboxId)
		if pod == nil {
			continue
		}

		mode := m.determineContainerMode(pod, container)
		// Get reserved CPUs before processing each container to avoid lock contention
		reserved := m.getReservedCPUsUnsafe()
		cpus, err := m.getContainerCPUs(pod, container, mode, alloc, reserved)
		if err != nil {
			// Log error but continue with other containers
			fmt.Printf("Error getting CPUs for container %s: %v\n", container.Id, err)
			continue
		}

		info := &ContainerInfo{
			ID:     container.Id,
			Mode:   mode,
			CPUs:   cpus,
			PodID:  container.PodSandboxId,
			PodUID: pod.Uid,
		}

		m.byCID[container.Id] = info

		// Update reservation maps
		switch mode {
		case ModeAnnotated:
			for _, cpu := range cpus {
				m.annotRef[cpu]++
			}
		case ModeInteger:
			for _, cpu := range cpus {
				m.intOwner[cpu] = container.Id
			}
		}
	}

	// Second pass: update shared containers
	sharedPool := m.computeSharedPool(alloc.GetOnlineCPUs())
	
	for _, container := range containers {
		info := m.byCID[container.Id]
		if info == nil || info.Mode != ModeShared {
			continue
		}

		// Update shared container to use current shared pool
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
			
			// Update our state
			info.CPUs = sharedPool
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

	// Check CPU limits and requests are equal and integer
	if cpu.Quota == nil || cpu.Period == nil || cpu.Quota.GetValue() <= 0 || cpu.Period.GetValue() <= 0 {
		return false
	}

	quota := cpu.Quota.GetValue()
	period := int64(cpu.Period.GetValue())
	if quota%period != 0 {
		return false
	}

	cpuCores := quota / period
	if cpuCores <= 0 {
		return false
	}

	// Check memory limits and requests are equal
	if memory.Limit == nil || memory.Limit.GetValue() <= 0 {
		return false
	}

	// For simplicity, we assume memory requirements are met if limit is set
	// In a real implementation, you might want to check request == limit
	
	return true
}

func (m *Manager) getContainerCPUs(pod *api.PodSandbox, container *api.Container, mode ContainerMode, alloc Allocator, reserved []int) ([]int, error) {
	switch mode {
	case ModeAnnotated:
		if pod.Annotations == nil {
			return nil, fmt.Errorf("missing annotations for annotated container")
		}
		
		cpuList, exists := pod.Annotations[WekaAnnotation]
		if !exists {
			return nil, fmt.Errorf("missing %s annotation", WekaAnnotation)
		}
		
		return numa.ParseCPUList(cpuList)
		
	case ModeInteger:
		if container.Linux == nil || container.Linux.Resources == nil || container.Linux.Resources.Cpu == nil {
			return nil, fmt.Errorf("missing CPU resources for integer container")
		}
		
		cpu := container.Linux.Resources.Cpu
		if cpu.Quota == nil || cpu.Period == nil || cpu.Quota.GetValue() <= 0 || cpu.Period.GetValue() <= 0 {
			return nil, fmt.Errorf("invalid CPU quota/period for integer container")
		}
		
		cpuCores := int(cpu.Quota.GetValue() / int64(cpu.Period.GetValue()))
		return alloc.AllocateExclusiveCPUs(cpuCores, reserved)
		
	case ModeShared:
		return m.computeSharedPool(alloc.GetOnlineCPUs()), nil
		
	default:
		return nil, fmt.Errorf("unknown container mode: %d", mode)
	}
}

func (m *Manager) IsExclusive(cpu int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	_, isInteger := m.intOwner[cpu]
	return isInteger
}

func (m *Manager) IsReserved(cpu int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	_, isAnnotated := m.annotRef[cpu]
	_, isInteger := m.intOwner[cpu]
	
	return isAnnotated || isInteger
}

