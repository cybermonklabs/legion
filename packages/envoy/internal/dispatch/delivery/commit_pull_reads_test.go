package delivery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// One claim per pair per CommitPullReadStaleAfter, and the decision and the record are one step:
// the second claim of a pair is refused however soon it asks, and the claim one span later is
// granted. Another commit of the same repository, and the same commit of another, are their own
// pairs. The read a read-only results read makes of a claim (Refuses) answers whether a claim
// would be refused at that moment, and records nothing: no entry for a pair nothing holds, and no
// moment moved for one a claim holds.
func TestACommitPullReadIsClaimedOncePerSpan(t *testing.T) {
	t.Parallel()
	var reads CommitPullReads
	start := time.Date(2026, 11, 5, 7, 15, 8, 0, time.UTC)
	if reads.Refuses("acme/widgets", "abc", start) {
		t.Fatal("the read of a pair nothing holds answered that a claim would be refused, want granted")
	}
	if held := len(reads.notBefore); held != 0 {
		t.Fatalf("the read of a pair nothing holds left %d entries, want none", held)
	}
	if !reads.Claim("acme/widgets", "abc", start) {
		t.Fatal("the first claim of a pair was refused, want it granted")
	}
	inside := start.Add(CommitPullReadStaleAfter - time.Nanosecond)
	if !reads.Refuses("acme/widgets", "abc", inside) {
		t.Fatal("the read of a claimed pair inside its span answered that a claim would be granted, want refused")
	}
	if reads.Refuses("acme/widgets", "def", inside) || reads.Refuses("acme/gadgets", "abc", inside) {
		t.Fatal("the read of another pair answered that a claim would be refused, want each pair bounded on its own")
	}
	if held := len(reads.notBefore); held != 1 {
		t.Fatalf("the reads left %d entries, want the one claim's", held)
	}
	if reads.Claim("acme/widgets", "abc", inside) {
		t.Fatal("a claim inside the span was granted, want it refused")
	}
	if !reads.Claim("acme/widgets", "def", start) || !reads.Claim("acme/gadgets", "abc", start) {
		t.Fatal("a claim of another pair was refused, want each pair bounded on its own")
	}
	if reads.Refuses("acme/widgets", "abc", start.Add(CommitPullReadStaleAfter)) {
		t.Fatal("the read of a claimed pair one span later answered that a claim would be refused, want granted")
	}
	if !reads.Claim("acme/widgets", "abc", start.Add(CommitPullReadStaleAfter)) {
		t.Fatal("the claim one span later was refused, want it granted")
	}
}

// The sweep: a claim granted once the map has passed commitPullReadSweepAt drops every entry no
// claim can be refused by any more, so a process reading results for weeks holds the commits of
// the last CommitPullReadStaleAfter and not every commit it ever asked about. The entries inside
// the span stay, bound and all. A read of a claim sweeps nothing, since it writes nothing.
func TestAClaimSweepsTheEntriesNoBoundCanRefuse(t *testing.T) {
	t.Parallel()
	var reads CommitPullReads
	start := time.Date(2026, 11, 5, 7, 15, 8, 0, time.UTC)
	for number := range commitPullReadSweepAt {
		if !reads.Claim("acme/widgets", fmt.Sprintf("%040x", number), start) {
			t.Fatalf("claim %d of a pair of its own was refused, want it granted", number)
		}
	}
	fresh := start.Add(CommitPullReadStaleAfter)
	if reads.Refuses("acme/widgets", "fresh", fresh) {
		t.Fatal("the read of a pair nothing holds answered that a claim would be refused, want granted")
	}
	if held := len(reads.notBefore); held != commitPullReadSweepAt {
		t.Fatalf("the read of a claim left %d entries, want the %d it found", held, commitPullReadSweepAt)
	}
	if !reads.Claim("acme/widgets", "fresh", fresh) {
		t.Fatal("a claim of a pair nothing is held for was refused, want it granted")
	}
	if held := len(reads.notBefore); held != 1 {
		t.Fatalf("the sweep left %d entries, want the one claim inside the span", held)
	}
	if reads.Claim("acme/widgets", "fresh", fresh.Add(time.Minute)) {
		t.Fatal("the entry the sweep kept refused nothing, want the bound to stand")
	}
}

