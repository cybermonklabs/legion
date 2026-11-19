// Package testnats runs NATS servers for Envoy's tests in the test process itself, and gives the
// tests that need no server of their own one server per test binary.
//
// Every server these helpers start is the nats-server this binary links in
// (github.com/nats-io/nats-server/v2), not a container: a test that needed a container runtime
// could not run where Envoy's agents do, which have none. Each server listens on a loopback port
// of its own choosing and is reached through a relay this process owns. The relay's port is the
// one the test's URL names and it holds that port for the test's life, which buys four things: a
// URL that survives a stop and a start (Server.Stop, Server.Start), a way to stop the bytes
// moving in either direction without dropping a connection (Server.Freeze), or the server's
// replies alone (Server.FreezeReplies), a reload with no client message in flight
// (Server.Reload), and a port no other test binary can take while the test runs.
//
// A package's tests share one server rather than start one each, and each test gets it as the user
// of a JetStream account no earlier test used (URL), so no test ever deletes and recreates a
// stream another test made. nats-server moves a deleted stream's directory aside and removes it
// from a background goroutine; a second delete of the same name before that goroutine has run
// leaves the stream's files where they were while it answers success, and the next create of that
// name recovers the deleted stream's messages. Every account keeps its streams in a directory of
// its own, so the names one test uses cannot reach another.
//
// The daemon's tests have a copy of this rig of their own, packages/daemon/internal/testnats, which
// starts a server in the test process as start and launch here do, one for each test. The two are
// copies rather than one package because Go's internal rule keeps the daemon's module from
// importing this one, so a change to how a server is started or configured belongs in both.
package testnats

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/testwait"
)

// Image is ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml), whose
// server, v2.10.29, Envoy refuses to run its session consumers on (natsversion.Refusal): a test
// names it only for a behaviour of that server itself, the listener's refusal of it among them,
// and the consumer cap's on a server older than 2.15. The envoy-go CI job reads this declaration
// and pulls the image before its first test step, so no test reaches the registry mid-run; keep it
// a single-line string constant.
const Image = "ghcr.io/cybermonklabs/legion/ci@sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f"

// HubImage is the NATS server image the GCP hub runs: ghcr.io/cybermonklabs/legion/ci, the copy of
// nats:2.15.1-alpine (.github/ci-images.yaml). The envoy-go and envoy-infra-go CI jobs read this
// declaration and pull the image before their tests run; keep it a single-line string constant.
const HubImage = "ghcr.io/cybermonklabs/legion/ci@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6"

// Images are the NATS server images the real-server tests run Envoy on, each with the short name
// its subtest takes: the hub's alone, since Envoy requires a server whose scan of a consumer's
// stored messages reads every filter (natsversion), which nats:2.10 is not.
var Images = []struct{ Name, Ref string }{{"hub", HubImage}}

var (
	mainRuns      bool
	sharedOnce    sync.Once
	shared        *accountServer
	sharedCleanup *sharedTB
)

// firstAccounts is how many accounts the shared server starts with. Every time a test asks for
// one past them, the server doubles them by a reload: a server's start time grows faster than its
// account count (0.9 s for 1,000 on a loaded devbox, 26 s for 5,000), and at half a CPU doubling
// 4,096 accounts took 36 s and 65 s in two measurements, which the test that asks for account
// 4,097 waits out through Connect's wait. A package run takes one account for each test and
// subtest that asks (103 for cmd/listener, 64 for internal/store), so -count=40 of cmd/listener in
// one binary reaches that doubling.
const firstAccounts = 256

// init runs in every test binary that imports this package, whichever helper its tests use. Every
// server these helpers start has no users unless a test configures some, and nats.go refuses an
// nkey when the server sends no nonce ("nats: nkeys not supported by the server"), so an
// operator's NATS_NKEY_SEED or NATS_NKEY_SEED_FILE never reaches the tests' clients. A test that
// means to pass a seed sets it itself.
func init() {
	os.Unsetenv("NATS_NKEY_SEED")
	os.Unsetenv("NATS_NKEY_SEED_FILE")
}

// Main runs the package's tests through run, then shuts the shared server down and removes its
// store if a test started it, and returns the exit code. A package whose tests use URL calls it
// from TestMain: os.Exit(testnats.Main(m.Run)). It takes the run function rather than *testing.M
// so that it composes with another fixture's Main, as storetest's does:
// os.Exit(storetest.Main(func() int { return testnats.Main(m.Run) })).
func Main(run func() int) int {
	mainRuns = true
	code := run()
	if sharedCleanup != nil {
		sharedCleanup.run()
		if sharedCleanup.failed && code == 0 {
			code = 1
		}
	}
	return code
}

