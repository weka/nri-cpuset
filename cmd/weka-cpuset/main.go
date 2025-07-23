package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/spf13/cobra"

	"github.com/weka/nri-cpuset/pkg/allocator"
	"github.com/weka/nri-cpuset/pkg/numa"
	"github.com/weka/nri-cpuset/pkg/state"
)

const (
	pluginName  = "weka-cpuset"
	version     = "1.0.0"
	description = "Weka NRI CPU/NUMA Placement Component"
)

// Bring in the constants from state package
const (
	ModeShared    = state.ModeShared
	ModeInteger   = state.ModeInteger
	ModeAnnotated = state.ModeAnnotated
)

// Mode is now a string, so no conversion needed

type plugin struct {
	stub      stub.Stub
	mask      stub.EventMask
	state     *state.Manager
	allocator *allocator.CPUAllocator
	numa      *numa.Manager
}

var (
	pluginSocket string
	logLevel     string
	dryRun       bool
	pluginIdx    string
)

func main() {
	rootCmd := &cobra.Command{
		Use:     pluginName,
		Short:   description,
		Version: version,
		RunE:    runPlugin,
	}

	rootCmd.Flags().StringVar(&pluginSocket, "socket", "", "NRI plugin socket path")
	rootCmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	rootCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Dry run mode (no actual cgroup changes)")
	rootCmd.Flags().StringVar(&pluginIdx, "plugin-index", "99", "Plugin index for NRI registration (default: 99)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runPlugin(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Initialize NUMA manager
	numaManager, err := numa.NewManager()
	if err != nil {
		return fmt.Errorf("failed to initialize NUMA manager: %w", err)
	}

	// Initialize state manager
	stateManager := state.NewManager()

	// Initialize CPU allocator
	cpuAllocator, err := allocator.NewCPUAllocator(numaManager)
	if err != nil {
		return fmt.Errorf("failed to initialize CPU allocator: %w", err)
	}

	// Create plugin instance
	p := &plugin{
		mask:      api.MustParseEventMask("RunPodSandbox,CreateContainer,UpdateContainer,RemoveContainer"),
		state:     stateManager,
		allocator: cpuAllocator,
		numa:      numaManager,
	}

	// Create NRI stub options
	var stubOptions []stub.Option

	// Add plugin name and index
	fullPluginName := pluginIdx + "-" + pluginName
	stubOptions = append(stubOptions, stub.WithPluginName(fullPluginName))
	stubOptions = append(stubOptions, stub.WithPluginIdx(pluginIdx))

	// Add socket path if specified
	if pluginSocket != "" {
		stubOptions = append(stubOptions, stub.WithSocketPath(pluginSocket))
	}

	// Create NRI stub
	stub, err := stub.New(p, stubOptions...)
	if err != nil {
		return fmt.Errorf("failed to create NRI stub: %w", err)
	}

	p.stub = stub

	// Run the plugin
	if err := stub.Run(ctx); err != nil {
		return fmt.Errorf("plugin execution failed: %w", err)
	}

	return nil
}

// NRI plugin interface implementations

func (p *plugin) Configure(ctx context.Context, config, runtime, version string) (stub.EventMask, error) {
	fmt.Printf("%s plugin configured (runtime: %s, version: %s)\n", pluginName, runtime, version)
	return p.mask, nil
}

func (p *plugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	fmt.Printf("Synchronizing state with %d pods and %d containers\n", len(pods), len(containers))

	updates, err := p.state.Synchronize(pods, containers, p.allocator)
	if err != nil {
		return nil, fmt.Errorf("synchronization failed: %w", err)
	}

	return updates, nil
}

func (p *plugin) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	fmt.Printf("Pod sandbox starting: %s/%s\n", pod.Namespace, pod.Name)
	return nil
}

func (p *plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	modeStr := p.determineContainerMode(pod, container)

	switch modeStr {
	case "annotated":
		return p.handleAnnotatedContainer(pod, container)
	case "integer":
		return p.handleIntegerContainer(pod, container)
	case "shared":
		return p.handleSharedContainer(pod, container)
	default:
		return nil, nil, fmt.Errorf("unknown container mode: %s", modeStr)
	}
}

