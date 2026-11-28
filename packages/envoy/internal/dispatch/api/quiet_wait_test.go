package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The quiet wait (awaitQuietDatabase) keeps every writer a test server runs past its requests
// from running beside a measurement, and it waits on the work itself rather than on a spell of
// quiet. Each row of its state table that holds a writer holds it on a channel the row owns, after
// the request that started it has answered, runs the wait on a goroutine of its own, and waits for
// the wait to read that writer busy - or, for a writer the wait joins, to begin the join that
// blocks on it. The row then checks that the wait has not returned, releases the writer, waits for
// the wait to return, and checks, at that return, that the writer's write has committed. No row
// holds a span: a busy read and a join are facts the wait reports (awaitQuiet's heard), and the
// return is read after them. A wait that stops - a database no server is registered on, or a read
// that failed - stops its goroutine without failing the row (stoppingTB), and the row reads why.
func TestTheQuietWaitWaitsForEachBackgroundWriter(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		run  func(t *testing.T)
	}{
		{"nothing running", quietWaitNothingRunning},
		{"detached work held", quietWaitDetachedWork},
		{"a keyed run held", quietWaitKeyedRun},
		{"a read worker held", quietWaitReadWorker},
		{"a mention send held", quietWaitMentionSend},
		{"a statistics refresh held", quietWaitStatisticsRefresh},
		{"a room's update held", quietWaitRoomUpdate},
		{"a settlement owed", quietWaitSettlementOwed},
		{"a settlement owed, not asked", quietWaitSettlementNotAsked},
		{"a settlement armed, not asked", quietWaitSettlementArmed},
		{"a settlement armed past the deadline, not asked", quietWaitSettlementArmedPastTheDeadline},
		{"another backend holding an xid", quietWaitOtherBackend},
		{"a writer that starts after a first idle read", quietWaitLaterWriter},
		{"the repair fixture's server", quietWaitRepairFixture},
		{"a database with no registered server", quietWaitNoServer},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

// quietWaitNothingRunning: a server whose boot mark has been taken runs nothing, so the wait
// returns at its first read, and none of its reads finds anything busy.
func quietWaitNothingRunning(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	testwait.Receive(t, "the server's boot mark", served.bootMark)
	run := startQuietWait(t, database, true, nil)
	run.returned(t)
	run.eachRead(t, func(read quietRead) {
		if read.busy != "" {
			t.Errorf("a read of the quiet wait over a server that runs nothing found %q busy, want none", read.busy)
		}
	})
}

// quietWaitDetachedWork: work a server detached from its request (goDetached) is waited for until
// it returns.
func quietWaitDetachedWork(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	writer := newHeldPerson(t, database, "detached@quiet.example")
	served.detached.goDetached(writer.run)
	run := startQuietWait(t, database, false, writer.committed)
	run.hears(t, "the detached work busy", readBusy("detached work is running"))
	run.stillWaiting(t, "the detached work")
	writer.releaseOnce()
	run.returned(t)
}

// quietWaitKeyedRun: a keyed run counts itself from its start to its done (keyedWrites,
// request_keys.go), and the wait waits until done.
func quietWaitKeyedRun(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	writer := newHeldPerson(t, database, "keyed@quiet.example")
	served.detached.start()
	go func() {
		defer served.detached.done()
		writer.run()
	}()
	run := startQuietWait(t, database, false, writer.committed)
	run.hears(t, "the keyed run busy", readBusy("detached work is running"))
	run.stillWaiting(t, "the keyed run")
	writer.releaseOnce()
	run.returned(t)
}

// quietWaitReadWorker: an issue read starts the route roster fetch on a read worker
// (readRouteRoster, goReadWorker) and answers once its own context ends, while the listener holds
// the fetch's answer; the wait waits until the fetch has stored that answer (route_roster). The
// registry counted the read workers (registerQuietServer): production leaves them uncounted.
func quietWaitReadWorker(t *testing.T) {
	served, calls, release := heldRosterServer(t)
	if served.readWorkers == nil {
		t.Fatal("a registered server counts no read workers, want the registry to count them")
	}
	database := served.deps.Store
	ctx, cancel := context.WithCancel(context.Background())
	read := make(chan routeRoster, 1)
	go func() { read <- readRoster(t, served, ctx) }()
	awaitCalls(t, calls, 1)
	cancel()
	if roster := testwait.Receive(t, "the read to answer before the fetch it started", read); roster.answered {
		t.Fatal("the read answered the roster while the listener held its answer")
	}
	served.rosters.mu.Lock()
	fetch := served.rosters.fetch
	served.rosters.mu.Unlock()
	if fetch == nil {
		t.Fatal("no roster fetch is in flight once the read answered, want the one the listener holds")
	}
	run := startQuietWait(t, database, false, func() error {
		select {
		case <-fetch.done:
		default:
			return errors.New("the quiet wait returned before the roster fetch had stored the listener's answer")
		}
		var stored int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from route_roster`).Scan(&stored); err != nil {
			return fmt.Errorf("read the stored listener answer: %w", err)
		}
		if stored != 1 {
			return fmt.Errorf("the roster fetch stored %d answers, want 1", stored)
		}
		return nil
	})
	run.hears(t, "the read worker busy", readBusy("a read worker is running"))
	run.stillWaiting(t, "the roster fetch")
	release()
	run.returned(t)
}

// quietWaitMentionSend: a comment that mentions a live session answers before its send, which the
// mention sender makes after it; the listener (newBroadcastListener) holds the send's answer, and
// the wait waits until the send is recorded sent.
func quietWaitMentionSend(t *testing.T) {
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"]}]`
	release, releaseOnce := heldUntilReleased(t)
	sending := make(chan struct{})
	var sendOnce sync.Once
	listener, _ := newBroadcastListener(t, sessions, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send" {
			sendOnce.Do(func() { close(sending) })
			<-release
		}
		return false
	})
	// The server's clock is held, so no window of its own - the creation's lookup, the send's
	// receipt - ends while the listener holds its answer.
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, clock: clocktest.Held()})
	issue := createInteractionIssue(t, handler, "QWM", "A held mention", "A spec.\n")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Look at this while the listener holds the send.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	testwait.Receive(t, "the mention's send to reach the listener", sending)
	run := startQuietWait(t, database, false, func() error {
		var state string
		if err := database.Pool.QueryRow(context.Background(), `
			select state from comment_deliveries where comment_id = $1 and target = 'session:s1' and attempt = 1
		`, comment.ID).Scan(&state); err != nil {
			return fmt.Errorf("read the mention's attempt: %w", err)
		}
		if state != "sent" {
			return fmt.Errorf("the quiet wait returned with the mention's attempt %s, want it recorded sent", state)
		}
		return nil
	})
	run.hears(t, "the mention sender busy", readBusy("the mention sender holds a send"))
	run.stillWaiting(t, "the mention's send")
	releaseOnce()
	run.returned(t)
}

