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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/natsgrant"
)

// The listener's start on NATS (Phase 5: connectListenerBus, which dials and reconciles the stream)
// waits out a refusal of its credential for up to listenerCredentialWait, so a pod that starts
// before the hub's servers have reloaded its new nkey connects once they have, rather than exiting
// and being restarted.

// listenerTryInstance is the instance the listener's start these tests run names its main client
// after (connectListenerBus), and listenerTryName the name that client gives each connection it
// dials, `envoy-listener <instance>`. A server's connection listing (/connz) tells the start's
// tries apart from the test's own connections by it.
const (
	listenerTryInstance = "start-try"
	listenerTryName     = "envoy-listener " + listenerTryInstance
)

// streamInfoSubject is the request the stream's reconcile sends first, which a grant without it
// refuses.
const streamInfoSubject = "$JS.API.STREAM.INFO." + bus.Stream

// authorizationViolation is the server's refusal of a credential it does not know, as nats-server's
// -ERR words it and as the error nats.go returns for it keeps it ("nats: Authorization Violation",
// which errors.Is matches to nats.ErrAuthorization, itself spelled in lower case).
const authorizationViolation = "Authorization Violation"

// credentialWaitTolerance is how far each wait of the start, as the server records it, may stand
// from the wait the start is due to make (c-start-accept).
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

// startOutcome is what one run of the listener's start returned, and when.
type startOutcome struct {
	client *bus.Client
	err    error
	at     time.Time
}

// listenerStartRun is a run of the listener's start, connectListenerBus as each try of main's Phase
// 5 calls it (begin), or the whole of Phase 5's start rule, connectListener (beginStartRule), on a
// goroutine of its own, so the test drives the servers and reads the probe routes meanwhile. began
// stands for the start's first try, which follows it at once.
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

// begin starts the run: one try of the listener's start over cfg (connectListenerBus), as an edge
// listener of edge's grant when edge is not nil, reporting to start.
func (r *listenerStartRun) begin(cfg config.Service, edge *edgeGrant, start *natsStart) {
	r.began = time.Now()
	go func() {
		client, err := connectListenerBus(cfg, edge, listenerTryInstance, start)
		r.outcome <- startOutcome{client: client, err: err, at: time.Now()}
	}()
}

// beginStartRule starts the run as main's Phase 5 makes it: the start rule's tries of the
// listener's start over cfg (connectListener), as an edge listener of edge's grant when edge is
// not nil, which log on logger and return only once a try connects. A try that ends the start ends
// the test binary, as it ends main's process.
func (r *listenerStartRun) beginStartRule(cfg config.Service, edge *edgeGrant, start *natsStart, logger *logging.Logger) {
	r.began = time.Now()
	go func() {
		client := connectListener(cfg, edge, listenerTryInstance, logger, start)
		r.outcome <- startOutcome{client: client, at: time.Now()}
	}()
}

// sleepUntil waits until offset after the run began, or until the start returns, whichever is
// first, and answers the outcome in the second case.
func (r *listenerStartRun) sleepUntil(t *testing.T, offset time.Duration) *startOutcome {
	t.Helper()
	select {
	case got := <-r.outcome:
		r.settle(t, got)
		return &got
	case <-time.After(time.Until(r.began.Add(offset))):
		return nil
	}
}

