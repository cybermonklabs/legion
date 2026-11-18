package bus_test

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// stoppedNATS is a NATS server with JetStream that a test started and then stopped, and its URL:
// the server's relay keeps the URL's port and closes each connection it accepts until the test
// starts the server again (testnats.Server.Start).
func stoppedNATS(t *testing.T) (*testnats.Server, string) {
	t.Helper()
	srv, uri := testnats.Start(t)
	srv.Stop(t)
	return srv, uri
}

// An owning client made while its server is down is returned at once, not connected, and
// reconciles the stream on its first connection: before WithConnectInBackground the constructor
// failed, which ended envoy-dispatch's boot.
func TestAnOwningClientMadeWhileTheServerIsDownCreatesTheStreamOnItsFirstConnection(t *testing.T) {
	srv, uri := stoppedNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect owning the stream while the server is down: %v", err)
	}
	t.Cleanup(client.Close)
	if client.Connected() {
		t.Fatal("a client whose server is down reports itself connected")
	}

	srv.Start(t)
	testwait.Eventually(t, "the client's first connection", client.Connected)
	js, err := testnats.Connect(t, uri).JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	testwait.Eventually(t, "the stream the client owns", func() bool {
		_, err := js.StreamInfo(bus.Stream)
		return err == nil
	})
}

// HookedConnectionEvents counts no connection event before an owning client that connects in the
// background has connected, and at least one as soon as that client's first reconcile has
// succeeded. The count rises only after a run of the hooks returned (rewatch), and the reconcile
// is the first hook of such a client (startKeeperOnce). A test that waits for this count waits
// for this client's own work. The stream alone proves less: another writer can leave it holding
// what the test asks of it.
func TestHookedConnectionEventsCountsTheFirstReconcileOfABackgroundClient(t *testing.T) {
	srv, uri := stoppedNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect owning the stream while the server is down: %v", err)
	}
	t.Cleanup(client.Close)
	if client.Connected() {
		t.Fatal("a client whose server is down reports itself connected")
	}
	if events := client.HookedConnectionEvents(); events != 0 {
		t.Fatalf("HookedConnectionEvents() before the first connection = %d, want 0", events)
	}

	srv.Start(t)
	testwait.Eventually(t, "the client's hooks to cover a connection event", func() bool {
		return client.HookedConnectionEvents() >= 1
	})
	if _, err := client.JS().StreamInfo(bus.Stream); err != nil {
		t.Fatalf("read %s once the hooks had covered a connection event: %v", bus.Stream, err)
	}
}

// A JetStream and a core subscription made before the client's first connection deliver once it
// has connected. The JetStream bind cannot reach the server before then, so it fails and its
// registration stays for the first connection to bind; the core subscription is sent by nats.go
// as it connects, and SubscribeCore returns at once rather than waiting out the outage.
func TestSubscriptionsMadeBeforeTheFirstConnectionDeliverAfterIt(t *testing.T) {
	srv, uri := testnats.Start(t)
	owner, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	owner.Close()
	srv.Stop(t)

	client, err := bus.Connect([]string{uri}, bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect while the server is down: %v", err)
	}
	t.Cleanup(client.Close)

	var streamed, core atomic.Int32
	if _, err := subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		streamed.Add(1)
		_ = msg.Ack()
	}); err == nil {
		t.Fatal("a JetStream bind before the first connection reported success")
	}
	const lane = "notifications.role.background-core"
	subscribed := make(chan error, 1)
	go func() {
		_, err := client.SubscribeCore(lane, func(*natsgo.Msg) { core.Add(1) })
		subscribed <- err
	}()
	// SubscribeCore that waited for a connection would wait for ever: the server starts only once
	// it has returned.
	if err := testwait.Receive(t, "SubscribeCore to return while the server is down", subscribed); err != nil {
		t.Fatalf("a core subscription before the first connection: %v", err)
	}

	srv.Start(t)
	testwait.Eventually(t, "the client's first connection", client.Connected)
	publisher, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	publishOnce(t, publisher, "before-the-first-connection")
	testwait.Eventually(t, "the JetStream subscription's delivery", func() bool { return streamed.Load() > 0 })
	// Core NATS keeps nothing for a subscriber the server has not seen yet, and nats.go reports the
	// connection CONNECTED a moment before the server has read the subscriptions it re-sent, so the
	// lane is published to until the subscription has received one.
	testwait.Eventually(t, "the core subscription's delivery", func() bool {
		_ = publisher.Publish(contracts.Envelope{
			EventID: "evt-core", Source: "agent", SourceEventID: "src-core", Topic: lane,
			DedupeKey: "core-before-the-first-connection", IssuedAt: contracts.NowMillis(),
		})
		return core.Load() > 0
	})
}

