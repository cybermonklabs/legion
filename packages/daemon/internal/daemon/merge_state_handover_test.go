package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

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
// of code on main. Each row holds that the step it failed logged its failure once, with the line of
// that step.
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
		// CONFLICTING after that. wrap, when there is one, puts in the step that fails once it is
		// armed, at answer after.
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
					armed.on.Store(true)
					return tc.answer
				case read == tc.after+1:
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

// A climb asked for while its base's reads follow the head - here the head's next CI settlement,
// after a climb handed the head over - takes the issue out of those reads only once its first read
// has read GitHub (climbMergeState). Its first read here is refused 403, as GitHub refuses every
// read once the implement App's REST budget is spent, so the climb has nothing to hand back, and
// the base's reads go on: they read the head on their ladder and record GitHub's CONFLICTING. Had
// the request taken the issue out of them at once, nothing would read the head again until its
// next event, and the record would stand at `unknown`.
func TestAClimbWhoseFirstReadFailsLeavesTheHeadWithItsBase(t *testing.T) {
	t.Parallel()
	// A larger unit, so the base's first read after the handover, two of them on, usually comes
	// after the climb's first read. The refusal is served by route (refuseREST), to the climb's REST
	// read alone, so a slow run in which the base's query comes first changes nothing it holds.
	const slower = 200 * time.Millisecond
	rig := newReadRig(t, approvedCapture, func(read int32) pullAnswer {
		if read <= mergeStateLadderReads {
			return unknownPull
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
	// The head's next CI settlement asks for a read of its own (workflow's askMergeState), and
	// GitHub refuses that climb's first read.
	rig.refuseREST.Store(true)
	later := settledChecksOnCode()
	later.Generation, later.Snapshot = 2, "settled-later"
	later.CheckRuns = []record.AttemptRun{{Name: "pr-checks-result", ID: 4}}
	applyFact(t, rig.pool, "github", "checks-on-code-again", later, rig.w.handlers...)

	const climbRead = "read GitHub's merge state for an issue whose read was asked for"
	testwait.Eventually(t, "the base's reads to record GitHub's CONFLICTING, and the climb's read to be refused", func() bool {
		_, pr := capturePhase(t, rig.pool)
		return pr.ConflictRead != nil && len(loggedLines(t, rig.logs.String(), climbRead)) > 0
	})
	refused := loggedLines(t, rig.logs.String(), climbRead)
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

// A push to the base that commits while a climb's first read runs asks the base's reads for a read
// of its own (queueBaseMergeStates), and that request stands: once its first read has read
// GitHub, the climb takes the issue out of its base's reads only under the request that stood when
// it began (climbMergeState, leaveBase), and request numbers are never reused (putLocked,
// workflowRuntime.baseAsks). The climb's first read answers MERGEABLE and clean, which ends the
// climb; GitHub's answer to the push's read is CONFLICTING, and the record holds it. The push's
// read waits until the climb has ended, so the climb's choice is made before that read applies its
// answer. Each row holds the issue somewhere else as the climb begins:
//
//   - In no batch: the climb has nothing to take out.
//   - In its base's batch, handed over as a climb hands a head over (followOnBase): the push's
//     request replaces that one in the same batch, under a newer number, which the climb's
//     leaveBase does not match. A climb that took the issue out whatever its number would take
//     the push's request out.
//   - In its base's batch, which ends during the climb's first read - the base's follow comes due,
//     GitHub answers it clean, and the issue leaves - before the push makes the batch again: the
//     push's request takes the next number of the runtime's one count, never the number the
//     climb noted. A count of each batch's own would start again at the number the climb noted.
//
// Had the climb taken the push's request out, the push's read would have read nothing or applied
// nothing (readChunk applies no answer for an issue its batch no longer holds), and the head would
// go unread after the push until its next event.
func TestABasePushDuringAClimbsFirstReadKeepsItsRead(t *testing.T) {
	t.Parallel()
	acme := ghrepo.MustParse("acme/widgets")
	key := baseReadKey(acme, "main")
	climbing := func(w *workflowRuntime) bool {
		w.mergeStateMu.Lock()
		defer w.mergeStateMu.Unlock()
		_, running := w.mergeStateRunning["CAPTURE-1"]
		return running
	}
	batched := func(w *workflowRuntime) bool {
		w.mergeStateMu.Lock()
		defer w.mergeStateMu.Unlock()
		return w.baseReads[key] != nil
	}
	for _, tc := range []struct {
		name string
		// handed is whether the head is handed to its base before the climb, and ended whether
		// that batch ends during the climb's first read, before the push.
		handed, ended bool
		// reads is GitHub's reads in all, the push's the last of them, and readsAre what they are.
		reads    int32
		readsAre string
	}{
		{name: "no batch holds the issue", reads: 2, readsAre: "the climb's first read and the push's"},
		{name: "its base's batch holds the issue", handed: true, reads: 2, readsAre: "the climb's first read and the push's"},
		{name: "its base's batch holds the issue and ends during the first read", handed: true, ended: true, reads: 3,
			readsAre: "the climb's first read, the base's follow that ended the batch, and the push's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			push := tc.reads
			var current atomic.Pointer[readRig]
			rig := newReadRig(t, captureInTesting, func(read int32) pullAnswer {
				w := current.Load().w
				switch {
				case read == 1:
					if tc.ended {
						// The base's follow comes due now; GitHub answers it clean, and the
						// batch, with nothing owed, ends.
						w.mergeStateMu.Lock()
						if batch := w.baseReads[key]; batch == nil {
							t.Errorf("no batch of main holds CAPTURE-1 as the climb's first read runs, want the one the hand-over made")
						} else {
							place := batch.owed["CAPTURE-1"]
							place.due = time.Now().UTC()
							batch.owed["CAPTURE-1"] = place
							nudge(batch.wake)
						}
						w.mergeStateMu.Unlock()
						if !testwait.Holds(10*time.Second, time.Millisecond, func() bool { return !batched(w) }) {
							t.Errorf("the base's batch did not end after its follow read the issue")
						}
					}
					// The push to main commits while the climb's first read runs.
					w.queueBaseMergeStates("acme/widgets", "main", []string{"CAPTURE-1"})
					return cleanPull
				case read < push:
					return cleanPull
				case read == push:
					if !testwait.Holds(10*time.Second, time.Millisecond, func() bool { return !climbing(w) }) {
						t.Errorf("the climb did not end before the push's read")
					}
				}
				return conflictingPull
			})
			current.Store(rig)
			// No wait of the ladder's comes due in the test: a follow falls due two hours on.
			rig.w.mergeStateRetry = time.Hour
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go rig.w.readMergeStates(ctx)
			if tc.handed {
				rig.w.queueBaseRead(acme, "main", map[string]string{"CAPTURE-1": "code"}, false)
				// The climb notes this batch and this request as it begins: nothing else puts the
				// issue anywhere before it, and the follow's first read is two hours on.
				if held, asked := rig.w.heldUnder("CAPTURE-1"); held != key || asked == 0 {
					t.Fatalf("the base's batch holds CAPTURE-1 as %q under request %d, want main's batch under a request of its own", held, asked)
				}
			}
			rig.w.queueMergeState("CAPTURE-1")

			testwait.Eventually(t, "the push's read to record GitHub's CONFLICTING after the climb's first read", func() bool {
				_, pr := capturePhase(t, rig.pool)
				return pr.ConflictRead != nil
			})
			if got := rig.reads.Load(); got != tc.reads {
				t.Fatalf("GitHub was read %d times, want %d: %s", got, tc.reads, tc.readsAre)
			}
			_, pr := capturePhase(t, rig.pool)
			if pr.Mergeability != record.MergeabilityConflicting || pr.ConflictRead.Head != "code" || pr.ConflictRead.Base != "main" {
				t.Fatalf("the record holds %s with first read %+v, want GitHub's CONFLICTING recorded as the first read of code on main", pr.Mergeability, pr.ConflictRead)
			}
		})
	}
}

// A climb takes the issue out of its base's reads once its first read has read GitHub and the
// record took the answer (climbMergeState): here the head was handed to its base, a climb is
// asked, and GitHub answers its first read MERGEABLE and clean. The climb ends on that answer, and
// the base's batch, which held the issue under the request the climb noted, is left with nothing
// owed and ends, reading GitHub no more. A climb that left the issue in its base's reads would
// leave that batch running, to read the head again at its follow's next step.
func TestAClimbsFirstReadTakesTheIssueOutOfItsBase(t *testing.T) {
	t.Parallel()
	rig := newReadRig(t, captureInTesting, func(int32) pullAnswer { return cleanPull })
	// No wait of the ladder's comes due in the test: the follow falls due two hours on.
	rig.w.mergeStateRetry = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rig.w.readMergeStates(ctx)
	rig.w.queueBaseRead(ghrepo.MustParse("acme/widgets"), "main", map[string]string{"CAPTURE-1": "code"}, false)
	rig.w.queueMergeState("CAPTURE-1")

	testwait.Eventually(t, "the climb's read, and the climb and the base's batch to end", func() bool {
		running, waiting := ladders(rig.w)
		return rig.reads.Load() == 1 && running == 0 && waiting == 0
	})
	if got := rig.reads.Load(); got != 1 {
		t.Fatalf("GitHub was read %d times, want the climb's one read", got)
	}
	if held, _ := rig.w.heldUnder("CAPTURE-1"); held != "" {
		t.Fatalf("CAPTURE-1 is held by %q after the climb read it, want no batch", held)
	}
}

// armedFailure is a failure a test arms (on) and the stand-in that fails takes up once.
type armedFailure struct {
	on atomic.Bool
}

// fire says whether the step that asks fails now: once, the first time it asks after the failure
// was armed.
func (a *armedFailure) fire() bool {
	return a.on.CompareAndSwap(true, false)
}

// failingPullRecords is a runtime's records whose next read of an issue's pull request rows
// (TaskPullRequestRows, the read issueRecord makes) fails once armed, as a transient error of the
// database fails it, and which read as records do after that.
type failingPullRecords struct {
	record.Store
	armed *armedFailure
}

func (s failingPullRecords) TaskPullRequestRows(ctx context.Context, tx pgx.Tx, issue string) ([]record.PullRequest, error) {
	if s.armed.fire() {
		return nil, errors.New("the database closed the connection")
	}
	return s.Store.TaskPullRequestRows(ctx, tx, issue)
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
