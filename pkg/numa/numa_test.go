package numa

import (
	"sort"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNuma(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "NUMA Suite")
}

var _ = Describe("ParseCPUList", func() {
	It("should parse empty string", func() {
		cpus, err := ParseCPUList("")
		Expect(err).ToNot(HaveOccurred())
		Expect(cpus).To(BeEmpty())
	})

	It("should parse single CPU", func() {
		cpus, err := ParseCPUList("5")
		Expect(err).ToNot(HaveOccurred())
		Expect(cpus).To(Equal([]int{5}))
	})

	It("should parse multiple CPUs", func() {
		cpus, err := ParseCPUList("0,2,4")
		Expect(err).ToNot(HaveOccurred())
		Expect(cpus).To(Equal([]int{0, 2, 4}))
	})

	It("should parse CPU ranges", func() {
		cpus, err := ParseCPUList("0-3")
		Expect(err).ToNot(HaveOccurred())
		Expect(cpus).To(Equal([]int{0, 1, 2, 3}))
	})

	It("should parse mixed format", func() {
		cpus, err := ParseCPUList("0,2-4,8")
		Expect(err).ToNot(HaveOccurred())
		Expect(cpus).To(Equal([]int{0, 2, 3, 4, 8}))
	})

	It("should handle whitespace", func() {
		cpus, err := ParseCPUList(" 0 , 2-4 , 8 ")
		Expect(err).ToNot(HaveOccurred())
		Expect(cpus).To(Equal([]int{0, 2, 3, 4, 8}))
	})

	It("should return error for invalid format", func() {
		_, err := ParseCPUList("0-")
		Expect(err).To(HaveOccurred())
	})

	It("should return error for invalid range", func() {
		_, err := ParseCPUList("4-2")
		Expect(err).To(HaveOccurred())
	})

	It("should return error for non-numeric values", func() {
		_, err := ParseCPUList("0,abc,2")
		Expect(err).To(HaveOccurred())
	})

	It("should return error for empty values in list", func() {
		_, err := ParseCPUList("0,,2")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty CPU value"))
	})

	It("should return error for trailing comma", func() {
		_, err := ParseCPUList("0,2,")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty CPU value"))
	})

	It("should return error for leading comma", func() {
		_, err := ParseCPUList(",0,2")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("empty CPU value"))
	})
})

var _ = Describe("FormatCPUList", func() {
	It("should format empty list", func() {
		result := FormatCPUList([]int{})
		Expect(result).To(Equal(""))
	})

	It("should format single CPU", func() {
		result := FormatCPUList([]int{5})
		Expect(result).To(Equal("5"))
	})

	It("should format non-consecutive CPUs", func() {
		result := FormatCPUList([]int{0, 2, 4})
		Expect(result).To(Equal("0,2,4"))
	})

	It("should format consecutive CPUs as ranges", func() {
		result := FormatCPUList([]int{0, 1, 2, 3})
		Expect(result).To(Equal("0-3"))
	})

	It("should format mixed consecutive and non-consecutive", func() {
		result := FormatCPUList([]int{0, 2, 3, 4, 8})
		Expect(result).To(Equal("0,2-4,8"))
	})

	It("should handle unsorted input", func() {
		result := FormatCPUList([]int{4, 0, 2, 1, 8})
		Expect(result).To(Equal("0-2,4,8"))
	})
})

