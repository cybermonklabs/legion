package docs

import (
	"context"
	"sync"
	"testing"
	"time"

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
// process's would, with its loop's gaps and due times on fake: the loop waits where a server's
// waits, and it runs a pass only where the test moves the fake to a deadline it armed.
func newEditNoticeSweeper(t *testing.T, database *store.Store, fake *clocktest.Fake) *Service {
	t.Helper()
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour, Clock: fake})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service
}

// runEditNoticeLoop runs the sweep until the test ends, and fails the test if it does not return
// once its context ends.
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
// registered, until the test ends or stop is called, and returns its wake once the hub has
// connected.
func startEditNoticeHub(t *testing.T, database *store.Store) (wake <-chan struct{}, stop func()) {
	t.Helper()
	hub := &pglisten.Hub{Owner: "edit notices test", Dial: database.Pool.Listener}
	wake = hub.Wake(EditNoticesChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	stop = sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	testwait.Receive(t, "the hub to connect", connected)
	return wake, stop
}

// awaitOneSweepTimer waits until the sweep's loop holds exactly one timer on fake, which accept
// takes, and returns its deadline: the loop arms one timer for its next attempt and none between
// attempts, so its armed deadline is the whole of what it will do next, and a fake the test does
// not move runs none of it.
func awaitOneSweepTimer(t *testing.T, fake *clocktest.Fake, what string, accept func(time.Time) bool) time.Time {
	t.Helper()
	armed := testwait.Until(t, what, func() ([]time.Time, bool) {
		armed := fake.Armed()
		return armed, len(armed) == 1 && accept(armed[0])
	})
	return armed[0]
}

// editNoticeDueIn is how long the earliest due time of any notice is off on the database's clock,
// the reading the sweep's pass arms its timer by (nextEditNoticeDue).
func editNoticeDueIn(t *testing.T, database *store.Store) time.Duration {
	t.Helper()
	var micros *int64
	if err := database.Pool.QueryRow(context.Background(), nextEditNoticeDue).Scan(&micros); err != nil {
		t.Fatalf("read how long until the earliest notice is due: %v", err)
	}
	if micros == nil {
		t.Fatal("no notice has a due time")
	}
	return time.Duration(*micros) * time.Microsecond
}

// With no notice due - one held, naming nobody, and one an hour off - the sweep's boot pass arms
// one timer, for the hour-off notice's due time, and nothing else: no sweep on a timer of five
// seconds, the period of the timer this loop replaced, and no lease attempt between. The loop
// waits on its clock alone, so the one deadline it holds on a fake the test does not move is
// everything it will do; a loop that swept on a period would hold that period's timer too. Its
// pass asks to run again at that due time.
func TestEditNoticeSweepMakesNoStatementWithNothingDue(t *testing.T) {
	fake := clocktest.New(time.Now())
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
	sweeper := newEditNoticeSweeper(t, database, fake)
	leases := excerptLeases(t, database)
	runEditNoticeLoop(t, func(ctx context.Context) { sweeper.RunEditNotices(ctx, leases, nil) })

	// Only a pass that read the due time can have armed this, so the boot pass ran.
	armed := awaitOneSweepTimer(t, fake, "the sweep's one timer, for the hour-off notice's due time", func(at time.Time) bool {
		return at.After(fake.Now().Add(editNoticeMinGap))
	})
	// The pass read the due time before this read, so what it armed is further off than this.
	if until, left := armed.Sub(fake.Now()), editNoticeDueIn(t, database); until > time.Hour || until < left {
		t.Fatalf("the sweep's boot pass armed its timer %v off; want the hour-off notice's due time, between %v and an hour", until, left)
	}
	next := sweeper.editNoticePass(ctx)
	if until, left := next.Sub(fake.Now()), editNoticeDueIn(t, database); until > time.Hour || until < left {
		t.Fatalf("a pass with only an hour-off notice asked to run again in %v; want at that notice's due time, between %v and an hour", until, left)
	}
	if edits := len(humanEdits(t, database, held)) + len(humanEdits(t, database, later)); edits != 0 {
		t.Fatalf("the sweep sent %d notices; want none, nothing being due", edits)
	}
}

// A person's settled edit while the sweep is idle wakes it through the notice's notification on a
// real pglisten.Hub; the sweep that wake earns finds nothing due yet, arms one timer for the
// notice's due time and holds nothing else, and the notice goes when the loop reaches that time,
// not before. The loop had no due time before the edit, so only the wake can have scheduled the
// sweep that sends it.
func TestEditNoticeSweepSendsAWokenNoticeAtItsDueTime(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)

	wake, stopHub := startEditNoticeHub(t, service.store)
	fake := clocktest.New(time.Now())
	sweeper := newEditNoticeSweeper(t, service.store, fake)
	leases := excerptLeases(t, service.store)
	runEditNoticeLoop(t, func(ctx context.Context) { sweeper.RunEditNotices(ctx, leases, wake) })

	alice := model.Actor{Kind: "user", ID: "alice@example.com"}
	editAsConnectedPeer(t, service, artifactID, 1, alice, replaceRun("First.", "First, edited."))
	settleCurrentGeneration(t, service, artifactID)
	if _, open := readEditNotice(t, service, artifactID); !open {
		t.Fatal("alice's settled edit opened no edit notice")
	}
	// The notice's notification wakes the loop, which starts its pass no sooner than its gap from
	// the pass before; the boot pass and the hub's signal after its LISTEN are the passes before it.
	for {
		at := awaitOneSweepTimer(t, fake, "the sweep's timer for the pass its wake earned", func(time.Time) bool { return true })
		if at.After(fake.Now().Add(editNoticeMinGap)) {
			// Further off than the gap: the due time a pass read after alice's notice was written.
			if until, left := at.Sub(fake.Now()), editNoticeDueIn(t, service.store); until > service.editNoticeQuiet || until < left {
				t.Fatalf("the woken sweep armed its timer %v off; want the notice's due time, between %v and the quiet window %v", until, left, service.editNoticeQuiet)
			}
			if edits := humanEdits(t, service.store, artifactID); len(edits) != 0 {
				t.Fatalf("the woken sweep sent %d notices inside the quiet window; want none before it ends", len(edits))
			}
			// The hub stops, so the ageing below wakes nothing: only the timer armed for the due
			// time can run the pass that sends the notice.
			stopHub()
			makeEditNoticeDue(t, service.store, artifactID)
			fake.Set(at)
			break
		}
		fake.Set(at)
	}
	waitFor(t, "the notice to go once the loop reached its due time", func() bool {
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
	fake := clocktest.New(time.Now())
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

	sweeper := newEditNoticeSweeper(t, database, fake)
	runEditNoticeLoop(t, func(ctx context.Context) { sweeper.RunEditNotices(ctx, leases, nil) })
	// Each attempt that finds the lease held arms the next one a gap, plus up to a tenth, off.
	for try := range 2 {
		at := awaitOneSweepTimer(t, fake, "the sweep's timer for its retry of the held lease", func(time.Time) bool { return true })
		if wait := at.Sub(fake.Now()); wait < editNoticeMinGap || wait > editNoticeMinGap+editNoticeMinGap/10 {
			t.Fatalf("retry %d of the held lease is armed %v off; want the sweep's gap %v plus up to a tenth", try+1, wait, editNoticeMinGap)
		}
		fake.Set(at)
	}
	waitFor(t, "the sweep to find the lease held twice", func() bool {
		return countExcerptLeaseLines(logs, "dispatch duty: leased pass skipped", editNoticeLease) >= 2
	})
	if edits := humanEdits(t, database, document); len(edits) != 0 {
		t.Fatalf("the sweep sent %d notices while the lease was held elsewhere; want none", len(edits))
	}
	close(release)
	if !<-held {
		t.Fatal("the other caller never held the lease, so the sweep was free to run")
	}
	fake.Set(awaitOneSweepTimer(t, fake, "the sweep's timer for its retry once the lease is free", func(time.Time) bool { return true }))
	waitFor(t, "the sweep to send the due notice once the lease was free", func() bool {
		return len(humanEdits(t, database, document)) == 1
	})
}
