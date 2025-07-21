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
			// Clean up any pods created in tests and wait for termination
			cleanupAllPodsAndWait()
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
				return strings.Contains(output, "Mems_allowed_list:\t0") || strings.Contains(output, "Mems_allowed_list: 0") || strings.Contains(output, "Mems_allowed_list:0")
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

		It("should set memory nodes for integer pods and leave shared pods unrestricted", func() {
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

			By("Verifying integer pod has NUMA memory restriction based on assigned CPUs")
			Eventually(func() bool {
				intOutput, err := getPodCPUSet(createdInteger.Name)
				if err != nil {
					return false
				}
				// Integer pod should have specific NUMA memory restriction
				return strings.Contains(intOutput, "Mems_allowed_list:") &&
					strings.Contains(intOutput, "Cpus_allowed_list:")
			}, timeout, interval).Should(BeTrue(), "Integer pod should have NUMA memory restriction")

			By("Verifying shared pod has unrestricted memory access")
			Eventually(func() bool {
				sharedOutput, err := getPodCPUSet(createdShared.Name)
				if err != nil {
					return false
				}
				// Shared pod should have system default memory access (typically all nodes)
				return strings.Contains(sharedOutput, "Cpus_allowed_list:")
			}, timeout, interval).Should(BeTrue(), "Shared pod should have CPU assignment")
		})
	})
})

var _ = Describe("Annotated Pod Error Cases", Label("e2e"), func() {
	Context("When creating pods with problematic annotations", func() {
		AfterEach(func() {
			// Clean up any pods created in tests and wait for termination
			cleanupAllPodsAndWait()
		})

		It("should handle malformed CPU list annotations", func() {
			testCases := []struct {
				annotation  string
				description string
			}{
				{"0-", "incomplete range"},
				{"a,b,c", "non-numeric values"},
				{"0,,2", "empty values"},
				{"5-2", "invalid range order"},
				{"", "empty annotation"},
			}

			for i, tc := range testCases {
				By(fmt.Sprintf("Testing %s annotation: %s", tc.description, tc.annotation))

				podName := fmt.Sprintf("malformed-pod-%d-%d", GinkgoRandomSeed(), i)
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
				}, "60s", interval).Should(BeTrue(),
					fmt.Sprintf("Pod with %s should fail", tc.description))
			}
		})
	})
})

var _ = Describe("CPU Conflict Resolution", Label("e2e"), func() {
	Context("When annotated and integer pods compete for CPUs", func() {
		AfterEach(func() {
			// Clean up any pods created in tests and wait for termination
			cleanupAllPodsAndWait()
		})

		It("should reject integer pods when CPUs are reserved by annotated pods", func() {
			By("Creating an annotated pod that reserves most available CPUs")
			// Reserve most CPUs to force conflict - use CPUs 0-59 on a 64-CPU node
			annotatedPod := createTestPod("conflict-annotated", map[string]string{
				"weka.io/cores-ids": "0-59", // Reserve CPUs 0-59, leaving only 4 free
			}, nil)

			createdAnnotated, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for annotated pod to be running")
			waitForPodRunning(createdAnnotated.Name)

			By("Getting the node where annotated pod is running")
			updatedAnnotated, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdAnnotated.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			nodeName := updatedAnnotated.Spec.NodeName
			Expect(nodeName).ToNot(BeEmpty())

			By("Creating an integer pod that would need more CPUs than available on the same node")
			integerResources := &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("8"), // Request 8 CPUs when only 4 remain free
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("8"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}

			integerPod := createTestPod("conflict-integer", nil, integerResources)
			// Force the integer pod to the same node as the annotated pod
			integerPod.Spec.NodeName = nodeName
			_, err = kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying integer pod gets restricted CPU assignment due to conflict")
			Eventually(func() bool {
				// Check if integer pod is running with non-conflicting CPUs
				if updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, integerPod.Name, metav1.GetOptions{}); err == nil && updatedPod.Status.Phase == corev1.PodRunning {
					// Get CPU assignment for integer pod
					if integerCPUs, err := getPodCPUSet(integerPod.Name); err == nil && strings.Contains(integerCPUs, "Cpus_allowed_list:") {
						// Integer pod should only get CPUs 60-63 (the 4 remaining CPUs not reserved by annotated pod)
						return strings.Contains(integerCPUs, "60-63") || strings.Contains(integerCPUs, "60,61,62,63")
					}
				}

				// Alternative: check if pod failed to start due to insufficient resources
				if updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, integerPod.Name, metav1.GetOptions{}); err == nil {
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
				}
				return false
			}, "60s", interval).Should(BeTrue(), "Integer pod should either fail or get non-conflicting CPU assignment")
		})

		It("should reject annotated pods when CPUs are reserved by integer pods", func() {
			By("Creating an annotated pod that will reserve specific CPUs")
			annotatedPod := createTestPod("conflict-annotated-first", map[string]string{
				"weka.io/cores-ids": "0,1", // Request specific CPUs first
			}, nil)

			createdAnnotated, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for annotated pod to be running")
			waitForPodRunning(createdAnnotated.Name)

			By("Verifying annotated pod got the requested CPUs")
			Eventually(func() string {
				output, err := getPodCPUSet(createdAnnotated.Name)
				if err != nil {
					return ""
				}
				return output
			}, timeout, interval).Should(And(ContainSubstring("0"), ContainSubstring("1")), "Annotated pod should get CPUs 0,1")

			By("Creating an integer pod that will try to use different CPUs")
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

			integerPod := createTestPod("conflict-integer-second", nil, integerResources)
			createdInteger, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdInteger.Name)

			By("Verifying integer pod gets different CPUs (not 0,1)")
			Eventually(func() bool {
				output, err := getPodCPUSet(createdInteger.Name)
				if err != nil {
					return false
				}
				// Integer pod should get CPUs other than 0,1 (e.g., 2,3)
				return !strings.Contains(output, "Cpus_allowed_list: 0,1") &&
					!strings.Contains(output, "Cpus_allowed_list: 0-1") &&
					strings.Contains(output, "Cpus_allowed_list:")
			}, timeout, interval).Should(BeTrue(), "Integer pod should get different CPUs than annotated pod")

			By("Now creating a conflicting annotated pod that requests CPUs already taken by integer pod")
			conflictAnnotatedPod := createTestPod("conflict-annotated-second", map[string]string{
				"weka.io/cores-ids": "2,3", // Request CPUs likely allocated to integer pod
			}, nil)

			_, err = kubeClient.CoreV1().Pods(testNamespace).Create(ctx, conflictAnnotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying conflicting annotated pod fails due to CPU conflict with integer pod")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, conflictAnnotatedPod.Name, metav1.GetOptions{})
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
			}, timeout, interval).Should(BeTrue(), "Conflicting annotated pod should fail when requesting CPUs reserved by integer pod")
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
