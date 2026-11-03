package containerd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
	cdilib "tags.cncf.io/container-device-interface/pkg/cdi"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/fastlet/gpu"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const testGPUUUID = "GPU-00000000-0000-0000-0000-000000000001"

func TestGPUInjectionUsesOnlyReservedCDIDevice(t *testing.T) {
	directory := t.TempDir()
	document := map[string]any{"cdiVersion": "0.5.0", "kind": "nvidia.com/gpu", "devices": []any{
		map[string]any{"name": testGPUUUID, "containerEdits": map[string]any{"env": []string{"ASSIGNED_GPU=one"}, "deviceNodes": []any{map[string]any{"path": "/dev/test-gpu", "hostPath": "/dev/null"}}}},
		map[string]any{"name": "not-assigned", "containerEdits": map[string]any{"env": []string{"UNASSIGNED_GPU=visible"}}},
	}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "nvidia.json"), raw, 0600))
	require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(directory), cdilib.WithAutoRefresh(false)))
	t.Cleanup(func() {
		require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(cdilib.DefaultSpecDirs...), cdilib.WithAutoRefresh(true)))
	})
	manager, err := gpu.New(t.TempDir(), []string{testGPUUUID}, 1, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	defer manager.Close()
	driver := newTestContainerdRuntime()
	require.NoError(t, driver.SetGPUManager(manager))
	identity := fastletapi.SandboxIdentity{SandboxUID: "sandbox-a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a", InstanceGeneration: 1, AssignmentAttempt: 1}
	input := &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{Identity: identity}}
	allocation, err := manager.Acquire(identity)
	require.NoError(t, err)
	require.NoError(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{GPU: allocation}))
	spec := &oci.Spec{Process: &specs.Process{Env: []string{"USER_CONFIG=kept"}}}
	require.NoError(t, gpuSpecOpt(allocation)(context.Background(), nil, nil, spec))
	require.Contains(t, spec.Process.Env, "ASSIGNED_GPU=one")
	require.Contains(t, spec.Process.Env, "USER_CONFIG=kept")
	require.NotContains(t, spec.Process.Env, "UNASSIGNED_GPU=visible")
	require.Len(t, spec.Linux.Devices, 1)
	require.Equal(t, "/dev/test-gpu", spec.Linux.Devices[0].Path)
	require.Error(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{GPU: &fastletapi.GPUAllocation{Devices: []string{"nvidia.com/gpu=all"}}}))
	require.Error(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{}))
	input.Sandbox.Identity.RuntimeInstanceID = "replacement"
	require.Error(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{GPU: allocation}))
}

func TestGPURecoveryDoesNotGuessMissingDeviceAssignments(t *testing.T) {
	manager, err := gpu.New(t.TempDir(), []string{testGPUUUID}, 1, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	defer manager.Close()
	driver := newTestContainerdRuntime()
	driver.gpuManager = manager
	driver.fastletPodUID = "pod-a"
	identity := fastletapi.SandboxIdentity{SandboxUID: "a", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a", InstanceGeneration: 1, AssignmentAttempt: 1}
	metadata := &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{Identity: identity}}
	require.Error(t, driver.recoverGPUs(context.Background(), []*SandboxMetadata{metadata}))
	metadata.Allocation.GPU = &fastletapi.GPUAllocation{Devices: []string{"nvidia.com/gpu=" + testGPUUUID}}
	require.NoError(t, driver.recoverGPUs(context.Background(), []*SandboxMetadata{metadata}))
	require.False(t, manager.Available())
	metadata.Config.Identity.RuntimeInstanceID = "other"
	require.Error(t, driver.recoverGPUs(context.Background(), []*SandboxMetadata{metadata}))
	require.False(t, manager.Available())
}

func TestGPURecoveryDoesNotCheckAnOldOwnerAgainstADifferentDaemon(t *testing.T) {
	driver := newTestContainerdRuntime()
	require.ErrorContains(t, driver.verifyOldGPUOwnerAbsent(context.Background(), gpu.Lease{Endpoint: "/other/containerd.sock"}), "different containerd endpoint")
}

func TestSetGPUManagerContractAndMissingCDI(t *testing.T) {
	gvisorDriver := newWithConfig(apiv1alpha2.RuntimeGVisor, "test-profile-hash", RuntimeConfig{})
	manager, err := gpu.New(t.TempDir(), []string{testGPUUUID}, 1, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	defer manager.Close()

	// Non-container runtime must be rejected.
	require.ErrorContains(t, gvisorDriver.SetGPUManager(manager), "unconfigured container runtime")

	driver := newTestContainerdRuntime()
	// Nil manager must be rejected.
	require.ErrorContains(t, driver.SetGPUManager(nil), "unconfigured container runtime")

	// Missing CDI definition in cache must fail Fastlet initialization.
	emptyCDIDir := t.TempDir()
	require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(emptyCDIDir), cdilib.WithAutoRefresh(false)))
	t.Cleanup(func() {
		require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(cdilib.DefaultSpecDirs...), cdilib.WithAutoRefresh(true)))
	})
	require.ErrorContains(t, driver.SetGPUManager(manager), "assigned GPU has no CDI definition")

	// Setting up a valid CDI spec allows SetGPUManager to succeed.
	document := map[string]any{"cdiVersion": "0.5.0", "kind": "nvidia.com/gpu", "devices": []any{
		map[string]any{"name": testGPUUUID, "containerEdits": map[string]any{"env": []string{"ASSIGNED_GPU=one"}}},
	}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(emptyCDIDir, "nvidia.json"), raw, 0600))
	require.NoError(t, driver.SetGPUManager(manager))

	// Calling SetGPUManager again must fail (cannot reconfigure).
	require.ErrorContains(t, driver.SetGPUManager(manager), "unconfigured container runtime")
}

