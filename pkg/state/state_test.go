package state

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestState(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "State Suite")
}

// Create a simple mock allocator that implements the needed methods for testing
type TestAllocator struct {
	onlineCPUs []int
}

func (t *TestAllocator) GetOnlineCPUs() []int {
	return append([]int(nil), t.onlineCPUs...)
}

func (t *TestAllocator) AllocateExclusiveCPUs(count int, reserved []int) ([]int, error) {
	reservedSet := make(map[int]struct{})
	for _, cpu := range reserved {
		reservedSet[cpu] = struct{}{}
	}

	var available []int
	for _, cpu := range t.onlineCPUs {
		if _, isReserved := reservedSet[cpu]; !isReserved {
			available = append(available, cpu)
		}
	}

	if len(available) < count {
		return nil, fmt.Errorf("insufficient free CPUs: need %d, have %d", count, len(available))
	}

	return available[:count], nil
}

// Mock allocator for testing - create a proper mock that works with the interface
func newMockAllocator() *TestAllocator {
	return &TestAllocator{
		onlineCPUs: []int{0, 1, 2, 3, 4, 5, 6, 7}, // Mock 8 CPUs for testing
	}
}


var _ = Describe("Manager", func() {
	var (
		manager *Manager
		alloc   *TestAllocator
	)

	BeforeEach(func() {
		manager = NewManager()
		alloc = newMockAllocator()
	})

	Describe("determineContainerMode", func() {
		It("should detect annotated containers", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "0,2,4",
				},
			}
			container := &api.Container{}
			mode := manager.determineContainerMode(pod, container)
			Expect(mode).To(Equal(ModeAnnotated))
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
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			mode := manager.determineContainerMode(pod, container)
			Expect(mode).To(Equal(ModeInteger))
		})

		It("should default to shared containers", func() {
			pod := &api.PodSandbox{}
			container := &api.Container{}
			mode := manager.determineContainerMode(pod, container)
			Expect(mode).To(Equal(ModeShared))
		})
	})

	Describe("GetReservedCPUs", func() {
		BeforeEach(func() {
			// Set up some reservations
			manager.annotRef[0] = 1
			manager.annotRef[2] = 2
			manager.intOwner[4] = "container1"
			manager.intOwner[6] = "container2"
		})

		It("should return all reserved CPUs", func() {
			reserved := manager.GetReservedCPUs()
			Expect(reserved).To(ConsistOf(0, 2, 4, 6))
		})
	})

	Describe("IsExclusive", func() {
		BeforeEach(func() {
			manager.intOwner[4] = "container1"
			manager.annotRef[2] = 1
		})

		It("should return true for integer-owned CPUs", func() {
			Expect(manager.IsExclusive(4)).To(BeTrue())
		})

		It("should return false for annotated CPUs", func() {
			Expect(manager.IsExclusive(2)).To(BeFalse())
		})

		It("should return false for unallocated CPUs", func() {
			Expect(manager.IsExclusive(6)).To(BeFalse())
		})
	})

	Describe("IsReserved", func() {
		BeforeEach(func() {
			manager.intOwner[4] = "container1"
			manager.annotRef[2] = 1
		})

		It("should return true for integer-owned CPUs", func() {
			Expect(manager.IsReserved(4)).To(BeTrue())
		})

		It("should return true for annotated CPUs", func() {
			Expect(manager.IsReserved(2)).To(BeTrue())
		})

		It("should return false for unallocated CPUs", func() {
			Expect(manager.IsReserved(6)).To(BeFalse())
		})
	})

	Describe("RemoveContainer", func() {
		var containerInfo *ContainerInfo

		BeforeEach(func() {
			containerInfo = &ContainerInfo{
				ID:   "container1",
				Mode: ModeInteger,
				CPUs: []int{0, 1},
			}
			manager.byCID["container1"] = containerInfo
			manager.intOwner[0] = "container1"
			manager.intOwner[1] = "container1"
		})

		It("should release integer reservations", func() {
			updates, err := manager.RemoveContainer("container1", alloc)
			Expect(err).ToNot(HaveOccurred())
			
			Expect(manager.intOwner).ToNot(HaveKey(0))
			Expect(manager.intOwner).ToNot(HaveKey(1))
			Expect(manager.byCID).ToNot(HaveKey("container1"))
			Expect(updates).To(HaveLen(0)) // Should return empty slice, not nil
		})

		It("should handle non-existent containers", func() {
			updates, err := manager.RemoveContainer("nonexistent", alloc)
			Expect(err).ToNot(HaveOccurred())
			Expect(updates).To(BeNil())
		})
	})

	Describe("Synchronize", func() {
		var (
			pods       []*api.PodSandbox
			containers []*api.Container
		)

		BeforeEach(func() {
			pods = []*api.PodSandbox{
				{
					Id:   "pod1",
					Name: "test-pod",
					Annotations: map[string]string{
						WekaAnnotation: "0,2",
					},
				},
				{
					Id:   "pod2",
					Name: "integer-pod",
				},
			}

			containers = []*api.Container{
				{
					Id:    "container1",
					Name:  "annotated-container",
					PodSandboxId: "pod1",
				},
				{
					Id:    "container2",
					Name:  "integer-container", 
					PodSandboxId: "pod2",
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
				},
			}
		})

		It("should rebuild state from containers", func() {
			updates, err := manager.Synchronize(pods, containers, alloc)
			Expect(err).ToNot(HaveOccurred())
			
			// Check that state was rebuilt
			Expect(manager.byCID).To(HaveKey("container1"))
			Expect(manager.byCID).To(HaveKey("container2"))
			
			// Check annotated container
			info1 := manager.byCID["container1"]
			Expect(info1.Mode).To(Equal(ModeAnnotated))
			Expect(info1.CPUs).To(Equal([]int{0, 2}))
			
			// Check integer container
			info2 := manager.byCID["container2"]
			Expect(info2.Mode).To(Equal(ModeInteger))
			Expect(len(info2.CPUs)).To(Equal(2))
			
			// Should return empty updates if no shared containers
			Expect(updates).ToNot(BeNil())
		})

		It("should handle containers without matching pods", func() {
			containers = append(containers, &api.Container{
				Id:    "orphan",
				PodSandboxId: "nonexistent",
			})
			
			updates, err := manager.Synchronize(pods, containers, alloc)
			Expect(err).ToNot(HaveOccurred())
			Expect(manager.byCID).ToNot(HaveKey("orphan"))
			Expect(updates).ToNot(BeNil())
		})
	})

	Describe("computeSharedPool", func() {
		BeforeEach(func() {
			manager.annotRef[0] = 1
			manager.intOwner[2] = "container1"
		})

		It("should exclude reserved CPUs", func() {
			onlineCPUs := []int{0, 1, 2, 3, 4, 5, 6, 7}
			pool := manager.computeSharedPool(onlineCPUs)
			Expect(pool).To(Equal([]int{1, 3, 4, 5, 6, 7}))
		})

		It("should handle empty reservations", func() {
			manager.annotRef = make(map[int]int)
			manager.intOwner = make(map[int]string)
			
			onlineCPUs := []int{0, 1, 2, 3}
			pool := manager.computeSharedPool(onlineCPUs)
			Expect(pool).To(Equal(onlineCPUs))
		})
	})
})

