//go:build natslive

// The container-backed half of the rig, built only under the natslive tag. Envoy's tests run a
// nats-server in the test process (nats.go), which needs no container runtime; what is here runs
// a published NATS image instead, which the natslive lane uses to check the rig's helpers against
// the server an operator deploys. Keeping it behind the tag keeps testcontainers out of every
// other build of this package, and the guard in nats_test.go keeps it out of the rest of the
// module.

package testnats

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sjawhar/envoy/internal/testport"
)

// imageClientPort is the container port a NATS image serves its clients on.
const imageClientPort nat.Port = "4222/tcp"

// imageConfigPath is where StartImageConfig puts a server's configuration: the path the alpine
// images' default command reads its configuration from.
const imageConfigPath = "/etc/nats/nats-server.conf"

// imageStartTries bounds the starts startImage makes while Docker refuses to publish the client
// port as in use: something took the probed port first (testport.LoopbackPorts), or, on a Docker
// daemon on another machine, every port Docker tried was held.
const imageStartTries = 5

// StartImageConfig runs image as a NATS test container whose whole configuration is config, at
// /etc/nats/nats-server.conf, with the arguments -c /etc/nats/nats-server.conf and then serverArgs
// (so -js for JetStream), removed when the test ends (even when its start fails). It returns the
// container with its client URL once the server answers a client with no credential (awaitAuth):
// with anonymous false, by refusing it; with anonymous true, by admitting it, as a configuration
// naming a no_auth_user makes it. Both official image variants take arguments that start with a
// flag as nats-server's own. Its address is startImage's.
func StartImageConfig(t testing.TB, image, config string, serverArgs []string, anonymous bool) (*tcnats.NATSContainer, string) {
	t.Helper()
	ctr, uri := startImage(t, image, config, serverArgs)
	awaitAuth(t, uri, anonymous)
	return ctr, uri
}

// StartImageReady runs the container StartImageConfig runs, on config, and returns it with its
// client URL once nats-server logs that it is ready, with no client's wait: for a configuration
// that admits no probe from outside (an auth callout holds a client with no credential, and a
// task's configuration may name no user the probe could be). nats-server 2.15.1 logs that line
// once its client listener is open, and Docker publishes the port before the server starts. Such a
// test starts the container on its final configuration rather than stop it and start it again on
// another: Docker publishes the client port again at each start, and while the container is
// stopped another process can take that port, so the start is refused, "address already in use".
// options go after the start's own (startImageAt): a volume mount, say, for a store a later
// container of the same test reads again.
func StartImageReady(t testing.TB, image, config string, serverArgs []string, options ...testcontainers.ContainerCustomizer) (*tcnats.NATSContainer, string) {
	t.Helper()
	return startImage(t, image, config, serverArgs, options...)
}

// imageStoreDir is where an image server given a store of its own (ImageStore) keeps JetStream.
const imageStoreDir = "/data"

// ImageStore is a Docker volume of t's own, removed when t ends, as the options that make an image
// server keep its JetStream store there (StartImageReady's options). A test that changes a
// server's configuration over the same store stops the first container and starts a fresh one on
// the same store, on a fresh address, rather than start the stopped one again (StartImageReady says
// why).
func ImageStore(t testing.TB) []testcontainers.ContainerCustomizer {
	t.Helper()
	name := "testnats-store-" + rand.Text()
	// Registered before any container on the volume starts, so it runs after their removal.
	t.Cleanup(func() {
		provider, err := testcontainers.NewDockerProvider()
		if err != nil {
			t.Errorf("remove the store volume %s: %v", name, err)
			return
		}
		defer provider.Close()
		if err := provider.Client().VolumeRemove(context.Background(), name, true); err != nil {
			t.Errorf("remove the store volume %s: %v", name, err)
		}
	})
	return []testcontainers.ContainerCustomizer{
		testcontainers.WithMounts(testcontainers.VolumeMount(name, imageStoreDir)),
		testcontainers.WithCmdArgs("-sd", imageStoreDir),
	}
}

