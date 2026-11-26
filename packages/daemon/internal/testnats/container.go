// The container half of the fixture. Every NATS server this package starts is a container of a
// published NATS image, and the package drives it through the docker CLI: create, copy the
// configuration in, start, wait for the server's ready line, read the published ports, remove.
//
// The package used testcontainers-go for this before. That library's package init (v0.41.0's
// internal/core; internal/core/bootstrap in later versions) reads every process on the machine
// through gopsutil's process.Processes(), so each start of a binary that links the library costs
// seconds and hundreds of megabytes of allocation: 7.6 to 11.5 s and 600 to 910 MB per start of
// internal/daemon's test binary, measured with GODEBUG=inittrace=1 on a machine that ran 13,000 to
// 20,000 processes. A test binary pays the init at each start, and
// internal/daemon's test binary starts itself again for each role process of its idle rig
// (TestIdleRigRoleProcess), so that package's TestThisTestBinaryLinksNoTestcontainers fails when
// the library is linked again, and so does this package's own.
//
// testcontainers-go also removed the containers of a test binary that ended before it removed them
// (a -timeout panic, a kill), through its Ryuk reaper. The reaper below does the same with no image
// of its own.

package testnats

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// clientPort and monitorPort are the server's client port and its monitoring port in the
	// container. Each is published on a port that Docker picks, of the daemon's machine
	// (daemonHost).
	clientPort  = "4222/tcp"
	monitorPort = "8222/tcp"

	// configPath is where a server's configuration is copied, and what -config names. It is the
	// path testcontainers-go's NATS module used.
	configPath = "/etc/nats.conf"

	// readyLine is the line nats-server logs when its Start returns, which is after its client
	// listener accepts connections (server.go, Start: `defer s.Noticef("Server is ready")`, in
	// v2.10 and v2.15). startupTimeout bounds the wait for it. It is testcontainers-go's default
	// startup timeout, which bounded that library's wait for the client port before.
	readyLine      = "Server is ready"
	startupTimeout = 60 * time.Second

	// sessionLabel is the label on each container this process creates, with the process's
	// session as its value. The reaper removes the containers that carry it.
	sessionLabel = "legion.testnats.session"
)

// Container is a NATS server container that this package started. The test or the Main that
// started it removes it.
type Container struct {
	id         string
	url        string
	monitorURL string
}

// Logs is all that the server has logged until now: its standard output and its standard error as
// one text, as `docker logs` prints them. nats-server logs to its standard error.
func (c *Container) Logs(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "logs", c.id).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker logs %s: %w: %s", c.id, err, bytes.TrimSpace(out))
	}
	return string(out), nil
}

// remove removes the container and its anonymous volumes, and stops it first when it runs.
func (c *Container) remove() error {
	_, err := docker(context.Background(), nil, "rm", "--force", "--volumes", c.id)
	return err
}

// startOptions is what one start of a container takes beyond its image. The zero value is a server
// with no configuration file, the fixture's labels only, and the wait for readyLine.
type startOptions struct {
	// config is the server's whole configuration, copied to configPath. Empty is no file.
	config string
	// labels go on the container beside the fixture's own.
	labels map[string]string
	// ready is the log line the start waits for, within. Empty is readyLine within
	// startupTimeout. A test of a failed start gives a line that the server never logs.
	ready  string
	within time.Duration
}

