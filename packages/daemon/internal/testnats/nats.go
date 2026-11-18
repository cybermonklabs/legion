// Package testnats runs a NATS server with JetStream for a daemon test in the test process itself:
// one server for each test, started by the test's first URL or JetStream call and shut down when
// that test ends. A second call in the same test gets the same server, and a subtest, being a test
// of its own, gets its own. Every server here is the nats-server this binary links in
// (github.com/nats-io/nats-server/v2, the version the GCP hub runs), on loopback, at a port it
// picks, with its store under the test's own temporary directory. So a test needs no container
// runtime and no server named in its environment, and nothing of one test reaches another: no
// stream, consumer or message of an earlier test is on a later test's server, and no publish an
// earlier test left in flight lands there, because no server outlives the test that started it.
// Main fails the run when one does. R1 of "The design of this repository's tests" in
// docs/testing.md says why, and its "A Go test that opens a NATS client" says the rest.
//
// The end of the test's server is its leak check (leakcheck.go): a client still connected to the
// server when the test ends fails that test, by name and CID.
//
// The server URL starts admits only a client that carries the test's credential, a token the URL
// carries as its user, so no client of another process stays connected to it. Such a client can
// reach the server, because the kernel may give the server a port that another process still
// dials. Before the token, one did (an Envoy client's reconnect, dispatch://INFRA-3149), and the
// leak check waited on it until the test binary's deadline.
//
// StartNkeyAuthorized and StartNkeyAuthorizedUsers start in-process servers of their own that admit
// only nkey users. StartImage and StartOpenImage (image.go) are the two helpers that run a
// container, for the tests whose subject is a published NATS image itself.
//
// Envoy's tests have a rig of their own, packages/envoy/internal/testnats, which runs its servers
// in the test process too (its start, launch and serverOptions are what start here follows). The
// two are copies rather than one package because Go's internal rule keeps this module from
// importing Envoy's internal packages, so a change to how a server is started or configured
// belongs in both.
package testnats

