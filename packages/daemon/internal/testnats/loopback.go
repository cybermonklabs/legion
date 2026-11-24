package testnats

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"runtime"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go"
)

// LoopbackIP is an address for one test listener a client dials: on Linux, where every address of
// 127.0.0.0/8 is the loopback, one of them chosen at random and never 127.0.0.1, and elsewhere
// 127.0.0.1.
//
// A listener holds its port only while its test runs. A client that test leaves behind keeps
// dialing the address (nats.go reconnects for minutes, a bus client forever), and Docker or the
// kernel can give the same port to a later listener, in the same test binary or in another of
// the binaries go test runs beside it, and that listener's test then takes a stranger's
// connections for its own. The leftover client dials the address its own listener had, so a
// listener on an address of its own is out of its reach, whichever port it gets.
//
// It is a copy of packages/envoy/internal/testport's LoopbackIP, for the reason
// packages/envoy/internal/testtmp gives: Go's internal rule keeps each module from importing the
// other's. Change one and change the other.
func LoopbackIP() net.IP {
	if runtime.GOOS != "linux" {
		return net.IPv4(127, 0, 0, 1).To4()
	}
	return net.IPv4(127, byte(1+rand.IntN(254)), byte(rand.IntN(256)), byte(1+rand.IntN(254))).To4()
}

// ListenLoopback listens on a port the kernel chooses at an address LoopbackIP picks.
func ListenLoopback() (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort(LoopbackIP().String(), "0"))
}

// loopbackPorts answers an address LoopbackIP picks and n ports on it the kernel chose, each the
// first of the range Docker publishes one of a container's ports from (publishedRange). Docker,
// asked for any port on an address, searches from the first port of its range it has not
// published on that address, which on a fresh address is the range's first port, held or not,
// and in envoy-infra-go it took one already held, refusing the start with "failed to listen on
// TCP socket: address already in use". The kernel, asked for port 0, never hands out a port that
// is held, so Docker's search starts at a port that was free a moment before. The probes are
// closed before loopbackPorts returns, so a port can be taken before Docker binds it;
// publishedRange says why the start still gets a port. It is a copy of
// packages/envoy/internal/testport's LoopbackPorts, as LoopbackIP is. It is a variable so a test
// can take a probed port before Docker binds it, as another program or Docker itself can
// (TestAStartPublishesPastAProbedPortTakenBeforeThePublish), and can hand a start a range whose
// every port is held (TestAStartRetriedAfterARefusedPublishEnforcesItsUsers).
var loopbackPorts = func(n int) (string, []int, error) {
	host := LoopbackIP().String()
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	ports := make([]int, 0, n)
	for range n {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return "", nil, err
		}
		listeners = append(listeners, listener)
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
	}
	return host, ports, nil
}

// lastPublishedPort ends every range start publishes a port from (publishedRange).
const lastPublishedPort = 65535

// publishedRange is the host ports Docker may publish a container port on, from first, a port
// loopbackPorts probed, through lastPublishedPort, written as a binding's HostPort. A probed port
// can be taken between the probe and the publish: by another program, or by Docker itself for a
// container another test publishes on every address. Given that one port, Docker refuses the
// start; Docker 29 refuses a port it holds on every address for any address, as "Bind for
// 0.0.0.0:<port> failed: port is already allocated", before it tries to bind (moby v29.9.3
// daemon/libnetwork/portallocator/portallocator.go, RequestPortsInRange). Given a range, Docker
// takes the first port of it that its own record shows free on the address and on every address,
// recording it as taken there, binds it, and while that bind fails, as on a port another program
// holds, goes on to the next, up to ten (osallocator_linux.go, maxAllocateAttempts). Its record
// holds a port from the moment it chooses it, so no other publish of Docker's takes it before the
// bind. A start's port is the one Docker reports (endpoint), not the probed one.
func publishedRange(first int) string { return fmt.Sprintf("%d-%d", first, lastPublishedPort) }

// addressInUse reports whether err is a bind refused because something already listens on the
// address, as Docker 29 words it ("failed to bind host port 127.1.2.3:33000/tcp: address already
// in use") and Docker 28 did when the listen failed ("failed to listen on TCP socket: address
// already in use").
func addressInUse(err error) bool {
	return err != nil && strings.Contains(err.Error(), "address already in use")
}

// clientPort and monitorPort are the container ports start publishes: the one a client dials and
// the monitoring endpoint's.
const (
	clientPort  nat.Port = "4222/tcp"
	monitorPort nat.Port = "8222/tcp"
)

// published is the part of a container endpoint reads: its published ports, and the host of the
// Docker daemon that runs it.
type published interface {
	Inspect(context.Context) (*container.InspectResponse, error)
	Host(context.Context) (string, error)
}

// endpoint is the host:port Docker published container's port on: the loopback address start
// chose (loopbackPorts) at the port Docker took of the range start gave it (publishedRange), or,
// where the port took Docker's default binding of every address (a daemon on another machine,
// localDaemon), the daemon's host at the port Docker chose. Docker publishes a port again at each
// start of a container, so a caller reads it once the container runs.
func endpoint(container published, port nat.Port) (string, error) {
	ctx := context.Background()
	info, err := container.Inspect(ctx)
	if err != nil {
		return "", fmt.Errorf("inspect the NATS container: %w", err)
	}
	if info.NetworkSettings == nil || len(info.NetworkSettings.Ports[port]) == 0 {
		return "", fmt.Errorf("the NATS container publishes no %s", port)
	}
	binding := info.NetworkSettings.Ports[port][0]
	host := binding.HostIP
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		if host, err = container.Host(ctx); err != nil {
			return "", fmt.Errorf("read the Docker daemon's host: %w", err)
		}
	}
	return net.JoinHostPort(host, binding.HostPort), nil
}

// localDaemon reports whether the Docker daemon testcontainers reaches runs on this machine
// (localDaemonHost of the host testcontainers places it at). It is a copy of
// packages/envoy/internal/testnats's, for the reason LoopbackIP gives.
func localDaemon(ctx context.Context) (bool, error) {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return false, err
	}
	defer provider.Close()
	host, err := provider.DaemonHost(ctx)
	if err != nil {
		return false, err
	}
	return localDaemonHost(host), nil
}

// localDaemonHost reports whether a Docker daemon testcontainers places at host is this machine,
// so a port it publishes on a loopback address is one this process can dial: testcontainers names
// such a daemon "localhost" (a unix socket, outside a container) or a loopback address. A daemon
// that DOCKER_HOST names on another machine, or the host's daemon as a container reaches it through
// its gateway, publishes on a machine whose loopback this one does not reach.
func localDaemonHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