// start runs a NATS container of image with JetStream, its debug and protocol trace (-DV), and its
// monitoring endpoint on 8222, which is the command testcontainers-go's NATS module gave it, capped
// at one CPU and 512 MiB as every container the repository's tests start is capped on a shared
// host. It returns once the server has logged its ready line, which is after its client listener
// accepts connections. A container that Docker created and that never became ready is returned
// beside the error, and is removed before start returns: no test's end would remove a container
// that the package shares.
//
// On a Docker daemon of this machine (dockerDaemon.local) the two ports are published on a
// loopback address of the container's own and ports the kernel chose there
// (loopbackPorts, loopback.go), so a client another test left dialing an earlier container's port
// cannot reach this one; a publish Docker refuses because a probed port was taken in between is
// tried again on a fresh address and ports, up to startTries starts, and the error then names
// every refusal. On another machine's daemon Docker picks the published ports.
func start(image string, o startOptions) (*Container, error) {
	label, err := armReaper()
	if err != nil {
		return nil, err
	}
	daemon, err := reachedDaemon()
	if err != nil {
		return nil, err
	}
	if !daemon.local() {
		return startAt(image, o, label, daemon.dial, clientPort, monitorPort)
	}
	var refused []error
	for range startTries {
		host, ports, err := loopbackPorts(2)
		if err != nil {
			return nil, errors.Join(append(refused, fmt.Errorf("choose loopback ports to publish NATS on: %w", err))...)
		}
		c, err := startAt(image, o, label, host, publishAt(host, ports[0], clientPort), publishAt(host, ports[1], monitorPort))
		if err == nil {
			return c, nil
		}
		if !addressInUse(err) {
			return c, errors.Join(append(refused, err)...)
		}
		refused = append(refused, err)
	}
	return nil, fmt.Errorf("every one of %d published addresses was refused as in use: %w", startTries, errors.Join(refused...))
}

// startTries bounds the starts start makes, each on a fresh loopback address and ports, while
// Docker refuses to publish a port because something took the probed one first.
const startTries = 5

// startAt creates and runs one container of image, with clientPublish and monitorPublish as Docker
// create's two --publish values, and dials the published ports at dial.
func startAt(image string, o startOptions, label, dial, clientPublish, monitorPublish string) (*Container, error) {
	ctx := context.Background()
	args := []string{"create", "--label", label, "--cpus", "1", "--memory", "512m",
		"--publish", clientPublish, "--publish", monitorPublish}
	for name, value := range o.labels {
		args = append(args, "--label", name+"="+value)
	}
	args = append(args, image, "-DV", "-js")
	if o.config != "" {
		args = append(args, "-config", configPath)
	}
	args = append(args, "--http_port", "8222")
	// docker create prints the progress of a pull on its standard error and the container's ID as
	// the last line of its standard output.
	out, err := docker(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("create a NATS container of %s: %w", image, err)
	}
	c := &Container{id: lastLine(out)}
	if c.id == "" {
		return nil, fmt.Errorf("create a NATS container of %s: docker create printed no container ID", image)
	}
	if err := c.run(ctx, o, dial); err != nil {
		return c, errors.Join(fmt.Errorf("start the NATS container %s of %s: %w", c.id, image, err), c.remove())
	}
	return c, nil
}

// run copies the configuration into the created container, starts it, waits for its ready line and
// reads its two published ports, which this process dials at dial.
func (c *Container) run(ctx context.Context, o startOptions, dial string) error {
	if o.config != "" {
		archive, err := configArchive(o.config)
		if err != nil {
			return err
		}
		if _, err := docker(ctx, archive, "cp", "-", c.id+":/"); err != nil {
			return fmt.Errorf("copy the configuration to %s: %w", configPath, err)
		}
	}
	if _, err := docker(ctx, nil, "start", c.id); err != nil {
		return err
	}
	ready, within := o.ready, o.within
	if ready == "" {
		ready, within = readyLine, startupTimeout
	}
	if err := c.awaitLog(ready, within); err != nil {
		return err
	}
	client, err := c.port(ctx, clientPort)
	if err != nil {
		return err
	}
	monitoring, err := c.port(ctx, monitorPort)
	if err != nil {
		return err
	}
	c.url, c.monitorURL = "nats://"+net.JoinHostPort(dial, client), "http://"+net.JoinHostPort(dial, monitoring)
	return nil
}

// configArchive is a tar stream holding config as configPath, mode 0644, for `docker cp -` into
// the container's root. Extraction makes the directory when the image has none.
func configArchive(config string) (io.Reader, error) {
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	if err := w.WriteHeader(&tar.Header{Name: strings.TrimPrefix(configPath, "/"), Mode: 0o644, Size: int64(len(config)), ModTime: time.Now()}); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, config); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &archive, nil
}

