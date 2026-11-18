// The container half of the rig. Every other helper of this package runs its server in the test
// process; StartImage and StartOpenImage run a published NATS image instead, for the tests whose
// subject is those images themselves: internal/daemon's TestDaemonPermissionsOnRealServers, which
// runs the daemon's grant on each image of testnatsimages.Images and reads the server's own log,
// and internal/intake's TestOpenConsumersRefusesAServerThatSkipsAFilter, which runs SkippingImage,
// a release no in-process server can be. Where no container runtime answers, those tests and the
// tests of this file's helpers skip, and fail on GitHub Actions, whose test job has one
// (skipWithoutContainerRuntime).

package testnats

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// SkippingImage is ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml),
// whose v2.10.29 can skip a message on a consumer filtering on a wildcard and a literal at the same
// token. The daemon refuses such a server (envoy's natsversion.Refusal, intake.OpenConsumers), and
// the one test that shows it refusing one starts this image (StartOpenImage).
const SkippingImage = "ghcr.io/cybermonklabs/legion/ci@sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f"

// StartImage runs image as a NATS container with JetStream that admits users alone, each with its
// permissions (the configuration StartNkeyAuthorizedUsers gives an in-process server), removed when
// t ends, and returns the container, whose log a test reads, beside its client URL once the server
// refuses a client with no credential (answering).
func StartImage(t testing.TB, image string, users ...NkeyUser) (*testcontainers.DockerContainer, string) {
	t.Helper()
	skipWithoutContainerRuntime(t)
	started, err := startContainer(testwait.Context(t), image, tcnats.WithConfigFile(strings.NewReader(nkeyConfig(users))))
	if err != nil {
		t.Fatalf("start nkey-authorized NATS on %s: %v", image, err)
	}
	testcontainers.CleanupContainer(t, started)
	url := loggedURL(t, started)
	answering(t, url)
	return started, url
}

// StartOpenImage runs image as a NATS container with JetStream and no users (-js alone), so it
// admits any client, removed when t ends, and returns its client URL once its JetStream API
// answers. It is for a test of the server itself, such as one the daemon refuses (SkippingImage);
// every other test takes a server of its own in this process through URL.
func StartOpenImage(t testing.TB, image string) string {
	t.Helper()
	skipWithoutContainerRuntime(t)
	started, err := startContainer(testwait.Context(t), image)
	if err != nil {
		t.Fatalf("start NATS on %s: %v", image, err)
	}
	testcontainers.CleanupContainer(t, started)
	url := loggedURL(t, started)
	testwait.Until(t, fmt.Sprintf("the JetStream API of the NATS server at %s to answer", url), func() (error, bool) {
		conn, err := nats.Connect(url, nats.Name(t.Name()))
		if err != nil {
			return err, false
		}
		defer conn.Close()
		js, err := jetstream.New(conn, RequestTimeout(t))
		if err == nil {
			_, err = js.AccountInfo(testwait.Context(t))
		}
		return err, err == nil
	})
	return url
}

// onTheHostNetwork runs a NATS test container on the host's network, where Docker publishes
// nothing, and caps it at one CPU and 512 MiB, as every container the repository's tests and proofs
// start is capped on a shared host: a test server holds a few small streams. A port Docker
// publishes is one its own allocator chooses, without asking the kernel whether another program
// holds it, and ten taken ports in a row fail the start (docs/testing.md, "A container a test
// starts publishes no port"); the NATS module of testcontainers publishes 4222, 6222 and 8222 so.
var onTheHostNetwork = testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
	hostConfig.NetworkMode = "host"
	hostConfig.NanoCPUs = 1_000_000_000
	hostConfig.Memory = 512 << 20
})

// serverArgs are the arguments every container of startContainer runs nats-server with, before
// any its options add (WithConfigFile's -config): JetStream, debug and trace logging, as the NATS
// module ran it, and a client listener on 127.0.0.1 on a port the kernel chooses when the server
// binds it (-p -1), which the server logs (clientPortLine). A flag overrides the configuration
// file's port.
var serverArgs = []string{"-DV", "-js", "-a", "127.0.0.1", "-p", "-1"}

// clientPortLine is the line the server logs once it has bound its client port, with the port.
var clientPortLine = regexp.MustCompile(`Listening for client connections on 127\.0\.0\.1:(\d+)`)

// startContainer runs a NATS container of image on the host's network (onTheHostNetwork, with
// serverArgs). A container Docker created and never saw ready (a readiness wait an option set that
// failed) comes back beside the error, and is removed before startContainer returns, since the
// caller registers no removal of a container it was not handed.
func startContainer(ctx context.Context, image string, options ...testcontainers.ContainerCustomizer) (*testcontainers.DockerContainer, error) {
	started, err := testcontainers.Run(ctx, image, append([]testcontainers.ContainerCustomizer{onTheHostNetwork, testcontainers.WithCmd(serverArgs...)}, options...)...)
	if err != nil {
		return started, errors.Join(err, testcontainers.TerminateContainer(started))
	}
	return started, nil
}

// loggedURL waits for the server in started to log its client port, and returns its client URL,
// nats://127.0.0.1:<port>.
func loggedURL(t testing.TB, started testcontainers.Container) string {
	t.Helper()
	port := testwait.Until(t, fmt.Sprintf("the NATS server of container %s to log %q", started.GetContainerID(), clientPortLine), func() (string, bool) {
		logs, err := started.Logs(context.Background())
		if err != nil {
			return err.Error(), false
		}
		defer logs.Close()
		text, err := io.ReadAll(logs)
		if err != nil {
			return err.Error(), false
		}
		found := clientPortLine.FindSubmatch(text)
		if found == nil {
			return "no such line", false
		}
		return string(found[1]), true
	})
	return "nats://127.0.0.1:" + port
}

// containerRuntime is the probe's answer for the test binary's life: a machine with no container
// runtime when the first test asks has none for the rest of the run.
var (
	containerRuntimeOnce sync.Once
	containerRuntimeErr  error
)

// skipWithoutContainerRuntime skips t where no container runtime answers, naming the runtime it
// needs, and fails it instead on GitHub Actions, whose test job has Docker: there a skip would hide
// the one run of the test that needs it.
func skipWithoutContainerRuntime(t testing.TB) {
	t.Helper()
	containerRuntimeOnce.Do(func() { containerRuntimeErr = probeContainerRuntime(testwait.Context(t)) })
	if containerRuntimeErr == nil {
		return
	}
	const needs = "this test needs a container runtime: a Docker daemon testcontainers reaches through DOCKER_HOST, a Docker context or a socket it finds on disk"
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatalf("%s, and GitHub Actions' test job has one: %v", needs, containerRuntimeErr)
	}
	t.Skipf("%s: %v", needs, containerRuntimeErr)
}

// probeContainerRuntime reports why testcontainers cannot reach a container runtime, or nil when
// one answers. testcontainers has no answer to whether one is there that is not a panic where none
// is: every route to a client runs through its MustExtractDockerHost, which panics ("rootless
// Docker not found") when neither DOCKER_HOST, a Docker context nor a socket on disk names a host,
// and a panic on a test's goroutine ends the whole test binary rather than the test. So the probe
// recovers that panic and reports it as the reason.
func probeContainerRuntime(ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("testcontainers found no Docker host: %v", recovered)
		}
	}()
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return err
	}
	return provider.Health(ctx)
}
