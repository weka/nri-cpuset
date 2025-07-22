package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TestArtifacts manages test execution artifacts and failure data collection
type TestArtifacts struct {
	ExecutionID   string
	BaseDir       string
	CurrentTest   string
	kubeClient    kubernetes.Interface
	testNamespace string
}

var globalArtifacts *TestArtifacts

// InitializeTestArtifacts creates the artifacts collection system
func InitializeTestArtifacts() *TestArtifacts {
	executionID := generateExecutionID()
	baseDir := filepath.Join(".test-reports", executionID)
	
	artifacts := &TestArtifacts{
		ExecutionID:   executionID,
		BaseDir:       baseDir,
		kubeClient:    kubeClient,
		testNamespace: testNamespace,
	}
	
	// Create base directory structure
	artifacts.ensureDirectoryStructure()
	artifacts.writeExecutionSummary()
	
	globalArtifacts = artifacts
	return artifacts
}

// generateExecutionID creates a unique identifier for this test execution
func generateExecutionID() string {
	timestamp := time.Now().Format("20060102-150405")
	shortUUID := uuid.New().String()[:8]
	return fmt.Sprintf("%s-%s", timestamp, shortUUID)
}

// ensureDirectoryStructure creates the directory layout for artifacts
func (ta *TestArtifacts) ensureDirectoryStructure() {
	dirs := []string{
		ta.BaseDir,
		filepath.Join(ta.BaseDir, "failures"),
		filepath.Join(ta.BaseDir, "logs"),
		filepath.Join(ta.BaseDir, "pod-describes"),
		filepath.Join(ta.BaseDir, "plugin-logs"),
		filepath.Join(ta.BaseDir, "cluster-state"),
		filepath.Join(ta.BaseDir, "test-debug"),
	}
	
	for _, dir := range dirs {
		os.MkdirAll(dir, 0755)
	}
}

// writeExecutionSummary creates the main execution summary file
func (ta *TestArtifacts) writeExecutionSummary() {
	summary := fmt.Sprintf(`# Test Execution Summary

**Execution ID:** %s
**Started:** %s
**Test Namespace Pattern:** wekaplugin-e2e-w*
**Plugin:** weka-nri-cpuset

## Directory Structure

- **failures/**: Individual test failure reports with pod states and logs
- **logs/**: Raw log files from various sources (plugin, test output, etc.)
- **pod-describes/**: Detailed pod descriptions for failed tests
- **plugin-logs/**: Plugin daemon logs organized by node and time
- **cluster-state/**: Cluster-wide state snapshots during failures
- **test-debug/**: Custom debug output from tests

## Analysis Workflow

1. Check **execution-summary.md** (this file) for overview
2. Look at **test-failures-index.md** for list of failed tests
3. For each failure, examine **failures/[test-name]-[timestamp]/summary.md**
4. Review corresponding pod states in **pod-describes/**
5. Analyze plugin behavior in **plugin-logs/**

## Key Files for LLM Analysis

- **test-failures-index.md**: Quick overview of all failures
- **cluster-health-snapshot.md**: Overall cluster state
- Individual failure summaries in **failures/** subdirectories

Generated: %s
`, ta.ExecutionID, time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339))

	filePath := filepath.Join(ta.BaseDir, "execution-summary.md")
	os.WriteFile(filePath, []byte(summary), 0644)
}

// CollectTestFailure gathers comprehensive failure data for a specific test
func (ta *TestArtifacts) CollectTestFailure(specReport SpecReport) {
	if !specReport.Failed() {
		return
	}
	
	testName := sanitizeTestName(specReport.FullText())
	timestamp := time.Now().Format("150405")
	failureDir := filepath.Join(ta.BaseDir, "failures", fmt.Sprintf("%s-%s", testName, timestamp))
	os.MkdirAll(failureDir, 0755)
	
	// Collect all failure data
	ta.writeFailureSummary(failureDir, specReport)
	ta.collectPodStates(failureDir)
	ta.collectPluginLogs(failureDir)
	ta.collectClusterSnapshot(failureDir)
	ta.updateFailureIndex(testName, timestamp, specReport)
}

