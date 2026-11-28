package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// The listener's start on NATS (Phase 5: connectListenerBus, which dials and reconciles the stream)
// waits out a refusal of its credential for up to listenerCredentialWait, so a pod that starts
// before the hub's servers have reloaded its new nkey connects once they have, rather than exiting
// and being restarted.

// listenerTryName is the name the listener's main client gives each connection it dials: the
// bus's default, since main names that client nothing else. A server's connection listing (/connz)
// tells the start's tries apart from the test's own connections by it.
const listenerTryName = "envoy"

// streamInfoSubject is the request the stream's reconcile sends first, which a grant without it
// refuses.
const streamInfoSubject = "$JS.API.STREAM.INFO." + bus.Stream

// authorizationViolation is the server's refusal of a credential it does not know, as nats-server's
// -ERR words it and as the error nats.go returns for it keeps it ("nats: Authorization Violation",
// which errors.Is matches to nats.ErrAuthorization, itself spelled in lower case).
const authorizationViolation = "Authorization Violation"

// credentialWaitTolerance is how far each wait of a listener run as a process on the wall clock
// (startListenerChild), as the server records it, may stand from the wait the start is due to make
// (c-start-never). A start this process runs on a held clock (heldStart) is held to the waits it
// asked for, exactly.
const credentialWaitTolerance = 500 * time.Millisecond

// startDrainBound is how long a start the test stopped waiting for may run on once the test's
// servers have stopped: its dials are refused then, so it ends after main's ten tries, one second
// apart, at most a wait of ten seconds after the try it was in.
const startDrainBound = time.Minute

// nkeyAuthorization is a server's authorization that admits fallback, as its no_auth_user, and each
// user entry of users, and refuses every other nkey. The server holds an nkey user, so it sends a
// nonce, and a client presenting any other nkey is refused with the server's authorization
// violation rather than with nats.go's "nkeys not supported by the server".
func nkeyAuthorization(fallback string, users ...string) string {
	entries := append([]string{nkeyUser(fallback)}, users...)
	return fmt.Sprintf("authorization {\n  users = [ %s ]\n}\nno_auth_user: %q\n", strings.Join(entries, ", "), fallback)
}

// nkeyUser is the user entry of the nkey user whose public key is public, which may do anything.
func nkeyUser(public string) string { return fmt.Sprintf("{nkey: %q}", public) }

// nkeyUserDenied is the user entry of the nkey user whose public key is public, which may publish
// to anything but subject: a publish to it is the server's permissions violation, as a per-client
// grant that lacks it makes it.
func nkeyUserDenied(public, subject string) string {
	return fmt.Sprintf("{nkey: %q, permissions: {publish: {allow: [\">\"], deny: [%q]}}}", public, subject)
}

// serveStartRoutes serves the listener's probe routes over start (registerProbeRoutes), as main
// serves them from Phase 4 on, with no dependencies set yet, which is the state they are in while
// the start runs. It returns their base URL.
func serveStartRoutes(t *testing.T, start *natsStart) string {
	t.Helper()
	var deps atomic.Pointer[listenerDeps]
	mux := http.NewServeMux()
	registerProbeRoutes(mux, &deps, start, nil)
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the probe routes: %v", err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + listener.Addr().String()
}

// probe answers GET path at base: its status, and its body's fields (nil for a body that is no JSON
// object, as /livez's empty one is not).
func probe(t *testing.T, base, path string) (int, map[string]any) {
	t.Helper()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer to GET %s: %v", path, err)
	}
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	return response.StatusCode, fields
}

// startOutcome is what one run of the listener's start returned, and when: at on the wall clock,
// took on the start's own (natsStart.clock), from its first try.
type startOutcome struct {
	client *bus.Client
	err    error
	at     time.Time
	took   time.Duration
}

// listenerStartRun is a run of the listener's start, connectListenerBus as main's Phase 5 calls it,
// on a goroutine of its own, so the test drives the servers and reads the probe routes meanwhile.
// began stands for the start's first try, which follows it at once.
type listenerStartRun struct {
	began   time.Time
	outcome chan startOutcome
	settled bool
}

// newListenerStartRun makes a run of t's. It registers its cleanup first, before the test starts a
// server, so that cleanup runs after every server has stopped: a run the test stopped waiting for
// (its watchdog fired, or a check failed first) then ends on refused dials, main's ten tries, and
// the client it made, if it made one, is closed.
func newListenerStartRun(t *testing.T) *listenerStartRun {
	t.Helper()
	run := &listenerStartRun{outcome: make(chan startOutcome, 1)}
	t.Cleanup(func() {
		if run.began.IsZero() || run.settled {
			return
		}
		select {
		case got := <-run.outcome:
			if got.client != nil {
				got.client.Close()
			}
		case <-time.After(startDrainBound):
			t.Errorf("the listener's start still ran %s after the test's servers stopped", startDrainBound)
		}
	})
	return run
}

// begin starts the run: the listener's start over cfg, as an edge listener of edge's grant when
// edge is not nil, reporting to start, on a goroutine of held's (goStart) when the start's clock is
// held (heldStart), held nil when it runs on the wall clock.
func (r *listenerStartRun) begin(cfg config.Service, edge *edgeGrant, start *natsStart, held *heldWaits) {
	r.began = time.Now()
	run := func() {
		client, err := connectListenerBus(cfg, edge, start)
		r.outcome <- startOutcome{client: client, err: err, at: time.Now(), took: start.clock.Now().Sub(start.began)}
	}
	if held == nil {
		go run()
		return
	}
	held.goStart(run)
}

// await waits for the start's outcome until the test-local watchdog, watchdog after the run began,
// and fails the test when the watchdog fires first.
func (r *listenerStartRun) await(t *testing.T, watchdog time.Duration) startOutcome {
	t.Helper()
	select {
	case got := <-r.outcome:
		r.settle(t, got)
		return got
	case <-time.After(time.Until(r.began.Add(watchdog))):
		t.Fatalf("watchdog: the listener's start had not returned %s after its first try", watchdog)
		return startOutcome{}
	}
}

// settle takes got as the run's outcome. The client it made is closed when the test ends, before
// the test's servers stop, since this cleanup is registered after theirs.
func (r *listenerStartRun) settle(t *testing.T, got startOutcome) {
	t.Helper()
	r.settled = true
	if got.client != nil {
		t.Cleanup(got.client.Close)
	}
}

// next answers the next wait the start asks clock for, its start's held clock, or, when the start
// returns first, its outcome (heldNext).
func (r *listenerStartRun) next(t *testing.T, clock *heldWaits) (*heldWait, *startOutcome) {
	t.Helper()
	w, got, returned := heldNext(t, clock, r.outcome, "the listener's start")
	if !returned {
		return w, nil
	}
	r.settle(t, got)
	return nil, &got
}

// waitsUntil lets each wait the start asks clock for end, one at a time, until it asks for one
// that would end past mark on the clock, and answers that wait, still held, and every wait the
// start asked for, that one last. Before it lets a wait end, and before it answers the one it
// holds, it calls each with the wait's place among them, when each is not nil. It fails the test
// when the start returns first: every case holds the start's credential refused until mark.
func (r *listenerStartRun) waitsUntil(t *testing.T, clock *heldWaits, mark time.Duration, each func(i int, w *heldWait)) (*heldWait, []time.Duration) {
	t.Helper()
	var waits []time.Duration
	for {
		w, got := r.next(t, clock)
		if got != nil {
			t.Fatalf("the listener's start returned %s after its first try on its clock, having waited %v, before the %s its credential was refused for: error %v", got.took, waits, mark, got.err)
		}
		waits = append(waits, w.wait)
		if each != nil {
			each(len(waits)-1, w)
		}
		if w.endsPast(mark) {
			return w, waits
		}
		w.end()
	}
}