// quietWaitStatisticsRefresh: a refresh of a project's statistics runs on the server's
// singleflight (termStats), which the wait joins for each project; a refresh in flight is joined
// and waited for. The row's own call stands for the refresh a related-item check starts
// (ensureSearchTermStats).
func quietWaitStatisticsRefresh(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	createTestProject(t, handler, "QWS")
	writer := newHeldPerson(t, database, "statistics@quiet.example")
	started := make(chan struct{})
	served.termStats.DoChan("QWS", func() (any, error) {
		close(started)
		writer.run()
		return nil, nil
	})
	testwait.Receive(t, "the held refresh to start", started)
	run := startQuietWait(t, database, false, writer.committed)
	run.hears(t, "the join of the held refresh", readJoining("the refresh of project QWS's statistics"))
	run.stillWaiting(t, "the refresh")
	writer.releaseOnce()
	run.returned(t)
}

// quietWaitRoomUpdate: a browser's edit reaches the room, whose persistence appends it after the
// edit has left the browser; the store holds that append, and the wait waits, on the document
// service (IdleForTest), until the update is durable. A browser's socket is admitted only by a
// document service that knows the caller (docs.Deps.Identity), which newRegisteredTestServer's
// does not, so the row stands its server up as the browser rows do (newInteractionServer, with a
// document service of its own) and registers it for the wait itself.
func quietWaitRoomUpdate(t *testing.T) {
	release := make(chan struct{})
	appends := &heldAppends{release: release, started: make(chan struct{}), stored: make(chan struct{})}
	var documentService *docs.Service
	handler, database, _, served := newInteractionServer(t, func(database *store.Store) docs.API {
		appends.PgVersioned = docs.NewPgVersioned(database)
		documentService = docs.New(docs.Deps{
			Store: database, Persistence: appends, Identity: headerIdentity(database), Settle: 20 * time.Millisecond,
		})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	registerQuietServer(t, database, served, documentService)
	// The release runs first of the cleanups, before the server's wait and the service's
	// shutdown, which a held append would hold.
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	issue := createInteractionIssue(t, handler, "QWR", "A held room update", "A spec.\n")
	// The spec's own settlement runs first, so nothing but the browser's append stores its edit.
	awaitDocumentSettled(t, database, issue.PrimaryArtifactID)
	peer := connectBrowserPeer(t, documentService, issue.PrimaryArtifactID)
	peer.appendParagraph(t, "Typed while the room's append is held.")
	testwait.Receive(t, "the room's append of the browser's edit to start", appends.started)
	run := startQuietWait(t, database, false, func() error {
		select {
		case <-appends.stored:
			return nil
		default:
			return errors.New("the quiet wait returned before the room's append of the browser's edit was stored")
		}
	})
	run.hears(t, "the join of the document service's work", readJoining("its document service's work"))
	run.stillWaiting(t, "the room's append")
	releaseOnce()
	run.returned(t)
}

// quietWaitSettlementOwed: the spec's settlement is held once it holds the issue's owner lock
// (docs.Service.AfterSettleLockForTest); the document service reads it running, and the wait,
// asked to wait for every settlement, waits until it commits.
func quietWaitSettlementOwed(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	release, releaseOnce := heldUntilReleased(t)
	holding := make(chan string, 1)
	var held atomic.Bool
	deps.Docs.(*docs.Service).AfterSettleLockForTest(func(room string) {
		if held.CompareAndSwap(false, true) {
			holding <- room
			<-release
		}
	})
	createInteractionIssue(t, handler, "QWT", "A held settlement", "A spec.\n")
	room := testwait.Receive(t, "the spec's settlement to be held", holding)
	run := startQuietWait(t, database, true, func() error {
		owed, err := settlementOwed(database, room)
		if err != nil {
			return err
		}
		if owed {
			return errors.New("the quiet wait returned while the spec still owed its settlement")
		}
		return nil
	})
	run.hears(t, "the running settlement busy", readBusy("a settlement of document "+room+" is running"))
	run.stillWaiting(t, "the settlement")
	releaseOnce()
	run.returned(t)
}

// quietWaitSettlementNotAsked: a document owes its settlement and nothing in this process runs or
// arms it - the spec has settled, and its pending-settlement row is written again as another
// process's append writes it, which arms no timer here, as a closed issue's document owes one
// until a reopen - and a wait that is not asked to wait for settlements returns with it still
// owed.
func quietWaitSettlementNotAsked(t *testing.T) {
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "QWN", "An owed settlement", "A spec.\n")
	awaitDocumentSettled(t, database, issue.PrimaryArtifactID)
	if _, err := database.Pool.Exec(context.Background(),
		`insert into doc_settlements_pending (artifact_id) values ($1)`, issue.PrimaryArtifactID); err != nil {
		t.Fatalf("write the spec's pending settlement as another process's append does: %v", err)
	}
	run := startQuietWait(t, database, false, func() error {
		owed, err := settlementOwed(database, issue.PrimaryArtifactID)
		if err != nil {
			return err
		}
		if !owed {
			return errors.New("the spec's pending settlement is gone, want it still owed: nothing here settles it")
		}
		return nil
	})
	run.returned(t)
	run.eachRead(t, func(read quietRead) {
		if strings.Contains(read.busy, "owes its settlement") {
			t.Errorf("a read of the quiet wait not asked to wait for settlements found %q busy", read.busy)
		}
	})
}

// quietWaitSettlementArmed: a settlement armed to run before the test's deadline is work the
// measurement would meet, so a wait not asked to wait for settlements still waits while one is
// armed - as the close a closed issue's request is measured after arms one (readyIssueReopen).
// The timer is armed a minute out, which no run of this package's deadline is near, and the row
// settles the document at once (SettleIssueDocuments, which takes the armed timer) rather than
// wait the minute out.
func quietWaitSettlementArmed(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{settle: time.Minute})
	issue := createInteractionIssue(t, handler, "QWA", "An armed settlement", "A spec.\n")
	run := startQuietWait(t, database, false, func() error {
		owed, err := settlementOwed(database, issue.PrimaryArtifactID)
		if err != nil {
			return err
		}
		if owed {
			return errors.New("the quiet wait returned before the spec's armed settlement had run")
		}
		return nil
	})
	run.hears(t, "the armed settlement busy", readBusy("a settlement of document "+issue.PrimaryArtifactID+" is armed"))
	run.stillWaiting(t, "the armed settlement")
	if err := deps.Docs.(*docs.Service).SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the spec now: %v", err)
	}
	run.returned(t)
}

