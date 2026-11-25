package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// promptShutdown bounds a SIGTERM's whole ordered shutdown with a dashboard open. Closing the
// sockets, finishing the requests in flight and settling one small document take milliseconds; a
// connection that held the HTTP drain open would cost the scene's whole drain share, which ends at
// the test's give-up there (endsAtTheGiveUp), and httpDrainBeforeDocuments, eleven seconds, in a
// deployment.
const promptShutdown = 3 * time.Second

// stagedPastTheRun is what a row stages a bound or a TTL the product judges against its own clock
// at, where the row needs the product never to reach it: a settle delay, an intake's hold, a
// peer's heartbeat, the expiry of an installation token a GitHub stand-in answers. It is longer
// than any go test -timeout CI gives this package (the multiprocess job's 35 minutes, every other
// lane's under 80), so no run reaches it. It is staged state, not a wait: no wait of a test reads
// it, and each wait the rows make still ends at the shared wait's give-up (testwait).
const stagedPastTheRun = 24 * time.Hour

// streamedSessionID is the session whose conversation the test's agent view watches; an agent
// view names a session uuid.
const streamedSessionID = "01a0e090-4848-7473-acc5-fc96e6a646d3"

// A deploy stops Dispatch while people have the dashboard open: every tab holds a live socket
// subscribed to the event feed, and one on an agent's page holds that conversation's agent view on
// its socket too. SIGTERM closes each socket with 1012, ending every subscription it carries, so
// http.Server.Shutdown returns once the requests in flight have, and the document service settles
// the document an edit has just left owing a settlement. The child's own settle timer would run
// that settlement two seconds after the edit (docs.Deps.Settle's default), and on a loaded machine
// the signal can reach the document service after it, so this child is given a settle delay past
// the run (stagedPastTheRun): the edit's settlement is still owed when the signal comes, and
// Shutdown is always the one that runs it. The child's HTTP drain share and its document service's
// budget end at the test's give-up (endsAtTheGiveUp), so however long a loaded machine takes over
// either, neither ends before the test's own waits would give up: the drain ends once the requests
// in flight have, and the settlement once it has run.
func TestSIGTERMWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocument(t *testing.T) {
	sigtermWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocument(t, benchtime.EveryRun)
}

// TestSIGTERMWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocumentOnTheClock is the row above in
// benchtime's timing lane, where the exit is held to promptShutdown (waitPromptExit).
func TestSIGTERMWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocumentOnTheClock(t *testing.T) {
	// Not parallel: it holds a step to a wall-clock bound load could break (waitPromptExit's
	// promptShutdown, 3 s).
	benchtime.TimedRow(t, os.Getenv(benchtime.TimingEnv), sigtermWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocument)
}

// sigtermWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocument is the scene of the two rows
// above; lane says whether its reading of the clock is held to its figure (benchtime.Lane).
func sigtermWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocument(t *testing.T, lane benchtime.Lane) {
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		SettleDelay: stagedPastTheRun,
		Env: map[string]string{
			"DISPATCH_TEST_HTTP_DRAIN_DEADLINE":        endsAtTheGiveUp(t),
			"DISPATCH_TEST_DOCUMENT_SHUTDOWN_DEADLINE": endsAtTheGiveUp(t),
		},
	})
	process.waitHealthy(t)
	_, artifactID := process.createIssue(t)

	agentPage := process.openLiveEvents(t)
	process.watchAgent(t, agentPage, streamedSessionID)
	// The agent page's socket is read from here on, as the page reads it, so it answers the
	// server's pings while the second socket waits for its events_start, which sits behind the
	// event horizon: a socket left unread through that wait would be closed by the server's pong
	// wait (two heartbeats), not by the signal.
	agentPageEnded := socketEnd(agentPage)
	sockets := map[string]<-chan streamEnd{
		"the agent page's live socket": agentPageEnded,
		"the event feed's live socket": socketEnd(process.openLiveEvents(t)),
	}
	process.editOwingSettlement(t, database, artifactID)

	signalled := process.Terminate(t)
	process.waitPromptExit(t, lane, signalled)
	for name, socketEnded := range sockets {
		ended := <-socketEnded
		t.Logf("%s ended %v after SIGTERM (%v)", name, ended.at.Sub(signalled), ended.err)
		// A socket the server closed reads its close frame; one the exiting process cut reads as an
		// abnormal closure, which the page also reconnects from, but which says the server held the
		// socket for as long as the process lived.
		if !gws.IsCloseError(ended.err, gws.CloseServiceRestart) {
			t.Errorf("%s ended with %v, want the server to close it with 1012", name, ended.err)
		}
	}
	// The drain's share ends at the test's give-up, so a drain that no request in flight holds open
	// ends before it, and the line that says the share ran out can come only from a defect that held
	// the drain - one that, with this share, keeps the process up until the give-up, where
	// waitPromptExit fails.
	if output := process.Output.String(); strings.Contains(output, `msg="dispatch: shutdown"`) ||
		strings.Contains(output, "requests still in flight") {
		t.Errorf("HTTP shutdown did not drain before the document service started")
	}
	process.checkSettledAtShutdown(t, database, artifactID)
}

// A request in flight can hold the HTTP drain for its whole share of the budget, here a title change
// queued behind another writer's lock on the issue. The document service starts once that share
// ends, so the settlement owed by the edit before the signal still runs and commits. Settlement waits
// on the same lock. The child's settle delay is past the run (stagedPastTheRun), so the edit's own
// timer never settles it and Shutdown's settlement is the one owed settlement there is; the lock is
// released once that settlement is queued behind it, behind the title change. Shutdown's worker
// starts its settlement only once it has read that the document owes one, so that settlement's
// place in the queue is the fact the release needs. The document service's budget ends at the
// test's give-up (endsAtTheGiveUp), so the settlement waits on the lock for as long as the row
// takes to see it queued and release the lock, and gives up no sooner than the row's own waits
// would; the drain's share keeps production's eleven seconds, since the row waits for that share to
// run out (its line) and nothing ends sooner for it.
func TestSIGTERMWhileARequestHoldsHTTPShutdownStillSettlesTheOwedDocument(t *testing.T) {
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		SettleDelay: stagedPastTheRun,
		Env:         map[string]string{"DISPATCH_TEST_DOCUMENT_SHUTDOWN_DEADLINE": endsAtTheGiveUp(t)},
	})
	process.waitHealthy(t)
	key, artifactID := process.createIssue(t)
	process.editOwingSettlement(t, database, artifactID)

	holder := lockIssue(t, database, key)
	go func() {
		// Its answer is not this test's.
		_, _ = process.renameIssue(key, "Renamed")
	}()
	// A block-index pass waits on the same lock with the same statement, so a count of waiters
	// could pass with the title change still on its way: watch the title change's own backend.
	waitForRenameBehind(t, database, holder)

	signalled := process.Terminate(t)
	process.WaitForOutput(t, `msg="dispatch: settle documents with requests still in flight"`)
	t.Logf("the document service started with the request in flight %v after SIGTERM", time.Since(signalled))
	// The title change waits on the lock already (waitForRenameBehind); the settlement is named by
	// its transaction's name, since a block-index pass waits on the same lock with the same
	// statement and the same locks.
	waiting := waitForSettlementsBehind(t, database, holder, 1)
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatalf("release the issue lock: %v", err)
	}
	process.WaitExit(t, "SIGTERM")
	t.Logf("released the issue lock with the title change and %d settlement waiting on it; exited %v after SIGTERM", waiting, time.Since(signalled))
	process.checkSettledAtShutdown(t, database, artifactID)
}

// A write in flight at SIGTERM can wait on a lock past the HTTP drain's share, the document
// service's budget and the point a fixed shutdown bound would end it, here a title change behind
// another writer that holds the issue for 27 seconds after the signal, 3 s inside the runtime's
// kill. The database is answering, so Dispatch keeps the request's connection open until the write
// commits and its answer is sent, and only then closes the pool, as the deferred close of the pool
// before this shutdown order did. Each health probe the stop runs meanwhile ends at the test's
// give-up (DISPATCH_TEST_SHUTDOWN_PROBE_DEADLINE): the database answers, and only three probes in a row
// unanswered within their bound would make Dispatch exit with the write uncommitted, which a loaded
// machine could do at production's two seconds a probe.
func TestSIGTERMWhileAWriteWaitsOnALockCommitsItAndAnswersBeforeExit(t *testing.T) {
	t.Parallel()
	const lockHeldAfterSIGTERM = 27 * time.Second
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Env: map[string]string{"DISPATCH_TEST_SHUTDOWN_PROBE_DEADLINE": endsAtTheGiveUp(t)},
	})
	process.waitHealthy(t)
	key, artifactID := process.createIssue(t)
	// The issue's spec settles on its own timer, which would queue behind the lock too.
	waitForSettlementOwed(t, database, artifactID, false)

	holder := lockIssue(t, database, key)
	type answer struct {
		status int
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		status, err := process.renameIssue(key, "Committed after the lock")
		answered <- answer{status, err}
	}()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Dispatch's output:\n%s", process.Output.String())
		}
	})
	// The block-index pass locks the issue too, so a count of the waiters could pass with the
	// title change still on its way: watch the title change's own backend.
	waitForRenameBehind(t, database, holder)

	signalled := process.Terminate(t)
	testwait.Quiet(t, "Dispatch's exit while the write still waits on the lock", process.Exited,
		time.Until(signalled.Add(lockHeldAfterSIGTERM)),
		"the row is about a write held in PostgreSQL across the whole stop: the process writes nothing that says "+
			"'I am still waiting', it is a child process so synctest does not reach it, and the only later action "+
			"there is - releasing the lock - is what ends the wait this span measures")
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatalf("release the issue lock: %v", err)
	}
	process.WaitExit(t, "SIGTERM")
	t.Logf("exited %v after SIGTERM", time.Since(signalled))
	if got := testwait.Receive(t, "the title change's answer once Dispatch exited", answered); got.err != nil || got.status != http.StatusOK {
		t.Errorf("the title change answered %d (%v), want 200", got.status, got.err)
	}
	var title string
	if err := database.Pool.QueryRow(context.Background(), `select title from issues where key = $1`, key).Scan(&title); err != nil {
		t.Fatalf("read the issue's title: %v", err)
	}
	if title != "Committed after the lock" {
		t.Errorf("the issue's title is %q, want the write that waited on the lock, %q", title, "Committed after the lock")
	}
	if code := process.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("Dispatch exited %d after SIGTERM, want 0", code)
	}
	if t.Failed() {
		t.Logf("Dispatch's output:\n%s", process.Output.String())
	}
}

// endsAtTheGiveUp is what a row hands a child process for every DISPATCH_TEST_*_DEADLINE setting
// it sets (DISPATCH_TEST_LISTENER_CALL_DEADLINE among them): the moment this test's own waits give
// up (testwait.GiveUpAt), or none under -timeout 0, in clock.Deadline's written form, so the
// product bound it replaces - a call, a step or a wait the test waits on - ends no sooner than the
// test would give up on it. It is the test's one bound, not a number of its own; a row's quiet
// span still reads the production window it is about (dispatchenvoy.CallWindow, for one).
func endsAtTheGiveUp(t testing.TB) string {
	return clock.DeadlineAt(testwait.GiveUpAt(t)).String()
}

