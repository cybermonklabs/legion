// Package testnats runs a NATS server with JetStream for a daemon test in the test process
// itself: one server per test, started by URL or JetStream and shut down when that test ends.
//
// Every server these helpers start is the nats-server this binary links in
// (github.com/nats-io/nats-server/v2), not a container, which buys two things. A test runs where
// no container runtime does, such as a Legion worker pod, so the fixture needs no external server
// named in the environment and no Docker host. And no two tests share a server: each gets its own
// store under a directory of its own, so one test's streams, consumers and messages are never
// another's to find, reset or wait out.
//
// The envoy module keeps the rig this one is cut down from at
// packages/envoy/internal/testnats/nats.go, whose start, serverOptions and logCapture these are.
// The two are copies, not one package imported twice, because Go's internal rule reads a path
// prefix: nothing outside packages/envoy may import github.com/sjawhar/envoy/internal/..., so
// packages/daemon cannot reach that file. What this copy keeps is what the daemon's tests use, so
// it has no cluster, relay, gate or KV machinery; a change to how a server is started or
// configured belongs in both.
//
// StartContainerNkeyAuthorizedUsers is the one helper here that still runs a container, for the
// test whose subject is the published NATS images themselves (packages/envoy/testnatsimages). It
// skips its test where no container runtime answers.
package testnats

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// readinessTimeout bounds every wait for a server to answer: for the in-process server, the time
// nats-server takes to open its listener and start JetStream; for a container, the time the
// server inside it takes to answer once Docker has it running.
const readinessTimeout = 30 * time.Second

// retryInterval is how long a readiness wait sleeps between attempts.
const retryInterval = 50 * time.Millisecond

var (
	// mainRuns records that the test binary's TestMain went through Main, which fails a run that
	// leaves a server of this process running.
	mainRuns bool

	// live holds every server this process has started and not yet shut down: start adds one and
	// its stop removes it, so what is left after the last test is what leaked.
	liveMu sync.Mutex
	live   = map[*Server]struct{}{}
)

// init runs in every test binary that imports this package, whichever helper its tests use. Every
// server these helpers start has no users unless a test configures some, and nats.go refuses an
// nkey when the server sends no nonce ("nats: nkeys not supported by the server"), so an
// operator's NATS_NKEY_SEED, NATS_DAEMON_NKEY_SEED, or either's _FILE pointer never reaches the
// tests' clients. A test that means to pass a seed sets it itself.
func init() {
	for _, name := range []string{"NATS_NKEY_SEED", "NATS_NKEY_SEED_FILE", "NATS_DAEMON_NKEY_SEED", "NATS_DAEMON_NKEY_SEED_FILE"} {
		os.Unsetenv(name)
	}
}

