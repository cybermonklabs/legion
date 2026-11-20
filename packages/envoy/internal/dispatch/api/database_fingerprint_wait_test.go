package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The tests of a database fingerprint's wait (takeFingerprint, backgroundWrites). Each holds one
// background writer an ordinary request leaves owed past its answer and takes a fingerprint whose
// wait lets the writer go heldWriterLag after it begins (heldWriter). The fingerprint's reads begin
// only once that writer has written, or the test fails naming it, so a wait that skips the branch
// that covers the writer fails its test every run, whatever the reads then see; a second
// fingerprint, equal to the first, shows the writer wrote before the first one read. Whether the
// writer has written each test reads from what a caller sees - the rows in the database, a
// statement waiting on a lock the test holds, the fake service the writer calls - never from the
// register the wait reads. Each first checks that the writer was owed when the fingerprint began
// and fails UNKNOWN otherwise, so a pass never comes from a writer that had already finished. The
// tests of a refusal take a fingerprint while the writer runs and read what it failed with
// (refusedFingerprint).

// heldRefresh takes project's term-statistics refresh key on served before a test's create, with
// a job that runs the real refresh only once released: the create's related-item check then joins
// it (ensureSearchTermStats) rather than starting one, so the test decides when the refresh the
// create would have started commits. failed answers the refresh's error once it has ended.
func heldRefresh(t *testing.T, served *server, project string) (held *heldWriter, failed func() error) {
	t.Helper()
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(open)
	var refreshed atomic.Bool
	var refreshErr atomic.Pointer[error]
	served.termStats.DoChan(project, func() (any, error) {
		<-gate
		result, err := refreshSearchTermStats(context.Background(), served.deps.Store.Pool, project)
		refreshErr.Store(&err)
		refreshed.Store(true)
		return result.Items, err
	})
	held = &heldWriter{
		name:    "the term-statistics refresh of " + project + " an issue create joined",
		release: func() { time.AfterFunc(heldWriterLag, open) },
		ended:   refreshed.Load,
	}
	return held, func() error { return *refreshErr.Load() }
}

func TestAFingerprintWaitsOutATermStatisticsRefreshAnIssueCreateJoined(t *testing.T) {
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	held, failed := heldRefresh(t, served, "TEST")
	createInteractionIssue(t, handler, "TEST", "A refresh held past the answer", "A spec")
	if !detachedRunning(&served.detached) {
		t.Fatal("UNKNOWN: the create's related-item check left no detached work running (it joined no refresh), so this run shows nothing")
	}
	before := takeFingerprint(t, served, held)
	if err := failed(); err != nil {
		t.Fatalf("the held refresh failed: %v", err)
	}
	assertUnchanged(t, "no request (the term-statistics refresh an issue create joined)", before, databaseFingerprint(t, served))
}

func TestAFingerprintWaitsOutAMentionSendACommentLeftRunning(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, settle: time.Hour})
	open := state.hold("s1")
	t.Cleanup(open)
	issue := createInteractionIssue(t, handler, "TEST", "A mention held past the answer", "A spec")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	state.awaitSend(t)
	if mentionSendsHeld(served.mentions) == 0 {
		t.Fatal("UNKNOWN: the mention sender held no send when the fingerprint began, so this run shows nothing")
	}
	before := takeFingerprint(t, served, &heldWriter{
		name:    "the mention send the comment left running",
		release: func() { time.AfterFunc(heldWriterLag, open) },
		ended:   func() bool { return !commentDeliveryPending(t, database, comment.ID) },
	})
	assertUnchanged(t, "no request (the mention send a comment left running)", before, databaseFingerprint(t, served))
}

// commentDeliveryPending is whether an attempt of comment's mentions is still pending, which its
// send's settle ends.
func commentDeliveryPending(t *testing.T, database *store.Store, comment string) bool {
	t.Helper()
	var pending bool
	if err := database.Pool.QueryRow(context.Background(),
		`select exists (select 1 from comment_deliveries where comment_id = $1 and state = 'pending')`, comment,
	).Scan(&pending); err != nil {
		t.Fatalf("read the pending attempts of comment %s: %v", comment, err)
	}
	return pending
}

func TestAFingerprintWaitsOutADocumentSettlementAWriteArmed(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "A settlement armed past the answer", "A spec")
	documents := served.deps.Docs.(*docs.Service)
	spec := issue.PrimaryArtifactID
	// The create's other writers - its term-statistics refresh - end first, so no wait for them can
	// cover the settlement in the fingerprint below.
	awaitBackgroundWrites(t, served)
	if !documentOwesSettlement(t, database, spec) {
		t.Fatal("UNKNOWN: the spec owed no settlement when the fingerprint began, so this run shows nothing")
	}
	// The create armed the spec's settlement an hour out; the fingerprint's wait begins by arming
	// it heldWriterLag out, inside backgroundSoon, which a wait must then see through to its end.
	before := takeFingerprint(t, served, &heldWriter{
		name:    "the settlement of the spec the create armed",
		release: func() { documents.SettleAfter(spec, heldWriterLag) },
		ended:   func() bool { return !documentOwesSettlement(t, database, spec) },
	})
	assertUnchanged(t, "no request (the document settlement an issue create armed)", before, databaseFingerprint(t, served))
}

func TestAFingerprintWaitsOutARouteRosterFetchACancelledReadLeftRunning(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, settle: time.Hour})
	// The fetch's own bound outlasts the hold, so it stores the listener's answer, not a silence.
	served.rosters.timeout = backgroundBound
	open := state.holdLookups("/v1/sessions")
	t.Cleanup(open)
	ctx, cancel := backgroundContext(t)
	defer cancel()
	reading, stopReading := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil).WithContext(reading)
	request.Header.Set("X-Dispatch-User", "alice")
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	awaitCondition(t, ctx, "the agents list's roster fetch to start", func() bool { return rosterFetching(served) })
	stopReading()
	select {
	case <-answered:
	case <-ctx.Done():
		t.Fatal("the agents list whose request ended did not answer")
	}
	if !rosterFetching(served) {
		t.Fatal("UNKNOWN: the roster fetch had ended when the fingerprint began, so this run shows nothing")
	}
	before := takeFingerprint(t, served, &heldWriter{
		name:    "the route roster fetch the cancelled read left running",
		release: func() { time.AfterFunc(heldWriterLag, open) },
		ended:   func() bool { return !rosterFetching(served) },
	})
	assertUnchanged(t, "no request (the route roster fetch a cancelled read left running)", before, databaseFingerprint(t, served))
}

