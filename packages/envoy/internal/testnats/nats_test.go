package testnats

//ci:builds packages/envoy ./... go list -f ... ./... in packages/envoy
//ci:reads packages/envoy/**/*.go !packages/envoy/infra/** !packages/envoy/testdata/** containerImporters and dockerDriverFiles walks packages/envoy but infra, node_modules and testdata, and reads every .go file it finds, test files included (dockerDriverFiles, called by TestNoPackageOutsideTheRigsImportsOrDrivesAContainerRuntime)

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/testwait"
)

func TestMain(m *testing.M) { os.Exit(Main(m.Run)) }

// This protects the rig's readiness boundary: a server can accept NATS's
// protocol handshake while JetStream is still starting, so a KV test that returns at the handshake
// races CreateKeyValue. The helper must retry the JetStream operation itself.
func TestWaitForJetStreamRetriesTheOperationUntilItIsReady(t *testing.T) {
	attempts := 0
	waitForJetStream(t, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("JetStream is starting")
		}
		return nil
	})
	if attempts != 3 {
		t.Fatalf("JetStream readiness attempts = %d, want 3", attempts)
	}
}

// noDeadline is t as testwait sees a test binary run under -timeout 0: with no deadline.
type noDeadline struct{ testing.TB }

func (noDeadline) Deadline() (time.Time, bool) { return time.Time{}, false }

// Under -timeout 0 the test binary has no deadline, so testwait.Context has none, and nats.go
// refuses a flush on a context with none. Context refuses nothing there: the flush on it is made.
func TestAFlushOnContextIsMadeWhenTheBinaryHasNoDeadline(t *testing.T) {
	conn := Connect(t, URL(t))
	defer conn.Close()
	timeless := noDeadline{t}
	if err := conn.FlushWithContext(testwait.Context(timeless)); !errors.Is(err, natsgo.ErrNoDeadlineContext) {
		t.Fatalf("a flush on testwait's context under no deadline: %v, want nats.go's refusal %v, which Context is there to spare", err, natsgo.ErrNoDeadlineContext)
	}
	if err := conn.FlushWithContext(Context(timeless)); err != nil {
		t.Fatalf("a flush on Context under no deadline: %v, want it made", err)
	}
}

// A create right after a stream delete can meet nats-server still removing the account's streams
// directory, and is answered with the store failure; RecreateKeyValue tries it again. Every other
// answer is the test's own failure and must not be retried into a slower one or a pass.
func TestRetryStreamStoreFailureRetriesOnlyTheStoreFailure(t *testing.T) {
	storeFailure := &natsgo.APIError{Code: 500, ErrorCode: streamCreateErrorCode, Description: streamStoreFailed}
	t.Run("store failure", func(t *testing.T) {
		attempts := 0
		err := retryStreamStoreFailure(t, func() error {
			attempts++
			if attempts < 3 {
				return storeFailure
			}
			return nil
		})
		if err != nil || attempts != 3 {
			t.Fatalf("err = %v after %d attempts, want nil after 3", err, attempts)
		}
	})
	t.Run("any other error", func(t *testing.T) {
		other := &natsgo.APIError{Code: 500, ErrorCode: streamCreateErrorCode, Description: "insufficient storage resources available"}
		attempts := 0
		err := retryStreamStoreFailure(t, func() error {
			attempts++
			return other
		})
		if err != other || attempts != 1 {
			t.Fatalf("err = %v after %d attempts, want %v after 1", err, attempts, other)
		}
	})
}

