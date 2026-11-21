package testnats

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
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
	tb := shortTB{fatalRecorder: &fatalRecorder{TB: t}, end: time.Now().Add(2 * window)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait(tb)
	}()
	<-done
	return tb
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
