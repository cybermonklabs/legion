package daemon

import (
	"context"
	"errors"
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

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
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

// start runs the runtime's reader until the test ends, and applies the CI settlement of head code,
// which asks for a read of GitHub's merge state when an approval waits on it (approvedCapture).
func (rig *readRig) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
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

// A head GitHub has answered for reaches its base's reads, and they read it until GitHub's answer is
// recorded (dispatch://INFRA-4374 R4), though one step fails once after that answer: the record's
// read of the pull request, as a transient error of the database fails it; the mint of the
// implement token for the base's query, as a mint GitHub does not answer in time fails it; GitHub's
// REST read, which answers 403 once the implement App's REST budget is spent, the failure this
// batch exists for (dispatch://INFRA-3663); or the record's write of GitHub's answer. Each row arms
// its failure at one of GitHub's answers (after), and the next step of that kind fails:
//
//   - After a climb's last `unknown`, and after a second read whose attempts all read `unknown`,
//     the head goes to its base with the pull request the read before held (followOnBase), so the
//     failed step is the base's own: its record read, or its mint, which keeps the issue owed. The
//     base logs that read with no answer and reads the head at its next step. Had the handover
//     read the record again, its failed read would have ended the reads there.
//   - After a climb's first `unknown`, its second read fails - the record's read, or GitHub's 403 -
//     and the climb hands the pull request its `unknown` was read of to its base (climbMergeState)
//     as after its last read. Had it handed over nothing, nothing would read the head again until
//     its next event, and the record would stand at `unknown`.
//   - GitHub answers a state, CONFLICTING, and the database fails the write of it
//     (applyMergeability): at a climb's first read, which hands the pull request to its base as if
//     GitHub had answered nothing (climbMergeState, errUnapplied), and at the base's first read,
//     which keeps the issue owed, logs that read with no answer and reads it at its next step
//     (readChunk). Had either taken GitHub's state as recorded, the climb would have ended on it
//     and the base would have let the issue go, and the record would hold what it held before the
//     read until the issue's next event.
//
// Either way GitHub's CONFLICTING, at the base's read, is recorded as the first conflicting read
// of code on main. Each row counts the failures it caused and holds that there was one, and holds
// that the step it failed logged its failure once, with the line of that step.
func TestAFailedReadLosesNoHeadHandedToItsBase(t *testing.T) {
	t.Parallel()
	// A read the base logs: its attempt, its wait in the ladder's waits, and what it answered.
	type logged struct {
		attempt, waits int
		answered       string
	}
	noAnswerThenDirty := []logged{{1, 2, "no answer"}, {2, 4, string(record.MergeStateDirty)}}
	dirty := []logged{{1, 2, string(record.MergeStateDirty)}}
	failReads := func(w *workflowRuntime, armed *armedFailure) {
		w.records = failingPullRecords{Store: w.records, armed: armed}
	}
	failMints := func(w *workflowRuntime, armed *armedFailure) {
		w.tokens = failingTokens{Tokens: w.tokens, armed: armed}
	}
	failWrites := func(w *workflowRuntime, armed *armedFailure) {
		w.engineOver(failingPullWrites{Store: record.NewStore(), armed: armed})
	}
	// The lines the failed steps log: the base's record read, the base's mint, the climb's read,
	// and a write the climb's read and the base's read made of GitHub's answer.
	const (
		baseRecordRead = "github: read the record of a pull request owed a merge-state read"
		baseMint       = "github: mint the implement App token to read the merge state"
		climbRead      = "read GitHub's merge state for an issue whose read was asked for"
		climbWrite     = "github: the record did not take GitHub's merge state for an issue whose read was asked for"
		baseWrite      = "github: the record did not take GitHub's merge state for a pull request owed a merge-state read"
	)
	for _, tc := range []struct {
		name string
		// second is whether a second read of a conflict hands the head over, rather than a climb.
		second bool
		// GitHub answers `unknown` before its answer after, answer at it, then the next time, and
		// CONFLICTING after that. A then that is no 2xx is the failure, and wrap nil arms nothing;
		// otherwise wrap puts in the step that fails once it is armed, at answer after.
		after        int32
		answer, then pullAnswer
		wrap         func(*workflowRuntime, *armedFailure)
		// warning is the line the failed step logs, once.
		warning string
		// logs is every read the base logs; reads, GitHub's reads in all, the base's among them.
		logs  []logged
		reads int32
	}{
		{name: "the base's record read after a climb's handover", after: mergeStateLadderReads, answer: unknownPull, then: conflictingPull, wrap: failReads, warning: baseRecordRead, logs: noAnswerThenDirty, reads: mergeStateLadderReads + 1},
		{name: "the base's record read after a second read's handover", second: true, after: mergeStateLadderReads, answer: unknownPull, then: conflictingPull, wrap: failReads, warning: baseRecordRead, logs: noAnswerThenDirty, reads: mergeStateLadderReads + 1},
		{name: "the base's mint after a climb's handover", after: mergeStateLadderReads, answer: unknownPull, then: conflictingPull, wrap: failMints, warning: baseMint, logs: noAnswerThenDirty, reads: mergeStateLadderReads + 1},
		{name: "a climb's record read after an unknown answer", after: 1, answer: unknownPull, then: conflictingPull, wrap: failReads, warning: climbRead, logs: dirty, reads: 2},
		{name: "a climb's REST read refused 403 after an unknown answer", after: 1, answer: unknownPull, then: refusedPull, warning: climbRead, logs: dirty, reads: 3},
		{name: "the record's write of a climb's answer", after: 1, answer: conflictingPull, then: conflictingPull, wrap: failWrites, warning: climbWrite, logs: dirty, reads: 2},
		{name: "the record's write of the base's answer", after: mergeStateLadderReads + 1, answer: conflictingPull, then: conflictingPull, wrap: failWrites, warning: baseWrite, logs: noAnswerThenDirty, reads: mergeStateLadderReads + 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			armed := &armedFailure{}
			seed := approvedCapture
			if tc.second {
				seed = captureInTesting
			}
			rig := newReadRig(t, seed, func(read int32) pullAnswer {
				switch {
				case read < tc.after:
					return unknownPull
				case read == tc.after:
					armed.on.Store(tc.wrap != nil)
					return tc.answer
				case read == tc.after+1:
					if tc.then.status != 0 {
						armed.fired.Add(1)
					}
					return tc.then
				}
				return conflictingPull
			})
			if tc.wrap != nil {
				tc.wrap(rig.w, armed)
			}
			if tc.second {
				rig.runSecondReadOut(t)
			} else {
				rig.start(t)
			}

			testwait.Eventually(t, "the base's reads to record GitHub's CONFLICTING after the failure", func() bool {
				_, pr := capturePhase(t, rig.pool)
				return pr.ConflictRead != nil && len(rig.follows(t)) >= len(tc.logs)
			})
			if fired := armed.fired.Load(); fired != 1 {
				t.Fatalf("%d steps failed, want the one armed at GitHub's answer %d", fired, tc.after)
			}
			if warned := loggedLines(t, rig.logs.String(), tc.warning); len(warned) != 1 {
				t.Fatalf("the daemon logged %v, want %q once", warned, tc.warning)
			}
			unit := rig.w.mergeStateWait()
			follows := rig.follows(t)
			matches := len(follows) == len(tc.logs)
			for i := 0; matches && i < len(follows); i++ {
				want := tc.logs[i]
				matches = follows[i]["attempt"] == float64(want.attempt) && follows[i]["wait"] == float64(time.Duration(want.waits)*unit) &&
					follows[i]["answered"] == want.answered && follows[i]["head"] == "code"
			}
			if !matches {
				t.Fatalf("the base's reads logged %v, want %+v in the ladder's waits of %s", follows, tc.logs, unit)
			}
			if got := rig.reads.Load(); got != tc.reads {
				t.Fatalf("GitHub was read %d times, want %d", got, tc.reads)
			}
			_, pr := capturePhase(t, rig.pool)
			if pr.Mergeability != record.MergeabilityConflicting || pr.ConflictRead.Head != "code" || pr.ConflictRead.Base != "main" {
				t.Fatalf("the record holds %s with first read %+v, want GitHub's CONFLICTING recorded as the first read of code on main", pr.Mergeability, pr.ConflictRead)
			}
		})
	}
}

