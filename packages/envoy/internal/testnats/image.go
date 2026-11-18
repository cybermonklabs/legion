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

// imageStartTries bounds the starts StartImageConfig makes, each on a fresh address and port,
// while Docker refuses to publish the client port because something took the probed port first
// (testport.LoopbackPorts).
const imageStartTries = 5

// StartImageConfig runs image as a NATS test container whose whole configuration is config, at
// /etc/nats/nats-server.conf, with the arguments -c /etc/nats/nats-server.conf and then serverArgs
// (so -js for JetStream), removed when the test ends (even when its start fails). It returns the
// container with its client URL once the server answers a client with no credential (awaitAuth):
// with anonymous false, by refusing it; with anonymous true, by admitting it, as a configuration
// naming a no_auth_user makes it. Both official image variants take arguments that start with a
// flag as nats-server's own. The client port is published on a loopback address of its own and a
// port the kernel chose there (testport.LoopbackPorts), as every other server of this package
// listens, and the URL names that address rather than localhost (ImageURL). A start Docker refuses
// because the port was taken between the probe and the publish is tried again on a fresh address
// and port, up to imageStartTries starts, and the failure names every one.
func StartImageConfig(t testing.TB, image, config string, serverArgs []string, anonymous bool) (*tcnats.NATSContainer, string) {
	t.Helper()
	var refused []string
	for range imageStartTries {
		host, ports, err := testport.LoopbackPorts(1)
		if err != nil {
			t.Fatalf("choose a loopback port to publish NATS on: %v", err)
		}
		address := net.JoinHostPort(host, strconv.Itoa(ports[0]))
		ctr, err := startImage(image, config, serverArgs, host, ports[0])
		if testport.AddressInUse(err) {
			refused = append(refused, fmt.Sprintf("%s: %v", address, errors.Join(err, testcontainers.TerminateContainer(ctr))))
			continue
		}
		testcontainers.CleanupContainer(t, ctr)
		if err != nil {
			t.Fatalf("start NATS on %s at %s: %v", image, address, err)
		}
		uri := ImageURL(t, ctr)
		awaitAuth(t, uri, anonymous)
		return ctr, uri
	}
	t.Fatalf("start NATS on %s: Docker refused every one of %d published addresses as in use:\n%s", image, imageStartTries, strings.Join(refused, "\n"))
	return nil, ""
}

// startImage is one start of StartImageConfig's container, its client port published at
// host:port.
func startImage(image, config string, serverArgs []string, host string, port int) (*tcnats.NATSContainer, error) {
	return tcnats.Run(context.Background(), image,
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
			if hostConfig.PortBindings == nil {
				hostConfig.PortBindings = nat.PortMap{}
			}
			hostConfig.PortBindings[imageClientPort] = []nat.PortBinding{{HostIP: host, HostPort: strconv.Itoa(port)}}
		}),
		// The line nats-server logs once its listeners are up, in place of the NATS module's own
		// wait, a dial of the client port at localhost, where the port is not published; awaitAuth
		// is the proof a client reaches the server.
		testcontainers.WithWaitStrategy(wait.ForLog("Server is ready")),
	)
}

// ImageServer is the part of the container StartImageConfig returns that ImageURL reads.
type ImageServer interface {
	Inspect(context.Context) (*container.InspectResponse, error)
}

// ImageURL is the client URL of an image server StartImageConfig started: its client port, as
// Docker published it on the server's loopback address. A test that restarts the container reads
// it again, since Docker publishes the port again at each start.
func ImageURL(t testing.TB, ctr ImageServer) string {
	t.Helper()
	info, err := ctr.Inspect(context.Background())
	if err != nil {
		t.Fatalf("inspect the NATS container: %v", err)
	}
	if info.NetworkSettings == nil || len(info.NetworkSettings.Ports[imageClientPort]) == 0 {
		t.Fatalf("the NATS container publishes no %s", imageClientPort)
	}
	binding := info.NetworkSettings.Ports[imageClientPort][0]
	return "nats://" + net.JoinHostPort(binding.HostIP, binding.HostPort)
}