// awaitLog follows the container's log until a line contains want, and fails when within passes
// first or when the log ends first, which is a container that exited. A failure carries the last
// lines that the server logged.
func (c *Container) awaitLog(want string, within time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	follow := exec.CommandContext(ctx, "docker", "logs", "--follow", c.id)
	read, write, err := os.Pipe()
	if err != nil {
		return err
	}
	defer read.Close()
	follow.Stdout, follow.Stderr = write, write
	err = follow.Start()
	write.Close()
	if err != nil {
		return fmt.Errorf("docker logs --follow %s: %w", c.id, err)
	}
	const kept = 20
	var last []string
	found := false
	lines := bufio.NewScanner(read)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		if strings.Contains(lines.Text(), want) {
			found = true
			break
		}
		if last = append(last, lines.Text()); len(last) > kept {
			last = last[1:]
		}
	}
	timedOut := ctx.Err() != nil
	cancel()
	_ = follow.Wait()
	switch {
	case found:
		return nil
	case timedOut:
		return fmt.Errorf("the server logged no line containing %q within %s; its last lines:\n%s", want, within, strings.Join(last, "\n"))
	default:
		return fmt.Errorf("the server's log ended before a line containing %q, so the container exited; its last lines:\n%s", want, strings.Join(last, "\n"))
	}
}

// port is the port of the daemon's host that Docker published the container's port on.
func (c *Container) port(ctx context.Context, port string) (string, error) {
	out, err := docker(ctx, nil, "port", c.id, port)
	if err != nil {
		return "", err
	}
	// One line for each address the port is published on, as 0.0.0.0:32768, [::]:32768 or
	// 127.0.0.1:32768; every line names the same host port.
	first, _, _ := strings.Cut(out, "\n")
	_, published, err := net.SplitHostPort(strings.TrimSpace(first))
	if err != nil || published == "" {
		return "", fmt.Errorf("docker port %s %s = %q, want <address>:<port>", c.id, port, out)
	}
	return published, nil
}

// dockerDaemon is where the Docker daemon the docker CLI reaches publishes a container's ports:
// dial is the host this process dials a published port on.
type dockerDaemon struct{ dial string }

// local is whether the daemon runs on this machine and publishes on its loopback, where start
// gives each container a loopback address of its own.
func (d dockerDaemon) local() bool { return isLoopback(d.dial) }

// publishAt is docker create's --publish value for the container's port at host's port.
func publishAt(host string, port int, containerPort string) string {
	return host + ":" + strconv.Itoa(port) + ":" + containerPort
}

// reachedDaemon is daemonHost of the endpoint the docker CLI resolves (cliEndpoint), with
// TESTCONTAINERS_HOST_OVERRIDE applied (withHostOverride), read once per process: every `docker
// create`, `start` and `port` of this file runs that CLI in this process's environment, so it acts
// on that daemon.
var reachedDaemon = sync.OnceValues(func() (dockerDaemon, error) {
	endpoint, err := cliEndpoint(context.Background())
	if err != nil {
		return dockerDaemon{}, err
	}
	daemon, err := daemonHost(endpoint)
	if err != nil {
		return dockerDaemon{}, err
	}
	return withHostOverride(daemon, os.Getenv(hostOverrideVariable)), nil
})

// hostOverrideVariable is testcontainers-go's name for the host a test reaches a container's
// published ports at, which the envoy module's container tests read through that library. This
// fixture reads it too, so one setting serves both modules.
const hostOverrideVariable = "TESTCONTAINERS_HOST_OVERRIDE"

// withHostOverride is d dialled at override, when override names a host. daemonHost reads the
// endpoint alone, and a process in a developer container that reaches the host's Docker socket
// reads it as this machine, whose 127.0.0.1 is the container's own: such a process names the host
// as it reaches it (the bridge gateway, host.docker.internal) in TESTCONTAINERS_HOST_OVERRIDE.
// testcontainers-go found the bridge gateway itself where /.dockerenv exists, and this fixture
// does not: there the variable is how to reach it.
func withHostOverride(d dockerDaemon, override string) dockerDaemon {
	if override == "" {
		return d
	}
	return dockerDaemon{dial: override}
}

// isLoopback is whether host names this machine's loopback interface: localhost, or a loopback
// address.
func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

