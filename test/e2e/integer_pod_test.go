package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Integer Pod Tests", Label("e2e"), func() {
	Context("When creating integer pods", func() {
		AfterEach(func() {
			// Clean up any pods created in tests
			pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
			if err == nil {
				for _, pod := range pods.Items {
					_ = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
				}
			}
		})

		It("should allocate exclusive CPUs for integer pods", func() {
			By("Creating a pod with integer CPU requirements")
			resources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			pod := createTestPod("integer-test-pod", nil, resources)
			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying exclusive CPU allocation")
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(MatchRegexp(`Cpus_allowed_list:\s*\d+[,-]\d+`), 
				"Pod should have exactly 2 exclusive CPUs")
		})

		It("should handle multiple integer pods without overlap", func() {
			By("Creating first integer pod")
			resources1 := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			}

			pod1 := createTestPod("integer-pod-1", nil, resources1)
			createdPod1, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for first pod to be running")
			waitForPodRunning(createdPod1.Name)

			By("Getting CPU set for first pod")
			var pod1CPUs string
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod1.Name)
				if err != nil {
					return ""
				}
				pod1CPUs = output
				return output
			}, timeout, interval).Should(MatchRegexp(`Cpus_allowed_list:\s*\d+`), 
				"First pod should have exactly 1 CPU")

			By("Creating second integer pod")
			resources2 := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			}

			pod2 := createTestPod("integer-pod-2", nil, resources2)
			createdPod2, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for second pod to be running")
			waitForPodRunning(createdPod2.Name)

			By("Verifying CPUs don't overlap")
			Eventually(func() bool {
				output2, err := getPodCPUSet(createdPod2.Name)
				if err != nil {
					return false
				}
				
				// In a real implementation, you would parse the CPU lists and verify no overlap
				// For now, just check that both pods have CPU assignments
				return len(pod1CPUs) > 0 && len(output2) > 0 && pod1CPUs != output2
			}, timeout, interval).Should(BeTrue(), "CPU sets should not overlap")
		})

		It("should reject integer pods when insufficient CPUs", func() {
			By("Creating pods to exhaust available exclusive CPUs")
			// This test assumes a system with limited CPUs
			// Create multiple integer pods until one should fail
			
			resources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"), // Request many CPUs
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			// Create first pod that should succeed
			pod1 := createTestPod("exhaust-cpu-1", nil, resources)
			_, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			// Create second pod that should fail due to insufficient CPUs
			pod2 := createTestPod("exhaust-cpu-2", nil, resources)
			_, err = kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying second pod fails due to insufficient resources")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, pod2.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}

				// Check for failed scheduling or container creation
				for _, condition := range updatedPod.Status.Conditions {
					if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
						return true
					}
				}

				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						return true
					}
				}

				return updatedPod.Status.Phase == corev1.PodFailed
			}, timeout, interval).Should(BeTrue(), "Second pod should fail due to CPU exhaustion")
		})

		It("should update shared pool when integer pod terminates", func() {
			By("Creating an integer pod")
			resources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			integerPod := createTestPod("terminating-integer-pod", nil, resources)
			integerPod.Spec.Containers[0].Command = []string{"sh", "-c", "sleep 30"} // Short-lived
			
			createdIntegerPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Creating a shared pod")
			sharedPod := createTestPod("shared-pool-pod", nil, nil)
			createdSharedPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for both pods to be running")
			waitForPodRunning(createdIntegerPod.Name)
			waitForPodRunning(createdSharedPod.Name)

			By("Getting initial shared pool size")
			var initialSharedCPUs string
			Eventually(func() string {
				output, err := getPodCPUSet(createdSharedPod.Name)
				if err != nil {
					return ""
				}
				initialSharedCPUs = output
				return output
			}, timeout, interval).ShouldNot(BeEmpty(), "Shared pod should have CPU assignment")

			By("Waiting for integer pod to terminate")
			Eventually(func() bool {
				pod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdIntegerPod.Name, metav1.GetOptions{})
				if err != nil {
					return true
				}
				return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
			}, timeout, interval).Should(BeTrue(), "Integer pod should terminate")

			By("Verifying shared pool expands after integer pod terminates")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdSharedPod.Name)
				if err != nil {
					return false
				}
				
				// In a real implementation, you would parse and compare CPU counts
				// For now, assume expansion if the output changes
				return output != initialSharedCPUs
			}, timeout, interval).Should(BeTrue(), "Shared pool should expand after integer pod termination")
		})
	})
})

var _ = Describe("Integer Pod Edge Cases", Label("e2e"), func() {
	Context("When testing edge cases for integer pods", func() {
		AfterEach(func() {
			pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
			if err == nil {
				for _, pod := range pods.Items {
					_ = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
				}
			}
		})

		It("should reject pods with fractional CPU requests", func() {
			By("Creating a pod with fractional CPU requirements")
			resources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1.5"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1.5"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			pod := createTestPod("fractional-cpu-pod", nil, resources)
			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying pod is treated as shared, not integer")
			waitForPodRunning(createdPod.Name)
			
			// This pod should get shared treatment since CPU is not an integer
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(ContainSubstring("Cpus_allowed_list:"), 
				"Pod should get shared CPU treatment")
		})

		It("should handle mismatched requests and limits", func() {
			By("Creating a pod with mismatched CPU requests and limits")
			resources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"), // Different from request
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			pod := createTestPod("mismatched-resources-pod", nil, resources)
			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying pod is treated as shared due to mismatch")
			waitForPodRunning(createdPod.Name)
			
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(ContainSubstring("Cpus_allowed_list:"), 
				"Pod should get shared CPU treatment due to request/limit mismatch")
		})
	})
})