// finish lets each further wait the start asks clock for end at once until the start returns, and
// answers its outcome and those waits. It fails the test when the start's clock would read past
// watchdog, counted from its first try, before the start has returned: a wait that would end past
// it, or a return past it, as a wait the test let end before it called finish can carry the start.
func (r *listenerStartRun) finish(t *testing.T, clock *heldWaits, watchdog time.Duration) (startOutcome, []time.Duration) {
	t.Helper()
	var waits []time.Duration
	for {
		w, got := r.next(t, clock)
		if got != nil {
			if got.took > watchdog {
				t.Fatalf("watchdog: the listener's start returned %s after its first try on its clock, past %s, having waited %v since the test let it go on: error %v", got.took, watchdog, waits, got.err)
			}
			return *got, waits
		}
		waits = append(waits, w.wait)
		if w.endsPast(watchdog) {
			t.Fatalf("watchdog: the listener's start had not returned %s after its first try on its clock, having waited %v since the test let it go on", watchdog, waits)
		}
		w.end()
	}
}

// untilLogged waits until records holds a record matching holds, and answers it, failing the test,
// naming what it waited for, when the start returns or asks clock for a wait first, or when
// heldStepWatchdog passes.
func (r *listenerStartRun) untilLogged(t *testing.T, records *logtest.Recorder, clock *heldWaits, what string, holds func(slog.Record) bool) slog.Record {
	t.Helper()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(heldStepWatchdog)
	for {
		for _, record := range records.Records() {
			if holds(record) {
				return record
			}
		}
		if w := clock.armed(); w != nil {
			t.Fatalf("the listener's start asked for a wait of %s at %s on its clock, before a log line showed %s", w.wait, w.at, what)
		}
		select {
		case got := <-r.outcome:
			r.settle(t, got)
			t.Fatalf("the listener's start returned %s after its first try, before a log line showed %s: error %v", got.at.Sub(r.began), what, got.err)
		case <-deadline:
			t.Fatalf("watchdog: no log line showed %s within %s", what, heldStepWatchdog)
		case <-tick.C:
		}
	}
}

// heldStepWatchdog bounds each wait of a test on a step of a start whose waits are held
// (heldStart): a try of the start, which takes milliseconds against a server on this host and
// waits on nothing on the wall clock but its dial (connectTimeout, 5 s) and its reads. It bites
// only on a hang.
const heldStepWatchdog = time.Minute

// heldOrigin is where the clock of every held start begins (heldStart), the reading of the
// start's first try, from which a held test counts each reading.
var heldOrigin = time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)

// heldWaits is a held start's clock and the goroutines a test runs the start's connects on. The
// clock is a clocktest.Fake that only the test moves, so a try takes no time on it, and each wait
// the start asks for is a timer the fake reports armed (Armed), on which the start waits until the
// test moves the fake to its deadline (heldWait.end). A test so reads the exact wait the production
// schedule asks for, and does what one of its offsets calls for, a reload of the server, while the
// start waits across that offset on its clock, with no wait on the wall clock. The goroutines are
// counted (goStart), and once the test has ended its release moves the fake past each wait they arm
// until every one has returned: a start the test stopped following, as a test that failed mid-run
// does, runs to its end and leaves no goroutine waiting on the clock.
type heldWaits struct {
	fake    *clocktest.Fake
	running sync.WaitGroup
}

// since is the clock's reading, counted from heldOrigin.
func (h *heldWaits) since() time.Duration { return h.fake.Now().Sub(heldOrigin) }

// goStart runs f, a connect of the held start, on a goroutine of its own, which the release waits
// for once the test has ended.
func (h *heldWaits) goStart(f func()) {
	h.running.Add(1)
	go func() {
		defer h.running.Done()
		f()
	}()
}

// armed is the wait the start has asked for and the test has not ended, the earliest when it
// asked for more than one, or nil when it waits on none.
func (h *heldWaits) armed() *heldWait {
	now, armed := h.fake.Now(), h.fake.Armed()
	if len(armed) == 0 {
		return nil
	}
	return &heldWait{at: now.Sub(heldOrigin), wait: armed[0].Sub(now), fake: h.fake, deadline: armed[0]}
}

// release moves the fake to each wait the test's goroutines on the start arm, earliest first,
// until every one of them has returned (goStart). A start that waits out refusals then ends at its
// bound on the fake, and one whose dials fail otherwise after its ten tries, each within
// milliseconds of the wall clock once nothing holds its waits. heldStart registers it after the
// test's servers, so it runs before they stop. It fails the test when a goroutine still runs
// startDrainBound after the test ended.
func (h *heldWaits) release(t *testing.T) {
	returned := make(chan struct{})
	go func() {
		h.running.Wait()
		close(returned)
	}()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	bound := time.After(startDrainBound)
	for {
		h.fake.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
			if len(armed) == 0 {
				return now, false
			}
			return armed[0], true
		})
		select {
		case <-returned:
			return
		case <-bound:
			t.Errorf("a connect of the held start still ran %s after the test ended, with waits %v armed on its clock", startDrainBound, h.fake.Armed())
			return
		case <-tick.C:
		}
	}
}

// heldWait is a wait the start asked its held clock for: at, the clock's reading then, counted from
// heldOrigin, and wait, what the start asked for, which ends at deadline on the fake.
type heldWait struct {
	at, wait time.Duration
	fake     *clocktest.Fake
	deadline time.Time
}

// end moves the fake to the wait's deadline, which ends the wait and lets the start go on.
func (w *heldWait) end() { w.fake.Set(w.deadline) }

// endsPast reports whether the wait ends past mark on its clock: whether the moment mark names
// falls inside it, or before it, as a moment the start waits across.
func (w *heldWait) endsPast(mark time.Duration) bool { return w.at+w.wait > mark }

// heldNext answers the next wait the start asks clock for, or what returned receives when the
// start returns first (returnedFirst). It fails the test, naming what it waited on, when neither
// comes within heldStepWatchdog.
func heldNext[T any](t *testing.T, clock *heldWaits, returned <-chan T, what string) (w *heldWait, got T, returnedFirst bool) {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.After(heldStepWatchdog)
	for {
		if w = clock.armed(); w != nil {
			return w, got, false
		}
		select {
		case got = <-returned:
			return nil, got, true
		case <-deadline:
			t.Fatalf("watchdog: %s neither asked for a wait nor returned within %s, at %s on its clock", what, heldStepWatchdog, clock.since())
			return nil, got, false
		case <-tick.C:
		}
	}
}

