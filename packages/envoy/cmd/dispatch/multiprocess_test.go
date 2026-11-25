//go:build multiprocess

// Two envoy-dispatch processes serving one Postgres and one NATS, which is what production runs
// and what INFRA-66 made correct. Every row here starts real binaries and drives the real surface
// - HTTP, the processes' own logs, NATS's own ConsumerInfo and stream reads - because each
// mechanism the issue added is a property of two processes together and of nothing one process
// can show on its own.
//
// Its own build tag, and its own CI job (dispatch-multiprocess), for the reason the memory tests
// have theirs: a row here waits for a durable hand-over or for a maintenance loop's tick, tens of
// seconds each, and go test's default ten-minute deadline is one package's whole run.

package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/crafted"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/serverwait"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// start starts both processes on the pair's database and NATS, each on a port held for it from the
// moment the kernel chose it (startDispatchProcessWith), in a home of its own and behind a NATS
// proxy of its own, both carrying multiprocessSigningKey, and returns once each answers /healthz.
// The two are started together rather than one after the other: several rows are about what the
// two do when their loops fire at the same moment, which a staggered start would hide.
func (p *dispatchPair) start(t *testing.T, env map[string]string) {
	t.Helper()
	p.startSettling(t, env, 0, 0)
}

// startSettling is start with each process's own settle delay (DISPATCH_TEST_SETTLE_DELAY): how
// long A, then B, waits after a live change before it settles the document. Zero keeps the
// server's default. Both run with the test hooks (DISPATCH_TEST_HOOKS=1), as every process
// startDispatchProcessWith starts does.
func (p *dispatchPair) startSettling(t *testing.T, env map[string]string, settleA, settleB time.Duration) {
	t.Helper()
	p.a = p.startProcessSettling(t, env, settleA)
	p.b = p.startProcessSettling(t, env, settleB)
	p.a.waitHealthy(t)
	p.b.waitHealthy(t)
}

// startEach is start with an environment of each process's own, A's first and then B's, both
// started together as start starts them. A row whose two processes must be told apart by the
// calls they make -- a GitHub of its own per process, whose answers record which process asked
// (mergedPullRequestFake) -- is what needs it; every other row gives the two one environment.
func (p *dispatchPair) startEach(t *testing.T, envA, envB map[string]string) {
	t.Helper()
	p.a = p.startProcess(t, envA)
	p.b = p.startProcess(t, envB)
	p.a.waitHealthy(t)
	p.b.waitHealthy(t)
}

// kill ends the process with SIGKILL (os.Process.Kill), as a task replaced mid-deploy or killed
// for memory ends, with no chance to finish anything, and returns once it has exited.
func (p *dispatchProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the process: %v", err)
	}
	<-p.Exited
}

// startProcess starts one process of the pair behind a NATS proxy of its own, and returns without
// waiting for it to be healthy.
func (p *dispatchPair) startProcess(t *testing.T, env map[string]string) *dispatchProcess {
	t.Helper()
	return p.startProcessSettling(t, env, 0)
}

// startProcessSettling is startProcess with a settle delay of the process's own
// (DISPATCH_TEST_SETTLE_DELAY): how long it waits after a live change before it settles the
// document. Zero keeps the server's default.
func (p *dispatchPair) startProcessSettling(t *testing.T, env map[string]string, settle time.Duration) *dispatchProcess {
	t.Helper()
	proxy := testnats.StartProxy(t, p.nats)
	process := startDispatchProcessWith(t, p.databaseURL(t),
		dispatchOptions{NATSURL: proxy.URL(), SigningKey: multiprocessSigningKey, Env: env, SettleDelay: settle})
	p.proxies[process] = proxy
	return process
}

// proxy is the NATS proxy the process reaches NATS through.
func (p *dispatchPair) proxy(one *dispatchProcess) *testnats.Proxy {
	return p.proxies[one]
}

// startPair is newPair and start together, for a row that seeds nothing first.
func startPair(t *testing.T, env map[string]string) *dispatchPair {
	t.Helper()
	pair := newPair(t)
	pair.start(t, env)
	return pair
}

// ───── the live socket's event feed ─────────────────────────────────────────

// feedEvent is one event the event feed sent: its type and the event's JSON.
type feedEvent struct {
	eventType string
	data      string
}

// openEventFeed holds GET /ws/live open as a dashboard tab does, subscribed to the event feed, and
// reports every event it is sent on the returned channel, until the test ends or the socket does.
// One subscription, read to the end: a row about the live feed carrying the other process's event
// has to show that event arriving on the subscription the page already had, never on a
// resubscribe's catch-up.
func (p *dispatchProcess) openEventFeed(t *testing.T) <-chan feedEvent {
	t.Helper()
	conn := p.openLiveEvents(t)
	events := make(chan feedEvent, 256)
	go func() {
		defer close(events)
		for {
			var frame struct {
				Type  string          `json:"type"`
				Event json.RawMessage `json:"event"`
			}
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type != "event" {
				continue
			}
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(frame.Event, &event); err != nil {
				return
			}
			events <- feedEvent{eventType: event.Type, data: string(frame.Event)}
		}
	}()
	return events
}

// ───── document editors and the document test hooks ───────────────────────

// servingProcess is the X-Dispatch-Process header of the editor's websocket upgrade: the process
// that served its socket, or "" when the answer carried none.
func servingProcess(editor *docstest.Peer) string {
	return editor.UpgradeHeader.Get("X-Dispatch-Process")
}

// authenticateEditor authenticates the editor at the schema version the dashboard presents, as
// its provider does, and answers the scope the room granted: "read-write" or "readonly".
func authenticateEditor(t *testing.T, editor *docstest.Peer) string {
	t.Helper()
	scope, err := editor.Authenticate(t, strconv.Itoa(pmdoc.SchemaVersion()))
	if err != nil {
		t.Fatalf("authenticate the editor: %v", err)
	}
	return scope
}

// residentRooms lists the documents the process holds a room of now
// (GET /api/v1/artifacts/_test/rooms).
func (p *dispatchProcess) residentRooms(t *testing.T) []string {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/_test/rooms", "", http.StatusOK)
	var listed struct {
		Rooms []string `json:"rooms"`
	}
	if err := json.Unmarshal(answer, &listed); err != nil {
		t.Fatalf("decode the resident rooms: %v: %s", err, answer)
	}
	return listed.Rooms
}

// roomCounters is what the process did with the document's rooms and owed settlement since it
// started: its room failures, lock-waiting compactions, room closes and settlement resumptions
// (GET /api/v1/artifacts/{id}/_test/room-counters).
func (p *dispatchProcess) roomCounters(t *testing.T, artifactID string) docs.RoomCounters {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/_test/room-counters", "", http.StatusOK)
	var counters docs.RoomCounters
	if err := json.Unmarshal(answer, &counters); err != nil {
		t.Fatalf("decode the room counters: %v: %s", err, answer)
	}
	return counters
}

// holdPersistence holds, for duration from now, every append of a browser update the process's
// rooms of the document make (POST /api/v1/artifacts/{id}/_test/hold-persistence).
func (p *dispatchProcess) holdPersistence(t *testing.T, artifactID string, duration time.Duration) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/_test/hold-persistence",
		fmt.Sprintf(`{"duration":%q}`, duration.String()), http.StatusNoContent)
}

// liveWritePausePath is the test hook route of the pause of the document's live writes at point.
func liveWritePausePath(artifactID string, point docs.LiveWritePausePoint) string {
	return "/api/v1/artifacts/" + artifactID + "/_test/live-write-pauses/" + string(point)
}

// armLiveWritePause makes the next live write of the document on this process park at point
// until releaseLiveWritePause. Arm it before the write is sent, then awaitLiveWriteParked.
func (p *dispatchProcess) armLiveWritePause(t *testing.T, artifactID string, point docs.LiveWritePausePoint) {
	t.Helper()
	p.call(t, http.MethodPut, liveWritePausePath(artifactID, point), "", http.StatusNoContent)
}

// awaitLiveWriteParked waits for a write of the document to park at the pause armed at point,
// and fails the test when none does. The wait is the process's own (the hook's ?wait), so it is
// handed the shared wait's one bound as a span (spanToTheGiveUp): the hook answers that no write
// parked only once the moment every wait of the test gives up has come.
func (p *dispatchProcess) awaitLiveWriteParked(t *testing.T, artifactID string, point docs.LiveWritePausePoint) {
	t.Helper()
	answer := p.call(t, http.MethodGet, liveWritePausePath(artifactID, point)+"?wait="+spanToTheGiveUp(t).String(), "", http.StatusOK)
	var parked struct {
		Parked bool `json:"parked"`
	}
	if err := json.Unmarshal(answer, &parked); err != nil {
		t.Fatalf("decode the live write pause: %v: %s", err, answer)
	}
	if !parked.Parked {
		t.Fatalf("no live write of %s parked at %s by the moment the test's waits give up:\n%s", artifactID, point, p.Output.String())
	}
}

// spanToTheGiveUp is what is left, from now, of the test's one bound: the moment every wait of
// t gives up (testwait.GiveUpAt), as a Go duration, for a wait the test makes inside a child
// process through a hook that takes a span. Under -timeout 0, where no wait ends, it is the
// longest span a Go duration holds. Once that moment has passed it is the shortest positive
// span, so the child looks once, as a wait asks once past it before it fails.
func spanToTheGiveUp(t *testing.T) time.Duration {
	end, bounded := testwait.GiveUpAt(t)
	if !bounded {
		return time.Duration(math.MaxInt64)
	}
	return max(time.Until(end), time.Nanosecond)
}

// releaseLiveWritePause lets the write parked at point go on and disarms the pause, answering
// whether a write had parked there.
func (p *dispatchProcess) releaseLiveWritePause(t *testing.T, artifactID string, point docs.LiveWritePausePoint) bool {
	t.Helper()
	answer := p.call(t, http.MethodDelete, liveWritePausePath(artifactID, point), "", http.StatusOK)
	var released struct {
		Parked bool `json:"parked"`
	}
	if err := json.Unmarshal(answer, &released); err != nil {
		t.Fatalf("decode the live write release: %v: %s", err, answer)
	}
	return released.Parked
}

// pollBehindHorizon asks condition through the shared wait until it holds (horizontest.Watch over
// the process's stall warnings), and fails the test naming what, with the process's output, when
// the wait gives up, its last reading the horizon's account, which names a hold in the process's
// own database as one. Work behind the horizon, such as an event the process's relay publishes,
// is as late as the longest event write open in the process's database, which no fixed deadline
// bounds.
func (p *dispatchProcess) pollBehindHorizon(t *testing.T, what string, condition func() bool) {
	t.Helper()
	horizontest.NewWatch(p.stallWarnings(), p.database).Poll(t, what, p.Output.String, condition)
}

// ───── the live feed carries the other process's events ─────────────────────

// A dashboard tab is open on one process and the work happens on the other: the event of an issue
// created through B's API has to reach the event feed A's socket holds, on the subscription it
// already has. The live source of an open feed is not this process's in-memory broker, which hears
// only its own writes, but a relay over `listen dispatch_events`, so what either process commits
// every process publishes.
func TestALiveStreamOnOneProcessCarriesTheOtherProcessesEvent(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "XPR")
	pair.a.createProject(t, project, "Cross process")

	events := pair.a.openEventFeed(t)
	key, _ := pair.b.createIssueIn(t, project, "Created on the other process", "spec")

	// A's relay publishes B's event once A's event horizon has passed it, so the wait is A's
	// horizon's.
	pair.a.pollBehindHorizon(t, "the issue B created, on the feed A holds", func() bool {
		for {
			select {
			case event, open := <-events:
				if !open {
					t.Fatalf("the feed A held closed before B's event arrived:\n%s", pair.a.Output.String())
				}
				if event.eventType == "issue.created" && strings.Contains(event.data, `"`+key+`"`) {
					return true
				}
			default:
				return false
			}
		}
	})
}

// ───── test sessions ────────────────────────────────────────────────────────

const (
	// writesInFlight bounds the comment writes a row keeps waiting on an answer, and is how many
	// idle connections the rig's HTTP client keeps to each process.
	writesInFlight = 32
	// holderMachineID is the machine every session a row registers is on.
	holderMachineID = "multiprocess-sessions"
	// sessionHeartbeat is how often a registered session's entry is written again, as a session
	// plugin's heartbeat writes it: well inside session.ClaimStaleAfter and the session bucket's
	// TTL, so a role's holder stays fresh however long its row runs.
	sessionHeartbeat = 30 * time.Second
)

// sessionRig is the test's own NATS connection, the role-claim and session buckets it registers
// sessions in, and the HTTP client it writes comments through, each write under writes, the shared
// wait's context (testwait.Context), so a write a loaded machine answers late is waited for.
type sessionRig struct {
	natsURL  string
	conn     *natsgo.Conn
	roles    *envoystore.Registry
	sessions *session.SessionRegistry
	client   *http.Client
	writes   context.Context
}

func newSessionRig(t *testing.T, natsURL string) *sessionRig {
	t.Helper()
	conn := testnats.Connect(t, natsURL)
	t.Cleanup(conn.Close)
	roles, err := envoystore.Open(t.Context(), conn)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(roles.StopWatch)
	sessions, err := session.OpenSessionRegistry(t.Context(), conn)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = writesInFlight
	t.Cleanup(transport.CloseIdleConnections)
	return &sessionRig{
		natsURL:  natsURL,
		conn:     conn,
		roles:    roles,
		sessions: sessions,
		client:   &http.Client{Transport: transport},
		writes:   testwait.Context(t),
	}
}

// registerSession registers id in the session bucket as a session plugin's own registration does:
// self-subscribed, with no port, updated now. It writes the entry again every sessionHeartbeat
// until the test ends.
func (r *sessionRig) registerSession(t *testing.T, id string) {
	t.Helper()
	entry := session.SessionEntry{
		MachineID:      holderMachineID,
		Title:          "Multiprocess session " + id,
		UpdatedAt:      time.Now().UnixMilli(),
		SelfSubscribed: true,
	}
	if err := r.sessions.Put(id, entry); err != nil {
		t.Fatalf("register session %s: %v", id, err)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		heartbeat := time.NewTicker(sessionHeartbeat)
		defer heartbeat.Stop()
		for {
			select {
			case <-stop:
				return
			case <-heartbeat.C:
				entry.UpdatedAt = time.Now().UnixMilli()
				_ = r.sessions.Put(id, entry)
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-stopped
	})
}

// registerHolder registers holder as a session (registerSession) and makes it role's holder.
func (r *sessionRig) registerHolder(t *testing.T, role, holder string) {
	t.Helper()
	r.registerSession(t, holder)
	if err := r.claimRole(role, holder); err != nil {
		t.Fatalf("claim role %s for %s: %v", role, holder, err)
	}
}

// claimRole writes holder's claim of role in the role-claim bucket, a hard claim that takes the
// role from whoever held it.
func (r *sessionRig) claimRole(role, holder string) error {
	_, err := r.roles.SetRole(holder, holderMachineID, role, false)
	return err
}

// commentWrite is one comment written: its id, or the error that kept it from being written.
type commentWrite struct {
	commentID string
	err       error
}

// postComment writes one comment with body on issue key through p, as the test's login, and
// returns its id.
func (r *sessionRig) postComment(p *dispatchProcess, key, body string) commentWrite {
	return r.writeComment(p, key, map[string]any{"body": body}, "")
}

// writeComment writes one comment on issue key through p, whose request body is fields, and returns
// its id. It writes as the test's login, or, when as names a session, as that session: with the
// shared agent token, as a session plugin calls Dispatch, and the session named as the actor.
func (r *sessionRig) writeComment(p *dispatchProcess, key string, fields map[string]any, as string) commentWrite {
	if as != "" {
		fields["actor"] = map[string]string{"kind": "session", "id": as}
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return commentWrite{err: err}
	}
	request, err := http.NewRequestWithContext(r.writes, http.MethodPost, p.url("/api/v1/issues/"+key+"/comments"), bytes.NewReader(payload))
	if err != nil {
		return commentWrite{err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	if as != "" {
		request.Header.Set("Authorization", "Bearer "+dispatchTestAgentToken)
	} else {
		request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return commentWrite{err: err}
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusCreated {
		return commentWrite{err: fmt.Errorf("comment on %s: status %d: %s", key, response.StatusCode, answer)}
	}
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer, &comment); err != nil || comment.ID == "" {
		return commentWrite{err: fmt.Errorf("comment on %s: no id in %s: %v", key, answer, err)}
	}
	return commentWrite{commentID: comment.ID}
}

// written is the ids of the writes that succeeded; the failures are reported as errors on t,
// counted and with the first of them.
func written(t *testing.T, what string, writes []commentWrite) (ids []string, failures int) {
	t.Helper()
	var first error
	for _, write := range writes {
		if write.err != nil {
			failures++
			if first == nil {
				first = write.err
			}
			continue
		}
		ids = append(ids, write.commentID)
	}
	if failures > 0 {
		t.Errorf("%s: %d of %d writes failed; the first: %v", what, failures, len(writes), first)
	}
	return ids, failures
}

// testSession is one agent session as a host plugin runs it: a NATS connection of its own,
// subscribed to its agent subject or to the topic families it follows, that answers no frame -
// no session answers a role forward, now that the stream's acknowledgement of the copy it stored
// is a forward's whole outcome - and drops a repeat as the host does (createDeliveryDedupe
// in packages/envoy-client): a frame whose event id it has taken, or whose dedupe key names an event
// it has taken, is not taken again. A role forward's dedupe key is contracts.RoleForwardDedupePrefix
// and its event's own key, so that prefix is what maps it to its event. Every frame is recorded in
// arrival order.
type testSession struct {
	id       string
	natsURL  string
	subjects []string

	mu     sync.Mutex
	conn   *natsgo.Conn
	frames []sessionFrame
	taken  map[string]bool
}

// sessionFrame is one frame as it reached a session.
type sessionFrame struct {
	at      time.Time
	subject string
	// event is the envelope's event id, dispatch-<id>; kind, owner, seq and commentID are read from
	// the event it carries. owner is the event's document, or else its issue: each has a seq of its
	// own.
	event     string
	kind      string
	owner     string
	seq       int
	commentID string
	// repeat is a frame of an event the session had already taken.
	repeat bool
}

// eventRow is the events row the frame's event is.
func (f sessionFrame) eventRow(t *testing.T) int64 {
	t.Helper()
	id, err := strconv.ParseInt(strings.TrimPrefix(f.event, "dispatch-"), 10, 64)
	if err != nil {
		t.Fatalf("read the event row of event id %q: %v", f.event, err)
	}
	return id
}

// newTestSession connects session id to the rig's NATS and subscribes it to subjects, until the
// test ends.
func newTestSession(t *testing.T, rig *sessionRig, id string, subjects ...string) *testSession {
	t.Helper()
	s := &testSession{id: id, natsURL: rig.natsURL, subjects: subjects, taken: map[string]bool{}}
	if err := s.connect(t); err != nil {
		t.Fatalf("connect session %s: %v", id, err)
	}
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.conn.Close()
	})
	return s
}

// connect opens a connection with the session's subscriptions and makes it the session's. Each
// dial's own budget is nats.go's default, and a dial that fails is made again through the shared
// wait, as testnats.Connect makes its dials; the flush of the subscriptions waits under the shared
// wait's context.
func (s *testSession) connect(t *testing.T) error {
	t.Helper()
	var conn *natsgo.Conn
	testwait.Until(t, fmt.Sprintf("session %s's connection to NATS", s.id), func() (error, bool) {
		var err error
		conn, err = natsgo.Connect(s.natsURL)
		return err, err == nil
	})
	for _, subject := range s.subjects {
		if _, err := conn.Subscribe(subject, s.receive); err != nil {
			conn.Close()
			return fmt.Errorf("subscribe to %s: %w", subject, err)
		}
	}
	if err := conn.FlushWithContext(testnats.Context(t)); err != nil {
		conn.Close()
		return fmt.Errorf("flush the subscriptions: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
	return nil
}

// reconnect drops the session's connection and opens a new one with the same subscriptions, as a
// host whose connection broke does. A frame sent between the two reaches neither.
func (s *testSession) reconnect(t *testing.T) error {
	t.Helper()
	s.mu.Lock()
	old := s.conn
	s.mu.Unlock()
	old.Close()
	return s.connect(t)
}

func (s *testSession) receive(msg *natsgo.Msg) {
	at := time.Now()
	var item contracts.Envelope
	if err := json.Unmarshal(msg.Data, &item); err != nil {
		return
	}
	var event struct {
		Type       string  `json:"type"`
		IssueKey   *string `json:"issue_key"`
		ArtifactID *string `json:"artifact_id"`
		Seq        int     `json:"seq"`
		Payload    struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	_ = json.Unmarshal([]byte(item.Payload), &event)
	frame := sessionFrame{
		at: at, subject: msg.Subject, event: item.EventID, kind: event.Type, seq: event.Seq,
	}
	switch {
	case event.ArtifactID != nil:
		frame.owner = "document " + *event.ArtifactID
	case event.IssueKey != nil:
		frame.owner = "issue " + *event.IssueKey
	}
	if event.Type == "comment.created" {
		frame.commentID = event.Payload.ID
	}
	names := []string{item.EventID, strings.TrimPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix)}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		frame.repeat = frame.repeat || (name != "" && s.taken[name])
	}
	if !frame.repeat {
		for _, name := range names {
			if name != "" {
				s.taken[name] = true
			}
		}
	}
	s.frames = append(s.frames, frame)
}

// allFrames is every frame that reached the session, in arrival order.
func (s *testSession) allFrames() []sessionFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.frames)
}

// takenFrames is the frames the session took, in arrival order: every frame but its repeats.
func (s *testSession) takenFrames() []sessionFrame {
	var taken []sessionFrame
	for _, frame := range s.allFrames() {
		if !frame.repeat {
			taken = append(taken, frame)
		}
	}
	return taken
}

// takenComments is when the session took each comment's comment.created event, by comment id.
func (s *testSession) takenComments() map[string]sessionFrame {
	taken := map[string]sessionFrame{}
	for _, frame := range s.takenFrames() {
		if frame.commentID != "" {
			taken[frame.commentID] = frame
		}
	}
	return taken
}

// requireTakenOnce fails the test for each comment of ids whose event the session has not taken.
// The session takes an event at most once, so each one it has is taken exactly once.
func (s *testSession) requireTakenOnce(t *testing.T, ids []string) {
	t.Helper()
	taken := s.takenComments()
	var missing []string
	for _, id := range ids {
		if _, ok := taken[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("session %s has not taken %d of the %d comments addressed to it: %v", s.id, len(missing), len(ids), missing)
	}
}

// requireInOrder fails the test for every event the session took out of its owner's seq order:
// each document's and each issue's events must reach it in the order they were written.
func (s *testSession) requireInOrder(t *testing.T) {
	t.Helper()
	last := map[string]sessionFrame{}
	for _, frame := range s.takenFrames() {
		if frame.owner == "" {
			continue
		}
		if previous, ok := last[frame.owner]; ok && frame.seq <= previous.seq {
			t.Errorf("session %s took %s's event %s (seq %d, %s) after event %s (seq %d, %s)",
				s.id, frame.owner, frame.event, frame.seq, frame.kind, previous.event, previous.seq, previous.kind)
		}
		last[frame.owner] = frame
	}
}

// ───── the outbox ───────────────────────────────────────────────────────────

// outboxRoleLane is the role the outbox row's issue is routed to, and outboxRoleHolder its holder:
// each event is forwarded to the holder's agent subject with a Request, which a core subscriber of
// that subject is handed every time it is sent, whatever the stream's duplicate window keeps. It
// is where a second publish of one event shows up.
const (
	outboxRoleLane   = "multiprocess-outbox"
	outboxRoleHolder = "ses_multiprocess_outbox"
)

// Both processes run an outbox on one events table, and one of them at a time holds the publisher
// hold and publishes, so one event is published once however many processes run: every row
// reaches published_at, the stream holds one retained message per (dedupe key, topic), and the
// role's holder - a core subscriber that is handed every forward and answers none, as a session
// does - takes one forward per event. Nothing crosses the role's own topic. The outbox forwards an
// event to the holder again only where the batch that forwarded it left the role's destination
// unreached - its forward failed, the event was released, the batch's record failed, or the hold
// went to the next turn with the batch unrecorded - and each of those writes a line of its own, so
// the holder takes each event once and once more for each such line or hand-over the two
// processes' outboxes record (lawfulResends): a forward past that count fails the row, and with
// no resend recorded that is any count but one. Two processes publishing at once record none.
func TestTheOutboxPublishesEachEventOnceAcrossBothProcesses(t *testing.T) {
	pair := startPair(t, nil)
	rig := newSessionRig(t, pair.nats)
	conn := rig.conn
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	// This row counts only what it publishes itself: readStreamMessagesFrom starts past this head.
	head := streamHead(t, js)

	forwards := make(chan contracts.Envelope, 256)
	holderSub, err := conn.Subscribe(contracts.LegacyAgentSubject(outboxRoleHolder), func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err == nil && strings.HasPrefix(item.DedupeKey, contracts.RoleForwardDedupePrefix) {
			forwards <- item
		}
	})
	if err != nil {
		t.Fatalf("subscribe the role's holder: %v", err)
	}
	t.Cleanup(func() { _ = holderSub.Unsubscribe() })
	lane := make(chan contracts.Envelope, 256)
	laneSub, err := conn.Subscribe(contracts.RoleTopicPrefix+outboxRoleLane, func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err == nil {
			lane <- item
		}
	})
	if err != nil {
		t.Fatalf("subscribe to the role's topic: %v", err)
	}
	t.Cleanup(func() { _ = laneSub.Unsubscribe() })
	if err := conn.FlushWithContext(testnats.Context(t)); err != nil {
		t.Fatalf("flush the subscriptions: %v", err)
	}
	rig.registerHolder(t, outboxRoleLane, outboxRoleHolder)

	project := uniqueProjectKey(t, "OBX")
	pair.a.createProject(t, project, "Outbox")
	key, _ := pair.a.createIssueIn(t, project, "Published once", "spec")
	pair.a.call(t, http.MethodPatch, "/api/v1/issues/"+key,
		fmt.Sprintf(`{"route":"role:%s"}`, outboxRoleLane), http.StatusOK)

	// Writes through both processes' APIs, interleaved, so both outboxes are scanning the same
	// unpublished rows while they are being written.
	const comments = 8
	for i := range comments {
		writer := pair.a
		if i%2 == 1 {
			writer = pair.b
		}
		writer.call(t, http.MethodPost, "/api/v1/issues/"+key+"/comments",
			fmt.Sprintf(`{"body":"comment %d"}`, i), http.StatusCreated)
	}

	waitForEveryEventPublished(t, pair.database)

	// One more comment, written only once every earlier event was marked published: its forward
	// is the marker that says every forward the comments above could produce has arrived. A
	// second publish of an already published event is the other process's outbox racing the
	// first over the same batch, so it is in flight at about the moment its original is and
	// never as late as an event published after the marks; the marker's forward is what the
	// settling read this row used to take stood in for. The outbox forwards every event of an
	// issue routed to a role to the role's holder (roleForward), the marker comment's own event
	// among them, so the wait ends on that forward, named by the envelope's EventID, and on
	// nothing else the holder is sent.
	answer := pair.b.call(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", `{"body":"marker"}`, http.StatusCreated)
	var marker struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer, &marker); err != nil || marker.ID == "" {
		t.Fatalf("the marker comment's answer names no id: %s (%v)", answer, err)
	}
	markerEvent := commentEventID(t, pair.database, marker.ID)
	var received []contracts.Envelope
	perEvent := map[string]int{}
	markerArrived := false
	testwait.Until(t, "the marker comment's forward ("+markerEvent+") to reach the role's holder", func() (int, bool) {
		for {
			select {
			case item := <-forwards:
				received = append(received, item)
				perEvent[item.SourceEventID]++
				markerArrived = markerArrived || item.EventID == markerEvent
			default:
				return len(perEvent), markerArrived
			}
		}
	})
	for _, item := range received {
		if item.Topic != contracts.RoleTopicPrefix+outboxRoleLane {
			t.Errorf("the holder received a forward of event %s on topic %q, want the role's", item.SourceEventID, item.Topic)
		}
	}
	if len(perEvent) < comments {
		t.Errorf("the role's holder took %d events, want at least the %d comments:\n%v", len(perEvent), comments, perEvent)
	}
	// The lines are read once every event is published, which is after every send the outbox
	// made of them and after each line that sent one again.
	logged := map[*dispatchProcess][]outboxLine{pair.a: pair.a.outboxLines(t), pair.b: pair.b.outboxLines(t)}
	for eventID, count := range perEvent {
		if count == 1 {
			continue
		}
		counted := lawfulResends(logged, eventID, roleDestination(outboxRoleLane), time.Time{})
		if count <= 1+len(counted) {
			t.Logf("event %s reached the role's holder %d times: its forward and the %d resends there the outbox's own lines record:\n%s",
				eventID, count, len(counted), counted)
			continue
		}
		t.Errorf("event %s reached the role's holder %d times, and the outbox's own lines record %d resends of it there, so %d of those forwards nothing the product recorded explains; two processes publishing one event is what the publisher hold prevents. The resends counted:\n%s\nEvery event's forwards: %v\n%s",
			eventID, count, len(counted), count-1-len(counted), counted, perEvent, outboxLinesAround(pair, logged, time.Time{}, time.Now()))
	}
	if carried := len(lane); carried != 0 {
		t.Errorf("the role's topic carried %d envelopes, want none: the outbox forwards to the holder itself", carried)
	}

	perMessage := map[string]int{}
	for _, message := range readStreamMessagesFrom(t, js, head) {
		perMessage[message.item.DedupeKey+" on "+message.item.Topic]++
	}
	if len(perMessage) == 0 {
		t.Fatalf("the notification stream holds nothing this row published")
	}
	for message, count := range perMessage {
		if count != 1 {
			t.Errorf("the stream holds %d retained messages for %s; want one", count, message)
		}
	}
}

// ───── the outbox's publisher across two processes ──────────────────────────

const (
	// takeOverWithin bounds a take-over: from the moment the publisher is killed, cut off NATS or
	// frozen, the other process takes the publisher hold, publishes its first batch and a session
	// takes a comment written since, each within it. It is the service level the product promises
	// (outbox.publisherHoldOptions: "a process that is killed is replaced by a standby within 6 s
	// of its last renewal"), with 2 s above the hold's own TTL and Margin (6 s and 2 s), read off
	// this machine's clock across two child processes, so only the rows' twins in benchtime's
	// timing lane (benchtime.OnTheClock) hold a take-over, or a stretch with no publisher, to it.
	// Every run holds that the take-over happens, that no two processes publish at once and that
	// sessions take each event once and in order; and the standby's timer to the lapse of the hold
	// it waits on, armed no later than the hold's TTL after the holder's last renewal, is held on a
	// fake by outbox.TestAStandbyTakesTheHoldOfAHolderThatStoppedRenewingOnceItsRowLapses.
	takeOverWithin = 10 * time.Second
	// frozenFor is how long the frozen-publisher row holds the publisher stopped.
	frozenFor = 10 * time.Second
	// resumedQuiet is how long the frozen-publisher row watches the sessions once the resumed
	// process has logged that it lost the hold: anything it sent on resuming arrives in this.
	resumedQuiet = 3 * time.Second
	// sceneWritesAtOnce is how many comments writeAll keeps waiting on an answer at once, so both
	// processes commit events of one issue at the same moment.
	sceneWritesAtOnce = 8

	// outboxLogPrefix starts every message the outbox logs; the messages below follow it, each line
	// carrying the process's instance.
	outboxLogPrefix = "dispatch outbox: "
	tookHold        = "took the publisher hold"
	releasedHold    = "released the publisher hold"
	lostHold        = "lost the publisher hold"
	sentBatch       = "sent batch"
	// scanFailed is the line, at ERROR, the scan writes for a batch that returned an error - a
	// statement of the batch failed, its record among them ("record the batch's outcome") - which
	// names the events that batch sent (event_ids) and leaves them unpublished for the next read.
	scanFailed = "scan"
	// eventBackedOff is the line, at ERROR, of an event a send of which NATS answered with a
	// failure, or whose destination could not be sent to at all: its error names each failed send
	// by its destination (`publish route "role:…": …`), and the event is read again, its retry
	// due, for every destination its batch did not reach (outbox's settleEvent, backOff).
	eventBackedOff = "publish event"
	// eventReleased is the line of an event whose batch left a send of it lost or unmade - the
	// connection lost or replaced, the process stopping - due again at once for every destination
	// its batch did not reach (outbox's settleEvent). Its error names only the sends that failed
	// some other way, so it does not say which destinations those are.
	eventReleased = "event released"
)

// healthReport is what a process's /healthz says of NATS and of the outbox. OutboxPublisher is nil
// when the body has no such field.
type healthReport struct {
	NATS            *bool `json:"nats"`
	OutboxPublisher *bool `json:"outbox_publisher"`
}

// publishing reports whether the report says the process publishes the outbox.
func (r healthReport) publishing() bool { return r.OutboxPublisher != nil && *r.OutboxPublisher }

func (r healthReport) String() string {
	nats := "no nats field"
	if r.NATS != nil {
		nats = fmt.Sprintf("nats %v", *r.NATS)
	}
	if r.OutboxPublisher == nil {
		return nats + ", no outbox_publisher field"
	}
	return fmt.Sprintf("%s, outbox_publisher %v", nats, *r.OutboxPublisher)
}

// reportRead is one read of a process's /healthz as a wait's reading: the report, and what kept
// the read from one.
type reportRead struct {
	report healthReport
	err    error
}

func (r reportRead) String() string { return fmt.Sprintf("%v (%v)", r.report, r.err) }

// readHealthReport reads the process's /healthz, whose body a 503 carries too, under ctx: the
// shared wait's (testwait.Context), so a probe a loaded machine answers late is waited for rather
// than failed, and one that never answers ends at the wait's deadline. A wait that reads it on
// every attempt makes ctx once, before the wait.
func (p *dispatchProcess) readHealthReport(ctx context.Context) (healthReport, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url("/healthz"), nil)
	if err != nil {
		return healthReport{}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return healthReport{}, err
	}
	defer response.Body.Close()
	var report healthReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		return healthReport{}, fmt.Errorf("decode /healthz (status %d): %w", response.StatusCode, err)
	}
	return report, nil
}

// waitForPublisher waits for exactly one process of the pair to report outbox_publisher true, and
// answers which, and when the read that saw it was issued. Its failure prints both processes'
// last reports.
//
// The second answer is for a caller that measures a stretch with no publisher up to this wait:
// what ends such a stretch is the read that found a publisher again, and that read dates it no
// later than the moment it was issued. Its answer is later by the whole of the health probe's
// own timeout, and the round before it by another read of the other process and the shared
// wait's pace between rounds, so measuring to anything but the issue of that one read counts as
// time with no publisher a stretch in which a publisher was already back.
func waitForPublisher(t *testing.T, pair *dispatchPair) (*dispatchProcess, time.Time) {
	t.Helper()
	ctx := testwait.Context(t)
	var publisher *dispatchProcess
	var issued time.Time
	testwait.Until(t, "one process of the pair to report outbox_publisher true", func() (string, bool) {
		// Each read is bracketed from its own issue, since the two go out one after the other and
		// the first one's answer can be a probe timeout later than the moment it was issued.
		issuedA := time.Now()
		a, errA := pair.a.readHealthReport(ctx)
		issuedB := time.Now()
		b, errB := pair.b.readHealthReport(ctx)
		if errA == nil && errB == nil && a.publishing() != b.publishing() {
			if a.publishing() {
				publisher, issued = pair.a, issuedA
			} else {
				publisher, issued = pair.b, issuedB
			}
			return "", true
		}
		return fmt.Sprintf("A: %v %v; B: %v %v", a, errA, b, errB), false
	})
	return publisher, issued
}

// signal sends the process sig and returns when it was sent.
func (p *dispatchProcess) signal(t *testing.T, sig syscall.Signal) time.Time {
	t.Helper()
	at := time.Now()
	if err := p.Cmd.Process.Signal(sig); err != nil {
		t.Fatalf("send %v to the process: %v", sig, err)
	}
	return at
}

// outboxLine is one line a process's outbox logged about its publisher hold, a batch or an
// event: when, its message after outboxLogPrefix, its instance, a release's reason, and the three
// moments the hold's own lines carry - started, which is a batch's first send on a sent batch
// line and the moment the process began publishing on a took line, and stopped and stoppedBy, the
// two bounds on the moment it stopped publishing, on the line that ends a turn as holder. stopped
// is at or before that moment and stoppedBy at or after it, and on every path that ends at the
// hold's deadline the two are the same value and the stop is named exactly. eventID is the event a
// line about one event names (its event_id), failure the error a line carries, and eventIDs the
// events a batch's lines name (event_ids): on a sent batch line the events that batch sent, on the
// scan's failure line those the failed batch sent; none on any other line.
type outboxLine struct {
	at        time.Time
	message   string
	instance  string
	reason    string
	started   time.Time
	stopped   time.Time
	stoppedBy time.Time
	eventID   string
	failure   string
	eventIDs  []string
	text      string
}

// logAttribute is one key=value of a slog text line, its value quoted or bare.
var logAttribute = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)

// outboxLines is every line the process's outbox has logged, in order.
func (p *dispatchProcess) outboxLines(t *testing.T) []outboxLine {
	t.Helper()
	var lines []outboxLine
	// The processes are still running while this reads their output, so its last line can have
	// been written only in part. strings.Lines yields that part as a line of its own, and it is
	// the one line it yields without a trailing newline, so whole and partial are told apart
	// where the output is already split: a whole line is followed by its newline, while the
	// partial one is followed by whatever the process writes next. One read of the output
	// answers both questions, which is what this does rather than search a second copy for each
	// matched line: waitForOutboxLine calls this every 50 ms while it waits, on the kind of
	// runner whose stalls this row is about.
	//
	// Leaving a partial line out rather than parsing it is what this row wants: a took, released
	// or lost line caught halfway carries no started, stopped or stopped_by yet, and the three
	// requirements below would fail the row on a line the binary wrote correctly. Nothing is
	// lost by it, since any later read of the output sees the line whole, and startOutboxPair
	// prints both processes' whole output when the row fails.
	for raw := range strings.Lines(p.Output.String()) {
		text, whole := strings.CutSuffix(raw, "\n")
		if !whole || !strings.Contains(text, `msg="`+outboxLogPrefix) {
			continue
		}
		attributes := map[string]string{}
		for _, match := range logAttribute.FindAllStringSubmatch(text, -1) {
			value := match[2]
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			if _, ok := attributes[match[1]]; !ok {
				attributes[match[1]] = value
			}
		}
		at, err := time.Parse(time.RFC3339Nano, attributes["time"])
		if err != nil {
			t.Fatalf("read the time of the line %q: %v", text, err)
		}
		line := outboxLine{
			at: at, message: strings.TrimPrefix(attributes["msg"], outboxLogPrefix),
			instance: attributes["instance"], reason: attributes["reason"], text: text,
			eventID: attributes["event_id"], failure: attributes["error"],
		}
		// slog writes the failure line's ids as a quoted list, "[3 5]".
		line.eventIDs = strings.Fields(strings.Trim(attributes["event_ids"], "[]"))
		// moment reads one of the RFC3339Nano moments the hold's own lines carry, and fails the
		// row when a line that must carry it does not.
		moment := func(key string, required bool) time.Time {
			value, ok := attributes[key]
			if !ok {
				if required {
					t.Fatalf("the line %q carries no %s, so the publishing period it bounds cannot be read", text, key)
				}
				return time.Time{}
			}
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				t.Fatalf("read the %s of the line %q: %v", key, text, err)
			}
			return at
		}
		ends := line.message == releasedHold || line.message == lostHold
		// A took line must date the start of the period and a released or lost line must bracket
		// its end from both sides, because publishingAt reads those three and nothing else to
		// decide a /healthz sample against the log - its doc comment says in full why the lines'
		// own times will not do, the stop having been seen 1.09 s before the line that reports it
		// (dispatch://INFRA-1568, job 42346322118). A binary that stops writing one of them is a
		// regression, and it is caught here rather than left to turn into a /healthz
		// disagreement whose reader has to work out where it came from.
		line.started = moment("started", line.message == tookHold)
		line.stopped = moment("stopped", ends)
		line.stoppedBy = moment("stopped_by", ends)
		lines = append(lines, line)
	}
	return lines
}

// waitForOutboxLine waits for the process to log message at or after since, and returns the first
// such line. A line's time is in milliseconds, so since is too.
func (p *dispatchProcess) waitForOutboxLine(t *testing.T, message string, since time.Time) outboxLine {
	t.Helper()
	from := since.Truncate(time.Millisecond)
	return testwait.Until(t, fmt.Sprintf("the process to log %q at or after %s", outboxLogPrefix+message, since.Format(time.RFC3339Nano)),
		func() (outboxLine, bool) {
			var last outboxLine
			for _, line := range p.outboxLines(t) {
				if line.message == message && !line.at.Before(from) {
					return line, true
				}
				last = line
			}
			return last, false
		})
}

// countOutboxLines is how many of lines say message.
func countOutboxLines(lines []outboxLine, message string) int {
	count := 0
	for _, line := range lines {
		if line.message == message {
			count++
		}
	}
	return count
}

// waitForNextOutboxLine waits for the process to log message once more than the seen lines of it
// it had logged when the caller counted them (countOutboxLines), and returns that line: the one
// that follows, in the order the process wrote its lines, whatever moment it carries.
func (p *dispatchProcess) waitForNextOutboxLine(t *testing.T, message string, seen int) outboxLine {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("the process to log %q once more than the %d times it had", outboxLogPrefix+message, seen),
		func() (outboxLine, bool) {
			var last outboxLine
			count := 0
			for _, line := range p.outboxLines(t) {
				if line.message == message {
					if count == seen {
						return line, true
					}
					count++
				}
				last = line
			}
			return last, false
		})
}

// publishingPeriod is one span over which a process held the publisher hold: from its took line to
// the released or lost line after it, or still open.
//
// It carries two starts and three ends, because the two questions asked of it are different.
// `from` is when the took line was written and `to` when the line that ended the period was, and
// those are the ends every assertion about batches and overlaps uses: a batch in flight when the
// loss deadline passes is fenced at its record, after the sends that succeeded are counted, so
// its sent batch line is written after the process stopped publishing and before the line that
// says so. `started` is the moment the process began publishing and `stopped` and `stoppedBy`
// bracket the moment it stopped, at or before and at or after it, which those two lines carry;
// only the agreement of /healthz with the log reads the three, and it needs both ends of the
// bracket for the same reason it needs both ends of the start: a read that falls inside one of
// them is a read the process could honestly have answered either way (publishingAt).
type publishingPeriod struct {
	instance                    string
	from, to                    time.Time
	started, stopped, stoppedBy time.Time
	open                        bool
	// ranTo is how far a period still open ran, as its own process's record shows it publishing
	// (openPeriodsRanTo), and the zero time until that record is read.
	ranTo time.Time
}

// publishingPeriods is every period the lines, one process's, show it holding the publisher hold.
// All three moments it reads are required of the lines by outboxLines, so none is defaulted here.
func publishingPeriods(lines []outboxLine) []publishingPeriod {
	var periods []publishingPeriod
	for _, line := range lines {
		switch line.message {
		case tookHold:
			periods = append(periods, publishingPeriod{
				instance: line.instance, from: line.at, started: line.started, open: true,
			})
		case releasedHold, lostHold:
			if last := len(periods) - 1; last >= 0 && periods[last].open {
				periods[last].to, periods[last].open = line.at, false
				periods[last].stopped, periods[last].stoppedBy = line.stopped, line.stoppedBy
			}
		}
	}
	return periods
}

// end is when the period ended: the moment of the line that ended it, or, while it is open, how
// far its own process's record shows it ran (ranTo).
func (p publishingPeriod) end() time.Time {
	if p.open {
		return p.ranTo
	}
	return p.to
}

// holds reports whether at falls inside the period.
func (p publishingPeriod) holds(at time.Time) bool {
	return !at.Before(p.from) && !at.After(p.end())
}

// openPeriodsRanTo is periods, one process's, with each one still open given, as its ranTo, the
// latest moment that process's own record shows it publishing in it: the took line's own moment,
// each batch the lines show it sent after that, and the from of each of its stamped reads of its
// publisher inside the period that answered true, as openPeriodsRan reads them. Every moment is
// one the process stamped on its own clock and none is the test's: a period still open is not run
// out past what the process recorded, to whenever the test happens to read its clock.
func openPeriodsRanTo(periods []publishingPeriod, lines []outboxLine, reads []stampedRead) []publishingPeriod {
	ran := slices.Clone(periods)
	for index, period := range ran {
		if !period.open {
			continue
		}
		period.ranTo = period.from
		for _, line := range lines {
			if line.message == sentBatch && line.instance == period.instance && line.at.After(period.ranTo) {
				period.ranTo = line.at
			}
		}
		inside := period.from.Add(logResolution)
		for _, read := range reads {
			if read.publishing && !read.bracket.from.Before(inside) && read.bracket.from.After(period.ranTo) {
				period.ranTo = read.bracket.from
			}
		}
		ran[index] = period
	}
	return ran
}

// logResolution is how much a line's own time understates when the line was written: slog's text
// handler writes that time truncated to the millisecond, so the log call ran at or after it and
// within this of it.
//
// publishingAt uses it for one thing, the upper bound of a period's start. The took line is
// written after Publishing() turned true, so the line's time plus this is past the real start for
// certain, and a read stamped from at or after it was made inside the period. The lower bound is
// not the line's time less this, because what separates the real start from the log call is not
// the truncation but the pre-emption between the store of the hold and the call, which has no
// bound; it is the `started` attribute the took line carries, read before the store. The end needs
// nothing of the kind either: the line that ends a period carries both of its bounds, `stopped`
// and `stopped_by`, as RFC3339Nano strings for exactly this reason.
const logResolution = time.Millisecond

// publishingAt reports what a process's own outbox lines say it answered to a read it stamped
// read's bracket on (readBracket), and whether they say anything at all. Every moment it compares
// is one the process read on its own clock: the read's from and by, and the bounds the hold's
// lines carry. Each end of a period carries two bounds, one for each of the two answers, and
// nothing in between: the read is inside the period when its from is at or after the upper bound
// of the start (the took line's own time plus logResolution) and its by at or before the lower
// bound of the end (`stopped`, at or before the real stop), and outside it when its by is before
// the lower bound of the start (`started`, at or before the real start) or its from after the
// upper bound of the end (`stopped_by`, at or after the real stop). A read that falls across either
// edge decides nothing, since the answer describes one instant inside the bracket and the row
// cannot say which. Where the lines fall in the output decides nothing either: each is written
// after the read or the store it reports, so a read's line can come before an edge's line it
// follows, or after one it precedes.
//
// Reading the start from the line's own time alone would decide "not publishing" of a read that
// the process may honestly have answered true to: the line is written after Publishing() turned
// true, so its time can be after the real start by a millisecond plus however long the goroutine
// was pre-empted in between.
//
// Reading the end from `stopped` alone has the same fault from the other side: that attribute is
// the clock read taken before the store that ends the turn, capped at the hold's deadline, so on
// the paths that do not end at the deadline it is at or before the real stop, and a read
// answered in between would be decided "publishing" where the process honestly answered false.
// `stopped_by`, the read taken after that store and capped the same way, is at or after the real
// stop, and the stretch between the two goes undecided instead. On the paths that do end at the
// deadline - the ordinary ones - the two attributes are the same value, so nothing there becomes
// undecided, which is what keeps this assertion worth making: the stretch between a deadline and
// the line that reports it has been measured at 1.09 s, and every read in it is still decided.
//
// A period still open when the lines were read has no end line, so no pair of bounds: its line
// may still be coming, the stop before it. What bounds how far it ran is the latest read inside
// it that answered true (openPeriodsRan): Publishing() is one unbroken stretch per turn as holder
// (outbox.publish), so the process was publishing from the start up to that read's from, which is
// its `stopped` here. Nothing after the start of an open period is decided outside it.
func publishingAt(periods []publishingPeriod, read readBracket) (publishing, decided bool) {
	for _, period := range periods {
		began, inside := period.started, period.from.Add(logResolution)
		switch {
		case !read.from.Before(inside) && !read.by.After(period.stopped):
			return true, true
		case read.by.Before(began) || (!period.open && read.from.After(period.stoppedBy)):
			// This period says nothing about the read; another may.
		default:
			return false, false
		}
	}
	return false, true
}

// openPeriodsRan is periods with each one still open given, as its `stopped`, the from of the
// latest of reads inside it that answered true, and the zero time where none did (publishingAt):
// how far the lines and the process's own reads show it ran.
func openPeriodsRan(periods []publishingPeriod, reads []stampedRead) []publishingPeriod {
	ran := slices.Clone(periods)
	for index, period := range ran {
		if !period.open {
			continue
		}
		inside := period.from.Add(logResolution)
		period.stopped = time.Time{}
		for _, read := range reads {
			if read.publishing && !read.bracket.from.Before(inside) && read.bracket.from.After(period.stopped) {
				period.stopped = read.bracket.from
			}
		}
		ran[index] = period
	}
	return ran
}

// resends is what lawfulResends counted, one entry for each lawful resend: the line that records
// it, or the hand-over of the publisher hold that does.
type resends []string

// String is the resends one to a line, or "none".
func (r resends) String() string {
	if len(r) == 0 {
		return "none"
	}
	return strings.Join(r, "\n")
}

// roleDestination is how the outbox names role's route in the failures it logs: by the route
// itself, role:<role> (outbox's addRoute), as in `publish route "role:<role>": …`.
func roleDestination(role string) string { return "role:" + role }

// lawfulResends is every resend of event eventID - the outbox's event_id, which a role forward
// carries as its SourceEventID and a session frame as dispatch-<id> - to destination that the
// outbox's own lines record, one entry for each. destination is what the outbox names that
// destination by in the failures it logs: `publish route "role:…"` (roleDestination), `publish
// owner topic "notifications.dispatch.…"`, `publish author route to "ses_…"`. A batch records each
// destination an event reached, whoever holds the hold when it records them, and no later read
// sends the event there again (outbox's recordBatch, planChains), so the outbox sends an event to
// a destination again only where a batch that sent it there left that destination unrecorded, and
// each way that happens is one of these, each counted once:
//
//   - a back-off line of the event (eventBackedOff) whose error names the destination's send: that
//     destination stays unreached, the stream's ErrNotAcknowledged among the failures that leave
//     it so, and the event's next read sends to it again;
//   - a release line of the event (eventReleased): the event is read again at once for every
//     destination its batch did not reach, sends lost or unmade among them, and the line does not
//     name them, so it counts once for whichever destination is judged;
//   - a scan-failure line (scanFailed) whose event_ids hold the event: the batch that sent it
//     returned an error, its record among its statements, which leaves what it sent unrecorded;
//   - a hand-over of the publisher hold: a turn that ended, by its lost or released line, after a
//     sent batch line of its own naming the event, where the turn of either process that began
//     next after it began at or after its end, by the bounds the hold's own lines carry (the
//     end's stopped_by, the start's started). The ended turn's batch can be unrecorded when the
//     next turn reads the event - a batch the hold's deadline fenced is never recorded, and a
//     record can wait past the hand-over - so the next turn sends it again, once per ended turn.
//
// Nothing else counts: a line about another event or about another destination of this one, and
// a line that records no send to be made again - a destination refused, which counts as reached;
// a stand-in, which is that send reaching it; a forward its holder's resolution held, which never
// left the process (event waits for its role's holder); a fallback copy waiting behind an earlier
// event, whose event_id is that earlier event. Two processes publishing at once record none
// either: the turn that began next after another began before that one ended, which is no
// hand-over.
//
// Only lines at or after since count, and only hand-overs whose next turn began at or after it,
// for a row whose window of sends begins there; a row that passes one process's lines finds only
// that process's own hand-overs.
func lawfulResends(lines map[*dispatchProcess][]outboxLine, eventID, destination string, since time.Time) resends {
	from := since.Truncate(time.Millisecond)
	names := strconv.Quote(destination) + ": "
	// turn is one turn as holder and every line of the process whose turn it was.
	type turn struct {
		period publishingPeriod
		lines  []outboxLine
	}
	var counted []outboxLine
	var turns []turn
	for _, own := range lines {
		for _, line := range own {
			if line.at.Before(from) {
				continue
			}
			ofEvent := line.eventID == eventID
			if (ofEvent && line.message == eventBackedOff && strings.Contains(line.failure, names)) ||
				(ofEvent && line.message == eventReleased) ||
				(line.message == scanFailed && slices.Contains(line.eventIDs, eventID)) {
				counted = append(counted, line)
			}
		}
		for _, period := range publishingPeriods(own) {
			turns = append(turns, turn{period: period, lines: own})
		}
	}
	slices.SortFunc(counted, func(a, b outboxLine) int { return a.at.Compare(b.at) })
	var found resends
	for _, line := range counted {
		found = append(found, line.text)
	}
	slices.SortFunc(turns, func(a, b turn) int { return a.period.started.Compare(b.period.started) })
	for index, ended := range turns {
		// The hand-over leaves unrecorded only the batches the ended turn sent, so it counts only
		// for an event one of that turn's sent batch lines names.
		if ended.period.open || index+1 == len(turns) || !slices.ContainsFunc(ended.lines, func(line outboxLine) bool {
			return line.message == sentBatch && ended.period.holds(line.at) && slices.Contains(line.eventIDs, eventID)
		}) {
			continue
		}
		next := turns[index+1].period
		if next.started.Before(ended.period.stoppedBy) || next.started.Before(from) {
			continue
		}
		found = append(found, fmt.Sprintf("the publisher hold went from instance %s, whose turn ended by %s after a batch that sent the event, to instance %s, whose turn began next, at %s",
			ended.period.instance, ended.period.stoppedBy.Format(time.RFC3339Nano), next.instance, next.started.Format(time.RFC3339Nano)))
	}
	return found
}

// outboxScene is a project whose issues a row writes comments on through the pair's processes, and
// the three sessions those comments are addressed to:
//
//   - watcher follows every issue's topic family;
//   - holder holds role, to which the role issues are routed, and takes each forward with its
//     receipt;
//   - author wrote the root comment on each reply issue, and every comment the row writes there
//     replies to it, which reaches the author on its agent subject (an author route).
type outboxScene struct {
	pair       *dispatchPair
	rig        *sessionRig
	role       string
	issues     []string
	roleIssues map[string]bool
	// roots is each reply issue's root comment.
	roots                   map[string]string
	watcher, holder, author *testSession

	mu      sync.Mutex
	written []sceneWrite
}

// sceneWrite is one comment a row wrote, on issue, the write started at started.
type sceneWrite struct {
	commentID string
	issue     string
	started   time.Time
}

// newOutboxScene creates a project of issues issues through A: the first roleIssues routed to the
// scene's role, whose holder is the holder session, the next replyIssues carrying a root comment
// the author session wrote, the rest neither. It returns once every event of the setup is
// published.
func newOutboxScene(t *testing.T, pair *dispatchPair, issues, roleIssues, replyIssues int) *outboxScene {
	t.Helper()
	rig := newSessionRig(t, pair.nats)
	project := uniqueProjectKey(t, "OBX")
	name := strings.ToLower(project)
	scene := &outboxScene{pair: pair, rig: rig, role: "outbox-" + name, roleIssues: map[string]bool{}, roots: map[string]string{}}
	pair.a.createProject(t, project, "Outbox "+project)
	var families []string
	for index := range issues {
		key, _ := pair.a.createIssueIn(t, project, fmt.Sprintf("Outbox %d", index+1), "Outbox.")
		scene.issues = append(scene.issues, key)
		families = append(families, "notifications.dispatch.issue."+key+".>")
	}
	scene.watcher = newTestSession(t, rig, "ses_"+name+"_watcher", families...)
	holder, author := "ses_"+name+"_holder", "ses_"+name+"_author"
	scene.holder = newTestSession(t, rig, holder, contracts.LegacyAgentSubject(holder))
	scene.author = newTestSession(t, rig, author, contracts.LegacyAgentSubject(author))
	rig.registerHolder(t, scene.role, holder)
	for index, key := range scene.issues {
		switch {
		case index < roleIssues:
			scene.roleIssues[key] = true
			pair.a.call(t, http.MethodPatch, "/api/v1/issues/"+key,
				fmt.Sprintf(`{"route":"role:%s"}`, scene.role), http.StatusOK)
		case index < roleIssues+replyIssues:
			root := rig.writeComment(pair.a, key, map[string]any{"body": "The thread every comment here replies to."}, author)
			if root.err != nil {
				t.Fatalf("write the root comment on %s as %s: %v", key, author, root.err)
			}
			scene.roots[key] = root.commentID
		}
	}
	waitForEveryEventPublished(t, pair.database)
	return scene
}

// sessions is the scene's sessions.
func (s *outboxScene) sessions() []*testSession {
	return []*testSession{s.watcher, s.holder, s.author}
}

// write writes one comment on issue through p - a reply to the root comment on a reply issue, a
// comment of its own elsewhere - and records it.
func (s *outboxScene) write(p *dispatchProcess, issue, body string) error {
	fields := map[string]any{"body": body}
	if root, ok := s.roots[issue]; ok {
		fields["reply_to"] = root
	}
	started := time.Now()
	write := s.rig.writeComment(p, issue, fields, "")
	if write.err != nil {
		return write.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, sceneWrite{commentID: write.commentID, issue: issue, started: started})
	return nil
}

// writerFor is the process the index-th write of a round robin over issues goes through:
// consecutive writes alternate between writers, and so do each issue's.
func writerFor(index, issues int, writers []*dispatchProcess) *dispatchProcess {
	return writers[(index/issues+index%issues)%len(writers)]
}

// writeAll writes count comments, round robin over the scene's issues, through writers in turn
// (writerFor), sceneWritesAtOnce at a time, and fails the test for each write that failed. The
// writes and the wait for a free slot among them run inside the shared wait (testwait.Returns): a
// write that never returns holds its slot, so the one bound on the join bounds the slots too. Each
// write's request also ends at the shared wait's end (sessionRig.writes).
func (s *outboxScene) writeAll(t *testing.T, count int, writers ...*dispatchProcess) {
	t.Helper()
	failures := make(chan error, count)
	testwait.Returns(t, fmt.Sprintf("the %d comment writes through the processes to return", count), func() {
		slots := make(chan struct{}, sceneWritesAtOnce)
		var writes sync.WaitGroup
		for index := range count {
			slots <- struct{}{}
			writes.Go(func() {
				defer func() { <-slots }()
				issue := s.issues[index%len(s.issues)]
				if err := s.write(writerFor(index, len(s.issues), writers), issue, fmt.Sprintf("Comment %d.", index)); err != nil {
					failures <- err
				}
			})
		}
		writes.Wait()
	})
	close(failures)
	for err := range failures {
		t.Errorf("write a comment: %v", err)
	}
}

// writeAtRate writes a comment rate times a second, round robin over the scene's issues, through
// writers in turn (writerFor), until the returned stop is called. stop returns once the writing
// has stopped and fails the test for each write that failed; calling it again does nothing.
func (s *outboxScene) writeAtRate(t *testing.T, rate int, writers ...*dispatchProcess) (stop func()) {
	t.Helper()
	done, stopped := make(chan struct{}), make(chan struct{})
	var failures []error
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Second / time.Duration(rate))
		defer ticker.Stop()
		for index := 0; ; index++ {
			issue := s.issues[index%len(s.issues)]
			if err := s.write(writerFor(index, len(s.issues), writers), issue, fmt.Sprintf("Comment %d at %d a second.", index, rate)); err != nil {
				failures = append(failures, err)
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return sync.OnceFunc(func() {
		close(done)
		<-stopped
		for _, err := range failures {
			t.Errorf("write a comment: %v", err)
		}
	})
}

// writtenCount is how many comments the scene has written.
func (s *outboxScene) writtenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.written)
}

// addressed is each session's share of the written comments: every one for the watcher, those on
// a role issue for the holder, those on a reply issue for the author.
func (s *outboxScene) addressed() map[*testSession][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	addressed := map[*testSession][]string{}
	for _, write := range s.written {
		addressed[s.watcher] = append(addressed[s.watcher], write.commentID)
		if s.roleIssues[write.issue] {
			addressed[s.holder] = append(addressed[s.holder], write.commentID)
		}
		if _, ok := s.roots[write.issue]; ok {
			addressed[s.author] = append(addressed[s.author], write.commentID)
		}
	}
	return addressed
}

// deliveredCount is how many of the written comments the watcher has taken.
func (s *outboxScene) deliveredCount() int {
	taken := s.watcher.takenComments()
	count := 0
	for _, id := range s.addressed()[s.watcher] {
		if _, ok := taken[id]; ok {
			count++
		}
	}
	return count
}

// delivered reports whether every session has taken every written comment addressed to it.
func (s *outboxScene) delivered() bool {
	for session, ids := range s.addressed() {
		taken := session.takenComments()
		for _, id := range ids {
			if _, ok := taken[id]; !ok {
				return false
			}
		}
	}
	return true
}

// waitDelivered waits for every session to take every written comment addressed to it, and fails
// the test naming what each has not taken when they do not.
func (s *outboxScene) waitDelivered(t *testing.T) {
	t.Helper()
	testwait.Until(t, "every session to take every comment written to it", func() (string, bool) {
		if s.delivered() {
			return "", true
		}
		var missing []string
		for session, ids := range s.addressed() {
			taken := session.takenComments()
			for _, id := range ids {
				if _, ok := taken[id]; !ok {
					missing = append(missing, session.id+" has not taken "+id)
				}
			}
		}
		return strings.Join(missing, "; "), false
	})
}

// requireOnceAndInOrder requires each session to have taken each written comment addressed to it,
// once, and every event it took in its owner's seq order.
func (s *outboxScene) requireOnceAndInOrder(t *testing.T) {
	t.Helper()
	addressed := s.addressed()
	for _, session := range s.sessions() {
		session.requireTakenOnce(t, addressed[session])
		session.requireInOrder(t)
	}
}

// firstArrivalSince is the first time any session took a comment whose write started at or after
// at, and false while none has.
func (s *outboxScene) firstArrivalSince(at time.Time) (time.Time, bool) {
	s.mu.Lock()
	since := map[string]bool{}
	for _, write := range s.written {
		if !write.started.Before(at) {
			since[write.commentID] = true
		}
	}
	s.mu.Unlock()
	var first time.Time
	for _, session := range s.sessions() {
		for id, frame := range session.takenComments() {
			if since[id] && (first.IsZero() || frame.at.Before(first)) {
				first = frame.at
			}
		}
	}
	return first, !first.IsZero()
}

// framesSince is every frame, repeats included, that reached a session at or
// after at.
func (s *outboxScene) framesSince(at time.Time) map[*testSession][]sessionFrame {
	frames := map[*testSession][]sessionFrame{}
	for _, session := range s.sessions() {
		for _, frame := range session.allFrames() {
			if !frame.at.Before(at) {
				frames[session] = append(frames[session], frame)
			}
		}
	}
	return frames
}

// destination is what the outbox names the destination by, in the failures it logs, through
// which session received frame (lawfulResends): the scene's role route for the holder, whose agent
// subject only the role's forwards reach; the author's own session for the author, whose agent
// subject the author route of each reply reaches (outbox's addAuthorRoutes, which names that
// route by the session); and the owner's topic the frame came on for the watcher, which follows
// the issues' topic families alone (outbox's eventDestinations, which names that topic by its
// subject).
func (s *outboxScene) destination(session *testSession, frame sessionFrame) string {
	switch session {
	case s.holder:
		return roleDestination(s.role)
	case s.author:
		return session.id
	default:
		return frame.subject
	}
}

// requireTakeOver waits for p to take the publisher hold at or after since, then to log its first
// batch, and for a session to take a comment written since since; and, on the clock
// (benchtime.OnTheClock), requires each within takeOverWithin of since, the moment what happened
// to the publisher before p.
func (s *outboxScene) requireTakeOver(t *testing.T, lane benchtime.Lane, p *dispatchProcess, since time.Time, what string) {
	t.Helper()
	took := p.waitForOutboxLine(t, tookHold, since)
	batch := p.waitForOutboxLine(t, sentBatch, took.at)
	var arrived time.Time
	testwait.Eventually(t, "a session taking a comment written after "+what, func() bool {
		var ok bool
		arrived, ok = s.firstArrivalSince(since)
		return ok
	})

	t.Logf("after %s, the other process took the publisher hold in %v and logged its first batch in %v, and a session took a comment written since in %v",
		what, took.at.Sub(since).Round(time.Millisecond), batch.at.Sub(since).Round(time.Millisecond), arrived.Sub(since).Round(time.Millisecond))
	for _, step := range []struct {
		what string
		at   time.Time
	}{
		{"the other process took the publisher hold", took.at},
		{"the other process logged its first batch", batch.at},
		{"a session took a comment written since", arrived},
	} {
		if after := step.at.Sub(since); lane == benchtime.OnTheClock && after > takeOverWithin {
			t.Errorf("%s %v after %s, past the %v bound", step.what, after.Round(time.Millisecond), what, takeOverWithin)
		}
	}
}

// requireRunning fails the test for each process of the pair that has exited.
func (p *dispatchPair) requireRunning(t *testing.T) {
	t.Helper()
	for _, process := range []*dispatchProcess{p.a, p.b} {
		select {
		case <-process.Exited:
			t.Errorf("process %s exited:\n%s", p.name(process), process.Output.String())
		default:
		}
	}
}

// readBracket is the two moments a process stamped one read of whether it publishes the outbox
// between (stampedPublisher in main.go, which DISPATCH_TEST_HEALTHZ_PUBLISHER_READS turns on):
// from, read before its read of Publishing(), and by, read after it, both on the clock the
// process's outbox lines carry their own moments on. The answer describes one instant in there
// and nowhere narrower, so a read is compared with the publishing periods as a span, and one that
// straddles an edge of them decides nothing. Both ends are the process's own record: the moments
// a reader of /healthz could take itself would put the database probe and the transport inside
// the span, and the reader's clock beside the process's.
type readBracket struct{ from, by time.Time }

// stampedRead is one read the process's /healthz made of whether it publishes the outbox, as the
// process logged it: what it answered - the outbox_publisher that response served - the bracket it
// stamped the read in, and the line.
type stampedRead struct {
	publishing bool
	bracket    readBracket
	text       string
}

// publisherReadsOf is every read the process's /healthz has made of its outbox publisher, as the
// process logged it (publisherReadLine), in the order it wrote them. Only whole lines are read, for
// the reason outboxLines gives; a line that carries no answer or no moment fails the row, since
// the agreement with the log reads exactly those.
func publisherReadsOf(t *testing.T, p *dispatchProcess) []stampedRead {
	t.Helper()
	var reads []stampedRead
	for raw := range strings.Lines(p.Output.String()) {
		text, whole := strings.CutSuffix(raw, "\n")
		if !whole || !strings.Contains(text, fmt.Sprintf("msg=%q", publisherReadLine)) {
			continue
		}
		read := stampedRead{text: text}
		switch answer := logAttributeOf(t, text, "publishing"); answer {
		case "true":
			read.publishing = true
		case "false":
		default:
			t.Fatalf("the line %q answers publishing=%s, want true or false", text, answer)
		}
		for key, moment := range map[string]*time.Time{"read_from": &read.bracket.from, "read_by": &read.bracket.by} {
			at, err := time.Parse(time.RFC3339Nano, logAttributeOf(t, text, key))
			if err != nil {
				t.Fatalf("read the %s of the line %q: %v", key, text, err)
			}
			*moment = at
		}
		reads = append(reads, read)
	}
	return reads
}

// publisherRead is one process's read of /healthz inside a sample: what it answered, and what kept
// it from answering.
type publisherRead struct {
	report healthReport
	err    error
}

// publisherSample is one read of both processes' /healthz, after its time into the sampling, each
// process's read under the process itself, and err what kept either of them from answering. at is
// when the sample's two reads went out, which were sent together, on this test's clock: the
// stretches with no publisher that benchtime's timing lane holds to takeOverWithin are measured
// from it (requireOnePublisherOverTheSamples), and nothing a row holds in every run reads it.
type publisherSample struct {
	after time.Duration
	at    time.Time
	reads map[*dispatchProcess]publisherRead
	err   error
}

// sampleOutboxPublisher reads both processes' /healthz every 100 ms, the two at once, each read
// under ctx (readHealthReport), until stop is closed or ctx ends: ctx is the shared wait's
// (testwait.Context), which ends with the test, so a row that fails before it closes stop leaves
// no sampler running. It returns a channel it closes once its first sample is taken, and the func
// that waits for the last read and answers them all.
func sampleOutboxPublisher(ctx context.Context, pair *dispatchPair, stop <-chan struct{}) (<-chan struct{}, func() []publisherSample) {
	started := time.Now()
	first := make(chan struct{})
	done := make(chan []publisherSample, 1)
	go func() {
		processes := []*dispatchProcess{pair.a, pair.b}
		var samples []publisherSample
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				done <- samples
				return
			case <-ctx.Done():
				done <- samples
				return
			case <-tick.C:
			}
			// Each read fills a slot of its own and the map is built once both have returned, so
			// the two goroutines write nothing in common.
			at := time.Now()
			got := make([]publisherRead, len(processes))
			var reads sync.WaitGroup
			for i, process := range processes {
				reads.Go(func() {
					report, err := process.readHealthReport(ctx)
					got[i] = publisherRead{report: report, err: err}
				})
			}
			reads.Wait()
			sample := publisherSample{after: time.Since(started), at: at, reads: make(map[*dispatchProcess]publisherRead, len(processes))}
			for i, process := range processes {
				sample.reads[process] = got[i]
				sample.err = errors.Join(sample.err, got[i].err)
			}
			samples = append(samples, sample)
			if len(samples) == 1 {
				close(first)
			}
		}
	}()
	return first, func() []publisherSample { return <-done }
}

// startOutboxPair is startPair for a row about the outbox's publisher, which logs both processes'
// output when the row fails: a hand-over is told from both sides. Each process logs every read its
// /healthz makes of the publisher with the moments it made it between
// (DISPATCH_TEST_HEALTHZ_PUBLISHER_READS, publisherReadsOf), which is what a row judges /healthz
// against the outbox's own lines by (requireHealthAgreesWithTheLog).
func startOutboxPair(t *testing.T) *dispatchPair {
	t.Helper()
	pair := startPair(t, map[string]string{"DISPATCH_TEST_HEALTHZ_PUBLISHER_READS": "1"})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("A's output:\n%s\nB's output:\n%s", pair.a.Output.String(), pair.b.Output.String())
		}
	})
	return pair
}

// Two processes run an outbox each, and one of them at a time holds the publisher hold. What this
// row proves is dispatch://INFRA-590's promise as the product makes it, read on /healthz every
// 100 ms on both and against each process's own outbox lines:
//
//   - never two publishers, at any sample and over the periods the lines show;
//   - in the row's twin in benchtime's timing lane (benchtime.OnTheClock), no stretch without a
//     publisher longer than takeOverWithin, the bound the kill and NATS rows hold a take-over to
//     there;
//   - the run ends with exactly one publisher, waited for after the last sample rather than
//     demanded of that sample, and that wait counted into a stretch with no publisher that the
//     last sample was still inside rather than granted on top of it, which is why it is waited
//     for before the writers are stopped;
//   - /healthz agrees with the outbox log at every read it can decide, each read and each edge
//     judged on the moments the process itself stamped them with (readBracket);
//   - each process's lines name its one instance, and every batch either logged was started and
//     finished inside one of its own periods.
//
// The sessions take every comment written meanwhile once and in order.
//
// What it does not assert is that a publisher exists at every sample from some moment on. A lease
// cannot promise that: each time a renewal is late the holder must stop at its loss deadline, and
// no other process may take the name until the row lapses, which is Margin later at the earliest.
// The row asserted it until dispatch://INFRA-1568, and a 3 s stall of the runner shared by one
// process's write and its renewal took the job red with both the product and the hold behaving as
// designed (job 128324773698, 2026-11-07). The take-over bound is what the product promises, and
// it is what the first assertion holds the gap to.
func TestOneProcessPublishesTheOutboxAtATime(t *testing.T) {
	oneProcessPublishesTheOutboxAtATime(t, benchtime.EveryRun)
}

// TestOneProcessPublishesTheOutboxAtATimeOnTheClock is the row above in benchtime's timing lane,
// where no stretch without a publisher may reach takeOverWithin.
func TestOneProcessPublishesTheOutboxAtATimeOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), oneProcessPublishesTheOutboxAtATime)
}

// oneProcessPublishesTheOutboxAtATime is the scene of the two rows above; lane says whether its
// readings of the clock are held to their figures (benchtime.Lane).
func oneProcessPublishesTheOutboxAtATime(t *testing.T, lane benchtime.Lane) {
	// samplingSpan is how long /healthz is read on both processes. It outruns the take-over bound
	// the gaps are judged against, so a gap that breaks the promise is seen whole.
	const samplingSpan = 12 * time.Second
	pair := startOutboxPair(t)
	stopSampling := make(chan struct{})
	sampledOnce, samples := sampleOutboxPublisher(testwait.Context(t), pair, stopSampling)
	scene := newOutboxScene(t, pair, 3, 1, 1)
	stop := scene.writeAtRate(t, 4, pair.a, pair.b)
	// Deferred so that a t.Fatalf below - the wait for a publisher is one of the failures this
	// row exists to catch - stops the writers too, rather than leaving the goroutine posting to
	// stopped processes until the binary exits with its write failures reported to nobody. stop
	// does nothing when it is called again, so the call after the wait stands as it is.
	defer stop()
	// What the window must hold for the assertions below to read anything is waited for first: a
	// sample of both processes' /healthz, and a batch either process logged it sent.
	testwait.Receive(t, "the sampler's first read of both processes' /healthz", sampledOnce)
	testwait.Eventually(t, "either process to log "+strconv.Quote(outboxLogPrefix+sentBatch), func() bool {
		for _, process := range []*dispatchProcess{pair.a, pair.b} {
			if slices.ContainsFunc(process.outboxLines(t), func(line outboxLine) bool { return line.message == sentBatch }) {
				return true
			}
		}
		return false
	})
	testwait.Span(t, samplingSpan, "this row's subject is a stretch with no publisher, read off /healthz on both "+
		"processes while they write: nothing either process records marks such a stretch's end, both are child "+
		"processes so synctest does not reach them, and an action of the test's would end the very hand-over it "+
		"is watching, so the window it watches for is its own")
	close(stopSampling)
	sampled := samples()
	// The run ends with exactly one publisher, and the wait for it is the product's own bound: a
	// renewal late over the final 100 ms of the span stops its holder there, and what INFRA-590
	// promises then is a take-over within takeOverWithin, not a publisher at that one sample. The
	// wait starts here, a step after the last read, and it is that bound and no more: when the
	// last sample saw no publisher, the stretch that sample is inside is measured up to the
	// moment the read of this wait that saw a publisher was issued
	// (requireOnePublisherOverTheSamples), so the stretch and the wait are held to one bound
	// between them rather than to one each.
	//
	// It comes before stop(), which waits for the writer goroutine and for the write it has in
	// flight - the request measured at 3.1 s in the job this row was fixed from
	// (dispatch://INFRA-1568) - because every moment between the last sample and the read that
	// finds a publisher is counted as a stretch with no publisher. Stopping the writers first
	// put that wait inside the stretch and failed a product that had a publisher back at once.
	_, sawPublisher := waitForPublisher(t, pair)
	stop()

	// held and logged are each process's publishing periods and the lines they were read from,
	// kept so that the assertions below and the failures they print work from one parse. reads,
	// each process's own record of every read its /healthz made of its publisher, is parsed
	// first, so no read is judged that was made after the lines were read: the sampler and the
	// wait above made the last of them before stop(), whose wait for the writer's last write
	// stands between them and the lines.
	reads := stampedReadsOf(t, pair)
	held := map[*dispatchProcess][]publishingPeriod{}
	logged := map[*dispatchProcess][]outboxLine{pair.a: pair.a.outboxLines(t), pair.b: pair.b.outboxLines(t)}
	// A period still open in the lines runs as far as its own process's record shows it
	// publishing - its batches and its stamped reads that answered true (openPeriodsRanTo) - and
	// no further: a moment of the test's own clock would run it out to whenever the test read the
	// lines, past anything the process recorded.
	owners := map[string]*dispatchProcess{}
	batches := 0
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		lines := logged[process]
		instances := map[string]bool{}
		for _, line := range lines {
			// Only the four messages this row reads are kept, and every one of them carries its
			// process's instance. The outbox messages that name no process - a refused
			// destination, a released event, a batch the hold's deadline overtook, and the record
			// that found the hold another's, which is the line the take-over path writes - are not
			// among them, so an empty instance on one of these four is a line that dropped the
			// attribute and is worth a failure of its own: a released or lost line that drops it
			// passes unseen otherwise, since a period takes its instance from the took line alone.
			// What the bookkeeping itself holds is that no line in this process's output names the
			// other process's instance.
			switch line.message {
			case tookHold, releasedHold, lostHold, sentBatch:
			default:
				continue
			}
			if line.instance == "" {
				// Left out of the bookkeeping below, where the empty name would read as the two
				// processes sharing an instance and bury this failure under that one.
				t.Errorf("process %s logged %q with no instance: %s", pair.name(process), line.message, line.text)
				continue
			}
			instances[line.instance] = true
			if owner, ok := owners[line.instance]; ok && owner != process {
				t.Errorf("both processes logged lines of instance %q", line.instance)
			}
			owners[line.instance] = process
		}
		// None is right for a process that never held the hold: it logs no outbox line at all,
		// which is what the job this issue came from shows of its standby.
		if len(instances) > 1 {
			t.Errorf("process %s's outbox lines name the instances %v, want its one", pair.name(process), slices.Collect(maps.Keys(instances)))
		}
		own := openPeriodsRanTo(publishingPeriods(lines), lines, reads[process])
		held[process] = own
		for _, line := range lines {
			if line.message != sentBatch {
				continue
			}
			batches++
			inside := slices.ContainsFunc(own, func(period publishingPeriod) bool {
				return period.instance == line.instance && period.holds(line.started) && period.holds(line.at)
			})
			if line.started.IsZero() || !inside {
				t.Errorf("process %s logged a batch outside every period it held the publisher hold: %s", pair.name(process), line.text)
			}
		}
	}
	for _, one := range held[pair.a] {
		for _, other := range held[pair.b] {
			if one.from.Before(other.end()) && other.from.Before(one.end()) {
				t.Errorf("process %s held the publisher hold from %s to %s, and process %s from %s to %s",
					pair.name(pair.a), one.from.Format(time.RFC3339Nano), one.end().Format(time.RFC3339Nano),
					pair.name(pair.b), other.from.Format(time.RFC3339Nano), other.end().Format(time.RFC3339Nano))
			}
		}
	}
	if batches == 0 {
		t.Errorf("neither process logged %q", outboxLogPrefix+sentBatch)
	}

	if len(sampled) == 0 {
		t.Fatalf("the sampler read /healthz not once in %v", samplingSpan)
	}
	if reached := sampled[len(sampled)-1].after; lane == benchtime.OnTheClock && reached < takeOverWithin {
		t.Fatalf("the %d reads of /healthz reached only %v in, short of the %v take-over bound the row judges gaps against",
			len(sampled), reached.Round(time.Millisecond), takeOverWithin)
	}
	requireOnePublisherOverTheSamples(t, lane, pair, logged, sampled, sawPublisher)
	requireHealthAgreesWithTheLog(t, pair, held, logged, reads)

	scene.waitDelivered(t)
	scene.requireOnceAndInOrder(t)
}

// requireOnePublisherOverTheSamples proves, over the samples alone, the two halves of INFRA-590's
// promise that /healthz can show: that no sample found both processes publishing, and, on the
// clock (benchtime.OnTheClock), that no stretch in which neither did reached takeOverWithin, the
// bound a take-over is held to. A read that failed is a failure of its own and ends the stretch it
// falls in, since it says nothing about what that process answered.
//
// sawPublisher is the moment the read of the row's own wait after the sampling that found a
// publisher again was issued, and it is what ends a stretch the sampling left open: such a
// stretch does not end at the last sample, it runs on past the end of the sampling.
func requireOnePublisherOverTheSamples(t *testing.T, lane benchtime.Lane, pair *dispatchPair,
	logged map[*dispatchProcess][]outboxLine, sampled []publisherSample, sawPublisher time.Time) {
	t.Helper()
	var run []publisherSample
	// endRun judges the run of samples with no publisher that has just ended and clears it. Every
	// run is measured the one way, from the moment its first sample's reads were issued to end,
	// the moment the next read that saw a publisher was issued: the closing sample's own at for
	// a run a later sample ended, and the row's wait after the sampling for the run the sampling
	// left open. Measuring the open run to its last sample instead would end it where the
	// sampling did and grant the wait's own takeOverWithin on top of it: a stretch of 6 s inside
	// the sampling and 8 s after it passed that way (dispatch://INFRA-1568), and the promise this
	// row holds the product to allows no such thing.
	endRun := func(end time.Time) {
		if len(run) == 0 {
			return
		}
		first := run[0]
		length := end.Sub(first.at)
		if length >= takeOverWithin && lane == benchtime.OnTheClock {
			t.Errorf("no process reported outbox_publisher true from %v to %v in (%v, at or past the %v take-over bound):\n%s",
				first.after.Round(time.Millisecond), (first.after + length).Round(time.Millisecond),
				length.Round(time.Millisecond), takeOverWithin,
				outboxLinesAround(pair, logged, first.at.Add(-time.Second), end.Add(time.Second)))
		}
		run = nil
	}
	for _, sample := range sampled {
		// A run an errored read or a two-publisher sample ends has no next read that saw a
		// publisher, so it ends at that sample's own at. Nothing rests on where exactly past
		// that sample the stretch ran: the row already fails on both of those samples, each with
		// a message of its own, so the stretch's exact length decides nothing further there.
		if sample.err != nil {
			t.Errorf("the read of /healthz %v in failed: %v", sample.after.Round(time.Millisecond), sample.err)
			endRun(sample.at)
			continue
		}
		publishing := 0
		for _, read := range sample.reads {
			if read.report.publishing() {
				publishing++
			}
		}
		switch {
		case publishing > 1:
			t.Errorf("/healthz %v in: both processes report outbox_publisher true", sample.after.Round(time.Millisecond))
			endRun(sample.at)
		case publishing == 0:
			run = append(run, sample)
		default:
			endRun(sample.at)
		}
	}
	endRun(sawPublisher)
}

// requireHealthAgreesWithTheLog proves that each process's /healthz answered what that process's
// own outbox lines say it was doing, at every read of reads - each process's stamped reads of its
// publisher (publisherReadsOf), the sampler's and every other /healthz read of the row alike - that
// those lines decide: one whose whole bracket falls inside a publishing period, or outside every
// one of them (publishingAt). Every moment compared is one the process read on its own clock. A
// read that straddles an edge decides nothing, since its answer describes one instant inside the
// bracket and the row cannot say which, and how many fell that way is logged so that a change
// leaving every read undecided - and this assertion vacuous - shows in the job's output rather
// than passing in silence. A row parses reads before the lines held and logged come from
// (stampedReadsOf), with no read in flight between the two parses, so no read it judges was made
// after the lines were read; a period still open in them runs as far as its own process's reads
// show (openPeriodsRan).
func requireHealthAgreesWithTheLog(t *testing.T, pair *dispatchPair, held map[*dispatchProcess][]publishingPeriod,
	logged map[*dispatchProcess][]outboxLine, reads map[*dispatchProcess][]stampedRead) {
	t.Helper()
	undecided := map[*dispatchProcess]int{}
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		periods := openPeriodsRan(held[process], reads[process])
		for _, read := range reads[process] {
			want, decided := publishingAt(periods, read.bracket)
			if !decided {
				undecided[process]++
				continue
			}
			if read.publishing != want {
				t.Errorf("process %s's /healthz read outbox_publisher %t from %s by %s, and its outbox lines say %t over the whole read: %s\n%s",
					pair.name(process), read.publishing, read.bracket.from.Format(time.RFC3339Nano), read.bracket.by.Format(time.RFC3339Nano),
					want, read.text, outboxLinesAround(pair, logged, read.bracket.from.Add(-time.Second), read.bracket.by.Add(time.Second)))
			}
		}
	}
	t.Logf("of A's %d and B's %d reads of /healthz, %d and %d straddled an edge of a publishing period and decided nothing",
		len(reads[pair.a]), len(reads[pair.b]), undecided[pair.a], undecided[pair.b])
}

// stampedReadsOf is each process of the pair's stamped reads of its publisher (publisherReadsOf),
// for a row to parse before the outbox lines it judges them against (requireHealthAgreesWithTheLog).
func stampedReadsOf(t *testing.T, pair *dispatchPair) map[*dispatchProcess][]stampedRead {
	t.Helper()
	return map[*dispatchProcess][]stampedRead{pair.a: publisherReadsOf(t, pair.a), pair.b: publisherReadsOf(t, pair.b)}
}

// outboxLinesAround is both processes' outbox lines from `from` to `to`, named by their process,
// for a failure a reader of the job log must be able to act on. It reads the parse the row
// already made rather than the processes' output again: the row can call it once per failing
// sample, and re-reading a live buffer there would copy and re-parse both whole logs each time.
// A line written after that parse is not lost, since startOutboxPair prints both processes'
// whole output when the row fails.
func outboxLinesAround(pair *dispatchPair, logged map[*dispatchProcess][]outboxLine, from, to time.Time) string {
	var out strings.Builder
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		for _, line := range logged[process] {
			if line.at.Before(from) || line.at.After(to) {
				continue
			}
			fmt.Fprintf(&out, "%s: %s\n", pair.name(process), line.text)
		}
	}
	if out.Len() == 0 {
		return fmt.Sprintf("neither process logged an outbox line between %s and %s",
			from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	}
	return out.String()
}

// 200 comments on 10 issues, written through both processes in turn and several at a time: four
// issues routed to a role, three replying to a session's thread, three neither. Whichever process
// publishes, every session takes each comment addressed to it once and each issue's events in seq
// order: the watcher on the issues' topic families, the role's holder through the outbox's forward
// with its receipt, the thread's author on its agent subject.
func TestEachSessionTakesEachEventOnceAndInOrderAcrossTwoProcesses(t *testing.T) {
	pair := startOutboxPair(t)
	scene := newOutboxScene(t, pair, 10, 4, 3)
	scene.writeAll(t, 200, pair.a, pair.b)
	scene.waitDelivered(t)
	scene.requireOnceAndInOrder(t)
	addressed := scene.addressed()
	t.Logf("the watcher took %d comments, the role's holder %d, the thread's author %d",
		len(addressed[scene.watcher]), len(addressed[scene.holder]), len(addressed[scene.author]))
}

// The publisher is killed while comments are written through the other process at 4 a second: the
// other process takes the publisher hold, logs its first batch, and a session takes a comment
// written after the kill, each within 10 s of it, and every session still takes each comment once
// and in order across the hand-over.
func TestAKilledPublisherIsReplacedWithin10Seconds(t *testing.T) {
	aKilledPublisherIsReplacedWithin10Seconds(t, benchtime.EveryRun)
}

// TestAKilledPublisherIsReplacedWithin10SecondsOnTheClock is the row above in benchtime's timing
// lane, where the take-over is held to takeOverWithin of the kill (requireTakeOver).
func TestAKilledPublisherIsReplacedWithin10SecondsOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), aKilledPublisherIsReplacedWithin10Seconds)
}

// aKilledPublisherIsReplacedWithin10Seconds is the scene of the two rows above; lane says whether
// its readings of the clock are held to their figures (benchtime.Lane).
func aKilledPublisherIsReplacedWithin10Seconds(t *testing.T, lane benchtime.Lane) {
	pair := startOutboxPair(t)
	scene := newOutboxScene(t, pair, 3, 1, 1)
	publisher, _ := waitForPublisher(t, pair)
	survivor := pair.other(publisher)
	stop := scene.writeAtRate(t, 4, survivor)
	defer stop()
	testwait.Eventually(t, "the publisher publishing the first comments", func() bool { return scene.deliveredCount() >= 4 })

	killed := publisher.signal(t, syscall.SIGKILL)
	publisher.WaitExit(t, "SIGKILL")
	scene.requireTakeOver(t, lane, survivor, killed, "the publisher was killed")
	written := scene.writtenCount()
	testwait.Eventually(t, "eight more comments written", func() bool { return scene.writtenCount() >= written+8 })
	stop()
	scene.waitDelivered(t)
	scene.requireOnceAndInOrder(t)
}

// restartedPublishesWithin bounds how long a restarted process takes, from its registration, to
// publish the events the process killed before it left unpublished: both on the database's clock,
// and held to it by the row's twin in benchtime's timing lane alone (benchtime.OnTheClock), as
// takeOverWithin is (outbox.publisherHoldOptions' promise; the standby's lapse timer on a fake is
// outbox.TestAStandbyTakesTheHoldOfAHolderThatStoppedRenewingOnceItsRowLapses).
const restartedPublishesWithin = 10 * time.Second

// The only process is killed while its batch carrying comment 1 of an issue is in flight - its NATS
// proxy frozen, so the envelope has left the process and reached nothing - with comment 2 of the
// same issue written behind it. Restarted on the same database and NATS, the process publishes
// both, comment 1 first: on the issue's topic family the stream holds comment 1 at a lower sequence
// than comment 2, a session following the issue takes comment 1 before comment 2, and neither is
// marked published before the restart and both are after the kill, on the database's clock: its
// now() read once the killed process had exited, and the published_at the restart stamps; in the
// timing lane, within 10 s of the restarted process's registration. The product does not promise
// that the restart publishes after it registers: its outbox starts before its registration does.
// What the killed process had in flight is dropped with its connection, as a dead host's sends
// that never reached the server are, so nothing of comment 1 reaches the stream but what the
// restarted process publishes.
func TestAPublisherKilledMidBatchAndRestartedPublishesTheIssuesCommentsInOrderWithin10Seconds(t *testing.T) {
	aPublisherKilledMidBatchAndRestartedPublishesTheIssuesCommentsInOrderWithin10Seconds(t, benchtime.EveryRun)
}

// TestAPublisherKilledMidBatchAndRestartedPublishesTheIssuesCommentsInOrderWithin10SecondsOnTheClock
// is the row above in benchtime's timing lane, where each comment is held to being marked
// published within restartedPublishesWithin of the restarted process's registration.
func TestAPublisherKilledMidBatchAndRestartedPublishesTheIssuesCommentsInOrderWithin10SecondsOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), aPublisherKilledMidBatchAndRestartedPublishesTheIssuesCommentsInOrderWithin10Seconds)
}

// aPublisherKilledMidBatchAndRestartedPublishesTheIssuesCommentsInOrderWithin10Seconds is the scene
// of the two rows above; lane says whether its readings of the clock are held to their figures
// (benchtime.Lane).
func aPublisherKilledMidBatchAndRestartedPublishesTheIssuesCommentsInOrderWithin10Seconds(t *testing.T, lane benchtime.Lane) {
	pair := newPair(t)
	proxy := testnats.StartProxy(t, pair.nats)
	tap := startNATSTap(t, proxy.URL())
	killed := startDispatchProcessWith(t, pair.databaseURL(t),
		dispatchOptions{NATSURL: tap.url, SigningKey: multiprocessSigningKey})
	var restarted *dispatchProcess
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("the killed process's output:\n%s", killed.Output.String())
		if restarted != nil {
			t.Logf("the restarted process's output:\n%s", restarted.Output.String())
		}
	})
	killed.waitHealthy(t)

	rig := newSessionRig(t, pair.nats)
	js, err := rig.conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	head := streamHead(t, js)
	project := uniqueProjectKey(t, "KMB")
	killed.createProject(t, project, "Killed mid-batch "+project)
	key, _ := killed.createIssueIn(t, project, "Killed mid-batch", "Killed mid-batch.")
	family := "notifications.dispatch.issue." + key + "."
	watcher := newTestSession(t, rig, "ses_"+strings.ToLower(project)+"_watcher", family+">")
	// The process publishes, and has nothing left to publish, when its NATS freezes.
	waitForEveryEventPublished(t, pair.database)

	proxy.Freeze()
	first := rig.postComment(killed, key, "Comment 1.")
	if first.err != nil {
		t.Fatalf("write comment 1: %v", first.err)
	}
	firstEvent := commentEventID(t, pair.database, first.commentID)
	testwait.Eventually(t, "the process sending comment 1's envelope", func() bool {
		return tap.hasSent(fmt.Sprintf(`"event_id":%q`, firstEvent))
	})

	second := rig.postComment(killed, key, "Comment 2.")
	if second.err != nil {
		t.Fatalf("write comment 2: %v", second.err)
	}
	secondEvent := commentEventID(t, pair.database, second.commentID)
	killed.signal(t, syscall.SIGKILL)
	killed.WaitExit(t, "SIGKILL")
	// The database's clock once the killed process has exited: nothing of that process stamps a
	// row after it, so a publication stamped later is the restart's.
	killedBy := testdb.Now(t, pair.database.Pool)
	proxy.Cut()
	proxy.Restore()
	// Nothing the killed process sent reached the stream, so neither comment is marked published
	// before the restart: what marks them is the restarted process.
	for _, event := range []string{firstEvent, secondEvent} {
		if at, ok := eventPublishedAt(t, pair.database, event); ok {
			t.Fatalf("%s was marked published at %s, before the restart: the killed process's batch was not dropped with its connection", event, at)
		}
	}

	restarted = pair.startProcess(t, nil)
	restarted.waitHealthy(t)
	started := latestProcessStart(t, pair.database)

	published := map[string]time.Time{}
	testwait.Eventually(t, "both comments marked published and taken by the session following the issue", func() bool {
		for _, event := range []string{firstEvent, secondEvent} {
			if at, ok := eventPublishedAt(t, pair.database, event); ok {
				published[event] = at
			}
		}
		taken := watcher.takenComments()
		_, hasFirst := taken[first.commentID]
		_, hasSecond := taken[second.commentID]
		return len(published) == 2 && hasFirst && hasSecond
	})

	sequences := map[string]uint64{}
	for _, message := range readStreamMessagesFrom(t, js, head) {
		if _, seen := sequences[message.item.EventID]; !seen && strings.HasPrefix(message.item.Topic, family) {
			sequences[message.item.EventID] = message.seq
		}
	}
	firstSeq, secondSeq := sequences[firstEvent], sequences[secondEvent]
	t.Logf("on the issue's topic family the stream holds comment 1 (%s) at sequence %d and comment 2 (%s) at %d",
		firstEvent, firstSeq, secondEvent, secondSeq)
	switch {
	case firstSeq == 0 || secondSeq == 0:
		t.Errorf("the stream does not hold both comments on the issue's topic family: comment 1 at sequence %d, comment 2 at %d", firstSeq, secondSeq)
	case secondSeq < firstSeq:
		t.Errorf("the stream holds comment 2 at sequence %d and comment 1 at %d: the restarted process published the issue's comments out of the order they were written",
			secondSeq, firstSeq)
	}

	taken := watcher.takenComments()
	firstTaken, secondTaken := taken[first.commentID], taken[second.commentID]
	if !secondTaken.at.After(firstTaken.at) {
		t.Errorf("the session following the issue took comment 2 (%s) at %s and comment 1 (%s) at %s: want comment 1 first",
			secondEvent, secondTaken.at.Format(time.RFC3339Nano), firstEvent, firstTaken.at.Format(time.RFC3339Nano))
	}
	watcher.requireInOrder(t)

	// Each comment is marked published by the restarted process, after the kill: both on the
	// database's clock, its now() once the killed process had exited and the stamp the restart's
	// publication wrote. How soon after its registration the restart publishes is the reading the
	// row's twin in the timing lane holds (benchtime.OnTheClock), on its records on that clock;
	// the product does not promise it publishes after it registers, since its outbox starts first.
	for _, comment := range []struct {
		name, event string
	}{{"comment 1", firstEvent}, {"comment 2", secondEvent}} {
		after := published[comment.event].Sub(started)
		t.Logf("%s (%s) was marked published %v after the restarted process registered", comment.name, comment.event, after.Round(time.Millisecond))
		if published[comment.event].Before(killedBy) {
			t.Errorf("%s (%s) was marked published at %s, before the killed process had exited at %s",
				comment.name, comment.event, published[comment.event].Format(time.RFC3339Nano), killedBy.Format(time.RFC3339Nano))
		}
		if lane == benchtime.OnTheClock && after > restartedPublishesWithin {
			t.Errorf("%s (%s) was marked published %v after the restarted process registered, past the %v bound",
				comment.name, comment.event, after.Round(time.Millisecond), restartedPublishesWithin)
		}
	}
}

// commentEventID is the event id, dispatch-<row>, of comment id's comment.created event.
func commentEventID(t *testing.T, database *store.Store, commentID string) string {
	t.Helper()
	var row int64
	if err := database.Pool.QueryRow(context.Background(),
		`select id from events where type = 'comment.created' and payload ->> 'id' = $1`, commentID).Scan(&row); err != nil {
		t.Fatalf("read the event row of comment %s: %v", commentID, err)
	}
	return fmt.Sprintf("dispatch-%d", row)
}

// eventPublishedAt is when the event of event id dispatch-<row> was marked published, and false
// while it is not.
func eventPublishedAt(t *testing.T, database *store.Store, eventID string) (time.Time, bool) {
	t.Helper()
	row, err := strconv.ParseInt(strings.TrimPrefix(eventID, "dispatch-"), 10, 64)
	if err != nil {
		t.Fatalf("read the event row of event id %q: %v", eventID, err)
	}
	var published *time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select published_at from events where id = $1`, row).Scan(&published); err != nil {
		t.Fatalf("read event %s's published_at: %v", eventID, err)
	}
	if published == nil {
		return time.Time{}, false
	}
	return *published, true
}

// latestProcessStart is when the latest Dispatch process to register started, on the database's
// clock (server_processes.started_at, which its registration stamps): a process's own record of
// its start, which its later writes' stamps are ordered against.
func latestProcessStart(t *testing.T, database *store.Store) time.Time {
	t.Helper()
	var started time.Time
	if err := database.Pool.QueryRow(context.Background(), `select max(started_at) from server_processes`).Scan(&started); err != nil {
		t.Fatalf("read the latest process's start: %v", err)
	}
	return started
}

// natsTap relays a process's NATS connections to the NATS URL it fronts and keeps every byte the
// process sent, so a row sees an envelope leave the process while a proxy behind the tap holds it
// back (testnats.Proxy.Freeze).
type natsTap struct {
	listener net.Listener
	target   string
	// url is the NATS URL of the tap's address: the target's, with the host and port the tap
	// listens on.
	url string

	mu   sync.Mutex
	sent []byte
}

// startNATSTap relays to the NATS URL target on a loopback port and address of its own
// (testport.LoopbackIP) until the test ends.
func startNATSTap(t *testing.T, target string) *natsTap {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the tap relays to: %v", target, err)
	}
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the NATS tap: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	tapped := *server
	tapped.Host = listener.Addr().String()
	tap := &natsTap{listener: listener, target: server.Host, url: tapped.String()}
	go tap.serve()
	return tap
}

// serve relays each connection the tap accepts until the tap's listener closes.
func (tap *natsTap) serve() {
	for {
		client, err := tap.listener.Accept()
		if err != nil {
			return
		}
		go tap.relay(client)
	}
}

// relay copies client to the target, keeping what it reads, and the target back to client, until
// either side ends; then it closes both.
func (tap *natsTap) relay(client net.Conn) {
	defer client.Close()
	server, err := net.Dial("tcp", tap.target)
	if err != nil {
		return
	}
	defer server.Close()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	_, _ = io.Copy(server, io.TeeReader(client, tap))
}

// Write keeps what the process sent.
func (tap *natsTap) Write(data []byte) (int, error) {
	tap.mu.Lock()
	defer tap.mu.Unlock()
	tap.sent = append(tap.sent, data...)
	return len(data), nil
}

// hasSent reports whether the process has sent text on any of its connections.
func (tap *natsTap) hasSent(text string) bool {
	tap.mu.Lock()
	defer tap.mu.Unlock()
	return bytes.Contains(tap.sent, []byte(text))
}

// The publisher loses NATS while comments are written through both processes at 4 a second, and
// the other process, whose NATS works, takes the publisher hold, logs its first batch and has a
// comment written since reach a session, each within 10 s. First its proxy cuts it off, which
// closes its connection; then, with the roles swapped, the new publisher's proxy freezes, which
// leaves its connection open and silent. Either way the publisher releases the hold for NATS lost,
// and every session takes each comment once and in order.
func TestAPublisherCutFromNATSHandsOverWithin10Seconds(t *testing.T) {
	aPublisherCutFromNATSHandsOverWithin10Seconds(t, benchtime.EveryRun)
}

// TestAPublisherCutFromNATSHandsOverWithin10SecondsOnTheClock is the row above in benchtime's
// timing lane, where each hand-over is held to takeOverWithin of the cut (requireTakeOver).
func TestAPublisherCutFromNATSHandsOverWithin10SecondsOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), aPublisherCutFromNATSHandsOverWithin10Seconds)
}

// aPublisherCutFromNATSHandsOverWithin10Seconds is the scene of the two rows above; lane says
// whether its readings of the clock are held to their figures (benchtime.Lane).
func aPublisherCutFromNATSHandsOverWithin10Seconds(t *testing.T, lane benchtime.Lane) {
	pair := startOutboxPair(t)
	scene := newOutboxScene(t, pair, 3, 1, 1)
	t.Run("cut", func(t *testing.T) {
		publisher, _ := waitForPublisher(t, pair)
		proxy := pair.proxy(publisher)
		defer proxy.Restore()
		scene.handOver(t, lane, publisher, "its NATS connection was cut", proxy.Cut)
	})
	if t.Failed() {
		return
	}
	t.Run("silent cut", func(t *testing.T) {
		publisher, _ := waitForPublisher(t, pair)
		standby := pair.other(publisher)
		reading := testwait.Context(t)
		testwait.Until(t, "the standby reconnecting to NATS", func() (reportRead, bool) {
			report, err := standby.readHealthReport(reading)
			return reportRead{report, err}, err == nil && report.NATS != nil && *report.NATS
		})

		proxy := pair.proxy(publisher)
		defer proxy.Restore()
		scene.handOver(t, lane, publisher, "its NATS connection went silent", proxy.Freeze)
	})
}

// handOver writes comments at 4 a second through both processes and, once they flow, runs cut on
// the publisher's NATS. It requires the other process to take over, within takeOverWithin on the
// clock (requireTakeOver), the publisher to release the hold for NATS lost, and every session to
// take each comment once and in order.
func (s *outboxScene) handOver(t *testing.T, lane benchtime.Lane, publisher *dispatchProcess, what string, cut func()) {
	t.Helper()
	before := s.deliveredCount()
	stop := s.writeAtRate(t, 4, s.pair.a, s.pair.b)
	defer stop()
	testwait.Eventually(t, "the publisher publishing comments", func() bool { return s.deliveredCount() >= before+4 })

	at := time.Now()
	cut()
	s.requireTakeOver(t, lane, s.pair.other(publisher), at, what)
	released := publisher.waitForOutboxLine(t, releasedHold, at)
	if released.reason != "NATS lost" {
		t.Errorf("the publisher released the hold for %q after %s, want NATS lost: %s", released.reason, what, released.text)
	}
	written := s.writtenCount()
	testwait.Eventually(t, "eight more comments written", func() bool { return s.writtenCount() >= written+8 })
	stop()
	s.waitDelivered(t)
	s.requireOnceAndInOrder(t)
}

// The publisher is stopped (SIGSTOP) for 10 s while comments are written through the other
// process, which takes the publisher hold and publishes them. Once they are all delivered, the
// stopped process is continued: its hold's deadline passed while it was stopped, so it logs that it
// lost the hold and sends nothing - no session receives anything after it resumes, it logs no batch
// started after it was stopped, and it neither takes the hold again nor releases a hold that is no
// longer its own. Each of those is judged while the standby's own lines show it still holding the
// hold it took over: a loaded machine can end that turn lawfully, a renewal late past the hold's
// deadline, and the hold may then go to either process (turnEndAfter).
func TestAFrozenPublisherSendsNothingWhenItResumes(t *testing.T) {
	aFrozenPublisherSendsNothingWhenItResumes(t, benchtime.EveryRun)
}

// TestAFrozenPublisherSendsNothingWhenItResumesOnTheClock is the row above in benchtime's timing
// lane, where the take-over of the stopped publisher is held to takeOverWithin (requireTakeOver).
func TestAFrozenPublisherSendsNothingWhenItResumesOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), aFrozenPublisherSendsNothingWhenItResumes)
}

// aFrozenPublisherSendsNothingWhenItResumes is the scene of the two rows above; lane says whether
// its readings of the clock are held to their figures (benchtime.Lane).
func aFrozenPublisherSendsNothingWhenItResumes(t *testing.T, lane benchtime.Lane) {
	pair := startOutboxPair(t)
	scene := newOutboxScene(t, pair, 3, 1, 1)
	publisher, _ := waitForPublisher(t, pair)
	standby := pair.other(publisher)
	for index := range 4 {
		if err := scene.write(standby, scene.issues[index%len(scene.issues)], fmt.Sprintf("Before the stop %d.", index)); err != nil {
			t.Fatalf("write a comment: %v", err)
		}
	}
	// The publisher is idle when it is stopped: everything written so far has reached its sessions.
	scene.waitDelivered(t)

	// The loss its resume finds is the publisher's next lost line: the line that follows the stop
	// in the order the process writes its lines (waitForNextOutboxLine), read off no moment of this
	// test's.
	lostBefore := countOutboxLines(publisher.outboxLines(t), lostHold)
	stopped := publisher.signal(t, syscall.SIGSTOP)
	stop := scene.writeAtRate(t, 4, standby)
	defer stop()
	scene.requireTakeOver(t, lane, standby, stopped, "the publisher was stopped")
	// The standby's took line of the take-over, which begins the turn every verdict below is
	// judged inside.
	took := standby.waitForOutboxLine(t, tookHold, stopped)
	testwait.Span(t, time.Until(stopped.Add(frozenFor)), "the publisher is held stopped for a span of this row's own: "+
		"what it proves is that a process frozen past its hold's deadline sends nothing when it resumes, and nothing "+
		"a stopped process writes can mark the end of its own freeze")
	stop()
	scene.waitDelivered(t)

	// Both processes' /healthz are read from just before the publisher resumes until it has
	// logged its loss, so the reads the resumed process makes between its resume and the store that
	// ends its turn - the stretch between a deadline and the line that reports it, measured at
	// 1.09 s - are among those judged against its lines below.
	stopSampling := make(chan struct{})
	_, samples := sampleOutboxPublisher(testwait.Context(t), pair, stopSampling)
	resumed := publisher.signal(t, syscall.SIGCONT)
	lost := publisher.waitForNextOutboxLine(t, lostHold, lostBefore)
	close(stopSampling)
	for _, sample := range samples() {
		if sample.err != nil {
			t.Errorf("the read of /healthz %v into the resume failed: %v", sample.after.Round(time.Millisecond), sample.err)
		}
	}
	t.Logf("the resumed process logged that it lost the hold %v after the hold's deadline %s, which passed while it was stopped",
		lost.at.Sub(lost.stopped).Round(time.Millisecond), lost.stopped.Format(time.RFC3339Nano))
	testwait.Span(t, resumedQuiet, "the resumed process must send nothing at all, and a process that sends nothing "+
		"writes no state to read, is a child process so synctest does not reach it, and would answer a later action "+
		"of the test's with work of its own rather than with the silence this row reads")

	reads := stampedReadsOf(t, pair)
	logged := map[*dispatchProcess][]outboxLine{publisher: publisher.outboxLines(t), standby: standby.outboxLines(t)}
	// The standby's turn as holder, from its take-over to the line that ended it if its lines
	// show one. A moment at or after that end's stopped (at or before the real end) is past the
	// turn, when the hold may lawfully be either process's, and nothing below judges it.
	turnEnd, turnEnded := turnEndAfter(logged[standby], took)
	pastTheTurn := func(at time.Time) bool { return turnEnded && !at.Before(turnEnd.stopped) }
	if turnEnded {
		t.Logf("the standby's turn as holder, from its take-over, ended: %s", turnEnd.text)
	}
	// Every comment had reached every session it is addressed to before the stopped publisher
	// resumed (waitDelivered), so each frame a session received since is a send again of an event
	// it had taken: lawful only as one of the resends to that session the standby's own lines since
	// its take-over record (lawfulResends), one frame for each, and with none recorded a failure
	// whatever its count.
	for session, frames := range scene.framesSince(resumed) {
		var events []string
		sent := map[string][]sessionFrame{}
		for _, frame := range frames {
			if pastTheTurn(frame.at) {
				t.Logf("session %s received %s's event %s (%s) %v after the stopped publisher resumed, once the standby's turn as holder had ended",
					session.id, frame.owner, frame.event, frame.kind, frame.at.Sub(resumed).Round(time.Millisecond))
				continue
			}
			if _, seen := sent[frame.event]; !seen {
				events = append(events, frame.event)
			}
			sent[frame.event] = append(sent[frame.event], frame)
		}
		for _, event := range events {
			again, first := len(sent[event]), sent[event][0]
			counted := lawfulResends(map[*dispatchProcess][]outboxLine{standby: logged[standby]},
				strings.TrimPrefix(event, "dispatch-"), scene.destination(session, first), took.at)
			if again <= len(counted) {
				t.Logf("session %s received %s's event %s (%s) %d times after the stopped publisher resumed, the first %v after it, within the %d resends to it the standby's own lines record:\n%s",
					session.id, first.owner, event, first.kind, again, first.at.Sub(resumed).Round(time.Millisecond), len(counted), counted)
				continue
			}
			t.Errorf("session %s received %s's event %s (%s) %d times after the stopped publisher resumed, the first %v after it, with nothing left to send but what the resumed process sent: the standby's own lines since its take-over record %d resends of it to that session:\n%s",
				session.id, first.owner, event, first.kind, again, first.at.Sub(resumed).Round(time.Millisecond), len(counted), counted)
		}
	}
	// What the resumed process logged is judged on its own lines' moments alone. Its turn as
	// holder ended at the hold's deadline, which passed while it was stopped, and the lost line
	// that follows the stop says so: its stopped and stopped_by are that deadline (outbox.publish
	// caps both at it). A batch whose first send is after it was started once the turn was over,
	// and a line written at or after it was written once the process had resumed, since a stopped
	// process writes nothing. A took line and a batch are placed by the start they carry, a
	// released line by its own moment, against the standby's turn.
	for _, line := range logged[publisher] {
		moment := line.started
		if moment.IsZero() {
			moment = line.at
		}
		switch {
		case pastTheTurn(moment):
		case line.message == sentBatch && line.started.After(lost.stopped):
			t.Errorf("the stopped publisher logged a batch started after its turn as holder ended at the hold's deadline %s: %s",
				lost.stopped.Format(time.RFC3339Nano), line.text)
		case (line.message == tookHold || line.message == releasedHold) && !line.at.Before(lost.stopped.Truncate(time.Millisecond)):
			t.Errorf("the resumed process logged %q at or after the hold's deadline %s, which passed while it was stopped: %s",
				line.message, lost.stopped.Format(time.RFC3339Nano), line.text)
		}
	}
	held := map[*dispatchProcess][]publishingPeriod{publisher: publishingPeriods(logged[publisher]), standby: publishingPeriods(logged[standby])}
	requireHealthAgreesWithTheLog(t, pair, held, logged, reads)
	reading := testwait.Context(t)
	var disagree []string
	for process, want := range map[*dispatchProcess]bool{publisher: false, standby: true} {
		report, err := process.readHealthReport(reading)
		switch {
		case err != nil:
			t.Errorf("process %s's /healthz read failed: %v", pair.name(process), err)
		case report.publishing() != want:
			disagree = append(disagree, fmt.Sprintf("process %s's /healthz reads %v, want outbox_publisher %v", pair.name(process), report, want))
		}
	}
	if len(disagree) > 0 {
		// A read that disagrees with the take-over is the defect unless the standby's turn has
		// ended, and the line that says so is written only after the store that ends the turn,
		// so it is waited for: the wait fails, naming the reads, when it never comes.
		ended := testwait.Until(t, "the standby to log the end of its turn as holder, the one thing that lets "+strings.Join(disagree, " and "),
			func() (string, bool) {
				lines := standby.outboxLines(t)
				if end, found := turnEndAfter(lines, took); found {
					return end.text, true
				}
				return turnSoFar(lines, took), false
			})
		t.Logf("%s, once the standby's turn as holder had ended: %s", strings.Join(disagree, "; "), ended)
	}
	scene.requireOnceAndInOrder(t)
}

// turnEndAfter is the line that ended the turn as holder that took, one of the process's took
// lines, began - the first lost or released line after it in lines, the same process's - and
// whether its lines show one yet.
func turnEndAfter(lines []outboxLine, took outboxLine) (outboxLine, bool) {
	begun := false
	for _, line := range lines {
		switch {
		case line.text == took.text:
			begun = true
		case begun && (line.message == lostHold || line.message == releasedHold):
			return line, true
		}
	}
	return outboxLine{}, false
}

// turnSoFar is what lines, the same process's, show of the turn as holder that took began while
// they show no line that ended it (turnEndAfter): how many of its lines follow took and the newest
// of them, or that took is not among them.
func turnSoFar(lines []outboxLine, took outboxLine) string {
	for index, line := range lines {
		if line.text != took.text {
			continue
		}
		after := lines[index+1:]
		if len(after) == 0 {
			return "the process's took line is its newest outbox line: " + took.text
		}
		return fmt.Sprintf("%d outbox lines of the process follow its took line, none a lost or released line; the newest: %s",
			len(after), after[len(after)-1].text)
	}
	return fmt.Sprintf("none of the process's %d outbox lines is its took line %s", len(lines), took.text)
}

// A role's events reach the role's holder once and in order, and a role that moves sends the
// event the move found undelivered, or written after it, to the session holding the role then:
// the outbox resolves the holder of each send it makes (roles.ResolveHolder) and marks each
// event published once the stream stored the copy it forwarded, which no session answers. In
// the first case the holder's connection drops between the two events and comes back; in the
// second the role moves between them, and the second event reaches the new holder, the old one
// not taking it; in the third the role moves while the forward of an event is outstanding - the
// stream has not acknowledged it - and the event reaches the holder the move made, once, with
// no copy left for the holder it left.
func TestARoleHolderWhoseConnectionMovesGetsTheEventOnceAndInOrder(t *testing.T) {
	pair := startOutboxPair(t)
	rig := newSessionRig(t, pair.nats)
	project := uniqueProjectKey(t, "RHM")
	name := strings.ToLower(project)
	pair.a.createProject(t, project, "Role holder "+project)
	routedIssue := func(t *testing.T, role string) string {
		t.Helper()
		key, _ := pair.a.createIssueIn(t, project, "Routed to "+role, "Routed.")
		pair.a.call(t, http.MethodPatch, "/api/v1/issues/"+key, fmt.Sprintf(`{"route":"role:%s"}`, role), http.StatusOK)
		waitForEveryEventPublished(t, pair.database)
		return key
	}
	writeComment := func(t *testing.T, key, body string) string {
		t.Helper()
		write := rig.postComment(pair.a, key, body)
		if write.err != nil {
			t.Fatalf("write %q: %v", body, write.err)
		}
		return write.commentID
	}
	takes := func(session *testSession, id string) func() bool {
		return func() bool {
			_, has := session.takenComments()[id]
			return has
		}
	}

	t.Run("the holder's connection drops", func(t *testing.T) {
		role := "drop-" + name
		holder := newTestSession(t, rig, "ses_"+name+"_dropping", contracts.LegacyAgentSubject("ses_"+name+"_dropping"))
		rig.registerHolder(t, role, holder.id)
		key := routedIssue(t, role)
		first := writeComment(t, key, "Event N.")
		testwait.Eventually(t, "the holder taking event N", takes(holder, first))
		if err := holder.reconnect(t); err != nil {
			t.Fatalf("reconnect the holder: %v", err)
		}
		second := writeComment(t, key, "Event N+1.")
		testwait.Eventually(t, "the holder taking event N+1", takes(holder, second))
		holder.requireTakenOnce(t, []string{first, second})
		holder.requireInOrder(t)
		requireCommentsPublished(t, pair, holder, first, second)
	})

	t.Run("the role moves to another session", func(t *testing.T) {
		role := "move-" + name
		leaving := newTestSession(t, rig, "ses_"+name+"_leaving", contracts.LegacyAgentSubject("ses_"+name+"_leaving"))
		arriving := newTestSession(t, rig, "ses_"+name+"_arriving", contracts.LegacyAgentSubject("ses_"+name+"_arriving"))
		rig.registerHolder(t, role, leaving.id)
		rig.registerSession(t, arriving.id)
		key := routedIssue(t, role)
		first := writeComment(t, key, "Event N.")
		testwait.Eventually(t, "the holder taking event N", takes(leaving, first))
		if err := rig.claimRole(role, arriving.id); err != nil {
			t.Fatalf("move the role: %v", err)
		}
		second := writeComment(t, key, "Event N+1.")
		testwait.Eventually(t, "the new holder taking event N+1", takes(arriving, second))
		if _, took := leaving.takenComments()[second]; took {
			t.Errorf("the session the role moved from took comment %s, written after the move", second)
		}
		requireCommentsPublished(t, pair, leaving, first)
		requireCommentsPublished(t, pair, arriving, second)
	})

	t.Run("the role moves while a forward of it is unacknowledged", func(t *testing.T) {
		role := "unacknowledged-" + name
		leaving := newTestSession(t, rig, "ses_"+name+"_stranded", contracts.LegacyAgentSubject("ses_"+name+"_stranded"))
		arriving := newTestSession(t, rig, "ses_"+name+"_taking", contracts.LegacyAgentSubject("ses_"+name+"_taking"))
		rig.registerHolder(t, role, leaving.id)
		rig.registerSession(t, arriving.id)
		key := routedIssue(t, role)
		js, err := rig.conn.JetStream(testnats.MaxWait(t))
		if err != nil {
			t.Fatalf("JetStream context: %v", err)
		}
		head := streamHead(t, js)
		// From here the stream captures the arriving session's agent subject alone, so the
		// forward to the holder of the moment is stored nowhere and nothing acknowledges it,
		// which is a forward outstanding: the event stays unpublished with its attempt counted
		// while the leaving session still holds the role, and its core subscription is handed
		// each forward all the same.
		capturing := narrowAgentCapture(t, js, arriving.id)
		first := writeComment(t, key, "Event N.")
		testwait.Eventually(t, "the holder being handed the forward", takes(leaving, first))
		forwarded := leaving.takenComments()[first]
		row, event := forwarded.eventRow(t), forwarded.event
		testwait.Eventually(t, "the forward the stream did not acknowledge being counted against the event", func() bool {
			var attempts int
			var failure *string
			var published *time.Time
			if err := pair.database.Pool.QueryRow(context.Background(),
				`select attempt_count, publish_error, published_at from events where id = $1`, row).Scan(&attempts, &failure, &published); err != nil {
				t.Fatalf("read event %s: %v", event, err)
			}
			if published != nil {
				t.Fatalf("event %s was published though the stream stored no forward of it", event)
			}
			return attempts > 0 && failure != nil && strings.Contains(*failure, bus.ErrNotAcknowledged.Error())
		})

		// The role moves with that forward still unacknowledged. A batch that resolved the old
		// holder just before the move can still be sending to it, so every agent subject is
		// captured again only once the new holder has taken the event, which is the outbox
		// resolving the role anew: from there a forward to the session the role left would be
		// stored like any other.
		if err := rig.claimRole(role, arriving.id); err != nil {
			t.Fatalf("move the role: %v", err)
		}

		testwait.Eventually(t, "the new holder taking the event", takes(arriving, first))
		capturing()
		requireCommentsPublished(t, pair, arriving, first)
		// The outbox forwards the event to the new holder again only where the batch that
		// forwarded it left the role's destination unreached, which a line of its own records
		// (lawfulResends): the stream not acknowledging a forward within roleForwardAckTimeout (the
		// outbox's) among them, whose back-off line names the role's destination with the stream's
		// ErrNotAcknowledged. The event's owner is one lane, so a forward to the new holder is made
		// only once the one before it, to the session the role left, has settled (outbox's lanes):
		// a line written at or after the new holder's first frame records a resend to it.
		frames := framesOfComment(arriving, first)
		var counted resends
		if len(frames) > 0 {
			logged := map[*dispatchProcess][]outboxLine{pair.a: pair.a.outboxLines(t), pair.b: pair.b.outboxLines(t)}
			counted = lawfulResends(logged, strconv.FormatInt(row, 10), roleDestination(role), frames[0].at)
		}
		if len(frames) == 0 || len(frames) > 1+len(counted) {
			t.Errorf("the session the role moved to was handed the event %d times, want the one forward that was made once it held the role and one more for each of the %d resends of it there the outbox's own lines record since that forward:\n%s",
				len(frames), len(counted), counted)
		}
		if copies := storedCopiesOf(t, js, head, contracts.LegacyAgentSubject(arriving.id), event); copies != 1 {
			t.Errorf("the stream holds %d copies of event %s on %s, want the one the new holder's consumer delivers",
				copies, event, contracts.LegacyAgentSubject(arriving.id))
		}
		if copies := storedCopiesOf(t, js, head, contracts.LegacyAgentSubject(leaving.id), event); copies != 0 {
			t.Errorf("the stream holds %d copies of event %s on %s, want none: the session the role left is delivered what its consumer holds, and nothing of this event is its to read",
				copies, event, contracts.LegacyAgentSubject(leaving.id))
		}
	})
}

// requireCommentsPublished requires holder to have taken each comment of ids and each of their
// events to be marked published, and both processes of the pair to be running. The batch that
// sent an event records it once every answer of the batch is in, which can land after the holder
// has been handed the forward, so each record is waited for through the shared wait.
func requireCommentsPublished(t *testing.T, pair *dispatchPair, holder *testSession, ids ...string) {
	t.Helper()
	taken := holder.takenComments()
	for _, id := range ids {
		frame, ok := taken[id]
		if !ok {
			t.Fatalf("the holder never took comment %s", id)
		}
		row := frame.eventRow(t)
		testwait.Eventually(t, fmt.Sprintf("event %s being marked published", frame.event), func() bool {
			var published *time.Time
			if err := pair.database.Pool.QueryRow(context.Background(),
				`select published_at from events where id = $1`, row).Scan(&published); err != nil {
				t.Fatalf("read event %s's published_at: %v", frame.event, err)
			}
			return published != nil
		})

	}
	pair.requireRunning(t)
}

// narrowAgentCapture has the stream capture the agent subjects of holders alone, in place of
// every agent subject, and lets it capture them all again when the returned func is called or
// the test ends. A forward to any other session is then stored nowhere, so nothing acknowledges
// it and the batch that sent it waits out its window, which is the shape a forward goes
// unanswered in; the session's own core subscription is handed it all the same. The processes'
// keepers leave the narrowed list alone: a keeper puts back only a subject of this binary's that
// no subject the stream holds overlaps (bus.keptSubjects), and a holder's agent subject overlaps
// notifications.agent.>.
//
// The returned restore is a plain func and not a sync.OnceFunc: a t.Fatalf inside a OnceFunc ends
// the goroutine (runtime.Goexit) in the once, which sync.OnceFunc turns into a panic with a nil
// argument that takes the whole binary down. Its first call restores the subjects and fails the
// row by name with t.Errorf when the stream refuses; a later call does nothing. Both the row and
// its cleanup call it on the test's goroutine, so the flag needs no lock.
func narrowAgentCapture(t *testing.T, js natsgo.JetStreamContext, holders ...string) func() {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the notification stream: %v", err)
	}
	capturing, narrowed := info.Config, info.Config
	narrowed.Subjects = slices.DeleteFunc(slices.Clone(capturing.Subjects), func(subject string) bool {
		return subject == contracts.AgentTopicPrefix+">"
	})
	for _, holder := range holders {
		narrowed.Subjects = append(narrowed.Subjects, contracts.LegacyAgentSubject(holder))
	}
	if _, err := js.UpdateStream(&narrowed); err != nil {
		t.Fatalf("narrow the stream's agent subjects: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if _, err := js.UpdateStream(&capturing); err != nil {
			t.Errorf("restore the stream's agent subjects: %v", err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// framesOfComment is every frame of comment id's event that reached session, repeats included, in
// arrival order: a forward sent twice is handed to a core subscriber twice, whatever the stream
// keeps of it.
func framesOfComment(session *testSession, id string) []sessionFrame {
	var frames []sessionFrame
	for _, frame := range session.allFrames() {
		if frame.commentID == id {
			frames = append(frames, frame)
		}
	}
	return frames
}

// storedCopiesOf is how many messages the stream holds past head on subject that carry event,
// read by sequence so the read creates nothing on the server (readStreamMessagesFrom).
func storedCopiesOf(t *testing.T, js natsgo.JetStreamContext, head uint64, subject, event string) int {
	t.Helper()
	copies := 0
	for _, message := range readStreamMessagesFrom(t, js, head) {
		if message.subject == subject && message.item.EventID == event {
			copies++
		}
	}
	return copies
}

// ───── the delivery durables ────────────────────────────────────────────────

// githubFake is the loopback GitHub the test's processes send their App calls to, so no call
// leaves the machine. It answers each path the processes read in the shape GitHub's API gives it
// (githubFakeAnswer): an empty webhook delivery list, the App's one installation on acme/deploy
// granting the reads the delivery reconcile checks for, an installation token, that
// installation's one repository, an empty pull-request search and empty workflow run and job listings, and GitHub's 404 for any other
// path. It holds the first webhook delivery listing - the webhook redelivery sweep's own first
// request, made at boot - until release is closed, which is how a row makes one process hold that
// sweep's lease long enough for the other to find it held. It counts those listings too: the
// listing is the call the webhook-redelivery lease exists to make once however many processes
// sweep, so a row about that lease reads the count rather than the log alone. It counts the pull
// requests the processes fetch for the same reason: one envelope handed to one intake is one
// fetch, and the count is the only thing that tells one bound subscriber from two
// (TestAProcessBackFromANATSCutKeepsDeliveryEventsAndDeliversThemOnce).
type githubFake struct {
	server  *httptest.Server
	release chan struct{}
	once    sync.Once

	mu      sync.Mutex
	listed  int
	fetched int
}

// startGitHubFake serves on loopback until the test ends.
func startGitHubFake(t *testing.T) *githubFake {
	t.Helper()
	fake := &githubFake{release: make(chan struct{})}
	fake.server = githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/hook/deliveries") {
			fake.countDeliveryListing()
			fake.holdFirstDeliveryListing(r.Context())
		}
		if pullRequestPath.MatchString(r.URL.Path) {
			fake.countPullRequestFetch()
		}
		status, body := githubFakeAnswer(r)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}})
	t.Cleanup(fake.Release)
	return fake
}

// githubFakeInstallation is GitHub's installation object for the App on acme/deploy (the deploy
// repository seedDeliverySettings names), with the Actions and Pull requests reads the delivery
// reconcile's permission check requires (githubapp.Client.DeliveryPermissions).
const githubFakeInstallation = `{"id":1,"app_slug":"multiprocess","account":{"login":"acme"},` +
	`"permissions":{"actions":"read","pull_requests":"read","contents":"read","metadata":"read"}}`

// githubFakeAnswer is GitHub's status and body for r, in the shape its API gives each path the
// processes read: the webhook redelivery sweep's listings, the installation lookups and token
// mint every installation-token call makes, the installation's repository list and the
// reconcile's GraphQL pull-request search over it, and its workflow run and job listings. Every listing is empty, so a pass reads its whole sequence and
// finds nothing to import.
func githubFakeAnswer(r *http.Request) (int, string) {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/app/hook/deliveries"):
		return http.StatusOK, "[]"
	case path == "/app/installations":
		return http.StatusOK, "[" + githubFakeInstallation + "]"
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens"):
		return http.StatusCreated, fmt.Sprintf(`{"token":"ghs_multiprocess","expires_at":%q}`,
			time.Now().Add(stagedPastTheRun).UTC().Format(time.RFC3339))
	case strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/installation"):
		return http.StatusOK, githubFakeInstallation
	case path == "/installation/repositories":
		return http.StatusOK, `{"total_count":1,"repositories":[{"id":1,"name":"deploy","full_name":"acme/deploy","private":true,"owner":{"login":"acme"}}]}`
	case r.Method == http.MethodPost && path == "/graphql":
		return http.StatusOK, `{"data":{"search":{"issueCount":0,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}`
	case strings.Contains(path, "/actions/workflows/") && strings.HasSuffix(path, "/runs"):
		return http.StatusOK, `{"total_count":0,"workflow_runs":[]}`
	case strings.Contains(path, "/actions/runs/") && strings.HasSuffix(path, "/jobs"):
		return http.StatusOK, `{"total_count":0,"jobs":[]}`
	}
	return http.StatusNotFound, `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`
}

// holdFirstDeliveryListing holds a listing until Release or until its caller gives up, by the
// process's own bound on the call (githubapp.RequestTimeout) or its exit; a listing after Release
// is answered at once. Nothing else ends the hold, so a slow step of the row while it holds
// changes nothing of what the row reads, and startGitHubFake releases it when the test ends, so a
// row that fails before it releases leaves no listing held.
func (f *githubFake) holdFirstDeliveryListing(ctx context.Context) {
	select {
	case <-f.release:
	case <-ctx.Done():
	}
}

// countDeliveryListing records one webhook delivery listing, whichever process made it.
func (f *githubFake) countDeliveryListing() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
}

// deliveryListings is how many webhook delivery listings the processes have made between them.
func (f *githubFake) deliveryListings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listed
}

// pullRequestPath is GitHub's path for one pull request, /repos/<owner>/<repo>/pulls/<number>,
// and nothing under it: a pull request's commits and its files are calls of their own, made for
// their own reasons, and a row counting how many processes fetched one pull request counts that
// one path.
var pullRequestPath = regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/[0-9]+$`)

// countPullRequestFetch records one pull-request fetch, whichever process made it.
func (f *githubFake) countPullRequestFetch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched++
}

// pullRequestFetches is how many pull requests the processes have fetched between them.
func (f *githubFake) pullRequestFetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetched
}

// Release lets every held listing answer.
func (f *githubFake) Release() { f.once.Do(func() { close(f.release) }) }

// URL is the origin DISPATCH_GITHUB_API_BASE names.
func (f *githubFake) URL() string { return f.server.URL }

// Each delivery durable is delivered from by one process: the one holding its hold. With two
// processes reading one NATS, exactly one of them binds each durable and the other stands by - an
// ordinary state, logged at INFO and never as a failure - polling every two seconds, so the work
// resumes within seconds of the holder's hold lapsing when it dies, rather than waiting for a
// deploy.
//
// What the row reads is the binding itself, from the two servers: NATS has the durable
// push-bound, exactly one client connection of the server's own connection listing subscribes to
// its deliver subject, and the durable's hold -- one name every process of every build takes
// (delivery.intakeHoldName) -- is live and records that subject. It then watches all of that
// stand still for longer than the standby's bind interval several times over
// (requireOneBinderThroughout), so the standby's own polls for the durable fall inside the
// window, and the standby binds nothing in it. A second process delivering from the durable
// would be a second subscriber, or a deliver subject that changed under the samples, since a
// take-over subscribes to a fresh deliver subject of its own (delivery.heldDurable.takeOver).
//
// The standby's standing-by line says nothing here and is not waited for. It is written on the
// standby's own poll of a durable it will not take, so it can come long after the durable is one
// process's, or not at all while the hold does its work: waiting for it failed 13 of 1,066
// dispatch-multiprocess job attempts on main (INFRA-1568). A pair holding a hold each would
// write it too -- each process reads the other's binding as a previous build's and stands by for
// it (delivery.heldDurable.boundElsewhere) -- so the line tells a shared hold from no hold at
// all no better than it tells a healthy pair from a slow one. The holds themselves do:
// requireOneHoldPerDurable reads which names the intakes hold.
//
// The delivery reconcile reads GitHub from whichever process takes its lease. Its first pass is
// jittered over its first five minutes, so without a knob it lands inside this row's window
// only now and then; DISPATCH_TEST_RECONCILE_AT_START runs it as each process starts, so every
// run of this row meets it, and neither process may log a delivery error for it.
//
// Both processes take their holds for a TTL past the run, and end each read and take of a hold
// and each GitHub call at the test's give-up (heldDeliveryEnv): a renewal a loaded machine makes late, or a
// statement it answers slowly, then moves no durable and writes no delivery error, so the window
// the row watches holds the defect alone. The hold of the holder it kills is then let lapse by the
// row (lapseDurableHold), which is what that TTL would have done on the database's clock.
//
// Every failure about a durable's binding prints both processes' whole output (pair.buffers), so
// the state that failed is in the job that failed. Two failures print one process's instead: a
// delivery error logged while the reconcile read GitHub names that process's error lines, and the
// survivor's wait for its bind after the kill (waitForLog) prints the survivor's output.
func TestOneProcessBindsEachDeliveryDurableAndTheOtherStandsBy(t *testing.T) {
	pair := newPair(t)
	seedDeliverySettings(t, pair.database)
	fake := startGitHubFake(t)
	fake.Release()
	env := heldDeliveryEnv(t, githubAppEnv(t, fake.URL()))
	env["DISPATCH_TEST_RECONCILE_AT_START"] = "1"
	pair.start(t, env)

	conn := testnats.Connect(t, pair.nats)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}

	holders := map[string]*dispatchProcess{}
	bindings := map[string]durableBinding{}
	for _, durable := range deliveryDurables {
		binding, holder := waitForDurableBinder(t, pair, js, durable)
		holders[durable], bindings[durable] = holder, binding
		t.Logf("%s delivers from %s: %s", pair.name(holder), durable, binding)

		for _, process := range []*dispatchProcess{pair.a, pair.b} {
			if errorLines := deliveryOutcomeFor(process, durable).errors; len(errorLines) > 0 {
				t.Errorf("%s logged a delivery error for %s; binding the durable and standing by for it are both ordinary:\n%s\n%s",
					pair.name(process), durable, strings.Join(errorLines, "\n"), pair.buffers())
			}
		}
	}
	requireOneBinderThroughout(t, pair, js, bindings, holders)

	// The reconcile's pass at start has read GitHub as a deployment's does: its permission check,
	// its searches and its run listings, answered in GitHub's own shapes. The lease logs its run
	// once the pass has returned, so every line it wrote is in the log by then.
	waitForLease(t, pair, leaseRan, "delivery-reconcile")
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		if errorLines := process.logLines("level=ERROR", "dispatch delivery:"); len(errorLines) > 0 {
			t.Errorf("%s logged a delivery error while the reconcile read GitHub:\n%s",
				pair.name(process), strings.Join(errorLines, "\n"))
		}
	}

	// Every assertion above reports and goes on, except the wait for each durable's binder,
	// which is the state the hand-over moves: the row reaches the kill with both durables one
	// process's, or it stopped on that wait.
	//
	// The holder dies the way a task replaced mid-deploy or killed for memory does, with no
	// chance to unsubscribe or release its hold, and the process that was standing by takes the
	// durable over once the dead holder's hold lapses. Its hold's TTL is past the run, so the row
	// lets it lapse once the holder is dead (lapseDurableHold), as the TTL would have, and the
	// survivor's next durableBindInterval poll finds it lapsed.
	dying := holders["delivery-events"]
	survivor := pair.other(dying)
	dying.kill(t)
	handedOver := time.Now()
	for _, durable := range deliveryDurables {
		if holders[durable] != dying {
			continue
		}
		lapseDurableHold(t, pair.database, durable, bindings[durable].hold.holder)
		// The survivor takes a dead holder's durable over once that holder's hold lapses: the
		// one durableBindInterval poll that finds it lapsed, and the take-over's own delete and
		// create.
		survivor.waitForLog(t, `msg="dispatch delivery: NATS consumer bound"`, "name="+durable)
		// And the binding moved with it: the durable now delivers to a subject of the
		// survivor's own, which one connection subscribes to, under a live hold recording it.
		// The dead holder's subject is gone, so nothing of what it had bound is still bound.
		if binding := readDurableBinding(t, pair, js, durable); !binding.bound() || binding.subject == bindings[durable].subject {
			t.Errorf("%s logged a bind of %s after %s died, and NATS has it %s, want it delivering to a fresh subject of the survivor's under a live hold recording that subject, and not the dead holder's %s.\n%s",
				pair.name(survivor), durable, pair.name(dying), binding, bindings[durable].subject, pair.buffers())
		}
	}
	t.Logf("the standby bound the dead holder's durables %v after it died", time.Since(handedOver))
}

// A process starts its delivery intakes before it joins the registry of live server processes, so
// a process that waits for admission (processes.Register: a live peer refuses its signing keys)
// can already deliver from both durables. A deploy that stops it in that wait stops its intakes
// too: each hold is released, not left to lapse with the stopped process's deliver subject as its
// token, and the process standing by takes each durable over within one bind interval. The peer
// that refuses it has no GitHub App, so its intakes take nothing.
func TestAProcessStoppedWhileWaitingForAdmissionReleasesItsDurables(t *testing.T) {
	pair := newPair(t)
	seedDeliverySettings(t, pair.database)
	fake := startGitHubFake(t)
	fake.Release()
	app := githubAppEnv(t, fake.URL())
	// The refused process waits for admission a live window and a heartbeat of its own pace
	// (processes.Register), and past it gives up on its own, exits 1 and releases both durables on
	// the way out -- not the stop this row is about, and the standby then binds them rather than
	// standing by, which fails the stand-by wait below. So it beats at stagedPastTheRun
	// (DISPATCH_TEST_PROCESS_HEARTBEAT), which puts that wait (admissionWait) past the run and
	// leaves the stop the row's own: no step of the row has to fit inside it. It takes its intake
	// holds for a TTL past the run too (DISPATCH_TEST_INTAKE_HOLD_TTL), so a renewal a loaded
	// machine makes late hands no durable to the standby before the stop; a hold the stop left in
	// place then holds the standby off for that TTL, which the release this row is about never
	// leaves. The three starts share one binary built here, since a `go build` of envoy-dispatch
	// takes 14 s on an idle two-core host and over 30 s on a loaded one.
	binary := cmdtest.Build(t, "envoy-dispatch")
	peer := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{NATSURL: pair.nats, SigningKey: multiprocessSigningKey, Binary: binary})
	peer.waitHealthy(t)

	waitingEnv := maps.Clone(app)
	waitingEnv["DISPATCH_TEST_PROCESS_HEARTBEAT"] = stagedPastTheRun.String()
	waitingEnv["DISPATCH_TEST_INTAKE_HOLD_TTL"] = stagedPastTheRun.String()
	waiting := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{NATSURL: pair.nats, SigningKey: "a-key-the-peer-does-not-share", Env: waitingEnv, Binary: binary})
	waiting.waitForLog(t, "a live server process refuses this one's signing keys; waiting for its heartbeat to lapse")
	for _, durable := range deliveryDurables {
		waiting.waitForLog(t, `msg="dispatch delivery: NATS consumer bound"`, "name="+durable)
	}
	standby := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{NATSURL: pair.nats, SigningKey: multiprocessSigningKey, Env: app, Binary: binary})
	for _, durable := range deliveryDurables {
		standby.waitForLog(t, `msg="dispatch delivery: NATS consumer held by another process; standing by"`, "name="+durable)
	}

	conn := testnats.Connect(t, pair.nats)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	subjects := map[string]string{}
	for _, durable := range deliveryDurables {
		info, err := js.ConsumerInfo(bus.Stream, durable)
		if err != nil {
			t.Fatalf("read %s: %v", durable, err)
		}
		subjects[durable] = info.Config.DeliverSubject
	}
	if waiting.logged(`msg="dispatch: listening"`) {
		t.Fatalf("the process refused admission serves; the row needs it still waiting:\n%s", waiting.Output.String())
	}
	// A process that gave up that wait never served either, and released its durables as it
	// exited, so there would be nothing left for the stop below to release.
	select {
	case <-waiting.Exited:
		t.Fatalf("the process refused admission %s before the row stopped it, having outlasted the %v its admission wait is given:\n%s",
			waiting.Cmd.ProcessState, admissionWait(stagedPastTheRun), waiting.Output.String())
	default:
	}

	waiting.Terminate(t)
	waiting.WaitExit(t, "SIGTERM")
	stopped := time.Now()
	if code := waiting.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("a process stopped while waiting for admission exited %d, want 0:\n%s", code, waiting.Output.String())
	}
	for _, durable := range deliveryDurables {
		var left int
		if err := pair.database.Pool.QueryRow(context.Background(),
			`select count(*) from duty_holds where name = $1 and token = $2`, "delivery-intake-"+durable, subjects[durable]).Scan(&left); err != nil {
			t.Fatalf("read the hold of %s: %v", durable, err)
		}
		if left != 0 {
			t.Errorf("the stopped process left its hold of %s in place, naming its deliver subject %s", durable, subjects[durable])
		}
	}
	// The read above is what tells a released hold from one left to lapse: a hold left in place
	// holds the standby off for the TTL past the run the stopped process took it for, and one
	// released lets its next bind poll (delivery.durableBindInterval, 2 s) take the durable, which
	// the row waits for.
	for _, durable := range deliveryDurables {
		standby.waitForLog(t, `msg="dispatch delivery: NATS consumer bound"`, "name="+durable)
	}
	t.Logf("the standby bound both durables %v after the stop", time.Since(stopped))
}

// ───── a frozen holder ──────────────────────────────────────────────────────

// frozenHolderResumedRounds is how many rounds of messages the row publishes once the frozen
// process has resumed, each followed by a whole check of each durable by the resumed process's
// intakes (delivery.heldDurable.check, every delivery.intakeHoldRenew, 5 s, its line
// delivery.DurableChecked numbered per intake) that began after the round went out
// (waitForPassAfter). Whatever a process that resumed holding nothing would still do -- bind the
// durable, handle a message of it -- it has those loops' turns to do it in, with messages
// arriving throughout, and the row knows each turn has passed from the process's own line rather
// than from a span of the clock.
const frozenHolderResumedRounds = 3

// A process frozen with its NATS connection still open -- a long garbage collection pause, a
// host that stops scheduling it, a pod whose node has stopped -- keeps every subscription it had
// and renews nothing. It is the production case the durable's hold is there for that no
// connection says anything about: the server still holds the frozen process's subscription, so
// nothing NATS knows tells the other process it may deliver, and only the hold lapsing on the
// database's clock does.
//
// The standby takes the durable over once the frozen holder's hold lapses, and the row reads that
// from the two servers as the one-binder row does (durableBinding.bound): the durable delivers
// to a fresh subject, exactly one client connection subscribes there, and the durable's one
// hold is live, under a holder that is not the frozen process's intake, recording that subject.
// That one subscriber is the standby's: the take-over mints the subject while the other process
// is frozen (delivery.heldDurable.takeOver), so no connection of the frozen process ever heard
// of it, and the standby is the process that logged the bind.
//
// What the freeze then shows, and a kill cannot, is the resumed process: it comes back with its
// connection, its subscription and its own hold still in hand, and must do nothing with any of
// them. GitHub-shaped messages are published in four rounds -- before the freeze, while the
// holder is frozen, once the standby has taken over, and in frozenHolderResumedRounds rounds after
// the resume -- and the row requires that the frozen process handled the first round and
// nothing from the freeze on, that every message was handled exactly once across the two
// processes (each process calls a GitHub of its own, so every handling is recorded with the
// process that made it: mergedPullRequestFake), that the resumed process logged no bind of
// either durable after it was frozen, that the deliver subject it had bound is left with no
// subscriber, that both durables still deliver as the take-over left them, and that the events
// durable ends with nothing pending, nothing unacknowledged and its ack floor at what it
// delivered.
//
// A, the holder, runs alone until it is frozen: it binds both durables under production's intake
// hold TTL, which lapses while it is frozen, as it does for a process its node stops: its own loss
// deadline passes with it, so the hold it resumes with is one it already knows lost. With no other
// process running, renewals a loaded machine makes late before the freeze move no durable to
// another process; A takes its own lapsed hold again, which freezeHolder waits out. B, the standby,
// starts once A is frozen delivering from both, and takes its holds, once it takes them over, for a
// TTL past the run (DISPATCH_TEST_INTAKE_HOLD_TTL), so a renewal a loaded machine makes late after
// the resume hands nothing back to A. Both end a message's handler at the test's give-up
// (DISPATCH_TEST_INTAKE_MESSAGE_DEADLINE, endsAtTheGiveUp), so a slow handler gives no message up.
func TestAFrozenHolderIsTakenOverAndHandlesNothingOnceResumed(t *testing.T) {
	pair := newPair(t)
	seedMergedPullRequestSettings(t, pair.database)
	fake := newMergedPullRequestFake()
	// A GitHub of each process's own, and the delivery reconcile on an interval the row meets
	// several times over rather than its five minutes, whose jittered first pass would land
	// inside this row now and then. A pass reads pull requests through the very call that
	// counts a handling here (delivery.FetchPullRequest), so a pass that re-read one of the
	// row's own merges would read as a second handling of a message; the merges published here
	// are complete reads, which the reconcile's merged arm never offers again
	// (publishMergedPullRequests), and running it throughout is what makes every run of the row
	// show that rather than most runs missing it.
	envFor := func(process string) map[string]string {
		env := githubAppEnv(t, fake.serve(t, process))
		env["DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL"] = (10 * time.Second).String()
		// Each intake's check of its durable logs a numbered line at DEBUG, the turns of the
		// resumed process's loops the row waits on.
		env["DISPATCH_TEST_DEBUG_MESSAGES"] = delivery.DurableChecked
		env["DISPATCH_TEST_INTAKE_MESSAGE_DEADLINE"] = endsAtTheGiveUp(t)
		return env
	}
	pair.a = pair.startProcess(t, envFor("A"))
	pair.a.waitHealthy(t)
	for _, durable := range deliveryDurables {
		pair.a.waitForLog(t, `msg="dispatch delivery: NATS consumer bound"`, "name="+durable)
	}

	conn := testnats.Connect(t, pair.nats)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}

	frozen := pair.a
	run := strings.ToLower(uniqueProjectKey(t, ""))
	// The holder handles messages before it is frozen, so what the resumed process must not do
	// is what it was doing until the freeze; and the durable is drained when the freeze comes,
	// so no message is in a handler of the frozen process for the standby to handle again.
	before := publishMergedPullRequests(t, js, run, 1, 2)
	waitForHandled(t, frozen.Output, fake, before)
	waitForDurableDrained(t, frozen.Output, js, bus.DispatchDeliveryDurable, "before the freeze")

	resumed := false
	// So a failure never leaves a stopped process behind: the cleanup cmdtest.Start registered
	// when the process started kills it, and runs after this one, which is registered later.
	t.Cleanup(func() {
		if !resumed {
			_ = frozen.Cmd.Process.Signal(syscall.SIGCONT)
		}
	})
	// Each durable's binding at the freeze, which the take-over replaces, and whose deliver
	// subject nothing may be left subscribed to once the frozen process resumes.
	atFreeze, frozenAt := freezeHolder(t, pair, js, frozen)
	for _, durable := range deliveryDurables {
		t.Logf("A was frozen delivering from %s: %s", durable, atFreeze[durable])
	}

	// Published at the frozen holder: NATS pushes them down its open connection, which reads
	// nothing, and the take-over's recreated durable redelivers them to the standby.
	whileFrozen := publishMergedPullRequests(t, js, run, 3, 3)
	standbyEnv := envFor("B")
	standbyEnv["DISPATCH_TEST_INTAKE_HOLD_TTL"] = stagedPastTheRun.String()
	pair.b = pair.startProcess(t, standbyEnv)
	pair.b.waitHealthy(t)
	standby := pair.b
	requireHandledBy(t, pair, fake, before, frozen, "before the freeze")
	requireOneHoldPerDurable(t, pair)
	bindings := map[string]durableBinding{}
	for _, durable := range deliveryDurables {
		// The standby started after the freeze, so every bind it logs is a take-over.
		bindings[durable] = waitForTakeOver(t, pair, js, durable, atFreeze[durable], standby, 0)
		t.Logf("%s took %s over %v after %s was frozen: %s", pair.name(standby), durable,
			time.Since(frozenAt).Round(time.Millisecond), pair.name(frozen), bindings[durable])
	}
	requireOneHoldPerDurable(t, pair)
	waitForHandled(t, pair.buffers(), fake, whileFrozen)
	afterTakeOver := publishMergedPullRequests(t, js, run, 6, 3)
	waitForHandled(t, pair.buffers(), fake, afterTakeOver)
	requireHandledBy(t, pair, fake, slices.Concat(whileFrozen, afterTakeOver), standby, "while the holder was frozen")

	resumedAt := frozen.signal(t, syscall.SIGCONT)
	resumed = true
	// Its hold's loss deadline passed while it was stopped, so the moment its timers fire it
	// finds the hold lost and stops delivering (delivery.heldDurable.run).
	frozen.waitForLog(t, `msg="dispatch delivery: the intake's hold is lost; it stops delivering"`, "name="+bus.DispatchDeliveryDurable)
	t.Logf("the resumed process reported its hold of %s lost %v after it resumed", bus.DispatchDeliveryDurable,
		time.Since(resumedAt).Round(time.Millisecond))
	var afterResume []int
	for round := range frozenHolderResumedRounds {
		checked := map[string]uint64{}
		for _, durable := range deliveryDurables {
			checked[durable] = passesLogged(t, frozen, `msg="`+delivery.DurableChecked+`"`, "name="+durable)
		}
		afterResume = append(afterResume, publishMergedPullRequests(t, js, run, 9+2*round, 2)...)
		for _, durable := range deliveryDurables {
			waitForPassAfter(t, frozen, fmt.Sprintf("the resumed process's check of %s", durable), checked[durable],
				`msg="`+delivery.DurableChecked+`"`, "name="+durable)
		}
	}
	waitForHandled(t, pair.buffers(), fake, afterResume)
	requireHandledBy(t, pair, fake, afterResume, standby, "after the frozen process resumed")
	// A delivery reconcile pass ran with every pull request of the row stored, and re-read none
	// of them: one that did would have read a pull request through the very call that counts a
	// handling below (delivery.FetchPullRequest), and the counts would say so. An intake
	// acknowledges a message once it has handled it, so once the durable is drained every pull
	// request of the row is stored. Passes of the lease never overlap, and each logs its line
	// while it still holds the lease (duty.Exclusive), so of the passes logged from there on at
	// most the first was under way then, and the second began with every pull request of the row
	// stored. Each process ticks at most ten seconds after its last pass ended, and a tick in a
	// period another process's pass has served skips, so the passes come a period or so apart.
	waitForDurableDrained(t, pair.buffers(), js, bus.DispatchDeliveryDurable, "once every published pull request was handled")
	reconciled := countLease(pair.a, leaseRan, "delivery-reconcile") + countLease(pair.b, leaseRan, "delivery-reconcile")
	testwait.Until(t, fmt.Sprintf("the second delivery reconcile pass logged after pass %d was, the first to begin with the row's pull requests stored", reconciled),
		func() (int, bool) {
			logged := countLease(pair.a, leaseRan, "delivery-reconcile") + countLease(pair.b, leaseRan, "delivery-reconcile")
			return logged, logged >= reconciled+2
		})

	published := slices.Concat(before, whileFrozen, afterTakeOver, afterResume)
	handled := fake.handled()
	for _, number := range published {
		if handlings := handled[number]; len(handlings) != 1 {
			t.Errorf("pull request %d was handled %d times (%v), want exactly once across the two processes: every message the standby took over is one the frozen process never handled, and the standby handles each once.\n%s",
				number, len(handlings), handlings, pair.buffers())
		}
	}
	// Every handling the frozen process made is of a pull request published before it was frozen,
	// each of which it had handled once before the freeze (requireHandledBy above): what reached
	// the durable from the freeze on is the standby's under its hold, and a second handling of an
	// earlier one is the repeat the count above fails. Which request a handling answered is read
	// off the pull request it fetched, not off the moment the stand-in served it: the stand-in
	// stamps a handling when the fetch reaches it, and a fetch the frozen process sent just before
	// the stop can reach it after the signal was sent, still one the process made while it held
	// the hold.
	publishedBefore := map[int]bool{}
	for _, number := range before {
		publishedBefore[number] = true
	}
	for number, handlings := range handled {
		if publishedBefore[number] {
			continue
		}
		for _, handling := range handlings {
			if handling.process == pair.name(frozen) {
				t.Errorf("the frozen process handled pull request %d (%s), which was not published before it was frozen: from the freeze on %s is the standby's under its hold, and the resumed process holds no hold of it.\n%s",
					number, handling, bus.DispatchDeliveryDurable, pair.buffers())
			}
		}
	}
	// A line the resumed process logged is stamped at or after the resume, by its clock and the
	// test's, which are the host's; a line from before the freeze is stamped a freeze earlier.
	since := resumedAt.Truncate(time.Millisecond)
	for _, durable := range deliveryDurables {
		var rebound []string
		for _, line := range deliveryOutcomeFor(frozen, durable).bound {
			if !lineTime(t, line).Before(since) {
				rebound = append(rebound, line)
			}
		}
		if len(rebound) > 0 {
			t.Errorf("the resumed process logged a bind of %s after it resumed, when the durable is the other process's under its hold:\n%s\n%s",
				durable, strings.Join(rebound, "\n"), pair.buffers())
		}
		subject := atFreeze[durable].subject
		if subscribers := natsSubscribers(t, pair, subject); len(subscribers) > 0 {
			t.Errorf("the deliver subject %s the frozen process had %s bound to is still subscribed by [%s] %v after it resumed; an intake that finds its hold lost drains its subscription (delivery.heldDurable.StopDelivering).\n%s",
				subject, durable, strings.Join(subscribers, ", "), time.Since(resumedAt).Round(time.Millisecond), pair.buffers())
		}
	}
	for _, durable := range deliveryDurables {
		if binding := readDurableBinding(t, pair, js, durable); !binding.bound() || binding.String() != bindings[durable].String() {
			t.Errorf("%s was %s once the take-over had settled and is %s now the frozen process has resumed, %v after it did: a process that resumed holding nothing takes no durable over.\n%s",
				durable, bindings[durable], binding, time.Since(resumedAt).Round(time.Millisecond), pair.buffers())
		}
	}
	requireOneHoldPerDurable(t, pair)
	pair.requireRunning(t)
}

// freezeHolder stops process, the one process of the pair running, at a moment it delivers from
// every delivery durable and holds no database transaction open, and answers each durable's
// binding at the stop and when the stop was sent. Both are read once every thread has stopped
// (waitFrozen), when nothing of the process can move them, so the bindings read then are the ones
// it froze with. Until then either can lawfully be otherwise: renewals a loaded machine makes late
// past the hold's loss deadline end the process's hold, which it then takes again and binds anew,
// and a stop can land inside one of its transactions, whose locks - the registry's
// (processes.registryLock), which the standby's admission takes, among them - would stay held for
// as long as the freeze lasts. A stop that finds a durable unbound or a transaction open is undone,
// and made again once the process delivers from every durable again. The whole of it is one shared
// wait (testwait.Until), each try one step: a read of the bindings while the process runs, and the
// stop once they are all bound; a read of the threads while the stop has not stopped them all
// (threadsStopped); and, once it has, the reads of the bindings and the transactions, which end
// the wait or undo the stop. A read that fails fails nothing itself: its error is that try's
// reading, the try leaves the stop as it stands, and the next try reads again, so a read that
// fails for good, as the NATS monitor's listing does once the shared wait's context ends
// (dispatchPair.monitorReads), ends at the wait's give-up. Its give-up prints that try's reading
// with the last stop's: the transactions that stop found open and the durables it found unbound,
// and the process's output. No other process connects to the pair's database before the standby
// starts, and the test's own pool holds no transaction between its reads, so every open
// transaction of a client is the frozen process's.
func freezeHolder(t *testing.T, pair *dispatchPair, js natsgo.JetStreamContext, process *dispatchProcess) (map[string]durableBinding, time.Time) {
	t.Helper()
	var bindings map[string]durableBinding
	var stoppedAt time.Time
	// unboundOf reads every durable's binding into bindings, and answers those not bound, or the
	// error of the first read that failed.
	unboundOf := func() ([]string, error) {
		bindings = map[string]durableBinding{}
		var unbound []string
		for _, durable := range deliveryDurables {
			binding, err := durableBindingOf(pair, js, durable)
			if err != nil {
				return nil, err
			}
			bindings[durable] = binding
			if !binding.bound() {
				unbound = append(unbound, durable+" is "+binding.String())
			}
		}
		return unbound, nil
	}
	// stopping is whether a stop was sent that has not been undone; lastStop is what the last stop
	// found once every thread had stopped.
	stopping, lastStop := false, "no stop has been made yet"
	testwait.Until(t, "the process to stop while it delivers from every delivery durable and holds no transaction open", func() (withOutput[string], bool) {
		if hasExited(process) {
			t.Fatalf("the process exited before it was frozen:\n%s", process.Output.String())
		}
		if !stopping {
			unbound, err := unboundOf()
			if err != nil {
				return readWith(fmt.Sprintf("the process runs, and its bindings could not be read: %v; %s", err, lastStop), process.Output), false
			}
			if len(unbound) > 0 {
				return readWith(fmt.Sprintf("the process runs with [%s]; %s", strings.Join(unbound, "; "), lastStop), process.Output), false
			}
			stoppedAt, stopping = process.signal(t, syscall.SIGSTOP), true
		}
		if thread, stopped := threadsStopped(process); !stopped {
			return readWith(fmt.Sprintf("the stop sent at %s has not stopped every thread: %s; %s",
				stoppedAt.Format(time.RFC3339Nano), thread, lastStop), process.Output), false
		}
		unbound, err := unboundOf()
		if err != nil {
			return readWith(fmt.Sprintf("the stop sent at %s has stopped every thread, and the bindings it froze with could not be read: %v; %s",
				stoppedAt.Format(time.RFC3339Nano), err, lastStop), process.Output), false
		}
		var open []string
		if err := pair.database.Pool.QueryRow(context.Background(),
			`select coalesce(array_agg(format('pid %s (%s, %s since %s): %s', pid, application_name, state, xact_start, left(query, 200))), '{}')
			 from pg_stat_activity
			 where datname = current_database() and backend_type = 'client backend'
			   and pid <> pg_backend_pid() and xact_start is not null`).Scan(&open); err != nil {
			return readWith(fmt.Sprintf("the stop sent at %s has stopped every thread, and the transactions open on the pair's database could not be read: %v; %s",
				stoppedAt.Format(time.RFC3339Nano), err, lastStop), process.Output), false
		}
		lastStop = fmt.Sprintf("the last stop, sent at %s, found %d transactions open on the pair's database %v and the durables unbound [%s]",
			stoppedAt.Format(time.RFC3339Nano), len(open), open, strings.Join(unbound, "; "))
		if len(unbound) == 0 && len(open) == 0 {
			return readWith(lastStop, process.Output), true
		}
		t.Logf("%s; the process runs again, to be stopped once it delivers from every durable again", lastStop)
		process.signal(t, syscall.SIGCONT)
		stopping = false
		return readWith(lastStop, process.Output), false
	})
	return bindings, stoppedAt
}

// waitFrozen waits for every thread of the process to be stopped as the kernel itself reports
// it: /proc/<pid>/task/*/stat's state, T for a stopped task. signal returns when the signal was
// sent and not when the process has stopped, and the difference is a message: a holder still
// running when the row publishes can be handed one and freeze with it in its handler, which
// leaves the message unacknowledged for the take-over to redeliver to the standby. Both would
// then handle it -- the at-least-once repeat every holder that stops mid-handling causes, and
// what the frozen-holder row is looking for is the opposite, a process that handles a message
// it no longer holds. One run of 20 published into that window and read it as a repeat. Its
// failure names the thread it last found running, with that thread's stat line, or what kept it
// from reading the threads (threadsStopped).
func waitFrozen(t *testing.T, p *dispatchProcess) {
	t.Helper()
	testwait.Until(t, "every thread of the frozen process to be stopped", func() (threadRead, bool) { return threadsStopped(p) })
}

// threadsStopped is one read of p's threads (/proc/<pid>/task/*/stat): whether every one of them
// is stopped (state T), and otherwise the first one that is not, with its stat line, or what kept
// the read from naming one.
func threadsStopped(p *dispatchProcess) (threadRead, bool) {
	threads := fmt.Sprintf("/proc/%d/task/*/stat", p.Cmd.Process.Pid)
	tasks, err := filepath.Glob(threads)
	if err != nil {
		return threadRead{err: err}, false
	}
	if len(tasks) == 0 {
		return threadRead{err: fmt.Errorf("no thread matches %s", threads)}, false
	}
	for _, task := range tasks {
		stat, err := os.ReadFile(task)
		if err != nil {
			return threadRead{err: err}, false
		}
		// The state follows comm, which is parenthesised and may hold spaces of its own.
		name := bytes.LastIndexByte(stat, ')')
		if name < 0 || name+2 >= len(stat) || stat[name+2] != 'T' {
			return threadRead{task: task, stat: stat}, false
		}
	}
	return threadRead{}, true
}

// threadRead is threadsStopped's reading: a thread of the process that is not stopped, with its
// stat line, or what kept the read of the threads from naming one.
type threadRead struct {
	task string
	stat []byte
	err  error
}

func (r threadRead) String() string {
	if r.err != nil {
		return r.err.Error()
	}
	return fmt.Sprintf("%s reads %q", r.task, bytes.TrimSpace(r.stat))
}

// mergedPullRequestAuthor is the one population author the row's delivery settings name, and the
// author of every merge it publishes, so each one is a pull request the intake completes rather
// than one it drops before it reads GitHub (delivery.IsPopulationPR).
const mergedPullRequestAuthor = "octocat"

// seedMergedPullRequestSettings is seedDeliverySettings with mergedPullRequestAuthor as the one
// population author, which the rows about the durables themselves have no need of: without it
// every merge published here is outside the population, and the intake answers it with no
// GitHub call at all, so no handling could be counted.
func seedMergedPullRequestSettings(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := delivery.PutSettings(context.Background(), database.Pool, delivery.DeliverySettings{
		DeployRepo:           "acme/deploy",
		DeployWorkflowPath:   ".github/workflows/deploy.yaml",
		PRChecksWorkflowPath: ".github/workflows/pr.yaml",
		ProductionJobNames:   []string{"production"},
		PopulationAuthors:    []string{mergedPullRequestAuthor},
	}, model.Actor{Kind: "user", ID: dispatchTestLogin}); err != nil {
		t.Fatalf("seed the delivery settings: %v", err)
	}
}

// mergedPullRequestFake is the loopback GitHub the pair's intakes read while they handle a
// merged pull request. It is one state served once per process (serve), so every handling is
// recorded with the process whose server answered it: startGitHubFake is one server the two
// processes share, and which of them handled a message cannot be read from that. The two-build
// suite's pullRequestFake records a handling the same way, per build.
//
// A handling is counted at the pull request's own fetch, GET /repos/{owner}/{repo}/pulls/{number}
// (delivery.FetchPullRequest): the merge path's first GitHub call, and the one call it makes
// exactly once however the handling then ends. Every other path is answered as the suite's own
// GitHub answers it (githubFakeAnswer), with the commit listing the fetch and the completion
// both page added.
type mergedPullRequestFake struct {
	mu   sync.Mutex
	done map[int][]pullRequestHandling
}

// pullRequestHandling is one handling of one merged pull request: the process whose GitHub
// answered its fetch, as the pair names it (dispatchPair.name), and when.
type pullRequestHandling struct {
	process string
	at      time.Time
}

func (h pullRequestHandling) String() string { return h.process + " at " + h.at.Format("15:04:05.000") }

func newMergedPullRequestFake() *mergedPullRequestFake {
	return &mergedPullRequestFake{done: map[int][]pullRequestHandling{}}
}

// serve starts a server on the fake's state whose handlings are process's, and returns its URL:
// the DISPATCH_GITHUB_API_BASE that process boots with.
func (f *mergedPullRequestFake) serve(t *testing.T, process string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.done[number] = append(f.done[number], pullRequestHandling{process: process, at: time.Now()})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"number":%d,"title":"A merged pull request","html_url":"https://github.com/acme/deploy/pull/%d",`+
			`"user":{"login":%q},"labels":[{"name":"non-task"}],"state":"closed","merged":true,`+
			`"created_at":"2026-10-26T00:00:00Z","merged_at":"2026-10-27T00:00:00Z","merge_commit_sha":"%040d",`+
			`"additions":1,"deletions":1,"head":{"ref":"a-branch","sha":"%040d","repo":{"full_name":"acme/deploy"}},`+
			`"base":{"ref":"main"}}`, number, number, mergedPullRequestAuthor, number, number)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"sha":"0000000000000000000000000000000000000001",`+
			`"commit":{"message":"A change.","author":{"date":"2026-10-26T00:00:00Z"},"committer":{"date":"2026-10-26T00:00:00Z"}}}]`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		status, body := githubFakeAnswer(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

// handled is every handling of every pull request, by number, in the order they were made.
func (f *mergedPullRequestFake) handled() map[int][]pullRequestHandling {
	f.mu.Lock()
	defer f.mu.Unlock()
	done := make(map[int][]pullRequestHandling, len(f.done))
	for number, handlings := range f.done {
		done[number] = slices.Clone(handlings)
	}
	return done
}

// publishMergedPullRequests publishes count GitHub-shaped messages, from pull request number
// first on: each a merge of a pull request of acme/deploy by mergedPullRequestAuthor, on
// notifications.github.acme.deploy.pr.<number>. It answers the numbers it published. run is this
// run's own, since the notification stream suppresses a repeat of a dedupe key for
// DeliveryDuplicateWindow and the row publishes against a server a harness may keep between runs.
//
// The payload carries what Envoy's normalizer puts in a `pull_request` envelope
// (contracts/normalize.go), head_repo above all: a merge whose head is named is a complete read
// of the pull request's attribution, so the head row it writes is stamped read and judged by
// this build's rule, and the delivery reconcile's merged arm never offers it again
// (delivery.ListStalePullRequestHeads). A reconcile pass that did read one of these pull
// requests again would read it through the very call that counts a handling here, and the row
// would see it as a second handling of a message.
func publishMergedPullRequests(t *testing.T, js natsgo.JetStreamContext, run string, first, count int) []int {
	t.Helper()
	numbers := make([]int, 0, count)
	for index := range count {
		number := first + index
		id := fmt.Sprintf("frozen-holder-%s-%d", run, number)
		payload, err := json.Marshal(map[string]string{
			"kind": "pr", "action": "closed", "merged": "true",
			"repo": "acme/deploy", "number": strconv.Itoa(number), "author": mergedPullRequestAuthor,
			"title": "A merged pull request", "url": fmt.Sprintf("https://github.com/acme/deploy/pull/%d", number),
			"head_repo": "acme/deploy", "head_ref": "a-branch", "head_sha": fmt.Sprintf("%040d", number),
			"base_ref": "main", "merge_commit_sha": fmt.Sprintf("%040d", number),
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			t.Fatalf("encode the payload of pull request %d: %v", number, err)
		}
		envelope := contracts.Envelope{
			EventID:       id,
			Source:        "github",
			SourceEventID: id,
			Topic:         contracts.GithubResourceSubject("acme", "deploy", "pr", strconv.Itoa(number)),
			DedupeKey:     id,
			IssuedAt:      time.Now().Unix(),
			Payload:       string(payload),
			TraceID:       id,
		}
		data, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("encode the envelope of pull request %d: %v", number, err)
		}
		if _, err := js.Publish(envelope.Topic, data, natsgo.MsgId(id)); err != nil {
			t.Fatalf("publish %s: %v", envelope.Topic, err)
		}
		numbers = append(numbers, number)
	}
	return numbers
}

// waitForHandled waits for each of numbers to have been handled by one of the processes, and
// names those that were not when it gives up, with output, the running processes' output: the
// process holding the durable's hold delivers them, and no other does.
func waitForHandled(t *testing.T, output fmt.Stringer, fake *mergedPullRequestFake, numbers []int) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("pull requests %v to be handled by the process holding %s's hold", numbers, bus.DispatchDeliveryDurable), func() (withOutput[[]int], bool) {
		handled := fake.handled()
		var missing []int
		for _, number := range numbers {
			if len(handled[number]) == 0 {
				missing = append(missing, number)
			}
		}
		return readWith(missing, output), len(missing) == 0
	})
}

// requireHandledBy requires each of numbers to have been handled, and every handling of it to be
// process's. when names the round for a failure.
func requireHandledBy(t *testing.T, pair *dispatchPair, fake *mergedPullRequestFake, numbers []int, process *dispatchProcess, when string) {
	t.Helper()
	handled := fake.handled()
	want := pair.name(process)
	for _, number := range numbers {
		handlings := handled[number]
		if len(handlings) == 0 {
			t.Errorf("pull request %d, published %s, was handled by neither process.\n%s", number, when, pair.buffers())
			continue
		}
		for _, handling := range handlings {
			if handling.process != want {
				t.Errorf("pull request %d, published %s, was handled by %s, want %s, the process delivering from %s then.\n%s",
					number, when, handling, want, bus.DispatchDeliveryDurable, pair.buffers())
			}
		}
	}
}

// waitForTakeOver waits for durable to be standby's to deliver from, having been the binding
// before's: NATS has it push-bound to a deliver subject that is not before's, exactly one client
// connection subscribes there, its hold is live and records that subject under a holder that is
// not the one before recorded (durableBinding.bound), and standby has logged a bind of the
// durable beyond the bound lines it had logged already. That one subscriber is standby's: the
// take-over mints the deliver subject (delivery.heldDurable.takeOver), so a process frozen
// before it existed can have no subscription to it. Its failure prints both processes' output.
func waitForTakeOver(t *testing.T, pair *dispatchPair, js natsgo.JetStreamContext, durable string,
	before durableBinding, standby *dispatchProcess, bound int,
) durableBinding {
	t.Helper()
	var binding durableBinding
	testwait.Until(t, fmt.Sprintf("%s to take %s over from the frozen holder, where it was %s, with a bind it logs past the %d it had logged by the freeze: a frozen holder renews its hold no more, so the hold lapses within delivery's intakeHoldTTL and the standby's next poll of it takes the durable over", pair.name(standby), durable, before, bound), func() (withOutput[string], bool) {
		binding = readDurableBinding(t, pair, js, durable)
		took := binding.bound() && binding.subject != before.subject && binding.hold.holder != before.hold.holder
		logged := len(deliveryOutcomeFor(standby, durable).bound)
		return readWith(fmt.Sprintf("NATS has it %s, and %s has logged %d binds of it", binding, pair.name(standby), logged), pair.buffers()), took && logged > bound
	})
	return binding
}

// waitForDurableDrained waits for the durable to have nothing left to deliver and nothing
// unacknowledged: no pending message, none awaiting an acknowledgement, and its acknowledgement
// floor at what it has delivered, by consumer sequence and by stream sequence. Every message
// published by then has been handled and acknowledged once, and the take-over left none behind.
// when names the moment for a failure, which prints output, the running processes' output.
func waitForDurableDrained(t *testing.T, output fmt.Stringer, js natsgo.JetStreamContext, durable, when string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("%s to have nothing pending and nothing awaiting acknowledgement %s, its ack floor at what it delivered", durable, when), func() (withOutput[string], bool) {
		info, err := js.ConsumerInfo(bus.Stream, durable)
		if err != nil {
			t.Fatalf("read %s: %v", durable, err)
		}
		return readWith(fmt.Sprintf("%d pending and %d awaiting acknowledgement, its ack floor at consumer %d of stream %d against %d of %d delivered",
				info.NumPending, info.NumAckPending, info.AckFloor.Consumer, info.AckFloor.Stream, info.Delivered.Consumer, info.Delivered.Stream), output),
			info.NumPending == 0 && info.NumAckPending == 0 &&
				info.AckFloor.Consumer == info.Delivered.Consumer && info.AckFloor.Stream == info.Delivered.Stream
	})
}

// A process whose NATS connection drops while it holds delivery-events keeps it, and goes on
// delivering from it once the link is back. What says which process delivers from a durable is
// its hold, and a hold is Postgres's (delivery.intakeHoldName): a cut link takes nothing of it
// away, the holder renews it throughout the outage, and the process standing by polls for a hold
// another process holds live and takes nothing. So a process back from a NATS cut has no second
// subscriber to deliver beside, which is what INFRA-1551 is about: nats.go re-sends a
// subscription inside its own reconnect, and the client binds its registration again on a
// replacement connection, so a durable another process had taken meanwhile would be delivered to
// both, with no bind of its own in any log to say so.
//
// A holder that stops renewing is the case where the durable does move, and there the take-over
// leaves the returned process nothing to deliver from: it recreates the durable on a deliver
// subject minted while that process was away. Two rows hold that already -- in process,
// delivery.TestAHolderCutOffAndRestoredHandlesNothingTheStandbyTookOver, and over two real
// processes, TestAFrozenHolderIsTakenOverAndHandlesNothingOnceResumed.
//
// What two real processes show here, and one cannot, is the fan-out: one pull request merged
// after the link is restored is fetched exactly once from the GitHub the two share. A second
// bound subscriber is handed that same envelope and fetches that same pull request, which no log
// line reports and which ConsumerInfo.PushBound -- one bool for one subscriber and for two --
// cannot say either.
//
// The fake answers the fetch GitHub's 404, which is all this row needs: it counts the fetch, not
// its answer. The intake logs the failed fetch at WARN ("process event") and acknowledges the
// message, so the 404 is redelivered to nobody and neither process logs a delivery error for the
// durable, which this row requires, as the one-binder row does.
//
// Both processes take their holds for a TTL past the run, and end each read and take of a hold
// and each GitHub call at the test's give-up (heldDeliveryEnv), so a renewal a loaded machine makes late
// hands no durable over and a slow statement or call writes no delivery error. That a cut leaves
// the renewals running is then read from the renewals themselves: the hold's expiry moves past
// the one it had at the cut, while the link is still down.
//
// Each process tries the delivery reconcile's pass as it starts (DISPATCH_TEST_RECONCILE_AT_START),
// on a goroutine of its own; one runs it and the other finds the lease held or the period served.
// The row waits for both of those first ticks before it publishes the envelope, so the pass has
// run before the envelope is published, and no pass runs after that. A later pass would read
// again the pull request the envelope names: the envelope names no head repository, so the intake
// writes that pull request's head row unread (Intake.recordPullRequestHead), and a pass re-reads
// every merged head row no read has reached (ListStalePullRequestHeads). A pass inside the second
// the fetches are counted in (deliverySecondFetchWindow) would make a second fetch on correct
// code. So the reconcile ticks here on staggeredSweepPeriod
// (DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL), and its lease period, duty.LeasePeriod of that, has
// one bucket from 1970's start to 2072. The first pass at start serves that bucket, and every
// later attempt of either process falls in it: the other process's pass at start, and each
// jittered tick due before 2072, wherever its draw puts it. Each finds the period served, or the
// lease held while the first pass runs, and runs nothing (duty.Exclusive). At
// delivery.ReconcileInterval the period was four and a half minutes, and a first tick due past its
// end, a uniform draw within five minutes of each start, ran a pass that fell inside that second
// now and then.
func TestAProcessBackFromANATSCutKeepsDeliveryEventsAndDeliversThemOnce(t *testing.T) {
	durable := bus.DispatchDeliveryDurable
	// The merged pull request the row publishes. Its author has to be one the settings name: a
	// merge by an author outside the population costs no GitHub call at all
	// (Intake.handlePullRequestEnvelope), and the call is what this row counts.
	const (
		owner  = "acme"
		repo   = "widgets"
		number = "7"
		author = "octocat"
	)

	pair := newPair(t)
	seedDeliverySettings(t, pair.database)
	addPopulationAuthor(t, pair.database, author)
	fake := startGitHubFake(t)
	fake.Release()
	env := heldDeliveryEnv(t, githubAppEnv(t, fake.URL()))
	env["DISPATCH_TEST_RECONCILE_AT_START"] = "1"
	env["DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL"] = staggeredSweepPeriod.String()
	pair.start(t, env)
	// Each first tick writes its line only once its pass, or its skip, has ended (duty.Exclusive),
	// on the reconcile's goroutine, so the row waits for the lines rather than reading them once.
	waitForLease(t, pair, leaseRan, "delivery-reconcile")
	waitForLeaseTickInEach(t, pair, "delivery-reconcile")

	conn := testnats.Connect(t, pair.nats)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}

	binding, holder := waitForDurableBinder(t, pair, js, durable)
	standby := pair.other(holder)
	t.Logf("%s delivers from %s (%s) and %s is standing by for it", pair.name(holder), durable, binding, pair.name(standby))
	requireOneHoldPerDurable(t, pair)

	// The link drops under the holder, as an outage drops it, and stays down for the whole window
	// a lapsed hold would be taken over in.
	proxy := pair.proxy(holder)
	proxy.Cut()
	expiresAtCut := readDurableHoldExpiry(t, pair.database, durable)
	cutAt := time.Now()
	for range int(deliveryHoldSurvivesACutFor / oneBinderSampleEvery) {
		testwait.Span(t, oneBinderSampleEvery, standbyPollsWhy)
		if hold := readDurableHold(t, pair.database, durable); !hold.found || !hold.live || hold.token != binding.subject {
			t.Fatalf("%s's hold is %s %v into %s's NATS outage, want it still live and still recording the deliver subject that process delivers at (%s): the renewal is a Postgres statement, which the cut leaves alone.\n%s",
				durable, hold, time.Since(cutAt).Round(time.Millisecond), pair.name(holder), binding.subject, pair.buffers())
		}
		if bound := deliveryOutcomeFor(standby, durable).bound; len(bound) > 0 {
			t.Fatalf("%s bound %s %v into the other process's NATS outage, when the hold %s moves under stayed %s's throughout:\n%s\n%s",
				pair.name(standby), durable, time.Since(cutAt).Round(time.Millisecond), durable, pair.name(holder),
				strings.Join(bound, "\n"), pair.buffers())
		}
	}

	// The holder renews its hold while the link is down: a renewal sets the hold's expiry from the
	// database's now() (duty.Hold), so an expiry past the one read before the cut is a renewal made
	// during it. The proxy restores the link only once one is.
	testwait.Until(t, fmt.Sprintf("%s to renew its hold of %s while its NATS link is cut (an expiry past %s)", pair.name(holder), durable, expiresAtCut.Format(time.RFC3339Nano)),
		func() (withOutput[time.Time], bool) {
			expires := readDurableHoldExpiry(t, pair.database, durable)
			return readWith(expires, pair.buffers()), expires.After(expiresAtCut)
		})
	proxy.Restore()
	reading := testwait.Context(t)
	testwait.Until(t, "the cut process's NATS connection coming back", func() (reportRead, bool) {
		report, err := holder.readHealthReport(reading)
		return reportRead{report, err}, err == nil && report.NATS != nil && *report.NATS
	})
	// The binding the two servers report is the one the process had before the cut: the same
	// deliver subject, one client connection subscribed to it, and the same holder of the same
	// live hold recording it (durableBinding.bound). The connection's own id is the one thing
	// here the cut does change -- nats.go reconnects on a new one -- so the two bindings are
	// compared by those three and not by the whole line. The wait is for that reconnect to have
	// re-sent the subscription, which is when the server counts the subscriber again.
	testwait.Until(t, fmt.Sprintf("%s, %s before the cut, to deliver to the same subject with one subscriber on it under the same holder once the link came back: a NATS outage takes no durable from the process whose hold names it", durable, binding),
		func() (withOutput[durableBinding], bool) {
			back := readDurableBinding(t, pair, js, durable)
			return readWith(back, pair.buffers()), back.bound() && back.subject == binding.subject && back.hold.holder == binding.hold.holder
		})
	t.Logf("%s was delivering from %s again %v after the cut", pair.name(holder), durable, time.Since(cutAt).Round(time.Millisecond))
	requireOneHoldPerDurable(t, pair)

	// One envelope, published on the pair's own NATS once the link is back: a pull request merged
	// by a population author, in the shape Envoy's publisher gives a GitHub resource event
	// (cmd/dispatch/shutdown_test.go). The pair's NATS server is this test's own, so the stream
	// holds no envelope of another run under this event id.
	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "closed", "merged": "true",
		"repo": owner + "/" + repo, "number": number, "author": author,
	})
	if err != nil {
		t.Fatalf("encode the merged pull request's payload: %v", err)
	}
	subject := contracts.GithubResourceSubject(owner, repo, "pr", number)
	eventID := "nats-cut-pr-" + number
	envelope, err := json.Marshal(contracts.Envelope{
		EventID: eventID, Source: "github", SourceEventID: eventID, TraceID: eventID,
		Topic: subject, Payload: string(payload), IssuedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("encode the merged pull request's envelope: %v", err)
	}
	publishedAt := time.Now()
	if _, err := js.Publish(subject, envelope); err != nil {
		t.Fatalf("publish the merged pull request on %s: %v", subject, err)
	}

	testwait.Until(t, fmt.Sprintf("the pull request of the one envelope published on %s to be fetched", subject),
		func() (fmt.Stringer, bool) {
			return pair.buffers(), fake.pullRequestFetches() > 0
		})
	t.Logf("the merged pull request was fetched %v after it was published", time.Since(publishedAt).Round(time.Millisecond))
	// The count is watched after it reaches one rather than read the moment it does: two bound
	// subscribers are handed one envelope at the same moment, so the second fetch is milliseconds
	// behind the first and a read taken at once would miss what this row exists to catch.
	testwait.Span(t, deliverySecondFetchWindow, "a second subscriber's fetch of the same envelope is what this row "+
		"must not see: the fake records a fetch only once it has happened, so no state says one is not on its way, "+
		"both processes are children so synctest does not reach them, and a later action of the test's would publish "+
		"an envelope of its own rather than settle this one")
	if fetches := fake.pullRequestFetches(); fetches != 1 {
		t.Errorf("the one pull request published after the restore was fetched %d times from the GitHub the two processes share, and a process standing by for %s fetches nothing:\n%s",
			fetches, durable, pair.buffers())
	}

	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		if errorLines := deliveryOutcomeFor(process, durable).errors; len(errorLines) > 0 {
			t.Errorf("%s logged a delivery error for %s; a NATS outage under the holder and a standby polling for a hold another process holds are both ordinary, and so is the fake's 404 for the pull request, which the intake logs at WARN and acknowledges:\n%s\n%s",
				pair.name(process), durable, strings.Join(errorLines, "\n"), pair.buffers())
		}
	}
	// The period each reconcile tick served, or found served, is read off its line, a record of the
	// interval the loop was given, and no clock: duty.LeasePeriod of staggeredSweepPeriod has one
	// bucket from 1970's start, and a reconcile main.go starts at delivery.ReconcileInterval rather
	// than at the interval DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL gives serves the
	// four-and-a-half-minute bucket its tick fell in, which lets a later pass land in the counting
	// second again. A skip that found the lease held names no period, so only the skips that found
	// the period served are read. The row waited for a pass line after the start, so there is at
	// least one line to read.
	firstBucket := time.Unix(0, 0).UTC().Format(time.RFC3339Nano)
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		ticks := slices.Concat(process.wholeLogLines(leaseRan, "lease=delivery-reconcile"),
			process.wholeLogLines(leaseSkipped, "lease=delivery-reconcile", "period="))
		for _, line := range ticks {
			if period := leasePeriodOf(t, line); period != firstBucket {
				t.Errorf("%s's delivery-reconcile tick was in the period starting %s, want %s, the first bucket of staggeredSweepPeriod: "+
					"the reconcile did not run at the interval DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL gave it: %s", pair.name(process), period, firstBucket, line)
			}
		}
	}
	pair.requireRunning(t)
}

// deliveryHoldSurvivesACutFor is how long a holder cut off from NATS is watched still holding its
// durable's hold: the whole of a deployment's intake hold TTL (delivery's intakeHoldTTL, 15 s),
// since a hold is at its oldest the moment before a renewal, and three of the standby's poll
// intervals (delivery.durableBindInterval, 2 s), so a standby that read the hold as lapsed would
// have had three turns to take it. The row's holds run at a TTL past the run (heldDeliveryEnv), so
// no lapse in it is lawful, and the span is the absence the row reads: no take-over lands in it.
const deliveryHoldSurvivesACutFor = 15*time.Second + 3*2*time.Second

// deliverySecondFetchWindow is how long the fetch count is watched after it has reached one. A
// second bound subscriber is handed the envelope at the same moment as the first, so its fetch is
// milliseconds behind; a second of stillness is the margin, not the expectation.
const deliverySecondFetchWindow = time.Second

// addPopulationAuthor adds author to the delivery settings' population, leaving every other
// setting as it is: the row is read back and written whole, which is the write the Delivery page's
// own save makes (delivery.PutSettings). seedDeliverySettings names no population author, and a
// merge by an author outside the population costs no GitHub call
// (Intake.handlePullRequestEnvelope), so a row that counts the fetch of a merged pull request puts
// its author in the population before the processes start.
func addPopulationAuthor(t *testing.T, database *store.Store, author string) {
	t.Helper()
	settings, err := delivery.GetSettings(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings to add %s to the population: %v", author, err)
	}
	settings.PopulationAuthors = append(settings.PopulationAuthors, author)
	if _, err := delivery.PutSettings(context.Background(), database.Pool, settings,
		model.Actor{Kind: "user", ID: dispatchTestLogin}); err != nil {
		t.Fatalf("add %s to the delivery population: %v", author, err)
	}
}

// deliveryOutcome is what one process has logged for one delivery durable: its binds of it and
// its delivery errors on it.
type deliveryOutcome struct {
	bound  []string
	errors []string
}

// oneBinderWindow is how long requireOneBinderThroughout watches each durable's binding stand
// still: three of the standby's bind intervals (delivery.durableBindInterval, 2 s) and a margin,
// so the standby polled for every durable at least three times inside the window, and a poll
// that bound one would be in the samples.
const oneBinderWindow = 7 * time.Second

// oneBinderSampleEvery is how often inside that window the two servers are read.
const oneBinderSampleEvery = 500 * time.Millisecond

// standbyPollsWhy is why a row that watches a durable stay one process's waits out each sample
// (oneBinderSampleEvery) as a span of its own: the standby takes a durable only on a poll of its
// own clock that finds the hold lapsed, and logs no poll that found it live, so no state says the
// polls have come; both processes are children, which synctest does not reach; and no action of
// the test's comes after a poll the standby makes on its own.
const standbyPollsWhy = "the standby takes a durable only on a poll of its own that finds the hold lapsed and records no poll " +
	"that found it live, both processes are children so synctest does not reach them, and no action of the test's " +
	"comes after a poll the standby makes on its own clock"

// durableBinding is what the servers themselves say of one delivery durable at one moment:
// whether NATS holds a consumer of that name at all, the deliver subject it delivers to and
// whether a subscriber is bound there, every client connection the NATS server has a
// subscription to that subject on, and the durable's hold as Postgres records it.
type durableBinding struct {
	exists      bool
	subject     string
	pushBound   bool
	subscribers []string
	hold        durableHold
}

// bound reports whether the durable is one process's to deliver from: NATS delivers it to one
// deliver subject that exactly one client connection subscribes to, and the durable's hold is
// live and records that subject, holding it being what makes the process subscribed there the
// one delivering from it (delivery.intakeHoldName, delivery.intakeHoldToken).
func (b durableBinding) bound() bool {
	return b.exists && b.pushBound && len(b.subscribers) == 1 &&
		b.hold.found && b.hold.live && b.hold.token == b.subject
}

// String is what a sample is reported and compared by: a binding that moved shows as the two
// sentences differing, whichever part of it moved.
func (b durableBinding) String() string {
	if !b.exists {
		return "no consumer of that name on the stream, held by " + b.hold.String()
	}
	return fmt.Sprintf("deliver subject %s (push-bound %t), subscribed by [%s], held by %s",
		b.subject, b.pushBound, strings.Join(b.subscribers, ", "), b.hold)
}

// durableHold is the row of a delivery durable's hold: whether duty_holds holds one of that name
// at all, who holds it, whether it is live on the database's clock, and the deliver subject it
// records (delivery.intakeHoldToken).
type durableHold struct {
	name   string
	found  bool
	holder string
	token  string
	live   bool
}

func (h durableHold) String() string {
	if !h.found {
		return "nobody: duty_holds holds no row named " + h.name
	}
	return fmt.Sprintf("%s (live %t, token %q)", h.holder, h.live, h.token)
}

// requireOneBinderThroughout watches each durable's binding for oneBinderWindow and requires it
// to stay the one process's that first holds it: the same deliver subject, the same one client
// connection subscribed to it, the same live hold recording it (durableBinding.bound), and no
// bind of the durable logged by the process standing by. Every sample is read from the two
// servers, so a second process that bound the durable shows as a second subscriber, or as a
// deliver subject and a hold token that changed under the samples, since a take-over subscribes
// to a fresh deliver subject of its own and records it (delivery.heldDurable.takeOver).
func requireOneBinderThroughout(t *testing.T, pair *dispatchPair, js natsgo.JetStreamContext,
	first map[string]durableBinding, holders map[string]*dispatchProcess,
) {
	t.Helper()
	requireOneHoldPerDurable(t, pair)
	started := time.Now()
	for range int(oneBinderWindow / oneBinderSampleEvery) {
		testwait.Span(t, oneBinderSampleEvery, standbyPollsWhy)
		for _, durable := range deliveryDurables {
			holder := holders[durable]
			binding := readDurableBinding(t, pair, js, durable)
			if !binding.bound() || binding.String() != first[durable].String() {
				t.Errorf("%s was %s and is %s %v later, within the window %s holds it: nothing takes a durable over from the process holding its hold.\n%s",
					durable, first[durable], binding, time.Since(started).Round(time.Millisecond), pair.name(holder), pair.buffers())
				return
			}
			standby := pair.other(holder)
			if bound := deliveryOutcomeFor(standby, durable).bound; len(bound) > 0 {
				t.Errorf("%s bound %s %v after %s was already delivering from it under the durable's hold:\n%s\n%s",
					pair.name(standby), durable, time.Since(started).Round(time.Millisecond), pair.name(holder),
					strings.Join(bound, "\n"), pair.buffers())
				return
			}
		}
	}
}

// requireOneHoldPerDurable requires that the holds the intakes took for the delivery durables are
// one per durable and not one per process: duty_holds holds delivery-intake-<durable> and nothing
// else of that shape. That one name per durable is what every envoy-dispatch process of every
// build takes (delivery.intakeHoldName), and so what holds every process but one off it; a name
// of each process's own would hold nobody off.
func requireOneHoldPerDurable(t *testing.T, pair *dispatchPair) {
	t.Helper()
	rows, err := pair.database.Pool.Query(context.Background(),
		`select name from duty_holds where name like 'delivery-intake-%' order by name`)
	if err != nil {
		t.Fatalf("read the delivery intakes' holds: %v", err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the delivery intakes' holds: %v", err)
	}
	want := make([]string, 0, len(deliveryDurables))
	for _, durable := range deliveryDurables {
		want = append(want, "delivery-intake-"+durable)
	}
	if !slices.Equal(held, want) {
		t.Errorf("the delivery intakes hold %v, want the one hold per durable every process takes, %v:\n%s", held, want, pair.buffers())
	}
}

// readDurableBinding reads one durable's binding from the NATS server and its hold from Postgres
// (durableBindingOf), and fails the test when a read fails.
func readDurableBinding(t *testing.T, pair *dispatchPair, js natsgo.JetStreamContext, durable string) durableBinding {
	t.Helper()
	binding, err := durableBindingOf(pair, js, durable)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

// durableBindingOf reads one durable's binding from the NATS server and its hold from Postgres,
// and answers the error of the first read that fails, for a wait whose try reports a failed read
// in its reading rather than failing the test (freezeHolder).
func durableBindingOf(pair *dispatchPair, js natsgo.JetStreamContext, durable string) (durableBinding, error) {
	var binding durableBinding
	info, err := js.ConsumerInfo(bus.Stream, durable)
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
	case err != nil:
		return binding, fmt.Errorf("read %s: %w", durable, err)
	default:
		binding.exists, binding.subject, binding.pushBound = true, info.Config.DeliverSubject, info.PushBound
		if binding.subscribers, err = natsSubscribersOf(pair, info.Config.DeliverSubject); err != nil {
			return binding, err
		}
	}
	if binding.hold, err = durableHoldOf(pair.database, durable); err != nil {
		return binding, err
	}
	return binding, nil
}

// natsSubscribers names every client connection the pair's NATS server has a subscription to
// subject on (natsSubscribersOf), and fails the test when the listing cannot be read.
func natsSubscribers(t *testing.T, pair *dispatchPair, subject string) []string {
	t.Helper()
	subscribers, err := natsSubscribersOf(pair, subject)
	if err != nil {
		t.Fatal(err)
	}
	return subscribers
}

// natsSubscribersOf names every client connection the pair's NATS server has a subscription to
// subject on, from the server's own connection listing (/connz) rather than from what a process
// logged: a second process delivering from a durable is a second connection here, whatever either
// wrote. The read waits under the pair's shared-wait context (monitorReads), so a listing a loaded
// machine answers late is waited for, and one not answered by the wait's give-up is an error.
func natsSubscribersOf(pair *dispatchPair, subject string) ([]string, error) {
	endpoint := pair.natsServer.MonitorURL() + "/connz?subs=1&limit=4096"
	request, err := http.NewRequestWithContext(pair.monitorReads, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET %s: %w", endpoint, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	var listing struct {
		Connections []struct {
			Cid           uint64   `json:"cid"`
			Name          string   `json:"name"`
			Subscriptions []string `json:"subscriptions_list"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		return nil, fmt.Errorf("decode %s: %w", endpoint, err)
	}
	var subscribers []string
	for _, connection := range listing.Connections {
		if slices.Contains(connection.Subscriptions, subject) {
			subscribers = append(subscribers, fmt.Sprintf("%s (cid %d)", connection.Name, connection.Cid))
		}
	}
	return subscribers, nil
}

// readDurableHold reads the row of the durable's hold, or the absence of one (durableHoldOf), and
// fails the test when the read fails.
func readDurableHold(t *testing.T, database *store.Store, durable string) durableHold {
	t.Helper()
	hold, err := durableHoldOf(database, durable)
	if err != nil {
		t.Fatal(err)
	}
	return hold
}

// durableHoldOf reads the row of the durable's hold, or the absence of one, and answers the read's
// error when it fails.
func durableHoldOf(database *store.Store, durable string) (durableHold, error) {
	hold := durableHold{name: "delivery-intake-" + durable}
	err := database.Pool.QueryRow(context.Background(),
		`select holder::text, token, expires_at > now() from duty_holds where name = $1`,
		hold.name).Scan(&hold.holder, &hold.token, &hold.live)
	if errors.Is(err, pgx.ErrNoRows) {
		return hold, nil
	}
	if err != nil {
		return hold, fmt.Errorf("read the hold of %s: %w", durable, err)
	}
	hold.found = true
	return hold, nil
}

// readDurableHoldExpiry reads when the durable's hold expires, on the database's clock: each
// renewal sets it a TTL past the database's now() (duty.Hold). A durable with no hold fails the
// test.
func readDurableHoldExpiry(t *testing.T, database *store.Store, durable string) time.Time {
	t.Helper()
	var expires time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select expires_at from duty_holds where name = $1`, "delivery-intake-"+durable).Scan(&expires); err != nil {
		t.Fatalf("read when the hold of %s expires: %v", durable, err)
	}
	return expires
}

// heldDeliveryEnv is env with every intake's hold staged past the run: each process takes its
// holds for stagedPastTheRun (DISPATCH_TEST_INTAKE_HOLD_TTL), staged state, and ends each read and
// take of a hold and each read of a durable (DISPATCH_TEST_INTAKE_HOLD_CALL_DEADLINE) and each
// GitHub call (DISPATCH_TEST_GITHUB_REQUEST_DEADLINE) at the moment the test's waits give up
// (endsAtTheGiveUp). A renewal a loaded machine makes late then moves no durable, and a statement
// or a call it answers slowly writes no delivery error, so a row that watches a durable stay one
// process's watches the defect alone. A hold staged so lapses only where a row lets it
// (lapseDurableHold).
func heldDeliveryEnv(t *testing.T, env map[string]string) map[string]string {
	env["DISPATCH_TEST_INTAKE_HOLD_TTL"] = stagedPastTheRun.String()
	env["DISPATCH_TEST_INTAKE_HOLD_CALL_DEADLINE"] = endsAtTheGiveUp(t)
	env["DISPATCH_TEST_GITHUB_REQUEST_DEADLINE"] = endsAtTheGiveUp(t)
	return env
}

// lapseDurableHold ends the hold holder has of the durable now, on the database's clock, keeping
// the row and its token, as the hold's TTL ends it for a holder that renews it no more: the
// statement is duty's own lapse of a hold (duty.Hold's lapseHold), with no notification, as a TTL
// sends none. A row that killed the holder stages that dead holder's lapse with it, its TTL being
// past the run (heldDeliveryEnv); the next poll of the process standing by finds the hold lapsed
// and takes the durable over. Only a dead holder's hold is let lapse so: a live holder's own loss
// deadline is its TTL less the margin, and a row that ended its row sooner would have it deliver
// past the lapse, which the hold's margin is there to prevent. It fails the test when the hold no
// longer names holder.
func lapseDurableHold(t *testing.T, database *store.Store, durable, holder string) {
	t.Helper()
	lapsed, err := database.Pool.Exec(context.Background(),
		`update duty_holds set expires_at = now() where name = $1 and holder::text = $2`, "delivery-intake-"+durable, holder)
	if err != nil {
		t.Fatalf("let the hold of %s lapse: %v", durable, err)
	}
	if lapsed.RowsAffected() != 1 {
		t.Fatalf("the hold of %s no longer names %s, the holder the row lets it lapse for: %s", durable, holder, readDurableHold(t, database, durable))
	}
}

// admissionWait is how long a process refused at boot waits for admission (processes.Register)
// when it beats at heartbeat (DISPATCH_TEST_PROCESS_HEARTBEAT): its live window, which main.go
// sets to three heartbeats, the ratio of production's processes.LiveWindow to its
// processes.HeartbeatInterval, and one heartbeat more.
func admissionWait(heartbeat time.Duration) time.Duration {
	return heartbeat*(processes.LiveWindow/processes.HeartbeatInterval) + heartbeat
}

// deliveryOutcomeFor reads what the process has logged for the durable so far, sorting every
// delivery line naming it into the binds and the errors.
func deliveryOutcomeFor(process *dispatchProcess, durable string) deliveryOutcome {
	var outcome deliveryOutcome
	for _, line := range process.logLines("dispatch delivery:", "name="+durable) {
		switch {
		case strings.Contains(line, "level=ERROR"):
			outcome.errors = append(outcome.errors, line)
		case strings.Contains(line, `msg="dispatch delivery: NATS consumer bound"`):
			outcome.bound = append(outcome.bound, line)
		}
	}
	return outcome
}

// boundBy is the processes of the pair that logged a bind of the durable, A before B.
func boundBy(pair *dispatchPair, durable string) []*dispatchProcess {
	var bound []*dispatchProcess
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		if len(deliveryOutcomeFor(process, durable).bound) > 0 {
			bound = append(bound, process)
		}
	}
	return bound
}

// waitForDurableBinder waits for the durable to be one process's to deliver from, and answers
// that binding and the process whose it is: NATS has the durable push-bound with exactly one
// client connection subscribed to its deliver subject and its hold live and recording that
// subject (durableBinding.bound), and exactly one of the two processes logged a bind of it,
// which is that process.
//
// The binding, and not the standing-by line of the process that did not bind it. That line is
// written on the standby's own poll of a durable it will not take, so it can come long after the
// durable is one process's, or not at all while the hold does its work: waiting for it is what
// failed 13 of 1,066 dispatch-multiprocess job attempts on main (INFRA-1568), in a run where the
// durable's holder had already been read.
//
// Both processes poll their durables every two seconds (delivery.durableBindInterval) from the
// moment each has read the delivery settings, and the first of them to take a durable's hold
// binds it on that poll. A process that has exited ends the wait at once, since a binding it
// never made will not arrive. Either failure prints both processes' output.
func waitForDurableBinder(t *testing.T, pair *dispatchPair, js natsgo.JetStreamContext, durable string) (durableBinding, *dispatchProcess) {
	t.Helper()
	var binding durableBinding
	var holder *dispatchProcess
	testwait.Until(t, fmt.Sprintf("%s to be one process's to deliver from: a process polls its durables every two seconds once it has read the delivery settings, and the first of the two to take the durable's hold binds it", durable), func() (withOutput[string], bool) {
		// Whether each has exited is read before the servers and the logs, since Exited closes
		// only once the output is whole: what is read after that flag is that process's last
		// word, and what is read before it may yet be added to.
		var gone []*dispatchProcess
		for _, process := range []*dispatchProcess{pair.a, pair.b} {
			if hasExited(process) {
				gone = append(gone, process)
			}
		}
		binding = readDurableBinding(t, pair, js, durable)
		bound := boundBy(pair, durable)
		if binding.bound() && len(bound) == 1 {
			holder = bound[0]
			return withOutput[string]{}, true
		}
		if len(gone) > 0 {
			t.Fatalf("%s exited before %s was one process's to deliver from: NATS has it %s, and %s logged a bind of it.\n%s",
				processNames(pair, gone), durable, binding, processNames(pair, bound), pair.buffers())
		}
		return readWith(fmt.Sprintf("NATS has it %s, and %s logged a bind of it", binding, processNames(pair, bound)), pair.buffers()), false
	})
	return binding, holder
}

// hasExited reports whether the process has exited, without waiting for it.
func hasExited(process *dispatchProcess) bool {
	select {
	case <-process.Exited:
		return true
	default:
		return false
	}
}

// processNames is what a failure calls a set of the pair's processes.
func processNames(pair *dispatchPair, processes []*dispatchProcess) string {
	if len(processes) == 0 {
		return "neither process"
	}
	names := make([]string, len(processes))
	for i, process := range processes {
		names[i] = pair.name(process)
	}
	return strings.Join(names, " and ")
}

// ───── documents ───────────────────────────────────────────────────────────

// A spec tab is open on one process while the other writes the document. The write announces
// itself over Postgres (dispatch_doc_head, with dispatch_doc_changed beside it for older builds)
// and A catches the room it holds up from the stored rows: the editor sees the write in its open
// socket, nobody is disconnected, A closes no room, and the reader that comes next reads the
// current document.
func TestADocumentWriteOnOneProcessReachesTheOpenEditorOnTheOther(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "DOC")
	pair.a.createProject(t, project, "Documents")
	_, artifactID := pair.a.createIssueIn(t, project, "Open here, written there", "before")

	editor := pair.a.openEditor(t, artifactID, "before\n")
	wrote := time.Now()
	pair.b.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		`{"ops":[{"op":"replace","find":"before","with":"after"}]}`, http.StatusOK)
	if err := editor.WaitForText(t, markdown, "after\n"); err != nil {
		t.Fatalf("the editor on A never saw B's write: %v; %s\nA:\n%s", err, pair.roomCloses(t, artifactID), pair.a.Output.String())
	}
	t.Logf("B's write reached the editor on A %v after it began", time.Since(wrote))
	pair.requireConnected(t, artifactID, "the editor on A", editor, "after B's write")
	pair.requireNoRoomCloses(t, artifactID)
	if text := pair.a.documentText(t, artifactID); text != "after\n" {
		t.Errorf("A serves %q after B's write; want the current document, %q", text, "after\n")
	}
}

// storedHead is the version of the document's latest durable update.
func storedHead(t *testing.T, pair *dispatchPair, artifactID string) int64 {
	t.Helper()
	var head int64
	if err := pair.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(version), 0) from doc_updates where artifact_id = $1`, artifactID).Scan(&head); err != nil {
		t.Fatalf("read the stored head of %s: %v", artifactID, err)
	}
	return head
}

// Rendering is not an acknowledgement of a browser update's durable append. A tab the rig takes
// as synced has had its own update stored (markEditor), so a row that opens a tab and marks it
// and then writes on the other process is not racing that tab's own append; this row is what
// says the rig keeps that promise. A transaction of the test's own holds doc_updates, so no
// append of the document can commit: the marker's wait holds, and the append it waits for is the
// one queued on the table lock. Once the lock goes, the wait ends and the marker leaves the
// document the tab opened as it was.
//
// The tab opens on the test goroutine; a sync that changes nothing stores nothing, so the lock
// does not hold it. The marker's wait runs on a goroutine of its own and answers an error rather
// than failing the test, and the row keeps the order the promise needs: the append queued on the
// lock, then the wait still holding while the gate stays shut, then the gate let go, then the
// wait's end. A wait that ended on the room's own acknowledgement of the marker, before the
// append that stores it committed, ends while the gate is shut, and fails the row by name.
func TestSyncedEditorWaitsForItsOwnAppend(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "SYNC")
	pair.a.createProject(t, project, "Documents")
	_, artifactID := pair.a.createIssueIn(t, project, "Held browser append", "before")
	gate, err := pair.database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	if _, err := gate.Exec(context.Background(), `lock table doc_updates in share mode`); err != nil {
		t.Fatal(err)
	}
	editor := pair.openSyncedEditor(t, pair.a, dispatchTestLogin, artifactID, "before\n")
	marked := make(chan error, 1)
	go func() { marked <- pair.markEditor(t.Name(), pair.a, dispatchTestLogin, artifactID, editor) }()
	// The tab's append is stored on the room-appends pool, whose connections name themselves
	// dispatch-room-appends in pg_stat_activity (store's roomAppendsApplicationName). Every other
	// insert into doc_updates - a settlement's, a server write's (docs' append_writes.go and
	// persistence.go) - runs on another pool, so the name tells the marker's append from them.
	testwait.Until(t, "the marker's append to be waiting on the lock", func() (string, bool) {
		select {
		case err := <-marked:
			t.Fatalf("the marker's wait returned (%v) while its append was held", err)
		default:
		}
		var appends, others int
		if err := pair.database.Pool.QueryRow(context.Background(), `
			select count(*) filter (where application_name = 'dispatch-room-appends'),
			       count(*) filter (where application_name <> 'dispatch-room-appends')
			from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like '%insert into doc_updates%'
		`).Scan(&appends, &others); err != nil {
			t.Fatalf("count the waits on doc_updates' lock: %v", err)
		}
		return fmt.Sprintf("%d browser appends and %d other inserts wait on doc_updates' lock", appends, others), appends > 0
	})
	testwait.Quiet(t, "the marker's wait while its append is still held", marked, 100*time.Millisecond,
		"what must not happen is a wait that answers before its own append committed: the append is parked in "+
			"PostgreSQL holding no state of the product's to read, the tab runs in this process over a real socket "+
			"so synctest does not reach it, and the only later action there is - committing the gate - is the thing "+
			"that releases it")
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatalf("let the gate on doc_updates go: %v", err)
	}
	if err := testwait.Receive(t, "the marker's wait to end once its append was released", marked); err != nil {
		t.Fatal(err)
	}
	if markdown(editor) != "before\n" {
		t.Fatalf("sync marker changed the document to %q", markdown(editor))
	}
}

// markEditor sends a metadata marker from editor, login's tab on process holding the document,
// and returns once that very update is stored, so every sync frame ahead of it on the socket has
// been taken by the room and stored too. A row that marks a tab this way knows the tab's own
// writing is durable, so only a later write somewhere else can leave the tab's room behind the
// durable head. The marker changes no text, so what the tab renders is what it opened. It takes
// no t, so a goroutine of a test's can run it: its wait is the shared wait's form for a caller
// with no t (testwait.InHelper, naming test), and it answers what went wrong as an error.
func (p *dispatchPair) markEditor(test string, process *dispatchProcess, login, artifactID string, editor *docstest.Peer) error {
	markers := editor.Doc.GetMap("test-sync")
	marker := strconv.FormatUint(uint64(editor.Doc.ClientID()), 10)
	update, err := editor.Send(func(txn *crdt.Transaction) { markers.Set(txn, marker, true) })
	if err != nil {
		return fmt.Errorf("send %s's sync marker: %w", login, err)
	}
	var failed error
	if err := testwait.InHelper(test, login+"'s sync marker to be stored", func() bool {
		var stored bool
		if err := p.database.Pool.QueryRow(context.Background(),
			`select exists(select 1 from doc_updates where artifact_id = $1 and update = $2)`, artifactID, update).Scan(&stored); err != nil {
			failed = fmt.Errorf("read %s's stored sync marker: %w", login, err)
			return true
		}
		if stored {
			return true
		}
		select {
		case <-editor.Ended:
			failed = fmt.Errorf("%s's tab closed before its sync marker was stored:\n%s", login, process.Output.String())
			return true
		default:
			return false
		}
	}); err != nil {
		return err
	}
	return failed
}

// ───── the leased maintenance loops ────────────────────────────────────────

// The two lines duty.Exclusive writes, which every leased loop reports its pass through.
const (
	leaseRan     = `msg="dispatch duty: leased pass ran"`
	leaseSkipped = `msg="dispatch duty: leased pass skipped"`
)

// countLease is how many of message's lines the process wrote for lease.
func countLease(p *dispatchProcess, message, lease string) int {
	return len(p.logLines(message, "lease="+lease))
}

// waitForLease waits for either process to write message for lease.
func waitForLease(t *testing.T, pair *dispatchPair, message, lease string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("either process to write %s for the %s lease", message, lease), func() (fmt.Stringer, bool) {
		if countLease(pair.a, message, lease)+countLease(pair.b, message, lease) > 0 {
			return nil, true
		}
		return pair.buffers(), false
	})
}

// waitForLeaseInEach waits for both processes to have written message for lease.
func waitForLeaseInEach(t *testing.T, pair *dispatchPair, message, lease string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("both processes to write %s for the %s lease", message, lease), func() (fmt.Stringer, bool) {
		if countLease(pair.a, message, lease) > 0 && countLease(pair.b, message, lease) > 0 {
			return nil, true
		}
		return pair.buffers(), false
	})
}

// waitForLeaseTickInEach waits for both processes to have reported a tick of lease, a pass or a
// skip, so a count of the lines after it reads every first tick: neither a pass nor a skip is
// required of either process here, which is what the caller's count judges.
func waitForLeaseTickInEach(t *testing.T, pair *dispatchPair, lease string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("both processes to report a tick of the %s lease, a pass or a skip", lease), func() (fmt.Stringer, bool) {
		ticked := func(process *dispatchProcess) bool {
			return countLease(process, leaseRan, lease)+countLease(process, leaseSkipped, lease) > 0
		}
		return pair.buffers(), ticked(pair.a) && ticked(pair.b)
	})
}

// requireLeasedOnce is what a leased loop's window must show: the pass ran once across the two
// processes, which is what one process alone would have done, and the other process found the
// lease held and skipped rather than running the same work beside it.
func requireLeasedOnce(t *testing.T, pair *dispatchPair, lease string) {
	t.Helper()
	ranA, ranB := countLease(pair.a, leaseRan, lease), countLease(pair.b, leaseRan, lease)
	skippedA, skippedB := countLease(pair.a, leaseSkipped, lease), countLease(pair.b, leaseSkipped, lease)
	t.Logf("%s: A ran %d skipped %d, B ran %d skipped %d", lease, ranA, skippedA, ranB, skippedB)
	if ranA+ranB != 1 {
		t.Errorf("the %s pass ran %d times across the two processes; one process alone would have run it once",
			lease, ranA+ranB)
	}
	if ranA == 1 && skippedB == 0 {
		t.Errorf("A ran the %s pass and B never reported finding the lease held", lease)
	}
	if ranB == 1 && skippedA == 0 {
		t.Errorf("B ran the %s pass and A never reported finding the lease held", lease)
	}
}

// requireLeasedOncePerPeriod is what a leased loop's window must show for a row whose own pass
// can take longer than its period: at least one pass across the two processes, and no period in
// which both of them passed. That is exactly what the lease guarantees. requireLeasedOnce's
// "once overall" is a stronger claim than the lease makes, and a row whose run time passes its
// period breaks it honestly: the block index's period is 30 s (docs.BlockIndexReconcileInterval)
// and the webhook redelivery's 108 s (duty.LeasePeriod of redeliver.Interval's two minutes), so a
// pass that overruns leaves the next honest pass in the next period and the count becomes two.
// Waiting out more boundaries cannot mend that, since each boundary it waits for is another
// period a pass may serve (INFRA-985).
func requireLeasedOncePerPeriod(t *testing.T, pair *dispatchPair, lease string) {
	t.Helper()
	passes := map[string][]string{}
	for name, process := range map[string]*dispatchProcess{"A": pair.a, "B": pair.b} {
		for _, line := range process.logLines(leaseRan, "lease="+lease) {
			period := leasePeriodOf(t, line)
			passes[period] = append(passes[period], name)
		}
	}
	skippedA, skippedB := countLease(pair.a, leaseSkipped, lease), countLease(pair.b, leaseSkipped, lease)
	t.Logf("%s: passes by period %v, A skipped %d, B skipped %d", lease, passes, skippedA, skippedB)
	if len(passes) == 0 {
		t.Errorf("the %s pass never ran across the two processes", lease)
	}
	for period, ran := range passes {
		if len(ran) > 1 {
			t.Errorf("the %s pass ran %d times in the period starting %s, by %v; the lease allows one pass a period",
				lease, len(ran), period, ran)
		}
	}
	// No skip is required. The other process skips only where its own tick falls inside the
	// window the first one is passing in; a tick that lands in the next period finds that period
	// unserved and passes, which is the lease working rather than failing. The skips are logged
	// above for a reader of a failure.
}

// leasePeriodOf is the period= field of one `leased pass ran` line, which names the period that
// pass served (duty.Exclusive).
func leasePeriodOf(t *testing.T, line string) string {
	t.Helper()
	return logAttributeOf(t, line, "period")
}

// logAttributeOf is the name= field of one whole slog text line, unquoted, and fails t when the
// line carries none.
func logAttributeOf(t *testing.T, line, name string) string {
	t.Helper()
	for _, match := range logAttribute.FindAllStringSubmatch(line, -1) {
		if match[1] != name {
			continue
		}
		value := match[2]
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		return value
	}
	t.Fatalf("a log line carries no %s=: %s", name, line)
	return ""
}

// wholeLogLines is logLines with only the lines the process has finished writing: it is still
// running while the test reads its output, and the last line can have been written only in part,
// before the attributes a caller reads off it.
func (p *dispatchProcess) wholeLogLines(parts ...string) []string {
	var matched []string
	for raw := range strings.Lines(p.Output.String()) {
		if line, whole := strings.CutSuffix(raw, "\n"); whole && holdsAll(line, parts...) {
			matched = append(matched, line)
		}
	}
	return matched
}

// passesLogged is the highest pass= of the whole lines the process wrote holding every one of
// parts, 0 for none: one loop's numbered pass lines (delivery.DurableChecked for one durable,
// startup.AnnouncerPassed), which a process started with DISPATCH_TEST_DEBUG_MESSAGES naming them
// writes at DEBUG, numbered from 1 in that process.
func passesLogged(t *testing.T, process *dispatchProcess, parts ...string) uint64 {
	t.Helper()
	var highest uint64
	for _, line := range process.wholeLogLines(parts...) {
		pass, err := strconv.ParseUint(logAttributeOf(t, line, "pass"), 10, 64)
		if err != nil {
			t.Fatalf("read the pass number of %s: %v", line, err)
		}
		highest = max(highest, pass)
	}
	return highest
}

// waitForPassAfter waits for the process to log a pass of the loop whose lines parts match
// numbered past since+1. since is the highest pass it had logged when the caller read it
// (passesLogged), and the pass after that one may have been under way then, so the one after it
// is the first that began after that moment: what the loop does in a whole pass after it is in
// the process's output once this returns. A process that never logs it fails the wait, naming
// the loop.
func waitForPassAfter(t *testing.T, process *dispatchProcess, loop string, since uint64, parts ...string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("%s to log pass %d, the first to begin after pass %d was logged", loop, since+2, since),
		func() (uint64, bool) {
			logged := passesLogged(t, process, parts...)
			return logged, logged >= since+2
		})
}

// lockTables holds every table named in mode (`exclusive`, or `access exclusive`) in a transaction
// of the test's own, as a writer that is taking its time would, and rolls it back when the test
// ends if the test has not. A maintenance pass whose first statement writes one of them - or, in
// access exclusive mode, reads one - waits there, inside its lease, which is the moment the lease
// is about: both processes' loops firing while one is still in its pass. Its statements run on the
// shared wait's context (testwait.Context), so a lock another transaction still holds is waited for
// until the give-up and then fails naming the table.
func lockTables(t *testing.T, database *store.Store, mode string, tables ...string) pgx.Tx {
	t.Helper()
	ctx := testwait.Context(t)
	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding %v: %v", tables, err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	for _, table := range tables {
		if _, err := gate.Exec(ctx, "lock table "+table+" in "+mode+" mode"); err != nil {
			t.Fatalf("hold %s: %v", table, err)
		}
	}
	return gate
}

// waitForPassBehind waits for a session of the pair's database whose current statement carries
// fragment to queue behind a lock holder's transaction owns (storetest.QueuedBehind), and fails
// naming the sessions it last found running such a statement, with both processes' output, when
// none does. The passes it watches run in the two processes under test, whose connections no
// pool of this test's can give, so the statement is what names the pass: a count of the backends
// behind the gate would also take the term-statistics refresh, which writes search_term_totals,
// and the block-index pass, which reads document_block_versions, for the reference-excerpt pass -
// all three queue behind the gates this test holds, and a count cannot tell them apart. It reads
// pg_stat_activity through the pool, a fresh view on each read.
func waitForPassBehind(t *testing.T, pair *dispatchPair, holder pgx.Tx, fragment, what string) {
	t.Helper()
	ctx := context.Background()
	testwait.Until(t, what, func() (withOutput[[]uint32], bool) {
		rows, err := pair.database.Pool.Query(ctx, `
			select pid from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid() and position($1 in query) > 0
		`, fragment)
		if err != nil {
			t.Fatalf("read the sessions running a statement carrying %q: %v", fragment, err)
		}
		pids, err := pgx.CollectRows(rows, pgx.RowTo[uint32])
		if err != nil {
			t.Fatalf("read the sessions running a statement carrying %q: %v", fragment, err)
		}
		for _, pid := range pids {
			queued, err := storetest.QueuedBehind(ctx, holder, pid)
			if err != nil {
				t.Fatal(err)
			}
			if queued {
				return readWith(pids, pair.buffers()), true
			}
		}
		return readWith(pids, pair.buffers()), false
	})
}

// The first statement of each pass this test waits for, as pg_stat_activity shows it: the
// reference-excerpt pass's write (refs.MarkStaleExcerpts) and the block-index pass's read of the
// documents owed an index (docs' owedDocuments), which reads each of its two partial indexes in a
// branch of its own and holds this text in both.
const (
	referenceExcerptPassStatement = "update refs r set excerpt_version = null"
	blockIndexPassStatement       = "select artifact_id from document_block_versions"
)

// Every leased maintenance loop takes a named lease for the length of its pass, so the second
// process's attempt finds it held and skips instead of doing the same work beside it. Each loop
// observed here is held inside its own first statement while the other process's attempt fires -
// by a table lock this test takes, or by the GitHub listing the fake holds - which is the only way
// two processes' passes are made to overlap on purpose rather than by luck.
//
// Five of the fourteen leases are observable here inside a window a test can wait out, and they
// keep different rules once the hold lets go. webhook-redelivery, block-index and
// search-term-stats are timer loops whose first pass is immediate, and what this row holds them
// to is per period: at least one pass across the two processes, and no period served twice.
// document-compaction's first tick is at start too, and it is not held inside its pass: both
// processes run it at staggeredSweepPeriod (DISPATCH_TEST_COMPACTION_SWEEP_PERIOD), whose one
// bucket holds both first ticks however long the starts take, so the row holds it to what the
// lease and the period record give two ticks of one period: one pass across the two processes,
// and the other process's tick skipping (requireLeasedOnce). The pass's own line names the period
// it served, and that must be staggeredSweepPeriod's first bucket, which opens at 1970's start: a
// sweep main.go starts at docs.CompactionSweepPeriod's hour, not at the period the setting gives,
// serves the hour its tick fell in. reference-excerpts is a
// woken loop (docs.RunReferenceExcerptReconciliation) that runs a pass at start and again on the
// pglisten.Hub wake after its first LISTEN: the process that found the lease held retries (every
// two seconds, its MinGap) until it runs a pass of its own, because the other's pass may have
// read before whatever woke it, so after the release each process runs at least one pass and no
// count is held to one. The other nine are not observable, and this row says so rather than
// pretending: issue-ask-repair's first leased pass is one IssueAskRepairInterval (10 min) in, the
// boot's repair taking no lease; suggestion-outcomes is held only by the keyed writes'
// maintenance sweep now (api.RunMaintenanceSweep, the suggestion outcome sweep taking no lease),
// which no longer touches write_suggestions, the table this row once held it by, and whose first
// leased pass is one MaintenanceSweepInterval (1 min) in on a phase of each process's own
// (api.TestTheMaintenanceLoopSweepsOncePerPeriodAcrossTwoRunners holds what its lease is for);
// architecture-import's and delivery-reconcile's first pass is jittered uniformly over
// SyncInterval/ReconcileInterval (5 min) and each process draws its own, so whether their passes
// ever overlap is a coin toss no test should turn into a flake, and so is whether the second's
// tick falls in the epoch-aligned bucket of 4.5 min (duty.LeasePeriod of the 5 min) the first's
// pass served, where it is refused, or in the next, where it runs; edit-notices is a woken loop
// whose passes here, with nothing due, take milliseconds, so whether the two processes' boot
// passes meet is luck too (docs.TestEditNoticeSweepRetriesAHeldLease holds its retry, and
// docs.TestEditNoticeSweepOfTwoServicesSendsEachNoticeOnce what its lease is for);
// settlement-resumption's period is SettlementResumeInterval (1 min) and each pass is over in
// milliseconds, so this window holds several of its periods, each served once;
// server-processes' prune is one delete on server_processes, the table every process's heartbeat
// writes every five seconds, so a lock that held the prune would hold the registry itself;
// text-tree-fill's first pass is at each process's start, and on a database whose stored texts
// already carry their trees it takes one candidate scan and ends done; and parent-status-fix is
// taken only at a boot with DISPATCH_PARENT_STATUS_RULE on, which this row does not set
// (api.TestTheFixRunsOnceUnderItsLeaseBesideASecondProcess holds what its lease is for).
func TestALeasedLoopsPassesNeverOverlapAcrossBothProcesses(t *testing.T) {
	pair := newPair(t)
	fake := startGitHubFake(t)
	// The term-statistics refresh writes nothing for a database with no project, so one exists
	// before either process starts.
	if _, err := pair.database.Pool.Exec(context.Background(), `insert into projects (key, name, default_assignee) values ('LEASE', 'Lease', 'alice')`); err != nil {
		t.Fatalf("create the project the term-statistics refresh rebuilds: %v", err)
	}
	// refs is where the reference-excerpt reconciliation's first statement writes
	// (refs.MarkStaleExcerpts), and search_term_totals where the term-statistics refresh's first
	// write for a project goes (its first statement only reads projects, which an exclusive lock
	// does not hold).
	gate := lockTables(t, pair.database, "exclusive", "refs", "search_term_totals")
	// The block-index reconciliation's first statements read document_block_versions and
	// block_grammar_owed, which only access exclusive mode holds back. They are held in a gate of
	// their own, released as soon as the other process has found the lease held: that pass's
	// interval is 30 s, so released later with the gate above, the other process's own next tick
	// could land beside the release and make the next period's pass before this row counts the
	// first.
	blockGate := lockTables(t, pair.database, "access exclusive", "document_block_versions", "block_grammar_owed")
	env := githubAppEnv(t, fake.URL())
	env["DISPATCH_TEST_COMPACTION_SWEEP_PERIOD"] = staggeredSweepPeriod.String()
	pair.start(t, env)

	waitForPassBehind(t, pair, gate, referenceExcerptPassStatement,
		"a reference-excerpt pass to reach the refs lock, which is one process taking the lease")
	waitForLease(t, pair, leaseSkipped, "reference-excerpts")
	if ran := countLease(pair.a, leaseRan, "reference-excerpts") + countLease(pair.b, leaseRan, "reference-excerpts"); ran != 0 {
		t.Errorf("a reference-excerpt pass reported running %d times while the one holding the lease is still inside the refs lock", ran)
	}
	waitForLease(t, pair, leaseSkipped, "search-term-stats")
	if ran := countLease(pair.a, leaseRan, "search-term-stats") + countLease(pair.b, leaseRan, "search-term-stats"); ran != 0 {
		t.Errorf("a term-statistics pass reported running %d times while the one holding the lease is still inside the search_term_totals lock", ran)
	}

	waitForPassBehind(t, pair, blockGate, blockIndexPassStatement,
		"a block-index pass to reach the document_block_versions lock, which is one process taking the lease")
	waitForLease(t, pair, leaseSkipped, "block-index")
	if ran := countLease(pair.a, leaseRan, "block-index") + countLease(pair.b, leaseRan, "block-index"); ran != 0 {
		t.Errorf("a block-index pass reported running %d times while the one holding the lease is still inside the document_block_versions lock", ran)
	}
	if err := blockGate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the tables the block-index pass is waiting on: %v", err)
	}
	waitForLease(t, pair, leaseRan, "block-index")
	requireLeasedOncePerPeriod(t, pair, "block-index")

	// The webhook redelivery sweep's first pass is held in its first GitHub call instead: it
	// lists the App webhook's failed deliveries before it writes anything. What the release below
	// needs is that pass to be inside that call, and the proof of that is the fake's own count:
	// the fake counts the listing and only then holds it, so a counted listing means a pass is in
	// the call or about to block in it. The pass's own log line is not that proof - main.go writes
	// it at boot, before the sweeper goroutine starts, so it is there before any GitHub call is
	// made. The other process's skip is no proof either, since a tick that lands in the next
	// period passes rather than skipping, and waiting for a skip here failed for exactly that
	// reason - two honest passes, each in a period of its own, and no skip at all (INFRA-985).
	testwait.Until(t, "a webhook delivery listing to reach the fake, which is a redelivery pass inside its GitHub call", func() (fmt.Stringer, bool) {
		return pair.buffers(), fake.deliveryListings() > 0
	})
	fake.Release()
	waitForLease(t, pair, leaseRan, "webhook-redelivery")
	requireLeasedOncePerPeriod(t, pair, "webhook-redelivery")

	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the table the passes are waiting on: %v", err)
	}
	// The process that skipped retries a little over two seconds after each skip, so the waits
	// below end once the held pass has finished and that retry has run a pass of its own.
	waitForLeaseInEach(t, pair, leaseRan, "reference-excerpts")
	waitForLease(t, pair, leaseRan, "search-term-stats")
	// Both processes' first compaction-sweep ticks came at their starts, inside the one bucket of
	// staggeredSweepPeriod: one of them passes and the other finds the period served, or the lease
	// held while the first is still passing.
	waitForLeaseTickInEach(t, pair, "document-compaction")
	requireLeasedOnce(t, pair, "document-compaction")
	// The period the pass served is read off its line, a record of the period the sweep was given,
	// and no clock (staggeredSweepPeriod's first bucket opens at 1970's start).
	firstBucket := time.Unix(0, 0).UTC().Format(time.RFC3339Nano)
	for name, process := range map[string]*dispatchProcess{"A": pair.a, "B": pair.b} {
		for _, line := range process.logLines(leaseRan, "lease=document-compaction") {
			if period := leasePeriodOf(t, line); period != firstBucket {
				t.Errorf("%s's document-compaction pass served the period starting %s, want %s, staggeredSweepPeriod's first bucket: "+
					"the sweep did not run at the period DISPATCH_TEST_COMPACTION_SWEEP_PERIOD gave it", name, period, firstBucket)
			}
		}
	}
	requireLeasedOncePerPeriod(t, pair, "search-term-stats")
	for _, unobserved := range []struct{ lease, why string }{
		{"issue-ask-repair", "its first leased pass is one IssueAskRepairInterval (10 min) after each process started"},
		{"suggestion-outcomes", "only the keyed writes' maintenance sweep holds it, which no longer touches write_suggestions; api.TestTheMaintenanceLoopSweepsOncePerPeriodAcrossTwoRunners holds its lease"},
		{"architecture-import", "its first pass is jittered over SyncInterval (5 min) and each process draws its own"},
		{"delivery-reconcile", "its first pass is jittered over ReconcileInterval (5 min) and each process draws its own"},
		{"edit-notices", "a woken loop whose passes take milliseconds here, so its boot passes meet by luck"},
		{"settlement-resumption", "its period is SettlementResumeInterval (1 min), so the window holds several periods, each served once"},
		{"server-processes", "its pass is one delete on server_processes, which every process's heartbeat writes, so holding it would hold the heartbeats too"},
		{"text-tree-fill", "its first pass is at each process's start, and on a database whose stored texts already carry their trees it takes one candidate scan and ends done"},
		{"parent-status-fix", "it is taken only at a boot with DISPATCH_PARENT_STATUS_RULE on, which this row does not set; api.TestTheFixRunsOnceUnderItsLeaseBesideASecondProcess holds its lease"},
	} {
		if ran := countLease(pair.a, leaseRan, unobserved.lease) + countLease(pair.b, leaseRan, unobserved.lease); ran > 0 {
			t.Logf("%s ran %d times in the window, which this row does not hold to a count: %s",
				unobserved.lease, ran, unobserved.why)
			continue
		}
		t.Logf("%s was not observed: %s", unobserved.lease, unobserved.why)
	}
}

// The suggestion-outcome sweep (api.RunSuggestionOutcomeSweep) is a woken loop that takes no
// lease: each of its pages runs in a transaction holding the suggestion_outcome_cursor row `for
// update`, which is what keeps two processes from judging one page twice. Each process runs a
// pass at start, and the two start together, so their boot passes meet. Both reach that row - a
// lease would have left one of them outside it, which is the change this row is about - and this
// test holds the row with a lock of its own to see both arrive. Once the lock lets go, one pass
// judges the page and advances the cursor and the other, which was waiting on the row, reads the
// cursor that pass advanced: no wake is lost to another process's pass, and neither process
// judges what the other judged. Neither boot pass failed: the row reads each process's output
// once that process's sweep is in its next pass, a minute after its boot pass began (the loop's
// gap), which orders each boot pass's end, and any failure it logged, before the read.
func TestTheSuggestionOutcomeSweepSerialisesOnTheCursorAcrossBothProcesses(t *testing.T) {
	pair := newPair(t)
	gate := lockTables(t, pair.database, "exclusive", "suggestion_outcome_cursor")
	pair.start(t, nil)

	// The wait fails the row by name at the binary's deadline, and the cleanup prints both
	// processes' output then, as this branch's failure did before the wait had a bound.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("A's output:\n%s\nB's output:\n%s", pair.a.Output.String(), pair.b.Output.String())
		}
	})
	storetest.WaitForLockWaiters(t, gate, 2)
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the cursor the passes are waiting on: %v", err)
	}
	// Both passes come out of the row in turn: the first judges every pending suggestion and sets
	// the cursor, the second reads it set and finds its page empty. A pass judges a page only once
	// the event horizon proves it (api/suggestion_outcomes.go), so the wait is a wait behind the
	// horizon (pollBehindHorizon on A), and a row that fails prints B's horizon's account too,
	// which names a hold there in this test's own database the way A's reading does.
	const advanced = "the cursor the first pass out of the lock advanced"
	behindB := horizontest.NewWatch(pair.b.stallWarnings(), pair.b.database)
	defer func() {
		if t.Failed() {
			t.Logf("B's horizon: %s\nB's output:\n%s", behindB, pair.b.Output.String())
		}
	}()
	pair.a.pollBehindHorizon(t, advanced, func() bool {
		var cursor *int64
		if err := pair.database.Pool.QueryRow(context.Background(), `select event_id from suggestion_outcome_cursor`).Scan(&cursor); err != nil {
			t.Fatalf("read the cursor: %v", err)
		}
		return cursor != nil
	})

	// The second pass out of the lock ends on its own goroutine after the cursor shows, and logs
	// a failure only as it ends, which nothing the processes write orders before a read of their
	// output. A process's sweep runs one pass at a time and logs a pass's failure before the pass
	// returns (api.RunSuggestionOutcomeSweep), so once each process's next pass waits on the row
	// again, behind a lock the test takes once both boot passes let it go, each boot pass has
	// ended and logged what it failed. A notification on the events channel owes each process
	// that next pass, which its loop starts no sooner than its gap after the boot pass's start.
	next := lockTables(t, pair.database, "exclusive", "suggestion_outcome_cursor")
	if _, err := pair.database.Pool.Exec(testwait.Context(t), `select pg_notify($1, '')`, api.EventsChannel); err != nil {
		t.Fatalf("wake both processes' suggestion-outcome sweeps: %v", err)
	}
	storetest.WaitForLockWaiters(t, next, 2)
	for name, process := range map[string]*dispatchProcess{"A": pair.a, "B": pair.b} {
		if failed := strings.Count(process.Output.String(), "suggestion outcome sweep failed"); failed != 0 {
			t.Errorf("process %s logged %d failed suggestion-outcome passes:\n%s", name, failed, process.Output.String())
		}
	}
}

// A rolling update starts the second process minutes after the first, and at a steady two
// replicas each process keeps a timer of its own: two ticks of one leased loop almost never
// overlap, because a pass is seconds and its interval is minutes. This row is that shape - the
// second process is started only after the first has finished a pass and given the lease back -
// and it is where "runs once overall" has to hold, not only while a pass is in flight.
//
// The webhook redelivery sweep is the loop driven here because its cost is countable from
// outside: every pass lists the App webhook's failed deliveries from GitHub before it writes
// anything, and its lease exists for exactly that reason ("N processes sweeping is N times those
// list calls for one result", redeliver/sweep.go). So the GitHub fake's count of those listings
// is what one sweep window cost the installation's rate limit, whichever process paid it.
//
// A period is a bucket of the tick's due time (duty.Exclusive, migration 0115): a second process
// whose first tick lands past a bucket boundary is entitled to sweep, and this row is about the
// one that lands inside the bucket its peer has already served. Both processes run the sweep at
// staggeredSweepPeriod, whose current bucket holds every tick the row makes, so which of the two
// this run gets is neither luck nor a race of the two starts against the clock. The period each
// tick's line names must be that bucket, which opens at 1970's start: a sweep that runs at another
// interval than the one DISPATCH_TEST_REDELIVERY_INTERVAL gives serves another period.
func TestALeasedLoopRunsOnceAcrossAStaggeredStart(t *testing.T) {
	pair := newPair(t)
	fake := startGitHubFake(t)
	// Nothing is held here: this row is about two passes that never meet, so the first one must
	// finish and give the lease back before the second process exists.
	fake.Release()
	env := githubAppEnv(t, fake.URL())
	env["DISPATCH_TEST_REDELIVERY_INTERVAL"] = staggeredSweepPeriod.String()

	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey, Env: env,
	})
	pair.a.waitHealthy(t)
	pair.a.waitForLog(t, leaseRan, "lease=webhook-redelivery")
	listedByA := fake.deliveryListings()
	if listedByA == 0 {
		t.Fatalf("the first process's sweep listed no webhook deliveries, so this row would measure nothing:\n%s",
			pair.a.Output.String())
	}

	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey, Env: env,
	})
	pair.b.waitHealthy(t)
	testwait.Until(t, "the second process to report a webhook-redelivery pass", func() (fmt.Stringer, bool) {
		return pair.b.Output, countLease(pair.b, leaseRan, "webhook-redelivery")+countLease(pair.b, leaseSkipped, "webhook-redelivery") > 0
	})

	requireLeasedOnce(t, pair, "webhook-redelivery")
	// The period each tick served, or found served, is read off its line, a record of the interval
	// the sweep was given, and no clock: the bucket of staggeredSweepPeriod (duty.LeasePeriod of it)
	// opens at 1970's start, and a sweep main.go starts at redeliver.Interval's two minutes, not at
	// the interval the setting gives, serves the 108 s bucket its tick fell in. A skip that found the
	// lease held names no period, so only the skips that found the period served are read.
	firstBucket := time.Unix(0, 0).UTC().Format(time.RFC3339Nano)
	for name, process := range map[string]*dispatchProcess{"A": pair.a, "B": pair.b} {
		ticks := slices.Concat(process.wholeLogLines(leaseRan, "lease=webhook-redelivery"),
			process.wholeLogLines(leaseSkipped, "lease=webhook-redelivery", "period="))
		for _, line := range ticks {
			if period := leasePeriodOf(t, line); period != firstBucket {
				t.Errorf("%s's webhook-redelivery tick was in the period starting %s, want %s, the first bucket of staggeredSweepPeriod: "+
					"the sweep did not run at the interval DISPATCH_TEST_REDELIVERY_INTERVAL gave it: %s", name, period, firstBucket, line)
			}
		}
	}
	if listed := fake.deliveryListings(); listed != listedByA {
		t.Errorf("the two processes listed the webhook's deliveries %d times for one sweep window; the first alone listed them %d times, and the lease is what keeps N processes from being N times those list calls",
			listed, listedByA)
	}
}

// staggeredSweepPeriod is the period a row gives a leased loop in place of production's, so that
// two processes' ticks fall in one period: the webhook redelivery sweep's interval in
// TestALeasedLoopRunsOnceAcrossAStaggeredStart (DISPATCH_TEST_REDELIVERY_INTERVAL, in place of
// redeliver.Interval's two minutes), the document compaction sweep's in
// TestALeasedLoopsPassesNeverOverlapAcrossBothProcesses (DISPATCH_TEST_COMPACTION_SWEEP_PERIOD, in
// place of docs.CompactionSweepPeriod's hour), and the delivery reconcile's in
// TestAProcessBackFromANATSCutKeepsDeliveryEventsAndDeliversThemOnce
// (DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL, in place of delivery.ReconcileInterval's five
// minutes). A period is a bucket of a tick's due time, floor(epoch / period) (duty.Exclusive): the
// compaction sweep's, a RunLeased loop's, is this period, whose first bucket runs from 1970 to
// 2084, and the redelivery sweep's and the delivery reconcile's, jittered loops', are nine tenths
// of it (duty.LeasePeriod), whose first bucket runs from 1970 to 2072. Both processes' first ticks
// fall in that one bucket however long their starts take, and a later tick while the row runs, if
// a jittered draw brings one, falls in it too and finds the period served.
const staggeredSweepPeriod = 1_000_000 * time.Hour

// ───── a sign-in across the processes ───────────────────────────────────────

// cookieSignIn is the environment that has a process sign people in through issuer's code flow
// under the group rule, on a plain-HTTP cookie, as the multiprocess sign-in rows run it.
func cookieSignIn(issuer *oidctest.Issuer) map[string]string {
	return map[string]string{
		"DISPATCH_IDENTITY":             "cookie",
		"DISPATCH_SIGNIN_ISSUER":        issuer.URL(),
		"DISPATCH_SIGNIN_CLIENT_ID":     cookieClientID,
		"DISPATCH_SIGNIN_CLIENT_SECRET": cookieClientSecret,
		"DISPATCH_SIGNIN_GROUP":         cookieGroup,
		"DISPATCH_INSECURE_COOKIE":      "1",
	}
}

// multiprocessServerURL is the one public origin a pair behind a load balancer is reached at,
// DISPATCH_SERVER_URL on both: each process builds the sign-in's redirect_uri from it, so the
// process that exchanges the code names the redirect_uri the process that started the sign-in
// sent the issuer, which the issuer requires. Without it each process would build its own from
// the request's Host, and the issuer would refuse the exchange on the other process. Nothing here
// resolves the name: the test carries every redirect itself.
const multiprocessServerURL = "http://dispatch.multiprocess.test"

// waitForSignInIssuer waits for each of processes to report on /healthz that it has read the
// sign-in issuer's discovery document (signin true). A process discovers the issuer in the
// background once it serves (main.go), and until then answers /auth/start and /auth/callback 503
// SIGNIN_UNAVAILABLE while /healthz already answers 200, so a row that signs someone in on a
// process waits for that fact, which no answer of /healthz's status says.
func waitForSignInIssuer(t *testing.T, processes ...*dispatchProcess) {
	t.Helper()
	for _, process := range processes {
		process.waitForHealth(t, "signin true, the sign-in issuer's discovery document read", func(health healthBody) bool {
			return health.SignIn != nil && *health.SignIn
		})
	}
}

// A person starts signing in on A and the issuer's redirect back lands on B, which is what a load
// balancer does when it sends the browser's two requests to different tasks. The sign-in in
// flight rides the browser's state cookie, signed under the pair's key, so B completes it: the
// callback answers 302 to the page the sign-in started from, with a session cookie, and both
// processes name the person on that cookie. Before INFRA-555 the state was held by the process
// that started the sign-in, and B answered 400 "invalid or expired state".
func TestASignInStartedOnOneProcessCompletesOnTheOther(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret, multiprocessServerURL+"/auth/callback")
	env := cookieSignIn(issuer)
	env["DISPATCH_SERVER_URL"] = multiprocessServerURL
	pair := startPair(t, env)
	waitForSignInIssuer(t, pair.a, pair.b)
	issuer.SignInAs(map[string]any{
		"sub":              "alice-subject",
		"cognito:username": "ExampleIdP_" + cookiePerson,
		"cognito:groups":   []string{cookieGroup},
		"identities":       []map[string]any{{"providerName": "ExampleIdP"}},
	})

	const next = "/issues/XPS-1?tab=spec"
	started := browserGet(t, pair.a.url("/auth/start?next="+url.QueryEscape(next)), nil)
	_ = started.Body.Close()
	if started.StatusCode != http.StatusFound {
		t.Fatalf("/auth/start on A answered %d, want a redirect to the issuer:\n%s", started.StatusCode, pair.a.Output.String())
	}
	var state *http.Cookie
	for _, cookie := range started.Cookies() {
		if cookie.Name == "dispatch_oauth_state" && cookie.Value != "" {
			state = cookie
		}
	}
	if state == nil {
		t.Fatalf("/auth/start on A set no sign-in state cookie: %v", started.Cookies())
	}
	authorize, err := url.Parse(started.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse the authorization URL A answered: %v", err)
	}
	if got, want := authorize.Query().Get("redirect_uri"), multiprocessServerURL+"/auth/callback"; got != want {
		t.Fatalf("A sent the issuer redirect_uri %q, want %q, the pair's public origin", got, want)
	}
	code, returned := issuer.Authorize(t, authorize.String())

	callback := browserGet(t, pair.b.url("/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(returned)), state)
	body, _ := io.ReadAll(callback.Body)
	_ = callback.Body.Close()
	if callback.StatusCode != http.StatusFound {
		t.Fatalf("/auth/callback on B answered %d, want 302: %s\nB:\n%s", callback.StatusCode, body, pair.b.Output.String())
	}
	if location := callback.Header.Get("Location"); location != next {
		t.Errorf("/auth/callback on B redirected to %q, want the page the sign-in started from, %q", location, next)
	}
	var session *http.Cookie
	for _, cookie := range callback.Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" {
			session = cookie
		}
	}
	if session == nil {
		t.Fatalf("/auth/callback on B set no session cookie: %v", callback.Cookies())
	}
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		if answer, status := whoami(t, process, session); status != http.StatusOK || !strings.Contains(answer, cookiePerson) {
			t.Errorf("%s answered whoami %d %s, want 200 naming %s", pair.name(process), status, answer, cookiePerson)
		}
	}
}

// ───── one signing key across the processes ────────────────────────────────

// A person signs in on one process and their next request lands on the other, which is what a
// load balancer in front of two tasks does on every page load. The session itself is in Postgres,
// which both read; what each process needs of its own is DISPATCH_SIGNING_KEY. A process carrying
// a key no live peer shares refuses to start (TestAProcessWhoseKeySetALivePeerDoesNotShareRefusesToStart),
// so no third process with a key of its own serves beside these two.
func TestASessionMintedOnOneProcessAuthenticatesOnTheOtherUnderOneSigningKey(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret, loopbackCallback)
	pair := newPair(t)
	pair.start(t, cookieSignIn(issuer))
	waitForSignInIssuer(t, pair.a, pair.b)

	session := signInThrough(t, pair.a, issuer)
	for _, target := range []struct {
		name    string
		process *dispatchProcess
		status  int
	}{
		{"the process it was minted on", pair.a, http.StatusOK},
		{"the other process of the pair", pair.b, http.StatusOK},
	} {
		answer, status := whoami(t, target.process, session)
		if status != target.status {
			t.Errorf("%s answered whoami %d, want %d: %s", target.name, status, target.status, answer)
			continue
		}
		if status == http.StatusOK && !strings.Contains(answer, cookiePerson) {
			t.Errorf("%s answered whoami %s, want the person the session names (%s)", target.name, answer, cookiePerson)
		}
	}
}

// The registry of live server processes refuses a process whose signing key set a live peer does
// not share both ways (processes.Register): with a pair live under one key, a process carrying a
// key of its own - a task rolled with a changed secret - and one with no DISPATCH_SIGNING_KEY,
// which signs with a key it minted in its own data dir, are each refused, logging the refusal and
// never answering /healthz, since each would sign people out of the pair or be signed out by it.
// A process given the pair's key first and another second, the first step of a rotation, starts
// beside them.
//
// A refused process judges the pair live by its own live window, production's here, against the
// pair's rows. The pair's liveness is what the row stages: the pair beats at stagedPastTheRun
// (DISPATCH_TEST_PROCESS_HEARTBEAT), so it stamps its rows once, as it registers, and the row then
// dates both rows a stagedPastTheRun ahead on the database's clock (stagePairLive), which no
// refused process's window outlasts however a loaded machine paces its boot. Each refused process
// is stopped once it has logged its refusal (requireRefusedToStart).
func TestAProcessWhoseKeySetALivePeerDoesNotShareRefusesToStart(t *testing.T) {
	pair := startPair(t, map[string]string{"DISPATCH_TEST_PROCESS_HEARTBEAT": stagedPastTheRun.String()})
	stagePairLive(t, pair)
	for _, refused := range []struct{ name, key string }{
		{"a process carrying a key of its own", "a-key-of-its-own"},
		{"a process with no key, which minted one in its data dir", ""},
	} {
		process := startDispatchProcessWith(t, pair.databaseURL(t),
			dispatchOptions{NATSURL: pair.nats, SigningKey: refused.key})
		requireRefusedToStart(t, process, refused.name)
	}

	rotating := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: multiprocessSigningKey + " a-key-of-its-own",
	})
	rotating.waitHealthy(t)
	var registered int
	if err := pair.database.Pool.QueryRow(context.Background(), `select count(*) from server_processes`).Scan(&registered); err != nil {
		t.Fatalf("count the registered server processes: %v", err)
	}
	if registered != 3 {
		t.Errorf("%d server processes registered, want the pair and the process given the pair's key first", registered)
	}
}

// stagePairLive dates the registry rows of the pair, every row server_processes holds, a
// stagedPastTheRun ahead on the database's clock: staged state, not a wait, so any process judges
// the pair live for longer than the run (processes.Live reads a row live while its heartbeat_at
// is within the reader's live window of now()). The pair must beat at stagedPastTheRun, or its
// next heartbeat writes now() over the staged moment.
func stagePairLive(t *testing.T, pair *dispatchPair) {
	t.Helper()
	staged, err := pair.database.Pool.Exec(context.Background(),
		`update server_processes set heartbeat_at = now() + make_interval(secs => $1)`, stagedPastTheRun.Seconds())
	if err != nil {
		t.Fatalf("date the pair's registry rows ahead: %v", err)
	}
	if staged.RowsAffected() != 2 {
		t.Fatalf("the registry holds %d rows, want the pair's two", staged.RowsAffected())
	}
}

// A process whose heartbeat stops for longer than the registry's live window drops out of Live
// while it still runs: a pod frozen by its node, or one cut from the database for that long. A
// process started meanwhile with a key set the first does not share is not refused, since no live
// row disagrees with it. Once the first process resumes, the two must not both serve: each refuses
// the cookies the other mints, which signs people out of whichever one their next request reaches,
// the very thing the boot refusal is there to prevent. Whichever of the two stops - exits, or stops
// answering /healthz 200 so the Service takes it out - the pair must not stay serving side by side.
func TestAProcessThatResumesBesideAPeerWhoseKeySetItDoesNotShareDoesNotServeBesideIt(t *testing.T) {
	pair := newPair(t)
	frozen := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{NATSURL: pair.nats, SigningKey: multiprocessSigningKey, OwnProcessGroup: true})
	frozen.waitHealthy(t)
	if err := frozen.Cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("freeze the first process: %v", err)
	}
	resumed := false
	t.Cleanup(func() {
		if !resumed {
			_ = frozen.Cmd.Process.Signal(syscall.SIGCONT)
		}
	})
	// The first process drops out of Live once its last heartbeat is older than the live window on
	// the database's clock, which is the clock the second process's registration reads it by.
	testwait.Until(t, "the frozen process's registry row to leave the live window", func() ([]processes.Process, bool) {
		live, err := processes.Live(context.Background(), pair.database.Pool)
		if err != nil {
			t.Fatalf("read the live server processes: %v", err)
		}
		return live, len(live) == 0
	})

	other := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{NATSURL: pair.nats, SigningKey: "a-key-of-its-own"})
	other.waitHealthy(t)
	if err := frozen.Cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("resume the first process: %v", err)
	}
	resumed = true

	// Each read of /healthz waits under the shared wait's context, so a process that still serves
	// and answers late on a loaded machine reads as serving: only an exit, a connection it refuses
	// or drops, or an answer other than 200 is a process that stopped. A read the context itself
	// ended reads as serving, so the wait gives up rather than passing at its deadline.
	reading := testwait.Context(t)
	serving := func(p *dispatchProcess) bool {
		select {
		case <-p.Exited:
			return false
		default:
		}
		request, err := http.NewRequestWithContext(reading, http.MethodGet, p.url("/healthz"), nil)
		if err != nil {
			t.Fatalf("build the /healthz read: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return reading.Err() != nil
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	}
	both := outputs{{"the resumed process", frozen}, {"its peer", other}}
	testwait.Until(t, "one of the two processes to stop serving, since neither may serve beside a peer whose key set it does not share",
		func() (fmt.Stringer, bool) { return both, !serving(frozen) || !serving(other) })
}

// requireRefusedToStart waits for the process to log the registry's refusal - the line its wait
// for admission writes when a live peer's key set does not share its own
// (processes.ErrKeySetMismatch) - asking its /healthz all the while, then stops it with SIGTERM and
// waits for its exit, and requires that it never answered /healthz nor logged that it serves. A
// refused process would go on to wait its own live window and one heartbeat for its refusers to
// lapse, and exit 1 after it (processes.Register): that wait is the product's, for a process
// restarted beside its own dead row, and the refusal the row is about is in its line already, so
// the row stops the process rather than wait the product's wait out. The waits are the shared
// wait's, and each failure prints the process's output.
func requireRefusedToStart(t *testing.T, p *dispatchProcess, name string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	answered := 0
	refusal := []string{`msg="dispatch: a live server process refuses this one's signing keys`,
		"signing key set does not match a live server process", "DISPATCH_SIGNING_KEY"}
	testwait.Until(t, name+" to log the registry's refusal", func() (fmt.Stringer, bool) {
		if p.logged(refusal...) {
			return p.Output, true
		}
		select {
		case <-p.Exited:
			// Exited closes only once the output is whole, so this read is final.
			if p.logged(refusal...) {
				return p.Output, true
			}
			t.Fatalf("%s exited %s without the registry's refusal:\n%s", name, p.Cmd.ProcessState, p.Output.String())
		default:
		}
		if response, err := client.Get(p.url("/healthz")); err == nil {
			_ = response.Body.Close()
			answered++
		}
		return p.Output, false
	})
	// The process can end its own wait before the signal reaches it, and exits 1 with the refusal
	// then: the signal finds it gone (os.ErrProcessDone), and its exit is the product's.
	stopped := p.Cmd.Process.Signal(syscall.SIGTERM)
	if stopped != nil && !errors.Is(stopped, os.ErrProcessDone) {
		t.Fatalf("SIGTERM %s: %v", name, stopped)
	}
	p.WaitExit(t, "SIGTERM, once it logged the registry's refusal")
	output := p.Output.String()
	if code := p.Cmd.ProcessState.ExitCode(); stopped != nil && code != 1 {
		t.Errorf("%s ended its wait for admission on its own and exited %d, want 1:\n%s", name, code, output)
	}
	if answered > 0 {
		t.Errorf("%s answered /healthz %d times before it was stopped", name, answered)
	}
	if p.logged(`msg="dispatch: listening"`) {
		t.Errorf("%s logged that it serves:\n%s", name, output)
	}
}

// The two keys TestAKeyRotationKeepsPeopleSignedInAcrossTwoProcesses rotates between: K0, the key
// the pair ran under, and K1, the key it moves to.
const (
	rotationK0 = "multiprocess-rotation-k0"
	rotationK1 = "multiprocess-rotation-k1"
)

// A rotation halfway rolled: A carries the new key first ("K1 K0") while B still signs with the
// old one ("K0 K1"), each holding both, which the registry admits. Nobody is signed out on either
// process. A person signed in on B holds a cookie signed and a refresh token sealed under K0, and
// A answers that cookie. Once the confirmation lapses, A's request refreshes it, opening the K0
// seal and sealing the token again under K1; once it lapses again, B's request refreshes it,
// which opens that K1 seal. A cookie minted on A, under K1, is answered on B. Before INFRA-555 a
// DISPATCH_SIGNING_KEY of two keys was one key, so each process refused the other's cookie.
func TestAKeyRotationKeepsPeopleSignedInAcrossTwoProcesses(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret, loopbackCallback)
	pair := newPair(t)
	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: rotationK1 + " " + rotationK0, Env: cookieSignIn(issuer),
	})
	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
		NATSURL: pair.nats, SigningKey: rotationK0 + " " + rotationK1, Env: cookieSignIn(issuer),
	})
	pair.a.waitHealthy(t)
	pair.b.waitHealthy(t)
	waitForSignInIssuer(t, pair.a, pair.b)

	signedInOnB := signInThrough(t, pair.b, issuer)
	requireWhoami(t, pair, pair.a, signedInOnB, "the cookie B minted under K0")
	requireSealedUnder(t, pair.database, rotationK0, "the refresh token B's sign-in stored")

	lapseConfirmation(t, pair.database)
	refreshes := issuer.Refreshes()
	requireWhoami(t, pair, pair.a, signedInOnB, "the cookie B minted, once the confirmation lapsed")
	if got := issuer.Refreshes(); got != refreshes+1 {
		t.Fatalf("A answered a lapsed confirmation with %d refreshes at the issuer, want 1", got-refreshes)
	}
	requireSealedUnder(t, pair.database, rotationK1, "the refresh token after A's refresh")

	lapseConfirmation(t, pair.database)
	refreshes = issuer.Refreshes()
	requireWhoami(t, pair, pair.b, signedInOnB, "its own cookie, once the confirmation A renewed lapsed")
	if got := issuer.Refreshes(); got != refreshes+1 {
		t.Fatalf("B answered a lapsed confirmation with %d refreshes at the issuer, want 1: it did not refresh with the token A sealed", got-refreshes)
	}
	requireSealedUnder(t, pair.database, rotationK0, "the refresh token after B's refresh")

	signedInOnA := signInThrough(t, pair.a, issuer)
	requireWhoami(t, pair, pair.b, signedInOnA, "the cookie A minted under K1")
}

// requireWhoami requires that p answers /auth/whoami 200 naming cookiePerson on session.
func requireWhoami(t *testing.T, pair *dispatchPair, p *dispatchProcess, session *http.Cookie, what string) {
	t.Helper()
	if answer, status := whoami(t, p, session); status != http.StatusOK || !strings.Contains(answer, cookiePerson) {
		t.Fatalf("%s answered whoami on %s with %d %s, want 200 naming %s:\n%s",
			pair.name(p), what, status, answer, cookiePerson, p.Output.String())
	}
}

// lapseConfirmation dates cookiePerson's last confirmation two hours back, past the hour a
// confirmation stands, so the next request either process answers for them refreshes it.
func lapseConfirmation(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update people set confirmed_at = now() - interval '2 hours' where email = $1`, cookiePerson); err != nil {
		t.Fatalf("date the confirmation two hours back: %v", err)
	}
}

// requireSealedUnder requires that the refresh token people holds for cookiePerson opens under
// signingKey alone. It opens the value as the v1 seal format is pinned to stay (an AES-256-GCM key
// derived from the signing key with HKDF-SHA256, a random nonce leading the ciphertext, the email
// as additional data), so it tells which key of the two sealed it, which a process holding both
// cannot show.
func requireSealedUnder(t *testing.T, database *store.Store, signingKey, what string) {
	t.Helper()
	var sealed string
	if err := database.Pool.QueryRow(context.Background(),
		`select refresh_token from people where email = $1`, cookiePerson).Scan(&sealed); err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	encoded, ok := strings.CutPrefix(sealed, "v1:")
	if !ok {
		t.Fatalf("%s is not in the v1 sealed format", what)
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("%s does not decode: %v", what, err)
	}
	key, err := hkdf.Key(sha256.New, []byte(signingKey), nil, "dispatch people.refresh_token v1", 32)
	if err != nil {
		t.Fatalf("derive the refresh-token key: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("refresh-token cipher: %v", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		t.Fatalf("refresh-token AEAD: %v", err)
	}
	if _, err := aead.Open(nil, nil, ciphertext, []byte(cookiePerson)); err != nil {
		t.Fatalf("%s does not open under %s alone", what, signingKey)
	}
}

// ───── the component's three-apply key rotation ─────────────────────────────

// The two keys the staged-rotation rows walk: generation 0's, the key the deployment already
// runs under, and generation 1's, the key a rotation adds. They are a pair of their own so that
// TestAKeyRotationKeepsPeopleSignedInAcrossTwoProcesses keeps rotationK0 and rotationK1 to
// itself, and they stand for the keys envoy-infra:gcp:Dispatch generates, one per generation of
// its signingKeyGenerations input.
const (
	stagedK0 = "multiprocess-staged-rotation-k0"
	stagedK1 = "multiprocess-staged-rotation-k1"
)

// The four DISPATCH_SIGNING_KEY settings the component renders along the rotation it is built
// for, [0] → [0, 1] → [1, 0] → [1]: one generation renders its key alone, several render the
// generations' keys in the list's order with exactly one space between them. Each is written as
// that join rather than as one literal, so what a reader checks against the component is the
// setting's shape and the generations' order rather than this file's spelling of two keys.
const (
	stagedGeneration0   = stagedK0
	stagedGenerations01 = stagedK0 + " " + stagedK1
	stagedGenerations10 = stagedK1 + " " + stagedK0
	stagedGeneration1   = stagedK1
)

// The whole rotation signingKeyGenerations is built for, three applies against two real
// processes: [0], the deployment before it starts, then [0, 1], which adds generation 1's key
// second, then [1, 0], which promotes it, then [1], which drops generation 0's. Each apply is a
// rolling update, one process replaced at a time with a live peer beside it, which is the pairing
// processes.keySetsShared judges at every boot: no step of the rotation is refused, and nobody
// holding a cookie is signed out at any of them. Every cookie is read on both processes and never
// only on the one that minted it, because a fleet halfway through a roll answers whichever
// request the load balancer sends to either task.
//
// The last apply is the one step that does sign someone out, and by design: a cookie signed under
// generation 0's key is refused once that key is gone, which is what dropping a generation means.
// Only the cookies signed under generation 1 survive it, which is why the component's README
// makes "no sooner than 30 days after the promote" - a session cookie's whole life - the
// precondition of that apply: run it then, and every cookie signed under generation 0 has already
// expired on its own, so the drop refuses nobody who is still signed in.
func TestAStagedKeyRotationAcrossThreeAppliesKeepsPeopleSignedIn(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret, loopbackCallback)
	pair := newPair(t)
	// Every process of the walk runs the same unstamped build, and only its DISPATCH_SIGNING_KEY
	// differs, so the binary is built once rather than once per start.
	binary := cmdtest.Build(t, "envoy-dispatch")
	start := func(setting string) *dispatchProcess {
		t.Helper()
		process := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
			NATSURL: pair.nats, SigningKey: setting, Env: cookieSignIn(issuer), Binary: binary,
		})
		process.waitHealthy(t)
		waitForSignInIssuer(t, process)
		return process
	}
	// apply is what one `pulumi up` of a changed signingKeyGenerations does to the app
	// Deployment: a rolling update that replaces one pod at a time, so the process coming up
	// always has a live peer still carrying the setting of the apply before it. That pairing,
	// not the setting on its own, is what the registry admits or refuses at a boot.
	apply := func(setting string) {
		t.Helper()
		pair.a.Terminate(t)
		pair.a.WaitExit(t, "SIGTERM to A")
		pair.a = start(setting)
		pair.b.Terminate(t)
		pair.b.WaitExit(t, "SIGTERM to B")
		pair.b = start(setting)
	}
	// readOnBoth requires both processes to name the person on the cookie, which is what "nobody
	// is signed out" means for a fleet: the browser's next request reaches either task.
	readOnBoth := func(session *http.Cookie, what string) {
		t.Helper()
		requireWhoami(t, pair, pair.a, session, what)
		requireWhoami(t, pair, pair.b, session, what)
	}

	// [0]: the deployment as it runs before the rotation, both processes on generation 0's key
	// alone. The person signed in here is the one every later apply must not sign out.
	pair.a, pair.b = start(stagedGeneration0), start(stagedGeneration0)
	underK0 := signInThrough(t, pair.b, issuer)
	readOnBoth(underK0, "the cookie B minted under generation 0")
	requireSealedUnder(t, pair.database, stagedK0, "the refresh token B's sign-in stored")

	// Apply 1, [0] → [0, 1]: generation 1's key is added second, so generation 0's still signs
	// every new cookie and seals every new refresh token while the roll brings both processes to
	// hold both keys. A sign-in during this apply is as good as the one before it, and the person
	// signed in at [0] carries on, which is the whole point of adding a key before promoting it.
	apply(stagedGenerations01)
	readOnBoth(underK0, "the cookie minted at [0], once both processes held both keys")
	alsoUnderK0 := signInThrough(t, pair.a, issuer)
	readOnBoth(alsoUnderK0, "the cookie A minted at [0, 1], which generation 0 still signs")
	requireSealedUnder(t, pair.database, stagedK0, "the refresh token after the add")

	// Apply 2, [0, 1] → [1, 0]: generation 1's key moves first, so it signs every cookie minted
	// from here on while generation 0's still verifies every cookie minted before it.
	apply(stagedGenerations10)
	readOnBoth(underK0, "the cookie minted at [0], after the promote")
	readOnBoth(alsoUnderK0, "the cookie minted at [0, 1], after the promote")

	// The seal follows the promote at the person's next hourly confirmation rather than at the
	// roll: the request that finds the confirmation lapsed opens the token under generation 0's
	// key and seals it again under generation 1's, so the drop two applies later leaves it
	// readable. Nothing in the roll itself rewrites a seal, which is why this is worth showing.
	lapseConfirmation(t, pair.database)
	refreshes := issuer.Refreshes()
	requireWhoami(t, pair, pair.a, underK0, "the cookie minted at [0], once the confirmation lapsed")
	if got := issuer.Refreshes(); got != refreshes+1 {
		t.Fatalf("A answered a lapsed confirmation with %d refreshes at the issuer, want 1", got-refreshes)
	}
	requireSealedUnder(t, pair.database, stagedK1, "the refresh token after the refresh that followed the promote")

	underK1 := signInThrough(t, pair.b, issuer)
	readOnBoth(underK1, "the cookie B minted at [1, 0], which generation 1 signs")

	// Apply 3, [1, 0] → [1]: generation 0's key is dropped. Everything signed under generation 1
	// - the cookies minted since the promote, and the seal the confirmation above moved - carries
	// on across the roll, and the fleet signs people in as it did before the rotation began.
	apply(stagedGeneration1)
	readOnBoth(underK1, "the cookie minted at [1, 0], after the drop")
	afterTheDrop := signInThrough(t, pair.a, issuer)
	readOnBoth(afterTheDrop, "the cookie A minted at [1], the rotation's last setting")
	requireSealedUnder(t, pair.database, stagedK1, "the refresh token after the drop")

	// The cookies signed under generation 0's key are what the drop signs out: no process holds
	// the key that verifies them any more, so each answers 401 rather than the person's name.
	// This is the loss the README's 30 days prevent - waited out, no such cookie is left to
	// refuse - and never something the component's inputs can guarantee, so the row asserts the
	// refusal rather than pretending the drop is free.
	for _, signedOut := range []struct {
		what    string
		session *http.Cookie
	}{
		{"the cookie minted at [0]", underK0},
		{"the cookie minted at [0, 1]", alsoUnderK0},
	} {
		for _, process := range []*dispatchProcess{pair.a, pair.b} {
			answer, status := whoami(t, process, signedOut.session)
			if status != http.StatusUnauthorized {
				t.Errorf("%s answered whoami on %s with %d %s, want 401: generation 0's key, which signed it, is gone\n%s",
					pair.name(process), signedOut.what, status, answer, process.Output.String())
			}
		}
	}
}

// ───── the fleet's build ────────────────────────────────────────────────────

// The two builds TestTwoProcessesAnnounceTheirBuildOnce rolls between, stamped into its binaries
// as the image build stamps a commit.
const (
	fleetBuildC1 = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	fleetBuildC2 = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
)

// waitForAnnouncerPassesAfter waits until every one of processes has made a whole pass of its
// build announcer that began after this call: each logs a numbered line for every pass
// (startup.AnnouncerPassed, at DEBUG, which the processes of TestTwoProcessesAnnounceTheirBuildOnce
// write), so once this returns, a pass that would announce after the step before it has had its
// chance to, on every live process, and the stream read after it holds what it announced.
func waitForAnnouncerPassesAfter(t *testing.T, processes ...*dispatchProcess) {
	t.Helper()
	line := `msg="` + startup.AnnouncerPassed + `"`
	passed := make([]uint64, len(processes))
	for i, process := range processes {
		passed[i] = passesLogged(t, process, line)
	}
	for i, process := range processes {
		waitForPassAfter(t, process, fmt.Sprintf("the build announcer of %s", process.url("")), passed[i], line)
	}
}

// server.started goes out once each time every live process comes to serve one build, never per
// process start: two processes stamped C1 put one envelope on the stream; B restarted on C1 puts
// none; B replaced by a C2 binary while A still runs C1 is a roll halfway through and puts none;
// A replaced by C2 completes it and puts one for C2. Each process's /healthz fleet_commit follows
// the announcements, whichever build that process itself runs. Each announcement row the two
// builds' own main wrote lists refs.TupleKinds, the relation kinds graph_edges' tuple arm renders.
//
// Which build the fleet runs is read off the live processes' rows, each judged by the reading
// process's live window. Every process here beats at stagedPastTheRun
// (DISPATCH_TEST_PROCESS_HEARTBEAT), so its window is three of that and no row a live process
// wrote lapses inside the run, however a loaded machine paces the heartbeats: a process leaves
// the fleet only by its SIGTERM, which deregisters it at once.
func TestTwoProcessesAnnounceTheirBuildOnce(t *testing.T) {
	pair := newPair(t)
	c1 := cmdtest.BuildStamped(t, "envoy-dispatch-c1", fleetBuildC1)
	c2 := cmdtest.BuildStamped(t, "envoy-dispatch-c2", fleetBuildC2)
	start := func(binary string) *dispatchProcess {
		t.Helper()
		process := startDispatchProcessWith(t, pair.databaseURL(t), dispatchOptions{
			NATSURL: pair.nats, SigningKey: multiprocessSigningKey, Binary: binary,
			Env: map[string]string{
				"DISPATCH_TEST_DEBUG_MESSAGES":    startup.AnnouncerPassed,
				"DISPATCH_TEST_PROCESS_HEARTBEAT": stagedPastTheRun.String(),
			},
		})
		process.waitHealthy(t)
		return process
	}
	replace := func(process *dispatchProcess, name, binary string) *dispatchProcess {
		t.Helper()
		process.Terminate(t)
		process.WaitExit(t, "SIGTERM to "+name)
		return start(binary)
	}
	conn := testnats.Connect(t, pair.nats)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	head := streamHead(t, js)
	announced := func() []contracts.Envelope {
		var starts []contracts.Envelope
		for _, item := range readStreamFrom(t, js, head) {
			if item.Topic == startup.Subject {
				starts = append(starts, item)
			}
		}
		return starts
	}

	a, b := start(c1), start(c1)
	waitForAnnouncements(t, announced, fleetBuildC1)
	waitForFleetCommit(t, fleetBuildC1, a, b)

	b = replace(b, "B", c1)
	waitForAnnouncerPassesAfter(t, a, b)
	requireAnnouncements(t, announced(), fleetBuildC1)

	b = replace(b, "B", c2)
	waitForAnnouncerPassesAfter(t, a, b)
	requireAnnouncements(t, announced(), fleetBuildC1)
	waitForFleetCommit(t, fleetBuildC1, a, b)

	a = replace(a, "A", c2)
	waitForAnnouncements(t, announced, fleetBuildC1, fleetBuildC2)
	requireAnnouncedTupleKinds(t, pair.database, fleetBuildC1, fleetBuildC2)
	waitForFleetCommit(t, fleetBuildC2, a, b)
	waitForAnnouncerPassesAfter(t, a, b)
	requireAnnouncements(t, announced(), fleetBuildC1, fleetBuildC2)
}

// requireAnnouncedTupleKinds reads server_build_announcements in seq order and requires one row
// per commit of commits, in that order, each listing refs.TupleKinds as its tuple_kinds. The rows
// are the ones the server's own main wrote through the announcer it wires, so a main that passes
// the announcer another list, or none, fails here where a test that builds its own announcer
// cannot.
func requireAnnouncedTupleKinds(t *testing.T, database *store.Store, commits ...string) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select commit, tuple_kinds from server_build_announcements order by seq`)
	if err != nil {
		t.Fatalf("read the announcements: %v", err)
	}
	type announcement struct {
		commit string
		kinds  []string
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (announcement, error) {
		var a announcement
		err := row.Scan(&a.commit, &a.kinds)
		return a, err
	})
	if err != nil {
		t.Fatalf("scan the announcements: %v", err)
	}
	// The rows are held to the commits the stream announced, so a table that holds no row
	// cannot pass the check of the kinds by having nothing to check.
	if len(read) != len(commits) {
		t.Fatalf("server_build_announcements holds %d rows, want %d naming %v: %+v", len(read), len(commits), commits, read)
	}
	want := refs.TupleKinds()
	for i, row := range read {
		if row.commit != commits[i] {
			t.Errorf("announcement %d names the commit %s, want %s", i, row.commit, commits[i])
		}
		if !slices.Equal(row.kinds, want) {
			t.Errorf("announcement %d, of %s, lists the tuple kinds %v; want %v, refs.TupleKinds, the list main wires into its announcer",
				i, row.commit, row.kinds, want)
		}
	}
}

// waitForAnnouncements waits for the stream to hold as many server.started envelopes as commits
// names, then requires them to be those (requireAnnouncements).
func waitForAnnouncements(t *testing.T, announced func() []contracts.Envelope, commits ...string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the stream to hold %d server.started envelopes", len(commits)), func() (int, bool) {
		got := len(announced())
		return got, got >= len(commits)
	})
	requireAnnouncements(t, announced(), commits...)
}

// requireAnnouncements requires got to be one server.started envelope per commit of commits, in
// that order, each from Dispatch, naming its commit in its payload and in its event id.
func requireAnnouncements(t *testing.T, got []contracts.Envelope, commits ...string) {
	t.Helper()
	if len(got) != len(commits) {
		t.Fatalf("the stream holds %d server.started envelopes, want %d naming %v: %+v", len(got), len(commits), commits, got)
	}
	for i, item := range got {
		var payload startup.Payload
		if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
			t.Fatalf("decode server.started payload %q: %v", item.Payload, err)
		}
		if item.Source != startup.Source || payload.Commit != commits[i] ||
			!strings.HasPrefix(item.EventID, "dispatch-server-started-"+commits[i]+"-") {
			t.Fatalf("server.started envelope %d = %+v, want one from dispatch announcing %s", i, item, commits[i])
		}
	}
}

// waitForFleetCommit waits for every one of processes to report want as its /healthz
// fleet_commit: each refreshes it on its announcer's pass.
func waitForFleetCommit(t *testing.T, want string, processes ...*dispatchProcess) {
	t.Helper()
	for _, process := range processes {
		testwait.Until(t, fmt.Sprintf("%s to report fleet_commit %s", process.url("/healthz"), want), func() (string, bool) {
			response, err := http.Get(process.url("/healthz"))
			if err != nil {
				t.Fatalf("GET /healthz: %v", err)
			}
			var health struct {
				FleetCommit *string `json:"fleet_commit"`
			}
			err = json.NewDecoder(response.Body).Decode(&health)
			_ = response.Body.Close()
			if err != nil {
				t.Fatalf("decode /healthz: %v", err)
			}
			if health.FleetCommit == nil {
				return "no fleet_commit yet", false
			}
			return *health.FleetCommit, *health.FleetCommit == want
		})
	}
}

// ───── one document, readers on two processes ─────────────────────────────
//
// The rows below are pull request #2414's: two people, or a person and an agent, on one
// document whose readers are on different processes. Each drives real editors (docstest.Peer
// speaks the dashboard's Hocuspocus framing) and the processes' own API, hooks and logs, and
// arranges each race it is about with a test hook (DISPATCH_TEST_HOOKS) - a held persistence, a
// parked live write - or a lock of its own, never with a sleep that hopes.

// liveWithin is how soon a change on one process must reach a reader on the other: the spec's
// "at once", which the plan pins at one second.
const liveWithin = time.Second

// TestALiveChangeReachesTheOtherProcessWithinASecond is the service level liveWithin names: the
// spec's "at once" for a change on one process reaching a reader on the other, which the plan
// pins at one second. A text edit typed on A reaches a reader on B, carried there by the catch-up
// the edit's notification asked B for (awaitNotifiedCatchUp): that, and not a poll of B's that
// would carry it too, only later, is what every run holds. The one second is a reading of this
// machine's clock across two child processes, so only the row's twin in benchtime's timing lane
// holds it (benchtime.OnTheClock), as the rows below hold each change they wait for there
// (withinLive).
func TestALiveChangeReachesTheOtherProcessWithinASecond(t *testing.T) {
	aLiveChangeReachesTheOtherProcessWithinASecond(t, benchtime.EveryRun)
}

// TestALiveChangeReachesTheOtherProcessWithinASecondOnTheClock is the row above in benchtime's
// timing lane, where the edit is held to reaching the reader within liveWithin of the keystroke.
func TestALiveChangeReachesTheOtherProcessWithinASecondOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), aLiveChangeReachesTheOtherProcessWithinASecond)
}

// aLiveChangeReachesTheOtherProcessWithinASecond is the scene of the two rows above; lane says
// whether its reading of the clock is held to liveWithin (benchtime.Lane).
func aLiveChangeReachesTheOtherProcessWithinASecond(t *testing.T, lane benchtime.Lane) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "LIV")
	pair.a.createProject(t, project, "Live within a second")
	_, artifactID := pair.a.createIssueIn(t, project, "Live edit", "before")
	writer := pair.openSyncedEditor(t, pair.a, "alice", artifactID, "before\n")
	reader := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "before\n")
	base := pair.catchUpBase(t, pair.b, artifactID)
	if err := typeAtEnd(writer, " after"); err != nil {
		t.Fatalf("type on A: %v", err)
	}
	typed := time.Now()
	if err := reader.WaitForText(t, markdown, "before after\n"); err != nil {
		t.Fatalf("the edit on A did not reach the reader on B: %v", err)
	}
	reached := time.Since(typed)
	t.Logf("the edit on A reached the reader on B %v after it was typed", reached)
	if lane == benchtime.OnTheClock && reached > liveWithin {
		t.Fatalf("the edit on A reached the reader on B %v after it was typed, want within %v", reached, liveWithin)
	}
	pair.b.awaitNotifiedCatchUp(t, artifactID, base, "the edit on A reaching the reader on B")
}

// multiprocessAgentSession is the session an agent's writes name (actor.kind session) when they
// come through the agent token (dispatchTestAgentToken).
const multiprocessAgentSession = "multiprocess-agent"

// asAgent is the header an agent's request carries: the agent token and no person.
func asAgent() http.Header {
	return http.Header{"Authorization": {"Bearer " + dispatchTestAgentToken}}
}

func editPath(artifactID string) string {
	return "/api/v1/artifacts/" + artifactID + "/edits"
}

// editBody is the body of an edit replacing find with with: an agent's when agent is set (a bearer
// names its session in actor), and conditional on the document's token when token is not "".
func editBody(t *testing.T, find, with, token string, agent bool) string {
	t.Helper()
	body := struct {
		Ops          []model.EditOp          `json:"ops"`
		Precondition *model.EditPrecondition `json:"precondition,omitempty"`
		Actor        *model.Actor            `json:"actor,omitempty"`
	}{Ops: []model.EditOp{{Op: "replace", Find: find, With: with}}}
	if token != "" {
		body.Precondition = &model.EditPrecondition{Document: token}
	}
	if agent {
		body.Actor = &model.Actor{Kind: "session", ID: multiprocessAgentSession}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	return string(encoded)
}

// textAndToken is the document as the process reads it now and the token a conditional edit
// against that reading passes (GET /api/v1/artifacts/{id}/text).
func (p *dispatchProcess) textAndToken(t *testing.T, artifactID string) (string, string) {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", http.StatusOK)
	var document struct {
		Markdown string `json:"markdown"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(answer, &document); err != nil || document.Token == "" {
		t.Fatalf("decode the document's text and token: %v: %s", err, answer)
	}
	return document.Markdown, document.Token
}

// holdsRoom reports whether the process holds a room of the document now (the rooms test hook).
func (p *dispatchProcess) holdsRoom(t *testing.T, artifactID string) bool {
	t.Helper()
	return slices.Contains(p.residentRooms(t), artifactID)
}

// roomCloses names what the room-counters hook counted for the document on either process so far
// - the rooms the service closed and the room failures - for a failure to show beside what it saw.
func (p *dispatchPair) roomCloses(t *testing.T, artifactID string) string {
	t.Helper()
	countersA, countersB := p.a.roomCounters(t, artifactID), p.b.roomCounters(t, artifactID)
	return fmt.Sprintf("the service closed rooms of the document %d times on A and %d on B; rooms of it failed %d times on A and %d on B",
		countersA.RoomCloses, countersB.RoomCloses, countersA.RoomFailures, countersB.RoomFailures)
}

// requireNoRoomCloses fails the test when either process's service closed a room of the document
// (the room-counters hook's room_closes): a process that keeps its rooms live closes none for a
// write, a cursor or a departure, on its own process or the other. The count rises once the close
// has returned, so a close still under way when this reads is missed.
func (p *dispatchPair) requireNoRoomCloses(t *testing.T, artifactID string) {
	t.Helper()
	countersA, countersB := p.a.roomCounters(t, artifactID), p.b.roomCounters(t, artifactID)
	if countersA.RoomCloses+countersB.RoomCloses > 0 {
		t.Errorf("the service closed a room of the document %d times on A and %d on B, want none on either: a process closed a room it should have kept live (rooms of the document failed %d times on A and %d on B)",
			countersA.RoomCloses, countersB.RoomCloses, countersA.RoomFailures, countersB.RoomFailures)
	}
}

// ───── editors that reconnect as a browser does ─────────────────────────────

func peerEnded(editor *docstest.Peer) bool {
	select {
	case <-editor.Ended:
		return true
	default:
		return false
	}
}

// requireConnected fails the test when the editor's connection has ended, naming who and when.
func (p *dispatchPair) requireConnected(t *testing.T, artifactID, who string, editor *docstest.Peer, when string) {
	t.Helper()
	if peerEnded(editor) {
		t.Fatalf("%s was disconnected %s (close code %d): the process closed the room it held rather than keeping it live; %s",
			who, when, editor.CloseCode(), p.roomCloses(t, artifactID))
	}
}

// lenientT hands a dial's refusal back to dialEditor instead of ending the test: a browser's
// provider dials again after a refused reconnect (ygo refuses a dial between a closing room's
// hang-up and its removal), and so do the rows that reconnect. docstest.Dial reports a refusal
// through Fatalf, which here unwinds to dialEditor's recover.
type lenientT struct{ *testing.T }

type dialRefused struct{ err error }

func (t lenientT) Fatalf(format string, args ...any) {
	panic(dialRefused{fmt.Errorf(format, args...)})
}

// dialEditor connects login's tab to the document's socket at url, answering a refusal as an
// error.
func dialEditor(t *testing.T, url, login, artifactID string) (editor *docstest.Peer, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			refused, ok := recovered.(dialRefused)
			if !ok {
				panic(recovered)
			}
			err = refused.err
		}
	}()
	return docstest.Dial(lenientT{t}, url, asUser(login), artifactID, crdt.New()), nil
}

// documentTry is one tab's connection while reconnectEditor waits for the room to send it the
// document: the peer, and whether an ask for the document is still unanswered on it.
type documentTry struct {
	editor *docstest.Peer
	asked  bool
}

// read reads, without waiting, whether the room has sent the tab its document: done once the
// tab's copy renders one, and an error once the connection closed first or an ask failed. It asks
// the room for the document when no ask is unanswered, and again after each answer that rendered
// none, as main's loop did; the room answers in order on the connection (docstest.AskForDocument).
func (d *documentTry) read() (done bool, err error) {
	if markdown(d.editor) != "" {
		return true, nil
	}
	if !d.asked {
		if err := d.editor.AskForDocument(); err != nil {
			return false, fmt.Errorf("ask the room for the document: %w", err)
		}
		d.asked = true
	}
	select {
	case <-d.editor.Answers:
		d.asked = false
	case <-d.editor.Ended:
		return false, fmt.Errorf("the connection closed (code %d) before the room sent the document", d.editor.CloseCode())
	default:
	}
	return markdown(d.editor) != "", nil
}

// reconnectEditor connects login's tab to the document on this process again, as the dashboard's
// provider does once its socket closed: it dials again after a refusal or a connection that
// closed before the room sent the document, with no limit on its tries (@hocuspocus/provider's
// maxAttempts is 0), and returns once the room has sent the tab a document. One shared wait holds
// every try, and each poll reads the open connection once without waiting (documentTry.read). A
// connection the room holds open and never answers is not dialled again after ten seconds on the
// wall clock, as main's loop did: a room that closes ends its sockets, which dials again (the
// close rows require that end, requireEndsWithin), so a room that keeps a socket open and never
// sends its document is a defect, which the row names at the give-up. A tab that never gets back
// fails the test through the shared wait, which prints what the last try read and this process's
// output.
func (p *dispatchProcess) reconnectEditor(t *testing.T, login, artifactID string) *docstest.Peer {
	t.Helper()
	var try *documentTry
	testwait.Until(t, fmt.Sprintf("%s's tab to reconnect to the document and be sent it", login), func() (reconnectRefused, bool) {
		if try == nil {
			dialed, err := dialEditor(t, p.documentURL(artifactID), login, artifactID)
			if err != nil {
				return reconnectRefused{err: err, process: p}, false
			}
			try = &documentTry{editor: dialed}
		}
		done, err := try.read()
		switch {
		case done:
			return reconnectRefused{}, true
		case err != nil:
			try.editor.Close()
			try = nil
			return reconnectRefused{err: err, process: p}, false
		default:
			return reconnectRefused{err: errors.New("the tab is connected and the room has sent it no document yet"), process: p}, false
		}
	})
	return try.editor
}

// reconnectRefused is what reconnectEditor last read: the refusal, and the process that refused,
// whose output a failure prints whole.
type reconnectRefused struct {
	err     error
	process *dispatchProcess
}

func (r reconnectRefused) String() string {
	return fmt.Sprintf("%v; that process's output:\n%s", r.err, r.process.Output.String())
}

// openSyncedEditor is openEditorAs that also checks that the tab's sync left the durable head
// where it was, so the next thing the row does is the only thing that moves the document. A
// browser's provider answers the room's sync step 1 with its own state, which changes nothing in
// the room of a tab that has typed nothing, and the room stores nothing for a transaction that
// changes nothing (ygo calls no update observer for one, reearth/ygo#350). openEditorAs has
// waited for the room to send the tab the document, so the tab is synced when this returns.
func (p *dispatchPair) openSyncedEditor(t *testing.T, process *dispatchProcess, login, artifactID, want string) *docstest.Peer {
	t.Helper()
	before := storedHead(t, p, artifactID)
	editor := process.openEditorAs(t, login, artifactID, want)
	if head := storedHead(t, p, artifactID); head != before {
		t.Fatalf("the document moved from update %d to %d while %s's tab connected on %s: a sync that changes nothing stores nothing, so something else wrote the document",
			before, head, login, p.name(process))
	}
	return editor
}

// waitStoredPast waits for the document's stored head to pass head, and fails the test naming
// what was to be stored when it does not.
func (p *dispatchPair) waitStoredPast(t *testing.T, artifactID string, head int64, what string) {
	t.Helper()
	testwait.Until(t, what+" to be stored", func() (int64, bool) {
		stored := storedHead(t, p, artifactID)
		return stored, stored > head
	})
}

// storedText renders the document the store holds: its updates folded in order, which is what
// every reload of it reads, whatever any room holds.
func storedText(t *testing.T, pair *dispatchPair, artifactID string) string {
	t.Helper()
	rows, err := pair.database.Pool.Query(context.Background(),
		`select update from doc_updates where artifact_id = $1 order by version`, artifactID)
	if err != nil {
		t.Fatalf("read the stored history of %s: %v", artifactID, err)
	}
	defer rows.Close()
	document := crdt.New()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			t.Fatalf("scan a stored update of %s: %v", artifactID, err)
		}
		if err := crdt.ApplyUpdateV1(document, update, nil); err != nil {
			t.Fatalf("apply a stored update of %s: %v", artifactID, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored history of %s: %v", artifactID, err)
	}
	return renderDocument(document)
}

// storedCaughtUpWith waits for the stored document to read as the editor's copy does, so nothing
// the editor's socket has already sent the room is still on its way to the store. It answers an
// error rather than failing the test, for a caller with something of its own to report first:
// the typists' row reports the room counters and each disconnect (keystroke).
func (p *dispatchPair) storedCaughtUpWith(t *testing.T, artifactID string, editor *docstest.Peer, whose string) error {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the store to hold what %s holds", whose), func() (string, bool) {
		held, stored := markdown(editor), storedText(t, p, artifactID)
		return fmt.Sprintf("that copy reads %q, the store reads %q", held, stored), held == stored
	})
	return nil
}

// waitStoredHolds waits for the stored document to hold want, and fails the test naming what was
// to be stored when it does not. It is waitStoredPast for a row that cares which update was
// stored: the head moves for whatever any writer of the document stores, and this one passes on
// the text alone.
func (p *dispatchPair) waitStoredHolds(t *testing.T, artifactID, want, what string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("%s to be stored, which is a stored document holding %q", what, want), func() (string, bool) {
		stored := storedText(t, p, artifactID)
		return stored, strings.Contains(stored, want)
	})
}

// typeAtEnd types text at the end of the editor's first paragraph in one keystroke's transaction,
// on the paragraph's own Yjs text as ProseMirror's binding does, and sends the room the update.
func typeAtEnd(editor *docstest.Peer, text string) error {
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	missing := false
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		paragraph := crafted.FirstText(fragment)
		if paragraph == nil {
			missing = true
			return
		}
		paragraph.Insert(txn, paragraph.Len(), text, nil)
	}); err != nil {
		return err
	}
	if missing {
		return errors.New("the editor's copy holds no paragraph text to type into")
	}
	return nil
}

// rewriteDocument changes the editor's document as one transaction - change edits the tree it
// reads, and pmdoc writes the difference back - and sends the room the update.
func rewriteDocument(editor *docstest.Peer, change func(*pmdoc.Node) error) error {
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	var changeErr error
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			changeErr = err
			return
		}
		if changeErr = change(tree); changeErr != nil {
			return
		}
		changeErr = pmdoc.Update(txn, fragment, tree)
	}); err != nil {
		return err
	}
	return changeErr
}

// deleteParagraph deletes, in the editor, the top-level block whose text holds word, as a person
// selecting the paragraph and pressing delete does.
func deleteParagraph(t *testing.T, editor *docstest.Peer, word string) {
	t.Helper()
	if err := removeParagraph(editor, word); err != nil {
		t.Fatalf("delete the paragraph holding %q: %v", word, err)
	}
}

// removeParagraph is deleteParagraph answering its error, for a row that reads a send on a
// closed socket as the state it is.
func removeParagraph(editor *docstest.Peer, word string) error {
	return rewriteDocument(editor, func(tree *pmdoc.Node) error {
		for index, block := range tree.Children {
			if strings.Contains(pmdoc.TextContent(block), word) {
				tree.Children = slices.Delete(tree.Children, index, index+1)
				return nil
			}
		}
		return fmt.Errorf("no paragraph holds %q", word)
	})
}

// appendMarkdown appends the blocks markdown parses to at the end of the editor's document, as a
// person typing them (an ask block's slash command included) does.
func appendMarkdown(t *testing.T, editor *docstest.Peer, markdown string) {
	t.Helper()
	parsed, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	if err := rewriteDocument(editor, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, parsed.Children...)
		return nil
	}); err != nil {
		t.Fatalf("append %q: %v", markdown, err)
	}
}

// placeMark marks quote in the editor's document with a comment mark carrying id, as the
// dashboard does before it posts the comment that mark anchors.
func placeMark(t *testing.T, editor *docstest.Peer, login, quote, id string) {
	t.Helper()
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	var markErr error
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			markErr = err
			return
		}
		quoted, err := pmdoc.FindQuote(tree, quote, nil, nil)
		if err != nil {
			markErr = err
			return
		}
		markErr = pmdoc.MarkRange(txn, fragment, quoted, pmdoc.Mark{
			Type:  string(docs.MarkComment),
			Attrs: pmdoc.Attrs{"id": id, "by": "user:" + login},
		})
	}); err != nil || markErr != nil {
		t.Fatalf("mark %q in the editor: %v %v", quote, err, markErr)
	}
}

// roomRendering asks the room for its whole document over the editor's socket and renders the
// answer on its own: what the room holds, not what the editor's copy holds. The room answers a
// sync step 1 after every message the editor sent before it, so this is also a barrier.
func roomRendering(t *testing.T, editor *docstest.Peer) (string, error) {
	t.Helper()
	for drained := false; !drained; {
		select {
		case <-editor.Answers:
		default:
			drained = true
		}
	}
	if err := editor.AskForDocument(); err != nil {
		return "", fmt.Errorf("ask the room for its document: %w", err)
	}
	var state []byte
	var closed error
	testwait.Eventually(t, "the room to send its document", func() bool {
		select {
		case state = <-editor.Answers:
			return true
		case <-editor.Ended:
			closed = fmt.Errorf("the connection closed (code %d) before the room sent its document", editor.CloseCode())
			return true
		default:
			return false
		}
	})
	if closed != nil {
		return "", closed
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		return "", fmt.Errorf("apply the room's document: %w", err)
	}
	return renderDocument(doc), nil
}

// typist is one person typing into a document in a row that counts what the session cost them:
// they reconnect whenever their socket closes, as the dashboard's provider does, and each
// disconnect's close code is noted.
type typist struct {
	who         string
	login       string
	process     *dispatchProcess
	editor      *docstest.Peer
	text        string
	disconnects []int
}

// disconnected reports, as an error message's clause, how often the typist was disconnected and
// with which close codes, or "" when never.
func (typing *typist) disconnected() string {
	if len(typing.disconnects) == 0 {
		return ""
	}
	codes := map[int]int{}
	for _, code := range typing.disconnects {
		codes[code]++
	}
	return fmt.Sprintf("%s was disconnected %d times (close code: times %v)", typing.who, len(typing.disconnects), codes)
}

// keystroke types the typist's text once and waits until the store has moved past where it stood
// before the keystroke, or until the typist's socket closed, which is a browser's lot when the
// room it typed into was closed. A closed socket is noted and reconnected before the next
// keystroke; a typist that never gets back fails the test through reconnectEditor's shared wait.
// It answers an error when the store does not come to hold what the reconnected tab holds or when
// the keystroke cannot be sent. A keystroke that is sent but is neither stored nor ended with its
// socket fails the test itself, in the shared wait (testwait.Until), which gives up 30 s before
// the test binary's deadline naming the typist and the last stored head; the calling row then
// logs nothing more of its own, neither its room counters nor the keystroke it stopped at.
func (p *dispatchPair) keystroke(t *testing.T, artifactID string, typing *typist) error {
	t.Helper()
	for attempt := 1; ; attempt++ {
		if peerEnded(typing.editor) {
			typing.disconnects = append(typing.disconnects, typing.editor.CloseCode())
			typing.editor = typing.process.reconnectEditor(t, typing.login, artifactID)
			// The tab hands the room the state of its own copy, which holds the keystroke the
			// room that closed may never have stored. The room appends whatever of it the room
			// lacks, and nothing at all where that sync changes nothing (ygo calls no update
			// observer for a transaction that changes nothing, reearth/ygo#350), so what is
			// waited for here is the store holding what the tab holds: an append of the tab's
			// state left in flight would pass the wait below on its own and the keystroke would
			// be taken for stored.
			if err := p.storedCaughtUpWith(t, artifactID, typing.editor, typing.who+"'s reconnected tab"); err != nil {
				return err
			}
		}
		before := storedHead(t, p, artifactID)
		if err := typeAtEnd(typing.editor, typing.text); err != nil {
			if peerEnded(typing.editor) && attempt < 5 {
				continue
			}
			return fmt.Errorf("%s's keystroke: %w", typing.who, err)
		}
		testwait.Until(t, typing.who+"'s keystroke to be stored or to end with its socket", func() (int64, bool) {
			stored := storedHead(t, p, artifactID)
			return stored, stored > before || peerEnded(typing.editor)
		})
		return nil
	}
}

// latestSeq is the issue's latest event sequence.
func latestSeq(t *testing.T, pair *dispatchPair, issueKey string) int {
	t.Helper()
	var seq int
	if err := pair.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(seq), 0) from events where issue_key = $1`, issueKey).Scan(&seq); err != nil {
		t.Fatalf("read the latest event of %s: %v", issueKey, err)
	}
	return seq
}

// versionActorsSince is the actor of each artifact.version event of the issue after seq, in order:
// one per version a settlement or a write minted.
func versionActorsSince(t *testing.T, pair *dispatchPair, issueKey string, seq int) []model.Actor {
	t.Helper()
	rows, err := pair.database.Pool.Query(context.Background(), `
		select actor::text from events
		where issue_key = $1 and type = 'artifact.version' and seq > $2
		order by seq
	`, issueKey, seq)
	if err != nil {
		t.Fatalf("read the versions of %s: %v", issueKey, err)
	}
	defer rows.Close()
	var actors []model.Actor
	for rows.Next() {
		var encoded string
		var actor model.Actor
		if err := rows.Scan(&encoded); err != nil {
			t.Fatalf("scan a version's actor: %v", err)
		}
		if err := json.Unmarshal([]byte(encoded), &actor); err != nil {
			t.Fatalf("decode a version's actor %s: %v", encoded, err)
		}
		actors = append(actors, actor)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the versions of %s: %v", issueKey, err)
	}
	return actors
}

// theClose is the sequence of the issue.closed event of issueKey's first close past event since,
// as a subquery: the moment the database fixes for that close, which every event of the close's
// own transaction and of every write before it comes ahead of.
const theClose = `(select min(seq) from events where issue_key = $2 and type = 'issue.closed' and seq > $3)`

// versionTheCloseLeft is the number, markdown and authors (JSON) of the version of the document
// that the first close of its issue past event since left: the newest version whose
// artifact.version event comes ahead of that close's issue.closed event. A settlement that runs
// after the close, which the close's own writes owe, mints a later version that a read of the
// latest version would take for the close's; the events' order is the database's, so this read
// is not. A close with no version ahead of it fails the test.
func versionTheCloseLeft(t *testing.T, pair *dispatchPair, issueKey, artifactID string, since int) (int, string, string) {
	t.Helper()
	var number int
	var markdown, authors string
	if err := pair.database.Pool.QueryRow(context.Background(), `
		select v.number, coalesce(v.markdown, ''), v.authors::text
		from artifact_versions v
		where v.artifact_id::text = $1 and v.number = (
			select max((e.payload->'version'->>'number')::int) from events e
			where e.issue_key = $2 and e.type = 'artifact.version' and e.payload->>'artifact_id' = $1
			  and e.seq < `+theClose+`)
	`, artifactID, issueKey, since).Scan(&number, &markdown, &authors); err != nil {
		t.Fatalf("read the version of %s the close of %s past event %d left: %v", artifactID, issueKey, since, err)
	}
	return number, markdown, authors
}

// versionsAfterTheClose is how many versions of the document were minted after the first close of
// its issue past event since: its artifact.version events behind that close's issue.closed event.
func versionsAfterTheClose(t *testing.T, pair *dispatchPair, issueKey, artifactID string, since int) int {
	t.Helper()
	var minted int
	if err := pair.database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where issue_key = $2 and type = 'artifact.version' and payload->>'artifact_id' = $1 and seq > `+theClose,
		artifactID, issueKey, since).Scan(&minted); err != nil {
		t.Fatalf("read the versions of %s minted after the close of %s past event %d: %v", artifactID, issueKey, since, err)
	}
	return minted
}

// typeAndSee types text in typist's editor and requires reader's editor, on the other process
// (readerProcess), to show it once it arrives, carried there by the catch-up the typist's write
// notified (awaitNotifiedCatchUp), and within liveWithin of the keystroke on the clock
// (withinLive).
func (p *dispatchPair) typeAndSee(t *testing.T, lane benchtime.Lane, artifactID, typistName string, typist *docstest.Peer, text, readerName string, reader *docstest.Peer, readerProcess *dispatchProcess, keystroke int) {
	t.Helper()
	p.requireConnected(t, artifactID, typistName, typist, fmt.Sprintf("before keystroke %d", keystroke))
	base := p.catchUpBase(t, readerProcess, artifactID)
	if err := typeAtEnd(typist, text); err != nil {
		t.Fatalf("%s's keystroke %d: %v", typistName, keystroke, err)
	}
	typed := time.Now()
	want := markdown(typist)
	if err := reader.WaitForText(t, markdown, want); err != nil {
		t.Fatalf("%s's keystroke %d did not reach %s live: %v; %s",
			typistName, keystroke, readerName, err, p.roomCloses(t, artifactID))
	}
	what := fmt.Sprintf("%s's keystroke %d reaching %s", typistName, keystroke, readerName)
	withinLive(t, lane, typed, what)
	readerProcess.awaitNotifiedCatchUp(t, artifactID, base, what)
}

// withinLive holds a change that began at since on one process and has now reached a reader on
// the other to liveWithin, on the clock only (benchtime.OnTheClock): across two child processes a
// reading of this machine's clock is its load as much as the product's work, so the row every run
// makes holds the change to arriving at all, through the shared wait, and to arriving on what the
// other process's notification carried: an edit on the catch-up its write's notification asked
// for (awaitNotifiedCatchUp), a cursor or a presence on what the other process's awareness relay
// sent (awaitRelayedAwareness), and a close of an issue's rooms on the issue's own notification
// (awaitNotifiedIssueClose). It logs how long the change took.
func withinLive(t *testing.T, lane benchtime.Lane, since time.Time, what string) {
	t.Helper()
	took := time.Since(since)
	t.Logf("%s took %v", what, took)
	if lane == benchtime.OnTheClock && took > liveWithin {
		t.Errorf("%s took %v, want within %v", what, took, liveWithin)
	}
}

// catchUpBase is what a wait for another process's change to reach a process's room
// (awaitNotifiedCatchUp) is measured from, read before the change is made: the document's stored
// head, past which the change's own row is the first, and how many LISTENs the process's listener
// had stood up (docs.RoomCounters.ListenStands).
type catchUpBase struct {
	head         int64
	listenStands int64
}

// catchUpBase reads, before another process changes the document, what a wait for that change to
// reach reader's room is measured from (awaitNotifiedCatchUp).
func (p *dispatchPair) catchUpBase(t *testing.T, reader *dispatchProcess, artifactID string) catchUpBase {
	t.Helper()
	return catchUpBase{listenStands: reader.roomCounters(t, artifactID).ListenStands, head: storedHead(t, p, artifactID)}
}

// awaitNotifiedCatchUp waits for the process's room of the document to hold a change another
// process stored after base was read, carried there by a catch-up the write's notification asked
// for: the head that catch-up left the room holding is past base.head, where the change's own row
// is the first (docs.RoomCounters.NotifiedHead). It is the half of withinLive every run holds. A
// write notifies each other process twice (docs.roomCatchUps): its head notification, and the
// bare twin after it. Whichever of the two asked, the catch-up records the head the room then
// holds, so a twin of an earlier change still queued when base was read records a head at or
// below base.head unless it read this change's row itself, and a catch-up another read of the
// process made first leaves the notified one finding the room at that head. A change carried by a
// poll of the reader's process instead - a catch-up on a timer of its own, with nothing notified -
// arrives all the same, later, and only the timing lane's reading of the clock would see that it
// was late; the head a notified catch-up left sees what carried it, which is no reading of the
// clock.
//
// The product has one carrier besides the notification: one sent while the process's listener had
// no connection reached nobody there, and the sweep after its next LISTEN catches the room up
// (docs.roomCatchUps.sweep). A listener redials when its ping times out, which a loaded machine
// can cause, so where it stood up a LISTEN again after base was read (ListenStands) the wait also
// ends at a head that sweep left past base.head (SweptHead), and logs that a redial carried the
// change. A process that loses notifications with no redial still fails it, naming what, at the
// shared wait's give-up.
func (p *dispatchProcess) awaitNotifiedCatchUp(t *testing.T, artifactID string, base catchUpBase, what string) {
	t.Helper()
	reached := testwait.Until(t, fmt.Sprintf("%s to be carried by a catch-up the write's notification asked for (notified head past %d), or by the sweep after a redial (swept head past %d with LISTENs stood past %d)",
		what, base.head, base.head, base.listenStands),
		func() (docs.RoomCounters, bool) {
			counters := p.roomCounters(t, artifactID)
			if counters.NotifiedHead > base.head {
				return counters, true
			}
			return counters, counters.ListenStands > base.listenStands && counters.SweptHead > base.head
		})
	if reached.NotifiedHead <= base.head {
		t.Logf("%s was carried by the sweep after the process's listener redialed (LISTENs stood %d, then %d; swept head %d, notified head %d, past %d): the write's notification reached no listener of the process",
			what, base.listenStands, reached.ListenStands, reached.SweptHead, reached.NotifiedHead, base.head)
	}
}

// awarenessBase is what a wait for a change of one awareness client to reach a process's room
// (awaitRelayedAwareness) is measured from, read before the change is made: what the room had
// taken of that client (docs.RoomCounters.RelayedAwareness), and how many LISTENs the process's
// listener had stood up (docs.RoomCounters.ListenStands).
type awarenessBase struct {
	taken        docs.RelayedAwarenessChanges
	listenStands int64
}

// relayedAwareness reads, before a change of client, what a wait for that change to reach the
// process's room of the document is measured from (awaitRelayedAwareness).
func (p *dispatchProcess) relayedAwareness(t *testing.T, artifactID string, client uint64) awarenessBase {
	t.Helper()
	counters := p.roomCounters(t, artifactID)
	return awarenessBase{taken: counters.RelayedAwareness[client], listenStands: counters.ListenStands}
}

// awaitRelayedAwareness waits for the process's room of the document to take a change of client
// from another process's notification past before, read before the change was made
// (relayedAwareness): a removal of the client when removed, a state of it otherwise. It is the half
// of withinLive every run holds for a cursor or a presence crossing processes. Awareness crosses
// by NOTIFY alone (docs/awareness_relay.go), but a room also removes a client it stops hearing
// from by itself once awarenessExpiry (30 s) has passed, so a relay that drops removals still
// clears a departed reader from the other process's readers; the count holds the removal to the
// one the reader's own process relayed. A room counts no entry it dropped as stale, so an earlier
// state still in flight when before was read never counts as this one.
//
// A removal has one other carrier, the room's own removal of the client: a removal notified while
// the process's listener had no connection reached nobody there, and nothing after the next
// LISTEN sends it again, so the room keeps the client until it expires it or a local peer's frame
// removes it. A listener redials when its ping times out, which a loaded machine can cause, so
// where the process stood up a LISTEN again after before was read (ListenStands), a removal wait
// also ends at the room's own removal of the client (docs.RelayedAwarenessChanges.Expired: its
// expiry of the client, or a local peer's frame that removes it), and logs that a redial carried
// the removal. A process that loses removals with no redial still fails it. A state has no such
// carrier: a state lost to a redial never reaches the reader's view either, so the wait for the
// view, before this one, fails first. It fails, naming what, at the shared wait's give-up.
func (p *dispatchProcess) awaitRelayedAwareness(t *testing.T, artifactID string, client uint64, removed bool, before awarenessBase, what string) {
	t.Helper()
	if !removed {
		testwait.Until(t, fmt.Sprintf("%s to be carried by the other process's awareness relay (a relayed state of client %d past %d)", what, client, before.taken.States),
			func() (awarenessBase, bool) {
				now := p.relayedAwareness(t, artifactID, client)
				return now, now.taken.States > before.taken.States
			})
		return
	}
	reached := testwait.Until(t, fmt.Sprintf("%s to be carried by the other process's awareness relay (a relayed removal of client %d past %d), or by the room's own removal of it after a redial, its expiry of the client or a local peer's frame that removes it (own removals counted as Expired past %d with LISTENs stood past %d)",
		what, client, before.taken.Removals, before.taken.Expired, before.listenStands),
		func() (awarenessBase, bool) {
			now := p.relayedAwareness(t, artifactID, client)
			if now.taken.Removals > before.taken.Removals {
				return now, true
			}
			return now, now.listenStands > before.listenStands && now.taken.Expired > before.taken.Expired
		})
	if reached.taken.Removals <= before.taken.Removals {
		t.Logf("%s was carried by the room's own removal of client %d, its expiry of the client or a local peer's frame that removes it, after the process's listener redialed (LISTENs stood %d, then %d; own removals counted as Expired %d, then %d): the removal's notification reached no listener of the process",
			what, client, before.listenStands, reached.listenStands, before.taken.Expired, reached.taken.Expired)
	}
}

// awarenessLossLines are the lines a process writes when an awareness state of one of its
// document rooms does not reach the other processes: a relay batch, or a room's query for theirs,
// whose send did not go out (each bounded by docs' roomSyncSendTimeout, 2 s). Each names the room.
var awarenessLossLines = []string{
	`msg="dispatch: relay a document room's awareness to the other processes"`,
	`msg="dispatch: ask the other processes for a document room's awareness"`,
}

// awarenessLosses is how many times the process has logged that an awareness state of its room
// of the document did not reach the other processes (awarenessLossLines).
func (p *dispatchProcess) awarenessLosses(artifactID string) int {
	losses := 0
	for _, line := range awarenessLossLines {
		losses += len(p.logLines(line, "room="+artifactID))
	}
	return losses
}

// awarenessWatch is what a wait for one client's awareness state to reach a tab on reader's room
// of the document (awaitAwarenessAcross) is measured from, read before the state is announced or
// the tab connects: how many lost awareness sends both processes had logged (awarenessLosses),
// and reader's own record of the client and of its listener (relayedAwareness).
type awarenessWatch struct {
	artifactID string
	reader     *dispatchProcess
	client     uint64
	losses     int
	base       awarenessBase
}

// watchAwareness reads, before client's state is announced or the tab that is to see it connects
// to reader, what a wait for that state to reach the tab is measured from (awaitAwarenessAcross).
func (p *dispatchPair) watchAwareness(t *testing.T, artifactID string, reader *dispatchProcess, client uint64) awarenessWatch {
	t.Helper()
	return awarenessWatch{
		artifactID: artifactID, reader: reader, client: client,
		losses: reader.awarenessLosses(artifactID) + p.other(reader).awarenessLosses(artifactID),
		base:   reader.relayedAwareness(t, artifactID, client),
	}
}

// awaitAwarenessAcross waits, through the shared wait, for tab - connected to the watch's reader -
// to hold the watched client's awareness state want, which writer, a tab on the pair's other
// process, announced. Awareness crosses processes by NOTIFY alone (docs/awareness_relay.go), and a
// state can be lost on the way, lawfully, three ways: a relay batch or a room's query whose send
// did not go out, which its sender logs (awarenessLosses); a notification sent while the reader's
// listener had no connection, after which that listener stands up a LISTEN again (ListenStands);
// and the state's expiry, docs' awarenessExpiry (30 s) after the last announcement, which the
// reader's room takes as a removal or an expiry of the client (RelayedAwareness). A browser's tab
// announces its state again on its heartbeat and when it sees the state removed, so on a try that
// finds one of the three since the watch, or since the last announcement it made, writer
// announces want again, as that tab would, and the wait goes on; nothing else announces it again.
// It fails t at the shared wait's give-up naming what the tab held, and returns an error when the
// tab's connection ends first.
func (p *dispatchPair) awaitAwarenessAcross(t *testing.T, watch awarenessWatch, tab, writer *docstest.Peer, want any) error {
	t.Helper()
	holds := holdsState(watch.client, want)
	other := p.other(watch.reader)
	var ended error
	testwait.Until(t, fmt.Sprintf("client %d's awareness state %v to reach the tab on %s", watch.client, want, p.name(watch.reader)), func() (string, bool) {
		states := map[uint64]string{}
		for client, state := range tab.AwarenessStates() {
			states[client] = string(state)
		}
		select {
		case <-tab.Ended:
			// The tab's reader has stopped, so these states are its last.
			if final := tab.AwarenessStates(); !holds(final) {
				ended = fmt.Errorf("the tab's connection closed before it held client %d's state; it holds %v", watch.client, states)
			}
			return fmt.Sprint(states), true
		default:
		}
		if holds(tab.AwarenessStates()) {
			return fmt.Sprint(states), true
		}
		losses := watch.reader.awarenessLosses(watch.artifactID) + other.awarenessLosses(watch.artifactID)
		now := watch.reader.relayedAwareness(t, watch.artifactID, watch.client)
		lost := losses > watch.losses || now.listenStands > watch.base.listenStands ||
			now.taken.Removals > watch.base.taken.Removals || now.taken.Expired > watch.base.taken.Expired
		if lost {
			t.Logf("client %d's state was lost on its way to the tab on %s (lost sends %d, then %d; the reader's process holds %+v, then %+v), so its tab announces it again",
				watch.client, p.name(watch.reader), watch.losses, losses, watch.base, now)
			if err := writer.SetAwareness(watch.client, want); err != nil {
				t.Fatalf("announce client %d's state again: %v", watch.client, err)
			}
			watch.losses, watch.base = losses, now
		}
		return fmt.Sprintf("the tab holds %v; the reader's process holds %+v", states, now), false
	})
	return ended
}

// issueCloseBase is what a wait for a close or a reopen of a document's issue to close a process's
// room of it (awaitNotifiedIssueClose) is measured from, read before the close or reopen: how
// many of the issue's notifications the process's issue-open lane had applied to the document
// (docs.RoomCounters.NotifiedIssueCloses), how many closes of the room the recheck after a LISTEN
// had made (RecheckedIssueCloses), and how many LISTENs the process's listener had stood up
// (ListenStands).
type issueCloseBase struct {
	notified, rechecked, listenStands int64
}

// issueCloseBase reads, before a close or a reopen of the document's issue, what a wait for it to
// close the process's room is measured from (awaitNotifiedIssueClose).
func (p *dispatchProcess) issueCloseBase(t *testing.T, artifactID string) issueCloseBase {
	t.Helper()
	counters := p.roomCounters(t, artifactID)
	return issueCloseBase{
		notified: counters.NotifiedIssueCloses, rechecked: counters.RecheckedIssueCloses, listenStands: counters.ListenStands,
	}
}

// awaitNotifiedIssueClose waits for the process's issue-open lane to apply a notification that the
// document's issue closed or reopened past before, read before the close or reopen
// (issueCloseBase): what closes the process's room of the document so its browsers reconnect
// with the scope the issue now allows. It is the half of withinLive every run holds for an issue's
// close or reopen. The append gate's refusal closes such a room too, and does not count.
//
// The close has one other carrier: a close or reopen notified while the process's listener had no
// connection reached nobody there, and the recheck after its next LISTEN (recheckResidentIssues)
// closes the room instead. A listener redials when its ping times out, which a loaded machine can
// cause, so where the process stood up a LISTEN again after before was read (ListenStands), the
// wait also ends at a close the recheck made (RecheckedIssueCloses), and logs that a redial
// carried it. A process that loses the issue's notifications with no redial still fails it,
// naming what, at the shared wait's give-up.
func (p *dispatchProcess) awaitNotifiedIssueClose(t *testing.T, artifactID string, before issueCloseBase, what string) {
	t.Helper()
	reached := testwait.Until(t, fmt.Sprintf("%s to be carried by the issue's notification (notified issue closes past %d), or by the recheck after a redial (rechecked closes past %d with LISTENs stood past %d)",
		what, before.notified, before.rechecked, before.listenStands),
		func() (issueCloseBase, bool) {
			now := p.issueCloseBase(t, artifactID)
			if now.notified > before.notified {
				return now, true
			}
			return now, now.listenStands > before.listenStands && now.rechecked > before.rechecked
		})
	if reached.notified <= before.notified {
		t.Logf("%s was carried by the recheck after the process's listener redialed (LISTENs stood %d, then %d; rechecked closes %d, then %d): the issue's notification reached no listener of the process",
			what, before.listenStands, reached.listenStands, before.rechecked, reached.rechecked)
	}
}

// awaitRemovalReachesReader waits for client, removed on another process, to leave reader's view:
// the tab reader is, connected to the process's room of the document. Where the process's listener
// stood up a LISTEN again after before was read (relayedAwareness), it also ends at the room's own
// removal of the client (docs.RelayedAwarenessChanges.Expired: its expiry of the client, or a
// local peer's frame that removes it), and logs that a redial carried the removal: a removal
// notified while the listener had no connection reached nobody there, and ygo tells a room's peers
// of no expiry. A browser drops a client it stops hearing from by its own timeout (y-protocols'
// outdatedTimeout, 30 s), which docstest.Peer does not do, so the room's own removal is the last
// of that removal the test can see. With no redial the client must leave the view. It fails,
// naming what, when the reader's connection ends first or at the shared wait's give-up;
// awaitRelayedAwareness then says what carried the removal.
func (p *dispatchProcess) awaitRemovalReachesReader(t *testing.T, artifactID string, client uint64, reader *docstest.Peer, before awarenessBase, what string) {
	t.Helper()
	expired := false
	testwait.Until(t, fmt.Sprintf("%s: client %d to leave the reader's view, or the reader's process to remove it itself after a redial, by its expiry of the client or a local peer's frame that removes it (own removals counted as Expired past %d with LISTENs stood past %d)",
		what, client, before.taken.Expired, before.listenStands),
		func() (string, bool) {
			select {
			case <-reader.Ended:
				t.Fatalf("%s: the reader's connection closed while client %d was still in its view", what, client)
			default:
			}
			if _, present := reader.AwarenessStates()[client]; !present {
				return "gone from the reader's view", true
			}
			now := p.relayedAwareness(t, artifactID, client)
			expired = now.listenStands > before.listenStands && now.taken.Expired > before.taken.Expired
			return fmt.Sprintf("still in the reader's view; the reader's process holds %+v", now), expired
		})
	if expired {
		t.Logf("%s: the reader's process removed client %d itself after its listener redialed, by its expiry of the client or a local peer's frame that removes it, and the reader's browser drops it by its own timeout", what, client)
	}
}

// cursorState is the awareness state a dashboard tab announces: who it is and where its cursor is.
func cursorState(login string, at int) map[string]any {
	return map[string]any{"user": map[string]any{"name": login}, "cursor": map[string]any{"anchor": at, "head": at}}
}

// holdsState is a WaitForAwareness condition: client's state is want, compared as JSON values.
func holdsState(client uint64, want any) func(map[uint64]json.RawMessage) bool {
	encoded, _ := json.Marshal(want)
	var wanted any
	_ = json.Unmarshal(encoded, &wanted)
	return func(states map[uint64]json.RawMessage) bool {
		raw, present := states[client]
		if !present {
			return false
		}
		var got any
		return json.Unmarshal(raw, &got) == nil && reflect.DeepEqual(got, wanted)
	}
}

// The awareness client ids the tabs below announce, as a Yjs provider announces its document's.
const (
	aliceAwarenessClient uint64 = 1001
	bobAwarenessClient   uint64 = 2002
)

// dialTabSocket connects a document socket of its own to process, as a browser tab's provider
// does, and drains what the room sends it so the connection never stalls on an unread message.
// The socket closes with the test; a test whose tab must leave sooner closes it itself.
func dialTabSocket(t *testing.T, process *dispatchProcess, artifactID, user string) *gws.Conn {
	t.Helper()
	socket, response, err := gws.DefaultDialer.Dial(process.documentURL(artifactID), asUser(user))
	if err != nil {
		t.Fatalf("connect %s's document socket to the document: response=%#v err=%v", user, response, err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	go func() {
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}()
	return socket
}

// announceAwarenessClients has socket announce count awareness clients from first, each at clock
// with the state its id is given, in one awareness frame under artifactID: what a tab's provider
// sends for its own client, re-sent at a later clock as its heartbeat, and what a flooding tab
// sends for the ids it invents.
func announceAwarenessClients(socket *gws.Conn, artifactID string, first, count, clock uint64, state func(client uint64) string) error {
	update := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarUint(count)
		for client := first; client < first+count; client++ {
			e.WriteVarUint(client)
			e.WriteVarUint(clock)
			e.WriteVarString(state(client))
		}
	})
	frame := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarString(artifactID)
		e.WriteVarUint(1) // awareness
		e.WriteVarBytes(update)
	})
	return socket.WriteMessage(gws.BinaryMessage, frame)
}

// Spec item 1: two people on one document, each connected to a different process, see each
// other's typing and cursors live, and nobody is disconnected; that each arrives within a second
// is held in the timing lane (the OnTheClock twin below). Each subtest has a document of its own,
// so one behaviour failing does not decide another's.
func TestTwoEditorsOnTwoProcessesSeeEachOthersEditsAndCursorsLive(t *testing.T) {
	twoEditorsOnTwoProcessesSeeEachOthersEditsAndCursorsLive(t, benchtime.EveryRun)
}

// TestTwoEditorsOnTwoProcessesSeeEachOthersEditsAndCursorsLiveOnTheClock is the row above in
// benchtime's timing lane, where each keystroke, cursor, presence and API edit it waits for is
// held to liveWithin of its start (withinLive). The product's promise is README.md's "Two
// processes share a live document session through Postgres": Edits ("The room stays open, and
// its browsers see the edit on the socket they already have") and Cursors and presence ("a
// reader who opens second sees the first reader's cursor at once", and readers "moving at the
// same moment all cross"); the spec's "at once" is the one second the plan pins (#2414).
func TestTwoEditorsOnTwoProcessesSeeEachOthersEditsAndCursorsLiveOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), twoEditorsOnTwoProcessesSeeEachOthersEditsAndCursorsLive)
}

// twoEditorsOnTwoProcessesSeeEachOthersEditsAndCursorsLive is the scene of the two rows above;
// lane says whether its readings of the clock are held to their figures (benchtime.Lane).
func twoEditorsOnTwoProcessesSeeEachOthersEditsAndCursorsLive(t *testing.T, lane benchtime.Lane) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "LIVE")
	pair.a.createProject(t, project, "Live across processes")

	t.Run("each keystroke reaches the editor on the other process", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Typed on both", "x")
		alice := pair.a.openEditorAs(t, "alice", artifactID, "x\n")
		bob := pair.b.openEditorAs(t, "bob", artifactID, "x\n")
		for keystroke := 1; keystroke <= 3; keystroke++ {
			pair.typeAndSee(t, lane, artifactID, "alice on A", alice, "a", "bob on B", bob, pair.b, keystroke)
			pair.typeAndSee(t, lane, artifactID, "bob on B", bob, "b", "alice on A", alice, pair.a, keystroke)
		}
		pair.requireConnected(t, artifactID, "alice on A", alice, "after the keystrokes")
		pair.requireConnected(t, artifactID, "bob on B", bob, "after the keystrokes")
		pair.requireNoRoomCloses(t, artifactID)
	})

	t.Run("cursors and presence cross processes", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Cursors on both", "x")
		alice := pair.a.openEditorAs(t, "alice", artifactID, "x\n")
		if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
			t.Fatalf("announce alice's cursor: %v", err)
		}
		// Bob opens the document second, on the other process: alice's cursor must be there for
		// him at once, not after her tab's next heartbeat.
		aliceOnB := pair.b.relayedAwareness(t, artifactID, aliceAwarenessClient)
		aliceToBob := pair.watchAwareness(t, artifactID, pair.b, aliceAwarenessClient)
		bob := pair.b.openEditorAs(t, "bob", artifactID, "x\n")
		opened := time.Now()
		if err := pair.awaitAwarenessAcross(t, aliceToBob, bob, alice, cursorState("alice", 0)); err != nil {
			t.Fatalf("bob, who opened the document second and on B, did not see the cursor alice set on A: %v",
				err)
		}
		what := "the cursor alice set on A reaching bob, who opened the document second and on B,"
		withinLive(t, lane, opened, what)
		pair.b.awaitRelayedAwareness(t, artifactID, aliceAwarenessClient, false, aliceOnB, what)
		bobOnA := pair.a.relayedAwareness(t, artifactID, bobAwarenessClient)
		bobToAlice := pair.watchAwareness(t, artifactID, pair.a, bobAwarenessClient)
		if err := bob.SetAwareness(bobAwarenessClient, cursorState("bob", 1)); err != nil {
			t.Fatalf("announce bob's cursor: %v", err)
		}
		set := time.Now()
		if err := pair.awaitAwarenessAcross(t, bobToAlice, alice, bob, cursorState("bob", 1)); err != nil {
			t.Fatalf("the cursor bob set on B did not reach alice on A: %v", err)
		}
		what = "the cursor bob set on B reaching alice on A"
		withinLive(t, lane, set, what)
		pair.a.awaitRelayedAwareness(t, artifactID, bobAwarenessClient, false, bobOnA, what)
		aliceOnB = pair.b.relayedAwareness(t, artifactID, aliceAwarenessClient)
		aliceToBob = pair.watchAwareness(t, artifactID, pair.b, aliceAwarenessClient)
		if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 1)); err != nil {
			t.Fatalf("move alice's cursor: %v", err)
		}
		moved := time.Now()
		if err := pair.awaitAwarenessAcross(t, aliceToBob, bob, alice, cursorState("alice", 1)); err != nil {
			t.Fatalf("alice's cursor moving on A did not reach bob on B: %v", err)
		}
		what = "alice's cursor moving on A reaching bob on B"
		withinLive(t, lane, moved, what)
		pair.b.awaitRelayedAwareness(t, artifactID, aliceAwarenessClient, false, aliceOnB, what)
		pair.requireConnected(t, artifactID, "alice on A", alice, "while the cursors moved")
		pair.requireConnected(t, artifactID, "bob on B", bob, "while the cursors moved")
		aliceOnB = pair.b.relayedAwareness(t, artifactID, aliceAwarenessClient)
		alice.Close()
		closed := time.Now()
		what = "alice's tab closing on A removing her presence from bob's tab on B"
		pair.b.awaitRemovalReachesReader(t, artifactID, aliceAwarenessClient, bob, aliceOnB, what)
		withinLive(t, lane, closed, what)
		pair.b.awaitRelayedAwareness(t, artifactID, aliceAwarenessClient, true, aliceOnB, what)
		pair.requireConnected(t, artifactID, "bob on B", bob, "after alice left")
		pair.requireNoRoomCloses(t, artifactID)
	})

	t.Run("an edit through B's API reaches an editor on A", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Written through the other API", "x")
		alice := pair.a.openEditorAs(t, "alice", artifactID, "x\n")
		base := pair.catchUpBase(t, pair.a, artifactID)
		if answer := pair.b.send(t, http.MethodPost, editPath(artifactID), editBody(t, "x", "y", "", false), asUser("bob")); answer.status != http.StatusOK {
			t.Fatalf("bob's edit through B: %v", answer)
		}
		sent := time.Now()
		if err := alice.WaitForText(t, markdown, "y\n"); err != nil {
			t.Fatalf("bob's edit through B's API did not reach alice's editor on A live: %v; %s",
				err, pair.roomCloses(t, artifactID))
		}
		withinLive(t, lane, sent, "bob's edit through B's API reaching alice's editor on A")
		pair.a.awaitNotifiedCatchUp(t, artifactID, base, "bob's edit through B's API reaching alice's editor on A")
		pair.requireConnected(t, artifactID, "alice on A", alice, "after bob's edit")
		pair.requireNoRoomCloses(t, artifactID)
	})

	// Finding 7 of the audit: a room whose append another process's write had left behind was
	// failed, and a failed room compacts while it waits for the document lock - the one
	// compaction that does. Two people taking turns, each keystroke stored before the next,
	// must fail no room and run no such compaction on either process (the room-counters hook).
	// A typist whose socket closes reconnects as the dashboard does, so a process that fails
	// rooms shows it in the counters rather than stopping the session at its first casualty.
	t.Run("two hundred alternating keystrokes fail no room and run no lock-waiting compaction", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Taking turns", "x")
		typists := []*typist{
			{who: "alice on A", login: "alice", process: pair.a, text: "a"},
			{who: "bob on B", login: "bob", process: pair.b, text: "b"},
		}
		typists[0].editor = pair.openSyncedEditor(t, pair.a, "alice", artifactID, "x\n")
		typists[1].editor = pair.openSyncedEditor(t, pair.b, "bob", artifactID, "x\n")
		const keystrokes = 200
		var stopped error
		typed := 0
		for ; typed < keystrokes && stopped == nil; typed++ {
			stopped = pair.keystroke(t, artifactID, typists[typed%2])
		}
		countersA, countersB := pair.a.roomCounters(t, artifactID), pair.b.roomCounters(t, artifactID)
		t.Logf("after %d keystrokes: A %+v, B %+v", typed, countersA, countersB)
		if countersA.RoomFailures+countersA.LockWaitingCompactions+countersB.RoomFailures+countersB.LockWaitingCompactions != 0 {
			t.Errorf("finding 7: %d alternating keystrokes failed rooms of the document and ran lock-waiting compactions: on A %d room failures and %d lock-waiting compactions, on B %d and %d; want none on either",
				typed, countersA.RoomFailures, countersA.LockWaitingCompactions, countersB.RoomFailures, countersB.LockWaitingCompactions)
		}
		for _, typing := range typists {
			if disconnected := typing.disconnected(); disconnected != "" {
				t.Errorf("%s during the session and had to reconnect", disconnected)
			}
		}
		if stopped != nil {
			t.Errorf("the session stopped after %d keystrokes: %v", typed, stopped)
		}
		pair.requireNoRoomCloses(t, artifactID)
		if t.Failed() {
			return
		}
		alice, bob := typists[0].editor, typists[1].editor
		testwait.Until(t, fmt.Sprintf("both editors to hold %d keystrokes of each", keystrokes/2), func() (string, bool) {
			seenByAlice, seenByBob := markdown(alice), markdown(bob)
			if seenByAlice == seenByBob && strings.Count(seenByAlice, "a") == keystrokes/2 && strings.Count(seenByAlice, "b") == keystrokes/2 {
				return "", true
			}
			if err := alice.AskForDocument(); err != nil {
				t.Fatalf("ask A's room for the document: %v", err)
			}
			if err := bob.AskForDocument(); err != nil {
				t.Fatalf("ask B's room for the document: %v", err)
			}
			return fmt.Sprintf("alice on A has %q, bob on B has %q", seenByAlice, seenByBob), false
		})
	})

	// One person types, then the other, each within the settle delay of the last: on one process
	// that is one burst and one version, credited to whoever typed last. Across two processes it
	// must be too (plan D3.7), whichever process wins the settlement. A settlement runs on a
	// process's own timer, once that process has applied no update for the settle delay, so a gap
	// between two keystrokes is one in which a settlement could lawfully run only when some process
	// could have gone that long without one. Each process applies a keystroke no earlier than it
	// was typed and no later than the moment both processes held it: stored by the typist's, and
	// shown to the other typist by the other's, which is the next keystroke's typist, so the next
	// keystroke comes after it. A process's span without an update between keystrokes k and k+1 is
	// then at most the time from k's typing to k+1's being held by both, on this host's clock, which
	// the processes' timers run on too. The burst is held to a version for each gap whose span
	// reached the delay and one more, the last credited to the last typist.
	t.Run("alternating typists within the debounce make one version credited to the last typist", func(t *testing.T) {
		issueKey, artifactID := pair.a.createIssueIn(t, project, "One burst", "x")
		waitForSettlementOwed(t, pair.database, artifactID, false)
		baseline := latestSeq(t, pair, issueKey)
		typists := []*typist{
			{who: "alice on A", login: "alice", process: pair.a, text: "a"},
			{who: "bob on B", login: "bob", process: pair.b, text: "b"},
		}
		typists[0].editor = pair.openSyncedEditor(t, pair.a, "alice", artifactID, "x\n")
		typists[1].editor = pair.openSyncedEditor(t, pair.b, "bob", artifactID, "x\n")
		// lapses counts the gaps between keystrokes in which a settlement could lawfully run.
		lapses := 0
		var typedBefore time.Time
		for keystroke := range 6 {
			typing, reading := typists[keystroke%2], typists[(keystroke+1)%2]
			typed := time.Now()
			if err := pair.keystroke(t, artifactID, typing); err != nil {
				t.Fatalf("keystroke %d: %v", keystroke+1, err)
			}
			if peerEnded(typing.editor) {
				t.Fatalf("%s's tab closed at keystroke %d: %s", typing.who, keystroke+1, typing.disconnected())
			}
			if err := reading.editor.WaitForText(t, markdown, markdown(typing.editor)); err != nil {
				t.Fatalf("%s's keystroke %d did not reach %s: %v", typing.who, keystroke+1, reading.who, err)
			}
			// The pair runs with no DISPATCH_TEST_SETTLE_DELAY, so each process's settle delay is the
			// document service's own (docs.DefaultSettle).
			if span := time.Since(typedBefore); keystroke > 0 && span >= docs.DefaultSettle {
				lapses++
				t.Logf("keystroke %d was held by both processes %v after keystroke %d was typed, at or past the settle delay", keystroke+1, span, keystroke)
			}
			typedBefore = typed
		}
		lastTypist := typists[1]
		burst := time.Now()
		// The wait ends as soon as the burst has settled: the settlement is no longer owed and
		// the version it minted is on the issue.
		testwait.Until(t, "the burst to settle into one version", func() (string, bool) {
			owed := settlementOwed(t, pair.database, artifactID)
			actors := versionActorsSince(t, pair, issueKey, baseline)
			return fmt.Sprintf("settlement owed: %t; versions credited to %+v", owed, actors), !owed && len(actors) > 0
		})
		actors := versionActorsSince(t, pair, issueKey, baseline)
		t.Logf("%v after the burst: versions credited to %+v; settlement owed: %t", time.Since(burst), actors, settlementOwed(t, pair.database, artifactID))
		if last := actors[len(actors)-1]; len(actors) > 1+lapses || last.Kind != "user" || last.ID != lastTypist.login {
			t.Errorf("six alternating keystrokes, %d gaps between them reaching the settle delay, made %d versions, credited to %+v; want one version for each such gap and one more, the last credited to %s, the last typist",
				lapses, len(actors), actors, lastTypist.login)
		}
		for _, typing := range typists {
			if disconnected := typing.disconnected(); disconnected != "" {
				t.Errorf("%s during the burst", disconnected)
			}
		}
	})
}

// patchIssueStatus sets the issue's status through the process's API: "done" closes it, any other
// status reopens it.
func (p *dispatchProcess) patchIssueStatus(t *testing.T, issueKey, status string) {
	t.Helper()
	p.call(t, http.MethodPatch, "/api/v1/issues/"+issueKey, fmt.Sprintf(`{"status":%q}`, status), http.StatusOK)
}

// issueCloses is, for each process of the pair, what a wait for a close or a reopen of a
// document's issue to close its room is measured from (issueCloseBase), read before the close or
// reopen: what requireEndsWithin waits past.
type issueCloses struct {
	artifactID string
	before     map[*dispatchProcess]issueCloseBase
}

// issueCloses reads, before a close or a reopen of the document's issue, what each process of the
// pair's wait for it is measured from.
func (p *dispatchPair) issueCloses(t *testing.T, artifactID string) issueCloses {
	t.Helper()
	return issueCloses{artifactID: artifactID, before: map[*dispatchProcess]issueCloseBase{
		p.a: p.a.issueCloseBase(t, artifactID), p.b: p.b.issueCloseBase(t, artifactID),
	}}
}

// requireEndsWithin waits for the editor's socket, on process, to close, and fails the test with
// what the process logged when it stays open; then it waits for process to have applied the
// issue's notification, or after a redial the recheck's close, past what closes read before the
// close or reopen (awaitNotifiedIssueClose), which is what closed the socket's room. On the clock
// it holds the close to liveWithin of the call (withinLive).
func requireEndsWithin(t *testing.T, lane benchtime.Lane, editor *docstest.Peer, what string, process *dispatchProcess, closes issueCloses) {
	t.Helper()
	since := time.Now()
	testwait.Until(t, what, func() (withOutput[string], bool) {
		select {
		case <-editor.Ended:
			return withOutput[string]{}, true
		default:
			return readWith("the socket is still open; that process's output:", process.Output), false
		}
	})
	withinLive(t, lane, since, "the close ("+what+")")
	process.awaitNotifiedIssueClose(t, closes.artifactID, closes.before[process], "the close ("+what+")")
}

// reconnectWithScope reconnects login's tab to the document on process, authenticates it as the
// dashboard does, and requires the scope the room grants it.
func reconnectWithScope(t *testing.T, process *dispatchProcess, login, artifactID, scope, what string) *docstest.Peer {
	t.Helper()
	editor := process.reconnectEditor(t, login, artifactID)
	if granted := authenticateEditor(t, editor); granted != scope {
		t.Fatalf("%s: the room granted %q, want %q", what, granted, scope)
	}
	return editor
}

// Spec item 2: closing or reopening an issue changes every process's copy of its documents at
// once. A tab on the process that did not take the request is closed with the others and comes
// back with the scope the issue now allows; an update sent on the closed issue is not stored.
//
// Neither tab's connect stores anything - its sync changes nothing in the room it joins, and a
// transaction that changes nothing commits no update - so both rooms stand at the stored head
// when the issue closes: what closes their sockets, if anything does, is the close.
func TestClosingAndReopeningAnIssueChangesEveryProcessesRooms(t *testing.T) {
	closingAndReopeningAnIssueChangesEveryProcessesRooms(t, benchtime.EveryRun)
}

// TestClosingAndReopeningAnIssueChangesEveryProcessesRoomsOnTheClock is the row above in
// benchtime's timing lane, where each tab's close is held to liveWithin of the close or reopen
// (requireEndsWithin). The product's promise is README.md's Issue close and reopen ("Every process
// of this build then closes its rooms of the issue's documents, and their browsers reconnect
// read-only or read-write as the issue now allows"), at the spec item's "at once", the one second
// the plan pins (#2414).
func TestClosingAndReopeningAnIssueChangesEveryProcessesRoomsOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), closingAndReopeningAnIssueChangesEveryProcessesRooms)
}

// closingAndReopeningAnIssueChangesEveryProcessesRooms is the scene of the two rows above; lane
// says whether its readings of the clock are held to their figures (benchtime.Lane).
func closingAndReopeningAnIssueChangesEveryProcessesRooms(t *testing.T, lane benchtime.Lane) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "SHUT")
	pair.a.createProject(t, project, "Closed across processes")
	issueKey, artifactID := pair.b.createIssueIn(t, project, "Closed on the other process", "before")

	bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "before\n")
	alice := pair.openSyncedEditor(t, pair.a, "alice", artifactID, "before\n")
	if scope := authenticateEditor(t, alice); scope != "read-write" {
		t.Fatalf("alice's tab on A on an open issue was granted %q, want read-write", scope)
	}
	bobEndedBeforeClose := peerEnded(bob)

	closes := pair.issueCloses(t, artifactID)
	pair.b.patchIssueStatus(t, issueKey, "done")
	requireEndsWithin(t, lane, alice,
		"closing the issue through B left alice's tab on A connected to a room that still takes edits on the closed issue", pair.a, closes)

	if bobEndedBeforeClose {
		t.Fatalf("bob's tab on B was disconnected before the close (close code %d), so the close cannot be shown closing it; %s",
			bob.CloseCode(), pair.roomCloses(t, artifactID))
	}
	requireEndsWithin(t, lane, bob, "closing the issue through B left bob's tab on B connected", pair.b, closes)

	alice = reconnectWithScope(t, pair.a, "alice", artifactID, "readonly", "alice's tab on A, reconnected after the close")
	bob = reconnectWithScope(t, pair.b, "bob", artifactID, "readonly", "bob's tab on B, reconnected after the close")
	closedHead := storedHead(t, pair, artifactID)
	if err := typeAtEnd(alice, "zzz"); err != nil {
		t.Fatalf("alice's keystroke on the closed issue: %v", err)
	}
	held, err := roomRendering(t, alice)
	if err != nil {
		t.Fatalf("read A's room after alice's keystroke on the closed issue: %v", err)
	}
	if strings.Contains(held, "zzz") {
		t.Errorf("A's room took alice's keystroke on the closed issue: it holds %q", held)
	}
	if head := storedHead(t, pair, artifactID); head != closedHead {
		t.Errorf("the document moved from update %d to %d after the close: an update on a closed issue was stored", closedHead, head)
	}

	closes = pair.issueCloses(t, artifactID)
	pair.a.patchIssueStatus(t, issueKey, "todo")
	requireEndsWithin(t, lane, alice, "reopening the issue through A left alice's read-only tab on A connected", pair.a, closes)
	requireEndsWithin(t, lane, bob,
		"reopening the issue through A left bob's read-only tab on B connected, so it cannot edit the reopened issue", pair.b, closes)

	alice = reconnectWithScope(t, pair.a, "alice", artifactID, "read-write", "alice's tab on A, reconnected after the reopen")
	bob = reconnectWithScope(t, pair.b, "bob", artifactID, "read-write", "bob's tab on B, reconnected after the reopen")
	for _, edit := range []struct {
		who    string
		editor *docstest.Peer
		text   string
	}{{"alice on A", alice, "typed-a"}, {"bob on B", bob, "typed-b"}} {
		if err := typeAtEnd(edit.editor, edit.text); err != nil {
			t.Fatalf("%s's keystroke on the reopened issue: %v", edit.who, err)
		}
		pair.waitStoredHolds(t, artifactID, edit.text, edit.who+"'s keystroke on the reopened issue")
	}
}

// documentAsk is an ask a document's settlement indexed: its id, its block and its author.
type documentAsk struct {
	ID      string      `json:"id"`
	BlockID *string     `json:"block_id"`
	Author  model.Actor `json:"author"`
}

// askOfBlock is the ask a settlement made of blockID on the issue, read through the process's API
// (GET /api/v1/issues/{key}/asks), or false while there is none.
func (p *dispatchProcess) askOfBlock(t *testing.T, issueKey, blockID string) (documentAsk, bool) {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks", "", http.StatusOK)
	var asks []documentAsk
	if err := json.Unmarshal(answer, &asks); err != nil {
		t.Fatalf("decode the document's asks: %v: %s", err, answer)
	}
	for _, ask := range asks {
		if ask.BlockID != nil && *ask.BlockID == blockID {
			return ask, true
		}
	}
	return documentAsk{}, false
}

// readAsk is the ask as the process's API reads it (GET /api/v1/asks/{id}).
func (p *dispatchProcess) readAsk(t *testing.T, askID string) model.Ask {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/asks/"+askID, "", http.StatusOK)
	var read struct {
		Ask model.Ask `json:"ask"`
	}
	if err := json.Unmarshal(answer, &read); err != nil {
		t.Fatalf("decode the ask: %v: %s", err, answer)
	}
	return read.Ask
}

// requireAskWithdrawn fails the test unless the process reads the ask as the close of issueKey
// withdrew it: resolved, its resolution of kind withdrawn naming that close.
func requireAskWithdrawn(t *testing.T, p *dispatchProcess, issueKey, askID, when string) {
	t.Helper()
	ask := p.readAsk(t, askID)
	if ask.State != "resolved" || ask.Resolution == nil || ask.Resolution.Kind != "withdrawn" ||
		ask.Resolution.Reason != "Withdrawn by the close of "+issueKey+"." {
		t.Fatalf("%s: the ask reads %s with resolution %+v, want it withdrawn by the close of %s", when, ask.State, ask.Resolution, issueKey)
	}
}

// newestVersionNumber is the number of the document's newest version as the process's API reads
// it (GET /api/v1/artifacts/{id}), or 0 while the document has none. The read is of the database
// alone: it loads no room, so it arms no settlement.
func (p *dispatchProcess) newestVersionNumber(t *testing.T, artifactID string) int {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID, "", http.StatusOK)
	var artifact struct {
		Versions []struct {
			Number int `json:"number"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(answer, &artifact); err != nil {
		t.Fatalf("decode the document: %v: %s", err, answer)
	}
	newest := 0
	for _, version := range artifact.Versions {
		newest = max(newest, version.Number)
	}
	return newest
}

// readVersionMarkdown is the text of the document's version number as the process's API reads it
// (GET /api/v1/artifacts/{id}/versions/{n}).
func (p *dispatchProcess) readVersionMarkdown(t *testing.T, artifactID string, number int) string {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/versions/"+strconv.Itoa(number), "", http.StatusOK)
	var version struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(answer, &version); err != nil {
		t.Fatalf("decode version %d of the document: %v: %s", number, err, answer)
	}
	return version.Markdown
}

// closingAskSpec is the spec of the issues below: a paragraph to type into and an open decision.
const closingAskSpec = "Context\n\n:::ask{#ship-it urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// closingAskResolved is the decision block of closingAskSpec as the close of its issue writes it.
const closingAskResolved = `#ship-it urgency="med" multiple="false" state="resolved"`

// closingAskIssue is an issue whose spec is closingAskSpec, as the rows below have it before the
// issue closes: A's settlement made the ask of its decision block and left nothing owed, and
// bob's writable tab on B holds the spec with his keystroke " late" stored.
type closingAskIssue struct {
	key, artifactID, askID string
	bob                    *docstest.Peer
}

// openClosingAskIssue creates a closingAskIssue titled title in project through A.
func openClosingAskIssue(t *testing.T, pair *dispatchPair, project, title string) closingAskIssue {
	t.Helper()
	issueKey, artifactID := pair.a.createIssueIn(t, project, title, closingAskSpec)
	askID := testwait.Until(t, "A's settlement to make an ask of the spec's decision block", func() (withOutput[string], bool) {
		ask, found := pair.a.askOfBlock(t, issueKey, "ship-it")
		if !found {
			return readWith("no ask of the block yet", pair.a.Output), false
		}
		return readWith(ask.ID, pair.a.Output), true
	}).read
	waitForSettlementOwed(t, pair.database, artifactID, false)
	if ask := pair.b.readAsk(t, askID); ask.State != "open" {
		t.Fatalf("the decision's ask reads %s before the close, want open", ask.State)
	}

	bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, pair.b.documentText(t, artifactID))
	if scope := authenticateEditor(t, bob); scope != "read-write" {
		t.Fatalf("bob's tab on B on an open issue was granted %q, want read-write", scope)
	}
	if err := typeAtEnd(bob, " late"); err != nil {
		t.Fatalf("bob's keystroke before the close: %v", err)
	}
	pair.waitStoredHolds(t, artifactID, "Context late", "bob's keystroke before the close")
	if peerEnded(bob) {
		t.Fatalf("bob's tab on B was disconnected before the close (close code %d), so the close cannot be shown closing it; %s",
			bob.CloseCode(), pair.roomCloses(t, artifactID))
	}
	return closingAskIssue{key: issueKey, artifactID: artifactID, askID: askID, bob: bob}
}

// waitForNoSettlementOwed waits for the document's pending-settlement row to be gone on the
// running servers, and when it stands names the resumptions of each process and prints both
// processes' output (owedSettlement). The close's own write of the decision block arms the
// settlement a settle delay later on the process that made it (docs' publishLiveWrite), unless
// that process's close of the document's room stops the timer first (closeRecordLocked). Then
// nothing need open the document: the resumption arms a pending row once it is a minute old, a
// pass runs once a docs.SettlementResumeInterval (one minute) on whichever process holds that
// period's lease, and the settlement the pass arms runs a settle delay later.
func (p *dispatchPair) waitForNoSettlementOwed(t *testing.T, artifactID, what string) {
	t.Helper()
	reading := owedSettlement{pair: p, artifactID: artifactID}
	testwait.Until(t, what+" to owe no settlement", func() (owedSettlement, bool) {
		return reading, !settlementOwed(t, p.database, artifactID)
	})
}

// owedSettlement is waitForNoSettlementOwed's reading. It reads the resumptions each process
// recorded for the document, and both processes' output, only when it is printed, as the wait
// gives up: each resumption count is a call on its process's API, which no attempt of the wait
// makes.
type owedSettlement struct {
	pair       *dispatchPair
	artifactID string
}

func (o owedSettlement) String() string {
	return fmt.Sprintf("the resumptions armed its settlement at %s on A and at %s on B\n%s",
		o.resumptions(o.pair.a), o.resumptions(o.pair.b), o.pair.buffers())
}

// resumptions is what process's room counters say of the document's settlement resumptions, or
// why they could not be read: it fails nothing, since it runs inside a wait's failure.
func (o owedSettlement) resumptions(process *dispatchProcess) string {
	answer, err := process.trySend(http.MethodGet, "/api/v1/artifacts/"+o.artifactID+"/_test/room-counters", "", asUser(dispatchTestLogin))
	if err != nil {
		return fmt.Sprintf("(unread: %v)", err)
	}
	var counters docs.RoomCounters
	if answer.status != http.StatusOK || json.Unmarshal(answer.body, &counters) != nil {
		return fmt.Sprintf("(unread: %v)", answer)
	}
	return fmt.Sprintf("%v", counters.SettlementResumptions)
}

// decodeAuthors is the people a version credits, decoded from the authors latestVersion reads.
func decodeAuthors(t *testing.T, version int, authors string) []model.Actor {
	t.Helper()
	var credited []model.Actor
	if err := json.Unmarshal([]byte(authors), &credited); err != nil {
		t.Fatalf("decode the authors of version %d: %v: %s", version, err, authors)
	}
	return credited
}

// The issue's close withdraws its open ask and freezes every process's room of its spec, and
// neither undoes the other; its reopen brings both back.
//
// Bob's tab on B holds the spec, whose decision block is an open ask, and his keystroke there is
// stored, which arms B's settlement of it. The issue closes through A, which settles the issue's
// documents before it closes the issue (settleIssueBeforeClose, in the api package), so the
// keystroke is saved before the close, by that settlement or by B's own, in a version that
// credits bob. B's room closes and bob reconnects read-only; the ask reads withdrawn and the
// block resolved. The close's write of the block resolved leaves the spec owing a settlement, and
// the spec settles while the issue is closed, as an open issue's document does: the version it
// writes is past the one the close left and holds bob's keystroke and the block resolved. That
// version credits no one, because its one change is the close's write, and an ask-state write
// credits no one (WriteIssueAskStates, docs/ask_lifecycle.go). The ask still reads withdrawn on
// both processes. The reopen goes through B: the ask reads open again, bob's read-only tab is
// closed and reconnects writable, his keystroke is stored, and the settlement the reopened
// document owes leaves the ask open.
func TestClosingAnIssueOnOneProcessWithdrawsItsAskAndFreezesTheOtherProcessesRoomUntilItsReopen(t *testing.T) {
	closingAnIssueOnOneProcessWithdrawsItsAskAndFreezesTheOtherProcessesRoomUntilItsReopen(t, benchtime.EveryRun)
}

// TestClosingAnIssueOnOneProcessWithdrawsItsAskAndFreezesTheOtherProcessesRoomUntilItsReopenOnTheClock
// is the row above in benchtime's timing lane, where bob's tab's close is held to liveWithin of
// the close and of the reopen (requireEndsWithin). The product's promise is README.md's Issue
// close and reopen ("Every process of this build then closes its rooms of the issue's documents"),
// at the spec's "at once", the one second the plan pins (#2414).
func TestClosingAnIssueOnOneProcessWithdrawsItsAskAndFreezesTheOtherProcessesRoomUntilItsReopenOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), closingAnIssueOnOneProcessWithdrawsItsAskAndFreezesTheOtherProcessesRoomUntilItsReopen)
}

// closingAnIssueOnOneProcessWithdrawsItsAskAndFreezesTheOtherProcessesRoomUntilItsReopen is the
// scene of the two rows above; lane says whether its readings of the clock are held to their
// figures (benchtime.Lane).
func closingAnIssueOnOneProcessWithdrawsItsAskAndFreezesTheOtherProcessesRoomUntilItsReopen(t *testing.T, lane benchtime.Lane) {
	const settleDelay = 2 * time.Second
	pair := newPair(t)
	pair.startSettling(t, nil, settleDelay, settleDelay)
	project := uniqueProjectKey(t, "WDRN")
	pair.a.createProject(t, project, "Asks of a closed issue across processes")
	issue := openClosingAskIssue(t, pair, project, "Closed with an open ask")
	issueKey, artifactID, askID, bob := issue.key, issue.artifactID, issue.askID, issue.bob

	closes := pair.issueCloses(t, artifactID)
	// The version the close left is read by where it falls in the issue's events, ahead of the
	// close's own (versionTheCloseLeft): the close's write owes a settlement a settle delay
	// later, which can mint the next version before a read of the latest one would come.
	beforeClose := latestSeq(t, pair, issueKey)
	pair.a.patchIssueStatus(t, issueKey, "done")
	versionBefore, markdownBefore, authorsBefore := versionTheCloseLeft(t, pair, issueKey, artifactID, beforeClose)
	if !strings.Contains(markdownBefore, "Context late") ||
		!slices.ContainsFunc(decodeAuthors(t, versionBefore, authorsBefore), model.Actor{Kind: "user", ID: "bob"}.SameAs) {
		t.Fatalf("version %d, the one the close left, does not hold bob's keystroke credited to him: it credits %s and reads:\n%s",
			versionBefore, authorsBefore, markdownBefore)
	}
	requireEndsWithin(t, lane, bob, "closing the issue through A left bob's tab on B connected", pair.b, closes)
	bob = reconnectWithScope(t, pair.b, "bob", artifactID, "readonly", "bob's tab on B, reconnected after the close")
	requireAskWithdrawn(t, pair.b, issueKey, askID, "after the close")
	requireClosedText := func(what, text string) {
		t.Helper()
		if !strings.Contains(text, "Context late") || !strings.Contains(text, closingAskResolved) {
			t.Fatalf("%s of the closed issue does not hold bob's keystroke and the decision block resolved:\n%s", what, text)
		}
	}
	room, err := roomRendering(t, bob)
	if err != nil {
		t.Fatalf("read B's room after the close: %v", err)
	}
	requireClosedText("B's room", room)
	pair.waitForNoSettlementOwed(t, artifactID, "the closed issue's spec")
	version, markdown, authors := latestVersion(t, pair.database, artifactID)
	if version <= versionBefore {
		t.Fatalf("the closed issue's spec owes no settlement, yet its latest version is %d, the one the close left", version)
	}
	requireClosedText(fmt.Sprintf("version %d", version), markdown)
	if credited := decodeAuthors(t, version, authors); len(credited) != 0 {
		t.Fatalf("version %d, which the closed issue's settlement wrote of the close's write alone, credits %s, want no one", version, authors)
	}
	requireAskWithdrawn(t, pair.a, issueKey, askID, "once the closed issue's spec settled")
	requireAskWithdrawn(t, pair.b, issueKey, askID, "once the closed issue's spec settled")
	requireClosedText("the stored document", pair.a.documentText(t, artifactID))

	closes = pair.issueCloses(t, artifactID)
	pair.b.patchIssueStatus(t, issueKey, "todo")
	if ask := pair.a.readAsk(t, askID); ask.State != "open" || ask.Resolution != nil {
		t.Fatalf("the reopen left the ask %s with resolution %+v, want it open again with none", ask.State, ask.Resolution)
	}
	requireEndsWithin(t, lane, bob, "reopening the issue through B left bob's read-only tab on B connected", pair.b, closes)
	bob = reconnectWithScope(t, pair.b, "bob", artifactID, "read-write", "bob's tab on B, reconnected after the reopen")
	if reopened, err := roomRendering(t, bob); err != nil || !strings.Contains(reopened, `#ship-it urgency="med" multiple="false" state="open"`) {
		t.Fatalf("B's room of the reopened issue does not hold the decision block open (%v):\n%s", err, reopened)
	}
	if err := typeAtEnd(bob, " again"); err != nil {
		t.Fatalf("bob's keystroke on the reopened issue: %v", err)
	}
	pair.waitStoredHolds(t, artifactID, "Context late again", "bob's keystroke on the reopened issue")
	waitForSettlementOwed(t, pair.database, artifactID, false)
	if ask := pair.b.readAsk(t, askID); ask.State != "open" {
		t.Fatalf("the reopened document's settlement left the ask %s, want open", ask.State)
	}
}

// A closed issue's document settles on the running servers as an open issue's does, with nobody
// opening it again.
//
// Bob's keystroke in his tab on B is stored, and the issue closes through A. The close settles the
// spec first, so the keystroke is in the version the close leaves; the close's withdrawal of the
// decision's ask then writes the block resolved, which leaves the spec owing a settlement, and the
// close closes B's room. Nothing opens the spec after that. The close's own write arms the
// settlement on A a settle delay later, unless A's close of its room stops that timer first; then
// a resumption pass arms it, on whichever process holds that period's lease. Which of the two
// arms it is a race this test does not decide, so it does not prove the resumption on its own:
// the docs package's tests drive that path (TestAClosedIssuesOwedSettlementCompletes and the
// bound's tests). The pending row in the database goes (waitForNoSettlementOwed). The settlement
// that deletes it writes its version in the same transaction, so the newest version, read once
// through A's own API, is past the one the close left and holds bob's keystroke and the block
// resolved, and the ask still reads withdrawn on both processes. The version the close left, and
// that the close left a settlement owed, are read by where they fall in the issue's events
// (versionTheCloseLeft, versionsAfterTheClose): the owed settlement can run a settle delay after
// the close, before any read after the close's answer would come.
func TestTheRunningServersSettleAClosedIssuesDocument(t *testing.T) {
	const settleDelay = 2 * time.Second
	pair := newPair(t)
	pair.startSettling(t, nil, settleDelay, settleDelay)
	project := uniqueProjectKey(t, "STLD")
	pair.a.createProject(t, project, "Closed issues' documents settle")
	issue := openClosingAskIssue(t, pair, project, "Settled while closed")

	beforeClose := latestSeq(t, pair, issue.key)
	closed := time.Now()
	pair.a.patchIssueStatus(t, issue.key, "done")
	before, _, _ := versionTheCloseLeft(t, pair, issue.key, issue.artifactID, beforeClose)
	// A settlement deletes the owed row in the transaction that writes its version, so a row
	// read gone before the versions are read is one whose version the second read finds.
	if owed := settlementOwed(t, pair.database, issue.artifactID); !owed && versionsAfterTheClose(t, pair, issue.key, issue.artifactID, beforeClose) == 0 {
		t.Fatalf("the close left the spec owing no settlement, so no settlement on the closed issue has anything to write")
	}
	requireAskWithdrawn(t, pair.a, issue.key, issue.askID, "after the close")

	pair.waitForNoSettlementOwed(t, issue.artifactID, "the closed issue's spec")
	newest := pair.a.newestVersionNumber(t, issue.artifactID)
	if newest <= before {
		t.Fatalf("the closed issue's spec owes no settlement, yet A reads version %d as its newest, the close having left %d", newest, before)
	}
	if markdown := pair.a.readVersionMarkdown(t, issue.artifactID, newest); !strings.Contains(markdown, "Context late") || !strings.Contains(markdown, closingAskResolved) {
		t.Fatalf("version %d of the closed issue's spec, as A reads it, does not hold bob's keystroke and the decision block resolved:\n%s",
			newest, markdown)
	}
	requireAskWithdrawn(t, pair.a, issue.key, issue.askID, "once the closed issue's spec settled")
	requireAskWithdrawn(t, pair.b, issue.key, issue.askID, "once the closed issue's spec settled")
	t.Logf("the spec settled %v after the close; the resumptions armed it at %v on A and at %v on B", time.Since(closed),
		pair.a.roomCounters(t, issue.artifactID).SettlementResumptions, pair.b.roomCounters(t, issue.artifactID).SettlementResumptions)
}

// Spec item 3: an ask block keeps the author who wrote it, whichever process settles it.
//
// Bob writes an ask block in his tab on B, and B is killed (SIGKILL) before it settles - B's
// settle delay is a day (stagedPastTheRun) - so the settlement the document owes is left to the
// resumption on A. The pending row is made older than the resumption age (one minute) by SQL
// rather than by waiting it out, so A's next pass (one a minute) arms it.
//
// Alice edits the document through B after bob's block and before the kill. Without her, the
// latest edit source the pending row names is bob's, and at this row's first commit a settlement
// that does not know who introduced a block credits the block to that latest source (the
// fallback in askBlockSources.author), so the row would pass by coincidence with the author
// living only in B's memory. With her, that fallback names alice, and only an author recorded
// where A can read it names bob.
func TestAnAskBlockKeepsItsAuthorWhenAnotherProcessSettlesIt(t *testing.T) {
	pair := newPair(t)
	pair.startSettling(t, nil, 0, stagedPastTheRun)
	project := uniqueProjectKey(t, "ASKA")
	pair.b.createProject(t, project, "Ask authors across processes")
	// Created through B, whose own settlement is a day away, so A never loads this
	// document before its resumption does.
	issueKey, artifactID := pair.b.createIssueIn(t, project, "Bob asks on B", "Intro.")

	bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "Intro.\n")
	const blockID = "ask-bob-region"
	head := storedHead(t, pair, artifactID)
	appendMarkdown(t, bob, ":::ask{#"+blockID+` urgency="med" multiple="false"}`+"\nWhich region goes first?\n:::\n")
	pair.waitStoredPast(t, artifactID, head, "bob's ask block")
	if answer := pair.b.send(t, http.MethodPost, editPath(artifactID), editBody(t, "Intro.", "Intro, edited.", "", false), asUser("alice")); answer.status != http.StatusOK {
		t.Fatalf("alice's edit through B after bob's ask block: %v", answer)
	}
	if _, found := pair.b.askOfBlock(t, issueKey, blockID); found {
		t.Fatalf("B already settled bob's ask block, so no other process is left to settle it")
	}
	pair.b.kill(t)

	backdated, err := pair.database.Pool.Exec(context.Background(), `
		update doc_settlements_pending set marked_at = now() - interval '2 minutes' where artifact_id = $1
	`, artifactID)
	if err != nil {
		t.Fatalf("age the document's pending settlement: %v", err)
	}
	if backdated.RowsAffected() != 1 {
		t.Fatalf("B left no pending settlement for the document, so there is nothing for A to resume")
	}
	// A resumption pass runs once a minute, and the wait ends on the ask that pass makes.
	ask := testwait.Until(t, "A to make an ask of bob's block on its resumption of the settlement", func() (withOutput[string], bool) {
		ask, found := pair.a.askOfBlock(t, issueKey, blockID)
		if !found {
			return readWith("no ask of the block yet", pair.a.Output), false
		}
		if ask.Author.Kind != "user" || ask.Author.ID != "bob" {
			t.Fatalf("A's resumed settlement made the ask of bob's block with author %+v, want bob (user), who wrote it on B; A's resumptions armed the document's settlement at %v",
				ask.Author, pair.a.roomCounters(t, artifactID).SettlementResumptions)
		}
		return readWith(ask.ID, pair.a.Output), true
	}).read
	t.Logf("A's resumed settlement made ask %s of bob's block, keeping bob as its author", ask)
}

// backgroundRequest is a request sent from a goroutine of its own, for a request a row parks
// with a hook while it does something else.
type backgroundRequest struct {
	answered chan httpAnswer
	failed   chan error
}

// requestInBackground sends the request from a goroutine of its own and returns at once.
func (p *dispatchProcess) requestInBackground(method, path, body string, header http.Header) backgroundRequest {
	pending := backgroundRequest{answered: make(chan httpAnswer, 1), failed: make(chan error, 1)}
	go func() {
		answer, err := p.trySend(method, path, body, header)
		if err != nil {
			pending.failed <- err
			return
		}
		pending.answered <- answer
	}()
	return pending
}

// await waits for the request's answer, and fails the test when the request failed instead.
func (pending backgroundRequest) await(t *testing.T, what string, process *dispatchProcess) httpAnswer {
	t.Helper()
	var answer httpAnswer
	testwait.Until(t, what, func() (fmt.Stringer, bool) {
		select {
		case answer = <-pending.answered:
			return nil, true
		case err := <-pending.failed:
			t.Fatalf("%s: %v", what, err)
		default:
		}
		return process.Output, false
	})
	return answer
}

// The document item 4's cases write into: P, the middle paragraph, is the one bob deletes and the
// agent writes into.
const (
	agentRowSpec     = "Intro.\n\nThe pear is ripe.\n\nOutro."
	agentRowWithoutP = "Intro.\n\nOutro.\n"
)

// lostEditAnswer is the body of a lost-edit refusal or of an edit's 200: the code and
// participants of the first, the lost_ops of either.
type lostEditAnswer struct {
	Code         string        `json:"code"`
	Participants []model.Actor `json:"participants"`
	LostOps      *[]int        `json:"lost_ops"`
}

func decodeLostEdit(t *testing.T, answer httpAnswer) lostEditAnswer {
	t.Helper()
	var decoded lostEditAnswer
	if err := json.Unmarshal(answer.body, &decoded); err != nil {
		t.Fatalf("decode the edit's answer: %v: %v", err, answer)
	}
	return decoded
}

// Spec item 4: an agent's write through one process sees what a person on the other process
// typed before it, and its lost-edit check judges against every process. Bob's tab is on B, with a
// second tab of his there that shows when B's room has applied his delete; A holds no room of
// the document, so the agent's write forks it from the store. Each case has its own document.
func TestAnAgentWriteSeesEditsInFlightOnTheOtherProcess(t *testing.T) {
	anAgentWriteSeesEditsInFlightOnTheOtherProcess(t, benchtime.EveryRun)
}

// TestAnAgentWriteSeesEditsInFlightOnTheOtherProcessOnTheClock is the row above in benchtime's
// timing lane, where bob's delete reaching his other tab and the agent's earlier write reaching
// bob's tab are held to liveWithin (withinLive). The product's promise is README.md's "Two
// processes share a live document session through Postgres", Edits ("The room stays open, and its
// browsers see the edit on the socket they already have"), at the spec's "at once", the one
// second the plan pins (#2414).
func TestAnAgentWriteSeesEditsInFlightOnTheOtherProcessOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), anAgentWriteSeesEditsInFlightOnTheOtherProcess)
}

// anAgentWriteSeesEditsInFlightOnTheOtherProcess is the scene of the two rows above; lane says
// whether its readings of the clock are held to their figures (benchtime.Lane).
func anAgentWriteSeesEditsInFlightOnTheOtherProcess(t *testing.T, lane benchtime.Lane) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "AGNT")
	pair.b.createProject(t, project, "Agent writes across processes")

	// open makes the case's document through B, opens bob's two tabs on it there, and requires
	// that A holds no room of it.
	open := func(t *testing.T, title string) (artifactID string, bob, bobsOtherTab *docstest.Peer) {
		t.Helper()
		_, artifactID = pair.b.createIssueIn(t, project, title, agentRowSpec)
		waitForSettlementOwed(t, pair.database, artifactID, false)
		want := pair.b.documentText(t, artifactID)
		bob = pair.openSyncedEditor(t, pair.b, "bob", artifactID, want)
		bobsOtherTab = pair.openSyncedEditor(t, pair.b, "bob", artifactID, want)
		if pair.a.holdsRoom(t, artifactID) {
			t.Fatalf("A holds a room of the document before the agent writes, so its write would not be the cold fork this case is about")
		}
		return artifactID, bob, bobsOtherTab
	}
	// deleteP deletes P in bob's tab and waits for B's room to show his other tab the delete,
	// leaving want.
	deleteP := func(t *testing.T, artifactID string, bob, bobsOtherTab *docstest.Peer, want string) {
		t.Helper()
		deleteParagraph(t, bob, "pear")
		deleted := time.Now()
		if err := bobsOtherTab.WaitForText(t, markdown, want); err != nil {
			t.Fatalf("B's room never showed bob's delete of P to his other tab: %v; %s", err, pair.roomCloses(t, artifactID))
		}
		withinLive(t, lane, deleted, "bob's delete of P reaching his other tab on B")
	}

	t.Run("before the fork: an edit into a paragraph deleted on the other process is refused", func(t *testing.T) {
		artifactID, bob, bobsOtherTab := open(t, "Deleted before the agent forks")
		// B applies and broadcasts bob's delete at once and stores it 300 ms later.
		pair.b.holdPersistence(t, artifactID, 300*time.Millisecond)
		deleted := time.Now()
		deleteP(t, artifactID, bob, bobsOtherTab, agentRowWithoutP)
		answer := pair.a.send(t, http.MethodPost, editPath(artifactID), editBody(t, "pear", "plum", "", true), asAgent())
		t.Logf("the agent's edit through A answered %v after bob's delete", time.Since(deleted))
		if answer.status == http.StatusOK {
			t.Fatalf("the agent's edit through A of a paragraph bob had already deleted on B answered 200: A forked the document from a store that did not hold the delete B's room had applied (its append was held 300 ms), so the write landed in a deleted paragraph; want 404 TARGET_NOT_FOUND. Answer: %v", answer)
		}
		if answer.status != http.StatusNotFound || answer.code() != "TARGET_NOT_FOUND" {
			t.Fatalf("the agent's edit of a paragraph bob had deleted on B answered %v, want 404 TARGET_NOT_FOUND", answer)
		}
	})

	t.Run("while the write is open: a delete on the other process refuses it with its participants", func(t *testing.T) {
		artifactID, bob, bobsOtherTab := open(t, "Deleted while the agent writes")
		head := storedHead(t, pair, artifactID)
		pair.a.armLiveWritePause(t, artifactID, docs.PauseAfterFirstAppend)
		write := pair.a.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "pear", "pear tart", "", true), asAgent())
		// Parked after its first append, the write holds the document's advisory lock, so B's
		// append of bob's delete waits on it: the delete is in B's room and not stored.
		pair.a.awaitLiveWriteParked(t, artifactID, docs.PauseAfterFirstAppend)
		deleteP(t, artifactID, bob, bobsOtherTab, agentRowWithoutP)
		if moved := storedHead(t, pair, artifactID); moved != head {
			t.Fatalf("the document moved from update %d to %d while the agent's write held its lock, so bob's delete is not the update in flight this case is about", head, moved)
		}
		if !pair.a.releaseLiveWritePause(t, artifactID, docs.PauseAfterFirstAppend) {
			t.Fatalf("the release found no write parked")
		}
		answer := write.await(t, "the agent's edit through A, after its release", pair.a)
		decoded := decodeLostEdit(t, answer)
		if answer.status != http.StatusConflict || decoded.Code != "EDIT_LOST_TO_CONCURRENT_CHANGE" {
			t.Fatalf("the agent's write into P through A, which bob deleted on B while the write was open, answered %v; want 409 EDIT_LOST_TO_CONCURRENT_CHANGE naming bob: the write's check judged a fork that never saw B's room, so the paragraph it wrote into reads as alive", answer)
		}
		if !slices.ContainsFunc(decoded.Participants, func(actor model.Actor) bool { return actor.Kind == "user" && actor.ID == "bob" }) {
			t.Errorf("the lost-edit refusal names participants %+v; want bob, connected on B, among them", decoded.Participants)
		}
		testwait.Until(t, "bob's delete to be stored once the refused write rolled back", func() (string, bool) {
			text := pair.a.documentText(t, artifactID)
			return text, !strings.Contains(text, "pear")
		})
	})

	// The write that is judged here first commits and then publishes, so B learns of its commit
	// before bob deletes. An earlier agent write through A, which this case does not judge, is
	// what makes that hold at this row's first commit too: a commit on A closes B's room there
	// unless an eviction of that room shortly before holds it off, so where the earlier write
	// closes B's room, bob's tabs reconnect as a browser's do, and the judged write lands inside
	// the window that close opened - B's room stays open, and behind.
	//
	// The case measures something only while B's room stays open with bob's delete in it until
	// the write is judged. A run whose own setup outlasted that window finds B's room closed,
	// which ends bob's tabs; that run is let go and the case set up again on a fresh document,
	// through the shared wait, so the state itself, read as the write is released to be judged,
	// decides the row and no reading of this host's clock does.
	t.Run("after the commit: a delete on the other process is reported in lost_ops", func(t *testing.T) {
		const earlier = "Intro, first.\n\nThe pear is ripe.\n\nOutro.\n"
		runs := 0
		judgedWhileOpen := func() (httpAnswer, string, bool) {
			runs++
			artifactID, bob, bobsOtherTab := open(t, fmt.Sprintf("Deleted before the agent publishes, run %d", runs))
			base := pair.catchUpBase(t, pair.b, artifactID)
			if answer := pair.a.send(t, http.MethodPost, editPath(artifactID), editBody(t, "Intro.", "Intro, first.", "", true), asAgent()); answer.status != http.StatusOK {
				t.Fatalf("the agent's earlier write through A: %v", answer)
			}
			wrote := time.Now()
			if err := bobsOtherTab.WaitForText(t, markdown, earlier); err == nil {
				const reached = "the agent's earlier write through A reaching bob's tab on B"
				withinLive(t, lane, wrote, reached)
				pair.b.awaitNotifiedCatchUp(t, artifactID, base, reached)
			} else {
				if !peerEnded(bobsOtherTab) {
					t.Fatalf("the agent's earlier write through A reached neither bob's tab on B nor closed it: %v", err)
				}
				t.Logf("the earlier write closed B's room (bob's tab: close code %d); bob's tabs reconnect", bobsOtherTab.CloseCode())
				bob = pair.reconnectSynced(t, pair.b, "bob", artifactID)
				bobsOtherTab = pair.reconnectSynced(t, pair.b, "bob", artifactID)
				// The judged write is to fork the store, as a cold write does, so the room the
				// earlier write's publish left on A has to be gone before it runs.
				testwait.Eventually(t, "A to let go of the room the earlier write's publish loaded, so the judged write forks cold",
					func() bool { return !pair.a.holdsRoom(t, artifactID) })
			}
			pair.a.armLiveWritePause(t, artifactID, docs.PauseAfterCommit)
			write := pair.a.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "pear", "pear tart", "", true), asAgent())
			pair.a.awaitLiveWriteParked(t, artifactID, docs.PauseAfterCommit)
			// The write is committed; bob's delete reaches B's room now, and B stores it 300 ms on.
			pair.b.holdPersistence(t, artifactID, 300*time.Millisecond)
			roomOpen := !peerEnded(bob) && !peerEnded(bobsOtherTab)
			if roomOpen {
				deleted := time.Now()
				if err := removeParagraph(bob, "pear"); err != nil && !peerEnded(bob) {
					t.Fatalf("delete the paragraph holding %q: %v", "pear", err)
				}
				switch err := bobsOtherTab.WaitForText(t, markdown, "Intro, first.\n\nOutro.\n"); {
				case err == nil:
					withinLive(t, lane, deleted, "bob's delete of P reaching his other tab on B")
				case !peerEnded(bobsOtherTab):
					t.Fatalf("B's room never showed bob's delete of P to his other tab: %v; %s", err, pair.roomCloses(t, artifactID))
				}
				// The state the case needs, read as the write is released to be judged: B's room
				// still open, so bob's tabs still connected, with his delete in it.
				roomOpen = !peerEnded(bob) && !peerEnded(bobsOtherTab)
			}
			if !pair.a.releaseLiveWritePause(t, artifactID, docs.PauseAfterCommit) {
				t.Fatalf("the release found no write parked")
			}
			answer := write.await(t, "the agent's edit through A, after its release", pair.a)
			if !roomOpen {
				return answer, fmt.Sprintf("run %d: B closed its room of the document before the write was judged; %s", runs, pair.roomCloses(t, artifactID)), false
			}
			return answer, "", true
		}
		var answer httpAnswer
		testwait.Until(t, "a run of the case whose write is judged while B's room still holds bob's delete", func() (string, bool) {
			judged, reading, held := judgedWhileOpen()
			answer = judged
			return reading, held
		})
		if answer.status != http.StatusOK {
			t.Fatalf("the agent's committed write answered %v, want 200 with lost_ops", answer)
		}
		decoded := decodeLostEdit(t, answer)
		if decoded.LostOps == nil || !slices.Equal(*decoded.LostOps, []int{0}) {
			got := "null"
			if decoded.LostOps != nil {
				got = fmt.Sprint(*decoded.LostOps)
			}
			t.Fatalf("the agent's write into P, which bob deleted on B after it committed and before it published, answered 200 with lost_ops %s; want [0], its one operation: the loss was judged on A's room alone, which never receives B's delete. Answer: %v", got, answer)
		}
	})
}

// reconnectSynced reconnects login's tab on process and checks that its sync left the durable
// head where it was, as openSyncedEditor does for a first connection. Its callers reconnect tabs
// that have typed nothing, so the state such a tab sends changes nothing in the room it rejoins
// and the room stores nothing for it.
func (p *dispatchPair) reconnectSynced(t *testing.T, process *dispatchProcess, login, artifactID string) *docstest.Peer {
	t.Helper()
	before := storedHead(t, p, artifactID)
	editor := process.reconnectEditor(t, login, artifactID)
	if head := storedHead(t, p, artifactID); head != before {
		t.Fatalf("the document moved from update %d to %d while %s's tab reconnected on %s: a sync that changes nothing stores nothing, so something else wrote the document",
			before, head, login, p.name(process))
	}
	return editor
}

// Spec item 5: a read never returns text older than the store holds, whichever process serves it.
func TestAReadOnTheOtherProcessIsNeverOlderThanTheStore(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "READ")
	pair.a.createProject(t, project, "Reads across processes")

	t.Run("a read on B right after each write on A shows it and its token passes the next conditional write", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Written on A, read on B", "v0")
		bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "v0\n")
		_, token := pair.a.textAndToken(t, artifactID)
		reconnects := 0
		for write := 1; write <= 50; write++ {
			before, after := fmt.Sprintf("v%d", write-1), fmt.Sprintf("v%d", write)
			if peerEnded(bob) {
				reconnects++
				bob = pair.reconnectSynced(t, pair.b, "bob", artifactID)
			}
			answer := pair.a.send(t, http.MethodPost, editPath(artifactID), editBody(t, before, after, token, false), asUser("alice"))
			if answer.status != http.StatusOK {
				t.Fatalf("write %d through A, conditional on the token of the read B served after write %d, answered %v: that read's token was not the stored document's",
					write, write-1, answer)
			}
			read, readToken := pair.b.textAndToken(t, artifactID)
			if read != after+"\n" {
				t.Fatalf("read %d on B, sent the moment write %d through A answered 200, answered %q: older than the store, which holds %q. Bob's tab on B, which keeps B's room resident, reconnected %d times by then, as a browser whose room was closed does; %s",
					write, write, read, after+"\n", reconnects, pair.roomCloses(t, artifactID))
			}
			token = readToken
		}
	})

	// A comment anchored to a mark a browser placed is verified against the document (VerifyMark),
	// which waits up to a second (MarkWait) for the mark to arrive. Alice places the mark on A;
	// the comment goes through B, where bob's tab keeps a room of the document.
	//
	// While alice connects and marks, a write through B is parked after its commit, in its
	// publish (the live-write pause hook), and released once the mark is stored: a process that
	// keeps its rooms current shows the mark whatever that write does.
	t.Run("a mark placed by an editor on A is verified by a comment anchored through B", func(t *testing.T) {
		issueKey, artifactID := pair.b.createIssueIn(t, project, "Marked on A, anchored through B", "The quick brown fox")
		bob := pair.openSyncedEditor(t, pair.b, "bob", artifactID, "The quick brown fox\n")
		pair.b.armLiveWritePause(t, artifactID, docs.PauseAfterCommit)
		write := pair.b.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "brown", "red", "", false), asUser("carol"))
		pair.b.awaitLiveWriteParked(t, artifactID, docs.PauseAfterCommit)
		alice := pair.openSyncedEditor(t, pair.a, "alice", artifactID, "The quick red fox\n")
		markID := "mark-" + uniqueProjectKey(t, "M")
		head := storedHead(t, pair, artifactID)
		placeMark(t, alice, "alice", "quick", markID)
		pair.waitStoredPast(t, artifactID, head, "alice's mark")
		if !pair.b.releaseLiveWritePause(t, artifactID, docs.PauseAfterCommit) {
			t.Fatalf("the release found no write parked")
		}
		if answer := write.await(t, "carol's write through B, after its release", pair.b); answer.status != http.StatusOK {
			t.Fatalf("carol's write through B: %v", answer)
		}
		pair.requireConnected(t, artifactID, "bob on B", bob, "before the comment")
		body := fmt.Sprintf(`{"body":"why","anchor":{"artifact":%q,"mark_id":%q}}`, artifactID, markID)
		answer := pair.b.send(t, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", body, asUser("bob"))
		if answer.status != http.StatusCreated {
			t.Fatalf("bob's comment through B, anchored on the mark alice placed on A and the store holds, answered %v; want 201: B verified the mark against a room older than the store", answer)
		}
		var comment struct {
			Anchor struct {
				Quote string `json:"quote"`
			} `json:"anchor"`
		}
		if err := json.Unmarshal(answer.body, &comment); err != nil || comment.Anchor.Quote != "quick" {
			t.Fatalf("the comment's anchor quotes %q (%v), want %q: %v", comment.Anchor.Quote, err, "quick", answer)
		}
	})
}

// corruptHistory makes the document's stored history one update ygo cannot decode, at the head
// it stood at, so a rebuild has something to rebuild and no room's cursor moves.
func corruptHistory(t *testing.T, pair *dispatchPair, artifactID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pair.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the corruption: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		`delete from doc_checkpoints where artifact_id = $1`,
		`delete from doc_updates where artifact_id = $1 and version < (select max(version) from doc_updates where artifact_id = $1)`,
		`update doc_updates set update = decode('deadbeef', 'hex') where artifact_id = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, artifactID); err != nil {
			t.Fatalf("corrupt the document's history: %s: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the corruption: %v", err)
	}
}

func rebuildPath(artifactID string) string {
	return "/api/v1/artifacts/" + artifactID + "/rebuild"
}

// resumptionsByPeriod is every time either process's settlement resumption armed the document's
// owed settlement (the room-counters hook's settlement_resumptions), each named by its process,
// under the period of the pass that armed it, and how many of those times no logged pass places
// yet. The period comes from the processes' own records, not from this test's clock: each pass of
// the resumption logs, as it ends, the tick it was due at and the period that due falls in
// ("leased pass ran", duty.Exclusive), and a process's ticks are SettlementResumeInterval apart
// from its own start (duty.RunLeased), so an arming belongs to the logged pass of its process due
// at or before it and less than an interval before it. A pass due at a period's end arms just
// after the end, which a period read off the arming's own time would count in the next. An
// arming whose pass has not logged yet is unplaced until it does.
func (p *dispatchPair) resumptionsByPeriod(t *testing.T, artifactID string) (byPeriod map[time.Time][]string, unplaced int) {
	t.Helper()
	byPeriod = map[time.Time][]string{}
	for _, process := range []*dispatchProcess{p.a, p.b} {
		type pass struct{ due, period time.Time }
		var passes []pass
		for _, line := range process.wholeLogLines(leaseRan, "lease=settlement-resumption") {
			due, err := time.Parse(time.RFC3339Nano, logAttributeOf(t, line, "due"))
			if err != nil {
				t.Fatalf("read the due of a settlement-resumption pass: %v: %s", err, line)
			}
			period, err := time.Parse(time.RFC3339Nano, leasePeriodOf(t, line))
			if err != nil {
				t.Fatalf("read the period of a settlement-resumption pass: %v: %s", err, line)
			}
			passes = append(passes, pass{due: due, period: period})
		}
		for _, armed := range process.roomCounters(t, artifactID).SettlementResumptions {
			made := slices.IndexFunc(passes, func(made pass) bool {
				return !made.due.After(armed) && armed.Sub(made.due) < docs.SettlementResumeInterval
			})
			if made < 0 {
				unplaced++
				continue
			}
			period := passes[made].period
			byPeriod[period] = append(byPeriod[period], p.name(process)+" at "+armed.Format(time.RFC3339Nano))
		}
	}
	return byPeriod, unplaced
}

// Spec item 6: a rebuild, the settlement resumption, the conditional-edit queue and a write to a
// document no room holds behave across processes as on one. Every process here has a settle
// delay of a day (stagedPastTheRun), so no settlement runs inside the row: an owed settlement
// stays owed for the resumption to find, and no settlement loads a room.
func TestRebuildResumptionQueueAndColdWritesHoldAcrossProcesses(t *testing.T) {
	pair := newPair(t)
	pair.startSettling(t, nil, stagedPastTheRun, stagedPastTheRun)
	project := uniqueProjectKey(t, "MAIN")
	pair.a.createProject(t, project, "Maintenance across processes")

	t.Run("a rebuild through A refuses a document live on B", func(t *testing.T) {
		_, artifactID := pair.b.createIssueIn(t, project, "Live on B, rebuilt through A", "before")
		pair.openSyncedEditor(t, pair.b, "bob", artifactID, "before\n")
		corruptHistory(t, pair, artifactID)
		answer := pair.a.send(t, http.MethodPost, rebuildPath(artifactID), `{}`, asUser("alice"))
		if answer.status != http.StatusConflict || answer.code() != "DOCUMENT_LIVE" {
			t.Fatalf("a rebuild through A while bob's tab holds a room of the document on B answered %v; want 409 DOCUMENT_LIVE: A asked only its own rooms whether the document is live", answer)
		}
	})

	t.Run("two rebuilds at once through A and B: one runs and the other is refused", func(t *testing.T) {
		issueKey, artifactID := pair.a.createIssueIn(t, project, "Rebuilt twice", "before")
		if pair.a.holdsRoom(t, artifactID) || pair.b.holdsRoom(t, artifactID) {
			t.Fatalf("a process holds a room of the document, so a rebuild would be refused as live rather than meet the other rebuild")
		}
		corruptHistory(t, pair, artifactID)
		// Both rebuilds validate the history they loaded and then wait here, on the issue's row
		// (lockArtifactOwner), so each has loaded the corrupt history before either replaces it.
		gate := lockIssue(t, pair.database, issueKey)
		throughA := pair.a.requestInBackground(http.MethodPost, rebuildPath(artifactID), `{}`, asUser("alice"))
		throughB := pair.b.requestInBackground(http.MethodPost, rebuildPath(artifactID), `{}`, asUser("alice"))
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("A's output:\n%s\nB's output:\n%s", pair.a.Output.String(), pair.b.Output.String())
			}
		})
		// A settlement or the block-index pass waits on the issue's lock with the same statement;
		// a rebuild alone has read pg_stat_activity in its transaction before it waits (which
		// processes hold the document live), so that lock names the two rebuilds.
		waitForOwnerLockWaiters(t, pair.database, gate, "both rebuilds to wait on the issue's lock",
			"rebuilds, which hold a lock of pg_stat_activity", 2, func(w ownerLockWaiter) bool {
				return w.query == store.OwnerLockStatement && w.holds("pg_stat_activity")
			})
		if err := gate.Rollback(context.Background()); err != nil {
			t.Fatalf("release the issue's row: %v", err)
		}
		answers := []httpAnswer{
			throughA.await(t, "the rebuild through A", pair.a),
			throughB.await(t, "the rebuild through B", pair.b),
		}
		succeeded := 0
		for _, answer := range answers {
			if answer.status == http.StatusOK {
				succeeded++
			}
		}
		if succeeded != 1 || (answers[0].status != http.StatusConflict && answers[1].status != http.StatusConflict) {
			t.Fatalf("two rebuilds of one document at once answered %v through A and %v through B; want one 200 and one 409: the second replaced the history the first had just rebuilt, judged on a load from before it", answers[0], answers[1])
		}
	})

	t.Run("an owed settlement is resumed by one process per period", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Owed and resumed", "before")
		pair.a.call(t, http.MethodPost, editPath(artifactID), editBody(t, "before", "after", "", false), http.StatusOK)
		waitForSettlementOwed(t, pair.database, artifactID, true)
		if _, err := pair.database.Pool.Exec(context.Background(), `
			update doc_settlements_pending set marked_at = now() - interval '2 minutes' where artifact_id = $1
		`, artifactID); err != nil {
			t.Fatalf("age the document's pending settlement: %v", err)
		}
		// A period is a bucket of a pass's due time (duty.Exclusive, migration 0115), and the
		// document is owed from the moment it aged to the row's end: no settlement runs inside the
		// row. Each process's resumption ticks once a minute from its own start, so each meets
		// every period once and one of the two passes it; what this counts is how many of those
		// meetings armed the document, each placed in its pass's period by the processes' own
		// records (resumptionsByPeriod), which the resumption records as it arms the settlement,
		// so the first is waited for.
		period := docs.SettlementResumeInterval
		// A try reads how many armings resumptionsByPeriod placed, the periods it placed them in,
		// and how many no logged pass places yet; the earliest of those periods is the first.
		var first time.Time
		testwait.Until(t, "a resumption of the document's owed settlement, placed in the period of the pass that armed it",
			func() (string, bool) {
				byPeriod, unplaced := pair.resumptionsByPeriod(t, artifactID)
				placed := 0
				periods := make([]time.Time, 0, len(byPeriod))
				for served, armings := range byPeriod {
					placed += len(armings)
					periods = append(periods, served)
				}
				slices.SortFunc(periods, time.Time.Compare)
				reading := fmt.Sprintf("%d armings placed in periods %v, and %d that no logged pass places yet", placed, periods, unplaced)
				if len(periods) == 0 {
					return reading, false
				}
				first = periods[0]
				return reading, true
			})
		end := first.Add(period)
		t.Logf("counting the resumptions of the document in the period from %s to %s", first.Format(time.TimeOnly), end.Format(time.TimeOnly))
		// A second resumption inside the period could come only from a tick of either process due
		// in it, and each tick writes one line, ran or skipped (duty.Exclusive), whether it found
		// the period served or the lease held. Each process ticks once a period from its own start,
		// so two more of its lines than it had written when the first resumption was found take it
		// past its tick in that period, whether that tick had begun, ended or not yet come: its line
		// for a tick due in the next period is written by then.
		ticks := func(process *dispatchProcess) int {
			return len(process.wholeLogLines(leaseRan, "lease=settlement-resumption")) +
				len(process.wholeLogLines(leaseSkipped, "lease=settlement-resumption"))
		}
		fromA, fromB := ticks(pair.a), ticks(pair.b)
		testwait.Until(t, "each process's settlement-resumption tick past the period of the first resumption", func() ([2]int, bool) {
			a, b := ticks(pair.a), ticks(pair.b)
			return [2]int{a - fromA, b - fromB}, a >= fromA+2 && b >= fromB+2
		})
		byPeriod := testwait.Until(t, "every arming of the document's owed settlement to be placed in the period of its pass",
			func() (map[time.Time][]string, bool) {
				byPeriod, unplaced := pair.resumptionsByPeriod(t, artifactID)
				return byPeriod, unplaced == 0
			})
		for served, armings := range byPeriod {
			if len(armings) != 1 {
				t.Errorf("the processes' settlement resumptions armed the document's owed settlement %d times in the %v period from %s, want once across both processes:\n%s",
					len(armings), period, served.Format(time.TimeOnly), strings.Join(armings, "\n"))
			}
		}
	})

	// A process counts in the document's queue only while its room-sync listener's connection is
	// up (docs' roomSyncMembers): a listener redials when its ping times out, which a loaded
	// machine can cause, and while it is down the other process's admissions neither count its
	// queued edits nor keep its row of them (docs' conditionalEditQueueOthers), so they can admit
	// past the 32-deep bound and none of the 33 need answer 429. A redial is a LISTEN stood up
	// again (docs.RoomCounters.ListenStands), so the exact count, one 429, is judged only where
	// neither process's listener stood one up since the subtest began, as the file's redial-aware
	// waits do (awaitNotifiedCatchUp). Through a redial the product guarantees no 429 at all, and
	// the row holds it to the bound a redial cannot lift: at most one, and each one EDIT_QUEUE_FULL.
	t.Run("33 conditional edits across A and B: 32 queue and one answers 429 EDIT_QUEUE_FULL", func(t *testing.T) {
		_, artifactID := pair.a.createIssueIn(t, project, "Queued on both", "alpha")
		_, token := pair.a.textAndToken(t, artifactID)
		standsA, standsB := pair.a.roomCounters(t, artifactID).ListenStands, pair.b.roomCounters(t, artifactID).ListenStands
		redialed := func() string {
			nowA, nowB := pair.a.roomCounters(t, artifactID).ListenStands, pair.b.roomCounters(t, artifactID).ListenStands
			if nowA == standsA && nowB == standsB {
				return ""
			}
			return fmt.Sprintf("A's listener stood up LISTENs %d, then %d, and B's %d, then %d", standsA, nowA, standsB, nowB)
		}
		// The first edit parks after its first append, holding the document; every later
		// conditional edit of it waits behind it, on A in A's queue and on B in B's.
		pair.a.armLiveWritePause(t, artifactID, docs.PauseAfterFirstAppend)
		first := pair.a.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "alpha", "beta", token, false), asUser("alice"))
		pair.a.awaitLiveWriteParked(t, artifactID, docs.PauseAfterFirstAppend)
		const queued = 32
		later := make([]backgroundRequest, queued)
		for index := range later {
			process := pair.a
			if index%2 == 1 {
				process = pair.b
			}
			later[index] = process.requestInBackground(http.MethodPost, editPath(artifactID), editBody(t, "alpha", "gamma", token, false), asUser("alice"))
		}
		answered := make([]*httpAnswer, queued)
		refused := 0
		// Once every one of the 32 has answered, a 429 can no longer come: that state ends the
		// wait at once, with the failure the row is about, rather than at the wait's give-up. A
		// redial of either process's listener ends it too, since it can drop that process's
		// queue from the count.
		testwait.Until(t, "one of the 33 conditional edits in flight to answer 429 EDIT_QUEUE_FULL, or either process's room-sync listener to redial", func() (string, bool) {
			for index, request := range later {
				if answered[index] != nil {
					continue
				}
				select {
				case answer := <-request.answered:
					answered[index] = &answer
					if answer.status == http.StatusTooManyRequests {
						refused++
					}
				case err := <-request.failed:
					t.Fatalf("conditional edit %d: %v", index+2, err)
				default:
				}
			}
			byStatus, waiting := map[int]int{}, 0
			for _, answer := range answered {
				if answer == nil {
					waiting++
					continue
				}
				byStatus[answer.status]++
			}
			redial := redialed()
			listeners := redial
			if listeners == "" {
				listeners = fmt.Sprintf("neither process's room-sync listener redialed: A's stood up %d LISTENs and B's %d, as at the start", standsA, standsB)
			}
			reading := fmt.Sprintf("of the 32 edits queued behind the first, %d answered (count by status %v) and %d have not; %s",
				queued-waiting, byStatus, waiting, listeners)
			if refused > 0 {
				return reading, true
			}
			if redial != "" {
				t.Logf("no conditional edit has answered 429 yet, and a process's room-sync listener redialed, which drops its queue from the count: %s", redial)
				return reading, true
			}
			if waiting == 0 {
				t.Fatalf("33 conditional edits of one document were in flight at once - the first parked holding it, 16 queued behind it on A and 16 on B - and none answered 429 EDIT_QUEUE_FULL: each process held its own queue to 32, so the two together queued 33")
			}
			return reading, false
		})
		if !pair.a.releaseLiveWritePause(t, artifactID, docs.PauseAfterFirstAppend) {
			t.Fatalf("the release found no edit parked")
		}
		first.await(t, "the first conditional edit", pair.a)
		refused = 0
		for index, request := range later {
			answer := answered[index]
			if answer == nil {
				awaited := request.await(t, fmt.Sprintf("conditional edit %d", index+2), pair.a)
				answer = &awaited
			}
			if answer.status == http.StatusTooManyRequests {
				if answer.code() != "EDIT_QUEUE_FULL" {
					t.Errorf("conditional edit %d answered %v, want the 429 to be EDIT_QUEUE_FULL", index+2, answer)
				}
				refused++
			}
		}
		// A redial can take a process's queued edits out of the sum an admission reads, and adds
		// none to it: each row the sum reads is a count its process admitted at some moment - an
		// admission writes its count under the document's queue lock, and a release only ever
		// lowers it (docs' admitConditionalEdit, releaseConditionalEdit) - and the admitting
		// process adds its own queue as it holds it. So an edit is refused only once 32 of the 33
		// have been admitted, which leaves at most one to refuse, with a redial or without.
		switch redial := redialed(); {
		case redial == "" && refused != 1:
			t.Errorf("%d of the 33 conditional edits answered 429 EDIT_QUEUE_FULL, want exactly one: 32 may queue across both processes", refused)
		case refused > 1:
			t.Errorf("%d of the 33 conditional edits answered 429 EDIT_QUEUE_FULL with a process's room-sync listener redialed while they queued, want at most one: a redial takes a process's queue out of the count and adds none to it, so an edit is refused only once 32 of the 33 were admitted: %s", refused, redial)
		case redial != "":
			t.Logf("%d of the 33 conditional edits answered 429 EDIT_QUEUE_FULL, within the one the bound allows, with a process's room-sync listener redialed while they queued, which can take that process's queue out of the count and the refusal with it: %s", refused, redial)
		}
	})

	t.Run("an agent write through A, which holds no room, leaves none resident there", func(t *testing.T) {
		_, artifactID := pair.b.createIssueIn(t, project, "Written cold", "before")
		if pair.a.holdsRoom(t, artifactID) {
			t.Fatalf("A holds a room of the document before the write, so the write would not be cold")
		}
		if answer := pair.a.send(t, http.MethodPost, editPath(artifactID), editBody(t, "before", "after", "", true), asAgent()); answer.status != http.StatusOK {
			t.Fatalf("the agent's write through A: %v", answer)
		}
		if pair.a.holdsRoom(t, artifactID) {
			t.Fatalf("an agent's write through A, which held no room of the document, left a room of it resident on A with no reader in it: the write's publish loaded the room from the store (audit finding 11); A's rooms: %q", pair.a.residentRooms(t))
		}
	})
}

// Plan D10: the answer to a document socket's upgrade names the process that serves it
// (X-Dispatch-Process, the pod name in production), which is how a two-pod production check
// records where each of its sockets landed. Both processes run on this host, and the binary
// reads its name from the host (no setting names it otherwise), so each must answer this host's
// name.
func TestEachDocumentSocketNamesItsServingProcess(t *testing.T) {
	pair := startPair(t, nil)
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		t.Fatalf("read this host's name: %q %v", hostname, err)
	}
	project := uniqueProjectKey(t, "HOST")
	pair.a.createProject(t, project, "Serving processes")
	_, artifactID := pair.a.createIssueIn(t, project, "Served by both", "x")
	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		editor := process.openEditorAs(t, "alice", artifactID, "x\n")
		switch served := servingProcess(editor); served {
		case hostname:
		case "":
			t.Errorf("the document socket's upgrade on %s carries no X-Dispatch-Process header; want the serving process's host name, %q. Upgrade header: %v",
				pair.name(process), hostname, editor.UpgradeHeader)
		default:
			t.Errorf("the document socket's upgrade on %s names %q as its serving process, want %q", pair.name(process), served, hostname)
		}
	}
}

// ───── deliveries a request or a process left behind ───────────────────────

// listenerRoute is one of the Envoy listener routes Dispatch calls, which a row can hold.
type listenerRoute string

const (
	listenerRegistry listenerRoute = "GET /v1/registry/{session_id}"
	listenerSessions listenerRoute = "GET /v1/sessions"
	listenerSend     listenerRoute = "POST /v1/messages/send"
)

// listenerSession is a live session's record as the listener answers it, on its registry read and
// on its live list alike.
type listenerSession struct {
	SessionID    string   `json:"session_id"`
	Title        string   `json:"title"`
	Roles        []string `json:"roles"`
	Capabilities []string `json:"capabilities"`
}

// listenerSendRecord is one send the fake listener answered: the key and the session it was sent
// under, its urgency, and whether the listener answered it as a repeat of a send it had already
// answered.
type listenerSendRecord struct {
	Key, Recipient, Urgency string
	Duplicate               bool
}

// heldRequest is one request a hold keeps unanswered: its route, what it names - the session id a
// registry read asks about, the idempotency key a send carries - and a channel closed once its
// caller has given up on it, a cancelled call or a killed process alike.
type heldRequest struct {
	route   listenerRoute
	subject string
	gone    <-chan struct{}
}

// listenerHold keeps every request on its route unanswered from when it is taken until Release,
// and reports each one on arrived as it comes in.
type listenerHold struct {
	arrived chan heldRequest
	release chan struct{}
	once    sync.Once
}

// Release answers every request the hold keeps and lets every later one through.
func (h *listenerHold) Release() { h.once.Do(func() { close(h.release) }) }

func (h *listenerHold) released() bool {
	select {
	case <-h.release:
		return true
	default:
		return false
	}
}

// next waits for the next request the hold keeps, naming what is awaited.
func (h *listenerHold) next(t *testing.T, what string) heldRequest {
	t.Helper()
	return testwait.Receive(t, what+" reaching the listener", h.arrived)
}

// requireAbandoned waits for each held request's caller to have gone away, as every request a
// killed process made has, so a release that follows answers none of them: a send the listener
// answers to a dead process would be one the row did not mean to land.
func requireAbandoned(t *testing.T, held ...heldRequest) {
	t.Helper()
	for _, request := range held {
		testwait.Receive(t, fmt.Sprintf("the caller of %s %s to go away", request.route, request.subject), request.gone)
	}
}

// fakeListener is the Envoy listener both processes of a pair deliver through (ENVOY_URL): an
// httptest server in the test process that knows a set of live sessions (a row may replace it,
// setSessions), serves the three routes a delivery calls (the registry read, the live list and
// the send), counts every live-list read it answers, and records every send it answers. A send
// whose key and recipient it has already answered is answered as a duplicate, as the real
// listener answers a repeat inside contracts.DeliveryDuplicateWindow, and recorded as one. A row
// holds a route (hold) to keep a request unanswered while it acts on the processes;
// a held request whose caller goes away is never answered and never recorded, so a send a killed
// process made is not one that reached the session. Every hold is released when the test ends,
// before the server closes, since a close waits for every request still in flight.
//
// Each route's answer is decided in serve, in one place per route. A row that needs the listener
// to misbehave for a while starts a take-over (takeOver): for its first phase the listener refuses
// every connection, for its second it answers 503 "service starting" on every route as a starting
// listener does, for its third the registry read is answered with the session's record while the
// send still meets that 503, and for its fourth it holds no session yet - the registry read and
// the send answer "no live session", the live list is empty - and after that it answers as before.
// Each phase ends on a fact the listener sees, never on a span of the clock (listenerTakeOver).
type fakeListener struct {
	server *httptest.Server

	mu       sync.Mutex
	sessions []listenerSession
	// sessionLists counts the GET /v1/sessions reads the listener answered.
	sessionLists int
	answered     []listenerSendRecord
	holds        map[listenerRoute]*listenerHold
	// running is the take-over the listener is running, nil when there is none.
	running *listenerTakeOver
	// takeOverReads counts, by session id, the registry reads a take-over answered other than
	// as the session's record.
	takeOverReads map[string]int
	// gatedSends counts, by idempotency key, the sends a take-over answered 503 "service
	// starting".
	gatedSends map[string]int
}

// startFakeListener serves the sessions on loopback until the test ends.
func startFakeListener(t *testing.T, sessions ...listenerSession) *fakeListener {
	t.Helper()
	f := &fakeListener{
		sessions: sessions, holds: map[listenerRoute]*listenerHold{},
		takeOverReads: map[string]int{}, gatedSends: map[string]int{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.mu.Lock()
		for _, hold := range f.holds {
			hold.Release()
		}
		f.mu.Unlock()
		f.server.Close()
	})
	return f
}

// processEnv is what a Dispatch process delivering through this listener is started with: the
// listener's URL. Nothing here sets the delivery sweep's period: it is a pass of the maintenance
// loop and runs at production's api.MaintenanceSweepInterval (a minute). A row that waits for
// that pass takes quickSweepEnv instead, and a row that no pass may serve takes unsweptEnv.
func (f *fakeListener) processEnv() map[string]string {
	return map[string]string{"ENVOY_URL": f.server.URL}
}

// quickMaintenanceSweep is the maintenance loop's period for a row that waits for the delivery
// sweep (DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL), in place of the minute a deployment runs it
// at. What such a row is about is that the surviving process sends what the killed one left owed,
// once, under the attempt and key the killed process used; how long its loop sleeps between
// passes is not, and at production's period one row waits out two ticks - two minutes - for a
// hand-over the pass itself makes in milliseconds.
const quickMaintenanceSweep = 2 * time.Second

// quickSweepEnv is processEnv with that period, for a row whose claim the delivery sweep settles.
func (f *fakeListener) quickSweepEnv() map[string]string {
	env := f.processEnv()
	env["DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL"] = quickMaintenanceSweep.String()
	return env
}

// unsweptEnv is processEnv with the maintenance loop's period staged past the run
// (DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL at stagedPastTheRun), for a row whose attempts only
// their own requests' senders may deliver. An attempt not settled within its claim's minute is
// owed to the delivery sweep too (api/delivery_sweep.go, owedMessageAttempt), so at production's
// period a pass a minute into a slow row would send it a second time; staged past the run, no
// pass comes inside it.
func (f *fakeListener) unsweptEnv() map[string]string {
	env := f.processEnv()
	env["DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL"] = stagedPastTheRun.String()
	return env
}

// setSessions replaces the sessions the listener knows: from now on its registry read, its live
// list and its send answer as though exactly these were live.
func (f *fakeListener) setSessions(sessions ...listenerSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = sessions
}

// sessionListings is how many GET /v1/sessions reads the listener has answered.
func (f *fakeListener) sessionListings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionLists
}

// hold keeps every request on route unanswered from now until the returned hold is released. A
// hold the route already had is released first, so nothing it keeps is left waiting for the end
// of the test.
func (f *fakeListener) hold(route listenerRoute) *listenerHold {
	hold := &listenerHold{arrived: make(chan heldRequest, 64), release: make(chan struct{})}
	f.mu.Lock()
	defer f.mu.Unlock()
	if previous := f.holds[route]; previous != nil {
		previous.Release()
	}
	f.holds[route] = hold
	return hold
}

// sendsUnder is every send the listener answered under key, in the order it answered them.
func (f *fakeListener) sendsUnder(key string) []listenerSendRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sends []listenerSendRecord
	for _, send := range f.answered {
		if send.Key == key {
			sends = append(sends, send)
		}
	}
	return sends
}

// listenerTakeOverPhase is what a take-over has the fake listener do with a request right now.
type listenerTakeOverPhase int

const (
	takeOverOver listenerTakeOverPhase = iota
	takeOverRefusing
	takeOverStarting
	takeOverHandingOver
	takeOverUnknown
)

// listenerTakeOver is a take-over's plan and where it stands. Each phase ends on a fact the
// listener sees, never on a span of the clock, so a loaded machine that spaces a sender's tries
// further apart meets every phase all the same: refusing ends once the listener has dropped
// refused registry reads (a negative count refuses until endTakeOver), starting once it has
// answered starting registry reads 503, handing-over once gated distinct send keys have met the
// starting gate, and unknown once it has answered unknown registry reads with no session. Every
// try of a sender reads the registry first, so a registry read is one try. A phase of no count is
// passed over, and the take-over is over once its last phase ends.
type listenerTakeOver struct {
	refused, starting, gated, unknown int
	phase                             listenerTakeOverPhase
	// answered is how many registry reads the phase has answered; gatedKeys the send keys the
	// handing-over phase has met.
	answered  int
	gatedKeys map[string]bool
}

// count is how many facts phase lasts for, 0 for a phase the take-over passes over.
func (o *listenerTakeOver) count(phase listenerTakeOverPhase) int {
	switch phase {
	case takeOverRefusing:
		return o.refused
	case takeOverStarting:
		return o.starting
	case takeOverHandingOver:
		return o.gated
	case takeOverUnknown:
		return o.unknown
	}
	return 0
}

// next moves the take-over to the first phase after its current one that lasts for anything, and
// reports whether there is one: false is a take-over that is over.
func (o *listenerTakeOver) next() bool {
	for o.phase++; o.phase <= takeOverUnknown; o.phase++ {
		if o.count(o.phase) != 0 {
			o.answered = 0
			return true
		}
	}
	return false
}

// takeOver begins a take-over now: the listener refuses refused registry reads (a negative count
// until endTakeOver), then answers starting registry reads 503 on every route, then answers 503 on
// the send alone until gated distinct send keys have met it, then holds no session for unknown
// registry reads.
//
// The handing-over phase is a replacement in which both listeners take requests behind the one
// address: the registry read reaches the outgoing listener, which still holds the session, and
// the send reaches its successor, still behind its starting gate (cmd/listener/starting_gate.go
// startingGate). It is the one phase in which a send itself meets that gate, because every try
// reads the registry before it sends.
func (f *fakeListener) takeOver(refused, starting, gated, unknown int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	takeOver := &listenerTakeOver{refused: refused, starting: starting, gated: gated, unknown: unknown, gatedKeys: map[string]bool{}}
	if takeOver.next() {
		f.running = takeOver
	}
}

// endTakeOver ends the take-over at once, whatever phase it is in.
func (f *fakeListener) endTakeOver() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = nil
}

// takeOverPhaseOf is the phase the take-over answers r in, a send under key, and counts r against
// that phase: the request that completes a phase's count is answered in that phase, and the next
// one meets the phase after it.
func (f *fakeListener) takeOverPhaseOf(r *http.Request, key string) listenerTakeOverPhase {
	f.mu.Lock()
	defer f.mu.Unlock()
	takeOver := f.running
	if takeOver == nil {
		return takeOverOver
	}
	phase := takeOver.phase
	ended := false
	switch {
	case phase == takeOverHandingOver:
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send" {
			takeOver.gatedKeys[key] = true
			ended = len(takeOver.gatedKeys) >= takeOver.gated
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
		takeOver.answered++
		ended = takeOver.count(phase) > 0 && takeOver.answered >= takeOver.count(phase)
	}
	if ended && !takeOver.next() {
		f.running = nil
	}
	return phase
}

// readsDuringTakeOver is how many registry reads of sessionID a take-over answered other than as
// the session's record.
func (f *fakeListener) readsDuringTakeOver(sessionID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.takeOverReads[sessionID]
}

// sendsGatedUnder is how many sends under key a take-over answered 503 "service starting".
func (f *fakeListener) sendsGatedUnder(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gatedSends[key]
}

// answerStarting answers as the real listener's starting gate does (cmd/listener/starting_gate.go
// startingGate).
func answerStarting(w http.ResponseWriter) {
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "service starting"})
}

func (f *fakeListener) serve(w http.ResponseWriter, r *http.Request) {
	// The whole body is read before anything waits, so the server sees the request end and
	// notices its caller going away while the request is held.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var sent struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send" {
		_ = json.Unmarshal(body, &sent)
	}
	phase := f.takeOverPhaseOf(r, sent.IdempotencyKey)
	if phase != takeOverOver && phase != takeOverHandingOver &&
		r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/") {
		f.mu.Lock()
		f.takeOverReads[strings.TrimPrefix(r.URL.Path, "/v1/registry/")]++
		f.mu.Unlock()
	}
	switch phase {
	case takeOverRefusing:
		// The connection is dropped unanswered, which the client meets as a listener that is not
		// there: what a deleted listener pod, or its successor not yet listening, gives.
		if connection, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = connection.Close()
		}
		return
	case takeOverStarting:
		answerStarting(w)
		return
	case takeOverHandingOver:
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send" {
			f.mu.Lock()
			f.gatedSends[sent.IdempotencyKey]++
			f.mu.Unlock()
			answerStarting(w)
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		if !f.await(listenerRegistry, sessionID, r) {
			return
		}
		session, ok := f.session(sessionID)
		if !ok || phase == takeOverUnknown {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no live session " + sessionID})
			return
		}
		_ = json.NewEncoder(w).Encode(session)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		if !f.await(listenerSessions, "", r) {
			return
		}
		f.mu.Lock()
		f.sessionLists++
		sessions := f.sessions
		f.mu.Unlock()
		if phase == takeOverUnknown || sessions == nil {
			sessions = []listenerSession{}
		}
		_ = json.NewEncoder(w).Encode(sessions)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
		var send struct {
			TargetSession  string `json:"target_session"`
			IdempotencyKey string `json:"idempotency_key"`
			Urgency        string `json:"urgency"`
		}
		if err := json.Unmarshal(body, &send); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !f.await(listenerSend, send.IdempotencyKey, r) {
			return
		}
		if _, ok := f.session(send.TargetSession); !ok || phase == takeOverUnknown {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no live session " + send.TargetSession})
			return
		}
		f.mu.Lock()
		duplicate := false
		for _, prior := range f.answered {
			duplicate = duplicate || (prior.Key == send.IdempotencyKey && prior.Recipient == send.TargetSession)
		}
		f.answered = append(f.answered, listenerSendRecord{
			Key: send.IdempotencyKey, Recipient: send.TargetSession, Urgency: send.Urgency, Duplicate: duplicate,
		})
		envelope := fmt.Sprintf("envelope-%d", len(f.answered))
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"event_id": envelope, "recipient": send.TargetSession, "duplicate": duplicate,
		})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// await keeps r unanswered while route is held, and reports whether it is still to be answered:
// false when its caller went away first.
func (f *fakeListener) await(route listenerRoute, subject string, r *http.Request) bool {
	f.mu.Lock()
	hold := f.holds[route]
	f.mu.Unlock()
	if hold == nil || hold.released() {
		return true
	}
	hold.arrived <- heldRequest{route: route, subject: subject, gone: r.Context().Done()}
	select {
	case <-hold.release:
		return r.Context().Err() == nil
	case <-r.Context().Done():
		return false
	}
}

func (f *fakeListener) session(sessionID string) (listenerSession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, session := range f.sessions {
		if session.SessionID == sessionID {
			return session, true
		}
	}
	return listenerSession{}, false
}

// resumeSession is the one live session the delivery rows send to; it takes every mode.
var resumeSession = listenerSession{
	SessionID: "resume-session", Title: "resume", Roles: []string{}, Capabilities: []string{"btw", "aside", "steer"},
}

// deliveryUnderTest is one delivery a row follows: what a failure calls it, the attempt table its
// rows are in, and the idempotency key its send carries, which begins with the id of its message
// or comment.
type deliveryUnderTest struct {
	what, table, key string
}

func messageDelivery(what, key string) deliveryUnderTest {
	return deliveryUnderTest{what: what, table: "message_deliveries", key: key}
}

func mentionDelivery(what, key string) deliveryUnderTest {
	return deliveryUnderTest{what: what, table: "comment_deliveries", key: key}
}

// owner is the column naming the message or comment, the receipt event type and the payload
// field naming it there, and its id.
func (d deliveryUnderTest) owner() (column, receipt, field, id string) {
	id, _, _ = strings.Cut(d.key, ":")
	if d.table == "message_deliveries" {
		return "message_id", "message.delivery", "message_id", id
	}
	return "comment_id", "comment.delivery", "comment_id", id
}

// attempts is every attempt of the delivery's message or comment, as "<attempt>:<state>".
func (d deliveryUnderTest) attempts(t *testing.T, database *store.Store) []string {
	t.Helper()
	column, _, _, id := d.owner()
	rows, err := database.Pool.Query(context.Background(),
		`select attempt::text || ':' || state from `+d.table+` where `+column+` = $1 order by attempt`, id)
	if err != nil {
		t.Fatalf("read the attempts of %s: %v", d.what, err)
	}
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the attempts of %s: %v", d.what, err)
	}
	return states
}

// receipts is how many delivery receipts the events hold for attempt 1 of the delivery.
func (d deliveryUnderTest) receipts(t *testing.T, database *store.Store) int {
	t.Helper()
	_, receipt, field, id := d.owner()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where type = $1 and payload ->> $2 = $3 and (payload ->> 'attempt')::int = 1
	`, receipt, field, id).Scan(&count); err != nil {
		t.Fatalf("count the receipts of %s: %v", d.what, err)
	}
	return count
}

// requireDeliveredOnce waits through the shared wait for every delivery to settle and requires
// each to have reached its session exactly once under its own key: attempt 1 alone, sent, with
// one receipt, and one send the listener answered as new. A delivery that never settles fails the
// row at the wait's end, naming its attempts; one that settles otherwise is reported with both
// processes' output, and the row goes on to the next.
func requireDeliveredOnce(t *testing.T, pair *dispatchPair, listener *fakeListener, deliveries ...deliveryUnderTest) {
	t.Helper()
	for _, delivery := range deliveries {
		states := testwait.Until(t, delivery.what+" to settle", func() ([]string, bool) {
			states := delivery.attempts(t, pair.database)
			settled := len(states) > 0 && !slices.ContainsFunc(states, func(state string) bool {
				return strings.HasSuffix(state, ":pending")
			})
			return states, settled
		})
		if !slices.Equal(states, []string{"1:sent"}) {
			t.Errorf("%s reads %v once it settled; want attempt 1 alone, sent.\nA:\n%s\nB:\n%s",
				delivery.what, states, pair.a.Output.String(), pair.b.Output.String())
			continue
		}
		if sends := listener.sendsUnder(delivery.key); len(sends) != 1 || sends[0].Duplicate || sends[0].Recipient != resumeSession.SessionID {
			t.Errorf("%s: the listener answered %+v under %s; want one new send to %s",
				delivery.what, sends, delivery.key, resumeSession.SessionID)
		}
		if receipts := delivery.receipts(t, pair.database); receipts != 1 {
			t.Errorf("%s: attempt 1 has %d delivery receipts, want one", delivery.what, receipts)
		}
	}
}

// sendInBackground sends body to path on p without waiting for the answer, for a request the row
// expects never to answer: the process is killed while it is held.
func (p *dispatchProcess) sendInBackground(method, path, body string) {
	go func() { _, _ = p.trySend(method, path, body, asUser(dispatchTestLogin)) }()
}

// backDatePendingAttempts moves every pending attempt's claim and creation two minutes back, past
// both ages the delivery sweep judges an attempt owed by (api/delivery_sweep.go,
// owedMessageAttempt): the one-minute claim lease, and the minute a row must have been open for,
// so a row does not wait real minutes for its attempts to be owed.
func backDatePendingAttempts(t *testing.T, database *store.Store) {
	t.Helper()
	for _, table := range []string{"message_deliveries", "comment_deliveries"} {
		if _, err := database.Pool.Exec(context.Background(), `
			update `+table+`
			set claimed_at = claimed_at - interval '2 minutes', created_at = created_at - interval '2 minutes'
			where state = 'pending'
		`); err != nil {
			t.Fatalf("back-date the pending attempts of %s: %v", table, err)
		}
	}
}

// A process killed while it delivers leaves its attempts pending: a direct message and a comment
// mention whose own request was sending them, and a broadcast recipient a background worker was
// sending. The other process's delivery sweep sends each one once its claim has lapsed, under the
// attempt and the idempotency key the killed process used, so the session gets each once. The
// subtest kills the process earlier, after the message is stored and before anything is resolved
// or claimed: the attempt the storing transaction wrote is what the survivor sends.
func TestADeliveryAKilledProcessLeftPendingIsSentByTheOther(t *testing.T) {
	t.Run("killed while sending", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		sends := listener.hold(listenerSend)
		pair := startPair(t, listener.quickSweepEnv())
		project := uniqueProjectKey(t, "RSM")
		pair.a.createProject(t, project, "Resumed deliveries")
		issue, _ := pair.a.createIssueIn(t, project, "Mentioned while sending", "spec")

		pair.a.sendInBackground(http.MethodPost, "/api/v1/agents/"+resumeSession.SessionID+"/messages",
			`{"body":"A direct message","delivery":"steer"}`)
		direct := sends.next(t, "the direct message's send")
		pair.a.sendInBackground(http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			fmt.Sprintf(`{"body":"A mention","mentions":[{"target":"session:%s"}]}`, resumeSession.SessionID))
		mention := sends.next(t, "the mention's send")
		pair.a.call(t, http.MethodPost, "/api/v1/broadcasts", fmt.Sprintf(
			`{"body":"A broadcast","delivery":"steer","session_ids":[%q]}`,
			resumeSession.SessionID), http.StatusCreated)
		broadcast := sends.next(t, "the broadcast recipient's send")

		pair.a.kill(t)
		requireAbandoned(t, direct, mention, broadcast)
		sends.Release()
		backDatePendingAttempts(t, pair.database)
		requireDeliveredOnce(t, pair, listener,
			messageDelivery("the direct message", direct.subject),
			mentionDelivery("the mention", mention.subject),
			messageDelivery("the broadcast recipient's message", broadcast.subject),
		)
	})

	t.Run("killed before the claim", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		lookups := listener.hold(listenerRegistry)
		pair := startPair(t, listener.quickSweepEnv())
		pair.a.sendInBackground(http.MethodPost, "/api/v1/agents/"+resumeSession.SessionID+"/messages",
			`{"body":"Stored, then killed","delivery":"steer"}`)
		lookup := lookups.next(t, "the direct message's registry read")
		pair.a.kill(t)
		requireAbandoned(t, lookup)
		lookups.Release()
		var messageID string
		if err := pair.database.Pool.QueryRow(context.Background(),
			`select id::text from messages where target = $1`, "session:"+resumeSession.SessionID,
		).Scan(&messageID); err != nil {
			t.Fatalf("read the stored message: %v", err)
		}
		backDatePendingAttempts(t, pair.database)
		requireDeliveredOnce(t, pair, listener,
			messageDelivery("the message stored before the kill", messageID+":steer"))

	})
}

// cancelGrace is how long cancelOnceHeld gives Dispatch to act on a cancelled request before the
// listener answers the send that request made, when Dispatch does not abandon that send itself:
// exactly dispatchenvoy.CallWindow, the longest a deployment's send is held, so a send cut anywhere
// inside that window by its request's end is cut within the grace. The send itself runs on the
// test's window (endsAtTheGiveUp, which the row's processes are started with), so the
// client's own window does not end it during the grace.
const cancelGrace = dispatchenvoy.CallWindow

// cancelOnceHeld sends body to path on p with a request the test cancels as soon as the listener
// holds the send it made, which is after its commit, and waits for the client to return. The send
// is then answered once Dispatch has had the cancellation: at once if Dispatch gave up on the send
// with its request, after cancelGrace if it did not. It answers the send's idempotency key.
func cancelOnceHeld(t *testing.T, p *dispatchProcess, listener *fakeListener, path, body string) string {
	t.Helper()
	sends := listener.hold(listenerSend)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
		}
	}()
	held := sends.next(t, "the send of POST "+path)
	cancel()
	testwait.Receive(t, "POST "+path+" to return once its request was cancelled", returned)
	testwait.Quiet(t, "the held send's caller", held.gone, cancelGrace,
		"the row is about a send the listener keeps unanswered while its caller goes: the hold records nothing "+
			"a test can read, the request is this process's own but its cancellation travels a real socket so "+
			"synctest does not reach it, and releasing the hold is the later action the row takes next; the send "+
			"runs on the test's window, and the span is the production CallWindow, past which a deployment's send "+
			"would have ended by itself, so a cut inside it is one the span sees")
	sends.Release()
	return held.subject
}

// A request cut off after its commit - a closed tab, a front door's pod roll - must not take the
// send it was making with it: the direct message and the mention each reach their session once
// and read sent, with no Retry. Nothing back-dates an attempt here, and no delivery sweep runs
// inside the row (unsweptEnv), so only the request's own send can deliver them.
func TestASendWhoseRequestIsCancelledAfterItsCommitReachesItsRecipient(t *testing.T) {
	listener := startFakeListener(t, resumeSession)
	env := listener.unsweptEnv()
	env["DISPATCH_TEST_LISTENER_CALL_DEADLINE"] = endsAtTheGiveUp(t)
	pair := startPair(t, env)
	project := uniqueProjectKey(t, "CNL")
	pair.a.createProject(t, project, "Cancelled requests")
	issue, _ := pair.a.createIssueIn(t, project, "Mentioned, then cut off", "spec")

	direct := cancelOnceHeld(t, pair.a, listener, "/api/v1/agents/"+resumeSession.SessionID+"/messages",
		`{"body":"Cut off after its commit","delivery":"steer"}`)
	mention := cancelOnceHeld(t, pair.a, listener, "/api/v1/issues/"+issue+"/comments",
		fmt.Sprintf(`{"body":"A mention cut off after its commit","mentions":[{"target":"session:%s"}]}`, resumeSession.SessionID))
	requireDeliveredOnce(t, pair, listener,
		messageDelivery("the direct message", direct),
		mentionDelivery("the mention", mention))
}

// takeOverDeliveries waits through the shared wait for the direct message and the mentioning
// comment a row sent, by their bodies, to be stored, and answers each as the delivery it follows.
// A try that finds no row of the body reads how many rows the table holds, which a give-up names.
func takeOverDeliveries(t *testing.T, database *store.Store, messageBody, commentBody string) (direct, mention deliveryUnderTest) {
	t.Helper()
	storedID := func(table, body string) string {
		var id string
		testwait.Until(t, fmt.Sprintf("%q to be stored in %s", body, table), func() (string, bool) {
			var stored *string
			var rows int
			if err := database.Pool.QueryRow(context.Background(),
				`select (select id::text from `+table+` where body = $1 limit 1), (select count(*) from `+table+`)`,
				body).Scan(&stored, &rows); err != nil {
				t.Fatalf("read the stored %s %q: %v", table, body, err)
			}
			if stored == nil {
				return fmt.Sprintf("none of the %d rows of %s holds the body", rows, table), false
			}
			id = *stored
			return "stored as " + id, true
		})
		return id
	}
	direct = messageDelivery("the direct message", storedID("messages", messageBody)+":steer")
	mention = mentionDelivery("the mention",
		storedID("comments", commentBody)+":session:"+resumeSession.SessionID+":steer")
	return direct, mention
}

// sendDuringTakeOver sends, on p, a direct message to the live session and a comment mentioning
// it, each without waiting for its answer, and returns the two deliveries once both are stored.
func sendDuringTakeOver(t *testing.T, p *dispatchProcess, database *store.Store, issue, what string) (direct, mention deliveryUnderTest) {
	t.Helper()
	messageBody, commentBody := "A direct message "+what, "A mention "+what
	p.sendInBackground(http.MethodPost, "/api/v1/agents/"+resumeSession.SessionID+"/messages",
		fmt.Sprintf(`{"body":%q,"delivery":"steer"}`, messageBody))
	p.sendInBackground(http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		fmt.Sprintf(`{"body":%q,"mentions":[{"target":"session:%s"}]}`, commentBody, resumeSession.SessionID))
	return takeOverDeliveries(t, database, messageBody, commentBody)
}

// requireWaitingOutTheTakeOver waits through the shared wait for both deliveries' senders to be
// waiting out the take-over: each attempt is still pending under a claim, and the session has
// been read at least five times during the take-over - the two first resolutions and at least
// three tries after them.
func requireWaitingOutTheTakeOver(t *testing.T, database *store.Store, listener *fakeListener, deliveries ...deliveryUnderTest) {
	t.Helper()
	claimed := func(delivery deliveryUnderTest) bool {
		column, _, _, id := delivery.owner()
		var held bool
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) = 1 and bool_and(state = 'pending' and claimed_at is not null)
			from `+delivery.table+` where `+column+` = $1
		`, id).Scan(&held); err != nil {
			t.Fatalf("read the attempts of %s: %v", delivery.what, err)
		}
		return held
	}
	testwait.Until(t, "both deliveries' senders to be waiting out the take-over", func() (string, bool) {
		reads := listener.readsDuringTakeOver(resumeSession.SessionID)
		waiting := reads >= 5
		for _, delivery := range deliveries {
			waiting = waiting && claimed(delivery)
		}
		return fmt.Sprintf("%d reads of the session during the take-over, every attempt pending under its claim: %t", reads, waiting), waiting
	})
}

// quickTakeoverWindow is how long a sender keeps trying a listener being replaced in the subtest
// that only waits that window out (DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW), in place of the
// twelve seconds a deployment runs (api's listenerTakeoverWindow).
const quickTakeoverWindow = time.Second

// heldTakeoverWindow is how long a sender keeps trying a listener being replaced in the subtests
// that drive a take-over (DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW): what is left, as the row
// starts its processes, until the test's own waits give up (testwait.GiveUpAt), so a sender tries
// until the take-over the fake runs has ended, however a loaded machine spaces its tries, and
// gives up no sooner than the test would. Each sender counts the window from its own first try,
// which comes after this read, so its window ends at or after the give-up. The setting is a
// duration, and a duration cannot say "none", so under -timeout 0, whose waits have no end, the
// window is a day (stagedPastTheRun) in place of none. The twelve seconds a deployment runs (api's
// listenerTakeoverWindow) are held where their timer is reachable, on the server's fake clock:
// api.TestAListenerTakeOverIsWaitedOutAndARefusalSettlesAtOnce and
// api.TestAMessageToASessionNoStoreHoldsIsQueuedOneTakeOverWindowAfterItsFirstTry.
func heldTakeoverWindow(t *testing.T) time.Duration {
	if end, bounded := testwait.GiveUpAt(t); bounded {
		return time.Until(end)
	}
	return stagedPastTheRun
}

// While the active listener is replaced, its successor refuses connections, then answers 503
// "service starting" - on every route, then on the send alone while the outgoing listener still
// answers the registry read - then does not yet hold the session, for a few seconds. A direct
// message and a mention sent then wait it out under the attempt each holds and reach the session
// once it answers: attempt 1 sent, one receipt, one send. A message to a session the listener
// never knows waits the take-over window out as well, and is then queued rather than failed "no
// live session": since R5 (dispatch://INFRA-3740), Dispatch queues it on that session's own
// subject, so a released Legion role that comes back under the id reads it from its consumer.
// And a process killed while it waits leaves its attempts pending under the claim it renewed
// last, which the other process's delivery sweep sends.
func TestASendDuringAListenerTakeOverReachesItsSession(t *testing.T) {
	t.Run("sent during the take-over", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		// No attempt here is back-dated, and no delivery sweep runs inside the row (unsweptEnv),
		// so only each request's own sender can deliver.
		env := listener.unsweptEnv()
		env["DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW"] = heldTakeoverWindow(t).String()
		pair := startPair(t, env)
		project := uniqueProjectKey(t, "TKO")
		pair.a.createProject(t, project, "Listener take-over")
		issue, _ := pair.a.createIssueIn(t, project, "Mentioned during a take-over", "spec")

		// A sender tries at once, then 0.25, 0.75 and 1.75 s after its first try, and two seconds
		// apart after that (takeoverRetry), each try reading the registry first. Each phase of the
		// take-over ends on what the listener saw, not on the clock (listenerTakeOver): two
		// registry reads refused, two answered 503, the send of each of the two deliveries met at
		// the starting gate, and two registry reads answered with no session. So each sender meets
		// the gate however a loaded machine spaces its tries, and the take-over ends inside
		// heldTakeoverWindow.
		listener.takeOver(2, 2, 2, 2)
		direct, mention := sendDuringTakeOver(t, pair.a, pair.database, issue, "sent during a take-over")
		requireDeliveredOnce(t, pair, listener, direct, mention)
		for _, delivery := range []deliveryUnderTest{direct, mention} {
			if gated := listener.sendsGatedUnder(delivery.key); gated == 0 {
				t.Errorf("%s: no send under %s met the starting gate; the row must cover a send answered 503 "+
					"\"service starting\"", delivery.what, delivery.key)
			}
		}
	})

	t.Run("sent to a session the listener never knows", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		// Since R5 (dispatch://INFRA-3740), Dispatch queues a targeted message to a session no
		// store of the listener holds on that session's own subject, so a released Legion role
		// that comes back under the id reads it from its consumer, rather than failing it "no live
		// session". It still waits the take-over window out first: during a take-over the
		// successor answers "no live session" for a session it does not hold yet. This subtest
		// waits that window out and nothing else, so its processes run it at quickTakeoverWindow
		// (DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW) in place of the twelve seconds a deployment
		// runs: what it is about is that the send settles queued once the window has passed. How
		// long it waits - one take-over window from its first try, and no more - is a timer of the
		// sender's, held on the server's fake clock by
		// api.TestAMessageToASessionNoStoreHoldsIsQueuedOneTakeOverWindowAfterItsFirstTry.
		env := listener.processEnv()
		env["DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW"] = quickTakeoverWindow.String()
		pair := startPair(t, env)
		conn := testnats.Connect(t, pair.nats)
		t.Cleanup(conn.Close)
		js, err := conn.JetStream()
		if err != nil {
			t.Fatalf("JetStream context: %v", err)
		}
		head := streamHead(t, js)
		var message model.Message
		if err := json.Unmarshal(pair.a.call(t, http.MethodPost, "/api/v1/agents/never-known/messages",
			`{"body":"To nobody","delivery":"steer"}`, http.StatusCreated), &message); err != nil {
			t.Fatalf("decode the message: %v", err)
		}
		if len(message.Deliveries) != 1 || message.Deliveries[0].Attempt != 1 || message.Deliveries[0].State != "queued" ||
			message.Deliveries[0].EnvelopeID == nil || message.Deliveries[0].Error != nil ||
			message.Deliveries[0].SessionID != "never-known" {
			t.Fatalf("deliveries = %#v, want attempt 1 queued for never-known, naming its envelope and no error",
				message.Deliveries)
		}
		gone := messageDelivery("the message to nobody", message.ID+":steer")
		if states := gone.attempts(t, pair.database); !slices.Equal(states, []string{"1:queued"}) {
			t.Fatalf("%s reads %v, want attempt 1 alone, queued", gone.what, states)
		}
		if receipts := gone.receipts(t, pair.database); receipts != 1 {
			t.Fatalf("%s: attempt 1 has %d delivery receipts, want one", gone.what, receipts)
		}
		subject := contracts.LegacyAgentSubject("never-known")
		var held []string
		for _, stored := range readStreamMessagesFrom(t, js, head) {
			if stored.subject == subject {
				held = append(held, stored.item.EventID+" under "+stored.item.DedupeKey)
			}
		}
		want := *message.Deliveries[0].EnvelopeID + " under " +
			contracts.AgentSendDedupeKey("never-known", message.ID+":steer")
		if !slices.Equal(held, []string{want}) {
			t.Fatalf("the stream holds %q on %s, want the one envelope attempt 1 names, %q", held, subject, want)
		}
	})

	t.Run("killed during the wait", func(t *testing.T) {
		listener := startFakeListener(t, resumeSession)
		env := listener.quickSweepEnv()
		env["DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW"] = heldTakeoverWindow(t).String()
		pair := startPair(t, env)
		project := uniqueProjectKey(t, "TKK")
		pair.a.createProject(t, project, "Killed during a take-over")
		issue, _ := pair.a.createIssueIn(t, project, "Mentioned, then killed", "spec")

		// The listener refuses every connection until the row ends the take-over, and the senders
		// keep trying it for heldTakeoverWindow, so they are still waiting it out when A is killed
		// however a loaded machine spaces their tries.
		listener.takeOver(-1, 0, 0, 0)
		direct, mention := sendDuringTakeOver(t, pair.a, pair.database, issue, "killed during a take-over")
		requireWaitingOutTheTakeOver(t, pair.database, listener, direct, mention)
		pair.a.kill(t)
		listener.endTakeOver()
		backDatePendingAttempts(t, pair.database)
		requireDeliveredOnce(t, pair, listener, direct, mention)
	})
}

// ───── webhook redelivery ───────────────────────────────────────────────────

// heldRedeliveryProxy stands between the redelivery command and the GitHub webhook fake. It
// forwards every call but the first redelivery request, which it holds and never forwards: held is
// closed when that request arrives, and the hold ends when its caller goes away (a killed process)
// or when the test ends (release), answered 502 so a process that outlived it records a refusal.
// Nothing else ends it, so a slow step of the row while it holds changes nothing.
type heldRedeliveryProxy struct {
	server  *httptest.Server
	held    chan struct{}
	once    sync.Once
	release chan struct{}
}

func startHeldRedeliveryProxy(t *testing.T, upstream string) *heldRedeliveryProxy {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("parse the GitHub fake's URL: %v", err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	proxy := &heldRedeliveryProxy{held: make(chan struct{}), release: make(chan struct{})}
	proxy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attempts") {
			first := false
			proxy.once.Do(func() {
				first = true
				close(proxy.held)
			})
			if first {
				select {
				case <-r.Context().Done():
				case <-proxy.release:
					http.Error(w, `{"message":"held by the test"}`, http.StatusBadGateway)
				}
				return
			}
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.server.Close)
	// Registered after the server's close, so it runs first: the close waits for the held request
	// to end.
	t.Cleanup(func() { close(proxy.release) })
	return proxy
}

// startRedeliverWebhooks starts `envoy-dispatch redeliver-webhooks --since 1h` with env, in a home
// of its own and nothing from the caller's environment.
func startRedeliverWebhooks(t *testing.T, binary string, env map[string]string) *cmdtest.Process {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(binary, "redeliver-webhooks", "--since", "1h")
	cmd.Dir = home
	environment := map[string]string{"HOME": home, "XDG_CONFIG_HOME": home, "XDG_DATA_HOME": home}
	maps.Copy(environment, env)
	for name, value := range environment {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	slices.Sort(cmd.Env)
	return cmdtest.Start(t, "redeliver-webhooks", cmd)
}

// backDateRedeliveryClaim moves the claimed_at of guid's record in the redelivery KV bucket back
// by, through the test's own NATS client, so a row does not wait out a claim's deadline.
func backDateRedeliveryClaim(t *testing.T, natsURL, guid string, by time.Duration) {
	t.Helper()
	nc := testnats.Connect(t, natsURL)
	defer nc.Close()
	js, err := nc.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	// One copy, as the single test NATS server holds and the runs under test open.
	state, err := redeliver.OpenState(t.Context(), nc, js, 1)
	if err != nil {
		t.Fatalf("open the redelivery bucket: %v", err)
	}
	key := "guid." + guid
	entry, err := state.Get(key)
	if err != nil {
		t.Fatalf("read the redelivery record of %s: %v", guid, err)
	}
	var stored map[string]any
	if err := json.Unmarshal(entry.Value(), &stored); err != nil {
		t.Fatalf("decode the redelivery record of %s: %v", guid, err)
	}
	stored["claimed_at"] = time.Now().Add(-by).UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Update(key, data, entry.Revision()); err != nil {
		t.Fatalf("back-date the redelivery claim of %s: %v", guid, err)
	}
}

// A webhook redelivery whose process was killed between claiming the delivery and recording
// GitHub's answer is requested again by the next sweep once the claim's deadline (three GitHub
// request timeouts, a minute) has passed. Two `envoy-dispatch redeliver-webhooks --since 1h` runs
// share one NATS, one database (the GitHub rate limits every process obeys) and one GitHub; the
// first is SIGKILLed while its redelivery request is held on the way to GitHub, the test moves the
// claim's time back past the deadline rather than waiting it out, and the second run asks GitHub
// for the delivery, once. Both runs end each GitHub call at the test's give-up
// (DISPATCH_TEST_GITHUB_REQUEST_DEADLINE, under DISPATCH_TEST_HOOKS=1): a run makes one request
// per delivery and records a request that ran out of its bound as refused, with its next attempt
// a backoff away, and exits, so at githubapp.RequestTimeout a first run the test reached late
// would end on its own before the kill. The claim's deadline stays three of
// githubapp.RequestTimeout (redeliver's claimDeadline), which the test moves the claim past.
func TestAWebhookRedeliveryWhoseProcessDiedAfterItsClaimIsRequestedAgain(t *testing.T) {
	_, natsURL := multiprocessNATS(t)
	key, pemText := githubapptest.Key(t)
	const clientID = "Iv1.multiprocess"
	webhook := githubapptest.NewWebhook(t, &key.PublicKey, clientID, nil)
	failed := webhook.Record(githubapptest.Attempt{GUID: "guid-orphaned-claim", Event: "push", Payload: []byte(`{}`)},
		"", 7, http.StatusServiceUnavailable, time.Now().Add(-time.Minute))
	proxy := startHeldRedeliveryProxy(t, webhook.URL())
	env := githubAppEnv(t, proxy.server.URL)
	env["DISPATCH_APP_CLIENT_ID"] = clientID
	env["DISPATCH_APP_PEM_B64"] = base64.StdEncoding.EncodeToString([]byte(pemText))
	env["NATS_URLS"] = natsURL
	env["DATABASE_URL"] = databaseURLOf(t, storetest.Open(t))
	env["DISPATCH_TEST_HOOKS"] = "1"
	env["DISPATCH_TEST_GITHUB_REQUEST_DEADLINE"] = endsAtTheGiveUp(t)
	binary := cmdtest.Build(t, "envoy-dispatch")

	first := startRedeliverWebhooks(t, binary, env)
	testwait.Until(t, "the first run to ask GitHub for the delivery", func() (withOutput[string], bool) {
		select {
		case <-proxy.held:
			return withOutput[string]{}, true
		default:
		}
		select {
		case <-first.Exited:
			t.Fatalf("the first run exited before it asked GitHub for the delivery:\n%s", first.Output.String())
		default:
		}
		return readWith("the first run has asked GitHub for nothing; its output:", first.Output), false
	})
	if err := first.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the first run: %v", err)
	}
	<-first.Exited
	if got := webhook.Requests(); len(got) != 0 {
		t.Fatalf("the killed run's held request reached GitHub: %v", got)
	}

	backDateRedeliveryClaim(t, natsURL, failed.GUID, 2*3*githubapp.RequestTimeout)
	second := startRedeliverWebhooks(t, binary, env)
	second.WaitExit(t, "it started")
	if code := second.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("the second run exited %d:\n%s", code, second.Output.String())
	}
	if got, want := webhook.Requests(), []int64{failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("GitHub got redelivery requests %v, want %v once:\n%s", got, want, second.Output.String())
	}
	if !strings.Contains(second.Output.String(), " redelivered=1") {
		t.Fatalf("the second run does not report the delivery redelivered:\n%s", second.Output.String())
	}
}

// ───── one GitHub rate limit across the processes ──────────────────────────

// rateLimitGitHub is the GitHub both processes' proxy reads go to: acme/web, the installation
// covering it (77), a token mint answering rateLimitToken, and every read under that token, which
// it records. A read of rateLimitedRead answers as GitHub answers an exhausted rate limit: 403,
// X-RateLimit-Remaining: 0 and Retry-After. Every other App call answers an empty list, as
// githubFake's do.
type rateLimitGitHub struct {
	server *httptest.Server

	mu         sync.Mutex
	tokenReads []string
}

const (
	rateLimitToken  = "ghs_rate_limit_77"
	rateLimitedRead = "/repos/acme/web/pulls/1"
)

func startRateLimitGitHub(t *testing.T) *rateLimitGitHub {
	t.Helper()
	fake := &rateLimitGitHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":77,"app_slug":"multiprocess","permissions":{"pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, rateLimitToken, time.Now().Add(stagedPastTheRun).Format(time.RFC3339))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+rateLimitToken {
			_, _ = io.WriteString(w, "[]")
			return
		}
		fake.mu.Lock()
		fake.tokenReads = append(fake.tokenReads, r.Method+" "+r.URL.RequestURI())
		fake.mu.Unlock()
		if r.URL.Path == rateLimitedRead {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", strconv.Itoa(int(rateLimitRetryAfter.Seconds())))
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 77."}`)
			return
		}
		_, _ = io.WriteString(w, `{"state":"open","title":"Ship it"}`)
	})
	fake.server = githubtest.Serve(t, githubtest.Spec{Handle: mux.ServeHTTP})
	return fake
}

// reads is every request GitHub has been sent under the installation's token, by either process.
func (f *rateLimitGitHub) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tokenReads)
}

// proxyRead sends GET path to p's GitHub proxy as the test's login and returns the status, the
// Retry-After header and the error code of the answer.
func (p *dispatchProcess) proxyRead(t *testing.T, path string) (status int, retryAfter, code string) {
	t.Helper()
	answered, err := p.trySend(http.MethodGet, path, "", asUser(dispatchTestLogin))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(answered.body, &body)
	return answered.status, answered.header.Get("Retry-After"), body.Code
}

// GitHub budgets an installation, not a process, so a rate limit one process meets holds every
// process. A meets an exhausted limit reading acme/web as its installation; B's read of another
// path under the same installation answers 503 GITHUB_RATE_LIMITED with a Retry-After, and GitHub
// is sent nothing more under the installation's token. B's installation lookup and token mint are
// the App's calls, under the App's own limit, and may still reach GitHub.
func TestARateLimitOneProcessMetStopsTheOtherFromCallingGitHub(t *testing.T) {
	github := startRateLimitGitHub(t)
	pair := startPair(t, githubAppEnv(t, github.server.URL))

	statusA, retryA, codeA := pair.a.proxyRead(t, "/api/github/rest"+rateLimitedRead)
	if reads := github.reads(); len(reads) != 1 {
		t.Fatalf("GitHub got %v under the installation's token, want A's one read", reads)
	}
	statusB, retryB, codeB := pair.b.proxyRead(t, "/api/github/rest/repos/acme/web/issues/2")
	if reads := github.reads(); len(reads) != 1 {
		t.Errorf("GitHub got %v under the installation's token: B asked it inside the limit A met", reads)
	}
	if statusB != http.StatusServiceUnavailable || codeB != "GITHUB_RATE_LIMITED" {
		t.Errorf("B's read inside the limit: status %d code %q, want 503 GITHUB_RATE_LIMITED", statusB, codeB)
	}
	if seconds, err := strconv.Atoi(retryB); err != nil || seconds < 1 || seconds > int(rateLimitRetryAfter.Seconds()) {
		t.Errorf("B's Retry-After %q, want the seconds left of GitHub's %d", retryB, int(rateLimitRetryAfter.Seconds()))
	}
	if statusA != http.StatusServiceUnavailable || codeA != "GITHUB_RATE_LIMITED" || retryA != strconv.Itoa(int(rateLimitRetryAfter.Seconds())) {
		t.Errorf("A's limited read: status %d code %q Retry-After %q, want 503 GITHUB_RATE_LIMITED after %d", statusA, codeA, retryA, int(rateLimitRetryAfter.Seconds()))
	}
}

// rateLimitRetryAfter is the Retry-After the GitHub stand-in answers an exhausted limit with: the
// longest wait the processes honour (serverwait.MaxWait, an hour, past which they hold GitHub's
// to it), staged state they judge against their own clocks, not a wait. It is longer than the
// multiprocess job's go test -timeout (35 minutes), the one lane this row runs in, so B's read,
// however late a loaded machine makes it after A's, comes inside the limit A met.
const rateLimitRetryAfter = serverwait.MaxWait

// ───── the database's clock ────────────────────────────────────────────────

// Each process's host keeps a clock of its own, so a time Dispatch stores is the database's: the
// writing transaction's now(), which every column that transaction stamps shares to the
// microsecond. A claim made on A answers a claim.at equal to the updated_at its own update
// stamped, and its issue.claimed event was created at that time; an answer given on B to an ask
// opened on A carries the `at` its ask.answered event was created at. No process's clock can
// produce either equality.
func TestClaimAndAnswerTimesComeFromTheDatabaseOnEitherProcess(t *testing.T) {
	pair := startPair(t, nil)
	ctx := context.Background()
	project := uniqueProjectKey(t, "CLK")
	pair.a.createProject(t, project, "Database clock")
	key, _ := pair.a.createIssueIn(t, project, "Claimed on A, answered on B", "spec")

	claimed := pair.a.call(t, http.MethodPost, "/api/v1/issues/"+key+"/claim", `{}`, http.StatusOK)
	var issue struct {
		UpdatedAt time.Time `json:"updated_at"`
		Claim     *struct {
			At time.Time `json:"at"`
		} `json:"claim"`
	}
	if err := json.Unmarshal(claimed, &issue); err != nil || issue.Claim == nil {
		t.Fatalf("decode the claim on A: %v: %s", err, claimed)
	}
	if !issue.Claim.At.Equal(issue.UpdatedAt) {
		t.Errorf("A's claim: claim.at %s, updated_at %s, want the one transaction time",
			issue.Claim.At.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano))
	}
	var claimedEvent time.Time
	if err := pair.database.Pool.QueryRow(ctx, `select created_at from events where issue_key = $1 and type = 'issue.claimed'`, key).Scan(&claimedEvent); err != nil {
		t.Fatalf("read the issue.claimed event: %v", err)
	}
	if !claimedEvent.Equal(issue.Claim.At) {
		t.Errorf("A's claim: claim.at %s, issue.claimed created at %s, want the one transaction time",
			issue.Claim.At.Format(time.RFC3339Nano), claimedEvent.Format(time.RFC3339Nano))
	}

	opened := pair.a.call(t, http.MethodPost, "/api/v1/issues/"+key+"/asks",
		`{"question":"Ship it?","options":[{"label":"Ship"},{"label":"Hold"}]}`, http.StatusCreated)
	var ask struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(opened, &ask); err != nil || ask.ID == "" {
		t.Fatalf("decode the ask opened on A: %v: %s", err, opened)
	}
	answered := pair.b.call(t, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer",
		`{"selected":["Ship"],"expected_edited_at":null}`, http.StatusOK)
	var answer struct {
		Answer *struct {
			At time.Time `json:"at"`
		} `json:"answer"`
	}
	if err := json.Unmarshal(answered, &answer); err != nil || answer.Answer == nil {
		t.Fatalf("decode the answer given on B: %v: %s", err, answered)
	}
	var answeredEvent time.Time
	if err := pair.database.Pool.QueryRow(ctx, `
		select created_at from events where issue_key = $1 and type = 'ask.answered' and payload->>'id' = $2
	`, key, ask.ID).Scan(&answeredEvent); err != nil {
		t.Fatalf("read the ask.answered event: %v", err)
	}
	if !answeredEvent.Equal(answer.Answer.At) {
		t.Errorf("B's answer: at %s, ask.answered created at %s, want the one transaction time",
			answer.Answer.At.Format(time.RFC3339Nano), answeredEvent.Format(time.RFC3339Nano))
	}
}

// ───── one configuration warning across the processes ──────────────────────

// misconfiguredRun is the deploy run the warning row seeds: a successful run on main whose one job
// is none of the production job names seedDeliverySettings configures.
const misconfiguredRun = int64(38041790209)

// A misconfigured production job name is named in the log once, not once per process: the Delivery
// page judges every deploy run on each read, on whichever process the read reached. The settings
// name a production job no job of the seeded run holds; the Delivery page is read on A, then on B,
// and the two processes' logs hold exactly one warning for that run between them. Each process's
// access line for its read is written after the read's warning, so once both are in, a warning
// either process wrote is in too.
func TestAConfigurationWarningIsLoggedOnceAcrossTheProcesses(t *testing.T) {
	pair := newPair(t)
	seedDeliverySettings(t, pair.database)
	ctx := context.Background()
	start := time.Date(2026, 10, 10, 9, 47, 17, 0, time.UTC)
	success := delivery.DeliveryRunConclusionSuccess
	run := strconv.FormatInt(misconfiguredRun, 10)
	if err := delivery.UpsertRun(ctx, pair.database.Pool, delivery.DeliveryRun{
		Repo: "acme/deploy", RunID: misconfiguredRun, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + run,
		HeadCommitAt: start.Add(-time.Hour), StartedAt: start, CompletedAt: new(start.Add(15 * time.Minute)), Conclusion: &success,
		URL: "https://github.com/acme/deploy/actions/runs/" + run, HeadBranch: new("main"), Event: new("workflow_dispatch"),
	}); err != nil {
		t.Fatalf("seed the run: %v", err)
	}
	if err := delivery.UpsertRunJobs(ctx, pair.database.Pool, "acme/deploy", misconfiguredRun, []delivery.DeliveryRunJob{{
		Repo: "acme/deploy", RunID: misconfiguredRun, Name: "prod-apply / prod-apply",
		Conclusion: new(delivery.DeliveryJobConclusionSuccess), CompletedAt: new(start.Add(10 * time.Minute)),
	}}, true); err != nil {
		t.Fatalf("seed the run's job: %v", err)
	}
	pair.start(t, nil)

	const timeline = "/api/v1/delivery/timeline?from=2026-10-10T00:00:00Z&to=2026-10-11T00:00:00Z"
	read := []string{`msg="dispatch: request"`, "route=/api/v1/delivery/timeline", "status=200"}
	pair.a.call(t, http.MethodGet, timeline, "", http.StatusOK)
	pair.a.waitForLog(t, read...)
	pair.b.call(t, http.MethodGet, timeline, "", http.StatusOK)
	pair.b.waitForLog(t, read...)

	warning := []string{"no job matching the configured production_job_names", "run_id=" + run}
	onA, onB := pair.a.logLines(warning...), pair.b.logLines(warning...)
	if len(onA)+len(onB) != 1 {
		t.Fatalf("warnings for run %s: %d on A, %d on B, want one between them:\nA:\n%s\nB:\n%s",
			run, len(onA), len(onB), strings.Join(onA, "\n"), strings.Join(onB, "\n"))
	}
}

// ───── two processes syncing one architecture source ───────────────────────

// architectureGitHub is the GitHub both processes' architecture syncs read: one repository whose
// branch head the test moves, each commit answering its own architecture files, and every blob
// read of one commit held until the test releases it or the caller gives up, by the process's own
// bound on the call (githubapp.RequestTimeout) or its exit. Nothing else ends the hold, so a slow
// step of the row while it holds changes nothing, and the test's end releases it. Any other App
// call answers an empty list, as githubFake's do.
type architectureGitHub struct {
	server      *httptest.Server
	held        chan struct{}
	release     chan struct{}
	heldOnce    sync.Once
	releaseOnce sync.Once

	mu          sync.Mutex
	head        string
	commits     map[string]map[string]string
	holdBlobsOf string
}

// startArchitectureGitHub serves on loopback until the test ends, its branch at head.
func startArchitectureGitHub(t *testing.T, head string, commits map[string]map[string]string) *architectureGitHub {
	t.Helper()
	fake := &architectureGitHub{held: make(chan struct{}), release: make(chan struct{}), head: head, commits: commits}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":77,"app_slug":"multiprocess","permissions":{"contents":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_multiprocess","expires_at":%q}`, time.Now().Add(stagedPastTheRun).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{branch}", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		head := fake.head
		fake.mu.Unlock()
		// As GitHub does: the bare sha for Accept: application/vnd.github.sha, JSON otherwise.
		if r.Header.Get("Accept") == "application/vnd.github.sha" {
			_, _ = io.WriteString(w, head)
			return
		}
		_, _ = fmt.Fprintf(w, `{"sha":%q}`, head)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, r *http.Request) {
		commit, _, _ := strings.Cut(r.PathValue("ref"), ":")
		fake.mu.Lock()
		files := fake.commits[commit]
		fake.mu.Unlock()
		entries := []map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(files)) {
			entries = append(entries, map[string]any{
				"path": name, "mode": "100644", "type": "blob", "sha": commit + "." + name, "size": len(files[name]),
			})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"sha": "tree-" + commit, "truncated": false, "tree": entries}); err != nil {
			t.Errorf("encode tree: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		commit, name, _ := strings.Cut(r.PathValue("sha"), ".")
		fake.mu.Lock()
		content, ok := fake.commits[commit][name]
		hold := commit == fake.holdBlobsOf
		fake.mu.Unlock()
		if hold {
			fake.heldOnce.Do(func() { close(fake.held) })
			select {
			case <-fake.release:
			case <-r.Context().Done():
			}
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64",
		}); err != nil {
			t.Errorf("encode blob: %v", err)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	})
	fake.server = githubtest.Serve(t, githubtest.Spec{Handle: mux.ServeHTTP})
	t.Cleanup(fake.releaseHeld)
	return fake
}

// holdBlobs holds every blob read of commit; moveHead pushes commit as the branch's new head.
func (f *architectureGitHub) holdBlobs(commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdBlobsOf = commit
}

func (f *architectureGitHub) moveHead(commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = commit
}

func (f *architectureGitHub) releaseHeld() { f.releaseOnce.Do(func() { close(f.release) }) }

// Two processes sync one project's architecture source through the Refresh route: A is held
// fetching the files of head X, the branch moves to Y, and B imports Y. Once A is released it
// must not record X over Y: the source stays at Y with Y's components, the only synced event is
// Y's, and A answers the row at Y. The source is disabled so the import ticker adds no sync of
// its own.
func TestTwoProcessesSyncingOneProjectNeverWriteTheOlderCommit(t *testing.T) {
	const older, newer = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	github := startArchitectureGitHub(t, older, map[string]map[string]string{
		older: {"api.md": "---\ntitle: API\n---\nThe API.\n"},
		newer: {"queue.md": "---\ntitle: Queue\n---\nThe queue.\n", "store.md": "---\ntitle: Store\n---\nThe store.\n"},
	})
	github.holdBlobs(older)
	// Each GitHub call of the processes ends at the test's give-up (DISPATCH_TEST_GITHUB_REQUEST_DEADLINE),
	// so A's held read of X's files ends only when the row releases it, however long B's sync
	// takes on a loaded machine; the row's own waits end at the shared wait's give-up.
	env := githubAppEnv(t, github.server.URL)
	env["DISPATCH_TEST_GITHUB_REQUEST_DEADLINE"] = endsAtTheGiveUp(t)
	pair := startPair(t, env)
	project := uniqueProjectKey(t, "ARC")
	pair.a.createProject(t, project, "Architecture")
	if _, err := pair.database.Pool.Exec(context.Background(), `
		insert into architecture_sources (project_key, repo, branch, enabled, installation_id, created_by)
		values ($1, 'acme/arch', 'main', false, 77, '{"kind":"user","id":"alice"}')
	`, project); err != nil {
		t.Fatalf("seed the architecture source: %v", err)
	}
	path := "/api/v1/projects/" + project + "/architecture-sources/acme/arch/sync"

	type answer struct {
		status int
		body   []byte
		err    error
	}
	held := make(chan answer, 1)
	go func() {
		answered, err := pair.a.trySend(http.MethodPost, path, "", asUser(dispatchTestLogin))
		if err != nil {
			held <- answer{err: err}
			return
		}
		held <- answer{status: answered.status, body: answered.body}
	}()
	testwait.Receive(t, "A's sync to fetch the files of "+older, github.held)
	github.moveHead(newer)
	pair.b.call(t, http.MethodPost, path, "", http.StatusOK)
	github.releaseHeld()

	fromA := testwait.Receive(t, "A's sync to answer", held)
	if fromA.err != nil || fromA.status != http.StatusOK {
		t.Fatalf("A's sync: status %d, err %v: %s", fromA.status, fromA.err, fromA.body)
	}
	var row struct {
		LastCommit *string `json:"last_commit"`
		LastError  *string `json:"last_error"`
	}
	if err := json.Unmarshal(fromA.body, &row); err != nil {
		t.Fatalf("decode A's answer: %v: %s", err, fromA.body)
	}
	if row.LastCommit == nil || *row.LastCommit != newer || row.LastError != nil {
		t.Errorf("A answered %s, want the row at %s with no error", fromA.body, newer)
	}

	var lastCommit string
	var components, synced []string
	if err := pair.database.Pool.QueryRow(context.Background(), `
		select last_commit,
		       array(select id from components where project_key = $1 order by id),
		       array(select payload->>'commit' from events where project_key = $1 and type = 'architecture.synced' order by id)
		from architecture_sources where project_key = $1
	`, project).Scan(&lastCommit, &components, &synced); err != nil {
		t.Fatalf("read the source: %v", err)
	}
	if lastCommit != newer {
		t.Errorf("last_commit = %s, want %s", lastCommit, newer)
	}
	if want := []string{"queue", "store"}; !slices.Equal(components, want) {
		t.Errorf("components = %v, want %s's %v", components, newer, want)
	}
	if want := []string{newer}; !slices.Equal(synced, want) {
		t.Errorf("architecture.synced events name %v, want %v once", synced, want)
	}
}

// ───── route status from one stored listener answer ────────────────────────

// rosterEnv is what both processes of a route-status row are started with: the fake listener as
// ENVOY_URL, and a listener answer that serves issue reads for a day
// (DISPATCH_TEST_ROUTE_ROSTER_TTL, under the test hooks every process here runs with): staged
// state for the product's clock, not a wait, and longer than the longest go test -timeout CI
// gives the package (80 minutes), so the row reads both processes inside one answer's life
// however slow the machine, with no timing window.
func rosterEnv(listener *fakeListener) map[string]string {
	env := listener.processEnv()
	env["DISPATCH_TEST_ROUTE_ROSTER_TTL"] = (24 * time.Hour).String()
	return env
}

// routeIssue creates an issue in project titled title and routes it to route, returning its key.
func (p *dispatchProcess) routeIssue(t *testing.T, project, title, route string) string {
	t.Helper()
	created := p.call(t, http.MethodPost, "/api/v1/issues",
		fmt.Sprintf(`{"project":%q,"title":%q,"force":true}`, project, title), http.StatusCreated)
	var issue struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" {
		t.Fatalf("create issue: %v: %s", err, created)
	}
	p.call(t, http.MethodPatch, "/api/v1/issues/"+issue.Key, fmt.Sprintf(`{"route":%q}`, route), http.StatusOK)
	return issue.Key
}

// issueReach is the issue's route_status and route_holder as GET /api/v1/issues/{key} on this
// process answers them, "<status> <holder>", each "null" when absent.
func (p *dispatchProcess) issueReach(t *testing.T, key string) string {
	t.Helper()
	var issue struct {
		RouteStatus *string `json:"route_status"`
		RouteHolder *string `json:"route_holder"`
	}
	answer := p.call(t, http.MethodGet, "/api/v1/issues/"+key, "", http.StatusOK)
	if err := json.Unmarshal(answer, &issue); err != nil {
		t.Fatalf("decode %s: %v: %s", key, err, answer)
	}
	orNull := func(value *string) string {
		if value == nil {
			return "null"
		}
		return *value
	}
	return orNull(issue.RouteStatus) + " " + orNull(issue.RouteHolder)
}

// liveRoutesPage is one page of project's open issues whose route reaches a live session, as GET
// /api/v1/issues?route_status=live on this process answers it: the page's keys and its total.
func (p *dispatchProcess) liveRoutesPage(t *testing.T, project string, limit, offset int) (keys []string, total int) {
	t.Helper()
	var page struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
		Total int `json:"total"`
	}
	answer := p.call(t, http.MethodGet,
		fmt.Sprintf("/api/v1/issues?project=%s&route_status=live&limit=%d&offset=%d", project, limit, offset), "", http.StatusOK)
	if err := json.Unmarshal(answer, &page); err != nil {
		t.Fatalf("decode the page at offset %d: %v: %s", offset, err, answer)
	}
	for _, issue := range page.Issues {
		keys = append(keys, issue.Key)
	}
	return keys, page.Total
}

// Both processes judge a route against one stored listener answer. A route A reads live reads the
// same on B, and again on A, after the listener stops listing its holder, inside the answer's life:
// B does not ask the listener for a copy of its own, so the listener answers one live-list read
// in all. A filtered listing paged across the two processes is cut from that one answer too: a
// page read on B after the listener's list has flipped holds the rest of the issues A's page was
// cut from, never issues judged against B's own later copy.
func TestBothProcessesJudgeARouteAgainstOneRoster(t *testing.T) {
	sre := listenerSession{SessionID: "sre-holder", Title: "sre", Roles: []string{"sre"}, Capabilities: []string{}}
	reviewer := listenerSession{SessionID: "reviewer-holder", Title: "reviewer", Roles: []string{"reviewer"}, Capabilities: []string{}}

	t.Run("an issue's route", func(t *testing.T) {
		listener := startFakeListener(t, sre)
		pair := startPair(t, rosterEnv(listener))
		project := uniqueProjectKey(t, "RST")
		pair.a.createProject(t, project, "One roster")
		key := pair.a.routeIssue(t, project, "Routed to sre", "role:sre")

		if got := pair.a.issueReach(t, key); got != "live sre-holder" {
			t.Fatalf("A reads %s while the listener lists the holder, want live sre-holder", got)
		}
		listener.setSessions()
		for _, reader := range []*dispatchProcess{pair.b, pair.a} {
			if got := reader.issueReach(t, key); got != "live sre-holder" {
				t.Fatalf("%s reads %s after the listener stopped listing the holder, want the stored live sre-holder",
					pair.name(reader), got)
			}
		}
		if calls := listener.sessionListings(); calls != 1 {
			t.Fatalf("the listener answered %d live-list reads, want 1: both processes judge against one stored answer", calls)
		}
	})

	t.Run("a filtered page", func(t *testing.T) {
		listener := startFakeListener(t, sre)
		pair := startPair(t, rosterEnv(listener))
		project := uniqueProjectKey(t, "RSP")
		pair.a.createProject(t, project, "One roster, paged")
		routedToSRE := make([]string, 0, 6)
		for index := range 6 {
			routedToSRE = append(routedToSRE, pair.a.routeIssue(t, project, fmt.Sprintf("SRE work %d", index), "role:sre"))
			pair.a.routeIssue(t, project, fmt.Sprintf("Review work %d", index), "role:reviewer")
		}

		first, firstTotal := pair.a.liveRoutesPage(t, project, 3, 0)
		listener.setSessions(reviewer)
		second, secondTotal := pair.b.liveRoutesPage(t, project, 3, 3)
		if firstTotal != 6 || secondTotal != 6 {
			t.Errorf("total reads %d on A and %d on B, want 6 on both", firstTotal, secondTotal)
		}
		paged := slices.Concat(first, second)
		slices.Sort(paged)
		want := slices.Clone(routedToSRE)
		slices.Sort(want)
		if !slices.Equal(paged, want) {
			t.Fatalf("page 1 on A holds %v and page 2 on B holds %v, want together exactly the issues routed to sre %v",
				first, second, routedToSRE)
		}
	})
}

// ───── the test hooks ───────────────────────────────────────────────────────

// hookViewerSession is the session whose conversation the test-hook row's viewer watches: any
// UUID, since the in-process relay the publish hook feeds knows no sessions of its own.
const hookViewerSession = "01a0e090-4848-7473-acc5-fc96e6a646d3"

// startHookPair starts both processes as the browser harness starts its server: with the test
// hooks, as every process here runs, and without NATS, so the agent conversation relay each
// serves is the in-process source the publish hook feeds (main.go), one per process. env is
// what both are started with besides.
func startHookPair(t *testing.T, env map[string]string) *dispatchPair {
	t.Helper()
	pair := &dispatchPair{database: storetest.Open(t)}
	options := dispatchOptions{SigningKey: multiprocessSigningKey, Env: env}
	pair.a = startDispatchProcessWith(t, pair.databaseURL(t), options)
	pair.b = startDispatchProcessWith(t, pair.databaseURL(t), options)
	pair.a.waitHealthy(t)
	pair.b.waitHealthy(t)
	return pair
}

// callUntilTheGiveUp is call with the request under the shared wait's context (testwait.Context):
// a call whose answer a process holds back, such as a test hook waiting on another process's
// acknowledgement with its own bound at the test's give-up (DISPATCH_TEST_HOOK_DEADLINE,
// endsAtTheGiveUp), fails the test at the give-up naming the call, rather than holding it past the
// binary's deadline.
func (p *dispatchProcess) callUntilTheGiveUp(t *testing.T, method, path, body string, status int) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(testwait.Context(t), method, p.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s did not answer by the moment the test's waits give up: %v\n%s", method, path, err, p.Output.String())
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer of %s %s: %v", method, path, err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, status, answer)
	}
	return answer
}

// liveSocketFrames reads conn, a live socket, until it ends: each frame of the agent view it
// holds lands on frames, and the read error that ended the socket on ended.
func liveSocketFrames(conn *gws.Conn) (frames <-chan string, ended <-chan streamEnd) {
	agent := make(chan string, 64)
	end := make(chan streamEnd, 1)
	go func() {
		defer close(agent)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				end <- streamEnd{at: time.Now(), err: err}
				return
			}
			var frame struct {
				Type  string          `json:"type"`
				Event string          `json:"event"`
				Data  json.RawMessage `json:"data"`
			}
			if json.Unmarshal(data, &frame) != nil || frame.Type != "agent" || frame.Event != "frame" {
				continue
			}
			agent <- string(frame.Data)
		}
	}()
	return agent, end
}

// requireSocketClosed waits for a live socket to end with code, failing the row when it ends
// another way.
func requireSocketClosed(t *testing.T, ended <-chan streamEnd, code int, what string) {
	t.Helper()
	if end := testwait.Receive(t, what+" to close", ended); !gws.IsCloseError(end.err, code) {
		t.Fatalf("%s ended with %v, want the hook's %d", what, end.err, code)
	}
}

// Every test hook acts on every process serving the database, whichever process the call landed
// on: a browser suite against two processes disconnects, rotates, quiesces and feeds frames to
// the process its page happens to be connected to, which is not the one its own API call reaches.
// A live socket - one per tab, holding both the event feed and a conversation view - a live
// document room and a second live socket are each held open on B, and each hook is called on A.
// quiesce answers only once B's room has closed too, which is what a harness truncating its
// database right after relies on. Once B is killed its lock is gone with its connection, and a
// hook called on A answers on A's acknowledgement alone rather than waiting out the timeout for a
// process that no longer exists.
func TestEveryTestHookActsOnBothProcesses(t *testing.T) {
	everyTestHookActsOnBothProcesses(t, benchtime.EveryRun)
}

// TestEveryTestHookActsOnBothProcessesOnTheClock is the row above in benchtime's timing lane,
// where the disconnect hook called on A once B is killed is held to answering well inside the
// hook's ten-second timeout.
func TestEveryTestHookActsOnBothProcessesOnTheClock(t *testing.T) {
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), everyTestHookActsOnBothProcesses)
}

// everyTestHookActsOnBothProcesses is the scene of the two rows above; lane says whether its
// reading of the clock is held to its figure (benchtime.Lane).
func everyTestHookActsOnBothProcesses(t *testing.T, lane benchtime.Lane) {
	// On every run the hooks' bound on another process's acknowledgement (testHookTimeout, ten
	// seconds) ends at the test's give-up instead (DISPATCH_TEST_HOOK_DEADLINE, endsAtTheGiveUp),
	// and each hook call waits under the shared wait's context (callUntilTheGiveUp); the timing
	// lane keeps the ten seconds, which its twin reads.
	var env map[string]string
	if lane == benchtime.EveryRun {
		env = map[string]string{"DISPATCH_TEST_HOOK_DEADLINE": endsAtTheGiveUp(t)}
	}
	pair := startHookPair(t, env)
	project := uniqueProjectKey(t, "HKS")
	pair.a.createProject(t, project, "Test hooks")
	_, artifactID := pair.a.createIssueIn(t, project, "Open on B", "before")

	// One socket per tab: B's holds the event feed and the conversation view the publish hook
	// feeds, as a page watching an agent holds them.
	viewer := pair.b.openLiveEvents(t)
	pair.b.watchAgent(t, viewer, hookViewerSession)
	published, viewerEnded := liveSocketFrames(viewer)
	const frame = `{"v":1,"kind":"message","seq":7}`
	pair.a.callUntilTheGiveUp(t, http.MethodPost, "/api/v1/agents/"+hookViewerSession+"/stream/_test/publish", frame, http.StatusOK)
	if got := testwait.Receive(t, "the conversation view B held to receive the frame published on A", published); got != frame {
		t.Fatalf("the conversation view B held received %s, want the frame published on A", got)
	}

	pair.a.callUntilTheGiveUp(t, http.MethodPost, "/api/v1/live/_test/disconnect", "", http.StatusOK)
	requireSocketClosed(t, viewerEnded, gws.CloseServiceRestart,
		"the live socket B held after the disconnect hook was called on A")

	_, rotated := liveSocketFrames(pair.b.openLiveEvents(t))
	pair.a.callUntilTheGiveUp(t, http.MethodPost, "/api/v1/live/_test/rotate", "", http.StatusOK)
	requireSocketClosed(t, rotated, contracts.LiveSocketMaxAgeClose,
		"the live socket B held after the rotate hook was called on A")

	editor := pair.b.openEditor(t, artifactID, "before\n")
	if rooms := pair.b.residentRooms(t); !slices.Contains(rooms, artifactID) {
		t.Fatalf("B holds rooms %v with the editor open on it, want %s among them", rooms, artifactID)
	}
	pair.a.callUntilTheGiveUp(t, http.MethodPost, "/api/v1/artifacts/_test/quiesce", "", http.StatusOK)
	// B closed the room before it acknowledged, and A answered only after that acknowledgement,
	// so the moment the answer arrives B holds the room no longer, which B's own list says; the
	// editor's socket closing is the same close reaching it over loopback.
	if rooms := pair.b.residentRooms(t); slices.Contains(rooms, artifactID) {
		t.Fatalf("B still held the room of %s (rooms %v) when quiesce answered on A; quiesce answers only once every process has closed its rooms",
			artifactID, rooms)
	}
	testwait.Receive(t, "the document room B held to close once quiesce answered on A", editor.Ended)

	_, survivor := liveSocketFrames(pair.a.openLiveEvents(t))
	pair.b.kill(t)
	called := time.Now()
	pair.a.callUntilTheGiveUp(t, http.MethodPost, "/api/v1/live/_test/disconnect", "", http.StatusOK)
	waited := time.Since(called)
	t.Logf("the disconnect hook on A answered %v after B was killed", waited)
	// The hook's timeout is ten seconds in a deployment, on A's clock inside a child process, and
	// the test's give-up on every run here: a call that waited for B to it would answer 504
	// TEST_HOOK_TIMEOUT in the timing lane, which the 200 the call requires already refuses, and on
	// every other run would fail at the shared wait's give-up. internal/dispatch/api's
	// TestATestHookStopsWaitingForAProcessWhoseLockIsGone proves on a fake clock that never reaches
	// the timeout that the call stops waiting once B's lock is gone. How far inside the timeout the
	// answer came is a reading of this machine's clock, so only the row's twin in benchtime's
	// timing lane holds it to a figure (benchtime.OnTheClock).
	if lane == benchtime.OnTheClock && waited >= 10*time.Second {
		t.Fatalf("the disconnect hook on A answered %v after B was killed, want well inside the 10s timeout", waited)
	}
	requireSocketClosed(t, survivor, gws.CloseServiceRestart,
		"the live socket A held after the disconnect hook answered with B killed")
}

// ───── two dashboard builds ─────────────────────────────────────────────────

// dashboardBuild is a web dist a test wrote: its directory and every file below its assets
// directory, by URL path.
type dashboardBuild struct {
	dir    string
	assets map[string][]byte
}

// writeDashboardBuild writes a dist as Vite does, for build name: an index.html naming its own
// entry chunk, the entry, chunks of the sizes the dashboard's own run to (its largest is 1,747,955
// bytes) whose names and bytes are this build's alone, and a chunk of the same name and bytes in
// every build, as yjs and the stylesheet are across two builds of one source.
func writeDashboardBuild(t *testing.T, name string) dashboardBuild {
	t.Helper()
	build := dashboardBuild{dir: t.TempDir(), assets: map[string][]byte{}}
	entry := "/assets/index-" + name + ".js"
	build.assets[entry] = []byte("import './chunk-" + name + "-0.js'; console.log('" + name + "')")
	for i, size := range []int{1_747_955, 420_000, 96_000, 12_000, 2_000, 300, 1} {
		build.assets[fmt.Sprintf("/assets/chunk-%s-%d.js", name, i)] = bytes.Repeat([]byte(name), size)
	}
	build.assets["/assets/fonts/inter-"+name+".woff2"] = []byte{0x77, 0x4f, 0x46, 0x32, 0x00, byte(len(name))}
	build.assets["/assets/yjs-shared.js"] = []byte("export const yjs = 'the same in every build'")
	files := map[string][]byte{"index.html": []byte(`<!doctype html><html><head><script type="module" crossorigin src="` + entry + `"></script></head><body><div id="root"></div></body></html>`)}
	for path, content := range build.assets {
		files[strings.TrimPrefix(path, "/")] = content
	}
	for name, content := range files {
		file := filepath.Join(build.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return build
}

// startOnBuild starts a Dispatch process serving build on database, with no NATS: the dashboard's
// assets need none. It carries multiprocessSigningKey, as every process beside a live peer must,
// and env besides.
func startOnBuild(t *testing.T, database string, build dashboardBuild, env map[string]string) *dispatchProcess {
	t.Helper()
	settings := maps.Clone(env)
	if settings == nil {
		settings = map[string]string{}
	}
	settings["DISPATCH_WEB_DIST"] = build.dir
	process := startDispatchProcessWith(t, database, dispatchOptions{SigningKey: multiprocessSigningKey, Env: settings})
	process.waitHealthy(t)
	return process
}

// getAsset fetches path from the process and says what is wrong with the answer, or nothing.
func getAsset(client *http.Client, p *dispatchProcess, path string, want []byte) error {
	response, err := client.Get(p.url(path))
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("GET %s: read: %w", path, err)
	}
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
		return fmt.Errorf("GET %s: status %d with %d bytes, want 200 with the %d bytes on disk", path, response.StatusCode, len(body), len(want))
	}
	if got := response.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		return fmt.Errorf("GET %s: Cache-Control %q, want the immutable year", path, got)
	}
	return nil
}

// A rolling update runs two builds on one database, each process holding its own build's files
// alone: a page one process served asks the other for its chunks. Every file of either build
// answers 200 with its bytes from both processes.
func TestDashboardServesEveryFileOfEitherBuildFromBothProcesses(t *testing.T) {
	database := (&dispatchPair{database: storetest.Open(t)}).databaseURL(t)
	builds := []dashboardBuild{writeDashboardBuild(t, "a"), writeDashboardBuild(t, "b")}
	processes := []*dispatchProcess{startOnBuild(t, database, builds[0], nil), startOnBuild(t, database, builds[1], nil)}

	for i, process := range processes {
		for j, build := range builds {
			for path, content := range build.assets {
				if err := getAsset(http.DefaultClient, process, path, content); err != nil {
					t.Errorf("process %s, build %s's file: %v", "AB"[i:i+1], "ab"[j:j+1], err)
				}
			}
		}
	}
}

// Every chunk a stale tab's route imports is asked for at once, by every such tab: a burst of 200
// concurrent requests for build A's files, every one but the shared chunk missing from B's disk, is
// answered 200 with each file's bytes by B.
func TestDashboardServesABurstOfTheOtherBuildsFiles(t *testing.T) {
	database := (&dispatchPair{database: storetest.Open(t)}).databaseURL(t)
	a := writeDashboardBuild(t, "a")
	startOnBuild(t, database, a, nil)
	// B reads each request's headers until the test's give-up (DISPATCH_TEST_READ_HEADER_DEADLINE,
	// endsAtTheGiveUp), in place of the deployment's ten seconds, which a burst on a loaded machine
	// can outlast; the burst's own wait ends at the shared wait's give-up (testwait.Returns).
	b := startOnBuild(t, database, writeDashboardBuild(t, "b"), map[string]string{"DISPATCH_TEST_READ_HEADER_DEADLINE": endsAtTheGiveUp(t)})
	paths := slices.Sorted(maps.Keys(a.assets))

	const burst = 200
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: burst}}
	failures := make([]error, burst)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range burst {
		wg.Go(func() {
			<-start
			path := paths[i%len(paths)]
			failures[i] = getAsset(client, b, path, a.assets[path])
		})
	}
	close(start)
	// No request carries a bound of its own (the client sets no timeout and no context), so the
	// join is their bound.
	testwait.Returns(t, fmt.Sprintf("the %d concurrent requests for A's files to B to return", burst), wg.Wait)

	failed := 0
	for _, err := range failures {
		if err != nil {
			failed++
			t.Error(err)
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d concurrent requests for A's files to B failed:\n%s", failed, burst, b.Output.String())
	}
}

// A pod restarted on the build it ran writes no asset: the restart's publish writes no file's
// bytes and leaves every stored asset row as it was, renewing only the build's last start, and the
// restarted process still serves every file.
func TestDashboardRestartOnTheSameBuildWritesNoAssetBytes(t *testing.T) {
	ctx := context.Background()
	pair := &dispatchPair{database: storetest.Open(t)}
	database := pair.databaseURL(t)
	build := writeDashboardBuild(t, "a")
	first := startOnBuild(t, database, build, nil)
	if !first.logged("dispatch: published dashboard assets", fmt.Sprintf("assets=%d written=%d", len(build.assets), len(build.assets))) {
		t.Fatalf("the first boot did not write every one of the build's %d files:\n%s", len(build.assets), first.Output.String())
	}
	assetRows := func() string {
		var rows string
		if err := pair.database.Pool.QueryRow(ctx, `select string_agg(path || ':' || xmin::text || ':' || created_at::text, ',' order by path) from dashboard_assets`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	lastStarted := func() time.Time {
		var at time.Time
		if err := pair.database.Pool.QueryRow(ctx, `select last_started_at from dashboard_builds`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	rowsBefore, startedBefore := assetRows(), lastStarted()
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")

	restarted := startOnBuild(t, database, build, nil)

	if !restarted.logged("dispatch: published dashboard assets", fmt.Sprintf("assets=%d written=0", len(build.assets))) {
		t.Errorf("the restart wrote asset bytes, want none:\n%s", restarted.Output.String())
	}
	if rowsAfter := assetRows(); rowsAfter != rowsBefore {
		t.Errorf("the restart changed asset rows: before %s, after %s", rowsBefore, rowsAfter)
	}
	if startedAfter := lastStarted(); !startedAfter.After(startedBefore) {
		t.Errorf("the build's last start is %s after the restart, want later than %s", startedAfter, startedBefore)
	}
	for path, content := range build.assets {
		if err := getAsset(http.DefaultClient, restarted, path, content); err != nil {
			t.Error(err)
		}
	}
}

// dashboardBuildsOf is GET /api/v1/dashboard/builds from the process, as the test's person.
func dashboardBuildsOf(t *testing.T, p *dispatchProcess) (serving string, live []string) {
	t.Helper()
	var answer struct {
		Serving string   `json:"serving"`
		Live    []string `json:"live"`
	}
	if err := json.Unmarshal(p.call(t, http.MethodGet, "/api/v1/dashboard/builds", "", http.StatusOK), &answer); err != nil {
		t.Fatal(err)
	}
	return answer.Serving, answer.Live
}

// A tab offers "A new version is available" only when one build is live and it is not the tab's,
// so the builds route is what a rolling update shows it. While A and B run both builds are live,
// whichever process answers; once B shuts down gracefully it has deregistered and only A's build
// is; and a process that stopped beating without deregistering drops out once its heartbeat is
// older than the live window, aged here in the database rather than waited for. A and B beat at
// stagedPastTheRun (DISPATCH_TEST_PROCESS_HEARTBEAT), so each judges a row by a live window of
// three of that, which no row a live process wrote, nor the beating row the test inserts, leaves
// inside the run; the lapsed row is aged past that window of A's.
func TestDashboardBuildsAnswersTheBuildsTheLiveProcessesServe(t *testing.T) {
	ctx := context.Background()
	pair := &dispatchPair{database: storetest.Open(t)}
	database := pair.databaseURL(t)
	const entryA, entryB = "/assets/index-a.js", "/assets/index-b.js"
	beating := map[string]string{"DISPATCH_TEST_PROCESS_HEARTBEAT": stagedPastTheRun.String()}
	buildB := writeDashboardBuild(t, "b")
	a := startOnBuild(t, database, writeDashboardBuild(t, "a"), beating)
	b := startOnBuild(t, database, buildB, beating)

	for _, process := range []struct {
		name    string
		p       *dispatchProcess
		serving string
	}{{"A", a, entryA}, {"B", b, entryB}} {
		if serving, live := dashboardBuildsOf(t, process.p); serving != process.serving || !slices.Equal(live, []string{entryA, entryB}) {
			t.Errorf("process %s with both live: serving %q, live %q; want %q and both builds", process.name, serving, live, process.serving)
		}
	}

	b.Terminate(t)
	b.WaitExit(t, "SIGTERM")
	if serving, live := dashboardBuildsOf(t, a); serving != entryA || !slices.Equal(live, []string{entryA}) {
		t.Fatalf("after B shut down: serving %q, live %q; want %q alone", serving, live, entryA)
	}

	// A process on B's build that is beating, and then one whose last heartbeat left the window. It
	// runs this build's binary, so its row names this build's owner lock protocol, as
	// processes.Register writes it.
	var instance string
	if err := pair.database.Pool.QueryRow(ctx, `
		insert into server_processes (instance, dashboard_build, signing_keys, owner_lock_protocol) values (gen_random_uuid(), $1, '{fingerprint}', $2)
		returning instance::text`, routes.DashboardBuild(buildB.dir), store.OwnerLockProtocol).Scan(&instance); err != nil {
		t.Fatal(err)
	}
	if _, live := dashboardBuildsOf(t, a); !slices.Equal(live, []string{entryA, entryB}) {
		t.Fatalf("with a beating process on B's build: live %q, want both builds", live)
	}
	// A's live window is three of its heartbeat (main.go sets it so, the ratio of production's
	// processes.LiveWindow to its processes.HeartbeatInterval).
	if _, err := pair.database.Pool.Exec(ctx, `
		update server_processes set heartbeat_at = now() - make_interval(secs => $2) where instance = $1::uuid`,
		instance, (stagedPastTheRun*(processes.LiveWindow/processes.HeartbeatInterval)).Seconds()+1); err != nil {
		t.Fatal(err)
	}
	if _, live := dashboardBuildsOf(t, a); !slices.Equal(live, []string{entryA}) {
		t.Fatalf("after its heartbeat aged past the live window: live %q, want %q alone", live, entryA)
	}
}

// ───── the keyword fill ─────────────────────────────────────────────────────

// The keyword fill (textversions.Run) writes search_text_versions under its hold,
// search-text-versions-fill, one process at a time: the rows below read which process wrote each
// of its batches from the cursor row's holder, and what duty_holds said of the hold in that
// batch's own transaction, both recorded by a trigger of the row's own (recordKeywordFillWrites),
// against the holder each process's line names when it takes the hold (keywordFillHolder). None
// of them needs NATS, so every process here runs without it.

// keywordFillHoldName is the fill's hold, textversions' fillHoldName, which that package keeps to
// itself: the name of the duty_holds row every batch of the fill checks names its holder live.
const keywordFillHoldName = "search-text-versions-fill"

// The line a holder logs as it finds its hold lost, which the frozen holder's row waits for beside
// the two lines of main_test.go (keywordFillTookLine, keywordFillStandbyLine).
const keywordFillLostHoldLine = `msg="dispatch: the keyword fill's hold is lost; it writes nothing more and stands by"`

// keywordFillTakeOverWithin is the service level a standby is held to in taking the hold of a
// holder that renews it no more, killed or frozen: the hold's whole 15 s TTL, since a holder
// stopped the moment after a renewal leaves the hold its full life to lapse on the database's
// clock; one 10 s retry of the standby's, which finds it lapsed; and a margin for the take and a
// first pass. Only the timing-lane rows read it (TestTheKeywordFillMovesWithinItsBoundWhenItsHolderIsKilled,
// TestAFrozenKeywordFillHolderIsTakenOverWithinItsBound).
const keywordFillTakeOverWithin = 15*time.Second + 10*time.Second + 5*time.Second

// A killed holder does not release its hold, so its hold lapses and the process that stood by takes
// it and writes the events written since, whose rows wait on the event horizon, which waits on the
// database's open event writes (textversionstest.AwaitRows). Every batch before the kill is the
// first process's and every batch after is the second's, and every event has exactly its row. How
// soon after the kill the second process takes the hold is a service level, which
// TestTheKeywordFillMovesWithinItsBoundWhenItsHolderIsKilled holds in benchtime's timing lane.
func TestTheKeywordFillMovesToTheOtherProcessWhenItsHolderIsKilled(t *testing.T) {
	keywordFillTakeOverAfterKill(t)
}

// The process that stood by takes a killed holder's hold within keywordFillTakeOverWithin of the
// kill. The rows of the events written after the kill wait on the event horizon, which waits on
// the database's open event writes, so no span bounds them. The processes' timers are inside child
// processes, which no clock of the test's reaches, so the reading is of the machine's clock, and
// the row runs only in benchtime's timing lane (benchtime.Timing); that the hold moves at all is
// TestTheKeywordFillMovesToTheOtherProcessWhenItsHolderIsKilled's, which the suite runs.
func TestTheKeywordFillMovesWithinItsBoundWhenItsHolderIsKilled(t *testing.T) {
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	if took := keywordFillTakeOverAfterKill(t); took > keywordFillTakeOverWithin {
		t.Fatalf("the second process took the keyword fill's hold %v after the kill; want within %v", took.Round(time.Millisecond), keywordFillTakeOverWithin)
	}
}

// keywordFillTakeOverAfterKill is TestTheKeywordFillMovesToTheOtherProcessWhenItsHolderIsKilled's
// run, and answers how long after the kill, on the machine's clock, the second process's take was
// read.
func keywordFillTakeOverAfterKill(t *testing.T) time.Duration {
	t.Helper()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	writes := recordKeywordFillWrites(t, database)
	url := databaseURLOf(t, database)
	first := startDispatchProcessWith(t, url, dispatchOptions{SigningKey: multiprocessSigningKey})
	first.waitHealthy(t)
	firstHolder := keywordFillHolder(t, first)
	second := startDispatchProcessWith(t, url, dispatchOptions{SigningKey: multiprocessSigningKey})
	second.waitHealthy(t)
	second.waitForLog(t, keywordFillStandbyLine)

	before := writeKeywordFillComments(t, database, 50)
	textversionstest.AwaitRows(t, database, before, nil)
	first.kill(t)
	killed := time.Now()
	after := writeKeywordFillComments(t, database, 50)
	secondHolder := keywordFillHolder(t, second)
	took := time.Since(killed)
	textversionstest.AwaitRows(t, database, after, nil)
	t.Logf("the second process took the hold %v and wrote the events written after the kill %v after it", took.Round(time.Millisecond), time.Since(killed).Round(time.Millisecond))

	requireKeywordFillWritersInTurn(t, writes(), firstHolder, secondHolder)
	textversionstest.RequireOnlyRowsOf(t, database, textversionstest.OneEach(slices.Concat(before, after)...))
	return took
}

// A process killed while it catches up a log of 20,000 events leaves its cursor where its last
// batch committed, and the process that starts after it continues from that cursor: its first batch
// starts past it, the rows the killed process wrote are left as they are, and at the end every
// event has exactly its row, none written twice and none missed.
//
// The row lets the first process's batches through one at a time on an exclusive lock of the
// cursor's table (keywordFillStepper) and kills the process, the lock held, once the cursor has
// reached the 2,000th event: no batch of it can commit after the cursor read under the lock, so
// that cursor is the one it left, however fast the database writes the backlog and however long
// the test waits between its reads. Every wait of the row is bounded by the test binary's deadline
// alone.
func TestTheKeywordFillContinuesFromItsCursorInANewProcess(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	writes := recordKeywordFillWrites(t, database)
	ids := writeKeywordFillComments(t, database, 20000)
	last := ids[len(ids)-1]
	url := databaseURLOf(t, database)
	step := keywordFillStepper(t, database)
	first := startDispatchProcessWith(t, url, dispatchOptions{SigningKey: multiprocessSigningKey})
	firstHolder := keywordFillHolder(t, first)
	// Four batches of 500 events, out of forty.
	left := step.through(t, ids[1999])
	first.kill(t)
	step.release(t)
	if left >= last {
		t.Fatalf("the first process was killed at cursor %d, past the last event %d: it had caught up already", left, last)
	}
	kept := keywordRowsThrough(t, database, left)

	second := startDispatchProcessWith(t, url, dispatchOptions{SigningKey: multiprocessSigningKey})
	secondHolder := keywordFillHolder(t, second)
	testwait.Until(t, "the new process to write every event's rows", func() (int64, bool) {
		cursor := textversionstest.Cursor(t, database)
		return cursor, cursor == last
	})

	recorded := writes()
	requireKeywordFillWritersInTurn(t, recorded, firstHolder, secondHolder)
	for i, write := range recorded {
		if write.holder != secondHolder {
			continue
		}
		if i == 0 || recorded[i-1].eventID != left || write.eventID <= left {
			t.Errorf("the new process's first batch moved the cursor to %d after %v; want a batch past the cursor %d the killed process left",
				write.eventID, recorded[max(i-1, 0)], left)
		}
		break
	}
	if got := keywordRowsThrough(t, database, left); got != kept {
		t.Errorf("the rows of the events through the cursor %d were %s when the first process was killed and are %s now: the new process wrote them again",
			left, kept, got)
	}
	textversionstest.RequireOnlyRowsOf(t, database, textversionstest.OneEach(ids...))
}

// A holder frozen with its connections open - a long garbage collection pause, a host that stops
// scheduling it - renews nothing, so its hold lapses on the database's clock and the standby takes
// it and writes the events written meanwhile. Once the frozen process resumes it finds its hold
// lost, says so and stands by, and writes nothing: every batch from the standby's first on is the
// standby's, across a window of events written after the resume, and every event has exactly its
// row. A batch the freeze caught in its transaction is ended by the batch's own
// idle_in_transaction_session_timeout, and any batch the resumed process begins finds in
// duty_holds that the hold is not its own (filler.batch). How soon after the freeze the standby
// takes the hold is a service level, which TestAFrozenKeywordFillHolderIsTakenOverWithinItsBound
// holds in benchtime's timing lane.
func TestAFrozenKeywordFillHolderIsTakenOverAndWritesNothingOnceResumed(t *testing.T) {
	keywordFillTakeOverAfterFreeze(t)
}

// The standby takes a frozen holder's hold within keywordFillTakeOverWithin of the freeze. The row
// runs only in benchtime's timing lane (benchtime.Timing), for the reason
// TestTheKeywordFillMovesWithinItsBoundWhenItsHolderIsKilled gives; that the standby takes the hold
// and the resumed holder writes nothing is
// TestAFrozenKeywordFillHolderIsTakenOverAndWritesNothingOnceResumed's, which the suite runs.
func TestAFrozenKeywordFillHolderIsTakenOverWithinItsBound(t *testing.T) {
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	if took := keywordFillTakeOverAfterFreeze(t); took > keywordFillTakeOverWithin {
		t.Fatalf("the standby took the frozen holder's hold %v after the freeze; want within %v", took.Round(time.Millisecond), keywordFillTakeOverWithin)
	}
}

// keywordFillTakeOverAfterFreeze is TestAFrozenKeywordFillHolderIsTakenOverAndWritesNothingOnceResumed's
// run, and answers how long after the freeze, on the machine's clock, the standby's take was read.
func keywordFillTakeOverAfterFreeze(t *testing.T) time.Duration {
	t.Helper()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	writes := recordKeywordFillWrites(t, database)
	url := databaseURLOf(t, database)
	frozen := startDispatchProcessWith(t, url, dispatchOptions{SigningKey: multiprocessSigningKey, OwnProcessGroup: true})
	frozen.waitHealthy(t)
	frozenHolder := keywordFillHolder(t, frozen)
	standby := startDispatchProcessWith(t, url, dispatchOptions{SigningKey: multiprocessSigningKey})
	standby.waitHealthy(t)
	standby.waitForLog(t, keywordFillStandbyLine)

	before := writeKeywordFillComments(t, database, 20)
	textversionstest.AwaitRows(t, database, before, nil)
	frozenAt := frozen.signal(t, syscall.SIGSTOP)
	resumed := false
	// So a failure never leaves a stopped process behind: the cleanup cmdtest.Start registered
	// when the process started kills it, and runs after this one, which is registered later.
	t.Cleanup(func() {
		if !resumed {
			_ = frozen.Cmd.Process.Signal(syscall.SIGCONT)
		}
	})
	waitFrozen(t, frozen)

	whileFrozen := writeKeywordFillComments(t, database, 20)
	standbyHolder := keywordFillHolder(t, standby)
	took := time.Since(frozenAt)
	textversionstest.AwaitRows(t, database, whileFrozen, nil)
	t.Logf("the standby took the hold %v and wrote the events written meanwhile %v after the holder was frozen",
		took.Round(time.Millisecond), time.Since(frozenAt).Round(time.Millisecond))

	resumedAt := frozen.signal(t, syscall.SIGCONT)
	resumed = true
	// Its hold's loss deadline passed while it was stopped, so its timer finds the hold lost as
	// it resumes, or its batch's next statement fails on a transaction the database ended.
	frozen.waitForLog(t, keywordFillLostHoldLine)
	frozen.waitForLog(t, keywordFillStandbyLine)
	t.Logf("the resumed process reported its hold lost and stood by %v after it resumed", time.Since(resumedAt).Round(time.Millisecond))
	// Events across three of the hold's renewal intervals after the resume, in which the resumed
	// process's timers and wakes have every turn to write them. What the row checks after them is an
	// absence (no write of the resumed process), so each pause is a span held for its why.
	var afterResume []int64
	for range 6 {
		afterResume = append(afterResume, writeKeywordFillComments(t, database, 5)...)
		testwait.Span(t, 2500*time.Millisecond, "a resumed process that writes nothing leaves no state; it is a child process out of synctest's reach; and the standby's later writes do not show that the resumed one held back")
	}
	textversionstest.AwaitRows(t, database, afterResume, nil)

	requireKeywordFillWritersInTurn(t, writes(), frozenHolder, standbyHolder)
	textversionstest.RequireOnlyRowsOf(t, database, textversionstest.OneEach(slices.Concat(before, whileFrozen, afterResume)...))
	select {
	case <-frozen.Exited:
		t.Fatalf("the resumed process exited; it should stand by for the hold:\n%s", frozen.Output.String())
	default:
	}
	return took
}

// keywordFillWrite is one write of the fill's cursor row: the holder that wrote it, the cursor it
// left, and what duty_holds said of the fill's hold as the batch wrote its cursor - holdHolder, the
// holder its row named ("" where there was no row), and holdLive, whether that row was live by the
// test the batch's own check makes (expires_at > now(), the transaction's clock; textversions'
// holdCheckQuery). The batch makes that check earlier in the same transaction, in a statement of
// its own; this is a second read of the hold, at the cursor write.
type keywordFillWrite struct {
	holder     string
	eventID    int64
	holdHolder string
	holdLive   bool
}

// held is what the hold's row said at the write.
func (w keywordFillWrite) held() string {
	switch {
	case w.holdHolder == "":
		return "no row at all"
	case w.holdLive:
		return "holder " + w.holdHolder + ", live"
	default:
		return "holder " + w.holdHolder + ", lapsed"
	}
}

// recordKeywordFillWrites installs a trigger on the fill's cursor row that records each write of it
// in its own transaction, so a batch that commits is recorded and one that rolls back is not, with
// what duty_holds says of the fill's hold (keywordFillHoldName) at that write - the trigger reads
// it as the batch writes its cursor (textversions' advanceCursorQuery), after the batch's own
// check of the hold - and answers the reader of what it recorded, in order.
func recordKeywordFillWrites(t *testing.T, database *store.Store) func() []keywordFillWrite {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		create table keyword_fill_writes (n bigserial primary key, holder text not null, event_id bigint not null,
			hold_holder text not null, hold_live boolean not null);
		create function keyword_fill_write() returns trigger language plpgsql as $$
		begin
			insert into keyword_fill_writes (holder, event_id, hold_holder, hold_live)
			select coalesce(new.holder::text, ''), new.event_id, coalesce(h.holder::text, ''), coalesce(h.expires_at > now(), false)
			from (select 1) as write left join duty_holds h on h.name = '`+keywordFillHoldName+`';
			return new;
		end $$;
		create trigger keyword_fill_write after insert or update on search_text_versions_fill
			for each row execute function keyword_fill_write();
	`); err != nil {
		t.Fatalf("record the keyword fill's writes: %v", err)
	}
	return func() []keywordFillWrite {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `select holder, event_id, hold_holder, hold_live from keyword_fill_writes order by n`)
		if err != nil {
			t.Fatalf("read the keyword fill's writes: %v", err)
		}
		recorded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (keywordFillWrite, error) {
			var write keywordFillWrite
			err := row.Scan(&write.holder, &write.eventID, &write.holdHolder, &write.holdLive)
			return write, err
		})
		if err != nil {
			t.Fatalf("read the keyword fill's writes: %v", err)
		}
		return recorded
	}
}

// keywordFillHolderPattern reads the holder of the line a process logs as it takes the fill's hold.
var keywordFillHolderPattern = regexp.MustCompile(`holder=(\S+)`)

// keywordFillHolder waits, through the shared wait (waitForLog), for the process to take the
// fill's hold, and answers the holder it took it as.
func keywordFillHolder(t *testing.T, p *dispatchProcess) string {
	t.Helper()
	p.waitForLog(t, keywordFillTookLine)
	line := p.logLines(keywordFillTookLine)[0]
	holder := keywordFillHolderPattern.FindStringSubmatch(line)
	if holder == nil {
		t.Fatalf("the line that took the keyword fill's hold names no holder: %s", line)
	}
	return holder[1]
}

// lineTime is the moment a slog text line carries in its time= attribute.
func lineTime(t *testing.T, line string) time.Time {
	t.Helper()
	for _, match := range logAttribute.FindAllStringSubmatch(line, -1) {
		if match[1] == "time" {
			at, err := time.Parse(time.RFC3339Nano, match[2])
			if err != nil {
				t.Fatalf("read the time of the line %q: %v", line, err)
			}
			return at
		}
	}
	t.Fatalf("the line %q carries no time", line)
	return time.Time{}
}

// requireKeywordFillWritersInTurn requires that every recorded write of the cursor row is by first
// or second, and by the holder duty_holds named live for the fill's hold in that write's own
// transaction; and that second writes at least once, the take-over the row makes, every write
// before its first being first's. It reads the product's own record and nothing else: no process's
// line, and no line's time. Each batch locks the cursor row and then checks, in a statement of its
// own and at READ COMMITTED, that duty_holds names its holder live for the hold, and writes nothing
// otherwise (textversions' lockCursorQuery and holdCheckQuery); a holder takes the hold only where
// its row is absent, lapsed or already its own (duty's takeHold); and a batch in flight as the
// hold is lost rolls back (textversions' fillUnder). The trigger reads the hold again later in the
// batch, at its cursor write, so a recorded write by a holder duty_holds does not name live there
// is a batch that committed after its hold moved: another process took the hold between the
// batch's check and its cursor write, and the batch did not roll back. The hold may lawfully move
// again after the take-over - a loaded machine lets a holder's renewals run late, its hold
// lapses, and the other process takes it and writes - and each write of such a move is by the
// holder duty_holds names live at it, whenever either process logs what it found.
func requireKeywordFillWritersInTurn(t *testing.T, recorded []keywordFillWrite, first, second string) {
	t.Helper()
	tookOver := false
	for i, write := range recorded {
		switch {
		case write.holder != first && write.holder != second:
			t.Fatalf("write %d of the cursor row is %+v, by a holder neither %s nor %s: %+v", i, write, first, second, recorded)
		case write.holder != write.holdHolder || !write.holdLive:
			t.Fatalf("write %d of the cursor row moved it to event %d as %s, while duty_holds held %s for the fill's hold %s as the batch wrote its cursor, later in its transaction than its own check of the hold: a batch committed after its hold moved, where a batch in flight as its hold is lost rolls back. Want every write by the holder duty_holds names live at it: %+v",
				i, write.eventID, write.holder, write.held(), keywordFillHoldName, recorded)
		}
		tookOver = tookOver || write.holder == second
	}
	if !tookOver {
		t.Fatalf("the cursor row was never written by %s, which took the hold: %+v", second, recorded)
	}
}

// keywordFillSteps lets the keyword fill's batches through one at a time. Each batch locks the
// cursor row (textversions' lockCursorQuery) before it writes and holds that lock to its end, so
// an exclusive lock of the cursor's table held here holds every batch back, while reads of the
// cursor go on. held is the transaction holding it.
type keywordFillSteps struct {
	database *store.Store
	held     pgx.Tx
}

// keywordFillStepper takes the exclusive lock of the cursor's table before any fill runs.
func keywordFillStepper(t *testing.T, database *store.Store) *keywordFillSteps {
	t.Helper()
	return &keywordFillSteps{database: database, held: keywordFillLockCursorTable(t, database)}
}

// keywordFillLockCursorTable begins a transaction and asks for the exclusive lock of the cursor's
// table in it, and answers the transaction once the lock is held. It is ended when the test ends,
// whatever else ends it before.
func keywordFillLockCursorTable(t *testing.T, database *store.Store) pgx.Tx {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin a lock of the keyword fill's cursor: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(context.Background(), `lock table search_text_versions_fill in exclusive mode`); err != nil {
		t.Fatalf("lock the keyword fill's cursor: %v", err)
	}
	return tx
}

// through lets batches through, one at a time, until the cursor has reached target, and answers
// the cursor then, the lock held: the fill can write nothing more until release.
//
// Each step waits for a batch to wait on the lock, queues a second lock behind that batch
// (QueuedBehind, through the holder's own connection), and ends the first: the batch runs, and the
// second lock is granted once it has committed or rolled back, before any batch begun after it.
// So the cursor read under the second lock is the one that batch left, and no batch runs while
// this test reads it or waits, however long. A batch that gives its 5 s wait on the lock up before
// the first lock ends, as when this test stalls that long inside a step, lets that step pass none,
// and the next step tries again: only a stall at every step keeps the row from going on, and the
// binary's deadline ends that.
func (s *keywordFillSteps) through(t *testing.T, target int64) int64 {
	t.Helper()
	ctx := context.Background()
	for {
		var cursor int64
		if err := s.held.QueryRow(ctx,
			`select coalesce((select event_id from search_text_versions_fill where id = 1), 0)`).Scan(&cursor); err != nil {
			t.Fatalf("read the keyword fill's cursor under its lock: %v", err)
		}
		if cursor >= target {
			return cursor
		}
		testwait.Until(t, "a batch of the keyword fill to wait on its cursor", func() (int, bool) {
			waiting, err := storetest.LockWaiters(ctx, s.held)
			if err != nil {
				t.Fatal(err)
			}
			return waiting, waiting >= 1
		})
		next, err := s.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the next lock of the keyword fill's cursor: %v", err)
		}
		// A failure while the next lock's request waits ends the lock it waits behind first and
		// then waits for the request, so no two statements ever run on its connection at once.
		// That request waits behind the batch in the dispatch process, whose kill cleanup was
		// registered before this one and so runs after it, so the wait goes through the shared
		// wait: a batch that never ends fails it by name.
		prev, locked, asked := s.held, make(chan error, 1), false
		t.Cleanup(func() {
			if asked {
				_ = prev.Rollback(context.Background())
				testwait.Receive(t, "the next lock of the keyword fill's cursor to answer once the lock it waited behind ended", locked)
			}
			_ = next.Rollback(context.Background())
		})
		var pid uint32
		if err := next.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatalf("read the next lock's backend: %v", err)
		}
		asked = true
		go func() {
			_, err := next.Exec(ctx, `lock table search_text_versions_fill in exclusive mode`)
			locked <- err
		}()
		testwait.Until(t, "the next lock of the keyword fill's cursor to queue behind the batch", func() (string, bool) {
			queued, err := storetest.QueuedBehind(ctx, s.held, pid)
			if err != nil {
				t.Fatalf("read the lock queue of the keyword fill's cursor: %v", err)
			}
			if queued {
				return "queued", true
			}
			var locks string
			if err := s.held.QueryRow(ctx, `
				select coalesce(string_agg(mode || case when granted then ' granted' else ' waiting' end, ', '), 'none')
				  from pg_locks where pid = $1 and relation = 'search_text_versions_fill'::regclass`, pid).Scan(&locks); err != nil {
				t.Fatalf("read the next lock's locks of the keyword fill's cursor table: %v", err)
			}
			waiting, err := storetest.LockWaiters(ctx, s.held)
			if err != nil {
				t.Fatalf("count the backends waiting behind the keyword fill's cursor: %v", err)
			}
			return fmt.Sprintf("backend %d is not queued behind the batch: its locks of the cursor's table: %s; %d backends wait behind the held lock",
				pid, locks, waiting), false
		})
		if err := s.held.Rollback(ctx); err != nil {
			t.Fatalf("let a batch of the keyword fill through: %v", err)
		}
		err = testwait.Receive(t, "the next lock of the keyword fill's cursor once the batch ended", locked)
		asked = false
		if err != nil {
			t.Fatalf("lock the keyword fill's cursor after the batch: %v", err)
		}
		s.held = next
	}
}

// release ends the lock, letting the fill's batches run again.
func (s *keywordFillSteps) release(t *testing.T) {
	t.Helper()
	if err := s.held.Rollback(context.Background()); err != nil {
		t.Fatalf("release the keyword fill's cursor: %v", err)
	}
}

// keywordRowsThrough is what tells the keyword table's rows of the events through cursor apart
// from the same rows written again: how many there are and the transactions that wrote them.
func keywordRowsThrough(t *testing.T, database *store.Store, cursor int64) string {
	t.Helper()
	var count int
	var writers string
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), coalesce(string_agg(distinct xmin::text, ',' order by xmin::text), '')
		  from search_text_versions where event_id <= $1
	`, cursor).Scan(&count, &writers); err != nil {
		t.Fatalf("read the keyword rows through event %d: %v", cursor, err)
	}
	return fmt.Sprintf("%d rows written by transactions [%s]", count, writers)
}
