package gpu

import (
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