import (
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

var (
	// mainRuns records that the test binary's TestMain went through Main, which fails a run that
	// leaves a server of this package running.
	mainRuns bool

	// serversMu guards the two records below, which parallel tests share.
	serversMu sync.Mutex
	// ofTest is the server URL started for each test still running, by the test's name.
	ofTest = map[string]*server{}
	// running is every server this process started and has not shut down, for Main.
	running = map[*server]struct{}{}
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

// Main runs the package's tests and returns the exit code, which is 1 when a server this package
// started is still running after the last test. Every server is shut down by a cleanup of the test
// that started it, so one still running is one whose test never ran that cleanup (a stand-in for t
// that drops its cleanups, or a call after the test ended); Main names each on stderr by its URL
// and the test that started it, then shuts it down. A package whose tests start a server calls it
// from TestMain: os.Exit(testnats.Main(m)).
func Main(m *testing.M) int {
	mainRuns = true
	code := m.Run()
	serversMu.Lock()
	left := slices.SortedFunc(maps.Keys(running), func(a, b *server) int { return strings.Compare(a.test, b.test) })
	serversMu.Unlock()
	for _, s := range left {
		fmt.Fprintf(os.Stderr, "testnats: the NATS server at %s that %s started is still running after the last test\n", s.url, s.test)
		s.stop()
	}
	if len(left) > 0 {
		return 1
	}
	return code
}

// URL returns the client URL of t's own NATS server, which has JetStream, starting it on t's first
// call; every later call in t returns the same server. The server holds nothing of another test's,
// so a test creates the streams it needs with CreateStream. When t ends, after every cleanup t
// registered after its first call, the server runs the leak check (leakcheck.go) and shuts down.
//
// The server admits only a client that carries a token of its own, which the URL carries as its
// user (nats://<token>@127.0.0.1:<port>). nats.go sends a URL's user as the token at CONNECT, so
// every client that dials the URL carries it: the fixture's, the daemon's, a child process's, and
// a raw nats.Connect. A client of another process carries none. The server refuses it at CONNECT
// (Authorization Violation) and closes it, and one that sends no CONNECT at all is closed when the
// server's auth timeout ends. So such a client is never one of the connections the leak check
// waits on.
func URL(t testing.TB) string {
	t.Helper()
	if s := serverOf(t); s != nil {
		return s.client
	}
	token := rand.Text()
	s := start(t, &natsserver.Options{Authorization: token}, true)
	s.client = "nats://" + token + "@" + strings.TrimPrefix(s.url, "nats://")
	serversMu.Lock()
	ofTest[t.Name()] = s
	serversMu.Unlock()
	return s.client
}

// serverOf is the server URL started for t, nil before t's first call.
func serverOf(t testing.TB) *server {
	serversMu.Lock()
	defer serversMu.Unlock()
	return ofTest[t.Name()]
}

// server is one nats-server running in this process, started for test.
type server struct {
	srv *natsserver.Server
	// url is the server's own address, with no credential.
	url string
	// client is the URL a test's clients dial: url with the token, for a server URL started.
	client string
	test   string
	log    *logCapture
}

// unbounded is the readiness wait under -timeout 0, where the test binary has no deadline.
const unbounded = time.Duration(math.MaxInt64)

// start runs a nats-server in this process on opts with JetStream, on loopback at a port the server
// picks, with no monitoring port, no signal handlers and its store in a directory of t's own, and
// returns it once it accepts connections. It is the one place a server of this package is made.
//
// It takes the store directory first and registers the server's stop next, before the server
// starts. Cleanups run the last registered first, so the stop runs after every cleanup t registers
// later (the close of a test's own clients, the stop of a daemon or a child process dialled to the
// server) and before the removal of the store under it. With check set, the stop runs the leak
// check first (leakcheck.go). The wait for connections is the one every wait of the daemon's tests
// has: what is left before the test binary's deadline less testwait's margin (testwait.Context),
// and no bound under -timeout 0.
func start(t testing.TB, opts *natsserver.Options, check bool) *server {
	t.Helper()
	if !mainRuns {
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m), which fails a run that leaves a NATS server of this package running")
	}
	opts.Host = "127.0.0.1"
	opts.Port = -1
	opts.HTTPPort = 0
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	opts.NoSigs = true
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("testnats: create the NATS server of %s: %v", t.Name(), err)
	}
	s := &server{srv: srv, test: t.Name(), log: &logCapture{}}
	t.Cleanup(func() {
		defer s.stop()
		if check {
			s.leakCheck(t)
		}
	})
	srv.SetLoggerV2(s.log, false, false, false)
	serversMu.Lock()
	running[s] = struct{}{}
	serversMu.Unlock()
	go srv.Start()
	within := unbounded
	if deadline, bounded := testwait.Context(t).Deadline(); bounded {
		within = time.Until(deadline)
	}
	if !srv.ReadyForConnections(within) {
		t.Fatalf("testnats: the NATS server of %s did not accept connections within %s, what was left before the test binary's deadline less testwait's margin; its log:\n%s",
			t.Name(), within, s.log)
	}
	s.url = srv.ClientURL()
	return s
}

// stop shuts the server down and returns once it has, so the store the test's temporary directory
// removes next is one nothing writes. Stopping a stopped server does nothing.
func (s *server) stop() {
	s.srv.Shutdown()
	s.srv.WaitForShutdown()
	serversMu.Lock()
	defer serversMu.Unlock()
	delete(running, s)
	if ofTest[s.test] == s {
		delete(ofTest, s.test)
	}
}

// Connections is what t's own server says its open client connections are, each by CID and name,
// read from the server object itself (Server.Connz); it fails t when t has no server of its own. A
// test of what a process leaves behind reads the server's own account rather than the connections
// it can see from inside its own process, since a connection the process has forgotten is exactly
// what such a test is looking for.
func Connections(t testing.TB) []Connection {
	t.Helper()
	s := serverOf(t)
	if s == nil {
		t.Fatalf("testnats: %s has no NATS server of its own to read: URL or JetStream starts one", t.Name())
	}
	return s.connections(t)
}

