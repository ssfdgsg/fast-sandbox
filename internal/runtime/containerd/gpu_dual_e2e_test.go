package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/stretchr/testify/require"

	api "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/fastlet/gpu"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	gputest "fast-sandbox/test/gpu"
)

type gpuHardwareFixture struct {
	driver                         *Driver
	manager                        *gpu.Manager
	ctx                            context.Context
	root, namespace, socket, image string
	ids, uids                      []string
}

func gpuHardwareDevices(t *testing.T) []string {
	t.Helper()
	if os.Getenv("FSB_GPU_E2E_MULTI") != "1" {
		t.Skip("set FSB_GPU_E2E_MULTI=1 and reserve exactly two idle GPU UUIDs")
	}
	require.Equal(t, "linux", runtime.GOOS)
	require.Zero(t, os.Geteuid())
	ids := strings.Split(os.Getenv("FSB_GPU_E2E_DEVICES"), ",")
	require.Len(t, ids, 2)
	require.NotEqual(t, ids[0], ids[1])
	require.NotEmpty(t, os.Getenv("FSB_GPU_E2E_IMAGE"), "set image to a preloaded Linux image containing python")
	return ids
}

func newGPUHardwareFixture(t *testing.T, ids []string, root string, count int) *gpuHardwareFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	socket := os.Getenv("FSB_GPU_E2E_SOCKET")
	if socket == "" {
		socket = gpu.DefaultContainerdSocket
	}
	snapshotter := os.Getenv("FSB_GPU_E2E_SNAPSHOTTER")
	if snapshotter == "" {
		snapshotter = "overlayfs"
	}
	ns := fmt.Sprintf("fsb-gpu-e2e-%d", time.Now().UnixNano())
	driver := newWithConfig(api.RuntimeContainer, "gpu-dual-test", RuntimeConfig{Handler: "io.containerd.runc.v2", Namespace: ns, Snapshotter: snapshotter})
	require.NoError(t, driver.Initialize(ctx, socket))
	driver.fastletPodUID = ns
	driver.SetNamespace("gpu-e2e")
	f := &gpuHardwareFixture{driver: driver, ctx: ctx, root: root, namespace: ns, socket: socket, image: os.Getenv("FSB_GPU_E2E_IMAGE"), ids: ids}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		for _, uid := range f.uids {
			// Preserve probe output even after the runtime's ordinary cleanup.
			if raw, err := os.ReadFile(filepath.Join("/var/log/fast-sandbox", uid+".log")); err == nil {
				t.Logf("probe %s: %s", uid, raw)
			}
			if err := driver.DeleteSandbox(c, uid); err != nil {
				t.Errorf("cleanup %s: %v", uid, err)
			}
		}
		containers, err := driver.client.Containers(namespaces.WithNamespace(c, ns))
		if err != nil {
			t.Errorf("final inventory: %v", err)
		} else if len(containers) != 0 {
			t.Errorf("leaked %d containers", len(containers))
		}
		tasks, err := driver.client.TaskService().List(namespaces.WithNamespace(c, ns), &tasksapi.ListTasksRequest{})
		if err != nil {
			t.Errorf("final task inventory: %v", err)
		} else if len(tasks.Tasks) != 0 {
			t.Errorf("leaked %d tasks", len(tasks.Tasks))
		}
		if err := driver.client.ImageService().Delete(namespaces.WithNamespace(c, ns), f.image); err != nil {
			t.Errorf("image reference cleanup: %v", err)
		}
		if err := driver.client.NamespaceService().Delete(c, ns); err != nil {
			t.Errorf("namespace cleanup: %v", err)
		}
		if err := driver.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	source, err := driver.client.GetImage(namespaces.WithNamespace(ctx, "k8s.io"), f.image)
	require.NoError(t, err, "preload the image in k8s.io")
	_, err = driver.client.ImageService().Create(namespaces.WithNamespace(ctx, ns), source.Metadata())
	require.NoError(t, err)
	if count > 0 {
		f.setManager(t, count)
	}
	return f
}

