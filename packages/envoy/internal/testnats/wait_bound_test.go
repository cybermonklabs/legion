package testnats

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testport"
)

// shortTB is t with a deadline of its own, end, and Fatal recorded and its goroutine ended as
// fatalRecorder does: a wait given it ends halfway to end (testDeadline), as one of a test run with
// a short -timeout does.
type shortTB struct {
	*fatalRecorder
	end time.Time
}

func (s shortTB) Deadline() (time.Time, bool) { return s.end, true }

// heldWait runs wait on its own goroutine with a t whose deadline is twice window away, so the
// wait's own end is window away, and returns that t once wait has returned or failed.
func heldWait(t *testing.T, window time.Duration, wait func(testing.TB)) shortTB {
	tb, done := startHeldWait(t, window, wait)
	<-done
	return tb
}

// startHeldWait is heldWait's start: the t and a channel closed once wait has returned or failed.
func startHeldWait(t *testing.T, window time.Duration, wait func(testing.TB)) (shortTB, <-chan struct{}) {
	tb := shortTB{fatalRecorder: &fatalRecorder{TB: t}, end: time.Now().Add(2 * window)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait(tb)
	}()
	return tb, done
}

// probes is a server that never becomes ready, keeping each time it was asked to wait for.
type probes struct{ asked []time.Duration }

func (p *probes) ReadyForConnections(d time.Duration) bool {
	p.asked = append(p.asked, d)
	time.Sleep(d)
	return false
}

// A start's readiness wait asks no probe longer than what is left of it, so it fails by name before
// the test's deadline. With 40 ms left, a probe of the full retry interval, 100 ms, would run past
// the deadline, where go test's -timeout panics with every goroutine's stack instead.
func TestAReadinessWaitAsksNoProbeLongerThanWhatIsLeft(t *testing.T) {
	const window = 40 * time.Millisecond
	server := &probes{}
	var err error
	heldWait(t, window, func(tb testing.TB) { err = awaitReady(tb, server, &logCapture{}) })
	if err == nil || !strings.Contains(err.Error(), "short of the test's deadline") {
		t.Fatalf("a readiness wait on a server that is never ready answered %v, want it to name the test's deadline", err)
	}
	if len(server.asked) == 0 {
		t.Fatal("the readiness wait asked the server nothing")
	}
	for _, d := range server.asked {
		if d > window {
			t.Errorf("the readiness wait asked a probe of %s with at most %s left of it (all asked: %v)", d, window, server.asked)
		}
	}
}

// dials is a server whose every handshake never comes: each dial takes the whole timeout it is
// given and fails, as a dial to a listener that accepts and says nothing does. It keeps each
// timeout.
type dials struct{ timeouts []time.Duration }

func (d *dials) dial(_ string, options ...natsgo.Option) (*natsgo.Conn, error) {
	opts := natsgo.GetDefaultOptions()
	for _, option := range options {
		if err := option(&opts); err != nil {
			return nil, err
		}
	}
	d.timeouts = append(d.timeouts, opts.Timeout)
	time.Sleep(opts.Timeout)
	return nil, errors.New("planted: the handshake never comes")
}

// A wait that dials gives no dial longer than what is left of it, so it fails by name before the
// test's deadline. With 450 ms left, a dial of the full second would run past the deadline.
func TestADialWaitGivesNoDialLongerThanWhatIsLeft(t *testing.T) {
	const window = 450 * time.Millisecond
	for _, row := range []struct {
		name string
		wait func(testing.TB)
	}{
		{"awaitAuth", func(tb testing.TB) { awaitAuth(tb, "nats://127.0.0.1:1", false) }},
		{"Connect", func(tb testing.TB) { Connect(tb, "nats://127.0.0.1:1") }},
		{"awaitSeed", func(tb testing.TB) { awaitSeed(tb, "nats://127.0.0.1:1", "") }},
	} {
		t.Run(row.name, func(t *testing.T) {
			server := &dials{}
			dial := dialNATS
			dialNATS = server.dial
			t.Cleanup(func() { dialNATS = dial })
			tb := heldWait(t, window, row.wait)
			if !strings.Contains(tb.message, "short of the test's deadline") {
				t.Fatalf("%s against a server that never completes a handshake failed with %q, want it to name the test's deadline", row.name, tb.message)
			}
			if len(server.timeouts) == 0 {
				t.Fatalf("%s dialed nothing", row.name)
			}
			for _, d := range server.timeouts {
				if d > window {
					t.Errorf("%s gave a dial %s with at most %s left of the wait (all given: %v)", row.name, d, window, server.timeouts)
				}
			}
		})
	}
}

