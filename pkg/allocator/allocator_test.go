package allocator

import (
	"testing"

	"github.com/containerd/nri/pkg/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/weka/nri-cpuset/pkg/numa"
)

func TestAllocator(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Allocator Suite")
}

// Mock NUMA manager for testing - created as a real numa.Manager for type compatibility
func newMockNumaManager() *numa.Manager {
	// Create a real numa.Manager but with mock data
	mgr := &numa.Manager{}
	
	// We can't directly access private fields, so we'll use a simplified approach
	// In a real test environment, you might need to use dependency injection or interfaces
	return mgr
}


var _ = Describe("CPUAllocator", func() {
	var (
		allocator *CPUAllocator
		numaMgr   *numa.Manager
	)

	BeforeEach(func() {
		numaMgr = newMockNumaManager()
		// Create allocator with mock online CPUs for testing
		allocator = &CPUAllocator{
			numa:       numaMgr,
			onlineCPUs: []int{0, 1, 2, 3, 4, 5, 6, 7}, // Mock online CPUs
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
			CPUs: []int{0, 1},
			Mode: "integer",
		}
		Expect(result.Mode).To(Equal("integer"))
		Expect(result.CPUs).To(Equal([]int{0, 1}))
		Expect(result.MemNodes).To(BeEmpty())
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
		allocator *CPUAllocator
		numaMgr   *numa.Manager
	)

	BeforeEach(func() {
		numaMgr = newMockNumaManager()
		allocator = &CPUAllocator{
			numa:       numaMgr,
			onlineCPUs: []int{0, 1, 2, 3, 4, 5, 6, 7},
		}
	})

	Describe("handleAnnotatedContainer", func() {
		It("should handle single CPU annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "5",
				},
			}
			result, err := allocator.handleAnnotatedContainer(pod)
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
			result, err := allocator.handleAnnotatedContainer(pod)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(Equal([]int{0, 1, 2}))
		})

		It("should handle mixed CPU annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "0,2-3,7",
				},
			}
			result, err := allocator.handleAnnotatedContainer(pod)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.CPUs).To(Equal([]int{0, 2, 3, 7}))
		})

		It("should return error for missing annotations", func() {
			pod := &api.PodSandbox{}
			_, err := allocator.handleAnnotatedContainer(pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing annotations"))
		})

		It("should return error for missing Weka annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					"other.io/annotation": "value",
				},
			}
			_, err := allocator.handleAnnotatedContainer(pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing weka.io/cores-ids"))
		})

		It("should return error for offline CPU in annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "0,99", // 99 is offline
				},
			}
			_, err := allocator.handleAnnotatedContainer(pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not online"))
		})

		It("should return error for invalid CPU list format", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "0-", // Invalid range
				},
			}
			_, err := allocator.handleAnnotatedContainer(pod)
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
})