// writeFailureSummary creates a comprehensive failure report
func (ta *TestArtifacts) writeFailureSummary(failureDir string, specReport SpecReport) {
	summary := fmt.Sprintf(`# Test Failure Report

**Test:** %s
**Execution ID:** %s
**Failed:** %s
**Duration:** %s

## Failure Details

**Location:** %s
**Message:**
%s

## Stack Trace
%s

## Artifacts Collected

- pod-states.md: All pod states across test namespaces
- plugin-logs.txt: Plugin daemon logs during test execution
- cluster-snapshot.md: Cluster-wide state at failure time
- test-debug-output.txt: Custom debug output from test

## Analysis Tips

1. Check pod-states.md for CreateContainerError or scheduling issues
2. Review plugin-logs.txt for reallocation or conflict resolution problems
3. Look for CPU/NUMA binding issues in plugin behavior
4. Check cluster-snapshot.md for resource exhaustion

`, 
		specReport.FullText(),
		ta.ExecutionID, 
		specReport.EndTime.Format(time.RFC3339),
		specReport.RunTime,
		specReport.Failure.Location.String(),
		specReport.Failure.Message,
		specReport.Failure.Location.FullStackTrace,
	)

	filePath := filepath.Join(failureDir, "summary.md")
	os.WriteFile(filePath, []byte(summary), 0644)
}

// collectPodStates gathers pod information from all test namespaces
func (ta *TestArtifacts) collectPodStates(failureDir string) {
	ctx := context.Background()
	var output strings.Builder
	
	output.WriteString("# Pod States at Test Failure\n\n")
	
	// Safety check for nil kube client
	if ta.kubeClient == nil {
		output.WriteString("Error: kubeClient is nil - cannot collect pod states\n")
		filePath := filepath.Join(failureDir, "pod-states.md")
		os.WriteFile(filePath, []byte(output.String()), 0644)
		return
	}
	
	// Get all test namespaces
	namespaces, err := ta.kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		output.WriteString(fmt.Sprintf("Error listing namespaces: %v\n", err))
		filePath := filepath.Join(failureDir, "pod-states.md")
		os.WriteFile(filePath, []byte(output.String()), 0644)
		return
	}
	
	for _, ns := range namespaces.Items {
		if !strings.HasPrefix(ns.Name, "wekaplugin-e2e") {
			continue
		}
		
		output.WriteString(fmt.Sprintf("## Namespace: %s\n\n", ns.Name))
		
		// Get pods in this namespace
		pods, err := ta.kubeClient.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			output.WriteString(fmt.Sprintf("Error listing pods: %v\n\n", err))
			continue
		}
		
		if len(pods.Items) == 0 {
			output.WriteString("No pods found\n\n")
			continue
		}
		
		for _, pod := range pods.Items {
			output.WriteString(fmt.Sprintf("### Pod: %s\n", pod.Name))
			output.WriteString(fmt.Sprintf("- **Status:** %s\n", pod.Status.Phase))
			output.WriteString(fmt.Sprintf("- **Node:** %s\n", pod.Spec.NodeName))
			output.WriteString(fmt.Sprintf("- **Created:** %s\n", pod.CreationTimestamp.Format(time.RFC3339)))
			
			// Add container statuses
			for _, containerStatus := range pod.Status.ContainerStatuses {
				output.WriteString(fmt.Sprintf("- **Container %s:** Ready=%t, Restarts=%d\n", 
					containerStatus.Name, containerStatus.Ready, containerStatus.RestartCount))
				
				if containerStatus.State.Waiting != nil {
					output.WriteString(fmt.Sprintf("  - Waiting: %s - %s\n", 
						containerStatus.State.Waiting.Reason, containerStatus.State.Waiting.Message))
				}
				if containerStatus.State.Terminated != nil {
					output.WriteString(fmt.Sprintf("  - Terminated: %s - %s\n", 
						containerStatus.State.Terminated.Reason, containerStatus.State.Terminated.Message))
				}
			}
			
			// Add pod events
			events, err := ta.kubeClient.CoreV1().Events(ns.Name).List(ctx, metav1.ListOptions{
				FieldSelector: fmt.Sprintf("involvedObject.name=%s", pod.Name),
			})
			if err == nil && len(events.Items) > 0 {
				output.WriteString("- **Recent Events:**\n")
				for i, event := range events.Items {
					if i >= 5 { // Limit to 5 most recent events
						break
					}
					output.WriteString(fmt.Sprintf("  - [%s] %s: %s\n", 
						event.LastTimestamp.Format("15:04:05"), event.Type, event.Message))
				}
			}
			output.WriteString("\n")
		}
		output.WriteString("\n")
	}
	
	filePath := filepath.Join(failureDir, "pod-states.md")
	os.WriteFile(filePath, []byte(output.String()), 0644)
}

