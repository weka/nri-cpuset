package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StressTestMetrics tracks metrics during chaos testing
type StressTestMetrics struct {
	mu                    sync.RWMutex
	PodsCreated           int
	PodsDeleted           int
	CreationFailures      int
	DeletionFailures      int
	StateValidationErrors int
	LiveReallocations     int
	ResourceExhaustions   int
	ConflictResolutions   int
}

func (m *StressTestMetrics) IncrementPodsCreated() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PodsCreated++
}

func (m *StressTestMetrics) IncrementPodsDeleted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PodsDeleted++
}

func (m *StressTestMetrics) IncrementCreationFailures() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CreationFailures++
}

func (m *StressTestMetrics) IncrementDeletionFailures() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeletionFailures++
}

func (m *StressTestMetrics) IncrementStateValidationErrors() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StateValidationErrors++
}

func (m *StressTestMetrics) IncrementLiveReallocations() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LiveReallocations++
}

func (m *StressTestMetrics) IncrementResourceExhaustions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ResourceExhaustions++
}

func (m *StressTestMetrics) IncrementConflictResolutions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ConflictResolutions++
}

func (m *StressTestMetrics) GetSnapshot() (int, int, int, int, int, int, int, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.PodsCreated, m.PodsDeleted, m.CreationFailures, m.DeletionFailures,
		m.StateValidationErrors, m.LiveReallocations, m.ResourceExhaustions, m.ConflictResolutions
}

// PodTemplate represents different types of pods for chaos testing
type PodTemplate struct {
	Name        string
	PodType     string // "annotated", "integer", "shared"
	Annotations map[string]string
	Resources   *corev1.ResourceRequirements
	Weight      int // Probability weight for selection
}

// StressTestState tracks the current state of pods during testing
type StressTestState struct {
	mu                  sync.RWMutex
	ActivePods          map[string]*corev1.Pod
	ExpectedAllocations map[string][]int  // podName -> expected CPUs
	PodTypes            map[string]string // podName -> pod type
}

func NewStressTestState() *StressTestState {
	return &StressTestState{
		ActivePods:          make(map[string]*corev1.Pod),
		ExpectedAllocations: make(map[string][]int),
		PodTypes:            make(map[string]string),
	}
}

func (s *StressTestState) AddPod(pod *corev1.Pod, podType string, expectedCPUs []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ActivePods[pod.Name] = pod
	s.PodTypes[pod.Name] = podType
	if len(expectedCPUs) > 0 {
		s.ExpectedAllocations[pod.Name] = expectedCPUs
	}
}

func (s *StressTestState) RemovePod(podName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ActivePods, podName)
	delete(s.ExpectedAllocations, podName)
	delete(s.PodTypes, podName)
}

func (s *StressTestState) GetActivePods() []*corev1.Pod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var pods []*corev1.Pod
	for _, pod := range s.ActivePods {
		pods = append(pods, pod)
	}
	return pods
}

func (s *StressTestState) GetPodCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ActivePods)
}

