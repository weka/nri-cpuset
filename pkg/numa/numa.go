package numa

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	sysDevicesSystemNode = "/sys/devices/system/node"
	sysCPUPath           = "/sys/devices/system/cpu"
)

type Manager struct {
	onlineCPUs []int
	nodeMap    map[int][]int // nodeID -> CPUs
	cpuNodeMap map[int]int   // cpuID -> nodeID
	siblingMap map[int][]int // cpuID -> sibling CPUs (including self)
}

type CPUSet []int

func NewManager() (*Manager, error) {
	m := &Manager{
		nodeMap:    make(map[int][]int),
		cpuNodeMap: make(map[int]int),
		siblingMap: make(map[int][]int),
	}

	if err := m.discoverTopology(); err != nil {
		return nil, err
	}

	if err := m.discoverSiblings(); err != nil {
		return nil, err
	}

	return m, nil
}

func (m *Manager) discoverTopology() error {
	// Discover NUMA nodes
	if err := m.discoverNodes(); err != nil {
		return fmt.Errorf("failed to discover NUMA nodes: %w", err)
	}

	// Discover online CPUs
	if err := m.discoverOnlineCPUs(); err != nil {
		return fmt.Errorf("failed to discover online CPUs: %w", err)
	}

	// Map CPUs to NUMA nodes
	if err := m.mapCPUsToNodes(); err != nil {
		return fmt.Errorf("failed to map CPUs to NUMA nodes: %w", err)
	}

	return nil
}

func (m *Manager) discoverNodes() error {
	entries, err := os.ReadDir(sysDevicesSystemNode)
	if err != nil {
		// If no NUMA nodes directory, assume single node system
		if os.IsNotExist(err) {
			// For single node systems, we'll populate this during mapCPUsToNodes
			return nil
		}
		return err
	}

	var nodes []int
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "node") && entry.IsDir() {
			nodeStr := strings.TrimPrefix(name, "node")
			if nodeID, err := strconv.Atoi(nodeStr); err == nil {
				nodes = append(nodes, nodeID)
			}
		}
	}

	if len(nodes) == 0 {
		// Will be handled in mapCPUsToNodes
		return nil
	}

	sort.Ints(nodes)
	return nil
}

func (m *Manager) discoverOnlineCPUs() error {
	onlineFile := filepath.Join(sysCPUPath, "online")
	data, err := os.ReadFile(onlineFile)
	if err != nil {
		return err
	}

	cpuList := strings.TrimSpace(string(data))
	cpus, err := ParseCPUList(cpuList)
	if err != nil {
		return fmt.Errorf("failed to parse online CPU list '%s': %w", cpuList, err)
	}

	m.onlineCPUs = cpus
	sort.Ints(m.onlineCPUs)
	return nil
}

func (m *Manager) mapCPUsToNodes() error {
	for _, cpu := range m.onlineCPUs {
		nodeID, err := m.getCPUNode(cpu)
		if err != nil {
			// If we can't determine the node, assume node 0
			nodeID = 0
		}

		m.cpuNodeMap[cpu] = nodeID
		m.nodeMap[nodeID] = append(m.nodeMap[nodeID], cpu)
	}

	// Sort CPU lists for each node
	for nodeID := range m.nodeMap {
		sort.Ints(m.nodeMap[nodeID])
	}

	return nil
}

func (m *Manager) getCPUNode(cpu int) (int, error) {
	// Look for the CPU in each NUMA node directory
	for nodeID := range m.nodeMap {
		cpuListFile := filepath.Join(sysDevicesSystemNode, fmt.Sprintf("node%d", nodeID), "cpulist")
		data, err := os.ReadFile(cpuListFile)
		if err != nil {
			continue
		}

		cpuList := strings.TrimSpace(string(data))
		cpus, err := ParseCPUList(cpuList)
		if err != nil {
			continue
		}

		for _, nodeCPU := range cpus {
			if nodeCPU == cpu {
				return nodeID, nil
			}
		}
	}

	return 0, fmt.Errorf("CPU %d not found in any NUMA node", cpu)
}