// A deploy can stop Dispatch while a person's send to one session is with the listener. The send's
// request is in flight, so HTTP shutdown waits for it, and its delivery runs on past any cut-off:
// the listener answers after the signal, the attempt settles sent, and only then does Dispatch
// exit. A process that exited first would leave the attempt pending under a claim no process
// holds, which the page's retry under the key reads as a send still on its way.
//
// The send runs on the test's window (endsAtTheGiveUp), so nothing but a defect ends
// it while the listener holds it, and the row holds Dispatch's exit off for exactly CallWindow,
// the longest a deployment's send is held: a shutdown that cut the send anywhere inside that
// window exits within the span.
func TestSIGTERMWithAKeyedSendAtTheListenerSettlesItSentBeforeExit(t *testing.T) {
	t.Parallel()
	const sessionID = "s1"
	held, release := make(chan struct{}), make(chan struct{})
	var holding atomic.Bool
	// Dispatch resolves a send's session by id against its record (`/v1/registry/{id}`); the live
	// list answers the same row for anything that still reads it.
	const session = `{"session_id":"` + sessionID + `","title":"planner","capabilities":["aside","btw","steer"],"last_seen":200}`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/"+sessionID:
			_, _ = w.Write([]byte(session))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte("[" + session + "]"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			if holding.CompareAndSwap(false, true) {
				close(held)
				select {
				case <-release:
				case <-r.Context().Done():
					// Dispatch gave the send up: the listener never answers it.
					return
				}
			}
			_, _ = w.Write([]byte(`{"event_id":"envelope-` + sessionID + `","recipient":"` + sessionID + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	// Registered after the listener's close, so it runs first and a failed test never leaves the
	// held send blocking that close.
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Env: map[string]string{"ENVOY_URL": listener.URL, "DISPATCH_TEST_LISTENER_CALL_DEADLINE": endsAtTheGiveUp(t)},
	})
	process.waitHealthy(t)
	type answer struct {
		httpAnswer
		err error
	}
	answered := make(chan answer, 1)
	go func() {
		sent, err := process.trySend(http.MethodPost, "/api/v1/agents/"+sessionID+"/messages",
			`{"body":"Ship it once the checks are green.","delivery":"steer"}`, asKeyed(dispatchTestLogin, "press-1"))
		answered <- answer{httpAnswer: sent, err: err}
	}()
	testwait.Receive(t, "the send to reach the listener", held)

	signalled := process.Terminate(t)
	testwait.Quiet(t, "Dispatch's exit with the send still at the listener", process.exitedWithOutput(), dispatchenvoy.CallWindow,
		"a process that has not exited records nothing that says so, it is a child process so synctest does not "+
			"reach it, and the later action this row takes - releasing the listener's answer - is what lets it exit; "+
			"the send runs on the test's window, and the span is the production CallWindow, past which a deployment's "+
			"send would have ended by itself, so a shutdown cut inside it is one the span sees")
	releaseOnce()
	t.Logf("the listener answered the send %v after SIGTERM", time.Since(signalled))
	process.WaitExit(t, "the listener's answer")
	t.Logf("exited %v after SIGTERM", time.Since(signalled))
	if code := process.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("Dispatch exited %d after SIGTERM, want 0", code)
	}

	got := testwait.Receive(t, "the send's answer once Dispatch exited", answered)
	var message struct {
		ID         string `json:"id"`
		Deliveries []struct {
			Attempt int    `json:"attempt"`
			State   string `json:"state"`
		} `json:"deliveries"`
	}
	if got.err != nil || got.status != http.StatusCreated || json.Unmarshal(got.body, &message) != nil ||
		len(message.Deliveries) != 1 || message.Deliveries[0].State != "sent" {
		t.Fatalf("the send answered %d %s (%v), want 201 with its one attempt sent:\n%s", got.status, got.body, got.err, process.Output.String())
	}
	// Read once the process is gone: what it wrote before it exited is all there is.
	rows, err := database.Pool.Query(context.Background(),
		`select attempt, state from message_deliveries where message_id = $1 order by attempt`, message.ID)
	if err != nil {
		t.Fatalf("read the send's attempts: %v", err)
	}
	defer rows.Close()
	var attempts []string
	for rows.Next() {
		var attempt int
		var state string
		if err := rows.Scan(&attempt, &state); err != nil {
			t.Fatalf("read an attempt: %v", err)
		}
		attempts = append(attempts, strconv.Itoa(attempt)+" "+state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the send's attempts: %v", err)
	}
	if !slices.Equal(attempts, []string{"1 sent"}) {
		t.Fatalf("the send's attempts after exit = %v, want [1 sent]", attempts)
	}
	// The send's Idempotency-Key kept its answer inside the drain, holding its message, so the
	// page's retry against the next process is answered from it rather than sent again.
	var status *int
	var recovered string
	if err := database.Pool.QueryRow(context.Background(),
		`select status, coalesce(recovery ->> 'message_id', '') from request_keys where idempotency_key = 'press-1'`,
	).Scan(&status, &recovered); err != nil || status == nil || *status != http.StatusCreated || recovered != message.ID {
		t.Fatalf("the send's key after exit: status %v, recovery %q (%v), want 201 holding message %s", status, recovered, err, message.ID)
	}
}

// A broadcast answers as soon as its messages are committed and delivers behind the response, so
// a deploy can stop Dispatch with recipients of it still at the listener. Those deliveries are
// not requests, so the HTTP drain does not cover them: shutdown waits for them itself
// (api.Register's wait), each on a context the signal does not cancel. The listener answers the
// held recipient after the signal, every recipient's attempt settles sent, and only then does
// Dispatch exit. Bound to the process lifetime instead, the signal cut the held recipient off
// and left every recipient the workers had not reached with a pending attempt no process holds.
//
// The held recipient's send runs on the test's window (endsAtTheGiveUp), and the row
// holds Dispatch's exit off for exactly CallWindow, the longest a deployment's send is held: a
// shutdown that cut the send anywhere inside that window exits within the span.
func TestSIGTERMWithABroadcastAtTheListenerSettlesEveryRecipientBeforeExit(t *testing.T) {
	t.Parallel()
	const heldSession = "s2"
	// Each selected session is resolved by id against its own record (`/v1/registry/{id}`).
	sessions := map[string]string{
		"s1": `{"session_id":"s1","title":"Planner","capabilities":["btw","steer"],"last_seen":300}`,
		"s2": `{"session_id":"s2","title":"Tester","capabilities":["btw","steer"],"last_seen":200}`,
		"s3": `{"session_id":"s3","title":"Reviewer","capabilities":["btw","steer"],"last_seen":100}`,
	}
	held, release := make(chan struct{}), make(chan struct{})
	var holding atomic.Bool
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			row, ok := sessions[strings.TrimPrefix(r.URL.Path, "/v1/registry/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(row))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte("[" + sessions["s1"] + "," + sessions["s2"] + "," + sessions["s3"] + "]"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode the broadcast's send: %v", err)
			}
			if request.TargetSession == heldSession && holding.CompareAndSwap(false, true) {
				close(held)
				select {
				case <-release:
				case <-r.Context().Done():
					// Dispatch gave the recipient up: the listener never answers it.
					return
				}
			}
			_, _ = w.Write([]byte(`{"event_id":"envelope-` + request.TargetSession + `","recipient":"` + request.TargetSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	// Registered after the listener's close, so it runs first and a failed test never leaves the
	// held recipient blocking that close.
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Env: map[string]string{"ENVOY_URL": listener.URL, "DISPATCH_TEST_LISTENER_CALL_DEADLINE": endsAtTheGiveUp(t)},
	})
	process.waitHealthy(t)
	if answer := process.send(t, http.MethodPost, "/api/v1/broadcasts",
		`{"body":"Status before the deploy?","delivery":"btw","session_ids":["s1","s2","s3"]}`,
		asKeyed(dispatchTestLogin, "press-1")); answer.status != http.StatusCreated {
		t.Fatalf("broadcast: status %d, want 201", answer.status)
	}
	testwait.Receive(t, "the broadcast to reach the listener", held)

	signalled := process.Terminate(t)
	testwait.Quiet(t, "Dispatch's exit with a recipient still at the listener", process.exitedWithOutput(), dispatchenvoy.CallWindow,
		"a process that has not exited records nothing that says so, it is a child process so synctest does not "+
			"reach it, and the later action this row takes - releasing the listener's answer - is what lets it exit; "+
			"the held recipient's send runs on the test's window, and the span is the production CallWindow, past which "+
			"a deployment's send would have ended by itself, so a shutdown cut inside it is one the span sees")
	releaseOnce()
	t.Logf("the listener answered the held recipient %v after SIGTERM", time.Since(signalled))
	process.WaitExit(t, "the listener's answer")
	t.Logf("exited %v after SIGTERM", time.Since(signalled))
	if code := process.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("Dispatch exited %d after SIGTERM, want 0", code)
	}

	// Read once the process is gone: what it wrote before it exited is all there is.
	rows, err := database.Pool.Query(context.Background(), `
		select m.target, d.attempt, d.state
		from messages m join message_deliveries d on d.message_id = m.id
		where m.broadcast_id is not null
		order by m.target, d.attempt
	`)
	if err != nil {
		t.Fatalf("read the broadcast's attempts: %v", err)
	}
	defer rows.Close()
	var attempts []string
	for rows.Next() {
		var target, state string
		var attempt int
		if err := rows.Scan(&target, &attempt, &state); err != nil {
			t.Fatalf("read an attempt: %v", err)
		}
		attempts = append(attempts, target+" "+strconv.Itoa(attempt)+" "+state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the broadcast's attempts: %v", err)
	}
	want := []string{"session:s1 1 sent", "session:s2 1 sent", "session:s3 1 sent"}
	if !slices.Equal(attempts, want) {
		t.Fatalf("the broadcast's attempts after exit = %v, want %v", attempts, want)
	}
}

// A live socket is hijacked from net/http, so HTTP shutdown does not wait for it. Production
// shutdown must wait for the socket handler and the delivery hub after cancelling Lifetime, before
// it closes the database pool they use.
func TestShutdownWaitsForDeliveryWorkBeforeClosingThePool(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: database, Events: broker})
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	appCtx, err := appContextFor(t, resolveWith(t, nil), routes.AppContextOptions{
		Store: database, Docs: documentService, Events: broker, Lifetime: lifetime,
	})
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}
	handler, background := routes.New(appCtx)
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", http.Header{
		"Authorization": {"Bearer agent-token"},
	})
	if err != nil {
		t.Fatalf("open the live socket: %v (%v)", err, response)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "subscribe", "sub": "pipeline", "view": map[string]string{"kind": "pipeline"}}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// The reads below wait under the shared wait's deadline, so a socket that answers nothing
	// fails the test rather than hangs it.
	deadline, _ := testwait.GiveUpAt(t)
	_ = conn.SetReadDeadline(deadline)
	var refusal struct {
		Type, Sub, Code string
	}
	for refusal.Type != "error" {
		if err := conn.ReadJSON(&refusal); err != nil {
			t.Fatalf("read the delivery hub's answer: %v", err)
		}
	}
	if refusal.Sub != "pipeline" || refusal.Code != "DELIVERY_NOT_CONFIGURED" {
		t.Fatalf("delivery hub answer = %+v, want DELIVERY_NOT_CONFIGURED for pipeline", refusal)
	}

	stop()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	// Released on every path out of the row, a failure included, and shutdown waited for after
	// it, so a failure below never leaves shutdown parked in the hook while the row's database
	// is dropped.
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		releaseOnce()
		testwait.Receive(t, "shutdown to return once the row let its delivery work end", done)
	})
	go func() {
		defer close(done)
		shutdown(clock.Real(), stopBounds{}, server.Config, documentService, database.Pool, func() {
			close(entered)
			<-release
			background.Wait()
		}, func() {}, func() {}, nil)
	}()
	testwait.Receive(t, "shutdown to wait for delivery work", entered)
	if _, err := database.Pool.Exec(context.Background(), "select 1"); err != nil {
		t.Fatalf("the pool closed while shutdown waited for delivery work: %v", err)
	}
	releaseOnce()
	testwait.Receive(t, "shutdown to return once delivery work ended", done)
	if _, _, err := conn.ReadMessage(); !gws.IsCloseError(err, gws.CloseServiceRestart) {
		t.Fatalf("the live socket ended with %v, want 1012", err)
	}
	if _, err := database.Pool.Exec(context.Background(), "select 1"); err == nil {
		t.Fatal("shutdown left the pool open")
	}
}

// stallingPublisher is an outbox publisher whose first send does not return until release is
// closed, so the outbox is inside its batch, holding no database connection, when the process
// context ends. It is always connected, so its connection never changes.
type stallingPublisher struct {
	entered, release chan struct{}
	once             sync.Once
}

func (p *stallingPublisher) Connected() bool { return true }

func (p *stallingPublisher) ConnectionChanged() <-chan struct{} { return nil }

func (p *stallingPublisher) WindowUntil(func() time.Time) bus.Sender { return p }

