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

func (a *CPUAllocator) AllocateContainer(pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	mode := a.determineContainerMode(pod, container)
	
	var result *AllocationResult
	var err error

	switch mode {
	case "annotated":
		result, err = a.handleAnnotatedContainer(pod)
	case "integer":
		result, err = a.handleIntegerContainer(container, nil) // TODO: pass reserved CPUs
	case "shared":
		result, err = a.handleSharedContainer(nil) // TODO: pass reserved CPUs
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

	// Set memory nodes for annotated containers
	if mode == "annotated" && len(result.MemNodes) > 0 {
		adjustment.Linux.Resources.Memory = &api.LinuxMemory{}
		// Note: NRI doesn't directly support cpuset.mems, but we can set it via annotations
		// or through the cgroup interface if available
	}

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

func (a *CPUAllocator) handleAnnotatedContainer(pod *api.PodSandbox) (*AllocationResult, error) {
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

	// Determine NUMA nodes for memory placement
	memNodes := a.numa.GetCPUNodesUnion(cpus)

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

	return &AllocationResult{
		CPUs: cpus,
		Mode: "integer",
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
			return fmt.Errorf("CPU %d is already reserved by an integer container", cpu)
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