// cliEndpoint is the endpoint of the Docker daemon the docker CLI reaches from this process's
// environment, as the CLI itself resolves it. `docker context inspect` with no name inspects the
// context the CLI resolves, which reads DOCKER_HOST, DOCKER_CONTEXT and the configuration's
// current context in the CLI's own order: in docker/cli's resolveContextName a non-empty
// DOCKER_HOST selects the default context, whose endpoint it is, over DOCKER_CONTEXT. Asking the
// CLI keeps the fixture on the daemon its commands reach whatever that order is in the CLI
// installed, rather than on a copy of the order that can drift from it.
func cliEndpoint(ctx context.Context) (string, error) {
	endpoint, err := docker(ctx, nil, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if err != nil {
		return "", fmt.Errorf("read the Docker endpoint the docker CLI resolves: %w", err)
	}
	return endpoint, nil
}

// daemonHost is where this process dials ports Docker publishes on the daemon at endpoint. A daemon
// on a unix socket, a named pipe or a loopback address runs on this machine. A daemon on another
// machine (tcp:// or ssh:// to a host that is not loopback) is dialled at its host name.
func daemonHost(endpoint string) (dockerDaemon, error) {
	local := dockerDaemon{dial: "127.0.0.1"}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return dockerDaemon{}, fmt.Errorf("the Docker endpoint %q: %w", endpoint, err)
	}
	switch parsed.Scheme {
	case "unix", "npipe":
		return local, nil
	case "tcp", "http", "https", "ssh":
		host := parsed.Hostname()
		if host == "" {
			return dockerDaemon{}, fmt.Errorf("the Docker endpoint %q names no host", endpoint)
		}
		if isLoopback(host) {
			return local, nil
		}
		return dockerDaemon{dial: host}, nil
	default:
		return dockerDaemon{}, fmt.Errorf("the Docker endpoint %q is not unix://, npipe://, tcp://, http(s):// or ssh://", endpoint)
	}
}

// docker runs the docker CLI with args and stdin, and returns its standard output with the
// surrounding white space removed, or an error that carries its standard error.
func docker(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	command.Stdin = stdin
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func lastLine(text string) string {
	return text[strings.LastIndexByte(text, '\n')+1:]
}

// The reaper removes the containers of this process when the process ends, however it ends.
// testcontainers-go's Ryuk did that from a container of its own, which removed the containers of a
// session when the session's connection to it closed. The reaper is a shell that this process
// starts before its first container. The shell reads a pipe whose one writer is this process, and
// the kernel closes that pipe when the process ends. The shell then removes every container that
// carries this process's session label. The shell runs in a process group of its own, so a signal
// to the test's process group (an interrupt at a terminal, timeout(1) at its bound) does not stop
// it too. Its output goes nowhere: a shell that held the test's standard output would keep `go
// test` waiting for it. Main closes the pipe itself and waits for the shell, so a run that ends
// normally leaves nothing of its own running. By then each container is removed, and the shell
// finds none.
const reaperScript = `cat >/dev/null
ids=$(docker ps --all --quiet --filter "label=$1")
[ -z "$ids" ] || docker rm --force --volumes $ids`

var reaper struct {
	once  sync.Once
	label string
	shell *exec.Cmd
	pipe  io.WriteCloser
	err   error
}

// armReaper starts the reaper once per process, and returns the session label that each
// container of this process carries.
func armReaper() (string, error) {
	reaper.once.Do(func() {
		// The session is random, as testcontainers-go's session ID was, so no other process's
		// containers carry it. The process ID beside it tells a reader which process made them.
		reaper.label = sessionLabel + "=" + strconv.Itoa(os.Getpid()) + "-" + rand.Text()
		shell := exec.Command("sh", "-c", reaperScript, "sh", reaper.label)
		shell.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		pipe, err := shell.StdinPipe()
		if err == nil {
			err = shell.Start()
		}
		if err != nil {
			reaper.err = fmt.Errorf("start the reaper of this process's NATS containers: %w", err)
			return
		}
		reaper.shell, reaper.pipe = shell, pipe
	})
	return reaper.label, reaper.err
}

// endReaper closes the reaper's pipe and waits for it to remove what is left of this process's
// containers. It does nothing when this process started no container.
func endReaper() error {
	if reaper.shell == nil {
		return nil
	}
	reaper.pipe.Close()
	if err := reaper.shell.Wait(); err != nil {
		return fmt.Errorf("the reaper of this process's NATS containers: %w", err)
	}
	return nil
}