// refusedUntil waits until offset after the run began, failing the test when the start returns
// first: every case holds the start's credential refused until then.
func (r *listenerStartRun) refusedUntil(t *testing.T, offset time.Duration) {
	t.Helper()
	if got := r.sleepUntil(t, offset); got != nil {
		t.Fatalf("the listener's start returned %s after its first try, before the %s its credential was refused for: error %v", got.at.Sub(r.began), offset, got.err)
	}
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

// awaitRefusedHealthz polls /healthz at base until it answers the start's own 503, "unhealthy"
// naming a refusal of the credential (natsStart.healthz), and fails the test when the start
// returns or offset after the run began passes first. It answers that 503's fields. The 503
// "starting" the listener answers until it can serve (listenerHealth) is not that answer.
func (r *listenerStartRun) awaitRefusedHealthz(t *testing.T, base string, offset time.Duration) map[string]any {
	t.Helper()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		status, fields := probe(t, base, "/healthz")
		if status == http.StatusServiceUnavailable && fields["status"] == "unhealthy" {
			return fields
		}
		if !time.Now().Before(r.began.Add(offset)) {
			t.Fatalf("/healthz answered %d %v up to %s after the start's first try, while the server refused its credential; want 503 unhealthy", status, fields, offset)
		}
		select {
		case got := <-r.outcome:
			r.settle(t, got)
			t.Fatalf("the listener's start returned %s after its first try, while the server refused its credential: error %v", got.at.Sub(r.began), got.err)
		case <-tick.C:
		}
	}
}

// requireStarting fails the test unless /healthz at base answers the listener's 503 "starting",
// which it answers until the pod can serve, naming no refusal (cmd/listener/README.md,
// "Readiness"): main's listener answered 200 "starting" there.
func requireStarting(t *testing.T, base, what string) {
	t.Helper()
	if status, fields := probe(t, base, "/healthz"); status != http.StatusServiceUnavailable || fields["status"] != "starting" {
		t.Fatalf("/healthz %s = %d %v, want 503 starting", what, status, fields)
	}
}