var _ = Describe("CPUSet", func() {
	Describe("Contains", func() {
		It("should return true for existing CPU", func() {
			cpus := CPUSet{0, 2, 4}
			Expect(cpus.Contains(2)).To(BeTrue())
		})

		It("should return false for non-existing CPU", func() {
			cpus := CPUSet{0, 2, 4}
			Expect(cpus.Contains(1)).To(BeFalse())
		})

		It("should handle empty set", func() {
			cpus := CPUSet{}
			Expect(cpus.Contains(0)).To(BeFalse())
		})

		It("should handle large CPU numbers", func() {
			cpus := CPUSet{0, 128, 256}
			Expect(cpus.Contains(128)).To(BeTrue())
			Expect(cpus.Contains(127)).To(BeFalse())
		})
	})

	Describe("Union", func() {
		It("should combine two CPU sets", func() {
			set1 := CPUSet{0, 2}
			set2 := CPUSet{2, 4}
			result := set1.Union(set2)
			Expect(result).To(Equal(CPUSet{0, 2, 4}))
		})

		It("should handle empty sets", func() {
			set1 := CPUSet{}
			set2 := CPUSet{0, 2}
			result := set1.Union(set2)
			Expect(result).To(Equal(CPUSet{0, 2}))
		})

		It("should handle both empty sets", func() {
			set1 := CPUSet{}
			set2 := CPUSet{}
			result := set1.Union(set2)
			Expect(result).To(BeEmpty())
		})

		It("should handle identical sets", func() {
			set1 := CPUSet{1, 3, 5}
			set2 := CPUSet{1, 3, 5}
			result := set1.Union(set2)
			Expect(result).To(Equal(CPUSet{1, 3, 5}))
		})

		It("should sort result", func() {
			set1 := CPUSet{5, 1}
			set2 := CPUSet{3, 7}
			result := set1.Union(set2)
			Expect(result).To(Equal(CPUSet{1, 3, 5, 7}))
		})
	})

	Describe("Difference", func() {
		It("should subtract one set from another", func() {
			set1 := CPUSet{0, 2, 4, 6}
			set2 := CPUSet{2, 4}
			result := set1.Difference(set2)
			Expect(result).To(Equal(CPUSet{0, 6}))
		})

		It("should handle no overlap", func() {
			set1 := CPUSet{0, 2}
			set2 := CPUSet{4, 6}
			result := set1.Difference(set2)
			Expect(result).To(Equal(CPUSet{0, 2}))
		})

		It("should handle empty first set", func() {
			set1 := CPUSet{}
			set2 := CPUSet{0, 2}
			result := set1.Difference(set2)
			Expect(result).To(BeEmpty())
		})

		It("should handle empty second set", func() {
			set1 := CPUSet{0, 2}
			set2 := CPUSet{}
			result := set1.Difference(set2)
			Expect(result).To(Equal(CPUSet{0, 2}))
		})

		It("should handle complete subtraction", func() {
			set1 := CPUSet{0, 2, 4}
			set2 := CPUSet{0, 2, 4}
			result := set1.Difference(set2)
			Expect(result).To(BeEmpty())
		})
	})

	Describe("String", func() {
		It("should format CPU set as string", func() {
			cpus := CPUSet{0, 2, 3, 4}
			Expect(cpus.String()).To(Equal("0,2-4"))
		})

		It("should handle empty set", func() {
			cpus := CPUSet{}
			Expect(cpus.String()).To(Equal(""))
		})

		It("should handle single CPU", func() {
			cpus := CPUSet{5}
			Expect(cpus.String()).To(Equal("5"))
		})

		It("should handle non-consecutive CPUs", func() {
			cpus := CPUSet{0, 2, 5, 7}
			Expect(cpus.String()).To(Equal("0,2,5,7"))
		})
	})
})

