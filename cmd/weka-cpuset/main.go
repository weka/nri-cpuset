package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
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

	// Container cache for stateless querying
	cachedContainersMu sync.RWMutex
	cachedContainers   []*api.Container
	cachedPods         []*api.PodSandbox
}

// updateRequest represents a background container update request
type updateRequest struct {
	updates   []*api.ContainerUpdate
	operation string
	callback  func(success bool) // Optional callback for result notification
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

	// Create NRI stub options with proper launch mode detection
	var stubOptions []stub.Option

	// Decide if we run externally (DaemonSet) or runtime-launched.
	// External if user provided a socket (flag or env). Otherwise assume runtime-launched.
	external := pluginSocket != "" || os.Getenv("NRI_SOCKET") != ""

	if external {
		// Connect to the runtime's NRI socket.
		socket := firstNonEmpty(pluginSocket, os.Getenv("NRI_SOCKET"), "/var/run/nri/nri.sock")
		stubOptions = append(stubOptions, stub.WithSocketPath(socket))

		// Set plugin name and index for external mode
		name := firstNonEmpty(os.Getenv("NRI_PLUGIN_NAME"), pluginName) // e.g. "weka-cpuset"
		idx := firstNonEmpty(os.Getenv("NRI_PLUGIN_IDX"), pluginIdx)    // e.g. "99"
		stubOptions = append(stubOptions, stub.WithPluginName(name))
		stubOptions = append(stubOptions, stub.WithPluginIdx(idx))
	}
	// For runtime-launched mode, don't set any options - stub derives name from argv[0]

	// Create NRI stub
	stub, err := stub.New(p, stubOptions...)
	if err != nil {
		return fmt.Errorf("failed to create NRI stub: %w", err)
	}

	p.stub = stub

	// Set up the callback for debounced shared pool updates AFTER stub is initialized
	stateManager.SetUpdateCallback(func(updates []*api.ContainerUpdate) {
		p.queueBackgroundUpdate(updates, "debounced shared pool update")
	})

	// Set up container query callback to enable true stateless operation
	stateManager.SetContainerQueryCallback(func() ([]*api.Container, error) {
		// Query containers from the cached state maintained during Synchronize
		return p.getCachedContainers()
	})

	// Set up pod-container query callback for proper classification
	stateManager.SetPodContainerQueryCallback(func() ([]*api.PodSandbox, []*api.Container, error) {
		// Query both pods and containers from cached state
		return p.getCachedPodsAndContainers()
	})


	// Start background update processor AFTER stub is initialized
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
// NOTE: These updates are intentionally asynchronous and run outside the allocation lock
// They only call stub.UpdateContainers() which doesn't modify our internal state
func (p *plugin) processBackgroundUpdates() {
	for {
		select {
		case <-p.updateCtx.Done():
			return
		case req := <-p.updateQueue:
			if len(req.updates) == 0 {
				continue
			}

			// Safety check to prevent nil pointer dereference
			if p.stub == nil {
				fmt.Printf("WARNING: Stub not initialized, skipping %d background updates for %s\n", len(req.updates), req.operation)
				if req.callback != nil {
					req.callback(false) // Treat uninitialized stub as failure
				}
				continue
			}

			fmt.Printf("Processing %d background container updates for %s\n", len(req.updates), req.operation)
			_, err := p.stub.UpdateContainers(req.updates)
			success := err == nil
			if err != nil {
				fmt.Printf("ERROR: Background UpdateContainers failed for %s: %v\n", req.operation, err)
			} else {
				fmt.Printf("Successfully applied %d background updates for %s\n", len(req.updates), req.operation)
			}

			// Call the callback if provided
			if req.callback != nil {
				req.callback(success)
			}
		}
	}
}

// queueBackgroundUpdate queues container updates for background processing (async)
func (p *plugin) queueBackgroundUpdate(updates []*api.ContainerUpdate, operation string) {
	p.queueBackgroundUpdateWithCallback(updates, operation, nil)
}

// queueBackgroundUpdateWithCallback queues container updates with a success/failure callback
func (p *plugin) queueBackgroundUpdateWithCallback(updates []*api.ContainerUpdate, operation string, callback func(success bool)) {
	if len(updates) == 0 {
		if callback != nil {
			callback(true) // No updates means success
		}
		return
	}

	req := updateRequest{
		updates:   updates,
		operation: operation,
		callback:  callback,
	}

	select {
	case p.updateQueue <- req:
		fmt.Printf("Queued %d container updates for background processing (%s)\n", len(updates), operation)
	case <-p.updateCtx.Done():
		fmt.Printf("Plugin shutting down, discarding %d updates for %s\n", len(updates), operation)
		if callback != nil {
			callback(false) // Treat shutdown as failure
		}
	default:
		fmt.Printf("WARNING: Update queue full, discarding %d updates for %s\n", len(updates), operation)
		if callback != nil {
			callback(false) // Treat queue full as failure
		}
	}
}

// NRI plugin interface implementations

func (p *plugin) Configure(ctx context.Context, config, runtime, version string) (stub.EventMask, error) {
	fmt.Printf("%s plugin configured (runtime: %s, version: %s)\n", pluginName, runtime, version)
	return p.mask, nil
}

func (p *plugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	fmt.Printf("Synchronizing state with %d pods and %d containers\n", len(pods), len(containers))

	// CRITICAL FIX: Acquire global allocation lock to prevent races with ongoing allocations
	// Synchronize() rebuilds the entire state, so it must be serialized with all other operations
	p.state.GetAllocationLock().Lock()
	defer p.state.GetAllocationLock().Unlock()

	fmt.Printf("DEBUG: Synchronize() acquired global allocation lock\n")

	// Cache the current state for stateless querying under the global lock
	p.cachedContainersMu.Lock()
	p.cachedContainers = containers
	p.cachedPods = pods
	p.cachedContainersMu.Unlock()

	updates, err := p.state.Synchronize(pods, containers, p.allocator)
	if err != nil {
		return nil, fmt.Errorf("synchronization failed: %w", err)
	}

	fmt.Printf("DEBUG: Synchronize() completed successfully, releasing global allocation lock\n")
	return updates, nil
}

// getCachedContainers returns the cached container state for stateless querying
func (p *plugin) getCachedContainers() ([]*api.Container, error) {
	p.cachedContainersMu.RLock()
	defer p.cachedContainersMu.RUnlock()
	
	if p.cachedContainers == nil {
		return nil, fmt.Errorf("no cached containers available")
	}
	
	// Return a copy to avoid race conditions
	result := make([]*api.Container, len(p.cachedContainers))
	copy(result, p.cachedContainers)
	return result, nil
}

// getCachedPodsAndContainers returns the cached pod and container state for stateless querying
func (p *plugin) getCachedPodsAndContainers() ([]*api.PodSandbox, []*api.Container, error) {
	p.cachedContainersMu.RLock()
	defer p.cachedContainersMu.RUnlock()
	
	if p.cachedContainers == nil || p.cachedPods == nil {
		return nil, nil, fmt.Errorf("no cached pods/containers available")
	}
	
	// Return copies to avoid race conditions
	pods := make([]*api.PodSandbox, len(p.cachedPods))
	copy(pods, p.cachedPods)
	
	containers := make([]*api.Container, len(p.cachedContainers))
	copy(containers, p.cachedContainers)
	
	return pods, containers, nil
}

// updateCachedContainers updates the cached container list when containers are added/removed
func (p *plugin) updateCachedContainers(container *api.Container, isAdd bool) {
	p.cachedContainersMu.Lock()
	defer p.cachedContainersMu.Unlock()
	
	if p.cachedContainers == nil {
		if isAdd {
			p.cachedContainers = []*api.Container{container}
		}
		return
	}
	
	if isAdd {
		// Add container to cache
		p.cachedContainers = append(p.cachedContainers, container)
	} else {
		// Remove container from cache
		for i, cached := range p.cachedContainers {
			if cached.Id == container.Id {
				p.cachedContainers = append(p.cachedContainers[:i], p.cachedContainers[i+1:]...)
				break
			}
		}
	}
}

func (p *plugin) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	fmt.Printf("Pod sandbox starting: %s/%s\n", pod.Namespace, pod.Name)
	return nil
}

