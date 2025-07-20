package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
	pluginIdx   = "20"
	version     = "1.0.0"
	description = "Weka NRI CPU/NUMA Placement Component"
)

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
)

func main() {
	var rootCmd = &cobra.Command{
		Use:     pluginName,
		Short:   description,
		Version: version,
		RunE:    runPlugin,
	}

	rootCmd.Flags().StringVar(&pluginSocket, "socket", "", "NRI plugin socket path")
	rootCmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	rootCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Dry run mode (no actual cgroup changes)")

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
	stubOptions := []stub.Option{
		stub.WithPluginName(pluginName),
		stub.WithPluginIdx(pluginIdx),
	}
	
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
	fmt.Printf("Creating container %s in pod %s/%s\n", container.Name, pod.Namespace, pod.Name)
	
	// Get current reserved CPUs from state
	reserved := p.state.GetReservedCPUs()
	
	// Determine container mode
	mode := p.determineContainerMode(pod, container)
	fmt.Printf("Container %s mode: %s\n", container.Name, mode)
	
	var adjustment *api.ContainerAdjustment
	var updates []*api.ContainerUpdate
	var err error
	
	switch mode {
	case "annotated":
		adjustment, updates, err = p.handleAnnotatedContainer(pod, container, reserved)
	case "integer":
		adjustment, updates, err = p.handleIntegerContainer(pod, container, reserved)
	case "shared":
		adjustment, updates, err = p.handleSharedContainer(pod, container, reserved)
	default:
		return nil, nil, fmt.Errorf("unknown container mode: %s", mode)
	}
	
	if err != nil {
		return nil, nil, fmt.Errorf("container allocation failed: %w", err)
	}
	
	// Update state
	if adjustment != nil {
		p.state.RecordContainer(container, adjustment)
	}
	
	return adjustment, updates, nil
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
	
	// Apply updates to shared containers
	if len(updates) > 0 {
		// In a real implementation, we would need to apply these updates
		// For now, just log them
		fmt.Printf("Would apply %d updates to shared containers\n", len(updates))
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

	// Check CPU limits and requests are equal and integer
	if cpu.Quota == nil || cpu.Period == nil || cpu.Quota.GetValue() <= 0 || cpu.Period.GetValue() <= 0 {
		return false
	}

	quota := cpu.Quota.GetValue()
	period := int64(cpu.Period.GetValue())
	if quota%period != 0 {
		return false
	}

	cpuCores := quota / period
	if cpuCores <= 0 {
		return false
	}

	// Check memory limits are set
	if memory.Limit == nil || memory.Limit.GetValue() <= 0 {
		return false
	}

	return true
}

func (p *plugin) handleAnnotatedContainer(pod *api.PodSandbox, container *api.Container, reserved []int) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	if pod.Annotations == nil {
		return nil, nil, fmt.Errorf("missing annotations for annotated container")
	}
	
	cpuList, exists := pod.Annotations["weka.io/cores-ids"]
	if !exists {
		return nil, nil, fmt.Errorf("missing weka.io/cores-ids annotation")
	}
	
	// Validate the CPU list
	if err := p.allocator.ValidateAnnotatedCPUs(cpuList, reserved); err != nil {
		return nil, nil, fmt.Errorf("invalid annotated CPUs: %w", err)
	}
	
	cpus, err := numa.ParseCPUList(cpuList)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid CPU list format: %w", err)
	}

	// Determine NUMA nodes for memory placement
	memNodes := p.numa.GetCPUNodesUnion(cpus)

	adjustment := &api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: cpuList,
				},
			},
		},
	}

	// Set memory nodes if available
	if len(memNodes) > 0 {
		adjustment.Linux.Resources.Memory = &api.LinuxMemory{}
		// Note: NRI v0.9 may not directly support cpuset.mems
		// This would need to be handled through cgroup operations or annotations
	}

	return adjustment, nil, nil
}

func (p *plugin) handleIntegerContainer(pod *api.PodSandbox, container *api.Container, reserved []int) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	if container.Linux == nil || container.Linux.Resources == nil || container.Linux.Resources.Cpu == nil {
		return nil, nil, fmt.Errorf("missing CPU resources for integer container")
	}
	
	cpu := container.Linux.Resources.Cpu
	if cpu.Quota == nil || cpu.Period == nil || cpu.Quota.GetValue() <= 0 || cpu.Period.GetValue() <= 0 {
		return nil, nil, fmt.Errorf("invalid CPU quota/period for integer container")
	}
	
	cpuCores := int(cpu.Quota.GetValue() / int64(cpu.Period.GetValue()))
	
	cpus, err := p.allocator.AllocateExclusiveCPUs(cpuCores, reserved)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to allocate exclusive CPUs: %w", err)
	}

	adjustment := &api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: numa.FormatCPUList(cpus),
				},
			},
		},
	}

	// Generate updates for shared containers that need to be moved
	var updates []*api.ContainerUpdate
	_ = p.allocator.ComputeSharedPool(append(reserved, cpus...))
	
	// This would update existing shared containers, but we need state management
	// for now, just return the adjustment
	
	return adjustment, updates, nil
}

func (p *plugin) handleSharedContainer(pod *api.PodSandbox, container *api.Container, reserved []int) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	sharedPool := p.allocator.ComputeSharedPool(reserved)
	
	if len(sharedPool) == 0 {
		return nil, nil, fmt.Errorf("shared CPU pool is empty")
	}

	adjustment := &api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: numa.FormatCPUList(sharedPool),
				},
			},
		},
	}

	return adjustment, nil, nil
}