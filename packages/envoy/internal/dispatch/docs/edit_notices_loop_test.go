package docs

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// newEditNoticeSweeper is a second document service on database that only sweeps, as another
// process's would: given a traced store, every statement its loop makes is counted apart from the
// settlements the test drives through its own service.
func newEditNoticeSweeper(t *testing.T, database *store.Store) *Service {
	t.Helper()
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service
}

// runEditNoticeLoop runs the sweep until the test ends, and fails the test, by name, if it never
// returns once its context ends. That wait ends when the sweep returns, and its one bound is the
// test binary's deadline (testwait.Receive), so a sweep that is merely slow to stop passes.
func runEditNoticeLoop(t *testing.T, run func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "the edit notice sweep to return once its context ended", done)
	})
}

// startEditNoticeHub runs a real pglisten.Hub on database's listener with the sweep's channel
// registered, until the test ends. It returns the hub's wake once the hub has connected and the
// signal the hub gives every wake after its LISTEN has been taken off it, so the wake's next signal
// is a notification's: a loop started on it runs its boot pass alone. Left on the wake, that signal
// earns a second pass or none, as the loop's first select happens to take it before or after its
// boot pass.
func startEditNoticeHub(t *testing.T, database *store.Store) <-chan struct{} {
	t.Helper()
	hub := &pglisten.Hub{Owner: "edit notices test", Dial: database.Pool.Listener}
	wake := hub.Wake(EditNoticesChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	testwait.Receive(t, "the edit notices hub to connect", connected)
	testwait.Receive(t, "the hub's signal of the sweep's wake after its LISTEN", wake)
	return wake
}

// leasedPassStatements counts every statement a leased loop's pools start, as
// storetest.StatementCounter does, and the leases those statements free. duty frees a leased pass's
// lease with the pass's last statement (freeLeaseOn, deferred until the pass has returned), and the
// loop starts no statement after it until its next pass, so a pass has ended once its free is
// counted: a fact, where a span with no statement in it is a guess that a stall inside a pass on a
// loaded machine makes wrong.
type leasedPassStatements struct {
	storetest.StatementCounter
	freed atomic.Int64
}

// freeLeaseStatement is the statement duty gives a lease back with (duty's freeLease): the unlock
// of two keys, duty's lease namespace and the lease name's hashtext. A document room's lock is
// given back with an unlock of one key, which this does not count.
const freeLeaseStatement = `select pg_advisory_unlock($1, hashtext($2))`

// TraceQueryStart counts the statement before it counts a free, so a reader that sees a free
// counted also sees its statement counted.
func (s *leasedPassStatements) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx = s.StatementCounter.TraceQueryStart(ctx, conn, data)
	if data.SQL == freeLeaseStatement {
		s.freed.Add(1)
	}
	return ctx
}

// awaitPasses waits until passes of the loop's passes have ended and freed their lease. It sets no
// span of its own (waitUntil).
func (s *leasedPassStatements) awaitPasses(t *testing.T, passes int64) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("%d of the loop's passes to end and free their lease", passes), func() bool {
		return s.freed.Load() >= passes
	})
}

// requireNoStatementsBefore fails the test if the sweep starts a statement from now until end;
// what names the window. It reads the count before the clock, so a statement it counts started
// before a clock reading that was still short of end: a slow machine reads later, which only
// shortens the span the window sees, and never counts a statement started after end.
func (s *leasedPassStatements) requireNoStatementsBefore(t *testing.T, end time.Time, what string) {
	t.Helper()
	before := s.Count()
	for {
		made := s.Count() - before
		now := time.Now()
		if !now.Before(end) {
			return
		}
		if made != 0 {
			t.Fatalf("%s: %d statements %v before the window's end, want none", what, made, end.Sub(now).Round(time.Millisecond))
		}
		time.Sleep(testwait.Poll)
	}
}

