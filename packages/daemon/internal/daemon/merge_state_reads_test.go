package daemon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// countingPullStandIn is GitHub's REST API for acme/widgets answering pull request 86 with the
// merge state named, and counting every read of it.
func countingPullStandIn(t *testing.T, state string, reads *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/86" {
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		w.Write([]byte(`{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"` + state + `"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// clearCaptureMergeState puts CAPTURE-1's pull request back to a head GitHub's merge state has
// never been read for, which is where a pull request whose head has just moved stands.
func clearCaptureMergeState(t *testing.T, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "update pull_requests set merge_state = '' where issue = 'CAPTURE-1'"); err != nil {
		t.Fatalf("clear the recorded merge state: %v", err)
	}
}

// approvedCapture seeds CAPTURE-1 in reviewing with its reviewer's approval standing on the head
// its pull request is at and no merge state read yet: an approval waiting on GitHub to report the
// head one the issue may advance on (workflow's reviewRound).
func approvedCapture(t *testing.T, pool *store.Pool) {
	t.Helper()
	seedCapture(t, pool, phase.Reviewing, "needs_review",
		record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim",
			Decision: &record.ReviewDecision{State: "approved", Head: "code"}})
	clearCaptureMergeState(t, pool)
}

// settledChecksOnCode is the CI settlement for the head CAPTURE-1's pull request stands at: one of
// the facts after which the daemon reads GitHub's merge state when an approval stands.
func settledChecksOnCode() intake.PullRequestChecks {
	return intake.PullRequestChecks{Repo: "acme/widgets", Number: 86, HeadSHA: "code", Generation: 1, Snapshot: "settled-again",
		CheckRuns: []record.AttemptRun{{Name: "pr-checks-result", ID: 3}}, Failing: []string{}, Cancelled: []string{}}
}

// start runs the runtime's reader until the test ends, and applies the CI settlement of head code,
// which asks for a read of GitHub's merge state when an approval waits on it (approvedCapture).
func (rig *readRig) start(t *testing.T) {
	t.Helper()
	rig.reading(t)
	applyFact(t, rig.pool, "github", "checks-on-code", settledChecksOnCode(), rig.w.handlers...)
}

// runSecondReadOut runs the runtime's reader until the test ends, applies a first read that found
// head code conflicting with main, made an hour behind the runtime's clock, and makes each of its
// second read's mergeStateLadderReads attempts, a minute apart from a minute after it, on that
// clock: so each answer an attempt applies is older than one a read on the runtime's own clock
// makes, and the workflow drops it as older (workflow's mergeability). It answers when the last
// attempt began, on the runtime's clock.
func (rig *readRig) runSecondReadOut(t *testing.T) time.Time {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
	// The record keeps the times to the microsecond.
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	rig.firstRead(t, first)
	var last time.Time
	for attempt := 1; attempt <= mergeStateLadderReads; attempt++ {
		last = time.Now()
		if ran := runSecondReads(t, rig.runner, first.Add(time.Duration(attempt)*time.Minute)); ran != 1 {
			t.Fatalf("attempt %d ran %d second reads, want one", attempt, ran)
		}
	}
	return last
}

// follows is every line of a read after the ladder, oldest first.
func (rig *readRig) follows(t *testing.T) []map[string]any {
	t.Helper()
	return loggedLines(t, rig.logs.String(), followReadLine)
}

// ladders is how many ladders w marks as running, of one pull request and of a base's batch, and
// how many issues and bases wait for one.
func ladders(w *workflowRuntime) (running, waiting int) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	return len(w.mergeStateRunning) + len(w.baseReads), w.mergeStates.waiting() + w.baseWake.waiting()
}

// endedWithNoRead holds that the reads after the ladder end, logging their end, with nothing
// standing behind them, and that GitHub is read no more after that end however long their
// longest wait is given. The count is taken once the end is logged, so a read that checked the
// record just before the change that ends them, and reached GitHub after it, is not counted as a
// read after the end.
func (rig *readRig) endedWithNoRead(t *testing.T) {
	t.Helper()
	testwait.Eventually(t, "the reads after the ladder to end", func() bool {
		running, _ := ladders(rig.w)
		return running == 0 && len(loggedLines(t, rig.logs.String(), followEndLine)) == 1
	})
	mark := rig.reads.Load()
	time.Sleep(2 * mergeStateFollowWaits * rig.w.mergeStateWait())
	if got := rig.reads.Load(); got > mark {
		t.Fatalf("GitHub was read %d times, want no read after the %d before the reads after the ladder ended", got, mark)
	}
	if running, waiting := ladders(rig.w); running != 0 || waiting != 0 {
		t.Fatalf("%d issues are queued for a read and %d ladders are running, want none standing behind the end", waiting, running)
	}
}

// GitHub computes a pull request's merge state lazily, and it can go on answering `unknown` for a
// head long after it holds it CONFLICTING: INFRA-4391's record read `unknown` for over an hour
// while GitHub answered CONFLICTING and DIRTY (dispatch://INFRA-4374 R4). So a ladder whose fifth
// read still answers `unknown` is followed by more reads of the same head - the base's batch, one
// GraphQL query of the open pull requests on main, which the rig answers as it answers a read of
// pull request 86 - each after twice the wait of the one before up to twenty of the ladder's waits,
// each logged with its wait and what it answered, until GitHub answers a state - which the record
// then holds, a CONFLICTING one going on to its second read and R0's decision - or the head moves,
// or the pull request closes, after which nothing reads it. A request for a read that arrives while
// such a read waits ends the wait and starts a climb at once. The waits are cut only through the
// runtime's own seam (mergeStateRetry), so the waits the lines name are the production ones in a
// smaller unit.
func TestTheMergeStateFollowsGitHubAfterTheLadderEndsOnUnknown(t *testing.T) {
	t.Parallel()
	const retry = readRigWait

	t.Run("unknown five times and then CONFLICTING is recorded and reaches R0's decision", func(t *testing.T) {
		rig := newReadRig(t, approvedCapture, func(read int32) pullAnswer {
			if read <= mergeStateLadderReads {
				return unknownPull
			}
			return conflictingPull
		})
		rig.start(t)
		testwait.Eventually(t, "the read after the ladder to record GitHub's CONFLICTING and log itself", func() bool {
			_, pr := capturePhase(t, rig.pool)
			return pr.ConflictRead != nil && len(rig.follows(t)) >= 1
		})
		follows := rig.follows(t)
		if len(follows) != 1 || follows[0]["attempt"] != float64(1) || follows[0]["wait"] != float64(2*retry) ||
			follows[0]["answered"] != string(record.MergeStateDirty) || follows[0]["head"] != "code" {
			t.Fatalf("the reads after the ladder logged %v, want one, after twice the ladder's wait, answering dirty", follows)
		}
		if got := rig.reads.Load(); got != mergeStateLadderReads+1 {
			t.Fatalf("GitHub was read %d times, want the ladder's %d and one after it", got, mergeStateLadderReads)
		}
		_, pr := capturePhase(t, rig.pool)
		if pr.Mergeability != record.MergeabilityConflicting || pr.MergeState != record.MergeStateDirty ||
			pr.ConflictRead.Head != "code" || pr.ConflictRead.Base != "main" || pr.ConflictingHead != "" {
			t.Fatalf("the record holds %s/%s, first read %+v and %q conflicting; want GitHub's CONFLICTING recorded as the first read of code",
				pr.Mergeability, pr.MergeState, pr.ConflictRead, pr.ConflictingHead)
		}

		// The second read the first one queued runs a minute on, and GitHub still answers
		// CONFLICTING: the conflict is confirmed and R0 decides on the base's rules.
		if ran := runSecondReads(t, rig.runner, pr.ConflictRead.At.Add(time.Minute)); ran != 1 {
			t.Fatalf("%d second reads ran a minute after the first read, want one", ran)
		}
		_, pr = capturePhase(t, rig.pool)
		if pr.ConflictingHead != "code" || pr.ConflictRead != nil {
			t.Fatalf("the record holds %q conflicting with first read %+v, want the conflict of code confirmed", pr.ConflictingHead, pr.ConflictRead)
		}
		if asked := pr.MergeForward; asked == nil || asked.Head != "code" || asked.Why != record.ConflictCheckNotGreen {
			t.Fatalf("the record holds merge forward %+v, want one asked of code for %s: lint is required and green nowhere", asked, record.ConflictCheckNotGreen)
		}
		if asked := loggedLines(t, rig.logs.String(), "workflow: merge forward asked"); len(asked) != 1 {
			t.Fatalf("the workflow logged %v, want one merge forward asked", asked)
		}
		if got := rig.reads.Load(); got != mergeStateLadderReads+2 {
			t.Fatalf("GitHub was read %d times, want the ladder's, the one after it and the second read", got)
		}
	})

	t.Run("the waits double up to twenty of the ladder's, and the reads end when the pull request closes", func(t *testing.T) {
		rig := newReadRig(t, approvedCapture, forever)
		rig.start(t)
		want := []time.Duration{2 * retry, 4 * retry, 8 * retry, 16 * retry, 20 * retry, 20 * retry}
		testwait.Eventually(t, "six reads after the ladder", func() bool { return len(rig.follows(t)) >= len(want) })
		for i, line := range rig.follows(t)[:len(want)] {
			if line["attempt"] != float64(i+1) || line["wait"] != float64(want[i]) || line["answered"] != string(record.MergeStateUnknown) {
				t.Fatalf("read %d after the ladder logged %v, want attempt %d after %s answering unknown", i+1, line, i+1, want[i])
			}
		}
		// GitHub's close, as its webhook records it. A close asks for no read.
		applyFact(t, rig.pool, "github", "closed-86", intake.PullRequestClosed{Repo: "acme/widgets", Number: 86, HeadSHA: "code",
			UpdatedAt: time.Now().UTC()}, rig.w.handlers...)
		if _, pr := capturePhase(t, rig.pool); pr.State != record.PullRequestClosed {
			t.Fatalf("the record holds the pull request %s, want it closed", pr.State)
		}
		rig.endedWithNoRead(t)
	})

	t.Run("the reads end when the record's head moves", func(t *testing.T) {
		rig := newReadRig(t, approvedCapture, forever)
		rig.start(t)
		testwait.Eventually(t, "three reads after the ladder", func() bool { return len(rig.follows(t)) >= 3 })
		// The record takes another head with no fact that asks for a read: the reads after the
		// ladder follow the head they read, and read nothing of another.
		recordCapture(t, rig.pool, func(pr *record.PullRequest) { pr.HeadSHA = "moved" })
		rig.endedWithNoRead(t)
	})

	t.Run("a request while a read after the ladder waits starts a climb at once", func(t *testing.T) {
		// A larger unit, so the wait the request ends - eight of them - is far longer than the
		// test's own step between the read before it and the request.
		const slower = 50 * time.Millisecond
		rig := newReadRig(t, approvedCapture, forever)
		rig.w.mergeStateRetry = slower
		rig.start(t)
		testwait.Eventually(t, "two reads after the ladder", func() bool { return len(rig.follows(t)) >= 2 })
		mark := rig.reads.Load()
		// The head's next CI settlement asks for a read of its own (workflow's askMergeState).
		later := settledChecksOnCode()
		later.Generation, later.Snapshot = 2, "settled-later"
		later.CheckRuns = []record.AttemptRun{{Name: "pr-checks-result", ID: 4}}
		applyFact(t, rig.pool, "github", "checks-on-code-again", later, rig.w.handlers...)
		testwait.Eventually(t, "the next read after the ladder", func() bool { return len(rig.follows(t)) >= 3 })
		// Had the request waited for the read after the ladder, that read would be attempt 3,
		// after eight waits. The climb ran instead, and its own follow starts again from two.
		if next := rig.follows(t)[2]; next["attempt"] != float64(1) || next["wait"] != float64(2*slower) {
			t.Fatalf("the read after the request logged %v, want the first read after a new climb", next)
		}
		if got := rig.reads.Load() - mark; got < mergeStateLadderReads+1 {
			t.Fatalf("GitHub was read %d times after the request, want the climb's %d and the read after it", got, mergeStateLadderReads)
		}
	})
}

// A second read of a conflicting head whose five attempts read no answer - GitHub's UNKNOWN at
// each, or no read that succeeded - releases the first read (dispatch://INFRA-4374 R2), and the
// head is then followed as after a ladder that ended on `unknown` (R4): it is read again after
// twice the ladder's wait, logged as a read after the ladder, and GitHub's CONFLICTING then is
// recorded as a new first conflicting read of the head, which queues a second read of its own.
// That one, a minute on, finds GitHub still CONFLICTING: the conflict is confirmed and R0 decides
// on it. Without the follow nothing would read the head again until its next event, and the
// record would read whatever the last attempt left.
func TestTheMergeStateFollowsGitHubAfterASecondReadEndsOnUnknown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer pullAnswer
	}{
		{name: "GitHub answers unknown at every attempt", answer: unknownPull},
		{name: "no attempt's read succeeds", answer: unavailablePull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The read after the second read is held until the test has read what the last
			// attempt left, so that is the attempt's work alone; readAt is when it arrived.
			held := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(held) }) }
			var readAt atomic.Pointer[time.Time]
			rig := newReadRig(t, captureInTesting, func(read int32) pullAnswer {
				switch {
				case read <= mergeStateLadderReads:
					return tc.answer
				case read == mergeStateLadderReads+1:
					now := time.Now()
					readAt.Store(&now)
					<-held
				}
				return conflictingPull
			})
			t.Cleanup(release)
			lastAttempt := rig.runSecondReadOut(t)
			_, pr := capturePhase(t, rig.pool)
			if _, _, found := secondReadRow(t, rig.pool); found || pr.ConflictRead != nil || pr.ConflictingHead != "" || pr.MergeForward != nil {
				t.Fatalf("after the last attempt the row is queued %t, the first read is %+v, %q is held conflicting and the merge forward is %+v; want the first read released and nothing acted on",
					found, pr.ConflictRead, pr.ConflictingHead, pr.MergeForward)
			}

			testwait.Eventually(t, "a read of the head after the second read", func() bool { return readAt.Load() != nil })
			if waited := readAt.Load().Sub(lastAttempt); waited < 2*rig.w.mergeStateWait() {
				t.Fatalf("GitHub was read again %s after the last attempt began, want at least twice the ladder's wait, %s", waited, 2*rig.w.mergeStateWait())
			}
			release()
			testwait.Eventually(t, "the read after the second read to record GitHub's CONFLICTING and log itself", func() bool {
				_, pr := capturePhase(t, rig.pool)
				return pr.ConflictRead != nil && len(rig.follows(t)) >= 1
			})
			follows := rig.follows(t)
			if len(follows) != 1 || follows[0]["attempt"] != float64(1) || follows[0]["wait"] != float64(2*rig.w.mergeStateWait()) ||
				follows[0]["head"] != "code" || follows[0]["answered"] != string(record.MergeStateDirty) {
				t.Fatalf("the reads after the second read logged %v, want one of head code, after twice the ladder's wait, answering dirty", follows)
			}
			// The first read keeps its time to the nanosecond, and the row its due time to the
			// microsecond.
			_, pr = capturePhase(t, rig.pool)
			due, attempts, found := secondReadRow(t, rig.pool)
			if pr.ConflictRead.Head != "code" || pr.ConflictRead.Base != "main" || pr.ConflictingHead != "" ||
				!found || attempts != 0 || due.Sub(pr.ConflictRead.At.Add(time.Minute)).Abs() >= time.Microsecond {
				t.Fatalf("the record holds first read %+v and %q conflicting, and a second read is queued %t, due %s after %d attempts; want a new first read of code with its second read due a minute on",
					pr.ConflictRead, pr.ConflictingHead, found, due, attempts)
			}

			if ran := runSecondReads(t, rig.runner, pr.ConflictRead.At.Add(time.Minute)); ran != 1 {
				t.Fatalf("%d second reads ran a minute after the new first read, want one", ran)
			}
			_, pr = capturePhase(t, rig.pool)
			if pr.ConflictingHead != "code" || pr.ConflictRead != nil {
				t.Fatalf("the record holds %q conflicting with first read %+v, want the conflict of code confirmed", pr.ConflictingHead, pr.ConflictRead)
			}
			if asked := pr.MergeForward; asked == nil || asked.Head != "code" || asked.Why != record.ConflictCheckNotGreen || asked.Phase != phase.Testing {
				t.Fatalf("the record holds merge forward %+v, want one asked of code in testing for %s: lint is required and green nowhere", asked, record.ConflictCheckNotGreen)
			}
			testwait.Eventually(t, "the follow to end on GitHub's answer", func() bool {
				running, waiting := ladders(rig.w)
				return running == 0 && waiting == 0
			})
			if got := rig.reads.Load(); got != mergeStateLadderReads+2 {
				t.Fatalf("GitHub was read %d times, want the second read's %d attempts, the read after them and the new second read", got, mergeStateLadderReads)
			}
		})
	}
}

// The reads that follow GitHub's merge state are held in memory, so a restart ends them, and the
// daemon's start read of every issue's pull request is what takes them up again (dispatch://
// INFRA-4374 R4): a start read that finds GitHub still computing the head's merge - merge state
// `unknown`, or `mergeable` null - queues a ladder for the issue, which reads GitHub again and
// records the state it answers then. A start read that answers a state queues none. The issue
// stands in testing, where none of the facts these reads apply owes a read
// (classify.MergeStateReadOwed), so a ladder here is the start read's alone; in merging the start
// read is the one read and queues none (TestABasePushOrAStartReadReadsAMergingHeadOnce).
func TestTheMergeStateFollowsGitHubAcrossARestart(t *testing.T) {
	t.Parallel()
	const pull = `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},`
	for _, tc := range []struct {
		name   string
		answer pullAnswer
		ladder bool
	}{
		{name: "unknown queues a ladder", answer: unknownPull, ladder: true},
		{name: "mergeable null queues a ladder", answer: pullAnswer{body: pull + `"mergeable":null}`}, ladder: true},
		{name: "clean queues none", answer: cleanPull},
		{name: "dirty queues none", answer: conflictingPull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReadRig(t, captureInTesting, func(read int32) pullAnswer {
				if read == 1 {
					return tc.answer
				}
				return cleanPull
			})
			rig.w.readIssuesAtStart(context.Background())
			if got := rig.reads.Load(); got != 1 {
				t.Fatalf("the start read read the pull request %d times, want once", got)
			}
			want := 0
			if tc.ladder {
				want = 1
			}
			if _, waiting := ladders(rig.w); waiting != want {
				t.Fatalf("the start read queued %d ladders, want a ladder %t", waiting, tc.ladder)
			}
			if !tc.ladder {
				return
			}
			if _, pr := capturePhase(t, rig.pool); pr.Mergeability != record.MergeabilityUnknown {
				t.Fatalf("after the start read the record reads %s, want GitHub's UNKNOWN", pr.Mergeability)
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go rig.w.readMergeStates(ctx)
			testwait.Eventually(t, "the ladder to read GitHub again and end on its answer", func() bool {
				running, waiting := ladders(rig.w)
				return rig.reads.Load() == 2 && running == 0 && waiting == 0
			})
			if _, pr := capturePhase(t, rig.pool); pr.MergeState != record.MergeStateClean || pr.Mergeability != record.MergeabilityMergeable {
				t.Fatalf("after the ladder the record reads %s/%s, want GitHub's MERGEABLE/clean", pr.Mergeability, pr.MergeState)
			}
		})
	}
}

// The read the ladder makes is the one that moves an approved issue on: GitHub answers the state
// clean, the daemon applies it as the fact a webhook would have carried, and the round the
// approval left waiting ends in accepting. One answer is enough, so the ladder reads once.
func TestTheMergeStateReadMovesAnApprovedIssueToAccepting(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	approvedCapture(t, pool)
	var reads atomic.Int32
	w := readRuntime(pool, countingPullStandIn(t, "clean", &reads).URL, quietLogger())
	w.mergeStateRetry = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.readMergeStates(ctx)

	if _, err := intake.ApplyFact(ctx, pool, "github", "checks-on-code", settledChecksOnCode(), w.handlers...); err != nil {
		t.Fatalf("apply the CI settlement: %v", err)
	}
	testwait.Eventually(t, "the approval reached accepting", func() bool {
		got, _ := capturePhase(t, pool)
		return got == phase.Accepting
	})
	if got := reads.Load(); got != 1 {
		t.Fatalf("GitHub was read %d times, want one: its first answer was a state the issue may advance on", got)
	}
}

// Nothing is read for an issue no merge state would move: the reviewer has decided nothing, so the
// same settlement leaves the daemon owing GitHub no read at all.
func TestNoMergeStateIsReadWithoutAnApprovalStanding(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Reviewing, "needs_review",
		record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim"})
	var reads atomic.Int32
	w := readRuntime(pool, countingPullStandIn(t, "clean", &reads).URL, quietLogger())
	w.mergeStateRetry = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.readMergeStates(ctx)

	if _, err := intake.ApplyFact(ctx, pool, "github", "checks-on-code", settledChecksOnCode(), w.handlers...); err != nil {
		t.Fatalf("apply the CI settlement: %v", err)
	}
	time.Sleep(20 * w.mergeStateRetry)
	if got := reads.Load(); got != 0 {
		t.Fatalf("GitHub was read %d times, want none: no approval stands on the head", got)
	}
}

// A fact of an issue whose ladder is already reading asks for a read of its own, and dropping it
// loses the one read that would answer it: the fact - a check that concluded, a push, a review -
// is what changes GitHub's answer, and the running ladder can already be past its last read, as
// it is here, where its first answer is one it ends on. The running ladder climbs again for the
// request instead, and that climb is the read that moves the issue.
func TestAMergeStateRequestArrivingWhileTheLadderReadsIsNotLost(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	approvedCapture(t, pool)
	var reads atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/86" {
			http.NotFound(w, r)
			return
		}
		state := "clean"
		if reads.Add(1) == 1 {
			// The first read is held until the second request has been queued, so the request
			// lands while this ladder is reading rather than before or after it. It answers
			// `behind`, which ends no round: `blocked` on this head, MERGEABLE with its checks
			// concluded and no review asked for, would end the round on the first read, and the
			// second read this test counts would race the move to accepting.
			state = "behind"
			<-release
		}
		w.Write([]byte(`{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"` + state + `"}`))
	}))
	t.Cleanup(server.Close)
	w := readRuntime(pool, server.URL, quietLogger())
	w.mergeStateRetry = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(release)
	go w.readMergeStates(ctx)

	if _, err := intake.ApplyFact(ctx, pool, "github", "checks-on-code", settledChecksOnCode(), w.handlers...); err != nil {
		t.Fatalf("apply the CI settlement: %v", err)
	}
	testwait.Eventually(t, "the ladder is reading GitHub", func() bool { return reads.Load() == 1 })

	// The next check of the head concludes while that read is in flight, and asks for a read of
	// its own (workflow's askMergeState).
	later := settledChecksOnCode()
	later.Generation, later.Snapshot = 2, "settled-later"
	later.CheckRuns = []record.AttemptRun{{Name: "pr-checks-result", ID: 4}}
	if _, err := intake.ApplyFact(ctx, pool, "github", "checks-on-code-again", later, w.handlers...); err != nil {
		t.Fatalf("apply the second CI settlement: %v", err)
	}
	testwait.Eventually(t, "the request reached the ladder that is running", func() bool {
		w.mergeStateMu.Lock()
		defer w.mergeStateMu.Unlock()
		run := w.mergeStateRunning["CAPTURE-1"]
		return run != nil && run.asked
	})
	release <- struct{}{}

	testwait.Eventually(t, "the approval reached accepting", func() bool {
		got, _ := capturePhase(t, pool)
		return got == phase.Accepting
	})
	if got := reads.Load(); got != 2 {
		t.Fatalf("GitHub was read %d times, want two: the first answer ended one climb, and the request that arrived during it asked for another", got)
	}
	w.mergeStateMu.Lock()
	running := len(w.mergeStateRunning)
	w.mergeStateMu.Unlock()
	if running != 0 {
		t.Fatalf("%d ladders are still marked as running, want none once the climbs are over", running)
	}
}

// captureOnMain seeds CAPTURE-1 in at with status, its pull request recorded on base main and its
// merge state read clean: a push to main finds the pull request by that base (workflow's basePush),
// and one whose base no read has filled matches no push.
func captureOnMain(at phase.Phase, status string) func(*testing.T, *store.Pool) {
	return func(t *testing.T, pool *store.Pool) {
		t.Helper()
		seedCapture(t, pool, at, status)
		recordCapture(t, pool, func(pr *record.PullRequest) { pr.Base = "main" })
	}
}

// basePushOntoMain is a push onto main, the base CAPTURE-1's pull request sits on (captureOnMain).
var basePushOntoMain = intake.BasePush{Repo: "acme/widgets", Branch: "main", After: "main-moved"}

// reading runs the runtime's reader until the test ends.
func (rig *readRig) reading(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
}

// climbedAndFollowed waits for a ladder's climb of CAPTURE-1 and the first read after it: more
// reads than one climb makes, and a read after the ladder logged.
func (rig *readRig) climbedAndFollowed(t *testing.T) {
	t.Helper()
	testwait.Eventually(t, "the climb's reads and a read after it", func() bool {
		return rig.reads.Load() > mergeStateLadderReads && len(rig.follows(t)) >= 1
	})
}

// readOnceMovingNothing holds that GitHub was read once for CAPTURE-1, which stands in merging with
// status retro, and that the answer, GitHub's `unknown`, asked for no read after it and moved
// nothing (readsMovingNothing). why says why one read is all.
func (rig *readRig) readOnceMovingNothing(t *testing.T, why string) {
	t.Helper()
	rig.readsMovingNothing(t, 1, record.MergeabilityUnknown, record.MergeStateUnknown, why)
}

// readsMovingNothing holds that GitHub was read reads times for CAPTURE-1, which stands in merging
// with status retro, and that the last answer, which the record holds as mergeability and state,
// asked for no read after it and moved nothing: the test waits out forty of the runtime's waits
// (readRigWait) after the reads, so a climb's next read, one wait on, would be seen, and the
// record then holds GitHub's answer as every read's answer is applied, with no base push standing
// unanswered, no conflict read and no row in the outbox. why says why that many reads are all.
func (rig *readRig) readsMovingNothing(t *testing.T, reads int32, mergeability record.Mergeability, state record.MergeState, why string) {
	t.Helper()
	testwait.Eventually(t, "the reads of GitHub", func() bool { return rig.reads.Load() >= reads })
	time.Sleep(2 * mergeStateFollowWaits * rig.w.mergeStateWait())
	if got := rig.reads.Load(); got != reads {
		t.Fatalf("GitHub was read %d times, want %d: %s, and GitHub's answer asks for no climb and no follow", got, reads, why)
	}
	if follows := rig.follows(t); len(follows) != 0 {
		t.Fatalf("the reads after the ladder logged %v, want none", follows)
	}
	if running, waiting := ladders(rig.w); running != 0 || waiting != 0 {
		t.Fatalf("%d issues are queued for a read and %d ladders are running, want none after the reads", waiting, running)
	}
	recorded, pr, err := issueRecord(context.Background(), rig.pool, rig.w.records, "CAPTURE-1")
	if err != nil {
		t.Fatal(err)
	}
	if recorded == nil || recorded.Phase != phase.Merging || recorded.Status != "retro" {
		t.Fatalf("after the reads the issue is %+v, want it still in merging with retro", recorded)
	}
	if pr == nil {
		t.Fatal("after the reads CAPTURE-1 records no pull request")
	}
	if pr.Mergeability != mergeability || pr.MergeState != state || pr.BaseMoved != "" {
		t.Fatalf("the record holds %s/%s with the base moved to %q, want GitHub's %s/%s recorded as the read's answer",
			pr.Mergeability, pr.MergeState, pr.BaseMoved, mergeability, state)
	}
	if pr.ConflictRead != nil || pr.ConflictingHead != "" || pr.MergeForward != nil {
		t.Fatalf("the record holds first read %+v, %q conflicting and merge forward %+v, want nothing acted on",
			pr.ConflictRead, pr.ConflictingHead, pr.MergeForward)
	}
	if rows := outboxRowsOf(t, rig.pool, "CAPTURE-1"); rows != 0 {
		t.Fatalf("the outbox holds %d rows of CAPTURE-1, want none: the reads moved nothing", rows)
	}
}

// A push to the base, and the daemon's start read, read the head of an issue that stands in
// merging once (classify.MergeStateReadOnce): its pull request sits in GitHub's merge queue, and
// GitHub answers `unknown` for a queued head, so a ladder there would climb and then follow the
// head on the App's rate limit for an answer GitHub does not give while it is queued. The one
// read's answer is applied as every read's is - the record holds GitHub's `unknown`, against the
// push where a push asked it - and asks for nothing more: no climb, no follow, and the issue moves
// nowhere. The same push or start over an issue in reviewing climbs and follows the head while
// GitHub answers `unknown` (dispatch://INFRA-4374 R4), so the one read is the merging phase's rule,
// not the push's or the start read's.
func TestABasePushOrAStartReadReadsAMergingHeadOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, why string
		trigger   func(t *testing.T, rig *readRig)
	}{
		{name: "a base push", why: "a base push reads a merging head once", trigger: func(t *testing.T, rig *readRig) {
			applyFact(t, rig.pool, "github", "base-push", basePushOntoMain, rig.w.handlers...)
		}},
		{name: "the start read", why: "the start read of a merging head is its one read", trigger: func(_ *testing.T, rig *readRig) {
			rig.w.readIssuesAtStart(context.Background())
		}},
	} {
		t.Run(tc.name+": merging reads once and moves nothing", func(t *testing.T) {
			rig := newReadRig(t, captureOnMain(phase.Merging, "retro"), forever)
			rig.reading(t)
			tc.trigger(t, rig)
			rig.readOnceMovingNothing(t, tc.why)
		})
		t.Run(tc.name+": reviewing climbs and follows", func(t *testing.T) {
			rig := newReadRig(t, captureOnMain(phase.Reviewing, "needs_review"), forever)
			rig.reading(t)
			tc.trigger(t, rig)
			rig.climbedAndFollowed(t)
		})
	}
}

// The one read of an issue that stands in merging ends on whatever GitHub answers, and not only on
// `unknown`: a `blocked` answer missing part of what the round decides it on is an answer the
// readers read again (answerIncomplete, classify.MergeAnswerIncomplete), and a ladder would climb
// on it, but the issue's one read was made (baseOwed.once, ladderAtStart). Two incomplete answers
// are read here. A base push's one read answers `blocked` with `mergeable` null, in the base's
// batch. The start read answers `blocked` and MERGEABLE whose whole answer GitHub fails to give
// (readWholeAnswer): the head's rollup and its count of review requests are then unread, which
// is the one leg of the start read's answer that is not `mergeable` null (readIssuePullRequest), so
// its two reads, the REST read and the whole answer's, are the one read. Each is read once for an
// issue in merging, which moves nothing; the negative control is each of them for an issue in
// reviewing, which climbs and follows on the same answers, so they are answers that are read again:
// the base push's batch with its case of readChunk ending on `answerIncomplete` alone, and the
// start read with the ladder ladderAtStart queues for every issue, would each read the merging
// issue more than that.
func TestABasePushOrAStartReadOfAMergingHeadThatAnswersIncompleteBlockedReadsItOnce(t *testing.T) {
	t.Parallel()
	const head = `"base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"}`
	// null is GitHub's `blocked` for a head whose mergeability it has not computed; mergeable is
	// its `blocked` and MERGEABLE, which the start read's REST read asks the whole answer for.
	null := pullAnswer{body: `{"number":86,"state":"open",` + head + `,"mergeable":null,"mergeable_state":"blocked","requested_reviewers":[],"requested_teams":[]}`}
	mergeable := pullAnswer{body: `{"number":86,"state":"open",` + head + `,"mergeable":true,"mergeable_state":"blocked","requested_reviewers":[],"requested_teams":[]}`}
	for _, tc := range []struct {
		name, why    string
		reads        int32
		mergeability record.Mergeability
		answer       func(read int32) pullAnswer
		trigger      func(t *testing.T, rig *readRig)
	}{
		{name: "a base push", why: "a base push reads a merging head once", reads: 1, mergeability: record.MergeabilityUnknown,
			answer: func(int32) pullAnswer { return null },
			trigger: func(t *testing.T, rig *readRig) {
				applyFact(t, rig.pool, "github", "base-push", basePushOntoMain, rig.w.handlers...)
			}},
		{name: "the start read", why: "the start read of a merging head is its one read, with the whole answer's", reads: 2, mergeability: record.MergeabilityMergeable,
			answer: func(read int32) pullAnswer {
				switch read {
				case 1:
					return mergeable
				case 2:
					return unavailablePull
				}
				return null
			},
			trigger: func(_ *testing.T, rig *readRig) { rig.w.readIssuesAtStart(context.Background()) }},
	} {
		t.Run(tc.name+": merging reads once and moves nothing", func(t *testing.T) {
			rig := newReadRig(t, captureOnMain(phase.Merging, "retro"), tc.answer)
			rig.reading(t)
			tc.trigger(t, rig)
			rig.readsMovingNothing(t, tc.reads, tc.mergeability, record.MergeStateBlocked, tc.why)
		})
		t.Run(tc.name+": reviewing climbs and follows", func(t *testing.T) {
			rig := newReadRig(t, captureOnMain(phase.Reviewing, "needs_review"), tc.answer)
			rig.reading(t)
			tc.trigger(t, rig)
			rig.climbedAndFollowed(t)
		})
	}
}

// A start read that fails - GitHub's 502, or its rate limit, which ends the start read - hands the
// pull request the record holds to the reads of its base (startReadFailed), and what those reads do
// with it depends on the phase the issue stands in. In reviewing they follow the head while GitHub
// answers `unknown` (followOnBase, dispatch://INFRA-4374 R4), as the round that waits on it needs. In
// merging the start read was to be the issue's one read (classify.MergeStateReadOnce): its head sits
// in GitHub's merge queue, where GitHub answers `unknown`, so the base's batch makes that one read
// (readOnceOnBase) and no more, and the issue moves nowhere. The reads here are the failed one and
// the base's one, which answers `unknown`. The negative control is startReadFailed as main had it,
// which hands every issue to followOnBase: the merging issue is then read again on the follow's
// waits, which the reviewing case holds is what a follow does.
func TestAFailedStartReadOfAMergingHeadLeavesItsOneReadToItsBase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		answer  func(read int32) pullAnswer
		prepare func(rig *readRig)
	}{
		{name: "GitHub's 502", answer: func(read int32) pullAnswer {
			if read == 1 {
				return unavailablePull
			}
			return unknownPull
		}, prepare: func(*readRig) {}},
		{name: "GitHub's rate limit", answer: forever, prepare: func(rig *readRig) { rig.refuseREST.Store(true) }},
	} {
		t.Run(tc.name+": merging reads once on its base and moves nothing", func(t *testing.T) {
			rig := newReadRig(t, captureOnMain(phase.Merging, "retro"), tc.answer)
			tc.prepare(rig)
			rig.reading(t)
			rig.w.readIssuesAtStart(context.Background())
			rig.readsMovingNothing(t, 2, record.MergeabilityUnknown, record.MergeStateUnknown,
				"the start read failed, and its base's batch made the one read of a merging head")
		})
		t.Run(tc.name+": reviewing follows on its base", func(t *testing.T) {
			rig := newReadRig(t, captureOnMain(phase.Reviewing, "needs_review"), tc.answer)
			tc.prepare(rig)
			rig.reading(t)
			rig.w.readIssuesAtStart(context.Background())
			testwait.Eventually(t, "the reads after the ladder that follow the head the failed start read handed over", func() bool {
				return len(rig.follows(t)) >= 2
			})
		})
	}
}

// A request for a climb or a follow of an issue outranks a request for one read of it, in its
// base's batch (queueBaseReads, baseOwed.once). Queued together before the reader takes the issue,
// in either order, the climb climbs and the follow follows: each reads the head as the one read
// would, and goes on reading it while GitHub answers `unknown`. A climb asked while the one read
// runs climbs once that read is over. A request for one read that arrives while a climb runs
// starts no climb of its own: the climb's next read, made after the request, answers it, and the
// climb ends on the state GitHub answers there.
func TestAClimbOrAFollowOutranksOneReadOfTheSameIssue(t *testing.T) {
	t.Parallel()
	acme := ghrepo.MustParse("acme/widgets")
	// once asks for one read of CAPTURE-1, as a push onto main asks it for an issue in merging;
	// climb asks for a climb, as the same push asks it for an issue in reviewing; follow hands the
	// batch the head a climb of one pull request ended on `unknown` for.
	once := func(rig *readRig) {
		rig.w.queueBaseMergeStates("acme/widgets", "main", []workflow.BaseRead{{Issue: "CAPTURE-1", Once: true}})
	}
	climb := func(rig *readRig) {
		rig.w.queueBaseMergeStates("acme/widgets", "main", []workflow.BaseRead{{Issue: "CAPTURE-1"}})
	}
	follow := func(rig *readRig) { rig.w.queueBaseRead(acme, "main", map[string]string{"CAPTURE-1": "code"}, false) }
	// placed waits until CAPTURE-1's place in its base's batch holds what holds reads.
	placed := func(t *testing.T, rig *readRig, what string, holds func(baseOwed) bool) {
		t.Helper()
		testwait.Eventually(t, what, func() bool {
			rig.w.mergeStateMu.Lock()
			defer rig.w.mergeStateMu.Unlock()
			read := rig.w.baseReads[rig.w.baseReadOf["CAPTURE-1"]]
			if read == nil {
				return false
			}
			place, owed := read.owed["CAPTURE-1"]
			return owed && holds(place)
		})
	}
	// askedNow is the number of the request CAPTURE-1's place in its batch stands under.
	askedNow := func(rig *readRig) uint64 {
		_, asked := rig.w.heldUnder("CAPTURE-1")
		return asked
	}
	// heldFirst answers GitHub's first read only once released, and every other as answer says.
	// The test registers release as a cleanup after the rig's own, so a test that fails while the
	// read is held releases it before the stand-in of GitHub closes, which waits for that read.
	heldFirst := func(answer func(read int32) pullAnswer) (func(int32) pullAnswer, func()) {
		held := make(chan struct{})
		var released sync.Once
		return func(read int32) pullAnswer {
			if read == 1 {
				<-held
			}
			return answer(read)
		}, func() { released.Do(func() { close(held) }) }
	}

	for _, onceFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("a climb queued beside one read climbs, one read first %t", onceFirst), func(t *testing.T) {
			rig := newReadRig(t, captureOnMain(phase.Merging, "retro"), forever)
			if onceFirst {
				once(rig)
				climb(rig)
			} else {
				climb(rig)
				once(rig)
			}
			rig.reading(t)
			rig.climbedAndFollowed(t)
		})
		t.Run(fmt.Sprintf("a follow queued beside one read follows, one read first %t", onceFirst), func(t *testing.T) {
			rig := newReadRig(t, captureOnMain(phase.Merging, "retro"), forever)
			if onceFirst {
				once(rig)
			}
			follow(rig)
			if !onceFirst {
				once(rig)
			}
			rig.reading(t)
			testwait.Eventually(t, "a read after the ladder", func() bool { return len(rig.follows(t)) >= 1 })
		})
	}

	t.Run("a climb asked while one read runs climbs after it", func(t *testing.T) {
		answer, release := heldFirst(forever)
		rig := newReadRig(t, captureOnMain(phase.Merging, "retro"), answer)
		t.Cleanup(release)
		rig.reading(t)
		once(rig)
		testwait.Eventually(t, "the one read to reach GitHub", func() bool { return rig.reads.Load() == 1 })
		climb(rig)
		placed(t, rig, "the climb asked of the issue its one read is running for", func(place baseOwed) bool {
			return place.climb && !place.once
		})
		release()
		rig.climbedAndFollowed(t)
	})

	t.Run("one read asked while a climb runs is answered by the climb's next read", func(t *testing.T) {
		// The climb's first read answers unknown, its second clean, which ends it; any read after
		// those answers unknown, which a climb would read again.
		answer, release := heldFirst(func(read int32) pullAnswer {
			if read == 2 {
				return cleanPull
			}
			return unknownPull
		})
		rig := newReadRig(t, captureOnMain(phase.Merging, "retro"), answer)
		t.Cleanup(release)
		rig.reading(t)
		climb(rig)
		testwait.Eventually(t, "the climb's first read to reach GitHub", func() bool { return rig.reads.Load() == 1 })
		before := askedNow(rig)
		once(rig)
		placed(t, rig, "the one read asked of the issue its climb is running for", func(place baseOwed) bool {
			return place.climb && !place.once && place.asked > before
		})
		release()
		testwait.Eventually(t, "the climb to end on the state its second read answers", func() bool {
			running, waiting := ladders(rig.w)
			return rig.reads.Load() >= 2 && running == 0 && waiting == 0
		})
		time.Sleep(2 * mergeStateFollowWaits * rig.w.mergeStateWait())
		if got := rig.reads.Load(); got != 2 {
			t.Fatalf("GitHub was read %d times, want two: the climb's, the second made after the one read was asked, which answers it", got)
		}
		if follows := rig.follows(t); len(follows) != 0 {
			t.Fatalf("the reads after the ladder logged %v, want none", follows)
		}
	})
}

// The daemon's reads of a pull request and of its base's rules are conditional
// (githubrest.Client.ETags): each carries the ETag GitHub gave the last answer for the same URL, and
// GitHub's 304, which costs the installation's rate limit nothing, is read as that answer. So a 304
// leaves the record holding what the read before it recorded, and a conflicting head's rules read
// the same rules again.
func TestAConditionalReadAnswered304LeavesTheRecordAsItWas(t *testing.T) {
	t.Parallel()
	const blocked = `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},"mergeable":true,"mergeable_state":"blocked"}`
	var mu sync.Mutex
	asked := map[string][]string{}
	served := map[string][]int{}
	bodies := map[string]string{
		"/repos/acme/widgets/pulls/86":            blocked,
		"/repos/acme/widgets/rules/branches/main": `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"lint"}]}}]`,
		"/repos/acme/widgets/branches/main":       `{"name":"main","protected":false}`,
	}
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		etag := `W/"` + strings.ReplaceAll(r.URL.Path, "/", "-") + `"`
		status := http.StatusOK
		if r.Header.Get("If-None-Match") == etag {
			status = http.StatusNotModified
		}
		mu.Lock()
		asked[r.URL.Path] = append(asked[r.URL.Path], r.Header.Get("If-None-Match"))
		served[r.URL.Path] = append(served[r.URL.Path], status)
		mu.Unlock()
		w.Header().Set("ETag", etag)
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, body)
		}
	}})

	t.Run("a pull request read again unchanged", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		captureInTesting(t, pool)
		w := readRuntime(pool, api, quietLogger())
		state, _, _, err := w.readMergeState(context.Background(), "CAPTURE-1")
		if err != nil || state != record.MergeStateBlocked {
			t.Fatalf("the first read answered %s, %v; want blocked", state, err)
		}
		first := recordedPull(t, pool, "CAPTURE-1")
		state, _, pr, err := w.readMergeState(context.Background(), "CAPTURE-1")
		if err != nil || state != record.MergeStateBlocked || pr == nil || pr.HeadSHA != "code" {
			t.Fatalf("the read answered 304 answered %s of %+v, %v; want the blocked of the answer it kept, at code", state, pr, err)
		}
		mu.Lock()
		path := "/repos/acme/widgets/pulls/86"
		gotAsked, gotServed := slices.Clone(asked[path]), slices.Clone(served[path])
		mu.Unlock()
		if len(gotServed) < 2 || gotServed[len(gotServed)-1] != http.StatusNotModified || gotAsked[len(gotAsked)-1] != `W/"-repos-acme-widgets-pulls-86"` {
			t.Fatalf("GitHub was asked with %q and answered %v, want the second read to carry the first answer's ETag and be answered 304", gotAsked, gotServed)
		}
		after := recordedPull(t, pool, "CAPTURE-1")
		if after.MergeState != first.MergeState || after.Mergeability != first.Mergeability || after.HeadSHA != first.HeadSHA ||
			after.Base != first.Base || after.ConflictRead != nil || after.StateReadAt.Before(first.StateReadAt) {
			t.Fatalf("after the 304 the record holds %s/%s at %s on %q, read %s; want the %s/%s at %s on %q of the read before it, read no earlier than %s",
				after.Mergeability, after.MergeState, after.HeadSHA, after.Base, after.StateReadAt,
				first.Mergeability, first.MergeState, first.HeadSHA, first.Base, first.StateReadAt)
		}
	})

	t.Run("a base's rules read again unchanged", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		w := readRuntime(pool, api, quietLogger())
		acme := ghrepo.MustParse("acme/widgets")
		github := githubrest.Client{Token: "outbox-token", API: githubrest.RepositoryAPI(api, acme), Scope: implementScope(acme)}
		pr := record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-1", Repo: "acme/widgets", Number: 86, Base: "main", HeadSHA: "code"}
		first := w.readBaseRules(context.Background(), github, pr, "main")
		again := w.readBaseRules(context.Background(), github, pr, "main")
		if !slices.Equal(first.Required, []string{"lint"}) || !slices.Equal(again.Required, first.Required) || again.Strict != first.Strict || again.Unread != "" {
			t.Fatalf("the rules read %+v and then %+v, want lint required both times", first, again)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, path := range []string{"/repos/acme/widgets/rules/branches/main", "/repos/acme/widgets/branches/main"} {
			if got := served[path]; len(got) < 2 || got[len(got)-1] != http.StatusNotModified {
				t.Fatalf("GitHub answered %s with %v, want the second read answered 304", path, got)
			}
		}
	})
}