var _ = Describe("Chaos Stress Testing", Label("e2e", "stress", "parallel"), func() {
	var (
		metrics      *StressTestMetrics
		stressState  *StressTestState
		nodeCPUCount int
		podTemplates []PodTemplate
		chaosCtx     context.Context
		chaosCancel  context.CancelFunc
	)

	BeforeEach(func() {
		// Initialize metrics and state tracking
		metrics = &StressTestMetrics{}
		stressState = NewStressTestState()

		// Clean state before stress test
		cleanupAllPodsAndWait()

		// Get node CPU capacity for intelligent pod generation
		node, err := kubeClient.CoreV1().Nodes().Get(ctx, currentTestNode, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		nodeCPUCapacity := node.Status.Allocatable[corev1.ResourceCPU]
		nodeCPUCount = int(nodeCPUCapacity.Value())

		AddDebugInfo(fmt.Sprintf("Stress test starting on node %s with %d CPUs", currentTestNode, nodeCPUCount))

		// Skip test if node is too small for meaningful stress testing
		if nodeCPUCount < 8 {
			Skip(fmt.Sprintf("Node %s has insufficient CPUs (%d) for stress testing. Need at least 8 CPUs.", currentTestNode, nodeCPUCount))
		}

		// Initialize pod templates for chaos generation
		podTemplates = createPodTemplates(nodeCPUCount)

		// Set up chaos context
		chaosCtx, chaosCancel = context.WithCancel(ctx)
	})

	AfterEach(func() {
		// Cancel any running chaos
		if chaosCancel != nil {
			chaosCancel()
		}

		// Report final metrics
		created, deleted, creationFails, deletionFails, stateErrors, reallocations, exhaustions, conflicts := metrics.GetSnapshot()
		AddDebugInfo(fmt.Sprintf("Final Stress Test Metrics: Created=%d, Deleted=%d, CreationFails=%d, DeletionFails=%d, StateErrors=%d, Reallocations=%d, Exhaustions=%d, Conflicts=%d",
			created, deleted, creationFails, deletionFails, stateErrors, reallocations, exhaustions, conflicts))

		// Conditional cleanup based on test results
		cleanupAllPodsConditional()
	})

	Describe("3-Minute Chaos with Pause Validation", func() {
		It("should maintain system stability and correctness under sustained load", func() {
			By("Starting 3-minute chaos engineering stress test")

			chaosWg := &sync.WaitGroup{}

			// Start chaos workers
			numWorkers := 4
			for i := 0; i < numWorkers; i++ {
				chaosWg.Add(1)
				go chaosWorker(chaosCtx, chaosWg, i, metrics, stressState, podTemplates, nodeCPUCount)
			}

			// Start state validator (runs during chaos)
			chaosWg.Add(1)
			go stateValidator(chaosCtx, chaosWg, metrics, stressState)

			// Schedule pause validations during chaos
			pauseScheduler := time.NewTicker(30 * time.Second) // Pause every 30 seconds
			defer pauseScheduler.Stop()

			chaosDuration := 3 * time.Minute
			chaosTimeout := time.After(chaosDuration)

			By("Running chaos for 3 minutes with periodic pause validations")
			for {
				select {
				case <-chaosTimeout:
					AddDebugInfo("Chaos duration completed - stopping chaos workers")
					chaosCancel()
					chaosWg.Wait()
					goto ChaosComplete

				case <-pauseScheduler.C:
					By("Performing pause validation during chaos")
					pauseValidation(metrics, stressState)

				case <-chaosCtx.Done():
					goto ChaosComplete
				}
			}

		ChaosComplete:
			By("Performing final comprehensive state validation")
			time.Sleep(10 * time.Second) // Allow final state synchronization
			finalValidation(metrics, stressState, nodeCPUCount)

			By("Verifying system health metrics")
			verifyStressTestSuccess(metrics, stressState)
		})

		It("should handle resource exhaustion scenarios gracefully", func() {
			By("Creating stress scenario designed to exhaust CPU resources")

			// Create pods that consume most of the node's CPUs
			maxPods := (nodeCPUCount - 2) / 2 // Leave 2 CPUs free, each pod takes 2

			var resourceIntensivePods []*corev1.Pod
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

				podName := fmt.Sprintf("exhaust-integer-%d", i)
				pod := createTestPod(podName, nil, integerResources)
				createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				resourceIntensivePods = append(resourceIntensivePods, createdPod)

				// Wait for pod to be running to ensure CPU allocation
				waitForPodRunning(createdPod.Name)
				stressState.AddPod(createdPod, "integer", nil)
			}

			AddDebugInfo(fmt.Sprintf("Created %d integer pods consuming most CPU resources", len(resourceIntensivePods)))

			By("Attempting to create pods that should fail due to resource exhaustion")
			// Try to create annotated pods requesting specific CPUs that should conflict
			conflictPod := createTestPod("should-fail-exhaustion", map[string]string{
				"weka.io/cores-ids": "0,1,2,3,4,5", // Request more CPUs than available
			}, &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
			})

			createdConflictPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, conflictPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("Verifying system handles resource exhaustion gracefully")
			Eventually(func() bool {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, createdConflictPod.Name, metav1.GetOptions{})
				if err != nil {
					return false
				}

				// Check for appropriate failure due to resource exhaustion
				for _, containerStatus := range updatedPod.Status.ContainerStatuses {
					if containerStatus.State.Waiting != nil {
						if strings.Contains(containerStatus.State.Waiting.Reason, "CreateContainerError") &&
							(strings.Contains(containerStatus.State.Waiting.Message, "insufficient") ||
								strings.Contains(containerStatus.State.Waiting.Message, "cannot reallocate")) {
							AddDebugInfo(fmt.Sprintf("SUCCESS: Resource exhaustion handled gracefully: %s - %s",
								containerStatus.State.Waiting.Reason, containerStatus.State.Waiting.Message))
							metrics.IncrementResourceExhaustions()
							return true
						}
					}
				}
				return false
			}, "2m", "5s").Should(BeTrue(), "System should handle resource exhaustion gracefully")

			By("Verifying existing pods remain stable during resource pressure")
			for _, pod := range resourceIntensivePods {
				updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(updatedPod.Status.Phase).To(Equal(corev1.PodRunning),
					fmt.Sprintf("Pod %s should remain running during resource pressure", pod.Name))
			}

			AddDebugInfo("SUCCESS: Resource exhaustion scenario completed - system remained stable")
		})

		It("should handle rapid conflict resolution under load", func() {
			By("Creating rapid conflict scenarios to test live reallocation under stress")

			// Create several integer pods first
			var integerPods []*corev1.Pod
			numIntegerPods := min(6, nodeCPUCount/4) // Each takes ~2-4 CPUs

			for i := 0; i < numIntegerPods; i++ {
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

				podName := fmt.Sprintf("conflict-integer-%d", i)
				pod := createTestPod(podName, nil, integerResources)
				createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				waitForPodRunning(createdPod.Name)
				integerPods = append(integerPods, createdPod)
				stressState.AddPod(createdPod, "integer", nil)
			}

			By("Rapidly creating conflicting annotated pods to trigger multiple reallocations")
			var conflictPods []*corev1.Pod

			// Create conflicts in rapid succession
			for i := 0; i < 10; i++ {
				conflictCPUs := fmt.Sprintf("%d,%d", i%nodeCPUCount, (i+1)%nodeCPUCount)
				podName := fmt.Sprintf("rapid-conflict-%d", i)

				conflictPod := createTestPod(podName, map[string]string{
					"weka.io/cores-ids": conflictCPUs,
				}, nil)

				createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, conflictPod, metav1.CreateOptions{})
				if err != nil {
					AddDebugInfo(fmt.Sprintf("Expected creation failure for conflict pod %d: %v", i, err))
					metrics.IncrementCreationFailures()
					continue
				}

				conflictPods = append(conflictPods, createdPod)

				// Small delay to allow processing
				time.Sleep(100 * time.Millisecond)
			}

			By("Verifying some conflicts were resolved successfully")
			successfulConflicts := 0
			for _, pod := range conflictPods {
				// Check if pod eventually becomes running
				podRunning := false
				for attempts := 0; attempts < 30; attempts++ { // 30 second timeout
					updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
					if err == nil && updatedPod.Status.Phase == corev1.PodRunning {
						podRunning = true
						break
					}
					time.Sleep(1 * time.Second)
				}

				if podRunning {
					successfulConflicts++
					metrics.IncrementConflictResolutions()
					stressState.AddPod(pod, "annotated", nil)
				}
			}

			// Expect at least some conflicts to be resolved (system should handle some load)
			Expect(successfulConflicts).To(BeNumerically(">", 0),
				"At least some conflicts should be resolved successfully under load")

			AddDebugInfo(fmt.Sprintf("Rapid conflict resolution completed: %d/%d conflicts resolved successfully",
				successfulConflicts, len(conflictPods)))

			By("Verifying system state remains consistent after rapid conflicts")
			pauseValidation(metrics, stressState)
		})
	})
})