// A settled pair is refused every claim until its span ends, and the sweep keeps it that long.
// GitHub has named a pull request for that commit whose head is another commit
// (ReadThroughCommitPulls' atHead), so it has answered about this commit for now: a claim inside
// CommitPullSettledFor buys the same answer again, and the sweep, which drops the moments no
// claim can be refused by, must not drop one that refuses every claim yet. Once the span has
// passed the pair is a pair like any other — a pull request opened later can have its head at
// that commit — so the claim is granted and the sweep drops what is left. A read of the claim
// answers as the claim does at each of those moments.
func TestASettledCommitIsRefusedUntilItsSpanEndsAndTheSweepKeepsItUntilThen(t *testing.T) {
	t.Parallel()
	var reads CommitPullReads
	start := time.Date(2026, 11, 5, 7, 15, 8, 0, time.UTC)
	if !reads.Claim("acme/widgets", "abc", start) || !reads.Claim("acme/widgets", "def", start) {
		t.Fatal("the first claim of a pair was refused, want it granted")
	}
	reads.Settle("acme/widgets", "abc", start)
	if !reads.Refuses("acme/widgets", "abc", start.Add(CommitPullSettledFor-time.Nanosecond)) {
		t.Fatal("the read of a settled pair inside its span answered that a claim would be granted, want refused")
	}
	if reads.Claim("acme/widgets", "abc", start.Add(CommitPullSettledFor-time.Nanosecond)) {
		t.Fatal("a claim of a settled pair inside its span was granted, want it refused")
	}
	// Enough pairs to pass commitPullReadSweepAt, then one claim to run the sweep over them: the
	// claims' own moments have passed and go, and the settled pair's stands and stays.
	for number := range commitPullReadSweepAt {
		if !reads.Claim("acme/widgets", fmt.Sprintf("%040x", number), start) {
			t.Fatalf("claim %d of a pair of its own was refused, want it granted", number)
		}
	}
	if !reads.Claim("acme/widgets", "fresh", start.Add(CommitPullReadStaleAfter)) {
		t.Fatal("a claim of a pair nothing is held for was refused, want it granted")
	}
	if reads.Claim("acme/widgets", "abc", start.Add(CommitPullReadStaleAfter)) {
		t.Fatal("the swept pair was claimed again, want the settling kept: GitHub has answered about it")
	}
	// A pair nothing settled is bounded by the claim's span alone, so the settling refuses its
	// own pair and no other.
	if !reads.Claim("acme/widgets", "def", start.Add(CommitPullReadStaleAfter)) {
		t.Fatal("a claim of an unsettled pair one span later was refused, want it granted")
	}
	// Past the settled span the pair is read through again, and the sweep drops what the
	// settling left: nothing here is held for the life of the process.
	ended := start.Add(CommitPullSettledFor)
	if reads.Refuses("acme/widgets", "abc", ended) {
		t.Fatal("the read of the settled pair once its span had passed answered that a claim would be refused, want granted")
	}
	if !reads.Claim("acme/widgets", "abc", ended) {
		t.Fatal("a claim of the settled pair once its span had passed was refused, want it granted")
	}
	reads.Settle("acme/widgets", "ghi", start)
	for number := range commitPullReadSweepAt {
		if !reads.Claim("acme/widgets", fmt.Sprintf("%040x", number), ended) {
			t.Fatalf("claim %d past the settled span was refused, want it granted", number)
		}
	}
	if _, held := reads.notBefore[commitPullKey{repo: "acme/widgets", sha: "ghi"}]; held {
		t.Fatal("the sweep kept a settled pair whose span had passed, want it dropped with the rest")
	}
}