func (p *stallingPublisher) PublishAsync(contracts.Envelope) <-chan error {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	answer := make(chan error, 1)
	answer <- nil
	return answer
}

func (p *stallingPublisher) Request(string, contracts.Envelope, bus.RequestOptions) <-chan error {
	return p.PublishAsync(contracts.Envelope{})
}

func (p *stallingPublisher) PublishStoredAsync(string, contracts.Envelope, string) <-chan error {
	return p.PublishAsync(contracts.Envelope{})
}

func (p *stallingPublisher) Probe(time.Duration) error { return nil }

// A graceful stop of the process that holds the publisher hold waits for its outbox before it
// closes the pool: the outbox, still inside a send when the process context ends, records its
// batch and releases the hold through that pool, so the hold's row is gone when Dispatch exits and
// a standby takes the hold at once rather than when it lapses. The shutdown runs on a fake clock
// the row never moves, so its wait for the outbox, armed for outboxStopBudget after it began,
// ends only when the outbox stops.
func TestShutdownWaitsForTheOutboxToReleaseThePublisherHold(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	observer := storetest.SecondProcess(t, database)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: database, Events: broker})
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	appCtx, err := appContextFor(t, resolveWith(t, nil), routes.AppContextOptions{
		Store: database, Docs: documentService, Events: broker, Lifetime: lifetime,
	})
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}
	handler, background := routes.New(appCtx)
	server := httptest.NewServer(handler)
	defer server.Close()

	outboxPool, err := database.Pool.Outbox()
	if err != nil {
		t.Fatalf("open the outbox's pool: %v", err)
	}
	publisher := &stallingPublisher{entered: make(chan struct{}), release: make(chan struct{})}
	released := sync.OnceFunc(func() { close(publisher.release) })
	defer released()
	box := outbox.New(outbox.Deps{Store: &store.Store{Pool: outboxPool}, Publisher: publisher, Broker: broker, Listen: database.Pool.Listener})
	outboxStopped := make(chan struct{})
	go func() {
		defer close(outboxStopped)
		box.Run(lifetime)
	}()
	testwait.Eventually(t, "the outbox to take the publisher hold", box.Publishing)

	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('TT', 'Test', 'alice')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('TT-1', 'TT', 1, 'Outbox at shutdown', '{"kind":"user","id":"alice"}', 'U')
	`); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the event's transaction: %v", err)
	}
	if _, err := broker.Append(ctx, tx, model.Event{
		IssueKey: new("TT-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "8f14e45f-ceea-467a-9c1e-000000000001", IssueKey: new("TT-1"), Body: "sent at shutdown"},
	}); err != nil {
		t.Fatalf("append the event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the event: %v", err)
	}
	testwait.Receive(t, "the outbox to send the event", publisher.entered)

	stop()
	// A fixed moment: nothing the shutdown arms on the fake is compared with the wall clock.
	began := time.Date(2026, time.November, 17, 9, 0, 0, 0, time.UTC)
	fake := clocktest.New(began)
	done := make(chan struct{})
	go func() {
		defer close(done)
		shutdown(fake, stopBounds{}, server.Config, documentService, database.Pool, background.Wait, func() {}, func() {}, outboxStopped)
	}()
	// The fake stands at the shutdown's start, so the wait for the outbox, once armed for its
	// budget (waitForOutbox), is where the shutdown stands: no other timer on the fake is due then,
	// and only the outbox's stop ends that wait while the fake is not moved.
	fake.AwaitArmedAt(t, "shutdown's wait for the outbox, outboxStopBudget after it began", began.Add(outboxStopBudget))
	select {
	case <-done:
		t.Fatal("shutdown returned with the outbox still inside its batch")
	default:
	}
	if _, err := database.Pool.Exec(ctx, "select 1"); err != nil {
		t.Fatalf("the pool closed while the outbox was still stopping: %v", err)
	}
	released()
	testwait.Receive(t, "shutdown to return once the outbox stopped", done)
	var holds int
	if err := observer.Pool.QueryRow(ctx, `select count(*) from duty_holds where name = 'dispatch-outbox'`).Scan(&holds); err != nil {
		t.Fatalf("read the publisher hold: %v", err)
	}
	if holds != 0 {
		t.Fatal("the publisher hold's row outlived the shutdown: the outbox did not release it before the pool closed")
	}
	if _, err := database.Pool.Exec(ctx, "select 1"); err == nil {
		t.Fatal("shutdown left the pool open")
	}
}

// A graceful stop waits for the events outbox until outboxStopBudget after it began and no longer:
// an outbox that never stops, its database having stopped answering inside its batch, would
// otherwise hold the pool open and Dispatch running until the runtime's kill. Shutdown reads the
// outbox only through outboxStopped, so one that never closes is an outbox that never stops. The
// shutdown runs on a fake clock the row moves to the budget's end once the wait is armed for it:
// the shutdown then warns that it closes the pool with the outbox still stopping (waitForOutbox),
// closes the pool and returns.
func TestShutdownGivesUpOnAnOutboxThatNeverStopsAtItsBudget(t *testing.T) {
	database := storetest.Open(t)
	documentService := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	logged := capturedLog(t)
	// A fixed moment: nothing the shutdown arms on the fake is compared with the wall clock.
	began := time.Date(2026, time.November, 17, 9, 0, 0, 0, time.UTC)
	fake := clocktest.New(began)
	budget := began.Add(outboxStopBudget)
	outboxStopped := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		shutdown(fake, stopBounds{}, server.Config, documentService, database.Pool, func() {}, func() {}, func() {}, outboxStopped)
	}()
	fake.AwaitArmedAt(t, "shutdown's wait for the outbox, outboxStopBudget after it began", budget)
	select {
	case <-done:
		t.Fatal("shutdown returned before its wait for the outbox reached its budget, with the outbox not stopped")
	default:
	}
	fake.Set(budget)
	testwait.Receive(t, "shutdown to return once its wait for the outbox reached outboxStopBudget", done)
	if !strings.Contains(logged.String(), `msg="dispatch: close the pool with the events outbox still stopping"`) {
		t.Errorf("the shutdown did not warn that it closes the pool with the outbox still stopping:\n%s", logged)
	}
	if _, err := database.Pool.Exec(context.Background(), "select 1"); err == nil {
		t.Fatal("shutdown left the pool open once its wait for the outbox was over")
	}
}

// intakeLeasesOf reserves the intakes' callers of pool's lease pool, as main does while it wires
// the server (delivery.ReserveIntakeLeases).
func intakeLeasesOf(t *testing.T, pool *store.Pool) delivery.IntakeLeases {
	t.Helper()
	callers, err := pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	leases, err := delivery.ReserveIntakeLeases(callers)
	if err != nil {
		t.Fatalf("reserve the intakes' callers of the lease pool: %v", err)
	}
	return leases
}

// githubAnswersAfterSignal is how long into the stop the fake GitHub of
// TestShutdownWaitsForAnIntakeHandlerBeforeClosingThePool holds a merged pull request's fetch: a
// GitHub call that answers a few seconds into the stop, as a REST call can. It is the stand-in's
// own latency, not a window the row leans on: the row gives the intakes a hold past its run
// (delivery.IntakeBounds' HoldTTL, stagedPastTheRun) and a stop that ends at the test's give-up
// (its StopDeadline), and its App client calls that end at the give-up too (testGitHubApp), so a
// stop that waits for the handler sees it finish rather than give up however long a loaded machine
// takes. A stop that closed the pool first would close it inside these seconds, and nothing the
// product reports marks that moment for a gate.
const githubAnswersAfterSignal = 3 * time.Second

// An intake handler waiting on GitHub when the signal comes holds no database connection, so on an
// otherwise idle server nothing else keeps the pool open. Shutdown waits for the handler before it
// closes the pool: GitHub answers a few seconds into the stop, and the handler's writes land and its
// message is acknowledged. With the pool closed under it, the handler's write failed and its
// message was acknowledged all the same, which left the event to the reconcile minutes later. The
// intakes' hold is past the run and their stop ends at the test's give-up, so neither a stop that
// gives the handler up at production's fifteen seconds nor a hold lost to a renewal late on a
// loaded machine (which gives the handler up too, and hands its message back) can stand in for the
// stop's wait.
func TestShutdownWaitsForAnIntakeHandlerBeforeClosingThePool(t *testing.T) {
	database := storetest.Open(t)
	// The fetched pull request's author is outside the population, so the handler deletes any row
	// it has and records the event's time: two writes to the pool once GitHub has answered.
	if _, err := delivery.PutSettings(context.Background(), database.Pool, delivery.DeliverySettings{
		DeployRepo:           "acme/deploy",
		DeployWorkflowPath:   ".github/workflows/deploy.yaml",
		PRChecksWorkflowPath: ".github/workflows/pr.yaml",
		ProductionJobNames:   []string{"production"},
		PopulationAuthors:    []string{"octocat"},
	}, model.Actor{Kind: "user", ID: dispatchTestLogin}); err != nil {
		t.Fatalf("seed the delivery settings: %v", err)
	}
	_, natsURL := testnats.Start(t)
	publisher, err := bus.ConnectOwningStream([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, publisher) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	var enter sync.Once
	github := http.NewServeMux()
	github.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "app_slug": "shutdown-test",
			"permissions": map[string]string{"contents": "read", "actions": "read", "pull_requests": "read"},
		})
	})
	github.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_shutdown", "expires_at": time.Now().Add(stagedPastTheRun).Format(time.RFC3339)})
	})
	github.HandleFunc("GET /repos/acme/widgets/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		enter.Do(func() { close(entered) })
		select {
		case <-ctx.Done():
		case <-r.Context().Done():
			return
		}
		select {
		case <-time.After(githubAnswersAfterSignal):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "title": "Widgets", "html_url": "https://github.com/acme/widgets/pull/7",
			"user": map[string]any{"login": "someone-else"}, "created_at": "2026-10-01T00:00:00Z",
			"merged_at": time.Now().UTC().Format(time.RFC3339),
		})
	})
	github.HandleFunc("GET /repos/acme/widgets/pulls/7/commits", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"sha": "abc", "commit": map[string]any{"author": map[string]any{"date": "2026-10-01T00:00:00Z"}}}})
	})
	githubServer := httptest.NewServer(github)
	t.Cleanup(githubServer.Close)
	app := testGitHubApp(t, githubServer.URL)

	stopIntakes, err := delivery.StartIntakes(ctx, func(name string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL}, bus.WithName(name))
	}, database.Pool, intakeLeasesOf(t, database.Pool), app, events.NewBroker(), nil, nil, delivery.NewAttributor(refs.Renames{}),
		delivery.IntakeBounds{HoldTTL: stagedPastTheRun, StopDeadline: clock.DeadlineAt(testwait.GiveUpAt(t))})
	if err != nil {
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(stopIntakes)
	consumer := func() *natsgo.ConsumerInfo {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
		if err != nil {
			return nil
		}
		return info
	}
	testwait.Eventually(t, "the intake's durable to be bound", func() bool {
		info := consumer()
		return info != nil && info.PushBound
	})

	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets", "number": "7", "author": "octocat",
	})
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	if err := publisher.Publish(contracts.Envelope{
		EventID: "shutdown-pr-7", Source: "github", SourceEventID: "shutdown-pr-7", TraceID: "shutdown-pr-7",
		Topic: contracts.GithubResourceSubject("acme", "widgets", "pr", "7"), Payload: string(payload), IssuedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("publish the merged pull request: %v", err)
	}
	testwait.Receive(t, "the intake to fetch the merged pull request", entered)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	documentService := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	served := make(chan error, 1)
	go func() {
		// As main does: the stop of the intakes, which begins as ctx is cancelled, is what shutdown
		// waits for beside the delivery sockets.
		served <- serveUntilStopped(ctx, cancel, stopBounds{}, &http.Server{Handler: http.NotFoundHandler()}, listener, documentService,
			database.Pool, func() {}, stopIntakes, func() {}, nil, nil)
	}()
	cancel()
	if err := testwait.Receive(t, "the shutdown to end", served); err != nil {
		t.Fatalf("serveUntilStopped: %v", err)
	}

	reader, err := pgx.Connect(context.Background(), database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("connect to read the delivery settings: %v", err)
	}
	defer reader.Close(context.Background())
	var recorded *time.Time
	if err := reader.QueryRow(context.Background(), `select last_event_at from delivery_settings`).Scan(&recorded); err != nil {
		t.Fatalf("read last_event_at: %v", err)
	}
	// The intakes' hold is past the run and their stop ends at the test's give-up, so the handler
	// finishes rather than being given up. Its acknowledgement is on its way to the server once the
	// stop returns.
	var info *natsgo.ConsumerInfo
	testwait.Eventually(t, "the message to be acknowledged", func() bool {
		info = consumer()
		return info != nil && info.AckFloor.Consumer >= 1 && info.NumAckPending == 0
	})

	if recorded == nil {
		t.Fatalf("the message was acknowledged but its handler's writes never landed (last_event_at null): the pool closed "+
			"under the handler; consumer delivered=%d ack_floor=%d", info.Delivered.Consumer, info.AckFloor.Consumer)
	}
}

// testGitHubApp is a GitHub App client signing with a key of the test's own and asking baseURL. Its
// calls end at the test's give-up (githubapptest.RequestDeadlineAtTheGiveUp), since a row's
// stand-in for GitHub holds a call while the row takes its next step: at githubapp.RequestTimeout
// a held call ends by itself whenever that step comes more than 20 s after the hold.
func testGitHubApp(t *testing.T, baseURL string) *githubapp.Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the test App key: %v", err)
	}
	app, err := githubapp.New(&auth.AppConfig{
		ClientID: "Iv1.shutdown",
		PEM:      string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	}, baseURL, githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t)))
	if err != nil || app == nil {
		t.Fatalf("build the GitHub App client: %v", err)
	}
	return app
}

// A panic in main once the intakes have started runs main's deferred stop of them on its way out
// (stopIntakesOnExit), and the panic still reaches the runtime, which prints it and ends the
// process. The intakes' Runs return only once the process context ends, and main's only cancel of
// it is deferred before the intakes start, so it runs after their stop: a stop that waited for them
// without cancelling first never returned, and the process hung with the panic unprinted while its
// intake kept consuming.
func TestAPanicAfterTheIntakesStartEndsTheProcess(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	_, natsURL := testnats.Start(t)
	// The publisher owns the stream, as the server's does, so the intakes bind their durables.
	publisher, err := bus.ConnectOwningStream([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, publisher) })
	githubServer := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(githubServer.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopIntakes, err := delivery.StartIntakes(ctx, func(name string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL}, bus.WithName(name))
	}, database.Pool, intakeLeasesOf(t, database.Pool), testGitHubApp(t, githubServer.URL), events.NewBroker(), nil, nil, delivery.NewAttributor(refs.Renames{}))
	if err != nil {
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(stopIntakes)

	const boot = "boot failed after the intakes started"
	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		defer stopIntakesOnExit(cancel, stopIntakes)
		panic(boot)
	}()
	if got := testwait.Receive(t, "the panic to get past the intakes' deferred stop", recovered); got != boot {
		t.Fatalf("the panic that reached the top of the stack = %v, want %q", got, boot)
	}
	if ctx.Err() == nil {
		t.Fatal("the deferred stop returned with the process context still live")
	}
}

// lockIssue holds the issue's owner lock (store.LockOwner) in a transaction of the test's own, as
// another writer of the issue would, and rolls it back when the test ends if the test has not.
func lockIssue(t *testing.T, database *store.Store, key string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if err := store.LockOwner(ctx, holder, store.IssueOwner(key)); err != nil {
		t.Fatalf("lock the issue: %v", err)
	}
	return holder
}

// waitForRenameBehind waits for the title change's backend to queue behind holder's lock on the
// issue, logs it, and returns its process id. The title change runs in the Dispatch process, so
// its backend is named by what it waits for and what it has read. It waits, not granted, for the
// issue's owner lock: the advisory lock holder holds other than the owner lock gate, which every
// write of this build takes shared before it waits on an owner (store.OwnerLockGateNamespace,
// migration 20261115200014). It runs the owner-lock statement a PATCH sends before it reads any
// issue's document (lockIssueOwners in internal/dispatch/api/issue_parent.go), and it holds no
// lock of artifacts. Every document write that waits on the same lock - the edit's settlement,
// the block-index pass - reads its artifact's row in the same transaction before it takes the
// owner lock (lockArtifactOwner in internal/dispatch/docs/service.go), so it holds a lock of
// artifacts while it waits, and no lock fact other than that one tells it from the title change:
// both run the same statement for the same key, and neither holds another advisory lock while it
// waits. The wait has no bound of its own (testwait.Until), and a try that finds no such
// backend reads every backend it found waiting on the issue's lock.
func waitForRenameBehind(t *testing.T, database *store.Store, holder pgx.Tx) uint32 {
	t.Helper()
	renamer := waitForOwnerLockWaiters(t, database, holder, "the title change to queue behind the issue lock",
		"the owner-lock statement with no lock of artifacts", 1, func(w ownerLockWaiter) bool {
			return w.query == store.OwnerLockStatement && !w.holds("artifacts")
		})[0]
	t.Logf("the title change's backend %d runs %q and waits on the advisory lock %s, the issue's owner lock", renamer.pid, renamer.query, renamer.lock)
	return renamer.pid
}

// waitForSettlementsBehind waits until want settlements of the issue's document wait on the
// issue's lock that holder holds, and answers how many do then. In a process with the test hooks
// on, as every process these rows start is, a settlement runs the owner-lock statement in a
// transaction that names itself docs.SettlementApplicationName in pg_stat_activity; every other
// document write that waits on the same lock - the block-index pass, an edit, a rebuild - runs the
// same statement on the same key with the same locks, and only that name tells them apart.
func waitForSettlementsBehind(t *testing.T, database *store.Store, holder pgx.Tx, want int) int {
	t.Helper()
	return len(waitForOwnerLockWaiters(t, database, holder, fmt.Sprintf("%d settlements of the document to queue behind the issue lock", want),
		"settlements, named "+docs.SettlementApplicationName, want, func(w ownerLockWaiter) bool {
			return w.query == store.OwnerLockStatement && w.app == docs.SettlementApplicationName
		}))
}

// ownerLockWaiter is a backend that waits, not granted, on an advisory lock a holder holds other
// than the owner lock gate: its pid, the lock it waits on (classid/objid/objsubid), its
// application name and statement as pg_stat_activity shows them, and the relations it holds a lock
// of, which are what its transaction has read.
type ownerLockWaiter struct {
	pid              uint32
	lock, app, query string
	relations        []string
}

// holds reports whether the waiter holds a lock of relation.
func (w ownerLockWaiter) holds(relation string) bool {
	for _, held := range w.relations {
		if held == relation {
			return true
		}
	}
	return false
}

func (w ownerLockWaiter) String() string {
	return fmt.Sprintf("backend %d (application %q, locks of %v) runs %q", w.pid, w.app, w.relations, w.query)
}

// waitForOwnerLockWaiters waits until want of the backends that wait on an advisory lock holder
// holds, other than the owner lock gate (store.OwnerLockGateNamespace, which every write of this
// build takes shared before it waits on an owner), are ones meant reports, and returns those. The
// code under test runs in the Dispatch process, so a waiter is named by what pg_locks and
// pg_stat_activity show of it, read through the pool, a fresh view on each try; named says what
// meant takes, for a try that finds too few, which reads every other waiter.
func waitForOwnerLockWaiters(t *testing.T, database *store.Store, holder pgx.Tx, what, named string, want int, meant func(ownerLockWaiter) bool) []ownerLockWaiter {
	t.Helper()
	ctx := context.Background()
	var holderPID uint32
	if err := holder.QueryRow(ctx, `select pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("read the lock holder's backend: %v", err)
	}
	var found []ownerLockWaiter
	testwait.Until(t, what, func() (string, bool) {
		rows, err := database.Pool.Query(ctx, `
			select w.pid, w.classid::text || '/' || w.objid::text || '/' || w.objsubid::text,
			       coalesce(a.application_name, ''), coalesce(a.query, ''),
			       array(select r.relation::regclass::text from pg_locks r
			             where r.pid = w.pid and r.locktype = 'relation' and r.granted order by 1)
			from pg_locks w left join pg_stat_activity a on a.pid = w.pid
			where w.locktype = 'advisory' and not w.granted
			  and (w.classid, w.objid, w.objsubid) in (
			    select classid, objid, objsubid from pg_locks
			    where pid = $1 and locktype = 'advisory' and granted
			      and not (objsubid = 2 and classid = $2::oid and objid = $3::oid))
			order by w.pid
		`, holderPID, int64(store.OwnerLockGateNamespace), int64(store.OwnerLockGateKey))
		if err != nil {
			t.Fatalf("read the backends waiting on the lock holder's owner locks: %v", err)
		}
		waiters, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ownerLockWaiter, error) {
			var w ownerLockWaiter
			return w, row.Scan(&w.pid, &w.lock, &w.app, &w.query, &w.relations)
		})
		if err != nil {
			t.Fatalf("read the backends waiting on the lock holder's owner locks: %v", err)
		}
		found = nil
		var others []string
		for _, w := range waiters {
			if meant(w) {
				found = append(found, w)
			} else {
				others = append(others, w.String())
			}
		}
		return fmt.Sprintf("%d of the %d backends wanted (%s) wait on the owner locks backend %d holds; the others waiting on them: %v",
			len(found), want, named, holderPID, others), len(found) >= want
	})
	return found
}

