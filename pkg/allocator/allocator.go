package allocator

import (
	"fmt"
	"sort"

	"github.com/containerd/nri/pkg/api"
	"github.com/weka/nri-cpuset/pkg/numa"
)

const (
	WekaAnnotation = "weka.io/cores-ids"
)

type CPUAllocator struct {
	numa       *numa.Manager
	onlineCPUs []int
}

type AllocationResult struct {
	CPUs     []int
	MemNodes []int
	Mode     string
}

func NewCPUAllocator(numaManager *numa.Manager) (*CPUAllocator, error) {
	return &CPUAllocator{
		numa:       numaManager,
		onlineCPUs: numaManager.GetOnlineCPUs(),
	}, nil
}

func (a *CPUAllocator) GetOnlineCPUs() []int {
	return append([]int(nil), a.onlineCPUs...)
}

func (a *CPUAllocator) AllocateContainer(pod *api.PodSandbox, container *api.Container, reserved []int) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	mode := a.determineContainerMode(pod, container)

	var result *AllocationResult
	var err error

	switch mode {
	case "annotated":
		result, err = a.handleAnnotatedContainer(pod, reserved)
	case "integer":
		result, err = a.handleIntegerContainer(container, reserved)
	case "shared":
		result, err = a.handleSharedContainer(reserved)
	default:
		return nil, nil, fmt.Errorf("unknown container mode: %s", mode)
	}

	if err != nil {
		return nil, nil, err
	}

	// Create container adjustment
	adjustment := &api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: numa.FormatCPUList(result.CPUs),
				},
			},
		},
	}

	// Set memory nodes for exclusive containers (annotated and integer)
	// Per PRD 3.3: restrict memory to NUMA nodes containing the assigned CPUs
	if (mode == "annotated" || mode == "integer") && len(result.MemNodes) > 0 {
		adjustment.Linux.Resources.Cpu.Mems = numa.FormatCPUList(result.MemNodes)
	}
	// Shared containers inherit system default memory placement (no restriction)

	return adjustment, nil, nil
}

func (a *CPUAllocator) AllocateExclusiveCPUs(count int, reserved []int) ([]int, error) {
	if count <= 0 {
		return nil, fmt.Errorf("invalid CPU count: %d", count)
	}

	reservedSet := make(map[int]struct{})
	for _, cpu := range reserved {
		reservedSet[cpu] = struct{}{}
	}

	var available []int
	for _, cpu := range a.onlineCPUs {
		if _, isReserved := reservedSet[cpu]; !isReserved {
			available = append(available, cpu)
		}
	}

	if len(available) < count {
		return nil, fmt.Errorf("insufficient free CPUs: need %d, have %d", count, len(available))
	}

	// Sort available CPUs for deterministic allocation
	sort.Ints(available)

	// Allocate first N available CPUs
	return available[:count], nil
}

func (a *CPUAllocator) determineContainerMode(pod *api.PodSandbox, container *api.Container) string {
	// Check for annotation first
	if pod.Annotations != nil {
		if _, hasAnnotation := pod.Annotations[WekaAnnotation]; hasAnnotation {
			return "annotated"
		}
	}

	// Check for integer semantics
	if a.hasIntegerSemantics(container) {
		return "integer"
	}

	return "shared"
}

func (a *CPUAllocator) hasIntegerSemantics(container *api.Container) bool {
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

	// Check memory limits are set
	if memory.Limit == nil || memory.Limit.GetValue() <= 0 {
		return false
	}

	return true
}

func (a *CPUAllocator) handleAnnotatedContainer(pod *api.PodSandbox, reserved []int) (*AllocationResult, error) {
	if pod.Annotations == nil {
		return nil, fmt.Errorf("missing annotations for annotated container")
	}

	cpuList, exists := pod.Annotations[WekaAnnotation]
	if !exists {
		return nil, fmt.Errorf("missing %s annotation", WekaAnnotation)
	}

	cpus, err := numa.ParseCPUList(cpuList)
	if err != nil {
		return nil, fmt.Errorf("invalid CPU list in annotation '%s': %w", cpuList, err)
	}

	// Validate CPUs are online
	onlineSet := make(map[int]struct{})
	for _, cpu := range a.onlineCPUs {
		onlineSet[cpu] = struct{}{}
	}

	for _, cpu := range cpus {
		if _, isOnline := onlineSet[cpu]; !isOnline {
			return nil, fmt.Errorf("CPU %d is not online", cpu)
		}
	}

	// Check for conflicts with reserved CPUs
	reservedSet := make(map[int]struct{})
	for _, cpu := range reserved {
		reservedSet[cpu] = struct{}{}
	}

	for _, cpu := range cpus {
		if _, isReserved := reservedSet[cpu]; isReserved {
			return nil, fmt.Errorf("CPU %d is already reserved by another container", cpu)
		}
	}

	// Determine NUMA nodes for memory placement
	// Per PRD 3.3: If all CPUs belong to one NUMA node, use only that node
	// Otherwise, use the union of all involved nodes
	memNodes := a.numa.GetCPUNodesUnion(cpus)
	
	// Always check if all CPUs belong to the same node for correctness
	if singleNode, isSingleNode := a.getSingleNUMANode(cpus); isSingleNode {
		memNodes = []int{singleNode}
	}

	return &AllocationResult{
		CPUs:     cpus,
		MemNodes: memNodes,
		Mode:     "annotated",
	}, nil
}