// Main runs the package's tests and returns the exit code, which is 1 when a server this process
// started is still running after the last test. Every server is shut down by the cleanup its
// start registers, so one still running leaked, and a run that leaks one is red here rather than
// a surprise later: it holds a port and a store for as long as the binary lives. A package whose
// tests use URL or JetStream calls Main from TestMain: os.Exit(testnats.Main(m)).
func Main(m *testing.M) int {
	mainRuns = true
	code := m.Run()
	liveMu.Lock()
	left := len(live)
	liveMu.Unlock()
	if left > 0 {
		fmt.Fprintf(os.Stderr, "testnats: %d NATS server(s) this process started are still running after the last test\n", left)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// URL starts a NATS server with JetStream for t alone and returns its client URL, which carries
// no credential: the server admits anyone. Its store is under a directory of t's own, and t's end
// shuts the server down and removes that directory. Nothing of another test is on it, so a test
// creates the streams it needs with CreateStream.
func URL(t testing.TB) string {
	t.Helper()
	return start(t, "", []string{"-js"}).URL()
}

// JetStream returns a JetStream client of a server URL started for t, closed when t ends.
func JetStream(t testing.TB) jetstream.JetStream {
	t.Helper()
	conn, js := connect(t, URL(t))
	t.Cleanup(conn.Close)
	return js
}

// CreateStream creates the stream config describes on js, a client of the server started for t,
// and returns it.
//
// It no longer retries the server's "error creating store for stream": that answer came from
// nats-server still removing the account's streams directory behind the delete-every-stream reset
// URL used to run on a shared server, and a server of the test's own has nothing to delete.
func CreateStream(t testing.TB, js jetstream.JetStream, config jetstream.StreamConfig) jetstream.Stream {
	t.Helper()
	stream, err := js.CreateStream(t.Context(), config)
	if err != nil {
		t.Fatalf("create stream %s: %v", config.Name, err)
	}
	return stream
}

// StartNkeyAuthorized runs a NATS server with JetStream for t alone that accepts only clients
// authenticating as the nkey user whose public key is user, shut down when t ends, and returns its
// client URL once the server refuses a client with no credential (answering).
func StartNkeyAuthorized(t testing.TB, user string) string {
	t.Helper()
	_, url := StartNkeyAuthorizedUsers(t, NkeyUser{Public: user})
	return url
}

// NkeyUser is one nkey user a server admits: its public key, and the server's permissions block for
// it in the configuration's syntax (`{ subscribe: { deny: ["x.>"] } }`), "" granting everything.
type NkeyUser struct {
	Public      string
	Permissions string
}

// StartNkeyAuthorizedUsers is StartNkeyAuthorized for a server admitting users alone, each with
// its permissions. It returns the server beside its client URL, for a test that reads the
// server's log.
func StartNkeyAuthorizedUsers(t testing.TB, users ...NkeyUser) (*Server, string) {
	t.Helper()
	server := start(t, nkeyConfig(users), nil)
	answering(t, server.URL())
	return server, server.URL()
}

// nkeyConfig is the whole configuration of a server with JetStream admitting users alone, each
// with its permissions.
func nkeyConfig(users []NkeyUser) string {
	entries := make([]string, 0, len(users))
	for _, user := range users {
		entry := fmt.Sprintf("{ nkey: %q", user.Public)
		if user.Permissions != "" {
			entry += ", permissions: " + user.Permissions
		}
		entries = append(entries, entry+" }")
	}
	return fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ %s ]\n}\n", strings.Join(entries, ", "))
}

// Server is a nats-server running in this test process. start is the only thing that makes one,
// and every helper here that starts an in-process server is a call on it.
type Server struct {
	// store is the server's JetStream store, under the directory start removes when the test ends.
	store string
	log   *logCapture
	url   string

	mu  sync.Mutex
	srv *natsserver.Server
}

// URL is the server's client URL.
func (s *Server) URL() string { return s.url }

// Log is everything the server has logged so far.
func (s *Server) Log() string { return s.log.String() }

// hostOnlyConfig is the configuration of a server a test starts with none of its own:
// nats-server warns on an empty configuration file ("config has no values or is empty"), and the
// host it names here is the one this rig imposes on every server anyway.
const hostOnlyConfig = "host: 127.0.0.1\n"

// start runs a nats-server in this process whose whole configuration is config, with serverArgs as
// its command line after it (so -js for JetStream), and returns it once it accepts connections.
// When the test ends the server is shut down and its directory removed.
func start(t testing.TB, config string, serverArgs []string) *Server {
	t.Helper()
	if !mainRuns {
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m), which fails a run leaving a NATS server of this process running")
	}
	if config == "" {
		config = hostOnlyConfig
	}
	dir, err := os.MkdirTemp("", "testnats")
	if err != nil {
		t.Fatalf("make the server's directory: %v", err)
	}
	server := &Server{store: filepath.Join(dir, "store"), log: &logCapture{}}
	t.Cleanup(func() {
		server.stop()
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove the test NATS server's directory: %v", err)
		}
	})
	configPath := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write the server's configuration: %v", err)
	}
	opts, err := serverOptions(configPath, serverArgs, server.store)
	if err != nil {
		t.Fatalf("read the NATS configuration: %v", err)
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("create the NATS server: %v", err)
	}
	srv.SetLoggerV2(server.log, opts.Debug, opts.Trace, false)
	go srv.Start()
	if !srv.ReadyForConnections(readinessTimeout) {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatalf("NATS did not accept connections within %s: %s", readinessTimeout, server.log.String())
	}
	server.url = srv.ClientURL()
	server.mu.Lock()
	server.srv = srv
	server.mu.Unlock()
	liveMu.Lock()
	live[server] = struct{}{}
	liveMu.Unlock()
	return server
}

