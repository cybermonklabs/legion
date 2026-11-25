// Package testnats serves daemon tests a pool of JetStream servers per test binary, each started on
// first use and all removed by Main. A test takes one server from URL and holds it to itself until
// it ends; it gets the server empty, and a test that leaves a client connected to it is the test
// that fails, at its own cleanup (leakcheck.go). A test takes its own client from Connect, which
// names it after the test and closes it when the test ends.
//
// The pool holds as many servers as the binary runs tests at once (-test.parallel), each started
// only when every server already started is held: a binary whose tests run one at a time starts
// one and hands it from each test to the next, and one whose tests call t.Parallel starts one for
// each test that holds one at the same moment, rather than queueing them all on one server. A
// server freed by a test goes to the next test that asks, newest freed first, so a run one test at
// a time takes the same server throughout.
//
// A server per slot, not per test. The shape was chosen for testcontainers-go's Ryuk reaper: Ryuk
// stayed only while a container of the process was connected, and a reaper left idle past its
// timeout removed itself, so a later test's start, finding it removing, failed ("unexpected
// container status removing"). The package starts its containers through the docker CLI now
// (container.go), and a pooled server still lives until Main.
//
// A host with no Docker, such as a Sandbox pod, runs a nats-server of its own with JetStream and
// its monitoring endpoint, and names it in LEGION_TEST_NATS_URL (its client URL) and
// LEGION_TEST_NATS_MONITOR_URL (the monitoring endpoint's base URL). With both set, the pool is that
// one server, emptied for each test exactly as a container is, it starts no container, and Main
// removes nothing; one set without the other fails the test, since a half-named server is a
// mistake rather than a reason to fall back to Docker. The reset holds that server for one test of
// one test binary at a time, waits for every client connection to it to end, and deletes each
// stream's consumers and empties the stream before it deletes it, so the server is the tests' alone
// and the packages using it run one after another (go test -p 1). The leak check at the end of each
// test depends on that rule too: with one binary on a server and one test holding it, every
// connection it holds is this binary's, so one that appeared while a test held the server is that
// test's own.
package testnats

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Image is the NATS server image the daemon's NATS tests use: ghcr.io/cybermonklabs/legion/ci, the
// copy of nats:2.15.1-alpine (.github/ci-images.yaml), the server the hub runs. The daemon refuses
// a server that can skip a message on a consumer filtering on a wildcard and a literal at the same
// token (envoy's natsversion.Refusal, intake.OpenConsumers), which nats:2.10 is.
const Image = "ghcr.io/cybermonklabs/legion/ci@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6"

// SkippingImage is ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml),
// whose v2.10.29 is such a server: the one test that shows the daemon refusing it starts it.
const SkippingImage = "ghcr.io/cybermonklabs/legion/ci@sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f"

// readinessTimeout bounds the wait for the JetStream API once the container runs. The container
// start is not bounded by it: how long Docker takes to create a container is the daemon's load.
const readinessTimeout = 30 * time.Second

// externalURLEnv and externalMonitorEnv name a running NATS server for URL to serve in place of a
// container (the package comment): its client URL, and its monitoring endpoint's base URL, which
// the reset reads for the client connections it waits out.
const (
	externalURLEnv     = "LEGION_TEST_NATS_URL"
	externalMonitorEnv = "LEGION_TEST_NATS_MONITOR_URL"
)

// server is one server of the package's pool: its container (nil for the external one), its client
// URL and its monitoring endpoint's base URL, and the leak check's record of it (leakcheck.go),
// which leakMu guards.
type server struct {
	container *Container
	url       string
	monitor   string

	// fixtureConns are the connections the fixture opened for the test holding the server: its
	// reset client, and every client that test took from Connect. The leak check closes them, so
	// whatever is open after that is a connection the fixture does not hold.
	fixtureConns []*nats.Conn
	// leftOpen is what the last test's leak check on this server found still open once it had
	// closed the fixture's own clients, and leftOpenBy names that test, for the next test's drain.
	leftOpen   []Connection
	leftOpenBy string
}