// A publish before the client's first connection waits within its own deadline and then fails, as
// one does while the client reconnects; the same client publishes once its server answers. The
// deadline is on a clocktest.Fake, which the test moves to it once the publish has armed it, so the
// test sees the deadline pass without waiting out the five seconds a publish has.
func TestAPublishBeforeTheFirstConnectionFailsWithinItsOwnDeadline(t *testing.T) {
	srv, uri := stoppedNATS(t)
	acknowledgements := clocktest.New(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	client, err := bus.ConnectOwningStream([]string{uri},
		bus.WithConnectInBackground(),
		bus.WithPublishAcknowledgementClock(acknowledgements))
	if err != nil {
		t.Fatalf("connect while the server is down: %v", err)
	}
	t.Cleanup(client.Close)
	envelope := contracts.Envelope{
		EventID: "evt-before-connect", Source: "github", SourceEventID: "src-before-connect",
		Topic:     "notifications.github.acme.widgets.push.branch.main",
		DedupeKey: "publish-before-connect", IssuedAt: contracts.NowMillis(),
	}
	published := make(chan error, 1)
	go func() { published <- client.Publish(envelope) }()
	deadline := acknowledgements.AwaitArmed(t, "the acknowledgement deadline of the publish before the first connection", 1)[0]
	if len(published) != 0 {
		t.Fatalf("a publish before the first connection returned %v before the clock reached its deadline", <-published)
	}
	acknowledgements.Set(deadline)
	err = testwait.Receive(t, "the publish before the first connection to end at its deadline", published)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a publish before the first connection = %v, want its deadline passed", err)
	}

	srv.Start(t)
	testwait.Eventually(t, "the client's first connection", client.Connected)
	// The stream is reconciled by the first connection's hook, which may still be running.
	testwait.Eventually(t, "a publish after the first connection", func() bool {
		return client.Publish(envelope) == nil
	})
}

// Dial with WithConnectInBackground returns a connection that has not connected, and a core
// subscription made on it before it connects relays once it has: envoy-dispatch's agent
// conversation relay takes its viewers' subscriptions on it while the bus is down.
func TestDialInTheBackgroundRelaysACoreSubscriptionMadeBeforeItConnects(t *testing.T) {
	srv, uri := stoppedNATS(t)
	conn, err := bus.Dial("background-dial-test", []string{uri}, lookupOf(nil), bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("dial while the server is down: %v", err)
	}
	t.Cleanup(conn.Close)
	if conn.IsConnected() {
		t.Fatal("a connection whose server is down reports itself connected")
	}
	const subject = "agent.background-dial.frames"
	var relayed atomic.Int32
	if _, err := conn.Subscribe(subject, func(*natsgo.Msg) { relayed.Add(1) }); err != nil {
		t.Fatalf("subscribe before the connection connects: %v", err)
	}

	srv.Start(t)
	testwait.Eventually(t, "the connection's first connection", conn.IsConnected)
	publisher := testnats.Connect(t, uri)
	testwait.Eventually(t, "the relayed message", func() bool {
		_ = publisher.Publish(subject, []byte("frame"))
		_ = publisher.Flush()
		return relayed.Load() > 0
	})
}

