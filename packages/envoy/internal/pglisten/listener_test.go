package pglisten

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestMain runs the package's tests storetest.DefaultParallel at a time (SetDefaultParallel), under
// storetest.Main.
func TestMain(m *testing.M) {
	storetest.SetDefaultParallel()
	os.Exit(storetest.Main(m))
}

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
// otherwise but for the backoff: a test here that loses a connection is about what the listener
// does next, not about how long it waits first, and the default's first wait is a second plus up
// to half of it again, which each such test slept through. What that wait is for every caller
// that sets none is RedialBackoff's own test, and the test of a listener returning while it
// backs off sets a wait of its own.
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
		Backoff:  func(int) time.Duration { return 20 * time.Millisecond },
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

// waitConnected waits for OnConnected, bounded by the test binary's deadline; a wait that gives up
// prints how many dials the listener had made.
func (p *listenerProbe) waitConnected(t *testing.T, when string) {
	t.Helper()
	testwait.Until(t, "OnConnected "+when, func() (string, bool) {
		select {
		case <-p.connected:
			return "", true
		default:
			return fmt.Sprintf("%d dials", p.dials.Load()), false
		}
	})
}

func (p *listenerProbe) waitNote(t *testing.T, when string) listenerNote {
	t.Helper()
	return testwait.Receive(t, "OnNotify: "+when, p.notes)
}

// runListener runs listener until the returned cancel is called or the test ends; done closes
// once Run has returned. The test's cleanup waits for Run to return once its context has ended.
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
		testwait.Receive(t, "Run's return after its context ended", done)
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

// waitBackendGone waits until Postgres no longer lists the backend pid in the test's database, so
// a backend of another test's database that took the pid since is not counted.
func waitBackendGone(t *testing.T, database *store.Store, pid uint32) {
	t.Helper()
	testwait.UntilPolling(t, 50*time.Millisecond, fmt.Sprintf("backend %d to leave pg_stat_activity after the listener returned", pid), func() (int, bool) {
		var count int
		if err := database.Pool.QueryRow(context.Background(),
			`select count(*) from pg_stat_activity where pid = $1 and datname = current_database()`, int(pid)).Scan(&count); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		return count, count == 0
	})
}