// The package's tests share one server, and each gets it as a fresh container was: a stream and its
// messages left by one test are not there when the next asks for the server, which hands it an
// account of its own.
func TestEachTestGetsTheSharedServerEmpty(t *testing.T) {
	t.Run("leaves a stream and a message", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream(MaxWait(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(&natsgo.StreamConfig{Name: "LEFT_BEHIND", Subjects: []string{"left.>"}}); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		if _, err := js.Publish("left.one", []byte("x")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	})
	t.Run("finds none of it", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream(MaxWait(t))
		if err != nil {
			t.Fatal(err)
		}
		info, err := js.AccountInfo()
		if err != nil {
			t.Fatalf("account info: %v", err)
		}
		if info.Streams != 0 {
			t.Errorf("the second subtest's account holds %d streams, want none of the first's", info.Streams)
		}
	})
}

// A test that publishes a burst without waiting for acks leaves messages the server routes after
// the test ends. They land in that test's own account, so the next test, creating a stream on the
// same subjects, finds none of them. The burst is a subtest, and this test is the next test: every
// test has an account of its own (URL), the subtest's apart from this one's, and running this test
// runs the burst first.
func TestABurstLeftInFlightDoesNotReachTheNextTest(t *testing.T) {
	const burst = 20000
	config := &natsgo.StreamConfig{Name: "BURST", Subjects: []string{"burst.>"}}
	var publisher uint64
	if !t.Run("publishes a burst and ends", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream(MaxWait(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(config); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		cid, err := conn.GetClientID()
		if err != nil {
			t.Fatalf("read the burst connection's client id: %v", err)
		}
		publisher = cid
		payload := []byte(strings.Repeat("x", 64))
		for i := range burst {
			if err := conn.Publish(fmt.Sprintf("burst.%d", i%16), payload); err != nil {
				t.Fatalf("publish %d: %v", i, err)
			}
		}
	}) {
		return
	}
	if publisher == 0 {
		t.Fatal("the burst's subtest did not run, so no burst was left for this test to find none of; a -run pattern that names a subtest must name that one")
	}
	conn := Connect(t, URL(t))
	defer conn.Close()
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatal(err)
	}
	// Every byte of the burst has been routed once the server has let its connection go: the
	// server routes what a client sends from that client's own read loop, and closes the
	// connection after the last of it.
	testwait.Eventually(t, fmt.Sprintf("the shared server to let the burst's connection (cid %d) go", publisher), func() bool {
		return openConnections(t, publisher) == 0
	})
	if _, err := js.AddStream(config); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	// A message of this test's own, published once the burst was routed, is stored after
	// anything the burst left for this stream: one stream appends what reaches it in order.
	ack, err := js.Publish("burst.0", []byte("marker"))
	if err != nil {
		t.Fatalf("publish this test's own message: %v", err)
	}
	if ack.Sequence != 1 {
		t.Errorf("this test's own message is at sequence %d, so %d of the previous test's %d messages reached the recreated stream before it",
			ack.Sequence, ack.Sequence-1, burst)
	}
	info, err := js.StreamInfo(config.Name)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Errorf("the recreated stream holds %d messages, want only this test's own: %d of the previous test's %d reached it",
			info.State.Msgs, info.State.Msgs-1, burst)
	}
}

// openConnections is how many connections the shared server still holds open under cid, read from
// the server object in this process.
func openConnections(t *testing.T, cid uint64) int {
	t.Helper()
	srv := shared.srv.natsServer()
	if srv == nil {
		t.Fatal("the package's shared NATS server is not running")
	}
	connections, err := srv.Connz(&natsserver.ConnzOptions{CID: cid, State: natsserver.ConnOpen})
	if err != nil {
		t.Fatalf("read the shared server's connections: %v", err)
	}
	return len(connections.Conns)
}

// fatalRecorder is t with Fatal recorded and the goroutine ended, as t.Fatal does, and with its
// test's deadline, which the shared wait reads.
type fatalRecorder struct {
	testing.TB
	message string
}

func (f *fatalRecorder) Fatal(args ...any) { f.message = fmt.Sprint(args...); runtime.Goexit() }
func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (f *fatalRecorder) Deadline() (time.Time, bool) {
	if timed, ok := f.TB.(interface{ Deadline() (time.Time, bool) }); ok {
		return timed.Deadline()
	}
	return time.Time{}, false
}

