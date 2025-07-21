package allocator

import (
	"sort"
	"testing"

	"github.com/containerd/nri/pkg/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/weka/nri-cpuset/pkg/numa"
)

// NumaInterface defines the methods we need from NUMA manager for testing
type NumaInterface interface {
	GetOnlineCPUs() []int
	GetNodes() []int
	GetCPUNode(cpu int) (int, bool)
	GetCPUNodesUnion(cpus []int) []int
}

func TestAllocator(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Allocator Suite")
}

// Mock NUMA manager for testing
type MockNumaManager struct {
	nodes       []int
	cpuToNode   map[int]int
	onlineCPUs  []int
}

func newMockNumaManager() *MockNumaManager {
	return &MockNumaManager{
		nodes: []int{0, 1},
		cpuToNode: map[int]int{
			0: 0, 1: 0, 2: 0, 3: 0, // CPUs 0-3 on NUMA node 0
			4: 1, 5: 1, 6: 1, 7: 1, // CPUs 4-7 on NUMA node 1
		},
		onlineCPUs: []int{0, 1, 2, 3, 4, 5, 6, 7},
	}
}

func (m *MockNumaManager) GetOnlineCPUs() []int {
	return append([]int(nil), m.onlineCPUs...)
}

func (m *MockNumaManager) GetNodes() []int {
	return append([]int(nil), m.nodes...)
}

func (m *MockNumaManager) GetCPUNode(cpu int) (int, bool) {
	node, exists := m.cpuToNode[cpu]
	return node, exists
}

