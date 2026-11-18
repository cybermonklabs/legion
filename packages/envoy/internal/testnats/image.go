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
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/testcontainers/testcontainers-go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// imageConfigPath is where StartImageConfig puts a server's configuration: the path the alpine
// images' default command reads its configuration from.
const imageConfigPath = "/etc/nats/nats-server.conf"

// ownPorts are the arguments StartImageConfig gives the server after the caller's: it listens for
// clients on 127.0.0.1 and opens its monitoring there, each on a port the kernel chooses when the
// server binds it (-1), and logs each port. The container runs on the host's network, where
// Docker publishes nothing: a port Docker publishes is one its own allocator chooses, without
// asking the kernel whether another program holds it, and ten taken ports in a row fail the start
// (docs/testing.md, "A container a test starts publishes no port"). A flag overrides the
// configuration file's port and an earlier flag's, so a configuration or serverArgs that name the
// hub's own ports (4222, -m 8222) serve on these instead.
var ownPorts = []string{"-a", "127.0.0.1", "-p", "-1", "-m", "-1"}

// The lines the server logs once it has bound each port, with the port.
var (
	clientPortLine  = regexp.MustCompile(`Listening for client connections on 127\.0\.0\.1:(\d+)`)
	monitorPortLine = regexp.MustCompile(`Starting http monitor on 127\.0\.0\.1:(\d+)`)
)

// ImageServer is a NATS container StartImageConfig started, read for its ports in what its server
// logged. Stopping it and starting it again through its Start restarts the server on new ports,
// which URL and MonitorURL then wait for.
type ImageServer struct {
	testcontainers.Container
	starts int
}

// StartImageConfig runs image as a NATS test container whose whole configuration is config, at
// /etc/nats/nats-server.conf, with the arguments -c /etc/nats/nats-server.conf, then serverArgs
// (so -js for JetStream), then ownPorts, removed when the test ends (even when its start fails).
// It returns the container with its client URL once the server answers a client with no
// credential (awaitAuth): with anonymous false, by refusing it; with anonymous true, by admitting
// it, as a configuration naming a no_auth_user makes it. Both official image variants take
// arguments that start with a flag as nats-server's own.
func StartImageConfig(t testing.TB, image, config string, serverArgs []string, anonymous bool) (*ImageServer, string) {
	t.Helper()
	ctx := context.Background()
	args := append(append([]string{"-c", imageConfigPath}, serverArgs...), ownPorts...)
	ctr, err := testcontainers.Run(ctx, image,
		testcontainers.WithCmd(args...),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(config),
			ContainerFilePath: imageConfigPath,
			FileMode:          0o644,
		}),
		// The host's network (ownPorts), and one CPU and 512 MiB, as every container the
		// repository's tests and proofs start is capped on a shared host: a test server holds a few
		// small streams.
		testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
			hostConfig.NetworkMode = "host"
			hostConfig.NanoCPUs = 1_000_000_000
			hostConfig.Memory = 512 << 20
		}),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS on %s: %v", image, err)
	}
	server := &ImageServer{Container: ctr, starts: 1}
	uri := server.URL(t)
	awaitAuth(t, uri, anonymous)
	return server, uri
}

// Start starts the stopped container again, which restarts its server: the server binds new ports,
// and URL and MonitorURL read them from this start's lines.
func (s *ImageServer) Start(ctx context.Context) error {
	s.starts++
	return s.Container.Start(ctx)
}

// URL waits for the server of the container's latest start to log its client port, and returns its
// client URL, nats://127.0.0.1:<port>.
func (s *ImageServer) URL(t testing.TB) string {
	t.Helper()
	return "nats://127.0.0.1:" + s.loggedPort(t, clientPortLine)
}

// MonitorURL waits for the server of the container's latest start to log its monitoring port, and
// returns its monitoring endpoint, http://127.0.0.1:<port>.
func (s *ImageServer) MonitorURL(t testing.TB) string {
	t.Helper()
	return "http://127.0.0.1:" + s.loggedPort(t, monitorPortLine)
}

// loggedPort waits for the container's log to hold line once for each start it has made, and
// returns the port of the latest start's: every start appends to the one log, so an earlier
// start's line names a port its server no longer holds.
func (s *ImageServer) loggedPort(t testing.TB, line *regexp.Regexp) string {
	t.Helper()
	what := fmt.Sprintf("the NATS server of start %d of container %s to log %q", s.starts, s.GetContainerID(), line)
	return testwait.Until(t, what, func() (string, bool) {
		logs, err := s.Logs(context.Background())
		if err != nil {
			return err.Error(), false
		}
		defer logs.Close()
		text, err := io.ReadAll(logs)
		if err != nil {
			return err.Error(), false
		}
		found := line.FindAllStringSubmatch(string(text), -1)
		if len(found) < s.starts {
			return fmt.Sprintf("%d such lines in the log", len(found)), false
		}
		return found[s.starts-1][1], true
	})
}