var _ = Describe("NUMA Manager", func() {
	var manager *Manager

	// Mock file system setup for testing
	createMockManager := func(nodes []int, cpuNodeMapping map[int]int, onlineCPUs []int) *Manager {
		m := &Manager{
			onlineCPUs: append([]int(nil), onlineCPUs...),
			nodeMap:    make(map[int][]int),
			cpuNodeMap: make(map[int]int),
			siblingMap: make(map[int][]int),
		}

		// Initialize all provided nodes (even those without CPUs for discovery)
		for _, nodeID := range nodes {
			m.nodeMap[nodeID] = []int{}
		}

		// Set up CPU to node mapping
		for cpu, node := range cpuNodeMapping {
			m.cpuNodeMap[cpu] = node
			m.nodeMap[node] = append(m.nodeMap[node], cpu)
		}

		// Sort CPU lists for each node
		for nodeID := range m.nodeMap {
			sort.Ints(m.nodeMap[nodeID])
		}

		return m
	}

	Describe("GetOnlineCPUs", func() {
		It("should return copy of online CPUs", func() {
			manager = createMockManager([]int{0, 1}, map[int]int{0: 0, 1: 0, 2: 1, 3: 1}, []int{0, 1, 2, 3})
			cpus := manager.GetOnlineCPUs()
			Expect(cpus).To(Equal([]int{0, 1, 2, 3}))

			// Modify returned slice should not affect internal state
			cpus[0] = 99
			Expect(manager.GetOnlineCPUs()).To(Equal([]int{0, 1, 2, 3}))
		})

		It("should handle empty online CPUs", func() {
			manager = createMockManager([]int{0}, map[int]int{}, []int{})
			cpus := manager.GetOnlineCPUs()
			Expect(cpus).To(BeEmpty())
		})

		It("should handle single CPU", func() {
			manager = createMockManager([]int{0}, map[int]int{0: 0}, []int{0})
			cpus := manager.GetOnlineCPUs()
			Expect(cpus).To(Equal([]int{0}))
		})
	})

	Describe("GetNodes", func() {
		It("should return copy of NUMA nodes", func() {
			manager = createMockManager([]int{0, 1, 2}, map[int]int{}, []int{})
			nodes := manager.GetNodes()
			Expect(nodes).To(Equal([]int{0, 1, 2}))

			// Modify returned slice should not affect internal state
			nodes[0] = 99
			Expect(manager.GetNodes()).To(Equal([]int{0, 1, 2}))
		})

		It("should handle single node", func() {
			manager = createMockManager([]int{0}, map[int]int{}, []int{})
			nodes := manager.GetNodes()
			Expect(nodes).To(Equal([]int{0}))
		})
	})

	Describe("GetCPUNode", func() {
		BeforeEach(func() {
			// Node 0: CPUs 0,1,2,3  Node 1: CPUs 4,5,6,7
			manager = createMockManager(
				[]int{0, 1},
				map[int]int{0: 0, 1: 0, 2: 0, 3: 0, 4: 1, 5: 1, 6: 1, 7: 1},
				[]int{0, 1, 2, 3, 4, 5, 6, 7},
			)
		})

		It("should return correct node for existing CPU", func() {
			node, exists := manager.GetCPUNode(2)
			Expect(exists).To(BeTrue())
			Expect(node).To(Equal(0))
		})

		It("should return false for non-existing CPU", func() {
			node, exists := manager.GetCPUNode(99)
			Expect(exists).To(BeFalse())
			Expect(node).To(Equal(0)) // Default value
		})

		It("should handle CPUs on different nodes", func() {
			node0, exists0 := manager.GetCPUNode(1)
			Expect(exists0).To(BeTrue())
			Expect(node0).To(Equal(0))

			node1, exists1 := manager.GetCPUNode(6)
			Expect(exists1).To(BeTrue())
			Expect(node1).To(Equal(1))
		})
	})

	Describe("GetNodeCPUs", func() {
		BeforeEach(func() {
			manager = createMockManager(
				[]int{0, 1},
				map[int]int{0: 0, 1: 0, 2: 0, 4: 1, 5: 1},
				[]int{0, 1, 2, 4, 5},
			)
		})

		It("should return CPUs for existing node", func() {
			cpus, exists := manager.GetNodeCPUs(0)
			Expect(exists).To(BeTrue())
			Expect(cpus).To(Equal([]int{0, 1, 2}))
		})

		It("should return CPUs for second node", func() {
			cpus, exists := manager.GetNodeCPUs(1)
			Expect(exists).To(BeTrue())
			Expect(cpus).To(Equal([]int{4, 5}))
		})

		It("should return false for non-existing node", func() {
			cpus, exists := manager.GetNodeCPUs(99)
			Expect(exists).To(BeFalse())
			Expect(cpus).To(BeNil())
		})

		It("should return copy of CPU list", func() {
			cpus, exists := manager.GetNodeCPUs(0)
			Expect(exists).To(BeTrue())

			// Modify returned slice should not affect internal state
			cpus[0] = 99
			cpus2, _ := manager.GetNodeCPUs(0)
			Expect(cpus2[0]).To(Equal(0))
		})

		It("should handle node with no CPUs", func() {
			manager = createMockManager([]int{0, 1}, map[int]int{0: 0}, []int{0})
			cpus, exists := manager.GetNodeCPUs(1)
			Expect(exists).To(BeFalse())
			Expect(cpus).To(BeNil())
		})
	})

	Describe("GetCPUNodesUnion", func() {
		BeforeEach(func() {
			// Node 0: CPUs 0,1,2  Node 1: CPUs 4,5  Node 2: CPUs 8,9
			manager = createMockManager(
				[]int{0, 1, 2},
				map[int]int{0: 0, 1: 0, 2: 0, 4: 1, 5: 1, 8: 2, 9: 2},
				[]int{0, 1, 2, 4, 5, 8, 9},
			)
		})

		It("should return single node for CPUs on same node", func() {
			nodes := manager.GetCPUNodesUnion([]int{0, 1, 2})
			Expect(nodes).To(Equal([]int{0}))
		})

		It("should return multiple nodes for CPUs on different nodes", func() {
			nodes := manager.GetCPUNodesUnion([]int{0, 4, 8})
			Expect(nodes).To(Equal([]int{0, 1, 2}))
		})

		It("should handle empty CPU list", func() {
			nodes := manager.GetCPUNodesUnion([]int{})
			Expect(nodes).To(BeEmpty())
		})

		It("should handle single CPU", func() {
			nodes := manager.GetCPUNodesUnion([]int{4})
			Expect(nodes).To(Equal([]int{1}))
		})

		It("should handle unknown CPUs", func() {
			nodes := manager.GetCPUNodesUnion([]int{99, 100})
			Expect(nodes).To(BeEmpty())
		})

		It("should handle mix of known and unknown CPUs", func() {
			nodes := manager.GetCPUNodesUnion([]int{0, 99, 4})
			Expect(nodes).To(Equal([]int{0, 1}))
		})

		It("should return sorted nodes", func() {
			nodes := manager.GetCPUNodesUnion([]int{8, 0, 4}) // Node 2, 0, 1
			Expect(nodes).To(Equal([]int{0, 1, 2}))
		})

		It("should deduplicate nodes", func() {
			nodes := manager.GetCPUNodesUnion([]int{0, 1, 2}) // All on node 0
			Expect(nodes).To(Equal([]int{0}))
		})
	})

	Describe("Complex NUMA topologies", func() {
		It("should handle asymmetric NUMA topology", func() {
			// Node 0: CPUs 0,1,2,3,4,5  Node 1: CPUs 6,7  Node 2: CPU 8
			manager = createMockManager(
				[]int{0, 1, 2},
				map[int]int{0: 0, 1: 0, 2: 0, 3: 0, 4: 0, 5: 0, 6: 1, 7: 1, 8: 2},
				[]int{0, 1, 2, 3, 4, 5, 6, 7, 8},
			)

			// Test node 0 has most CPUs
			cpus0, exists0 := manager.GetNodeCPUs(0)
			Expect(exists0).To(BeTrue())
			Expect(cpus0).To(Equal([]int{0, 1, 2, 3, 4, 5}))

			// Test node 1 has fewer CPUs
			cpus1, exists1 := manager.GetNodeCPUs(1)
			Expect(exists1).To(BeTrue())
			Expect(cpus1).To(Equal([]int{6, 7}))

			// Test node 2 has single CPU
			cpus2, exists2 := manager.GetNodeCPUs(2)
			Expect(exists2).To(BeTrue())
			Expect(cpus2).To(Equal([]int{8}))

			// Test union across asymmetric nodes
			nodes := manager.GetCPUNodesUnion([]int{2, 7, 8})
			Expect(nodes).To(Equal([]int{0, 1, 2}))
		})

		It("should handle sparse node numbering", func() {
			// Nodes 0, 3, 7 (non-consecutive)
			manager = createMockManager(
				[]int{0, 3, 7},
				map[int]int{0: 0, 1: 0, 4: 3, 5: 3, 8: 7},
				[]int{0, 1, 4, 5, 8},
			)

			nodes := manager.GetNodes()
			Expect(nodes).To(Equal([]int{0, 3, 7}))

			cpus3, exists3 := manager.GetNodeCPUs(3)
			Expect(exists3).To(BeTrue())
			Expect(cpus3).To(Equal([]int{4, 5}))

			node, exists := manager.GetCPUNode(8)
			Expect(exists).To(BeTrue())
			Expect(node).To(Equal(7))
		})
	})

	Describe("Edge cases", func() {
		It("should handle manager with no nodes", func() {
			manager = &Manager{
				onlineCPUs: []int{},
				nodeMap:    make(map[int][]int),
				cpuNodeMap: make(map[int]int),
				siblingMap: make(map[int][]int),
			}

			Expect(manager.GetNodes()).To(BeEmpty())
			Expect(manager.GetOnlineCPUs()).To(BeEmpty())

			_, exists := manager.GetCPUNode(0)
			Expect(exists).To(BeFalse())

			_, exists = manager.GetNodeCPUs(0)
			Expect(exists).To(BeFalse())

			nodes := manager.GetCPUNodesUnion([]int{0, 1})
			Expect(nodes).To(BeEmpty())
		})

		It("should handle manager with nil maps", func() {
			manager = &Manager{
				onlineCPUs: []int{0},
				nodeMap:    nil,
				cpuNodeMap: nil,
				siblingMap: nil,
			}

			// Should not panic and return sensible defaults
			_, exists := manager.GetCPUNode(0)
			Expect(exists).To(BeFalse())

			_, exists = manager.GetNodeCPUs(0)
			Expect(exists).To(BeFalse())
		})
	})
})
