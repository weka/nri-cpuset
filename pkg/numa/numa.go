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
	nodes       []int
	cpuToNode   map[int]int
	nodeToCPUs  map[int][]int
	onlineCPUs  []int
}

type CPUSet []int

func NewManager() (*Manager, error) {
	m := &Manager{
		cpuToNode:  make(map[int]int),
		nodeToCPUs: make(map[int][]int),
	}

	if err := m.discoverTopology(); err != nil {
		return nil, fmt.Errorf("failed to discover NUMA topology: %w", err)
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
			m.nodes = []int{0}
			return nil
		}
		return err
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "node") && entry.IsDir() {
			nodeStr := strings.TrimPrefix(name, "node")
			if nodeID, err := strconv.Atoi(nodeStr); err == nil {
				m.nodes = append(m.nodes, nodeID)
			}
		}
	}

	if len(m.nodes) == 0 {
		m.nodes = []int{0}
	}

	sort.Ints(m.nodes)
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

		m.cpuToNode[cpu] = nodeID
		m.nodeToCPUs[nodeID] = append(m.nodeToCPUs[nodeID], cpu)
	}

	// Sort CPU lists for each node
	for nodeID := range m.nodeToCPUs {
		sort.Ints(m.nodeToCPUs[nodeID])
	}

	return nil
}

func (m *Manager) getCPUNode(cpu int) (int, error) {
	// Look for the CPU in each NUMA node directory
	for _, nodeID := range m.nodes {
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
	return append([]int(nil), m.nodes...)
}

func (m *Manager) GetCPUNode(cpu int) (int, bool) {
	node, exists := m.cpuToNode[cpu]
	return node, exists
}

func (m *Manager) GetNodeCPUs(nodeID int) ([]int, bool) {
	cpus, exists := m.nodeToCPUs[nodeID]
	if !exists {
		return nil, false
	}
	return append([]int(nil), cpus...), true
}

func (m *Manager) GetCPUNodesUnion(cpus []int) []int {
	nodeSet := make(map[int]struct{})
	
	for _, cpu := range cpus {
		if nodeID, exists := m.cpuToNode[cpu]; exists {
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
			continue
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