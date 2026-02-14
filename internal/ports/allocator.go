package ports

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/UDL-TF/TourneyController/internal/config"
)

// Assignment represents a concrete set of NodePorts for a server.
type Assignment struct {
	Game     int
	SourceTV int
	Client   int
	Steam    int
}

// Allocator tracks which ranges are reserved for each port type.
type Allocator struct {
	ranges config.PortsConfig
}

// NewAllocator builds a range-aware Allocator.
func NewAllocator(ranges config.PortsConfig) *Allocator {
	return &Allocator{ranges: ranges}
}

// Allocate returns the next free port in each configured range.
func (a *Allocator) Allocate(ctx context.Context, svcClient corev1client.ServiceInterface) (Assignment, error) {
	svcList, err := svcClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return Assignment{}, fmt.Errorf("list services: %w", err)
	}

	used := map[int]struct{}{}
	for _, svc := range svcList.Items {
		for _, port := range svc.Spec.Ports {
			if port.NodePort > 0 {
				used[int(port.NodePort)] = struct{}{}
			}
		}
	}

	assign := Assignment{}
	if assign.Game, err = a.nextFree(a.ranges.Game, used); err != nil {
		return Assignment{}, err
	}
	if assign.SourceTV, err = a.nextFree(a.ranges.SourceTV, used); err != nil {
		return Assignment{}, err
	}
	if assign.Client, err = a.nextFree(a.ranges.Client, used); err != nil {
		return Assignment{}, err
	}
	if assign.Steam, err = a.nextFree(a.ranges.Steam, used); err != nil {
		return Assignment{}, err
	}

	return assign, nil
}

func (a *Allocator) nextFree(pr config.PortRange, used map[int]struct{}) (int, error) {
	for port := pr.Start; port <= pr.End; port++ {
		if _, exists := used[port]; exists {
			continue
		}
		used[port] = struct{}{}
		return port, nil
	}
	return 0, fmt.Errorf("no free ports available in range %d-%d", pr.Start, pr.End)
}
