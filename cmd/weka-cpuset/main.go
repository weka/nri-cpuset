package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/spf13/cobra"

	"github.com/weka/nri-cpuset/pkg/allocator"
	containerPkg "github.com/weka/nri-cpuset/pkg/container"
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

	// Background update system
	updateQueue  chan updateRequest
	updateCtx    context.Context
	updateCancel context.CancelFunc
}

// updateRequest represents a background container update request
type updateRequest struct {
	updates   []*api.ContainerUpdate
	operation string
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

	// Create plugin instance with background update system
	updateCtx, updateCancel := context.WithCancel(ctx)
	p := &plugin{
		mask:         api.MustParseEventMask("RunPodSandbox,CreateContainer,UpdateContainer,RemoveContainer"),
		state:        stateManager,
		allocator:    cpuAllocator,
		numa:         numaManager,
		updateQueue:  make(chan updateRequest, 100), // Buffered channel for async updates
		updateCtx:    updateCtx,
		updateCancel: updateCancel,
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

	// Start background update processor
	go p.processBackgroundUpdates()

	// Ensure cleanup on exit
	defer func() {
		p.updateCancel()
		close(p.updateQueue)
	}()

	// Run the plugin
	if err := stub.Run(ctx); err != nil {
		return fmt.Errorf("plugin execution failed: %w", err)
	}

	return nil
}

// processBackgroundUpdates handles unsolicited container updates in a serialized manner
func (p *plugin) processBackgroundUpdates() {
	for {
		select {
		case <-p.updateCtx.Done():
			return
		case req := <-p.updateQueue:
			if len(req.updates) == 0 {
				continue
			}

			fmt.Printf("Processing %d background container updates for %s\n", len(req.updates), req.operation)
			_, err := p.stub.UpdateContainers(req.updates)
			if err != nil {
				fmt.Printf("ERROR: Background UpdateContainers failed for %s: %v\n", req.operation, err)
			} else {
				fmt.Printf("Successfully applied %d background updates for %s\n", len(req.updates), req.operation)
			}
		}
	}
}

// queueBackgroundUpdate queues container updates for background processing (async)
func (p *plugin) queueBackgroundUpdate(updates []*api.ContainerUpdate, operation string) {
	if len(updates) == 0 {
		return
	}

	req := updateRequest{
		updates:   updates,
		operation: operation,
	}

	select {
	case p.updateQueue <- req:
		fmt.Printf("Queued %d container updates for background processing (%s)\n", len(updates), operation)
	case <-p.updateCtx.Done():
		fmt.Printf("Plugin shutting down, discarding %d updates for %s\n", len(updates), operation)
	default:
		fmt.Printf("WARNING: Update queue full, discarding %d updates for %s\n", len(updates), operation)
	}
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
	modeStr := containerPkg.DetermineContainerMode(pod, container)

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

	// CRITICAL FIX: When NRI calls UpdateContainer, we need to return the proper CPU assignment
	// for containers we're managing. Returning nil,nil might cause NRI to apply default/shared CPUs.

	// Get the container's current CPU assignment from our state
	containerInfo := p.state.GetContainerInfo(container.Id)
	if containerInfo == nil {
		// Container not managed by us, let NRI handle it
		return nil, nil
	}

	fmt.Printf("DEBUG: UpdateContainer called for managed container %s (mode: %s, CPUs: %v)\n",
		safeShortID(container.Id), containerInfo.Mode, containerInfo.CPUs)

	// Return the current CPU assignment to prevent NRI from overriding it
	update := &api.ContainerUpdate{
		ContainerId: container.Id,
		Linux: &api.LinuxContainerUpdate{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: formatCPUListForUpdate(containerInfo.CPUs, containerInfo.Mode),
				},
			},
		},
	}

	fmt.Printf("DEBUG: Returning CPU assignment %s for container %s in UpdateContainer\n",
		update.Linux.Resources.Cpu.Cpus, safeShortID(container.Id))

	return []*api.ContainerUpdate{update}, nil
}

// formatCPUListForUpdate formats CPU list based on container mode
func formatCPUListForUpdate(cpus []int, mode string) string {
	if len(cpus) == 0 {
		return ""
	}

	// For annotated containers, use comma-separated format to ensure NRI compatibility
	if mode == "annotated" {
		sort.Ints(cpus)
		strs := make([]string, len(cpus))
		for i, cpu := range cpus {
			strs[i] = fmt.Sprintf("%d", cpu)
		}
		return strings.Join(strs, ",")
	}

	// For other containers, use the standard format from numa package
	return numa.FormatCPUList(cpus)
}

// safeShortID safely truncates a container ID to 12 characters for logging
func safeShortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func (p *plugin) RemoveContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) error {
	fmt.Printf("Removing container %s from pod %s/%s\n", container.Name, pod.Namespace, pod.Name)

	// Release resources and trigger shared pool update via background processing
	updates, err := p.state.RemoveContainer(container.Id, p.allocator)
	if err != nil {
		return fmt.Errorf("container removal failed: %w", err)
	}

	// Queue updates for background processing (async, no protocol violation)
	if len(updates) > 0 {
		p.queueBackgroundUpdate(updates, "container removal")
	}

	return nil
}

// Helper methods for container mode determination and handling

func (p *plugin) handleAnnotatedContainer(pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	// For annotated containers, try allocation with potential live reallocation
	adjustment, updates, err := p.state.AllocateAnnotatedWithReallocation(pod, p.allocator)
	if err != nil {
		return nil, nil, err
	}

	// Queue live reallocation updates for background processing (async, no blocking)
	if len(updates) > 0 {
		fmt.Printf("Queuing %d live reallocation updates for annotated container %s\n", len(updates), container.Name)
		// Apply the reallocation plan optimistically - the background update will handle any failures
		p.state.ApplySuccessfulReallocation()
		// Queue updates asynchronously (no blocking of NRI callback)
		p.queueBackgroundUpdate(updates, "live reallocation")
		fmt.Printf("Queued live reallocation updates for background processing\n")
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
