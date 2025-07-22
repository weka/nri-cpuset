package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	baseTestNamespace = "wekaplugin-e2e"
	pluginName        = "weka-nri-cpuset"
	timeout           = 2 * time.Minute  // Increased for resource conflicts and reallocation
	interval          = 2 * time.Second  // Reduced from 10 seconds
)

var (
	kubeClient        kubernetes.Interface
	kubeConfig        *rest.Config
	ctx               context.Context
	availableNodes    []string
	preserveOnFailure bool
	testNamespace     string
	hasFailedTests    bool
	nodeAssignments   map[int]string // worker_id -> node_name
	exclusiveNode     string         // Node assigned exclusively to this worker
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Weka NRI CPUSet E2E Suite", Label("e2e"))
}

var _ = BeforeSuite(func() {
	ctx = context.Background()

	// Initialize test artifact collection system once per test suite execution
	// Check if artifacts have already been initialized to prevent multiple instances
	if globalArtifacts == nil {
		InitializeTestArtifacts()
		fmt.Printf("Initialized test artifacts collection - Execution ID: %s\n", globalArtifacts.ExecutionID)
	} else {
		fmt.Printf("Using existing test artifacts collection - Execution ID: %s\n", globalArtifacts.ExecutionID)
	}

	// Check if we should preserve resources on failure
	if os.Getenv("PRESERVE_ON_FAILURE") == "true" {
		preserveOnFailure = true
		fmt.Println("PRESERVE_ON_FAILURE=true: Resources will be preserved on test failures for debugging")
	}

	// Set up Kubernetes client
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		kubeconfigPath = os.ExpandEnv("$HOME/.kube/config")
	}

	var err error
	kubeConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	Expect(err).ToNot(HaveOccurred(), "Failed to load kubeconfig")

	kubeClient, err = kubernetes.NewForConfig(kubeConfig)
	Expect(err).ToNot(HaveOccurred(), "Failed to create Kubernetes client")

	// Get list of available nodes for distribution
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list nodes")
	for _, node := range nodes.Items {
		availableNodes = append(availableNodes, node.Name)
	}
	fmt.Printf("Found %d available nodes for test distribution: %v\n", len(availableNodes), availableNodes)

	// Initialize node assignments for parallel workers
	nodeAssignments = make(map[int]string)

	// Get exclusive node assignment for this worker
	workerID := GinkgoParallelProcess()
	if len(availableNodes) > 0 {
		exclusiveNode = reserveExclusiveNode(workerID, availableNodes)
		nodeAssignments[workerID] = exclusiveNode
		fmt.Printf("Worker %d got exclusive access to node: %s\n", workerID, exclusiveNode)
	}

	// Create unique namespace per worker to avoid conflicts
	testNamespace = fmt.Sprintf("%s-w%d", baseTestNamespace, workerID)
	fmt.Printf("Using test namespace: %s\n", testNamespace)

	// Create test namespace with proper lifecycle management
	err = createTestNamespaceWithRetry(testNamespace, 5*time.Minute)
	Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Failed to create test namespace %s", testNamespace))

	// Verify plugin is installed
	By("Verifying the Weka NRI CPUSet plugin is installed")
	Eventually(func() bool {
		daemonSets, err := kubeClient.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + pluginName,
		})
		if err != nil {
			return false
		}

		if len(daemonSets.Items) == 0 {
			fmt.Printf("No DaemonSet found with label app=%s\n", pluginName)
			return false
		}

		ds := daemonSets.Items[0]
		return ds.Status.NumberReady > 0
	}, timeout, interval).Should(BeTrue(), "Plugin DaemonSet should be ready")
})

