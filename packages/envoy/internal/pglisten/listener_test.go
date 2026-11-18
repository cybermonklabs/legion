package pglisten

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m.Run)) }

// testChannels are the channels the listener under test listens on: the delivery hub's three, the
// one deployment where three channels share a connection (api/delivery_live_hub.go).
var testChannels = []string{"delivery_changes", "delivery_freshness", "dispatch_events"}

// listenerNote is one notification a listener under test reported.
type listenerNote struct{ channel, payload string }

// listenerProbe records what a Listener under test does: each dial, each connection a dial
// returned, each OnConnected and each notification.
type listenerProbe struct {
	dials     atomic.Int32
	connected chan struct{}
	notes     chan listenerNote
	stop      chan struct{}
	mu        sync.Mutex
	conns     []*pgx.Conn
}

func newListenerProbe() *listenerProbe {
	return &listenerProbe{connected: make(chan struct{}, 16), notes: make(chan listenerNote, 64), stop: make(chan struct{})}
}

// listener is a Listener that dials with dial and reports to the probe, at its defaults
// otherwise.
func (p *listenerProbe) listener(dial func(context.Context) (*pgx.Conn, error)) *Listener {
	return &Listener{
		Dial:     p.dialing(dial),
		Channels: testChannels,
		OnConnected: func() {
			select {
			case p.connected <- struct{}{}:
			case <-p.stop:
			}
		},
		OnNotify: p.notify,
	}
}

// dialing is dial, counted and recorded by the probe.
func (p *listenerProbe) dialing(dial func(context.Context) (*pgx.Conn, error)) func(context.Context) (*pgx.Conn, error) {
	return func(ctx context.Context) (*pgx.Conn, error) {
		p.dials.Add(1)
		conn, err := dial(ctx)
		if err == nil {
			p.mu.Lock()
			p.conns = append(p.conns, conn)
			p.mu.Unlock()
		}
		return conn, err
	}
}

func (p *listenerProbe) notify(channel, payload string) {
	select {
	case p.notes <- listenerNote{channel: channel, payload: payload}:
	case <-p.stop:
	}
}

// lastConn is the connection the listener's latest successful dial returned.
func (p *listenerProbe) lastConn(t *testing.T) *pgx.Conn {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.conns) == 0 {
		t.Fatal("the listener has dialed no connection")
	}
	return p.conns[len(p.conns)-1]
}

func (p *listenerProbe) waitConnected(t *testing.T, when string) {
	t.Helper()
	testwait.Receive(t, when+" (OnConnected)", p.connected)
}

func (p *listenerProbe) waitNote(t *testing.T, when string) listenerNote {
	t.Helper()
	return testwait.Receive(t, when+" (OnNotify)", p.notes)
}

// runListener runs listener until the returned cancel is called or the test ends; done closes
// once Run has returned. The test's cleanup cancels it and waits for Run to return.
func runListener(t *testing.T, probe *listenerProbe, listener *Listener) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		listener.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		close(probe.stop)
		testwait.Receive(t, "Run to return once its context ended", done)
	})
	return cancel, done
}

func notifyChannel(t *testing.T, database *store.Store, channel, payload string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `select pg_notify($1, $2)`, channel, payload); err != nil {
		t.Fatalf("pg_notify(%q, %q): %v", channel, payload, err)
	}
}

func terminateBackend(t *testing.T, database *store.Store, pid uint32) {
	t.Helper()
	var terminated bool
	if err := database.Pool.QueryRow(context.Background(), `select pg_terminate_backend($1)`, int(pid)).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v; want true", pid, terminated, err)
	}
}

