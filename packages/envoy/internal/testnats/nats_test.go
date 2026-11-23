package testnats

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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// This protects the rig's readiness boundary: a server can accept NATS's
// protocol handshake while JetStream is still starting, so a KV test that returns at the handshake
// races CreateKeyValue. The helper must retry the JetStream operation itself.
func TestWaitForJetStreamRetriesTheOperationUntilItIsReady(t *testing.T) {
	attempts := 0
	waitForJetStream(t, func(context.Context) error {
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

// A create right after a stream delete can meet nats-server still removing the account's streams
// directory, and is answered with the store failure; RecreateKeyValue tries it again. Every other
// answer is the test's own failure and must not be retried into a slower one or a pass.
func TestRetryStreamStoreFailureRetriesOnlyTheStoreFailure(t *testing.T) {
	storeFailure := &natsgo.APIError{Code: 500, ErrorCode: streamCreateErrorCode, Description: streamStoreFailed}
	t.Run("store failure", func(t *testing.T) {
		attempts := 0
		err := retryStreamStoreFailure(func() error {
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
		err := retryStreamStoreFailure(func() error {
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
		js, err := conn.JetStream()
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
		js, err := conn.JetStream()
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
// same subjects, finds none of them.
func TestABurstLeftInFlightDoesNotReachTheNextTest(t *testing.T) {
	const burst = 20000
	config := &natsgo.StreamConfig{Name: "BURST", Subjects: []string{"burst.>"}}
	t.Run("publishes a burst and ends", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(config); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		payload := []byte(strings.Repeat("x", 64))
		for i := range burst {
			if err := conn.Publish(fmt.Sprintf("burst.%d", i%16), payload); err != nil {
				t.Fatalf("publish %d: %v", i, err)
			}
		}
	})
	t.Run("recreates the stream and finds none of it", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(config); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		time.Sleep(time.Second)
		info, err := js.StreamInfo(config.Name)
		if err != nil {
			t.Fatalf("stream info: %v", err)
		}
		if info.State.Msgs != 0 {
			t.Errorf("the recreated stream holds %d of the previous test's %d messages", info.State.Msgs, burst)
		}
	})
}

// fatalRecorder is t with Fatal recorded and the goroutine ended, as t.Fatal does, and with its
// test's deadline, which a wait bounded by it reads (waitBound).
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
	js, err := conn.JetStream()
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
		subJS, err := sub.JetStream()
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

// Deadline is the deadline of the test namedTB renames, so a wait given namedTB (a start, Connect,
// deleteStreams) ends by the test's deadline as the test's own would.
func (n namedTB) Deadline() (time.Time, bool) {
	if timed, ok := n.TB.(interface{ Deadline() (time.Time, bool) }); ok {
		return timed.Deadline()
	}
	return time.Time{}, false
}

// ownAccountServer starts an account server of the test's own, declaring accounts accounts, which
// is shut down and removed when the test ends.
func ownAccountServer(t *testing.T, accounts int) *accountServer {
	t.Helper()
	return startAccountServer(t, accounts)
}

// A server whose accounts are all handed out is left to the tests that hold them: the next account
// is on a server started for it, the full server is never reloaded, and an account a test is still
// using on it keeps its connection and its watcher.
func TestAServerOutOfAccountsHandsTheNextOutFromAServerOfItsOwn(t *testing.T) {
	server := ownAccountServer(t, 2)
	full := server.srv
	live := Connect(t, server.url(namedTB{t, "live"}))
	defer live.Close()
	js, err := live.JetStream()
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
	if entry := <-watcher.Updates(); entry != nil {
		t.Fatalf("the empty bucket's scan delivered %v before its marker", entry)
	}

	server.url(namedTB{t, "second"})
	third := server.url(namedTB{t, "third"})
	if server.srv == full {
		t.Fatal("the third test's account is on the server whose two accounts were handed out")
	}
	if want := fmt.Sprintf("nats://t1:t1@%s", strings.TrimPrefix(server.srv.URL(), "nats://")); third != want {
		t.Fatalf("the third test's URL is %q, want %q, the first account of the next server", third, want)
	}
	if strings.Contains(full.Log(), "Reloaded server configuration") {
		t.Fatalf("the full server was reloaded:\n%s", full.Log())
	}
	if _, err := kv.Put("after-the-next-server", []byte("1")); err != nil {
		t.Fatalf("put after the next server started: %v", err)
	}
	entry := testwait.Receive(t, "the watcher of an account in use to deliver the put after the next server started", watcher.Updates())
	if entry == nil || entry.Key() != "after-the-next-server" {
		t.Fatalf("the watcher delivered %v after the next server started, want the put", entry)
	}
	if status := live.Status(); status != natsgo.CONNECTED {
		t.Fatalf("the connection of an account in use is %v after the next server started, want CONNECTED", status)
	}
}

// A test that holds an account and asks again is answered at once, and the end of its hold runs,
// while another test's first ask waits for the start of the account server's next server: only a
// first ask waits for a start (accountServer.url). The row holds that start until the holder has
// asked again and ended, so a holder that waited for it would wait to the test's deadline and fail
// naming what it waited for.
func TestAnAccountAskedAgainDoesNotWaitForAnotherTestsStart(t *testing.T) {
	server := ownAccountServer(t, 1)
	held := make(chan string, 1)
	end := make(chan struct{})
	ended := make(chan struct{})
	letGo := sync.OnceFunc(func() { close(end) })
	// The holder's subtest must return before this test does, whatever failed first.
	defer func() { <-ended }()
	defer letGo()
	var holder string
	go func() {
		defer close(ended)
		t.Run("holder", func(t *testing.T) {
			holder = t.Name()
			held <- server.url(t)
			<-end
		})
	}()
	uri := testwait.Receive(t, "the holder's account", held)

	startServer := startNext
	t.Cleanup(func() { startNext = startServer })
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseStart := sync.OnceFunc(func() { close(release) })
	defer releaseStart()
	startNext = func(t testing.TB, config string, args []string) *Server {
		close(entered)
		<-release
		return startServer(t, config, args)
	}
	next := make(chan string, 1)
	go func() { next <- server.url(namedTB{t, "next"}) }()
	testwait.Receive(t, "the next test's ask to reach the start of the account server's next server", entered)

	again := make(chan string, 1)
	go func() { again <- server.url(namedTB{t, holder}) }()
	if got := testwait.Receive(t, "the holder's second ask while another test's start is held", again); got != uri {
		t.Errorf("the holder's second ask answered %q, want its account %q", got, uri)
	}
	letGo()
	testwait.Receive(t, "the end of the holder's hold while another test's start is held", ended)
	releaseStart()
	if got := testwait.Receive(t, "the next test's account once its start is let go", next); got == uri {
		t.Errorf("the next test got the holder's account %q", uri)
	}
}

// A start of an account server's next server that fails fails the test that asked, with its own
// reason, and tears down at once what it set up: the server's registration, its relay's port and
// its directory are gone before the ask returns, not when the account server ends. The next ask
// starts a server afresh. The row plants the failure after a real start.
func TestAFailedStartOfAnAccountServersNextServerLeavesNothingBehind(t *testing.T) {
	server := ownAccountServer(t, 1)
	server.url(namedTB{t, "first"})
	startServer := startNext
	t.Cleanup(func() { startNext = startServer })
	var planted *Server
	startNext = func(t testing.TB, config string, args []string) *Server {
		planted = startServer(t, config, args)
		t.Fatalf("a planted failure after the start of the next server")
		return nil
	}
	failed := &fatalRecorder{TB: namedTB{t, "failed"}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.url(failed)
	}()
	<-done
	if !strings.Contains(failed.message, "planted failure") {
		t.Fatalf("the ask whose start failed ended with %q, want the planted failure", failed.message)
	}
	if planted == nil {
		t.Fatal("the planted start never ran")
	}
	address := strings.TrimPrefix(planted.URL(), "nats://")
	serversMu.Lock()
	_, registered := servers[address]
	serversMu.Unlock()
	if registered {
		t.Errorf("the failed start's server is still registered at %s", address)
	}
	if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		conn.Close()
		t.Errorf("the failed start's relay still accepts connections at %s", address)
	}
	if _, err := os.Stat(filepath.Dir(planted.store)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the failed start's directory %s is still there: %v", filepath.Dir(planted.store), err)
	}
	startNext = startServer
	Connect(t, server.url(namedTB{t, "after"})).Close()
}

// A wait given namedTB ends by the test's deadline and fails naming it: namedTB passes the
// deadline of the test it renames on, as fatalRecorder does. Without it, a wait given namedTB (a
// next server's start, Connect, deleteStreams) would run to go test's -timeout and panic instead.
// The row dials a server whose handshake never comes (dialNATS) through namedTB.
func TestAWaitGivenANamedTestEndsByItsDeadline(t *testing.T) {
	server := &dials{}
	dial := dialNATS
	dialNATS = server.dial
	t.Cleanup(func() { dialNATS = dial })
	tb := heldWait(t, 450*time.Millisecond, func(tb testing.TB) { Connect(namedTB{tb, "named"}, "nats://127.0.0.1:1") })
	if !strings.Contains(tb.message, "short of the test's deadline") {
		t.Fatalf("Connect through namedTB against a server that never completes a handshake failed with %q, want it to name the test's deadline", tb.message)
	}
}

// A server that hands accounts to many tests refuses a Reload: it fails the test that asked,
// naming the server, and goes on serving the configuration it had.
func TestAServerOtherTestsUseRefusesAReload(t *testing.T) {
	server := ownAccountServer(t, 1)
	uri := server.url(namedTB{t, "holder"})
	refused := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.srv.Reload(refused, accountsConfig(2))
	}()
	<-done
	if want := fmt.Sprintf("a reload of the NATS server at %s, which other tests use", server.srv.URL()); !strings.Contains(refused.message, want) {
		t.Fatalf("the reload ended with %q, want it to say %q", refused.message, want)
	}
	if strings.Contains(server.srv.Log(), "Reloaded server configuration") {
		t.Fatalf("the server reloaded:\n%s", server.srv.Log())
	}
	Connect(t, uri).Close()
}

// directGetReloads is how many reloads TestAReloadUnderDirectGetsAnswersEveryOne makes under its
// readers' direct gets, on reloadConnections connections with reloadRequesters readers each.
const directGetReloads = 25

// directGetForm is one way a client's direct get reaches a stream: config is the server's
// configuration, and getter opens the bucket "direct" on conn, creating it with key "k" when
// create is set, and returns one direct get of that key. Each request may wait until the test's
// deadline, its wait taken when it starts (reloadWait).
type directGetForm struct {
	name   string
	config string
	getter func(t testing.TB, conn *natsgo.Conn, create bool) (func() error, error)
}

// kvGetter is the getter of a KV get, through a JetStream context with opts: the direct get of
// the bucket's key that kv.Get sends for a bucket that allows direct gets, as every bucket nats.go
// creates does. Each get takes its wait when it starts: a JetStream context's MaxWait is a span
// fixed when the context is made, and nats.go times every request of the context from that span
// (jsm.go:1833-1838 in v1.55.0), so a get started late would wait past the test's deadline.
func kvGetter(opts ...natsgo.JSOpt) func(testing.TB, *natsgo.Conn, bool) (func() error, error) {
	return func(t testing.TB, conn *natsgo.Conn, create bool) (func() error, error) {
		js, err := conn.JetStream(append(opts, natsgo.MaxWait(reloadWait(t)))...)
		if err != nil {
			return nil, err
		}
		if create {
			var kv natsgo.KeyValue
			if kv, err = js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "direct"}); err == nil {
				_, err = kv.Put("k", []byte("v"))
			}
		} else {
			_, err = js.KeyValue("direct")
		}
		if err != nil {
			return nil, err
		}
		return func() error {
			msg, err := js.GetLastMsg("KV_direct", "$KV.direct.k", natsgo.DirectGet(), natsgo.MaxWait(reloadWait(t)))
			if err == nil && string(msg.Data) != "v" {
				err = fmt.Errorf("the get answered %q, want v", msg.Data)
			}
			return err
		}, nil
	}
}

// directGetForms are the three ways a direct get reaches a stream that the reload test runs: a KV
// get, a KV get through the prefix of the server's JetStream domain, and a request on a subject the
// global account's mapping turns into a direct get.
var directGetForms = []directGetForm{
	{name: "kv", config: hostOnlyConfig, getter: kvGetter()},
	{name: "domain", config: "jetstream { domain: hub }\n", getter: kvGetter(natsgo.Domain("hub"))},
	{name: "mapped", config: "mappings { \"dg.>\": \"$JS.API.DIRECT.GET.>\" }\n", getter: func(t testing.TB, conn *natsgo.Conn, create bool) (func() error, error) {
		if create {
			if _, err := kvGetter()(t, conn, true); err != nil {
				return nil, err
			}
		}
		return func() error {
			reply, err := conn.Request("dg.KV_direct.$KV.direct.k", nil, reloadWait(t))
			if err != nil {
				return err
			}
			if stream := reply.Header.Get("Nats-Stream"); stream != "KV_direct" || string(reply.Data) != "v" {
				return fmt.Errorf("the mapped request answered stream %q, status %q, data %q; want a direct get of KV_direct", stream, reply.Header.Get("Status"), reply.Data)
			}
			return nil
		}, nil
	}},
}

// A reload under clients' direct gets answers every get and keeps every connection, and under the
// race detector it is no data race, whichever way the gets reach the stream (directGetForms).
// nats-server reads its options without the lock as it answers a direct get, on the client's own
// goroutine (Server.Reload), and every reload sets them anew (setOpts) whatever it changes: with
// the hold and the barrier alone, without the lock order Reload takes (Server.ordered), -race
// reported that race in each form.
func TestAReloadUnderDirectGetsAnswersEveryOne(t *testing.T) {
	for _, form := range directGetForms {
		t.Run(form.name, func(t *testing.T) {
			server, uri := StartConfig(t, form.config, []string{"-js"}, true)
			first := Connect(t, uri)
			t.Cleanup(first.Close)
			if _, err := form.getter(t, first, true); err != nil {
				t.Fatalf("create the bucket: %v", err)
			}
			stop := make(chan struct{})
			var answered atomic.Int64
			failures := make(chan error, reloadConnections*reloadRequesters)
			var readers sync.WaitGroup
			halt := sync.OnceFunc(func() {
				close(stop)
				readers.Wait()
			})
			defer halt()
			for range reloadConnections {
				conn := Connect(t, uri)
				t.Cleanup(conn.Close)
				get, err := form.getter(t, conn, false)
				if err != nil {
					t.Fatalf("open the bucket: %v", err)
				}
				for range reloadRequesters {
					readers.Go(func() {
						for {
							select {
							case <-stop:
								return
							default:
							}
							if err := get(); err != nil {
								failures <- err
								return
							}
							answered.Add(1)
						}
					})
				}
			}
			for n := 1; n <= directGetReloads; n++ {
				if err := server.Reload(t, form.config); err != nil {
					t.Fatalf("reload %d: %v", n, err)
				}
			}
			halt()
			close(failures)
			for err := range failures {
				t.Errorf("a direct get across the reloads failed: %v", err)
			}
			if answered.Load() == 0 {
				t.Fatal("no direct get was answered; this test says nothing about a reload under direct gets")
			}
			if reloads := strings.Count(server.Log(), "Reloaded server configuration"); reloads != directGetReloads {
				t.Fatalf("the server reloaded %d times, want %d", reloads, directGetReloads)
			}
			t.Logf("%d direct gets answered across %d reloads", answered.Load(), directGetReloads)
		})
	}
}

// streamCreateReloads is how many reloads each form of TestAReloadUnderStreamCreatesAnswersEveryOne
// makes while reloadConnections clients create streams, and streamCreateBurst how many creates a
// client of the burst form sends before it reads their answers.
const (
	streamCreateReloads = 25
	streamCreateBurst   = 50
)

// A reload while clients create streams answers every create, and under the race detector it is
// no data race. nats-server answers a stream create on the JetStream API's own goroutines, where
// it reads the options' stream limits without the lock (stream.go:1013 in v2.15.1), and a create a
// client sent before the hold can still be served after the barrier; Reload waits until the API
// serves none (apiSettled). In the burst form each client sends creates in bursts before it reads
// their answers, so the API has a backlog when a reload holds the clients; in the fresh form each
// client connects afresh and sends one create as its first request, so a reload holds clients
// mid-handshake and finds clients the server has not yet dropped.
func TestAReloadUnderStreamCreatesAnswersEveryOne(t *testing.T) {
	for _, form := range []struct {
		name   string
		create func(t *testing.T, uri string, c int, stop <-chan struct{}, created *atomic.Int64) error
	}{
		{"burst", createInBursts},
		{"fresh", createOnFreshClients},
	} {
		t.Run(form.name, func(t *testing.T) {
			server, uri := Start(t)
			Connect(t, uri).Close()
			stop := make(chan struct{})
			var created atomic.Int64
			failures := make(chan error, reloadConnections)
			var creators sync.WaitGroup
			halt := sync.OnceFunc(func() {
				close(stop)
				creators.Wait()
			})
			defer halt()
			for c := range reloadConnections {
				creators.Go(func() {
					if err := form.create(t, uri, c, stop, &created); err != nil {
						failures <- err
					}
				})
			}
			for n := 1; n <= streamCreateReloads; n++ {
				if err := server.Reload(t, hostOnlyConfig); err != nil {
					t.Fatalf("reload %d: %v", n, err)
				}
			}
			halt()
			close(failures)
			for err := range failures {
				t.Errorf("a stream create across the reloads failed: %v", err)
			}
			if created.Load() == 0 {
				t.Fatal("no stream was created; this test says nothing about a reload under stream creates")
			}
			if reloads := strings.Count(server.Log(), "Reloaded server configuration"); reloads != streamCreateReloads {
				t.Fatalf("the server reloaded %d times, want %d", reloads, streamCreateReloads)
			}
			t.Logf("%d streams created across %d reloads", created.Load(), streamCreateReloads)
		})
	}
}

// createInBursts sends stream creates on one connection of client c in bursts of
// streamCreateBurst, each burst's answers read before the next, until stop. Its connect and each
// answer may wait until the test's deadline (reloadWait).
func createInBursts(t *testing.T, uri string, c int, stop <-chan struct{}, created *atomic.Int64) error {
	conn, err := natsgo.Connect(uri, natsgo.NoReconnect(), natsgo.Timeout(reloadWait(t)))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	inbox := natsgo.NewInbox()
	replies := make(chan *natsgo.Msg, streamCreateBurst)
	if _, err := conn.ChanSubscribe(inbox+".*", replies); err != nil {
		return fmt.Errorf("subscribe to the answers: %w", err)
	}
	for burst := 0; ; burst++ {
		select {
		case <-stop:
			return nil
		default:
		}
		for i := range streamCreateBurst {
			name := fmt.Sprintf("C%d_%d_%d", c, burst, i)
			config := fmt.Sprintf(`{"name":%q,"subjects":["created.%s"],"storage":"memory"}`, name, name)
			if err := conn.PublishRequest("$JS.API.STREAM.CREATE."+name, inbox+"."+name, []byte(config)); err != nil {
				return fmt.Errorf("send the create of %s: %w", name, err)
			}
		}
		for range streamCreateBurst {
			wait := reloadWait(t)
			select {
			case reply := <-replies:
				if strings.Contains(string(reply.Data), `"error"`) {
					return fmt.Errorf("a create was answered %s", reply.Data)
				}
				created.Add(1)
			case <-time.After(wait):
				return fmt.Errorf("a burst's create went unanswered for %s", wait)
			}
		}
	}
}

// createOnFreshClients connects client c afresh for each stream create, its first request, until
// stop. Each connect and create may wait until the test's deadline (reloadWait): a connect made
// while holds stand has its CONNECT and PING held, and its handshake waits them out.
func createOnFreshClients(t *testing.T, uri string, c int, stop <-chan struct{}, created *atomic.Int64) error {
	for n := 0; ; n++ {
		select {
		case <-stop:
			return nil
		default:
		}
		conn, err := natsgo.Connect(uri, natsgo.NoReconnect(), natsgo.Timeout(reloadWait(t)))
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		js, err := conn.JetStream(natsgo.MaxWait(reloadWait(t)))
		if err == nil {
			name := fmt.Sprintf("C%d_%d", c, n)
			_, err = js.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"created." + name}})
		}
		conn.Close()
		if err != nil {
			return fmt.Errorf("create a stream: %w", err)
		}
		created.Add(1)
	}
}

// reloadWait is how long one connect or request of a row that reloads under clients may wait: until
// the test's deadline (boundOf), or connectTimeout for a test with none. A reload's hold keeps a
// client's bytes back, and back-to-back reloads keep them back across several holds, since the next
// hold often starts before a held client's write gets through (a connect waited 6 to 9 s under
// holds of 3 s). The wait cannot outlast the row's reloads, which release every hold when they end.
// nats.go's own defaults, a 2 s connect timeout among them, are shorter than one hold a loaded
// machine makes.
func reloadWait(t testing.TB) time.Duration {
	bound := boundOf(t)
	if !bound.bounded {
		return connectTimeout
	}
	return bound.step(time.Until(bound.end))
}

// A Reload of a server that a client reached around the relay, at the server's own port, and sent
// a message fails the test naming that client, and the server does not reload: the hold cannot stop
// that client, so nothing orders its gets before the reload. Such a client that has sent nothing
// leaves the reload as it was.
func TestAReloadOfAServerAClientReachedAroundTheRelayFailsNamingIt(t *testing.T) {
	server, _ := Start(t)
	address, err := server.address()
	if err != nil {
		t.Fatal(err)
	}
	around, err := natsgo.Connect("nats://"+address, natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("connect at the server's own port: %v", err)
	}
	t.Cleanup(around.Close)
	if err := server.Reload(t, hostOnlyConfig); err != nil {
		t.Fatalf("a reload while that client has sent nothing: %v", err)
	}
	if err := around.Publish("around.relay", []byte("one")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := around.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	refused := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Reload(refused, hostOnlyConfig)
	}()
	<-done
	if want := "reached it around the relay and sent it 1 messages, which no hold reaches"; !strings.Contains(refused.message, want) {
		t.Fatalf("the reload ended with %q, want it to say %q", refused.message, want)
	}
	if reloads := strings.Count(server.Log(), "Reloaded server configuration"); reloads != 1 {
		t.Fatalf("the server reloaded %d times, want once, before the client sent a message", reloads)
	}
}

// A Reload of a server whose configuration names routes, leafnodes or a gateway fails the test
// naming them, whether or not such a link is up, and the server does not reload: it answers a
// direct get that comes over such a link on the stream's own goroutine, which no hold of its
// clients reaches. So does one whose new configuration names a link the running one does not.
func TestAReloadOfAServerLinkedToOthersFailsNamingTheLinks(t *testing.T) {
	for _, row := range []struct {
		name, running, given, links string
	}{
		{"routes", "cluster { name: \"one\", listen: \"127.0.0.1:-1\" }\n", hostOnlyConfig, "routes"},
		{"leafnodes", "leafnodes { listen: \"127.0.0.1:-1\" }\n", hostOnlyConfig, "leafnodes"},
		{"gateway", "gateway { name: \"one\", listen: \"127.0.0.1:-1\" }\n", hostOnlyConfig, "a gateway"},
		{"a link the new configuration adds", hostOnlyConfig, "leafnodes { listen: \"127.0.0.1:-1\" }\n", "leafnodes"},
	} {
		t.Run(row.name, func(t *testing.T) {
			server, uri := StartConfig(t, row.running, nil, true)
			refused := &fatalRecorder{TB: t}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = server.Reload(refused, row.given)
			}()
			<-done
			if want := fmt.Sprintf("a reload of the NATS server at %s, whose configuration names %s: it answers the direct gets that come over them", uri, row.links); !strings.Contains(refused.message, want) {
				t.Fatalf("the reload ended with %q, want it to say %q", refused.message, want)
			}
			if strings.Contains(server.Log(), "Reloaded server configuration") {
				t.Fatalf("the server reloaded:\n%s", server.Log())
			}
		})
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
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the readiness wait is still waiting on a server that admits anyone")
	}
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

// A gate's port must lie below the machine's ephemeral range, where no :0 bind can take it while
// the gate is closed. The range is fed in as text, each kernel's answer in turn, so the band is
// pinned whatever range the machine running this test has: a CI runner's default range would hide
// a band that lands inside the 16000-65535 a Legion worker pod (gVisor) has.
func TestTheGateBandLiesBelowTheEphemeralRangeItIsGiven(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		low   int
		first int
	}{
		{name: "gVisor's range in a Legion worker pod", text: "16000\t65535\n", low: 16000, first: 15000},
		{name: "Linux's default range", text: "32768\t60999\n", low: 32768, first: 31768},
		{name: "text that is no range", text: "not a range\n", low: 16000, first: 15000},
		{name: "empty text", text: "", low: 16000, first: 15000},
		{name: "the lowest range that leaves a band", text: "3048\t65535\n", low: 3048, first: 2048},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, count, err := gatePorts(tc.text)
			if err != nil {
				t.Fatalf("gatePorts(%q) = %v, want a band", tc.text, err)
			}
			if first != tc.first || count != 1000 {
				t.Errorf("gatePorts(%q) = %d ports from %d, want 1000 from %d", tc.text, count, first, tc.first)
			}
			if last := first + count - 1; last >= tc.low {
				t.Errorf("gatePorts(%q) = %d-%d, which reaches the range starting at %d", tc.text, first, last, tc.low)
			}
		})
	}
	t.Run("a range too low to leave a band", func(t *testing.T) {
		first, count, err := gatePorts("1024\t65535\n")
		if err == nil {
			t.Fatalf("gatePorts = %d ports from %d, want a refusal", count, first)
		}
		if !strings.Contains(err.Error(), "1024") {
			t.Errorf("refusal %v does not name the range's first port, 1024", err)
		}
	})
}