// refusedPull is GitHub's answer to a read of the pull request once the implement App
// installation's REST budget is spent: 403, with the budget's remaining count at 0.
var refusedPull = pullAnswer{status: http.StatusForbidden, header: http.Header{"X-Ratelimit-Remaining": {"0"}},
	body: `{"message":"API rate limit exceeded for installation ID 171972716.","documentation_url":"https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api"}`}

// A climb asked for while its base's reads follow the head - here the head's next CI settlement,
// after a climb handed the head over - takes the issue out of those reads only once its first read
// has read GitHub (climbMergeState). Its first read here is refused 403, as GitHub refuses every
// read once the implement App's REST budget is spent, so the climb has nothing to hand back, and
// the base's reads go on: they read the head on their ladder and record GitHub's CONFLICTING. Had
// the request taken the issue out of them at once, nothing would read the head again until its
// next event, and the record would stand at `unknown`.
func TestAClimbWhoseFirstReadFailsLeavesTheHeadWithItsBase(t *testing.T) {
	t.Parallel()
	// A larger unit, so the base's first read after the handover, two of them on, is far later
	// than the test's own step from the handover to the request, and the refused read is the
	// climb's.
	const slower = 200 * time.Millisecond
	rig := newReadRig(t, approvedCapture, func(read int32) pullAnswer {
		switch {
		case read <= mergeStateLadderReads:
			return unknownPull
		case read == mergeStateLadderReads+1:
			return refusedPull
		}
		return conflictingPull
	})
	rig.w.mergeStateRetry = slower
	rig.start(t)
	testwait.Eventually(t, "the climb to hand the head to its base", func() bool {
		rig.w.mergeStateMu.Lock()
		defer rig.w.mergeStateMu.Unlock()
		return rig.w.baseReadOf["CAPTURE-1"] != ""
	})
	// The head's next CI settlement asks for a read of its own (workflow's askMergeState).
	later := settledChecksOnCode()
	later.Generation, later.Snapshot = 2, "settled-later"
	later.CheckRuns = []record.AttemptRun{{Name: "pr-checks-result", ID: 4}}
	applyFact(t, rig.pool, "github", "checks-on-code-again", later, rig.w.handlers...)

	testwait.Eventually(t, "the base's reads to record GitHub's CONFLICTING after the climb's refused read", func() bool {
		_, pr := capturePhase(t, rig.pool)
		return pr.ConflictRead != nil
	})
	refused := loggedLines(t, rig.logs.String(), "read GitHub's merge state for an issue whose read was asked for")
	if len(refused) != 1 || refused[0]["read"] != float64(1) || refused[0]["handedToBase"] != false {
		t.Fatalf("the climbs logged %v failed reads, want the second climb's first read refused, with nothing to hand back", refused)
	}
	follows := rig.follows(t)
	if len(follows) != 1 || follows[0]["attempt"] != float64(1) || follows[0]["wait"] != float64(2*slower) ||
		follows[0]["answered"] != string(record.MergeStateDirty) {
		t.Fatalf("the base's reads logged %v, want its first read of the head handed over, after two of the ladder's waits, answering dirty", follows)
	}
	if got := rig.reads.Load(); got != mergeStateLadderReads+2 {
		t.Fatalf("GitHub was read %d times, want the first climb's %d, the second climb's refused read and the base's", got, mergeStateLadderReads)
	}
	_, pr := capturePhase(t, rig.pool)
	if pr.Mergeability != record.MergeabilityConflicting || pr.ConflictRead.Head != "code" || pr.ConflictRead.Base != "main" {
		t.Fatalf("the record holds %s with first read %+v, want GitHub's CONFLICTING recorded as the first read of code on main", pr.Mergeability, pr.ConflictRead)
	}
}

