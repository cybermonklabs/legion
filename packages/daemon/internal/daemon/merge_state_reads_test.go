package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
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

// The lines the reads after a climb log (followBase's reads of the base's batch): one for each
// pull request a read answers, and one for the end that finds the pull request no longer open at
// the head they read.
const (
	followRead = followReadLine
	followEnd  = followEndLine
)

// start runs the runtime's reader until the test ends, and applies the CI settlement of head code,
// which asks for a read of GitHub's merge state when an approval waits on it (approvedCapture).
func (rig *readRig) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
	applyFact(t, rig.pool, "github", "checks-on-code", settledChecksOnCode(), rig.w.handlers...)
}

// follows is every line of a read after the ladder, oldest first.
func (rig *readRig) follows(t *testing.T) []map[string]any {
	t.Helper()
	return loggedLines(t, rig.logs.String(), followRead)
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
		return running == 0 && len(loggedLines(t, rig.logs.String(), followEnd)) == 1
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
	forever := func(int32) pullAnswer { return unknownPull }

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
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go rig.w.readMergeStates(ctx)

			// The row's attempts run on a clock an hour behind the runtime's, so each UNKNOWN they
			// apply is a read older than the one the follow makes on the runtime's own clock: the
			// workflow drops a read older than the one the record holds (workflow's mergeability).
			first := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			rig.firstRead(t, first)
			at := first.Add(time.Minute)
			var lastAttempt time.Time
			for attempt := 1; attempt <= mergeStateLadderReads; attempt++ {
				lastAttempt = time.Now()
				if ran := runSecondReads(t, rig.runner, at); ran != 1 {
					t.Fatalf("attempt %d ran %d second reads, want one", attempt, ran)
				}
				at = at.Add(time.Minute)
			}
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
				return pr.ConflictRead != nil && len(loggedLines(t, rig.logs.String(), followRead)) >= 1
			})
			follows := loggedLines(t, rig.logs.String(), followRead)
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
// (classify.MergeStateReadOwed), so a ladder here is the start read's alone.
func TestTheMergeStateFollowsGitHubAcrossARestart(t *testing.T) {
	t.Parallel()
	const pull = `{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"code"},`
	clean := pullAnswer{body: pull + `"mergeable":true,"mergeable_state":"clean"}`}
	for _, tc := range []struct {
		name   string
		answer pullAnswer
		ladder bool
	}{
		{name: "unknown queues a ladder", answer: unknownPull, ladder: true},
		{name: "mergeable null queues a ladder", answer: pullAnswer{body: pull + `"mergeable":null}`}, ladder: true},
		{name: "clean queues none", answer: clean},
		{name: "dirty queues none", answer: conflictingPull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReadRig(t, captureInTesting, func(read int32) pullAnswer {
				if read == 1 {
					return tc.answer
				}
				return clean
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
			// lands while this ladder is reading rather than before or after it.
			state = "blocked"
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

// batchRequest is the GraphQL query a read of a base's batch sends (openPullRequestsQuery), as the
// stand-ins read it.
type batchRequest struct {
	Query     string `json:"query"`
	Variables struct {
		Owner  string  `json:"owner"`
		Name   string  `json:"name"`
		Base   string  `json:"base"`
		Cursor *string `json:"cursor"`
	} `json:"variables"`
}

// readBatchRequest decodes r as a read of a base's batch of acme/widgets, failing the test (from
// the stand-in's own goroutine, so with Errorf) for any other request to the GraphQL endpoint.
func readBatchRequest(t *testing.T, r *http.Request) batchRequest {
	t.Helper()
	var request batchRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode the GraphQL request: %v", err)
		return request
	}
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer outbox-token" ||
		!strings.Contains(request.Query, "pullRequests(states: OPEN, baseRefName: $base") ||
		request.Variables.Owner != "acme" || request.Variables.Name != "widgets" {
		t.Errorf("GraphQL was sent %s %+v with %q, want the batch query of acme/widgets under the implement App's token",
			r.Method, request.Variables, r.Header.Get("Authorization"))
	}
	return request
}

// batchBase is the base a read of a base's batch asks for (readBatchRequest).
func batchBase(t *testing.T, r *http.Request) string {
	t.Helper()
	return readBatchRequest(t, r).Variables.Base
}

// batchNode is one open pull request as the batch query answers it: its number and head, and
// GitHub's `mergeable` and `mergeStateStatus` in GraphQL's capitals.
func batchNode(number int, head, mergeable, state string) map[string]any {
	return map[string]any{"number": number, "headRefOid": head, "headRefName": fmt.Sprintf("legion/CAPTURE-%d", number-85),
		"mergeable": mergeable, "mergeStateStatus": state}
}

// batchPage is one page of the batch query's answer: nodes, and next, the cursor of the page after
// it, "" for none.
func batchPage(t *testing.T, next string, nodes ...map[string]any) string {
	t.Helper()
	page := map[string]any{"hasNextPage": next != "", "endCursor": nil}
	if next != "" {
		page["endCursor"] = next
	}
	if nodes == nil {
		nodes = []map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{
		"pullRequests": map[string]any{"nodes": nodes, "pageInfo": page}}}})
	if err != nil {
		t.Errorf("encode the batch page: %v", err)
	}
	return string(body)
}

// graphQLAnswerOf is rest, an answer to GET /pulls/86, as the batch query answers the same pull
// request: GitHub's `mergeable` null, true and false are UNKNOWN, MERGEABLE and CONFLICTING, and its
// `mergeStateStatus` is the `mergeable_state` in capitals. An answer that is no 2xx is the query's
// own status and body.
func graphQLAnswerOf(t *testing.T, rest pullAnswer) (int, string) {
	t.Helper()
	if rest.status >= http.StatusMultipleChoices {
		return rest.status, rest.body
	}
	var pull githubPullRequest
	if err := json.Unmarshal([]byte(rest.body), &pull); err != nil {
		t.Errorf("decode the REST answer %s: %v", rest.body, err)
		return http.StatusInternalServerError, `{"message":"the stand-in's answer did not decode"}`
	}
	mergeable := "UNKNOWN"
	if pull.Mergeable != nil {
		mergeable = "CONFLICTING"
		if *pull.Mergeable {
			mergeable = "MERGEABLE"
		}
	}
	node := map[string]any{"number": pull.Number, "headRefOid": pull.Head.SHA, "headRefName": pull.Head.Ref,
		"mergeable": mergeable, "mergeStateStatus": strings.ToUpper(pull.MergeableState)}
	return http.StatusOK, batchPage(t, "", node)
}

// seedOnMain records issue key of CAPTURE in at, with status, and its pull request number open on
// base main at head head-<number>, GitHub's last read finding it MERGEABLE and clean; the issue's
// implementer, and in reviewing a reviewer that has decided nothing.
func seedOnMain(t *testing.T, pool *store.Pool, key string, number int, at phase.Phase, status, rank string) {
	t.Helper()
	records := record.NewStore()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: key, Project: "CAPTURE", Title: "root", Phase: at, Generation: 1, Status: status, Rank: rank}); err != nil {
			return err
		}
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "acme/widgets",
			Number: number, Branch: "legion/" + key, Base: "main", HeadSHA: fmt.Sprintf("head-%d", number), Failing: []string{},
			Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean}); err != nil {
			return err
		}
		rows := []record.PhaseRow{{Issue: key, Role: claim.RoleImplementer, Claim: "implement-" + key}}
		if at == phase.Reviewing {
			rows = append(rows, record.PhaseRow{Issue: key, Role: claim.RoleReviewer, Claim: "review-" + key})
		}
		for _, row := range rows {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
}