// waitBackendGone waits until Postgres no longer lists the backend pid.
func waitBackendGone(t *testing.T, database *store.Store, pid uint32) {
	t.Helper()
	testwait.Eventually(t, "the listener's killed backend to leave pg_stat_activity", func() bool {
		var count int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity where pid = $1`, int(pid)).Scan(&count); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		return count == 0
	})
}

// awaitIdleWait waits until the listener is waiting out its idle period on the fake alone - the one
// timer armed a full idle period ahead, told apart from the short-lived ping timeout by its
// deadline - and returns that deadline, so a test can move the fake to it to make the listener
// ping. It is for a listener whose Clock is a clocktest.Fake.
func awaitIdleWait(t *testing.T, fake *clocktest.Fake) time.Time {
	t.Helper()
	armed := testwait.Until(t, "the listener's idle wait on the fake clock", func() ([]time.Time, bool) {
		armed := fake.Armed()
		return armed, len(armed) == 1 && armed[0].Sub(fake.Now()) > DefaultPingTimeout
	})
	return armed[0]
}

func TestTheListenerReconnectsAfterItsBackendIsKilled(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	runListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, "first connection")
	killed := probe.lastConn(t).PgConn().PID()

	terminateBackend(t, database, killed)
	probe.waitConnected(t, "after its backend was killed")
	if dials := probe.dials.Load(); dials < 2 {
		t.Fatalf("dials after the kill = %d, want a second", dials)
	}
	if pid := probe.lastConn(t).PgConn().PID(); pid == killed {
		t.Fatalf("the listener reconnected on backend %d, the one that was killed", pid)
	}

	notifyChannel(t, database, "delivery_changes", "")
	if note := probe.waitNote(t, "a notification after the reconnect"); note != (listenerNote{channel: "delivery_changes"}) {
		t.Fatalf("notification after the reconnect = %+v, want delivery_changes with an empty payload", note)
	}
}

// blindingProxy forwards each TCP connection it accepts to target until blind is called. From then
// on the connections it already holds forward nothing, and neither side of them is closed, so each
// looks open to both ends and stays silent: a half-open connection. A connection it accepts after
// blind forwards as usual.
type blindingProxy struct {
	listener net.Listener
	target   string
	closed   chan struct{}
	mu       sync.Mutex
	shut     bool
	gates    []*atomic.Bool
	conns    []net.Conn
}

func newBlindingProxy(t *testing.T, target string) *blindingProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the proxy: %v", err)
	}
	proxy := &blindingProxy{listener: listener, target: target, closed: make(chan struct{})}
	go proxy.serve()
	t.Cleanup(proxy.close)
	return proxy
}

func (p *blindingProxy) addr() string { return p.listener.Addr().String() }

func (p *blindingProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		gate := &atomic.Bool{}
		p.mu.Lock()
		if p.shut {
			p.mu.Unlock()
			_ = client.Close()
			_ = server.Close()
			return
		}
		p.gates = append(p.gates, gate)
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(gatedWriter{to: server, gate: gate, closed: p.closed}, client) }()
		go func() { _, _ = io.Copy(gatedWriter{to: client, gate: gate, closed: p.closed}, server) }()
	}
}

// blind stops every connection the proxy holds from forwarding, closing neither side.
func (p *blindingProxy) blind() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, gate := range p.gates {
		gate.Store(true)
	}
}

func (p *blindingProxy) close() {
	p.mu.Lock()
	p.shut = true
	conns := p.conns
	p.mu.Unlock()
	close(p.closed)
	_ = p.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// gatedWriter forwards writes until its gate is set, then holds every write until the proxy closes.
type gatedWriter struct {
	to     io.Writer
	gate   *atomic.Bool
	closed <-chan struct{}
}

func (w gatedWriter) Write(b []byte) (int, error) {
	if w.gate.Load() {
		<-w.closed
		return 0, net.ErrClosed
	}
	return w.to.Write(b)
}

// proxiedDial dials the test database through proxy.
func proxiedDial(database *store.Store, proxy *blindingProxy) func(context.Context) (*pgx.Conn, error) {
	config := database.Pool.Config().ConnConfig
	config.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", proxy.addr())
	}
	return func(ctx context.Context) (*pgx.Conn, error) { return pgx.ConnectConfig(ctx, config) }
}

func databaseAddress(database *store.Store) string {
	config := database.Pool.Config().ConnConfig
	return net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port)))
}

// A healthy connection the listener has been idle on for several idle periods is pinged, not
// replaced: one dial, one OnConnected, the backend's last statement the ping, and a notification
// still reaches OnNotify on it.
func TestTheListenerKeepsAHealthyIdleConnection(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	fake := clocktest.New(time.Now())
	listener.Clock = fake
	runListener(t, probe, listener)
	probe.waitConnected(t, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	// Move the fake past the idle period: the listener ends its idle wait and pings the healthy
	// connection rather than redialing. Driven until the ping lands, in case an earlier wait ended
	// on something other than the deadline.
	testwait.Eventually(t, "the listener to ping its healthy idle connection", func() bool {
		deadline := awaitIdleWait(t, fake)
		fake.Advance(deadline.Sub(fake.Now()) + time.Millisecond)
		awaitIdleWait(t, fake)
		var query string
		if err := database.Pool.QueryRow(context.Background(), `select query from pg_stat_activity where pid = $1`, int(pid)).Scan(&query); err != nil {
			t.Fatalf("read the listener's backend: %v", err)
		}
		return query == "-- ping"
	})
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials after the idle periods = %d, want 1", dials)
	}
	select {
	case <-probe.connected:
		t.Fatal("OnConnected fired again on an idle, healthy connection")
	default:
	}
	notifyChannel(t, database, "dispatch_events", "")
	if note := probe.waitNote(t, "a notification after the idle periods"); note != (listenerNote{channel: "dispatch_events"}) {
		t.Fatalf("notification = %+v, want dispatch_events with an empty payload", note)
	}
}

func TestTheListenerReplacesAHalfOpenConnection(t *testing.T) {
	database := storetest.Open(t)
	proxy := newBlindingProxy(t, databaseAddress(database))
	probe := newListenerProbe()
	listener := probe.listener(proxiedDial(database, proxy))
	fake := clocktest.New(time.Now())
	listener.Clock = fake
	runListener(t, probe, listener)
	probe.waitConnected(t, "first connection")
	notifyChannel(t, database, "delivery_changes", "")
	if note := probe.waitNote(t, "a notification through the proxy"); note != (listenerNote{channel: "delivery_changes"}) {
		t.Fatalf("notification through the proxy = %+v, want delivery_changes with an empty payload", note)
	}

	proxy.blind()
	// The connection is half-open: it delivers nothing and fails nothing. Move the fake past the
	// idle period so the listener pings it; the ping hangs, so move past the ping timeout to fail
	// it; closing the half-open connection hangs too, then comes the redial backoff. The listener
	// then dials a fresh connection the proxy forwards and LISTENs on it again.
	idle := awaitIdleWait(t, fake)
	fake.Advance(idle.Sub(fake.Now()) + time.Millisecond)
	ping := fake.AwaitArmed(t, "the ping timeout on the half-open connection", 1)
	fake.Advance(ping[0].Sub(fake.Now()) + time.Millisecond)
	closing := fake.AwaitArmed(t, "the close of the half-open connection", 1)
	fake.Advance(closing[0].Sub(fake.Now()) + time.Millisecond)
	backoff := fake.AwaitArmed(t, "the redial backoff", 1)
	fake.Advance(backoff[0].Sub(fake.Now()) + time.Millisecond)
	probe.waitConnected(t, "after its connection went half-open")
	if dials := probe.dials.Load(); dials < 2 {
		t.Fatalf("dials after the connection went half-open = %d, want a second", dials)
	}
	notifyChannel(t, database, "delivery_changes", "")
	if note := probe.waitNote(t, "a notification on the replacement"); note != (listenerNote{channel: "delivery_changes"}) {
		t.Fatalf("notification on the replacement = %+v, want delivery_changes with an empty payload", note)
	}
}

// OnNotify gets each of the listener's three channels' payloads exactly as sent, in the order
// sent, and nothing from a channel the listener does not listen on.
func TestTheListenerPassesEachPayloadUnchanged(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	runListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, "first connection")

	freshness := `{"last_event_at" : "2026-10-12T15:04:05.123456+00:00", "last_reconcile_at" : null, "last_error" : "GitHub answered 502: \"bad gateway\" (réessayer)"}`
	sent := []listenerNote{
		{channel: "delivery_changes", payload: ""},
		{channel: "delivery_freshness", payload: freshness},
		{channel: "dispatch_events", payload: ""},
	}
	notifyChannel(t, database, "delivery_unheard", "never reported")
	for _, note := range sent {
		notifyChannel(t, database, note.channel, note.payload)
	}
	for index, want := range sent {
		if got := probe.waitNote(t, "notification "+strconv.Itoa(index)); got != want {
			t.Fatalf("notification %d = %+v, want %+v", index, got, want)
		}
	}
	if !json.Valid([]byte(freshness)) {
		t.Fatal("the freshness payload sent is not JSON")
	}
	// Every note sent on a listened channel was received above, in order, and delivery_unheard is a
	// channel the listener does not name, so Postgres never delivers it: nothing more can arrive.
	select {
	case note := <-probe.notes:
		t.Fatalf("an extra notification reached OnNotify: %+v", note)
	default:
	}
}

// A listener listens on the channels it names and no others, and needs no OnConnected: a caller
// with nothing to catch up after a LISTEN leaves it nil (Listener.OnConnected).
func TestAListenerOnOneChannelWithoutAnOnConnectedHearsThatChannelAlone(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := &Listener{
		Dial:     probe.dialing(database.Pool.Listener),
		Channels: []string{"dispatch_doc_changed"},
		OnNotify: probe.notify,
	}
	runListener(t, probe, listener)

	// The LISTEN has to be in place before a notification can reach it, and this listener reports no
	// connection: notifications sent before the LISTEN stands are lost. Repeat it until one is
	// reported.
	var got listenerNote
	testwait.Eventually(t, "a notification on the one channel once its LISTEN stands", func() bool {
		notifyChannel(t, database, "dispatch_doc_changed", "artifact-1")
		select {
		case got = <-probe.notes:
			return true
		default:
			return false
		}
	})
	if got != (listenerNote{channel: "dispatch_doc_changed", payload: "artifact-1"}) {
		t.Fatalf("notification = %+v, want dispatch_doc_changed carrying artifact-1", got)
	}

	// A channel this listener does not name never reaches OnNotify. Send one, then a marker on the
	// named channel; once the marker arrives, every note the listener will report has been read, and
	// the unnamed channel was not among them.
	notifyChannel(t, database, "delivery_changes", "")
	notifyChannel(t, database, "dispatch_doc_changed", "marker")
	for {
		note := probe.waitNote(t, "a notification after the unnamed channel")
		if note.payload == "marker" {
			break
		}
		if note.channel != "dispatch_doc_changed" {
			t.Fatalf("a channel the listener does not name reached OnNotify: %+v", note)
		}
	}
}

// The channels Dispatch's own writers notify reach a listener that names them: a settings write
// notifies delivery_changes and delivery_freshness (migration 0100) and an event insert
// dispatch_events (migration 0107).
func TestTheListenerHearsTheTriggersItListensFor(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	runListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, "first connection")
	ctx := context.Background()

	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"widgets-release / widgets-release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('LIVE', 'Live', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('LIVE-1', 'LIVE', 1, 'x', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into events (issue_key, seq, type, actor, payload, notify) values ('LIVE-1', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{}', false)`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	heard := map[string]string{}
	for len(heard) < 3 {
		note := probe.waitNote(t, "the triggers' notifications")
		heard[note.channel] = note.payload
	}
	if heard["delivery_changes"] != "" || heard["dispatch_events"] != "" {
		t.Fatalf("delivery_changes and dispatch_events payloads = %q, %q; want both empty", heard["delivery_changes"], heard["dispatch_events"])
	}
	var freshness map[string]any
	if err := json.Unmarshal([]byte(heard["delivery_freshness"]), &freshness); err != nil {
		t.Fatalf("delivery_freshness payload %q is not JSON: %v", heard["delivery_freshness"], err)
	}
	for _, key := range []string{"last_event_at", "last_reconcile_at", "last_error"} {
		if _, ok := freshness[key]; !ok {
			t.Fatalf("delivery_freshness payload %q lacks %q", heard["delivery_freshness"], key)
		}
	}
}

func TestTheListenerReturnsWithItsConnectionClosedWhenItsContextEnds(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	cancel, done := runListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, "first connection")
	conn := probe.lastConn(t)
	pid := conn.PgConn().PID()

	cancel()
	testwait.Receive(t, "Run to return once its context ended", done)
	if !conn.IsClosed() {
		t.Fatal("Run returned with its connection open")
	}
	waitBackendGone(t, database, pid)
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials = %d, want 1", dials)
	}
}