// stop shuts the server down and returns once it has, so the store the caller then removes is one
// nothing is writing. Stopping a stopped server does nothing.
func (s *Server) stop() {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.mu.Unlock()
	if srv == nil {
		return
	}
	srv.Shutdown()
	srv.WaitForShutdown()
	liveMu.Lock()
	delete(live, s)
	liveMu.Unlock()
}

// flagsMu serialises the calls that meet over natsserver.FlagSnapshot: ConfigureOptions sets it,
// whichever server made it.
var flagsMu sync.Mutex

// serverOptions parses the configuration at path and serverArgs as nats-server parses its own
// command line, then imposes what this rig owns: loopback, a client port the server picks, the
// test's own store, no signal handlers, and path as the configuration file. serverArgs replace
// nats-server's defaults rather than add to them.
func serverOptions(path string, serverArgs []string, store string) (*natsserver.Options, error) {
	flags := flag.NewFlagSet("nats-server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	silent := func() {}
	flagsMu.Lock()
	opts, err := natsserver.ConfigureOptions(flags, append([]string{"-c", path}, serverArgs...), silent, silent, silent)
	// ConfigureOptions leaves its parse in natsserver.FlagSnapshot, which every later ReloadOptions
	// in this process merges over the configuration it is given, whatever server it belongs to.
	// Each server here carries its own options, so nothing of this parse outlives it.
	natsserver.FlagSnapshot = nil
	flagsMu.Unlock()
	if err != nil {
		return nil, err
	}
	opts.Host = "127.0.0.1"
	opts.Port = -1
	opts.StoreDir = store
	opts.NoSigs = true
	opts.ConfigFile = path
	return opts, nil
}

// logCapture is the server's logger: it keeps every line the server logs, for Server.Log.
type logCapture struct {
	mu   sync.Mutex
	text strings.Builder
}

func (c *logCapture) write(level, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(&c.text, "[%s] %s\n", level, line)
}

func (c *logCapture) Noticef(format string, args ...any) { c.write("INF", format, args...) }
func (c *logCapture) Warnf(format string, args ...any)   { c.write("WRN", format, args...) }
func (c *logCapture) Errorf(format string, args ...any)  { c.write("ERR", format, args...) }
func (c *logCapture) Fatalf(format string, args ...any)  { c.write("FTL", format, args...) }
func (c *logCapture) Debugf(format string, args ...any)  { c.write("DBG", format, args...) }
func (c *logCapture) Tracef(format string, args ...any)  { c.write("TRC", format, args...) }

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

// StartContainerNkeyAuthorizedUsers is StartNkeyAuthorizedUsers on a NATS container of image,
// removed when t ends, for the one test whose subject is the published server images themselves
// (packages/envoy/testnatsimages) rather than anything the daemon does. It returns the container,
// whose log that test reads, beside the client URL. Where no container runtime answers it skips
// the test, naming what the test needs.
func StartContainerNkeyAuthorizedUsers(t testing.TB, image string, users ...NkeyUser) (*tcnats.NATSContainer, string) {
	t.Helper()
	RequireContainerRuntime(t)
	started, err := startContainer(image, tcnats.WithConfigFile(strings.NewReader(nkeyConfig(users))))
	if err != nil {
		t.Fatalf("start nkey-authorized NATS: %v", err)
	}
	testcontainers.CleanupContainer(t, started)
	url, err := started.ConnectionString(context.Background())
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
	answering(t, url)
	return started, url
}

// limits caps a NATS test container at one CPU and 512 MiB, as every container the repository's
// tests and proofs start is capped on a shared host: a test server holds a few small streams.
var limits = testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
	hostConfig.NanoCPUs = 1_000_000_000
	hostConfig.Memory = 512 << 20
})