// recordedPull is the pull request issue's record holds.
func recordedPull(t *testing.T, pool *store.Pool, issue string) *record.PullRequest {
	t.Helper()
	_, pr, err := issueRecord(context.Background(), pool, record.NewStore(), issue)
	if err != nil || pr == nil {
		t.Fatalf("read %s's pull request: %v, %v", issue, pr, err)
	}
	return pr
}

// pushOntoMain applies another pull request's merge onto main of acme/widgets, as Envoy relays the
// push (intake's decodePush reads it as an intake.BasePush), through w's engine.
func pushOntoMain(t *testing.T, pool *store.Pool, w *workflowRuntime, after string) {
	t.Helper()
	applyFact(t, pool, "github", "push-onto-main-"+after, intake.BasePush{Repo: "acme/widgets", Branch: "main", After: after}, w.handlers...)
}

// A push to a base owes a read of the merge state of every open pull request on it whose issue
// stands past implementing (classify.MergeStateReadOwed), and the daemon reads them all in one
// batch: one GraphQL query a page of the base's open pull requests, however many are owed, where
// it made one GET /pulls/{number} of each before. Each owed pull request's answer is applied as
// that GET's would be - the record holds GitHub's state - and an answer the push owes no read,
// a pull request in implementing or one Legion does not record, is answered and left alone. The
// batch reads its pages until it has every owed pull request, and no further.
func TestABasePushReadsItsOwedPullRequestsInOneQueryAPage(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Reviewing, "needs_review", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	seedOnMain(t, pool, "CAPTURE-3", 88, phase.Testing, "testing", "W")
	seedOnMain(t, pool, "CAPTURE-4", 89, phase.Implementing, "in_progress", "X")
	var queries, pulls atomic.Int32
	var mu sync.Mutex
	var cursors []string
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/pulls/") {
			pulls.Add(1)
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/graphql" {
			githubtest.Unexpected(t, w, r)
			return
		}
		queries.Add(1)
		request := readBatchRequest(t, r)
		cursor := ""
		if request.Variables.Cursor != nil {
			cursor = *request.Variables.Cursor
		}
		mu.Lock()
		cursors = append(cursors, cursor)
		mu.Unlock()
		switch {
		case request.Variables.Base != "main":
			http.Error(w, `{"message":"wrong base"}`, http.StatusBadRequest)
		case cursor == "":
			_, _ = io.WriteString(w, batchPage(t, "page-2",
				batchNode(86, "head-86", "MERGEABLE", "BLOCKED"),
				batchNode(87, "head-87", "MERGEABLE", "BEHIND"),
				batchNode(99, "head-99", "CONFLICTING", "DIRTY")))
		case cursor == "page-2":
			_, _ = io.WriteString(w, batchPage(t, "page-3",
				batchNode(89, "head-89", "CONFLICTING", "DIRTY"),
				batchNode(88, "head-88", "MERGEABLE", "UNSTABLE")))
		default:
			t.Errorf("the batch read asked for page %q, want no page after the one that holds the last owed pull request", cursor)
			_, _ = io.WriteString(w, batchPage(t, ""))
		}
	}})
	logs := &syncBuffer{}
	w := readRuntime(pool, api, slog.New(slog.NewJSONHandler(logs, nil)))
	w.mergeStateRetry = readRigWait
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.readMergeStates(ctx)

	pushOntoMain(t, pool, w, "main-2")
	want := map[string]record.MergeState{"CAPTURE-1": record.MergeStateBlocked, "CAPTURE-2": record.MergeStateBehind, "CAPTURE-3": record.MergeStateUnstable}
	testwait.Eventually(t, "the owed pull requests to hold GitHub's answers and the batch to end", func() bool {
		for issue, state := range want {
			if recordedPull(t, pool, issue).MergeState != state {
				return false
			}
		}
		running, waiting := ladders(w)
		return running == 0 && waiting == 0
	})
	if got := queries.Load(); got != 2 {
		t.Fatalf("the push made %d GraphQL queries, want one for each of the two pages that hold its three owed pull requests", got)
	}
	if got := pulls.Load(); got != 0 {
		t.Fatalf("the push made %d reads of a pull request over REST, want none: the batch answers every owed pull request", got)
	}
	mu.Lock()
	asked := slices.Clone(cursors)
	mu.Unlock()
	if !slices.Equal(asked, []string{"", "page-2"}) {
		t.Fatalf("the batch asked for the pages after cursors %q, want the first page and page-2", asked)
	}
	if pr := recordedPull(t, pool, "CAPTURE-4"); pr.MergeState != record.MergeStateClean || pr.Mergeability != record.MergeabilityMergeable || pr.ConflictRead != nil {
		t.Fatalf("the pull request of the issue in implementing holds %s/%s, first read %+v; want it left as it was: the push owes it no read",
			pr.Mergeability, pr.MergeState, pr.ConflictRead)
	}
	read := loggedMessages(t, logs.String(), "github: read the merge state of the open pull requests on a base")
	if len(read) != 1 || read[0]["owed"] != float64(3) || read[0]["pages"] != float64(2) || read[0]["base"] != "main" {
		t.Fatalf("the batch logged %v, want one read of main's three owed pull requests over two pages", read)
	}
}

