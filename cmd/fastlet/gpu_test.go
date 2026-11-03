package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/fastlet/gpu"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

type mockDriverWithoutGPU struct {
	runtimecontract.Driver
}

type mockDriverWithGPU struct {
	runtimecontract.Driver
	manager *gpu.Manager
}

func (m *mockDriverWithGPU) SetGPUManager(manager *gpu.Manager) error {
	m.manager = manager
	return nil
}

func TestConfigureGPUContracts(t *testing.T) {
	ctx := context.Background()

	// 1. CPU-only profile (GPUs == 0) returns nil immediately
	require.NoError(t, configureGPU(ctx, &mockDriverWithGPU{}, apiv1alpha2.SandboxResourceProfile{GPUs: 0}, apiv1alpha2.RuntimeContainer, "k8s.io", "/run/containerd/containerd.sock"))

	gpuProfile := apiv1alpha2.SandboxResourceProfile{GPUs: 1}

	// 2. Non-container runtime (e.g. RuntimeGVisor) rejected
	err := configureGPU(ctx, &mockDriverWithGPU{}, gpuProfile, apiv1alpha2.RuntimeGVisor, "k8s.io", "/run/containerd/containerd.sock")
	require.ErrorIs(t, err, runtimecontract.ErrUnsupportedRuntime)

	// 3. Driver that does not implement SetGPUManager rejected
	err = configureGPU(ctx, &mockDriverWithoutGPU{}, gpuProfile, apiv1alpha2.RuntimeContainer, "k8s.io", "/run/containerd/containerd.sock")
	require.ErrorIs(t, err, runtimecontract.ErrUnsupportedRuntime)

	// 4. Missing FAST_SANDBOX_GPU_EXPECTED_DEVICES
	t.Setenv("FASTLET_CAPACITY", "2")
	t.Setenv("POD_UID", "pod-uid-test")
	t.Setenv("FAST_SANDBOX_GPU_EXPECTED_DEVICES", "")
	err = configureGPU(ctx, &mockDriverWithGPU{}, gpuProfile, apiv1alpha2.RuntimeContainer, "k8s.io", "/run/containerd/containerd.sock")
	require.ErrorIs(t, err, runtimecontract.ErrInvalidConfig)

	// 5. Missing POD_UID
	t.Setenv("POD_UID", "")
	t.Setenv("FAST_SANDBOX_GPU_EXPECTED_DEVICES", "2")
	err = configureGPU(ctx, &mockDriverWithGPU{}, gpuProfile, apiv1alpha2.RuntimeContainer, "k8s.io", "/run/containerd/containerd.sock")
	require.ErrorIs(t, err, runtimecontract.ErrInvalidConfig)

	// 6. Mismatched device count (capacity 2 * GPUs 1 = 2, but expected is set to 4)
	t.Setenv("POD_UID", "pod-uid-test")
	t.Setenv("FAST_SANDBOX_GPU_EXPECTED_DEVICES", "4")
	err = configureGPU(ctx, &mockDriverWithGPU{}, gpuProfile, apiv1alpha2.RuntimeContainer, "k8s.io", "/run/containerd/containerd.sock")
	require.ErrorIs(t, err, runtimecontract.ErrInvalidConfig)

	// 7. Valid environment matches capacity * GPUs, but kubelet socket does not exist
	t.Setenv("FAST_SANDBOX_GPU_EXPECTED_DEVICES", "2")
	t.Setenv("NAMESPACE", "default")
	t.Setenv("POD_NAME", "fastlet-test-pod")
	t.Setenv("FAST_SANDBOX_GPU_CONTAINER_NAME", "fastlet")
	err = configureGPU(ctx, &mockDriverWithGPU{}, gpuProfile, apiv1alpha2.RuntimeContainer, "k8s.io", "/run/containerd/containerd.sock")
	require.Error(t, err)
	require.NotErrorIs(t, err, runtimecontract.ErrInvalidConfig)
	require.NotErrorIs(t, err, runtimecontract.ErrUnsupportedRuntime)
}
