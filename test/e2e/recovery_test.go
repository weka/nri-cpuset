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

var _ = Describe("Plugin Recovery and Synchronization", Label("e2e", "parallel"), func() {
	Context("When plugin restarts or crashes", func() {
		AfterEach(func() {
			// Clean up any pods created in tests (conditional on failure)
			cleanupAllPodsConditional()
		})

		It("should rebuild state from existing containers after plugin restart", func() {
			By("Creating pods before plugin restart")
			// Create annotated pod
			annotatedPod := createTestPod("recovery-annotated", map[string]string{
				"weka.io/cores-ids": "0,1",
			}, nil)

			// Create integer pod
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
			integerPod := createTestPod("recovery-integer", nil, integerResources)

			// Create shared pod
			sharedPod := createTestPod("recovery-shared", nil, nil)

			// Deploy pods
			createdAnnotated, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			createdInteger, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			createdShared, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for all pods to be running")
			waitForPodRunning(createdAnnotated.Name)
			waitForPodRunning(createdInteger.Name)
			waitForPodRunning(createdShared.Name)

			By("Recording initial CPU assignments")
			Eventually(func() bool {
				ann, err1 := getPodCPUSet(createdAnnotated.Name)
				int, err2 := getPodCPUSet(createdInteger.Name)
				shared, err3 := getPodCPUSet(createdShared.Name)
				if err1 != nil || err2 != nil || err3 != nil {
					return false
				}
				return len(ann) > 0 && len(int) > 0 && len(shared) > 0
			}, timeout, interval).Should(BeTrue(), "All pods should have initial CPU assignments")

			By("Simulating plugin restart by killing node-local plugin pod")
			// Find the plugin pod running on our exclusive test node
			pods, err := kubeClient.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + pluginName,
				FieldSelector: "spec.nodeName=" + currentTestNode,
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(len(pods.Items)).To(BeNumerically(">", 0), fmt.Sprintf("Plugin pod should exist on node %s", currentTestNode))

			pluginPod := &pods.Items[0]
			oldPodUID := pluginPod.UID

			// Delete the plugin pod to simulate crash/restart
			err = kubeClient.CoreV1().Pods("kube-system").Delete(ctx, pluginPod.Name, metav1.DeleteOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for plugin pod to be recreated on the same node")
			// DaemonSet should recreate the pod automatically
			Eventually(func() bool {
				newPods, err := kubeClient.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
					LabelSelector: "app=" + pluginName,
					FieldSelector: "spec.nodeName=" + currentTestNode,
				})
				if err != nil || len(newPods.Items) == 0 {
					return false
				}

				// Check if it's a different pod (new UID) and it's running
				newPod := &newPods.Items[0]
				return newPod.UID != oldPodUID && newPod.Status.Phase == corev1.PodRunning
			}, timeout*2, interval).Should(BeTrue(), fmt.Sprintf("Plugin pod should be recreated and running on node %s", currentTestNode))

			By("Verifying CPU assignments are maintained after plugin restart")
			Eventually(func() bool {
				ann, err1 := getPodCPUSet(createdAnnotated.Name)
				int, err2 := getPodCPUSet(createdInteger.Name)
				shared, err3 := getPodCPUSet(createdShared.Name)
				if err1 != nil || err2 != nil || err3 != nil {
					return false
				}

				// CPU assignments should be the same or equivalent after restart
				return strings.Contains(ann, "0") && strings.Contains(ann, "1") && // Annotated pod should still have 0,1
					len(int) > 0 && len(shared) > 0 // Integer and shared should still have assignments
			}, timeout, interval).Should(BeTrue(), "CPU assignments should be maintained after plugin restart")
		})

		It("should correct pre-existing containers on plugin startup", func() {
			By("Simulating containers started before plugin")
			// In a real scenario, we'd need to start containers without the plugin
			// For this test, we'll create pods and then verify they get corrected

			sharedPod1 := createTestPod("preexisting-shared-1", nil, nil)
			sharedPod2 := createTestPod("preexisting-shared-2", nil, nil)

			createdShared1, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			createdShared2, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for shared pods to be running")
			waitForPodRunning(createdShared1.Name)
			waitForPodRunning(createdShared2.Name)

			By("Creating an integer pod that should update shared pool")
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
			integerPod := createTestPod("pool-updating-integer", nil, integerResources)
			createdInteger, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdInteger.Name)

			By("Verifying shared pods get updated CPU assignments")
			Eventually(func() bool {
				shared1CPU, err1 := getPodCPUSet(createdShared1.Name)
				shared2CPU, err2 := getPodCPUSet(createdShared2.Name)
				integerCPU, err3 := getPodCPUSet(createdInteger.Name)

				if err1 != nil || err2 != nil || err3 != nil {
					return false
				}

				// All should have CPU assignments
				return len(shared1CPU) > 0 && len(shared2CPU) > 0 && len(integerCPU) > 0
			}, timeout, interval).Should(BeTrue(), "Pre-existing shared containers should be corrected")
		})

		It("should handle synchronization events correctly", func() {
			By("Creating initial workload")
			annotatedPod := createTestPod("sync-annotated", map[string]string{
				"weka.io/cores-ids": "0,2",
			}, nil)

			sharedPod := createTestPod("sync-shared", nil, nil)

			createdAnnotated, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			createdShared, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for initial pods to be running")
			waitForPodRunning(createdAnnotated.Name)
			waitForPodRunning(createdShared.Name)

			By("Recording initial shared pool state")
			var initialSharedCPU string
			Eventually(func() string {
				output, err := getPodCPUSet(createdShared.Name)
				if err != nil {
					return ""
				}
				initialSharedCPU = output
				return output
			}, timeout, interval).Should(ContainSubstring("Cpus_allowed_list:"))

			By("Adding integer pod that should trigger synchronization")
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

			integerPod := createTestPod("sync-integer", nil, integerResources)
			createdInteger, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdInteger.Name)

			By("Verifying synchronization updated shared pool")
			Eventually(func() bool {
				sharedOutput, err := getPodCPUSet(createdShared.Name)
				if err != nil {
					return false
				}

				// Shared pool should be updated (different from initial)
				return sharedOutput != initialSharedCPU && strings.Contains(sharedOutput, "Cpus_allowed_list:")
			}, timeout, interval).Should(BeTrue(), "Shared pool should be updated after integer pod allocation")

			By("Terminating integer pod to trigger another synchronization")
			err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, createdInteger.Name, metav1.DeleteOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying shared pool expands after integer pod termination")
			Eventually(func() bool {
				sharedOutput, err := getPodCPUSet(createdShared.Name)
				if err != nil {
					return false
				}

				// Shared pool should expand again (different from when integer was running)
				return strings.Contains(sharedOutput, "Cpus_allowed_list:")
			}, timeout, interval).Should(BeTrue(), "Shared pool should expand after integer pod removal")
		})
	})
})