// quietWaitSettlementArmedPastTheDeadline: a settlement armed to fire an hour after the wait gives
// up (testwait.GiveUpAt: the binary's deadline less testwait's margin) fires after the binary's
// deadline under any -timeout, as a settlement of the fixtures that settle nothing during their
// test does (xidFixtureSettlesNothing), so it is no work a measurement meets: a wait not asked to
// wait for settlements returns with it still armed and its document still owing it. Under
// -timeout 0, whose wait has no deadline, a settlement of any delay counts as work (docs'
// settlesBeforeForTest), so the row has nothing to show there and skips.
func quietWaitSettlementArmedPastTheDeadline(t *testing.T) {
	deadline, bounded := testwait.GiveUpAt(t)
	if !bounded {
		t.Skip("under -timeout 0 an armed settlement of any delay is work the quiet wait waits for")
	}
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{settle: time.Until(deadline) + time.Hour})
	issue := createInteractionIssue(t, handler, "QWH", "A settlement armed past the deadline", "A spec.\n")
	run := startQuietWait(t, database, false, func() error {
		owed, err := settlementOwed(database, issue.PrimaryArtifactID)
		if err != nil {
			return err
		}
		if !owed {
			return errors.New("the spec's pending settlement is gone, want it still owed: its settlement is armed past the deadline")
		}
		return nil
	})
	run.returned(t)
	run.eachRead(t, func(read quietRead) {
		if strings.Contains(read.busy, "is armed") {
			t.Errorf("a read of the quiet wait found %q busy, want a settlement armed past the deadline left out", read.busy)
		}
	})
}

