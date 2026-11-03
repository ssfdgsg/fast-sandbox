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
	pool.Spec.Runtime = apiv1alpha2.RuntimeContainer
	pool.Spec.FastletTemplate.Spec.Containers[0].Resources.Limits = nil
	require.ErrorContains(t, applyGPUResources(pool, runtimeenv.ResolvedRuntimePlan{}, pod), "kubelet root")
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

func TestGPUResourcesRejectsConflictingVolumesAndMounts(t *testing.T) {
	pool := resourceAwareScalingPool()
	pool.Spec.Runtime = apiv1alpha2.RuntimeContainer
	pool.Spec.SandboxResources.GPUs = 1
	pool.Spec.MaxSandboxesPerPod = 1
	pool.Spec.FastletTemplate.Spec.Containers = []corev1.Container{{Name: "fastlet"}}
	plan := runtimeenv.ResolvedRuntimePlan{Kubelet: runtimeenv.KubeletEnvironment{Root: "/var/lib/kubelet"}}

	// Conflicting volume host path
	conflictingVolPod := pool.Spec.FastletTemplate.Spec.DeepCopy()
	conflictingVolPod.Volumes = []corev1.Volume{{
		Name:         "gpu-pod-resources",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/different/path"}},
	}}
	require.ErrorContains(t, applyGPUResources(pool, plan, conflictingVolPod), "conflicts with runtime host path")

	// Conflicting mount path used by a different volume
	conflictingMountPod := pool.Spec.FastletTemplate.Spec.DeepCopy()
	conflictingMountPod.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name:      "user-vol",
		MountPath: "/run/fast-sandbox/pod-resources",
	}}
	require.ErrorContains(t, applyGPUResources(pool, plan, conflictingMountPod), "is reserved by runtime volume")

	// Conflicting mount mode (read-write when read-only is required)
	conflictingModePod := pool.Spec.FastletTemplate.Spec.DeepCopy()
	conflictingModePod.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name:      "gpu-pod-resources",
		MountPath: "/run/fast-sandbox/pod-resources",
		ReadOnly:  false,
	}}
	require.ErrorContains(t, applyGPUResources(pool, plan, conflictingModePod), "conflicts with runtime mount")
}

func TestGPUResourcesVerifiesHostPathRequirements(t *testing.T) {
	pool := resourceAwareScalingPool()
	pool.Spec.Runtime = apiv1alpha2.RuntimeContainer
	pool.Spec.SandboxResources.GPUs = 2
	pool.Spec.MaxSandboxesPerPod = 2
	pool.Spec.FastletTemplate.Spec.Containers = []corev1.Container{{Name: "fastlet"}}
	pod := pool.Spec.FastletTemplate.Spec.DeepCopy()
	plan := runtimeenv.ResolvedRuntimePlan{Kubelet: runtimeenv.KubeletEnvironment{Root: "/var/lib/kubelet"}}

	require.NoError(t, applyGPUResources(pool, plan, pod))

	expectedVolumes := map[string]struct {
		hostPath string
		volType  corev1.HostPathType
		readOnly bool
	}{
		"gpu-pod-resources": {"/var/lib/kubelet/pod-resources", corev1.HostPathDirectory, true},
		"gpu-cdi-etc":       {"/etc/cdi", corev1.HostPathDirectoryOrCreate, true},
		"gpu-cdi-run":       {"/var/run/cdi", corev1.HostPathDirectoryOrCreate, true},
		"gpu-leases":        {"/var/lib/fast-sandbox/gpu", corev1.HostPathDirectoryOrCreate, false},
	}

	for _, vol := range pod.Volumes {
		exp, ok := expectedVolumes[vol.Name]
		if !ok {
			continue
		}
		require.NotNil(t, vol.HostPath)
		require.Equal(t, exp.hostPath, vol.HostPath.Path)
		require.Equal(t, exp.volType, *vol.HostPath.Type)
	}

	for _, mount := range pod.Containers[0].VolumeMounts {
		exp, ok := expectedVolumes[mount.Name]
		if !ok {
			continue
		}
		require.Equal(t, exp.readOnly, mount.ReadOnly)
		delete(expectedVolumes, mount.Name)
	}
	require.Empty(t, expectedVolumes)
}

func TestRemoveRuntimeOwnedEnvStripsGPUEnvVars(t *testing.T) {
	env := []corev1.EnvVar{
		{Name: "CUSTOM_USER_VAR", Value: "custom_val"},
		{Name: "FAST_SANDBOX_GPU_CONTAINER_NAME", Value: "fastlet"},
		{Name: "FAST_SANDBOX_GPU_EXPECTED_DEVICES", Value: "4"},
		{Name: "FAST_SANDBOX_GPU_CUSTOM_EXTRA", Value: "extra"},
		{Name: "NODE_NAME", Value: "node-1"},
		{Name: "ANOTHER_USER_VAR", Value: "val-2"},
	}

	cleaned := removeRuntimeOwnedEnv(env)
	require.Len(t, cleaned, 2)
	require.Equal(t, "CUSTOM_USER_VAR", cleaned[0].Name)
	require.Equal(t, "ANOTHER_USER_VAR", cleaned[1].Name)
}