// A test gets one account however often it asks, so every connection it makes reaches the streams
// any other made; a subtest gets an account of its own, where none of its parent's streams is.
func TestATestGetsOneAccountAndEachSubtestItsOwn(t *testing.T) {
	uri := URL(t)
	if again := URL(t); again != uri {
		t.Fatalf("a second URL for the same test is %q, want its first, %q", again, uri)
	}
	conn := Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "PARENT", Subjects: []string{"parent.>"}}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	t.Run("subtest", func(t *testing.T) {
		subURI := URL(t)
		if subURI == uri {
			t.Fatalf("the subtest got its parent's account, %q", uri)
		}
		sub := Connect(t, subURI)
		defer sub.Close()
		subJS, err := sub.JetStream(MaxWait(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := subJS.StreamInfo("PARENT"); !errors.Is(err, natsgo.ErrStreamNotFound) {
			t.Fatalf("the subtest's account answers its parent's stream with %v, want not found", err)
		}
	})
}

// namedTB is a test under another name, so one test can ask a server for accounts as many tests.
type namedTB struct {
	testing.TB
	name string
}

func (n namedTB) Name() string { return n.name }

// ownAccountServer starts an account server of the test's own, declaring accounts accounts, which
// is shut down and removed when the test ends.
func ownAccountServer(t *testing.T, accounts int) *accountServer {
	t.Helper()
	return startAccountServer(t, accounts)
}

// A server whose accounts are all handed out declares twice as many by a reload and hands out the
// next, and an account a test is still using keeps its connection and its watcher across the
// reload.
func TestAServerOutOfAccountsDoublesThemWithoutDisturbingOneInUse(t *testing.T) {
	server := ownAccountServer(t, 2)
	live := Connect(t, server.url(namedTB{t, "live"}))
	defer live.Close()
	js, err := live.JetStream(MaxWait(t))
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "live"})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	watcher, err := kv.WatchAll()
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() { _ = watcher.Stop() }()
	if entry := testwait.Receive(t, "the empty bucket's scan to deliver its marker", watcher.Updates()); entry != nil {
		t.Fatalf("the empty bucket's scan delivered %v before its marker", entry)
	}

	server.url(namedTB{t, "second"})
	third := server.url(namedTB{t, "third"})
	if server.declared != 4 {
		t.Fatalf("the server declares %d accounts after handing out its third, want 4", server.declared)
	}
	if want := fmt.Sprintf("nats://t3:t3@%s", server.host); third != want {
		t.Fatalf("the third test's URL is %q, want %q", third, want)
	}
	if _, err := kv.Put("after-the-reload", []byte("1")); err != nil {
		t.Fatalf("put after the reload: %v", err)
	}
	if entry := testwait.Receive(t, "the watcher of an account in use to deliver the put made after the reload", watcher.Updates()); entry == nil || entry.Key() != "after-the-reload" {
		t.Fatalf("the watcher delivered %v after the reload, want the put", entry)
	}
	if status := live.Status(); status != natsgo.CONNECTED {
		t.Fatalf("the connection of an account in use is %v after the reload, want CONNECTED", status)
	}
}

// StartNkeyAuthorized's readiness wait refuses a server that admits a client with no credential:
// such a server enforces no nkey users, and every refusal a test expects of it would pass for the
// wrong reason. A server started with no configuration, which asks for no credential, stands in
// for one.
func TestTheNkeyReadinessWaitRefusesAServerThatAdmitsAnyone(t *testing.T) {
	_, uri := Start(t)
	wait := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		awaitAuth(wait, uri, false)
	}()
	testwait.Receive(t, "the readiness wait on a server that admits anyone to end", done)
	if want := "admitted a client with no credential"; !strings.Contains(wait.message, want) {
		t.Errorf("the readiness wait ended with %q, want it to say %q", wait.message, want)
	}
}

// A restart keeps its server's URL even when something else takes the URL's port while the server is
// stopped, as another test's server or a free-port pick can on a busy host.
func TestARestartKeepsItsURLWhenThePortIsContendedWhileStopped(t *testing.T) {
	server, uri := Start(t)
	server.Stop(t)
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %s: %v", uri, err)
	}
	if taken, err := net.Listen("tcp", u.Host); err == nil {
		t.Cleanup(func() { _ = taken.Close() })
	}
	server.Start(t)
	Connect(t, uri).Close()
}

// Server.Log is everything the server has said, which a test reads to see the server's own account
// of what it did: a server stopped and started again says it is ready twice, once for each server
// that ran behind the one URL.
func TestTheLogHoldsWhatTheServerSaidAcrossARestart(t *testing.T) {
	server, _ := Start(t)
	server.Stop(t)
	server.Start(t)
	if ready := strings.Count(server.Log(), "Server is ready"); ready != 2 {
		t.Errorf("the server said it was ready %d times across a stop and a start, want twice:\n%s", ready, server.Log())
	}
}

// Terminate ends the server for good and gives up the port its URL named, so nothing answers there
// any more; the cleanup that runs at the test's end finds the work done and does it no second time.
func TestTerminateEndsTheServerAndGivesUpItsPort(t *testing.T) {
	server, uri := Start(t)
	Connect(t, uri).Close()
	server.Terminate(t)
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %s: %v", uri, err)
	}
	if conn, err := net.Dial("tcp", u.Host); err == nil {
		conn.Close()
		t.Fatalf("%s still answers after the server was terminated", u.Host)
	}
}

// MonitorURL is where the server's /varz and /connz are, which a test reads to see what the server
// makes of its own state.
func TestTheMonitorURLAnswersVarz(t *testing.T) {
	server, _ := Start(t)
	response, err := http.Get(server.MonitorURL() + "/varz")
	if err != nil {
		t.Fatalf("GET %s/varz: %v", server.MonitorURL(), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("GET %s/varz: %s, want 200 OK", server.MonitorURL(), response.Status)
	}
}

// A second Grant on one server judges the outcome of its own reload, not the first Grant's: a
// second grant the server refuses fails the test with the server's reason for that grant, which
// comes back from that Grant's own reload.
func TestASecondGrantWaitsForItsOwnReload(t *testing.T) {
	_, public := User(t)
	grant := StartNkeyGranted(t, public, "first.>")
	grant.Grant(t, "second.>")

	refused := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		grant.Grant(refused, "not..a.subject")
	}()
	testwait.Receive(t, "the second Grant to return or refuse", done)
	if !strings.Contains(refused.message, "NATS refused the new grant") ||
		!strings.Contains(refused.message, `subject "not..a.subject" is not a valid subject`) {
		t.Fatalf("a second Grant the server refused ended with %q, want the server's refusal", refused.message)
	}
}