// databaseNow is the database's own clock, the one pg_stat_activity dates a statement's start on.
func databaseNow(t *testing.T, database *store.Store) time.Time {
	t.Helper()
	var now time.Time
	if err := database.Pool.QueryRow(context.Background(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	return now
}

// waitForPings waits for the backend pid to run n pings that began after since, on the database's
// own clock. Each ping is a statement of its own and moves the backend's query_start, so the
// distinct starts pg_stat_activity shows beside "-- ping" are pings the backend ran; a loaded
// machine can only make a read miss some, and the wait then longer. A connection the listener
// replaced is closed, its backend gone from the test's database, so the read fails rather than
// count the replacement's pings.
func waitForPings(t *testing.T, database *store.Store, pid uint32, since time.Time, n int) {
	t.Helper()
	pings := map[int64]bool{}
	testwait.Until(t, fmt.Sprintf("%d pings on backend %d begun after %s", n, pid, since.Format(time.RFC3339Nano)), func() (string, bool) {
		var query string
		var started time.Time
		if err := database.Pool.QueryRow(context.Background(),
			`select query, query_start from pg_stat_activity where pid = $1 and datname = current_database()`, int(pid)).Scan(&query, &started); err != nil {
			t.Fatalf("read the listener's backend %d: %v", pid, err)
		}
		if query == "-- ping" && started.After(since) {
			pings[started.UnixMicro()] = true
		}
		return fmt.Sprintf("%d pings, last statement %q", len(pings), query), len(pings) >= n
	})
}

func TestTheListenerReconnectsAfterItsBackendIsKilled(t *testing.T) {
	t.Parallel()
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
// replaced: three pings on the first connection's backend, one dial, one OnConnected, and a
// notification still reaches OnNotify on it.
//
// The ping timeout is past the run: a ping that outlasted a timeout on a loaded machine would
// replace the connection and fail the row on the machine's speed, and what the row holds - a
// healthy idle connection is pinged and kept - needs no timeout on the ping at all, since a ping
// that never answered would stop the pings the row waits for. For the same reason the row waits
// for the pings themselves, as the backend runs them, rather than sleeping out seven idle periods
// and then requiring the backend's last statement to be a ping: the sleep was a window the pings
// had to land in.
func TestTheListenerKeepsAHealthyIdleConnection(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	listener.ListenIdle, listener.PingTimeout = 200*time.Millisecond, testwait.PastTheRun
	runListener(t, probe, listener)
	probe.waitConnected(t, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	waitForPings(t, database, pid, time.Time{}, 3)
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials after three idle periods = %d, want 1", dials)
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
	// Not parallel: its 1 s PingTimeout is the bound the row is about, and every ping and LISTEN
	// on a healthy connection - the first one before the proxy blinds it, and the replacement -
	// must answer inside it, or the listener replaces a connection that was never half-open.
	database := storetest.Open(t)
	proxy := newBlindingProxy(t, databaseAddress(database))
	probe := newListenerProbe()
	listener := probe.listener(proxiedDial(database, proxy))
	listener.ListenIdle, listener.PingTimeout = time.Second, time.Second
	runListener(t, probe, listener)
	probe.waitConnected(t, "first connection")
	notifyChannel(t, database, "delivery_changes", "")
	if note := probe.waitNote(t, "a notification through the proxy"); note != (listenerNote{channel: "delivery_changes"}) {
		t.Fatalf("notification through the proxy = %+v, want delivery_changes with an empty payload", note)
	}

	proxy.blind()
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
	t.Parallel()
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
	// A marker sent after them, on a channel the listener names: Postgres delivers notifications
	// in the order their transactions committed, so anything else that reached OnNotify comes
	// before the marker, however slowly a loaded machine delivers it.
	marker := listenerNote{channel: "dispatch_events", payload: "marker"}
	notifyChannel(t, database, "delivery_unheard", "never reported")
	for _, note := range append(sent, marker) {
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
	if note := probe.waitNote(t, "the marker"); note != marker {
		t.Fatalf("an extra notification reached OnNotify before the marker: %+v", note)
	}
}

// A listener listens on the channels it names and no others, and needs no OnConnected: a caller
// with nothing to catch up after a LISTEN leaves it nil (Listener.OnConnected).
func TestAListenerOnOneChannelWithoutAnOnConnectedHearsThatChannelAlone(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := &Listener{
		Dial:     probe.dialing(database.Pool.Listener),
		Channels: []string{"dispatch_doc_changed"},
		OnNotify: probe.notify,
	}
	runListener(t, probe, listener)

	// The LISTEN has to be in place before a notification can reach it, and this listener reports
	// no connection: a notification repeated until one arrives is what a test without OnConnected
	// has. Every repeat is delivered once the LISTEN stands, so the first note is the first that
	// found it.
	go func() {
		for {
			select {
			case <-probe.stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if _, err := database.Pool.Exec(context.Background(), `select pg_notify('dispatch_doc_changed', 'artifact-1')`); err != nil {
				return
			}
		}
	}()
	if note := probe.waitNote(t, "a notification on the one channel"); note != (listenerNote{channel: "dispatch_doc_changed", payload: "artifact-1"}) {
		t.Fatalf("notification = %+v, want dispatch_doc_changed carrying artifact-1", note)
	}

	notifyChannel(t, database, "delivery_changes", "")
	// A marker on the one channel, sent after it: Postgres delivers notifications in the order
	// their transactions committed, so a note of the other channel, had the listener heard it,
	// comes before the marker, however slowly a loaded machine delivers it.
	notifyChannel(t, database, "dispatch_doc_changed", "marker")
	for {
		note := probe.waitNote(t, "the marker on the one channel")
		if note.channel != "dispatch_doc_changed" {
			t.Fatalf("a channel the listener does not name reached OnNotify: %+v", note)
		}
		if note.payload == "marker" {
			return
		}
	}
}

// The channels Dispatch's own writers notify reach a listener that names them: a save of a
// project's delivery settings notifies delivery_changes and delivery_freshness (migration
// 20261123010254) and an event insert dispatch_events (migration 0107).
func TestTheListenerHearsTheTriggersItListensFor(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	probe := newListenerProbe()
	runListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, "first connection")
	ctx := context.Background()

	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee, default_repo) values ('LIVE', 'Live', 'alice', 'acme/widgets')`); err != nil {
		t.Fatalf("insert the project: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settings save: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := delivery.PutProjectSettings(ctx, tx, delivery.ProjectSettingsInput{
		Project: "LIVE", Delivery: true, PopulationAuthors: []string{"octocat"},
		Sources: []delivery.SourceInput{{
			Kind: delivery.SourceKindGitHubWorkflow, DeployWorkflowPath: new(".github/workflows/deploy.yml"),
			ProductionJobNames: []string{"widgets-release / widgets-release"}, PRChecksWorkflowPath: new(".github/workflows/pr-checks.yml"),
		}},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("PutProjectSettings: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the settings save: %v", err)
	}
	for _, statement := range []string{
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
	t.Parallel()
	database := storetest.Open(t)
	probe := newListenerProbe()
	cancel, done := runListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, "first connection")
	conn := probe.lastConn(t)
	pid := conn.PgConn().PID()

	cancel()
	testwait.Receive(t, "Run's return after its context ended", done)
	if !conn.IsClosed() {
		t.Fatal("Run returned with its connection open")
	}
	waitBackendGone(t, database, pid)
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials = %d, want 1", dials)
	}
}

func TestTheListenerReturnsWhileBackingOffWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	backingOff := make(chan int, 4)
	listener.Backoff = func(attempt int) time.Duration {
		backingOff <- attempt
		return time.Hour
	}
	cancel, done := runListener(t, probe, listener)
	probe.waitConnected(t, "first connection")
	conn := probe.lastConn(t)
	terminateBackend(t, database, conn.PgConn().PID())
	if attempt := testwait.Receive(t, "the listener's backoff after its backend was killed", backingOff); attempt != 1 {
		t.Fatalf("the first backoff after a lost connection is attempt %d, want 1", attempt)
	}

	cancel()
	testwait.Receive(t, "Run's return after its context ended while it backed off an hour", done)
	if !conn.IsClosed() {
		t.Fatal("Run returned with its lost connection open")
	}
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials = %d, want 1: the backoff was cut short by a redial", dials)
	}
}

// The default backoff waits a second, doubling to thirty, and adds up to half of that again.
func TestTheListenerBacksOffFromOneSecondToThirtyWithJitter(t *testing.T) {
	t.Parallel()
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
//
// The ping timeout is past the run, for the reason TestTheListenerKeepsAHealthyIdleConnection
// gives.
func TestAListenerWithNoOnNotifyDropsItsNotificationsAndDoesNotPanic(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	listener.OnNotify = nil
	listener.ListenIdle, listener.PingTimeout = 200*time.Millisecond, testwait.PastTheRun
	runListener(t, probe, listener)
	probe.waitConnected(t, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	notified := databaseNow(t, database)
	notifyChannel(t, database, "dispatch_events", "dropped")
	// A notification it has nowhere to report must leave it waiting and pinging on the same
	// connection, so the row waits for pings on that backend begun after the notification was
	// sent, as the backend runs them, rather than sleeping half a second and requiring the last
	// statement to be one. Three, because the first may have been in flight as the notification
	// was sent and the notification then reaches the listener with its answer; by the third, the
	// listener has read it and dropped it and gone on waiting.
	waitForPings(t, database, pid, notified, 3)
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials after a notification the listener has nowhere to report = %d, want 1: the notification cost it the connection", dials)
	}
}