// createPodTemplates creates templates for different pod types used in chaos testing
func createPodTemplates(nodeCPUCount int) []PodTemplate {
	templates := []PodTemplate{
		// Annotated pods - specific CPU pinning
		{
			Name:    "annotated-single",
			PodType: "annotated",
			Annotations: map[string]string{
				"weka.io/cores-ids": "0",
			},
			Resources: nil,
			Weight:    2,
		},
		{
			Name:    "annotated-pair",
			PodType: "annotated",
			Annotations: map[string]string{
				"weka.io/cores-ids": "0,1",
			},
			Resources: nil,
			Weight:    2,
		},
		{
			Name:    "annotated-range",
			PodType: "annotated",
			Annotations: map[string]string{
				"weka.io/cores-ids": "0-2",
			},
			Resources: nil,
			Weight:    1,
		},
		// Integer pods - exclusive CPU allocation
		{
			Name:        "integer-small",
			PodType:     "integer",
			Annotations: nil,
			Resources: &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
			Weight: 3,
		},
		{
			Name:        "integer-medium",
			PodType:     "integer",
			Annotations: nil,
			Resources: &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("200Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("200Mi"),
				},
			},
			Weight: 2,
		},
		// Shared pods - use shared CPU pool
		{
			Name:        "shared-light",
			PodType:     "shared",
			Annotations: nil,
			Resources: &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("50Mi"),
				},
			},
			Weight: 4,
		},
		{
			Name:        "shared-heavy",
			PodType:     "shared",
			Annotations: nil,
			Resources: &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("200Mi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("200Mi"),
				},
			},
			Weight: 2,
		},
	}

	// Add dynamic annotated templates that use actual node CPUs
	maxCPU := min(nodeCPUCount-1, 15) // Use up to CPU 15 or node max
	for i := 2; i < maxCPU; i += 2 {
		templates = append(templates, PodTemplate{
			Name:    fmt.Sprintf("annotated-dynamic-%d", i),
			PodType: "annotated",
			Annotations: map[string]string{
				"weka.io/cores-ids": fmt.Sprintf("%d", i),
			},
			Resources: nil,
			Weight:    1,
		})
	}

	return templates
}

