package reconciler

import (
	"fmt"
	"path/filepath"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/runtimeenv"
)

// A GPU Pool has a fixed device cost per slot; both existing slot scheduling and
// Pool scaling therefore use the same Kubernetes-reserved device capacity.
func applyGPUResources(pool *apiv1alpha2.SandboxPool, plan runtimeenv.ResolvedRuntimePlan, pod *corev1.PodSpec) error {
	count := pool.Spec.SandboxResources.GPUs
	if count == 0 {
		return nil
	}
	if err := pool.Spec.ValidateGPUContract(); err != nil {
		return err
	}
	if plan.Kubelet.Root == "" {
		return fmt.Errorf("GPU Pool requires a kubelet root in the runtime environment")
	}
	owner := &pod.Containers[0]
	total := int64(count) * int64(pool.Spec.MaxSandboxesPerPod)
	quantity := *resource.NewQuantity(total, resource.DecimalSI)
	if owner.Resources.Requests == nil {
		owner.Resources.Requests = corev1.ResourceList{}
	}
	if owner.Resources.Limits == nil {
		owner.Resources.Limits = corev1.ResourceList{}
	}
	owner.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")] = quantity
	owner.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")] = quantity
	owner.Env = append(owner.Env, corev1.EnvVar{Name: "FAST_SANDBOX_GPU_CONTAINER_NAME", Value: owner.Name}, corev1.EnvVar{Name: "FAST_SANDBOX_GPU_EXPECTED_DEVICES", Value: strconv.FormatInt(total, 10)})
	directory := corev1.HostPathDirectory
	create := corev1.HostPathDirectoryOrCreate
	requirements := []runtimecatalog.HostPathRequirement{
		{Name: "gpu-pod-resources", HostPath: filepath.Join(plan.Kubelet.Root, "pod-resources"), MountPath: "/run/fast-sandbox/pod-resources", Type: directory, ReadOnly: true},
		{Name: "gpu-cdi-etc", HostPath: "/etc/cdi", MountPath: "/etc/cdi", Type: create, ReadOnly: true},
		{Name: "gpu-cdi-run", HostPath: "/var/run/cdi", MountPath: "/var/run/cdi", Type: create, ReadOnly: true},
		{Name: "gpu-leases", HostPath: "/var/lib/fast-sandbox/gpu", MountPath: "/var/lib/fast-sandbox/gpu", Type: create},
	}
	return mergeRuntimeHostPaths(pod, owner, requirements)
}
