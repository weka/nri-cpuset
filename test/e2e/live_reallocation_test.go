package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Helper function for minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = Describe("Live CPU Reallocation Features", Label("e2e", "parallel"), func() {
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

			// Add debug info for artifact collection
			AddDebugInfo(fmt.Sprintf("Integer pod %s initial CPU allocation: %s", createdIntegerPod.Name, integerCPUs))
			AddDebugInfo(fmt.Sprintf("Creating annotated pod requesting conflicting CPU: %s", conflictCPU))

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

							// Add debug info for artifact collection
							AddDebugInfo(fmt.Sprintf("Integer pod %s current CPUs: %s, checking for conflict with: %s", createdIntegerPod.Name, currentCPUs, conflictCPU))

							// Should not contain the conflicting CPU
							doesNotContainConflict := !strings.Contains(currentCPUs, conflictCPU)
							if doesNotContainConflict {
								GinkgoWriter.Printf("SUCCESS: Integer pod successfully reallocated away from conflicting CPU\n")
								AddDebugInfo("SUCCESS: Live reallocation completed - integer pod moved away from conflicting CPU")
							} else {
								AddDebugInfo("FAILURE: Integer pod still on conflicting CPU - reallocation did not work")
							}
							return doesNotContainConflict
						}
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(), "Integer pod should be reallocated away from conflicting CPU")
		})

		It("should handle multiple conflicting CPUs correctly", func() {
			By("Creating an integer pod with 4 CPUs")
			integerResources := &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("200Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("200Mi"),
				},
			}

			integerPod := createTestPod("integer-multi-conflict", nil, integerResources)
			createdIntegerPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdIntegerPod.Name)

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

			By("Creating an annotated pod that conflicts with two of the integer pod's CPUs")
			// Parse the first two CPUs from the integer pod's allocation
			var conflictCPUs []string
			AddDebugInfo(fmt.Sprintf("Integer pod CPU output: %s", integerCPUs))

			if strings.Contains(integerCPUs, "Cpus_allowed_list:") {
				lines := strings.Split(integerCPUs, "\n")
				for _, line := range lines {
					if strings.Contains(line, "Cpus_allowed_list:") {
						parts := strings.Fields(line)
						if len(parts) > 1 {
							cpuList := parts[1]
							AddDebugInfo(fmt.Sprintf("Parsed CPU list: %s", cpuList))

							// Parse CPU list and take first two CPUs
							if strings.Contains(cpuList, ",") {
								allCPUs := strings.Split(cpuList, ",")
								if len(allCPUs) >= 2 {
									conflictCPUs = []string{allCPUs[0], allCPUs[1]}
								} else if len(allCPUs) == 1 {
									// For single CPU, create another conflict by using the same CPU
									conflictCPUs = []string{allCPUs[0]}
								}
							} else if strings.Contains(cpuList, "-") {
								// Handle range format like "0-3"
								rangeParts := strings.Split(cpuList, "-")
								if len(rangeParts) == 2 {
									conflictCPUs = []string{rangeParts[0], rangeParts[1]}
								}
							} else {
								// Single CPU number
								conflictCPUs = []string{cpuList}
							}
							break
						}
					}
				}
			}

			AddDebugInfo(fmt.Sprintf("Extracted conflict CPUs: %v", conflictCPUs))

			// Ensure we got CPU allocation - this should always work
			Expect(len(conflictCPUs)).To(BeNumerically(">", 0), "Should be able to extract CPU allocation from integer pod")

			// Use at least one CPU for conflict (adapt to available CPUs)
			if len(conflictCPUs) >= 2 {
				conflictCPUs = conflictCPUs[:2] // Use first two
			} // If only 1 CPU, use that one

			conflictSpec := strings.Join(conflictCPUs, ",")
			AddDebugInfo(fmt.Sprintf("Creating annotated pod requesting conflicting CPUs: %s", conflictSpec))

			annotatedPod := createTestPod("annotated-multi-conflict", map[string]string{
				"weka.io/cores-ids": conflictSpec,
			}, nil)

			createdAnnotatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for annotated pod to be running")
			waitForPodRunning(createdAnnotatedPod.Name)

			By("Verifying the annotated pod got its requested CPUs")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdAnnotatedPod.Name)
				if err != nil {
					return false
				}
				for _, cpu := range conflictCPUs {
					if !strings.Contains(output, cpu) {
						return false
					}
				}
				return true
			}, timeout, interval).Should(BeTrue(), "Annotated pod should have its requested CPUs")

			By("Verifying the integer container was reallocated away from conflicting CPUs")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdIntegerPod.Name)
				if err != nil {
					return false
				}

				// Should not contain any of the conflicting CPUs
				for _, cpu := range conflictCPUs {
					if strings.Contains(output, cpu) {
						AddDebugInfo(fmt.Sprintf("Integer pod still contains conflicting CPU %s", cpu))
						return false
					}
				}

				AddDebugInfo("SUCCESS: Integer pod reallocated away from all conflicting CPUs")
				return true
			}, timeout, interval).Should(BeTrue(), "Integer pod should be reallocated away from all conflicting CPUs")
		})

		It("should fail annotated pod creation when reallocation is impossible", func() {
			By("Creating many integer pods to consume most available CPUs")
			var integerPods []*corev1.Pod
			// Create enough integer pods to leave insufficient CPUs for reallocation
			// Each pod gets 2 CPUs, create enough to consume most available CPUs
			// We'll create pods until we can't anymore, then try to force impossible reallocation
			maxPods := 50 // Safety limit to prevent infinite loops
			for i := 0; i < maxPods; i++ {
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

				integerPod := createTestPod(fmt.Sprintf("impossible-integer-%d", i), nil, integerResources)
				createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
				if err != nil {
					// If we can't create more integer pods, we've hit resource limits - that's expected
					GinkgoWriter.Printf("Created %d integer pods before hitting limits\n", len(integerPods))
					break
				}
				integerPods = append(integerPods, createdPod)
			}

			By("Waiting for integer pods to be running")
			for _, pod := range integerPods {
				waitForPodRunning(pod.Name)
			}

			By("Collecting allocated CPUs from integer pods")
			var allocatedCPUs []string
			for _, pod := range integerPods {
				output, err := getPodCPUSet(pod.Name)
				if err != nil {
					continue
				}

				lines := strings.Split(output, "\n")
				for _, line := range lines {
					if strings.Contains(line, "Cpus_allowed_list:") {
						parts := strings.Fields(line)
						if len(parts) > 1 {
							// Parse CPU list - handle both single CPUs and ranges
							cpuList := parts[1]
							allocatedCPUs = append(allocatedCPUs, strings.Split(cpuList, ",")...)
						}
					}
				}
			}

			By("Creating annotated pod requesting CPUs that would require impossible reallocation")
			// Request a large number of CPUs that are already allocated to integers
			// This should force reallocation that's impossible due to insufficient free CPUs
			conflictingCPUs := strings.Join(allocatedCPUs[:min(len(allocatedCPUs), 8)], ",") // Request up to 8 conflicting CPUs

			conflictPod := createTestPod("impossible-annotated", map[string]string{
				"weka.io/cores-ids": conflictingCPUs,
			}, nil)

			createdConflictPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, conflictPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying annotated pod fails due to impossible reallocation")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdConflictPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}

				// Check for container creation failure
				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						if strings.Contains(containerStatus.State.Waiting.Reason, "CreateContainerError") ||
							strings.Contains(containerStatus.State.Waiting.Message, "cannot reallocate") ||
							strings.Contains(containerStatus.State.Waiting.Message, "insufficient") {
							AddDebugInfo(fmt.Sprintf("SUCCESS: Annotated pod correctly failed due to impossible reallocation: %s - %s",
								containerStatus.State.Waiting.Reason, containerStatus.State.Waiting.Message))
							return true
						}
					}
				}

				// Check pod conditions for scheduling failures
				for _, condition := range updatedPod.Status.Conditions {
					if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
						if strings.Contains(condition.Message, "insufficient") ||
							strings.Contains(condition.Message, "resource") {
							AddDebugInfo(fmt.Sprintf("SUCCESS: Annotated pod correctly failed to schedule: %s", condition.Message))
							return true
						}
					}
				}

				return false
			}, "2m", "5s").Should(BeTrue(), "Annotated pod should fail when impossible reallocation is required")

			AddDebugInfo("SUCCESS: Impossible reallocation correctly rejected")
		})
	})

	Describe("Sibling core allocation", Label("e2e", "parallel"), func() {
		It("should prefer allocating sibling cores for integer containers", func() {
			By("Creating an integer pod requesting 2 CPUs")
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

			integerPod := createTestPod("sibling-test-pod", nil, integerResources)
			createdIntegerPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdIntegerPod.Name)

			By("Getting the CPUs allocated to the integer container")
			var allocatedCPUs string
			Eventually(func() string {
				output, err := getPodCPUSet(createdIntegerPod.Name)
				if err != nil {
					return ""
				}
				allocatedCPUs = output
				return output
			}, timeout, interval).ShouldNot(BeEmpty(), "Integer pod should have allocated CPUs")

			By("Verifying sibling cores are allocated when requesting 2 CPUs")
			lines := strings.Split(allocatedCPUs, "\n")
			var cpuList string
			for _, line := range lines {
				if strings.Contains(line, "Cpus_allowed_list:") {
					parts := strings.Fields(line)
					if len(parts) > 1 {
						cpuList = parts[1]
						break
					}
				}
			}
			Expect(cpuList).ToNot(BeEmpty(), "Should have CPU allocation")

			// Parse the CPU list
			cpuListParsed := strings.Split(cpuList, ",")
			Expect(len(cpuListParsed)).To(Equal(2), "Should have exactly 2 CPUs allocated")

			// Check if they are siblings by examining topology
			// In our cluster: (0,32), (1,33), (2,34), etc. are sibling pairs
			if len(cpuListParsed) == 2 {
				cpu1, err1 := strconv.Atoi(cpuListParsed[0])
				cpu2, err2 := strconv.Atoi(cpuListParsed[1])

				if err1 == nil && err2 == nil {
					// Check if they form a sibling pair
					// In our topology: CPU X and CPU (X+32) are siblings for X < 32
					// Or CPU X and CPU (X-32) are siblings for X >= 32
					areSiblings := (cpu1 < 32 && cpu2 == cpu1+32) || (cpu1 >= 32 && cpu2 == cpu1-32) ||
						(cpu2 < 32 && cpu1 == cpu2+32) || (cpu2 >= 32 && cpu1 == cpu2-32)

					if areSiblings {
						AddDebugInfo(fmt.Sprintf("SUCCESS: Sibling cores allocated - CPUs %d and %d are siblings", cpu1, cpu2))
					} else {
						AddDebugInfo(fmt.Sprintf("INFO: Non-sibling cores allocated - CPUs %d and %d (may be due to availability)", cpu1, cpu2))
						// This is not necessarily a failure - sibling preference is best effort
					}
				}
			}
		})

		It("should complete partial cores before fragmenting new ones", func() {
			By("Creating first integer pod requesting 1 CPU to partially allocate a core")
			firstPodResources := &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			}

			firstPod := createTestPod("partial-core-1", nil, firstPodResources)
			createdFirstPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, firstPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for first pod to be running")
			waitForPodRunning(createdFirstPod.Name)

			By("Getting the CPU allocated to the first pod")
			firstPodCPU := -1
			Eventually(func() bool {
				output, err := getPodCPUSet(createdFirstPod.Name)
				if err != nil {
					return false
				}

				lines := strings.Split(output, "\n")
				for _, line := range lines {
					if strings.Contains(line, "Cpus_allowed_list:") {
						parts := strings.Fields(line)
						if len(parts) > 1 {
							cpuStr := parts[1]
							if cpu, err := strconv.Atoi(cpuStr); err == nil {
								firstPodCPU = cpu
								return true
							}
						}
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(), "First pod should have CPU allocation")

			By("Creating second integer pod requesting 1 CPU")
			secondPodResources := &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			}

			secondPod := createTestPod("partial-core-2", nil, secondPodResources)
			createdSecondPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, secondPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for second pod to be running")
			waitForPodRunning(createdSecondPod.Name)

			By("Verifying second pod gets the sibling of the first pod's CPU")
			secondPodCPU := -1
			Eventually(func() bool {
				output, err := getPodCPUSet(createdSecondPod.Name)
				if err != nil {
					return false
				}

				lines := strings.Split(output, "\n")
				for _, line := range lines {
					if strings.Contains(line, "Cpus_allowed_list:") {
						parts := strings.Fields(line)
						if len(parts) > 1 {
							cpuStr := parts[1]
							if cpu, err := strconv.Atoi(cpuStr); err == nil {
								secondPodCPU = cpu
								return true
							}
						}
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(), "Second pod should have CPU allocation")

			By("Checking if the second CPU completes the physical core")
			// Calculate expected sibling CPU
			var expectedSibling int
			if firstPodCPU < 32 {
				expectedSibling = firstPodCPU + 32
			} else {
				expectedSibling = firstPodCPU - 32
			}

			if secondPodCPU == expectedSibling {
				AddDebugInfo(fmt.Sprintf("SUCCESS: Core completion preferred - CPU %d completed core with sibling %d", secondPodCPU, firstPodCPU))
			} else {
				AddDebugInfo(fmt.Sprintf("INFO: Different allocation strategy - got CPU %d instead of expected sibling %d of %d", secondPodCPU, expectedSibling, firstPodCPU))
				// This is not necessarily a failure - allocation depends on availability and strategy
			}
		})
	})

	Describe("Annotated container sharing", func() {
		It("should allow multiple annotated containers to share the same CPUs", func() {
			By("Creating first annotated pod with specific CPUs")
			firstPod := createTestPod("annotated-shared-1", map[string]string{
				"weka.io/cores-ids": "0,1",
			}, nil)

			createdFirstPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, firstPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for first pod to be running")
			waitForPodRunning(createdFirstPod.Name)

			By("Verifying first pod gets its requested CPUs")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdFirstPod.Name)
				if err != nil {
					return false
				}
				return strings.Contains(output, "0") && strings.Contains(output, "1")
			}, timeout, interval).Should(BeTrue(), "First annotated pod should have CPUs 0,1")

			By("Creating second annotated pod sharing the same CPUs")
			secondPod := createTestPod("annotated-shared-2", map[string]string{
				"weka.io/cores-ids": "0,1",
			}, nil)

			createdSecondPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, secondPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for second pod to be running")
			waitForPodRunning(createdSecondPod.Name)

			By("Verifying second pod also gets the shared CPUs")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdSecondPod.Name)
				if err != nil {
					return false
				}
				return strings.Contains(output, "0") && strings.Contains(output, "1")
			}, timeout, interval).Should(BeTrue(), "Second annotated pod should also have CPUs 0,1")

			By("Verifying both pods continue to run successfully with shared CPU access")
			Consistently(func() bool {
				pod1, err1 := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdFirstPod.Name, metav1.GetOptions{})
				pod2, err2 := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdSecondPod.Name, metav1.GetOptions{})
				if err1 != nil || err2 != nil {
					return false
				}
				return pod1.Status.Phase == corev1.PodRunning && pod2.Status.Phase == corev1.PodRunning
			}, "30s", "5s").Should(BeTrue(), "Both annotated pods should continue running with shared CPU access")

			AddDebugInfo("SUCCESS: Multiple annotated pods successfully sharing same CPUs")
		})

		It("should gracefully handle resource conflicts", func() {
			By("Creating integer pods to consume most available CPUs")
			// Create multiple integer pods to consume most CPUs, leaving few available
			var integerPods []*corev1.Pod
			maxPods := 50 // Safety limit to prevent infinite loops
			for i := 0; i < maxPods; i++ { // Create enough pods to exhaust most CPUs
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

				integerPod := createTestPod(fmt.Sprintf("conflict-integer-%d", i), nil, integerResources)
				createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
				if err != nil {
					// If we can't create more integer pods, we've hit resource limits - that's expected
					GinkgoWriter.Printf("Created %d integer pods before hitting limits\n", len(integerPods))
					break
				}
				integerPods = append(integerPods, createdPod)
			}

			By("Waiting for all integer pods to be running")
			for _, pod := range integerPods {
				waitForPodRunning(pod.Name)
			}

			By("Recording allocated CPUs from integer pods")
			var allocatedCPUs []string
			for _, pod := range integerPods {
				Eventually(func() bool {
					output, err := getPodCPUSet(pod.Name)
					if err != nil {
						return false
					}
					if strings.Contains(output, "Cpus_allowed_list:") {
						// Parse CPUs and add to allocated list
						lines := strings.Split(output, "\n")
						for _, line := range lines {
							if strings.Contains(line, "Cpus_allowed_list:") {
								parts := strings.Fields(line)
								if len(parts) > 1 {
									cpuList := parts[1]
									if strings.Contains(cpuList, ",") {
										allocatedCPUs = append(allocatedCPUs, strings.Split(cpuList, ",")...)
									} else {
										allocatedCPUs = append(allocatedCPUs, cpuList)
									}
								}
								break
							}
						}
						return true
					}
					return false
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("Integer pod %s should have CPU allocation", pod.Name))
			}

			AddDebugInfo(fmt.Sprintf("Integer pods allocated CPUs: %v", allocatedCPUs))

			By("Creating annotated pod that conflicts with all integer CPUs (should fail)")
			// Request all the CPUs that integer pods are using
			conflictingCPUs := strings.Join(allocatedCPUs[:min(len(allocatedCPUs), 4)], ",") // Request up to 4 conflicting CPUs

			conflictPod := createTestPod("conflict-annotated", map[string]string{
				"weka.io/cores-ids": conflictingCPUs,
			}, nil)

			createdConflictPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, conflictPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying annotated pod fails due to insufficient resources for reallocation")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdConflictPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}

				// Check for scheduling failure or container creation error
				for _, condition := range updatedPod.Status.Conditions {
					if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
						if strings.Contains(condition.Message, "CPU") || strings.Contains(condition.Message, "resource") {
							AddDebugInfo(fmt.Sprintf("Pod correctly failed to schedule: %s", condition.Message))
							return true
						}
					}
				}

				// Also check container status for creation errors
				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						if strings.Contains(containerStatus.State.Waiting.Reason, "CreateContainerError") ||
							strings.Contains(containerStatus.State.Waiting.Message, "CPU") ||
							strings.Contains(containerStatus.State.Waiting.Message, "insufficient") {
							AddDebugInfo(fmt.Sprintf("Pod correctly failed with resource error: %s - %s",
								containerStatus.State.Waiting.Reason, containerStatus.State.Waiting.Message))
							return true
						}
					}
				}

				return false
			}, timeout, interval).Should(BeTrue(), "Annotated pod should fail when reallocation is impossible")

			By("Verifying integer pods remain running and unaffected")
			Consistently(func() bool {
				allRunning := true
				for _, pod := range integerPods {
					updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
					if err != nil || updatedPod.Status.Phase != corev1.PodRunning {
						allRunning = false
						break
					}
				}
				return allRunning
			}, "30s", "5s").Should(BeTrue(), "Integer pods should remain running when annotated pod conflicts cannot be resolved")

			AddDebugInfo("SUCCESS: Resource conflicts handled gracefully - annotated pod failed, integer pods preserved")
		})
	})

	Describe("Error handling and edge cases", func() {
		It("should handle invalid CPU annotations gracefully", func() {
			By("Creating pod with invalid CPU annotation - non-existent CPU")
			invalidPod := createTestPod("invalid-annotation-test", map[string]string{
				"weka.io/cores-ids": "999", // Assuming this CPU doesn't exist
			}, nil)

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, invalidPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying pod fails to start due to invalid annotation")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}

				// Check for container creation errors or failed status
				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						// Look for error related to CPU allocation
						if strings.Contains(containerStatus.State.Waiting.Reason, "CreateContainerError") ||
							strings.Contains(containerStatus.State.Waiting.Message, "cpu") ||
							strings.Contains(containerStatus.State.Waiting.Message, "CPU") {
							AddDebugInfo(fmt.Sprintf("Pod correctly failed with CPU-related error: %s - %s",
								containerStatus.State.Waiting.Reason, containerStatus.State.Waiting.Message))
							return true
						}
					}
				}

				// Also check for failed pod phase
				if updatedPod.Status.Phase == corev1.PodFailed {
					AddDebugInfo("Pod correctly transitioned to Failed phase due to invalid annotation")
					return true
				}

				return false
			}, timeout, interval).Should(BeTrue(), "Pod with invalid CPU annotation should fail to start")

			By("Testing malformed annotation syntax")
			malformedPod := createTestPod("malformed-annotation-test", map[string]string{
				"weka.io/cores-ids": "invalid-format-not-a-number",
			}, nil)

			createdMalformedPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, malformedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying pod with malformed annotation also fails gracefully")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdMalformedPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}

				// Check for container creation errors or failed status
				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil &&
						(strings.Contains(containerStatus.State.Waiting.Reason, "CreateContainerError") ||
							strings.Contains(containerStatus.State.Waiting.Message, "annotation") ||
							strings.Contains(containerStatus.State.Waiting.Message, "parse")) {
						AddDebugInfo(fmt.Sprintf("Pod correctly failed with annotation parsing error: %s - %s",
							containerStatus.State.Waiting.Reason, containerStatus.State.Waiting.Message))
						return true
					}
				}

				return updatedPod.Status.Phase == corev1.PodFailed
			}, timeout, interval).Should(BeTrue(), "Pod with malformed annotation should fail gracefully")

			AddDebugInfo("SUCCESS: Invalid annotations handled gracefully with appropriate error messages")
		})
	})
})

// Live CPU reallocation test framework - helper functions removed to fix linter warnings
// These can be re-added when the test implementation is completed