// URL returns the package's shared NATS server as the user of a JetStream account no earlier test
// used, so none of a previous test's streams, KV buckets, consumers or messages is there, and no
// stream name another test deleted can be recovered under it. A test gets one account however
// often it asks, so every connection it makes reaches the same streams; each subtest is a test of
// its own and gets its own account, since subtests that each delete and recreate one name in a
// shared account would race as tests did on a shared server.
// When the test ends, its account's streams are deleted to free their storage; nothing creates a
// stream in that account again, so a delete that leaves files behind reaches no test.
func URL(t testing.TB) string {
	t.Helper()
	if !mainRuns {
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m.Run), which shuts the shared NATS server down")
	}
	sharedOnce.Do(func() {
		sharedCleanup = &sharedTB{TB: t}
		shared = startAccountServer(sharedCleanup, firstAccounts)
	})
	if shared == nil {
		t.Fatal("testnats: the package's shared NATS server did not start; the test that first asked for it reports why")
	}
	return shared.url(t)
}

// sharedTB stands in for the test that first asks for the package's shared server. That server
// outlives the test, so the cleanup start registers on it runs from Main, after the last test, and
// what the cleanup reports goes to stderr and the exit code: a t.Errorf on a test that has already
// ended panics.
type sharedTB struct {
	testing.TB
	cleanups []func()
	failed   bool
}

func (s *sharedTB) Cleanup(clean func()) { s.cleanups = append(s.cleanups, clean) }

func (s *sharedTB) Errorf(format string, args ...any) {
	s.failed = true
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// run runs the cleanups the shared server registered, the most recent first, as testing does.
func (s *sharedTB) run() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
}

// accountServer is a NATS server that hands each test a JetStream account no earlier test used.
type accountServer struct {
	srv  *Server
	host string

	mu sync.Mutex
	// declared is how many accounts the server's configuration declares, and handedOut how many of
	// them it has handed to a test. handedOut only grows: an account is never handed out twice.
	declared  int
	handedOut int
	// holders maps each test holding an account to that account's URL.
	holders map[string]string
}

// startAccountServer starts a server declaring accounts accounts, removed when t ends.
func startAccountServer(t testing.TB, accounts int) *accountServer {
	t.Helper()
	srv := start(t, accountsConfig(accounts), []string{"-js"})
	return &accountServer{
		srv:      srv,
		host:     strings.TrimPrefix(srv.URL(), "nats://"),
		declared: accounts,
		holders:  map[string]string{},
	}
}

// url is URL on s.
func (s *accountServer) url(t testing.TB) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if uri, ok := s.holders[t.Name()]; ok {
		return uri
	}
	if s.handedOut == s.declared {
		if err := s.srv.Reload(t, accountsConfig(2*s.declared)); err != nil {
			t.Fatalf("NATS refused %d accounts: %v", 2*s.declared, err)
		}
		s.declared *= 2
	}
	s.handedOut++
	account := s.handedOut
	uri := fmt.Sprintf("nats://t%d:t%d@%s", account, account, s.host)
	// Wait for the account to answer, after the start or the reload that declared it.
	Connect(t, uri).Close()
	name := t.Name()
	s.holders[name] = uri
	t.Cleanup(func() {
		s.mu.Lock()
		delete(s.holders, name)
		s.mu.Unlock()
		deleteStreams(t, uri)
	})
	return uri
}

// accountsConfig declares accounts T1 to Tn, each with JetStream and one user, tn, whose password
// is its name.
func accountsConfig(n int) string {
	var config strings.Builder
	config.WriteString("accounts {\n")
	for account := 1; account <= n; account++ {
		fmt.Fprintf(&config, "  T%d: { jetstream: enabled, users: [ { user: t%d, password: t%d } ] }\n", account, account, account)
	}
	config.WriteString("}\n")
	return config.String()
}

// deleteStreams deletes every stream in the account uri names, as its test ends.
func deleteStreams(t testing.TB, uri string) {
	t.Helper()
	conn := Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Errorf("open JetStream to delete the test's streams: %v", err)
		return
	}
	var streams []string
	for name := range js.StreamNames() {
		streams = append(streams, name)
	}
	for _, name := range streams {
		if err := js.DeleteStream(name); err != nil && !errors.Is(err, natsgo.ErrStreamNotFound) {
			t.Errorf("delete the test's stream %s: %v", name, err)
		}
	}
}

// Server is a nats-server running in this test process, reached through a relay this process owns.
// Every helper of this package that starts a server returns one, and start is the only thing that
// makes one.
type Server struct {
	// store is the server's JetStream store, configPath its configuration file and args the
	// arguments it was started with: a restart and a reload read the configuration again from that
	// file, so a Reload the server took is the configuration a later start serves.
	store      string
	configPath string
	args       []string
	log        *logCapture
	relay      *relay
	url        string
	// name is the server_name StartCluster gives a member, "" for a lone server.
	name string

	mu        sync.Mutex
	srv       *natsserver.Server
	jetStream bool
	monitor   string
}

// URL is the server's client URL, which stays the same across every Stop and Start: it names the
// relay's port, not the server's own.
func (s *Server) URL() string { return s.url }

// Name is the server_name of a cluster member (StartCluster), the name JetStream reports it by in
// a stream's or a consumer's cluster information. A lone server has none and answers "".
func (s *Server) Name() string { return s.name }

// MonitorURL is the server's monitoring endpoint, where /varz and /connz are. The monitoring
// listener is the server's own rather than the relay's, so a Stop and a Start give it a new port
// and a test reads this again after one.
func (s *Server) MonitorURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.monitor
}