// collectPluginLogs gathers plugin daemon logs
func (ta *TestArtifacts) collectPluginLogs(failureDir string) {
	ctx := context.Background()
	
	// Safety check for nil kube client
	if ta.kubeClient == nil {
		filePath := filepath.Join(failureDir, "plugin-logs.txt")
		os.WriteFile(filePath, []byte("Error: kubeClient is nil - cannot collect plugin logs\n"), 0644)
		return
	}
	
	// Get plugin pods
	pods, err := ta.kubeClient.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: "app=weka-nri-cpuset",
	})
	if err != nil {
		filePath := filepath.Join(failureDir, "plugin-logs.txt")
		os.WriteFile(filePath, []byte(fmt.Sprintf("Error getting plugin pods: %v\n", err)), 0644)
		return
	}
	
	var allLogs strings.Builder
	allLogs.WriteString("# Plugin Logs at Test Failure\n\n")
	
	for _, pod := range pods.Items {
		allLogs.WriteString(fmt.Sprintf("## Pod: %s (Node: %s)\n\n", pod.Name, pod.Spec.NodeName))
		
		// Get logs from the last 10 minutes
		logOptions := &corev1.PodLogOptions{
			SinceSeconds: int64Ptr(600), // 10 minutes
			Timestamps:   true,
		}
		
		logs, err := ta.kubeClient.CoreV1().Pods("kube-system").GetLogs(pod.Name, logOptions).DoRaw(ctx)
		if err != nil {
			allLogs.WriteString(fmt.Sprintf("Error getting logs: %v\n\n", err))
			continue
		}
		
		allLogs.WriteString("```\n")
		allLogs.WriteString(string(logs))
		allLogs.WriteString("\n```\n\n")
	}
	
	filePath := filepath.Join(failureDir, "plugin-logs.txt")
	os.WriteFile(filePath, []byte(allLogs.String()), 0644)
}