// startImage runs the container StartImageConfig runs and returns it with its client URL, before
// any wait for the server to answer a client: each caller waits the way its configuration admits.
// Where the Docker daemon is this machine (localDaemon), the client port is published on a
// loopback address of its own and a port the kernel chose there (testport.LoopbackPorts), as every
// other server of this package listens, and the URL names that address rather than localhost
// (ImageURL). A daemon on another machine publishes on that machine, whose loopback this one does
// not reach, so there the port takes Docker's default binding and Docker's choice of port, and the
// URL names the daemon's host. Either way a start Docker refuses as in use, because the port was
// taken between the probe and the publish or Docker tried ports another process holds, is tried
// again, on a fresh address and port where they are chosen here, up to imageStartTries starts, and
// a failure names every refusal before it.
func startImage(t testing.TB, image, config string, serverArgs []string, options ...testcontainers.ContainerCustomizer) (*tcnats.NATSContainer, string) {
	t.Helper()
	local, err := localDaemon(context.Background())
	if err != nil {
		t.Fatalf("find where the Docker daemon publishes a container's ports: %v", err)
	}
	publishOn := testport.LoopbackPorts
	if !local {
		// No address, so startImageAt leaves Docker's default binding, and Docker picks the port.
		publishOn = func(n int) (string, []int, error) { return "", make([]int, n), nil }
	}
	var refused []string
	// before is the refusals so far, for a failure's message.
	before := func() string {
		if len(refused) == 0 {
			return ""
		}
		return "\nafter Docker refused, as in use:\n" + strings.Join(refused, "\n")
	}
	for range imageStartTries {
		host, ports, err := publishOn(1)
		if err != nil {
			t.Fatalf("choose a loopback port to publish NATS on: %v%s", err, before())
		}
		address := "Docker's default binding"
		if host != "" {
			address = net.JoinHostPort(host, strconv.Itoa(ports[0]))
		}
		ctr, err := startImageAt(image, config, serverArgs, host, ports[0], options)
		if testport.AddressInUse(err) {
			refused = append(refused, fmt.Sprintf("%s: %v", address, errors.Join(err, testcontainers.TerminateContainer(ctr))))
			continue
		}
		testcontainers.CleanupContainer(t, ctr)
		if err != nil {
			t.Fatalf("start NATS on %s at %s: %v%s", image, address, err, before())
		}
		return ctr, ImageURL(t, ctr)
	}
	t.Fatalf("start NATS on %s: Docker refused every one of %d published addresses as in use:\n%s", image, imageStartTries, strings.Join(refused, "\n"))
	return nil, ""
}

// startImageAt is one start of startImage's container, its client port published at host:port,
// or at Docker's default binding when host is "", with options after its own: a volume mount, say
// (testcontainers.WithMounts), never a host config modifier, of which a request keeps only the last
// and this one holds the port's binding.
func startImageAt(image, config string, serverArgs []string, host string, port int, options []testcontainers.ContainerCustomizer) (*tcnats.NATSContainer, error) {
	return tcnats.Run(context.Background(), image, append([]testcontainers.ContainerCustomizer{
		testcontainers.WithCmd(append([]string{"-c", imageConfigPath}, serverArgs...)...),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(config),
			ContainerFilePath: imageConfigPath,
			FileMode:          0o644,
		}),
		testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
			// One CPU and 512 MiB, as every container the repository's tests and proofs start is
			// capped on a shared host: a test server holds a few small streams.
			hostConfig.NanoCPUs = 1_000_000_000
			hostConfig.Memory = 512 << 20
			if host == "" {
				return
			}
			if hostConfig.PortBindings == nil {
				hostConfig.PortBindings = nat.PortMap{}
			}
			hostConfig.PortBindings[imageClientPort] = []nat.PortBinding{{HostIP: host, HostPort: strconv.Itoa(port)}}
		}),
		// The line nats-server logs once its listeners are up, in place of the NATS module's own
		// wait, a dial of the client port at localhost, where the port is not published; each
		// caller's own wait (awaitAuth) is the proof a client reaches the server.
		testcontainers.WithWaitStrategy(wait.ForLog("Server is ready")),
	}, options...)...)
}

// localDaemon reports whether the Docker daemon testcontainers reaches runs on this machine
// (localDaemonHost of the host testcontainers places it at).
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

// ImageServer is the part of the container StartImageConfig returns that ImageURL reads.
type ImageServer interface {
	Inspect(context.Context) (*container.InspectResponse, error)
	Host(context.Context) (string, error)
}

// ImageURL is the client URL of an image server StartImageConfig started: its client port, as
// Docker published it, on the server's loopback address (startImage), or at the Docker daemon's
// host where the port took Docker's default binding of every address.
func ImageURL(t testing.TB, ctr ImageServer) string {
	t.Helper()
	ctx := context.Background()
	info, err := ctr.Inspect(ctx)
	if err != nil {
		t.Fatalf("inspect the NATS container: %v", err)
	}
	if info.NetworkSettings == nil || len(info.NetworkSettings.Ports[imageClientPort]) == 0 {
		t.Fatalf("the NATS container publishes no %s", imageClientPort)
	}
	binding := info.NetworkSettings.Ports[imageClientPort][0]
	host := binding.HostIP
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		if host, err = ctr.Host(ctx); err != nil {
			t.Fatalf("read the Docker daemon's host: %v", err)
		}
	}
	return "nats://" + net.JoinHostPort(host, binding.HostPort)
}