func TestTheListenerReturnsWhileBackingOffWhenItsContextEnds(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	backingOff := make(chan int, 4)
	listener.Backoff = func(attempt int) time.Duration {
		backingOff <- attempt
		// An hour keeps the redial away so the test can cancel mid-backoff; it never elapses.
		return time.Hour
	}
	cancel, done := runListener(t, probe, listener)
	probe.waitConnected(t, "first connection")
	conn := probe.lastConn(t)
	terminateBackend(t, database, conn.PgConn().PID())
	if attempt := testwait.Receive(t, "the listener to back off after its backend was killed", backingOff); attempt != 1 {
		t.Fatalf("the first backoff after a lost connection is attempt %d, want 1", attempt)
	}

	cancel()
	testwait.Receive(t, "Run to return once its context ended while it backed off", done)
	if !conn.IsClosed() {
		t.Fatal("Run returned with its lost connection open")
	}
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials = %d, want 1: the backoff was cut short by a redial", dials)
	}
}

// The default backoff waits a second, doubling to thirty, and adds up to half of that again.
func TestTheListenerBacksOffFromOneSecondToThirtyWithJitter(t *testing.T) {
	for attempt, base := range map[int]time.Duration{
		1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 5: 16 * time.Second,
		6: 30 * time.Second, 7: 30 * time.Second, 50: 30 * time.Second,
	} {
		for range 200 {
			if wait := RedialBackoff(attempt); wait < base || wait > base+base/2 {
				t.Fatalf("RedialBackoff(%d) = %v, want within [%v, %v]", attempt, wait, base, base+base/2)
			}
		}
	}
	waits := map[time.Duration]bool{}
	for range 50 {
		waits[RedialBackoff(1)] = true
	}
	if len(waits) < 2 {
		t.Fatalf("RedialBackoff(1) returned %v every time, want jitter", waits)
	}
}