// StartGate takes its port from just below this machine's ephemeral range, which it reads from
// ephemeralRangePath. The test reads that file itself, as the rig does, and checks the ports of 20
// gates against it: a StartGate that stopped reading the file and took a band of its own fails
// here on every machine whose range does not start where that band ends.
func TestEveryGatePortLiesJustBelowThisMachinesEphemeralRange(t *testing.T) {
	low := fallbackEphemeralLow
	text, err := os.ReadFile(ephemeralRangePath)
	if fields := strings.Fields(string(text)); err == nil && len(fields) == 2 {
		first, firstErr := strconv.Atoi(fields[0])
		last, lastErr := strconv.Atoi(fields[1])
		if firstErr == nil && lastErr == nil && 0 < first && first <= last && last <= 65535 {
			low = first
		}
	}
	server, _ := Start(t)
	for range 20 {
		gate := StartGate(t, server)
		address, err := url.Parse(gate.URL())
		if err != nil {
			t.Fatalf("parse the gate's URL %s: %v", gate.URL(), err)
		}
		port, err := strconv.Atoi(address.Port())
		if err != nil {
			t.Fatalf("read the port of the gate's URL %s: %v", gate.URL(), err)
		}
		if port >= low || port < low-gatePortCount {
			t.Errorf("a gate took port %d, want one of the %d ports below %d, the first port of this machine's ephemeral range (%s holds %q)", port, gatePortCount, low, ephemeralRangePath, text)
		}
	}
}