// waitForIssueCreateBehind waits for the issue create's backend to queue behind holder's lock of
// the project's row, and answers its pid. The create's update of the project's next_number is
// the statement that queues there, and an issue create is its one writer (api/issue_create.go:
// "next_number is written by an issue create alone"), where every other writer of a projects row
// - an event append, a mapping write, the default assignee's PUT, the gated steps' publish -
// writes another column, so the statement names the create's backend. It reads pg_stat_activity
// through the pool, a fresh view on each try.
func waitForIssueCreateBehind(t *testing.T, database *store.Store, holder pgx.Tx) uint32 {
	t.Helper()
	ctx := context.Background()
	var holderPID uint32
	if err := holder.QueryRow(ctx, `select pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatalf("read the project lock holder's backend: %v", err)
	}
	var creator uint32
	testwait.Until(t, "the issue create to queue behind the project's row", func() (string, bool) {
		var blocked []string
		rows, err := database.Pool.Query(ctx, `
			select pid, coalesce(query, '') from pg_stat_activity where $1 = any(pg_blocking_pids(pid)) order by pid`, holderPID)
		if err != nil {
			t.Fatalf("read the backends blocked by the project lock holder: %v", err)
		}
		type session struct {
			pid   uint32
			query string
		}
		sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (session, error) {
			var s session
			return s, row.Scan(&s.pid, &s.query)
		})
		if err != nil {
			t.Fatalf("read the backends blocked by the project lock holder: %v", err)
		}
		for _, s := range sessions {
			if strings.Contains(s.query, "set next_number = next_number + 1") {
				creator = s.pid
				return fmt.Sprintf("backend %d", s.pid), true
			}
			blocked = append(blocked, fmt.Sprintf("backend %d runs %q", s.pid, s.query))
		}
		return fmt.Sprintf("no backend blocked by backend %d updates next_number; blocked by it: %v", holderPID, blocked), false
	})
	return creator
}

// renameIssue changes the issue's title as the test's login and returns the status Dispatch
// answered. It reports failures rather than ending the test, so a goroutine can call it.
func (p *dispatchProcess) renameIssue(key, title string) (int, error) {
	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return 0, err
	}
	answer, err := p.trySend(http.MethodPatch, "/api/v1/issues/"+key, string(body), asUser(dispatchTestLogin))
	if err != nil {
		return 0, err
	}
	return answer.status, nil
}

// A deploy usually finds someone with a document open: the spec tab holds the document's websocket,
// so its room is loaded with an editor connected. The test gives its test-only process a settle
// delay past the run (stagedPastTheRun), so only Shutdown can version the edit, and a document
// service budget that ends at the test's give-up (endsAtTheGiveUp), so the settlement runs however
// long a loaded machine takes over it, up to the moment the test's own waits would give up.
func TestSIGTERMWithAnEditorConnectedSettlesItsDocumentBeforeExit(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		SettleDelay: stagedPastTheRun,
		Env:         map[string]string{"DISPATCH_TEST_DOCUMENT_SHUTDOWN_DEADLINE": endsAtTheGiveUp(t)},
	})
	process.waitHealthy(t)
	_, artifactID := process.createIssue(t)
	editor := process.openEditor(t, artifactID, "before\n")
	// The initial spec is already owed. Its row makes the Shutdown settlement load and settle this
	// room, so wait for the editor's durable update to advance that same row before signalling.
	waitForSettlementOwed(t, database, artifactID, true)
	marked := settlementMarkedAt(t, database, artifactID)
	replaceText(t, editor, "before", "after")
	waitForSettlementMarkedAfter(t, database, artifactID, marked)

	signalled := process.Terminate(t)
	process.WaitExit(t, "SIGTERM")
	t.Logf("exited %v after SIGTERM", time.Since(signalled))
	testwait.Receive(t, "the editor's connection to end once Dispatch exited", editor.Ended)
	t.Logf("the editor's connection ended %v after SIGTERM", editor.EndedAt().Sub(signalled))
	process.checkSettledAtShutdown(t, database, artifactID)
	number, markdown, authors := latestVersion(t, database, artifactID)
	if number != 2 || markdown != "after\n" || !strings.Contains(authors, `"`+dispatchTestLogin+`"`) {
		t.Errorf("the document's latest version is %d holding %q by %s, want version 2 holding the editor's edit, %q, credited to %s",
			number, markdown, authors, "after\n", dispatchTestLogin)
	}
	for _, line := range strings.Split(process.Output.String(), "\n") {
		if strings.Contains(line, "room="+artifactID) {
			t.Logf("Dispatch logged: %s", line)
		}
	}
}

// A database that stops answering - a failover, a partition - holds every connection waiting on it,
// and closing the pool waits for every connection in use. Here that is an issue create's: it waits
// in Postgres on another writer's lock on its project's row when the relay stops forwarding, so
// whatever Postgres answers it never arrives. HTTP shutdown waits for that request, so the document
// service starts httpDrainBeforeDocuments after the signal, with no document to settle: the issue
// it would create has none yet. Dispatch then gives up on the connection when silentProbes health
// probes in a row have failed with no connection taken back meanwhile, so it exits well inside a
// runtime's grace period instead of being killed, and says why.
func TestSIGTERMWithTheDatabaseUnansweringExits(t *testing.T) {
	exited := exitAfterSIGTERMWithTheDatabaseUnanswering(t)
	t.Logf("exited %v after SIGTERM", exited)
}

// TestSIGTERMWithTheDatabaseUnansweringExitsWithinTheShutdownBudget is the service level that
// stop promises: the process is gone well inside a runtime's grace period rather than killed.
// The budget is spent by timers inside the child process - its HTTP drain, and silentProbes
// health probes each with its own two seconds and poll interval - which no clock of ours
// reaches, so the reading is of the machine's clock and the row runs only in benchtime's timing
// lane. That it exits at all, saying why, is the row above, which every run proves.
func TestSIGTERMWithTheDatabaseUnansweringExitsWithinTheShutdownBudget(t *testing.T) {
	// Not parallel: it holds a step to a wall-clock bound load could break (the shutdown budget the
	// exit is compared against).
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	exited := exitAfterSIGTERMWithTheDatabaseUnanswering(t)
	// The request in flight holds the HTTP drain for its whole share, then the document service
	// returns at once with no room to settle, and each failed probe takes its two seconds and a
	// poll interval. The slack is a loaded box's jitter on this real binary.
	if bound := httpDrainBeforeDocuments + silentProbes*(2*time.Second+drainPollInterval) + 5*time.Second; exited > bound {
		t.Errorf("Dispatch exited %v after SIGTERM with the database unanswering, want within %v", exited, bound)
	}
}

// exitAfterSIGTERMWithTheDatabaseUnanswering runs the row above's scene - a request in flight,
// waiting in Postgres behind another writer's lock, when the relay in front of the database
// stops forwarding - signals the process, and answers how long after the signal it exited. It
// fails t unless the process said both of the things this scene is about.
func exitAfterSIGTERMWithTheDatabaseUnanswering(t *testing.T) time.Duration {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	relay := startDatabaseRelay(t, database.Pool.Config().ConnString())
	process := startDispatchProcess(t, relay.url)
	process.waitHealthy(t)
	process.call(t, http.MethodPost, "/api/v1/projects", `{"key":"STOP","name":"Shutdown"}`, http.StatusCreated)

	// An issue create takes its number from the project's row, so it queues behind this lock.
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `select 1 from projects where key = 'STOP' for no key update`); err != nil {
		t.Fatalf("lock the project: %v", err)
	}
	// It is never answered: the process exits with it in flight.
	go func() {
		_, _ = process.trySend(http.MethodPost, "/api/v1/issues", `{"project":"STOP","title":"Never answered"}`, asUser(dispatchTestLogin))
	}()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Dispatch's output:\n%s", process.Output.String())
		}
	})
	waitForIssueCreateBehind(t, database, holder)

	relay.freeze()
	signalled := process.Terminate(t)
	process.WaitExit(t, "SIGTERM with the database unanswering")
	exited := time.Since(signalled)
	output := process.Output.String()
	if !strings.Contains(output, `msg="dispatch: settle documents with requests still in flight"`) {
		t.Errorf("the issue create was not in flight when the document service started")
	}
	if !strings.Contains(output, "once the database stopped answering") {
		t.Errorf("Dispatch did not say it exited because the database stopped answering")
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "level=WARN") {
			t.Logf("Dispatch logged: %s", line)
		}
	}
	return exited
}

// A database that stops answering during a stop must not keep Dispatch running into the runtime's
// kill: the stop decides to exit once silentProbes health probes in a row have failed, and it has
// to decide inside stopGrace less the reserve its budget keeps there, the time the process has
// left to exit in. The child-process rows above run this scene - a request in flight behind
// another writer's lock when the relay in front of the database freezes - and prove that Dispatch
// exits on every run, but its timers run on the machine's clock, so only the timing lane reads how
// long the exit took, and a stop that ran past stopGrace would fail no other row. This row exists
// to fail it on every run: it calls shutdown in this process on a fake clock and moves the fake to
// each deadline the shutdown arms, so the moment it returns is the sum of the waits it armed, which
// no load stretches.
func TestAShutdownWithTheDatabaseUnansweringDecidesToExitInsideStopGrace(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	// Nothing the frozen database holds comes back until the relay's own cleanup cuts its
	// connections, and closing the pool waits for them, so the close is registered before the
	// relay: cleanups run last first.
	var pool *store.Pool
	t.Cleanup(func() {
		if pool != nil {
			testwait.Returns(t, "every connection of the pool through the relay to come back, so the pool closes", pool.Close)
		}
	})
	relay := startDatabaseRelay(t, database.Pool.Config().ConnString())
	config, err := pgxpool.ParseConfig(relay.url)
	if err != nil {
		t.Fatalf("parse the relay's URL: %v", err)
	}
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool through the relay: %v", err)
	}
	pool = store.NewPool(opened)
	// The probe answers through the relay before it freezes, so its failures after are the freeze's.
	if _, err := pool.Healthy(ctx); err != nil {
		t.Fatalf("health probe through the relay: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('STOP', 'Shutdown', 'alice')`); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `select 1 from projects where key = 'STOP' for no key update`); err != nil {
		t.Fatalf("lock the project: %v", err)
	}
	// The request in flight takes its connection from the pool shutdown watches and queues behind
	// the holder's lock on the project, as the issue create of the rows above does.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the request in flight: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = pool.Exec(r.Context(), `select 1 from projects where key = 'STOP' for no key update`)
	})}
	go func() { _ = server.Serve(listener) }()
	go func() {
		// It is never answered while the test runs, and its answer is not this test's.
		if response, err := http.Post("http://"+listener.Addr().String()+"/", "text/plain", nil); err == nil {
			_ = response.Body.Close()
		}
	}()
	storetest.WaitForLockWaiters(t, holder, 1)
	documentService := docs.New(docs.Deps{Store: &store.Store{Pool: pool}, Events: events.NewBroker()})

	relay.freeze()
	logged := capturedLog(t)
	// A fixed moment: nothing the shutdown arms on the fake is compared with the wall clock.
	began := time.Date(2026, time.November, 17, 9, 0, 0, 0, time.UTC)
	fake := clocktest.New(began)
	done := make(chan struct{})
	go func() {
		defer close(done)
		shutdown(fake, stopBounds{}, server, documentService, pool, func() {}, func() {}, func() {}, nil)
	}()
	// The request holds the HTTP drain, so the first wait the shutdown arms is the drain's share.
	drainEnds := fake.AwaitArmed(t, "the HTTP drain's share", 1)[0]
	fake.Set(drainEnds)
	// The document service's timeout is armed next, at the moment the drain's share ended, and
	// stays armed until shutdown returns. The service has no room to settle here and returns
	// without waiting on it, so the row never moves the fake to it: a move made while the service
	// was still returning would end its timeout under it. Every other wait - each probe's bound and
	// each pause between the probes - the row moves the fake to in turn, earliest first, until
	// shutdown returns.
	documentsEnd := drainEnds.Add(documentShutdownTimeout)
	type step struct {
		to       time.Time
		returned bool
		// armed is every deadline pending when the step was read, which a wait that gives up prints.
		armed []time.Time
	}
	for {
		next := testwait.Until(t, "the shutdown to arm its next wait, or to return", func() (step, bool) {
			select {
			case <-done:
				return step{returned: true}, true
			default:
			}
			armed := fake.Armed()
			for _, at := range armed {
				if !at.Equal(documentsEnd) {
					return step{to: at, armed: armed}, true
				}
			}
			return step{armed: armed}, false
		})
		if next.returned {
			break
		}
		if waited := fake.Now().Sub(began); waited > stopGrace {
			t.Fatalf("the shutdown was still waiting %v after it began, past stopGrace (%v), the runtime's kill; its next wait ends %v after it began",
				waited, stopGrace, next.to.Sub(began))
		}
		fake.Set(next.to)
	}
	decided := fake.Now().Sub(began)
	t.Logf("the shutdown decided to exit %v after it began, on its clock", decided)
	// httpDrainBeforeDocuments is sized so that the document service's budget ends this far inside
	// stopGrace.
	reserve := stopGrace - httpDrainBeforeDocuments - documentShutdownTimeout
	if bound := stopGrace - reserve; decided > bound {
		t.Errorf("the shutdown decided to exit %v after it began with the database unanswering, want at or before %v: stopGrace (%v) less the reserve its budget keeps inside it (%v)",
			decided, bound, stopGrace, reserve)
	}
	if !strings.Contains(logged.String(), `msg="dispatch: settle documents with requests still in flight"`) {
		t.Errorf("the request was not in flight when the document service started:\n%s", logged)
	}
	if !strings.Contains(logged.String(), "once the database stopped answering") {
		t.Errorf("the shutdown did not say it exits because the database stopped answering:\n%s", logged)
	}
}