func (f *gpuHardwareFixture) setManager(t *testing.T, count int) {
	t.Helper()
	if f.manager != nil {
		require.NoError(t, f.manager.Close())
		f.driver.gpuManager = nil
	}
	var err error
	f.manager, err = gpu.New(f.root, f.ids, count, f.namespace, f.namespace, f.socket)
	require.NoError(t, err)
	require.NoError(t, f.driver.SetGPUManager(f.manager))
}

func (f *gpuHardwareFixture) input(name string, count int) *fastletapi.EnsureSandboxInput {
	uid := f.namespace + "-" + name
	f.uids = append(f.uids, uid)
	return &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{
		Identity: fastletapi.SandboxIdentity{SandboxUID: uid, Name: uid, Namespace: "gpu-e2e", FastletPodUID: f.namespace, RuntimeInstanceID: uid + "-instance", InstanceGeneration: 1, AssignmentAttempt: 1, RouteGeneration: 1},
		Desired:  fastletapi.SandboxDesiredSpec{Image: f.image, RuntimeProfileHash: "gpu-dual-test", ResourceProfileHash: "gpu-dual-resource", Command: []string{"python", "-u", "-c", gputest.DriverProbe, "--expect-count", fmt.Sprint(count), "--hold-seconds", "600"}},
		Limits:   fastletapi.SandboxRuntimeLimits{CPU: "1", Memory: "1Gi", PIDs: 128},
	}}
}