var _ = AfterSuite(func() {
	// Release exclusive node lock
	workerID := GinkgoParallelProcess()
	releaseNodeLock(workerID, exclusiveNode)
	
	// Check environment settings and test outcomes
	preserveOnFailure := os.Getenv("PRESERVE_ON_FAILURE") == "true"
	skipCleanup := os.Getenv("SKIP_CLEANUP") == "true"
	
	// Decide whether to clean up namespace
	shouldCleanupNamespace := !skipCleanup && !(hasFailedTests && preserveOnFailure)
	
	if shouldCleanupNamespace {
		fmt.Printf("Cleaning up test namespace %s (no failures in this worker)\n", testNamespace)
		if kubeClient != nil {
			err := deleteTestNamespaceWithWait(testNamespace, 2*time.Minute)
			if err != nil {
				fmt.Printf("Warning: Failed to cleanup namespace %s: %v\n", testNamespace, err)
			} else {
				fmt.Printf("Successfully cleaned up namespace %s\n", testNamespace)
			}
		}
	} else {
		if kubeClient != nil {
			fmt.Printf("Preserving namespace %s for debugging (failures: %v, preserve on failure: %v, skip cleanup: %v)\n", 
				testNamespace, hasFailedTests, preserveOnFailure, skipCleanup)
			fmt.Printf("Debug commands:\n")
			fmt.Printf("  kubectl get pods -n %s -o wide\n", testNamespace)
			fmt.Printf("  kubectl describe pods -n %s\n", testNamespace)
			fmt.Printf("  kubectl logs -n kube-system -l app=weka-nri-cpuset --since=5m\n")
			fmt.Printf("Manual cleanup: kubectl delete namespace %s\n", testNamespace)
		}
	}
	
	// Always show test artifacts execution ID for debugging
	if globalArtifacts != nil {
		fmt.Printf("=== Test Artifacts Collection ===\n")
		fmt.Printf("Execution ID: %s\n", globalArtifacts.ExecutionID)
		fmt.Printf("Artifacts directory: %s\n", globalArtifacts.BaseDir)
		if hasFailedTests {
			fmt.Printf("Test failure artifacts collected in: .test-reports/%s/\n", globalArtifacts.ExecutionID)
			fmt.Printf("View failure reports: ls -la .test-reports/%s/failures/\n", globalArtifacts.ExecutionID)
		}
		fmt.Printf("=====================================\n")
	}
})

// Namespace lifecycle management functions

// createTestNamespaceWithRetry creates a test namespace, handling the case where a previous namespace is still terminating
func createTestNamespaceWithRetry(namespaceName string, timeout time.Duration) error {
	start := time.Now()
	
	for time.Since(start) < timeout {
		// First, wait for any existing namespace to be fully deleted
		if err := waitForNamespaceDeletion(namespaceName, 60*time.Second); err != nil {
			fmt.Printf("Continuing after namespace deletion wait: %v\n", err)
		}
		
		// Try to create the namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespaceName,
			},
		}
		
		_, err := kubeClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		if err == nil {
			fmt.Printf("Successfully created namespace: %s\n", namespaceName)
			return nil
		}
		
		// Check if the error is due to namespace terminating
		if strings.Contains(err.Error(), "being terminated") {
			fmt.Printf("Namespace %s is terminating, waiting for deletion to complete...\n", namespaceName)
			time.Sleep(5 * time.Second)
			continue
		}
		
		// Check if namespace already exists and is active
		existingNs, getErr := kubeClient.CoreV1().Namespaces().Get(ctx, namespaceName, metav1.GetOptions{})
		if getErr == nil && existingNs.Status.Phase == corev1.NamespaceActive {
			fmt.Printf("Namespace %s already exists and is active\n", namespaceName)
			return nil
		}
		
		// For other errors, retry after a short delay
		fmt.Printf("Failed to create namespace %s (attempt %v): %v\n", namespaceName, time.Since(start), err)
		time.Sleep(2 * time.Second)
	}
	
	return fmt.Errorf("timeout waiting to create namespace %s after %v", namespaceName, timeout)
}

// waitForNamespaceDeletion waits for a namespace to be completely deleted
func waitForNamespaceDeletion(namespaceName string, timeout time.Duration) error {
	start := time.Now()
	
	for time.Since(start) < timeout {
		_, err := kubeClient.CoreV1().Namespaces().Get(ctx, namespaceName, metav1.GetOptions{})
		if err != nil {
			// Namespace doesn't exist, deletion is complete
			return nil
		}
		
		time.Sleep(1 * time.Second)
	}
	
	return fmt.Errorf("timeout waiting for namespace %s to be deleted after %v", namespaceName, timeout)
}