// untilLogged waits until records holds a record matching holds, and answers it, failing the test,
// naming what it waited for, when the start returns first or watchdog after the run began passes.
func (r *listenerStartRun) untilLogged(t *testing.T, records *logtest.Recorder, watchdog time.Duration, what string, holds func(slog.Record) bool) slog.Record {
	t.Helper()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, record := range records.Records() {
			if holds(record) {
				return record
			}
		}
		if !time.Now().Before(r.began.Add(watchdog)) {
			t.Fatalf("watchdog: no log line showed %s within %s of the start's first try", what, watchdog)
		}
		select {
		case got := <-r.outcome:
			r.settle(t, got)
			t.Fatalf("the listener's start returned %s after its first try, before a log line showed %s: error %v", got.at.Sub(r.began), what, got.err)
		case <-tick.C:
		}
	}
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
// server refuses at the connect waits out the refusal with waits of 1, 2, 4 and 8 seconds between
// its first five tries, answers /healthz 503 naming the refusal and /livez 204 meanwhile, and
// connects and reconciles the stream once the server reloads an authorization that admits it, 30 s
// after the start's first try, as the hub's reloader does. The start then returns no error and
// /healthz answers 503 starting, as before any refusal. Without the wait the start ends with the
// refusal after main's ten tries, about ten seconds.
//
// A start whose nkey the server admits while its grant lacks the stream's info is refused at the
// reconcile: that is the grant's refusal, which the start rule keeps up and tries again
// (connectListener), each try logging streamRefusedLine at ERROR with the server's violation, with
// the same waits, /healthz 503 starting and /livez 204, until the reload grants the read.
func TestTheListenerStartWaitsOutACredentialRefusal(t *testing.T) {
	const (
		reloadAt = 30 * time.Second
		watchdog = 90 * time.Second
	)
	wantWaits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for _, row := range []struct {
		name string
		// refusing and admitting are the server's authorization before and after its reload, for
		// the listener's nkey user public and the server's no_auth_user fallback.
		refusing, admitting func(public, fallback string) string
		// refusal is what the refused tries end on, which /healthz names meanwhile, or, for a
		// refusal of the grant, each try's ERROR line.
		refusal string
		// grant says the refusal is the grant's and not the credential's.
		grant bool
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
			grant:     true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newListenerStartRun(t)
			seed, public := testnats.User(t)
			_, fallback := testnats.User(t)
			server, uri := testnats.StartConfig(t, row.refusing(public, fallback), []string{"-js"}, true)
			t.Setenv(bus.NkeySeedEnvVar, seed)
			start := newNATSStart(listenerCredentialWait)
			base := serveStartRoutes(t, start)
			requireStarting(t, base, "before the start")
			cfg := config.Service{MachineID: "start-credential-wait", NATSURLs: []string{uri}, NATSReplicas: 1}

			if row.grant {
				// It follows the README's start rule (cmd/listener/README.md, "Start"): a grant
				// that refuses the stream's reconcile keeps the pod up and is tried again, named at
				// ERROR at each try; main waited it out as a refusal of the credential and then
				// exited.
				var logs safeLog
				run.beginStartRule(cfg, nil, start, logging.NewWithWriter("test", &logs))
				for deadline := run.began.Add(reloadAt); ; time.Sleep(200 * time.Millisecond) {
					refusals := linesIn(logs.String(), streamRefusedLine, map[string]any{"level": "ERROR", "stream": bus.Stream})
					if len(refusals) >= 2 {
						if text, _ := refusals[0]["error"].(string); !strings.Contains(text, row.refusal) || refusals[0]["retry_in"] != "1s" || refusals[1]["retry_in"] != "2s" {
							t.Fatalf("the start's first refusals = %v, want the server's %q, retry_in 1s then 2s", refusals[:2], row.refusal)
						}
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("the start named the grant's refusal %d times at ERROR within %s, want it named at each try:\n%s", len(refusals), reloadAt, logs.String())
					}
				}
				requireStarting(t, base, "while the grant refused the stream's reconcile")
			} else {
				run.begin(cfg, nil, start)
				refused := run.awaitRefusedHealthz(t, base, reloadAt)
				if text, _ := refused["error"].(string); !strings.Contains(text, row.refusal) {
					t.Fatalf("/healthz answered 503 %v while the server refused the start's credential, want an error naming %q", refused, row.refusal)
				}
			}
			if status, _ := probe(t, base, "/livez"); status != http.StatusNoContent {
				t.Fatalf("/livez answered %d while the server refused the start, want 204", status)
			}
			run.refusedUntil(t, reloadAt)
			if err := server.Reload(t, row.admitting(public, fallback)); err != nil {
				t.Fatalf("reload the server with an authorization that admits the listener: %v", err)
			}

			got := run.await(t, watchdog)
			if got.err != nil {
				t.Fatalf("the listener's start returned %v %s after its first try, want it to connect once the server admits it", got.err, got.at.Sub(run.began))
			}
			if _, err := got.client.JS().StreamInfo(bus.Stream); err != nil {
				t.Fatalf("read the %s stream the start reconciled: %v", bus.Stream, err)
			}
			requireStarting(t, base, "after the start")
			tries := listenerTries(t, server)
			waits := triesWaits(t, tries, len(wantWaits)+1)
			t.Logf("the server recorded %d tries; waits between the first five: %v", len(tries), waits)
			for i, want := range wantWaits {
				if gap := waits[i] - want; gap > credentialWaitTolerance || gap < -credentialWaitTolerance {
					t.Fatalf("the server recorded waits %v between the start's first five tries, want %v, each within %s", waits, wantWaits, credentialWaitTolerance)
				}
			}
		})
	}
}