// Stop shuts the server down, as stopping a server an operator restarts does, and keeps its store
// and its URL for the Start that follows. Stopping a stopped server does nothing.
func (s *Server) Stop(t testing.TB) {
	t.Helper()
	s.stop()
}

func (s *Server) stop() {
	srv := s.detach()
	if srv == nil {
		return
	}
	srv.Shutdown()
	srv.WaitForShutdown()
}

// detach takes the running nats-server off s and returns it, or nil when none runs, for the caller
// to shut down: from then on the relay refuses every new connection, as it does for a stopped
// server, and Start treats the server as stopped.
func (s *Server) detach() *natsserver.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	srv := s.srv
	s.srv = nil
	s.monitor = ""
	return srv
}

// LameDuck shuts the server down as an operator's lame-duck signal does: the server stops
// accepting clients, hands off the JetStream leaderships it holds, and closes its clients over its
// lame-duck window (rigLameDuck), so they move to the other servers they know rather than all at
// once. It returns once the server has shut down, keeping its store and URL for a later Start as
// Stop does, and has no bound of its own, as Stop has none. A stopped server does nothing. It takes
// no t, so a caller may run it on a goroutine of its own while the test's goroutine waits for the
// clients to move, and wait for its end there (testwait.Receive on a channel it closes after). It
// returns only once the lame duck itself has returned, which shuts JetStream down, writing every
// stream's state to the store, before it shuts the server down, so the cleanup start registers
// never removes that store under it. It returns nil: the error result is main's, from when a lame
// duck had a bound of its own.
// A2's callers still use this error result; A2 deletes it with its last caller.
func (s *Server) LameDuck() error {
	srv := s.detach()
	if srv == nil {
		return nil
	}
	srv.LameDuckShutdown()
	srv.WaitForShutdown()
	return nil
}

// Start runs the server again on the store and the configuration it had, and returns once it
// accepts connections. Its clients reach it at the same URL: the relay held the port while it was
// down, and now joins each connection to wherever the server listens.
func (s *Server) Start(t testing.TB) {
	t.Helper()
	s.mu.Lock()
	running := s.srv != nil
	s.mu.Unlock()
	if running {
		t.Fatal("start a NATS server that is already running")
	}
	s.launch(t)
}

// Terminate shuts the server down for good and closes its relay, which frees the URL's port. The
// cleanup start registers does this itself, so a test calls it only to end a server early.
func (s *Server) Terminate(t testing.TB) {
	t.Helper()
	s.stop()
	s.relay.Close()
}

// Freeze stops the relay moving bytes between the test and the server, in either direction, and
// returns the func that thaws it. Both sides keep their connections, so a request made while the
// relay is frozen times out on a connection that is still CONNECTED, as a server that has stopped
// answering leaves it, and the traffic resumes when the returned func runs.
func (s *Server) Freeze(t testing.TB) func() {
	t.Helper()
	return s.relay.freeze()
}

// FreezeReplies stops the relay passing what the server sends its clients, while what they send
// goes on reaching the server, and returns the func that passes it again, as Freeze's does. The
// server serves every request and stores every publish it is sent, and its answers wait in the
// relay: a JetStream publish is stored and its acknowledgement never reaches the client, which is
// an acknowledgement lost on its way back, on a connection that stays CONNECTED. What the server
// holds meanwhile is read beside the relay (StoredOn).
func (s *Server) FreezeReplies(t testing.TB) func() {
	t.Helper()
	return s.relay.freezeReplies()
}

// StoredOn is how many messages stream holds on subject, read by a client of the server's own in
// this process (natsgo.InProcessServer) rather than through the relay, so it answers while the
// relay holds the server's replies (FreezeReplies) or is frozen (Freeze). It reads a server that
// admits a client with no credential, as Start's does, and fails t when the read fails.
func (s *Server) StoredOn(t testing.TB, stream, subject string) uint64 {
	t.Helper()
	srv := s.natsServer()
	if srv == nil {
		t.Fatalf("read stream %s of a NATS server that is not running", stream)
	}
	conn, err := natsgo.Connect(srv.ClientURL(), natsgo.InProcessServer(srv), natsgo.Name("testnats stored-on"))
	if err != nil {
		t.Fatalf("connect to the NATS server in this process: %v", err)
	}
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream on the NATS server in this process: %v", err)
	}
	info, err := js.StreamInfo(stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject}, natsgo.Context(testwait.Context(t)))
	if err != nil {
		t.Fatalf("read what stream %s holds on %s in the NATS server itself: %v", stream, subject, err)
	}
	return info.State.Subjects[subject]
}