// silentServers is n listeners that accept every connection and never send it NATS's INFO, as
// servers whose handshakes never come, and their URLs as one comma-separated list.
func silentServers(t *testing.T, n int) string {
	t.Helper()
	var mu sync.Mutex
	var conns []net.Conn
	urls := make([]string, 0, n)
	for range n {
		listener, err := testport.ListenLoopback()
		if err != nil {
			t.Fatalf("listen for a silent server: %v", err)
		}
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				conns = append(conns, conn)
				mu.Unlock()
			}
		}()
		t.Cleanup(func() { listener.Close() })
		urls = append(urls, "nats://"+listener.Addr().String())
	}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})
	return strings.Join(urls, ",")
}

// A connect to a list of servers whose handshakes never come ends with the wait, not once per
// server: nats.go bounds each server's handshake by its Timeout alone and tries each server in
// turn, so three such servers with 600 ms left would take 1.8 s, past the test's deadline 1.2 s
// away. The row dials real listeners, through nats.go's own connect.
func TestAConnectToSilentServersEndsWithTheWait(t *testing.T) {
	const window = 600 * time.Millisecond
	url := silentServers(t, 3)
	begun := time.Now()
	tb := heldWait(t, window, func(tb testing.TB) { Connect(tb, url) })
	took := time.Since(begun)
	if !strings.Contains(tb.message, "short of the test's deadline") {
		t.Fatalf("a connect to three servers whose handshakes never come failed with %q, want it to name the test's deadline", tb.message)
	}
	if took >= 2*window {
		t.Errorf("the connect to three silent servers failed after %s, past the test's deadline %s after it began", took, 2*window)
	}
}

// A connect to a wss:// URL whose server accepts and sends nothing ends with the wait too: nats.go
// runs a wss:// URL's TLS handshake before it sets its handshake's deadline, so only the deadline
// the dial sets bounds it. The row gives the wait twice its window again before it calls the wait
// lost, so a regression fails the row by name rather than hanging it to the binary's timeout; the
// listener's cleanup then closes the held connection.
func TestAConnectToASilentWebSocketServerEndsWithTheWait(t *testing.T) {
	const window = 600 * time.Millisecond
	url := strings.Replace(silentServers(t, 1), "nats://", "wss://", 1)
	begun := time.Now()
	tb, done := startHeldWait(t, window, func(tb testing.TB) { Connect(tb, url) })
	select {
	case <-done:
	case <-time.After(4 * window):
		t.Fatalf("a connect to a silent wss:// server was still running %s after it began, past the test's deadline %s after it began", 4*window, 2*window)
	}
	took := time.Since(begun)
	if !strings.Contains(tb.message, "short of the test's deadline") {
		t.Fatalf("a connect to a silent wss:// server failed with %q, want it to name the test's deadline", tb.message)
	}
	if took >= 2*window {
		t.Errorf("the connect to a silent wss:// server failed after %s, past the test's deadline %s after it began", took, 2*window)
	}
}

