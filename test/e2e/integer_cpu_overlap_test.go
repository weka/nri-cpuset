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

// parseCPUListForOverlapTest parses a CPU list string like "1-4,32-36" into a slice of integers
func parseCPUListForOverlapTest(cpuList string) ([]int, error) {
	cpus := make([]int, 0)
	parts := strings.Split(cpuList, ",")
	
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			// Handle range like "1-4"
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) != 2 {
				return nil, fmt.Errorf("invalid CPU range: %s", part)
			}
			start, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
			if err != nil {
				return nil, fmt.Errorf("invalid start CPU: %s", rangeParts[0])
			}
			end, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
			if err != nil {
				return nil, fmt.Errorf("invalid end CPU: %s", rangeParts[1])
			}
			for i := start; i <= end; i++ {
				cpus = append(cpus, i)
			}
		} else {
			// Handle single CPU like "0"
			cpu, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid CPU: %s", part)
			}
			cpus = append(cpus, cpu)
		}
	}
	
	return cpus, nil
}

// extractCPUList extracts the CPU list from /proc/self/status output
func extractCPUList(statusOutput string) (string, error) {
	lines := strings.Split(statusOutput, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Cpus_allowed_list:") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1]), nil
			}
		}
	}
	return "", fmt.Errorf("CPU list not found in status output")
}

// findCPUOverlaps returns the overlapping CPUs between two CPU lists
func findCPUOverlaps(cpus1, cpus2 []int) []int {
	cpuSet1 := make(map[int]struct{})
	for _, cpu := range cpus1 {
		cpuSet1[cpu] = struct{}{}
	}
	
	var overlaps []int
	for _, cpu := range cpus2 {
		if _, exists := cpuSet1[cpu]; exists {
			overlaps = append(overlaps, cpu)
		}
	}
	
	return overlaps
}