// Connection is one client connection the server holds: the id it gave it, and the name the
// client gave itself (nats.Name), empty for a client that gave none — a raw nats.Connect or a
// child process's. The CID is what a check compares, since the server never reuses one within its
// run, and the String is what a report prints.
type Connection struct {
	CID  uint64
	Name string
}

func (c Connection) String() string { return fmt.Sprintf("%d %q", c.CID, c.Name) }

// connections lists the server's open client connections, every one of them: Connz's own default
// is the first 1024.
func (s *server) connections(t testing.TB) []Connection {
	t.Helper()
	connz, err := s.srv.Connz(&natsserver.ConnzOptions{Limit: math.MaxInt})
	if err != nil {
		t.Fatalf("testnats: read the client connections of the NATS server of %s: %v", s.test, err)
	}
	open := make([]Connection, 0, len(connz.Conns))
	for _, c := range connz.Conns {
		open = append(open, Connection{CID: c.Cid, Name: c.Name})
	}
	return open
}

// JetStream returns a JetStream client of the server URL gives t, on a client Connect opens for t,
// its requests bounded as RequestTimeout bounds them.
func JetStream(t testing.TB) jetstream.JetStream {
	t.Helper()
	js, err := jetstream.New(Connect(t, URL(t)), RequestTimeout(t))
	if err != nil {
		t.Fatalf("testnats: open JetStream on the NATS server of %s: %v", t.Name(), err)
	}
	return js
}

// RequestTimeout is the option every JetStream client a test makes takes
// (jetstream.New(conn, testnats.RequestTimeout(t))). nats.go bounds a request whose context has no
// deadline by its own DefaultTimeout, five seconds on the wall clock, and a test's request often
// has none: context.Background, t.Context, or a context of a clocktest.Fake, which reports no
// deadline. So a server that answered late on a loaded machine failed the test with "context
// deadline exceeded" (TestABacklogOfOtherPullRequestsDrainsAndAppliesEveryLegionEvent, with the
// server's CONSUMER.INFO answer held 6 s). With this option such a request is bounded by what is
// left of the test binary's deadline less testwait's margin, and with no deadline (-timeout 0) by
// no bound a test can reach, so a late answer costs the test time and not its result.
func RequestTimeout(t testing.TB) jetstream.JetStreamOpt {
	if end, bounded := testwait.Context(t).Deadline(); bounded {
		return jetstream.WithDefaultTimeout(max(time.Until(end), time.Millisecond))
	}
	return jetstream.WithDefaultTimeout(time.Duration(math.MaxInt64))
}

// Connect is the client a test takes of a server: it dials url, named t.Name(), and closes it when
// t ends. A test takes its client from here rather than dialling itself because the name is what
// the leak check prints when a connection outlives the test that opened it, and because the
// fixture closes what it opened, so the test owes that close to nobody. The fixture gives the
// client its name and its close and leaves the dial's own budget to nats.go's default or to a
// caller that passes one: of the fifteen sites that moved to this helper ten dialled with
// nats.Timeout(time.Second) and five dialled with no timeout at all, which is nats.go's two-second
// default, so a second of the fixture's own would shorten those five. The caller's own options come
// after the fixture's, so a caller wanting another name or a timeout passes one and wins.
func Connect(t testing.TB, url string, options ...nats.Option) *nats.Conn {
	t.Helper()
	conn, err := nats.Connect(url, append([]nats.Option{nats.Name(t.Name())}, options...)...)
	if err != nil {
		t.Fatalf("connect to the NATS server at %s: %v", url, err)
	}
	t.Cleanup(conn.Close)
	fixtureOpened(conn)
	return conn
}

// nats-server answers a stream create it could not make a file store for with streamStoreFailed,
// under the stream-create error code; CreateStream retries that answer.
const (
	streamStoreFailed     = "error creating store for stream"
	streamCreateErrorCode = jetstream.ErrorCode(10049)
)

