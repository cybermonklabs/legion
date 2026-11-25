package cireconcile

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows of this file are the floor of the passes after a starved one (Reconciler.starved, and
// gap says why): a pass that its own bound ended before it reached a head sets it, a pass that
// reaches a head clears it, and every other ending leaves it as it was. Every pass runs on the
// harness's clock, which only the test moves: a pass's bound falls where the test moves the clock
// to it, and each wait a pass arms between two reads of GitHub ends where the test moves the clock
// to that wait (passThroughItsWaits), so no row's verdict turns on how fast the machine runs.

// The pass after one that its own bound ended before it reached a head reads at the floor. That
// pass listed the heads and learned no page of any of them, so the work it leaves is one
// repository's identity, and nine tenths of a bound of a second or more spread over one request
// is above the ceiling: set by the arithmetic, the gap would be Pace. Paced so, the next pass
// could reach no head either, and nor could any pass after it. At the floor the pass reads the
// heads, learns their pages, and the pass after it paces itself by those again.
func TestThePassAfterOneThatReadNoHeadWholeReadsAtTheFloor(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	starveFirstPass(t, h)
	h.reconciler.Pace = RequestPace

	// Set by the arithmetic, the gap would be nine tenths of the Interval over the one identity
	// read the starved pass left, which the ceiling clamps to 500ms.
	h.passThroughItsWaits()

	line := h.passLine()
	if got, want := text(t, line, "pace"), FastestRequestPace.String(); got != want {
		t.Errorf("the pass after one that reached no head paced itself at %s, want the floor %s: at the ceiling a pass can reach no head either: %v", got, want, line)
	}
	if number(t, line, "heads") != 2 || line["deadline_reached"] != false {
		t.Fatalf("the pass after the one that reached no head reached %v heads (deadline_reached %v), want both within its bound: %v", line["heads"], line["deadline_reached"], line)
	}

	h.passThroughItsWaits()

	// Two heads over REST are two listings each, and the repository's identity: five requests,
	// and nine tenths of the Interval over five, which the ceiling clamps to 500ms. The floor was
	// the starved pass's successor's alone.
	line = h.passLine()
	if got, want := text(t, line, "pace"), RequestPace.String(); got != want {
		t.Errorf("the pass after one that read every head paced itself at %s, want %s from the work it learned: %v", got, want, line)
	}
}

// starveFirstPass runs one pass that lists both heads and reaches neither: GitHub holds the first
// head's check runs until the request is given up, and the test moves the passes' clock to the
// pass's own bound once that request has reached the stand-in (passCutByItsBound), so the pass
// lists the heads and reaches none of them however fast or slow the machine runs. The pass waits
// for no pace: the row sets the pace it is about only after it. It then lets GitHub answer at
// speed again.
func starveFirstPass(t *testing.T, h *harness) {
	t.Helper()
	h.github.hold(runsOf(head(7)))
	h.passCutByItsBound()
	line := h.passLine()
	if number(t, line, "heads") != 0 || number(t, line, "heads_known") != 2 {
		t.Fatalf("the pass GitHub held at the first head's check runs reached %v heads and knew of %v, want none reached and both listed: nothing here is tested: %v", line["heads"], line["heads_known"], line)
	}
	if line["deadline_reached"] != true {
		t.Fatalf("the pass that listed both heads and reached neither reads deadline_reached %v, want its bound to end it: nothing here is tested: %v", line["deadline_reached"], line)
	}
	h.github.release(runsOf(head(7)))
}

// passThroughItsWaitsToTheHold runs one pass on ctx and moves the clock to each wait it arms
// between two reads of GitHub, as passThroughItsWaits does, until the pass waits on what the test
// holds at the request to path: the stand-in has been asked path more times than before the pass,
// and the clock holds no wait of the pass's but deadlines. What the test holds there is GitHub's
// answer (fakeGitHub.hold), or the write of the limit GitHub answered with to the ledger the test
// holds against writes; between the request and either, the pass arms no wait on the clock.
// deadlines are the moments that are no wait of the pass's - its own bound, and a caller's
// deadline - which the clock is never moved to here. It returns the pass's end, which the caller
// brings by moving the clock to one of the deadlines.
func (h *harness) passThroughItsWaitsToTheHold(ctx context.Context, path string, deadlines ...time.Time) <-chan struct{} {
	h.t.Helper()
	asked := h.github.countPath(path)
	ended := h.startPass(ctx)
	for {
		var waits []time.Time
		testwait.Until(h.t, "the pass to arm a wait beside its deadlines, or to wait on what the test holds at "+path, func() (string, bool) {
			select {
			case <-ended:
				h.t.Fatalf("the pass ended before it reached what the test holds at %s", path)
			default:
			}
			armed := h.clock.Armed()
			var armedWaits []time.Time
			for _, at := range armed {
				if !slices.ContainsFunc(deadlines, at.Equal) {
					armedWaits = append(armedWaits, at)
				}
			}
			sent := h.github.countPath(path) - asked
			if len(armedWaits) == 0 && sent == 0 {
				return fmt.Sprintf("armed %v beside the deadlines %v, and no request to %s since the pass began", armed, deadlines, path), false
			}
			waits = armedWaits
			return fmt.Sprintf("waits %v armed, %d requests to %s since the pass began", armedWaits, sent, path), true
		})
		if len(waits) == 0 {
			return ended
		}
		h.clock.Set(waits[0])
	}
}

// A rate limit that ends the passes after a starved one leaves the floor in place: the limit,
// not the pace, stopped them, and they learned no work either. So the pass after the limit's
// reset reads at the floor too, whether GitHub answered the limit inside a pass or the limit was
// still in force as a pass began. Cleared by the limited pass, the floor would give way to the
// arithmetic, which spreads the one identity read over the bound and lands on the ceiling again.
func TestARateLimitAfterAPassThatReachedNoHeadLeavesTheFloorInPlace(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	starveFirstPass(t, h)
	h.reconciler.Pace = RequestPace

	// GitHub answers the first head's check runs as an exhausted limit, which ends the second
	// pass before it reaches a head.
	h.github.rateLimitNext(runsOf(head(7)))
	h.passThroughItsWaits()
	line := h.passLine()
	if line["rate_limited"] != true || number(t, line, "heads") != 0 {
		t.Fatalf("the second pass reads rate_limited %v and %v heads reached, want the limit to end it before it reached a head: %v",
			line["rate_limited"], line["heads"], line)
	}
	// The limit is still in the ledger every process shares, so the third pass reaches no head
	// either.
	h.passThroughItsWaits()
	if line := h.passLine(); line["rate_limited"] != true || number(t, line, "heads") != 0 {
		t.Fatalf("the third pass reads rate_limited %v and %v heads reached, want the limit still in force to end it: %v",
			line["rate_limited"], line["heads"], line)
	}

	// GitHub's reset passes, which takes the limit out of the ledger.
	scope := githubapp.InstallationScope(h.github.installationID())
	limit, found, err := h.reconciler.Limits.Get(h.ctx, scope)
	if err != nil || !found {
		t.Fatalf("the limit GitHub answered with was recorded under %s: %v, %v", scope, found, err)
	}
	if err := h.reconciler.Limits.Clear(h.ctx, limit); err != nil {
		t.Fatalf("clear the limit as its reset does: %v", err)
	}
	h.passThroughItsWaits()

	line = h.passLine()
	if got, want := text(t, line, "pace"), FastestRequestPace.String(); got != want {
		t.Errorf("the pass after the limit's reset paced itself at %s, want the floor %s: no pass since the one its bound ended has reached a head: %v", got, want, line)
	}
	if number(t, line, "heads") != 2 {
		t.Errorf("the pass after the limit's reset reached %v heads, want both: %v", line["heads"], line)
	}
}

// A deployment that configures a pause below the floor keeps it for the pass after a starved one:
// the floor never makes a pass slower than the pause configured.
func TestThePassAfterOneThatReachedNoHeadKeepsAShorterConfiguredPace(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	starveFirstPass(t, h)
	const configured = 50 * time.Millisecond
	h.reconciler.Pace = configured

	h.passThroughItsWaits()

	line := h.passLine()
	if got, want := text(t, line, "pace"), configured.String(); got != want {
		t.Errorf("the pass after one that reached no head paced itself at %s, want the configured %s, which is shorter than the floor: %v", got, want, line)
	}
	if number(t, line, "heads") != 2 {
		t.Errorf("the pass after one that reached no head reached %v heads, want both: %v", line["heads"], line)
	}
}

// A pass at the floor that reaches a head and then ends on its own bound clears the floor: it
// learned the pages of the head it reached, so the pass after it paces itself by them. Left in
// place, the floor would hold every pass of a deployment whose passes end on their bound -- the
// cursor's own case -- at ten requests a second for good.
func TestAPassThatReachesAHeadAndThenEndsOnItsBoundClearsTheFloor(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	starveFirstPass(t, h)
	h.reconciler.Pace = RequestPace

	// The pass at the floor reads the first head whole, and GitHub holds the second head's check
	// runs until the pass's own bound gives the request up.
	h.github.hold(runsOf(head(8)))
	bound := h.clock.Now().Add(Interval)
	ended := h.passThroughItsWaitsToTheHold(h.ctx, runsOf(head(8)), bound)
	h.clock.Set(bound)
	testwait.Receive(t, "the pass at the floor to end on its bound at the second head's check runs", ended)
	line := h.passLine()
	if got, want := text(t, line, "pace"), FastestRequestPace.String(); got != want {
		t.Fatalf("the pass after the starved one paced itself at %s, want the floor %s: nothing here is tested: %v", got, want, line)
	}
	if number(t, line, "heads") != 1 || line["deadline_reached"] != true {
		t.Fatalf("the pass at the floor reached %v heads (deadline_reached %v), want the first head and its bound to end it in the second: nothing here is tested: %v",
			line["heads"], line["deadline_reached"], line)
	}
	h.github.release(runsOf(head(8)))

	// The work is the first head's two listings and the repository's identity: nine tenths of the
	// Interval over three requests, which the ceiling clamps to 500ms.
	h.passThroughItsWaits()
	line = h.passLine()
	if got, want := text(t, line, "pace"), RequestPace.String(); got != want {
		t.Errorf("the pass after one that reached a head and then ended on its bound paced itself at %s, want %s from the work it learned: reaching a head clears the floor: %v", got, want, line)
	}
}

// readBothHeads runs a pass that reads both heads whole, so the Reconciler holds their pages: two
// listings of each and the repository's identity, five requests, which nine tenths of the Interval
// spread over the ceiling clamps to 500ms.
func readBothHeads(t *testing.T, h *harness) {
	t.Helper()
	h.passThroughItsWaits()
	if line := h.passLine(); number(t, line, "heads") != 2 || line["deadline_reached"] != false {
		t.Fatalf("the first pass reached %v heads (deadline_reached %v), want both within its bound: nothing here is tested: %v",
			line["heads"], line["deadline_reached"], line)
	}
}

// A deadline of the caller's own that ends a pass before it reaches a head is no bound of the
// pass's: the pace did not stop that pass, so the pass after it paces itself by the work the
// Reconciler holds, not at the floor. The caller's deadline is on the passes' clock, a minute
// away, inside the pass's own bound of an Interval, and the test moves the clock to it once the
// pass waits on the first head's check runs, which GitHub holds until the request is given up.
func TestACallersDeadlineLeavesThePaceAsItWas(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	readBothHeads(t, h)

	h.github.hold(runsOf(head(7)))
	started := h.clock.Now()
	callerDeadline := started.Add(time.Minute)
	ctx, cancel := h.clock.WithDeadline(h.ctx, callerDeadline)
	defer cancel()
	ended := h.passThroughItsWaitsToTheHold(ctx, runsOf(head(7)), callerDeadline, started.Add(Interval))
	h.clock.Set(callerDeadline)
	testwait.Receive(t, "the pass to end on its caller's deadline at the first head's check runs", ended)
	if line := h.passLine(); number(t, line, "heads") != 0 || line["deadline_reached"] != true {
		t.Fatalf("the pass the caller's deadline ended reached %v heads (deadline_reached %v), want a deadline to end it before it reached one: nothing here is tested: %v",
			line["heads"], line["deadline_reached"], line)
	}
	h.github.release(runsOf(head(7)))

	h.passThroughItsWaits()

	line := h.passLine()
	if got, want := text(t, line, "pace"), RequestPace.String(); got != want {
		t.Errorf("the pass after one its caller's deadline ended paced itself at %s, want %s from the work the Reconciler holds: that deadline is no bound of the pass's: %v", got, want, line)
	}
}

// A rate limit GitHub answers as the pass's own bound falls, before the pass has reached a head,
// leaves the floor as it was: the limit ended that pass, not its pace. GitHub answers the first
// head's check runs with an exhausted limit, and the client writes that limit to the shared
// ledger on a context of its own, bounded by githubapp.LimitRecordTimeout on the clock the
// stand-in's client bounds that write on (fakeGitHub.recordClock), while this test holds the
// ledger against writes. Once that write waits on the ledger's lock, which the database's own lock
// table shows, the test moves the passes' clock to the pass's bound, so the bound falls during the
// write, after the limit's answer has come, and then moves the write's clock to the write's own
// bound, which ends the write with the limit unwritten. Both moves are the test's, so the write
// waits on the lock however long the test takes to see it there. The pass after it paces itself
// by the work the Reconciler holds.
func TestALimitGitHubAnswersAsTheBoundFallsLeavesThePaceAsItWas(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	readBothHeads(t, h)

	ledger, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatalf("open the transaction that holds the rate-limit ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Rollback(context.Background()) })
	// Exclusive mode refuses every write to the table and no read of it, so the pass still reads
	// the limits in force and only the record of GitHub's answer waits.
	if _, err := ledger.Exec(h.ctx, `lock table github_rate_limits in exclusive mode`); err != nil {
		t.Fatalf("hold the rate-limit ledger against writes: %v", err)
	}
	h.github.rateLimitNext(runsOf(head(7)))
	bound := h.clock.Now().Add(Interval)
	ended := h.passThroughItsWaitsToTheHold(h.ctx, runsOf(head(7)), bound)
	// The stand-in has answered the first head's check runs with the limit, and the pass arms no
	// wait on the passes' clock from there to the write, so the clock stays where it is while the
	// test waits for the write on the lock.
	testwait.Until(t, "the record of GitHub's limit to wait on the ledger's lock", func() (string, bool) {
		select {
		case <-ended:
			t.Fatalf("the pass ended before the record of GitHub's limit waited on the ledger's lock: %v", h.passLine())
		default:
		}
		var waiting int
		if err := h.pool.QueryRow(h.ctx, `
			select count(*) from pg_locks
			where not granted and relation = 'github_rate_limits'::regclass`).Scan(&waiting); err != nil {
			t.Fatalf("read the waits on the rate-limit ledger: %v", err)
		}
		return fmt.Sprintf("%d writes waiting on the lock of github_rate_limits", waiting), waiting > 0
	})
	h.clock.Set(bound)
	records := h.github.recordClock.AwaitArmed(t, "the bound of the record of GitHub's limit", 1)
	if len(records) != 1 {
		t.Fatalf("the clock the record of GitHub's limit is bounded on holds %v, want that record's bound alone", records)
	}
	h.github.recordClock.Set(records[0])
	testwait.Receive(t, "the pass to end once its bound fell during the limit's record, and the record's own bound after it", ended)
	if err := ledger.Rollback(h.ctx); err != nil {
		t.Fatalf("let the rate-limit ledger go: %v", err)
	}
	if line := h.passLine(); line["rate_limited"] != true || number(t, line, "heads") != 0 {
		t.Fatalf("the pass reads rate_limited %v and %v heads reached, want GitHub's limit to end it before it reached a head: nothing here is tested: %v",
			line["rate_limited"], line["heads"], line)
	}
	// The limit's record gave up at its own bound, where the test moved the record's clock, so the
	// ledger holds no limit and the next pass reads GitHub.
	scope := githubapp.InstallationScope(h.github.installationID())
	if _, found, err := h.reconciler.Limits.Get(h.ctx, scope); err != nil || found {
		t.Fatalf("the ledger holds a limit under %s (found %v, %v) once the record of GitHub's limit reached its own bound, want none: nothing here is tested", scope, found, err)
	}

	h.passThroughItsWaits()

	line := h.passLine()
	if got, want := text(t, line, "pace"), RequestPace.String(); got != want {
		t.Errorf("the pass after one a limit ended as its bound fell paced itself at %s, want %s from the work the Reconciler holds: the limit, not the pace, ended that pass: %v", got, want, line)
	}
}

// A bound that ends a pass before it reads GitHub at all -- inside the read of the repositories
// it covers, before the walk that counts a cut (counts.deadline) -- is the pass's own bound ending
// it with no head reached, as a bound inside a head's reads is: the pass after it reads at the
// floor. The bound is already over as the pass begins: that one pass runs on the real clock with
// a bound of a nanosecond, which has passed before the pass's first read, since a bound on the
// harness's clock falls only when the test moves it and so never before a read the pass has begun.
func TestABoundThatEndsAPassBeforeItReadsGitHubSetsTheFloor(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	readBothHeads(t, h)

	// A bound already over as the pass begins: every read it makes fails before anything is
	// sent -- the shared limits', which the pass reads first, and the covered repositories' -- so
	// no request reaches GitHub and the walk counts no cut.
	h.reconciler.Clock, h.reconciler.PassTimeout = clock.Real(), time.Nanosecond
	sentBefore := h.github.count()
	h.pass()
	h.reconciler.Clock, h.reconciler.PassTimeout = h.clock, 0
	if line := h.passLine(); number(t, line, "heads") != 0 || line["deadline_reached"] != false || h.github.count() != sentBefore {
		t.Fatalf("the pass under a spent bound reached %v heads (deadline_reached %v) and sent GitHub %d requests, want none, and no cut counted: nothing here is tested: %v",
			line["heads"], line["deadline_reached"], h.github.count()-sentBefore, line)
	}

	h.passThroughItsWaits()

	line := h.passLine()
	if got, want := text(t, line, "pace"), FastestRequestPace.String(); got != want {
		t.Errorf("the pass after one its bound ended before it read GitHub paced itself at %s, want the floor %s: %v", got, want, line)
	}
}