// safeLog is a log writer the start writes to from its own goroutine while the test reads it.
type safeLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *safeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *safeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
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
// and /livez 204, and the server records waits of 1, 2, 4 and 8 s between the start's first five
// tries. The server reloads an authorization that admits the nkey 20 s after the first try, past
// the ten tries a start without the wait makes; the start then connects and finds the stream
// (edgeStreamPresent), and /healthz answers 503 starting, as before any refusal. Without the wait
// the start ends with the refusal after those ten tries, about ten seconds.
func TestAnEdgeListenerStartWaitsOutACredentialRefusal(t *testing.T) {
	const (
		reloadAt = 20 * time.Second
		watchdog = 60 * time.Second
	)
	wantWaits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	run := newListenerStartRun(t)
	rig := startEdgeStartServer(t, func(_, fallback string) string { return nkeyAuthorization(fallback) })
	cfg, edge := edgeStartRun(t, rig.uri)
	t.Setenv(bus.NkeySeedEnvVar, rig.seed)
	start := newNATSStart(listenerCredentialWait)
	base := serveStartRoutes(t, start)

	run.begin(cfg, edge, start)
	refused := run.awaitRefusedHealthz(t, base, reloadAt)
	if text, _ := refused["error"].(string); !strings.Contains(text, authorizationViolation) {
		t.Fatalf("/healthz answered 503 %v while the server refused the edge listener's nkey, want an error naming %q", refused, authorizationViolation)
	}
	if status, _ := probe(t, base, "/livez"); status != http.StatusNoContent {
		t.Fatalf("/livez answered %d while the server refused the edge listener's nkey, want 204", status)
	}
	run.refusedUntil(t, reloadAt)
	if err := rig.server.Reload(t, nkeyAuthorization(rig.fallback, nkeyUser(rig.public))); err != nil {
		t.Fatalf("reload the server with an authorization that admits the edge listener: %v", err)
	}

	got := run.await(t, watchdog)
	if got.err != nil {
		t.Fatalf("the edge listener's start returned %v %s after its first try, want it to connect once the server admits it", got.err, got.at.Sub(run.began))
	}
	requireStarting(t, base, "after the edge listener's start")
	tries := listenerTries(t, rig.server)
	waits := triesWaits(t, tries, len(wantWaits)+1)
	t.Logf("the server recorded %d tries; waits between the first five: %v", len(tries), waits)
	for i, want := range wantWaits {
		if gap := waits[i] - want; gap > credentialWaitTolerance || gap < -credentialWaitTolerance {
			t.Fatalf("the server recorded waits %v between the edge listener's first five tries, want %v, each within %s", waits, wantWaits, credentialWaitTolerance)
		}
	}
}

