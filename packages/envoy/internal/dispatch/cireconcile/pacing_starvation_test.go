package cireconcile

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows of this file are the floor of the passes after a starved one (Reconciler.starved, and
// gap says why): a pass that its own bound ended before it reached a head sets it, a pass that
// reaches a head clears it, and every other ending leaves it as it was.

// The pass after one that its own bound ended before it reached a head reads at the floor. That
// pass listed the heads and learned no page of any of them, so the work it leaves is one
// repository's identity, and nine tenths of a bound of a second or more spread over one request
// is above the ceiling: set by the arithmetic, the gap would be Pace. Paced so, the next pass
// could reach no head either, and nor could any pass after it. At the floor the pass reads the
// heads, learns their pages, and the pass after it paces itself by those again.
func TestThePassAfterOneThatReadNoHeadWholeReadsAtTheFloor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, reviewerAppID)
	clock := h.paceOnTheTestClock()
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	starveFirstPass(t, h)
	h.requireWaits(clock, 100*time.Millisecond)

	// Set by the arithmetic, the gap would be nine tenths of the five-second bound over the one
	// identity read the starved pass left: 4.5 seconds, which the ceiling clamps to 500ms.
	h.pass()
	h.requireWaits(clock, 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond)

	line := h.passLine()
	if got, want := text(t, line, "pace"), FastestRequestPace.String(); got != want {
		t.Errorf("the pass after one that reached no head paced itself at %s, want the floor %s: at the ceiling a pass can reach no head either: %v", got, want, line)
	}
	if number(t, line, "heads") != 2 || line["deadline_reached"] != false {
		t.Fatalf("the pass after the one that reached no head reached %v heads (deadline_reached %v), want both within its bound: %v", line["heads"], line["deadline_reached"], line)
	}

	h.pass()
	h.requireWaits(clock, 500*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond)

	// Two heads over REST are two listings each, and the repository's identity: five requests,
	// and nine tenths of five seconds over five is 900ms, which the ceiling clamps to 500ms. The
	// floor was the starved pass's successor's alone.
	line = h.passLine()
	if got, want := text(t, line, "pace"), RequestPace.String(); got != want {
		t.Errorf("the pass after one that read every head paced itself at %s, want %s from the work it learned: %v", got, want, line)
	}
}

// starveFirstPass ends the first pass on its own bound at its read of the first head, which GitHub
// holds (passCutByItsBound), after the pass listed both heads. Both the pass's bound and its
// pacing gaps run on the test's clock, so no retry or wall time is needed.
func starveFirstPass(t *testing.T, h *harness) {
	t.Helper()
	h.reconciler.PassTimeout = time.Second
	h.github.hold(runsOf(head(7)))
	h.passCutByItsBound()
	line := h.passLine()
	if number(t, line, "heads") != 0 {
		t.Fatalf("a pass reached %v heads while GitHub held the first head's check runs, want none: nothing here is tested: %v", line["heads"], line)
	}
	if number(t, line, "heads_known") != 2 {
		t.Fatalf("the pass listed %v heads, want both before the held read: nothing here is tested: %v", line["heads_known"], line)
	}
	if line["deadline_reached"] != true {
		t.Fatalf("the pass that listed both heads and reached neither reads deadline_reached %v, want its bound to end it: nothing here is tested: %v", line["deadline_reached"], line)
	}
	h.github.release(runsOf(head(7)))
	h.reconciler.PassTimeout = 5 * time.Second
}