// quietWaitOtherBackend: a transaction on another connection that has written holds an xid until
// it commits, and the wait waits for that commit.
func quietWaitOtherBackend(t *testing.T) {
	_, database, _, _ := newRegisteredTestServer(t, testServerOptions{})
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the held transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `insert into people (email) values ($1)`, "xid@quiet.example"); err != nil {
		t.Fatalf("write in the held transaction: %v", err)
	}
	run := startQuietWait(t, database, false, func() error {
		return personRecorded(database, "xid@quiet.example")
	})
	run.hears(t, "the other backend's xid busy", readBusy("another backend of the database holds an xid"))
	run.stillWaiting(t, "the open transaction")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the held transaction: %v", err)
	}
	run.returned(t)
}

// quietWaitLaterWriter: detached work that, just before it returns, detaches a second writer
// hands the count on without its falling to nothing, and the wait waits for the second writer too.
func quietWaitLaterWriter(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	first := newHeldPerson(t, database, "first@quiet.example")
	second := newHeldPerson(t, database, "second@quiet.example")
	secondStarted := make(chan struct{})
	served.detached.goDetached(func() {
		first.run()
		served.detached.goDetached(func() {
			close(secondStarted)
			second.run()
		})
	})
	run := startQuietWait(t, database, false, second.committed)
	run.hears(t, "the first writer busy", readBusy("detached work is running"))
	run.stillWaiting(t, "the first writer")
	first.releaseOnce()
	testwait.Receive(t, "the second writer to start", secondStarted)
	run.stillWaiting(t, "the second writer")
	second.releaseOnce()
	run.returned(t)
}

// quietWaitRepairFixture: the repair fixture builds its server itself (newRepairFixtureOn), and
// registers it as newRegisteredTestServer registers its own, so work it detached is waited for.
func quietWaitRepairFixture(t *testing.T) {
	f := newRepairFixture(t)
	writer := newHeldPerson(t, f.database, "repair@quiet.example")
	f.served.detached.goDetached(writer.run)
	run := startQuietWait(t, f.database, false, writer.committed)
	run.hears(t, "the repair server's detached work busy", readBusy("detached work is running"))
	run.stillWaiting(t, "the repair server's detached work")
	writer.releaseOnce()
	run.returned(t)
}