// Reload has the running server take config as its whole configuration, as an operator signalling
// a reload does: the server keeps its connections and applies the new configuration to them. It
// returns the server's own refusal when the server would not take the configuration, and nil when
// it did. A refused configuration never becomes the server's: the file a restart reads keeps the
// configuration the server is serving.
//
// The server reloads with no client message in flight. nats-server (2.15.1 and 2.15.3) writes each
// service import's account on every authorization reload (configureAccounts, server.go:1398) under
// the importing account's lock, and a client's request through a service import - every JetStream
// API request - reads that field without the lock as the server sets up the response
// (setupResponseServiceImport and addRespServiceImport, client.go:4643 and accounts.go:2671 in
// 2.15.1): a reload while a client's request is being processed is a data race in nats-server,
// which the race detector reports in this process. Holding the clients' bytes is not enough on its
// own, since a request the server has already read can still be mid-way through: so the relay
// holds what the clients send at a message boundary (relay.hold), sends the server a PING of its
// own on each client's connection and waits for its PONG (relay.barrier), which the server sends
// only once it has processed everything the client sent before it, the server reloads, and the
// relay lets the clients' bytes through. Each client keeps its connection and sees its messages
// delayed by the reload; what the server sends reaches the clients throughout, less the relay's
// own PONGs. A server whose relay is frozen cannot answer the relay, so its Reload fails.
func (s *Server) Reload(t testing.TB, config string) error {
	t.Helper()
	s.mu.Lock()
	srv, jetStream := s.srv, s.jetStream
	s.mu.Unlock()
	if srv == nil {
		t.Fatal("reload the configuration of a NATS server that is not running")
	}
	pending := s.configPath + ".pending"
	if err := os.WriteFile(pending, []byte(config), 0o600); err != nil {
		t.Fatalf("write the new configuration: %v", err)
	}
	defer func() { _ = os.Remove(pending) }()
	opts, err := serverOptions(pending, s.args, s.store)
	if err != nil {
		return err
	}
	// ReloadOptions refuses an Options whose listening ports differ from the ones the server was
	// started with ("config reload not supported for HTTPPort: old=-1, new=52544"), so the new
	// options carry what the server was started with, which serverOptions imposes, and never a
	// port read back from the running server. The configuration file and JetStream are the
	// server's as well: a reload can neither move the one nor turn the other on or off.
	opts.ConfigFile = s.configPath
	opts.JetStream = jetStream
	release, err := s.relay.hold(t)
	if err == nil {
		err = s.relay.barrier(t)
	}
	if err != nil {
		release()
		t.Fatalf("hold the NATS server's clients still for its reload: %v", err)
	}
	// ConfigureOptions and ReloadOptions meet over natsserver.FlagSnapshot, a package-level
	// variable of nats-server; serverOptions clears it under the same lock this takes.
	flagsMu.Lock()
	err = srv.ReloadOptions(opts)
	flagsMu.Unlock()
	release()
	if err != nil {
		return err
	}
	if err := os.Rename(pending, s.configPath); err != nil {
		t.Fatalf("keep the configuration the server took: %v", err)
	}
	return nil
}

// Restart stops the server, makes config its whole configuration and starts it again on the same
// store and URL, as an operator restarts a server after changing a grant, and returns once it
// accepts connections. Unlike Reload, every connection drops: each client reconnects to the server
// config governs and sends its subscriptions again, which that server accepts or refuses afresh.
func (s *Server) Restart(t testing.TB, config string) {
	t.Helper()
	s.stop()
	if err := os.WriteFile(s.configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write the new configuration: %v", err)
	}
	s.launch(t)
}

// Log is everything the server has logged so far, the debug and protocol trace among it when the
// server was started with -DV.
func (s *Server) Log() string { return s.log.String() }

// CutStream makes stream lose data as nats-server's own
// TestJetStreamConsumerReconcileConsumerAfterStreamDataLoss makes its stream lose it
// (server/jetstream_consumer_test.go v2.15.1): it stops the server, truncates the stream's first
// message block (jetstream/$G/streams/<stream>/msgs/1.blk in the store) to the size keep returns
// for the block's size, removes the stream's index (msgs/index.db beside it), and starts the
// server again, which rebuilds the stream from what the block still holds. A keep that returns 0
// cuts every message the block held, and one that returns half the size plus one byte cuts the end
// of them. It returns the block's size before the cut and after it, for the test's log, and fails
// the test when the block or the index is not there.
//
// It works for a server whose streams are in the global account ($G), which is where the streams
// of a server one Start made are: the shared server (URL) keeps each test's streams in the
// directory of that test's account instead. The store survives the cut as it survives every Stop
// and Start (Stop), so the restarted server recovers the stream and its consumers from what the
// cut left of them.
func (s *Server) CutStream(t testing.TB, stream string, keep func(size int64) int64) (before, after int64) {
	t.Helper()
	s.Stop(t)
	messages := filepath.Join(s.store, "jetstream", "$G", "streams", stream, "msgs")
	block, index := filepath.Join(messages, "1.blk"), filepath.Join(messages, "index.db")
	stat, err := os.Stat(block)
	if err != nil {
		t.Fatalf("the stream's first message block: %v", err)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("the stream's index: %v", err)
	}
	before, after = stat.Size(), keep(stat.Size())
	if err := os.Truncate(block, after); err != nil {
		t.Fatalf("cut the stream's first message block: %v", err)
	}
	if err := os.Remove(index); err != nil {
		t.Fatalf("remove the stream's index: %v", err)
	}
	s.Start(t)
	return before, after
}

// address is where the server listens for clients at this moment, which the relay dials. A stopped
// server has none, and the relay ends the connection as a refused dial would.
func (s *Server) address() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return "", errors.New("the NATS server is stopped")
	}
	addr := s.srv.Addr()
	if addr == nil {
		return "", errors.New("the NATS server has no client address")
	}
	return addr.String(), nil
}

