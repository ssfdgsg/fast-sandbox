package gpu

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1"
)

func TestKubeletDiscoveryUsesOnlyOwnedExclusiveDevices(t *testing.T) {
	pod := func(name, id string) *podresources.PodResources {
		return &podresources.PodResources{Name: name, Namespace: "ns", Containers: []*podresources.ContainerResources{{Name: "fastlet", Devices: []*podresources.ContainerDevices{{ResourceName: ResourceName, DeviceIds: []string{id}}}}}}
	}
	response := &podresources.ListPodResourcesResponse{PodResources: []*podresources.PodResources{pod("ours", deviceA), pod("other", deviceB)}}
	ids, err := devicesForContainer(response, "ns", "ours", "fastlet", 1)
	require.NoError(t, err)
	require.Equal(t, []string{deviceA}, ids)
	_, err = devicesForContainer(response, "ns", "ours", "sidecar", 1)
	require.Error(t, err)
	_, err = devicesForContainer(response, "ns", "ours", "fastlet", 2)
	require.Error(t, err)
	response.PodResources[1] = pod("other", deviceA)
	_, err = devicesForContainer(response, "ns", "ours", "fastlet", 1)
	require.ErrorContains(t, err, "unique")
	response.PodResources = []*podresources.PodResources{pod("ours", "0")}
	_, err = devicesForContainer(response, "ns", "ours", "fastlet", 1)
	require.Error(t, err)
}

func TestAssignedDevicesParameterValidation(t *testing.T) {
	ctx := context.Background()

	// Empty socket
	_, err := AssignedDevices(ctx, "", "ns", "pod", "ctr", 1)
	require.ErrorContains(t, err, "GPU discovery requires kubelet socket")

	// Empty namespace
	_, err = AssignedDevices(ctx, "/path/socket", "", "pod", "ctr", 1)
	require.ErrorContains(t, err, "GPU discovery requires kubelet socket")

	// Empty pod
	_, err = AssignedDevices(ctx, "/path/socket", "ns", "", "ctr", 1)
	require.ErrorContains(t, err, "GPU discovery requires kubelet socket")

	// Empty container
	_, err = AssignedDevices(ctx, "/path/socket", "ns", "pod", "", 1)
	require.ErrorContains(t, err, "GPU discovery requires kubelet socket")

	// Expected count <= 0
	_, err = AssignedDevices(ctx, "/path/socket", "ns", "pod", "ctr", 0)
	require.ErrorContains(t, err, "GPU discovery requires kubelet socket")

	// Non-existent socket fails dial
	_, err = AssignedDevices(ctx, "/tmp/non-existent-kubelet.sock", "ns", "pod", "ctr", 1)
	require.Error(t, err)
}

func TestDevicesForContainerSortsAndIgnoresForeignResourceNames(t *testing.T) {
	response := &podresources.ListPodResourcesResponse{
		PodResources: []*podresources.PodResources{
			{
				Name:      "ours",
				Namespace: "ns",
				Containers: []*podresources.ContainerResources{
					{
						Name: "fastlet",
						Devices: []*podresources.ContainerDevices{
							// NVIDIA GPU devices assigned in reverse order
							{ResourceName: ResourceName, DeviceIds: []string{deviceB, deviceA}},
							// Foreign device from a different device plugin
							{ResourceName: "intel.com/qat", DeviceIds: []string{"qat-0001"}},
						},
					},
				},
			},
		},
	}

	// Should ignore intel.com/qat and sort deviceA and deviceB
	ids, err := devicesForContainer(response, "ns", "ours", "fastlet", 2)
	require.NoError(t, err)
	require.Equal(t, []string{deviceA, deviceB}, ids)

	// Fails if expected count does not match NVIDIA count
	_, err = devicesForContainer(response, "ns", "ours", "fastlet", 3)
	require.ErrorContains(t, err, "kubelet assigned 2 GPUs; expected 3")

	// Fails if a duplicate device appears within the container
	dupResponse := &podresources.ListPodResourcesResponse{
		PodResources: []*podresources.PodResources{
			{
				Name:      "ours",
				Namespace: "ns",
				Containers: []*podresources.ContainerResources{
					{
						Name: "fastlet",
						Devices: []*podresources.ContainerDevices{
							{ResourceName: ResourceName, DeviceIds: []string{deviceA, deviceA}},
						},
					},
				},
			},
		},
	}
	_, err = devicesForContainer(dupResponse, "ns", "ours", "fastlet", 2)
	require.ErrorContains(t, err, "unique full-device UUID allocation")
}