func TestMultiGPUSpecOptAndDeviceIsolation(t *testing.T) {
	const (
		gpuUUID1 = "GPU-00000000-0000-0000-0000-000000000001"
		gpuUUID2 = "GPU-00000000-0000-0000-0000-000000000002"
		gpuUUID3 = "GPU-00000000-0000-0000-0000-000000000003"
	)
	directory := t.TempDir()
	document := map[string]any{"cdiVersion": "0.5.0", "kind": "nvidia.com/gpu", "devices": []any{
		map[string]any{"name": gpuUUID1, "containerEdits": map[string]any{
			"env":         []string{"ASSIGNED_GPU_0=one"},
			"deviceNodes": []any{map[string]any{"path": "/dev/nvidia0", "hostPath": "/dev/null"}},
		}},
		map[string]any{"name": gpuUUID2, "containerEdits": map[string]any{
			"env":         []string{"ASSIGNED_GPU_1=two"},
			"deviceNodes": []any{map[string]any{"path": "/dev/nvidia1", "hostPath": "/dev/null"}},
		}},
		map[string]any{"name": gpuUUID3, "containerEdits": map[string]any{
			"env":         []string{"UNASSIGNED_GPU=three"},
			"deviceNodes": []any{map[string]any{"path": "/dev/nvidia2", "hostPath": "/dev/null"}},
		}},
	}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "nvidia.json"), raw, 0600))
	require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(directory), cdilib.WithAutoRefresh(false)))
	t.Cleanup(func() {
		require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(cdilib.DefaultSpecDirs...), cdilib.WithAutoRefresh(true)))
	})

	manager, err := gpu.New(t.TempDir(), []string{gpuUUID1, gpuUUID2}, 2, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	defer manager.Close()

	driver := newTestContainerdRuntime()
	require.NoError(t, driver.SetGPUManager(manager))

	identity := fastletapi.SandboxIdentity{
		SandboxUID: "multi-gpu-sandbox", FastletPodUID: "pod-a", RuntimeInstanceID: "runtime-a",
		InstanceGeneration: 1, AssignmentAttempt: 1,
	}
	allocation, err := manager.Acquire(identity)
	require.NoError(t, err)
	require.Len(t, allocation.Devices, 2)

	spec := &oci.Spec{Process: &specs.Process{Env: []string{"BASE=true"}}}
	require.NoError(t, gpuSpecOpt(allocation)(context.Background(), nil, nil, spec))

	require.Contains(t, spec.Process.Env, "BASE=true")
	require.Contains(t, spec.Process.Env, "ASSIGNED_GPU_0=one")
	require.Contains(t, spec.Process.Env, "ASSIGNED_GPU_1=two")
	require.NotContains(t, spec.Process.Env, "UNASSIGNED_GPU=three")

	require.Len(t, spec.Linux.Devices, 2)
	paths := []string{spec.Linux.Devices[0].Path, spec.Linux.Devices[1].Path}
	require.Contains(t, paths, "/dev/nvidia0")
	require.Contains(t, paths, "/dev/nvidia1")
	require.NotContains(t, paths, "/dev/nvidia2")
}