func (p *plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	modeStr := containerPkg.DetermineContainerMode(pod, container)

	fmt.Printf("DEBUG: CreateContainer called for %s container %s/%s (container %s)\n", 
		modeStr, pod.Namespace, pod.Name, safeShortID(container.Id))

	// Note: We update cached containers after successful allocation, not here

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
	fmt.Printf("DEBUG: UpdateContainer called for container %s in pod %s/%s\n", 
		safeShortID(container.Id), pod.Namespace, pod.Name)

	// NEW STATELESS ARCHITECTURE: Use stateless update handling with global lock
	update := p.state.StatelessUpdateContainer(container.Id)
	if update == nil {
		// Container not managed by us, let NRI handle it
		fmt.Printf("DEBUG: Container %s not managed, letting NRI handle\n", safeShortID(container.Id))
		return nil, nil
	}

	fmt.Printf("DEBUG: Returning CPU assignment %s for managed container %s\n",
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

	// CRITICAL FIX: Acquire global allocation lock to prevent races
	// RemoveContainer() modifies state and cache, so it must be serialized
	p.state.GetAllocationLock().Lock()
	defer p.state.GetAllocationLock().Unlock()

	fmt.Printf("DEBUG: RemoveContainer() acquired global allocation lock for container %s\n", safeShortID(container.Id))

	// Update cached containers to remove the container for stateless querying
	p.updateCachedContainers(container, false)

	// Release resources and trigger shared pool update via background processing
	updates, err := p.state.RemoveContainer(container.Id, p.allocator)
	if err != nil {
		return fmt.Errorf("container removal failed: %w", err)
	}

	// Queue updates for background processing (async, no protocol violation)
	if len(updates) > 0 {
		p.queueBackgroundUpdate(updates, "container removal")
	}

	fmt.Printf("DEBUG: RemoveContainer() completed successfully, releasing global allocation lock\n")
	return nil
}

// Helper methods for container mode determination and handling

// handleAnnotatedContainer uses the memory-based architecture with proper synchronization
func (p *plugin) handleAnnotatedContainer(pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	fmt.Printf("DEBUG: Handling annotated container %s with synchronized architecture\n", safeShortID(container.Id))

	// Use memory-based allocation method with proper synchronization and global locking
	adjustment, updates, err := p.state.AllocateAnnotated(pod, container, p.allocator)
	if err != nil {
		fmt.Printf("DEBUG: Stateless annotated allocation failed for container %s: %v\n", safeShortID(container.Id), err)
		return nil, nil, err
	}

	// CRITICAL FIX: Update the cached containers immediately after successful allocation
	// This ensures that subsequent allocations can see this container's CPU assignment
	if adjustment != nil {
		// Create a new container to avoid copying locks
		updatedContainer := &api.Container{
			Id:           container.Id,
			PodSandboxId: container.PodSandboxId,
			Name:         container.Name,
			State:        container.State,
			Linux: &api.LinuxContainer{
				Resources: &api.LinuxResources{
					Cpu: &api.LinuxCPU{},
				},
			},
		}
		
		// Set the allocated CPU assignment from the adjustment
		if adjustment.Linux != nil && adjustment.Linux.Resources != nil && 
			adjustment.Linux.Resources.Cpu != nil {
			updatedContainer.Linux.Resources.Cpu.Cpus = adjustment.Linux.Resources.Cpu.Cpus
		}
		
		// Update the cache with the allocated CPU assignment
		p.updateCachedContainers(updatedContainer, true)
		fmt.Printf("DEBUG: Updated cached containers after successful allocation for %s with CPUs: %s\n", 
			safeShortID(container.Id), updatedContainer.Linux.Resources.Cpu.Cpus)
	}

	// If we have reallocation updates, queue them for background processing
	if len(updates) > 0 {
		fmt.Printf("DEBUG: Queuing %d reallocation updates for annotated container %s\n", 
			len(updates), safeShortID(container.Id))
		
		p.queueBackgroundUpdate(updates, "annotated container reallocation")
	}

	fmt.Printf("DEBUG: Successfully handled annotated container %s\n", safeShortID(container.Id))
	return adjustment, nil, nil
}

// Remove old deprecated method

// handleAnnotatedContainerWithReallocation handles annotated containers with potential live reallocation
// This is now the fallback method when atomic allocation fails due to conflicts
func (p *plugin) handleAnnotatedContainerWithReallocation(pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	// For annotated containers, try allocation with potential live reallocation
	adjustment, updates, err := p.state.AllocateAnnotatedWithReallocation(pod, p.allocator)
	if err != nil {
		return nil, nil, err
	}

	// Queue live reallocation updates for background processing (async, no blocking)
	if len(updates) > 0 {
		fmt.Printf("Queuing %d live reallocation updates for annotated container %s\n", len(updates), container.Name)
		// CRITICAL FIX: Only apply reallocation plan after updates succeed
		// For now, queue updates and let them apply the plan on success
		p.queueBackgroundUpdateWithCallback(updates, "live reallocation", func(success bool) {
			if success {
				p.state.ApplySuccessfulReallocation()
				fmt.Printf("Applied reallocation plan after successful background updates\n")
			} else {
				p.state.ClearPendingReallocation()
				fmt.Printf("Cleared pending reallocation plan due to failed background updates\n")
			}
		})
		fmt.Printf("Queued live reallocation updates for background processing\n")
	}

	// Record the successful allocation with proper reference counting for annotated containers
	// Note: In the reallocation case, the allocation has already been recorded in AllocateAnnotatedWithReallocation
	// so we don't need to record it again here

	// Return adjustment only, since updates have been applied already
	return adjustment, nil, nil
}

// handleIntegerContainer uses the memory-based architecture with proper synchronization
func (p *plugin) handleIntegerContainer(pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	fmt.Printf("DEBUG: Handling integer container %s with synchronized architecture\n", safeShortID(container.Id))

	// Get forbidden CPUs from annotation (should never use these)
	forbidden := containerPkg.GetForbiddenCPUs(pod)
	
	fmt.Printf("DEBUG: Integer container %s - forbidden CPUs from annotation: %v\n", 
		safeShortID(container.Id), forbidden)

	// Use memory-based allocation method with proper synchronization and global locking
	adjustment, err := p.state.AllocateInteger(pod, container, p.allocator, forbidden)
	if err != nil {
		fmt.Printf("DEBUG: Stateless integer allocation failed for container %s: %v\n", safeShortID(container.Id), err)
		return nil, nil, err
	}

	// Extract CPUs for logging
	var cpus []int
	if adjustment.Linux != nil && adjustment.Linux.Resources != nil && 
		adjustment.Linux.Resources.Cpu != nil && adjustment.Linux.Resources.Cpu.Cpus != "" {
		cpus, _ = numa.ParseCPUList(adjustment.Linux.Resources.Cpu.Cpus)
	}

	// CRITICAL FIX: Update the cached containers immediately after successful allocation
	// This ensures that subsequent allocations can see this container's CPU assignment
	if adjustment != nil {
		// Create a new container to avoid copying locks
		updatedContainer := &api.Container{
			Id:           container.Id,
			PodSandboxId: container.PodSandboxId,
			Name:         container.Name,
			State:        container.State,
			Linux: &api.LinuxContainer{
				Resources: &api.LinuxResources{
					Cpu: &api.LinuxCPU{},
				},
			},
		}
		
		// Set the allocated CPU assignment from the adjustment
		if adjustment.Linux != nil && adjustment.Linux.Resources != nil && 
			adjustment.Linux.Resources.Cpu != nil {
			updatedContainer.Linux.Resources.Cpu.Cpus = adjustment.Linux.Resources.Cpu.Cpus
		}
		
		// Update the cache with the allocated CPU assignment  
		p.updateCachedContainers(updatedContainer, true)
		fmt.Printf("DEBUG: Updated cached containers after successful allocation for %s with CPUs: %s\n", 
			safeShortID(container.Id), updatedContainer.Linux.Resources.Cpu.Cpus)
	}

	fmt.Printf("DEBUG: Successfully allocated integer container %s with CPUs: %v\n", safeShortID(container.Id), cpus)
	return adjustment, nil, nil
}

// Remove old deprecated method

func (p *plugin) handleSharedContainer(pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	// For shared containers, get all reserved CPUs (both annotated and integer)
	reserved := p.state.GetReservedCPUs()

	// Get forbidden CPUs from annotation
	forbidden := containerPkg.GetForbiddenCPUs(pod)

	// Use the new allocator method that respects forbidden CPUs for shared containers
	cpus, _, err := p.allocator.AllocateContainerCPUsWithForbidden(pod, container, reserved, forbidden)
	if err != nil {
		return nil, nil, err
	}

	// Create the adjustment manually
	adjustment := &api.ContainerAdjustment{
		Linux: &api.LinuxContainerAdjustment{
			Resources: &api.LinuxResources{
				Cpu: &api.LinuxCPU{
					Cpus: numa.FormatCPUList(cpus),
				},
			},
		},
	}

	return adjustment, nil, nil
}

// firstNonEmpty returns the first non-empty string from the provided list
func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
