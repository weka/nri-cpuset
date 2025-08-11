package e2e

import (
	"fmt"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// extractCPUListFromProcStatus extracts the Cpus_allowed_list value from /proc/self/status output
func extractCPUListFromProcStatus(procOutput string) string {
	lines := strings.Split(procOutput, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Cpus_allowed_list:") {
			parts := strings.Fields(line)
			if len(parts) > 1 {
				return parts[1] // Return the CPU list part (e.g., "0-3,5,7")
			}
		}
	}
	return ""
}

// Helper function to parse CPU list format like "2-3,33-34" into individual CPU integers
func parseCPUListToInts(cpuList string) []int {
	var allCPUs []int

	// Handle comma-separated list that may contain ranges
	cpuParts := strings.Split(cpuList, ",")
	for _, part := range cpuParts {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			// Handle range format like "0-3" or "34-35"
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) == 2 {
				start, err1 := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
				end, err2 := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
				if err1 == nil && err2 == nil {
					for i := start; i <= end; i++ {
						allCPUs = append(allCPUs, i)
					}
				}
			}
		} else {
			// Single CPU number
			if cpu, err := strconv.Atoi(part); err == nil {
				allCPUs = append(allCPUs, cpu)
			}
		}
	}
	return allCPUs
}

var _ = Describe("Forbidden CPU Tests", Label("e2e", "parallel"), func() {
	Context("When creating pods with forbidden CPU annotation", func() {
		AfterEach(func() {
			// Clean up any pods created in tests (conditional on failure)
			cleanupAllPodsConditional()
		})

		It("should avoid forbidden CPUs for integer pods", func() {
			By("Creating an integer pod with forbidden CPU annotation")
			pod := createTestPod("forbidden-integer-pod", map[string]string{
				"weka.io/forbid-core-ids": "1,3,5",
			}, &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			})

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying CPUs avoid forbidden cores")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return false
				}

				// Check that none of the forbidden CPUs (1,3,5) are allocated
				forbiddenCPUs := []string{"1", "3", "5"}
				for _, forbiddenCPU := range forbiddenCPUs {
					if strings.Contains(output, forbiddenCPU) {
						// Need to be careful - CPU 1 could be part of "10", "11", etc.
						// So we need to check for exact matches or comma-separated values
						cpuList := strings.Split(output, ",")
						for _, cpu := range cpuList {
							cpu = strings.TrimSpace(cpu)
							if cpu == forbiddenCPU {
								return false // Found a forbidden CPU
							}
						}
					}
				}
				return true // No forbidden CPUs found
			}, timeout, interval).Should(BeTrue(), "Pod should not be assigned forbidden CPUs 1,3,5")
		})

		It("should avoid forbidden CPUs for shared pods", func() {
			By("Creating a shared pod with forbidden CPU annotation")
			pod := createTestPod("forbidden-shared-pod", map[string]string{
				"weka.io/forbid-core-ids": "0,2,4",
			}, &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"), // Fractional CPU
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
			})

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying CPUs avoid forbidden cores")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return false
				}

				// Check that none of the forbidden CPUs (0,2,4) are in the shared pool
				forbiddenCPUs := []string{"0", "2", "4"}
				for _, forbiddenCPU := range forbiddenCPUs {
					cpuList := strings.Split(output, ",")
					for _, cpu := range cpuList {
						cpu = strings.TrimSpace(cpu)
						if cpu == forbiddenCPU {
							return false // Found a forbidden CPU
						}
					}
				}
				return true // No forbidden CPUs found
			}, timeout, interval).Should(BeTrue(), "Shared pod should not have access to forbidden CPUs 0,2,4")
		})

		It("should handle both reserved and forbidden CPUs correctly", func() {
			By("Creating an annotated pod to reserve some CPUs")
			annotatedPod := createTestPod("reserved-cpu-pod", map[string]string{
				"weka.io/cores-ids": "0,1",
			}, nil)

			createdAnnotatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for annotated pod to be running")
			waitForPodRunning(createdAnnotatedPod.Name)

			By("Creating an integer pod with forbidden CPUs annotation")
			integerPod := createTestPod("forbidden-and-reserved-pod", map[string]string{
				"weka.io/forbid-core-ids": "2,3",
			}, &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			})

			createdIntegerPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdIntegerPod.Name)

			By("Verifying integer pod avoids both reserved and forbidden CPUs")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdIntegerPod.Name)
				if err != nil {
					return false
				}

				// Should not have reserved CPUs (0,1) or forbidden CPUs (2,3)
				excludedCPUs := []string{"0", "1", "2", "3"}
				cpuList := strings.Split(output, ",")
				for _, cpu := range cpuList {
					cpu = strings.TrimSpace(cpu)
					for _, excluded := range excludedCPUs {
						if cpu == excluded {
							return false // Found an excluded CPU
						}
					}
				}
				return true // No excluded CPUs found
			}, timeout, interval).Should(BeTrue(), "Integer pod should avoid both reserved (0,1) and forbidden (2,3) CPUs")
		})

		It("should handle forbidden CPU ranges correctly", func() {
			By("Creating an integer pod with forbidden CPU range annotation")
			pod := createTestPod("forbidden-range-pod", map[string]string{
				"weka.io/forbid-core-ids": "0-2,6",
			}, &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			})

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying CPUs avoid forbidden range")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return false
				}

				// Check that none of the forbidden CPUs (0,1,2,6) are allocated
				forbiddenCPUs := []string{"0", "1", "2", "6"}
				cpuList := strings.Split(output, ",")
				for _, cpu := range cpuList {
					cpu = strings.TrimSpace(cpu)
					for _, forbidden := range forbiddenCPUs {
						if cpu == forbidden {
							return false // Found a forbidden CPU
						}
					}
				}
				return true // No forbidden CPUs found
			}, timeout, interval).Should(BeTrue(), "Pod should not be assigned forbidden CPUs from range 0-2,6")
		})

		It("should gracefully handle invalid forbidden CPU annotation", func() {
			By("Creating a pod with invalid forbidden CPU annotation")
			pod := createTestPod("invalid-forbidden-pod", map[string]string{
				"weka.io/forbid-core-ids": "invalid-cpu-format",
			}, &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			})

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying pod still gets CPU allocation despite invalid annotation")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return false
				}
				// Should still get some CPU allocation (invalid annotation should be ignored)
				return len(strings.TrimSpace(output)) > 0
			}, timeout, interval).Should(BeTrue(), "Pod should still get CPU allocation when forbidden annotation is invalid")
		})

		It("should not affect annotated pods (they get exact CPUs)", func() {
			By("Creating an annotated pod that requests forbidden CPUs")
			pod := createTestPod("annotated-forbidden-pod", map[string]string{
				"weka.io/cores-ids":       "1,3", // Request specific CPUs
				"weka.io/forbid-core-ids": "1,3", // Same CPUs are forbidden for others
			}, nil)

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying annotated pod gets exactly the requested CPUs")
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(MatchRegexp("1.*3|3.*1"), "Annotated pod should get exactly CPUs 1,3 regardless of forbidden annotation")
		})

		It("should create different CPU sets for shared pods with different forbidden CPU patterns", func() {
			By("Creating first shared pod with no forbidden CPUs annotation")
			pod1 := createTestPod("shared-no-forbidden", map[string]string{}, &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"), // Fractional CPU = shared
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
			})

			createdPod1, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Creating second shared pod with forbidden CPUs 0,2,4")
			pod2 := createTestPod("shared-forbidden-024", map[string]string{
				"weka.io/forbid-core-ids": "0,2,4",
			}, &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"), // Fractional CPU = shared
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
			})

			createdPod2, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Creating third shared pod with forbidden CPUs 1,3,5")
			pod3 := createTestPod("shared-forbidden-135", map[string]string{
				"weka.io/forbid-core-ids": "1,3,5",
			}, &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"), // Fractional CPU = shared
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
			})

			createdPod3, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod3, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for all pods to be running")
			waitForPodRunning(createdPod1.Name)
			waitForPodRunning(createdPod2.Name)
			waitForPodRunning(createdPod3.Name)

			var cpuSet1, cpuSet2, cpuSet3 string

			By("Getting CPU sets for all three pods")
			Eventually(func() bool {
				var err1, err2, err3 error
				cpuSet1, err1 = getPodCPUSet(createdPod1.Name)
				cpuSet2, err2 = getPodCPUSet(createdPod2.Name)
				cpuSet3, err3 = getPodCPUSet(createdPod3.Name)

				return err1 == nil && err2 == nil && err3 == nil &&
					len(cpuSet1) > 0 && len(cpuSet2) > 0 && len(cpuSet3) > 0
			}, timeout, interval).Should(BeTrue(), "All pods should have CPU sets assigned")

			By("Verifying that pod with no forbidden annotation has access to full shared pool")
			// Pod1 should have the largest CPU set (full shared pool minus any reserved by integer containers)
			cpuListRaw1 := extractCPUListFromProcStatus(cpuSet1)
			cpuList1 := parseCPUListToInts(cpuListRaw1)

			// Debug output to help diagnose failures
			fmt.Printf("DEBUG: Pod1 raw CPU output: %s\n", cpuSet1)
			fmt.Printf("DEBUG: Pod1 extracted CPU list: %s\n", cpuListRaw1)
			fmt.Printf("DEBUG: Pod1 parsed CPU list: %v\n", cpuList1)

			Expect(len(cpuList1)).To(BeNumerically(">", 0), "Pod without forbidden annotation should have some CPUs")

			By("Verifying that pod2 avoids forbidden CPUs 0,2,4")
			cpuListRaw2 := extractCPUListFromProcStatus(cpuSet2)
			cpuList2 := parseCPUListToInts(cpuListRaw2)
			forbiddenCPUs2 := []int{0, 2, 4}

			// Debug output to help diagnose failures
			fmt.Printf("DEBUG: Pod2 raw CPU output: %s\n", cpuSet2)
			fmt.Printf("DEBUG: Pod2 extracted CPU list: %s\n", cpuListRaw2)
			fmt.Printf("DEBUG: Pod2 parsed CPU list: %v\n", cpuList2)
			fmt.Printf("DEBUG: Pod2 forbidden CPUs: %v\n", forbiddenCPUs2)

			// Check for forbidden CPUs with detailed error messages
			for _, forbiddenCPU := range forbiddenCPUs2 {
				found := false
				for _, cpu := range cpuList2 {
					if cpu == forbiddenCPU {
						found = true
						break
					}
				}
				Expect(found).To(BeFalse(), fmt.Sprintf("Pod2 should not have forbidden CPU %d. Got CPU list: %v", forbiddenCPU, cpuList2))
			}

			By("Verifying that pod3 avoids forbidden CPUs 1,3,5")
			cpuListRaw3 := extractCPUListFromProcStatus(cpuSet3)
			cpuList3 := parseCPUListToInts(cpuListRaw3)
			forbiddenCPUs3 := []int{1, 3, 5}

			// Debug output to help diagnose failures
			fmt.Printf("DEBUG: Pod3 raw CPU output: %s\n", cpuSet3)
			fmt.Printf("DEBUG: Pod3 extracted CPU list: %s\n", cpuListRaw3)
			fmt.Printf("DEBUG: Pod3 parsed CPU list: %v\n", cpuList3)
			fmt.Printf("DEBUG: Pod3 forbidden CPUs: %v\n", forbiddenCPUs3)

			// Check for forbidden CPUs with detailed error messages
			for _, forbiddenCPU := range forbiddenCPUs3 {
				found := false
				for _, cpu := range cpuList3 {
					if cpu == forbiddenCPU {
						found = true
						break
					}
				}
				Expect(found).To(BeFalse(), fmt.Sprintf("Pod3 should not have forbidden CPU %d. Got CPU list: %v", forbiddenCPU, cpuList3))
			}

			By("Verifying that each pod has a different effective CPU set")
			// Due to different forbidden patterns, the effective available CPU sets should be different
			// Pod1: Full shared pool
			// Pod2: Shared pool minus {0,2,4}
			// Pod3: Shared pool minus {1,3,5}

			// Check that pod2 doesn't have the forbidden CPUs it should avoid
			hasAnyForbidden2 := false
			for _, cpu := range cpuList2 {
				if cpu == 0 || cpu == 2 || cpu == 4 {
					hasAnyForbidden2 = true
					break
				}
			}
			Expect(hasAnyForbidden2).To(BeFalse(), "Pod2 should not have any of CPUs 0,2,4")

			// Check that pod3 doesn't have the forbidden CPUs it should avoid
			hasAnyForbidden3 := false
			for _, cpu := range cpuList3 {
				if cpu == 1 || cpu == 3 || cpu == 5 {
					hasAnyForbidden3 = true
					break
				}
			}
			Expect(hasAnyForbidden3).To(BeFalse(), "Pod3 should not have any of CPUs 1,3,5")

			By("Verifying logical CPU set relationships")
			// Pod1 (no restrictions) should potentially have CPUs that pod2 and pod3 cannot have
			// This demonstrates that forbidden CPU logic is working per-pod
			pod1HasSome024 := false
			for _, cpu := range cpuList1 {
				if cpu == 0 || cpu == 2 || cpu == 4 {
					pod1HasSome024 = true
					break
				}
			}

			pod1HasSome135 := false
			for _, cpu := range cpuList1 {
				if cpu == 1 || cpu == 3 || cpu == 5 {
					pod1HasSome135 = true
					break
				}
			}

			// At least one of these should be true, showing pod1 has access to CPUs others don't
			Expect(pod1HasSome024 || pod1HasSome135).To(BeTrue(),
				"Pod without forbidden annotation should have access to CPUs that other pods cannot access")
		})
	})
})
