package docs

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
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
// settlements the test drives through its own service. clk is its clock (Deps.Clock), nil for the
// real one.
func newEditNoticeSweeper(t *testing.T, database *store.Store, clk clock.Clock) *Service {
	t.Helper()
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour, Clock: clk})
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

// relayWakes hands each signal of wake on to the wake it returns, a buffer of one as a
// pglisten.Hub's is, until the stop it returns is called; a signal after that is taken off wake
// and dropped. A test that makes a notice due by writing its row, which notifies the hub as every
// writer's does, stops the relay first, so the loop it feeds sweeps on its own timer alone.
func relayWakes(t *testing.T, wake <-chan struct{}) (<-chan struct{}, func()) {
	t.Helper()
	relayed := make(chan struct{}, 1)
	var stopped atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				if stopped.Load() {
					continue
				}
				select {
				case relayed <- struct{}{}:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return relayed, func() { stopped.Store(true) }
}

// awaitArmed waits until the deadlines armed on clk answer want, and returns them; what names the
// wait. It sets no span of its own (testwait.UntilPolling, at waitUntil's pace), so a loop slow to
// arm its timer passes, and one that never arms what the test expects fails by name with the
// deadlines it had armed.
func awaitArmed(t *testing.T, clk *clocktest.Fake, what string, want func(armed []time.Time) bool) []time.Time {
	t.Helper()
	return testwait.UntilPolling(t, 5*time.Millisecond, what, func() ([]time.Time, bool) {
		armed := clk.Armed()
		return armed, want(armed)
	})
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
	sweeper := newEditNoticeSweeper(t, counted, nil)
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
// real pglisten.Hub; that sweep finds nothing due yet, arms one timer, for the notice's due time,
// and makes no statement until the timer fires, even once the notice is due on the database's
// clock; then the notice goes. The loop had no due time before the edit, so only the wake can
// have scheduled the sweep that sends it. The loop runs on a fake clock the test moves
// (Deps.Clock), so when it sweeps is the timer it armed and never the machine's pace. The quiet
// time is an hour, so the woken sweep finds the notice not yet due however late it runs, and the
// test makes the notice due by moving its row into the past (makeEditNoticeDue), as the quiet
// time passing does: a fake Go clock does not move the database's, which judges a notice due.
func TestEditNoticeSweepSendsAWokenNoticeAtItsDueTime(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.editNoticeQuiet = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)

	wake, stopWakes := relayWakes(t, startEditNoticeHub(t, service.store))
	statements := &leasedPassStatements{}
	counted := &store.Store{Pool: storetest.Traced(t, service.store.Pool, statements, nil)}
	booted := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	clk := clocktest.New(booted)
	sweeper := newEditNoticeSweeper(t, counted, clk)
	leases := excerptLeases(t, counted)
	schedule := editNoticeSchedule{minGap: 200 * time.Millisecond}
	runEditNoticeLoop(t, func(ctx context.Context) { sweeper.runEditNotices(ctx, leases, wake, schedule) })
	// The boot pass, then nothing: the hub's signal after its LISTEN was taken off the wake, and a
	// pass that finds no notice arms no timer.
	statements.awaitPasses(t, 1)

	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	settleCurrentGeneration(t, service, artifactID)
	row, open := readEditNotice(t, service, artifactID)
	if !open {
		t.Fatal("alice's settled edit opened no edit notice")
	}
	// The notice's notification wakes the loop, which holds its next pass to the gap after its
	// boot pass began.
	gapEnd := booted.Add(schedule.minGap)
	awaitArmed(t, clk, "the woken loop to arm its timer for the end of its gap", func(deadlines []time.Time) bool {
		return len(deadlines) == 1 && deadlines[0].Equal(gapEnd)
	})
	// The woken pass reads how long the notice has left between these two reads of the database's
	// clock, the one due_at is judged by.
	beforePass := databaseNow(t, service.store)
	clk.Set(gapEnd)
	armed := awaitArmed(t, clk, "the woken pass to arm the loop's timer for the notice's due time", func(deadlines []time.Time) bool {
		return len(deadlines) == 1 && deadlines[0].After(gapEnd)
	})
	afterPass := databaseNow(t, service.store)
	statements.awaitPasses(t, 2)
	if edits := humanEdits(t, service.store, artifactID); len(edits) != 0 {
		t.Fatalf("the woken sweep sent %d notices inside the notice's hour of quiet; want none before it ends", len(edits))
	}
	// The timer is the notice's due time on the loop's clock: the pass began at gapEnd and added
	// what the notice had left as the pass read it, due_at less the database's now() rounded up to
	// a microsecond (nextEditNoticeDue).
	if left, least, most := armed[0].Sub(gapEnd), row.dueAt.Sub(afterPass), row.dueAt.Sub(beforePass)+time.Microsecond; left < least || left > most {
		t.Fatalf("the woken pass armed the loop's timer %v after the pass began; want what the notice had left as the pass read it, %v to %v", left, least, most)
	}

	// Due on the database's clock now, the notice still waits for the loop's timer: the sweep makes
	// no statement until the fake clock reaches it. The row's write notifies the hub as every
	// writer's does, so the relay stops first and drops that signal, leaving the loop its timer
	// alone.
	stopWakes()
	idle := statements.Count()
	makeEditNoticeDue(t, service.store, artifactID)
	// A slow machine cannot fail this window on a correct run: the loop's one timer is on the fake
	// clock, which nothing moves during it, so a slower run only shortens what the window sees.
	storetest.RequireNoStatements(t, &statements.StatementCounter, 5*schedule.minGap, "the edit notice sweep waiting for its timer while its notice is due")
	if made := statements.Count() - idle; made != 0 {
		t.Fatalf("the edit notice sweep made %d statements once its notice was due and before its timer fired; want none", made)
	}

	clk.Set(armed[0])
	waitUntil(t, "the notice to go once the loop's timer for its due time fired", func() bool {
		return len(humanEdits(t, service.store, artifactID)) == 1
	})
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

	sweeper := newEditNoticeSweeper(t, database, nil)
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