// A closed gate keeps its port. While it is closed a dial of its address is refused, as a client's
// failed reconnect needs, a program that binds the port without SO_REUSEADDR is refused, and no
// other gate can take the port; Open then listens on the same address again and a client connects
// through it. Each row is the control of one hold: with the reservation gone the bind succeeds and
// Open fails, with the claim gone the second gate's hold succeeds, and with the reservation
// listening the dial succeeds.
func TestAClosedGateKeepsItsPort(t *testing.T) {
	server, _ := Start(t)
	gate := StartGate(t, server)
	address, err := net.ResolveTCPAddr("tcp", gate.address)
	if err != nil {
		t.Fatalf("resolve the gate's address %s: %v", gate.address, err)
	}
	gate.Close()

	conn, err := net.DialTimeout("tcp", gate.address, 5*time.Second)
	if err == nil {
		_ = conn.Close()
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("a dial of the closed gate at %s: %v, want connection refused", gate.address, err)
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatalf("open the other program's socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	err = syscall.Bind(fd, &syscall.SockaddrInet4{Port: address.Port, Addr: [4]byte(address.IP.To4())})
	if err == nil {
		// The port was free: hold it listening, as a program that took it would, so the Open below
		// meets it.
		_ = syscall.Listen(fd, 16)
		t.Errorf("another program bound the closed gate's port %d", address.Port)
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("another program's bind of the closed gate's port %d: %v, want address already in use", address.Port, err)
	}

	if claim, reservation, err := holdGatePort(address.IP, address.Port); err == nil {
		_ = syscall.Close(reservation)
		_ = claim.Close()
		t.Errorf("a second gate held the closed gate's port %d", address.Port)
	}

	gate.Open()
	nc, err := natsgo.Connect(gate.URL(), natsgo.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect through the reopened gate at %s: %v", gate.URL(), err)
	}
	nc.Close()
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
	if conn, err := net.DialTimeout("tcp", u.Host, dialTimeout); err == nil {
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
	<-done
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

	conn, err := ConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("the user of the configuration the server kept was refused: %v", err)
	}
	defer conn.Close()
	if err := conn.Publish("first.one", nil); err != nil {
		t.Fatalf("publish to the subject the kept grant allows: %v", err)
	}
	if _, err := ConnectSeed(t, uri, ""); !errors.Is(err, natsgo.ErrAuthorization) {
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
	var answered atomic.Int64
	failures := make(chan error, reloadConnections*reloadRequesters)
	var requesters sync.WaitGroup
	for range reloadConnections {
		conn, err := ConnectSeed(t, uri, seed)
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
					if _, err := conn.Request("$JS.API.INFO", nil, 5*time.Second); err != nil {
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
	conn, err := ConnectSeed(t, uri, seed)
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
	if err := conn.Flush(); err != nil {
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
	if _, err := conn.Request("more.echo", nil, 5*time.Second); !errors.Is(err, natsgo.ErrNoResponders) {
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
		conn, err := ConnectSeed(t, uri, seed)
		if err != nil {
			t.Fatalf("connect as the user: %v", err)
		}
		defer conn.Close()
		if _, err := conn.SubscribeSync("granted.subject"); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if err := conn.Flush(); err != nil {
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
	conn, err := ConnectSeed(t, uri, seed)
	if err != nil {
		t.Fatalf("connect with the seed: %v", err)
	}
	defer conn.Close()
	js, err := conn.JetStream()
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
	if _, err := conn.Request("echo", nil, connectTimeout); err != nil {
		t.Fatalf("request before the freeze: %v", err)
	}

	thaw := server.Freeze(t)
	if _, err := conn.Request("echo", nil, time.Second); !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("a request to a frozen server: %v, want %v", err, natsgo.ErrTimeout)
	}
	if status := conn.Status(); status != natsgo.CONNECTED {
		t.Fatalf("the connection to a frozen server is %v, want CONNECTED", status)
	}

	thaw()
	answer, err := conn.Request("echo", nil, connectTimeout)
	if err != nil {
		t.Fatalf("request after the thaw: %v", err)
	}
	if string(answer.Data) != "answer" {
		t.Errorf("the answer after the thaw is %q, want %q", answer.Data, "answer")
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
			if _, err := conn.Request("echo", nil, connectTimeout); err != nil {
				t.Fatalf("request after every thaw: %v", err)
			}

			later := server.Freeze(t)
			first()
			if _, err := conn.Request("echo", nil, time.Second); !errors.Is(err, natsgo.ErrTimeout) {
				t.Fatalf("a request after a stale thaw of an earlier freeze: %v, want %v", err, natsgo.ErrTimeout)
			}
			later()
			if _, err := conn.Request("echo", nil, connectTimeout); err != nil {
				t.Fatalf("request after the later thaw: %v", err)
			}
		})
	}
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
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the anonymous wait is still waiting on a server that refuses anyone")
	}
	if want := "refused a client with no credential"; !strings.Contains(wait.message, want) {
		t.Errorf("the anonymous wait ended with %q, want it to say %q", wait.message, want)
	}
}

// envoyRoot is the module's directory, packages/envoy, from this package's own.
const envoyRoot = "../.."

// thisPackage is the natslive half of this rig, in image.go and image_test.go, which an untagged
// build does not see at all.
const thisPackage = "github.com/sjawhar/envoy/internal/testnats"

// containerRigs are the packages of this module that may reach testcontainers: this rig, and the
// two whose containers are not NATS and are not this issue's (internal/testpg runs pgvector,
// internal/tests3 SeaweedFS). Every other package is a finding, whichever testcontainers package
// it imports: modules/nats is the obvious way to start a NATS container, and
// testcontainers.Run(ctx, testnats.Image) is the one that needs no such import.
var containerRigs = []string{
	thisPackage,
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