// heldFinish lets each further wait the start asks clock for end at once until returned receives,
// and answers what it received and those waits. It fails the test, naming what it waited on, when
// the start's clock would read past by, a reading counted from heldOrigin, before returned
// receives: a wait that would end past it, or a return past it, as a wait the test let end before
// it called heldFinish can carry the start.
func heldFinish[T any](t *testing.T, clock *heldWaits, returned <-chan T, what string, by time.Duration) (T, []time.Duration) {
	t.Helper()
	var waits []time.Duration
	for {
		w, got, done := heldNext(t, clock, returned, what)
		if done {
			if at := clock.since(); at > by {
				t.Fatalf("watchdog: %s returned at %s on its clock, past %s, having waited %v since the test let it go on", what, at, by, waits)
			}
			return got, waits
		}
		waits = append(waits, w.wait)
		if w.endsPast(by) {
			t.Fatalf("watchdog: %s had not returned by %s on its clock, having waited %v since the test let it go on", what, by, waits)
		}
		w.end()
	}
}

// heldStart is the listener's start as main makes it (newNATSStart, with listenerCredentialWait),
// on a clocktest.Fake (heldWaits) that the start counts its bound by and each of its connects waits
// on (bus.WithStartClock), and whose connects also read their nkey seed, seed, from an environment
// of their own (testnats.SeedOnly, bus.WithEnvironment) and not from the process's, which only a
// test that runs alone may change. A test of the start so runs beside the others, and none of its
// waits takes time on the wall clock. It registers the release of the clock's waits (heldWaits)
// as a cleanup, so it runs before the cleanups the test registered earlier: its servers' stops.
func heldStart(t *testing.T, seed string) (*natsStart, *heldWaits) {
	t.Helper()
	clock := &heldWaits{fake: clocktest.New(heldOrigin)}
	t.Cleanup(func() { clock.release(t) })
	start := newNATSStart(listenerCredentialWait)
	start.clock = clock.fake
	start.extra = append(start.extra, bus.WithEnvironment(testnats.SeedOnly(seed)), bus.WithStartClock(clock.fake))
	return start, clock
}

// awaitClosedTries waits until server has recorded tries of the listener's start, each closed, and
// fails the test when it records more: a start on a held clock that is waiting makes no try, so the
// try before its wait is the last the server takes until the wait ends.
func awaitClosedTries(t *testing.T, server *testnats.Server, tries int) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the server to record %d tries of the listener's start, each closed", tries), func() (string, bool) {
		got := listenerTries(t, server)
		if len(got) > tries {
			t.Fatalf("the server recorded %d tries of the listener's start while it waited after its try %d: %+v", len(got), tries, got)
		}
		closed := 0
		for _, try := range got {
			if try.Stop != nil {
				closed++
			}
		}
		return fmt.Sprintf("%d tries, %d closed", len(got), closed), closed == tries
	})
}

// recordAttr is the value of record's attribute key as text, "" when it has none.
func recordAttr(record slog.Record, key string) string {
	var value string
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			value = attr.Value.String()
			return false
		}
		return true
	})
	return value
}

// listenerTries are the connections of the listener's start that server took, open and closed, in
// the order it took them, as its settled record of them lists them (testnats.Server.Connections):
// the server's record of the start's tries, each of which dials one connection. A start ends right
// after it closes its last try's connection, and one read of the server's listings at that moment
// can find that connection on neither its list of open connections nor its list of closed ones,
// and so leave the last try out.
func listenerTries(t *testing.T, server *testnats.Server) []*natsserver.ConnInfo {
	t.Helper()
	var tries []*natsserver.ConnInfo
	for _, connection := range server.Connections(t) {
		if connection.Name == listenerTryName {
			tries = append(tries, connection)
		}
	}
	return tries
}

// triesWaits are the waits between the first n of tries, each from the moment the server closed one
// try's connection to the moment it took the next one's: the time the start spent waiting, to which
// a try's own handshake and refusal add nothing.
func triesWaits(t *testing.T, tries []*natsserver.ConnInfo, n int) []time.Duration {
	t.Helper()
	if len(tries) < n {
		t.Fatalf("the server recorded %d tries of the listener's start, want at least %d: %+v", len(tries), n, tries)
	}
	waits := make([]time.Duration, 0, n-1)
	for i := 1; i < n; i++ {
		if tries[i-1].Stop == nil {
			t.Fatalf("try %d of the listener's start is still open on the server, but a later one came: %+v", i, tries)
		}
		waits = append(waits, tries[i].Start.Sub(*tries[i-1].Stop))
	}
	return waits
}

// TestTheListenerStartWaitsOutACredentialRefusal (c-start-accept): a start whose credential the
// server refuses, at the connect or at the stream's reconcile, waits out the refusal with waits of
// 1, 2, 4 and 8 seconds between its first five tries, and of the 10 s cap after them, answers
// /healthz 503 naming the refusal and /livez 204 meanwhile, and connects and reconciles the stream
// once the server reloads an authorization that admits it, 30 s after the start's first try, as the
// hub's reloader does. The start then returns no error and /healthz answers as on main. Without the
// wait the start ends with the refusal: after main's ten tries, about ten seconds, at the connect,
// and at once at the reconcile.
//
// The start runs on a held clock (heldStart): each wait is the one the start asked for, the server
// records one try, closed, before each wait and none during it, and the reload comes while the
// start waits across the 30 s mark on its clock.
func TestTheListenerStartWaitsOutACredentialRefusal(t *testing.T) {
	t.Parallel()
	const (
		reloadAt = 30 * time.Second
		watchdog = 90 * time.Second
	)
	wantWaits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for _, row := range []struct {
		name string
		// refusing and admitting are the server's authorization before and after its reload, for
		// the listener's nkey user public and the server's no_auth_user fallback.
		refusing, admitting func(public, fallback string) string
		// refusal is what the refused tries end on, which /healthz names meanwhile.
		refusal string
	}{
		{
			name:      "the server does not know the nkey",
			refusing:  func(_, fallback string) string { return nkeyAuthorization(fallback) },
			admitting: func(public, fallback string) string { return nkeyAuthorization(fallback, nkeyUser(public)) },
			refusal:   authorizationViolation,
		},
		{
			name: "the nkey's grant lacks the stream's info",
			refusing: func(public, fallback string) string {
				return nkeyAuthorization(fallback, nkeyUserDenied(public, streamInfoSubject))
			},
			admitting: func(public, fallback string) string { return nkeyAuthorization(fallback, nkeyUser(public)) },
			refusal:   `Permissions Violation for Publish to "` + streamInfoSubject + `"`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			run := newListenerStartRun(t)
			seed, public := testnats.User(t)
			_, fallback := testnats.User(t)
			server, uri := testnats.StartConfig(t, row.refusing(public, fallback), []string{"-js"}, true)
			start, clock := heldStart(t, seed)
			base := serveStartRoutes(t, start)
			if status, fields := probe(t, base, "/healthz"); status != http.StatusOK || fields["status"] != "starting" {
				t.Fatalf("/healthz before the start = %d %v, want main's 200 starting", status, fields)
			}

			run.begin(config.Service{MachineID: "start-credential-wait", NATSURLs: []string{uri}, NATSReplicas: 1}, nil, start, clock)
			held, waits := run.waitsUntil(t, clock, reloadAt, func(i int, _ *heldWait) {
				awaitClosedTries(t, server, i+1)
				if i > 0 {
					return
				}
				if status, fields := probe(t, base, "/healthz"); status != http.StatusServiceUnavailable || !strings.Contains(fmt.Sprint(fields["error"]), row.refusal) {
					t.Fatalf("/healthz answered %d %v while the server refused the start's credential, want 503 naming %q", status, fields, row.refusal)
				}
				if status, _ := probe(t, base, "/livez"); status != http.StatusNoContent {
					t.Fatalf("/livez answered %d while the server refused the start's credential, want 204", status)
				}
			})
			if err := server.Reload(t, row.admitting(public, fallback)); err != nil {
				t.Fatalf("reload the server with an authorization that admits the listener: %v", err)
			}
			held.end()

			got, after := run.finish(t, clock, watchdog)
			if got.err != nil {
				t.Fatalf("the listener's start returned %v %s after its first try on its clock, want it to connect once the server admits it", got.err, got.took)
			}
			if _, err := got.client.JS().StreamInfo(bus.Stream); err != nil {
				t.Fatalf("read the %s stream the start reconciled: %v", bus.Stream, err)
			}
			if status, fields := probe(t, base, "/healthz"); status != http.StatusOK || fields["status"] != "starting" {
				t.Fatalf("/healthz after the start = %d %v, want main's 200 starting", status, fields)
			}
			t.Logf("the start waited %v up to the reload and %v after it, and connected %s after its first try on its clock; the server recorded %d tries", waits, after, got.took, len(listenerTries(t, server)))
			if !slices.Equal(waits, wantWaits) {
				t.Fatalf("the start waited %v between its tries up to the reload at %s, want %v", waits, reloadAt, wantWaits)
			}
		})
	}
}