// A connection taken after shutdown saw the pools idle - ygo's persistence worker compacting a room
// as it exits takes one with no deadline - is in use when the pool closes, and closing the pool
// waits for it. On a database that has stopped answering it never comes back, so closePool gives up
// on it by waitUntilIdle's rule, silentProbes failed health probes in a row with no connection taken
// back, and says so, instead of waiting on it until the runtime kills Dispatch. The test's pool pings
// every connection it hands out, as pgxpool pings one idle for over a second, so the late taker is
// held by a ping the frozen database never answers without the test waiting out that second.
// closePool's pauses and its probes' bounds run on a fake clock the row moves to each wait it arms,
// so the give-up is read on that clock: no later than silentProbes pauses and probe bounds.
func TestClosingThePoolGivesUpOnALateConnectionOnceTheDatabaseStopsAnswering(t *testing.T) {
	// Not parallel: it replaces the process's default logger (capturedLog, logtest.SetDefault).
	ctx := context.Background()
	database := storetest.Open(t)
	relay := startDatabaseRelay(t, database.Pool.Config().ConnString())
	config, err := pgxpool.ParseConfig(relay.url)
	if err != nil {
		t.Fatalf("parse the relay's URL: %v", err)
	}
	config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return true }
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a pool through the relay: %v", err)
	}
	pool := store.NewPool(opened)
	// The probe answers through the relay before it freezes, so its failures after are the freeze's.
	if _, err := pool.Healthy(ctx); err != nil {
		t.Fatalf("health probe through the relay: %v", err)
	}
	if _, err := pool.Exec(ctx, "select 1"); err != nil {
		t.Fatalf("open a shared connection through the relay: %v", err)
	}

	relay.freeze()
	taken := make(chan error, 1)
	go func() {
		conn, err := pool.Acquire(context.Background())
		if err == nil {
			conn.Release()
		}
		taken <- err
	}()
	testwait.Eventually(t, "the late taker to take the idle shared connection", func() bool { return pool.Use().InUse > 0 })

	logged := capturedLog(t)
	// A fixed moment: nothing closePool arms on the fake is compared with the wall clock.
	began := time.Date(2026, time.November, 17, 9, 0, 0, 0, time.UTC)
	fake := clocktest.New(began)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		closePool(fake, pool, clock.Deadline{})
	}()
	type step struct {
		to       time.Time
		returned bool
	}
	for {
		var next step
		testwait.Until(t, "closePool to arm its next wait, or to return", func() (string, bool) {
			select {
			case <-returned:
				next = step{returned: true}
				return "returned", true
			default:
			}
			armed := fake.Armed()
			if len(armed) == 0 {
				return fmt.Sprintf("closePool has not returned, and no wait is armed on its clock at %v", fake.Now()), false
			}
			next = step{to: armed[0]}
			return fmt.Sprintf("waits armed at %v", armed), true
		})
		if next.returned {
			break
		}
		fake.Set(next.to)
	}
	gaveUp := fake.Now().Sub(began)
	t.Logf("closePool gave up %v after it began, on its clock", gaveUp)
	select {
	case err := <-taken:
		t.Errorf("the late taker's connection came back (%v), want it held by the frozen database", err)
	default:
	}
	if !strings.Contains(logged.String(), "once the database stopped answering") {
		t.Errorf("closePool did not say it gave up because the database stopped answering:\n%s", logged.String())
	}
	// healthProbeBound is the bound HealthyOn arms on each probe (store's healthProbeTimeout).
	const healthProbeBound = 2 * time.Second
	if bound := silentProbes * (drainPollInterval + healthProbeBound); gaveUp > bound {
		t.Errorf("closePool gave up %v after it began with the database unanswering, want within %v: %d pauses of %v and %d probes of %v",
			gaveUp, bound, silentProbes, drainPollInterval, silentProbes, healthProbeBound)
	}
}