// rosterFetching is whether served's route roster fetch runs: it stores the listener's answer
// before it lets the fetch go (fetchRouteRoster).
func rosterFetching(served *server) bool {
	served.rosters.mu.Lock()
	defer served.rosters.mu.Unlock()
	return served.rosters.fetch != nil
}

// A refresh of the mention sender's claims writes its statement without the sender's lock, so a
// send it named can leave held while the statement still writes that send's row (a send whose own
// claim statement failed ends that way). The wait counts the refresh's statement as well as the
// sends held (mentionSenderWriting).
func TestAFingerprintWaitsOutAMentionClaimRefreshWhoseSendLeftHeld(t *testing.T) {
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(open)
	started := make(chan struct{})
	start := sync.OnceFunc(func() { close(started) })
	var wrote atomic.Bool
	refresh := func(context.Context, []mentionClaim) ([]mentionClaim, error) {
		start()
		<-gate
		wrote.Store(true)
		return nil, nil
	}
	read := func(context.Context, []mentionClaim) ([]mentionClaim, error) { return nil, nil }
	sender := newMentionSender(t.Context(), 10*time.Millisecond,
		func(context.Context, mentionSend) (mentionSend, bool) { return mentionSend{}, false },
		refresh, read, func(context.Context, []mentionClaim) error { return nil })
	t.Cleanup(sender.wait)
	key := mentionKey{comment: "comment", target: "session:s1", attempt: 1}
	sender.mu.Lock()
	sender.held[key] = mentionClaimState{claimed: time.Now()}
	sender.mu.Unlock()
	ctx, cancel := backgroundContext(t)
	defer cancel()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("UNKNOWN: the sender's claim keeper never refreshed the claim of the send it held, so this run shows nothing")
	}
	// The send ends without a claim of its own while the refresh that named it writes.
	sender.mu.Lock()
	delete(sender.held, key)
	sender.mu.Unlock()
	backgroundWrites(t, ctx, &server{mentions: sender}, func() { time.AfterFunc(heldWriterLag, open) })
	if !wrote.Load() {
		t.Fatal("the wait for the server's background writes returned while the claim refresh of a send that had left held still wrote: it did not wait for the refresh")
	}
}