// quietWaitNoServer: a database no server is registered on fails the wait at once, naming the
// database, since nothing tells the wait what runs there.
func quietWaitNoServer(t *testing.T) {
	database := storetest.Open(t)
	name := database.Pool.Config().ConnConfig.Database
	run := startQuietWait(t, database, false, nil)
	testwait.Receive(t, "the quiet wait to stop", run.done)
	if stop := run.stopping.stop(); !strings.Contains(stop, "no test server is registered on database "+name) {
		t.Fatalf("the quiet wait on a database no server is registered on stopped with %q, want a failure naming %s", stop, name)
	}
}

// heardRead is what a row hears of the quiet wait it runs (quietWaitRun): a read the wait told of,
// or, once, that the wait has ended - returned or stopped.
type heardRead struct {
	read  quietRead
	ended bool
}

// quietWaitRun is a quiet wait a row runs on a goroutine of its own (startQuietWait).
type quietWaitRun struct {
	heard    testwait.Stream[heardRead]
	stopping *stoppingTB
	done     chan struct{}
	// failed is the answer of the row's check of its writer (startQuietWait's atReturn), which the
	// wait's goroutine runs as the wait returns; it is read once done has closed.
	failed error
}

// startQuietWait runs the quiet wait on database (awaitQuiet), on a goroutine of its own under a
// testing.TB that stops that goroutine rather than fail the row (stoppingTB). As the wait returns
// it runs atReturn, when set, at once.
func startQuietWait(t *testing.T, database *store.Store, settled bool, atReturn func() error) *quietWaitRun {
	t.Helper()
	run := &quietWaitRun{stopping: &stoppingTB{TB: t}, done: make(chan struct{})}
	go func() {
		defer close(run.done)
		defer run.heard.Add(heardRead{ended: true})
		awaitQuiet(run.stopping, database, settled, func(read quietRead) { run.heard.Add(heardRead{read: read}) })
		if atReturn != nil {
			run.failed = atReturn()
		}
	}()
	return run
}

// hears waits for a read of the wait that matches, and fails the row should the wait return or
// stop first.
func (r *quietWaitRun) hears(t *testing.T, what string, match func(quietRead) bool) {
	t.Helper()
	testwait.Next(t, "the quiet wait to read "+what, &r.heard, func(heard heardRead) bool {
		if heard.ended {
			t.Fatalf("the quiet wait ended (stopped: %q) before any of its reads saw %s", r.stopping.stop(), what)
		}
		return match(heard.read)
	})
}

// stillWaiting fails the row unless the wait has not returned while what is held.
func (r *quietWaitRun) stillWaiting(t *testing.T, what string) {
	t.Helper()
	select {
	case <-r.done:
		t.Fatalf("the quiet wait returned while %s was held (stopped: %q)", what, r.stopping.stop())
	default:
	}
}

// returned waits for the wait to return, and fails the row when it stopped instead or the row's
// check at its return failed.
func (r *quietWaitRun) returned(t *testing.T) {
	t.Helper()
	testwait.Receive(t, "the quiet wait to return", r.done)
	if stop := r.stopping.stop(); stop != "" {
		t.Fatalf("the quiet wait stopped: %s", stop)
	}
	if r.failed != nil {
		t.Fatal(r.failed)
	}
}

// eachRead reads every read the wait tells of until the wait's end, and calls check on each.
func (r *quietWaitRun) eachRead(t *testing.T, check func(quietRead)) {
	t.Helper()
	for {
		heard := testwait.Next(t, "the quiet wait's next read", &r.heard, func(heardRead) bool { return true })
		if heard.ended {
			return
		}
		check(heard.read)
	}
}

// readBusy matches a read that ended finding busy what its text holds.
func readBusy(text string) func(quietRead) bool {
	return func(read quietRead) bool { return strings.Contains(read.busy, text) }
}

// readJoining matches a read about to block on a join of the writer its text names.
func readJoining(text string) func(quietRead) bool {
	return func(read quietRead) bool { return strings.Contains(read.joining, text) }
}

// heldPerson is a writer a row holds: run waits for the row's release, records email as a person,
// and closes wrote once that write has returned, its error in err.
type heldPerson struct {
	database    *store.Store
	email       string
	release     <-chan struct{}
	releaseOnce func()
	wrote       chan struct{}
	err         error
}

// newHeldPerson is a held writer of email on database, released by its releaseOnce or, at the
// latest, by t's end (heldUntilReleased), whose server wait runs t's releases first.
func newHeldPerson(t *testing.T, database *store.Store, email string) *heldPerson {
	t.Helper()
	release, releaseOnce := heldUntilReleased(t)
	return &heldPerson{database: database, email: email, release: release, releaseOnce: releaseOnce, wrote: make(chan struct{})}
}