var (
	// poolMu guards the pool: free holds the servers no test holds, newest freed last; started
	// every server started, which Main removes; starting how many starts are under way; and holders
	// the server each test holds, by the test's name, so that a second take by it or by one of its
	// subtests, which would wait on itself, fails instead. poolFreed wakes a take waiting for a
	// server once one is freed or a start ends.
	poolMu    sync.Mutex
	poolFreed = sync.NewCond(&poolMu)
	free      []*server
	started   []*server
	starting  int
	holders   = map[string]*server{}

	mainRuns bool
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

// Main runs the package's tests, then removes every NATS container a test started, ends the reaper
// (container.go), and returns the exit code; a run on an external server (external) started none
// and removes nothing. A package whose tests use URL or JetStream calls it from TestMain:
// os.Exit(testnats.Main(m)).
func Main(m *testing.M) int {
	mainRuns = true
	code := m.Run()
	for _, s := range started {
		if s.container == nil {
			continue
		}
		if err := s.container.remove(); err != nil {
			fmt.Fprintf(os.Stderr, "remove one of the package's NATS containers: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	if err := endReaper(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// URL returns a server of the package's pool (take) with no stream on it, so none of a previous
// test's streams, consumers or messages, and holds it for t until t ends. The pool is the external
// server LEGION_TEST_NATS_URL and LEGION_TEST_NATS_MONITOR_URL name when both are set (external),
// and otherwise the package's containers, each started on first use. The reset waits for the server
// to report no client connection first: a previous test that published without waiting for acks
// leaves messages the server still routes after the test ends, into a stream this test recreates.
// The reset then deletes each stream's consumers, purges it and deletes it, so a test creates its
// own there and finds it empty with CreateStream, and a durable it creates there starts where its
// own configuration says. A deleted stream's store can outlive the delete that answered; a purged
// one carries no message into the next test, though it does carry its sequence, and one whose
// consumers were deleted first carries no consumer's position. The purge and the consumer deletes,
// below, state the mechanism and name the source lines; the comments elsewhere in this package
// point to them rather than stating it again. Last, the reset arms the leak check (leakcheck.go),
// which fails t at its end for a client connection t left open on the server.
func URL(t testing.TB) string {
	t.Helper()
	return reset(t).url
}

// reset is URL's work, answering the server it emptied for t.
func reset(t testing.TB) *server {
	t.Helper()
	if !mainRuns {
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m), which removes the package's NATS containers")
	}
	s := take(t)
	drained(t, s)
	fixtureOpenedNone(s)
	conn, js := connect(t, s)
	defer conn.Close()
	names := js.StreamNames(t.Context())
	var streams []string
	for name := range names.Name() {
		streams = append(streams, name)
	}
	if err := names.Err(); err != nil {
		t.Fatalf("list the NATS server's streams: %v", err)
	}
	for _, name := range streams {
		// Purge before the delete, so the store this delete leaves behind holds no message.
		//
		// nats-server does a stream delete's store work before it answers, and the work itself is
		// what can leave the store behind (v2.10.22: jetstream_api.go:2915-2921 answers only after
		// mset.delete(), which calls store.Delete() inline and ignores its error,
		// stream.go:5234-5237). fileStore.Delete closes the stream's message blocks without
		// removing them and renames streams/<name> to streams/.<name>
		// (filestore.go:7769-7773, 7793-7797); only the removal of that .<name> runs in a
		// goroutine (:7800-7817). So when an earlier delete of the same name is still removing
		// its .<name>, this delete's rename fails onto that directory, the error is dropped, and
		// streams/<name> stays where it is with its blocks, until the next create of that name
		// loads it again. The test that create belongs to is then handed a stream whose own
		// Created is new, holding the previous test's messages at their own stored times, with
		// the account read below reporting no stream in between, and it counts them as its own.
		//
		// A purged store's blocks hold no message, so the store that comes back after a failed
		// rename carries none. It carries its sequence: a purge sets FirstSeq to LastSeq+1 and
		// writes that state (filestore.go:6918, 6980-6991), so the next test's first message need
		// not be at sequence 1. Nothing asserts a sequence, and nothing should: the rename fails
		// just as often after the purge, so a sequence check would bring the random failures back
		// by another door.
		//
		// Measured in an issue pod on 2026-11-10 (dispatch://INFRA-2583), against nats-server
		// 2.10.22, of the 2.10 line this package's image is (the image reports v2.10.29 now), one
		// round being create, publish 12, delete, create again, read. Under a load of four busy
		// loops on its two CPUs, 9 of 10,000 rounds of delete-then-create came back holding all 12
		// messages, and 0 of 10,000 rounds that purged first did. A run that also read the store
		// directory immediately before each delete found the rename collision itself: of 6,000
		// rounds, 28 met a .<name> the previous delete had not finished removing, and all 19 of the
		// rounds that recovered anything were among those 28. The purge is a lookup of the stream
		// and one purge request per stream, and the server answers the purge when the store is
		// empty.
		stream, err := js.Stream(t.Context(), name)
		if err != nil {
			t.Fatalf("take the NATS server's stream %s to empty it: %v", name, err)
		}
		// Delete the stream's consumers before the purge and the delete, so the store the delete
		// can leave behind (the purge, above, says how) holds no consumer's state either.
		//
		// A durable on this one server writes its position to a state file in the stream's
		// directory when it is created, unless it or its stream asks for memory storage (v2.15.1:
		// consumer.go:6481-6488, SetStarting, under selectStartingSeqNo, for a consumer on one
		// replica; filestore.go:13513-13521), and a consumer deleted with its stream leaves that
		// file for the stream's own removal (filestore.go:14607-14612). When the rename that
		// removal starts with fails, the file stays in streams/<name>/obs/<consumer>, and a
		// durable of the same name created on the next stream of that name finds it: a consumer
		// whose store has a state file restores it rather than selecting its start
		// (consumer.go:1350-1353, the else of :1399-1402; filestore.go:13819-13828). It starts
		// where the earlier test's durable stood, which for one created on a stream that never
		// held a message is sequence 1, whatever deliver policy the test gave it, and delivers
		// every message the test published before creating it: the "1 messages of history
		// pending, want 0" of intake's TestAFreshConsumerStartsAtTheNextMessage, run after a test
		// that made the same durables. nats-server 2.10.29 does the same (consumer.go:962-965,
		// 4945-4947, 5280-5281).
		//
		// A consumer delete removes the consumer's directory before it answers (jetstream_api.go:
		// 5835 answers after obs.delete(), which reaches consumer.go:6887's store.Delete(),
		// filestore.go:14582-14583, whose removal at :14607-14611 runs inline), so once these
		// deletes have answered, nothing of a consumer is left for the stream's delete to leave.
		// They come before the purge because a purge updates each consumer's position
		// (consumer.go:6691), which starts a write of its state file the delete would then race.
		// A consumer the server removed itself after the list, an ephemeral one past its
		// inactivity threshold, is gone or going already, and its generated name is not one a
		// later test's durable takes.
		var consumers []string
		listed := stream.ConsumerNames(t.Context())
		for consumer := range listed.Name() {
			consumers = append(consumers, consumer)
		}
		if err := listed.Err(); err != nil {
			t.Fatalf("list the consumers of the NATS server's stream %s: %v", name, err)
		}
		for _, consumer := range consumers {
			if err := stream.DeleteConsumer(t.Context(), consumer); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
				t.Fatalf("delete consumer %s of the NATS server's stream %s: %v", consumer, name, err)
			}
		}
		if err := stream.Purge(t.Context()); err != nil {
			t.Fatalf("purge the NATS server's stream %s: %v", name, err)
		}
		if err := js.DeleteStream(t.Context(), name); err != nil {
			t.Fatalf("empty the NATS server of stream %s: %v", name, err)
		}
	}
	info, err := js.AccountInfo(t.Context())
	if err != nil {
		t.Fatalf("read the NATS server's account after emptying it: %v", err)
	}
	if info.Streams != 0 {
		t.Fatalf("the NATS server still holds %d streams after emptying it", info.Streams)
	}
	armLeakCheck(t, s)
	return s
}

// external is the running server LEGION_TEST_NATS_URL and LEGION_TEST_NATS_MONITOR_URL name: its
// client URL and its monitoring endpoint's base URL, without a trailing slash, as the reset appends
// /connz to it. Neither set is no external server, both empty; one set without the other is an
// error naming both.
func external() (string, string, error) {
	server, monitoring := os.Getenv(externalURLEnv), os.Getenv(externalMonitorEnv)
	if (server == "") != (monitoring == "") {
		return "", "", fmt.Errorf("set both %s and %s to use a running NATS server, or neither to start a container (%s is %q, %s is %q)",
			externalURLEnv, externalMonitorEnv, externalURLEnv, server, externalMonitorEnv, monitoring)
	}
	return server, strings.TrimSuffix(monitoring, "/"), nil
}

// take holds a server of the pool for t until t ends: the one freed last, or, when every server
// started is held and fewer are started (or starting) than the binary runs tests at once
// (capacity), a new one (newServer); otherwise it waits for a test to free one. It refuses a take
// by a test already holding a server, or by one of its subtests, which would wait for itself for
// the whole test binary's timeout.
func take(t testing.TB) *server {
	t.Helper()
	poolMu.Lock()
	if holding, ok := holderOf(t.Name()); ok {
		poolMu.Unlock()
		t.Fatalf("testnats: %s already holds a NATS server of the package, so %s would wait for itself: take it once per test, through URL or JetStream", holding, t.Name())
	}
	var s *server
	for s == nil {
		if n := len(free); n > 0 {
			s, free = free[n-1], free[:n-1]
			break
		}
		if len(started)+starting < capacity() {
			starting++
			poolMu.Unlock()
			made, err := newServer()
			poolMu.Lock()
			starting--
			if err != nil {
				poolFreed.Broadcast()
				poolMu.Unlock()
				t.Fatalf("start NATS JetStream: %v", err)
			}
			started = append(started, made)
			s = made
			break
		}
		poolFreed.Wait()
	}
	holders[t.Name()] = s
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		delete(holders, t.Name())
		free = append(free, s)
		poolFreed.Signal()
		poolMu.Unlock()
	})
	return s
}

// holderOf names the test holding a server whose name is test's or one of its ancestors', under
// poolMu.
func holderOf(test string) (string, bool) {
	for holding := range holders {
		if test == holding || strings.HasPrefix(test, holding+"/") {
			return holding, true
		}
	}
	return "", false
}

// heldBy is the server t holds, or one of its ancestors holds, and nil when none does.
func heldBy(t testing.TB) *server {
	poolMu.Lock()
	defer poolMu.Unlock()
	if holding, ok := holderOf(t.Name()); ok {
		return holders[holding]
	}
	return nil
}

// capacity is how many servers the pool may hold: the external server alone when one is named,
// and otherwise as many as the test binary runs tests at once, its -test.parallel (GOMAXPROCS
// unless the run sets it).
func capacity() int {
	if url, _, _ := external(); url != "" {
		return 1
	}
	if parallel := flag.Lookup("test.parallel"); parallel != nil {
		if n, err := strconv.Atoi(parallel.Value.String()); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// newServer is a server for the pool: the external one when LEGION_TEST_NATS_URL and
// LEGION_TEST_NATS_MONITOR_URL name it, and otherwise a container of Image (start).
func newServer() (*server, error) {
	url, monitoring, err := external()
	if err != nil {
		return nil, err
	}
	if url != "" {
		return &server{url: url, monitor: monitoring}, nil
	}
	container, err := start(Image, startOptions{})
	if err != nil {
		return nil, err
	}
	return &server{container: container, url: container.url, monitor: container.monitorURL}, nil
}

// drained waits, within readinessTimeout, for s's monitoring endpoint to report no client
// connection, and fails naming the connections left when it does not. Connections the last test's
// own leak check on s reported and could not close are not waited out at all: nothing will close
// them, so this test is failed at once, naming the test that left them (leakcheck.go).
func drained(t testing.TB, s *server) {
	t.Helper()
	deadline := time.Now().Add(readinessTimeout)
	for {
		connections, err := readConnections(s)
		if err == nil && len(connections) == 0 {
			return
		}
		if err == nil {
			if leaker, all := leftByTheLastTest(s, connections); all {
				t.Fatalf("the NATS server holds %d client connections %v, every one of them left open by %s, which could not close them",
					len(connections), connections, leaker)
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("read the NATS server's connections before emptying it: %v", err)
			}
			if leaker, _ := leftByTheLastTest(s, connections); leaker != "" {
				t.Fatalf("the NATS server still has %d client connections %v from a previous test after %s, and %s left connections of its own it could not close",
					len(connections), connections, readinessTimeout, leaker)
			}
			t.Fatalf("the NATS server still has %d client connections %v from a previous test after %s", len(connections), connections, readinessTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Connections is what the server t holds says its open client connections are, each by id and
// name, as the drain reads them; it fails t when t holds no server of the package or the
// monitoring endpoint cannot be read. A test of what a process leaves behind on its server reads
// the server's own account rather than the connections it can see from inside its own process,
// since a connection the process has forgotten is exactly what such a test is looking for.
func Connections(t testing.TB) []Connection {
	t.Helper()
	s := heldBy(t)
	if s == nil {
		t.Fatalf("testnats: %s holds no NATS server of the package: take one through URL or JetStream first", t.Name())
	}
	connections, err := readConnections(s)
	if err != nil {
		t.Fatalf("read the NATS server's connections: %v", err)
	}
	return connections
}

// Connection is one client connection the server holds: the id it gave it, and the name the
// client gave itself (nats.Name), empty for a client that gave none — a raw nats.Connect or a
// child process's, which is the connection this package's leak check exists to catch. The CID is
// what a check compares, since the server never reuses one within its run, and the String is what
// a report prints.
type Connection struct {
	CID  uint64
	Name string
}

func (c Connection) String() string { return fmt.Sprintf("%d %q", c.CID, c.Name) }

// monitorClient bounds each monitoring request, so one the server never answers cannot outlast the
// drain's own bound.
var monitorClient = &http.Client{Timeout: 5 * time.Second}

// readConnections is the one question this fixture asks a server of its pool — which clients it
// holds — and every caller inside the package asks it here: the drain, Connections and the leak
// check's arm and settle. clientConnections, reading the monitoring endpoint's /connz, is its one
// implementation here. Two rows of this package's own tests replace it with a reader that fails,
// which is how a failing read is driven without waiting on anything:
// TestAFailedReadAfterTheCloseKeepsTheRecord, where the read fails only once the leak check has
// closed the fixture's clients, and TestACheckWhoseEveryReadFailsLeavesNoEarlierTestsName, where
// every read fails.
var readConnections = clientConnections

// clientConnections lists s's open client connections (connectionsAt its monitoring endpoint). It
// is readConnections' one implementation here, and the one function to re-point: a server running
// inside the test process answers the same question from the server object instead.
func clientConnections(s *server) ([]Connection, error) { return connectionsAt(s.monitor) }

// connectionsAt lists the open client connections of the server whose monitoring endpoint's base
// URL is monitor, each by id and name, by reading its /connz: a pooled server's
// (clientConnections) or a test's own (armServerLeakCheck).
func connectionsAt(monitor string) ([]Connection, error) {
	response, err := monitorClient.Get(monitor + "/connz")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/connz answered %s", monitor, response.Status)
	}
	var connz struct {
		Connections []struct {
			CID  uint64 `json:"cid"`
			Name string `json:"name"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&connz); err != nil {
		return nil, fmt.Errorf("decode %s/connz: %w", monitor, err)
	}
	connections := make([]Connection, 0, len(connz.Connections))
	for _, c := range connz.Connections {
		connections = append(connections, Connection{CID: c.CID, Name: c.Name})
	}
	return connections, nil
}

// JetStream returns a JetStream client of a server of the package's pool, as URL gives it to t.
func JetStream(t testing.TB) jetstream.JetStream {
	t.Helper()
	conn, js := connect(t, reset(t))
	t.Cleanup(conn.Close)
	return js
}

// Connect is the client a test of a pooled server takes: it dials url, the URL that server gave t,
// named t.Name(), and closes it when t ends. A test takes its client from here rather
// than dialling itself because the name is what the leak check prints when a connection outlives
// the test that opened it, and because the fixture closes what it opened, so the test owes that
// close to nobody. The fixture gives the client its name and its close and leaves the dial's own
// budget to nats.go's default or to a caller that passes one: of the fifteen sites that moved to
// this helper ten dialled with nats.Timeout(time.Second) and five dialled with no timeout at all,
// which is nats.go's two-second default, so a second of the fixture's own would shorten those
// five. The caller's own options come after the fixture's, so a caller wanting another name or a
// timeout passes one and wins.
func Connect(t testing.TB, url string, options ...nats.Option) *nats.Conn {
	t.Helper()
	conn, err := nats.Connect(url, append([]nats.Option{nats.Name(t.Name())}, options...)...)
	if err != nil {
		t.Fatalf("connect to the NATS server at %s: %v", url, err)
	}
	t.Cleanup(conn.Close)
	if s := heldBy(t); s != nil && s.url == url {
		fixtureOpened(s, conn)
	}
	return conn
}

// nats-server answers a stream create it could not make a file store for with streamStoreFailed,
// under the stream-create error code; CreateStream retries that answer for up to createTimeout.
const (
	streamStoreFailed     = "error creating store for stream"
	streamCreateErrorCode = jetstream.ErrorCode(10049)
	createTimeout         = 10 * time.Second
)

// CreateStream creates the stream config describes on js, a client of the server URL has emptied
// for t, and returns it.
//
// nats-server answers a stream delete before it is done with the account's directories: a
// goroutine of its own then removes the account's streams directory, which it can only do once
// that directory is empty, so only when the deleted stream was the account's last (stream.go,
// stop, v2.10 through v2.15). URL's reset deletes every stream, so its last delete always empties
// the account, and the test's first create can arrive before that goroutine has run. Such a create
// can lose the directory between making it and making its stream's own inside it: the server logs
// "could not create storage directory - mkdir .../streams/<stream>: no such file or directory" and
// answers the create "error creating store for stream". No API says when the goroutine has run, so
// the create is retried on exactly that answer, every 50 ms; any other error fails the test at
// once.
//
// The stream the create answers is read once and must be empty; the purge in URL says how a
// create can come back holding an earlier stream's store. A stream handed to a test holding an
// earlier test's messages makes that test count them as its own, so the failure names them here,
// instead of surfacing as a count the test reads later: in the failure this guard was written
// for, the row took its stream at consumer_lag_test.go:30 and read the count at :50.
func CreateStream(t testing.TB, js jetstream.JetStream, config jetstream.StreamConfig) jetstream.Stream {
	t.Helper()
	var stream jetstream.Stream
	err := retryStreamStoreFailure(func() error {
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
	t.Fatalf("stream %s is not empty at its create, holding %s (most probably the store of a stream of this name that a delete left in place; the purge in URL says how that happens)",
		name, strings.Join(held, "; "))
}

// retryStreamStoreFailure runs create again while it fails with the server's stream store
// failure, for up to createTimeout, and returns its last error.
func retryStreamStoreFailure(create func() error) error {
	deadline := time.Now().Add(createTimeout)
	for {
		err := create()
		var apiErr *jetstream.APIError
		storeFailed := errors.As(err, &apiErr) && apiErr.ErrorCode == streamCreateErrorCode && apiErr.Description == streamStoreFailed
		if !storeFailed || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// StartNkeyAuthorized runs a NATS container of its own, with JetStream, that accepts only clients
// authenticating as the nkey user whose public key is user, removed when t ends, and returns its
// client URL once the server answers there (answering).
func StartNkeyAuthorized(t testing.TB, user string) string {
	t.Helper()
	_, url := StartNkeyAuthorizedUsers(t, Image, NkeyUser{Public: user})
	return url
}

// NkeyUser is one nkey user a server admits: its public key, and the server's permissions block for
// it in the configuration's syntax (`{ subscribe: { deny: ["x.>"] } }`), "" granting everything.
type NkeyUser struct {
	Public      string
	Permissions string
}

// StartNkeyAuthorizedUsers is StartNkeyAuthorized for a server of image admitting users alone,
// each with its permissions. It returns the container, for a test that reads the server's log.
func StartNkeyAuthorizedUsers(t testing.TB, image string, users ...NkeyUser) (*Container, string) {
	t.Helper()
	entries := make([]string, 0, len(users))
	for _, user := range users {
		entry := fmt.Sprintf("{ nkey: %q", user.Public)
		if user.Permissions != "" {
			entry += ", permissions: " + user.Permissions
		}
		entries = append(entries, entry+" }")
	}
	config := fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ %s ]\n}\n", strings.Join(entries, ", "))
	container, err := start(image, startOptions{config: config})
	if err != nil {
		t.Fatalf("start nkey-authorized NATS: %v", err)
	}
	t.Cleanup(func() {
		if err := container.remove(); err != nil {
			t.Errorf("remove the nkey-authorized NATS container: %v", err)
		}
	})
	answering(t, container.url)
	armServerLeakCheck(t, container.monitorURL)
	return container, container.url
}

// StartImage runs a NATS container of image of its own, with JetStream and no users (start's
// command), removed when t ends, and returns its client URL once its JetStream API answers. It is
// for a test of the server itself, such as one the daemon refuses (SkippingImage); every other test
// takes a server of the package's pool through URL.
func StartImage(t testing.TB, image string) string {
	t.Helper()
	container, err := start(image, startOptions{})
	if err != nil {
		t.Fatalf("start NATS of %s: %v", image, err)
	}
	t.Cleanup(func() {
		if err := container.remove(); err != nil {
			t.Errorf("remove the NATS container of %s: %v", image, err)
		}
	})
	url := container.url
	ctx, cancel := context.WithTimeout(t.Context(), readinessTimeout)
	defer cancel()
	for {
		conn, _, err := ready(ctx, url, t.Name())
		if err == nil {
			conn.Close()
			return url
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for NATS at %s to answer within %s: %v", url, readinessTimeout, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// answering returns once the server at url refuses a connection with no credential with its own
// authorization violation: the proof it speaks the client protocol and enforces its nkey users. The
// log line the container's start waits for can come before a connection there is served, and until
// then a dial ends in EOF or a refusal that says nothing about the server's users. A server that
// admits the connection enforces no users, and every refusal a test expects would pass for the wrong
// reason, so that fails the test.
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
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// connect returns a connection to the server once its JetStream API answers. The connection
// carries t's name, as a test's own client does, and is the fixture's to close: the reset's
// client and a test's client are alike the fixture's (leakcheck.go).
func connect(t testing.TB, s *server) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), readinessTimeout)
	defer cancel()
	for {
		conn, js, err := ready(ctx, s.url, t.Name())
		if err == nil {
			fixtureOpened(s, conn)
			return conn, js
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for NATS JetStream readiness: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func ready(ctx context.Context, url, name string) (*nats.Conn, jetstream.JetStream, error) {
	conn, err := nats.Connect(url, nats.Name(name), nats.Timeout(time.Second))
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