// noticesSentBefore counts the notices about document whose event the database wrote before at,
// on its own clock: a count that a sweep running after at cannot change, as it changes a count of
// every notice sent.
func noticesSentBefore(t *testing.T, database *store.Store, document string, at time.Time) int {
	t.Helper()
	var sent int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where type = 'artifact.human_edited' and payload->>'artifact_id' = $1 and created_at < $2
	`, document, at).Scan(&sent); err != nil {
		t.Fatalf("count the notices sent before %v: %v", at, err)
	}
	return sent
}

// With no notice due - one held, naming nobody, and one an hour off - the sweep makes no statement
// at all after its boot pass, over a window longer than the five-second period of the timer it
// replaced: no sweep on a timer and no lease attempt. Its pass asks to run again at the hour-off
// notice's due time, the one timer it keeps.
func TestEditNoticeSweepMakesNoStatementWithNothingDue(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	held := createIssueDocument(t, database, 1, "Held.")
	later := createIssueDocument(t, database, 2, "Later.")
	if _, err := database.Pool.Exec(ctx, `
		insert into document_edit_notices (artifact_id, from_version, to_version, editors, last_editor, due_at)
		values ($1, 1, 2, '["alice@example.com"]', null, null),
		       ($2, 1, 2, '["alice@example.com"]', '{"kind":"user","id":"alice@example.com"}', now() + interval '1 hour')
	`, held, later); err != nil {
		t.Fatalf("open a held notice and one due in an hour: %v", err)
	}
	statements := &leasedPassStatements{}
	counted := &store.Store{Pool: storetest.Traced(t, database.Pool, statements, nil)}
	sweeper := newEditNoticeSweeper(t, counted)
	leases := excerptLeases(t, counted)
	runEditNoticeLoop(t, func(ctx context.Context) { sweeper.RunEditNotices(ctx, leases, nil) })

	// With no wake the loop runs its boot pass alone.
	statements.awaitPasses(t, 1)
	// A slow machine cannot fail this window on a correct run: with nothing due inside the hour the
	// sweep makes no statement at any pace, so a slower run only shortens what the window sees.
	storetest.RequireNoStatements(t, &statements.StatementCounter, editNoticeSweepSchedule.minGap+2*time.Second, "the edit notice sweep with nothing due")
	next := sweeper.editNoticePass(ctx)
	if until := time.Until(next); until < 59*time.Minute || until > time.Hour {
		t.Fatalf("a pass with only an hour-off notice asked to run again in %v; want at that notice's due time, an hour off", until)
	}
	if edits := len(humanEdits(t, database, held)) + len(humanEdits(t, database, later)); edits != 0 {
		t.Fatalf("the sweep sent %d notices; want none, nothing being due", edits)
	}
}

// A person's settled edit while the sweep is idle wakes it through the notice's notification on a
// real pglisten.Hub; that sweep finds nothing due yet, arms one timer for the notice's due time and
// makes no statement until then, and the notice goes once its quiet window ends, not before. The
// loop had no due time before the edit, so only the wake can have scheduled the sweep that sends
// it. The sweep here starts passes 200 ms apart, not five seconds.
func TestEditNoticeSweepSendsAWokenNoticeAtItsDueTime(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const quiet = 3 * time.Second
	service.editNoticeQuiet = quiet
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)

	wake := startEditNoticeHub(t, service.store)
	statements := &leasedPassStatements{}
	counted := &store.Store{Pool: storetest.Traced(t, service.store.Pool, statements, nil)}
	sweeper := newEditNoticeSweeper(t, counted)
	leases := excerptLeases(t, counted)
	schedule := editNoticeSchedule{minGap: 200 * time.Millisecond}
	runEditNoticeLoop(t, func(ctx context.Context) { sweeper.runEditNotices(ctx, leases, wake, schedule) })
	// The boot pass, then nothing: the hub's signal after its LISTEN was taken off the wake.
	statements.awaitPasses(t, 1)

	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	settleCurrentGeneration(t, service, artifactID)
	// due is at or before the notice's due time on this process's clock: the read's now() is
	// taken after readAt, so readAt and what was left of the window at that now() come to no
	// later than the window's end, however long the read took.
	readAt := time.Now()
	row, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("alice's settled edit opened no edit notice")
	}
	due := readAt.Add(row.dueAt.Sub(row.now))
	// The notice's notification wakes the sweep, whose pass finds nothing due yet and arms one
	// timer for the notice's due time.
	statements.awaitPasses(t, 2)
	if sent := noticesSentBefore(t, service.store, artifactID, row.dueAt); sent != 0 {
		t.Fatalf("the woken sweep sent %d notices inside the quiet window; want none before it ends", sent)
	}
	// Until the due time the sweep holds its timer and asks nothing. A slow machine cannot fail
	// this window on a correct run: the window ends at or before the due time, the timer fires at
	// or after it, and a later reading only shortens what the window sees.
	statements.requireNoStatementsBefore(t, due, "the edit notice sweep waiting for a notice's due time")

	waitUntil(t, "the notice to go once its quiet window ended", func() bool {
		return len(humanEdits(t, service.store, artifactID)) == 1
	})
	var sentAt time.Time
	if err := service.store.Pool.QueryRow(context.Background(), `
		select created_at from events where type = 'artifact.human_edited' and payload->>'artifact_id' = $1
	`, artifactID).Scan(&sentAt); err != nil {
		t.Fatalf("read when the notice went: %v", err)
	}
	if late := sentAt.Sub(row.dueAt); late < 0 || late > schedule.minGap+time.Second {
		t.Fatalf("the notice went %v after its due time; want at it, within the sweep's gap and a second", late)
	}
	if edit := humanEdits(t, service.store, artifactID)[0]; !edit.actor.SameAs(alice) {
		t.Fatalf("the notice names %+v; want alice", edit.actor)
	}
}

// A sweep that finds the edit-notices lease held - the other process's sweep inside its pass -
// keeps trying, every gap plus up to a tenth, until a sweep of its own runs, and that sweep sends
// the due notice: the other process's pass may have read before it was due.
func TestEditNoticeSweepRetriesAHeldLease(t *testing.T) {
	database := storetest.Open(t)
	document := createIssueDocument(t, database, 1, "First.")
	addPersonsVersion(t, database, document, 2, "Second.\n", model.Actor{Kind: "user", ID: "alice@example.com"})
	logs := captureLogs(t)
	leases := excerptLeases(t, database)
	taken, release := make(chan struct{}), make(chan struct{})
	held := make(chan bool, 1)
	go func() {
		held <- duty.Exclusive(context.Background(), leases, editNoticeLease, time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	testwait.Receive(t, "the other caller to take the edit-notices lease", taken)

	sweeper := newEditNoticeSweeper(t, database)
	runEditNoticeLoop(t, func(ctx context.Context) {
		sweeper.runEditNotices(ctx, leases, nil, editNoticeSchedule{minGap: 100 * time.Millisecond})
	})
	waitUntil(t, "the sweep to find the lease held twice", func() bool {
		return countExcerptLeaseLines(logs, "dispatch duty: leased pass skipped", editNoticeLease) >= 2
	})
	if edits := humanEdits(t, database, document); len(edits) != 0 {
		t.Fatalf("the sweep sent %d notices while the lease was held elsewhere; want none", len(edits))
	}
	close(release)
	if !<-held {
		t.Fatal("the other caller never held the lease, so the sweep was free to run")
	}
	waitUntil(t, "the sweep to send the due notice once the lease was free", func() bool {
		return len(humanEdits(t, database, document)) == 1
	})
}
