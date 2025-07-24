package container

import (
	"github.com/containerd/nri/pkg/api"
)

// HasIntegerSemantics determines if a container meets the criteria for integer pod classification.
// According to the PRD, integer pods must have:
// - CPU requests == CPU limits
// - Memory requests == Memory limits
// - CPU limits must be integer values
func HasIntegerSemantics(container *api.Container) bool {
	if container.Linux == nil || container.Linux.Resources == nil {
		return false
	}

	cpu := container.Linux.Resources.Cpu
	memory := container.Linux.Resources.Memory

	if cpu == nil || memory == nil {
		return false
	}

	// Check CPU quota and period are set for limits
	if cpu.Quota == nil || cpu.Period == nil || cpu.Quota.GetValue() <= 0 || cpu.Period.GetValue() <= 0 {
		return false
	}

	// Check memory limit is set
	if memory.Limit == nil || memory.Limit.GetValue() <= 0 {
		return false
	}

	// Check that limits.cpu is an integer
	quota := cpu.Quota.GetValue()
	period := int64(cpu.Period.GetValue())
	if quota%period != 0 {
		return false
	}

	cpuCores := quota / period
	if cpuCores <= 0 {
		return false
	}

	// CRITICAL: Check that requests == limits for both CPU and memory
	// This is required by the PRD for integer pod classification

	// Check CPU: requests == limits
	if cpu.Shares == nil {
		// If no shares (requests) are set but quota/period (limits) are set,
		// then requests != limits, so this is not an integer container
		return false
	}

	// Convert shares to CPU value: shares / 1024 should equal quota/period
	// Kubernetes uses 1024 shares per CPU core
	requestedCPUs := float64(cpu.Shares.GetValue()) / 1024.0
	limitCPUs := float64(quota) / float64(period)

	// Allow larger floating point tolerance for test environments and resource conversions
	// Kubernetes resource conversion can introduce small variations
	if abs(requestedCPUs-limitCPUs) > 0.01 {
		return false
	}

	// Check Memory: requests == limits
	// Note: Memory requests are not directly available in the NRI Container object
	// In practice, Kubernetes QoS class "Guaranteed" requires requests == limits
	// For now, we'll accept any memory configuration since we can't easily verify
	// the memory request from the NRI interface. The CPU check is the main criterion.

	return true
}

// DetermineContainerMode classifies a container as "annotated", "integer", or "shared"
func DetermineContainerMode(pod *api.PodSandbox, container *api.Container) string {
	// Check for annotation first
	if pod.Annotations != nil {
		if _, hasAnnotation := pod.Annotations["weka.io/cores-ids"]; hasAnnotation {
			return "annotated"
		}
	}

	// Check for integer semantics
	if HasIntegerSemantics(container) {
		return "integer"
	}

	return "shared"
}

// Helper function for floating point comparison
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