// Two processes on one database read one commit through once a span between them. Each keeps the
// moment in its memory, and the claim in the database (commit_pull_reads) holds it for both: the
// second process's claim inside the span is refused, so it reads nothing, and its claim at the
// span's end is granted, since it keeps the moment the database holds and not a span past its own
// refused claim. A settle in one process holds the commit off in the other for
// CommitPullSettledFor. Two processes that claim one commit at one moment are granted once. A
// claim that each process keeps in its memory alone grants the second process inside the span; a
// settle kept in one process alone grants the other process after the short span; a process that
// keeps its own refused claim is refused at the span's end.
func TestTwoProcessesReadACommitThroughOnceASpanBetweenThem(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	second := storetest.SecondProcess(t, &store.Store{Pool: pool}).Pool
	start := time.Date(2026, 11, 19, 17, 40, 0, 0, time.UTC)
	claim := func(t *testing.T, reads *CommitPullReads, on *store.Pool, sha string, at time.Time) bool {
		t.Helper()
		granted, err := reads.ClaimAcrossProcesses(ctx, on, "acme/widgets", sha, at)
		if err != nil {
			t.Fatalf("claim %s at %s: %v", sha, at, err)
		}
		return granted
	}

	t.Run("one_claim_a_span", func(t *testing.T) {
		var first, other CommitPullReads
		if !claim(t, &first, pool, "abc", start) {
			t.Fatal("the first process's first claim was refused, want it granted")
		}
		if claim(t, &other, second, "abc", start.Add(time.Minute)) {
			t.Fatal("the second process's claim one minute into the span was granted, want it refused: the first process read the commit through")
		}
		if !claim(t, &other, second, "abc", start.Add(CommitPullReadStaleAfter)) {
			t.Fatal("the second process's claim at the span's end was refused, want it granted: it keeps the moment the database holds")
		}
		if claim(t, &first, pool, "abc", start.Add(CommitPullReadStaleAfter+time.Second)) {
			t.Fatal("the first process's claim just after the second process read the commit through was granted, want it refused")
		}
	})
	t.Run("a_settle_holds_every_process", func(t *testing.T) {
		var first, other CommitPullReads
		if !claim(t, &first, pool, "def", start) {
			t.Fatal("the first process's first claim was refused, want it granted")
		}
		if err := first.SettleAcrossProcesses(ctx, pool, "acme/widgets", "def", start); err != nil {
			t.Fatal(err)
		}
		if claim(t, &other, second, "def", start.Add(CommitPullSettledFor-time.Second)) {
			t.Fatal("the second process's claim inside the settled span was granted, want it refused: GitHub put the pull request's head elsewhere")
		}
		if !claim(t, &other, second, "def", start.Add(CommitPullSettledFor)) {
			t.Fatal("the second process's claim at the settled span's end was refused, want it granted")
		}
	})
	t.Run("two_claims_at_once", func(t *testing.T) {
		var first, other CommitPullReads
		for number := range 40 {
			sha := fmt.Sprintf("%040x", 1000+number)
			var wait sync.WaitGroup
			var granted [2]bool
			var failed [2]error
			for i, process := range []struct {
				reads *CommitPullReads
				pool  *store.Pool
			}{{&first, pool}, {&other, second}} {
				wait.Go(func() {
					granted[i], failed[i] = process.reads.ClaimAcrossProcesses(ctx, process.pool, "acme/widgets", sha, start)
				})
			}
			wait.Wait()
			if failed[0] != nil || failed[1] != nil {
				t.Fatalf("two processes claimed %s at one moment: %v", sha, failed)
			}
			if granted[0] == granted[1] {
				t.Fatalf("two processes claimed %s at one moment and were granted %v, want exactly one granted", sha, granted)
			}
		}
	})
}

// A claim in the database deletes each other row whose moment has passed, so the table holds
// about one row for each commit asked about in the last CommitPullSettledFor and not every commit
// ever asked about. A row whose moment has not passed stays, and so does the row of the claim
// itself.
func TestAClaimInTheDatabaseDeletesTheRowsNoMomentHolds(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	start := time.Date(2026, 11, 19, 17, 40, 0, 0, time.UTC)
	var reads CommitPullReads
	for _, sha := range []string{"old", "settled", "fresh"} {
		if granted, err := reads.ClaimAcrossProcesses(ctx, pool, "acme/widgets", sha, start); err != nil || !granted {
			t.Fatalf("claim %s: granted %v, error %v; want it granted", sha, granted, err)
		}
	}
	if err := reads.SettleAcrossProcesses(ctx, pool, "acme/widgets", "settled", start); err != nil {
		t.Fatal(err)
	}
	later := start.Add(CommitPullReadStaleAfter + time.Minute)
	if granted, err := reads.ClaimAcrossProcesses(ctx, pool, "acme/widgets", "fresh", later); err != nil || !granted {
		t.Fatalf("claim fresh one span later: granted %v, error %v; want it granted", granted, err)
	}
	rows, err := pool.Query(ctx, `select sha from commit_pull_reads order by sha`)
	if err != nil {
		t.Fatal(err)
	}
	var held []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			t.Fatal(err)
		}
		held = append(held, sha)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(held) != "[fresh settled]" {
		t.Fatalf("the table holds %v after a claim one span later, want [fresh settled]: the row of old, whose moment passed, deleted", held)
	}
}