// A configuration error still fails each constructor with WithConnectInBackground, before any
// dial: a seed that is not one, a server that is not this machine's, a SOCKS5 setting that is not
// an address. Only an unreachable or refusing server waits for the background. Every case names a
// server of the test's own first (an acceptRecorder), which nats.go dials first, so a constructor
// that dialled before it refused shows on the recorder.
func TestConfigurationErrorsStillFailTheConstructorInTheBackground(t *testing.T) {
	unsetNkeyEnvironment(t)
	recorder := startAcceptRecorder(t)
	local := []string{recorder.url()}
	cases := []struct {
		name        string
		urls        []string
		environment map[string]string
		want        string
	}{
		{"an unusable seed", local, map[string]string{"NATS_NKEY_SEED": "hunter2"}, "NATS_NKEY_SEED does not hold a valid nkey seed"},
		{"another machine's server", []string{recorder.url(), "nats://nats.example.test:4222"}, nil, bus.ErrRemoteNATS.Error()},
		{"a proxy that is not an address", local, map[string]string{bus.SOCKS5ProxyEnvVar: "127.0.0.1", bus.AllowRemoteEnvVar: "1"}, bus.SOCKS5ProxyEnvVar},
	}
	constructors := map[string]func(urls []string, environment func(string) (string, bool)) error{
		"Connect": func(urls []string, environment func(string) (string, bool)) error {
			client, err := bus.Connect(urls, bus.WithEnvironment(environment), bus.WithConnectInBackground())
			if err == nil {
				client.Close()
			}
			return err
		},
		"ConnectOwningStream": func(urls []string, environment func(string) (string, bool)) error {
			client, err := bus.ConnectOwningStream(urls, bus.WithEnvironment(environment), bus.WithConnectInBackground())
			if err == nil {
				client.Close()
			}
			return err
		},
		"Dial": func(urls []string, environment func(string) (string, bool)) error {
			conn, err := bus.Dial("background-config-test", urls, environment, bus.WithConnectInBackground())
			if err == nil {
				conn.Close()
			}
			return err
		},
	}
	for _, tc := range cases {
		for name, construct := range constructors {
			err := construct(tc.urls, lookupOf(tc.environment))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s with %s: %v, want a refusal naming %q", name, tc.name, err, tc.want)
			}
			recorder.requireNoDialBefore(t, name+" with "+tc.name)
		}
	}
}

// acceptRecorder listens on loopback and records the remote address of each connection it
// accepts, closing each at once, as a port with no NATS behind it looks to a dial.
type acceptRecorder struct {
	listener net.Listener
	mu       sync.Mutex
	accepted []string
}

func startAcceptRecorder(t *testing.T) *acceptRecorder {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for dials: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	r := &acceptRecorder{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			r.mu.Lock()
			r.accepted = append(r.accepted, conn.RemoteAddr().String())
			r.mu.Unlock()
			_ = conn.Close()
		}
	}()
	return r
}

func (r *acceptRecorder) url() string { return "nats://" + r.listener.Addr().String() }

// requireNoDialBefore fails t when anything dialled the recorder since the last call, before what
// returned. It dials the recorder itself and waits for that marker's accept: the listener hands
// out the connections dialled before the marker ahead of it, so a marker accepted alone shows that
// no dial came first.
func (r *acceptRecorder) requireNoDialBefore(t *testing.T, what string) {
	t.Helper()
	marker, err := net.Dial("tcp", r.listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the recorder after %s: %v", what, err)
	}
	defer marker.Close()
	at := marker.LocalAddr().String()
	accepted := testwait.Until(t, "the recorder to accept the marker dialled after "+what, func() ([]string, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return slices.Clone(r.accepted), slices.Contains(r.accepted, at)
	})
	if len(accepted) != 1 {
		t.Fatalf("%s dialled before it refused: the recorder accepted %v, the marker being %s", what, accepted, at)
	}
	r.mu.Lock()
	r.accepted = nil
	r.mu.Unlock()
}

