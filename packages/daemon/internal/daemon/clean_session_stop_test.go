package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// storedRegistration is what the fake listener's registry holds for the session under test: the
// client process whose registration it is — empty for an entry a client predating that field
// wrote — and the moment that process last wrote it, which the listener stamps on its own clock
// at every write. absent is a registry that holds no record of the session at all.
type storedRegistration struct {
	absent           bool
	clientInstanceID string
	lastSeen         time.Time
}

// askedRoute is what one of the listener's routes was asked: the method, path, query and bearer
// of the last request to reach it, and how many reached it at all, so a test can show that one
// stop is one request and that nothing is sent twice.
type askedRoute struct {
	method, path, query, bearer string
	requests                    int
}

// listenerRecorder is the pair of routes one clean stop uses — the registry read it makes first
// (GET /v1/registry/<session_id>) and the session delete it then sends (DELETE
// /v1/sessions/<session_id>) — keeping what each was asked and answering as the real listener
// does. held is the registration both routes work on, and relaunched, when a test sets it,
// replaces it the moment the read has been answered: a claim relaunching under the same session
// id while the stop is in flight. register is that relaunch at the moment a test's own runtime
// picks: the process it started registering the session. The delete is judged as
// deleteSessionHandler judges it (packages/envoy/cmd/listener/api.go), under both conditions at
// the moment it lands, and entryRemoved says whether the registration went; one it removed is
// held no longer. readStatus is a registry route that refuses the read instead of answering it,
// and heldRead one that answers only once the test closes it (or the request's own context ends
// first): a read still in flight when the daemon's stop begins, or, never closed, a read held
// past the stop's own budget. heldDelete is a delete route that judges the delete only once the
// test closes it, whatever became of the request meanwhile: a delete still on its way to the
// listener, which lands after the daemon has stopped waiting for it. order is what happened at
// the listener, in the order it happened: each read answered, each registration a process made
// (register), and each delete judged.
type listenerRecorder struct {
	// mu guards what the routes and register read and write, since a stop's requests and a
	// relaunch's registration reach the recorder on goroutines of their own. A test reads the
	// fields directly once the request it waited for has been answered.
	mu           sync.Mutex
	held         storedRegistration
	relaunched   *storedRegistration
	readStatus   int
	heldRead     chan struct{}
	heldDelete   chan struct{}
	read, delete askedRoute
	entryRemoved bool
	order        []string
	// arrived is closed when the first request of a stop reaches the listener, deleteArrived when
	// its delete does, and settled once a delete has been judged: a stop runs on a goroutine, so a
	// test waits on these rather than on the call it made.
	arrived, deleteArrived, settled chan struct{}
	// readReleased and deleteReleased close heldRead and heldDelete once each, so a test that
	// fails before it lets them go still lets them go when it ends (releaseHeld).
	readReleased, deleteReleased sync.Once
}

// register is a process registering the session under test: the listener holds its
// registration from now on.
func (l *listenerRecorder) register(registration storedRegistration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held = registration
	l.order = append(l.order, "registered "+registration.clientInstanceID)
}

// note records event in what happened at the listener, at the moment a test reaches it, so the
// order a test checks can place a step of its own among the listener's.
func (l *listenerRecorder) note(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, event)
}

// releaseRead lets the held read be answered, and releaseDelete lets the held delete be judged;
// releaseHeld lets both go, for a test that ends before it did.
func (l *listenerRecorder) releaseRead() {
	if l.heldRead != nil {
		l.readReleased.Do(func() { close(l.heldRead) })
	}
}

func (l *listenerRecorder) releaseDelete() {
	if l.heldDelete != nil {
		l.deleteReleased.Do(func() { close(l.heldDelete) })
	}
}

func (l *listenerRecorder) releaseHeld() {
	l.releaseRead()
	l.releaseDelete()
}

// happened is what has happened at the listener so far, in order.
func (l *listenerRecorder) happened() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.order)
}

// holding is the registration the listener holds now.
func (l *listenerRecorder) holding() storedRegistration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