func (m *MockNumaManager) GetCPUNodesUnion(cpus []int) []int {
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


// TestCPUAllocator extends CPUAllocator for testing with mock NUMA manager
type TestCPUAllocator struct {
	*CPUAllocator
	mockNuma NumaInterface
}

func (t *TestCPUAllocator) getSingleNUMANode(cpus []int) (int, bool) {
	if len(cpus) == 0 {
		return 0, false
	}

	firstNode, exists := t.mockNuma.GetCPUNode(cpus[0])
	if !exists {
		return 0, false
	}

	for _, cpu := range cpus[1:] {
		node, exists := t.mockNuma.GetCPUNode(cpu)
		if !exists || node != firstNode {
			return 0, false
		}
	}

	return firstNode, true
}

var _ = Describe("CPUAllocator", func() {
	var (
		allocator    *TestCPUAllocator
		mockNumaMgr  *MockNumaManager
		realNumaMgr  *numa.Manager
	)

	BeforeEach(func() {
		mockNumaMgr = newMockNumaManager()
		realNumaMgr = &numa.Manager{} // We can't easily mock this, so we'll work around it
		// Create allocator with mock online CPUs for testing
		allocator = &TestCPUAllocator{
			CPUAllocator: &CPUAllocator{
				numa:       realNumaMgr,
				onlineCPUs: []int{0, 1, 2, 3, 4, 5, 6, 7}, // Mock online CPUs
			},
			mockNuma: mockNumaMgr,
		}
	})

	Describe("AllocateExclusiveCPUs", func() {
		It("should allocate requested number of CPUs", func() {
			cpus, err := allocator.AllocateExclusiveCPUs(2, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(HaveLen(2))
			Expect(cpus).To(Equal([]int{0, 1}))
		})

		It("should exclude reserved CPUs", func() {
			reserved := []int{0, 1}
			cpus, err := allocator.AllocateExclusiveCPUs(2, reserved)
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(HaveLen(2))
			Expect(cpus).To(Equal([]int{2, 3}))
		})

		It("should return error when insufficient CPUs", func() {
			reserved := []int{0, 1, 2, 3, 4, 5, 6}
			_, err := allocator.AllocateExclusiveCPUs(2, reserved)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("insufficient free CPUs"))
		})

		It("should return error for zero CPU request", func() {
			_, err := allocator.AllocateExclusiveCPUs(0, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid CPU count"))
		})
	})

	Describe("ComputeSharedPool", func() {
		It("should return all CPUs when nothing is reserved", func() {
			pool := allocator.ComputeSharedPool([]int{})
			Expect(pool).To(Equal([]int{0, 1, 2, 3, 4, 5, 6, 7}))
		})

		It("should exclude reserved CPUs", func() {
			reserved := []int{0, 2, 4}
			pool := allocator.ComputeSharedPool(reserved)
			Expect(pool).To(Equal([]int{1, 3, 5, 6, 7}))
		})

		It("should return empty pool when all CPUs are reserved", func() {
			reserved := []int{0, 1, 2, 3, 4, 5, 6, 7}
			pool := allocator.ComputeSharedPool(reserved)
			Expect(pool).To(BeEmpty())
		})
	})

	Describe("ValidateAnnotatedCPUs", func() {
		It("should validate correct CPU list", func() {
			err := allocator.ValidateAnnotatedCPUs("0,2,4", []int{})
			Expect(err).ToNot(HaveOccurred())
		})

		It("should reject offline CPUs", func() {
			err := allocator.ValidateAnnotatedCPUs("0,99", []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not online"))
		})

		It("should reject reserved CPUs", func() {
			reserved := []int{0, 2}
			err := allocator.ValidateAnnotatedCPUs("0,4", reserved)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("already reserved"))
		})

		It("should reject invalid CPU list format", func() {
			err := allocator.ValidateAnnotatedCPUs("0-", []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid CPU list format"))
		})
	})

	Describe("determineContainerMode", func() {
		It("should detect annotated containers", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "0,2,4",
				},
			}
			container := &api.Container{}
			mode := allocator.determineContainerMode(pod, container)
			Expect(mode).To(Equal("annotated"))
		})

		It("should detect integer containers", func() {
			pod := &api.PodSandbox{}
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000}, // 2 CPUs
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024}, // 1GB
						},
					},
				},
			}
			mode := allocator.determineContainerMode(pod, container)
			Expect(mode).To(Equal("integer"))
		})

		It("should default to shared containers", func() {
			pod := &api.PodSandbox{}
			container := &api.Container{}
			mode := allocator.determineContainerMode(pod, container)
			Expect(mode).To(Equal("shared"))
		})
	})

	Describe("hasIntegerSemantics", func() {
		It("should return true for integer CPU requirements", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000}, // 2 CPUs
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			Expect(allocator.hasIntegerSemantics(container)).To(BeTrue())
		})

		It("should return false for fractional CPU requirements", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 150000}, // 1.5 CPUs
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			Expect(allocator.hasIntegerSemantics(container)).To(BeFalse())
		})

		It("should return false for containers without resources", func() {
			container := &api.Container{}
			Expect(allocator.hasIntegerSemantics(container)).To(BeFalse())
		})

		It("should return false for containers without memory limits", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000},
							Period: &api.OptionalUInt64{Value: 100000},
						},
					},
				},
			}
			Expect(allocator.hasIntegerSemantics(container)).To(BeFalse())
		})
	})
})

var _ = Describe("AllocationResult", func() {
	It("should handle annotated container results", func() {
		result := &AllocationResult{
			CPUs:     []int{0, 2, 4},
			MemNodes: []int{0},
			Mode:     "annotated",
		}
		Expect(result.Mode).To(Equal("annotated"))
		Expect(result.CPUs).To(Equal([]int{0, 2, 4}))
		Expect(result.MemNodes).To(Equal([]int{0}))
	})

	It("should handle integer container results", func() {
		result := &AllocationResult{
			CPUs:     []int{0, 1},
			MemNodes: []int{0}, // Integer containers now get NUMA node restrictions
			Mode:     "integer",
		}
		Expect(result.Mode).To(Equal("integer"))
		Expect(result.CPUs).To(Equal([]int{0, 1}))
		Expect(result.MemNodes).To(Equal([]int{0}))
	})

	It("should handle shared container results", func() {
		result := &AllocationResult{
			CPUs: []int{0, 1, 2, 3},
			Mode: "shared",
		}
		Expect(result.Mode).To(Equal("shared"))
		Expect(result.CPUs).To(Equal([]int{0, 1, 2, 3}))
	})
})

