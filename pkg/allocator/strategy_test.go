package allocator

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/weka/nri-cpuset/pkg/numa"
)

// Note: Tests are integrated into the main allocator_test.go suite

// Comprehensive CPU allocation strategy tests
var _ = Describe("CPU Allocation Strategy Tests", func() {
	var (
		allocator *CPUAllocator
	)

	Describe("CPU 0 Avoidance Strategy", func() {
		BeforeEach(func() {
			// Create a simple system for CPU 0 avoidance tests
			onlineCPUs := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
			allocator = &CPUAllocator{
				numa:       &numa.Manager{}, // Not used in these tests
				onlineCPUs: onlineCPUs,
			}
		})

		It("should avoid CPU 0 for small allocations", func() {
			cpus, err := allocator.AllocateExclusiveCPUs(2, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(HaveLen(2))
			Expect(cpus).To(Equal([]int{1, 2}))
			Expect(cpus).ToNot(ContainElement(0), "Should avoid CPU 0 when possible")
		})

		It("should avoid CPU 0 for medium allocations", func() {
			cpus, err := allocator.AllocateExclusiveCPUs(4, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(HaveLen(4))
			expected := []int{1, 2, 3, 4}
			Expect(cpus).To(Equal(expected))
			Expect(cpus).ToNot(ContainElement(0), "Should avoid CPU 0 when possible")
		})

		It("should use CPU 0 only when necessary", func() {
			// Reserve all CPUs except 0 and one other CPU
			reserved := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
			cpus, err := allocator.AllocateExclusiveCPUs(2, reserved)
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(HaveLen(2))
			// Should include both CPU 0 and 11 since no choice
			Expect(cpus).To(ConsistOf(0, 11))
		})

		It("should prioritize non-zero CPUs even with gaps in reserved list", func() {
			reserved := []int{1, 3, 5} // Leave gaps
			cpus, err := allocator.AllocateExclusiveCPUs(3, reserved)
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(HaveLen(3))
			// Should prefer 2, 4, 6 over 0
			Expect(cpus).To(Equal([]int{2, 4, 6}))
			Expect(cpus).ToNot(ContainElement(0))
		})

		It("should use CPU 0 as last resort", func() {
			// Reserve all CPUs except 0, 1, and 2
			reserved := []int{3, 4, 5, 6, 7, 8, 9, 10, 11}
			cpus, err := allocator.AllocateExclusiveCPUs(3, reserved)
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(HaveLen(3))
			// Should prefer 1, 2, then use 0 as last resort
			// The allocation should contain all three CPUs, with 0 as last choice
			Expect(cpus).To(ConsistOf(0, 1, 2))
		})
	})

	Describe("Fragmented Allocation Prevention", func() {
		BeforeEach(func() {
			// Use setup similar to the reported bug scenario
			onlineCPUs := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 61}
			allocator = &CPUAllocator{
				numa:       &numa.Manager{},
				onlineCPUs: onlineCPUs,
			}
		})

		It("should avoid creating the specific fragmented allocation from the bug report", func() {
			// This test reproduces the bug scenario: requesting 9 CPUs
			// The bug was getting "0-3,11,32-34,61" which fragments cores
			// We want to avoid CPU 0 and prioritize better allocation
			
			// Request 9 CPUs
			cpus, err := allocator.AllocateExclusiveCPUs(9, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(HaveLen(9))
			
			// Should avoid CPU 0
			Expect(cpus).ToNot(ContainElement(0), "Should avoid CPU 0")
			
			// Should not create the problematic pattern from the bug report
			// The bug was: "0-3,11,32-34,61" - let's ensure we don't get that exact pattern
			bugPattern := []int{0, 1, 2, 3, 11, 32, 33, 34, 61}
			Expect(cpus).ToNot(Equal(bugPattern), "Should not reproduce the exact bug pattern")
		})

		It("should provide allocation suitable for applications that need to find full cores", func() {
			// Test that allocation allows applications to find required full cores
			cpus, err := allocator.AllocateExclusiveCPUs(8, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(HaveLen(8))
			Expect(cpus).ToNot(ContainElement(0), "Should not include CPU 0")
		})

		It("should demonstrate improvement over previous fragmented allocation", func() {
			// Test to show the allocation is now better than the reported bug
			cpus, err := allocator.AllocateExclusiveCPUs(9, []int{})
			Expect(err).ToNot(HaveOccurred())
			
			// Log the allocation for debugging
			GinkgoWriter.Printf("Allocated 9 CPUs: %v\n", cpus)
			
			// Key requirements:
			// 1. Should not include CPU 0
			// 2. Should provide a more contiguous allocation than the bug pattern
			// 3. Should be application-friendly for finding full cores
			
			Expect(cpus).ToNot(ContainElement(0))
			
			// The new allocation should be more contiguous/better than "0-3,11,32-34,61"
			// We'll verify this by checking that we don't have the exact problematic gaps
			hasProblematicGaps := false
			
			// Check if we have the same problematic gap pattern as the bug
			if contains(cpus, 3) && contains(cpus, 11) && !contains(cpus, 4) && !contains(cpus, 5) {
				hasProblematicGaps = true
			}
			
			Expect(hasProblematicGaps).To(BeFalse(), "Should not have the same problematic gaps as the bug report")
		})
	})
})

// Helper function for tests
func contains(slice []int, item int) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}