func (m *Manager) GetOnlineCPUs() []int {
	return append([]int(nil), m.onlineCPUs...)
}

func (m *Manager) GetNodes() []int {
	var nodes []int
	for nodeID := range m.nodeMap {
		nodes = append(nodes, nodeID)
	}
	sort.Ints(nodes)
	return nodes
}

func (m *Manager) GetCPUNode(cpu int) (int, bool) {
	node, exists := m.cpuNodeMap[cpu]
	return node, exists
}

func (m *Manager) GetNodeCPUs(nodeID int) ([]int, bool) {
	cpus, exists := m.nodeMap[nodeID]
	if !exists || len(cpus) == 0 {
		return nil, false
	}
	return append([]int(nil), cpus...), true
}

func (m *Manager) GetCPUNodesUnion(cpus []int) []int {
	nodeSet := make(map[int]struct{})

	for _, cpu := range cpus {
		if nodeID, exists := m.cpuNodeMap[cpu]; exists {
			nodeSet[nodeID] = struct{}{}
		}
	}

	var nodes []int
	for nodeID := range nodeSet {
		nodes = append(nodes, nodeID)
	}

	sort.Ints(nodes)
	return nodes
}

func ParseCPUList(cpuList string) ([]int, error) {
	if cpuList == "" {
		return nil, nil
	}

	var cpus []int
	parts := strings.Split(cpuList, ",")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty CPU value in list: %s", cpuList)
		}

		if strings.Contains(part, "-") {
			// Range format: "0-3"
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("invalid CPU range format: %s", part)
			}

			start, err := strconv.Atoi(rangeParts[0])
			if err != nil {
				return nil, fmt.Errorf("invalid start CPU in range %s: %w", part, err)
			}

			end, err := strconv.Atoi(rangeParts[1])
			if err != nil {
				return nil, fmt.Errorf("invalid end CPU in range %s: %w", part, err)
			}

			if start > end {
				return nil, fmt.Errorf("invalid CPU range %s: start > end", part)
			}

			for cpu := start; cpu <= end; cpu++ {
				cpus = append(cpus, cpu)
			}
		} else {
			// Single CPU
			cpu, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid CPU number %s: %w", part, err)
			}
			cpus = append(cpus, cpu)
		}
	}

	return cpus, nil
}

