package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	testNamespace = "wekaplugin-e2e"
	pluginName    = "weka-nri-cpuset"
	timeout       = 5 * time.Minute
	interval      = 10 * time.Second
)

var (
	kubeClient kubernetes.Interface
	kubeConfig *rest.Config
	ctx        context.Context
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Weka NRI CPUSet E2E Suite", Label("e2e"))
}

var _ = BeforeSuite(func() {
	ctx = context.Background()

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

	// Create test namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	_, err = kubeClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		// Namespace might already exist
		fmt.Printf("Warning: Failed to create namespace %s: %v\n", testNamespace, err)
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
	// Clean up test namespace
	if kubeClient != nil {
		err := kubeClient.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})
		if err != nil {
			fmt.Printf("Warning: Failed to delete namespace %s: %v\n", testNamespace, err)
		}
	}
})

// Helper functions for tests
func createTestPod(name string, annotations map[string]string, resources *corev1.ResourceRequirements) *corev1.Pod {
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
	Eventually(func() bool {
		_, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
		// Pod is considered terminated when it no longer exists
		return err != nil
	}, timeout, interval).Should(BeTrue(), fmt.Sprintf("Pod %s should be fully terminated", podName))
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
		// Use graceful deletion with a 30-second timeout
		gracePeriod := int64(30)
		deleteOptions := metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}
		_ = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, pod.Name, deleteOptions)
	}

	// Wait for all pods to be fully terminated
	waitForPodsTermination(podNames)
}

// deletePodAndWait deletes a specific pod and waits for its termination
func deletePodAndWait(podName string) {
	gracePeriod := int64(30)
	deleteOptions := metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	}
	err := kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, podName, deleteOptions)
	if err != nil {
		// Pod might already be deleted or not exist
		return
	}
	waitForPodTermination(podName)
}