// deleteTestNamespaceWithWait deletes a namespace and waits for the deletion to complete
func deleteTestNamespaceWithWait(namespaceName string, timeout time.Duration) error {
	// First, try to delete the namespace
	err := kubeClient.CoreV1().Namespaces().Delete(ctx, namespaceName, metav1.DeleteOptions{})
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("failed to initiate namespace deletion: %v", err)
	}
	
	// Wait for the deletion to complete
	return waitForNamespaceDeletion(namespaceName, timeout)
}

// Helper functions for tests
func createTestPod(name string, annotations map[string]string, resources *corev1.ResourceRequirements) *corev1.Pod {
	// Use distributed pod creation by default to prevent resource conflicts
	return createDistributedTestPod(name, annotations, resources)
}

// createTestPodRaw creates a pod without node assignment (use sparingly)
func createTestPodRaw(name string, annotations map[string]string, resources *corev1.ResourceRequirements) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNamespace,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: "busybox:1.35",
					Command: []string{
						"sh", "-c", "echo 'Test container started' && exec sleep infinity",
					},
				},
			},
		},
	}

	if resources != nil {
		pod.Spec.Containers[0].Resources = *resources
	}

	return pod
}

func waitForPodRunning(podName string) {
	Eventually(func() bool {
		pod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return pod.Status.Phase == corev1.PodRunning
	}, timeout, interval).Should(BeTrue(), fmt.Sprintf("Pod %s should be running", podName))
}

func getPodCPUSet(podName string) (string, error) {
	pod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	// Execute command in pod to get CPU set
	output, err := execInPod(pod, []string{"cat", "/proc/self/status"})
	if err != nil {
		return "", err
	}

	// Parse Cpus_allowed_list from /proc/self/status output
	// This is simplified - in practice you'd need to parse the output
	return string(output), nil
}

func execInPod(pod *corev1.Pod, command []string) ([]byte, error) {
	// Use kubectl exec to get the real CPU/memory status
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		kubeconfigPath = os.ExpandEnv("$HOME/.kube/config")
	}

	cmdArgs := []string{"--kubeconfig", kubeconfigPath, "exec", "-n", pod.Namespace, pod.Name, "--"}
	cmdArgs = append(cmdArgs, command...)

	cmd := exec.Command("kubectl", cmdArgs...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to exec command in pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}

	return output, nil
}

// waitForPodTermination waits for a pod to be fully terminated and removed
func waitForPodTermination(podName string) {
	// First try waiting with a shorter timeout
	Eventually(func() bool {
		_, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
		// Pod is considered terminated when it no longer exists
		return err != nil
	}, time.Minute, interval).Should(BeTrue(), fmt.Sprintf("Pod %s should be fully terminated", podName))
}

// waitForPodsTermination waits for multiple pods to be fully terminated
func waitForPodsTermination(podNames []string) {
	for _, podName := range podNames {
		waitForPodTermination(podName)
	}
}

// cleanupAllPodsAndWait deletes all pods in the test namespace and waits for termination
func cleanupAllPodsAndWait() {
	pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	var podNames []string
	for _, pod := range pods.Items {
		podNames = append(podNames, pod.Name)
		// Use graceful deletion with a shorter timeout
		gracePeriod := int64(10)
		deleteOptions := metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}
		_ = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, pod.Name, deleteOptions)
	}

	// Wait for all pods to be terminated with shorter timeout, then force delete if needed
	for _, podName := range podNames {
		// First try graceful termination with 30s timeout
		success := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					// Timeout occurred, success remains false
				}
			}()
			Eventually(func() bool {
				_, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
				if err != nil {
					success = true
					return true
				}
				return false
			}, 60*time.Second, interval).Should(BeTrue())
		}()

		// If graceful termination failed, force delete
		if !success {
			fmt.Printf("Pod %s stuck, force deleting...\n", podName)
			gracePeriod := int64(0)
			forceDeleteOptions := metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
			}
			_ = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, podName, forceDeleteOptions)

			// Wait a bit more for force delete (don't fail if still stuck)
			func() {
				defer func() { recover() }()
				Eventually(func() bool {
					_, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
					return err != nil
				}, 10*time.Second, interval).Should(BeTrue())
			}()
		}
	}
}

// Enhanced helper functions for optimized testing