func FormatCPUList(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}

	sort.Ints(cpus)

	var parts []string
	start := cpus[0]
	prev := cpus[0]

	for i := 1; i < len(cpus); i++ {
		current := cpus[i]
		if current == prev+1 {
			prev = current
			continue
		}

		// End of sequence
		if start == prev {
			parts = append(parts, fmt.Sprintf("%d", start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}

		start = current
		prev = current
	}

	// Handle last sequence
	if start == prev {
		parts = append(parts, fmt.Sprintf("%d", start))
	} else {
		parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
	}

	return strings.Join(parts, ",")
}

func (c CPUSet) Contains(cpu int) bool {
	for _, existing := range c {
		if existing == cpu {
			return true
		}
	}
	return false
}

func (c CPUSet) Union(other CPUSet) CPUSet {
	seen := make(map[int]struct{})
	result := make(CPUSet, 0, len(c)+len(other))

	for _, cpu := range c {
		if _, exists := seen[cpu]; !exists {
			result = append(result, cpu)
			seen[cpu] = struct{}{}
		}
	}

	for _, cpu := range other {
		if _, exists := seen[cpu]; !exists {
			result = append(result, cpu)
			seen[cpu] = struct{}{}
		}
	}

	sort.Ints(result)
	return result
}

func (c CPUSet) Difference(other CPUSet) CPUSet {
	otherSet := make(map[int]struct{})
	for _, cpu := range other {
		otherSet[cpu] = struct{}{}
	}

	var result CPUSet
	for _, cpu := range c {
		if _, exists := otherSet[cpu]; !exists {
			result = append(result, cpu)
		}
	}

	return result
}

func (c CPUSet) String() string {
	return FormatCPUList(c)
}

// discoverSiblings discovers CPU sibling relationships (hyperthreading)
func (m *Manager) discoverSiblings() error {
	for _, cpu := range m.onlineCPUs {
		siblings, err := m.getCPUSiblings(cpu)
		if err != nil {
			// If we can't discover siblings, treat CPU as its own sibling
			siblings = []int{cpu}
		}
		m.siblingMap[cpu] = siblings
	}
	return nil
}

// getCPUSiblings reads the sibling CPU list for a given CPU
func (m *Manager) getCPUSiblings(cpu int) ([]int, error) {
	threadSiblingsPath := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/topology/thread_siblings_list", cpu)

	data, err := os.ReadFile(threadSiblingsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read thread siblings for CPU %d: %w", cpu, err)
	}

	siblingsStr := strings.TrimSpace(string(data))
	if siblingsStr == "" {
		return []int{cpu}, nil
	}

	siblings, err := ParseCPUList(siblingsStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse sibling list '%s' for CPU %d: %w", siblingsStr, cpu, err)
	}

	// Filter siblings to only include online CPUs
	var onlineSiblings []int
	onlineSet := make(map[int]struct{})
	for _, onlineCPU := range m.onlineCPUs {
		onlineSet[onlineCPU] = struct{}{}
	}

	for _, sibling := range siblings {
		if _, isOnline := onlineSet[sibling]; isOnline {
			onlineSiblings = append(onlineSiblings, sibling)
		}
	}

	if len(onlineSiblings) == 0 {
		return []int{cpu}, nil
	}

	sort.Ints(onlineSiblings)
	return onlineSiblings, nil
}

// GetCPUSiblings returns the sibling CPUs for a given CPU
func (m *Manager) GetCPUSiblings(cpu int) []int {
	if siblings, exists := m.siblingMap[cpu]; exists {
		result := make([]int, len(siblings))
		copy(result, siblings)
		return result
	}
	return []int{cpu}
}

// GetPhysicalCoreGroups returns groups of sibling CPUs (representing physical cores)
func (m *Manager) GetPhysicalCoreGroups() [][]int {
	seen := make(map[int]struct{})
	var coreGroups [][]int

	for _, cpu := range m.onlineCPUs {
		if _, alreadySeen := seen[cpu]; alreadySeen {
			continue
		}

		siblings := m.GetCPUSiblings(cpu)
		coreGroups = append(coreGroups, siblings)

		// Mark all siblings as seen
		for _, sibling := range siblings {
			seen[sibling] = struct{}{}
		}
	}

	// Sort core groups by their first CPU for deterministic behavior
	sort.Slice(coreGroups, func(i, j int) bool {
		return coreGroups[i][0] < coreGroups[j][0]
	})

	return coreGroups
}

// IsHyperthreadingEnabled returns true if hyperthreading is detected
func (m *Manager) IsHyperthreadingEnabled() bool {
	coreGroups := m.GetPhysicalCoreGroups()
	for _, group := range coreGroups {
		if len(group) > 1 {
			return true
		}
	}
	return false
}

// GetCoreUtilization returns information about how many CPUs from each physical core are reserved
func (m *Manager) GetCoreUtilization(reservedCPUs []int) map[int]int {
	reservedSet := make(map[int]struct{})
	for _, cpu := range reservedCPUs {
		reservedSet[cpu] = struct{}{}
	}

	coreUtilization := make(map[int]int) // core group index -> reserved count
	coreGroups := m.GetPhysicalCoreGroups()

	for groupIdx, group := range coreGroups {
		reservedCount := 0
		for _, cpu := range group {
			if _, isReserved := reservedSet[cpu]; isReserved {
				reservedCount++
			}
		}
		coreUtilization[groupIdx] = reservedCount
	}

	return coreUtilization
}
