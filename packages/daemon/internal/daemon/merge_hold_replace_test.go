package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A controller replacing the hold list and a merge run deciding that a hold holds its submission
// are two transactions, and what matters is the window where they overlap. The run reads the
// list, calls GitHub for the pull request's changed files, and only then writes what it decided;
// the route replaces the list and makes every merge a hold is still keeping back due at once,
// which it finds by the held moment on the row's payload (api's mergeHolds,
// record.Store.HeldReadyMerges).
//
// The project's merge-hold lock leaves the two of them one order or the other and no third, and
// the merge is woken in both. The replacement first: its whole transaction commits while the run
// is inside the GitHub read, so its scan finds no row carrying a held moment — this row carries
// none yet — and the run, which decides under the lock on the list as it stands, holds nothing
// and submits the merge. Without that re-read the run writes the held moment it decided on the
// list it read before the call, and the merge sleeps out the whole of a hold the controller has
// already taken off, with nothing left to wake it. The run first: its decision, its held moment
// and its deferral to the hold's end are one commit, so the replacement that follows — in the
// run's last moments, or long after it — finds the row and expedites it, and nothing of the run
// puts it back to sleep. The three cases below are those two and the ordinary one between them,
// which has always worked and must go on working.
func TestAHoldListReplacedWhileAMergeRunDecidesLeavesNoMergeAsleep(t *testing.T) {
	t.Run("the replacement lands while the run reads the pull request's files", func(t *testing.T) {
		github := newQueueGitHub(t, "CLEAN")
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		now := time.Now().UTC().Truncate(time.Second)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
		ends := now.Add(30 * time.Minute)
		setMergeHolds(t, pool, records, windowHold(ends, heldPattern))
		github.set(func(g *armableGitHub) {
			g.changedFiles = []string{heldFile}
			g.atFiles = func() { replaceHoldsAsTheRouteDoes(t, pool, records, now) }
		})
		runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now })

		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}

		if _, arms := github.counts(); arms != 1 {
			t.Fatalf("submissions %d, want the one merge: the hold it read is gone, and the list as it stands holds nothing: %v",
				arms, github.requestLog())
		}
		if posts := messagePosts(t, pool); len(posts) != 0 {
			t.Fatalf("Dispatch messages %q, want none: no hold of the list the run decided on holds this merge", posts)
		}
		assertMergeRows(t, pool, 1, "merge-held", 0)
		if held, since, told := heldMarks(t, pool); held != 0 || !since.IsZero() || told {
			t.Fatalf("the row holds %s held since %s told %t, want a row no hold caught", held, since, told)
		}
		nextAt, _, _, _ := readyMergeState(t, pool)
		if !nextAt.Before(ends) {
			t.Fatalf("the row runs again at %s, want a moment before the dead hold's end %s: the merge was submitted, "+
				"and a row asleep on that hold's clock has nothing left to wake it", nextAt, ends)
		}
	})

	t.Run("the replacement follows the run, and its own expedite makes the row due", func(t *testing.T) {
		github := newQueueGitHub(t, "CLEAN")
		github.set(func(g *armableGitHub) { g.changedFiles = []string{heldFile} })
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		now := time.Now().UTC().Truncate(time.Second)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
		ends := now.Add(30 * time.Minute)
		setMergeHolds(t, pool, records, windowHold(ends, heldPattern))
		runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return now })

		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}

		if _, arms := github.counts(); arms != 0 {
			t.Fatalf("submissions %d, want none: the hold holds the merge back", arms)
		}
		nextAt, _, _, _ := readyMergeState(t, pool)
		if !nextAt.Equal(ends) {
			t.Fatalf("the held row runs again at %s, want the hold's end %s", nextAt, ends)
		}

		later := now.Add(time.Minute)
		replaceHoldsAsTheRouteDoes(t, pool, records, later)

		if nextAt, _, _, _ := readyMergeState(t, pool); !nextAt.Equal(later) {
			t.Fatalf("the row the controller released runs again at %s, want the moment of the call %s", nextAt, later)
		}
	})

	t.Run("the replacement's scan lands between the run's write and the end of the run", func(t *testing.T) {
		github := newQueueGitHub(t, "CLEAN")
		github.set(func(g *armableGitHub) { g.changedFiles = []string{heldFile} })
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
		now := time.Now().UTC().Truncate(time.Second)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
		ends := now.Add(30 * time.Minute)
		setMergeHolds(t, pool, records, windowHold(ends, heldPattern))
		// The clock is the seam: the run's own writes are all it has left to make once the hold
		// decision has committed, so the first reading of the clock that sees the held moment in
		// the record is the moment the controller's replacement can land in. This is the other
		// order of the two — the run's write first, the replacement's scan after it — and it is
		// the one that needs the row's deferral written inside the decision's own transaction:
		// a row still carrying the next_at it was claimed at is a row that scan cannot see, and
		// the run would then write the dead hold's end over everything the controller did.
		var once sync.Once
		clock := func() time.Time {
			if heldMomentRecorded(t, pool) {
				once.Do(func() { replaceHoldsAsTheRouteDoes(t, pool, records, now) })
			}
			return now
		}
		runner := mergeRunner(pool, records, github.url, quietLogger(), clock)

		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}

		if _, arms := github.counts(); arms != 0 {
			t.Fatalf("submissions %d, want none: the hold stood when the run read the list", arms)
		}
		if nextAt, _, _, _ := readyMergeState(t, pool); !nextAt.Equal(now) {
			t.Fatalf("the row runs again at %s, want the moment of the controller's call %s: the replacement found the row "+
				"and expedited it, and nothing of the run that held it may put it back to sleep at %s", nextAt, now, ends)
		}
	})
}