// Server.Reload answers with the server's own reason for a configuration the server will not take,
// and the server goes on serving the configuration it had: its user is still admitted and a client
// with no credential still refused.
func TestARefusedReloadAnswersWithTheServersReasonAndKeepsTheOldConfiguration(t *testing.T) {
	seed, public := User(t)
	server, uri := StartConfig(t, publishGrantConfig(public, []string{"first.>"}), []string{"-js"}, false)

	err := server.Reload(t, publishGrantConfig(public, []string{"not..a.subject"}))
	if err == nil {
		t.Fatal("the server took a configuration granting an invalid subject")
	}
	if want := `subject "not..a.subject" is not a valid subject`; !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal is %v, want it to say %s", err, want)
	}

	conn, err := AwaitConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("the user of the configuration the server kept was refused: %v", err)
	}
	defer conn.Close()
	if err := conn.Publish("first.one", nil); err != nil {
		t.Fatalf("publish to the subject the kept grant allows: %v", err)
	}
	if _, err := AwaitConnectSeed(t, uri, ""); !errors.Is(err, natsgo.ErrAuthorization) {
		t.Fatalf("a client with no credential after the refused reload: %v, want %v", err, natsgo.ErrAuthorization)
	}
}

// reloadRequesters is how many requesters TestAReloadUnderJetStreamRequestsAnswersEveryOne runs on
// each of reloadConnections connections, each keeping one JetStream request in flight, and
// reloadCount how many reloads it makes under them.
const (
	reloadConnections = 4
	reloadRequesters  = 8
	reloadCount       = 200
)

// A reload under clients' JetStream requests answers every request and keeps every connection, and
// under the race detector it is no data race. nats-server writes each service import's account on
// an authorization reload and reads it without the lock while it sets up the response to a request
// through that import (Server.Reload), so Reload holds the clients' messages until the server has
// processed the ones it has, and lets them through once it has reloaded. Before Reload held them,
// this test's reloads drew that race report in each of twenty runs under -race, and so did a Reload
// that held the clients without waiting for the server to finish what it had; each reload here
// changes the user's grant, so each one runs the server's authorization reload, and the server's
// log says so.
func TestAReloadUnderJetStreamRequestsAnswersEveryOne(t *testing.T) {
	seed, public := User(t)
	grant := func(n int) string {
		return publishGrantConfig(public, []string{"$JS.API.>", "_INBOX.>", fmt.Sprintf("reload%d.>", n)})
	}
	server, uri := StartConfig(t, grant(0), []string{"-js"}, false)
	stop := make(chan struct{})
	// A request held across a reload is answered once the reload lets its bytes through; it is
	// waited for as every wait of these tests is (testwait.Context), so a slow reload fails no
	// request.
	requestWait := testwait.Context(t)
	var answered atomic.Int64
	failures := make(chan error, reloadConnections*reloadRequesters)
	var requesters sync.WaitGroup
	for range reloadConnections {
		conn, err := AwaitConnectSeed(t, uri, seed)
		if err != nil {
			t.Fatalf("connect as the user: %v", err)
		}
		t.Cleanup(conn.Close)
		for range reloadRequesters {
			requesters.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := conn.RequestWithContext(requestWait, "$JS.API.INFO", nil); err != nil {
						failures <- err
						return
					}
					answered.Add(1)
				}
			})
		}
	}
	for n := 1; n <= reloadCount; n++ {
		if err := server.Reload(t, grant(n)); err != nil {
			t.Fatalf("reload %d: %v", n, err)
		}
	}
	close(stop)
	requesters.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("a request across the reloads failed: %v", err)
	}
	if answered.Load() == 0 {
		t.Fatal("no request was answered; this test says nothing about a reload under requests")
	}
	if reloads := strings.Count(server.Log(), "Reloaded: authorization nkey users"); reloads != reloadCount {
		t.Fatalf("the server reloaded its nkey users %d times, want %d: a reload that changed no grant tests nothing", reloads, reloadCount)
	}
	t.Logf("%d requests answered across %d reloads", answered.Load(), reloadCount)
}