// edgeStartMachine is the machine id of the edge listener these tests start, one subject token as an
// edge listener's must be.
const edgeStartMachine = "edge-start-wait"

// edgeStartServer is a server for an edge listener's start: its authorization is what the test's
// function made of a fresh nkey user, public, whose seed it holds, and of the server's no_auth_user,
// fallback; and it holds the stream, which fallback created, as a shared listener or Dispatch
// creates it for an edge listener that only reads it.
type edgeStartServer struct {
	server                      *testnats.Server
	uri, seed, public, fallback string
}

// startEdgeStartServer starts an edgeStartServer whose authorization is authorization(public,
// fallback). The stream's creator is named, so the server's record of the start's tries
// (listenerTries) leaves its connection out, and closed before the server stops.
func startEdgeStartServer(t *testing.T, authorization func(public, fallback string) string) edgeStartServer {
	t.Helper()
	var rig edgeStartServer
	rig.seed, rig.public = testnats.User(t)
	_, rig.fallback = testnats.User(t)
	rig.server, rig.uri = testnats.StartConfig(t, authorization(rig.public, rig.fallback), []string{"-js"}, true)
	owner, err := bus.ConnectOwningStream([]string{rig.uri}, bus.WithName("stream-owner"))
	if err != nil {
		t.Fatalf("create the %s stream as the server's no_auth_user: %v", bus.Stream, err)
	}
	t.Cleanup(owner.Close)
	return rig
}

// edgeStartRun is the configuration and the grant of the edge listener of edgeStartMachine, serving
// edgeProject, on the server at uri, as its start runs them (connectListenerBus).
func edgeStartRun(t *testing.T, uri string) (config.Service, *edgeGrant) {
	t.Helper()
	edge, err := newEdgeGrant(edgeStartMachine, []string{edgeProject}, metrics.New())
	if err != nil {
		t.Fatalf("the edge listener's grant: %v", err)
	}
	return config.Service{MachineID: edgeStartMachine, NATSURLs: []string{uri}, NATSReplicas: 1, EdgeProjects: []string{edgeProject}}, edge
}

// TestAnEdgeListenerStartWaitsOutACredentialRefusal: an edge listener (ENVOY_EDGE_PROJECTS) is
// cmd/listener too, so its start waits out a refusal of its nkey as every listener's does
// (connectListenerBus). While the server refuses the nkey, /healthz answers 503 naming the refusal
// and /livez 204, and the start waits 1, 2, 4 and 8 s between its first five tries, and the 10 s
// cap after them, the server recording one try, closed, before each wait. The server reloads an
// authorization that admits the nkey 20 s after the first try on the start's clock (heldStart); the
// start then connects and finds the stream (edgeStreamPresent), and /healthz answers as on main.
// Without the wait the start ends with the refusal after the ten tries, a second apart, that a
// start without the wait makes on the wall clock, before it asks the start's clock for any wait,
// and /healthz answers 200 "starting" throughout.
func TestAnEdgeListenerStartWaitsOutACredentialRefusal(t *testing.T) {
	t.Parallel()
	const (
		reloadAt = 20 * time.Second
		watchdog = 60 * time.Second
	)
	wantWaits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second}
	run := newListenerStartRun(t)
	rig := startEdgeStartServer(t, func(_, fallback string) string { return nkeyAuthorization(fallback) })
	cfg, edge := edgeStartRun(t, rig.uri)
	start, clock := heldStart(t, rig.seed)
	base := serveStartRoutes(t, start)

	run.begin(cfg, edge, start, clock)
	held, waits := run.waitsUntil(t, clock, reloadAt, func(i int, _ *heldWait) {
		awaitClosedTries(t, rig.server, i+1)
		if i > 0 {
			return
		}
		if status, fields := probe(t, base, "/healthz"); status != http.StatusServiceUnavailable || !strings.Contains(fmt.Sprint(fields["error"]), authorizationViolation) {
			t.Fatalf("/healthz answered %d %v while the server refused the edge listener's nkey, want 503 naming %q", status, fields, authorizationViolation)
		}
		if status, _ := probe(t, base, "/livez"); status != http.StatusNoContent {
			t.Fatalf("/livez answered %d while the server refused the edge listener's nkey, want 204", status)
		}
	})
	if err := rig.server.Reload(t, nkeyAuthorization(rig.fallback, nkeyUser(rig.public))); err != nil {
		t.Fatalf("reload the server with an authorization that admits the edge listener: %v", err)
	}
	held.end()

	got, after := run.finish(t, clock, watchdog)
	if got.err != nil {
		t.Fatalf("the edge listener's start returned %v %s after its first try on its clock, want it to connect once the server admits it", got.err, got.took)
	}
	if status, fields := probe(t, base, "/healthz"); status != http.StatusOK || fields["status"] != "starting" {
		t.Fatalf("/healthz after the edge listener's start = %d %v, want main's 200 starting", status, fields)
	}
	t.Logf("the start waited %v up to the reload and %v after it, and connected %s after its first try on its clock; the server recorded %d tries", waits, after, got.took, len(listenerTries(t, rig.server)))
	if !slices.Equal(waits, wantWaits) {
		t.Fatalf("the edge listener's start waited %v between its tries up to the reload at %s, want %v", waits, reloadAt, wantWaits)
	}
}