// chaosWorker runs continuous pod creation/deletion during chaos phase
func chaosWorker(ctx context.Context, wg *sync.WaitGroup, workerID int, metrics *StressTestMetrics,
	stressState *StressTestState, templates []PodTemplate, nodeCPUCount int) {
	defer wg.Done()

	AddDebugInfo(fmt.Sprintf("Chaos worker %d starting", workerID))
	rand.Seed(time.Now().UnixNano() + int64(workerID))

	operationCounter := 0
	for {
		select {
		case <-ctx.Done():
			AddDebugInfo(fmt.Sprintf("Chaos worker %d stopping after %d operations", workerID, operationCounter))
			return
		default:
			operationCounter++

			// Decide operation: 70% create, 30% delete (if pods exist)
			activePods := stressState.GetActivePods()
			shouldCreate := rand.Float64() < 0.7 || len(activePods) == 0

			if shouldCreate {
				// Create a pod
				template := selectRandomTemplate(templates)
				podName := fmt.Sprintf("chaos-w%d-op%d-%s", workerID, operationCounter, template.PodType)

				// Randomize annotated pod CPU selections
				annotations := template.Annotations
				if template.PodType == "annotated" && annotations != nil {
					annotations = randomizeAnnotations(annotations, nodeCPUCount)
				}

				pod := createTestPod(podName, annotations, template.Resources)
				createdPod, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})

				if err != nil {
					metrics.IncrementCreationFailures()
					AddDebugInfo(fmt.Sprintf("Worker %d: Creation failure for %s: %v", workerID, podName, err))
				} else {
					metrics.IncrementPodsCreated()
					stressState.AddPod(createdPod, template.PodType, nil)
					AddDebugInfo(fmt.Sprintf("Worker %d: Created %s pod %s", workerID, template.PodType, podName))
				}
			} else if len(activePods) > 0 {
				// Delete a random pod
				podToDelete := activePods[rand.Intn(len(activePods))]
				err := kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, podToDelete.Name, metav1.DeleteOptions{})

				if err != nil {
					metrics.IncrementDeletionFailures()
					AddDebugInfo(fmt.Sprintf("Worker %d: Deletion failure for %s: %v", workerID, podToDelete.Name, err))
				} else {
					metrics.IncrementPodsDeleted()
					stressState.RemovePod(podToDelete.Name)
					AddDebugInfo(fmt.Sprintf("Worker %d: Deleted pod %s", workerID, podToDelete.Name))
				}
			}

			// Variable delay between operations (100ms to 2s)
			delay := time.Duration(100+rand.Intn(1900)) * time.Millisecond
			time.Sleep(delay)
		}
	}
}

// selectRandomTemplate selects a pod template based on weights
func selectRandomTemplate(templates []PodTemplate) PodTemplate {
	totalWeight := 0
	for _, t := range templates {
		totalWeight += t.Weight
	}

	randomValue := rand.Intn(totalWeight)
	currentWeight := 0

	for _, t := range templates {
		currentWeight += t.Weight
		if randomValue < currentWeight {
			return t
		}
	}

	// Fallback to first template
	return templates[0]
}

// randomizeAnnotations creates variations of CPU annotations for chaos testing
func randomizeAnnotations(baseAnnotations map[string]string, nodeCPUCount int) map[string]string {
	if _, exists := baseAnnotations["weka.io/cores-ids"]; exists {
		// Create random variations of CPU selections
		maxCPU := min(nodeCPUCount-1, 31) // Use reasonable CPU range

		variations := []string{
			fmt.Sprintf("%d", rand.Intn(maxCPU)),
			fmt.Sprintf("%d,%d", rand.Intn(maxCPU), rand.Intn(maxCPU)),
			fmt.Sprintf("%d-%d", rand.Intn(maxCPU/2), rand.Intn(maxCPU/2)+maxCPU/2),
		}

		newAnnotations := make(map[string]string)
		for k, v := range baseAnnotations {
			if k == "weka.io/cores-ids" {
				newAnnotations[k] = variations[rand.Intn(len(variations))]
			} else {
				newAnnotations[k] = v
			}
		}
		return newAnnotations
	}
	return baseAnnotations
}