var _ = Describe("ContainerInfo", func() {
	It("should store container information", func() {
		info := &ContainerInfo{
			ID:     "container1",
			Mode:   ModeAnnotated,
			CPUs:   []int{0, 2, 4},
			PodID:  "pod1",
			PodUID: "uid123",
		}
		
		Expect(info.ID).To(Equal("container1"))
		Expect(info.Mode).To(Equal(ModeAnnotated))
		Expect(info.CPUs).To(Equal([]int{0, 2, 4}))
		Expect(info.PodID).To(Equal("pod1"))
		Expect(info.PodUID).To(Equal("uid123"))
	})
})

var _ = Describe("Container Modes", func() {
	It("should have correct mode values", func() {
		Expect(ModeShared).To(Equal(ContainerMode(0)))
		Expect(ModeInteger).To(Equal(ContainerMode(1)))
		Expect(ModeAnnotated).To(Equal(ContainerMode(2)))
	})
})

var _ = Describe("Advanced State Synchronization", func() {
	var (
		manager *Manager
		alloc   *TestAllocator
	)

	BeforeEach(func() {
		manager = NewManager()
		alloc = newMockAllocator()
	})

	Describe("Complex synchronization scenarios", func() {
		It("should handle mixed container types during synchronization", func() {
			pods := []*api.PodSandbox{
				{
					Id:   "pod1",
					Name: "annotated-pod",
					Uid:  "uid1",
					Annotations: map[string]string{
						WekaAnnotation: "0,2",
					},
				},
				{
					Id:   "pod2",
					Name: "integer-pod",
					Uid:  "uid2",
				},
				{
					Id:   "pod3",
					Name: "shared-pod",
					Uid:  "uid3",
				},
			}

			containers := []*api.Container{
				{
					Id:           "container1",
					Name:         "annotated-container",
					PodSandboxId: "pod1",
				},
				{
					Id:           "container2",
					Name:         "integer-container",
					PodSandboxId: "pod2",
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
				},
				{
					Id:           "container3",
					Name:         "shared-container",
					PodSandboxId: "pod3",
				},
			}

			updates, err := manager.Synchronize(pods, containers, alloc)
			Expect(err).ToNot(HaveOccurred())

			// Check annotated container state
			info1 := manager.byCID["container1"]
			Expect(info1).ToNot(BeNil())
			Expect(info1.Mode).To(Equal(ModeAnnotated))
			Expect(info1.CPUs).To(Equal([]int{0, 2}))

			// Check integer container state
			info2 := manager.byCID["container2"]
			Expect(info2).ToNot(BeNil())
			Expect(info2.Mode).To(Equal(ModeInteger))
			Expect(len(info2.CPUs)).To(Equal(2))
			// CPUs should not overlap with annotated
			for _, cpu := range info2.CPUs {
				Expect(cpu).ToNot(BeElementOf([]int{0, 2}))
			}

			// Check shared container state
			info3 := manager.byCID["container3"]
			Expect(info3).ToNot(BeNil())
			Expect(info3.Mode).To(Equal(ModeShared))

			// Should have updates for shared container
			Expect(updates).ToNot(BeEmpty())
			foundSharedUpdate := false
			for _, update := range updates {
				if update.ContainerId == "container3" {
					foundSharedUpdate = true
					Expect(update.Linux.Resources.Cpu.Cpus).ToNot(BeEmpty())
				}
			}
			Expect(foundSharedUpdate).To(BeTrue(), "Shared container should receive update")
		})

		It("should handle multiple annotated containers sharing CPUs", func() {
			pods := []*api.PodSandbox{
				{
					Id:   "pod1",
					Name: "shared-annotated-1",
					Annotations: map[string]string{
						WekaAnnotation: "0,1",
					},
				},
				{
					Id:   "pod2",
					Name: "shared-annotated-2",
					Annotations: map[string]string{
						WekaAnnotation: "0,1", // Same CPUs
					},
				},
			}

			containers := []*api.Container{
				{
					Id:           "container1",
					PodSandboxId: "pod1",
				},
				{
					Id:           "container2",
					PodSandboxId: "pod2",
				},
			}

			_, err := manager.Synchronize(pods, containers, alloc)
			Expect(err).ToNot(HaveOccurred())

			// Both containers should have the same CPUs
			info1 := manager.byCID["container1"]
			info2 := manager.byCID["container2"]
			Expect(info1.CPUs).To(Equal([]int{0, 1}))
			Expect(info2.CPUs).To(Equal([]int{0, 1}))

			// Reference count should be 2 for each shared CPU
			Expect(manager.annotRef[0]).To(Equal(2))
			Expect(manager.annotRef[1]).To(Equal(2))
		})

		It("should handle containers without matching pods gracefully", func() {
			pods := []*api.PodSandbox{
				{
					Id:   "pod1",
					Name: "existing-pod",
				},
			}

			containers := []*api.Container{
				{
					Id:           "container1",
					PodSandboxId: "pod1", // Matches
				},
				{
					Id:           "container2",
					PodSandboxId: "nonexistent-pod", // Orphan
				},
			}

			updates, err := manager.Synchronize(pods, containers, alloc)
			Expect(err).ToNot(HaveOccurred())

			// Only container1 should be in state
			Expect(manager.byCID).To(HaveKey("container1"))
			Expect(manager.byCID).ToNot(HaveKey("container2"))
			Expect(updates).ToNot(BeNil()) // Should return valid slice
		})

		It("should handle empty pods and containers", func() {
			updates, err := manager.Synchronize([]*api.PodSandbox{}, []*api.Container{}, alloc)
			Expect(err).ToNot(HaveOccurred())
			Expect(updates).ToNot(BeNil())
			Expect(updates).To(BeEmpty())

			// State should be empty
			Expect(manager.byCID).To(BeEmpty())
			Expect(manager.annotRef).To(BeEmpty())
			Expect(manager.intOwner).To(BeEmpty())
		})

		It("should clear previous state during synchronization", func() {
			// Set up initial state
			manager.byCID["old-container"] = &ContainerInfo{ID: "old-container"}
			manager.annotRef[5] = 1
			manager.intOwner[6] = "old-container"

			// Synchronize with new containers
			pods := []*api.PodSandbox{{Id: "pod1"}}
			containers := []*api.Container{{Id: "container1", PodSandboxId: "pod1"}}

			_, err := manager.Synchronize(pods, containers, alloc)
			Expect(err).ToNot(HaveOccurred())

			// Old state should be cleared
			Expect(manager.byCID).ToNot(HaveKey("old-container"))
			Expect(manager.annotRef).ToNot(HaveKey(5))
			Expect(manager.intOwner).ToNot(HaveKey(6))

			// New state should be present
			Expect(manager.byCID).To(HaveKey("container1"))
		})
	})

	Describe("Error handling during synchronization", func() {
		It("should continue processing other containers when one fails", func() {
			pods := []*api.PodSandbox{
				{
					Id:   "pod1",
					Name: "good-pod",
					Annotations: map[string]string{
						WekaAnnotation: "0,1",
					},
				},
				{
					Id:   "pod2",
					Name: "bad-pod",
					Annotations: map[string]string{
						WekaAnnotation: "invalid-cpu-list",
					},
				},
			}

			containers := []*api.Container{
				{
					Id:           "good-container",
					PodSandboxId: "pod1",
				},
				{
					Id:           "bad-container",
					PodSandboxId: "pod2",
				},
			}

			updates, err := manager.Synchronize(pods, containers, alloc)
			Expect(err).ToNot(HaveOccurred()) // Should not fail entire sync

			// Good container should be processed
			Expect(manager.byCID).To(HaveKey("good-container"))
			info := manager.byCID["good-container"]
			Expect(info.Mode).To(Equal(ModeAnnotated))
			Expect(info.CPUs).To(Equal([]int{0, 1}))

			// Bad container should be skipped
			Expect(manager.byCID).ToNot(HaveKey("bad-container"))

			Expect(updates).ToNot(BeNil())
		})

		It("should handle insufficient CPUs for integer containers", func() {
			// Set up allocator with limited CPUs
			limitedAlloc := &TestAllocator{
				onlineCPUs: []int{0, 1}, // Only 2 CPUs available
			}

			pods := []*api.PodSandbox{
				{Id: "pod1"},
				{Id: "pod2"},
			}

			containers := []*api.Container{
				{
					Id:           "container1",
					PodSandboxId: "pod1",
					Linux: &api.LinuxContainer{
						Resources: &api.LinuxResources{
							Cpu: &api.LinuxCPU{
								Quota:  &api.OptionalInt64{Value: 400000}, // 4 CPUs - more than available
								Period: &api.OptionalUInt64{Value: 100000},
							},
							Memory: &api.LinuxMemory{
								Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
							},
						},
					},
				},
				{
					Id:           "container2",
					PodSandboxId: "pod2", // This one should still work
				},
			}

			updates, err := manager.Synchronize(pods, containers, limitedAlloc)
			Expect(err).ToNot(HaveOccurred())

			// First container should fail to allocate and be skipped
			Expect(manager.byCID).ToNot(HaveKey("container1"))

			// Second container (shared) should work
			Expect(manager.byCID).To(HaveKey("container2"))
			Expect(updates).ToNot(BeNil())
		})
	})

	Describe("Shared pool computation edge cases", func() {
		It("should handle complex reservation patterns", func() {
			// Mix of annotations and integer reservations
			manager.annotRef[0] = 1   // CPU 0 referenced once
			manager.annotRef[2] = 2   // CPU 2 referenced twice
			manager.intOwner[4] = "container1"  // CPU 4 owned by integer container
			manager.intOwner[5] = "container2"  // CPU 5 owned by integer container

			onlineCPUs := []int{0, 1, 2, 3, 4, 5, 6, 7}
			sharedPool := manager.computeSharedPool(onlineCPUs)

			// Should exclude all reserved CPUs regardless of reference count or type
			expected := []int{1, 3, 6, 7}
			Expect(sharedPool).To(Equal(expected))
		})

		It("should handle all CPUs reserved", func() {
			onlineCPUs := []int{0, 1, 2, 3}
			manager.annotRef[0] = 1
			manager.annotRef[1] = 1
			manager.intOwner[2] = "container1"
			manager.intOwner[3] = "container2"

			sharedPool := manager.computeSharedPool(onlineCPUs)
			Expect(sharedPool).To(BeEmpty())
		})

		It("should handle no reservations", func() {
			onlineCPUs := []int{0, 1, 2, 3, 4, 5, 6, 7}
			sharedPool := manager.computeSharedPool(onlineCPUs)
			Expect(sharedPool).To(Equal(onlineCPUs))
		})

		It("should handle sparse CPU numbering", func() {
			onlineCPUs := []int{1, 3, 5, 7, 9} // Non-consecutive
			manager.annotRef[3] = 1
			manager.intOwner[7] = "container1"

			sharedPool := manager.computeSharedPool(onlineCPUs)
			expected := []int{1, 5, 9}
			Expect(sharedPool).To(Equal(expected))
		})
	})

	Describe("Container removal complex scenarios", func() {
		BeforeEach(func() {
			// Set up complex initial state
			manager.byCID["annotated1"] = &ContainerInfo{
				ID:   "annotated1",
				Mode: ModeAnnotated,
				CPUs: []int{0, 2},
			}
			manager.byCID["annotated2"] = &ContainerInfo{
				ID:   "annotated2",
				Mode: ModeAnnotated,
				CPUs: []int{0, 4}, // Shares CPU 0 with annotated1
			}
			manager.byCID["integer1"] = &ContainerInfo{
				ID:   "integer1",
				Mode: ModeInteger,
				CPUs: []int{6, 7},
			}
			manager.byCID["shared1"] = &ContainerInfo{
				ID:   "shared1",
				Mode: ModeShared,
				CPUs: []int{1, 3, 5}, // Current shared pool
			}

			// Set up reservation maps
			manager.annotRef[0] = 2 // Shared by two containers
			manager.annotRef[2] = 1
			manager.annotRef[4] = 1
			manager.intOwner[6] = "integer1"
			manager.intOwner[7] = "integer1"
		})

		It("should properly decrement annotation reference count", func() {
			updates, err := manager.RemoveContainer("annotated1", alloc)
			Expect(err).ToNot(HaveOccurred())

			// CPU 0 should still be reserved (referenced by annotated2)
			Expect(manager.annotRef[0]).To(Equal(1))
			
			// CPU 2 should be freed
			Expect(manager.annotRef).ToNot(HaveKey(2))

			// Container should be removed from state
			Expect(manager.byCID).ToNot(HaveKey("annotated1"))

			// Shared containers should get updated with expanded pool
			Expect(updates).ToNot(BeEmpty())
		})

		It("should remove annotation completely when reference count reaches zero", func() {
			// Remove first container sharing CPU 0
			_, err := manager.RemoveContainer("annotated1", alloc)
			Expect(err).ToNot(HaveOccurred())
			Expect(manager.annotRef[0]).To(Equal(1)) // Still referenced

			// Remove second container sharing CPU 0
			_, err = manager.RemoveContainer("annotated2", alloc)
			Expect(err).ToNot(HaveOccurred())
			
			// CPU 0 should now be completely freed
			Expect(manager.annotRef).ToNot(HaveKey(0))
			Expect(manager.annotRef).ToNot(HaveKey(4))
		})

		It("should update all shared containers after removal", func() {
			// Remove integer container to expand shared pool
			updates, err := manager.RemoveContainer("integer1", alloc)
			Expect(err).ToNot(HaveOccurred())

			// Integer CPUs should be freed
			Expect(manager.intOwner).ToNot(HaveKey(6))
			Expect(manager.intOwner).ToNot(HaveKey(7))

			// Should have updates for shared containers
			foundUpdate := false
			for _, update := range updates {
				if update.ContainerId == "shared1" {
					foundUpdate = true
					// New shared pool should include freed CPUs 6,7
					Expect(update.Linux.Resources.Cpu.Cpus).To(Or(
						ContainSubstring("6"),
						ContainSubstring("7"),
					))
				}
			}
			Expect(foundUpdate).To(BeTrue())
		})

		It("should handle removal of non-existent container", func() {
			updates, err := manager.RemoveContainer("nonexistent", alloc)
			Expect(err).ToNot(HaveOccurred())
			Expect(updates).To(BeNil())

			// State should remain unchanged
			Expect(len(manager.byCID)).To(Equal(4))
		})

		It("should handle shared container removal without affecting reservations", func() {
			initialReservations := len(manager.annotRef) + len(manager.intOwner)

			updates, err := manager.RemoveContainer("shared1", alloc)
			Expect(err).ToNot(HaveOccurred())

			// Reservations should be unchanged
			currentReservations := len(manager.annotRef) + len(manager.intOwner)
			Expect(currentReservations).To(Equal(initialReservations))

			// Container should be removed
			Expect(manager.byCID).ToNot(HaveKey("shared1"))

			// No updates should be generated for removing shared containers
			Expect(updates).To(BeEmpty())
		})
	})

	Describe("getContainerCPUs edge cases", func() {
		It("should handle annotated container with empty annotation", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					WekaAnnotation: "",
				},
			}
			container := &api.Container{}

			cpus, err := manager.getContainerCPUs(pod, container, ModeAnnotated, alloc, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(BeEmpty()) // Empty annotation should result in empty CPU list
		})

		It("should handle integer container with very large CPU requirement", func() {
			container := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 1000000000}, // 10000 CPUs
							Period: &api.OptionalUInt64{Value: 100000},
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}

			_, err := manager.getContainerCPUs(&api.PodSandbox{}, container, ModeInteger, alloc, []int{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("insufficient free CPUs"))
		})

		It("should handle shared container with no online CPUs", func() {
			emptyAlloc := &TestAllocator{onlineCPUs: []int{}}
			
			cpus, err := manager.getContainerCPUs(&api.PodSandbox{}, &api.Container{}, ModeShared, emptyAlloc, []int{})
			Expect(err).ToNot(HaveOccurred())
			Expect(cpus).To(BeEmpty())
		})
	})
})