// nats-server takes in a message whose Nats-Trace-Only header asks for a trace alone and whose
// Nats-Trace-Dest is not a subject, and drops it without processing or counting it (msgtrace.go
// 470-475 and parser.go 512-538 in 2.15.1: no in_msgs for it in /connz). A Reload after one still
// reloads: the hold waits for the server's answer to the relay's PING, which the server sends once
// it has read everything before it, and not for a count of messages the server never reaches.
func TestAReloadAfterATraceOnlyMessageTheServerDropsStillReloads(t *testing.T) {
	seed, public := User(t)
	grant := func(subjects ...string) string { return publishGrantConfig(public, subjects) }
	server, uri := StartConfig(t, grant("traced.>"), []string{"-js"}, false)
	conn, err := AwaitConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("connect as the user: %v", err)
	}
	t.Cleanup(conn.Close)
	message := natsgo.NewMsg("traced.one")
	message.Header.Set("Nats-Trace-Dest", "not..a.subject")
	message.Header.Set("Nats-Trace-Only", "true")
	if err := conn.PublishMsg(message); err != nil {
		t.Fatalf("publish the trace-only message: %v", err)
	}
	if err := conn.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("flush the trace-only message: %v", err)
	}
	if !strings.Contains(server.Log(), `Destination "not..a.subject" is not valid`) {
		t.Fatalf("the server did not drop the trace-only message, so this test says nothing about one:\n%s", server.Log())
	}
	if err := server.Reload(t, grant("traced.>", "more.>")); err != nil {
		t.Fatalf("reload after the trace-only message: %v", err)
	}
	if reloads := strings.Count(server.Log(), "Reloaded: authorization nkey users"); reloads != 1 {
		t.Fatalf("the server reloaded its nkey users %d times, want 1", reloads)
	}
	if _, err := conn.RequestWithContext(testwait.Context(t), "more.echo", nil); !errors.Is(err, natsgo.ErrNoResponders) {
		t.Fatalf("a request on the subject the reload granted: %v, want %v from the server that took the grant", err, natsgo.ErrNoResponders)
	}
}

// A Restart serves its new configuration under the same URL: the user the first configuration lets
// subscribe to a subject is refused it once the server restarts on one that does not, and has it
// again once the server restarts on the first.
func TestARestartServesItsNewConfiguration(t *testing.T) {
	seed, public := User(t)
	granted := SubscribeGrantConfig(public, "granted.>")
	server, uri := StartConfig(t, granted, []string{"-js"}, false)
	subscribe := func() error {
		t.Helper()
		conn, err := AwaitConnectSeed(t, uri, seed)
		if err != nil {
			t.Fatalf("connect as the user: %v", err)
		}
		defer conn.Close()
		if _, err := conn.SubscribeSync("granted.subject"); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if err := conn.FlushWithContext(Context(t)); err != nil {
			t.Fatalf("flush the subscription: %v", err)
		}
		return conn.LastError()
	}
	if err := subscribe(); err != nil {
		t.Fatalf("the first configuration refused the subscription it grants: %v", err)
	}

	server.Restart(t, SubscribeGrantConfig(public, "other.>"))
	if err := subscribe(); !errors.Is(err, natsgo.ErrPermissionViolation) {
		t.Fatalf("after a restart on a configuration that does not grant it, the subscription got %v, want the server's permissions violation", err)
	}

	server.Restart(t, granted)
	if err := subscribe(); err != nil {
		t.Fatalf("after a restart back on the configuration that grants it, the subscription got %v", err)
	}
}

// StartConfig gives the server the arguments it was given and no others, as nats-server's own
// command line does: -DV puts the protocol trace in its log, and without -js among them the server
// has no JetStream, whatever a server started by another helper has.
func TestStartConfigRunsTheArgumentsItWasGivenAndNoOther(t *testing.T) {
	seed, public := User(t)
	config := fmt.Sprintf("authorization {\n  users: [ { nkey: %q } ]\n}\n", public)
	server, uri := StartConfig(t, config, []string{"-DV"}, false)
	conn, err := AwaitConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("connect with the seed: %v", err)
	}
	defer conn.Close()
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	if _, err := js.AccountInfo(); !errors.Is(err, natsgo.ErrJetStreamNotEnabled) {
		t.Fatalf("JetStream account info on a server given no -js: %v, want %v", err, natsgo.ErrJetStreamNotEnabled)
	}
	if !strings.Contains(server.Log(), "[TRC]") {
		t.Errorf("the log of a server given -DV holds no protocol trace:\n%s", server.Log())
	}
}

// frozenRequest is how long a request to a frozen server waits before it counts as unanswered:
// nats.go's own request timer is the span that proves the freeze held the bytes, since an answer
// the server never sent leaves no state to read and nothing the test does later orders against
// it. A request the server answers waits on testwait.Context, so no bound of the test's own
// decides a slow round trip.
const frozenRequest = time.Second