// collectClusterSnapshot gathers cluster-wide state information
func (ta *TestArtifacts) collectClusterSnapshot(failureDir string) {
	ctx := context.Background()
	var output strings.Builder
	
	output.WriteString("# Cluster State Snapshot\n\n")
	output.WriteString(fmt.Sprintf("**Captured:** %s\n\n", time.Now().Format(time.RFC3339)))
	
	// Plugin DaemonSet status
	ds, err := ta.kubeClient.AppsV1().DaemonSets("kube-system").Get(ctx, "weka-nri-cpuset", metav1.GetOptions{})
	if err == nil {
		output.WriteString("## Plugin DaemonSet Status\n\n")
		output.WriteString(fmt.Sprintf("- **Desired:** %d\n", ds.Status.DesiredNumberScheduled))
		output.WriteString(fmt.Sprintf("- **Current:** %d\n", ds.Status.CurrentNumberScheduled))
		output.WriteString(fmt.Sprintf("- **Ready:** %d\n", ds.Status.NumberReady))
		output.WriteString(fmt.Sprintf("- **Available:** %d\n", ds.Status.NumberAvailable))
		output.WriteString("\n")
	}
	
	// Node resource status
	nodes, err := ta.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err == nil {
		output.WriteString("## Node Resources\n\n")
		for _, node := range nodes.Items {
			output.WriteString(fmt.Sprintf("### %s\n", node.Name))
			
			if cpu, ok := node.Status.Capacity["cpu"]; ok {
				output.WriteString(fmt.Sprintf("- **CPU Capacity:** %s\n", cpu.String()))
			}
			if mem, ok := node.Status.Capacity["memory"]; ok {
				output.WriteString(fmt.Sprintf("- **Memory Capacity:** %s\n", mem.String()))
			}
			
			// Node conditions
			for _, condition := range node.Status.Conditions {
				if condition.Type == "Ready" {
					output.WriteString(fmt.Sprintf("- **Ready:** %s\n", condition.Status))
					break
				}
			}
			output.WriteString("\n")
		}
	}
	
	filePath := filepath.Join(failureDir, "cluster-snapshot.md")
	os.WriteFile(filePath, []byte(output.String()), 0644)
}

// updateFailureIndex maintains an index of all test failures
func (ta *TestArtifacts) updateFailureIndex(testName, timestamp string, specReport SpecReport) {
	indexPath := filepath.Join(ta.BaseDir, "test-failures-index.md")
	
	entry := fmt.Sprintf("- **%s** (Failed: %s) - [View Details](failures/%s-%s/summary.md)\n  - Duration: %s\n  - Error: %s\n\n", 
		testName,
		specReport.EndTime.Format("15:04:05"),
		testName, 
		timestamp,
		specReport.RunTime,
		truncateString(specReport.Failure.Message, 100))
	
	// Read existing content
	existing := ""
	if content, err := os.ReadFile(indexPath); err == nil {
		existing = string(content)
	} else {
		existing = fmt.Sprintf("# Test Failures Index - Execution %s\n\n", ta.ExecutionID)
	}
	
	// Append new entry
	updated := existing + entry
	os.WriteFile(indexPath, []byte(updated), 0644)
}

// AddTestDebugInfo allows tests to add custom debug information
func (ta *TestArtifacts) AddTestDebugInfo(testName, info string) {
	if ta == nil {
		return
	}
	
	debugDir := filepath.Join(ta.BaseDir, "test-debug")
	filePath := filepath.Join(debugDir, fmt.Sprintf("%s-debug.txt", sanitizeTestName(testName)))
	
	timestamp := time.Now().Format("15:04:05")
	entry := fmt.Sprintf("[%s] %s\n", timestamp, info)
	
	// Append to debug file
	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer file.Close()
	
	file.WriteString(entry)
}

// Helper functions
func sanitizeTestName(name string) string {
	// Replace spaces and special characters for filename safety
	sanitized := strings.ReplaceAll(name, " ", "-")
	sanitized = strings.ReplaceAll(sanitized, "/", "-")
	sanitized = strings.ReplaceAll(sanitized, "\\", "-")
	sanitized = strings.ReplaceAll(sanitized, ":", "-")
	return sanitized
}

func truncateString(s string, length int) string {
	if len(s) <= length {
		return s
	}
	return s[:length] + "..."
}

func int64Ptr(i int64) *int64 {
	return &i
}

// Global helper functions for use in tests
func AddDebugInfo(info string) {
	if globalArtifacts != nil {
		testName := CurrentSpecReport().FullText()
		globalArtifacts.AddTestDebugInfo(testName, info)
	}
}

func CollectFailureArtifacts() {
	if globalArtifacts != nil {
		globalArtifacts.CollectTestFailure(CurrentSpecReport())
	}
}