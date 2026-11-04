package containerd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/fastlet/gpu"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// This exercises the direct containerd driver, including real CUDA and recovery.
// It does not claim to validate Kubernetes device-plugin allocation or WSL GPU isolation.
func TestGPUContainerdCUDA(t *testing.T) {
	devices := os.Getenv("FSB_GPU_E2E_DEVICES")
	if devices == "" {
		t.Skip("set FSB_GPU_E2E_DEVICES to explicitly assigned GPU UUIDs")
	}
	require.Equal(t, "linux", runtime.GOOS, "real containerd GPU validation requires Linux or WSL2")
	require.Zero(t, os.Geteuid(), "run on an authorized GPU test host as root")
	image := os.Getenv("FSB_GPU_E2E_IMAGE")
	require.NotEmpty(t, image, "set FSB_GPU_E2E_IMAGE to a pre-pulled image containing Python and CUDA-enabled PyTorch")
	socket := os.Getenv("FSB_GPU_E2E_SOCKET")
	if socket == "" {
		socket = "/run/containerd/containerd.sock"
	}
	snapshotter := os.Getenv("FSB_GPU_E2E_SNAPSHOTTER")
	if snapshotter == "" {
		snapshotter = "overlayfs"
	}
	namespace := fmt.Sprintf("fsb-gpu-e2e-%d", time.Now().UnixNano())
	t.Setenv("POD_UID", "")
	t.Setenv("POD_NAME", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	driver := newWithConfig(apiv1alpha2.RuntimeContainer, "gpu-e2e-profile", RuntimeConfig{Handler: "io.containerd.runc.v2", Namespace: namespace, Snapshotter: snapshotter})
	require.NoError(t, driver.Initialize(ctx, socket))
	driver.fastletPodUID = namespace
	driver.SetNamespace("gpu-e2e")
	ids := strings.Split(devices, ",")
	root := t.TempDir()
	manager, err := gpu.New(root, ids, 1, namespace, namespace, socket)
	require.NoError(t, err)
	require.NoError(t, driver.SetGPUManager(manager))
	// Reuse an explicitly pre-pulled image without another registry resolution.
	copyGPUHardwareImage(t, driver, ctx, namespace, image)
	uid := namespace + "-sandbox"
	input := &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{
		Identity: fastletapi.SandboxIdentity{SandboxUID: uid, Name: uid, Namespace: "gpu-e2e", FastletPodUID: namespace, RuntimeInstanceID: uid + "-instance", InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1},
		Desired:  fastletapi.SandboxDesiredSpec{Image: image, RuntimeProfileHash: "gpu-e2e-profile", ResourceProfileHash: "gpu-e2e-resource", Command: []string{"python", "-c", `import torch,time,signal,sys; signal.signal(signal.SIGTERM,lambda *_:sys.exit(0)); assert torch.cuda.is_available(); assert torch.cuda.device_count()==1; x=torch.ones((32,32),device='cuda'); y=x@x; torch.cuda.synchronize(); assert y.sum().item()==32768; print('FSB_GPU_CUDA_OK',torch.cuda.get_device_name(0),flush=True); time.sleep(240)`}},
		Limits:   fastletapi.SandboxRuntimeLimits{CPU: "1", Memory: "4Gi", PIDs: 512},
	}}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := driver.DeleteSandbox(cleanupCtx, uid); err != nil {
			t.Errorf("GPU runtime cleanup failed: %v", err)
		}
		imageCtx := namespaces.WithNamespace(cleanupCtx, namespace)
		if err := driver.client.ImageService().Delete(imageCtx, image, images.SynchronousDelete()); err != nil {
			t.Errorf("remove test image reference: %v", err)
		}
		if err := driver.client.NamespaceService().Delete(cleanupCtx, namespace); err != nil {
			t.Errorf("remove test namespace: %v", err)
		}
		_ = driver.Close()
		_ = os.Remove(filepath.Join("/var/log/fast-sandbox", uid+".log"))
	})
	first, err := driver.EnsureSandbox(ctx, input)
	require.NoError(t, err)
	require.Len(t, first.Allocation.GPU.Devices, 1)
	logPath := filepath.Join("/var/log/fast-sandbox", uid+".log")
	require.Eventually(t, func() bool { raw, _ := os.ReadFile(logPath); return strings.Contains(string(raw), "FSB_GPU_CUDA_OK") }, 90*time.Second, 250*time.Millisecond, "CUDA kernel never completed; inspect %s", logPath)
	duplicate, err := driver.EnsureSandbox(ctx, input)
	require.NoError(t, err)
	require.Equal(t, first.PID, duplicate.PID)
	require.Equal(t, first.Allocation.GPU, duplicate.Allocation.GPU)
	// Drop/reload the journal manager as a Fastlet process restart would; retain the task.
	require.NoError(t, manager.Close())
	driver.gpuManager = nil
	manager, err = gpu.New(root, ids, 1, namespace, namespace, socket)
	require.NoError(t, err)
	require.NoError(t, driver.SetGPUManager(manager))
	managed, err := driver.ListManagedSandboxes(ctx)
	require.NoError(t, err)
	require.Len(t, managed, 1)
	require.NoError(t, driver.RecoverRuntimeResources(ctx, managed, nil))
	require.Len(t, manager.Leases(), 1)
	recovered, err := driver.EnsureSandbox(ctx, input)
	require.NoError(t, err)
	require.Equal(t, first.PID, recovered.PID)
	require.NoError(t, driver.DeleteSandbox(ctx, uid))
	require.Empty(t, manager.Leases())
	require.True(t, manager.Available())
	// Confirm no task/container remains before accepting successful reclamation.
	containers, err := driver.client.Containers(namespaces.WithNamespace(ctx, namespace))
	require.NoError(t, err)
	require.Empty(t, containers)
}