// createTestPodWithNode creates a pod with node affinity for distributed testing
func createTestPodWithNode(name, nodeName string, annotations map[string]string, resources *corev1.ResourceRequirements) *corev1.Pod {
	pod := createTestPodRaw(name, annotations, resources) // Use raw creation to avoid circular dependency
	if nodeName != "" {
		pod.Spec.NodeSelector = map[string]string{
			"kubernetes.io/hostname": nodeName,
		}
	}
	return pod
}

// getRandomNode returns a random node from available nodes for load distribution
func getRandomNode() string {
	if len(availableNodes) == 0 {
		return ""
	}
	return availableNodes[rand.Intn(len(availableNodes))]
}

// getNodeForTest returns a deterministic node based on test name for consistent distribution
func getNodeForTest(testName string) string {
	if len(availableNodes) == 0 {
		return ""
	}
	// Use simple hash to distribute tests across nodes
	hash := 0
	for _, c := range testName {
		hash += int(c)
	}
	return availableNodes[hash%len(availableNodes)]
}

// cleanupAllPodsConditional performs cleanup based on test failure status and environment settings
func cleanupAllPodsConditional() {
	// Check if current test failed
	testFailed := CurrentSpecReport().Failed()
	
	// Collect failure artifacts if test failed
	if testFailed {
		hasFailedTests = true // Track failures globally for this worker
		CollectFailureArtifacts()
	}
	
	// Check environment setting for cleanup behavior
	preserveOnFailure := os.Getenv("PRESERVE_ON_FAILURE") == "true"
	skipCleanup := os.Getenv("SKIP_CLEANUP") == "true"
	
	// Decide whether to clean up
	shouldCleanup := !skipCleanup && !(testFailed && preserveOnFailure)
	
	if shouldCleanup {
		fmt.Printf("Cleaning up pods from test (test failed: %v, preserve on failure: %v)\n", testFailed, preserveOnFailure)
		cleanupAllPodsAndWait()
		// Wait for plugin to process removal events
		waitForPluginStateSync()
	} else {
		if testFailed {
			fmt.Printf("Test failed - artifacts collected in .test-reports/%s\n", globalArtifacts.ExecutionID)
		}
		fmt.Printf("Preserving pods for debugging (test failed: %v, skip cleanup: %v, preserve on failure: %v)\n", 
			testFailed, skipCleanup, preserveOnFailure)
		fmt.Printf("Debug pods with: kubectl get pods -n %s -o wide\n", testNamespace)
		fmt.Printf("View logs with: kubectl logs -n %s <pod-name>\n", testNamespace)
		fmt.Printf("Check CPU assignment: kubectl exec -n %s <pod-name> -- cat /proc/self/status | grep Cpus_allowed_list\n", testNamespace)
		fmt.Printf("Check plugin logs: kubectl logs -n kube-system -l app=weka-nri-cpuset --since=2m | grep %s\n", testNamespace)
	}
}

// waitForPluginStateSync waits for the plugin to process container removals
// and update its internal state, particularly the shared pool
func waitForPluginStateSync() {
	// Give the plugin sufficient time to process all removal events and update shared pool
	// This is crucial for test isolation - subsequent tests should see clean state
	// 
	// The plugin needs to:
	// 1. Process RemoveContainer events from NRI
	// 2. Update internal state (intOwner, annotRef maps)
	// 3. Recalculate shared pool 
	// 4. Update any running shared containers with new pool
	//
	// In parallel execution, multiple workers may be creating/deleting pods rapidly
	// so we need to ensure all events are fully processed before next test starts
	time.Sleep(5 * time.Second)
}

// createDistributedTestPod creates a pod on the exclusively reserved node
func createDistributedTestPod(name string, annotations map[string]string, resources *corev1.ResourceRequirements) *corev1.Pod {
	// Use exclusively reserved node for this worker
	workerID := GinkgoParallelProcess()
	
	// Make pod names unique per worker and timestamp to prevent name collisions
	// Use timestamp suffix to ensure uniqueness across multiple test runs
	timestamp := time.Now().Unix()
	uniquePodName := fmt.Sprintf("%s-w%d-%d", name, workerID, timestamp)
	
	return createTestPodWithNode(uniquePodName, exclusiveNode, annotations, resources)
}

// createTestPodOnWorkerNode creates a pod on the node assigned to current worker
func createTestPodOnWorkerNode(name string, annotations map[string]string, resources *corev1.ResourceRequirements) *corev1.Pod {
	return createDistributedTestPod(name, annotations, resources)
}