// TestAnEdgeListenerStartWaitsOutARefusedReadOfTheStream: an edge listener's start reads the stream
// (edgeStreamPresent) inside its credential wait (bus.WithStartCheck), so a grant that refuses the
// read, as a server that has yet to reload the edge's grant refuses it, is waited out under the same
// bound as a refusal of the connect. The server admits the edge's nkey throughout and refuses its
// publish to the stream's info request until it reloads a grant that holds it. The first case
// reloads that grant 20 s after the start's first try, on the start's clock (heldStart): meanwhile
// /healthz answers 503 naming the read's refusal, and then the start completes. The second, its
// control, never grants the read: the listener, run as a process with a credential wait of
// startChildWait on the wall clock, exits 1 at the bound, its last line naming its nkey's public key
// and the read's refusal. Without the wait over the read, the start ends at the first refusal,
// milliseconds after its first try, and the child exits as soon. The third case holds the wait to
// refusals: a stream the server does not hold ends the start at its first try, before any wait.
// TestAnEdgeListenerStartWaitsOutAnAuthorizationRefusalItsReadMeetsAsTheConnectionsLastError has
// the refusal reach the read as the connection's last error, and waits it out too.
func TestAnEdgeListenerStartWaitsOutARefusedReadOfTheStream(t *testing.T) {
	t.Parallel()
	const watchdog = 60 * time.Second
	readRefusal := `Permissions Violation for Publish to "` + streamInfoSubject + `"`
	refusingRead := func(public, fallback string) string {
		return nkeyAuthorization(fallback, nkeyUserDenied(public, streamInfoSubject))
	}

	t.Run("a read the grant refuses and then holds is waited out", func(t *testing.T) {
		t.Parallel()
		const reloadAt = 20 * time.Second
		run := newListenerStartRun(t)
		rig := startEdgeStartServer(t, refusingRead)
		cfg, edge := edgeStartRun(t, rig.uri)
		start, clock := heldStart(t, rig.seed)
		base := serveStartRoutes(t, start)

		run.begin(cfg, edge, start, clock)
		held, waits := run.waitsUntil(t, clock, reloadAt, func(i int, _ *heldWait) {
			if i > 0 {
				return
			}
			if status, fields := probe(t, base, "/healthz"); status != http.StatusServiceUnavailable || !strings.Contains(fmt.Sprint(fields["error"]), readRefusal) {
				t.Fatalf("/healthz answered %d %v while the grant refused the edge listener's read of the stream, want 503 naming %q", status, fields, readRefusal)
			}
		})
		if err := rig.server.Reload(t, nkeyAuthorization(rig.fallback, nkeyUser(rig.public))); err != nil {
			t.Fatalf("reload the server with a grant that holds the edge listener's read: %v", err)
		}
		held.end()

		got, after := run.finish(t, clock, watchdog)
		if got.err != nil {
			t.Fatalf("the edge listener's start returned %v %s after its first try on its clock, want it to complete once the grant holds its read", got.err, got.took)
		}
		if status, fields := probe(t, base, "/healthz"); status != http.StatusOK || fields["status"] != "starting" {
			t.Fatalf("/healthz after the edge listener's start = %d %v, want main's 200 starting", status, fields)
		}
		t.Logf("the start waited %v up to the reload and %v after it, and completed %s after its first try on its clock, having tried %d times", waits, after, got.took, len(listenerTries(t, rig.server)))
	})

	t.Run("control: a read the grant refuses for good ends the listener at the bound", func(t *testing.T) {
		t.Parallel()
		rig := startEdgeStartServer(t, refusingRead)
		broker := ciphertokentest.NewStubBroker(t)
		account := filepath.Join(t.TempDir(), "account-token")
		if err := os.WriteFile(account, []byte("edge-listener-account-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		listener := startListenerChild(t, rig.uri, rig.seed, edgeStartMachine, func(port int) []string {
			return []string{
				config.EdgeProjectsEnvVar + "=" + edgeProject,
				EnvBrokerURL + "=" + broker.Server.URL,
				EnvBrokerAccountTokenFile + "=" + account,
				EnvPublicURL + "=http://127.0.0.1:" + strconv.Itoa(port),
			}
		})
		// The bound, plus the try at the bound: its dial (connectTimeout, 5 s), the connect's read of
		// the deployed subjects, which the grant refuses too (at most firstStreamSubjectsRead, 2 s),
		// and a second of scheduling margin; the refused read of the stream ends at the server's
		// refusal.
		awaitChildExitAtBound(t, listener, rig, watchdog, startChildWait+5*time.Second+2*time.Second+time.Second, readRefusal)
	})

	// Only a refusal of the read is waited out: a stream the server does not hold ends the start at
	// its first try, as it did before the read joined the wait, rather than after the ten tries a
	// second apart that the wait gives any other error of the connect: the start asks for no wait.
	t.Run("a stream the server does not hold ends the start at once", func(t *testing.T) {
		t.Parallel()
		run := newListenerStartRun(t)
		seed, public := testnats.User(t)
		_, fallback := testnats.User(t)
		_, uri := testnats.StartConfig(t, nkeyAuthorization(fallback, nkeyUser(public)), []string{"-js"}, true)
		cfg, edge := edgeStartRun(t, uri)
		start, clock := heldStart(t, seed)
		run.begin(cfg, edge, start, clock)
		w, got := run.next(t, clock)
		if w != nil {
			t.Fatalf("the edge listener's start on a server with no stream asked for a wait of %s at %s on its clock, want it to end at its first try", w.wait, w.at)
		}
		took := got.at.Sub(run.began)
		const want = "the server holds no " + bus.Stream + " stream"
		if got.err == nil || !strings.Contains(got.err.Error(), want) {
			t.Fatalf("the edge listener's start on a server with no stream returned %v after %s, want an error naming %q", got.err, took, want)
		}
		// The connect's read of the deployed subjects can wait out its two seconds
		// (firstStreamSubjectsRead) before the read of the stream; one more second tells that from a
		// start that tried a second time.
		if took > 3*time.Second {
			t.Fatalf("the edge listener's start on a server with no stream ended %s after its first try, want at its first try: %v", took, got.err)
		}
	})
}

// TestAnEdgeListenerStartWaitsOutAnAuthorizationRefusalItsReadMeetsAsTheConnectionsLastError: a
// refusal can reach an edge listener's read of the stream (edgeStreamPresent) as the connection's
// last error rather than as the read's own: the server takes the edge's user away while the read
// waits for an answer it will never get (the user may not subscribe to the edge's inboxes, so the
// server drops the reply), so the read ends on a connection the server refused, whose last error
// is the authorization violation. edgeStreamPresent wraps that error, so the start waits it out as
// it waits out a refusal of the connect: it logs the refusal at WARN and asks its clock for a wait
// (heldStart), and /healthz names that error meanwhile. It completes once the server admits the
// user again. With the last error formatted rather than wrapped, the start ended at that read.
func TestAnEdgeListenerStartWaitsOutAnAuthorizationRefusalItsReadMeetsAsTheConnectionsLastError(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault), where the bus writes the
	// start's WARN of each refusal.
	const watchdog = 60 * time.Second
	var records logtest.Recorder
	logtest.SetDefault(t, &records)
	run := newListenerStartRun(t)
	inboxes := natsgrant.EdgeInboxPrefix(edgeStartMachine) + ".>"
	rig := startEdgeStartServer(t, func(public, fallback string) string {
		return nkeyAuthorization(fallback, fmt.Sprintf("{nkey: %q, permissions: {subscribe: {deny: [%q]}}}", public, inboxes))
	})
	cfg, edge := edgeStartRun(t, rig.uri)
	start, clock := heldStart(t, rig.seed)
	base := serveStartRoutes(t, start)

	run.begin(cfg, edge, start, clock)
	// The connect's read of the deployed subjects has waited out the answer the server dropped, so
	// the start's read of the stream is next, and meets the same silence.
	run.untilLogged(t, &records, clock, "the connect's read of the deployed subjects to end", func(record slog.Record) bool {
		return record.Message == "envoy nats could not read the stream's subjects" && recordAttr(record, "name") == listenerTryName
	})
	if err := rig.server.Reload(t, nkeyAuthorization(rig.fallback)); err != nil {
		t.Fatalf("reload the server without the edge listener's user: %v", err)
	}
	w, got := run.next(t, clock)
	if got != nil {
		t.Fatalf("the edge listener's start returned %v %s after its first try, want it to wait out the read's refusal", got.err, got.at.Sub(run.began))
	}
	// The start logs each refusal, with the wait that follows it, before it waits: the line is
	// there once the start has asked for the wait.
	const lastError = "the connection's last error: nats: authorization violation"
	refusal, logged := slog.Record{}, false
	for _, record := range records.Records() {
		if record.Message == "envoy nats credential refused at start; trying again" && strings.Contains(strings.ToLower(recordAttr(record, "error")), lastError) {
			refusal, logged = record, true
			break
		}
	}
	if !logged {
		t.Fatalf("the start asked for a wait of %s at %s on its clock, but no WARN line says it waits out a refusal naming %q", w.wait, w.at, lastError)
	}
	t.Logf("the start waits out: %s", recordAttr(refusal, "error"))
	if status, fields := probe(t, base, "/healthz"); status != http.StatusServiceUnavailable || !strings.Contains(strings.ToLower(fmt.Sprint(fields["error"])), lastError) {
		t.Fatalf("/healthz answered %d %v while the start waited out a refusal, want 503 naming %q", status, fields, lastError)
	}
	if err := rig.server.Reload(t, nkeyAuthorization(rig.fallback, nkeyUser(rig.public))); err != nil {
		t.Fatalf("reload the server with the edge listener's user: %v", err)
	}
	w.end()

	outcome, _ := run.finish(t, clock, watchdog)
	if outcome.err != nil {
		t.Fatalf("the edge listener's start returned %v %s after its first try on its clock, want it to complete once the server admits it again", outcome.err, outcome.took)
	}
	if status, fields := probe(t, base, "/healthz"); status != http.StatusOK || fields["status"] != "starting" {
		t.Fatalf("/healthz after the edge listener's start = %d %v, want main's 200 starting", status, fields)
	}
}