// CreateStream creates the stream config describes on js and returns it. js may be a client of any
// server: the server URL gives t, or one a test names itself, such as the server an end-to-end
// script runs (internal/daemon's live_proof_test.go).
//
// nats-server answers a stream delete before it is done with the account's directories: a
// goroutine of its own then removes the account's streams directory, which it can only do once
// that directory is empty, so only when the deleted stream was the account's last (stream.go,
// stop, v2.10 through v2.15). A create that arrives on that account before the goroutine has run
// can lose the directory between making it and making its stream's own inside it: the server logs
// "could not create storage directory - mkdir .../streams/<stream>: no such file or directory" and
// answers the create "error creating store for stream". A test that deletes its last stream and
// creates another, and every test on a server an end-to-end script shares among its drivers, can
// meet that answer. No API says when the goroutine has run, so the create is asked again on exactly
// that answer, through the shared wait; any other error fails the test at once.
//
// The stream the create answers is read once and must be empty. A stream handed to a test holding
// an earlier stream's messages makes that test count them as its own, so the failure names them
// here, instead of surfacing as a count the test reads later: in the failure this guard was
// written for, the row took its stream at consumer_lag_test.go:30 and read the count at :50. A
// create can come back holding an earlier stream's store when a stream of that name was deleted
// shortly before on the same server. nats-server does a stream delete's store work before it
// answers, and that work is what can leave the store behind (v2.10.22: jetstream_api.go:2915-2921
// answers only after mset.delete(), which calls store.Delete() inline and ignores its error,
// stream.go:5234-5237). fileStore.Delete closes the stream's message blocks without removing them
// and renames streams/<name> to streams/.<name> (filestore.go:7769-7773, 7793-7797); only the
// removal of that .<name> runs in a goroutine (:7800-7817). So when an earlier delete of the same
// name is still removing its .<name>, the next delete's rename fails onto that directory, the error
// is dropped, and streams/<name> stays where it is with its blocks, until the next create of that
// name loads it again, with its own Created new and the earlier messages at their stored times.
// Measured in an issue pod on 2026-11-10 (dispatch://INFRA-2583) against nats-server 2.10.22, under
// a load of four busy loops on two CPUs: 9 of 10,000 rounds of create, publish 12, delete, create
// again came back holding all 12 messages. On a server of the test's own only the test's own
// deletes can do that; on a server an end-to-end script runs, every driver of the script's run
// creates ENVOY_NOTIFICATIONS on the one server.
func CreateStream(t testing.TB, js jetstream.JetStream, config jetstream.StreamConfig) jetstream.Stream {
	t.Helper()
	var stream jetstream.Stream
	err := retryStreamStoreFailure(t, func() error {
		var err error
		stream, err = js.CreateStream(t.Context(), config)
		return err
	})
	if err != nil {
		t.Fatalf("create stream %s: %v", config.Name, err)
	}
	assertCreatedEmpty(t, stream, config.Name)
	return stream
}

// namedInFailure bounds the messages assertCreatedEmpty names one by one; it counts the rest.
const namedInFailure = 10

// assertCreatedEmpty fails the test when the stream just created holds a message or a consumer,
// naming each message by subject, stream sequence and stored time and each consumer by name. It
// costs one STREAM.INFO round trip on a clean create, and reads nothing else unless the stream is
// not empty. The create's own answer carries that state too, but this read is a moment later, so
// it also catches a message that reached the stream after the create answered, which is the same
// contamination arriving by the other door.
func assertCreatedEmpty(t testing.TB, stream jetstream.Stream, name string) {
	t.Helper()
	info, err := stream.Info(t.Context())
	if err != nil {
		t.Fatalf("read stream %s after creating it: %v", name, err)
	}
	if info.State.Msgs == 0 && info.State.Consumers == 0 {
		return
	}
	var held []string
	var named uint64
	for seq := info.State.FirstSeq; named < info.State.Msgs && named < namedInFailure; {
		// Step by the first stored message at or after seq, so a run of deleted sequences costs
		// one read rather than one read each, and none of the namedInFailure messages this names
		// is spent on one.
		message, err := stream.GetMsg(t.Context(), seq, jetstream.WithGetMsgSubject(">"))
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			break
		}
		if err != nil {
			held = append(held, fmt.Sprintf("read the first message at sequence %d or after it: %v", seq, err))
			break
		}
		held = append(held, fmt.Sprintf("message %d %s stored %s", message.Sequence, message.Subject, message.Time.Format(time.RFC3339Nano)))
		named++
		seq = message.Sequence + 1
	}
	if left := info.State.Msgs - named; left > 0 {
		held = append(held, fmt.Sprintf("and %d more", left))
	}
	consumers := stream.ConsumerNames(t.Context())
	for consumer := range consumers.Name() {
		held = append(held, "consumer "+consumer)
	}
	if err := consumers.Err(); err != nil {
		held = append(held, fmt.Sprintf("list its consumers: %v", err))
	}
	t.Fatalf("stream %s is not empty at its create, holding %s (most probably the store of a stream of this name that a delete left in place; CreateStream's comment says how that happens)",
		name, strings.Join(held, "; "))
}

