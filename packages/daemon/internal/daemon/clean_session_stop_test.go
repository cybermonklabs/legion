package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/notify"
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
// id while the stop is in flight. The delete is judged as deleteSessionHandler judges it
// (packages/envoy/cmd/listener/api.go), under both conditions at the moment it lands, and
// entryRemoved says whether the registration went. readStatus is a registry route that refuses
// the read instead of answering it, and hangRead one that never answers at all until the
// request's own context ends.
type listenerRecorder struct {
	held         storedRegistration
	relaunched   *storedRegistration
	readStatus   int
	hangRead     bool
	read, delete askedRoute
	entryRemoved bool
	// arrived is closed when the first request of a stop reaches the listener, and settled once
	// a delete has been answered: a stop runs on a goroutine, so a test waits on these rather
	// than on the call it made.
	arrived, settled chan struct{}
}

func (l *listenerRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked := &l.read
		if r.Method == http.MethodDelete {
			asked = &l.delete
		}
		asked.method, asked.path, asked.query, asked.bearer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		asked.requests++
		if l.arrived != nil && l.read.requests+l.delete.requests == 1 {
			close(l.arrived)
		}
		if r.Method != http.MethodDelete {
			l.answerRead(w, r)
			return
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
// route does (sessionInfo, packages/envoy/cmd/listener/api.go). A relaunch the test staged takes
// the entry's place once this answer is written, so the delete that follows meets what the
// relaunch wrote and not what the read reported.
func (l *listenerRecorder) answerRead(w http.ResponseWriter, r *http.Request) {
	if l.hangRead {
		<-r.Context().Done()
		return
	}
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
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"error":"no live session %s"}`, session)
		return
	}
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
	_, _ = io.WriteString(w, `{"deleted":true}`)
}

func bufferedLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

// The registration of the process the probe judged gone is the one the stop must remove, and the
// listener stamps that registration's last write with its own clock: a dead pane's last
// heartbeat lands moments before the probe says the process is gone. Here it lands 500 ms
// before, and the stop deletes it.
//
// This is the test that is red at main, and the reason it is kept. Main dated the stop a whole
// second before the moment its probe judged, a margin for a clock skew the listener's own
// stamping makes no difference to, so this registration read as written after the stop and the
// listener kept it — the dead process's own heartbeat refusing the stop that was owed
// (dispatch://INFRA-2064).
func TestCleanSessionStopDeletesARegistrationWrittenBeforeTheProbesMoment(t *testing.T) {
	listener := &listenerRecorder{
		held:    storedRegistration{clientInstanceID: "inst_dead", lastSeen: time.Now().Add(-500 * time.Millisecond)},
		settled: make(chan struct{}),
	}
	server := listener.server(t)
	stop := cleanSessionStop(context.Background(), server.URL, "envoy-token", quietLogger())

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

	stop.send(context.Background(), "ses_dead", goneAt)

	if listener.read.method != http.MethodGet || listener.read.path != "/v1/registry/ses_dead" || listener.read.requests != 1 {
		t.Fatalf("the registry was asked %s %s %d times, want one read of the dead session's entry",
			listener.read.method, listener.read.path, listener.read.requests)
	}
	if listener.delete.method != http.MethodDelete || listener.delete.path != "/v1/sessions/ses_dead" ||
		listener.delete.bearer != "Bearer envoy-token" || listener.delete.requests != 1 {
		t.Fatalf("the listener was asked %s %s with %q %d times, want one DELETE of the session's key with the bearer",
			listener.delete.method, listener.delete.path, listener.delete.bearer, listener.delete.requests)
	}
	want := fmt.Sprintf("if_not_registered_after=%d&client_instance_id=inst_dead", goneAt.UnixMilli())
	if listener.delete.query != want {
		t.Fatalf("the listener was asked %q, want the entry's own instance and the probe's moment, %q", listener.delete.query, want)
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

	stop.send(context.Background(), "ses_relaunched", time.Now())

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

// The listener keeps a registration for three separate reasons, and only it knows which: the
// daemon says that the registration was kept and hands on the listener's own words for why, in
// the reason field, instead of asserting one of the three. Here it is kept because the entry
// belongs to another client instance, which the old line — "it was written after the stop" —
// stated falsely.
func TestCleanSessionStopKeptLineCarriesTheListenersOwnReason(t *testing.T) {
	listener := &listenerRecorder{
		held:       storedRegistration{clientInstanceID: "inst_dead", lastSeen: time.Now().Add(-time.Second)},
		relaunched: &storedRegistration{clientInstanceID: "inst_relaunched", lastSeen: time.Now().Add(-time.Second)},
	}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}

	stop.send(context.Background(), "ses_relaunched", time.Now())

	if !strings.Contains(logged.String(), "the registration belongs to another client instance") {
		t.Fatalf("logged %q, want the listener's own reason for keeping the registration", logged.String())
	}
	if strings.Contains(logged.String(), "it was written after the stop") {
		t.Fatalf("logged %q, want no verdict on why the listener kept the registration", logged.String())
	}
}

// A registration that names no client process — one a client predating that field wrote — leaves
// the moment the probe judged to decide the delete on its own, and the stop names no instance at
// all rather than naming an empty one, which the listener would read as a condition of its own.
func TestCleanSessionStopDeletesOnTheMomentAloneWhenTheEntryNamesNoInstance(t *testing.T) {
	listener := &listenerRecorder{held: storedRegistration{lastSeen: time.Now().Add(-time.Second)}}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}
	goneAt := time.Now()

	stop.send(context.Background(), "ses_old_client", goneAt)

	if want := fmt.Sprintf("if_not_registered_after=%d", goneAt.UnixMilli()); listener.delete.query != want {
		t.Fatalf("the listener was asked %q, want the moment alone, %q", listener.delete.query, want)
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

	stop.send(context.Background(), "ses_already_gone", time.Now())

	if listener.delete.requests != 0 {
		t.Fatalf("the listener was asked %d deletes, want none for a session it holds no registration of", listener.delete.requests)
	}
	if !strings.Contains(logged.String(), "the Envoy listener holds no registration of a dead role's session, so none was stopped") {
		t.Fatalf("logged %q, want the absent registration reported", logged.String())
	}
}

// A read the listener refuses names no registration, and a delete sent on it would rest on the
// moment alone — the very judgement that refused a dead pane's own last heartbeat. So nothing is
// deleted and nothing is tried again: the registration lapses at the gone horizon, which is the
// wait the session already had.
func TestCleanSessionStopSendsNoDeleteWhenTheListenerRefusesTheRead(t *testing.T) {
	listener := &listenerRecorder{readStatus: http.StatusInternalServerError}
	server := listener.server(t)
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, "envoy-token"), log: bufferedLogger(&logged)}

	stop.send(context.Background(), "ses_dead", time.Now())

	if listener.read.requests != 1 || listener.delete.requests != 0 {
		t.Fatalf("the listener was asked %d reads and %d deletes, want the one read and no delete at all",
			listener.read.requests, listener.delete.requests)
	}
	if !strings.Contains(logged.String(), "the Envoy listener did not answer what a dead role's session registration is") {
		t.Fatalf("logged %q, want the unanswered read reported", logged.String())
	}
}

// A listener that never answers does not leave the stop in flight for as long as it likes: the
// work is bounded, by cleanSessionStopBudget and by supervision's own end, and the stop that is
// abandoned there sends no delete either.
func TestCleanSessionStopIsAbandonedWhenSupervisionEnds(t *testing.T) {
	arrived := make(chan struct{})
	listener := &listenerRecorder{hangRead: true, arrived: arrived}
	server := listener.server(t)
	supervising, endSupervision := context.WithCancel(context.Background())
	defer endSupervision()
	go func() {
		<-arrived
		endSupervision()
	}()
	var logged bytes.Buffer
	stop := sessionStop{sessions: notify.New(server.URL, ""), log: bufferedLogger(&logged)}

	stop.send(supervising, "ses_hung", time.Now())

	if listener.delete.requests != 0 {
		t.Fatalf("the listener was asked %d deletes, want none from a stop whose read never answered", listener.delete.requests)
	}
	if !strings.Contains(logged.String(), "the Envoy listener did not answer what a dead role's session registration is") {
		t.Fatalf("logged %q, want the abandoned stop reported", logged.String())
	}
}

// The machine calls the stop inside its own decision, under its lock, so what cleanSessionStop
// returns reads the moment there — where no lock is waited on — and leaves the read and the
// delete to a goroutine.
func TestCleanSessionStopRunsOffTheMachinesDecision(t *testing.T) {
	arrived := make(chan struct{})
	listener := &listenerRecorder{
		held:    storedRegistration{clientInstanceID: "inst_dead", lastSeen: time.Now().Add(-time.Second)},
		arrived: arrived,
	}
	server := listener.server(t)
	stop := cleanSessionStop(context.Background(), server.URL, "envoy-token", quietLogger())

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
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the clean stop never reached the listener")
	}
}

// With no listener configured there is nothing to tell, and the machines are given no SessionGone
// at all.
func TestNoCleanSessionStopWithoutAListener(t *testing.T) {
	if stop := cleanSessionStop(context.Background(), "", "", quietLogger()); stop != nil {
		t.Fatal("a daemon with no Envoy listener was given a clean stop to call")
	}
}