// A server that refuses the client's credential does not fail the constructor with
// WithConnectInBackground: the client keeps trying, never gives the connection up as nats.go
// otherwise does on a second refusal, and logs the refusal at ERROR once a minute, not once an
// attempt. nats.go reports no refusal before a connection has first connected, so without the
// client's own report the process would sit refused with nothing in its log.
func TestARefusedCredentialIsLoggedOnceAMinuteAndRetried(t *testing.T) {
	unsetNkeyEnvironment(t)
	_, allowed := testnats.User(t)
	refused, _ := testnats.User(t)
	uri := testnats.StartNkeyAuthorized(t, allowed)
	logs := captureBusLogs(t)
	const refusal = "envoy nats authorization refused"
	clock := newReconnectErrClock()
	name := "refusal-test-" + t.Name()
	client, err := bus.Connect([]string{uri},
		bus.WithName(name),
		bus.WithEnvironment(lookupOf(map[string]string{"NATS_NKEY_SEED": refused})),
		bus.WithReconnectErrClock(clock.Now),
		bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect with a refused credential: %v", err)
	}
	t.Cleanup(client.Close)

	testwait.Eventually(t, "the refusal's line", func() bool { return logs.count(name, refusal) > 0 })
	if line := logs.lineAt("ERROR", refusal); line == "" || !strings.Contains(strings.ToLower(line), "authorization violation") {
		t.Fatalf("the refusal is not an ERROR naming the server's answer:\n%s", logs.String())
	}
	// nats.go retries about once a second and the client reads each refusal off the connection,
	// judging it on the throttle, which reads the clock once for each: three more judged, with the
	// clock standing still, and still one line.
	clock.awaitJudged(t, "three more refusals", clock.reads.Load(), 3)
	if got := logs.count(name, refusal); got != 1 {
		t.Fatalf("%d refusal lines within a minute, want 1:\n%s", got, logs.String())
	}
	if client.Conn.IsClosed() {
		t.Fatal("the client gave its connection up over a refused credential")
	}
	clock.Advance(time.Minute)
	testwait.Eventually(t, "the next minute's refusal line", func() bool { return logs.count(name, refusal) == 2 })
	if client.Connected() {
		t.Fatal("a client whose credential is refused reports itself connected")
	}
}

// A connection whose server stops answering without closing it (packets dropped, no reset) is
// declared lost within seconds with WithSilentCutDetection, and reconnects once the traffic
// moves again. With nats.go's defaults (a ping every two minutes, two outstanding) the client
// would read connected for minutes into the cut. nats.go times the pings on its own clock, which
// no clock of the test reaches, so the seconds are read off the pings the connection runs with:
// it declares itself lost on the interval that would put one ping more than MaxPingsOut out.
func TestASilentCutIsNoticedWithinSecondsAndHealsWhenTrafficResumes(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.Connect([]string{uri}, bus.WithSilentCutDetection())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	testwait.Eventually(t, "the client's connection", client.Connected)
	interval, out := client.Conn.Opts.PingInterval, client.Conn.Opts.MaxPingsOut
	if noticedWithin := interval * time.Duration(out+1); noticedWithin >= 5*time.Second {
		t.Fatalf("pings every %s with %d out notice a silent cut only %s into it, past the 5 s envoy-dispatch's /healthz has to report it in",
			interval, out, noticedWithin)
	}

	thaw := srv.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes the server's shutdown.
	t.Cleanup(thaw)
	testwait.Eventually(t, "the client to notice the silent cut", func() bool { return !client.Connected() })

	thaw()
	testwait.Eventually(t, "the client to reconnect once the traffic moves again", client.Connected)
}