// databaseRelay forwards Dispatch's database connections to the test's Postgres until frozen, and
// then forwards nothing more in either direction while holding every connection open, as a
// database that has stopped answering does.
type databaseRelay struct {
	url    string
	frozen chan struct{}
	once   sync.Once
}

func (r *databaseRelay) freeze() { r.once.Do(func() { close(r.frozen) }) }

// startDatabaseRelay starts a relay to the database databaseURL names and returns it with url, the
// same URL through the relay.
func startDatabaseRelay(t *testing.T, databaseURL string) *databaseRelay {
	t.Helper()
	target, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse the database URL: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the database relay: %v", err)
	}
	relay := &databaseRelay{frozen: make(chan struct{})}
	done := make(chan struct{})
	var conns sync.WaitGroup
	t.Cleanup(func() {
		close(done)
		_ = listener.Close()
		conns.Wait()
	})
	upstream := target.Host
	through := *target
	through.Host = listener.Addr().String()
	relay.url = through.String()
	forward := func(to, from net.Conn) {
		buffer := make([]byte, 32<<10)
		for {
			n, err := from.Read(buffer)
			select {
			case <-relay.frozen:
				<-done
				return
			default:
			}
			if n > 0 {
				if _, writeErr := to.Write(buffer[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = client.Close()
				continue
			}
			conns.Add(2)
			go func() {
				defer conns.Done()
				forward(server, client)
			}()
			go func() {
				defer conns.Done()
				forward(client, server)
			}()
			go func() {
				<-done
				_ = client.Close()
				_ = server.Close()
			}()
		}
	}()
	return relay
}

// checkSettledAtShutdown requires the exited process to have stopped in order with the document's
// owed settlement run and confirmed.
func (p *dispatchProcess) checkSettledAtShutdown(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	output := p.Output.String()
	if code := p.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("Dispatch exited %d after SIGTERM, want 0", code)
	}
	if strings.Contains(output, "settlement unconfirmed") {
		t.Errorf("the document service could not confirm the owed settlement")
	}
	if !strings.Contains(output, `msg="dispatch: document settled before shutdown" room=`+artifactID) {
		t.Errorf("the document service did not settle the owed document before exiting")
	}
	if settlementOwed(t, database, artifactID) {
		t.Errorf("the document still owes its settlement after the shutdown that ran it")
	}
	if t.Failed() {
		t.Logf("Dispatch's output:\n%s", output)
	}
}

// A process that has exited is not healthy because its port answers: it released the port when it
// exited, and whatever bound the port next answers /healthz in its place. That holds whether it
// exited before it served (its listen failed) or after it logged that it serves. The wait takes
// serving from the process's own output and refuses any process that has exited, so it refuses
// both answers, and takes the same answer from a running process whose output says it serves on
// the port.
func TestTheHealthWaitTakesNoAnswerOnThePortForAProcessThatLostIt(t *testing.T) {
	t.Parallel()
	holder := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(holder.Close)
	port := holder.Listener.Addr().(*net.TCPAddr).Port
	address := "addr=127.0.0.1:" + strconv.Itoa(port)
	listening := `level=INFO msg="dispatch: listening" ` + address
	process := func(output string, exited bool) *dispatchProcess {
		logged := &cmdtest.LockedBuffer{}
		_, _ = logged.Write([]byte(output + "\n"))
		done := make(chan struct{})
		if exited {
			close(done)
		}
		return &dispatchProcess{Process: &cmdtest.Process{Output: logged, Exited: done}, port: port}
	}

	lost := process(`level=ERROR msg="dispatch: listen" `+address+` error="DISPATCH_LISTEN_FD=3: serve on the inherited socket: bad file descriptor"`, true)
	if err := lost.healthy(t); err == nil || !strings.Contains(err.Error(), "exited before it became healthy") {
		t.Errorf("a process that exited on its listen passed as healthy on a port another server holds: %v", err)
	}
	gone := process(listening+"\n"+`level=INFO msg="dispatch: shutting down"`, true)
	if err := gone.healthy(t); err == nil || !strings.Contains(err.Error(), "exited before it became healthy") {
		t.Errorf("a process that logged it serves and then exited passed as healthy on a port another server holds: %v", err)
	}
	bound := process(listening, false)
	if err := bound.healthy(t); err != nil {
		t.Errorf("a running process that logged it serves, on a port that answers, was not healthy: %v", err)
	}
}

// A process that exits before it is healthy fails its test with the reason at the head of the
// failure, before the whole output. An exit for any reason quotes the output's last lines. An exit
// on a connection Postgres refused for too many clients (SQLSTATE 53300) quotes that line instead
// and names the server's max_connections and the client backends open on it then
// (connectionRefusalCause): the connection ceiling check counts only this binary's Dispatch
// processes, so a server whose slots other clients hold passes it, and the process fails later.
func TestAProcessThatExitsBeforeItIsHealthySaysWhy(t *testing.T) {
	// Not parallel: its verdict reads every client backend of the Postgres server.
	exited := func(output, databaseURL string) error {
		logged := &cmdtest.LockedBuffer{}
		_, _ = logged.Write([]byte(output))
		done := make(chan struct{})
		close(done)
		process := &dispatchProcess{Process: &cmdtest.Process{Output: logged, Exited: done}, databaseURL: databaseURL}
		return process.healthy(t)
	}
	// cause is what the failure says before the process's whole output.
	cause := func(err error) string {
		if err == nil {
			t.Fatal("a process that exited passed as healthy")
		}
		head, _, found := strings.Cut(err.Error(), "\n"+dispatchOutputHeading+"\n")
		if !found {
			t.Fatalf("the failure does not set the process's output apart under %q:\n%v", dispatchOutputHeading, err)
		}
		return head
	}

	lines := make([]string, 0, exitCauseLines+3)
	for i := range exitCauseLines + 3 {
		lines = append(lines, fmt.Sprintf(`level=INFO msg="dispatch: step %d"`, i+1))
	}
	head := cause(exited(strings.Join(lines, "\n")+"\n", ""))
	for _, line := range lines[len(lines)-exitCauseLines:] {
		if !strings.Contains(head, line) {
			t.Errorf("the failure of a process that exited does not quote %q, one of its last %d lines:\n%s", line, exitCauseLines, head)
		}
	}
	if earlier := lines[len(lines)-exitCauseLines-1]; strings.Contains(head, earlier) {
		t.Errorf("the failure quotes %q, which is not one of the process's last %d lines:\n%s", earlier, exitCauseLines, head)
	}

	database := storetest.Open(t)
	var allowed string
	if err := database.Pool.QueryRow(context.Background(), "select current_setting('max_connections')").Scan(&allowed); err != nil {
		t.Fatalf("read the server's max_connections: %v", err)
	}
	refused := `level=ERROR msg="dispatch: register this server process" error="probe the database: failed to connect to ` +
		"`user=postgres database=dispatch_test_0`: 127.0.0.1:5432 (127.0.0.1): server error: FATAL: sorry, too many clients already (SQLSTATE 53300)\""
	head = cause(exited(`level=INFO msg="dispatch: starting"`+"\n"+refused+"\n"+`level=INFO msg="dispatch duty: leased pass cut short by shutdown"`+"\n",
		database.Pool.Config().ConnString()))
	for _, want := range []string{"exited before it became healthy", refused, "max_connections=" + allowed, "client backends open"} {
		if !strings.Contains(head, want) {
			t.Errorf("the failure of a process Postgres refused for too many clients does not say %q:\n%s", want, head)
		}
	}
}

// A harness that holds the port hands Dispatch the listening socket and sets neither
// DISPATCH_LISTEN_HOST nor DISPATCH_PORT: Dispatch serves on the socket, at the address the kernel
// chose for it, and says that address in its listening line. Without the socket it would bind
// every interface on 8766.
func TestDispatchServesOnTheListenSocketItInheritsWithNoAddressSet(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Without: []string{"DISPATCH_LISTEN_HOST", "DISPATCH_PORT"},
	})
	if environment := strings.Join(process.Cmd.Env, "\n"); strings.Contains(environment, "DISPATCH_PORT=") || strings.Contains(environment, "DISPATCH_LISTEN_HOST=") {
		t.Fatalf("Dispatch was started with DISPATCH_PORT or DISPATCH_LISTEN_HOST set:\n%s", environment)
	}
	// healthy waits for this line and a 200 from /healthz on the held port.
	process.waitHealthy(t)
	if listening := process.listeningLine(); !strings.Contains(process.Output.String(), listening) {
		t.Errorf("Dispatch did not log %q:\n%s", listening, process.Output.String())
	}
}

// A DISPATCH_LISTEN_FD that names something other than a listening socket refuses startup, naming
// the variable, and Dispatch binds nothing in its place: no DISPATCH_PORT, which here names a port
// the test holds, so a bind would log its failure. The refusal comes before Dispatch reads its
// database, so the test needs none.
func TestDispatchRefusesAListenFDThatIsNotASocketAndBindsNothing(t *testing.T) {
	t.Parallel()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold the port DISPATCH_PORT names: %v", err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	notASocket, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cmd := exec.Command(cmdtest.Build(t, "envoy-dispatch"))
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"DATABASE_URL=postgres://nobody@127.0.0.1:1/never-read",
		"DISPATCH_AGENT_TOKEN=shutdown-test-token",
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + strconv.Itoa(occupied.Addr().(*net.TCPAddr).Port),
		"DISPATCH_LISTEN_FD=3",
	}
	cmd.ExtraFiles = []*os.File{notASocket}
	process := cmdtest.Start(t, "Dispatch", cmd)
	process.WaitExit(t, "its start")
	output := process.Output.String()
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("Dispatch exited %d, want 1:\n%s", code, output)
	}
	if !strings.Contains(output, `msg="dispatch: resolve boot config"`) || !strings.Contains(output, "DISPATCH_LISTEN_FD=3 is not a socket") {
		t.Errorf("Dispatch did not refuse its boot naming DISPATCH_LISTEN_FD=3 and what is wrong with it:\n%s", output)
	}
	if strings.Contains(output, `msg="dispatch: listen`) {
		t.Errorf("Dispatch bound or served an address in the descriptor's place:\n%s", output)
	}
}

