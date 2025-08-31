package container_test

import (
	"github.com/containerd/nri/pkg/api"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/weka/nri-cpuset/pkg/container"
)

var _ = Describe("Container Classification", func() {
	Describe("HasIntegerSemantics", func() {
		It("should return true for containers with matching requests and limits", func() {
			c := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000}, // 2 CPUs limit
							Period: &api.OptionalUInt64{Value: 100000},
							Shares: &api.OptionalUInt64{Value: 2048}, // 2 CPUs request (2 * 1024)
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			Expect(container.HasIntegerSemantics(c)).To(BeTrue())
		})

		It("should return false for containers with mismatched requests and limits", func() {
			c := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000}, // 2 CPUs limit
							Period: &api.OptionalUInt64{Value: 100000},
							Shares: &api.OptionalUInt64{Value: 1024}, // 1 CPU request (mismatched!)
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			Expect(container.HasIntegerSemantics(c)).To(BeFalse())
		})

		It("should return false for containers with fractional CPU limits", func() {
			c := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 150000}, // 1.5 CPUs (fractional)
							Period: &api.OptionalUInt64{Value: 100000},
							Shares: &api.OptionalUInt64{Value: 1536}, // 1.5 CPUs request
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			Expect(container.HasIntegerSemantics(c)).To(BeFalse())
		})

		It("should return false for containers without CPU shares (requests)", func() {
			c := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000}, // 2 CPUs limit
							Period: &api.OptionalUInt64{Value: 100000},
							// No Shares set - missing requests
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024},
						},
					},
				},
			}
			Expect(container.HasIntegerSemantics(c)).To(BeFalse())
		})

		It("should return false for containers without resources", func() {
			c := &api.Container{}
			Expect(container.HasIntegerSemantics(c)).To(BeFalse())
		})
	})

	Describe("DetermineContainerMode", func() {
		It("should detect annotated containers", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					"weka.io/cores-ids": "0,1",
				},
			}
			c := &api.Container{}
			mode := container.DetermineContainerMode(pod, c)
			Expect(mode).To(Equal("annotated"))
		})

		It("should detect integer containers", func() {
			pod := &api.PodSandbox{}
			c := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000}, // 2 CPUs limit
							Period: &api.OptionalUInt64{Value: 100000},
							Shares: &api.OptionalUInt64{Value: 2048}, // 2 CPUs request (2 * 1024)
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024}, // 1GB
						},
					},
				},
			}
			mode := container.DetermineContainerMode(pod, c)
			Expect(mode).To(Equal("integer"))
		})

		It("should detect shared containers with mismatched requests and limits", func() {
			pod := &api.PodSandbox{}
			c := &api.Container{
				Linux: &api.LinuxContainer{
					Resources: &api.LinuxResources{
						Cpu: &api.LinuxCPU{
							Quota:  &api.OptionalInt64{Value: 200000}, // 2 CPUs limit
							Period: &api.OptionalUInt64{Value: 100000},
							Shares: &api.OptionalUInt64{Value: 1024}, // 1 CPU request (mismatched!)
						},
						Memory: &api.LinuxMemory{
							Limit: &api.OptionalInt64{Value: 1024 * 1024 * 1024}, // 1GB
						},
					},
				},
			}
			mode := container.DetermineContainerMode(pod, c)
			Expect(mode).To(Equal("shared"))
		})

		It("should default to shared containers", func() {
			pod := &api.PodSandbox{}
			c := &api.Container{}
			mode := container.DetermineContainerMode(pod, c)
			Expect(mode).To(Equal("shared"))
		})
	})

	Describe("GetForbiddenCPUs", func() {
		It("should return empty slice when no annotations", func() {
			pod := &api.PodSandbox{}
			forbidden := container.GetForbiddenCPUs(pod)
			Expect(forbidden).To(BeEmpty())
		})

		It("should return CPUs when cores-ids annotation is present (treated as forbidden for integer pods)", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					"weka.io/cores-ids": "0,1",
				},
			}
			forbidden := container.GetForbiddenCPUs(pod)
			Expect(forbidden).To(ConsistOf(0, 1))
		})

		It("should return empty slice when forbid-core-ids annotation is empty", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					"weka.io/forbid-core-ids": "",
				},
			}
			forbidden := container.GetForbiddenCPUs(pod)
			Expect(forbidden).To(BeEmpty())
		})

		It("should parse simple CPU list", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					"weka.io/forbid-core-ids": "1,3,5",
				},
			}
			forbidden := container.GetForbiddenCPUs(pod)
			Expect(forbidden).To(Equal([]int{1, 3, 5}))
		})

		It("should parse CPU ranges", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					"weka.io/forbid-core-ids": "0-2,6",
				},
			}
			forbidden := container.GetForbiddenCPUs(pod)
			Expect(forbidden).To(Equal([]int{0, 1, 2, 6}))
		})

		It("should return empty slice for invalid CPU list format", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					"weka.io/forbid-core-ids": "invalid-format",
				},
			}
			forbidden := container.GetForbiddenCPUs(pod)
			Expect(forbidden).To(BeEmpty())
		})

		It("should handle complex CPU ranges and lists", func() {
			pod := &api.PodSandbox{
				Annotations: map[string]string{
					"weka.io/forbid-core-ids": "0,2-4,7",
				},
			}
			forbidden := container.GetForbiddenCPUs(pod)
			Expect(forbidden).To(Equal([]int{0, 2, 3, 4, 7}))
		})
	})
})