func TestValidateGPUAllocationValidationBranches(t *testing.T) {
	driver := newTestContainerdRuntime()

	// 1. When GPU manager is not configured:
	// CPU-only allocations are allowed.
	require.NoError(t, driver.validateGPUAllocation(nil, fastletapi.RuntimeAllocation{}))
	// Non-nil GPU allocation without a manager is rejected.
	require.ErrorContains(t, driver.validateGPUAllocation(nil, fastletapi.RuntimeAllocation{
		GPU: &fastletapi.GPUAllocation{Devices: []string{"nvidia.com/gpu=GPU-test"}},
	}), "requires a configured GPU manager")

	// 2. Configure GPU manager
	directory := t.TempDir()
	document := map[string]any{"cdiVersion": "0.5.0", "kind": "nvidia.com/gpu", "devices": []any{
		map[string]any{"name": testGPUUUID, "containerEdits": map[string]any{"env": []string{"ASSIGNED_GPU=one"}}},
	}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "nvidia.json"), raw, 0600))
	require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(directory), cdilib.WithAutoRefresh(false)))
	t.Cleanup(func() {
		require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(cdilib.DefaultSpecDirs...), cdilib.WithAutoRefresh(true)))
	})

	manager, err := gpu.New(t.TempDir(), []string{testGPUUUID}, 1, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, driver.SetGPUManager(manager))

	identity := fastletapi.SandboxIdentity{
		SandboxUID: "sb-1", FastletPodUID: "pod-a", RuntimeInstanceID: "rt-1",
		InstanceGeneration: 1, AssignmentAttempt: 1,
	}
	input := &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{Identity: identity}}

	// input == nil is rejected
	require.ErrorContains(t, driver.validateGPUAllocation(nil, fastletapi.RuntimeAllocation{}), "reserved GPU allocation")

	// allocation.GPU == nil is rejected when manager is configured
	require.ErrorContains(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{}), "reserved GPU allocation")

	// sandbox has no durable reservation yet
	alloc := &fastletapi.GPUAllocation{Devices: []string{"nvidia.com/gpu=" + testGPUUUID}}
	require.ErrorContains(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{GPU: alloc}), "not durably reserved")

	// Acquire valid lease
	acquired, err := manager.Acquire(identity)
	require.NoError(t, err)

	// Valid allocation passes
	require.NoError(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{GPU: acquired}))

	// Identity mismatch: FastletPodUID
	badPod := *input
	badPod.Sandbox.Identity.FastletPodUID = "pod-other"
	require.ErrorContains(t, driver.validateGPUAllocation(&badPod, fastletapi.RuntimeAllocation{GPU: acquired}), "does not match its durable runtime owner")

	// Identity mismatch: InstanceGeneration
	badGen := *input
	badGen.Sandbox.Identity.InstanceGeneration = 2
	require.ErrorContains(t, driver.validateGPUAllocation(&badGen, fastletapi.RuntimeAllocation{GPU: acquired}), "does not match its durable runtime owner")

	// Identity mismatch: AssignmentAttempt
	badAttempt := *input
	badAttempt.Sandbox.Identity.AssignmentAttempt = 2
	require.ErrorContains(t, driver.validateGPUAllocation(&badAttempt, fastletapi.RuntimeAllocation{GPU: acquired}), "does not match its durable runtime owner")

	// Tampered devices slice
	require.ErrorContains(t, driver.validateGPUAllocation(input, fastletapi.RuntimeAllocation{
		GPU: &fastletapi.GPUAllocation{Devices: []string{"nvidia.com/gpu=GPU-other"}},
	}), "does not match its durable runtime owner")
}

func TestDriverCloseClosesGPUManager(t *testing.T) {
	directory := t.TempDir()
	document := map[string]any{"cdiVersion": "0.5.0", "kind": "nvidia.com/gpu", "devices": []any{
		map[string]any{"name": testGPUUUID, "containerEdits": map[string]any{"env": []string{"ASSIGNED_GPU=one"}}},
	}}
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "nvidia.json"), raw, 0600))
	require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(directory), cdilib.WithAutoRefresh(false)))
	t.Cleanup(func() {
		require.NoError(t, cdilib.Configure(cdilib.WithSpecDirs(cdilib.DefaultSpecDirs...), cdilib.WithAutoRefresh(true)))
	})

	manager, err := gpu.New(t.TempDir(), []string{testGPUUUID}, 1, "pod-a", "k8s.io", gpu.DefaultContainerdSocket)
	require.NoError(t, err)

	driver := newTestContainerdRuntime()
	require.NoError(t, driver.SetGPUManager(manager))

	require.NoError(t, driver.Close())

	// Manager should now be poisoned/closed; acquiring a device should fail.
	identity := fastletapi.SandboxIdentity{SandboxUID: "sb-closed"}
	_, err = manager.Acquire(identity)
	require.ErrorContains(t, err, "GPU manager is closed")
}
