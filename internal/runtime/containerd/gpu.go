package containerd

import (
	"context"
	"errors"
	"fmt"
	"slices"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	containerdcdi "github.com/containerd/containerd/v2/pkg/cdi"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cdilib "tags.cncf.io/container-device-interface/pkg/cdi"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/fastlet/gpu"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func (r *Driver) SetGPUManager(manager *gpu.Manager) error {
	if r.runtimeName != apiv1alpha2.RuntimeContainer || manager == nil || r.gpuManager != nil {
		return errors.New("GPU manager requires an unconfigured container runtime")
	}
	if err := cdilib.Refresh(); err != nil {
		return fmt.Errorf("refresh NVIDIA CDI inventory: %w", err)
	}
	// Resolve every assigned GPU before the Fastlet can advertise readiness.
	for _, device := range manager.Devices() {
		if cdilib.GetDefaultCache().GetDevice(device) == nil {
			return fmt.Errorf("assigned GPU has no CDI definition: %s", device)
		}
	}
	r.gpuManager = manager
	return nil
}

func (r *Driver) validateGPUAllocation(input *fastletapi.EnsureSandboxInput, allocation fastletapi.RuntimeAllocation) error {
	if r.gpuManager == nil {
		if allocation.GPU != nil {
			return errors.New("GPU allocation requires a configured GPU manager")
		}
		return nil
	}
	if input == nil || allocation.GPU == nil {
		return errors.New("GPU Pool requires a reserved GPU allocation")
	}
	for _, lease := range r.gpuManager.Leases() {
		if lease.Owner.SandboxUID == input.Sandbox.Identity.SandboxUID {
			if !sameGPUOwner(lease.Owner, input.Sandbox.Identity) || !slices.Equal(lease.Devices, allocation.GPU.Devices) {
				return errors.New("GPU allocation does not match its durable runtime owner")
			}
			return nil
		}
	}
	return errors.New("GPU allocation is not durably reserved")
}

func sameGPUOwner(a, b fastletapi.SandboxIdentity) bool {
	return a.SandboxUID == b.SandboxUID && a.FastletPodUID == b.FastletPodUID && a.InstanceGeneration == b.InstanceGeneration && a.RuntimeInstanceID == b.RuntimeInstanceID && a.AssignmentAttempt == b.AssignmentAttempt
}

func (r *Driver) recoverGPUs(ctx context.Context, managed []*SandboxMetadata) error {
	if r.gpuManager == nil {
		for _, metadata := range managed {
			if metadata != nil && metadata.Allocation.GPU != nil {
				return errors.New("GPU runtimes cannot recover without their GPU manager")
			}
		}
		return nil
	}
	present, err := r.recoverGPUInventory(managed)
	if err != nil {
		return err
	}
	// A crash may leave a reservation before the container object was created.
	// Cleanup uses exact IDs and retains the lease on any ambiguous outcome.
	records, err := r.gpuManager.Records()
	if err != nil {
		return err
	}
	for _, lease := range records {
		if lease.Owner.FastletPodUID != r.fastletPodUID {
			if err := r.verifyOldGPUOwnerAbsent(ctx, lease); err != nil {
				return err
			}
			if err := r.gpuManager.ReleaseVerified(lease); err != nil {
				return err
			}
			continue
		}
		if !present[lease.Owner.SandboxUID] {
			if _, err := r.InspectSandbox(ctx, lease.Owner.SandboxUID); !errors.Is(err, ErrSandboxNotFound) {
				return fmt.Errorf("GPU lease cannot be reconciled with runtime inventory: %w", err)
			}
			if err := r.DeleteSandbox(ctx, lease.Owner.SandboxUID); err != nil {
				return err
			}
		}
	}
	return nil
}

func gpuSpecOpt(allocation *fastletapi.GPUAllocation) oci.SpecOpts {
	return containerdcdi.WithCDIDevices(allocation.Devices...)
}

// A new Pod must not reuse a GPU while an orphan task of its previous owner is
// waiting for NodeJanitor. This only observes foreign runtimes; it never kills them.
func (r *Driver) verifyOldGPUOwnerAbsent(ctx context.Context, lease gpu.Lease) error {
	if lease.Endpoint != r.gpuRuntimeEndpoint() {
		return errors.New("cannot verify prior GPU owner from a different containerd endpoint; operator reconciliation is required")
	}
	ctx = namespaces.WithNamespace(ctx, lease.Namespace)
	_, err := r.client.LoadContainer(ctx, lease.Owner.SandboxUID)
	if err == nil {
		return fmt.Errorf("GPU is still owned by container %s of Fastlet %s", lease.Owner.SandboxUID, lease.Owner.FastletPodUID)
	}
	if !errdefs.IsNotFound(err) {
		return err
	}
	_, err = r.client.TaskService().Get(ctx, &tasksapi.GetRequest{ContainerID: lease.Owner.SandboxUID})
	if err == nil {
		return fmt.Errorf("GPU is still owned by orphan task %s", lease.Owner.SandboxUID)
	}
	if !errdefs.IsNotFound(err) && status.Code(err) != codes.NotFound {
		return err
	}
	return nil
}

func (r *Driver) recoverGPUInventory(managed []*SandboxMetadata) (map[string]bool, error) {
	present := map[string]bool{}
	for _, metadata := range managed {
		if metadata == nil {
			continue
		}
		if metadata.Allocation.GPU == nil {
			return nil, errors.New("GPU Pool runtime has no durable device allocation")
		}
		if metadata.Config.Identity.FastletPodUID != r.fastletPodUID {
			return nil, errors.New("GPU runtime belongs to a different Fastlet Pod")
		}
		if err := r.gpuManager.Recover(gpu.Lease{Owner: metadata.Config.Identity, Devices: metadata.Allocation.GPU.Devices, Namespace: r.containerdNamespace(), Endpoint: r.gpuRuntimeEndpoint()}); err != nil {
			return nil, err
		}
		present[metadata.Config.Identity.SandboxUID] = true
	}
	return present, nil
}

func (r *Driver) gpuRuntimeEndpoint() string {
	if r.socketPath != "" {
		return r.socketPath
	}
	return gpu.DefaultContainerdSocket
}
