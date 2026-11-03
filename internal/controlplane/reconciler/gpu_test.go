package reconciler

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/runtimeenv"
)

func TestGPUResourcesDerivedFromSlotsAndUseEnvironmentKubelet(t *testing.T) {
	pool := resourceAwareScalingPool()
	pool.Spec.Runtime = apiv1alpha2.RuntimeContainer
	pool.Spec.SandboxResources.GPUs = 2
	pool.Spec.MaxSandboxesPerPod = 3
	pool.Spec.FastletTemplate.Spec.Containers = []corev1.Container{{Name: "fastlet"}}
	pod := pool.Spec.FastletTemplate.Spec.DeepCopy()
	plan := runtimeenv.ResolvedRuntimePlan{Kubelet: runtimeenv.KubeletEnvironment{Root: "/custom/kubelet"}}
	require.NoError(t, applyGPUResources(pool, plan, pod))
	key := corev1.ResourceName("nvidia.com/gpu")
	q := pod.Containers[0].Resources.Requests[key]
	require.Equal(t, int64(6), q.Value())
	q = pod.Containers[0].Resources.Limits[key]
	require.Equal(t, int64(6), q.Value())
	found := false
	for _, volume := range pod.Volumes {
		if volume.Name == "gpu-pod-resources" {
			found = true
			require.Equal(t, "/custom/kubelet/pod-resources", volume.HostPath.Path)
		}
	}
	require.True(t, found)
	pool.Spec.FastletTemplate.Spec.Containers[0].Resources.Limits = corev1.ResourceList{key: resource.MustParse("8")}
	require.Error(t, applyGPUResources(pool, plan, pod))
	pool.Spec.Runtime = apiv1alpha2.RuntimeGVisor
	require.Error(t, pool.Spec.ValidateGPUContract())
}

func TestCPUOnlyPoolDoesNotAcquireGPUResources(t *testing.T) {
	pool := resourceAwareScalingPool()
	pool.Spec.SandboxResources.GPUs = 0
	pod := pool.Spec.FastletTemplate.Spec.DeepCopy()
	before := pod.DeepCopy()
	require.NoError(t, applyGPUResources(pool, runtimeenv.ResolvedRuntimePlan{}, pod))
	require.Equal(t, before, pod)
}

func TestGPUPoolPodCarriesBothDeviceBudgetAndDiscoverySettings(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	pool := resourceAwareScalingPool()
	pool.Spec.Runtime = apiv1alpha2.RuntimeContainer
	pool.Spec.SandboxResources.GPUs = 1
	pool.Spec.MaxSandboxesPerPod = 2
	pool.Spec.FastletTemplate.Spec.Containers = []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}}
	catalog := runtimecatalog.Builtin()
	profile, err := catalog.Resolve(apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: catalog}
	pod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)
	key := corev1.ResourceName("nvidia.com/gpu")
	q := pod.Spec.Containers[0].Resources.Limits[key]
	require.Equal(t, int64(2), q.Value())
	expected := map[string]string{"FAST_SANDBOX_GPU_EXPECTED_DEVICES": "2", "FAST_SANDBOX_GPU_CONTAINER_NAME": "fastlet"}
	for _, env := range pod.Spec.Containers[0].Env {
		if value, exists := expected[env.Name]; exists {
			require.Equal(t, value, env.Value)
			delete(expected, env.Name)
		}
	}
	require.Empty(t, expected)
	require.Equal(t, pool.Spec.SandboxResources.Hash(), pod.Annotations["fast-sandbox.io/resource-profile-hash"])
}