var _ = Describe("Concurrency Safety", func() {
	var (
		manager *Manager
		alloc   *TestAllocator
	)

	BeforeEach(func() {
		manager = NewManager()
		alloc = newMockAllocator()
	})

	Describe("Concurrent access patterns", func() {
		It("should handle concurrent reads", func() {
			// Set up some initial state
			manager.byCID["container1"] = &ContainerInfo{
				ID:   "container1",
				Mode: ModeAnnotated,
				CPUs: []int{0, 2, 4},
			}
			manager.annotRef[0] = 1
			manager.annotRef[2] = 1
			manager.annotRef[4] = 1

			const numGoroutines = 50
			const operationsPerGoroutine = 100
			
			var wg sync.WaitGroup
			wg.Add(numGoroutines)

			// Start multiple goroutines doing concurrent reads
			for i := 0; i < numGoroutines; i++ {
				go func() {
					defer wg.Done()
					for j := 0; j < operationsPerGoroutine; j++ {
						// Test various read operations
						manager.GetReservedCPUs()
						manager.IsReserved(0)
						manager.IsReserved(1)
						manager.IsExclusive(0)
						manager.IsExclusive(2)
					}
				}()
			}

			// Wait for all goroutines to complete
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			// Should complete without deadlock
			select {
			case <-done:
				// Success
			case <-time.After(5 * time.Second):
				Fail("Concurrent reads timed out - possible deadlock")
			}
		})

		It("should handle concurrent read/write operations", func() {
			const numReaders = 20
			const numWriters = 10
			const operations = 50

			var wg sync.WaitGroup
			wg.Add(numReaders + numWriters)

			// Start reader goroutines
			for i := 0; i < numReaders; i++ {
				go func(id int) {
					defer wg.Done()
					for j := 0; j < operations; j++ {
						manager.GetReservedCPUs()
						manager.IsReserved(id % 8)
						manager.IsExclusive((id + j) % 8)
						time.Sleep(time.Microsecond) // Small delay to increase interleaving
					}
				}(i)
			}

			// Start writer goroutines
			for i := 0; i < numWriters; i++ {
				go func(id int) {
					defer wg.Done()
					for j := 0; j < operations; j++ {
						containerID := fmt.Sprintf("writer-%d-%d", id, j)
						info := &ContainerInfo{
							ID:   containerID,
							Mode: ModeShared,
							CPUs: []int{id % 8},
						}
						
						// Simulate container addition and removal
						manager.mu.Lock()
						manager.byCID[containerID] = info
						manager.mu.Unlock()
						
						time.Sleep(time.Microsecond)
						
						manager.RemoveContainer(containerID, alloc)
						time.Sleep(time.Microsecond)
					}
				}(i)
			}

			// Wait for completion with timeout
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				// Success - verify final state is consistent
				reserved := manager.GetReservedCPUs()
				// Should not panic - reserved can be empty slice which is still valid
				_ = reserved
			case <-time.After(10 * time.Second):
				Fail("Concurrent read/write operations timed out")
			}
		})

		It("should handle concurrent synchronization calls", func() {
			const numSyncs = 20
			var wg sync.WaitGroup
			wg.Add(numSyncs)

			// Create test data
			pods := []*api.PodSandbox{
				{
					Id:   "pod1",
					Name: "test-pod",
					Annotations: map[string]string{
						WekaAnnotation: "0,2",
					},
				},
			}

			containers := []*api.Container{
				{
					Id:           "container1",
					PodSandboxId: "pod1",
				},
			}

			// Start multiple synchronization operations concurrently
			for i := 0; i < numSyncs; i++ {
				go func() {
					defer wg.Done()
					_, err := manager.Synchronize(pods, containers, alloc)
					Expect(err).ToNot(HaveOccurred())
				}()
			}

			// Wait for completion
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				// Verify final state is consistent
				info := manager.byCID["container1"]
				Expect(info).ToNot(BeNil())
				Expect(info.Mode).To(Equal(ModeAnnotated))
				Expect(info.CPUs).To(Equal([]int{0, 2}))
			case <-time.After(10 * time.Second):
				Fail("Concurrent synchronization timed out")
			}
		})

		It("should handle concurrent container removals", func() {
			// Set up initial state with multiple containers
			const numContainers = 50
			
			for i := 0; i < numContainers; i++ {
				containerID := fmt.Sprintf("container-%d", i)
				manager.byCID[containerID] = &ContainerInfo{
					ID:   containerID,
					Mode: ModeShared,
					CPUs: []int{i % 8},
				}
			}

			var wg sync.WaitGroup
			wg.Add(numContainers)

			// Start concurrent removal operations
			for i := 0; i < numContainers; i++ {
				go func(id int) {
					defer wg.Done()
					containerID := fmt.Sprintf("container-%d", id)
					_, err := manager.RemoveContainer(containerID, alloc)
					Expect(err).ToNot(HaveOccurred())
				}(i)
			}

			// Wait for all removals to complete
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				// All containers should be removed
				Expect(manager.byCID).To(BeEmpty())
			case <-time.After(10 * time.Second):
				Fail("Concurrent container removals timed out")
			}
		})

		It("should maintain consistency during mixed operations", func() {
			const duration = 2 * time.Second
			const numWorkers = 10
			
			ctx := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(numWorkers * 4) // 4 types of workers

			// Reader workers
			for i := 0; i < numWorkers; i++ {
				go func() {
					defer wg.Done()
					for {
						select {
						case <-ctx:
							return
						default:
							manager.GetReservedCPUs()
							manager.IsReserved(0)
							manager.IsExclusive(1)
						}
					}
				}()
			}

			// Container addition workers
			for i := 0; i < numWorkers; i++ {
				go func(id int) {
					defer wg.Done()
					counter := 0
					for {
						select {
						case <-ctx:
							return
						default:
							containerID := fmt.Sprintf("add-worker-%d-%d", id, counter)
							manager.mu.Lock()
							manager.byCID[containerID] = &ContainerInfo{
								ID:   containerID,
								Mode: ModeShared,
								CPUs: []int{counter % 4},
							}
							manager.mu.Unlock()
							counter++
						}
					}
				}(i)
			}

			// Container removal workers
			for i := 0; i < numWorkers; i++ {
				go func(id int) {
					defer wg.Done()
					counter := 0
					for {
						select {
						case <-ctx:
							return
						default:
							containerID := fmt.Sprintf("add-worker-%d-%d", id, counter)
							manager.RemoveContainer(containerID, alloc)
							counter++
							time.Sleep(time.Microsecond)
						}
					}
				}(i)
			}

			// Synchronization workers
			pods := []*api.PodSandbox{{Id: "sync-pod"}}
			containers := []*api.Container{{Id: "sync-container", PodSandboxId: "sync-pod"}}
			for i := 0; i < numWorkers; i++ {
				go func() {
					defer wg.Done()
					for {
						select {
						case <-ctx:
							return
						default:
							manager.Synchronize(pods, containers, alloc)
							time.Sleep(time.Millisecond)
						}
					}
				}()
			}

			// Run for specified duration
			time.Sleep(duration)
			close(ctx)

			// Wait for all workers to stop
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				// Verify state is still accessible and consistent
				reserved := manager.GetReservedCPUs()
				// Should not panic - reserved can be empty slice which is still valid
				_ = reserved
				
				// Final synchronization should work
				_, err := manager.Synchronize(pods, containers, alloc)
				Expect(err).ToNot(HaveOccurred())
			case <-time.After(5 * time.Second):
				Fail("Mixed concurrent operations cleanup timed out")
			}
		})

		It("should handle race conditions in reference counting", func() {
			const numGoroutines = 100
			var wg sync.WaitGroup
			wg.Add(numGoroutines * 2) // Add and remove workers

			// Workers that add annotated containers sharing the same CPU
			for i := 0; i < numGoroutines; i++ {
				go func(id int) {
					defer wg.Done()
					containerID := fmt.Sprintf("annotated-%d", id)
					
					manager.mu.Lock()
					manager.byCID[containerID] = &ContainerInfo{
						ID:   containerID,
						Mode: ModeAnnotated,
						CPUs: []int{0}, // All share CPU 0
					}
					manager.annotRef[0]++
					manager.mu.Unlock()
				}(i)
			}

			// Workers that remove annotated containers
			for i := 0; i < numGoroutines; i++ {
				go func(id int) {
					defer wg.Done()
					containerID := fmt.Sprintf("annotated-%d", id)
					
					// Small delay to let adds happen first
					time.Sleep(time.Microsecond)
					
					manager.RemoveContainer(containerID, alloc)
				}(i)
			}

			// Wait for completion
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				// Reference count should be consistent (likely 0 if all were removed)
				manager.mu.RLock()
				refCount := manager.annotRef[0]
				numContainers := 0
				for _, info := range manager.byCID {
					if info.Mode == ModeAnnotated && len(info.CPUs) > 0 && info.CPUs[0] == 0 {
						numContainers++
					}
				}
				manager.mu.RUnlock()
				
				// Reference count should match the number of remaining containers
				Expect(refCount).To(Equal(numContainers))
			case <-time.After(10 * time.Second):
				Fail("Reference counting race condition test timed out")
			}
		})
	})
})