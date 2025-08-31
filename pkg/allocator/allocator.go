package allocator

import (
	"fmt"
	"sort"

	"github.com/containerd/nri/pkg/api"
	containerutil "github.com/weka/nri-cpuset/pkg/container"
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

// SiblingAllocationStrategy represents different strategies for sibling allocation
type SiblingAllocationStrategy struct {
	PreferSiblings     bool
	AllowFragmentation bool
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

// AllocateExclusiveCPUsWithSiblings allocates CPUs with sibling preference
func (a *CPUAllocator) AllocateExclusiveCPUsWithSiblings(count int, reserved []int) ([]int, error) {
	if count <= 0 {
		return nil, fmt.Errorf("invalid CPU count: %d", count)
	}

	reservedSet := make(map[int]struct{})
	for _, cpu := range reserved {
		reservedSet[cpu] = struct{}{}
	}

	// Get available CPUs
	var available []int
	for _, cpu := range a.onlineCPUs {
		if _, isReserved := reservedSet[cpu]; !isReserved {
			available = append(available, cpu)
		}
	}

	// ENHANCED DEBUG LOG FOR CPU ALLOCATION
	fmt.Printf("DEBUG: AllocateExclusiveCPUs need=%d, reserved=%v (%d CPUs), available=%v (%d CPUs), online=%v (%d CPUs)\n", 
		count, reserved, len(reserved), available, len(available), a.onlineCPUs, len(a.onlineCPUs))

	if len(available) < count {
		// ENHANCED ERROR WITH DEBUG INFO
		fmt.Printf("ERROR: CPU allocation failed - need %d CPUs, have %d available. Reserved CPUs: %v. Online CPUs: %v\n", 
			count, len(available), reserved, a.onlineCPUs)
		return nil, fmt.Errorf("insufficient free CPUs: need %d, have %d (reserved: %v)", count, len(available), reserved)
	}

	// Try sibling-aware allocation first
	allocated := a.allocateWithSiblingPreference(available, count, reservedSet)
	if len(allocated) == count {
		return allocated, nil
	}

	// Fallback to simple allocation if sibling strategy doesn't work
	sort.Ints(available)
	return available[:count], nil
}

// allocateWithSiblingPreference implements the improved sibling allocation strategy
func (a *CPUAllocator) allocateWithSiblingPreference(available []int, count int, reservedSet map[int]struct{}) []int {
	htEnabled := a.numa.IsHyperthreadingEnabled()
	fmt.Printf("DEBUG: allocateWithSiblingPreference count=%d, hyperthreading=%t\n", count, htEnabled)
	
	if !htEnabled {
		// No hyperthreading, avoid CPU 0 if possible and return first available CPUs
		return a.selectAvoidingCPUZero(available, count)
	}

	coreGroups := a.numa.GetPhysicalCoreGroups()
	fmt.Printf("DEBUG: Found %d core groups: %v\n", len(coreGroups), coreGroups)
	var allocated []int
	remaining := count

	// Build availability map for quick lookup
	availableSet := make(map[int]struct{})
	for _, cpu := range available {
		availableSet[cpu] = struct{}{}
	}

	// IMPROVED STRATEGY: 
	// 1. Complete partial cores that are already reserved (efficient use of existing fragmentation)
	// 2. Allocate full cores (maximizing full core availability for future containers)
	// 3. Avoid CPU 0 unless absolutely necessary
	// 4. Use partial cores only as last resort

	// Phase 1: Complete partially allocated cores (if any exist in reserved set)
	// This is efficient - if we already have fragmention, complete those cores first
	if len(reservedSet) > 0 {
		coreUtilization := a.numa.GetCoreUtilization(getKeysFromSet(reservedSet))
		for groupIdx, group := range coreGroups {
			if remaining <= 0 {
				break
			}

			reservedInCore := coreUtilization[groupIdx]
			if reservedInCore > 0 && reservedInCore < len(group) {
				// This core is partially used, complete it to avoid further fragmentation
				for _, cpu := range group {
					if remaining <= 0 {
						break
					}
					if _, isAvailable := availableSet[cpu]; isAvailable {
						allocated = append(allocated, cpu)
						delete(availableSet, cpu)
						remaining--
					}
				}
			}
		}
	}

	// Phase 2: Allocate full cores for remaining CPUs (prioritize full cores)
	for remaining >= 2 {
		bestGroup := -1
		bestScore := -1

		for groupIdx, group := range coreGroups {
			if len(group) < 2 {
				continue // Not a hyperthreaded core
			}

			// Check if entire core is available
			availableInGroup := 0
			containsCPUZero := false
			for _, cpu := range group {
				if _, isAvailable := availableSet[cpu]; isAvailable {
					availableInGroup++
					if cpu == 0 {
						containsCPUZero = true
					}
				}
			}

			if availableInGroup == len(group) {
				// Full core available - score it
				score := 100 // Base score for full core
				
				// Prefer cores that don't contain CPU 0
				if containsCPUZero {
					score -= 50 // Significant penalty for CPU 0
				}
				
				// Prefer smaller core IDs (better locality)
				score -= groupIdx // Small penalty for higher core indices
				
				if score > bestScore {
					bestScore = score
					bestGroup = groupIdx
				}
			}
		}

		if bestGroup >= 0 {
			group := coreGroups[bestGroup]
			coresNeeded := min(remaining, len(group))
			for i := 0; i < coresNeeded; i++ {
				cpu := group[i]
				if _, isAvailable := availableSet[cpu]; isAvailable {
					allocated = append(allocated, cpu)
					delete(availableSet, cpu)
					remaining--
				}
			}
		} else {
			break // No full cores available, move to partial allocation
		}
	}

	// Phase 3: Allocate remaining CPUs with intelligent partial core strategy
	if remaining > 0 {
		fmt.Printf("DEBUG: Phase 3 - allocating %d remaining CPUs\n", remaining)
		
		// Strategy for remaining CPUs (typically 1 CPU for odd requests):
		// 1. First: Use partial cores (where sibling is already reserved)
		// 2. Second: Break up a new core, preferring core 0↔32 for fragmentation
		// 3. Last resort: Simple CPU selection with CPU 0 avoidance
		
		remainingAllocated := 0
		
		// Step 1: Look for partial cores (one sibling already reserved)
		for groupIdx, group := range coreGroups {
			if remainingAllocated >= remaining {
				break
			}
			if len(group) < 2 {
				continue // Skip non-hyperthreaded cores
			}
			
			// Check if this core has one sibling reserved and one available
			availableInGroup := 0
			reservedInGroup := 0
			var availableCPU int
			
			for _, cpu := range group {
				if _, isAvailable := availableSet[cpu]; isAvailable {
					availableInGroup++
					availableCPU = cpu
				} else {
					reservedInGroup++
				}
			}
			
			// Perfect case: partial core (1 reserved, 1 available)
			if availableInGroup == 1 && reservedInGroup == 1 {
				fmt.Printf("DEBUG: Using partial core %d (group %d) - CPU %d available, sibling reserved\n", 
					groupIdx, groupIdx, availableCPU)
				allocated = append(allocated, availableCPU)
				delete(availableSet, availableCPU)
				remainingAllocated++
			}
		}
		
		// Step 2: If no partial cores, break up a new core (prefer core 0↔32)
		if remainingAllocated < remaining {
			fmt.Printf("DEBUG: No partial cores available, breaking up a new core\n")
			
			// Find the best core to break up - prefer core 0↔32, then others
			bestCoreToBreak := -1
			bestCPU := -1
			
			for groupIdx, group := range coreGroups {
				if len(group) < 2 {
					continue
				}
				
				// Check if entire core is available
				availableInGroup := 0
				containsCPUZero := false
				var firstAvailableCPU int
				
				for _, cpu := range group {
					if _, isAvailable := availableSet[cpu]; isAvailable {
						if availableInGroup == 0 {
							firstAvailableCPU = cpu
						}
						availableInGroup++
						if cpu == 0 {
							containsCPUZero = true
						}
					}
				}
				
				if availableInGroup == len(group) {
					// This core is completely available
					if containsCPUZero {
						// Prefer breaking up core 0↔32, but use sibling (32) first
						bestCoreToBreak = groupIdx
						// Use sibling of 0 first (should be 32)
						for _, cpu := range group {
							if cpu != 0 {
								bestCPU = cpu
								break
							}
						}
						break // Core 0 found, use it immediately
					} else if bestCoreToBreak == -1 {
						// Use first available core if no better option
						bestCoreToBreak = groupIdx  
						bestCPU = firstAvailableCPU
					}
				}
			}
			
			// Allocate from the chosen core to break up
			if bestCoreToBreak >= 0 && remainingAllocated < remaining {
				fmt.Printf("DEBUG: Breaking up core %d, using CPU %d (leaves sibling for future reuse)\n", 
					bestCoreToBreak, bestCPU)
				allocated = append(allocated, bestCPU)
				delete(availableSet, bestCPU)
				remainingAllocated++
			}
		}
		
		// Step 3: Last resort - simple allocation with CPU 0 avoidance
		if remainingAllocated < remaining {
			fmt.Printf("DEBUG: Last resort allocation for remaining %d CPUs\n", remaining - remainingAllocated)
			
			var remainingCPUs []int
			for cpu := range availableSet {
				remainingCPUs = append(remainingCPUs, cpu)
			}
			
			// Sort with preference: non-zero CPUs first
			sort.Slice(remainingCPUs, func(i, j int) bool {
				cpuA, cpuB := remainingCPUs[i], remainingCPUs[j]
				
				// Strongly prefer non-zero CPUs
				if (cpuA == 0) != (cpuB == 0) {
					return cpuB == 0 // cpuA is preferred if cpuB is 0
				}
				
				// Among non-zero CPUs, prefer lower CPU numbers
				return cpuA < cpuB
			})
			
			// Allocate remaining CPUs
			needed := remaining - remainingAllocated
			for i := 0; i < min(needed, len(remainingCPUs)); i++ {
				allocated = append(allocated, remainingCPUs[i])
			}
		}
	}

	sort.Ints(allocated)
	return allocated
}

// selectAvoidingCPUZero selects CPUs while avoiding CPU 0 if possible
func (a *CPUAllocator) selectAvoidingCPUZero(available []int, count int) []int {
	if len(available) < count {
		return nil
	}

	// Sort CPUs with preference: non-zero CPUs first
	sort.Slice(available, func(i, j int) bool {
		cpuA, cpuB := available[i], available[j]
		
		// Strongly prefer non-zero CPUs
		if (cpuA == 0) != (cpuB == 0) {
			return cpuB == 0 // cpuA is preferred if cpuB is 0
		}
		
		// Among CPUs of same type (zero/non-zero), prefer lower numbers
		return cpuA < cpuB
	})
	
	return available[:count]
}

// getKeysFromSet converts a set to a slice of keys
func getKeysFromSet(set map[int]struct{}) []int {
	var keys []int
	for key := range set {
		keys = append(keys, key)
	}
	return keys
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (a *CPUAllocator) AllocateExclusiveCPUs(count int, reserved []int) ([]int, error) {
	// Use sibling-aware allocation by default
	return a.AllocateExclusiveCPUsWithSiblings(count, reserved)
}

// CanReallocateInteger checks if an integer container can be reallocated to avoid conflicts
func (a *CPUAllocator) CanReallocateInteger(currentCPUs []int, conflictCPUs []int, allReserved []int) ([]int, bool) {
	// Remove current container's CPUs from reserved set
	reservedSet := make(map[int]struct{})
	for _, cpu := range allReserved {
		reservedSet[cpu] = struct{}{}
	}
	for _, cpu := range currentCPUs {
		delete(reservedSet, cpu)
	}

	// Add conflict CPUs to reserved set (they will be taken by annotated pod)
	for _, cpu := range conflictCPUs {
		reservedSet[cpu] = struct{}{}
	}

	reserved := getKeysFromSet(reservedSet)
	newCPUs, err := a.AllocateExclusiveCPUsWithSiblings(len(currentCPUs), reserved)
	if err != nil {
		return nil, false
	}

	return newCPUs, true
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

	// Set memory nodes only for annotated containers (fixed CPU allocation)
	// Per PRD 3.3: integer pods keep flexible NUMA memory to support live reallocation
	if mode == "annotated" && len(result.MemNodes) > 0 {
		adjustment.Linux.Resources.Cpu.Mems = numa.FormatCPUList(result.MemNodes)
	}
	// Integer and shared containers inherit system default memory placement (no restriction)

	return adjustment, nil, nil
}

// AllocateContainerCPUs provides unified allocation logic for both normal and synchronization paths
func (a *CPUAllocator) AllocateContainerCPUs(pod *api.PodSandbox, container *api.Container, reserved []int) ([]int, string, error) {
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
		return nil, "", fmt.Errorf("unknown container mode: %s", mode)
	}

	if err != nil {
		return nil, "", err
	}

	return result.CPUs, result.Mode, nil
}

// AllocateContainerCPUsWithForbidden provides allocation logic that respects forbidden CPUs
func (a *CPUAllocator) AllocateContainerCPUsWithForbidden(pod *api.PodSandbox, container *api.Container, reserved []int, forbidden []int) ([]int, string, error) {
	mode := a.determineContainerMode(pod, container)

	var result *AllocationResult
	var err error

	switch mode {
	case "annotated":
		result, err = a.handleAnnotatedContainer(pod, reserved)
	case "integer":
		result, err = a.handleIntegerContainerWithForbidden(container, reserved, forbidden)
	case "shared":
		// For shared containers, forbidden CPUs are treated like reserved CPUs
		combinedReserved := append(reserved, forbidden...)
		result, err = a.handleSharedContainer(combinedReserved)
	default:
		return nil, "", fmt.Errorf("unknown container mode: %s", mode)
	}

	if err != nil {
		return nil, "", err
	}

	return result.CPUs, result.Mode, nil
}

func (a *CPUAllocator) determineContainerMode(pod *api.PodSandbox, container *api.Container) string {
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

func (a *CPUAllocator) handleAnnotatedContainer(pod *api.PodSandbox, reserved []int) (*AllocationResult, error) {
	if pod.Annotations == nil {
		return nil, fmt.Errorf("missing annotations for annotated container")
	}

	cpuList, exists := pod.Annotations[WekaAnnotation]
	if !exists {
		return nil, fmt.Errorf("missing %s annotation", WekaAnnotation)
	}

	cpus, err := a.numa.ParseAndValidateCPUList(cpuList)
	if err != nil {
		return nil, fmt.Errorf("invalid CPU list in annotation '%s': %w", cpuList, err)
	}

	// Check for conflicts with reserved CPUs
	// Note: For annotated containers, we need to distinguish between:
	// - CPUs reserved by INTEGER containers (conflict - not allowed)
	// - CPUs reserved by ANNOTATED containers (sharing - allowed)
	// Since we don't have mode information in the reserved list, we'll allow
	// the state manager to handle this properly during synchronization

	// For now, we'll allow annotated containers to request any online CPU
	// The state manager will handle proper conflict resolution during sync

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

// HandleAnnotatedContainerWithIntegerConflictCheck handles annotated containers with proper conflict checking
// It only rejects CPUs that are reserved by integer containers, allowing sharing between annotated containers
func (a *CPUAllocator) HandleAnnotatedContainerWithIntegerConflictCheck(pod *api.PodSandbox, integerReserved []int) (*AllocationResult, error) {
	if pod.Annotations == nil {
		return nil, fmt.Errorf("missing annotations for annotated container")
	}

	cpuList, exists := pod.Annotations[WekaAnnotation]
	if !exists {
		return nil, fmt.Errorf("missing %s annotation", WekaAnnotation)
	}

	cpus, err := a.numa.ParseAndValidateCPUList(cpuList)
	if err != nil {
		return nil, fmt.Errorf("invalid CPU list in annotation '%s': %w", cpuList, err)
	}

	// Check for conflicts with integer-reserved CPUs only
	integerReservedSet := make(map[int]struct{})
	for _, cpu := range integerReserved {
		integerReservedSet[cpu] = struct{}{}
	}

	for _, cpu := range cpus {
		if _, isIntegerReserved := integerReservedSet[cpu]; isIntegerReserved {
			return nil, fmt.Errorf("CPU %d is reserved by an integer container", cpu)
		}
	}

	// Determine NUMA nodes for memory placement
	memNodes := a.numa.GetCPUNodesUnion(cpus)
	if singleNode, isSingleNode := a.getSingleNUMANode(cpus); isSingleNode {
		memNodes = []int{singleNode}
	}

	fmt.Printf("DEBUG: HandleAnnotatedContainerWithIntegerConflictCheck returning CPUs: %v for annotation: %s\n", cpus, cpuList)
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

	// Use sibling-aware allocation for integer containers
	cpus, err := a.AllocateExclusiveCPUsWithSiblings(cpuCores, reserved)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate exclusive CPUs: %w", err)
	}

	// Per PRD 3.3: Integer pods keep flexible NUMA memory (no binding) to support live reallocation
	// This prevents memory placement conflicts during live reassignment

	return &AllocationResult{
		CPUs:     cpus,
		MemNodes: nil, // No NUMA memory binding for integer pods
		Mode:     "integer",
	}, nil
}

// handleIntegerContainerWithForbidden handles integer containers while respecting forbidden CPUs
func (a *CPUAllocator) handleIntegerContainerWithForbidden(container *api.Container, reserved []int, forbidden []int) (*AllocationResult, error) {
	if container.Linux == nil || container.Linux.Resources == nil || container.Linux.Resources.Cpu == nil {
		return nil, fmt.Errorf("missing CPU resources for integer container")
	}

	cpu := container.Linux.Resources.Cpu
	if cpu.Quota == nil || cpu.Period == nil || cpu.Quota.GetValue() <= 0 || cpu.Period.GetValue() <= 0 {
		return nil, fmt.Errorf("invalid CPU quota/period for integer container")
	}

	cpuCores := int(cpu.Quota.GetValue() / int64(cpu.Period.GetValue()))

	// Combine reserved and forbidden CPUs to exclude from allocation
	combinedReserved := append(reserved, forbidden...)

	// Use sibling-aware allocation for integer containers
	cpus, err := a.AllocateExclusiveCPUsWithSiblings(cpuCores, combinedReserved)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate exclusive CPUs (avoiding forbidden cores): %w", err)
	}

	return &AllocationResult{
		CPUs:     cpus,
		MemNodes: nil, // No NUMA memory binding for integer pods
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
		if node, exists := a.numa.GetCPUNode(cpu); !exists || node != firstNode {
			return 0, false
		}
	}

	return firstNode, true
}
