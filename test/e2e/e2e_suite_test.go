package e2e

import (
	"context"
	"fmt"
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
	timeout           = 2 * time.Minute // Increased for resource conflicts and reallocation
	interval          = 2 * time.Second // Reduced from 10 seconds
)


var (
	kubeClient        kubernetes.Interface
	kubeConfig        *rest.Config
	ctx               context.Context
	availableNodes    []string
	preserveOnFailure bool
	testNamespace     string
	hasFailedTests    bool
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Weka NRI CPUSet E2E Suite", Label("e2e"))
}

var _ = BeforeSuite(func() {
	ctx = context.Background()

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

	// Get list of available nodes for deterministic assignment
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	Expect(err).ToNot(HaveOccurred(), "Failed to list nodes")
	for _, node := range nodes.Items {
		availableNodes = append(availableNodes, node.Name)
	}
	fmt.Printf("Found %d available nodes for deterministic assignment: %v\n", len(availableNodes), availableNodes)

	// No pre-assignment of nodes - tests will get nodes assigned per-test deterministically
	// This allows for proper parallelization across multiple workers
	testNamespace = "" // Will be set per test in BeforeEach

	// Initialize test artifact collection system - namespace will be set per test
	if globalArtifacts == nil {
		globalArtifacts = InitializeTestArtifacts(kubeClient, "")
		fmt.Printf("Initialized test artifacts collection - Execution ID: %s\n", globalArtifacts.ExecutionID)
	} else {
		fmt.Printf("Using existing test artifacts collection - Execution ID: %s\n", globalArtifacts.ExecutionID)
	}

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
	// Dynamic node acquisition system - nodes are acquired/released per test
	workerID := GinkgoParallelProcess()
	fmt.Printf("Worker %d completing test suite\n", workerID)

	// Check environment settings and test outcomes
	preserveOnFailure := os.Getenv("PRESERVE_ON_FAILURE") == "true"
	skipCleanup := os.Getenv("SKIP_CLEANUP") == "true"

	// With dynamic node acquisition, each test handles its own namespace cleanup
	// AfterSuite doesn't need to clean specific namespaces
	fmt.Printf("Dynamic node acquisition active - per-test cleanup handled individually\n")
	fmt.Printf("Test outcomes: failures=%v, preserve_on_failure=%v, skip_cleanup=%v\n", 
		hasFailedTests, preserveOnFailure, skipCleanup)

	// Always show test artifacts execution ID for debugging
	if globalArtifacts != nil {
		fmt.Printf("=== Test Artifacts Collection ===\n")
		fmt.Printf("Execution ID: %s\n", globalArtifacts.ExecutionID)
		fmt.Printf("Artifacts directory: %s\n", globalArtifacts.BaseDir)
		if hasFailedTests {
			fmt.Printf("Test failure artifacts collected in: %s/\n", globalArtifacts.BaseDir)
			fmt.Printf("View failure reports: ls -la %s/failures/\n", globalArtifacts.BaseDir)
		}
		fmt.Printf("=====================================\n")
	}
})

// Dynamic node acquisition for each test
var (
	currentTestNode      string
	currentTestNamespace string
	currentTestFailed    bool
)

var _ = BeforeEach(func() {
	// Use deterministic node assignment based on Ginkgo worker ID and available nodes
	// This ensures proper distribution across parallel processes without locking conflicts
	workerID := GinkgoParallelProcess()
	
	if len(availableNodes) == 0 {
		Fail("No available nodes for test execution")
	}
	
	// Assign node deterministically but cycle through all available nodes
	nodeIndex := (workerID - 1) % len(availableNodes) // Ginkgo workers are 1-based
	currentTestNode = availableNodes[nodeIndex]
	
	// Create unique namespace for this worker/node combination
	sanitizedNodeName := strings.ToLower(strings.ReplaceAll(currentTestNode, ".", "-"))
	currentTestNamespace = fmt.Sprintf("%s-%s-w%d", baseTestNamespace, sanitizedNodeName, workerID)
	
	// Set the global testNamespace for this test
	testNamespace = currentTestNamespace
	
	// Create the namespace
	err := createTestNamespaceWithRetry(currentTestNamespace, 2*time.Minute)
	Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("Failed to create namespace %s", currentTestNamespace))
	
	fmt.Printf("Worker %d assigned to node %s with namespace %s (available nodes: %d)\n", 
		workerID, currentTestNode, currentTestNamespace, len(availableNodes))
})