// holdBlocking begins a transaction of the test's own on database and runs statement in it, which
// takes a lock or a transaction id, and answers release, which rolls it back, and blocked, which
// answers whether a statement of any session waits on it: the database's own account of whether a
// writer the test holds behind it has gone on. The transaction is rolled back when the test ends.
func holdBlocking(t *testing.T, ctx context.Context, database *store.Store, statement string) (func(), func() bool) {
	t.Helper()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holding transaction: %v", err)
	}
	release := sync.OnceFunc(func() { _ = holder.Rollback(context.Background()) })
	t.Cleanup(release)
	var pid int32
	if err := holder.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the holder's backend: %v", err)
	}
	if _, err := holder.Exec(ctx, statement); err != nil {
		t.Fatalf("hold %q: %v", statement, err)
	}
	blocked := func() bool {
		var waiting bool
		if err := database.Pool.QueryRow(context.Background(), `select exists (select 1 from pg_stat_activity where $1 = any(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatalf("read what waits on the holder: %v", err)
		}
		return waiting
	}
	return release, blocked
}

// relayRunning is whether served's event stream relay has a generation running or starting.
func relayRunning(served *server) bool {
	served.eventRelay.mu.Lock()
	defer served.eventRelay.mu.Unlock()
	return served.eventRelay.generation != nil || served.eventRelay.starting != nil
}

// An events view starts the server's event stream relay, which observes the event horizon on its
// own goroutine and writes it to event_horizon at moments no wait can see: a fingerprint taken
// while the view is open fails, naming the relay. Once the view's subscription has ended, the
// relay's generation has stopped, and the fingerprints agree.
func TestAFingerprintRefusesAServerWhoseEventStreamRelayRuns(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{settle: time.Hour}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Events beside the relay", "A spec")
	client := rig.dial(t, asUser("alice"))
	subscribeEvents(t, client, "events", nil)
	renameIssue(t, rig.handler, issue.Key, "Renamed beside the relay")
	if refused := refusedFingerprint(t, rig.served, backgroundBound); !strings.Contains(refused, "event stream relay runs") {
		t.Fatalf("a fingerprint taken with an events view open answered %q, want it refused naming the event stream relay", refused)
	}
	client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: "events"})
	ctx, cancel := backgroundContext(t)
	defer cancel()
	awaitCondition(t, ctx, "the relay's generation to stop with the view's subscription", func() bool { return !relayRunning(rig.served) })
	assertUnchanged(t, "no request (an event stream relay that stopped)", databaseFingerprint(t, rig.served), databaseFingerprint(t, rig.served))
}

// A relay whose generation is starting is refused as well: its first cursor waits for a horizon
// proven past the log's head (firstRelayCursor), and the observation that proves it writes it to
// event_horizon. A transaction of the test's own holds a transaction id, which no horizon can be
// proven past while it stays open, so the start waits on it while the fingerprint is taken.
func TestAFingerprintRefusesAServerWhoseEventStreamRelayIsStarting(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{settle: time.Hour}, nil)
	createInteractionIssue(t, rig.handler, "TEST", "Events beside a starting relay", "A spec")
	awaitBackgroundWrites(t, rig.served)
	ctx, cancel := backgroundContext(t)
	defer cancel()
	release, _ := holdBlocking(t, ctx, rig.database, `select pg_current_xact_id()`)
	client := rig.dial(t, asUser("alice"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitCondition(t, ctx, "the events view to start the relay", func() bool { return relayRunning(rig.served) })
	rig.served.eventRelay.mu.Lock()
	starting := rig.served.eventRelay.starting != nil
	rig.served.eventRelay.mu.Unlock()
	if !starting {
		t.Fatal("UNKNOWN: the relay's generation had started, not waited on the held transaction, when the fingerprint began, so this run shows nothing")
	}
	// The start waits as long as the holder stays open, so a few seconds bound a refusal that
	// fails at once, and a fingerprint the wait let through answers "" or names the horizon.
	if refused := refusedFingerprint(t, rig.served, 5*time.Second); !strings.Contains(refused, "event stream relay runs") {
		t.Fatalf("a fingerprint taken while the relay was starting answered %q, want it refused naming the event stream relay", refused)
	}
	release()
	eventsStart(t, client, "events")
	client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: "events"})
	awaitCondition(t, ctx, "the relay's generation to stop with the view's subscription", func() bool { return !relayRunning(rig.served) })
	assertUnchanged(t, "no request (an event stream relay that started and stopped)", databaseFingerprint(t, rig.served), databaseFingerprint(t, rig.served))
}

// An observation of the event horizon that no request waits for - the last read of an event
// stream relay generation that stopped while it observed - writes what it proves to event_horizon
// on its own goroutine once it ends. The test holds one on the event log's lock, from a goroutine
// of its own, and the fingerprint's reads must not begin while the observation still waits there,
// which the database answers (holdBlocking).
func TestAFingerprintWaitsOutAnEventHorizonObservationNoRequestWaitedFor(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	createInteractionIssue(t, handler, "TEST", "An observation held past the answer", "A spec")
	awaitBackgroundWrites(t, served)
	<-served.bootMark
	ctx, cancel := backgroundContext(t)
	defer cancel()
	release, blocked := holdBlocking(t, ctx, database, `lock table events in access exclusive mode`)
	horizon := served.horizon()
	observed := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(context.Background())
		observed <- err
	}()
	awaitCondition(t, ctx, "the observation to wait on the held event log", blocked)
	before := takeFingerprint(t, served, &heldWriter{
		name:    "an observation of the event horizon no request waited for",
		release: func() { time.AfterFunc(heldWriterLag, release) },
		ended:   func() bool { return !blocked() },
	})
	if err := <-observed; err != nil {
		t.Fatalf("the held observation failed: %v", err)
	}
	assertUnchanged(t, "no request (an event horizon observation no request waited for)", before, databaseFingerprint(t, served))
}

// The write of a horizon an observation proved runs after the observation has answered its
// waiters (events.Horizon.run, writePersisted), under a client deadline of its own
// (horizonPersistTimeout, 2 s). The test holds the write in the server: a transaction locks
// event_horizon in share mode, which takes no transaction id and so keeps no observation from
// proving a horizon, and observations no request waits for run until one proves a horizon past the
// one persisted and its write waits on the lock. On an instance other tests share, their open
// transactions can hold a proof back for a while, and a write comes at most once a second
// (horizonPersistEvery), so the test observes again until one does. While the write's client waits
// the wait sees it through Horizon.Busy; once its deadline ends the call, pgx sends Postgres a
// cancel on a connection of its own and returns, and until that cancel lands - late, or never, on
// a loaded server - the statement still waits on the lock and commits once it is let go, which
// only the database's own account of its statements sees (statementsRunning). A run at a
// one-minute load of 371 failed so before the wait read that account. The fingerprint's reads must
// not begin while the write still waits on the lock, which the database answers.
func TestAFingerprintWaitsOutAnEventHorizonWriteHeldPastItsObservation(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	createInteractionIssue(t, handler, "TEST", "A horizon write held past its observation", "A spec")
	awaitBackgroundWrites(t, served)
	<-served.bootMark
	ctx, cancel := backgroundContext(t)
	defer cancel()
	release, blocked := holdBlocking(t, ctx, database, `lock table event_horizon in share mode`)
	horizon := served.horizon()
	for !blocked() {
		observed := make(chan error, 1)
		go func() {
			_, err := horizon.Safe(context.Background())
			observed <- err
		}()
		// The observation ends having written nothing the lock holds, or its write waits on the
		// lock, in which case it may not end until the holder lets go.
		awaitCondition(t, ctx, "UNKNOWN: an observation to end or its write to wait on the held event_horizon", func() bool {
			select {
			case err := <-observed:
				if err != nil {
					t.Fatalf("an observation failed: %v", err)
				}
				return true
			default:
				return blocked()
			}
		})
	}
	before := takeFingerprint(t, served, &heldWriter{
		name:    "the write to event_horizon of an observation no request waited for",
		release: func() { time.AfterFunc(heldWriterLag, release) },
		ended:   func() bool { return !blocked() },
	})
	assertUnchanged(t, "no request (a horizon write held past its observation)", before, databaseFingerprint(t, served))
}

// A writer whose call ends before its statement does - a deadline ends the call, and the cancel
// pgx then sends on a connection of its own lands late or never - leaves the statement running in
// the server, where no count in the server's code still holds it. The test sends such a statement
// itself, an insert that waits on a lock the test holds, from a goroutine nothing in the server
// counts, and the fingerprint's reads must not begin while it still waits, which the database
// answers (statementsRunning is the branch that sees it).
func TestAFingerprintWaitsOutAStatementNoCountInTheServerHolds(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	createInteractionIssue(t, handler, "TEST", "A statement no count holds", "A spec")
	awaitBackgroundWrites(t, served)
	ctx, cancel := backgroundContext(t)
	defer cancel()
	release, blocked := holdBlocking(t, ctx, database, `lock table people in share mode`)
	var inserted atomic.Bool
	go func() {
		if _, err := database.Pool.Exec(context.Background(), `insert into people (email) values ('a-statement-no-count-holds@example.com')`); err != nil {
			t.Errorf("the held insert: %v", err)
			return
		}
		inserted.Store(true)
	}()
	awaitCondition(t, ctx, "the insert to wait on the held people", blocked)
	before := takeFingerprint(t, served, &heldWriter{
		name:    "an insert no count in the server holds",
		release: func() { time.AfterFunc(heldWriterLag, release) },
		ended:   inserted.Load,
	})
	assertUnchanged(t, "no request (a statement no count in the server holds)", before, databaseFingerprint(t, served))
}

// A document whose rows have aged past a compaction's tail - a test that ran that long after its
// second row, as one can under load - may be compacted at any moment, by its room's close or by
// the compaction queue, and the fingerprint's wait compacts it first, so the two fingerprints
// agree rather than fail naming that compaction.
func TestAFingerprintCompactsADocumentWhoseRowsACompactionMayFoldNow(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "A document past the compaction tail", "A spec")
	spec := issue.PrimaryArtifactID
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "A second row."}},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("edit the spec: status=%d body=%s", edited.Code, edited.Body.String())
	}
	awaitBackgroundWrites(t, served)
	if _, err := database.Pool.Exec(context.Background(),
		`update doc_updates set created_at = created_at - interval '1 hour' where artifact_id = $1`, spec); err != nil {
		t.Fatalf("age the spec's rows: %v", err)
	}
	rows := storedUpdateRows(t, database, spec)
	if rows < 2 {
		t.Fatalf("UNKNOWN: the spec stores %d rows, so no compaction can fold any, and this run shows nothing", rows)
	}
	before := databaseFingerprint(t, served)
	if folded := storedUpdateRows(t, database, spec); folded >= rows {
		t.Fatalf("the spec stores %d rows after the fingerprint, as many as the %d before it: the wait did not compact the rows a compaction may fold", folded, rows)
	}
	assertUnchanged(t, "no request (a document whose rows a compaction may fold)", before, databaseFingerprint(t, served))
}

// storedUpdateRows is how many doc_updates rows document stores.
func storedUpdateRows(t *testing.T, database *store.Store, document string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, document).Scan(&rows); err != nil {
		t.Fatalf("count the rows of document %s: %v", document, err)
	}
	return rows
}

// A document whose oldest row after its first comes of age between two fingerprints may be
// compacted between them, by its room's close or by the compaction queue, and that compaction
// would read as the change of a request between the two. The wait compacts only rows that have
// aged already; for the rest it answers the moment the oldest of them comes of age, read by the
// compaction queue's own check (docs.BackgroundQuiet), and the pair's comparison fails naming that
// compaction when the second fingerprint's reads end past it. The test reads that moment from a
// first fingerprint, ages the spec's rows so it comes a few seconds after the next fingerprint,
// and takes the second fingerprint once it has passed.
func TestAFingerprintPairFailsNamingADocumentRowThatCameOfAgeBetweenThem(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "A row that comes of age between fingerprints", "A spec")
	spec := issue.PrimaryArtifactID
	if edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "A second row."}},
	}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("edit the spec: status=%d body=%s", edited.Code, edited.Body.String())
	}
	awaitBackgroundWrites(t, served)
	if rows := storedUpdateRows(t, database, spec); rows < 2 {
		t.Fatalf("UNKNOWN: the spec stores %d rows, so no compaction can fold any, and this run shows nothing", rows)
	}
	first := databaseFingerprint(t, served)
	if !strings.Contains(first.quiet.Writer, spec) {
		t.Fatalf("UNKNOWN: the wait answered %q at %s, not the compaction of the spec, so this run shows nothing", first.quiet.Writer, first.quiet.At.Format(time.RFC3339Nano))
	}
	// The row comes of age margin after the next fingerprint begins: past its reads, which take
	// milliseconds, and soon enough that the test waits only that long.
	const margin = 5 * time.Second
	if _, err := database.Pool.Exec(context.Background(),
		`update doc_updates set created_at = created_at - make_interval(secs => $2) where artifact_id = $1`,
		spec, time.Until(first.quiet.At.Add(-margin)).Seconds()); err != nil {
		t.Fatalf("age the spec's rows: %v", err)
	}
	before := databaseFingerprint(t, served)
	if !strings.Contains(before.quiet.Writer, spec) || time.Until(before.quiet.At) > margin {
		t.Fatalf("UNKNOWN: the wait answered %q at %s, not the spec's compaction within %s, so this run shows nothing", before.quiet.Writer, before.quiet.At.Format(time.RFC3339Nano), margin)
	}
	time.Sleep(time.Until(before.quiet.At))
	after := databaseFingerprint(t, served)
	failures := unchangedFailures("no request (a document row that came of age between the fingerprints)", before, after)
	named := slices.ContainsFunc(failures, func(failure string) bool {
		return strings.Contains(failure, "a compaction of document "+spec) && strings.Contains(failure, "before the second fingerprint's reads ended")
	})
	if !named {
		t.Fatalf("the pair of fingerprints across the moment the spec's row came of age failed with %q, want a failure naming the spec's compaction", failures)
	}
}

// An architecture sync runs as a flight its callers join (architecture.Importer.Sync), on a
// context their cancel does not reach, so a caller that leaves before the flight answers - a page
// closed, a request that timed out - leaves it writing the source's snapshot, components and facts
// after the request has answered. The fake GitHub holds the flight's blob reads until the test
// lets them go. A wait whose bound ends while the flight runs fails naming the sync, and the
// fingerprint's reads begin only once the flight has written, which the source's row answers.
func TestAFingerprintWaitsOutAnArchitectureSyncWhoseCallerLeft(t *testing.T) {
	fake := &fakeGitHubApp{
		installations: map[string]string{"legion/arch": "read"},
		repos:         map[string]*fakeArchitectureRepo{"legion/arch": {commit: shaArch, files: archFiles()}},
	}
	inner := fake.handler(t)
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	var holding atomic.Bool
	var held atomic.Int32
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the App's key: %v", err)
	}
	github := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if holding.Load() && strings.Contains(r.URL.Path, "/git/blobs/") {
			held.Add(1)
			<-gate
		}
		inner.ServeHTTP(w, r)
	}})
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		settle:        time.Hour,
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))},
		githubAPIBase: github,
	})
	// Registered after the fake GitHub and the server, so it runs before either closes: the fake's
	// Close waits for the blob reads the gate holds.
	t.Cleanup(open)
	createTestProject(t, handler, "CORE")
	putSource(t, handler, "CORE", "legion/arch", "main")
	awaitBackgroundWrites(t, served)
	synced := func() bool {
		var commit *string
		if err := database.Pool.QueryRow(context.Background(),
			`select last_commit from architecture_sources where project_key = 'CORE' and repo = 'legion/arch'`).Scan(&commit); err != nil {
			t.Fatalf("read the source's last commit: %v", err)
		}
		return commit != nil && *commit == shaArch
	}
	ctx, cancel := backgroundContext(t)
	defer cancel()
	holding.Store(true)
	leaving, leave := context.WithCancel(context.Background())
	defer leave()
	request := httptest.NewRequest(http.MethodPost, sourcePath("CORE", "legion/arch")+"/sync", nil).WithContext(leaving)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	awaitCondition(t, ctx, "the sync's flight to read a blob the fake GitHub holds", func() bool { return held.Load() > 0 })
	leave()
	select {
	case <-answered:
	case <-ctx.Done():
		t.Fatal("the sync whose caller left did not answer")
	}
	if synced() {
		t.Fatal("UNKNOWN: the sync had written when the fingerprint began, so this run shows nothing")
	}
	// The flight stays held, so a wait that sees it runs to this bound and names it.
	if refused := refusedFingerprint(t, served, 3*time.Second); !strings.Contains(refused, "architecture importer's syncs") {
		t.Fatalf("a fingerprint taken while the sync whose caller left was held answered %q, want its wait to fail at its bound naming the architecture importer's syncs", refused)
	}
	before := takeFingerprint(t, served, &heldWriter{
		name:    "the architecture sync whose caller left",
		release: func() { time.AfterFunc(heldWriterLag, open) },
		ended:   synced,
	})
	assertUnchanged(t, "no request (an architecture sync whose caller left)", before, databaseFingerprint(t, served))
}

// openLiveSockets waits, within ctx, for exactly n live sockets open on served.
func openLiveSockets(t *testing.T, ctx context.Context, served *server, n int) {
	t.Helper()
	awaitCondition(t, ctx, fmt.Sprintf("%d live sockets to be open on the server", n), func() bool { return len(served.liveSockets.all()) == n })
}

// A live socket resolves the credential it was admitted with again on every heartbeat
// (whileCallerResolves), and under a personal agent token that resolution stamps the token's last
// use (personalTokenActor) once it is a minute old: a write on the socket's own clock, which no
// wait can see. A fingerprint of a server holding such a socket fails, naming it; one beside a
// socket under the test server's header identity, whose resolution writes nothing, is taken.
func TestAFingerprintRefusesAServerWithALiveSocketUnderAPersonalAgentToken(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{settle: time.Hour}, nil)
	ctx, cancel := backgroundContext(t)
	defer cancel()
	minted := mintAgentToken(t, rig.handler)
	token := rig.dial(t, withBearer(minted.Token))
	openLiveSockets(t, ctx, rig.served, 1)
	if refused := refusedFingerprint(t, rig.served, backgroundBound); !strings.Contains(refused, "live socket admitted by a personal agent token") {
		t.Fatalf("a fingerprint taken beside a live socket under a personal agent token answered %q, want it refused naming that socket", refused)
	}
	_ = token.conn.Close()
	openLiveSockets(t, ctx, rig.served, 0)
	rig.dial(t, asUser("alice"))
	openLiveSockets(t, ctx, rig.served, 1)
	if refused := refusedFingerprint(t, rig.served, backgroundBound); refused != "" {
		t.Fatalf("a fingerprint taken beside a live socket under the header identity failed: %s", refused)
	}
}

// A heartbeat's resolution of a socket's caller under way when the socket closes runs to its end
// (whileCallerResolves), so its stamp of a personal agent token's last use can commit after the
// socket has gone, which no open socket shows. The test holds the stamp: the token's last use is
// aged past the minute and a transaction locks agent_tokens in share mode, so the next heartbeat's
// stamp waits on it; then the socket closes. The fingerprint's reads must not begin while the
// stamp still waits, which the database answers.
func TestAFingerprintWaitsOutTheLastHeartbeatOfASocketThatClosed(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{settle: time.Hour, streamHeartbeat: 200 * time.Millisecond}, nil)
	ctx, cancel := backgroundContext(t)
	defer cancel()
	minted := mintAgentToken(t, rig.handler)
	client := rig.dial(t, withBearer(minted.Token))
	openLiveSockets(t, ctx, rig.served, 1)
	if _, err := rig.database.Pool.Exec(ctx, `update agent_tokens set last_used_at = now() - interval '2 minutes'`); err != nil {
		t.Fatalf("age the token's last use: %v", err)
	}
	release, blocked := holdBlocking(t, ctx, rig.database, `lock table agent_tokens in share mode`)
	awaitCondition(t, ctx, "a heartbeat's stamp of the token's last use to wait on the held agent_tokens", blocked)
	_ = client.conn.Close()
	openLiveSockets(t, ctx, rig.served, 0)
	before := takeFingerprint(t, rig.served, &heldWriter{
		name:    "the stamp of a personal agent token's last use by the heartbeat of a socket that closed",
		release: func() { time.AfterFunc(heldWriterLag, release) },
		ended:   func() bool { return !blocked() },
	})
	assertUnchanged(t, "no request (the last heartbeat of a socket that closed)", before, databaseFingerprint(t, rig.served))
}

// The delivery hub's generation records a deploy run's warning on its own goroutine whenever a
// view it reads again finds one (firstToWarn), on the generation's context: an insert under way
// when the generation stops can commit after its worker has returned, which nothing then shows. A
// fingerprint of a server whose hub has run a generation fails, naming the hub, whether or not the
// generation still runs.
func TestAFingerprintRefusesAServerWhoseDeliveryHubRan(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{settle: time.Hour}, nil)
	ctx, cancel := backgroundContext(t)
	defer cancel()
	client := rig.dial(t, asUser("alice"))
	client.send(t, deliverySubscribe("pipeline", livePipelineView(nil)))
	awaitCondition(t, ctx, "the pipeline view to start the hub's generation", func() bool { return hubConnCount(rig.served.deliveryHub) == 1 })
	client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: "pipeline"})
	awaitCondition(t, ctx, "the hub's generation to stop with the view's subscription", func() bool { return hubConnCount(rig.served.deliveryHub) == 0 })
	if refused := refusedFingerprint(t, rig.served, backgroundBound); !strings.Contains(refused, "delivery hub has run") {
		t.Fatalf("a fingerprint taken once the hub's generation had stopped answered %q, want it refused naming the delivery hub", refused)
	}
}

// One background writer can start another - a detached run that writes a document arms its
// settlement - and the writer a pass of the wait waited for may start one in a branch that pass
// had already looked at. The wait passes over every writer again after any wait, and returns only
// from a pass that waited for none. The test holds a mention send, and as it lets the send go it
// starts a detached writer on the server, which writes heldWriterLag later: a wait that returned
// after the pass that waited for the send would read before that writer had written.
func TestAFingerprintPassesOverEveryWriterAgainAfterAWait(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, settle: time.Hour})
	open := state.hold("s1")
	t.Cleanup(open)
	issue := createInteractionIssue(t, handler, "TEST", "A writer that starts another", "A spec")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	state.awaitSend(t)
	if mentionSendsHeld(served.mentions) == 0 {
		t.Fatal("UNKNOWN: the mention sender held no send when the fingerprint began, so this run shows nothing")
	}
	var wrote atomic.Bool
	startSecond := func() {
		served.detached.goDetached(func() {
			time.Sleep(heldWriterLag)
			if _, err := database.Pool.Exec(context.Background(), `insert into people (email) values ('a-second-writer@example.com')`); err != nil {
				t.Errorf("the second writer's insert: %v", err)
			}
			wrote.Store(true)
		})
		open()
	}
	before := takeFingerprint(t, served, &heldWriter{
		name:    "the detached writer the mention send's release started",
		release: func() { time.AfterFunc(heldWriterLag, startSecond) },
		ended:   func() bool { return wrote.Load() && !commentDeliveryPending(t, database, comment.ID) },
	})
	assertUnchanged(t, "no request (a writer another writer started)", before, databaseFingerprint(t, served))
}

// gatedHorizonDB is a Horizon's database that holds each batch held picks at a gate before it
// sends it, so that statement is held in the Go code, with no statement in the server; every other
// batch passes. entered is closed once a held batch first reaches the gate, and ended is set once a
// held batch's results are closed: its caller's call has returned, whatever the statement did.
type gatedHorizonDB struct {
	events.HorizonDB
	held    func(*pgx.Batch) bool
	gate    chan struct{}
	entered chan struct{}
	enter   sync.Once
	ended   atomic.Bool
}

// newGatedHorizonDB is db with each batch held picks held at a gate the test closes when it ends.
func newGatedHorizonDB(t *testing.T, db events.HorizonDB, held func(*pgx.Batch) bool) (gated *gatedHorizonDB, open func()) {
	t.Helper()
	gated = &gatedHorizonDB{HorizonDB: db, held: held, gate: make(chan struct{}), entered: make(chan struct{})}
	open = sync.OnceFunc(func() { close(gated.gate) })
	t.Cleanup(open)
	return gated, open
}

func (db *gatedHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if !db.held(batch) {
		return db.HorizonDB.SendBatch(ctx, batch)
	}
	db.enter.Do(func() { close(db.entered) })
	<-db.gate
	return &endedBatch{BatchResults: db.HorizonDB.SendBatch(ctx, batch), ended: &db.ended}
}

// horizonWriteBatch picks the batch that writes a proven horizon to event_horizon
// (events.Horizon.writePersisted).
func horizonWriteBatch(batch *pgx.Batch) bool {
	return len(batch.QueuedQueries) > 0 && strings.Contains(batch.QueuedQueries[0].SQL, "into event_horizon")
}

// horizonObservationBatch picks an observation's batch (events.Horizon.take): its third statement,
// after the begin and the setting, reads the snapshot.
func horizonObservationBatch(batch *pgx.Batch) bool {
	return len(batch.QueuedQueries) > 2 && strings.Contains(batch.QueuedQueries[2].SQL, "pg_snapshot_xmin")
}

// endedBatch is a batch's results that set ended once closed.
type endedBatch struct {
	pgx.BatchResults
	ended *atomic.Bool
}

func (results *endedBatch) Close() error {
	err := results.BatchResults.Close()
	results.ended.Store(true)
	return err
}

// The write of a proven horizon to event_horizon runs on the observer's goroutine after the
// observation has answered its waiters (events.Horizon.run, writePersisted), and Horizon.Busy
// counts it from before it sends its statement. The test holds the write in the Go code, before
// its statement reaches the server, where the database's own account of its statements
// (statementsRunning) cannot see it: the server's horizon is replaced by one whose database holds
// each write to event_horizon at a gate, and an observation that proves a horizon runs its write
// into the gate. A lone observation proves a horizon only when no transaction id of the instance
// is in progress, and on an instance other tests share, their open transactions can hold a proof
// back for a while, so the test observes again, as its sister test does, until a write reaches
// the gate. The fingerprint's reads must not begin before the held write's call has returned,
// which the gate's database answers.
func TestAFingerprintWaitsOutAnEventHorizonWriteHeldBeforeItsStatement(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	createInteractionIssue(t, handler, "TEST", "A horizon write held before its statement", "A spec")
	awaitBackgroundWrites(t, served)
	<-served.bootMark
	gated, open := newGatedHorizonDB(t, database.Pool, horizonWriteBatch)
	horizon := events.NewHorizon(gated)
	horizon.PersistProven()
	served.eventHorizon = horizon
	ctx, cancel := backgroundContext(t)
	defer cancel()
	for entered := false; !entered; {
		observed := make(chan error, 1)
		go func() {
			_, err := horizon.Safe(context.Background())
			observed <- err
		}()
		select {
		case err := <-observed:
			if err != nil {
				t.Fatalf("an observation failed: %v", err)
			}
			// That observation proved nothing a write followed, or its write is on its way to
			// the gate: the next look, a poll later, finds which.
			select {
			case <-gated.entered:
				entered = true
			case <-time.After(testwait.Poll):
			case <-ctx.Done():
			}
		case <-gated.entered:
			entered = true
		case <-ctx.Done():
			t.Fatalf("UNKNOWN: no write to event_horizon reached the gate (%v), so this run shows nothing", context.Cause(ctx))
		}
	}
	before := takeFingerprint(t, served, &heldWriter{
		name:    "the write to event_horizon held before it sent its statement",
		release: func() { time.AfterFunc(heldWriterLag, open) },
		ended:   gated.ended.Load,
	})
	assertUnchanged(t, "no request (a horizon write held before its statement)", before, databaseFingerprint(t, served))
}

// gatedIdentity is the test server's header identity whose Login, once gating is on, holds at a
// gate and then records the person with a write of its own before it answers, as a browser
// identity's resolution can (identity.CookieIdentity confirms a membership). wrote is set once that
// write has returned without an error.
type gatedIdentity struct {
	identity.Identity
	database *store.Store
	gating   atomic.Bool
	gate     chan struct{}
	entered  chan struct{}
	enter    sync.Once
	wrote    atomic.Bool
}

func (g *gatedIdentity) Login(r *http.Request) (string, error) {
	if g.gating.Load() {
		g.enter.Do(func() { close(g.entered) })
		<-g.gate
		if _, err := g.database.Pool.Exec(r.Context(), `insert into people (email) values ('a-held-heartbeat@example.com') on conflict do nothing`); err != nil {
			return "", err
		}
		g.wrote.Store(true)
	}
	return g.Identity.Login(r)
}

// A heartbeat's resolution of its socket's caller (whileCallerResolves) runs on to its end when
// the socket closes meanwhile, and the server counts it from before it begins (server.heartbeats).
// The test holds a resolution in the Go code, before it sends a statement, where the database's own
// account of its statements cannot see it: the server's identity holds at a gate once the socket
// is open, then writes. The socket closes while its heartbeat is held, and the fingerprint's reads
// must not begin before the held resolution has written, which the identity answers.
func TestAFingerprintWaitsOutAHeartbeatResolutionHeldBeforeItsStatement(t *testing.T) {
	gated := &gatedIdentity{gate: make(chan struct{}), entered: make(chan struct{})}
	rig := newLiveSocketRig(t, testServerOptions{settle: time.Hour, streamHeartbeat: 200 * time.Millisecond, identity: gated}, nil)
	gated.Identity, gated.database = headerIdentity(rig.database), rig.database
	open := sync.OnceFunc(func() { close(gated.gate) })
	t.Cleanup(open)
	ctx, cancel := backgroundContext(t)
	defer cancel()
	client := rig.dial(t, asUser("alice"))
	openLiveSockets(t, ctx, rig.served, 1)
	gated.gating.Store(true)
	select {
	case <-gated.entered:
	case <-ctx.Done():
		t.Fatalf("UNKNOWN: no heartbeat reached the identity's gate (%v), so this run shows nothing", context.Cause(ctx))
	}
	_ = client.conn.Close()
	openLiveSockets(t, ctx, rig.served, 0)
	before := takeFingerprint(t, rig.served, &heldWriter{
		name:    "a heartbeat's resolution of its caller held before its statement",
		release: func() { time.AfterFunc(heldWriterLag, open) },
		ended:   gated.wrote.Load,
	})
	assertUnchanged(t, "no request (a heartbeat resolution held before its statement)", before, databaseFingerprint(t, rig.served))
}

// An observation of the event horizon runs on the goroutine of the caller that started it, and
// one no request waits for - the last read of an event stream relay generation that stopped while
// it observed - goes on to write what it proves to event_horizon once it ends. Horizon.Busy counts
// it from before it sends its statement (its observation in flight). The test holds an observation
// in the Go code, before its statement reaches the server, where the database's own account of its
// statements (statementsRunning) cannot see it: the server's horizon is replaced by one whose
// database holds each observation at a gate. The fingerprint's reads must not begin before the
// held observation's call has returned, which the gate's database answers; a write it proves after
// that the second fingerprint, equal to the first, shows waited out too.
func TestAFingerprintWaitsOutAnEventHorizonObservationHeldBeforeItsStatement(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	createInteractionIssue(t, handler, "TEST", "An observation held before its statement", "A spec")
	awaitBackgroundWrites(t, served)
	<-served.bootMark
	gated, open := newGatedHorizonDB(t, database.Pool, horizonObservationBatch)
	horizon := events.NewHorizon(gated)
	horizon.PersistProven()
	served.eventHorizon = horizon
	ctx, cancel := backgroundContext(t)
	defer cancel()
	go func() { _, _ = horizon.Safe(context.Background()) }()
	select {
	case <-gated.entered:
	case <-ctx.Done():
		t.Fatalf("UNKNOWN: no observation reached the gate (%v), so this run shows nothing", context.Cause(ctx))
	}
	before := takeFingerprint(t, served, &heldWriter{
		name:    "an observation of the event horizon held before it sent its statement",
		release: func() { time.AfterFunc(heldWriterLag, open) },
		ended:   gated.ended.Load,
	})
	assertUnchanged(t, "no request (an event horizon observation held before its statement)", before, databaseFingerprint(t, served))
}

// A lookup of the test server's GitHub App client runs as a fetch of the client's cache that its
// callers join (githubapp.ttlCache.get), on a context their cancel does not reach, so a caller
// that leaves before it answers - the caller of readCIRunPages (results_ci.go) among them - leaves
// it running, and a rate limit GitHub answers it with is recorded in github_rate_limits after the
// request has answered. The lookup waits on HTTP in the Go code, with no statement in the server,
// so only the wait's branch for the client's lookups (githubapp.Client.Fetching) can see it. The
// fake GitHub answers a first lookup at once, and the fingerprint does not wait on it once it has
// ended; it holds a second lookup, whose caller leaves, until the test lets it go, and then answers
// it with no requests left. A fingerprint whose bound ends while that lookup is held fails naming
// the client's lookups, and the fingerprint's reads begin only once github_rate_limits holds the
// lookup's row.
func TestAFingerprintWaitsOutAGitHubLookupWhoseCallerLeft(t *testing.T) {
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	var held atomic.Int32
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the App's key: %v", err)
	}
	github := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/legion/held/installation" {
			http.NotFound(w, r)
			return
		}
		held.Add(1)
		<-gate
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}})
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{
		settle:        time.Hour,
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))},
		githubAPIBase: github,
	})
	// Registered after the fake GitHub and the server, so it runs before either closes: the fake's
	// Close waits for the lookup the gate holds.
	t.Cleanup(open)
	awaitBackgroundWrites(t, served)
	limits := func() int {
		var n int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from github_rate_limits`).Scan(&n); err != nil {
			t.Fatalf("count the rows of github_rate_limits: %v", err)
		}
		return n
	}
	ctx, cancel := backgroundContext(t)
	defer cancel()

	// A lookup whose caller waited for its answer has ended, and the wait must not wait on it: a
	// count that did not fall with its answer would hold this fingerprint to its bound.
	if _, err := served.deps.GitHub.RepositoryToken(ctx, "legion", "answered"); err == nil {
		t.Fatal("the lookup of a repository the fake GitHub does not know answered a token")
	}
	started := time.Now()
	if refused := refusedFingerprint(t, served, 3*time.Second); refused != "" {
		t.Fatalf("a fingerprint taken once the client's one lookup had answered failed after %s: %s", time.Since(started), refused)
	}
	if n := limits(); n != 0 {
		t.Fatalf("UNKNOWN: github_rate_limits holds %d rows before the held lookup, so this run shows nothing", n)
	}

	leaving, leave := context.WithCancel(context.Background())
	defer leave()
	answered := make(chan error, 1)
	go func() {
		_, err := served.deps.GitHub.RepositoryToken(leaving, "legion", "held")
		answered <- err
	}()
	awaitCondition(t, ctx, "the lookup to reach the fake GitHub", func() bool { return held.Load() > 0 })
	leave()
	if err := <-answered; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that left was answered %v, want its own cancel", err)
	}
	if n := limits(); n != 0 {
		t.Fatalf("UNKNOWN: github_rate_limits holds %d rows when the fingerprint begins, so this run shows nothing", n)
	}
	// The lookup stays held, so a wait that sees it runs to this bound and names it.
	if refused := refusedFingerprint(t, served, 3*time.Second); !strings.Contains(refused, "GitHub App client's cached lookups") {
		t.Fatalf("a fingerprint taken while the lookup whose caller left was held answered %q, want its wait to fail at its bound naming the GitHub App client's cached lookups", refused)
	}
	before := takeFingerprint(t, served, &heldWriter{
		name:    "the GitHub App lookup whose caller left (its rate limit's row in github_rate_limits)",
		release: func() { time.AfterFunc(heldWriterLag, open) },
		ended:   func() bool { return limits() > 0 },
	})
	assertUnchanged(t, "no request (a GitHub App lookup whose caller left)", before, databaseFingerprint(t, served))
}