// Node reservation system for exclusive access

// reserveExclusiveNode attempts to get exclusive access to a node using ConfigMaps as distributed locks
func reserveExclusiveNode(workerID int, nodes []string) string {
	lockNamespace := "kube-system" // Use kube-system for locks to avoid conflicts
	maxWaitTime := 10 * time.Minute // Wait up to 10 minutes for a node
	retryInterval := 5 * time.Second
	
	start := time.Now()
	fmt.Printf("Worker %d: Attempting to reserve exclusive node (timeout: %v)...\n", workerID, maxWaitTime)
	
	for time.Since(start) < maxWaitTime {
		// Try each node in order
		for _, nodeName := range nodes {
			lockName := fmt.Sprintf("e2e-node-lock-%s", nodeName)
			
			// Try to create lock ConfigMap
			lockCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      lockName,
					Namespace: lockNamespace,
					Labels: map[string]string{
						"e2e-test": "node-lock",
						"worker":   fmt.Sprintf("%d", workerID),
					},
				},
				Data: map[string]string{
					"worker-id":   fmt.Sprintf("%d", workerID),
					"node-name":   nodeName,
					"locked-at":   time.Now().Format(time.RFC3339),
					"test-run-id": fmt.Sprintf("e2e-%d", time.Now().Unix()),
				},
			}
			
			_, err := kubeClient.CoreV1().ConfigMaps(lockNamespace).Create(ctx, lockCM, metav1.CreateOptions{})
			if err == nil {
				fmt.Printf("Worker %d: Successfully reserved node %s\n", workerID, nodeName)
				return nodeName
			}
			
			// Lock already exists, check if it's stale
			if strings.Contains(err.Error(), "already exists") {
				if isLockStale(lockName, lockNamespace) {
					fmt.Printf("Worker %d: Found stale lock for node %s, attempting to remove...\n", workerID, nodeName)
					err := kubeClient.CoreV1().ConfigMaps(lockNamespace).Delete(ctx, lockName, metav1.DeleteOptions{})
					if err == nil {
						// Try to acquire again immediately
						_, err := kubeClient.CoreV1().ConfigMaps(lockNamespace).Create(ctx, lockCM, metav1.CreateOptions{})
						if err == nil {
							fmt.Printf("Worker %d: Successfully reserved node %s after removing stale lock\n", workerID, nodeName)
							return nodeName
						}
					}
				}
			}
		}
		
		fmt.Printf("Worker %d: All nodes busy, waiting %v before retry...\n", workerID, retryInterval)
		time.Sleep(retryInterval)
	}
	
	// If we get here, we couldn't reserve any node - fail the test
	panic(fmt.Sprintf("Worker %d: Failed to reserve exclusive node access after %v - all %d nodes busy", 
		workerID, maxWaitTime, len(nodes)))
}

// isLockStale checks if a node lock is stale (older than 30 minutes)
func isLockStale(lockName, namespace string) bool {
	cm, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, lockName, metav1.GetOptions{})
	if err != nil {
		return true // If we can't read it, consider it stale
	}
	
	lockedAtStr, exists := cm.Data["locked-at"]
	if !exists {
		return true
	}
	
	lockedAt, err := time.Parse(time.RFC3339, lockedAtStr)
	if err != nil {
		return true
	}
	
	// Consider lock stale if older than 30 minutes
	return time.Since(lockedAt) > 30*time.Minute
}

// releaseNodeLock releases the exclusive node lock
func releaseNodeLock(workerID int, nodeName string) {
	if nodeName == "" {
		return
	}
	
	lockNamespace := "kube-system"
	lockName := fmt.Sprintf("e2e-node-lock-%s", nodeName)
	
	err := kubeClient.CoreV1().ConfigMaps(lockNamespace).Delete(ctx, lockName, metav1.DeleteOptions{})
	if err != nil && !strings.Contains(err.Error(), "not found") {
		fmt.Printf("Worker %d: Warning - failed to release node lock for %s: %v\n", workerID, nodeName, err)
	} else {
		fmt.Printf("Worker %d: Released exclusive lock on node %s\n", workerID, nodeName)
	}
}