// startContainer runs a NATS container. A container Docker created and never saw ready (a
// readiness wait that timed out under load) comes back beside the error, and is removed before
// startContainer returns.
func startContainer(image string, options ...testcontainers.ContainerCustomizer) (*tcnats.NATSContainer, error) {
	started, err := tcnats.Run(context.Background(), image, append([]testcontainers.ContainerCustomizer{limits}, options...)...)
	if err != nil {
		return started, errors.Join(err, testcontainers.TerminateContainer(started))
	}
	return started, nil
}

// containerProbeTimeout bounds the one question asked of a container runtime: whether it is there.
const containerProbeTimeout = 10 * time.Second

// containerRuntimeOnce holds the probe's answer for the test binary's life: a machine with no
// container runtime when the first test asks has none for the rest of the run.
var (
	containerRuntimeOnce sync.Once
	containerRuntimeErr  error
)

// RequireContainerRuntime skips t, naming the resource it needs, where no container runtime
// answers. A test whose subject is a container image calls it first, so the whole test skips
// rather than each of its cases in turn.
func RequireContainerRuntime(t testing.TB) {
	t.Helper()
	containerRuntimeOnce.Do(func() { containerRuntimeErr = probeContainerRuntime() })
	if containerRuntimeErr != nil {
		t.Skipf("this test needs a container runtime, a Docker socket testcontainers can reach: %v", containerRuntimeErr)
	}
}

// probeContainerRuntime reports why testcontainers cannot reach a container runtime on this
// machine, or nil when it can.
//
// testcontainers has no answer to that question that is not a panic: every route to a client runs
// through its internal MustExtractDockerHost, which panics when neither DOCKER_HOST, a Docker
// context nor a socket on disk names a host, and a panic on a test's goroutine ends the whole
// test binary rather than the test. So the probe recovers that panic and reports it as the reason.
func probeContainerRuntime() (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("testcontainers found no Docker host: %v", recovered)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), containerProbeTimeout)
	defer cancel()
	client, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return err
	}
	return client.Close()
}

// answering returns once the server at url refuses a connection with no credential with its own
// authorization violation: the proof it speaks the client protocol and enforces its nkey users. A
// server can answer a dial before it serves the NATS protocol, and until then a dial ends in EOF
// or a refusal that says nothing about the server's users. A server that admits the connection
// enforces no users, and every refusal a test expects would pass for the wrong reason, so that
// fails the test.
func answering(t testing.TB, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), readinessTimeout)
	defer cancel()
	for {
		conn, err := nats.Connect(url, nats.Timeout(time.Second), nats.NoReconnect())
		if err == nil {
			conn.Close()
			t.Fatalf("NATS at %s admitted a client with no credential: it enforces no nkey users", url)
		}
		if errors.Is(err, nats.ErrAuthorization) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for NATS at %s to answer within %s: %v", url, readinessTimeout, err)
		case <-time.After(retryInterval):
		}
	}
}

// connect returns a connection to the server at url once its JetStream API answers: nats-server
// accepts clients before JetStream has finished starting.
func connect(t testing.TB, url string) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), readinessTimeout)
	defer cancel()
	for {
		conn, js, err := ready(ctx, url)
		if err == nil {
			return conn, js
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for NATS JetStream readiness: %v", err)
		case <-time.After(retryInterval):
		}
	}
}

func ready(ctx context.Context, url string) (*nats.Conn, jetstream.JetStream, error) {
	conn, err := nats.Connect(url, nats.Timeout(time.Second))
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(conn)
	if err == nil {
		_, err = js.AccountInfo(ctx)
	}
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, js, nil
}