// Two claims of two commits whose moments have both passed, made at once by two reads (of one
// process or of two), are both granted. Each claim writes its own row and deletes the rows whose
// moments have passed, the other's among them; a claim that held its own row while it waited on
// the other's would deadlock with the other, and the read whose claim lost would ask GitHub
// nothing. "at_a_barrier" makes the two claims 200 times; "rows_held" holds each claim's row for
// 200 ms once the claim has locked it and before it writes it (a trigger that sleeps), so the two
// claims overlap for certain. A claim the database did not take holds nothing in the process's
// memory, so the next read's claim is granted and that read asks GitHub.
func TestTwoClaimsOfTwoPassedCommitsMadeAtOnceAreBothGranted(t *testing.T) {
	start := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		rounds int
		hold   bool
	}{{"at_a_barrier", 200, false}, {"rows_held", 3, true}} {
		t.Run(c.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			if c.hold {
				if _, err := pool.Exec(ctx, `
					create function hold_commit_pull_read() returns trigger language plpgsql as
						$$ begin perform pg_sleep(0.2); return new; end $$;
					create trigger hold_commit_pull_read before update on commit_pull_reads
						for each row execute function hold_commit_pull_read();`); err != nil {
					t.Fatalf("SETUP: the trigger that holds a claim's row: %v", err)
				}
			}
			granted, failed := 0, 0
			var failures []string
			for round := range c.rounds {
				now := start.Add(time.Duration(round) * time.Second)
				if _, err := pool.Exec(ctx, `delete from commit_pull_reads`); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `insert into commit_pull_reads (repo, sha, not_before) select 'acme/widgets', sha, $1 from unnest(array['x', 'y']) sha`,
					now.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
				begin := make(chan struct{})
				var wait sync.WaitGroup
				var mu sync.Mutex
				for _, sha := range []string{"x", "y"} {
					wait.Go(func() {
						var reads CommitPullReads
						<-begin
						ok, err := reads.ClaimAcrossProcesses(ctx, pool, "acme/widgets", sha, now)
						mu.Lock()
						defer mu.Unlock()
						if err != nil {
							failed++
							failures = append(failures, fmt.Sprintf("round %d, %s: %v", round, sha, err))
						}
						if ok {
							granted++
						}
					})
				}
				close(begin)
				wait.Wait()
			}
			t.Logf("%s: %d rounds, %d of %d claims granted, %d failed", c.name, c.rounds, granted, 2*c.rounds, failed)
			if failed != 0 || granted != 2*c.rounds {
				t.Errorf("%d of %d claims of two passed commits made at once were granted and %d failed, want every one granted: %s",
					granted, 2*c.rounds, failed, strings.Join(failures[:min(len(failures), 3)], "; "))
			}
		})
	}
	t.Run("a_lost_claim_is_asked_again", func(t *testing.T) {
		pool, ctx := deliveryTestPool(t)
		var reads CommitPullReads
		gone, cancel := context.WithCancel(ctx)
		cancel()
		if granted, err := reads.ClaimAcrossProcesses(gone, pool, "acme/widgets", "abc", start); err == nil || granted {
			t.Fatalf("a claim whose statement could not run: granted %v, error %v; want an error and no grant", granted, err)
		}
		if granted, err := reads.ClaimAcrossProcesses(ctx, pool, "acme/widgets", "abc", start); err != nil || !granted {
			t.Fatalf("the next read's claim at the same moment: granted %v, error %v; want it granted, since the lost claim holds nothing", granted, err)
		}
	})
}