// dispatchTestLogin is the person the test calls the server as, through header identity.
const dispatchTestLogin = "alice"

// dispatchTestAgentToken is the shared agent token (DISPATCH_AGENT_TOKEN) every process a test
// starts trusts: a bearer caller presenting it acts as the session its request body names.
const dispatchTestAgentToken = "shutdown-test-token"

// dispatchProcess is a Dispatch binary a test started, serving on the loopback port port.
type dispatchProcess struct {
	*cmdtest.Process
	port int
	// database is the Postgres database the test gave the process (DATABASE_URL's). The wait
	// for the event feed's events_start (waitEventsStart) hands it to horizontest.Wait, whose
	// account, when the wait gives up, names a holder of the horizon in that database, with its
	// client and how long its transaction held the horizon back.
	database string
	// databaseURL is the URL the test gave the process (DATABASE_URL). A start that exits before
	// it is healthy reads the server's connection ceiling through it when Postgres refused the
	// process a connection for too many clients (connectionRefusalCause).
	databaseURL string
}

// outputs is the whole output of each of its processes under the label a failure gives it, read
// when it is printed: a wait that returns it as its reading prints it when it gives up, and no
// attempt copies it.
type outputs []labelledProcess

// labelledProcess is one process of outputs and what a failure calls it.
type labelledProcess struct {
	label   string
	process *dispatchProcess
}

func (o outputs) String() string {
	var text strings.Builder
	for i, one := range o {
		if i > 0 {
			text.WriteString("\n")
		}
		fmt.Fprintf(&text, "%s:\n%s", one.label, one.process.Output.String())
	}
	return text.String()
}

// withOutput is a wait's reading and, after it, the output its failure prints: a process's
// Output, or a pair's buffers. testwait.Until formats its last reading only when it gives up, so
// the output is read once, then, and no attempt of the wait copies it.
type withOutput[T any] struct {
	read   T
	output fmt.Stringer
}

// readWith is read, with output after it, as a wait's reading (withOutput).
func readWith[T any](read T, output fmt.Stringer) withOutput[T] {
	return withOutput[T]{read: read, output: output}
}

func (r withOutput[T]) String() string { return fmt.Sprintf("%+v\n%s", r.read, r.output) }

// exitedWithOutput yields the process's Output once the process has exited, for a quiet span
// over its exit (testwait.Quiet): an exit within the span fails with what yielded, and the
// buffer's String prints the process's whole output there, as a reading.
func (p *dispatchProcess) exitedWithOutput() <-chan *cmdtest.LockedBuffer {
	exited := make(chan *cmdtest.LockedBuffer, 1)
	go func() {
		<-p.Exited
		exited <- p.Output
	}()
	return exited
}

// dispatchOptions is what a test varies about the Dispatch process it starts. Its zero value is
// the shutdown tests' process: no NATS, no DISPATCH_SIGNING_KEY, the default settle delay.
type dispatchOptions struct {
	// NATSURL is the NATS server the process publishes to and reads its durables from. Empty
	// sets DISPATCH_NATS_DISABLED=1 instead, which is what every test but the multiprocess
	// ones wants: they have no NATS to reach.
	NATSURL string
	// SigningKey is DISPATCH_SIGNING_KEY, the HMAC key session cookies are signed with. Empty
	// leaves it unset, so the process writes one of its own into its data dir.
	SigningKey string
	// SettleDelay is DISPATCH_TEST_SETTLE_DELAY; zero leaves the server's own default.
	SettleDelay time.Duration
	// Binary is the envoy-dispatch binary to run. Empty builds the command's own, unstamped
	// (cmdtest.Build); a row whose processes serve given builds builds each once with
	// cmdtest.BuildStamped and runs it here, and the two-build test names an earlier build's
	// binary here (twobuild_test.go).
	Binary string
	// Env overrides and adds to the rows below, by name: a row the caller names wins over the
	// one here, and a name not here is added. It is how a test gives the process a GitHub App,
	// a sign-in issuer or cookie identity without every other test growing those rows.
	Env map[string]string
	// WebDist is DISPATCH_WEB_DIST, the web app the process serves. Empty serves the process's own
	// empty home, which is all a test that loads no page needs.
	WebDist string
	// OwnProcessGroup starts the process in a process group of its own, so a test can stop it
	// with SIGSTOP and resume it without the stop reaching the test's own process group: a
	// kernel that hangs up an orphaned process group holding a stopped process (gVisor does)
	// would otherwise end the test with it.
	OwnProcessGroup bool
	// Without names rows below the process does not get at all, as a deployment that leaves
	// them unset.
	Without []string
}

// startDispatchProcess builds Dispatch and starts it on a loopback port it holds against
// databaseURL, with no NATS and nothing from the caller's environment or home, and kills it when the
// test ends if it still runs. DISPATCH_TEST_HOOKS serves the agent conversation relay in-process,
// as the browser harness does, since there is no NATS to relay it from.
func startDispatchProcess(t *testing.T, databaseURL string) *dispatchProcess {
	return startDispatchProcessWith(t, databaseURL, dispatchOptions{})
}

func startDispatchProcessWithSettleDelay(t *testing.T, databaseURL string, settleDelay time.Duration) *dispatchProcess {
	return startDispatchProcessWith(t, databaseURL, dispatchOptions{SettleDelay: settleDelay})
}

// startDispatchProcessWith starts Dispatch under options: a home of its own, nothing from the
// caller's environment, and a loopback port the kernel chose and the test holds from that moment,
// which the process inherits as DISPATCH_LISTEN_FD and serves on, so no other program can take the
// port before it serves. DISPATCH_PORT names the same port, which Dispatch checks against the socket.
func startDispatchProcessWith(t *testing.T, databaseURL string, options dispatchOptions) *dispatchProcess {
	t.Helper()
	// A slot of the lane's server first (dispatchSlots.take): a parallel test waits here for another
	// test's process to exit rather than running more processes than the server is sized for.
	releaseSlot := dispatchSlots.take(t)
	// The server must allow what this process and every other Dispatch process this test binary
	// runs may open (connection_ceiling_test.go), since a server that allows fewer fails later and
	// elsewhere.
	releaseConnections := requireConnectionCeiling(t, databaseURL)
	// The process's own database, for the events_start wait; a URL it cannot serve against is
	// the test's own mistake, not a stall to wait out.
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse the database URL the process is given: %v", err)
	}
	home := t.TempDir()
	binary := options.Binary
	if binary == "" {
		binary = cmdtest.Build(t, "envoy-dispatch")
	}
	webDist := options.WebDist
	if webDist == "" {
		webDist = home
	}
	cmd := exec.Command(binary)
	held := cmdtest.HoldPort(t)
	cmd.Dir = home
	environment := map[string]string{
		"HOME":                             home,
		"XDG_CONFIG_HOME":                  home,
		"XDG_DATA_HOME":                    home,
		"DATABASE_URL":                     databaseURL,
		"DISPATCH_AGENT_TOKEN":             dispatchTestAgentToken,
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
		"DISPATCH_LISTEN_HOST":             "127.0.0.1",
		"DISPATCH_PORT":                    strconv.Itoa(held.Port),
		"DISPATCH_WEB_DIST":                webDist,
		"DISPATCH_TEST_HOOKS":              "1",
		// Both thresholds, so the stall warning arrives and repeats inside a test's patience
		// (horizontest.StallWarning, which the events_start wait keys on).
		"DISPATCH_TEST_HORIZON_STALL_AFTER": horizontest.StallWarning.String(),
		"DISPATCH_TEST_HORIZON_WARN_EVERY":  horizontest.StallWarning.String(),
		// The boot exits 1 when its publish of the dashboard build outruns its bound, a minute in a
		// deployment, and every row waits on a boot: the bound is the test's give-up
		// (endsAtTheGiveUp), so a loaded machine's slow publish is a slow boot and never a failed
		// one before the test's own waits would give up. A binary of an earlier build, which has no
		// such setting, reads nothing of it.
		"DISPATCH_TEST_DASHBOARD_PUBLISH_DEADLINE": endsAtTheGiveUp(t),
	}
	if options.NATSURL == "" {
		environment["DISPATCH_NATS_DISABLED"] = "1"
	} else {
		environment["NATS_URLS"] = options.NATSURL
	}
	if options.SigningKey != "" {
		environment["DISPATCH_SIGNING_KEY"] = options.SigningKey
	}
	if options.SettleDelay > 0 {
		environment["DISPATCH_TEST_SETTLE_DELAY"] = options.SettleDelay.String()
	}
	maps.Copy(environment, options.Env)
	for _, name := range options.Without {
		delete(environment, name)
	}
	cmd.Env = make([]string, 0, len(environment)+1)
	for name, value := range environment {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	held.Pass(cmd, "DISPATCH_LISTEN_FD")
	// Sorted, so a failure's process listing and a test's own log read the same whichever run
	// printed them: Go's map iteration order is not.
	slices.Sort(cmd.Env)
	if options.OwnProcessGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	// The process leaves the count and gives back its slot once it has exited and before Exited
	// closes, so a test that waits on Exited and starts another process is counted with this one
	// gone: a row that restarts a process one after another asks the server for one process's
	// worth, not one per start. A start that fails leaves both at the test's cleanup.
	return &dispatchProcess{Process: cmdtest.StartThen(t, "Dispatch", cmd, func() {
		releaseConnections()
		releaseSlot()
	}), port: held.Port, database: config.Database, databaseURL: databaseURL}
}

func (p *dispatchProcess) url(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(p.port) + path
}

// listeningLine is the line the process writes once it serves on its own port (shutdown.go), as
// a reader of its output matches it: the message and the address, which is the port this test
// holds.
func (p *dispatchProcess) listeningLine() string {
	return `msg="dispatch: listening" addr=127.0.0.1:` + strconv.Itoa(p.port)
}

// waitHealthy waits for the process to be healthy (healthy), and fails the test when it is not.
func (p *dispatchProcess) waitHealthy(t *testing.T) {
	t.Helper()
	if err := p.healthy(t); err != nil {
		t.Fatal(err)
	}
}

// exitCauseLines is how many of its last output lines the failure of a process that exited before
// it was healthy quotes as the reason, when no line of it is a refusal for too many clients.
const exitCauseLines = 5

// dispatchOutputHeading sets a process's whole output apart from the reason its failure gives.
const dispatchOutputHeading = "Dispatch's output:"

// healthy waits for the process to log that it serves on its own port and for /healthz there to
// answer 200 while the process still runs, and says why it did not, with the process's output;
// for a process that exited, the reason its output gives comes first (exitCause). The socket the
// process inherited is not readiness: the kernel queues connections on it from before the process
// starts, and only the process's listening line says it serves them. Nor is an answer once the
// process has exited, which released the port to whatever took it next. The process serves after
// the SIGTERM handler is installed, so a server that answers is one a signal stops in order. A
// process that exits first ends the wait with that error, which is what the rows about a process
// that never serves read.
func (p *dispatchProcess) healthy(t *testing.T) error {
	t.Helper()
	listening := p.listeningLine()
	exited := func() bool {
		select {
		case <-p.Exited:
			return true
		default:
			return false
		}
	}
	ctx := testwait.Context(t)
	notListening := "no line " + strconv.Quote(listening) + " yet"
	var failed error
	testwait.Until(t, "Dispatch to serve on its own port", func() (withOutput[string], bool) {
		if exited() {
			failed = fmt.Errorf("Dispatch exited before it became healthy. %s\n%s\n%s", p.exitCause(), dispatchOutputHeading, p.Output.String())
			return withOutput[string]{}, true
		}
		if !strings.Contains(p.Output.String(), listening) {
			return readWith(notListening, p.Output), false
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url("/healthz"), nil)
		if err != nil {
			failed = err
			return withOutput[string]{}, true
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return readWith("GET /healthz: "+err.Error(), p.Output), false
		}
		// A body read to its end puts its connection back in the client's pool before the
		// caller's next request asks for one. Closed unread, Go's transport drains it after
		// Close returns (since Go 1.27), so a request sent at once dials a second connection
		// and is then handed the first: the second joins the pool and never carries a request.
		// Server.Shutdown counts a connection that has sent nothing as busy until it is more
		// than 5 s old, so that one would hold a SIGTERM's HTTP drain for 5 to 6 s.
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return readWith("/healthz answered "+response.Status, p.Output), response.StatusCode == http.StatusOK && !exited()
	})
	return failed
}