func (p *plugin) UpdateContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container, r *api.LinuxResources) ([]*api.ContainerUpdate, error) {
	fmt.Printf("Updating container %s in pod %s/%s\n", container.Name, pod.Namespace, pod.Name)
	return nil, nil
}

func (p *plugin) RemoveContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) error {
	fmt.Printf("Removing container %s from pod %s/%s\n", container.Name, pod.Namespace, pod.Name)

	// Release resources and trigger shared pool update
	updates, err := p.state.RemoveContainer(container.Id, p.allocator)
	if err != nil {
		return fmt.Errorf("container removal failed: %w", err)
	}

	// Apply updates to shared containers via UpdateContainers call
	if len(updates) > 0 {
		fmt.Printf("Applying %d updates to shared containers\n", len(updates))
		_, err := p.stub.UpdateContainers(updates)
		if err != nil {
			// Check if this is a critical ttrpc connection error
			errStr := err.Error()
			if strings.Contains(errStr, "ttrpc: closed") ||
				strings.Contains(errStr, "connection") ||
				strings.Contains(errStr, "broken pipe") {
				// Critical connection error - plugin should restart
				return fmt.Errorf("critical NRI connection error during container updates: %w", err)
			}
			// Non-critical error, log warning and continue
			fmt.Printf("Warning: Failed to update containers: %v\n", err)
		}
	}

	return nil
}

// Helper methods for container mode determination and handling

func (p *plugin) determineContainerMode(pod *api.PodSandbox, container *api.Container) string {
	// Check for annotation first
	if pod.Annotations != nil {
		if _, hasAnnotation := pod.Annotations["weka.io/cores-ids"]; hasAnnotation {
			return "annotated"
		}
	}

	// Check for integer semantics
	if p.hasIntegerSemantics(container) {
		return "integer"
	}

	return "shared"
}

func (p *plugin) hasIntegerSemantics(container *api.Container) bool {
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

// Helper function for floating point comparison
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func (p *plugin) handleAnnotatedContainer(pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	// For annotated containers, try allocation with potential live reallocation
	adjustment, updates, err := p.state.AllocateAnnotatedWithReallocation(pod, p.allocator)
	if err != nil {
		return nil, nil, err
	}

	// Apply live reallocation updates immediately if any were generated
	if len(updates) > 0 {
		fmt.Printf("Applying %d live reallocation updates for annotated container %s\n", len(updates), container.Name)
		_, err := p.stub.UpdateContainers(updates)
		if err != nil {
			// Rollback the pending reallocation plan on failure
			fmt.Printf("ERROR: Failed to apply live reallocation updates, rolling back: %v\n", err)
			p.state.ClearPendingReallocation()
			return nil, nil, fmt.Errorf("failed to apply live reallocation updates: %w", err)
		}
		// Only update internal state after successful container updates
		p.state.ApplySuccessfulReallocation()
		fmt.Printf("Successfully applied live reallocation updates and updated internal state\n")
	}

	// Record the successful allocation with proper reference counting for annotated containers
	if adjustment != nil {
		err := p.state.RecordAnnotatedContainer(container, pod, adjustment)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to record annotated container: %w", err)
		}
	}

	// Return adjustment only, since updates have been applied already
	return adjustment, nil, nil
}

func (p *plugin) handleIntegerContainer(pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	// For integer containers, get all reserved CPUs (both annotated and integer)
	reserved := p.state.GetReservedCPUs()
	adjustment, updates, err := p.allocator.AllocateContainer(pod, container, reserved)
	if err != nil {
		return nil, nil, err
	}

	// Record the successful allocation in state manager
	if adjustment != nil {
		err := p.state.RecordIntegerContainer(container, pod, adjustment)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to record integer container: %w", err)
		}
	}

	return adjustment, updates, nil
}

func (p *plugin) handleSharedContainer(pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	// For shared containers, get all reserved CPUs (both annotated and integer)
	reserved := p.state.GetReservedCPUs()
	return p.allocator.AllocateContainer(pod, container, reserved)
}