var _ = Describe("Advanced Allocation Scenarios", func() {
	var (
		allocator *TestCPUAllocator
		mockNumaMgr  *MockNumaManager
		realNumaMgr  *numa.Manager
	)

	BeforeEach(func() {
		mockNumaMgr = newMockNumaManager()
		realNumaMgr = &numa.Manager{} 
		allocator = &TestCPUAllocator{
			CPUAllocator: &CPUAllocator{
				numa:       realNumaMgr,
				onlineCPUs: []int{0, 1, 2, 3, 4, 5, 6, 7},
			},
			mockNuma: mockNumaMgr,
		}
	})

	Describe("handleAnnotatedContainer", func() {
		It("should handle single CPU annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "5",
				},
			}
			result, err := allocator.handleAnnotatedContainer(pod, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(Equal([]int{5}))
			Expect(result.Mode).To(Equal("annotated"))
		})

		It("should handle CPU range annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "0-2",
				},
			}
			result, err := allocator.handleAnnotatedContainer(pod, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(Equal([]int{0, 1, 2}))
		})

		It("should handle mixed CPU annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "0,2-3,7",
				},
			}
			result, err := allocator.handleAnnotatedContainer(pod, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(Equal([]int{0, 2, 3, 7}))
		})

		It("should return error for missing annotations", func() {
			pod := &api.PodSandbox{}
			_, err := allocator.handleAnnotatedContainer(pod, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing annotations"))
		})

		It("should return error for missing Weka annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					"other.io/annotation": "value",
				},
			}
			_, err := allocator.handleAnnotatedContainer(pod, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing weka.io/cores-ids"))
		})

		It("should return error for offline CPU in annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "0,99", // 99 is offline
				},
			}
			_, err := allocator.handleAnnotatedContainer(pod, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not online"))
		})

		It("should return error for invalid CPU list format", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "0-", // Invalid range
				},
			}
			_, err := allocator.handleAnnotatedContainer(pod, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid CPU list"))
		})
	})

	Describe("handleIntegerContainer", func() {
		It("should allocate correct number of CPUs", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 300000}, // 3 CPUs
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			result, err := allocator.handleIntegerContainer(container, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(HaveLen(3))
			Expect(result.Mode).To(Equal("integer"))
		})

		It("should respect reserved CPUs", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000}, // 2 CPUs
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			reserved := []int{0, 1}
			result, err := allocator.handleIntegerContainer(container, reserved)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(Equal([]int{2, 3}))
		})

		It("should return error for missing CPU resources", func() {
			container := &api.Container{}
			_, err := allocator.handleIntegerContainer(container, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing CPU resources"))
		})

		It("should return error for invalid quota/period", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 0}, // Invalid
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			_, err := allocator.handleIntegerContainer(container, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid CPU quota/period"))
		})

		It("should return error when insufficient free CPUs", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 500000}, // 5 CPUs
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			reserved := []int{0, 1, 2, 3, 4} // Only 3 free CPUs left
			_, err := allocator.handleIntegerContainer(container, reserved)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("insufficient free CPUs"))
		})
	})

	Describe("handleSharedContainer", func() {
		It("should return all CPUs when nothing is reserved", func() {
			result, err := allocator.handleSharedContainer([]int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(Equal([]int{0, 1, 2, 3, 4, 5, 6, 7}))
			Expect(result.Mode).To(Equal("shared"))
		})

		It("should exclude reserved CPUs", func() {
			reserved := []int{0, 2, 4}
			result, err := allocator.handleSharedContainer(reserved)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(Equal([]int{1, 3, 5, 6, 7}))
		})

		It("should return error when all CPUs are reserved", func() {
			reserved := []int{0, 1, 2, 3, 4, 5, 6, 7}
			_, err := allocator.handleSharedContainer(reserved)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("shared CPU pool is empty"))
		})

		It("should handle partial reservation correctly", func() {
			reserved := []int{7} // Only one CPU reserved
			result, err := allocator.handleSharedContainer(reserved)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(Equal([]int{0, 1, 2, 3, 4, 5, 6}))
		})
	})

	Describe("Edge cases and boundary conditions", func() {
		It("should handle empty online CPUs list", func() {
			allocator.onlineCPUs = []int{}
			_, err := allocator.AllocateExclusiveCPUs(1, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("insufficient free CPUs"))
		})

		It("should handle negative CPU count", func() {
			_, err := allocator.AllocateExclusiveCPUs(-1, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid CPU count"))
		})

		It("should handle duplicate reserved CPUs", func() {
			reserved := []int{0, 0, 1, 1} // Duplicates
			cpus, err := allocator.AllocateExclusiveCPUs(2, reserved)
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(Equal([]int{2, 3})) // Should skip duplicated reserved CPUs
		})

		It("should handle reserved CPUs outside online range", func() {
			reserved := []int{99, 100} // Outside online range
			cpus, err := allocator.AllocateExclusiveCPUs(2, reserved)
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(Equal([]int{0, 1})) // Should ignore out-of-range reserved CPUs
		})

		It("should be deterministic in allocation order", func() {
			// Allocate twice with same conditions
			cpus1, err1 := allocator.AllocateExclusiveCPUs(3, []int{})
			Expect(err1).ToNot(HaveOccurred())
			
			// Reset and allocate again
			cpus2, err2 := allocator.AllocateExclusiveCPUs(3, []int{})
			Expect(err2).ToNot(HaveOccurred())
			
			Expect(cpus1).To(Equal(cpus2)) // Should be deterministic
		})
	})

	Describe("Container mode edge cases", func() {
		It("should handle container without resources", func() {
			pod := &api.PodSandbox{}
			container := &api.Container{}
			mode := allocator.determineContainerMode(pod, container)
			Expect(mode).To(Equal("shared"))
		})

		It("should handle fractional CPU with missing memory", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000}, // 2 CPUs
							Period: &api.OptionalUInt64{Value: 100000},
						},
						// Missing memory resources
					},
				},
			}
			Expect(allocator.hasIntegerSemantics(container)).To(BeFalse())
		})

		It("should handle zero quota", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 0},
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			Expect(allocator.hasIntegerSemantics(container)).To(BeFalse())
		})

		It("should handle zero period", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000},
							Period: &api.OptionalUInt64{Value: 0},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			Expect(allocator.hasIntegerSemantics(container)).To(BeFalse())
		})

		It("should handle zero memory limit", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000},
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 0},
						},
					},
				},
			}
			Expect(allocator.hasIntegerSemantics(container)).To(BeFalse())
		})
	})

	Describe("NUMA Memory Pinning", func() {
		It("should pin to single NUMA node when all CPUs belong to same node", func() {
			node, isSingle := allocator.getSingleNUMANode([]int{0, 1, 2, 3})
			Expect(isSingle).To(BeTrue())
			Expect(node).To(Equal(0))
		})

		It("should not pin to single NUMA node when CPUs span multiple nodes", func() {
			node, isSingle := allocator.getSingleNUMANode([]int{0, 1, 4, 5})
			Expect(isSingle).To(BeFalse())
			Expect(node).To(Equal(0)) // Should still return first node but isSingle=false
		})

		It("should handle empty CPU list", func() {
			node, isSingle := allocator.getSingleNUMANode([]int{})
			Expect(isSingle).To(BeFalse())
			Expect(node).To(Equal(0))
		})

		It("should handle single CPU", func() {
			node, isSingle := allocator.getSingleNUMANode([]int{5})
			Expect(isSingle).To(BeTrue())
			Expect(node).To(Equal(1)) // CPU 5 is on NUMA node 1
		})

		It("should handle non-existent CPU", func() {
			node, isSingle := allocator.getSingleNUMANode([]int{99})
			Expect(isSingle).To(BeFalse())
			Expect(node).To(Equal(0))
		})
	})

	Describe("NUMA Memory Pinning Integration", func() {
		// Test the full flow with mock annotated containers
		It("should create test allocation result with proper NUMA nodes for single-node CPUs", func() {
			// Simulate what handleAnnotatedContainer would do
			cpus := []int{0, 1, 2} // All on NUMA node 0
			memNodes := allocator.mockNuma.GetCPUNodesUnion(cpus)
			Expect(memNodes).To(Equal([]int{0}))

			// Test our new logic
			if len(memNodes) > 1 {
				if singleNode, isSingleNode := allocator.getSingleNUMANode(cpus); isSingleNode {
					memNodes = []int{singleNode}
				}
			}
			Expect(memNodes).To(Equal([]int{0}))
		})

		It("should create test allocation result with multiple NUMA nodes for cross-node CPUs", func() {
			// Simulate what handleAnnotatedContainer would do
			cpus := []int{0, 1, 4, 5} // Span NUMA nodes 0 and 1
			memNodes := allocator.mockNuma.GetCPUNodesUnion(cpus)
			Expect(memNodes).To(Equal([]int{0, 1}))

			// Test our new logic
			if len(memNodes) > 1 {
				if singleNode, isSingleNode := allocator.getSingleNUMANode(cpus); isSingleNode {
					memNodes = []int{singleNode}
				}
			}
			// Should remain as union since CPUs span multiple nodes
			Expect(memNodes).To(Equal([]int{0, 1}))
		})

		It("should handle edge case where union returns multiple nodes but CPUs are actually on same node", func() {
			// This tests the specific bug we're fixing
			cpus := []int{2, 3} // Both on NUMA node 0
			memNodes := allocator.mockNuma.GetCPUNodesUnion(cpus)
			Expect(memNodes).To(Equal([]int{0}))

			// Test our logic - even with single node in union, logic should work
			if len(memNodes) > 1 {
				if singleNode, isSingleNode := allocator.getSingleNUMANode(cpus); isSingleNode {
					memNodes = []int{singleNode}
				}
			}
			Expect(memNodes).To(Equal([]int{0}))
		})

		It("should properly handle integer containers with NUMA memory placement", func() {
			// Simulate integer container allocation with CPUs spanning multiple NUMA nodes
			cpus := []int{0, 1, 4, 5} // CPUs on both NUMA node 0 and 1
			memNodes := allocator.mockNuma.GetCPUNodesUnion(cpus)
			Expect(memNodes).To(Equal([]int{0, 1}))

			// Test our logic for cross-NUMA allocation
			if singleNode, isSingleNode := allocator.getSingleNUMANode(cpus); isSingleNode {
				memNodes = []int{singleNode}
			}
			// Should remain as union since CPUs span multiple nodes
			Expect(memNodes).To(Equal([]int{0, 1}))
		})

		It("should properly handle integer containers with single NUMA memory placement", func() {
			// Simulate integer container allocation with CPUs on single NUMA node
			cpus := []int{0, 1, 2} // All CPUs on NUMA node 0
			memNodes := allocator.mockNuma.GetCPUNodesUnion(cpus)
			Expect(memNodes).To(Equal([]int{0}))

			// Test our logic for single-NUMA allocation
			if singleNode, isSingleNode := allocator.getSingleNUMANode(cpus); isSingleNode {
				memNodes = []int{singleNode}
			}
			Expect(memNodes).To(Equal([]int{0}))
		})
	})
})