// exitCause is the reason a process that exited before it was healthy gives in its output: the
// first line where Postgres refused it a connection for too many clients, with what the server
// says of its connections now (connectionRefusalCause), or else its last exitCauseLines lines.
func (p *dispatchProcess) exitCause() string {
	output := strings.TrimRight(p.Output.String(), "\n")
	if strings.TrimSpace(output) == "" {
		return "It wrote no output."
	}
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, tooManyConnections) {
			return connectionRefusalCause(line, p.databaseURL)
		}
	}
	last := lines[max(0, len(lines)-exitCauseLines):]
	return fmt.Sprintf("Its last %d line(s):\n%s", len(last), strings.Join(last, "\n"))
}

// waitPromptExit waits for Dispatch to exit after the SIGTERM sent at signalled. How soon it
// exits is a service level its stop promises, measured on this machine's clock against timers
// inside the child process, so the figure is held only by a row's twin in benchtime's timing lane
// (benchtime.OnTheClock); that it exits at all is held on every run.
func (p *dispatchProcess) waitPromptExit(t *testing.T, lane benchtime.Lane, signalled time.Time) {
	t.Helper()
	p.WaitExit(t, "SIGTERM")
	exited := time.Since(signalled)
	t.Logf("exited %v after SIGTERM", exited)
	if lane == benchtime.OnTheClock && exited >= promptShutdown {
		t.Errorf("Dispatch exited %v after SIGTERM, want under %v", exited, promptShutdown)
	}
}

// asUser is the header a signed-in person's request carries (X-Dispatch-User, which these
// processes trust).
func asUser(login string) http.Header {
	return http.Header{"X-Dispatch-User": {login}}
}

// asKeyed is login's header for a write sent under an idempotency key, as the dashboard sends
// one.
func asKeyed(login, key string) http.Header {
	header := asUser(login)
	header.Set("Idempotency-Key", key)
	return header
}

// httpAnswer is one answer of a process's API.
type httpAnswer struct {
	status int
	header http.Header
	body   []byte
}

// code is the answer's error code ({"code": ...}), or "" for an answer that names none.
func (a httpAnswer) code() string {
	var coded struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(a.body, &coded)
	return coded.Code
}

func (a httpAnswer) String() string {
	return fmt.Sprintf("%d %s", a.status, strings.TrimSpace(string(a.body)))
}

// call sends body as the test's login and requires status, returning the response body.
func (p *dispatchProcess) call(t *testing.T, method, path, body string, status int) []byte {
	t.Helper()
	answer := p.send(t, method, path, body, asUser(dispatchTestLogin))
	if answer.status != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, answer.status, status, answer.body)
	}
	return answer.body
}

// send is trySend on the test's goroutine: a process it cannot reach fails the test.
func (p *dispatchProcess) send(t *testing.T, method, path, body string, header http.Header) httpAnswer {
	t.Helper()
	answer, err := p.trySend(method, path, body, header)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return answer
}

// trySend sends body to path as JSON, under header, and answers whatever the process answers. It
// never fails the test, so a goroutine may call it.
func (p *dispatchProcess) trySend(method, path, body string, header http.Header) (httpAnswer, error) {
	request, err := http.NewRequest(method, p.url(path), strings.NewReader(body))
	if err != nil {
		return httpAnswer{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return httpAnswer{}, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return httpAnswer{}, err
	}
	return httpAnswer{status: response.StatusCode, header: response.Header, body: answer}, nil
}

// createIssue creates a project and an issue whose spec is one word, returning the issue's key and
// its spec document's id.
func (p *dispatchProcess) createIssue(t *testing.T) (key, artifactID string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/projects", `{"key":"STOP","name":"Shutdown"}`, http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	created := p.call(t, http.MethodPost, "/api/v1/issues", `{"project":"STOP","title":"Settle at shutdown","spec":"before"}`, http.StatusCreated)
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" || issue.PrimaryArtifactID == "" {
		t.Fatalf("create issue: %v: %s", err, created)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// editOwingSettlement edits the document and requires the edit to have left it owing a settlement.
func (p *dispatchProcess) editOwingSettlement(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		`{"ops":[{"op":"replace","find":"before","with":"after"}]}`, http.StatusOK)
	if !settlementOwed(t, database, artifactID) {
		t.Fatalf("the edit left no pending-settlement row, so the signal would find nothing owed:\n%s", p.Output.String())
	}
}

// streamEnd is when a socket stopped and the read error that stopped it.
type streamEnd struct {
	at  time.Time
	err error
}

// openLiveEvents opens GET /ws/live as a signed-in tab does and subscribes it to the event feed,
// from the head, returning once Dispatch has started the subscription (its events_start frame) with
// the socket the feed's frames arrive on. The frame is behind the event horizon, so the wait for it
// is the server's own stall warning rather than a deadline (live_events_wait_test.go). The socket
// closes when the test ends.
func (p *dispatchProcess) openLiveEvents(t *testing.T) *gws.Conn {
	t.Helper()
	conn, response, err := gws.DefaultDialer.Dial("ws://127.0.0.1:"+strconv.Itoa(p.port)+"/ws/live", http.Header{
		"X-Dispatch-User": {dispatchTestLogin},
	})
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("open the live socket: %v (%s)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]any{"type": "subscribe", "sub": "events", "view": map[string]string{"kind": "events"}}); err != nil {
		t.Fatalf("subscribe the live socket to the event feed: %v", err)
	}
	p.waitEventsStart(t, conn, "events")
	return conn
}

// watchAgent subscribes conn, a live socket, to session's agent view, as an agent's page does,
// returning once Dispatch has answered whether the session is responding (the view's first frame).
func (p *dispatchProcess) watchAgent(t *testing.T, conn *gws.Conn, session string) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"type": "subscribe", "sub": "agent", "view": map[string]string{"kind": "agent", "session_id": session}}); err != nil {
		t.Fatalf("subscribe the live socket to the agent view: %v", err)
	}
	deadline, _ := testwait.GiveUpAt(t)
	_ = conn.SetReadDeadline(deadline)
	for {
		// An event frame's event is an object, an agent frame's a string, so it is read raw.
		var frame struct {
			Type, Sub string
			Event     json.RawMessage
		}
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read the agent view's responder: %v\n%s", err, p.Output.String())
		}
		if frame.Type == "agent" && frame.Sub == "agent" && string(frame.Event) == `"responder"` {
			break
		}
		if frame.Type != "heartbeat" && frame.Type != "event" {
			t.Fatalf("the live socket answered the agent view's subscribe with a %s frame, want its responder", frame.Type)
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
}

// socketEnd reads conn until it ends, reporting when and how on the returned channel.
func socketEnd(conn *gws.Conn) <-chan streamEnd {
	ended := make(chan streamEnd, 1)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				ended <- streamEnd{at: time.Now(), err: err}
				return
			}
		}
	}()
	return ended
}

// openEditor connects a signed-in spec tab to the document at the schema version the dashboard
// presents, so the room takes its edits, and returns once the editor's copy renders want.
func (p *dispatchProcess) openEditor(t *testing.T, artifactID, want string) *docstest.Peer {
	t.Helper()
	return p.openEditorAs(t, dispatchTestLogin, artifactID, want)
}

// openEditorAs is openEditor for login, the person the tab is signed in as (X-Dispatch-User, the
// identity these processes trust).
func (p *dispatchProcess) openEditorAs(t *testing.T, login, artifactID, want string) *docstest.Peer {
	t.Helper()
	editor := docstest.Dial(t, p.documentURL(artifactID), http.Header{"X-Dispatch-User": []string{login}}, artifactID, crdt.New())
	if err := editor.WaitForText(t, markdown, want); err != nil {
		t.Fatalf("%v:\n%s", err, p.Output.String())
	}
	return editor
}

// documentURL is the address of the document's socket on this process, at the schema version the
// dashboard presents.
func (p *dispatchProcess) documentURL(artifactID string) string {
	return "ws://127.0.0.1:" + strconv.Itoa(p.port) + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
}

// markdown renders the editor's copy of the document, read under its lock while the room's updates
// apply, or "" while the copy holds no document it can render.
func markdown(editor *docstest.Peer) string {
	return renderDocument(editor.Doc)
}

// renderDocument renders doc's Proof document, read under its lock, or "" while doc holds no
// document it can render.
func renderDocument(doc *crdt.Doc) string {
	fragment := doc.GetXmlFragment("prosemirror")
	var rendered string
	doc.Transact(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			return
		}
		rendered, _ = pmdoc.Render(tree)
	}, nil)
	return rendered
}

// replaceText types replacement over the first paragraph's text, old, in one keystroke's
// transaction, and sends the room the update it made.
func replaceText(t *testing.T, editor *docstest.Peer, old, replacement string) {
	t.Helper()
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	var changeErr error
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			changeErr = err
			return
		}
		if len(tree.Children) == 0 || len(tree.Children[0].Children) != 1 || tree.Children[0].Children[0].Text != old {
			changeErr = errors.New("the document's first paragraph does not hold only " + old)
			return
		}
		tree.Children[0].Children[0].Text = replacement
		changeErr = pmdoc.Update(txn, fragment, tree)
	}); err != nil || changeErr != nil {
		t.Fatalf("the editor's edit: %v %v", err, changeErr)
	}
}

// settlementOwed reports whether the document's pending-settlement row is there.
func settlementOwed(t *testing.T, database *store.Store, artifactID string) bool {
	t.Helper()
	var owed bool
	if err := database.Pool.QueryRow(context.Background(),
		`select exists (select 1 from doc_settlements_pending where artifact_id = $1)`, artifactID,
	).Scan(&owed); err != nil {
		t.Fatalf("read the pending settlement: %v", err)
	}
	return owed
}

func settlementMarkedAt(t *testing.T, database *store.Store, artifactID string) time.Time {
	t.Helper()
	var marked time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select marked_at from doc_settlements_pending where artifact_id = $1`, artifactID,
	).Scan(&marked); err != nil {
		t.Fatalf("read the pending settlement marker: %v", err)
	}
	return marked
}

func waitForSettlementMarkedAfter(t *testing.T, database *store.Store, artifactID string, before time.Time) {
	t.Helper()
	testwait.Until(t, "the editor's durable update to advance the pending settlement marker", func() (time.Time, bool) {
		marked := settlementMarkedAt(t, database, artifactID)
		return marked, marked.After(before)
	})
}

// waitForSettlementOwed waits for the document's pending-settlement row to be there, or gone.
func waitForSettlementOwed(t *testing.T, database *store.Store, artifactID string, owed bool) {
	t.Helper()
	testwait.Eventually(t, "the document's pending-settlement row to be "+map[bool]string{true: "there", false: "gone"}[owed],
		func() bool { return settlementOwed(t, database, artifactID) == owed })
}

// latestVersion is the number, markdown and authors (JSON) of the document's latest version.
func latestVersion(t *testing.T, database *store.Store, artifactID string) (int, string, string) {
	t.Helper()
	var number int
	var markdown, authors string
	if err := database.Pool.QueryRow(context.Background(), `
		select number, coalesce(markdown, ''), authors::text
		from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&number, &markdown, &authors); err != nil {
		t.Fatalf("read the document's latest version: %v", err)
	}
	return number, markdown, authors
}
