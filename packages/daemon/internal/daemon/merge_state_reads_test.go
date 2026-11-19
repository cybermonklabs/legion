package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
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

// followRig is CAPTURE-1 in reviewing with its reviewer's approval standing on head code and no
// merge state read yet (approvedCapture), GitHub answering the nth read of its pull request as
// answer says and counting the reads, the base main requiring lint (green nowhere) and
// pr-checks-result, and the runtime whose ladder reads it, its waits cut to retry through its own
// seam (mergeStateRetry) and its log kept.
type followRig struct {
	pool  *store.Pool
	w     *workflowRuntime
	logs  *syncBuffer
	reads atomic.Int32
}

func newFollowRig(t *testing.T, retry time.Duration, answer func(read int32) pullAnswer) *followRig {
	t.Helper()
	rig := &followRig{pool: isolatedOutboxPool(t), logs: &syncBuffer{}}
	approvedCapture(t, rig.pool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			got := answer(rig.reads.Add(1))
			if got.status != 0 {
				w.WriteHeader(got.status)
			}
			w.Write([]byte(got.body))
		case "/repos/acme/widgets/rules/branches/main":
			w.Write([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"lint"},{"context":"pr-checks-result"}]}}]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	rig.w = readRuntime(rig.pool, server.URL, slog.New(slog.NewJSONHandler(rig.logs, nil)))
	rig.w.mergeStateRetry = retry
	return rig
}

// start runs the runtime's reader until the test ends, and applies the CI settlement of head code,
// which asks for a read of GitHub's merge state since the approval waits on it.
func (rig *followRig) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
	applyFact(t, rig.pool, "github", "checks-on-code", settledChecksOnCode(), rig.w.handlers...)
}

// follows is every line of a read after the ladder, oldest first.
func (rig *followRig) follows(t *testing.T) []map[string]any {
	t.Helper()
	return loggedLines(t, rig.logs.String(), followRead)
}

// running is how many ladders the runtime marks as running, and waiting how many issues wait for
// one.
func (rig *followRig) running() (running, waiting int) {
	rig.w.mergeStateMu.Lock()
	defer rig.w.mergeStateMu.Unlock()
	return len(rig.w.mergeStateRunning), rig.w.mergeStates.waiting()
}

// endedWithNoRead holds that the reads after the ladder end, logging their end, with nothing
// standing behind them, and that GitHub is read no more than mark times however long their longest
// wait is given.
func (rig *followRig) endedWithNoRead(t *testing.T, mark int32) {
	t.Helper()
	testwait.Eventually(t, "the reads after the ladder to end", func() bool {
		running, _ := rig.running()
		return running == 0 && len(loggedLines(t, rig.logs.String(), followEnd)) == 1
	})
	time.Sleep(2 * mergeStateFollowWaits * rig.w.mergeStateWait())
	if got := rig.reads.Load(); got > mark {
		t.Fatalf("GitHub was read %d times, want no read after the %d before the reads after the ladder ended", got, mark)
	}
	if running, waiting := rig.running(); running != 0 || waiting != 0 {
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
	const retry = 10 * time.Millisecond
	forever := func(int32) pullAnswer { return unknownPull }

	t.Run("unknown five times and then CONFLICTING is recorded and reaches R0's decision", func(t *testing.T) {
		rig := newFollowRig(t, retry, func(read int32) pullAnswer {
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
		if ran := runSecondReads(t, secondReadRunner(rig.w), pr.ConflictRead.At.Add(time.Minute)); ran != 1 {
			t.Fatalf("%d second reads ran a minute after the first read, want one", ran)
		}
		_, pr = capturePhase(t, rig.pool)
		if pr.ConflictingHead != "code" || pr.ConflictRead != nil {
			t.Fatalf("the record holds %q conflicting with first read %+v, want the conflict of code confirmed", pr.ConflictingHead, pr.ConflictRead)
		}
		if asked := pr.MergeForward; asked == nil || asked.Head != "code" || asked.Why != classify.ConflictCheckNotGreen {
			t.Fatalf("the record holds merge forward %+v, want one asked of code for %s: lint is required and green nowhere", asked, classify.ConflictCheckNotGreen)
		}
		if asked := loggedLines(t, rig.logs.String(), "workflow: merge forward asked"); len(asked) != 1 {
			t.Fatalf("the workflow logged %v, want one merge forward asked", asked)
		}
		if got := rig.reads.Load(); got != mergeStateLadderReads+2 {
			t.Fatalf("GitHub was read %d times, want the ladder's, the one after it and the second read", got)
		}
	})

	t.Run("the waits double up to twenty of the ladder's, and the reads end when the pull request closes", func(t *testing.T) {
		rig := newFollowRig(t, retry, forever)
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
		mark := rig.reads.Load()
		if _, pr := capturePhase(t, rig.pool); pr.State != record.PullRequestClosed {
			t.Fatalf("the record holds the pull request %s, want it closed", pr.State)
		}
		rig.endedWithNoRead(t, mark)
	})

	t.Run("the reads end when the record's head moves", func(t *testing.T) {
		rig := newFollowRig(t, retry, forever)
		rig.start(t)
		testwait.Eventually(t, "three reads after the ladder", func() bool { return len(rig.follows(t)) >= 3 })
		// The record takes another head with no fact that asks for a read: the reads after the
		// ladder follow the head they read, and read nothing of another.
		recordCapture(t, rig.pool, func(pr *record.PullRequest) { pr.HeadSHA = "moved" })
		rig.endedWithNoRead(t, rig.reads.Load())
	})

	t.Run("a request while a read after the ladder waits starts a climb at once", func(t *testing.T) {
		// A larger unit, so the wait the request ends - eight of them - is far longer than the
		// test's own step between the read before it and the request.
		const slower = 50 * time.Millisecond
		rig := newFollowRig(t, slower, forever)
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

// The read the ladder makes is the one that moves an approved issue on: GitHub answers the state
// clean, the daemon applies it as the fact a webhook would have carried, and the round the
// approval left waiting ends in accepting. One answer is enough, so the ladder reads once.
func TestTheMergeStateReadMovesAnApprovedIssueToAccepting(t *testing.T) {
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