// A frozen server stops the bytes in either direction without dropping anything: the request times
// out while its connection is still CONNECTED, as a server that has stopped answering leaves it,
// and the traffic resumes when the test thaws it.
func TestAFrozenServerTimesOutARequestWithoutDroppingTheConnection(t *testing.T) {
	server, uri := Start(t)
	conn := Connect(t, uri)
	defer conn.Close()
	sub, err := conn.Subscribe("echo", func(msg *natsgo.Msg) { _ = msg.Respond([]byte("answer")) })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if _, err := conn.RequestWithContext(testwait.Context(t), "echo", nil); err != nil {
		t.Fatalf("request before the freeze: %v", err)
	}

	thaw := server.Freeze(t)
	if _, err := conn.Request("echo", nil, frozenRequest); !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("a request to a frozen server: %v, want %v", err, natsgo.ErrTimeout)
	}
	if status := conn.Status(); status != natsgo.CONNECTED {
		t.Fatalf("the connection to a frozen server is %v, want CONNECTED", status)
	}

	thaw()
	answer, err := conn.RequestWithContext(testwait.Context(t), "echo", nil)
	if err != nil {
		t.Fatalf("request after the thaw: %v", err)
	}
	if string(answer.Data) != "answer" {
		t.Errorf("the answer after the thaw is %q, want %q", answer.Data, "answer")
	}
}

// natsDefaultWait is how long nats.go waits for the answer to a JetStream request a test's context
// makes without MaxWait or RequestTimeout: the legacy context's MaxWait default and the
// JetStream client's DefaultTimeout, five seconds each (nats.go v1.55.0).
const natsDefaultWait = 5 * time.Second

// A JetStream request of a context a test makes with MaxWait or RequestTimeout waits past
// nats.go's own five seconds for a server that answers late: each context's account info, sent to
// a frozen server, is answered once the server is thawed a second after nats.go's own wait would
// have ended it, and neither request fails. The second is counted from the moment the connection
// has published both requests (Stats().OutMsgs), and each request's wait began before its publish:
// nats.go makes both contexts' timeouts before it sends (nats.go v1.55.0 jsm.go getJSContextOpts,
// jetstream/jetstream.go wrapContextWithoutDeadline).
func TestAJetStreamRequestOfATestsContextOutwaitsNatsGosFiveSeconds(t *testing.T) {
	server, uri := Start(t)
	conn := Connect(t, uri)
	defer conn.Close()
	legacy, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("the legacy JetStream context: %v", err)
	}
	client, err := jetstream.New(conn, RequestTimeout(t))
	if err != nil {
		t.Fatalf("the JetStream client: %v", err)
	}

	sentBefore := conn.Stats().OutMsgs
	thaw := server.Freeze(t)
	defer thaw()
	type answer struct {
		context string
		err     error
	}
	answers := make(chan answer, 2)
	go func() {
		_, err := legacy.AccountInfo()
		answers <- answer{"the legacy context (MaxWait)", err}
	}()
	go func() {
		_, err := client.AccountInfo(context.Background())
		answers <- answer{"the JetStream client (RequestTimeout)", err}
	}()
	testwait.Until(t, "the connection to publish both requests", func() (uint64, bool) {
		sent := conn.Stats().OutMsgs - sentBefore
		return sent, sent >= 2
	})
	testwait.Span(t, natsDefaultWait+time.Second, "the row's subject is a request that outlives nats.go's own five-second wait, "+
		"and nothing the server or the client holds shows a timer that has not fired yet")
	thaw()
	for range 2 {
		got := testwait.Receive(t, "the answer to a request sent to the frozen server", answers)
		if got.err != nil {
			t.Errorf("%s: the account info sent to a server thawed %s later: %v, want its answer", got.context, natsDefaultWait+time.Second, got.err)
		}
	}
}