// clusterAddress is where the server listens for routes at this moment, which its route relay
// dials (StartCluster). A stopped server has none, as address says.
func (s *Server) clusterAddress() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return "", errors.New("the NATS server is stopped")
	}
	addr := s.srv.ClusterAddr()
	if addr == nil {
		return "", errors.New("the NATS server has no route address")
	}
	return addr.String(), nil
}

// natsServer is the running nats-server, or nil while the server is stopped.
func (s *Server) natsServer() *natsserver.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv
}

// running reports whether the server is running: started and not stopped since.
func (s *Server) running() bool { return s.natsServer() != nil }

// hostOnlyConfig is the configuration of a server a test starts with none of its own:
// nats-server warns on an empty configuration file ("config has no values or is empty"), and the
// host it names here is the one this rig imposes on every server anyway. The file is never empty,
// so a server started without a configuration can still be reloaded with one.
const hostOnlyConfig = "host: 127.0.0.1\n"

// start runs a nats-server in this process whose whole configuration is config, with serverArgs as
// its command line after it (so -js for JetStream, -DV for the protocol trace), behind a relay
// holding the port of the URL it returns, and returns it once it accepts connections. When the
// test ends the server is shut down, the relay closed and the server's directory removed; until
// then AwaitConsumerStored finds the server by the relay address its URL names. The relay listens
// on 127.0.0.1; startOn is start with the relay on another loopback address.
func start(t testing.TB, config string, serverArgs []string) *Server {
	t.Helper()
	return startOn(t, "127.0.0.1", config, serverArgs)
}

// startOn is start with the relay listening on host. It is the one place a server of this
// package is made: every helper here is a call on it.
func startOn(t testing.TB, host, config string, serverArgs []string) *Server {
	t.Helper()
	if config == "" {
		config = hostOnlyConfig
	}
	dir, err := os.MkdirTemp("", "testnats")
	if err != nil {
		t.Fatalf("make the server's directory: %v", err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("hold the server's port: %v", err)
	}
	address := listener.Addr().String()
	server := &Server{
		store:      filepath.Join(dir, "store"),
		configPath: filepath.Join(dir, "nats.conf"),
		args:       serverArgs,
		log:        &logCapture{},
		url:        "nats://" + address,
	}
	server.relay = newRelay(listener, server.address, true)
	registerServer(address, server)
	t.Cleanup(func() {
		unregisterServer(address)
		server.stop()
		server.relay.Close()
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove the test NATS server's directory: %v", err)
		}
	})
	if err := os.WriteFile(server.configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write the server's configuration: %v", err)
	}
	server.launch(t)
	return server
}

// unbounded is the readiness wait under -timeout 0, where the test binary has no deadline.
const unbounded = time.Duration(math.MaxInt64)