// rotatingForwarder is the one address the listener is given in
// TestTheListenerStartConnectsOnceTheServersHaveReloaded: it hands each connection it accepts to
// the next of its members in turn, as the hub's Service hands a new connection to one of the
// servers behind it, and relays the two until either ends.
type rotatingForwarder struct {
	listener net.Listener
	members  []string

	mu     sync.Mutex
	next   int
	conns  map[net.Conn]struct{}
	closed bool
}

// startRotatingForwarder forwards to the servers memberURLs name, on a loopback address of its own
// (testport.ListenLoopback), until the test ends.
func startRotatingForwarder(t *testing.T, memberURLs []string) *rotatingForwarder {
	t.Helper()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the forwarder: %v", err)
	}
	forwarder := &rotatingForwarder{listener: listener, conns: map[net.Conn]struct{}{}}
	for _, member := range memberURLs {
		parsed, err := url.Parse(member)
		if err != nil {
			t.Fatalf("read the member URL %q: %v", member, err)
		}
		forwarder.members = append(forwarder.members, parsed.Host)
	}
	go forwarder.serve()
	t.Cleanup(forwarder.close)
	return forwarder
}

// url is the NATS URL of the forwarder.
func (f *rotatingForwarder) url() string { return "nats://" + f.listener.Addr().String() }

// serve hands each connection the forwarder accepts to the next member, until it closes.
func (f *rotatingForwarder) serve() {
	for {
		client, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		member := f.members[f.next%len(f.members)]
		f.next++
		f.mu.Unlock()
		go f.join(client, member)
	}
}

// join dials member for client and relays the two into each other until either ends, or closes
// client at once when the dial fails, as a connection to a server that is down ends.
func (f *rotatingForwarder) join(client net.Conn, member string) {
	upstream, err := net.DialTimeout("tcp", member, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	if !f.track(client, upstream) {
		return
	}
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
		_ = client.Close()
		close(copied)
	}()
	_, _ = io.Copy(client, upstream)
	_ = upstream.Close()
	_ = client.Close()
	<-copied
	f.mu.Lock()
	delete(f.conns, client)
	delete(f.conns, upstream)
	f.mu.Unlock()
}

// track records conns as relayed and reports true, or closes them and reports false once the
// forwarder has closed.
func (f *rotatingForwarder) track(conns ...net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		for _, conn := range conns {
			_ = conn.Close()
		}
		return false
	}
	for _, conn := range conns {
		f.conns[conn] = struct{}{}
	}
	return true
}

// close stops accepting and closes every connection the forwarder relays.
func (f *rotatingForwarder) close() {
	f.mu.Lock()
	f.closed = true
	conns := make([]net.Conn, 0, len(f.conns))
	for conn := range f.conns {
		conns = append(conns, conn)
	}
	f.mu.Unlock()
	_ = f.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// memberReload is one member's reload of an authorization that admits the listener, at an offset
// from the start's first try on the start's clock (heldStart).
type memberReload struct {
	member int
	at     time.Duration
}

// TestTheListenerStartConnectsOnceTheServersHaveReloaded (c-start-cluster): three servers in a
// cluster that advertises no client address (no_advertise), as the hub runs, all refuse the
// listener's nkey, and the listener reaches them through one address that hands each new connection
// to the next of them, as the hub's Service does. The start connects through a server once that
// server has reloaded an authorization that admits the nkey, however many of the others still
// refuse it: with only the third (nats-2, as on 2026-11-22) reloading, through it, within three
// rotations of the forwarder after its reload. Without the wait the start ends after main's ten
// tries, before any server reloads. The start runs on a held clock (heldStart), and each server
// reloads while the start waits across that server's offset on it.
func TestTheListenerStartConnectsOnceTheServersHaveReloaded(t *testing.T) {
	t.Parallel()
	const watchdog = 120 * time.Second
	for _, row := range []struct {
		name    string
		reloads []memberReload
		// throughThird is whether the start must connect through the third server within
		// withinFirst of its reload, the first in reloads.
		throughThird bool
		withinFirst  time.Duration
	}{
		{
			name:         "only the third server reloads",
			reloads:      []memberReload{{member: 2, at: 30 * time.Second}},
			throughThird: true,
			// Three waits of at most 10 s, one before each of the three tries the forwarder hands to
			// each server in turn; a try takes no time on the start's clock.
			withinFirst: 30 * time.Second,
		},
		{
			name:    "the third, the first and the second server reload in turn",
			reloads: []memberReload{{member: 2, at: 30 * time.Second}, {member: 0, at: 50 * time.Second}, {member: 1, at: 70 * time.Second}},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			run := newListenerStartRun(t)
			seed, public := testnats.User(t)
			_, fallback := testnats.User(t)
			cluster := testnats.StartClusterConfig(t, 3, nkeyAuthorization(fallback))
			forwarder := startRotatingForwarder(t, cluster.URLs())
			start, clock := heldStart(t, seed)
			reload := func(reload memberReload) {
				if err := cluster.ReloadMember(t, reload.member, nkeyAuthorization(fallback, nkeyUser(public))); err != nil {
					t.Fatalf("reload server %s with an authorization that admits the listener: %v", cluster.Name(reload.member), err)
				}
			}

			run.begin(config.Service{MachineID: "start-cluster-reload", NATSURLs: []string{forwarder.url()}, NATSReplicas: 1}, nil, start, clock)
			pending := row.reloads
			var got *startOutcome
			var waits []time.Duration
			for got == nil {
				var w *heldWait
				if w, got = run.next(t, clock); got != nil {
					break
				}
				waits = append(waits, w.wait)
				for len(pending) > 0 && w.endsPast(pending[0].at) {
					reload(pending[0])
					pending = pending[1:]
				}
				if w.endsPast(watchdog) {
					t.Fatalf("watchdog: the listener's start had not returned %s after its first try on its clock, having waited %v", watchdog, waits)
				}
				w.end()
			}
			if len(pending) == len(row.reloads) {
				t.Fatalf("the listener's start returned %s after its first try on its clock, before the %s its credential was refused for: error %v", got.took, pending[0].at, got.err)
			}
			// A start that connected before a later reload's offset leaves it to be made now, as the
			// servers reload whatever the start did.
			for _, later := range pending {
				reload(later)
			}
			if got.err != nil {
				t.Fatalf("the listener's start returned %v %s after its first try on its clock, want it to connect once a server admits it", got.err, got.took)
			}
			if _, err := got.client.JS().StreamInfo(bus.Stream); err != nil {
				t.Fatalf("read the %s stream the start reconciled: %v", bus.Stream, err)
			}
			server, after := got.client.Conn.ConnectedServerName(), got.took-row.reloads[0].at
			t.Logf("the start connected through %s, %s after the first reload and %s after its first try on its clock, having waited %v", server, after, got.took, waits)
			if row.throughThird {
				if want := cluster.Name(2); server != want {
					t.Fatalf("the start connected through %s, want %s, the one server that admits it", server, want)
				}
				if after > row.withinFirst {
					t.Fatalf("the start connected %s after the third server reloaded on its clock, want within %s", after, row.withinFirst)
				}
			}
		})
	}
}