// armedFailure is a failure a test arms (on) and the stand-in that fails takes up once, counting
// each time it fails (fired).
type armedFailure struct {
	on    atomic.Bool
	fired atomic.Int32
}

// fire says whether the step that asks fails now: once, the first time it asks after the failure
// was armed.
func (a *armedFailure) fire() bool {
	if a.on.CompareAndSwap(true, false) {
		a.fired.Add(1)
		return true
	}
	return false
}

// failingPullRecords is a runtime's records whose next read of a pull request's row (PullRequestRow,
// the read issueRecord makes) fails once armed, as a transient error of the database fails it, and
// which read as records do after that.
type failingPullRecords struct {
	record.Store
	armed *armedFailure
}

func (s failingPullRecords) PullRequestRow(ctx context.Context, tx pgx.Tx, issue string) (*record.PullRequest, error) {
	if s.armed.fire() {
		return nil, errors.New("the database closed the connection")
	}
	return s.Store.PullRequestRow(ctx, tx, issue)
}

// failingPullWrites is an engine's records whose next write of a pull request fails once armed, as
// a transient error of the database fails it, and which write as records do after that.
type failingPullWrites struct {
	record.Store
	armed *armedFailure
}

func (s failingPullWrites) PutPullRequest(ctx context.Context, tx pgx.Tx, pr record.PullRequest) error {
	if s.armed.fire() {
		return errors.New("the database closed the connection")
	}
	return s.Store.PutPullRequest(ctx, tx, pr)
}

// failingTokens is a runtime's token source whose next mint of an installation token fails once
// armed, as a mint GitHub does not answer in time fails, and which mints as before after it.
type failingTokens struct {
	appauth.Tokens
	armed *armedFailure
}

func (s failingTokens) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	if s.armed.fire() {
		return appauth.Lease{}, mintTimedOut(role, owner)
	}
	return s.Tokens.Token(ctx, role, owner)
}

// mintTimedOut is the error a mint of role's token for owner answers when GitHub does not answer
// it in time: appauth's TransientError, which carries no answer of GitHub's.
func mintTimedOut(role appauth.AppRole, owner string) error {
	return &appauth.TransientError{Err: fmt.Errorf("mint the %s token for %s: %w", role, owner, context.DeadlineExceeded)}
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
		state, pr, err := w.readMergeState(context.Background(), "CAPTURE-1")
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