// A settle and a release write the claim every process shares on a context their caller's end
// cannot cancel: a read-through that GitHub answered keeps what it learned though its request was
// cancelled after the answer. Here the caller's context is cancelled before either is called. The
// settle records CommitPullSettledFor past now in the table, and the release gives back the claim
// a read took. And a release gives back only the claim it took: a later moment, which a settle or
// another process's later claim recorded, stays in the table and in the process's memory.
func TestASettleAndAReleaseWriteOnAContextTheirCallerCannotCancel(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	start := time.Date(2026, 11, 20, 4, 0, 0, 0, time.UTC)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	moment := func(t *testing.T, sha string) *time.Time {
		t.Helper()
		var notBefore *time.Time
		if err := pool.QueryRow(ctx, `select max(not_before) from commit_pull_reads where repo = 'acme/widgets' and sha = $1`, sha).Scan(&notBefore); err != nil {
			t.Fatal(err)
		}
		return notBefore
	}

	t.Run("the_settle_outlives_its_caller", func(t *testing.T) {
		var reads CommitPullReads
		if granted, err := reads.ClaimAcrossProcesses(ctx, pool, "acme/widgets", "settled", start); err != nil || !granted {
			t.Fatalf("claim: granted %v, error %v", granted, err)
		}
		if err := reads.SettleAcrossProcesses(gone, pool, "acme/widgets", "settled", start); err != nil {
			t.Fatalf("a settle whose caller hung up: %v, want it written", err)
		}
		if got, want := moment(t, "settled"), start.Add(CommitPullSettledFor); got == nil || !got.Equal(want) {
			t.Errorf("after a settle whose caller hung up, the table holds %v, want %v", got, want)
		}
	})
	t.Run("the_release_outlives_its_caller", func(t *testing.T) {
		var reads CommitPullReads
		if granted, err := reads.ClaimAcrossProcesses(ctx, pool, "acme/widgets", "released", start); err != nil || !granted {
			t.Fatalf("claim: granted %v, error %v", granted, err)
		}
		if err := reads.ReleaseAcrossProcesses(gone, pool, "acme/widgets", "released", start); err != nil {
			t.Fatalf("a release whose caller hung up: %v, want it written", err)
		}
		if got := moment(t, "released"); got != nil {
			t.Errorf("after a release whose caller hung up, the table holds %v, want no claim", got)
		}
	})
	t.Run("a_release_keeps_a_later_moment", func(t *testing.T) {
		var reads CommitPullReads
		if granted, err := reads.ClaimAcrossProcesses(ctx, pool, "acme/widgets", "later", start); err != nil || !granted {
			t.Fatalf("claim: granted %v, error %v", granted, err)
		}
		if err := reads.SettleAcrossProcesses(ctx, pool, "acme/widgets", "later", start); err != nil {
			t.Fatal(err)
		}
		if err := reads.ReleaseAcrossProcesses(ctx, pool, "acme/widgets", "later", start); err != nil {
			t.Fatal(err)
		}
		if got, want := moment(t, "later"), start.Add(CommitPullSettledFor); got == nil || !got.Equal(want) {
			t.Errorf("after a release of the claim at %v, the table holds %v, want the settled moment %v kept", start, got, want)
		}
		if reads.Claim("acme/widgets", "later", start.Add(CommitPullReadStaleAfter)) {
			t.Error("after the release, the process's memory granted a claim one short span later, want it refused: the settled moment is later than the claim it gave back")
		}
	})
}

// Two sweeps at once over the same passed rows wait on no row. Sweep A runs the sweep's statement
// in a transaction that stays open, so it holds the two passed rows it deleted. A granted claim of
// a third pair then runs its own sweep (ClaimAcrossProcesses, sweepCommitPullReadsSQL) while A
// holds them. The sweep skips each row another statement holds, so the claim answers with no
// backend of the database waiting on a lock, and A's rows are A's to delete. A sweep that locked
// the rows it reads without skipping them would wait on A's transaction for as long as A lasted,
// and every granted claim behind it with it. The claim runs on its own goroutine; the test reads
// pg_stat_activity until the claim answers or a backend waits on a lock, so no time decides it.
func TestTwoSweepsAtOnceOverOnePassedRowWaitOnNoRow(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	now := time.Now()
	passed := now.Add(-time.Hour)
	for _, sha := range []string{"e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1", "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"} {
		if _, err := pool.Exec(ctx, `insert into commit_pull_reads (repo, sha, not_before) values ('acme/widgets', $1, $2)`, sha, passed); err != nil {
			t.Fatalf("store the passed claim of %s: %v", sha, err)
		}
	}
	sweepA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sweepA.Rollback(context.Background()) }()
	deleted, err := sweepA.Exec(ctx, sweepCommitPullReadsSQL, now)
	if err != nil {
		t.Fatalf("sweep A: %v", err)
	}
	if deleted.RowsAffected() != 2 {
		t.Fatalf("sweep A deleted %d rows, want the 2 passed ones", deleted.RowsAffected())
	}

	var reads CommitPullReads
	type claimed struct {
		granted bool
		err     error
	}
	answer := make(chan claimed, 1)
	claimCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	go func() {
		granted, err := reads.ClaimAcrossProcesses(claimCtx, pool, "acme/widgets", "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3", now)
		answer <- claimed{granted, err}
	}()
	waitsOnALock := func() int {
		var count int
		if err := pool.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	var got *claimed
	for got == nil {
		select {
		case answer := <-answer:
			got = &answer
		case <-time.After(20 * time.Millisecond):
			if waiting := waitsOnALock(); waiting > 0 {
				t.Fatalf("the claim's sweep waits on a lock while sweep A holds the passed rows (%d backends waiting): it locks rows another sweep holds rather than skipping them", waiting)
			}
		}
	}
	if got.err != nil || !got.granted {
		t.Fatalf("the claim of a third pair while sweep A held the passed rows = %v, %v; want granted", got.granted, got.err)
	}
	if err := sweepA.Commit(ctx); err != nil {
		t.Fatalf("sweep A's commit: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `select count(*) from commit_pull_reads where not_before < $1`, now).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d passed rows are left after both sweeps, want none", left)
	}
}