// A connection a wait made keeps working once the wait's end has passed: the end bounds the
// connect, not the connection the test goes on to use. nats.go sets a write deadline at each write
// of the connection, and a cap at the wait's end still in force past it would make every write
// past the end time out. The row connects with 1 s left, then publishes and flushes 1.2 s after it
// began, past the wait's end and short of the test's deadline.
func TestAConnectionAWaitMadeKeepsWorkingPastTheWaitsEnd(t *testing.T) {
	const window = time.Second
	server := start(t, "", []string{"-js"})
	begun := time.Now()
	var conn *natsgo.Conn
	tb := heldWait(t, window, func(tb testing.TB) { conn = Connect(tb, server.URL()) })
	if tb.message != "" {
		t.Fatalf("the connect failed: %s", tb.message)
	}
	defer conn.Close()
	time.Sleep(time.Until(begun.Add(window + window/5)))
	if err := conn.Publish("past.the.end", []byte("x")); err != nil {
		t.Fatalf("publish past the wait's end: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush past the wait's end, %s after the connect began: %v", time.Since(begun).Round(time.Millisecond), err)
	}
}

// A JetStream wait gives each request a context that ends no later than the wait does, so a request
// the server never answers fails the wait by name before the test's deadline, where nats.go's own
// 5 s for a request would run past it.
func TestAJetStreamWaitEndsEveryRequestWithTheWait(t *testing.T) {
	const window = 400 * time.Millisecond
	var ends []time.Time
	tb := heldWait(t, window, func(tb testing.TB) {
		waitForJetStream(tb, func(ctx context.Context) error {
			end, ok := ctx.Deadline()
			if !ok {
				return errors.New("the request's context has no deadline")
			}
			ends = append(ends, end)
			<-ctx.Done()
			return ctx.Err()
		})
	})
	if !strings.Contains(tb.message, "short of the test's deadline") {
		t.Fatalf("a JetStream wait whose requests are never answered failed with %q, want it to name the test's deadline", tb.message)
	}
	if len(ends) == 0 {
		t.Fatal("the JetStream wait made no request")
	}
	// The wait ends window before the test's deadline, a little later by the time its own reading
	// of the clock took; window/2 before the deadline leaves room for that and no more.
	waitEnd := tb.end.Add(-window / 2)
	for _, end := range ends {
		if end.After(waitEnd) {
			t.Errorf("a request of the JetStream wait ends at %s, past the wait's own end, near %s (test deadline %s)", end.Format(time.RFC3339Nano), tb.end.Add(-window).Format(time.RFC3339Nano), tb.end.Format(time.RFC3339Nano))
		}
	}
}

// A server that answers JetStream's account info and holds every other request of its JetStream
// API unanswered, as JetStream does while its meta group is still forming: a client of the test
// takes the API's subjects in place of a JetStream the server does not run.
func heldJetStream(t *testing.T) *Server {
	t.Helper()
	server := start(t, "", nil)
	conn, err := natsgo.Connect(server.URL(), natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("connect the JetStream stand-in: %v", err)
	}
	t.Cleanup(conn.Close)
	if _, err := conn.Subscribe("$JS.API.>", func(m *natsgo.Msg) {
		if m.Subject == "$JS.API.INFO" {
			_ = m.Respond([]byte(`{"type":"io.nats.jetstream.api.v1.account_info_response"}`))
		}
	}); err != nil {
		t.Fatalf("subscribe the JetStream stand-in: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the JetStream stand-in: %v", err)
	}
	return server
}

// A bucket wait on a member whose stream lookups go unanswered fails by name before the test's
// deadline: each lookup's context ends with the wait, where the lookup's own 10 s would run past it.
func TestABucketWaitWhoseLookupsAreHeldFailsByNameInTime(t *testing.T) {
	const window = 2 * time.Second
	cluster := &Cluster{Servers: []*Server{heldJetStream(t)}}
	begun := time.Now()
	tb := heldWait(t, window, func(tb testing.TB) { cluster.AwaitBucket(tb, "held") })
	took := time.Since(begun)
	if !strings.Contains(tb.message, "short of the test's deadline") || !strings.Contains(tb.message, "KV_held") {
		t.Fatalf("a bucket wait whose lookups are never answered failed with %q, want it to name the stream and the test's deadline", tb.message)
	}
	if took >= 2*window {
		t.Errorf("the bucket wait failed after %s, past the test's deadline %s after it began", took, 2*window)
	}
}