// signingInIdentity is the test server's header identity under a type of its own, as a
// deployment's sign-in identity is (identity.CookieIdentity), whose Login, once writing is on,
// writes a row of its own each time it resolves a caller, as a sign-in's resolution confirms the
// person's membership. wrote counts those writes.
type signingInIdentity struct {
	identity.Identity
	database *store.Store
	writing  atomic.Bool
	wrote    atomic.Int32
}

func (s *signingInIdentity) Login(r *http.Request) (string, error) {
	if s.writing.Load() {
		if _, err := s.database.Pool.Exec(r.Context(), `insert into people (email) values ('a-sign-in-' || gen_random_uuid() || '@example.com')`); err != nil {
			return "", err
		}
		s.wrote.Add(1)
	}
	return s.Identity.Login(r)
}

// A live socket admitted by a person's sign-in resolves its caller again on every heartbeat
// (whileCallerResolves), and under any identity but the test server's header one that resolution
// can write - a sign-in cookie's confirms or ends the person's membership - on the socket's own
// clock, which no wait can see. The test server's identity here writes a row on each resolution
// once the socket is open, so its heartbeats write: a fingerprint of the server fails, naming the
// sign-in. Once the socket has closed, the wait sees its last heartbeat out and two fingerprints
// agree.
func TestAFingerprintRefusesAServerWithALiveSocketUnderASignIn(t *testing.T) {
	signingIn := &signingInIdentity{}
	rig := newLiveSocketRig(t, testServerOptions{settle: time.Hour, streamHeartbeat: 200 * time.Millisecond, identity: signingIn}, nil)
	signingIn.Identity, signingIn.database = headerIdentity(rig.database), rig.database
	ctx, cancel := backgroundContext(t)
	defer cancel()
	client := rig.dial(t, asUser("alice"))
	openLiveSockets(t, ctx, rig.served, 1)
	signingIn.writing.Store(true)
	awaitCondition(t, ctx, "a heartbeat's resolution of the socket's caller to write", func() bool { return signingIn.wrote.Load() > 0 })
	if refused := refusedFingerprint(t, rig.served, backgroundBound); !strings.Contains(refused, "a person's sign-in") {
		t.Fatalf("a fingerprint taken beside a live socket whose sign-in heartbeat writes answered %q, want it refused naming the sign-in", refused)
	}
	_ = client.conn.Close()
	openLiveSockets(t, ctx, rig.served, 0)
	assertUnchanged(t, "no request (a live socket under a sign-in that closed)", databaseFingerprint(t, rig.served), databaseFingerprint(t, rig.served))
}

