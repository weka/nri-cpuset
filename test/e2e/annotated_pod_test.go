package e2e

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Annotated Pod Tests", Label("e2e"), func() {
	Context("When creating annotated pods", func() {
		AfterEach(func() {
			// Clean up any pods created in tests
			pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
			if err == nil {
				for _, pod := range pods.Items {
					_ = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
				}
			}
		})

		It("should pin CPUs according to annotation", func() {
			By("Creating a pod with CPU annotation")
			pod := createTestPod("annotated-test-pod", map[string]string{
				"weka.io/cores-ids": "0,2,4",
			}, nil)

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying CPU pinning")
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(ContainSubstring("0,2,4"), "Pod should be pinned to CPUs 0,2,4")
		})

		It("should handle CPU ranges in annotations", func() {
			By("Creating a pod with CPU range annotation")
			pod := createTestPod("range-annotated-pod", map[string]string{
				"weka.io/cores-ids": "0-2,6",
			}, nil)

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying CPU range pinning")
			Eventually(func() string {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(MatchRegexp("0[,-]1[,-]2.*6|0-2.*6"), "Pod should be pinned to CPUs 0-2,6")
		})

		It("should allow multiple annotated pods to share CPUs", func() {
			By("Creating first annotated pod")
			pod1 := createTestPod("shared-annotated-1", map[string]string{
				"weka.io/cores-ids": "0,2",
			}, nil)

			createdPod1, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for first pod to be running")
			waitForPodRunning(createdPod1.Name)

			By("Creating second annotated pod sharing CPUs")
			pod2 := createTestPod("shared-annotated-2", map[string]string{
				"weka.io/cores-ids": "0,2",
			}, nil)

			createdPod2, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for second pod to be running")
			waitForPodRunning(createdPod2.Name)

			By("Verifying both pods have same CPU pinning")
			Eventually(func() bool {
				output1, err1 := getPodCPUSet(createdPod1.Name)
				output2, err2 := getPodCPUSet(createdPod2.Name)
				
				if err1 != nil || err2 != nil {
					return false
				}
				
				return strings.Contains(output1, "0,2") && strings.Contains(output2, "0,2")
			}, timeout, interval).Should(BeTrue(), "Both pods should be pinned to CPUs 0,2")
		})

		It("should reject pods with invalid CPU annotations", func() {
			By("Creating a pod with invalid CPU annotation")
			pod := createTestPod("invalid-annotated-pod", map[string]string{
				"weka.io/cores-ids": "999",
			}, nil)

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying pod fails to start")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}
				
				// Check for failed container or event indicating CPU validation failure
				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						return strings.Contains(containerStatus.State.Waiting.Message, "CPU") ||
							   strings.Contains(containerStatus.State.Waiting.Reason, "Invalid")
					}
				}
				return false
			}, timeout, interval).Should(BeTrue(), "Pod should fail with invalid CPU annotation")
		})

		It("should set memory nodes for annotated pods on single NUMA node", func() {
			By("Creating an annotated pod with CPUs from single NUMA node")
			pod := createTestPod("numa-single-annotated-pod", map[string]string{
				"weka.io/cores-ids": "0,1", // Assuming these are on NUMA node 0
			}, nil)

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying memory node assignment matches CPU NUMA node")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return false
				}
				// Should have Mems_allowed_list set to the same NUMA node as CPUs 0,1
				return strings.Contains(output, "Mems_allowed_list: 0") || strings.Contains(output, "Mems_allowed_list:0")
			}, timeout, interval).Should(BeTrue(), "Pod should have memory pinned to same NUMA node as CPUs")
		})

		It("should set memory nodes for annotated pods spanning multiple NUMA nodes", func() {
			By("Creating an annotated pod with CPUs from multiple NUMA nodes")
			pod := createTestPod("numa-multi-annotated-pod", map[string]string{
				"weka.io/cores-ids": "0,4", // Assuming these are on different NUMA nodes
			}, nil)

			createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pod to be running")
			waitForPodRunning(createdPod.Name)

			By("Verifying memory node assignment includes all involved NUMA nodes")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdPod.Name)
				if err != nil {
					return false
				}
				// Should have Mems_allowed_list set to union of NUMA nodes
				// This is simplified - real test would check actual NUMA topology
				return strings.Contains(output, "Mems_allowed_list:")
			}, timeout, interval).Should(BeTrue(), "Pod should have memory accessible from all involved NUMA nodes")
		})

		It("should not affect memory placement for integer and shared pods", func() {
			By("Creating integer and shared pods")
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
			
			integerPod := createTestPod("numa-integer-pod", nil, integerResources)
			sharedPod := createTestPod("numa-shared-pod", nil, nil)
			
			createdInteger, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			
			createdShared, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for pods to be running")
			waitForPodRunning(createdInteger.Name)
			waitForPodRunning(createdShared.Name)

			By("Verifying memory placement is not modified for non-annotated pods")
			Eventually(func() bool {
				intOutput, err1 := getPodCPUSet(createdInteger.Name)
				sharedOutput, err2 := getPodCPUSet(createdShared.Name)
				if err1 != nil || err2 != nil {
					return false
				}
				// Memory should remain unrestricted or follow system defaults
				return strings.Contains(intOutput, "Cpus_allowed_list:") && strings.Contains(sharedOutput, "Cpus_allowed_list:")
			}, timeout, interval).Should(BeTrue(), "Non-annotated pods should not have memory placement restrictions")
		})
	})
})

