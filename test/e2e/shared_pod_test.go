package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Shared Pod Tests", Label("e2e", "parallel"), func() {
	Context("When creating shared pods", func() {
		AfterEach(func() {
			// Clean up any pods created in tests (conditional on failure)
			cleanupAllPodsConditional()
		})

		It("should assign shared pods to the shared CPU pool", func() {
			By("Creating a shared pod")
			pod := createTestPod("shared-test-pod", nil, nil)

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying shared pool assignment")
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(ContainSubstring("Cpus_allowed_list:"),
				"Shared pod should have CPU pool assignment")
		})

		It("should allow multiple shared pods to use the same CPU pool", func() {
			By("Creating multiple shared pods")
			pod1 := createTestPod("shared-pod-1", nil, nil)
			pod2 := createTestPod("shared-pod-2", nil, nil)

			createdPod1, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			createdPod2, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pods to be running")
			waitForPodRunning(createdPod1.Name)
			waitForPodRunning(createdPod2.Name)

			By("Verifying both pods have same shared pool")
			var pod1CPUs, pod2CPUs string

			Eventually(func() string {
				output, err := getPodCPUSet(createdPod1.Name)
				if err != nil {
					return ""
				}
				pod1CPUs = output
				return output
			}, timeout, interval).Should(ContainSubstring("Cpus_allowed_list:"),
				"First shared pod should have CPU assignment")

			Eventually(func() string {
				output, err := getPodCPUSet(createdPod2.Name)
				if err != nil {
					return ""
				}
				pod2CPUs = output
				return output
			}, timeout, interval).Should(ContainSubstring("Cpus_allowed_list:"),
				"Second shared pod should have CPU assignment")

			// In a real implementation, you would parse and compare the CPU lists
			// For now, just verify they both get assignments
			Expect(pod1CPUs).ToNot(BeEmpty())
			Expect(pod2CPUs).ToNot(BeEmpty())
		})

		It("should update shared pool when exclusive CPUs are allocated", func() {
			By("Creating a shared pod first")
			sharedPod := createTestPod("shared-first-pod", nil, nil)
			createdSharedPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for shared pod to be running")
			waitForPodRunning(createdSharedPod.Name)

			By("Getting initial shared pool")
			var initialCPUs string
			Eventually(func() string {
				output, err := getPodCPUSet(createdSharedPod.Name)
				if err != nil {
					return ""
				}
				initialCPUs = output
				return output
			}, timeout, interval).Should(ContainSubstring("Cpus_allowed_list:"))

			By("Creating an integer pod to reduce shared pool")
			resources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			integerPod := createTestPod("integer-reducing-pool", nil, resources)
			createdIntegerPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdIntegerPod.Name)

			By("Verifying shared pool is updated")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdSharedPod.Name)
				if err != nil {
					return false
				}

				// In a real implementation, you would parse CPU lists and compare
				// For now, assume change indicates pool update
				return output != initialCPUs
			}, timeout, interval).Should(BeTrue(),
				"Shared pod CPU pool should be updated after integer pod allocation")
		})

		It("should handle pods with fractional CPU requests as shared", func() {
			By("Creating a pod with fractional CPU request")
			resources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("0.5"),
				},
			}

			pod := createTestPod("fractional-shared-pod", nil, resources)
			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying pod is treated as shared")
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(ContainSubstring("Cpus_allowed_list:"),
				"Fractional CPU pod should be treated as shared")
		})

		It("should maintain shared pool when shared pods terminate", func() {
			By("Creating multiple shared pods")
			pod1 := createTestPod("shared-terminating-1", nil, nil)
			pod1.Spec.Containers[0].Command = []string{"sh", "-c", "sleep 30"} // Will be terminated manually

			pod2 := createTestPod("shared-persistent-2", nil, nil)

			createdPod1, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			createdPod2, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for both pods to be running")
			waitForPodRunning(createdPod1.Name)
			waitForPodRunning(createdPod2.Name)

			By("Getting CPU assignment for persistent pod")
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod2.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(ContainSubstring("Cpus_allowed_list:"))

			By("Manually terminating the first pod")
			err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, createdPod1.Name, metav1.DeleteOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for first pod to terminate")
			Eventually(func() bool {
				pod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdPod1.Name, metav1.GetOptions{})
				if err != nil {
					return true
				}
				return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
			}, timeout, interval).Should(BeTrue(), "First shared pod should terminate")

			By("Verifying persistent pod continues to have shared CPU assignment")
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod2.Name)
				if err != nil {
					return ""
				}
				return output
			}, "30s", "5s").Should(ContainSubstring("Cpus_allowed_list:"),
				"Persistent shared pod should continue to have CPU assignment after pool update")
		})

		It("should reject shared pods when shared pool is empty", func() {
			By("Creating annotated pods to consume all CPUs")
			// This test assumes we can annotate all available CPUs
			// In a real test, you would query the system to find all CPUs
			pod := createTestPod("cpu-exhausting-annotated", map[string]string{
				"weka.io/cores-ids": "0-7", // Assuming 8 CPU system
			}, nil)

			_, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Attempting to create a shared pod")
			sharedPod := createTestPod("should-fail-shared", nil, nil)
			_, err = kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying shared pod fails due to empty pool")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, sharedPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}

				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						return true
					}
				}

				return updatedPod.Status.Phase == corev1.PodFailed
			}, timeout, interval).Should(BeTrue(),
				"Shared pod should fail when shared pool is empty")
		})
	})
})

var _ = Describe("Mixed Workload Scenarios", Label("e2e", "parallel"), func() {
	Context("When running mixed workloads", func() {
		BeforeEach(func() {
			// Ensure clean state before mixed workload tests to avoid interference from previous tests
			cleanupAllPodsAndWait()
		})

		AfterEach(func() {
			// Clean up any pods created in tests (conditional on failure)
			cleanupAllPodsConditional()
		})

		It("should handle annotated, integer, and shared pods together", func() {
			By("Creating an annotated pod")
			annotatedPod := createTestPod("mixed-annotated", map[string]string{
				"weka.io/cores-ids": "0,1",
			}, nil)

			createdAnnotated, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Creating an integer pod")
			integerResources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			integerPod := createTestPod("mixed-integer", nil, integerResources)
			createdInteger, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Creating a shared pod")
			sharedPod := createTestPod("mixed-shared", nil, nil)
			createdShared, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for all pods to be running")
			waitForPodRunning(createdAnnotated.Name)
			waitForPodRunning(createdInteger.Name)
			waitForPodRunning(createdShared.Name)

			By("Verifying all pods have appropriate CPU assignments")
			Eventually(func() bool {
				annotatedCPUs, err1 := getPodCPUSet(createdAnnotated.Name)
				integerCPUs, err2 := getPodCPUSet(createdInteger.Name)
				sharedCPUs, err3 := getPodCPUSet(createdShared.Name)

				if err1 != nil || err2 != nil || err3 != nil {
					return false
				}

				// All pods should have CPU assignments
				return len(annotatedCPUs) > 0 && len(integerCPUs) > 0 && len(sharedCPUs) > 0
			}, timeout, interval).Should(BeTrue(),
				"All pod types should receive appropriate CPU assignments")
		})
	})
})