// runEndlessStatement runs statement, which must not end by itself, on a connection of its own to
// the database config names, and answers once the database's own account shows it running, with
// its backend's pid. The statement is cancelled and the connection closed when the test ends.
func runEndlessStatement(t *testing.T, ctx context.Context, database *store.Store, config *pgx.ConnConfig, statement string) int32 {
	t.Helper()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to database %s: %v", config.Database, err)
	}
	var pid int32
	if err := conn.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the backend of database %s: %v", config.Database, err)
	}
	stop, cancelStatement := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = conn.Exec(stop, statement)
	}()
	t.Cleanup(func() {
		cancelStatement()
		<-ended
		_ = conn.Close(context.Background())
	})
	awaitCondition(t, ctx, fmt.Sprintf("%q to run in database %s", statement, config.Database), func() bool { return statementRuns(t, database, pid) })
	return pid
}

// statementRuns answers whether the backend pid, of any database of database's server, runs a
// statement now, by the server's own account (pg_stat_activity), read through database's pool.
func statementRuns(t *testing.T, database *store.Store, pid int32) bool {
	t.Helper()
	var runs bool
	if err := database.Pool.QueryRow(context.Background(), `select exists (select 1 from pg_stat_activity where pid = $1 and state = 'active')`, pid).Scan(&runs); err != nil {
		t.Fatalf("read whether backend %d runs a statement: %v", pid, err)
	}
	return runs
}