// Two freezes may overlap, and each thaw may be called more than once: neither ends the test
// binary (a second close of one gate panicked it once), a thaw left over from an earlier freeze
// does not end a later one, and the traffic resumes once every thaw has run.
func TestOverlappingFreezesThawInEitherOrderWithoutPanicking(t *testing.T) {
	for _, secondFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("second thawed first %v", secondFirst), func(t *testing.T) {
			server, uri := Start(t)
			conn := Connect(t, uri)
			defer conn.Close()
			sub, err := conn.Subscribe("echo", func(msg *natsgo.Msg) { _ = msg.Respond([]byte("answer")) })
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			defer func() { _ = sub.Unsubscribe() }()

			first, second := server.Freeze(t), server.Freeze(t)
			if secondFirst {
				first, second = second, first
			}
			first()
			second()
			first()
			second()
			if _, err := conn.RequestWithContext(testwait.Context(t), "echo", nil); err != nil {
				t.Fatalf("request after every thaw: %v", err)
			}

			later := server.Freeze(t)
			first()
			if _, err := conn.Request("echo", nil, frozenRequest); !errors.Is(err, natsgo.ErrTimeout) {
				t.Fatalf("a request after a stale thaw of an earlier freeze: %v, want %v", err, natsgo.ErrTimeout)
			}
			later()
			if _, err := conn.RequestWithContext(testwait.Context(t), "echo", nil); err != nil {
				t.Fatalf("request after the later thaw: %v", err)
			}
		})
	}
}

// A server whose replies the relay holds still takes what its clients send: a JetStream publish is
// stored, which StoredOn reads in the server itself, while its acknowledgement waits in the relay
// on a connection that stays CONNECTED, as an acknowledgement lost on its way back does, and
// reaches the client once the relay passes the server's replies again.
func TestFrozenRepliesKeepAPublishAndHoldItsAcknowledgement(t *testing.T) {
	server, uri := Start(t)
	conn := Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "held", Subjects: []string{"held.>"}}, natsgo.Context(testwait.Context(t))); err != nil {
		t.Fatalf("add the stream: %v", err)
	}

	thaw := server.FreezeReplies(t)
	t.Cleanup(thaw)
	ack, err := js.PublishAsync("held.kept", []byte("kept"))
	if err != nil {
		t.Fatalf("publish while the relay holds the server's replies: %v", err)
	}
	// The acknowledgement can reach the client only through the relay, which holds it, so each
	// read of the store, the one that finds the publish stored among them, finds it unanswered on a
	// connection that is still CONNECTED.
	ctx := Context(t)
	testwait.Until(t, "the server to store the publish whose acknowledgement the relay holds", func() (uint64, bool) {
		stored := server.StoredOn(t, ctx, "held", "held.kept")
		select {
		case <-ack.Ok():
			t.Fatal("the publish was acknowledged while the relay held the server's replies")
		case err := <-ack.Err():
			t.Fatalf("the publish failed while the relay held the server's replies: %v", err)
		default:
		}
		if status := conn.Status(); status != natsgo.CONNECTED {
			t.Fatalf("the connection whose replies the relay holds is %v, want CONNECTED", status)
		}
		return stored, stored == 1
	})

	thaw()
	testwait.Receive(t, "the publish's acknowledgement once the relay passes the server's replies again", ack.Ok())
}

// The anonymous wait ends at once, failing the test, on a server that refuses a client with no
// credential: that server is up and has no no_auth_user, so waiting out the deadline would only
// report the same refusal later.
func TestTheAnonymousWaitRefusesAServerThatRefusesAnyone(t *testing.T) {
	_, public := User(t)
	uri := StartNkeyAuthorized(t, public)
	wait := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		awaitAuth(wait, uri, true)
	}()
	testwait.Receive(t, "the anonymous wait on a server that refuses anyone to end", done)
	if want := "refused a client with no credential"; !strings.Contains(wait.message, want) {
		t.Errorf("the anonymous wait ended with %q, want it to say %q", wait.message, want)
	}
}

// envoyRoot is the module's directory, packages/envoy, from this package's own.
const envoyRoot = "../.."

// containerRigs are the packages of this module that may reach testcontainers in an untagged build:
// the two whose containers are not NATS and are not this issue's (internal/testpg runs pgvector,
// internal/tests3 SeaweedFS). This rig is not among them: its container half (image.go,
// callout_image.go) is built only under natslive, so an untagged go list reads no testcontainers
// import of it, and one there means that half lost its tag. Every other package is a finding,
// whichever testcontainers package it imports: modules/nats is the obvious way to start a NATS
// container, and testcontainers.Run(ctx, testnats.Image) is the one that needs no such import.
var containerRigs = []string{
	"github.com/sjawhar/envoy/internal/testpg",
	"github.com/sjawhar/envoy/internal/tests3",
}

// containerModule is the import prefix every testcontainers package shares.
const containerModule = "github.com/testcontainers/testcontainers-go"

// dockerDriver matches a file of this module driving a container runtime by hand: exec's run with
// docker as the command, however the call spells its context argument, and testcontainers' own
// Docker client. The context argument runs to the end of the line rather than to the first ')',
// since the module already passes context.Background() inline and a test's own is t.Context().
// It is written from pieces so that this file, which has to name them, is not itself a finding.
var dockerDriver = regexp.MustCompile(
	`exec\.Command(Context)?\([^\n]*` + `"docker"` + `|NewDockerClient` + `WithOpts`)