func (a *CPUAllocator) handleIntegerContainer(container *api.Container, reserved []int) (*AllocationResult, error) {
	if container.Linux == nil || container.Linux.Resources == nil || container.Linux.Resources.Cpu == nil {
		return nil, fmt.Errorf("missing CPU resources for integer container")
	}

	cpu := container.Linux.Resources.Cpu
	if cpu.Quota == nil || cpu.Period == nil || cpu.Quota.GetValue() <= 0 || cpu.Period.GetValue() <= 0 {
		return nil, fmt.Errorf("invalid CPU quota/period for integer container")
	}

	cpuCores := int(cpu.Quota.GetValue() / int64(cpu.Period.GetValue()))

	cpus, err := a.AllocateExclusiveCPUs(cpuCores, reserved)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate exclusive CPUs: %w", err)
	}

	// Determine NUMA nodes for memory placement
	// Per PRD 3.3: restrict memory to NUMA nodes containing the assigned CPUs
	memNodes := a.numa.GetCPUNodesUnion(cpus)
	
	// Always check if all CPUs belong to the same node for correctness
	if singleNode, isSingleNode := a.getSingleNUMANode(cpus); isSingleNode {
		memNodes = []int{singleNode}
	}

	return &AllocationResult{
		CPUs:     cpus,
		MemNodes: memNodes,
		Mode:     "integer",
	}, nil
}

func (a *CPUAllocator) handleSharedContainer(reserved []int) (*AllocationResult, error) {
	reservedSet := make(map[int]struct{})
	for _, cpu := range reserved {
		reservedSet[cpu] = struct{}{}
	}

	var sharedPool []int
	for _, cpu := range a.onlineCPUs {
		if _, isReserved := reservedSet[cpu]; !isReserved {
			sharedPool = append(sharedPool, cpu)
		}
	}

	if len(sharedPool) == 0 {
		return nil, fmt.Errorf("shared CPU pool is empty")
	}

	return &AllocationResult{
		CPUs: sharedPool,
		Mode: "shared",
	}, nil
}

func (a *CPUAllocator) ValidateAnnotatedCPUs(cpuList string, reserved []int) error {
	cpus, err := numa.ParseCPUList(cpuList)
	if err != nil {
		return fmt.Errorf("invalid CPU list format: %w", err)
	}

	// Check CPUs are online
	onlineSet := make(map[int]struct{})
	for _, cpu := range a.onlineCPUs {
		onlineSet[cpu] = struct{}{}
	}

	for _, cpu := range cpus {
		if _, isOnline := onlineSet[cpu]; !isOnline {
			return fmt.Errorf("CPU %d is not online", cpu)
		}
	}

	// Check for conflicts with integer reservations
	reservedSet := make(map[int]struct{})
	for _, cpu := range reserved {
		reservedSet[cpu] = struct{}{}
	}

	for _, cpu := range cpus {
		if _, isReserved := reservedSet[cpu]; isReserved {
			return fmt.Errorf("CPU %d is already reserved by an integer-based container", cpu)
		}
	}

	return nil
}

func (a *CPUAllocator) ComputeSharedPool(reserved []int) []int {
	reservedSet := make(map[int]struct{})
	for _, cpu := range reserved {
		reservedSet[cpu] = struct{}{}
	}

	var sharedPool []int
	for _, cpu := range a.onlineCPUs {
		if _, isReserved := reservedSet[cpu]; !isReserved {
			sharedPool = append(sharedPool, cpu)
		}
	}

	return sharedPool
}

func (a *CPUAllocator) getSingleNUMANode(cpus []int) (int, bool) {
	if len(cpus) == 0 {
		return 0, false
	}

	firstNode, exists := a.numa.GetCPUNode(cpus[0])
	if !exists {
		return 0, false
	}

	for _, cpu := range cpus[1:] {
		node, exists := a.numa.GetCPUNode(cpu)
		if !exists || node != firstNode {
			return 0, false
		}
	}

	return firstNode, true
}