// The wait for the statements the database still runs (statementsRunning) reads the test's own
// database alone, so a statement another database of the same server runs holds no fingerprint.
// A statement of the test's own database that outlasts the wait's bound fails the fingerprint at
// that bound naming the statement, which the wait's last read before the bound found
// (awaitStatements): the read that the bound itself ends answers nothing about the statement.
func TestAFingerprintNamesAStatementOfItsOwnDatabaseThatOutlastsItsBound(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	createInteractionIssue(t, handler, "TEST", "A statement that outlasts the bound", "A spec")
	awaitBackgroundWrites(t, served)
	ctx, cancel := backgroundContext(t)
	defer cancel()
	elsewhere := database.Pool.Config().ConnConfig.Copy()
	elsewhere.Database = "postgres"
	otherPID := runEndlessStatement(t, ctx, database, elsewhere, "select pg_sleep(600) -- a statement of another database")
	started := time.Now()
	refused := refusedFingerprint(t, served, 20*time.Second)
	took := time.Since(started)
	if refused != "" {
		t.Fatalf("a fingerprint beside a statement of another database of the same server failed after %s: %s", took, refused)
	}
	if !statementRuns(t, database, otherPID) {
		t.Fatalf("UNKNOWN: the other database's statement had ended when the fingerprint answered (after %s), so this run shows nothing of the wait's scope", took)
	}

	runEndlessStatement(t, ctx, database, database.Pool.Config().ConnConfig.Copy(), "select pg_sleep(600) -- a statement of the test's own database")
	const bound = 3 * time.Second
	started = time.Now()
	refused = refusedFingerprint(t, served, bound)
	took = time.Since(started)
	if !strings.Contains(refused, "the statements the database still runs for this test's database") ||
		!strings.Contains(refused, "a statement of the test's own database") {
		t.Fatalf("a fingerprint beside a statement of the test's own database that does not end answered %q after %s, want it to fail naming that statement", refused, took)
	}
	if took < bound || took > bound+10*time.Second {
		t.Fatalf("the fingerprint failed after %s, want about its bound of %s: %s", took, bound, refused)
	}
}