// stateValidator continuously validates system state during chaos
func stateValidator(ctx context.Context, wg *sync.WaitGroup, metrics *StressTestMetrics, stressState *StressTestState) {
	defer wg.Done()

	AddDebugInfo("State validator starting continuous monitoring")
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			AddDebugInfo("State validator stopping")
			return
		case <-ticker.C:
			// Perform lightweight state validation
			if err := validateSystemState(stressState); err != nil {
				metrics.IncrementStateValidationErrors()
				AddDebugInfo(fmt.Sprintf("State validation error: %v", err))
			}
		}
	}
}

// pauseValidation performs comprehensive validation during pause phases
func pauseValidation(metrics *StressTestMetrics, stressState *StressTestState) {
	By("=== PAUSE VALIDATION PHASE ===")

	activePods := stressState.GetActivePods()
	AddDebugInfo(fmt.Sprintf("Validating %d active pods during pause phase", len(activePods)))

	// Give system time to settle
	time.Sleep(5 * time.Second)

	// Check each active pod's state and CPU allocation
	for _, pod := range activePods {
		// Verify pod is in expected state
		updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			AddDebugInfo(fmt.Sprintf("ERROR: Cannot get pod %s: %v", pod.Name, err))
			metrics.IncrementStateValidationErrors()
			continue
		}

		// Check if pod should be running
		if updatedPod.Status.Phase != corev1.PodRunning && updatedPod.Status.Phase != corev1.PodPending {
			// Pod might have failed due to resource constraints - this could be expected
			if updatedPod.Status.Phase == corev1.PodFailed {
				AddDebugInfo(fmt.Sprintf("Pod %s failed (possibly expected due to resource constraints)", pod.Name))
				stressState.RemovePod(pod.Name)
				continue
			}
		}

		// For running pods, validate CPU allocation
		if updatedPod.Status.Phase == corev1.PodRunning {
			if err := validatePodCPUAllocation(updatedPod, stressState); err != nil {
				AddDebugInfo(fmt.Sprintf("CPU allocation validation failed for pod %s: %v", pod.Name, err))
				metrics.IncrementStateValidationErrors()
			}
		}
	}

	// Validate plugin state consistency
	if err := validatePluginLogs(); err != nil {
		AddDebugInfo(fmt.Sprintf("Plugin state validation warning: %v", err))
	}

	AddDebugInfo("=== PAUSE VALIDATION COMPLETE ===")
}

// finalValidation performs comprehensive final validation
func finalValidation(metrics *StressTestMetrics, stressState *StressTestState, nodeCPUCount int) {
	By("=== FINAL COMPREHENSIVE VALIDATION ===")

	// Wait for system to fully settle
	time.Sleep(15 * time.Second)

	activePods := stressState.GetActivePods()
	AddDebugInfo(fmt.Sprintf("Final validation of %d remaining active pods", len(activePods)))

	// Comprehensive pod state validation
	runningPods := 0
	failedPods := 0
	pendingPods := 0

	for _, pod := range activePods {
		updatedPod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			AddDebugInfo(fmt.Sprintf("ERROR: Cannot get pod %s for final validation: %v", pod.Name, err))
			metrics.IncrementStateValidationErrors()
			continue
		}

		switch updatedPod.Status.Phase {
		case corev1.PodRunning:
			runningPods++
			// Validate CPU allocation for running pods
			if err := validatePodCPUAllocation(updatedPod, stressState); err != nil {
				AddDebugInfo(fmt.Sprintf("Final CPU validation failed for pod %s: %v", pod.Name, err))
				metrics.IncrementStateValidationErrors()
			}
		case corev1.PodFailed:
			failedPods++
		case corev1.PodPending:
			pendingPods++
		}
	}

	AddDebugInfo(fmt.Sprintf("Final pod states: Running=%d, Failed=%d, Pending=%d", runningPods, failedPods, pendingPods))

	// Validate no resource leaks or orphaned allocations
	if err := validateNoResourceLeaks(); err != nil {
		AddDebugInfo(fmt.Sprintf("Resource leak validation failed: %v", err))
		metrics.IncrementStateValidationErrors()
	}

	AddDebugInfo("=== FINAL VALIDATION COMPLETE ===")
}

