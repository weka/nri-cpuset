package e2e

import (
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Aggressive Race Condition Tests", Label("e2e", "parallel"), func() {
	Context("When stress testing concurrent integer container creation", func() {
		AfterEach(func() {
			cleanupAllPodsConditional()
		})

		It("should handle simultaneous integer container creation without CPU overlap", func() {
			By("Creating multiple integer containers simultaneously")
			
			// Create 5 integer containers with varying CPU requirements
			podConfigs := []struct{
				name string
				cpus int
			}{
				{"race-client", 5},
				{"race-compute", 12}, 
				{"race-drive", 7},
				{"race-backend", 4},
				{"race-frontend", 3},
			}
			
			var wg sync.WaitGroup
			var createdPods []*corev1.Pod
			var podCreationMutex sync.Mutex
			
			// Create all pods simultaneously to trigger race conditions
			for i, config := range podConfigs {
				wg.Add(1)
				go func(index int, cfg struct{name string; cpus int}) {
					defer wg.Done()
					
					resources := &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", cfg.cpus)),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", cfg.cpus)),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					}

					pod := createTestPod(cfg.name, nil, resources)
					
					// Add small random delay to simulate real-world timing variations
					time.Sleep(time.Duration(index*50) * time.Millisecond)
					
					createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
					if err != nil {
						GinkgoWriter.Printf("Failed to create pod %s: %v\n", cfg.name, err)
						return
					}
					
					podCreationMutex.Lock()
					createdPods = append(createdPods, createdPod)
					podCreationMutex.Unlock()
					
					GinkgoWriter.Printf("Created pod %s requesting %d CPUs\n", cfg.name, cfg.cpus)
				}(i, config)
			}
			
			// Wait for all pod creation attempts to complete
			wg.Wait()
			
			By("Waiting for all successfully created pods to be running")
			var runningPods []*corev1.Pod
			
			for _, pod := range createdPods {
				if pod != nil {
					// Wait for pod to be running (or fail)
					Eventually(func() bool {
						updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
						if err != nil {
							return false
						}
						return updatedPod.Status.Phase == corev1.PodRunning || 
							   updatedPod.Status.Phase == corev1.PodFailed ||
							   updatedPod.Status.Phase == corev1.PodSucceeded
					}, timeout, interval).Should(BeTrue(), fmt.Sprintf("Pod %s should reach a final state", pod.Name))
					
					// Check if pod is actually running
					updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
					if err == nil && updatedPod.Status.Phase == corev1.PodRunning {
						runningPods = append(runningPods, updatedPod)
						GinkgoWriter.Printf("Pod %s is running\n", updatedPod.Name)
					} else {
						GinkgoWriter.Printf("Pod %s failed to run (phase: %s)\n", pod.Name, updatedPod.Status.Phase)
					}
				}
			}
			
			By("Collecting CPU assignments from all running pods")
			podCPUs := make(map[string][]int)
			
			for _, pod := range runningPods {
				Eventually(func() bool {
					output, err := getPodCPUSet(pod.Name)
					if err != nil {
						GinkgoWriter.Printf("Error getting CPU set for %s: %v\n", pod.Name, err)
						return false
					}
					
					cpuList, err := extractCPUList(output)
					if err != nil {
						GinkgoWriter.Printf("Error extracting CPU list for %s: %v\n", pod.Name, err)
						return false
					}
					
					cpus, err := parseCPUListForOverlapTest(cpuList)
					if err != nil {
						GinkgoWriter.Printf("Error parsing CPU list for %s: %v\n", pod.Name, err)
						return false
					}
					
					podCPUs[pod.Name] = cpus
					GinkgoWriter.Printf("Pod %s has CPUs: %v\n", pod.Name, cpus)
					return len(cpus) > 0
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("Pod %s should have CPU assignment", pod.Name))
			}
			
			By("Verifying NO CPU overlaps between any running integer containers")
			var allOverlaps []string
			podNames := make([]string, 0, len(podCPUs))
			for name := range podCPUs {
				podNames = append(podNames, name)
			}
			
			// Check all pairs for overlaps
			for i := 0; i < len(podNames); i++ {
				for j := i+1; j < len(podNames); j++ {
					pod1Name := podNames[i]
					pod2Name := podNames[j]
					
					overlaps := findCPUOverlaps(podCPUs[pod1Name], podCPUs[pod2Name])
					if len(overlaps) > 0 {
						msg := fmt.Sprintf("%s↔%s overlap: %v", pod1Name, pod2Name, overlaps)
						allOverlaps = append(allOverlaps, msg)
						GinkgoWriter.Printf("CRITICAL BUG: %s\n", msg)
					}
				}
			}
			
			if len(allOverlaps) > 0 {
				GinkgoWriter.Printf("\n=== CRITICAL RACE CONDITION BUG DETECTED ===\n")
				for podName, cpus := range podCPUs {
					GinkgoWriter.Printf("%s: %v\n", podName, cpus)
				}
				GinkgoWriter.Printf("Overlaps found:\n")
				for _, overlap := range allOverlaps {
					GinkgoWriter.Printf("- %s\n", overlap)
				}
				GinkgoWriter.Printf("===========================================\n")
			}
			
			Expect(allOverlaps).To(BeEmpty(), 
				fmt.Sprintf("Race condition caused CPU overlaps in simultaneous integer container creation: %v", allOverlaps))
		})

		It("should handle rapid pod creation and deletion cycles", func() {
			By("Creating and deleting integer pods in rapid succession")
			
			// Create initial pod
			resources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"), 
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			}
			
			// Rapid create-delete-create cycle to trigger state inconsistencies
			for cycle := 0; cycle < 3; cycle++ {
				podName := fmt.Sprintf("rapid-cycle-%d", cycle)
				
				// Create pod
				pod := createTestPod(podName, nil, resources)
				_, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				GinkgoWriter.Printf("Created rapid pod %s\n", podName)
				
				// Wait briefly for it to start
				time.Sleep(2 * time.Second)
				
				// Immediately delete it
				err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, podName, metav1.DeleteOptions{
					GracePeriodSeconds: &[]int64{0}[0], // Immediate deletion
				})
				if err != nil {
					GinkgoWriter.Printf("Warning: Failed to delete pod %s: %v\n", podName, err)
				} else {
					GinkgoWriter.Printf("Deleted rapid pod %s\n", podName)
				}
				
				// Small delay before next cycle
				time.Sleep(500 * time.Millisecond)
			}
			
			// Now create multiple pods after the rapid cycles
			var finalPods []*corev1.Pod
			for i := 0; i < 3; i++ {
				podName := fmt.Sprintf("post-rapid-%d", i)
				pod := createTestPod(podName, nil, resources)
				createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				finalPods = append(finalPods, createdPod)
				GinkgoWriter.Printf("Created final pod %s\n", podName)
				
				// Small delay between creations
				time.Sleep(100 * time.Millisecond)
			}
			
			By("Waiting for final pods to be running")
			for _, pod := range finalPods {
				waitForPodRunning(pod.Name)
			}
			
			By("Checking CPU assignments for overlaps after rapid cycles")
			podCPUs := make(map[string][]int)
			
			for _, pod := range finalPods {
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
					
					podCPUs[pod.Name] = cpus
					GinkgoWriter.Printf("Final pod %s has CPUs: %v\n", pod.Name, cpus)
					return len(cpus) > 0
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("Pod %s should have CPU assignment", pod.Name))
			}
			
			// Check for overlaps
			var overlaps []string
			podNames := make([]string, 0, len(podCPUs))
			for name := range podCPUs {
				podNames = append(podNames, name)
			}
			
			for i := 0; i < len(podNames); i++ {
				for j := i+1; j < len(podNames); j++ {
					pod1Name := podNames[i]
					pod2Name := podNames[j]
					
					cpuOverlaps := findCPUOverlaps(podCPUs[pod1Name], podCPUs[pod2Name])
					if len(cpuOverlaps) > 0 {
						msg := fmt.Sprintf("%s↔%s: %v", pod1Name, pod2Name, cpuOverlaps)
						overlaps = append(overlaps, msg)
					}
				}
			}
			
			Expect(overlaps).To(BeEmpty(), 
				fmt.Sprintf("Rapid creation/deletion cycles caused CPU overlaps: %v", overlaps))
		})

		It("should handle large integer containers with exact CPU overlap potential", func() {
			By("Creating integer containers with sizes that would naturally overlap in numa-1 scenario")
			
			// Mimic the exact numa-1 cluster scenario
			podConfigs := []struct{
				name string
				cpus int
			}{
				{"mimic-client", 5},  // Should get CPUs like 1-4,32-36 (5 CPUs)
				{"mimic-drive", 7},   // Should get different CPUs, not 1-6,32-38 (7 CPUs)
			}
			
			var createdPods []*corev1.Pod
			
			// Create pods with slight delay to simulate realistic timing
			for i, config := range podConfigs {
				resources := &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", config.cpus)),
						corev1.ResourceMemory: resource.MustParse("4Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", config.cpus)),
						corev1.ResourceMemory: resource.MustParse("4Gi"),
					},
				}

				pod := createTestPod(config.name, nil, resources)
				createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				
				createdPods = append(createdPods, createdPod)
				GinkgoWriter.Printf("Created mimic pod %s requesting %d CPUs\n", config.name, config.cpus)
				
				// Add small delay between pod creations
				if i == 0 {
					time.Sleep(100 * time.Millisecond)
				}
			}
			
			By("Waiting for mimic pods to be running")
			for _, pod := range createdPods {
				waitForPodRunning(pod.Name)
			}
			
			By("Getting CPU assignments and checking for numa-1 style overlap")
			var clientCPUs, driveCPUs []int
			
			// Get client CPUs
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPods[0].Name)
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
				
				clientCPUs = cpus
				GinkgoWriter.Printf("Mimic client CPUs: %v\n", cpus)
				return len(cpus) == 5
			}, timeout, interval).Should(BeTrue(), "Mimic client should have 5 CPUs")
			
			// Get drive CPUs  
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPods[1].Name)
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
				
				driveCPUs = cpus
				GinkgoWriter.Printf("Mimic drive CPUs: %v\n", cpus)
				return len(cpus) == 7
			}, timeout, interval).Should(BeTrue(), "Mimic drive should have 7 CPUs")
			
			// Check for the exact overlap pattern seen in numa-1
			overlaps := findCPUOverlaps(clientCPUs, driveCPUs)
			
			if len(overlaps) > 0 {
				GinkgoWriter.Printf("REPRODUCED NUMA-1 BUG!\n")
				GinkgoWriter.Printf("Client CPUs: %v\n", clientCPUs)
				GinkgoWriter.Printf("Drive CPUs: %v\n", driveCPUs)
				GinkgoWriter.Printf("Overlaps: %v\n", overlaps)
				
				// Additional analysis
				GinkgoWriter.Printf("This matches the numa-1 pattern where:\n")
				GinkgoWriter.Printf("- client had: 1-4,32-36\n")
				GinkgoWriter.Printf("- drive had: 1-6,32-38\n")
				GinkgoWriter.Printf("- overlap was: 1-4,32-36\n")
			}
			
			Expect(overlaps).To(BeEmpty(), 
				fmt.Sprintf("CRITICAL: Reproduced numa-1 CPU overlap bug! Client=%v, Drive=%v, Overlaps=%v", 
					clientCPUs, driveCPUs, overlaps))
		})
	})
})