// startChildSeedFile names, in the environment of this test binary run again as the listener
// (startListenerChild), the file that holds its nkey seed. testnats's init unsets
// NATS_NKEY_SEED_FILE in every binary that imports it, so the child names it again from here before
// it runs the listener.
const startChildSeedFile = "ENVOY_TEST_LISTENER_START_SEED_FILE"

// startChildWait is that child's credential wait, the bound its exit is timed against.
const startChildWait = 5 * time.Second

// runStartChild is this test binary run as the listener: the listener's main with a credential
// wait of startChildWait, as NATS_NKEY_SEED_FILE seedFile's user. runListener ends the process. The
// log package's lines carry microseconds here (childLineStamp), so the parent reads the instant of
// the child's last line, which it writes as it exits, from the line itself.
func runStartChild(seedFile string) {
	if err := os.Setenv("NATS_NKEY_SEED_FILE", seedFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	runListener(startChildWait)
	os.Exit(2)
}

// childLineStamp is the layout of the date and time a startListenerChild's log package lines begin
// with (runStartChild), in the machine's own zone.
const childLineStamp = "2006/01/02 15:04:05.000000"

// startListenerChild runs this test binary again as the listener of machineID (runStartChild),
// against the NATS at uri as the nkey user whose seed is seed, on a loopback port the test holds
// (startListenerProcess says why), with env(port) added to its environment. The child runs
// TestTheListenerStartEndsAtTheCredentialBound, whose first statement turns it into the listener.
func startListenerChild(t *testing.T, uri, seed, machineID string, env func(port int) []string) *cmdtest.Process {
	t.Helper()
	seedFile := filepath.Join(t.TempDir(), "seed")
	if err := os.WriteFile(seedFile, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	held := cmdtest.HoldPort(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestTheListenerStartEndsAtTheCredentialBound$", "-test.timeout=2m")
	cmd.Env = append([]string{
		"PORT=" + strconv.Itoa(held.Port),
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=" + machineID,
		"NATS_URLS=" + uri,
		startChildSeedFile + "=" + seedFile,
	}, env(held.Port)...)
	held.Pass(cmd, "ENVOY_LISTEN_FD")
	return cmdtest.Start(t, "the listener", cmd)
}

// awaitChildExitAtBound waits, until watchdog, for listener, a startListenerChild whose server
// never admits its start, to exit, and fails the test unless it exited 1, its last line naming its
// nkey's public key and refusal, at an instant between the bound and most after the server took its
// first try. It answers the server's record of the child's tries.
//
// The instant is the child's own: the time its last line, the start's last line written just before
// it exits, carries (childLineStamp), not the moment this test saw it exit, which waits on this
// test's own scheduling and on the reaping of the process. The child counts its bound from the
// moment it began its first try, and the server records that try when it took the connection, up to
// a dial's latency later, so the lower bound is startChildWait less credentialWaitTolerance.
func awaitChildExitAtBound(t *testing.T, listener *cmdtest.Process, rig edgeStartServer, watchdog, most time.Duration, refusal string) []*natsserver.ConnInfo {
	t.Helper()
	select {
	case <-listener.Exited:
	case <-time.After(watchdog):
		t.Fatalf("watchdog: the listener still ran %s after it started, against a server that never admits its start:\n%s", watchdog, listener.Output.String())
	}
	seen := time.Now()
	output := listener.Output.String()
	if code := listener.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("the listener exited %d, want 1:\n%s", code, output)
	}
	tries := listenerTries(t, rig.server)
	if len(tries) == 0 {
		t.Fatalf("the server recorded no try of the listener's start:\n%s", output)
	}
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	last := lines[len(lines)-1]
	if len(last) < len(childLineStamp) {
		t.Fatalf("the listener's last line %q carries no time:\n%s", last, output)
	}
	exited, err := time.ParseInLocation(childLineStamp, last[:len(childLineStamp)], time.Local)
	if err != nil {
		t.Fatalf("read the time of the listener's last line %q: %v\n%s", last, err, output)
	}
	took, least := exited.Sub(tries[0].Start), startChildWait-credentialWaitTolerance
	t.Logf("the listener wrote its last line %s after its first try, having tried %d times; the test saw it exit %s after that line", took, len(tries), seen.Sub(exited))
	if took < least || took > most {
		t.Fatalf("the listener wrote its last line %s after its first try, want between %s and %s:\n%s", took, least, most, output)
	}
	for _, want := range []string{rig.public, refusal} {
		if !strings.Contains(last, want) {
			t.Fatalf("the listener's last line %q does not name %q:\n%s", last, want, output)
		}
	}
	return tries
}

// TestTheListenerStartEndsAtTheCredentialBound (c-start-never): the start's wait has a bound. A
// listener whose credential the server never admits exits 1 once the bound has passed, its last
// line naming its credential's public key and the last refusal; a start whose server is not there
// at all ends after main's ten tries, not at the bound; and the production start's bound is 180 s,
// which main passes to the start (runListener, newNATSStart), whose options connectListenerBus
// hands to the owning connect and, on an edge listener, to bus.Connect, and startRoleLane and
// startSessionDelivery hand to the role lane's and the session consumers' connects:
// bus.WithCredentialWait has one call outside tests, natsStart.options in cmd/listener, and
// bus.WithStartClock none, so every start outside a test waits on the process's clock.
func TestTheListenerStartEndsAtTheCredentialBound(t *testing.T) {
	// Not parallel: it holds the child's waits between tries to credentialWaitTolerance (500 ms)
	// of 1 and 2 s, and a start of ten failed dials to mainsEndMargin (500 ms) past main's connect
	// beside it (requireMainsEnd). Its rows run beside each other alone: the child and the two
	// connects spend their time waiting on timers, and the third row reads source files.
	if seedFile, child := os.LookupEnv(startChildSeedFile); child {
		runStartChild(seedFile)
	}
	const watchdog = 60 * time.Second

	t.Run("a credential the server never admits ends the listener at the bound", func(t *testing.T) {
		t.Parallel()
		var rig edgeStartServer
		rig.seed, rig.public = testnats.User(t)
		_, fallback := testnats.User(t)
		rig.server, rig.uri = testnats.StartConfig(t, nkeyAuthorization(fallback), []string{"-js"}, true)
		listener := startListenerChild(t, rig.uri, rig.seed, "start-bound", func(int) []string {
			return []string{"ENVOY_API_TOKEN=" + listenerTestToken}
		})
		// The bound, plus one try's connectTimeout of 5 s that may run past it, and a second of
		// scheduling margin.
		tries := awaitChildExitAtBound(t, listener, rig, watchdog, startChildWait+5*time.Second+time.Second, authorizationViolation)
		// Its tries came at 0, 1 and 3 s, and the wait of 4 s due after the third was cut to end at
		// the bound, where the fourth and last came: four tries, and not the ten of a start that
		// waits out nothing, which end about as late.
		wantWaits := []time.Duration{time.Second, 2 * time.Second, 2 * time.Second}
		waits := triesWaits(t, tries, len(tries))
		if len(tries) != len(wantWaits)+1 {
			t.Fatalf("the server recorded %d tries of the listener's start, with waits %v between them, want %d with waits %v", len(tries), waits, len(wantWaits)+1, wantWaits)
		}
		for i, want := range wantWaits {
			if gap := waits[i] - want; gap > credentialWaitTolerance || gap < -credentialWaitTolerance {
				t.Fatalf("the server recorded waits %v between the start's tries, want %v, each within %s", waits, wantWaits, credentialWaitTolerance)
			}
		}
	})

	t.Run("a port where nothing listens ends the start after main's ten tries", func(t *testing.T) {
		t.Parallel()
		run := newListenerStartRun(t)
		start := newNATSStart(listenerCredentialWait)
		cfg := config.Service{MachineID: "start-refused-port", NATSURLs: []string{"nats://" + testport.Refused(t)}, NATSReplicas: 1}
		mains := mainsConnect(cfg.NATSURLs)
		run.begin(cfg, nil, start, nil)
		got := run.await(t, watchdog)
		took := got.at.Sub(run.began)
		if got.err == nil {
			t.Fatalf("the listener's start connected to a port where nothing listens, %s after its first try", took)
		}
		// Ten tries, each followed by main's wait of a second, the tenth too, ending where main's
		// connect beside it ends (connectTrying): no sooner, and no later than mainsEndMargin past it.
		requireMainsEnd(t, "the listener's start", took, mains)
		if refusal := start.refusal.Load(); refusal != nil {
			t.Fatalf("the start reported a refused dial as a refusal of its credential: %s", *refusal)
		}
	})

	t.Run("the production start waits 180 s and no other caller waits", func(t *testing.T) {
		t.Parallel()
		if listenerCredentialWait != 180*time.Second {
			t.Fatalf("listenerCredentialWait is %s, want 180s", listenerCredentialWait)
		}
		sources := listenerSources(t)
		// main passes the constant to runListener, and nothing else calls runListener.
		runs := callsTo(sources, "runListener")
		if len(runs) != 1 || runs[0].in.Name.Name != "main" || !identArgument(runs[0].call, 0, "listenerCredentialWait") {
			t.Fatalf("calls of runListener outside tests: %s; want main's one, passing listenerCredentialWait", describeCalls(runs))
		}
		// runListener hands that parameter to the start, and no other start is made.
		starts := callsTo(sources, "newNATSStart")
		if len(starts) != 1 || starts[0].in.Name.Name != "runListener" || !identArgument(starts[0].call, 0, firstParameter(starts[0].in)) {
			t.Fatalf("calls of newNATSStart outside tests: %s; want runListener's one, passing its credential wait parameter", describeCalls(starts))
		}
		// The one wait any program of the module passes to a connect is the listener's own, and no
		// program gives a start a clock of its own (bus.WithStartClock, which only tests pass), so
		// every start waits on the process's clock. The walk must have found each option's own
		// declaration in internal/bus, or it read too little to say that no other file calls it.
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		here, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		busDir := filepath.Join(root, "internal", "bus")
		calls, declared := optionCalls(t, root, "WithCredentialWait")
		if len(declared) != 1 || filepath.Dir(declared[0]) != busDir {
			t.Fatalf("the walk of %s found bus.WithCredentialWait declared in %q, want once, in %s", root, declared, busDir)
		}
		if len(calls) != 1 || filepath.Dir(calls[0]) != here {
			t.Fatalf("calls of bus.WithCredentialWait outside tests: %q; want one, in %s", calls, here)
		}
		calls, declared = optionCalls(t, root, "WithStartClock")
		if len(declared) != 1 || filepath.Dir(declared[0]) != busDir {
			t.Fatalf("the walk of %s found bus.WithStartClock declared in %q, want once, in %s", root, declared, busDir)
		}
		if len(calls) != 0 {
			t.Fatalf("calls of bus.WithStartClock outside tests: %q; want none", calls)
		}
	})
}

// sourceCall is a call in one of cmd/listener's sources and the function it is in.
type sourceCall struct {
	file string
	in   *ast.FuncDecl
	call *ast.CallExpr
}

// listenerSources parses every Go file of cmd/listener that is not a test.
func listenerSources(t *testing.T) map[string]*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]*ast.File{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		sources[name] = file
	}
	return sources
}