// launch runs a nats-server on the server's configuration and store, and returns once it accepts
// connections. That wait is nats-server's own (ReadyForConnections), which takes a span: it is
// given what is left before the test binary's deadline less testwait's margin (testwait.Context),
// the bound every wait of these tests has, and no bound under -timeout 0, as the daemon's copy of
// this rig gives its own.
func (s *Server) launch(t testing.TB) {
	t.Helper()
	opts, err := serverOptions(s.configPath, s.args, s.store)
	if err != nil {
		t.Fatalf("read the NATS configuration: %v", err)
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("create the NATS server: %v", err)
	}
	srv.SetLoggerV2(s.log, opts.Debug, opts.Trace, false)
	go srv.Start()
	within := unbounded
	if deadline, bounded := testwait.Context(t).Deadline(); bounded {
		within = time.Until(deadline)
	}
	if !srv.ReadyForConnections(within) {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatalf("NATS did not accept connections within %s, what was left before the test binary's deadline less testwait's margin: %s", within, s.log.String())
	}
	// Before s.srv is set the relay refuses every connection, so no client request reaches the
	// server before its system account's response timers are set past any test (responseThreshold).
	if err := outliveResponseTimers(srv); err != nil {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatalf("keep the NATS server's response timers from firing during the test: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.srv = srv
	s.jetStream = opts.JetStream
	if addr := srv.MonitorAddr(); addr != nil {
		s.monitor = "http://" + addr.String()
	}
}

// flagsMu serialises the calls that meet over natsserver.FlagSnapshot: ConfigureOptions sets it and
// ReloadOptions merges it over the options it is given, whichever server made it.
var flagsMu sync.Mutex

// rigLameDuck is the lame_duck_duration and rigLameDuckGrace the lame_duck_grace_period of every
// server whose configuration sets neither: nats-server's two minutes would hold a test that long
// in each Server.LameDuck, and its configuration file refuses a duration under 30 seconds, so the
// rig sets them on the options it parsed. Four seconds still spreads the closing of the server's
// clients over time, after a second in which they hear the server is going.
const (
	rigLameDuck      = 4 * time.Second
	rigLameDuckGrace = time.Second
)

// serverOptions parses the configuration at path and serverArgs as nats-server parses its own
// command line, then imposes what this rig owns: loopback, a client and a monitoring port the
// server picks, the test's own store, no signal handlers, path as the configuration file, and the
// lame duck's timing when the configuration sets none (rigLameDuck). serverArgs replace
// nats-server's defaults rather than add to them, as the arguments after the image's own command
// do.
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
	opts.HTTPHost = "127.0.0.1"
	opts.HTTPPort = -1
	opts.StoreDir = store
	opts.NoSigs = true
	opts.ConfigFile = path
	if opts.LameDuckDuration == 0 && opts.LameDuckGracePeriod == 0 {
		opts.LameDuckDuration = rigLameDuck
		opts.LameDuckGracePeriod = rigLameDuckGrace
	}
	// A server with an auth callout holds a client it sends to the service until the service
	// answers or its authorization timeout passes (nats-server v2.15.1 server/auth_callout.go:457-464),
	// and it starts that client's first PING timer before it asks (client.go:2352-2353): two seconds
	// plus up to a fifth of that (client.go:7136-7142), against the callout's two. So a client the
	// service never answers is now and then sent a PING before its refusal, and nats.go fails the
	// connect with "expected 'PONG', got 'PING'" rather than nats.ErrAuthorization, which a test
	// that waits out the hub's timeout for the refusal reads as a failure. The rig turns the short
	// first PING off on such a server, so its first PING waits for the ping interval, two minutes.
	if opts.AuthCallout != nil {
		opts.DisableShortFirstPing = true
	}
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

// Start runs a NATS server with JetStream, shut down when the test ends (even when its start
// fails), and returns it with its client URL. That URL stays the server's across every Stop and
// Start a test makes: it names a port this process holds for the test's life, whatever port the
// server itself listens on after a restart, and the restart keeps the server's JetStream store,
// as a server restarted on its own volume does.
func Start(t testing.TB) (*Server, string) {
	t.Helper()
	server := start(t, "", []string{"-js"})
	return server, server.URL()
}

// StartNkeyAuthorized runs a NATS server with JetStream that accepts only clients authenticating
// as the nkey user whose public key is user, shut down when the test ends, and returns its client
// URL once the server refuses a client with no credential (awaitAuth).
func StartNkeyAuthorized(t testing.TB, user string) string {
	t.Helper()
	_, uri := StartNkeyAuthorizedServer(t, user)
	return uri
}

// StartNkeyAuthorizedServer runs the server StartNkeyAuthorized runs and returns it with its
// client URL, for a test that must stop and start it under a connected client.
func StartNkeyAuthorizedServer(t testing.TB, user string) (*Server, string) {
	t.Helper()
	return startNkeyConfig(t, nkeyConfig(fmt.Sprintf("{ nkey: %q }", user)))
}

// NkeyGrant is a NATS test server, started by StartNkeyGranted, whose one nkey user may publish
// only to the subjects its grant names; Grant changes the grant.
type NkeyGrant struct {
	URL  string
	user string
	srv  *Server
}

// StartNkeyGranted runs a NATS server as StartNkeyAuthorized does, whose one user may publish
// only to the subjects allow names (and subscribe to anything): a publish to any other subject is
// the server's permissions violation, as a per-client grant makes it. The test can change the
// grant.
func StartNkeyGranted(t testing.TB, user string, allow ...string) *NkeyGrant {
	t.Helper()
	server, uri := startNkeyConfig(t, publishGrantConfig(user, allow))
	return &NkeyGrant{URL: uri, user: user, srv: server}
}

// Grant replaces the user's publish grant with allow and has the server reload its configuration,
// as an operator changing a grant does: the server keeps its connections and applies the new grant
// to them. It fails the test, with the server's reason, when the server refused the new
// configuration.
func (g *NkeyGrant) Grant(t testing.TB, allow ...string) {
	t.Helper()
	if err := g.srv.Reload(t, publishGrantConfig(g.user, allow)); err != nil {
		t.Fatalf("NATS refused the new grant %q: %v", allow, err)
	}
}

func publishGrantConfig(user string, allow []string) string {
	return grantConfig(user, "publish", allow)
}

// StartNkeyDenied runs a NATS server with JetStream whose nkey user, the one whose public key is
// user, may publish to anything but deny, and whose no_auth_user, an admin in the same account, may
// do anything, and returns its client URL. A test plays a program whose grant lacks the subjects of
// deny with the user (its seed in NATS_NKEY_SEED), and sets the server up, its stream included,
// through a connection that names no credential.
func StartNkeyDenied(t testing.TB, user string, deny ...string) string {
	t.Helper()
	_, admin := User(t)
	quoted := make([]string, len(deny))
	for index, subject := range deny {
		quoted[index] = fmt.Sprintf("%q", subject)
	}
	config := fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ { nkey: %q, permissions: { publish: { deny: [%s] } } }, { nkey: %q } ]\n}\nno_auth_user: %q\n",
		user, strings.Join(quoted, ", "), admin, admin)
	_, uri := StartConfig(t, config, []string{"-js"}, true)
	return uri
}

