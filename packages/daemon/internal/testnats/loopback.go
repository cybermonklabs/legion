package testnats

import (
	"context"
	"fmt"
	"net"

	"github.com/docker/go-connections/nat"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// clientPort and monitorPort are the container ports start publishes: the one a client dials and
// the monitoring endpoint's.
const (
	clientPort  nat.Port = "4222/tcp"
	monitorPort nat.Port = "8222/tcp"
)

// endpoint is the host:port Docker published container's port on, the loopback address and port
// start chose (testport.LoopbackPorts). Docker publishes a port again at each start of a container,
// so a caller reads it once the container runs.
func endpoint(container *tcnats.NATSContainer, port nat.Port) (string, error) {
	info, err := container.Inspect(context.Background())
	if err != nil {
		return "", fmt.Errorf("inspect the NATS container: %w", err)
	}
	if info.NetworkSettings == nil || len(info.NetworkSettings.Ports[port]) == 0 {
		return "", fmt.Errorf("the NATS container publishes no %s", port)
	}
	binding := info.NetworkSettings.Ports[port][0]
	return net.JoinHostPort(binding.HostIP, binding.HostPort), nil
}