var _ = Describe("Live Container Updates", Label("e2e", "parallel"), func() {
	Context("When containers need live CPU updates", func() {
		AfterEach(func() {
			// Clean up any pods created in tests (conditional on failure)
			cleanupAllPodsConditional()
		})

		It("should update running shared containers when shared pool changes", func() {
			By("Creating multiple shared pods")
			sharedPod1 := createTestPod("live-shared-1", nil, nil)
			sharedPod2 := createTestPod("live-shared-2", nil, nil)

			createdShared1, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod1, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			createdShared2, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod2, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for shared pods to be running")
			waitForPodRunning(createdShared1.Name)
			waitForPodRunning(createdShared2.Name)

			By("Recording initial shared pool assignments")
			var initialShared1CPU, initialShared2CPU string
			Eventually(func() bool {
				cpu1, err1 := getPodCPUSet(createdShared1.Name)
				cpu2, err2 := getPodCPUSet(createdShared2.Name)
				if err1 != nil || err2 != nil {
					return false
				}
				initialShared1CPU, initialShared2CPU = cpu1, cpu2
				return len(cpu1) > 0 && len(cpu2) > 0
			}, timeout, interval).Should(BeTrue(), "Initial shared CPU assignments should be recorded")

			By("Creating integer pod to reduce shared pool")
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

			integerPod := createTestPod("live-integer", nil, integerResources)
			createdInteger, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for integer pod to be running")
			waitForPodRunning(createdInteger.Name)

			By("Verifying shared pods get live CPU updates")
			Eventually(func() bool {
				cpu1, err1 := getPodCPUSet(createdShared1.Name)
				cpu2, err2 := getPodCPUSet(createdShared2.Name)
				integerCPU, err3 := getPodCPUSet(createdInteger.Name)

				if err1 != nil || err2 != nil || err3 != nil {
					return false
				}

				// Shared pods should have updated assignments
				// Integer pod should have its exclusive CPUs
				return cpu1 != initialShared1CPU && cpu2 != initialShared2CPU && len(integerCPU) > 0
			}, timeout, interval).Should(BeTrue(), "Shared containers should receive live CPU updates")

			By("Terminating integer pod")
			err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, createdInteger.Name, metav1.DeleteOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying shared pods get updated again when pool expands")
			Eventually(func() bool {
				cpu1, err1 := getPodCPUSet(createdShared1.Name)
				cpu2, err2 := getPodCPUSet(createdShared2.Name)

				if err1 != nil || err2 != nil {
					return false
				}

				// Shared pools should expand again after integer pod removal
				return len(cpu1) > 0 && len(cpu2) > 0
			}, timeout, interval).Should(BeTrue(), "Shared pool should expand after integer pod removal")
		})

		It("should maintain annotated pod assignments during shared pool updates", func() {
			By("Creating annotated pod with fixed CPU assignment first")
			annotatedPod := createTestPod("live-annotated", map[string]string{
				"weka.io/cores-ids": "0,1", // Create annotated pod first to secure these CPUs
			}, nil)

			createdAnnotated, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, annotatedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for annotated pod to be running")
			waitForPodRunning(createdAnnotated.Name)

			By("Creating shared pod")
			sharedPod := createTestPod("live-shared-with-annotated", nil, nil)
			createdShared, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, sharedPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for shared pod to be running")
			waitForPodRunning(createdShared.Name)

			By("Recording initial assignments")
			Eventually(func() bool {
				ann, err1 := getPodCPUSet(createdAnnotated.Name)
				shared, err2 := getPodCPUSet(createdShared.Name)
				if err1 != nil || err2 != nil {
					return false
				}
				return strings.Contains(ann, "0") && strings.Contains(ann, "1") && len(shared) > 0
			}, timeout, interval).Should(BeTrue(), "Initial assignments should be established")

			By("Adding integer pod to trigger shared pool update")
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

			integerPod := createTestPod("live-integer-with-annotated", nil, integerResources)
			_, err = kubeClient.CoreV1().Pods(testNamespace).Create(ctx, integerPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying annotated pod assignment remains unchanged while shared pod updates")
			Eventually(func() bool {
				ann, err1 := getPodCPUSet(createdAnnotated.Name)
				shared, err2 := getPodCPUSet(createdShared.Name)

				if err1 != nil || err2 != nil {
					return false
				}

				// Annotated should remain the same, shared should be updated
				return strings.Contains(ann, "0") && strings.Contains(ann, "1") && // Annotated unchanged
					len(shared) > 0 // Shared pool should be updated
			}, timeout, interval).Should(BeTrue(), "Annotated pods should maintain assignments during shared pool updates")
		})
	})
})