// validateSystemState performs lightweight state validation
func validateSystemState(stressState *StressTestState) error {
	activePods := stressState.GetActivePods()

	// Basic sanity checks
	if len(activePods) > 100 {
		return fmt.Errorf("too many active pods: %d (possible leak)", len(activePods))
	}

	return nil
}

// validatePodCPUAllocation validates that a pod has proper CPU allocation
func validatePodCPUAllocation(pod *corev1.Pod, stressState *StressTestState) error {
	// Get actual CPU allocation
	output, err := getPodCPUSet(pod.Name)
	if err != nil {
		return fmt.Errorf("cannot get CPU set: %w", err)
	}

	if !strings.Contains(output, "Cpus_allowed_list:") {
		return fmt.Errorf("no CPU allocation found in output")
	}

	// For annotated pods, check if they got requested CPUs
	podType := stressState.PodTypes[pod.Name]
	if podType == "annotated" {
		if annotation, exists := pod.Annotations["weka.io/cores-ids"]; exists {
			// Parse requested CPUs from annotation
			requestedCPUs := parseCPUList(annotation)

			// Check if at least some requested CPUs are present
			foundAny := false
			for _, requestedCPU := range requestedCPUs {
				if strings.Contains(output, requestedCPU) {
					foundAny = true
					break
				}
			}

			if !foundAny {
				return fmt.Errorf("annotated pod did not receive any of its requested CPUs: wanted %s", annotation)
			}
		}
	}

	return nil
}

// validatePluginLogs checks plugin logs for errors or inconsistencies
func validatePluginLogs() error {
	// This is a placeholder for plugin log validation
	// In a real implementation, you would check plugin logs for errors
	return nil
}

// validateNoResourceLeaks checks for resource leaks or orphaned allocations
func validateNoResourceLeaks() error {
	// This is a placeholder for resource leak detection
	// In a real implementation, you would check for orphaned CPU allocations
	return nil
}

// verifyStressTestSuccess checks if the stress test met success criteria
func verifyStressTestSuccess(metrics *StressTestMetrics, stressState *StressTestState) {
	created, deleted, creationFails, deletionFails, stateErrors, reallocations, exhaustions, conflicts := metrics.GetSnapshot()

	By("Verifying stress test success criteria")

	// Success criteria:
	// 1. At least some pods were created and deleted (system was active)
	Expect(created).To(BeNumerically(">", 10), "Should have created at least 10 pods during stress test")

	// 2. Failure rate should be reasonable (less than 50% for a stress test)
	if created > 0 {
		failureRate := float64(creationFails) / float64(created+creationFails) * 100
		Expect(failureRate).To(BeNumerically("<", 50),
			fmt.Sprintf("Creation failure rate too high: %.1f%% (%d failures out of %d attempts)",
				failureRate, creationFails, created+creationFails))
	}

	// 3. State validation errors should be minimal
	if stateErrors > 0 {
		errorRate := float64(stateErrors) / float64(created+1) * 100
		Expect(errorRate).To(BeNumerically("<", 20),
			fmt.Sprintf("State validation error rate too high: %.1f%% (%d errors)", errorRate, stateErrors))
	}

	// 4. System should have handled some complexity (conflicts, reallocations, etc.)
	totalComplexOperations := reallocations + exhaustions + conflicts
	if totalComplexOperations > 0 {
		AddDebugInfo(fmt.Sprintf("SUCCESS: System handled %d complex operations (reallocations=%d, exhaustions=%d, conflicts=%d)",
			totalComplexOperations, reallocations, exhaustions, conflicts))
	}

	activePods := stressState.GetPodCount()
	AddDebugInfo(fmt.Sprintf("Stress test completed successfully: %d pods still active, system remained stable under load", activePods))

	// Final success message
	GinkgoWriter.Printf("\n=== STRESS TEST SUCCESS ===\n")
	GinkgoWriter.Printf("Operations: %d created, %d deleted\n", created, deleted)
	GinkgoWriter.Printf("Failures: %d creation, %d deletion, %d state validation\n", creationFails, deletionFails, stateErrors)
	GinkgoWriter.Printf("Complex Operations: %d reallocations, %d exhaustions, %d conflicts\n", reallocations, exhaustions, conflicts)
	GinkgoWriter.Printf("Final State: %d active pods\n", activePods)
	GinkgoWriter.Printf("===========================\n")
}