// SubscribeGrantConfig is the configuration of a server with JetStream whose one user, the nkey user
// whose public key is user, may subscribe only to the subjects allow names, in any queue group, and
// publish to anything: a subscription to any other subject is the server's permissions violation,
// as a per-client grant makes it. StartConfig takes it, and Restart or Reload a changed one.
func SubscribeGrantConfig(user string, allow ...string) string {
	return grantConfig(user, "subscribe", allow)
}

// grantConfig is the configuration of a server with JetStream whose one nkey user may use direction
// (publish or subscribe) only on the subjects allow names.
func grantConfig(user, direction string, allow []string) string {
	quoted := make([]string, len(allow))
	for index, subject := range allow {
		quoted[index] = fmt.Sprintf("%q", subject)
	}
	return nkeyConfig(fmt.Sprintf("{ nkey: %q, permissions: { %s: { allow: [%s] } } }",
		user, direction, strings.Join(quoted, ", ")))
}

func nkeyConfig(entry string) string {
	return fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ %s ]\n}\n", entry)
}

// startNkeyConfig runs a NATS server with JetStream and the debug and protocol trace in its log,
// whose configuration is config: StartConfig with the arguments -DV -js. config must declare the
// users it enforces.
func startNkeyConfig(t testing.TB, config string) (*Server, string) {
	t.Helper()
	return StartConfig(t, config, []string{"-DV", "-js"}, false)
}

// StartConfig runs a NATS server whose whole configuration is config, with serverArgs as its
// command line after it (so -js for JetStream), shut down when the test ends (even when its start
// fails). It returns the server with its client URL once it answers a client with no credential
// (awaitAuth): with anonymous false, by refusing it; with anonymous true, by admitting it, as a
// configuration naming a no_auth_user makes it.
func StartConfig(t testing.TB, config string, serverArgs []string, anonymous bool) (*Server, string) {
	t.Helper()
	server := start(t, config, serverArgs)
	awaitAuth(t, server.URL(), anonymous)
	return server, server.URL()
}

// awaitAuth waits for the server at uri to answer a connection with no credential the way its
// configuration says: to admit it when admit is set, as a server with a no_auth_user does, and
// otherwise to refuse it with its own authorization violation, the proof it enforces its nkey
// users. Either answer also proves it speaks the client protocol. A server can answer a dial
// before the NATS protocol handshake, and a dial then ends in EOF or a refusal that says nothing
// about the server's users, so that is retried through the shared wait. The opposite answer fails
// the test at once: a server that admits the connection enforces no users, and every refusal a
// test expects of it would pass for the wrong reason; one that refuses it has no no_auth_user.
func awaitAuth(t testing.TB, uri string, admit bool) {
	t.Helper()
	what := fmt.Sprintf("NATS at %s to refuse a client with no credential with its authorization violation", uri)
	if admit {
		what = fmt.Sprintf("NATS at %s to admit a client with no credential", uri)
	}
	testwait.Until(t, what, func() (error, bool) {
		conn, err := natsgo.Connect(uri, natsgo.NoReconnect())
		switch {
		case err == nil:
			conn.Close()
			if admit {
				return nil, true
			}
			t.Fatalf("NATS at %s admitted a client with no credential: it enforces no nkey users", uri)
		case errors.Is(err, natsgo.ErrAuthorization):
			if !admit {
				return err, true
			}
			t.Fatalf("NATS at %s refused a client with no credential: it has no no_auth_user", uri)
		}
		return err, false
	})
}

// ConnectSeed connects to uri as the nkey user whose seed is seed, or with no credential when seed
// is "", with no reconnect, then opts, and returns the connection or the server's answer: a test
// that expects a refusal reads it from the error. The dial's own budget is nats.go's default,
// which a caller may replace by passing natsgo.Timeout among opts.
func ConnectSeed(t testing.TB, uri, seed string, opts ...natsgo.Option) (*natsgo.Conn, error) {
	t.Helper()
	options := []natsgo.Option{natsgo.NoReconnect()}
	if seed != "" {
		user, err := nkeys.FromSeed([]byte(seed))
		if err != nil {
			t.Fatalf("read the seed: %v", err)
		}
		public, err := user.PublicKey()
		if err != nil {
			t.Fatalf("read the seed's public key: %v", err)
		}
		options = append(options, natsgo.Nkey(public, user.Sign))
	}
	return natsgo.Connect(uri, append(options, opts...)...)
}

// dialAttempt is one dial of Connect's wait, so the failure of a wait that ran out prints the
// server's answer to its last dial rather than a nil connection.
type dialAttempt struct {
	conn *natsgo.Conn
	err  error
}

func (a dialAttempt) String() string {
	if a.err == nil {
		return "connected"
	}
	return a.err.Error()
}

// Connect waits for a test NATS server to accept a real connection and JetStream to answer an
// operation before returning it. The relay accepts a connection before the server behind it has
// finished its protocol handshake, and NATS can complete that handshake while JetStream is still
// starting; a KV test needs both readiness boundaries. Each dial's own budget is nats.go's
// default, and a dial that fails is made again by the shared wait. Each account info is bounded
// by readWait and sent again by the shared wait when it runs out: only the leader of a cluster's
// meta group answers one, so a member asked while that group has no leader answers nothing
// (nats-server v2.15.1, jsAccountInfoRequest).
func Connect(t testing.TB, uri string) *natsgo.Conn {
	t.Helper()
	dial := testwait.Until(t, fmt.Sprintf("a connection to the NATS server at %s", uri), func() (dialAttempt, bool) {
		conn, err := natsgo.Connect(uri, natsgo.NoReconnect())
		return dialAttempt{conn: conn, err: err}, err == nil
	})
	conn := dial.conn
	js, err := conn.JetStream(natsgo.MaxWait(readWait))
	if err != nil {
		conn.Close()
		t.Fatalf("open JetStream on the connection to %s: %v", uri, err)
	}
	waitForJetStream(t, func() error {
		_, err := js.AccountInfo()
		return err
	})
	return conn
}