func (h *heldPerson) run() {
	<-h.release
	_, h.err = h.database.Pool.Exec(context.Background(), `insert into people (email) values ($1)`, h.email)
	close(h.wrote)
}

// committed is the row's check as the wait returns: the writer's write has returned and is in the
// database.
func (h *heldPerson) committed() error {
	select {
	case <-h.wrote:
	default:
		return fmt.Errorf("the quiet wait returned before the held writer of %s had written", h.email)
	}
	if h.err != nil {
		return fmt.Errorf("the held writer of %s: %w", h.email, h.err)
	}
	return personRecorded(h.database, h.email)
}

// personRecorded answers an error unless email is recorded as a person on database.
func personRecorded(database *store.Store, email string) error {
	var recorded bool
	if err := database.Pool.QueryRow(context.Background(), `select exists (select 1 from people where email = $1)`, email).Scan(&recorded); err != nil {
		return fmt.Errorf("read the person %s: %w", email, err)
	}
	if !recorded {
		return fmt.Errorf("the quiet wait returned before %s's write had committed", email)
	}
	return nil
}

// settlementOwed reads whether artifactID owes its settlement on database: whether its
// pending-settlement row is there. It takes no t, so a row's check at the wait's return, which
// runs off the test goroutine, can read it.
func settlementOwed(database *store.Store, artifactID string) (bool, error) {
	var owed bool
	if err := database.Pool.QueryRow(context.Background(),
		`select exists (select 1 from doc_settlements_pending where artifact_id = $1)`, artifactID).Scan(&owed); err != nil {
		return false, fmt.Errorf("read the spec's pending settlement: %w", err)
	}
	return owed, nil
}

// awaitDocumentSettled waits until artifactID owes no settlement: its pending-settlement row is
// gone (documentOwesSettlement). Its one bound is the test binary's deadline (testwait.Until).
func awaitDocumentSettled(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	testwait.Until(t, "the pending settlement of "+artifactID+" to be gone (owed at the last read)", func() (bool, bool) {
		owed := documentOwesSettlement(t, database, artifactID)
		return owed, !owed
	})
}

// heldAppends is a document store whose first append of a room's own update - a batch the room's
// persistence stores (AppendRoomFlush), or an update no class was recorded for
// (AppendUpdatesWithCredit) - waits for release: started closes once that append has begun, and
// stored once it has stored the update.
type heldAppends struct {
	*docs.PgVersioned
	release <-chan struct{}
	holding atomic.Bool
	started chan struct{}
	stored  chan struct{}
}

// hold holds the store's first append until release, and reports whether this one was it.
func (h *heldAppends) hold() bool {
	if !h.holding.CompareAndSwap(false, true) {
		return false
	}
	close(h.started)
	<-h.release
	return true
}

func (h *heldAppends) AppendRoomFlush(ctx context.Context, room string, flush *docs.RoomFlush, through uint64, credit *docs.UpdateCredit) (persistence.Version, error) {
	held := h.hold()
	version, err := h.PgVersioned.AppendRoomFlush(ctx, room, flush, through, credit)
	if held && err == nil {
		close(h.stored)
	}
	return version, err
}

func (h *heldAppends) AppendUpdatesWithCredit(ctx context.Context, room string, updates []docs.BrowserUpdate) (persistence.Version, error) {
	held := h.hold()
	version, err := h.PgVersioned.AppendUpdatesWithCredit(ctx, room, updates)
	if held && err == nil {
		close(h.stored)
	}
	return version, err
}

// stoppingTB is a testing.TB whose Fatal, Fatalf and FailNow record why and stop the goroutine that
// calls them (runtime.Goexit), as a test's own do, without failing the test it wraps.
type stoppingTB struct {
	testing.TB
	mu      sync.Mutex
	stopped string
}

func (s *stoppingTB) Fatalf(format string, args ...any) {
	s.mu.Lock()
	s.stopped = fmt.Sprintf(format, args...)
	s.mu.Unlock()
	runtime.Goexit()
}

func (s *stoppingTB) Fatal(args ...any) { s.Fatalf("%s", fmt.Sprint(args...)) }

func (s *stoppingTB) FailNow() { s.Fatalf("FailNow") }

// stop is why the TB was stopped, empty when it was not.
func (s *stoppingTB) stop() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}