// A listener that names channels and leaves OnNotify nil drops what arrives on them and lives:
// OnNotify is nil-checked where OnConnected is, so the caller loses its notifications and
// nothing else - the connection it heard them on is still the one it waits and pings on.
// Without the check the first notification called a nil func, and the panic took the whole
// process with it - the delivery hub's listener and the event stream relay's included, since
// they run in it.
func TestAListenerWithNoOnNotifyDropsItsNotificationsAndDoesNotPanic(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	listener.OnNotify = nil
	fake := clocktest.New(time.Now())
	listener.Clock = fake
	runListener(t, probe, listener)
	probe.waitConnected(t, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	notifyChannel(t, database, "dispatch_events", "dropped")
	// The notification is dropped (nil OnNotify) rather than crashing the process. Drive the idle
	// period until the connection is pinged: a dropped notification may end one idle wait early (the
	// connection carried it), so the ping can take a further period; the connection is kept either
	// way, never replaced.
	testwait.Eventually(t, "the listener to ping the connection it dropped a notification on", func() bool {
		deadline := awaitIdleWait(t, fake)
		fake.Advance(deadline.Sub(fake.Now()) + time.Millisecond)
		awaitIdleWait(t, fake)
		var query string
		if err := database.Pool.QueryRow(context.Background(), `select query from pg_stat_activity where pid = $1`, int(pid)).Scan(&query); err != nil {
			t.Fatalf("read the listener's backend: %v", err)
		}
		return query == "-- ping"
	})
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials after a notification the listener has nowhere to report = %d, want 1: the notification cost it the connection", dials)
	}
}