// No package of this module outside the rigs above imports a container runtime directly or drives
// docker by hand: Envoy's agents have no container runtime, and a test that needs one cannot run
// where they do. This pins those two ways to get one — a direct import of testcontainers, read
// from go list's Imports, TestImports and XTestImports, and a hand-driven docker — against the
// packages allowed to have them. It does not prove that no other package needs a runtime: a
// package that imports an allowlisted rig passes and still starts that rig's container, as
// cmd/listener's tests do through testpg.URL whenever DISPATCH_TEST_DATABASE_URL is unset. Direct
// imports are the right scope today because that is the one such case with no opt-out (tests3's
// one consumer, internal/dispatch/files, skips under DISPATCH_TEST_NO_CONTAINERS=1), and a
// separate issue removes Postgres through testpg; widening this guard to walk go list -deps is
// that issue's work, not this one's. internal/smoke's containers are behind the smoke build tag
// and the rig's own behind natslive, so an untagged go list never sees their imports. The walk
// does not go by tag: it reads every .go file, those included, and passes them because none
// drives docker by hand.
func TestNoPackageOutsideTheRigsImportsOrDrivesAContainerRuntime(t *testing.T) {
	t.Run("imports", func(t *testing.T) {
		for _, finding := range containerImporters(t) {
			t.Errorf("package %s imports %s, which belongs to a container runtime; a test that needs a NATS server starts it with testnats.Start, StartConfig or URL", finding.pkg, finding.imported)
		}
	})
	t.Run("docker", func(t *testing.T) {
		for _, file := range dockerDriverFiles(t) {
			t.Errorf("%s drives a container runtime itself; a test that has to stop a server's traffic uses testnats.Server.Freeze, and one that has to stop the server, Server.Stop", file)
		}
	})
}

// The file walk's matcher recognises a hand-driven docker in every way a test of this module spells
// the call, the context argument included: the module already passes context.Background() inline,
// and t.Context() is the test's own. Each line is assembled from pieces so this file, which the walk
// also reads, is not a finding itself.
func TestTheDockerDriverMatchCatchesEveryContextArgument(t *testing.T) {
	const run, docker = "exec." + "Command", `"dock` + `er"`
	for _, line := range []string{
		run + "(" + docker + `, "pause", id)`,
		run + "Context(ctx, " + docker + `, "pause", id)`,
		run + "Context(context.Background(), " + docker + `, "pause", id)`,
		run + "Context(t.Context(), " + docker + `, "unpause", id)`,
		"client.NewDocker" + "ClientWithOpts(client.FromEnv)",
	} {
		if !dockerDriver.MatchString(line) {
			t.Errorf("the docker-driver match misses %s", line)
		}
	}
	if line := run + `("go", "list", "./...")`; dockerDriver.MatchString(line) {
		t.Errorf("the docker-driver match flags %s, which runs no container", line)
	}
}

// containerImport is a package of this module importing a testcontainers package, and the import
// it was found by: a package reaching only testcontainers-go/wait has imported neither a runtime
// client nor modules/nats, and the finding has to say which it did import.
type containerImport struct{ pkg, imported string }

// containerImporters lists every package of this module, outside the rigs allowed one, that
// imports a testcontainers package, from its own sources or its tests', with the first such
// import of each.
func containerImporters(t *testing.T) []containerImport {
	t.Helper()
	const format = `{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`
	list := exec.Command("go", "list", "-f", format, "./...")
	list.Dir = envoyRoot
	out, err := list.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("go list ./...: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("go list ./...: %v", err)
	}
	var importers []containerImport
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || slices.Contains(containerRigs, fields[0]) {
			continue
		}
		for _, imported := range fields[1:] {
			if imported == containerModule || strings.HasPrefix(imported, containerModule+"/") {
				importers = append(importers, containerImport{pkg: fields[0], imported: imported})
				break
			}
		}
	}
	return importers
}

// dockerDriverFiles lists every Go file under packages/envoy that drives a container runtime
// itself, by its path from there. The nested infra module is left out: its pulumilive lane runs a
// real stack of containers on a host that has a runtime, which is the one place in this tree that
// legitimately drives Docker.
func dockerDriverFiles(t *testing.T) []string {
	t.Helper()
	var drivers []string
	err := filepath.WalkDir(envoyRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path == filepath.Join(envoyRoot, "infra") || entry.Name() == "node_modules" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if dockerDriver.Match(text) {
			relative, err := filepath.Rel(envoyRoot, path)
			if err != nil {
				return err
			}
			drivers = append(drivers, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", envoyRoot, err)
	}
	return drivers
}