// A rate limit that ends the passes after a starved one leaves the floor in place: the limit,
// not the pace, stopped them, and they learned no work either. So the pass after the limit's
// reset reads at the floor too, whether GitHub answered the limit inside a pass or the limit was
// still in force as a pass began. Cleared by the limited pass, the floor would give way to the
// arithmetic, which spreads the one identity read over the bound and lands on the ceiling again.
func TestARateLimitAfterAPassThatReachedNoHeadLeavesTheFloorInPlace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, reviewerAppID)
	clock := h.paceOnTheTestClock()
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	starveFirstPass(t, h)
	h.requireWaits(clock, 100*time.Millisecond)

	// GitHub answers the first head's check runs as an exhausted limit, which ends the second
	// pass before it reaches a head.
	h.github.rateLimitNext(runsOf(head(7)))
	h.pass()
	h.requireWaits(clock, 100*time.Millisecond)
	line := h.passLine()
	if line["rate_limited"] != true || number(t, line, "heads") != 0 {
		t.Fatalf("the second pass reads rate_limited %v and %v heads reached, want the limit to end it before it reached a head: %v",
			line["rate_limited"], line["heads"], line)
	}
	// The limit is still in the ledger every process shares, so the third pass reaches no head
	// either.
	h.pass()
	h.requireWaits(clock)
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
	h.pass()
	h.requireWaits(clock, 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond)

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
	t.Parallel()
	h := newHarness(t, reviewerAppID)
	clock := h.paceOnTheTestClock()
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	const configured = 50 * time.Millisecond
	h.reconciler.Pace = configured
	starveFirstPass(t, h)
	h.requireWaits(clock, 50*time.Millisecond)

	h.pass()
	h.requireWaits(clock, 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond)

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
	t.Parallel()
	h := newHarness(t, reviewerAppID)
	clock := h.paceOnTheTestClock()
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	starveFirstPass(t, h)
	h.requireWaits(clock, 100*time.Millisecond)

	// The pass at the floor reads the first head whole, and its own bound ends it once GitHub
	// holds the second head's check runs.
	h.github.hold(runsOf(head(8)))
	h.passCutByItsBound()
	h.requireWaits(clock, 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond)
	line := h.passLine()
	if got, want := text(t, line, "pace"), FastestRequestPace.String(); got != want {
		t.Fatalf("the pass after the starved one paced itself at %s, want the floor %s: nothing here is tested: %v", got, want, line)
	}
	if number(t, line, "heads") != 1 || line["deadline_reached"] != true {
		t.Fatalf("the pass at the floor reached %v heads (deadline_reached %v), want the first head and its bound to end it in the second: nothing here is tested: %v",
			line["heads"], line["deadline_reached"], line)
	}
	h.github.release(runsOf(head(8)))

	// The work is the first head's two listings and the repository's identity: nine tenths of
	// five seconds over three requests is 1.5 seconds, which the ceiling clamps to 500ms.
	h.pass()
	h.requireWaits(clock, 500*time.Millisecond, 500*time.Millisecond)
	line = h.passLine()
	if got, want := text(t, line, "pace"), RequestPace.String(); got != want {
		t.Errorf("the pass after one that reached a head and then ended on its bound paced itself at %s, want %s from the work it learned: reaching a head clears the floor: %v", got, want, line)
	}
}

// readBothHeads runs a pass under a five-second bound that reads both heads whole, so the
// Reconciler holds their pages: two listings of each and the repository's identity, five
// requests, which nine tenths of the bound spread over is 900ms and the ceiling clamps to 500ms.
func readBothHeads(t *testing.T, h *harness, clock *advancingPaceClock) {
	t.Helper()
	h.reconciler.PassTimeout = 5 * time.Second
	h.pass()
	h.requireWaits(clock, 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond)
	if line := h.passLine(); number(t, line, "heads") != 2 || line["deadline_reached"] != false {
		t.Fatalf("the first pass reached %v heads (deadline_reached %v), want both within its bound: nothing here is tested: %v",
			line["heads"], line["deadline_reached"], line)
	}
}

// A deadline of the caller's own that ends a pass before it reaches a head is no bound of the
// pass's: the pace did not stop that pass, so the pass after it paces itself by the work the
// Reconciler holds, not at the floor.
func TestACallersDeadlineLeavesThePaceAsItWas(t *testing.T) {
	t.Parallel()
	h := newHarness(t, reviewerAppID)
	clock := h.paceOnTheTestClock()
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	readBothHeads(t, h, clock)

	// GitHub holds the first head's check runs until the request is given up, and the caller's
	// own deadline, a second away on a clock of its own, gives it up once the stand-in holds the
	// request, well inside the pass's own bound of five seconds.
	h.github.hold(runsOf(head(7)))
	caller := clocktest.New(clock.Now())
	ctx, cancel := caller.WithTimeout(h.ctx, time.Second)
	ended := h.startPass(ctx)
	testwait.Next(t, "GitHub to hold the pass's read of the first head's check runs", &h.github.held, func(string) bool { return true })
	caller.Advance(time.Second)
	testwait.Receive(t, "the pass to end on its caller's deadline", ended)
	cancel()
	h.requireWaits(clock, 500*time.Millisecond)
	if line := h.passLine(); number(t, line, "heads") != 0 || line["deadline_reached"] != true {
		t.Fatalf("the pass the caller's deadline ended reached %v heads (deadline_reached %v), want a deadline to end it before it reached one: nothing here is tested: %v",
			line["heads"], line["deadline_reached"], line)
	}
	h.github.release(runsOf(head(7)))

	h.pass()
	h.requireWaits(clock, 500*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond)

	line := h.passLine()
	if got, want := text(t, line, "pace"), RequestPace.String(); got != want {
		t.Errorf("the pass after one its caller's deadline ended paced itself at %s, want %s from the work the Reconciler holds: that deadline is no bound of the pass's: %v", got, want, line)
	}
}

