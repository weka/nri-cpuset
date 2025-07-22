package e2e

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Live CPU Reallocation Features", Label("e2e", "sequential"), func() {
	// const (
	// 	WekaAnnotation = "weka.io/cores-ids"
	// )

	BeforeEach(func() {
		// Test setup if needed
	})

	AfterEach(func() {
		// Clean up any test pods created
		cleanupAllPodsConditional()
	})

	Describe("Integer container live reallocation", func() {
		It("should reallocate integer containers when annotated pod creates conflicts", func() {
			By("Creating an integer pod to establish initial CPU allocation")
			integerResources := &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			}

			integerPod := createTestPod("integer-realloc-test", nil, integerResources)
			createdIntegerPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdIntegerPod.Name)

			By("Waiting for integer pod to be fully allocated and recorded")
			time.Sleep(5 * time.Second)

			By("Getting the CPUs allocated to the integer container")
			var integerCPUs string
			Eventually(func() string {
				output, err := getPodCPUSet(createdIntegerPod.Name)
				if err != nil {
					return ""
				}
				integerCPUs = output
				return output
			}, timeout, interval).ShouldNot(BeEmpty(), "Integer pod should have allocated CPUs")

			By("Creating an annotated pod that conflicts with integer pod's CPUs")
			// Parse the first CPU from the integer pod's allocation to create a conflict
			var conflictCPU string
			if strings.Contains(integerCPUs, "Cpus_allowed_list:") {
				// Extract the first CPU from the output
				lines := strings.Split(integerCPUs, "\n")
				for _, line := range lines {
					if strings.Contains(line, "Cpus_allowed_list:") {
						parts := strings.Fields(line)
						if len(parts) > 1 {
							cpuList := parts[1]
							if strings.Contains(cpuList, ",") {
								conflictCPU = strings.Split(cpuList, ",")[0]
							} else if strings.Contains(cpuList, "-") {
								conflictCPU = strings.Split(cpuList, "-")[0]
							} else {
								conflictCPU = cpuList
							}
							break
						}
					}
				}
			}
			Expect(conflictCPU).ToNot(BeEmpty(), "Should be able to extract a CPU from integer pod allocation")
			
			// Log the initial state for debugging
			GinkgoWriter.Printf("Integer pod CPU allocation: %s\n", integerCPUs)
			GinkgoWriter.Printf("Will create annotated pod requesting CPU: %s\n", conflictCPU)

			annotatedPod := createTestPod("annotated-realloc-conflict", map[string]string{
				"weka.io/cores-ids": conflictCPU,
			}, nil)

			createdAnnotatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for annotated pod to be running")
			waitForPodRunning(createdAnnotatedPod.Name)

			By("Verifying the annotated pod got its requested CPU")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdAnnotatedPod.Name)
				if err != nil {
					return false
				}
				return strings.Contains(output, conflictCPU)
			}, timeout, interval).Should(BeTrue(), "Annotated pod should have its requested CPU")

			By("Verifying the integer container was reallocated to different CPUs")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdIntegerPod.Name)
				if err != nil {
					GinkgoWriter.Printf("Error getting CPU set for integer pod: %v\n", err)
					return false
				}
				
				// The integer pod should still be running but with different CPUs
				// It should have 2 CPUs total but not include the conflicting CPU
				lines := strings.Split(output, "\n")
				for _, line := range lines {
					if strings.Contains(line, "Cpus_allowed_list:") {
						parts := strings.Fields(line)
						if len(parts) > 1 {
							currentCPUs := parts[1]
							GinkgoWriter.Printf("Integer pod current CPUs: %s, conflict CPU: %s\n", currentCPUs, conflictCPU)
							// Should not contain the conflicting CPU
							doesNotContainConflict := !strings.Contains(currentCPUs, conflictCPU)
							if doesNotContainConflict {
								GinkgoWriter.Printf("SUCCESS: Integer pod successfully reallocated away from conflicting CPU\n")
							}
							return doesNotContainConflict
						}
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(), "Integer pod should be reallocated away from conflicting CPU")
		})

		It("should handle multiple conflicting CPUs correctly", func() {
			Skip("Multi-CPU conflict test - requires implementation verification")

			// Test framework:
			// 1. Create an integer container with 4 CPUs
			// 2. Create an annotated pod that conflicts with two of the integer container's CPUs
			// 3. Verify that the integer container was reallocated away from conflicting CPUs
			// 4. Verify that both containers have their expected CPU counts
		})

		It("should fail annotated pod creation when reallocation is impossible", func() {
			Skip("Impossible reallocation test - requires implementation verification")

			// Test framework:
			// 1. Create multiple integer containers to consume most CPUs
			// 2. Attempt to create an annotated pod that would require impossible reallocation
			// 3. Verify that the annotated pod fails to schedule due to insufficient resources
		})
	})

	Describe("Sibling core allocation", func() {
		It("should prefer allocating sibling cores for integer containers", func() {
			Skip("Sibling allocation test - requires specific hardware topology knowledge")

			// Test framework:
			// 1. Discover the actual hyperthreading topology of the test system
			// 2. Create integer containers requesting 2 cores
			// 3. Verify that sibling cores are preferred when available
			// 4. This is hardware-dependent and might not be suitable for automated testing
		})

		It("should complete partial cores before fragmenting new ones", func() {
			Skip("Partial core completion test - requires specific hardware topology knowledge")

			// Test framework:
			// 1. Partially allocate a physical core (1 thread from a hyperthreaded core)
			// 2. Create a new integer container requesting 2 cores
			// 3. Verify that it completes the partial core first
			// 4. This is hardware-dependent and might not be suitable for automated testing
		})
	})

	Describe("Annotated container sharing", func() {
		It("should allow multiple annotated containers to share the same CPUs", func() {
			Skip("Live reallocation test - requires implementation verification")
		})

		It("should gracefully handle resource conflicts", func() {
			Skip("Live reallocation test - requires implementation verification")
		})
	})

	Describe("Error handling and edge cases", func() {
		It("should handle invalid CPU annotations gracefully", func() {
			Skip("Live reallocation test - requires implementation verification")
		})
	})
})

// Live CPU reallocation test framework - helper functions removed to fix linter warnings
// These can be re-added when the test implementation is completed