// retryStreamStoreFailure runs create again while it fails with the server's stream store failure,
// waiting through the shared wait (testwait.Until), and returns its first other answer.
func retryStreamStoreFailure(t testing.TB, create func() error) error {
	t.Helper()
	return testwait.Until(t, "a stream create the NATS server answers with other than its stream store failure", func() (error, bool) {
		err := create()
		var apiErr *jetstream.APIError
		storeFailed := errors.As(err, &apiErr) && apiErr.ErrorCode == streamCreateErrorCode && apiErr.Description == streamStoreFailed
		return err, !storeFailed
	})
}

// StartNkeyAuthorized starts a NATS server with JetStream of t's own in this process that admits
// only the nkey user whose public key is user, shut down when t ends, and returns its client URL
// once the server refuses a client with no credential (answering).
func StartNkeyAuthorized(t testing.TB, user string) string {
	t.Helper()
	return StartNkeyAuthorizedUsers(t, NkeyUser{Public: user})
}

// NkeyUser is one nkey user a server admits: its public key, and the server's permissions block for
// it in the configuration's syntax (`{ subscribe: { deny: ["x.>"] } }`), "" granting everything.
type NkeyUser struct {
	Public      string
	Permissions string
}

// StartNkeyAuthorizedUsers is StartNkeyAuthorized for a server admitting users alone, each with its
// permissions. The configuration is the one StartImage gives a container, read as nats-server reads
// its configuration file.
func StartNkeyAuthorizedUsers(t testing.TB, users ...NkeyUser) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nats.conf")
	if err := os.WriteFile(path, []byte(nkeyConfig(users)), 0o600); err != nil {
		t.Fatalf("write the nkey-authorized NATS configuration: %v", err)
	}
	opts, err := natsserver.ProcessConfigFile(path)
	if err != nil {
		t.Fatalf("read the nkey-authorized NATS configuration: %v", err)
	}
	s := start(t, opts, false)
	answering(t, s.url)
	return s.url
}

// nkeyConfig is the whole configuration of a server with JetStream admitting users alone, each with
// its permissions.
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

// answering returns once the server at url refuses a connection with no credential with its own
// authorization violation: the proof it speaks the client protocol and enforces its nkey users. A
// container's port can answer before the server inside it serves a connection, and until then a
// dial ends in EOF or a refusal that says nothing about the server's users. A server that admits
// the connection enforces no users, and every refusal a test expects would pass for the wrong
// reason, so that fails the test.
func answering(t testing.TB, url string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the NATS server at %s to refuse a client with no credential with its authorization violation", url), func() (error, bool) {
		conn, err := nats.Connect(url, nats.NoReconnect())
		if err == nil {
			conn.Close()
			t.Fatalf("NATS at %s admitted a client with no credential: it enforces no nkey users", url)
		}
		return err, errors.Is(err, nats.ErrAuthorization)
	})
}

// logCapture is a server's logger: it keeps every line the server logs, for the failure of a
// server that never accepted connections.
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