func assertGPUHardwareProbe(t *testing.T, f *gpuHardwareFixture, input *fastletapi.EnsureSandboxInput, metadata *SandboxMetadata) {
	t.Helper()
	path := filepath.Join("/var/log/fast-sandbox", input.Sandbox.Identity.SandboxUID+".log")
	var payload string
	require.Eventually(t, func() bool {
		raw, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "FSB_GPU_DRIVER_OK ") {
				payload = strings.TrimPrefix(line, "FSB_GPU_DRIVER_OK ")
				return true
			}
		}
		return false
	}, 90*time.Second, 200*time.Millisecond, "CUDA probe failed; inspect %s", path)
	var result struct {
		UUIDs []string `json:"uuids"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &result))
	expected := []string{}
	if metadata.Allocation.GPU != nil {
		for _, device := range metadata.Allocation.GPU.Devices {
			expected = append(expected, strings.TrimPrefix(device, "nvidia.com/gpu="))
		}
	}
	sort.Strings(expected)
	sort.Strings(result.UUIDs)
	require.Equal(t, expected, result.UUIDs, "CUDA/NVML physical devices must equal the durable CDI allocation")
	t.Logf("CUDA allocation verified: %s", payload)
}

// This is native Linux driver-level qualification; it does not claim Kubernetes placement or Fastlet Pod crash coverage.
func TestGPUContainerdDualCUDA(t *testing.T) {
	ids := gpuHardwareDevices(t)
	t.Setenv("POD_UID", "")
	t.Setenv("POD_NAME", "")
	f := newGPUHardwareFixture(t, ids, t.TempDir(), 1)
	a, b := f.input("a", 1), f.input("b", 1)
	type result struct {
		metadata *SandboxMetadata
		err      error
	}
	results := make(chan result, 2)
	for _, input := range []*fastletapi.EnsureSandboxInput{a, b} {
		go func(in *fastletapi.EnsureSandboxInput) {
			m, e := f.driver.EnsureSandbox(f.ctx, in)
			results <- result{m, e}
		}(input)
	}
	firstResult, secondResult := <-results, <-results
	require.NoError(t, firstResult.err)
	require.NoError(t, secondResult.err)
	ma, err := f.driver.EnsureSandbox(f.ctx, a)
	require.NoError(t, err)
	mb, err := f.driver.EnsureSandbox(f.ctx, b)
	require.NoError(t, err)
	require.NotEqual(t, ma.Allocation.GPU.Devices, mb.Allocation.GPU.Devices)
	assertGPUHardwareProbe(t, f, a, ma)
	assertGPUHardwareProbe(t, f, b, mb)
	overflow := f.input("overflow", 1)
	_, err = f.driver.EnsureSandbox(f.ctx, overflow)
	require.ErrorIs(t, err, gpu.ErrExhausted)
	_, err = f.driver.InspectSandbox(f.ctx, overflow.Sandbox.Identity.SandboxUID)
	require.ErrorIs(t, err, ErrSandboxNotFound)
	require.Len(t, f.manager.Leases(), 2)
	// Recover durable labels while both real GPU tasks remain alive.
	f.setManager(t, 1)
	inventory, err := f.driver.ListManagedSandboxes(f.ctx)
	require.NoError(t, err)
	require.Len(t, inventory, 2)
	require.NoError(t, f.driver.RecoverRuntimeResources(f.ctx, inventory, nil))
	again, err := f.driver.EnsureSandbox(f.ctx, a)
	require.NoError(t, err)
	require.Equal(t, ma.PID, again.PID)
	require.NoError(t, f.driver.DeleteSandbox(f.ctx, a.Sandbox.Identity.SandboxUID))
	replacement, err := f.driver.EnsureSandbox(f.ctx, overflow)
	require.NoError(t, err)
	require.Equal(t, ma.Allocation.GPU.Devices, replacement.Allocation.GPU.Devices)
	assertGPUHardwareProbe(t, f, overflow, replacement)
	// A CPU runtime must not enumerate either GPU while GPU work is running.
	cpu := newGPUHardwareFixture(t, nil, t.TempDir(), 0)
	cpuInput := cpu.input("cpu", 0)
	cpuRuntime, err := cpu.driver.EnsureSandbox(cpu.ctx, cpuInput)
	require.NoError(t, err)
	assertGPUHardwareProbe(t, cpu, cpuInput, cpuRuntime)
	require.NoError(t, cpu.driver.DeleteSandbox(cpu.ctx, cpuInput.Sandbox.Identity.SandboxUID))
	for _, input := range []*fastletapi.EnsureSandboxInput{b, overflow} {
		require.NoError(t, f.driver.DeleteSandbox(f.ctx, input.Sandbox.Identity.SandboxUID))
	}
	require.Empty(t, f.manager.Leases())
	// Allocate the same two physical cards to a single Sandbox and execute on each.
	f.setManager(t, 2)
	both := f.input("both", 2)
	bothRuntime, err := f.driver.EnsureSandbox(f.ctx, both)
	require.NoError(t, err)
	require.Len(t, bothRuntime.Allocation.GPU.Devices, 2)
	assertGPUHardwareProbe(t, f, both, bothRuntime)
	require.NoError(t, f.driver.DeleteSandbox(f.ctx, both.Sandbox.Identity.SandboxUID))
	require.Empty(t, f.manager.Leases())
	require.True(t, f.manager.Available())
}

func TestGPUContainerdMultiFastletCUDA(t *testing.T) {
	ids := gpuHardwareDevices(t)
	t.Setenv("POD_UID", "")
	t.Setenv("POD_NAME", "")
	root := t.TempDir()
	a := newGPUHardwareFixture(t, ids[:1], root, 1)
	b := newGPUHardwareFixture(t, ids[1:], root, 1)
	ia, ib := a.input("owner-a", 1), b.input("owner-b", 1)
	ma, err := a.driver.EnsureSandbox(a.ctx, ia)
	require.NoError(t, err)
	mb, err := b.driver.EnsureSandbox(b.ctx, ib)
	require.NoError(t, err)
	assertGPUHardwareProbe(t, a, ia, ma)
	assertGPUHardwareProbe(t, b, ib, mb)
	require.NotEqual(t, ma.Allocation.GPU.Devices, mb.Allocation.GPU.Devices)
	require.NoError(t, a.driver.DeleteSandbox(a.ctx, ia.Sandbox.Identity.SandboxUID))
	require.Len(t, b.manager.Leases(), 1, "other Fastlet journal update must survive deletion")
	require.NoError(t, b.driver.DeleteSandbox(b.ctx, ib.Sandbox.Identity.SandboxUID))
	records, err := a.manager.Records()
	require.NoError(t, err)
	require.Empty(t, records)
}
