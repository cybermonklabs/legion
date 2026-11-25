package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

// The lines the reads after a ladder log (followMergeState): one for each read, and one for the
// end that finds the pull request no longer open at the head they read.
const (
	followRead = "github: a merge state read after the ladder"
	followEnd  = "github: the merge state reads after the ladder end"
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

// ladders is how many ladders w marks as running, and how many issues wait for one.
func ladders(w *workflowRuntime) (running, waiting int) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	return len(w.mergeStateRunning), w.mergeStates.waiting()
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
// read still answers `unknown` is followed by more reads of the same head, each after twice the
// wait of the one before up to twenty of the ladder's waits, each logged with its wait and what it
// answered, until GitHub answers a state - which the record then holds, a CONFLICTING one going on
// to its second read and R0's decision - or the head moves, or the pull request closes, after
// which nothing reads it. A request for a read that arrives while such a read waits ends the wait
// and starts a climb at once. The waits are cut only through the runtime's own seam
// (mergeStateRetry), so the waits the lines name are the production ones in a smaller unit.
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

// climbFailed starts the line a climb logs for a read that failed (climbMergeState).
const climbFailed = "read GitHub's merge state for an issue whose read was asked for"

// limitedPull is GitHub's rate limit answering a read of pull request 86: a 429 with no
// Retry-After, which asks the caller to wait a minute (githubrest.RetryAfter).
var limitedPull = pullAnswer{status: http.StatusTooManyRequests, body: `{"message":"API rate limit exceeded"}`}

// owesCapture says whether w's reader still holds a ladder for CAPTURE-1 (mergeStateRunning), read
// under the lock the reader marks and unmarks it under.
func owesCapture(w *workflowRuntime) bool {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	_, running := w.mergeStateRunning["CAPTURE-1"]
	return running
}

// mergeForwardAsks is every ask of a merge forward the outbox holds for CAPTURE-1, oldest first:
// each row of kind merge_forward whose step is an ask, decoded as its executor decodes it.
func mergeForwardAsks(t *testing.T, pool *store.Pool) []record.MergeForwardMessage {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, payload from outbox where issue = 'CAPTURE-1' and kind = $1 order by id`,
		string(record.OutboxKindMergeForward))
	if err != nil {
		t.Fatalf("read the merge-forward rows: %v", err)
	}
	defer rows.Close()
	var asks []record.MergeForwardMessage
	for rows.Next() {
		var id int64
		var payload []byte
		if err := rows.Scan(&id, &payload); err != nil {
			t.Fatalf("read a merge-forward row: %v", err)
		}
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: id, Kind: record.OutboxKindMergeForward, Issue: "CAPTURE-1",
			Payload: json.RawMessage(payload), NextAt: time.Now()})
		if err != nil {
			t.Fatalf("decode merge-forward row %d: %v", id, err)
		}
		if message, ok := decoded.(record.MergeForwardMessage); ok && message.Step == record.MergeForwardAsk {
			asks = append(asks, message)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the merge-forward rows: %v", err)
	}
	return asks
}

// failingIssueRead is records whose next read of an issue fails while fail holds, once: the
// database refusing the read of the record the merge-state reader makes before it reads GitHub
// (readMergeState's issueRecord).
type failingIssueRead struct {
	record.Store
	fail atomic.Bool
}

func (r *failingIssueRead) Issue(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	if r.fail.CompareAndSwap(true, false) {
		return nil, errors.New("the database refused the read")
	}
	return r.Store.Issue(ctx, tx, key)
}

// keptAfterTheFailure holds that the climb logged its first read as failed, and that the reader
// still holds the read CAPTURE-1 is owed once it reads GitHub again, its read numbered next, or
// lets the issue go: the first wait after the failure is the ladder's, and the failure is the
// climb's first read still.
func (rig *readRig) keptAfterTheFailure(t *testing.T, next int32) {
	t.Helper()
	testwait.Eventually(t, "the failed read to be logged", func() bool { return len(rig.lines(t, climbFailed)) >= 1 })
	testwait.Eventually(t, "the reader to read GitHub again or let CAPTURE-1 go", func() bool {
		return rig.reads.Load() >= next || !owesCapture(rig.w)
	})
	if !owesCapture(rig.w) {
		t.Fatalf("after the failed read no reader holds CAPTURE-1's read, and GitHub was read %d times; want the read kept and made again",
			rig.reads.Load())
	}
	if failed := rig.lines(t, climbFailed)[0]; failed["read"] != float64(1) || failed["wait"] != float64(rig.w.mergeStateWait()) {
		t.Fatalf("the climb logged %v, want its first read failed and read again after the ladder's wait, %s", failed, rig.w.mergeStateWait())
	}
}

// confirmedAndAskedOnce holds that GitHub's CONFLICTING answer after the failure took the road of
// every CONFLICTING answer: a first conflicting read of head code that asks nothing, with the
// reader done; the second read a minute on, which confirms the conflict; and then the one ask of a
// merge forward the confirmed conflict makes, to the implementer at code. GitHub was read reads
// times in all.
func (rig *readRig) confirmedAndAskedOnce(t *testing.T, reads int32) {
	t.Helper()
	testwait.Eventually(t, "GitHub's CONFLICTING to be recorded and the reader to end", func() bool {
		_, pr := capturePhase(t, rig.pool)
		running, waiting := ladders(rig.w)
		return pr.ConflictRead != nil && running == 0 && waiting == 0
	})
	if asks := mergeForwardAsks(t, rig.pool); len(asks) != 0 {
		t.Fatalf("the first CONFLICTING read left the merge-forward asks %+v, want none before a second read confirms it", asks)
	}
	_, pr := capturePhase(t, rig.pool)
	if pr.ConflictRead.Head != "code" || pr.ConflictingHead != "" {
		t.Fatalf("the record holds first read %+v and %q conflicting, want a first read of code and nothing confirmed", pr.ConflictRead, pr.ConflictingHead)
	}
	if ran := runSecondReads(t, rig.runner, pr.ConflictRead.At.Add(time.Minute)); ran != 1 {
		t.Fatalf("%d second reads ran a minute after the first read, want one", ran)
	}
	if _, pr := capturePhase(t, rig.pool); pr.ConflictingHead != "code" || pr.ConflictRead != nil {
		t.Fatalf("the record holds %q conflicting with first read %+v, want the conflict of code confirmed", pr.ConflictingHead, pr.ConflictRead)
	}
	if asks := mergeForwardAsks(t, rig.pool); len(asks) != 1 || asks[0].Role != claim.RoleImplementer || asks[0].Head != "code" {
		t.Fatalf("the outbox holds the merge-forward asks %+v, want one, to the implementer at head code", asks)
	}
	if got := rig.reads.Load(); got != reads {
		t.Fatalf("GitHub was read %d times, want %d", got, reads)
	}
}

// endedWithNothingAsked holds that the reader let CAPTURE-1 go with nothing standing behind it,
// that GitHub is read no more after that however long the longest wait is given, and that nothing
// was read conflicting, queued for a second read or asked.
func (rig *readRig) endedWithNothingAsked(t *testing.T) {
	t.Helper()
	testwait.Eventually(t, "the reader to let CAPTURE-1 go", func() bool {
		running, waiting := ladders(rig.w)
		return running == 0 && waiting == 0
	})
	mark := rig.reads.Load()
	time.Sleep(2 * mergeStateFollowWaits * rig.w.mergeStateWait())
	if got := rig.reads.Load(); got != mark {
		t.Fatalf("GitHub was read %d times, want no read after the %d before the reader let CAPTURE-1 go", got, mark)
	}
	if running, waiting := ladders(rig.w); running != 0 || waiting != 0 {
		t.Fatalf("%d issues are queued for a read and %d ladders are running, want none", waiting, running)
	}
	if asks := mergeForwardAsks(t, rig.pool); len(asks) != 0 {
		t.Fatalf("the outbox holds the merge-forward asks %+v, want none", asks)
	}
	if _, _, found := secondReadRow(t, rig.pool); found {
		t.Fatalf("a second read of CAPTURE-1 is queued, want none")
	}
	if _, pr := capturePhase(t, rig.pool); pr.ConflictRead != nil || pr.ConflictingHead != "" {
		t.Fatalf("the record holds first read %+v and %q conflicting, want nothing read conflicting", pr.ConflictRead, pr.ConflictingHead)
	}
}

// A read of the merge state that a fact asks for is owed until an answer applies (climbMergeState).
// A read that fails - GitHub's read of the pull request, or the read of the record before it -
// answers nothing about the head, so the reader keeps the read and makes it again after a wait,
// with no other fact asking. Here the first read fails before GitHub has answered anything, with no
// head to follow; GitHub then answers CONFLICTING, which takes the road of every CONFLICTING
// answer: a first conflicting read, the second read a minute on that confirms it, and the one ask
// of a merge forward the confirmed conflict makes. The waits after failed reads double from the
// ladder's wait up to twenty of them, a rate limit's longer wait is waited out instead, and a
// failed read counts as none of the climb's five reads. A request for a read ends the wait and
// reads at once, and the pull request's close and the reader's end each end the reads with nothing
// asked. The control, dropFirstReadFailure, ends the climb on the failure as a climb once ended:
// no reader then holds the read, and nothing is ever asked.
func TestTheAfterFixReadSurvivesItsFirstError(t *testing.T) {
	t.Parallel()

	t.Run("a failed read of GitHub is made again, and its CONFLICTING answer is confirmed and asked once", func(t *testing.T) {
		// The read after the failure is held until the test has seen the reader keep the read.
		held := make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(held) }) }
		rig := newReadRig(t, approvedCapture, func(read int32) pullAnswer {
			switch read {
			case 1:
				return unavailablePull
			case 2:
				<-held
			}
			return conflictingPull
		})
		t.Cleanup(release)
		rig.start(t)
		rig.keptAfterTheFailure(t, 2)
		release()
		rig.confirmedAndAskedOnce(t, 3)
	})

	t.Run("a failed read of the record is made again, and GitHub's CONFLICTING answer is confirmed and asked once", func(t *testing.T) {
		held := make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(held) }) }
		rig := newReadRig(t, approvedCapture, func(read int32) pullAnswer {
			if read == 1 {
				<-held
			}
			return conflictingPull
		})
		t.Cleanup(release)
		records := &failingIssueRead{Store: rig.w.records}
		records.fail.Store(true)
		rig.w.records = records
		rig.start(t)
		rig.keptAfterTheFailure(t, 1)
		if failed, _ := rig.lines(t, climbFailed)[0]["error"].(string); !strings.Contains(failed, "read the record of CAPTURE-1") {
			t.Fatalf("the climb logged the error %q, want the failed read of CAPTURE-1's record", failed)
		}
		release()
		rig.confirmedAndAskedOnce(t, 2)
	})

	t.Run("the waits after failed reads double up to twenty of the ladder's, and the close of the pull request ends the reads", func(t *testing.T) {
		rig := newReadRig(t, approvedCapture, func(int32) pullAnswer { return unavailablePull })
		rig.start(t)
		rig.keptAfterTheFailure(t, 2)
		unit := rig.w.mergeStateWait()
		want := []time.Duration{unit, 2 * unit, 4 * unit, 8 * unit, 16 * unit, 20 * unit, 20 * unit}
		testwait.Eventually(t, "seven failed reads", func() bool { return len(rig.lines(t, climbFailed)) >= len(want) })
		for i, line := range rig.lines(t, climbFailed)[:len(want)] {
			// Every failure is the climb's first read still: a failed read is none of its five.
			if line["read"] != float64(1) || line["wait"] != float64(want[i]) {
				t.Fatalf("failed read %d logged %v, want the climb's first read still, made again after %s", i+1, line, want[i])
			}
		}
		// GitHub's close, as its webhook records it. A close asks for no read: the next read of
		// the record finds no open pull request, which is an answer the climb ends on.
		applyFact(t, rig.pool, "github", "closed-86", intake.PullRequestClosed{Repo: "acme/widgets", Number: 86, HeadSHA: "code",
			UpdatedAt: time.Now().UTC()}, rig.w.handlers...)
		rig.endedWithNothingAsked(t)
	})

	t.Run("a rate limit's longer wait is waited out, and the reader's end ends it", func(t *testing.T) {
		rig := newReadRig(t, approvedCapture, func(int32) pullAnswer { return limitedPull })
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go rig.w.readMergeStates(ctx)
		applyFact(t, rig.pool, "github", "checks-on-code", settledChecksOnCode(), rig.w.handlers...)
		testwait.Eventually(t, "the failed read to be logged", func() bool { return len(rig.lines(t, climbFailed)) >= 1 })
		if testwait.Holds(2*mergeStateFollowWaits*rig.w.mergeStateWait(), testwait.Poll, func() bool {
			return !owesCapture(rig.w) || rig.reads.Load() > 1
		}) {
			t.Fatalf("after the rate-limited read the reader holds CAPTURE-1's read %t, and GitHub was read %d times; want the read held through the minute the limit asks for, with no read before it",
				owesCapture(rig.w), rig.reads.Load())
		}
		if failed := rig.lines(t, climbFailed); len(failed) != 1 || failed[0]["wait"] != float64(time.Minute) {
			t.Fatalf("the climb logged %v, want one failed read, made again after the minute GitHub's limit asks for", failed)
		}
		cancel()
		rig.endedWithNothingAsked(t)
	})

	t.Run("a request for a read ends the wait after a failed read and reads at once", func(t *testing.T) {
		rig := newReadRig(t, approvedCapture, func(read int32) pullAnswer {
			if read == 1 {
				return limitedPull
			}
			return conflictingPull
		})
		rig.start(t)
		testwait.Eventually(t, "the failed read to be logged", func() bool { return len(rig.lines(t, climbFailed)) >= 1 })
		asked := time.Now()
		// The head's next CI settlement asks for a read of its own (workflow's askMergeState).
		later := settledChecksOnCode()
		later.Generation, later.Snapshot = 2, "settled-later"
		later.CheckRuns = []record.AttemptRun{{Name: "pr-checks-result", ID: 4}}
		applyFact(t, rig.pool, "github", "checks-on-code-again", later, rig.w.handlers...)
		rig.confirmedAndAskedOnce(t, 3)
		if waited := time.Since(asked); waited >= time.Minute/2 {
			t.Fatalf("GitHub answered %s after the request, want the request to end the minute's wait", waited)
		}
	})

	t.Run("with the failure dropped no reader holds the read, and nothing is ever asked", func(t *testing.T) {
		rig := newReadRig(t, approvedCapture, func(read int32) pullAnswer {
			if read == 1 {
				return unavailablePull
			}
			return conflictingPull
		})
		rig.w.dropFirstReadFailure = true
		rig.start(t)
		testwait.Eventually(t, "the failed read to be logged", func() bool { return len(rig.lines(t, climbFailed)) >= 1 })
		rig.endedWithNothingAsked(t)
		if got := rig.reads.Load(); got != 1 {
			t.Fatalf("GitHub was read %d times, want the one read that failed", got)
		}
	})
}