// TestAnEdgeListenerStartWaitsOutARefusedReadOfTheStream: an edge listener's start reads the stream
// (edgeStreamPresent) inside its credential wait (bus.WithStartCheck), so a grant that refuses the
// read, as a server that has yet to reload the edge's grant refuses it, is waited out under the same
// bound as a refusal of the connect, and past that bound the start rule tries it again for as long
// as it lasts (connectListener, waitOutGrantRefusal), since only the grant lifts it. The server
// admits the edge's nkey throughout and refuses its publish to the stream's info request until it
// reloads a grant that holds it. The first case reloads that grant 20 s after the start's first
// try: meanwhile /healthz answers 503 naming the read's refusal, and then the start completes. The
// second never grants the read: the listener, run as a process with a credential wait of
// startChildWait, stays up and not ready past that bound and past the instant it exited at under
// the former rule, /livez 204 and /healthz 503, and logs streamRefusedLine at ERROR with the
// read's refusal at each try after the bound; under the former rule it exited 1 at the bound. The
// third has the server hold no stream: the start rule logs edgeStreamMissingLine at ERROR at each
// try, 1 s and then 2 s apart, with /healthz 503 starting and /livez 204, and completes once a
// program whose grant may create the stream has created it; under the former rule the start ended
// at its first try. The fourth has the refusal reach the read as the connection's last error, an
// authorization violation, rather than as the read's own error, and waits it out too.
func TestAnEdgeListenerStartWaitsOutARefusedReadOfTheStream(t *testing.T) {
	const watchdog = 60 * time.Second
	readRefusal := `Permissions Violation for Publish to "` + streamInfoSubject + `"`
	refusingRead := func(public, fallback string) string {
		return nkeyAuthorization(fallback, nkeyUserDenied(public, streamInfoSubject))
	}

	t.Run("a read the grant refuses and then holds is waited out", func(t *testing.T) {
		const reloadAt = 20 * time.Second
		run := newListenerStartRun(t)
		rig := startEdgeStartServer(t, refusingRead)
		cfg, edge := edgeStartRun(t, rig.uri)
		t.Setenv(bus.NkeySeedEnvVar, rig.seed)
		start := newNATSStart(listenerCredentialWait)
		base := serveStartRoutes(t, start)

		run.begin(cfg, edge, start)
		refused := run.awaitRefusedHealthz(t, base, reloadAt)
		if text, _ := refused["error"].(string); !strings.Contains(text, readRefusal) {
			t.Fatalf("/healthz answered 503 %v while the grant refused the edge listener's read of the stream, want an error naming %q", refused, readRefusal)
		}
		run.refusedUntil(t, reloadAt)
		if err := rig.server.Reload(t, nkeyAuthorization(rig.fallback, nkeyUser(rig.public))); err != nil {
			t.Fatalf("reload the server with a grant that holds the edge listener's read: %v", err)
		}

		got := run.await(t, watchdog)
		if got.err != nil {
			t.Fatalf("the edge listener's start returned %v %s after its first try, want it to complete once the grant holds its read", got.err, got.at.Sub(run.began))
		}
		requireStarting(t, base, "after the edge listener's start")
		t.Logf("the start completed %s after its first try, having tried %d times", got.at.Sub(run.began), len(listenerTries(t, rig.server)))
	})

	t.Run("a read the grant refuses for good keeps the listener up and not ready past the bound", func(t *testing.T) {
		rig := startEdgeStartServer(t, refusingRead)
		broker := ciphertokentest.NewStubBroker(t)
		account := filepath.Join(t.TempDir(), "account-token")
		if err := os.WriteFile(account, []byte("edge-listener-account-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		var port int
		child := startListenerChild(t, rig.uri, rig.seed, edgeStartMachine, func(held int) []string {
			port = held
			return []string{
				config.EdgeProjectsEnvVar + "=" + edgeProject,
				EnvBrokerURL + "=" + broker.Server.URL,
				EnvBrokerAccountTokenFile + "=" + account,
				EnvPublicURL + "=http://127.0.0.1:" + strconv.Itoa(held),
			}
		})
		listener := &listenerProcess{Process: child, port: port}
		// The bound, plus the try at the bound: its dial (connectTimeout, 5 s), the connect's read of
		// the deployed subjects, which the grant refuses too (at most firstStreamSubjectsRead, 2 s),
		// and a second of scheduling margin. That is the instant the former rule had the listener
		// exit by, and the tries after it come at most 2 s and then 4 s apart.
		pastBound := time.Now().Add(startChildWait + 5*time.Second + 2*time.Second + time.Second)
		refusals := func() []map[string]any {
			return linesIn(child.Output.String(), streamRefusedLine, map[string]any{"level": "ERROR", "stream": bus.Stream, "grant": edgeListenerGrant})
		}
		for deadline := time.Now().Add(watchdog); time.Now().Before(pastBound) || len(refusals()) < 2; time.Sleep(200 * time.Millisecond) {
			select {
			case <-child.Exited:
				t.Fatalf("the edge listener exited with %s while the grant refused its read of the stream; its last line was %q:\n%s",
					child.Cmd.ProcessState, lastOutputLine(child.Output.String()), child.Output.String())
			default:
			}
			if status, body := callListener(t, port, http.MethodGet, "/healthz", ""); status != http.StatusServiceUnavailable {
				t.Fatalf("/healthz while the grant refused the edge listener's read of the stream = %d %s, want 503", status, body)
			}
			if status, body := callListener(t, port, http.MethodGet, "/livez", ""); status != http.StatusNoContent {
				t.Fatalf("/livez while the grant refused the edge listener's read of the stream = %d %s, want 204", status, body)
			}
			if time.Now().After(deadline) {
				t.Fatalf("watchdog: the edge listener logged %d of the two %q it owes within %s:\n%s", len(refusals()), streamRefusedLine, watchdog, child.Output.String())
			}
		}
		for _, refusal := range refusals() {
			if text, _ := refusal["error"].(string); !strings.Contains(text, readRefusal) || refusal["subject"] != streamInfoSubject || refusal["retry_in"] == nil {
				t.Fatalf("the edge listener's refusal line %v, want the read's refusal %q, the subject %q and retry_in", refusal, readRefusal, streamInfoSubject)
			}
		}
		requireLiveAndStarting(t, listener, "the grant refused the edge listener's read of the stream past the credential wait")
		t.Logf("the edge listener still runs past the bound, having logged %d refusals of its read", len(refusals()))
	})

	// A stream the server does not hold is no refusal of the credential, so the credential wait ends
	// the try at once, and the start rule tries the read again for as long as the stream is missing:
	// an edge listener may not create it, and a shared listener or Dispatch, whose grants may, does.
	t.Run("a stream the server does not hold is read again until a program creates it", func(t *testing.T) {
		var logs safeLog
		run := newListenerStartRun(t)
		seed, public := testnats.User(t)
		_, fallback := testnats.User(t)
		_, uri := testnats.StartConfig(t, nkeyAuthorization(fallback, nkeyUser(public)), []string{"-js"}, true)
		cfg, edge := edgeStartRun(t, uri)
		t.Setenv(bus.NkeySeedEnvVar, seed)
		start := newNATSStart(listenerCredentialWait)
		base := serveStartRoutes(t, start)
		missing := func() []map[string]any {
			return linesIn(logs.String(), edgeStreamMissingLine, map[string]any{"level": "ERROR", "stream": bus.Stream})
		}

		run.beginStartRule(cfg, edge, start, logging.NewWithWriter("test", &logs))
		for len(missing()) < 2 {
			if got := run.sleepUntil(t, time.Since(run.began)+200*time.Millisecond); got != nil {
				t.Fatalf("the edge listener's start on a server with no stream returned %s after its first try, want it to go on reading the stream:\n%s", got.at.Sub(run.began), logs.String())
			}
			if time.Since(run.began) > watchdog {
				t.Fatalf("watchdog: the edge listener's start logged %d of the two %q it owes within %s:\n%s", len(missing()), edgeStreamMissingLine, watchdog, logs.String())
			}
		}
		const want = "the server holds no " + bus.Stream + " stream"
		if lines := missing(); lines[0]["retry_in"] != "1s" || lines[1]["retry_in"] != "2s" || !strings.Contains(fmt.Sprint(lines[0]["error"]), want) {
			t.Fatalf("the edge listener's first lines for the missing stream = %v, want the read's error naming %q, retry_in 1s then 2s", lines[:2], want)
		}
		requireStarting(t, base, "while the server held no stream")
		if status, _ := probe(t, base, "/livez"); status != http.StatusNoContent {
			t.Fatalf("/livez answered %d while the server held no stream, want 204", status)
		}

		owner, err := bus.ConnectOwningStream([]string{uri}, bus.WithName("stream-owner"))
		if err != nil {
			t.Fatalf("create the %s stream, as a shared listener or Dispatch does: %v", bus.Stream, err)
		}
		t.Cleanup(owner.Close)
		got := run.await(t, watchdog)
		if got.client == nil {
			t.Fatalf("the edge listener's start returned no client %s after its first try, once the stream was created", got.at.Sub(run.began))
		}
		requireStarting(t, base, "after the edge listener's start")
		t.Logf("the start completed %s after its first try, having logged %d lines for the missing stream", got.at.Sub(run.began), len(missing()))
	})

	// A refusal can reach the read as the connection's last error rather than as the read's own:
	// the server takes the edge's user away while the read waits for an answer it will never get
	// (the user may not subscribe to the edge's inboxes, so the server drops the reply), so the read
	// ends on a connection the server refused, whose last error is the authorization violation.
	// edgeStreamPresent wraps that error, so the start waits it out as it waits out a refusal of the
	// connect, and completes once the server admits the user again. With the last error formatted
	// rather than wrapped, the start ended at that read.
	t.Run("an authorization refusal the read meets as the connection's last error is waited out", func(t *testing.T) {
		var records logtest.Recorder
		logtest.SetDefault(t, &records)
		run := newListenerStartRun(t)
		inboxes := natsgrant.EdgeInboxPrefix(edgeStartMachine) + ".>"
		rig := startEdgeStartServer(t, func(public, fallback string) string {
			return nkeyAuthorization(fallback, fmt.Sprintf("{nkey: %q, permissions: {subscribe: {deny: [%q]}}}", public, inboxes))
		})
		cfg, edge := edgeStartRun(t, rig.uri)
		t.Setenv(bus.NkeySeedEnvVar, rig.seed)
		start := newNATSStart(listenerCredentialWait)
		base := serveStartRoutes(t, start)

		run.begin(cfg, edge, start)
		// The connect's read of the deployed subjects has waited out the answer the server dropped,
		// so the start's read of the stream is next, and meets the same silence.
		run.untilLogged(t, &records, watchdog, "the connect's read of the deployed subjects to end", func(record slog.Record) bool {
			return record.Message == "envoy nats could not read the stream's subjects" && recordAttr(record, "name") == listenerTryName
		})
		if err := rig.server.Reload(t, nkeyAuthorization(rig.fallback)); err != nil {
			t.Fatalf("reload the server without the edge listener's user: %v", err)
		}
		const lastError = "the connection's last error: nats: authorization violation"
		refusal := run.untilLogged(t, &records, watchdog, "the start to wait out the read's refusal through the connection's last error", func(record slog.Record) bool {
			return record.Message == "envoy nats credential refused at start; trying again" && strings.Contains(strings.ToLower(recordAttr(record, "error")), lastError)
		})
		t.Logf("the start waits out: %s", recordAttr(refusal, "error"))
		if status, fields := probe(t, base, "/healthz"); status != http.StatusServiceUnavailable {
			t.Fatalf("/healthz answered %d %v while the start waited out a refusal, want 503", status, fields)
		}
		if err := rig.server.Reload(t, nkeyAuthorization(rig.fallback, nkeyUser(rig.public))); err != nil {
			t.Fatalf("reload the server with the edge listener's user: %v", err)
		}

		got := run.await(t, watchdog)
		if got.err != nil {
			t.Fatalf("the edge listener's start returned %v %s after its first try, want it to complete once the server admits it again", got.err, got.at.Sub(run.began))
		}
		requireStarting(t, base, "after the edge listener's start")
	})
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
// from the start's first try.
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
// tries, before any server reloads.
func TestTheListenerStartConnectsOnceTheServersHaveReloaded(t *testing.T) {
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
			// Three waits of at most 10 s, one connectTimeout of 5 s, and 10 s for the refused tries
			// and the reconcile.
			withinFirst: 45 * time.Second,
		},
		{
			name:    "the third, the first and the second server reload in turn",
			reloads: []memberReload{{member: 2, at: 30 * time.Second}, {member: 0, at: 50 * time.Second}, {member: 1, at: 70 * time.Second}},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newListenerStartRun(t)
			seed, public := testnats.User(t)
			_, fallback := testnats.User(t)
			cluster := testnats.StartClusterConfig(t, 3, nkeyAuthorization(fallback))
			forwarder := startRotatingForwarder(t, cluster.URLs())
			t.Setenv(bus.NkeySeedEnvVar, seed)
			start := newNATSStart(listenerCredentialWait)

			run.begin(config.Service{MachineID: "start-cluster-reload", NATSURLs: []string{forwarder.url()}, NATSReplicas: 1}, nil, start)
			var connected *startOutcome
			var firstReload time.Time
			for i, reload := range row.reloads {
				if i == 0 {
					run.refusedUntil(t, reload.at)
				} else if connected == nil {
					connected = run.sleepUntil(t, reload.at)
				}
				if err := cluster.ReloadMember(t, reload.member, nkeyAuthorization(fallback, nkeyUser(public))); err != nil {
					t.Fatalf("reload server %s with an authorization that admits the listener: %v", cluster.Name(reload.member), err)
				}
				if i == 0 {
					firstReload = time.Now()
				}
			}
			got := connected
			if got == nil {
				outcome := run.await(t, watchdog)
				got = &outcome
			}
			if got.err != nil {
				t.Fatalf("the listener's start returned %v %s after its first try, want it to connect once a server admits it", got.err, got.at.Sub(run.began))
			}
			if _, err := got.client.JS().StreamInfo(bus.Stream); err != nil {
				t.Fatalf("read the %s stream the start reconciled: %v", bus.Stream, err)
			}
			server, after := got.client.Conn.ConnectedServerName(), got.at.Sub(firstReload)
			t.Logf("the start connected through %s, %s after the first reload and %s after its first try", server, after, got.at.Sub(run.began))
			if row.throughThird {
				if want := cluster.Name(2); server != want {
					t.Fatalf("the start connected through %s, want %s, the one server that admits it", server, want)
				}
				if after > row.withinFirst {
					t.Fatalf("the start connected %s after the third server reloaded, want within %s", after, row.withinFirst)
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
		// The instance names the child's main client (connectListenerBus), so the server's record
		// of its tries (listenerTries) finds them by listenerTryName.
		"ENVOY_INSTANCE_ID=" + listenerTryInstance,
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
// which main passes to the start (runListener, newNATSStart), whose option connectListenerBus hands
// to the owning connect and, on an edge listener, to bus.Connect, and startRoleLane and
// startSessionDelivery hand to the role lane's and the session consumers' connects:
// bus.WithCredentialWait has one call outside tests, natsStart.option in cmd/listener.
func TestTheListenerStartEndsAtTheCredentialBound(t *testing.T) {
	if seedFile, child := os.LookupEnv(startChildSeedFile); child {
		runStartChild(seedFile)
	}
	const watchdog = 60 * time.Second

	t.Run("a credential the server never admits ends the listener at the bound", func(t *testing.T) {
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
		run := newListenerStartRun(t)
		start := newNATSStart(listenerCredentialWait)
		cfg := config.Service{MachineID: "start-refused-port", NATSURLs: []string{"nats://" + testport.Refused(t)}, NATSReplicas: 1}
		mains := mainsConnect(cfg.NATSURLs)
		run.begin(cfg, nil, start)
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
		// The one wait any program of the module passes to a connect is the listener's own. The walk
		// must have found the option's own declaration in internal/bus, or it read too little to
		// say that no other file calls it.
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		here, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		calls, declared := credentialWaitCalls(t, root)
		if want := filepath.Join(root, "internal", "bus"); len(declared) != 1 || filepath.Dir(declared[0]) != want {
			t.Fatalf("the walk of %s found bus.WithCredentialWait declared in %q, want once, in %s", root, declared, want)
		}
		if len(calls) != 1 || filepath.Dir(calls[0]) != here {
			t.Fatalf("calls of bus.WithCredentialWait outside tests: %q; want one, in %s", calls, here)
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

// credentialWaitCalls are the files under root that call bus.WithCredentialWait, once per call, and
// the files that declare it, among the Go files that are not tests. A file that does not spell the
// name is not parsed.
func credentialWaitCalls(t *testing.T, root string) (calls, declared []string) {
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
		if !bytes.Contains(source, []byte("WithCredentialWait")) {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				if node.Recv == nil && node.Name.Name == "WithCredentialWait" {
					declared = append(declared, path)
				}
			case *ast.CallExpr:
				switch fun := node.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "WithCredentialWait" {
						calls = append(calls, path)
					}
				case *ast.Ident:
					if fun.Name == "WithCredentialWait" {
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
