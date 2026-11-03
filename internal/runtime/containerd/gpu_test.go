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