var _ = Describe("Integer Container CPU Exclusivity Bug Reproduction", Label("e2e", "parallel"), func() {
	Context("When multiple integer containers run on the same node", func() {
		AfterEach(func() {
			cleanupAllPodsConditional()
		})

		It("should NOT assign overlapping CPUs to integer containers - CRITICAL BUG TEST", func() {
			By("Creating first integer container")
			resources1 := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			}

			pod1 := createTestPod("integer-container-1", nil, resources1)
			createdPod1, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for first integer container to be running")
			waitForPodRunning(createdPod1.Name)

			By("Getting CPU assignment for first integer container")
			var pod1CPUs []int
			var pod1CPUList string
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPod1.Name)
				if err != nil {
					GinkgoWriter.Printf("Error getting CPU set for pod1: %v\n", err)
					return false
				}
				
				cpuList, err := extractCPUList(output)
				if err != nil {
					GinkgoWriter.Printf("Error extracting CPU list from pod1 output: %v\n", err)
					return false
				}
				
				cpus, err := parseCPUListForOverlapTest(cpuList)
				if err != nil {
					GinkgoWriter.Printf("Error parsing CPU list for pod1: %v\n", err)
					return false
				}
				
				pod1CPUs = cpus
				pod1CPUList = cpuList
				GinkgoWriter.Printf("Pod1 CPUs: %v (from list: %s)\n", cpus, cpuList)
				return len(cpus) == 4 // Should have exactly 4 CPUs
			}, timeout, interval).Should(BeTrue(), "First pod should have exactly 4 CPUs")

			By("Creating second integer container")
			resources2 := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("3"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("3"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			pod2 := createTestPod("integer-container-2", nil, resources2)
			createdPod2, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for second integer container to be running")
			waitForPodRunning(createdPod2.Name)

			By("Getting CPU assignment for second integer container")
			var pod2CPUs []int
			var pod2CPUList string
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPod2.Name)
				if err != nil {
					GinkgoWriter.Printf("Error getting CPU set for pod2: %v\n", err)
					return false
				}
				
				cpuList, err := extractCPUList(output)
				if err != nil {
					GinkgoWriter.Printf("Error extracting CPU list from pod2 output: %v\n", err)
					return false
				}
				
				cpus, err := parseCPUListForOverlapTest(cpuList)
				if err != nil {
					GinkgoWriter.Printf("Error parsing CPU list for pod2: %v\n", err)
					return false
				}
				
				pod2CPUs = cpus
				pod2CPUList = cpuList
				GinkgoWriter.Printf("Pod2 CPUs: %v (from list: %s)\n", cpus, cpuList)
				return len(cpus) == 3 // Should have exactly 3 CPUs
			}, timeout, interval).Should(BeTrue(), "Second pod should have exactly 3 CPUs")

			By("Verifying CPU exclusivity - NO OVERLAPS ALLOWED")
			overlaps := findCPUOverlaps(pod1CPUs, pod2CPUs)
			
			if len(overlaps) > 0 {
				// Log detailed information about the bug
				GinkgoWriter.Printf("CRITICAL BUG DETECTED!\n")
				GinkgoWriter.Printf("Pod1 (%s) CPUs: %v (list: %s)\n", createdPod1.Name, pod1CPUs, pod1CPUList)
				GinkgoWriter.Printf("Pod2 (%s) CPUs: %v (list: %s)\n", createdPod2.Name, pod2CPUs, pod2CPUList)
				GinkgoWriter.Printf("Overlapping CPUs: %v\n", overlaps)
				GinkgoWriter.Printf("This is a SEVERE CPU exclusivity violation!\n")
			}
			
			Expect(overlaps).To(BeEmpty(), 
				fmt.Sprintf("INTEGER CONTAINERS MUST HAVE EXCLUSIVE CPU ACCESS! Found overlapping CPUs: %v\n"+
					"Pod1 (%s): %v\n"+
					"Pod2 (%s): %v\n"+
					"This violates the fundamental exclusivity guarantee for integer containers.",
					overlaps, createdPod1.Name, pod1CPUs, createdPod2.Name, pod2CPUs))
		})

		It("should detect CPU overlap in multi-container scenario similar to numa-1 cluster", func() {
			By("Creating client container (similar to numa-test-client)")
			clientResources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("5"), // Similar to client pods
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("5"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			}

			clientPod := createTestPod("test-client", nil, clientResources)
			createdClientPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, clientPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Creating compute container (similar to numa-test-compute)")
			computeResources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("12"), // Similar to compute pods
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("12"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			}

			computePod := createTestPod("test-compute", nil, computeResources)
			createdComputePod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, computePod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Creating drive container (similar to numa-test-drive)")
			driveResources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("7"), // Similar to drive pods
					corev1.ResourceMemory: resource.MustParse("6Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("7"),
					corev1.ResourceMemory: resource.MustParse("6Gi"),
				},
			}

			drivePod := createTestPod("test-drive", nil, driveResources)
			createdDrivePod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, drivePod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for all containers to be running")
			waitForPodRunning(createdClientPod.Name)
			waitForPodRunning(createdComputePod.Name)
			waitForPodRunning(createdDrivePod.Name)

			By("Collecting CPU assignments from all containers")
			containers := map[string][]int{
				"client":  {},
				"compute": {},
				"drive":   {},
			}
			
			pods := []*corev1.Pod{createdClientPod, createdComputePod, createdDrivePod}
			names := []string{"client", "compute", "drive"}
			
			for i, pod := range pods {
				name := names[i]
				Eventually(func() bool {
					output, err := getPodCPUSet(pod.Name)
					if err != nil {
						return false
					}
					
					cpuList, err := extractCPUList(output)
					if err != nil {
						return false
					}
					
					cpus, err := parseCPUListForOverlapTest(cpuList)
					if err != nil {
						return false
					}
					
					containers[name] = cpus
					GinkgoWriter.Printf("%s container CPUs: %v\n", name, cpus)
					return len(cpus) > 0
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("%s container should have CPU assignment", name))
			}

			By("Verifying no CPU overlaps between any containers")
			var allOverlaps []string
			
			// Check client vs compute
			overlaps := findCPUOverlaps(containers["client"], containers["compute"])
			if len(overlaps) > 0 {
				msg := fmt.Sprintf("client-compute overlap: %v", overlaps)
				allOverlaps = append(allOverlaps, msg)
				GinkgoWriter.Printf("BUG: %s\n", msg)
			}
			
			// Check client vs drive
			overlaps = findCPUOverlaps(containers["client"], containers["drive"])
			if len(overlaps) > 0 {
				msg := fmt.Sprintf("client-drive overlap: %v", overlaps)
				allOverlaps = append(allOverlaps, msg)
				GinkgoWriter.Printf("BUG: %s\n", msg)
			}
			
			// Check compute vs drive
			overlaps = findCPUOverlaps(containers["compute"], containers["drive"])
			if len(overlaps) > 0 {
				msg := fmt.Sprintf("compute-drive overlap: %v", overlaps)
				allOverlaps = append(allOverlaps, msg)
				GinkgoWriter.Printf("BUG: %s\n", msg)
			}
			
			if len(allOverlaps) > 0 {
				GinkgoWriter.Printf("CRITICAL BUGS DETECTED:\n")
				GinkgoWriter.Printf("Client CPUs: %v\n", containers["client"])
				GinkgoWriter.Printf("Compute CPUs: %v\n", containers["compute"])
				GinkgoWriter.Printf("Drive CPUs: %v\n", containers["drive"])
				for _, overlap := range allOverlaps {
					GinkgoWriter.Printf("- %s\n", overlap)
				}
			}
			
			Expect(allOverlaps).To(BeEmpty(), 
				fmt.Sprintf("All integer containers must have exclusive CPU access! Found overlaps: %v\n"+
					"Client: %v\n"+
					"Compute: %v\n"+
					"Drive: %v",
					allOverlaps, containers["client"], containers["compute"], containers["drive"]))
		})
	})
})