// A rate limit answered as the pass's bound falls leaves the pace unchanged. The ledger's write
// fails with DeadlineExceeded after ending the pass's deadline, as a blocked database write does.
func TestALimitGitHubAnswersAsTheBoundFallsLeavesThePaceAsItWas(t *testing.T) {
	t.Parallel()
	h := newHarness(t, reviewerAppID)
	clock := h.paceOnTheTestClock()
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	readBothHeads(t, h, clock)

	h.reconciler.GitHub = h.github.client(&boundRecordLimits{
		Limits: ratelimit.New(h.pool), t: t, recordClock: h.github.recordClock, falls: h.endBound, refuse: context.DeadlineExceeded,
	})
	h.github.rateLimitNext(runsOf(head(7)))
	h.pass()
	h.requireWaits(clock, 500*time.Millisecond)
	if line := h.passLine(); line["rate_limited"] != true || number(t, line, "heads") != 0 {
		t.Fatalf("the pass reads rate_limited %v and %v heads reached, want GitHub's limit to end it before it reached a head: nothing here is tested: %v",
			line["rate_limited"], line["heads"], line)
	}

	// The limit's record failed, so the ledger holds no limit and this pass reads GitHub.
	h.pass()
	h.requireWaits(clock, 500*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond, 500*time.Millisecond)

	line := h.passLine()
	if got, want := text(t, line, "pace"), RequestPace.String(); got != want {
		t.Errorf("the pass after one a limit ended as its bound fell paced itself at %s, want %s from the work the Reconciler holds: the limit, not the pace, ended that pass: %v", got, want, line)
	}
}

// A bound that ends a pass before it reads GitHub at all -- inside the read of the repositories
// it covers, before the walk that counts a cut (counts.deadline) -- is the pass's own bound ending
// it with no head reached, as a bound inside a head's reads is: the pass after it reads at the
// floor.
func TestABoundThatEndsAPassBeforeItReadsGitHubSetsTheFloor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, reviewerAppID)
	clock := h.paceOnTheTestClock()
	h.seedProjectDefault(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	readBothHeads(t, h, clock)

	// A bound already over as the pass begins: every read it makes fails before anything is
	// sent -- the shared limits', which the pass reads first, and the covered repositories' -- so
	// no request reaches GitHub and the walk counts no cut.
	h.reconciler.PassTimeout = time.Nanosecond
	passClock := h.reconciler.Clock
	h.reconciler.Clock = expiredPassClock{Clock: passClock, advance: clock.Advance}
	sentBefore := h.github.count()
	h.pass()
	h.requireWaits(clock)
	if line := h.passLine(); number(t, line, "heads") != 0 || line["deadline_reached"] != false || h.github.count() != sentBefore {
		t.Fatalf("the pass under a spent bound reached %v heads (deadline_reached %v) and sent GitHub %d requests, want none, and no cut counted: nothing here is tested: %v",
			line["heads"], line["deadline_reached"], h.github.count()-sentBefore, line)
	}
	h.reconciler.PassTimeout = 5 * time.Second
	h.reconciler.Clock = passClock

	h.pass()
	h.requireWaits(clock, 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond)

	line := h.passLine()
	if got, want := text(t, line, "pace"), FastestRequestPace.String(); got != want {
		t.Errorf("the pass after one its bound ended before it read GitHub paced itself at %s, want the floor %s: %v", got, want, line)
	}
}