var _ = AfterEach(func() {
	// Determine if current test failed
	currentTestFailed = CurrentSpecReport().Failed()
	if currentTestFailed {
		hasFailedTests = true
	}
	
	// Handle namespace cleanup based on failure status
	preserveOnFailure := os.Getenv("PRESERVE_ON_FAILURE") == "true"
	skipCleanup := os.Getenv("SKIP_CLEANUP") == "true"
	shouldCleanup := !skipCleanup && (!currentTestFailed || !preserveOnFailure)

	if shouldCleanup {
		fmt.Printf("Cleaning up namespace %s for node %s\n", currentTestNamespace, currentTestNode)
		err := deleteTestNamespaceWithWait(currentTestNamespace, 2*time.Minute)
		if err != nil {
			fmt.Printf("Warning: Failed to cleanup namespace %s: %v\n", currentTestNamespace, err)
		} else {
			fmt.Printf("Successfully cleaned up namespace %s\n", currentTestNamespace)
		}
	} else {
		fmt.Printf("Preserving namespace %s on node %s for debugging (test_failed=%v, preserve_on_failure=%v)\n", 
			currentTestNamespace, currentTestNode, currentTestFailed, preserveOnFailure)
		fmt.Printf("Debug commands:\n")
		fmt.Printf("  kubectl get pods -n %s -o wide\n", currentTestNamespace)
		fmt.Printf("  kubectl describe pods -n %s\n", currentTestNamespace) 
		fmt.Printf("  kubectl logs -n kube-system -l app=weka-nri-cpuset --since=5m\n")
		fmt.Printf("Manual cleanup: kubectl delete namespace %s\n", currentTestNamespace)
	}
	
	// Reset globals
	testNamespace = ""
	currentTestNode = ""
	currentTestNamespace = ""
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

// waitForPodTermination and waitForPodsTermination functions removed as unused

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
					fmt.Printf("Timeout during pod termination check: %v\n", r)
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
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("Timeout during force delete: %v\n", r)
					}
				}()
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

// getRandomNode function removed as unused

// getNodeForTest function removed as unused

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
	shouldCleanup := !skipCleanup && (!testFailed || !preserveOnFailure)

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
	
	// Increased wait time for better reliability in parallel execution
	// Previous 5s was insufficient when multiple workers compete for resources
	time.Sleep(15 * time.Second)
	
	// Additional verification: ensure no pods are in terminating state
	// This helps prevent race conditions where new tests start before cleanup completes
	Eventually(func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false // Continue waiting if we can't check
		}
		
		// Check if any pods are still terminating
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				fmt.Printf("Waiting for pod %s to finish terminating...\n", pod.Name)
				return false
			}
		}
		return true
	}, 30 * time.Second, 2 * time.Second).Should(BeTrue(), "All pods should finish terminating before next test")
}

// createDistributedTestPod creates a pod on the dynamically assigned node
func createDistributedTestPod(name string, annotations map[string]string, resources *corev1.ResourceRequirements) *corev1.Pod {
	// Use the node acquired by BeforeEach for this test
	timestamp := time.Now().Unix()
	sanitizedNodeName := strings.ToLower(strings.ReplaceAll(currentTestNode, ".", "-"))
	uniquePodName := fmt.Sprintf("%s-%s-%d", name, sanitizedNodeName, timestamp)

	return createTestPodWithNode(uniquePodName, currentTestNode, annotations, resources)
}

// createTestPodOnWorkerNode function removed as unused

// Node-based deterministic assignment system
//
// Each Ginkgo parallel worker gets assigned to a specific node deterministically:
// - Worker 1 -> Node 0, Worker 2 -> Node 1, etc.
// - If more workers than nodes, we cycle: Worker N -> Node (N-1) % len(nodes)
// - This eliminates race conditions and complex locking mechanisms
// - Namespace names are derived from node names for predictable isolation
//
// Benefits:
// 1. No ConfigMap-based locks needed (eliminates race conditions)
// 2. Predictable resource allocation (no "all nodes busy" scenarios)
// 3. Simplified debugging (worker ID -> node name -> namespace mapping)
// 4. Better test isolation (each worker has dedicated resources)