// Each hold that holds one merge posts its own line on the issue, and the architect's merge-held
// notice is one for the submission. A line names the hold holding the merge — its paths, the
// reason the controller set it for, the session that set it and when it ends — so the line a
// first hold posted says nothing true once a second hold is what the merge waits on, and the
// second hold posts its own. What is told once per hold is told once only: a further run under
// the hold already posted about posts nothing at all.
func TestASecondMergeHoldPostsItsOwnLineAndTellsTheArchitectOnce(t *testing.T) {
	github := newQueueGitHub(t, "CLEAN")
	github.set(func(g *armableGitHub) { g.changedFiles = []string{heldFile} })
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedReadyMerge(t, pool, records, "LEGION-208", awaitingREADY)
	now := time.Now().UTC().Truncate(time.Second)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head", Queued: now}, now))
	first := windowHold(now.Add(30*time.Minute), heldPattern)
	setMergeHolds(t, pool, records, first)
	clock := now
	runner := mergeRunner(pool, records, github.url, quietLogger(), func() time.Time { return clock })
	run := func(step string) {
		t.Helper()
		readyMergeDue(t, pool, clock.Add(-time.Second))
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce for %s: %v", step, err)
		}
		clock = clock.Add(time.Minute)
	}

	run("the first run under the first hold")
	run("a second run under the same hold")

	posts := messagePosts(t, pool)
	if len(posts) != 1 {
		t.Fatalf("Dispatch messages %q, want the one line the first hold posted: a run under the hold already told of posts nothing", posts)
	}
	if !strings.Contains(posts[0], first.Reason) {
		t.Fatalf("the first line %q, want it naming the hold that held the merge, %q", posts[0], first.Reason)
	}
	assertMergeRows(t, pool, 1, "merge-held", 1)

	second := record.MergeHold{Paths: []string{"packages/cli/", heldPattern}, Reason: "the migration lands by hand",
		EndsAt: now.Add(2 * time.Hour), Session: "ses_controller_two", SetAt: clock}
	setMergeHolds(t, pool, records, second)
	run("the run the second hold catches")

	posts = messagePosts(t, pool)
	if len(posts) != 2 {
		t.Fatalf("Dispatch messages %q, want a second line: the merge waits on a second hold, and nothing the first line says is true of it", posts)
	}
	for _, named := range []string{"packages/cli/", second.Reason, second.Session, second.EndsAt.UTC().Format(time.RFC3339)} {
		if !strings.Contains(posts[1], named) {
			t.Fatalf("the second line %q, want it naming %q of the hold that now holds the merge", posts[1], named)
		}
	}
	reasons := assertMergeRows(t, pool, 1, "merge-held", 1)
	if !strings.Contains(reasons[0], first.Reason) {
		t.Fatalf("the merge-held notice %q, want the one the submission's first hold wrote: the notice is one per submission", reasons[0])
	}

	run("a second run under the second hold")

	if posts := messagePosts(t, pool); len(posts) != 2 {
		t.Fatalf("Dispatch messages %q, want the two lines the two holds posted: a run under the hold already told of posts nothing", posts)
	}
	assertMergeRows(t, pool, 1, "merge-held", 1)
	if nextAt, _, _, _ := readyMergeState(t, pool); !nextAt.Equal(second.EndsAt) {
		t.Fatalf("the held row runs again at %s, want the second hold's end %s", nextAt, second.EndsAt)
	}
	if held, since, told := heldMarks(t, pool); held != 0 || since.IsZero() || !told {
		t.Fatalf("the row holds %s held since %s told %t, want the one stretch the two holds kept open", held, since, told)
	}
}