// The batch of a base follows GitHub's `unknown` as one pull request's ladder does (dispatch://
// INFRA-4374 R4): it reads the base again, mergeStateLadderWait apart, up to mergeStateLadderReads
// reads in its climb, and then after a wait that doubles, each read one query however many owed
// pull requests GitHub still answers `unknown` for, each owed answer logged with its wait. A pull
// request GitHub answers a state for leaves the batch, and the batch ends once no owed pull
// request on the base reads `unknown`: GitHub is read no more after that.
func TestAnUnknownAnswerReadsTheBaseBatchAgainOnTheLadder(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedOnMain(t, pool, "CAPTURE-1", 86, phase.Testing, "testing", "U")
	seedOnMain(t, pool, "CAPTURE-2", 87, phase.Testing, "testing", "V")
	var queries, pulls atomic.Int32
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			pulls.Add(1)
			http.NotFound(w, r)
			return
		}
		readBatchRequest(t, r)
		// The first read finds both unknown, the second answers 87 and still not 86, and 86 reads
		// unknown to the end of the climb and is answered by the first read after it.
		read := queries.Add(1)
		second := batchNode(87, "head-87", "MERGEABLE", "CLEAN")
		if read == 1 {
			second = batchNode(87, "head-87", "UNKNOWN", "UNKNOWN")
		}
		first := batchNode(86, "head-86", "UNKNOWN", "UNKNOWN")
		if read > mergeStateLadderReads {
			first = batchNode(86, "head-86", "MERGEABLE", "BLOCKED")
		}
		_, _ = io.WriteString(w, batchPage(t, "", first, second))
	}})
	logs := &syncBuffer{}
	w := readRuntime(pool, api, slog.New(slog.NewJSONHandler(logs, nil)))
	w.mergeStateRetry = readRigWait
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.readMergeStates(ctx)

	pushOntoMain(t, pool, w, "main-2")
	testwait.Eventually(t, "the read after the climb to answer 86 and the batch to end", func() bool {
		running, waiting := ladders(w)
		return recordedPull(t, pool, "CAPTURE-1").MergeState == record.MergeStateBlocked && running == 0 && waiting == 0
	})
	if got := queries.Load(); got != mergeStateLadderReads+1 {
		t.Fatalf("GitHub was queried %d times, want the climb's %d and one read after it", got, mergeStateLadderReads)
	}
	if got := pulls.Load(); got != 0 {
		t.Fatalf("GitHub was read %d times over REST, want none: each read of the base is one query", got)
	}
	if pr := recordedPull(t, pool, "CAPTURE-2"); pr.MergeState != record.MergeStateClean || pr.Mergeability != record.MergeabilityMergeable {
		t.Fatalf("87 holds %s/%s, want GitHub's MERGEABLE/clean from the second read", pr.Mergeability, pr.MergeState)
	}
	follows := loggedLines(t, logs.String(), followRead)
	if len(follows) != 1 || follows[0]["issue"] != "CAPTURE-1" || follows[0]["attempt"] != float64(1) ||
		follows[0]["wait"] != float64(2*readRigWait) || follows[0]["answered"] != string(record.MergeStateBlocked) || follows[0]["head"] != "head-86" {
		t.Fatalf("the reads after the climb logged %v, want one for CAPTURE-1, after twice the ladder's wait, answering blocked", follows)
	}
	mark := queries.Load()
	time.Sleep(2 * mergeStateFollowWaits * readRigWait)
	if got := queries.Load(); got != mark {
		t.Fatalf("GitHub was queried %d times after the batch ended, want none", got-mark)
	}
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
		state, _, err := w.readMergeState(context.Background(), "CAPTURE-1")
		if err != nil || state != record.MergeStateBlocked {
			t.Fatalf("the first read answered %s, %v; want blocked", state, err)
		}
		first := recordedPull(t, pool, "CAPTURE-1")
		state, head, err := w.readMergeState(context.Background(), "CAPTURE-1")
		if err != nil || state != record.MergeStateBlocked || head != "code" {
			t.Fatalf("the read answered 304 answered %s at %q, %v; want the blocked of the answer it kept, at code", state, head, err)
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
		github := githubrest.Client{Token: "outbox-token", API: githubrest.RepositoryAPI(api, ghrepo.MustParse("acme/widgets"))}
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
