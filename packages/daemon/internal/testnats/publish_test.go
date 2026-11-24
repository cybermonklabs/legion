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
)

// The client port start is handed can be taken before Docker publishes on it, since the probe
// lets it go first (loopbackPorts): by Docker, for a container another test publishes on every
// address at a port Docker chose, as one was in a run of the daemon's packages ("Bind for
// 0.0.0.0:34737 failed: port is already allocated"), or by another program, binding it at the
// probe's address. Either way the start publishes the client port in its first start, at the
// probe's address and on a port past the taken one (publishedRange). Each case takes a port
// between the probe and the publish and hands it to the start as its client port: Docker's case
// the port Docker chose for the other container, the program's case the probed one. Then it checks
// that the take held while the start published: Docker refuses a publish of the taken port alone
// at the probe's address, as it refused that run's start. Docker refuses only where this process
// can take a port Docker publishes, on a daemon on this machine, so on a daemon on another machine
// the row fails as UNKNOWN, naming the daemon.
func TestAStartPublishesPastAProbedPortTakenBeforeThePublish(t *testing.T) {
	if server, _, err := external(); err == nil && server != "" {
		t.Skipf("%s names a running NATS server, so the package starts no container", externalURLEnv)
	}
	requireLocalDaemon(t)
	for _, row := range []struct {
		name string
		// take takes a port once the probe at host chose probed, and answers the port it took.
		take func(t *testing.T, host string, probed int) int
	}{
		{"Docker publishes another container on it on every address", publishOnEveryAddress},
		{"another program listens on it at the probe's address", listenAt},
	} {
		t.Run(row.name, func(t *testing.T) {
			probe := loopbackPorts
			t.Cleanup(func() { loopbackPorts = probe })
			var host string
			var taken, probes int
			loopbackPorts = func(n int) (string, []int, error) {
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
			served := StartImage(t, Image)
			if probes != 1 {
				t.Fatalf("start chose ports %d times: Docker refused a start whose client port was taken, rather than publish past it", probes)
			}
			at, err := url.Parse(served)
			if err != nil {
				t.Fatalf("parse the server's URL %q: %v", served, err)
			}
			if at.Hostname() != host || at.Port() == strconv.Itoa(taken) {
				t.Fatalf("the server answers at %s; want the probe's address %s, at a port other than the taken %d", served, host, taken)
			}
			_, err = publishUnserved(t, host, strconv.Itoa(taken))
			if err == nil {
				t.Fatalf("Docker published %s:%d alone after it was taken: the take this row stands on did not hold", host, taken)
			}
			if !addressInUse(err) && !strings.Contains(err.Error(), "port is already allocated") {
				t.Fatalf("Docker refused a publish of %s:%d alone with %v, not as a port in use", host, taken, err)
			}
			t.Logf("with %s:%d taken, the server answers at %s, and Docker refuses %s:%d alone: %v", host, taken, served, host, taken, err)
		})
	}
}

// publishOnEveryAddress publishes a container of t's own on every address at a port Docker
// chooses, as a container another test publishes on its default binding, and answers that port.
// Docker chooses it, rather than this row, because a bind on every address fails on a port any
// socket holds on any address, such as the local port of a connection to 127.0.0.1, which no probe
// of one address rules out; Docker goes on to its next port on such a failure.
func publishOnEveryAddress(t *testing.T, _ string, _ int) int {
	t.Helper()
	ctr, err := publishUnserved(t, "0.0.0.0", "")
	if err != nil {
		t.Fatalf("UNKNOWN: publish a container on every address once the probe let its ports go: %v", err)
	}
	info, err := ctr.Inspect(context.Background())
	if err != nil {
		t.Fatalf("inspect the container published on every address: %v", err)
	}
	bindings := info.NetworkSettings.Ports[unservedPort]
	if len(bindings) == 0 {
		t.Fatalf("the container published on every address publishes no %s", unservedPort)
	}
	port, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil {
		t.Fatalf("the port Docker published %s on, %q: %v", unservedPort, bindings[0].HostPort, err)
	}
	t.Logf("Docker published another container on %s:%d once the probe let its ports go", bindings[0].HostIP, port)
	return port
}

// listenAt listens at host on probed for t, as another program binds a port it was handed, and
// answers probed.
func listenAt(t *testing.T, host string, probed int) int {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(probed)))
	if err != nil {
		t.Fatalf("UNKNOWN: listen on %s:%d, the first probe's client port, once the probe let it go: %v", host, probed, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Logf("listened on %s, the first probe's client port, once the probe let it go", listener.Addr())
	return probed
}

// unservedPort is a port nothing in a NATS container listens on: a client reaching a host port
// publishUnserved publishes reaches nothing.
const unservedPort nat.Port = "9/tcp"

// publishUnserved runs a container of Image, removed when t ends, with its unservedPort published
// at hostIP on hostPort, "" for a port Docker chooses, capped as every NATS test container is
// (hostConfig). It answers the container, and Docker's refusal of that publish, nil when Docker
// published it.
func publishUnserved(t *testing.T, hostIP, hostPort string) (testcontainers.Container, error) {
	t.Helper()
	ctr, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{string(unservedPort)},
			HostConfigModifier: func(config *container.HostConfig) {
				config.NanoCPUs = 1_000_000_000
				config.Memory = 512 << 20
				config.PortBindings = nat.PortMap{unservedPort: {{HostIP: hostIP, HostPort: hostPort}}}
			},
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	return ctr, err
}

// requireLocalDaemon fails t as UNKNOWN unless the Docker daemon testcontainers reaches is on this
// machine: only there can this process take a port Docker publishes, which a row making Docker
// meet a taken port stands on.
func requireLocalDaemon(t *testing.T) {
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