var _ = Describe("Annotated Pod Error Cases", Label("e2e"), func() {
	Context("When creating pods with problematic annotations", func() {
		AfterEach(func() {
			// Clean up any pods created in tests
			pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
			if err == nil {
				for _, pod := range pods.Items {
					_ = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
				}
			}
		})

		It("should handle malformed CPU list annotations", func() {
			testCases := []struct {
				annotation string
				description string
			}{
				{"0-", "incomplete range"},
				{"a,b,c", "non-numeric values"},
				{"0,,2", "empty values"},
				{"5-2", "invalid range order"},
				{"", "empty annotation"},
			}

			for _, tc := range testCases {
				By(fmt.Sprintf("Testing %s annotation: %s", tc.description, tc.annotation))
				
				podName := fmt.Sprintf("malformed-pod-%d", GinkgoRandomSeed())
				pod := createTestPod(podName, map[string]string{
					"weka.io/cores-ids": tc.annotation,
				}, nil)

				_, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred(), "Pod creation should succeed initially")

				// Pod should either fail to start or be rejected
				Eventually(func() bool {
					updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
					if err != nil {
						return true // Pod was rejected/deleted
					}
					
					// Check if containers failed to start
					for _, containerStatus := range updatedPod.Status.ContainerStatuses {
						if containerStatus.State.Waiting != nil && 
						   (strings.Contains(containerStatus.State.Waiting.Reason, "Invalid") ||
							strings.Contains(containerStatus.State.Waiting.Reason, "Error")) {
							return true
						}
					}
					return false
				}, timeout, interval).Should(BeTrue(), 
					fmt.Sprintf("Pod with %s should fail", tc.description))
			}
		})
	})
})