func (l *listenerRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		asked := &l.read
		if r.Method == http.MethodDelete {
			asked = &l.delete
		}
		asked.method, asked.path, asked.query, asked.bearer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		asked.requests++
		first := l.read.requests+l.delete.requests == 1
		l.mu.Unlock()
		if l.arrived != nil && first {
			close(l.arrived)
		}
		if r.Method != http.MethodDelete {
			l.answerRead(w, r)
			return
		}
		if l.deleteArrived != nil {
			close(l.deleteArrived)
		}
		if l.heldDelete != nil {
			<-l.heldDelete
		}
		l.answerDelete(w, r)
		if l.settled != nil {
			close(l.settled)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// answerRead is the registry route: it answers the held registration, 404 for a registry that
// holds none, and leaves out client_instance_id for an entry that names no process, as the real
// route's registration view does (listenerwire.SessionRegistration). A
// relaunch the test staged takes the entry's place once this answer is written, so the delete
// that follows meets what the relaunch wrote and not what the read reported.
func (l *listenerRecorder) answerRead(w http.ResponseWriter, r *http.Request) {
	if l.heldRead != nil {
		select {
		case <-l.heldRead:
		case <-r.Context().Done():
			return
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.readStatus != 0 {
		w.WriteHeader(l.readStatus)
		_, _ = io.WriteString(w, `{"error":"the session registry did not answer"}`)
		return
	}
	defer func() {
		if l.relaunched != nil {
			l.held = *l.relaunched
		}
	}()
	session := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
	if l.held.absent {
		l.order = append(l.order, "read answered none")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"error":"no live session %s"}`, session)
		return
	}
	l.order = append(l.order, "read answered "+l.held.clientInstanceID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if l.held.clientInstanceID == "" {
		_, _ = fmt.Fprintf(w, `{"session_id":%q,"last_seen":%d}`, session, l.held.lastSeen.UnixMilli())
		return
	}
	_, _ = fmt.Fprintf(w, `{"session_id":%q,"client_instance_id":%q,"last_seen":%d}`,
		session, l.held.clientInstanceID, l.held.lastSeen.UnixMilli())
}

// answerDelete judges the delete as the listener does: an id naming a client instance other than
// the stored one keeps the registration, so does a registration written after the moment the
// delete belongs to, and otherwise the entry goes. Both answers are 200, since the caller has
// nothing to retry either way.
func (l *listenerRecorder) answerDelete(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, "delete judged")
	conditions, _ := url.ParseQuery(r.URL.RawQuery)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if named := conditions.Get("client_instance_id"); named != "" && l.held.clientInstanceID != "" && named != l.held.clientInstanceID {
		_, _ = io.WriteString(w, `{"deleted":false,"reason":"the registration belongs to another client instance"}`)
		return
	}
	millis, _ := strconv.ParseInt(conditions.Get("if_not_registered_after"), 10, 64)
	if millis > 0 && l.held.lastSeen.UnixMilli() > millis {
		_, _ = io.WriteString(w, `{"deleted":false,"reason":"the registration was written after the moment the delete belongs to"}`)
		return
	}
	l.entryRemoved = true
	l.held = storedRegistration{absent: true}
	_, _ = io.WriteString(w, `{"deleted":true}`)
}

// deleteConditions is what the stop's delete named, read out of the query the fake recorded, so
// a test here asserts the conditions the delete holds under and leaves the query's own spelling
// — the parameter order and the escaping — to the one package that writes it
// (packages/daemon/internal/notify's session_test.go).
func (l *listenerRecorder) deleteConditions(t *testing.T) url.Values {
	t.Helper()
	conditions, err := url.ParseQuery(l.delete.query)
	if err != nil {
		t.Fatalf("parse the delete's query %q: %v", l.delete.query, err)
	}
	return conditions
}

func bufferedLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

// The registration of the process the probe judged gone is the one the stop must remove, and the
// listener stamps that registration's last write with its own clock: a dead pane's last
// heartbeat lands moments before the probe says the process is gone. Here it lands 500 ms
// before, and the stop deletes it. A stop that dated its moment before the probe's by any
// margin wider than that gap would read this heartbeat as a registration written since and
// leave it standing, which is the whole of the defect (dispatch://INFRA-2064).
func TestCleanSessionStopDeletesARegistrationWrittenBeforeTheProbesMoment(t *testing.T) {
	listener := &listenerRecorder{
		held:    storedRegistration{clientInstanceID: "inst_dead", lastSeen: time.Now().Add(-500 * time.Millisecond)},
		settled: make(chan struct{}),
	}
	server := listener.server(t)
	var inFlight sessionStopsInFlight
	defer inFlight.wait()
	stop := cleanSessionStop(&inFlight, server.URL, "envoy-token", sessionStopOverrides{}, quietLogger())

	stop("ses_dead")

	select {
	case <-listener.settled:
	case <-time.After(5 * time.Second):
		t.Fatal("the clean stop never reached the listener's session route")
	}
	if !listener.entryRemoved {
		t.Fatalf("the listener kept the registration asked for with %q, want the dead process's last heartbeat deleted rather than taken for a successor's",
			listener.delete.query)
	}
}

// The stop is one DELETE of the dead session's key, with the listener's bearer, and it names both
// of its conditions: the client instance the registry answered for that session, and the moment
// the probe judged the process gone. One read, one delete, and nothing sent twice.
func TestCleanSessionStopNamesTheEntrysInstanceAndTheProbesMoment(t *testing.T) {
	listener := &listenerRecorder{held: storedRegistration{clientInstanceID: "inst_dead", lastSeen: time.Now().Add(-time.Second)}}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}
	goneAt := time.Now()

	stop.send("ses_dead", goneAt)

	if listener.read.method != http.MethodGet || listener.read.path != "/v1/registry/ses_dead" || listener.read.requests != 1 {
		t.Fatalf("the registry was asked %s %s %d times, want one read of the dead session's entry",
			listener.read.method, listener.read.path, listener.read.requests)
	}
	if listener.delete.method != http.MethodDelete || listener.delete.path != "/v1/sessions/ses_dead" ||
		listener.delete.bearer != "Bearer envoy-token" || listener.delete.requests != 1 {
		t.Fatalf("the listener was asked %s %s with %q %d times, want one DELETE of the session's key with the bearer",
			listener.delete.method, listener.delete.path, listener.delete.bearer, listener.delete.requests)
	}
	conditions := listener.deleteConditions(t)
	if got := conditions.Get("client_instance_id"); got != "inst_dead" {
		t.Fatalf("the delete named client instance %q, want the one the registry answered, inst_dead", got)
	}
	if got := conditions.Get("if_not_registered_after"); got != strconv.FormatInt(goneAt.UnixMilli(), 10) {
		t.Fatalf("the delete named the moment %q, want the probe's own, %d", got, goneAt.UnixMilli())
	}
	if !strings.Contains(logged.String(), "the Envoy session of a role whose process is gone was stopped") {
		t.Fatalf("logged %q, want the stop reported done", logged.String())
	}
}

// The race the instance id is read for: the claim relaunched under the same session id and the
// new process registered between the read and the delete, so the delete named the dead process
// and the listener kept the live registration it found instead. The stop is still sent — only
// the listener can tell what is stored when it lands — and what it reports is that the
// registration stayed, with the listener's own reason, not that the session was stopped.
func TestCleanSessionStopReportsARelaunchsRegistrationTheListenerKept(t *testing.T) {
	listener := &listenerRecorder{
		held:       storedRegistration{clientInstanceID: "inst_dead", lastSeen: time.Now().Add(-time.Second)},
		relaunched: &storedRegistration{clientInstanceID: "inst_relaunched", lastSeen: time.Now()},
	}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}

	stop.send("ses_relaunched", time.Now())

	if listener.entryRemoved {
		t.Fatal("the relaunch's registration was deleted by a stop that belonged to the process it replaced")
	}
	if listener.delete.requests != 1 {
		t.Fatalf("the listener was asked %d deletes, want the one stop, judged there", listener.delete.requests)
	}
	if !strings.Contains(logged.String(), "the Envoy listener kept the registration of a dead role's session") {
		t.Fatalf("logged %q, want the kept registration reported", logged.String())
	}
	if strings.Contains(logged.String(), "the Envoy session of a role whose process is gone was stopped") {
		t.Fatalf("logged %q, want no claim that a registration the listener kept was stopped", logged.String())
	}
}

// What the moment keeps on its own: an entry the read hands back is newer than the stop, so the
// delete names that entry's own instance id and the id refuses nothing. The daemon starts no
// relaunch while its stop is in flight (supervise.Machine.awaitSessionStops, and
// TestARelaunchIsNotRemovedByTheStopBeforeItWhenTheListenersClockIsBehind for a listener clock
// that runs behind), so a relaunch of its own never registers before the read; on clocks that
// agree the moment still keeps an entry that did, since the listener stamped it after the moment.
// So the listener keeps the registration and the stop reports it kept, with the listener's own
// reason, and the session stays reachable.
func TestCleanSessionStopKeepsARelaunchThatRegisteredBeforeItsRead(t *testing.T) {
	goneAt := time.Now()
	listener := &listenerRecorder{held: storedRegistration{clientInstanceID: "inst_relaunched", lastSeen: goneAt.Add(2 * time.Second)}}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}

	stop.send("ses_released", goneAt)

	if listener.entryRemoved {
		t.Fatalf("the relaunch's registration was removed by the delete %q, which belonged to the process before it", listener.delete.query)
	}
	if listener.delete.requests != 1 {
		t.Fatalf("the listener was asked %d deletes, want the one stop, judged there", listener.delete.requests)
	}
	conditions := listener.deleteConditions(t)
	if got := conditions.Get("client_instance_id"); got != "inst_relaunched" {
		t.Fatalf("the delete named client instance %q, want the one the read answered, inst_relaunched", got)
	}
	if got := conditions.Get("if_not_registered_after"); got != strconv.FormatInt(goneAt.UnixMilli(), 10) {
		t.Fatalf("the delete named the moment %q, want the stop's own, %d", got, goneAt.UnixMilli())
	}
	kept := slices.IndexFunc(strings.Split(logged.String(), "\n"), func(line string) bool {
		return strings.Contains(line, "the Envoy listener kept the registration of a dead role's session") &&
			strings.Contains(line, "the registration was written after the moment the delete belongs to")
	})
	if kept < 0 {
		t.Fatalf("logged %q, want the registration reported kept for being written after the moment", logged.String())
	}
}

// The listener keeps a registration for three separate reasons — another client instance owns
// it, it was written after the moment, or the delete lost its compare-and-swap — and only the
// listener knows which. So the daemon says that the registration was kept and hands on the
// listener's own words for why, in the reason field, rather than asserting one of the three.
// Here it is kept because the entry belongs to another client instance.
func TestCleanSessionStopKeptLineCarriesTheListenersOwnReason(t *testing.T) {
	listener := &listenerRecorder{
		held:       storedRegistration{clientInstanceID: "inst_dead", lastSeen: time.Now().Add(-time.Second)},
		relaunched: &storedRegistration{clientInstanceID: "inst_relaunched", lastSeen: time.Now().Add(-time.Second)},
	}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}

	stop.send("ses_relaunched", time.Now())

	if !strings.Contains(logged.String(), "the registration belongs to another client instance") {
		t.Fatalf("logged %q, want the listener's own reason for keeping the registration", logged.String())
	}
	if !strings.Contains(logged.String(), "the Envoy listener kept the registration of a dead role's session") {
		t.Fatalf("logged %q, want the kept registration reported as kept", logged.String())
	}
}

// A registration that names no client process — one a client predating that field wrote — leaves
// the moment the probe judged to decide the delete on its own, and the stop names no instance at
// all rather than naming an empty one: the listener compares no instance for an empty value as
// for an absent one (session.InstanceMatches), so the query states only the condition the stop
// really holds.
func TestCleanSessionStopDeletesOnTheMomentAloneWhenTheEntryNamesNoInstance(t *testing.T) {
	listener := &listenerRecorder{held: storedRegistration{lastSeen: time.Now().Add(-time.Second)}}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}
	goneAt := time.Now()

	stop.send("ses_old_client", goneAt)

	conditions := listener.deleteConditions(t)
	if _, named := conditions["client_instance_id"]; named {
		t.Fatalf("the delete named client instance %q, want no instance parameter at all for an entry that carries none",
			conditions.Get("client_instance_id"))
	}
	if got := conditions.Get("if_not_registered_after"); got != strconv.FormatInt(goneAt.UnixMilli(), 10) {
		t.Fatalf("the delete named the moment %q, want the probe's own, %d", got, goneAt.UnixMilli())
	}
	if !listener.entryRemoved {
		t.Fatal("the registration of a process that named no instance was kept, want it deleted on the moment alone")
	}
	if !strings.Contains(logged.String(), "the Envoy session of a role whose process is gone was stopped") {
		t.Fatalf("logged %q, want the stop reported done", logged.String())
	}
}

// A registry that holds no record of the session has already reached what the stop asked for, so
// no delete is sent at all: there is nothing to name and nothing to remove.
func TestCleanSessionStopSendsNoDeleteWhenTheListenerHoldsNoRegistration(t *testing.T) {
	listener := &listenerRecorder{held: storedRegistration{absent: true}}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}

	stop.send("ses_already_gone", time.Now())

	if listener.delete.requests != 0 {
		t.Fatalf("the listener was asked %d deletes, want none for a session it holds no registration of", listener.delete.requests)
	}
	if !strings.Contains(logged.String(), "the Envoy listener answered no registration of a dead role's session, so none was stopped") {
		t.Fatalf("logged %q, want the absent registration reported", logged.String())
	}
}

// A read the listener refuses leaves unknown whether the entry names a client process, so a
// delete sent then would rest on the moment alone for an entry that may name one (sessionStop.send
// says why that is not sent). So nothing is deleted and nothing is tried again, and the
// registration lapses when its record expires, which is the wait the session already had.
func TestCleanSessionStopSendsNoDeleteWhenTheListenerRefusesTheRead(t *testing.T) {
	listener := &listenerRecorder{readStatus: http.StatusInternalServerError}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}

	stop.send("ses_dead", time.Now())

	if listener.read.requests != 1 || listener.delete.requests != 0 {
		t.Fatalf("the listener was asked %d reads and %d deletes, want the one read and no delete at all",
			listener.read.requests, listener.delete.requests)
	}
	if !strings.Contains(logged.String(), "the Envoy listener did not answer what a dead role's session registration is") {
		t.Fatalf("logged %q, want the unanswered read reported", logged.String())
	}
}

// A listener that never answers does not leave the stop in flight for as long as it likes: the
// read and the delete share one budget, and nothing else ends them, since the daemon's stop does
// not cut a stop in flight short (sessionStopsInFlight). Here the listener holds the read past
// that budget. A read that runs the budget out sends no delete, and it is the listener's failure
// to answer, logged at WARN. The stop ran under exactly its budget, held from both sides with no
// margin (wantStopRanItsBudget).
func TestCleanSessionStopEndsAtItsOwnBudgetWhenTheListenerNeverAnswers(t *testing.T) {
	const budget = 300 * time.Millisecond
	listener := &listenerRecorder{heldRead: make(chan struct{}), arrived: make(chan struct{})}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, ""), log: bufferedLogger(&logged), budget: budget}

	began := time.Now()
	stop.send("ses_hung", time.Now())
	returned := time.Now()

	select {
	case <-listener.arrived:
	default:
		t.Fatal("the stop's read never reached the listener")
	}
	wantStopRanItsBudget(t, logged.String(), began, returned, budget, "read the listener never answered")
	if listener.read.requests != 1 || listener.delete.requests != 0 {
		t.Fatalf("the listener was asked %d reads and %d deletes, want the one read and no delete after a read nobody answered",
			listener.read.requests, listener.delete.requests)
	}
	unanswered := slices.ContainsFunc(strings.Split(logged.String(), "\n"), func(line string) bool {
		return strings.Contains(line, "level=WARN") &&
			strings.Contains(line, "the Envoy listener did not answer what a dead role's session registration is")
	})
	if !unanswered {
		t.Fatalf("logged %q, want the unanswered read reported at WARN", logged.String())
	}
}

// The machine calls the stop inside its own decision, under its lock, so what cleanSessionStop
// returns reads the moment there — where no lock is waited on — and leaves the read and the
// delete to a goroutine, here while the listener still holds the read. That goroutine is counted
// in flight until its delete has been answered (sessionStopsInFlight).
func TestCleanSessionStopRunsOffTheMachinesDecision(t *testing.T) {
	listener := &listenerRecorder{
		held:     storedRegistration{clientInstanceID: "inst_dead", lastSeen: time.Now().Add(-time.Second)},
		heldRead: make(chan struct{}),
		arrived:  make(chan struct{}),
		settled:  make(chan struct{}),
	}
	server := listener.server(t)
	var inFlight sessionStopsInFlight
	stop := cleanSessionStop(&inFlight, server.URL, "envoy-token", sessionStopOverrides{}, quietLogger())

	returned := make(chan struct{})
	go func() {
		stop("ses_dead")
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the clean stop held the machine's decision until the listener answered")
	}
	select {
	case <-listener.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the clean stop never reached the listener")
	}
	close(listener.heldRead)
	inFlight.wait()
	select {
	case <-listener.settled:
	default:
		t.Fatal("the clean stop was counted done before the listener answered its delete")
	}
}

// With no listener configured there is nothing to tell, and the machines are given no SessionGone
// at all.
func TestNoCleanSessionStopWithoutAListener(t *testing.T) {
	if stop := cleanSessionStop(&sessionStopsInFlight{}, "", "", sessionStopOverrides{}, quietLogger()); stop != nil {
		t.Fatal("a daemon with no Envoy listener was given a clean stop to call")
	}
}

// A stop sent once the daemon's stop has begun its final wait is left out of that wait
// (sessionStopsInFlight), not out of the work: it still reads the registration and deletes it, on
// its own budget. Dropped instead, it would leave the ended process's registration standing until
// its record expires, and a message to a released role would be stored for a session nothing
// reads, with no wake.
func TestCleanSessionStopSentAfterTheFinalWaitBeganStillDeletesTheRegistration(t *testing.T) {
	listener := &listenerRecorder{
		held:     storedRegistration{clientInstanceID: "inst_dead", lastSeen: time.Now().Add(-time.Second)},
		heldRead: make(chan struct{}),
		arrived:  make(chan struct{}),
		settled:  make(chan struct{}),
	}
	server := listener.server(t)
	var inFlight sessionStopsInFlight
	stop := cleanSessionStop(&inFlight, server.URL, "envoy-token", sessionStopOverrides{}, quietLogger())
	// Nothing is counted yet, so the wait returns at once, and from here it has begun.
	inFlight.wait()

	stop("ses_dead")
	select {
	case <-listener.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the clean stop sent after the final wait began never reached the listener")
	}
	close(listener.heldRead)
	select {
	case <-listener.settled:
	case <-time.After(5 * time.Second):
		t.Fatal("the clean stop sent after the final wait began sent no delete")
	}
	if !listener.entryRemoved {
		t.Fatalf("the listener kept the registration asked for with %q, want the ended process's registration deleted",
			listener.delete.query)
	}
}

// Each stop sessionStopsInFlight counts before its final wait began holds that wait until the stop
// ends.
func TestSessionStopsInFlightWaitsForEveryStopStartedBeforeTheFinalWait(t *testing.T) {
	var stops sessionStopsInFlight
	if !stops.start() || !stops.start() {
		t.Fatal("a stop started before the final wait was not counted")
	}
	returned := make(chan struct{})
	go func() {
		stops.wait()
		close(returned)
	}()
	awaitFinalWaitBegun(t, &stops)

	stops.end()
	select {
	case <-returned:
		t.Fatal("the final wait returned with a stop it counted still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	stops.end()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the final wait did not return once every stop it counted had ended")
	}
}

// A stop started once the daemon's stop has begun its final wait is not counted, so the wait ends
// when the stops it did count end (sessionStopsInFlight).
func TestSessionStopsInFlightDoesNotWaitForAStopStartedAfterTheFinalWaitBegan(t *testing.T) {
	var stops sessionStopsInFlight
	if !stops.start() {
		t.Fatal("a stop started before the final wait was not counted")
	}
	returned := make(chan struct{})
	go func() {
		stops.wait()
		close(returned)
	}()
	awaitFinalWaitBegun(t, &stops)

	if stops.start() {
		t.Fatal("a stop started once the final wait had begun was counted, so the wait would wait for it")
	}
	stops.end()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the final wait did not return once the stop it counted ended: it waited for the stop started after it began")
	}
}

// Nothing orders a decision's clean stop before the daemon's final wait (sessionStopsInFlight), so
// a stop can start from a count of zero while that wait begins. Here the two run on two goroutines
// with nothing between them that the race detector reads as an order: the wait's goroutine
// sleeps before it begins, so the start comes first on any runner that is not stalled, and the
// wait then blocks on the stop it counted until the test ends that stop after a sleep of its own.
// On a sync.WaitGroup that is an Add from zero beside a Wait that blocks, which -race reports as
// a data race. This type takes both under one mutex, so the run is clean whichever comes first,
// and the wait returns once the stop it counted has ended.
func TestSessionStopsInFlightTakesAStopStartedFromZeroBesideTheFinalWait(t *testing.T) {
	var stops sessionStopsInFlight
	returned := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		stops.wait()
		close(returned)
	}()
	counted := stops.start()
	time.Sleep(200 * time.Millisecond)
	if counted {
		select {
		case <-returned:
			t.Fatal("the final wait returned with the stop it counted still in flight")
		default:
		}
		stops.end()
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the final wait did not return once the stop started beside it had ended")
	}
}

// The daemon's stop waits for the clean stops sessionStopsInFlight counts, and each ends within
// CleanSessionStopBudget of being sent. At half of stopBudget or less, a stop sent before the
// halt or in the first half of the stop's budget ends inside that budget: only one sent later in
// the drain can still be in flight when run stops waiting at the budget (awaitStop).
func TestCleanSessionStopFitsTwiceInTheStopBudget(t *testing.T) {
	if 2*CleanSessionStopBudget > stopBudget {
		t.Fatalf("CleanSessionStopBudget is %s, more than half of stopBudget's %s: a clean stop sent in the first half of the daemon's stop budget could still be in flight when that budget runs out",
			CleanSessionStopBudget, stopBudget)
	}
}

// A listener that accepts TCP and then sends no TLS bytes leaves the stop's TLS handshake pending,
// and the clean stop must end on its own context deadline, CleanSessionStopBudget
// (awaitStopOnItsDeadline). The stop's transport is its own, with no TLS-handshake limit
// (notify.stopTransport), where Go's shared one has 10 s. A TLS limit under the budget on it would
// end the stop first, with the transport's "TLS handshake timeout"; with no deadline on the
// stop's context the handshake would hang until the test's bound. The test waits for each fact it
// checks, the stop's connect and then its return, bounded by the test binary's deadline rather
// than a span of its own, so a correct stop that is slow to begin or to return on a loaded machine
// passes (testwait.Bounded). The stop's failed read ends the dial it detached from the request
// itself (notify's stopFailed); closing the accepted connection at the test's end is clean-up.
func TestCleanSessionStopContextEndsBeforeDefaultTransportTLSHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	var inFlight sessionStopsInFlight
	var logged bytes.Buffer
	stop := cleanSessionStop(&inFlight, "https://"+listener.Addr().String(), "envoy-token", sessionStopOverrides{}, bufferedLogger(&logged))
	began := time.Now()
	ended := stop("ses_dead")
	var conn net.Conn
	select {
	case conn = <-accepted:
		t.Cleanup(func() { _ = conn.Close() })
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case <-ended:
		t.Fatalf("the clean stop returned before it reached the TLS listener and logged %q", logged.String())
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("the clean stop did not reach the TLS listener")
	}
	awaitStopOnItsDeadline(t, ended, began, &logged, "TLS handshake")
}

// awaitStopOnItsDeadline waits for the clean stop sent at began, whose return is ended, while the
// step it names (what) is stalled, and requires that it ended on its own context's deadline after
// exactly its budget, CleanSessionStopBudget (wantStopRanItsBudget). The wait is bounded by a
// minute, or by the test binary's deadline less a second where that is nearer (testwait.Bounded),
// not by the budget: how long after its deadline a stop returns on a loaded machine is no part of
// what this checks, and a stop that never returns still fails, by name, at that bound. logged is
// read only once the stop has returned, so the stop's goroutine has written all it will.
func awaitStopOnItsDeadline(t *testing.T, ended <-chan struct{}, began time.Time, logged *bytes.Buffer, what string) {
	t.Helper()
	bound := testwait.Bounded(t, time.Minute, time.Second)
	select {
	case <-ended:
	case <-time.After(bound):
		t.Fatalf("the clean stop with its %s stalled had not returned %s after it was sent", what, bound)
	}
	wantStopRanItsBudget(t, logged.String(), began, time.Now(), CleanSessionStopBudget, what+" stalled")
}

// wantStopRanItsBudget requires that the clean stop sent at began, which had returned at returned
// and logged logged, ended on its own context's deadline after exactly budget. From below: it ran
// that whole budget, which a correct stop always does, since its context starts after began, and it
// logged that context's deadline rather than a limit of the transport. From above, with no margin:
// the stop's line names the moment its budget began and its context's deadline
// (sessionStop.send), and the two must be exactly the budget apart, the budget must have begun
// after began, and the stop must have returned no earlier than that deadline. Its end, measured
// from the moment its budget began, is then the budget plus the time its request took to return
// after the deadline, which the test measures and no fixed margin stands in for; a stop given any
// longer context, or one with none, fails here by name, whatever the load. what names the stop's
// stalled step in the failure.
func wantStopRanItsBudget(t *testing.T, logged string, began, returned time.Time, budget time.Duration, what string) {
	t.Helper()
	took := returned.Sub(began)
	if took < budget {
		t.Fatalf("the clean stop with its %s ended after %s and logged %q, before its budget (%s): something other than its context's deadline ended it",
			what, took, logged, budget)
	}
	if !strings.Contains(logged, context.DeadlineExceeded.Error()) {
		t.Fatalf("the clean stop with its %s ended after %s and logged %q, want it ended at its context's deadline, its budget (%s), before any limit of the transport",
			what, took, logged, budget)
	}
	fields := stopBudgetLine.FindStringSubmatch(logged)
	if fields == nil {
		t.Fatalf("the clean stop with its %s logged %q, want its line to name budgetBegan and deadline", what, logged)
	}
	budgetBegan, _ := strconv.ParseInt(fields[1], 10, 64)
	deadline, _ := strconv.ParseInt(fields[2], 10, 64)
	span := time.Duration(deadline-budgetBegan) * time.Millisecond
	lag := time.Duration(returned.UnixMilli()-deadline) * time.Millisecond
	if span != budget || budgetBegan < began.UnixMilli() || lag < 0 {
		t.Fatalf("the clean stop with its %s ran under a context of %s from its budget's start, %s after it was sent, and returned %s after that context's deadline, %s after it was sent; want a context of exactly its budget (%s), begun after it was sent, and a return no earlier than its deadline; logged %q",
			what, span, time.Duration(budgetBegan-began.UnixMilli())*time.Millisecond, lag, took, budget, logged)
	}
}

// stopBudgetLine reads the moment a clean stop's budget began and its context's deadline, in Unix
// milliseconds, off the line the stop logged (sessionStop.send) through bufferedLogger.
var stopBudgetLine = regexp.MustCompile(`budgetBegan=(-?\d+) deadline=(-?\d+)`)

// awaitFinalWaitBegun waits until stops' final wait has begun, which is the moment start stops
// counting: each stop the poll counts before then it ends again at once.
func awaitFinalWaitBegun(t *testing.T, stops *sessionStopsInFlight) {
	t.Helper()
	testwait.Eventually(t, "the final wait to begin", func() bool {
		if stops.start() {
			stops.end()
			return false
		}
		return true
	})
}