// RequestTimeout is the option every JetStream client a test makes takes
// (jetstream.New(conn, testnats.RequestTimeout(t))). nats.go bounds a request whose context has no
// deadline by its own DefaultTimeout, five seconds on the wall clock, and a test's request often
// has none: context.Background, t.Context, or a context of a clocktest.Fake, which reports no
// deadline. So a server that answers late on a loaded machine would fail the test with "context
// deadline exceeded". With this option such a request is bounded by what is left of the test
// binary's deadline less testwait's margin, and with no deadline (-timeout 0) by no bound a test
// can reach, so a late answer costs the test time and not its result. The daemon's testnats holds
// the same option.
func RequestTimeout(t testing.TB) jetstream.JetStreamOpt {
	if deadline, bounded := testwait.Context(t).Deadline(); bounded {
		return jetstream.WithDefaultTimeout(max(time.Until(deadline), time.Millisecond))
	}
	return jetstream.WithDefaultTimeout(unbounded)
}

// MaxWait is the option every legacy JetStream context a test makes takes
// (conn.JetStream(testnats.MaxWait(t))). nats.go's legacy context bounds each request made without
// a nats.Context option, a KeyValue operation's among them, by its own wait, five seconds on the
// wall clock unless natsgo.MaxWait sets another, and a test's requests rarely carry that option.
// So a server that answers late on a loaded machine would fail the test with "context deadline
// exceeded". With this option such a request is bounded by what is left of the test binary's
// deadline less testwait's margin, and with no deadline (-timeout 0) by no bound a test can reach,
// so a late answer costs the test time and not its result. The same wait is the default expiry of
// a pull subscription's Fetch and the time a KV watcher waits for its initial values, so neither
// ends early either. A wait that sends a request again when it draws no answer bounds each send by
// readWait instead (Connect, awaitInfo, awaitBucketOn): with this option a send the server never
// answers would hold that wait until the deadline.
func MaxWait(t testing.TB) natsgo.JSOpt {
	if deadline, bounded := testwait.Context(t).Deadline(); bounded {
		return natsgo.MaxWait(max(time.Until(deadline), time.Millisecond))
	}
	return natsgo.MaxWait(unbounded)
}

// waitForJetStream retries a real JetStream operation rather than trusting a successful core-NATS
// connect. Kept separate so its transient boundary has a direct unit test.
func waitForJetStream(t testing.TB, check func() error) {
	t.Helper()
	testwait.Until(t, "JetStream to answer an operation", func() (error, bool) {
		err := check()
		return err, err == nil
	})
}

// nats-server answers a stream create it could not make a file store for with streamStoreFailed,
// under the stream-create error code; RecreateKeyValue retries that answer.
const (
	streamStoreFailed     = "error creating store for stream"
	streamCreateErrorCode = natsgo.ErrorCode(10049)
)

// RecreateKeyValue creates the KV bucket config names in place of one the test has just deleted,
// and returns it.
//
// nats-server answers a stream delete before it is done with the account's directories: a
// goroutine of its own then removes the account's streams directory, which it can only do once
// that directory is empty, so only when the deleted stream was the account's last (stream.go,
// stop, v2.10 through v2.15). A create that arrives before that goroutine has run can lose the
// directory between making it and making its stream's own inside it: the server logs "could not
// create storage directory - mkdir .../streams/<stream>: no such file or directory" and answers
// the create "error creating store for stream". No API says when the goroutine has run, so the
// create is retried on exactly that answer; any other error fails the test at once.
func RecreateKeyValue(t testing.TB, js natsgo.JetStreamContext, config *natsgo.KeyValueConfig) natsgo.KeyValue {
	t.Helper()
	var kv natsgo.KeyValue
	err := retryStreamStoreFailure(t, func() error {
		var err error
		kv, err = js.CreateKeyValue(config)
		return err
	})
	if err != nil {
		t.Fatalf("recreate KV bucket %s: %v", config.Bucket, err)
	}
	return kv
}

// retryStreamStoreFailure runs create again while it fails with the server's stream store
// failure, waiting through the shared wait, and returns its first other answer.
func retryStreamStoreFailure(t testing.TB, create func() error) error {
	t.Helper()
	return testwait.Until(t, "a stream create the NATS server answers with other than its stream store failure", func() (error, bool) {
		err := create()
		var apiErr *natsgo.APIError
		storeFailed := errors.As(err, &apiErr) && apiErr.ErrorCode == streamCreateErrorCode && apiErr.Description == streamStoreFailed
		return err, !storeFailed
	})
}