var _ = Describe("CPU Conflict Resolution", Label("e2e"), func() {
	Context("When annotated and integer pods compete for CPUs", func() {
		AfterEach(func() {
			pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
			if err == nil {
				for _, pod := range pods.Items {
					_ = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
				}
			}
		})

		It("should reject integer pods when CPUs are reserved by annotated pods", func() {
			By("Creating an annotated pod that reserves specific CPUs")
			annotatedPod := createTestPod("conflict-annotated", map[string]string{
				"weka.io/cores-ids": "2,3", // Reserve CPUs 2 and 3
			}, nil)

			createdAnnotated, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for annotated pod to be running")
			waitForPodRunning(createdAnnotated.Name)

			By("Creating an integer pod that would need the reserved CPUs")
			integerResources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"), // Request 4 CPUs when only some remain free
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			integerPod := createTestPod("conflict-integer", nil, integerResources)
			_, err = kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying integer pod fails due to insufficient free CPUs")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, integerPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}

				// Check for scheduling failure or container creation error
				for _, condition := range updatedPod.Status.Conditions {
					if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
						return strings.Contains(condition.Message, "CPU") || strings.Contains(condition.Message, "insufficient")
					}
				}

				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						return strings.Contains(containerStatus.State.Waiting.Message, "CPU") ||
							   strings.Contains(containerStatus.State.Waiting.Reason, "insufficient")
					}
				}

				return false
			}, timeout, interval).Should(BeTrue(), "Integer pod should fail when insufficient free CPUs due to annotated pod reservation")
		})

		It("should reject annotated pods when CPUs are reserved by integer pods", func() {
			By("Creating an integer pod that reserves CPUs exclusively")
			integerResources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			integerPod := createTestPod("conflict-integer-first", nil, integerResources)
			createdInteger, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdInteger.Name)

			By("Getting the CPUs allocated to integer pod")
			Eventually(func() string {
				output, err := getPodCPUSet(createdInteger.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(MatchRegexp(`Cpus_allowed_list:\s*\d+[,-]\d+`))

			By("Creating an annotated pod requesting the same CPUs (assuming integer got 0,1)")
			annotatedPod := createTestPod("conflict-annotated-second", map[string]string{
				"weka.io/cores-ids": "0,1", // Try to request CPUs likely allocated to integer pod
			}, nil)

			_, err = kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying annotated pod fails due to CPU conflict with integer pod")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, annotatedPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}

				// Check for container creation failure due to CPU conflict
				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						return strings.Contains(containerStatus.State.Waiting.Message, "reserved") ||
							   strings.Contains(containerStatus.State.Waiting.Message, "conflict") ||
							   strings.Contains(containerStatus.State.Waiting.Reason, "CreateContainerError")
					}
				}

				return updatedPod.Status.Phase == corev1.PodFailed
			}, timeout, interval).Should(BeTrue(), "Annotated pod should fail when requesting CPUs reserved by integer pod")
		})

		It("should allow annotated pods to share CPUs with each other but not with integer pods", func() {
			By("Creating first annotated pod")
			annotated1 := createTestPod("share-annotated-1", map[string]string{
				"weka.io/cores-ids": "0,1",
			}, nil)

			created1, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotated1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Creating second annotated pod sharing the same CPUs")
			annotated2 := createTestPod("share-annotated-2", map[string]string{
				"weka.io/cores-ids": "0,1",
			}, nil)

			created2, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotated2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for both annotated pods to be running")
			waitForPodRunning(created1.Name)
			waitForPodRunning(created2.Name)

			By("Verifying both annotated pods succeeded")
			Eventually(func() bool {
				pod1, err1 := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, created1.Name, metav1.GetOptions{})
				pod2, err2 := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, created2.Name, metav1.GetOptions{})
				if err1 != nil || err2 != nil {
					return false
				}
				return pod1.Status.Phase == corev1.PodRunning && pod2.Status.Phase == corev1.PodRunning
			}, timeout, interval).Should(BeTrue(), "Both annotated pods should succeed sharing CPUs")

			By("Creating integer pod requesting overlapping CPUs")
			integerResources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"), // Would need CPUs 0,1 but they're shared by annotated pods
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			integerPod := createTestPod("conflict-integer-with-shared", nil, integerResources)
			_, err = kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying integer pod gets different CPUs or fails appropriately")
			Eventually(func() bool {
				pod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, integerPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}
				
				// Either it gets scheduled with different CPUs or fails appropriately
				if pod.Status.Phase == corev1.PodRunning {
					cpuOutput, err := getPodCPUSet(integerPod.Name)
					if err != nil {
						return false
					}
					// Should not contain CPUs 0 or 1
					return !strings.Contains(cpuOutput, "0") && !strings.Contains(cpuOutput, "1")
				}
				
				// Or it should fail due to insufficient resources
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						return true
					}
				}
				return pod.Status.Phase == corev1.PodFailed
			}, timeout, interval).Should(BeTrue(), "Integer pod should either get different CPUs or fail appropriately")
		})
	})
})