// replaceHoldsAsTheRouteDoes replaces the project's whole merge hold list with holds and makes
// every merge a hold is still keeping back due at at, in one transaction and in the order the
// controller's route runs it: the project's merge-hold lock, the replacement, the scan of the
// rows whose payload carries a held moment, and the expedite of each (api's mergeHolds).
//
// The route itself is the api package's and cannot be served from here, so what this drives is
// the record store that route is made of, call for call. That the route takes the lock before it
// writes anything is pinned where the route lives
// (api's TestMergeHoldsWaitsOnTheProjectsHoldLockBeforeItWrites), so neither side of the pair
// rests on this model alone.
//
// It reports a failure rather than ending the test, because a test gives it to the GitHub fake
// to run (armableGitHub.atFiles), where it runs on the server's goroutine and not the test's.
func replaceHoldsAsTheRouteDoes(t *testing.T, pool *pgxpool.Pool, records record.Store, at time.Time, holds ...record.MergeHold) {
	t.Helper()
	ctx := context.Background()
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, pool, func(tx pgx.Tx) error {
		if err := records.SerializeMergeHolds(scoped, tx, "LEGION"); err != nil {
			return err
		}
		if err := records.ReplaceMergeHolds(scoped, tx, "LEGION", holds); err != nil {
			return err
		}
		held, err := records.HeldReadyMerges(scoped, tx, "LEGION", at)
		if err != nil {
			return err
		}
		for _, row := range held {
			if err := records.ExpediteOutbox(scoped, tx, row, at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Errorf("replace the merge holds of LEGION as the controller's route does: %v", err)
		return
	}
	committed()
}

// heldMomentRecorded says whether the one ready_merge row's payload carries the moment a merge
// hold caught it (record.ReadyMerge.HeldSince), which is the mark the controller's replacement
// of the hold list looks for (record.Store.HeldReadyMerges, the mergeHeldPayload condition). A
// test reads it to know the run's hold decision has committed, and reports a failure rather
// than ending the test because it is read from the runner's own goroutine.
func heldMomentRecorded(t *testing.T, pool *pgxpool.Pool) bool {
	var recorded bool
	if err := pool.QueryRow(context.Background(),
		`select coalesce(bool_or(payload ? 'heldSince' and payload->>'heldSince' <> ''), false) from outbox where kind = 'ready_merge'`).
		Scan(&recorded); err != nil {
		t.Errorf("read whether the ready_merge row records a held moment: %v", err)
		return false
	}
	return recorded
}
