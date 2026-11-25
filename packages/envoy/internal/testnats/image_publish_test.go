//go:build natslive

package testnats

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go"

	"github.com/sjawhar/envoy/internal/testport"
)

// The port startImage is handed can be taken before Docker publishes on it, since the probe lets
// it go first (testport.LoopbackPorts): by Docker, for a container another test publishes on every
// address at a port Docker chose, which Docker 29 then refuses the one port for ("Bind for
// 0.0.0.0:<port> failed: port is already allocated"), or by another program, binding it at the
// probe's address. Either way the start publishes the client port in its first start, at the
// probe's address and on a port past the taken one (testport.PublishedRange). Each case takes a
// port between the probe and the publish and hands it to the start: Docker's case the port Docker
// chose for the other container, the program's case the probed one. Then it checks that the take
// held while the start published: Docker refuses a publish of the taken port alone at the probe's
// address. Docker refuses only where this process can take a port Docker publishes, on a daemon on
// this machine, so on a daemon on another machine the row fails as UNKNOWN, naming the daemon.
func TestAnImageStartPublishesPastAProbedPortTakenBeforeThePublish(t *testing.T) {
	requireLocalImageDaemon(t)
	for _, row := range []struct {
		name string
		// take takes a port once the probe at host chose probed, and answers the port it took.
		take func(t *testing.T, host string, probed int) int
	}{
		{"Docker publishes another container on it on every address", publishImageOnEveryAddress},
		{"another program listens on it at the probe's address", listenAtImagePort},
	} {
		t.Run(row.name, func(t *testing.T) {
			probe := imagePorts
			t.Cleanup(func() { imagePorts = probe })
			var host string
			var taken, probes int
			imagePorts = func(n int) (string, []int, error) {
				address, ports, err := probe(n)
				probes++
				if err != nil || probes > 1 {
					return address, ports, err
				}
				host = address
				taken = row.take(t, host, ports[0])
				ports[0] = taken
				return address, ports, nil
			}
			_, served := StartImageConfig(t, HubImage, "jetstream {}\n", nil, true)
			if probes != 1 {
				t.Fatalf("startImage chose ports %d times: Docker refused a start whose port was taken, rather than publish past it", probes)
			}
			at, err := url.Parse(served)
			if err != nil {
				t.Fatalf("parse the server's URL %q: %v", served, err)
			}
			if at.Hostname() != host || at.Port() == strconv.Itoa(taken) {
				t.Fatalf("the server answers at %s; want the probe's address %s, at a port other than the taken %d", served, host, taken)
			}
			_, err = publishUnservedImage(t, host, strconv.Itoa(taken))
			if err == nil {
				t.Fatalf("Docker published %s:%d alone after it was taken: the take this row stands on did not hold", host, taken)
			}
			if !testport.AddressInUse(err) && !strings.Contains(err.Error(), "port is already allocated") {
				t.Fatalf("Docker refused a publish of %s:%d alone with %v, not as a port in use", host, taken, err)
			}
			t.Logf("with %s:%d taken, the server answers at %s, and Docker refuses %s:%d alone: %v", host, taken, served, host, taken, err)
		})
	}
}

// publishImageOnEveryAddress publishes a container of t's own on every address at a port Docker
// chooses, as a container another test publishes on its default binding, and answers that port.
// Docker chooses it, rather than this row, because a bind on every address fails on a port any
// socket holds on any address, such as the local port of a connection to 127.0.0.1, which no probe
// of one address rules out; Docker goes on to its next port on such a failure.
func publishImageOnEveryAddress(t *testing.T, _ string, _ int) int {
	t.Helper()
	ctr, err := publishUnservedImage(t, "0.0.0.0", "")
	if err != nil {
		t.Fatalf("UNKNOWN: publish a container on every address once the probe let its port go: %v", err)
	}
	info, err := ctr.Inspect(context.Background())
	if err != nil {
		t.Fatalf("inspect the container published on every address: %v", err)
	}
	bindings := info.NetworkSettings.Ports[unservedImagePort]
	if len(bindings) == 0 {
		t.Fatalf("the container published on every address publishes no %s", unservedImagePort)
	}
	port, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil {
		t.Fatalf("the port Docker published %s on, %q: %v", unservedImagePort, bindings[0].HostPort, err)
	}
	t.Logf("Docker published another container on %s:%d once the probe let its port go", bindings[0].HostIP, port)
	return port
}

// listenAtImagePort listens at host on probed for t, as another program binds a port it was
// handed, and answers probed.
func listenAtImagePort(t *testing.T, host string, probed int) int {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(probed)))
	if err != nil {
		t.Fatalf("UNKNOWN: listen on %s:%d, the probed port, once the probe let it go: %v", host, probed, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Logf("listened on %s, the probed port, once the probe let it go", listener.Addr())
	return probed
}

// unservedImagePort is a port nothing in a NATS container listens on: a client reaching a host
// port publishUnservedImage publishes reaches nothing.
const unservedImagePort nat.Port = "9/tcp"

// publishUnservedImage runs a container of HubImage, removed when t ends, with its
// unservedImagePort published at hostIP on hostPort, "" for a port Docker chooses, capped as every
// test container is (startImageAt). It answers the container, and Docker's refusal of that
// publish, nil when Docker published it.
func publishUnservedImage(t *testing.T, hostIP, hostPort string) (testcontainers.Container, error) {
	t.Helper()
	ctr, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        HubImage,
			ExposedPorts: []string{string(unservedImagePort)},
			HostConfigModifier: func(config *container.HostConfig) {
				config.NanoCPUs = 1_000_000_000
				config.Memory = 512 << 20
				config.PortBindings = nat.PortMap{unservedImagePort: {{HostIP: hostIP, HostPort: hostPort}}}
			},
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	return ctr, err
}

// requireLocalImageDaemon fails t as UNKNOWN unless the Docker daemon testcontainers reaches is on
// this machine: only there can this process take a port Docker publishes, which a row making
// Docker meet a taken port stands on.
func requireLocalImageDaemon(t *testing.T) {
	t.Helper()
	local, err := localDaemon(context.Background())
	if err != nil {
		t.Fatalf("find where the Docker daemon publishes a container's ports: %v", err)
	}
	if local {
		return
	}
	daemon := "another machine"
	if provider, err := testcontainers.NewDockerProvider(); err == nil {
		if host, err := provider.DaemonHost(context.Background()); err == nil {
			daemon = host
		}
		provider.Close()
	}
	t.Fatalf("UNKNOWN: the Docker daemon is at %s, not this machine, so this process cannot take a port Docker publishes there; run the row against a daemon on this machine", daemon)
}