// callsTo are the calls of the function name, by its bare name, in sources.
func callsTo(sources map[string]*ast.File, name string) []sourceCall {
	var calls []sourceCall
	for file, source := range sources {
		for _, decl := range source.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == name {
						calls = append(calls, sourceCall{file: file, in: fn, call: call})
					}
				}
				return true
			})
		}
	}
	return calls
}

// identArgument reports whether argument i of call is the bare identifier name.
func identArgument(call *ast.CallExpr, i int, name string) bool {
	if len(call.Args) <= i {
		return false
	}
	ident, ok := call.Args[i].(*ast.Ident)
	return ok && ident.Name == name
}

// firstParameter is the name of fn's first parameter, "" when it has none.
func firstParameter(fn *ast.FuncDecl) string {
	if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 || len(fn.Type.Params.List[0].Names) == 0 {
		return ""
	}
	return fn.Type.Params.List[0].Names[0].Name
}

// describeCalls names each call by its file and the function it is in, for a failure.
func describeCalls(calls []sourceCall) string {
	described := make([]string, 0, len(calls))
	for _, call := range calls {
		described = append(described, fmt.Sprintf("%s in %s", call.file, call.in.Name.Name))
	}
	return "[" + strings.Join(described, ", ") + "]"
}

// optionCalls are the files under root that call the bus option name (bus.WithCredentialWait,
// bus.WithStartClock), once per call, and the files that declare it, among the Go files that are
// not tests. A file that does not spell the name is not parsed.
func optionCalls(t *testing.T, root, name string) (calls, declared []string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(source, []byte(name)) {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				if node.Recv == nil && node.Name.Name == name {
					declared = append(declared, path)
				}
			case *ast.CallExpr:
				switch fun := node.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == name {
						calls = append(calls, path)
					}
				case *ast.Ident:
					if fun.Name == name {
						calls = append(calls, path)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return calls, declared
}
