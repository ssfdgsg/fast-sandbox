package gpu

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	podresources "k8s.io/kubelet/pkg/apis/podresources/v1"
)

// AssignedDevices returns only devices allocated by kubelet to this container.
// UUID strategy and exclusive device-plugin allocation are required.
func AssignedDevices(ctx context.Context, socket, namespace, pod, container string, expected int) ([]string, error) {
	if socket == "" || namespace == "" || pod == "" || container == "" || expected <= 0 {
		return nil, fmt.Errorf("GPU discovery requires kubelet socket, Pod/container identity and expected count")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient("passthrough:///pod-resources", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	response, err := podresources.NewPodResourcesListerClient(conn).List(ctx, &podresources.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("query kubelet GPU allocation: %w", err)
	}
	return devicesForContainer(response, namespace, pod, container, expected)
}

func devicesForContainer(response *podresources.ListPodResourcesResponse, namespace, pod, container string, expected int) ([]string, error) {
	var assigned []string
	consumers := map[string]int{}
	for _, p := range response.GetPodResources() {
		for _, c := range p.GetContainers() {
			for _, d := range c.GetDevices() {
				if d.GetResourceName() != ResourceName {
					continue
				}
				for _, id := range d.GetDeviceIds() {
					consumers[id]++
				}
				if p.GetNamespace() == namespace && p.GetName() == pod && c.GetName() == container {
					assigned = append(assigned, d.GetDeviceIds()...)
				}
			}
		}
	}
	if len(assigned) != expected {
		return nil, fmt.Errorf("kubelet assigned %d GPUs; expected %d for %s/%s/%s", len(assigned), expected, namespace, pod, container)
	}
	for _, id := range assigned {
		if !uuidPattern.MatchString(id) || consumers[id] != 1 {
			return nil, fmt.Errorf("GPU %q is not a unique full-device UUID allocation; sharing and MIG are unsupported", id)
		}
	}
	slices.Sort(assigned)
	return assigned, nil
}
