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

// A port start probed can be taken before Docker publishes on it, since the probe lets it go
// first (loopbackPorts): by Docker, for a container another test publishes on every address, as
// one was in a run of the daemon's packages ("Bind for 0.0.0.0:34737 failed: port is already
// allocated"), or by another program, binding it at the probe's address. Either way the start
// publishes the client port in its first start, at the probe's address and on a port past the
// taken one (publishedRange). Each case takes the first probe's client port once the probe lets
// it go, and then checks that the take held while the start published: Docker refuses a publish
// of the taken port alone at the probe's address, as it refused that run's start. Docker refuses
// only where this process can take a port Docker publishes, on a daemon on this machine, so on a
// daemon on another machine the row fails as UNKNOWN, naming the daemon.
func TestAStartPublishesPastAProbedPortTakenBeforeThePublish(t *testing.T) {
	if server, _, err := external(); err == nil && server != "" {
		t.Skipf("%s names a running NATS server, so the package starts no container", externalURLEnv)
	}
	requireLocalDaemon(t)
	for _, row := range []struct {
		name string
		take func(t *testing.T, host string, port int)
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
				host, taken = address, ports[0]
				row.take(t, host, taken)
				return address, ports, nil
			}
			served := StartImage(t, Image)
			if probes != 1 {
				t.Fatalf("start chose ports %d times: Docker refused a start whose probed client port was taken, rather than publish past it", probes)
			}
			at, err := url.Parse(served)
			if err != nil {
				t.Fatalf("parse the server's URL %q: %v", served, err)
			}
			if at.Hostname() != host || at.Port() == strconv.Itoa(taken) {
				t.Fatalf("the server answers at %s; want the probe's address %s, at a port other than the taken %d", served, host, taken)
			}
			err = publishUnserved(t, host, taken)
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

// publishOnEveryAddress takes port on every address for a container of t's own, as Docker takes a
// port for a container another test publishes on its default binding.
func publishOnEveryAddress(t *testing.T, _ string, port int) {
	t.Helper()
	if err := publishUnserved(t, "0.0.0.0", port); err != nil {
		t.Fatalf("UNKNOWN: publish a container on 0.0.0.0:%d, the first probe's client port, once the probe let it go: %v", port, err)
	}
	t.Logf("published another container on 0.0.0.0:%d, the first probe's client port, once the probe let it go", port)
}

// listenAt takes port at host for a listener of t's own, as another program binds a port it was
// handed.
func listenAt(t *testing.T, host string, port int) {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("UNKNOWN: listen on %s:%d, the first probe's client port, once the probe let it go: %v", host, port, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Logf("listened on %s, the first probe's client port, once the probe let it go", listener.Addr())
}

// unservedPort is a port nothing in a NATS container listens on: a client reaching a host port
// publishUnserved publishes reaches nothing.
const unservedPort nat.Port = "9/tcp"

// publishUnserved runs a container of Image, removed when t ends, with its unservedPort published
// at hostIP on hostPort alone, capped as every NATS test container is (hostConfig), and answers
// Docker's refusal of that publish, nil when Docker published it.
func publishUnserved(t *testing.T, hostIP string, hostPort int) error {
	t.Helper()
	ctr, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{string(unservedPort)},
			HostConfigModifier: func(config *container.HostConfig) {
				config.NanoCPUs = 1_000_000_000
				config.Memory = 512 << 20
				config.PortBindings = nat.PortMap{unservedPort: {{HostIP: hostIP, HostPort: strconv.Itoa(hostPort)}}}
			},
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	return err
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
