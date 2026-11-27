package supervise

// A merge forward is the daemon's ask that a role merge the base forward onto the issue's head
// (internal/workflow's mergeForward). It is an Envoy role message and no take, so between two runs
// of that merge, or before its first run starts, no role of the issue is in a turn
// (Deps.IssueInTurn), and a take that adopts the shared working copy then lands an empty commit on
// the merge in progress (dispatch://INFRA-4172). While the record holds a merge forward owed to a
// role and not landed (Deps.MergeForwardOwed), a take that would adopt the working copy is refused
// in-turn, whatever role takes, the asked role's own included (takeable). These tests are rows 40
// to 46 of plan v154's state table, and the take whose read of that record fails.
//
// Every negative control is 6ed3e46f98896fb4f010554d9be7d9bd4e93a81f, where takeable holds a take
// for another role's turn alone: no Deps.MergeForwardOwed is read, the take is accepted and the
// working copy adopted over the merge, and each test fails at its first want of the hold.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// mergeForwardRecord is the record as Deps.MergeForwardOwed reads it: the role the issue's merge
// forward is owed to while it is owed and not landed, a failure the read can be made to answer, and
// every issue it was asked about.
type mergeForwardRecord struct {
	mu     sync.Mutex
	owed   bool
	role   claim.Role
	fail   error
	issues []string
}

// asks makes the record hold a merge forward owed to the implementer, not landed.
func (r *mergeForwardRecord) asks() { r.set(true, claim.RoleImplementer, nil) }

// ends makes the record hold none owed: the merge landed, was withdrawn, or was cleared.
func (r *mergeForwardRecord) ends() { r.set(false, "", nil) }

func (r *mergeForwardRecord) set(owed bool, role claim.Role, fail error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owed, r.role, r.fail = owed, role, fail
}

func (r *mergeForwardRecord) read(_ context.Context, issue string) (claim.Role, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.issues = append(r.issues, issue)
	if r.fail != nil {
		return "", false, r.fail
	}
	return r.role, r.owed, nil
}

// asked is how many times the record was read, for the issue named.
func (r *mergeForwardRecord) asked(issue string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	asked := 0
	for _, got := range r.issues {
		if got == issue {
			asked++
		}
	}
	return asked
}

// mergeForwardHarness is a ready claim c of the one issue the harnesses share (LEGION-209) whose
// takes read record, adopt the working copy, and see no role of the issue in a turn: the merge
// runs between two of its turns. The worker takes nothing on its own, so each test drives every
// take (manualTakes).
func mergeForwardHarness(t *testing.T, c Claim, record *mergeForwardRecord, set ...func(*Deps)) *harness {
	t.Helper()
	h := newHarnessOf(t, c, append([]func(*Deps){func(d *Deps) {
		d.Identity = adoptingIdentity
		d.MergeForwardOwed = record.read
		d.IssueInTurn = func(string, claim.Token) (claim.Token, bool) { return "", false }
	}}, set...)...)
	h.manualTakes()
	h.reach(StateReady)
	return h
}

// testersTask delivers the tester's phase task, which adopts the working copy.
func testersTask(h *harness) {
	h.must(RequestDeliver{Claim: testerToken, Task: "test the branch", Phase: phase.Testing, Generation: 7})
}

// heldByTheMerge is the tester of a merge forward owed to the implementer: its phase task is
// delivered, its take is refused in-turn, no working copy is adopted, and the value stays in the
// bucket at the revision the refusal was made at, which it answers.
func heldByTheMerge(t *testing.T, record *mergeForwardRecord) (*harness, TaskValue) {
	t.Helper()
	h := mergeForwardHarness(t, testerClaim(), record)
	testersTask(h)
	h.wantRefused(HoldInTurn)
	h.wantCalls("AdoptWorkingCopy", 0)
	h.wantPrompts(0)
	value, held := h.values.value(testerToken)
	if !held {
		t.Fatal("the refusal deleted the value; a refused take leaves the task owed")
	}
	return h, value
}

// wantWrittenAgain fails the test unless the bucket holds token's value under number at a newer
// revision than after, the one signal the worker asks to take at again.
func wantWrittenAgain(t *testing.T, h *harness, token claim.Token, after TaskValue) {
	t.Helper()
	got, held := h.values.value(token)
	if !held || got.Number != after.Number || got.Revision <= after.Revision {
		t.Fatalf("the bucket holds %+v (held %t) for %s, want task %d written again at a revision past %d", got, held, token, after.Number, after.Revision)
	}
}

// Row 40: a merge forward owed to the implementer and not landed, between two of its runs (no role
// of the issue is in a turn): the tester's take of its phase task, which would adopt the working
// copy, is refused in-turn, and no adoption runs. The record is read for the claim's own issue.
//
// The negative control is the take decided as at 6ed3e46f: takeable reads no merge forward, so the
// take is accepted, the working copy adopted, and heldByTheMerge's want of the refusal fails.
func TestTakeOfAnotherRoleIsRefusedInTurnWhileAMergeForwardIsOwed(t *testing.T) {
	record := &mergeForwardRecord{}
	record.asks()

	h, _ := heldByTheMerge(t, record)

	if p := h.pending(); p.taken() || h.claim().TaskTakeState != TakeRefused {
		t.Fatalf("pending %+v with take state %q, want the task untaken and its take refused at the value's revision", p, h.claim().TaskTakeState)
	}
	if got := record.asked(testerClaim().Issue); got == 0 {
		t.Fatalf("the record was asked about %v, want the claim's own issue %s", record.issues, testerClaim().Issue)
	}
}

// Row 41: as row 40, then the merge lands: the daemon posts the issue's claims the event a turn's
// end posts, the hold ends, the value is written again under its number, and the next take adopts
// the merged working copy, once.
//
// A wake while the merge is still owed writes nothing: a hold that still stands costs the worker
// nothing. The negative control is as row 40's: the first take is accepted.
func TestTakeHeldByAMergeForwardIsWrittenAgainWhenItLandsAndTheNextTakeAdopts(t *testing.T) {
	record := &mergeForwardRecord{}
	record.asks()
	h, refused := heldByTheMerge(t, record)

	h.must(IssueTurnEnded{Claim: testerToken})
	if got, _ := h.values.value(testerToken); got != refused {
		t.Fatalf("a wake while the merge is owed wrote %+v over %+v, want nothing written", got, refused)
	}

	record.ends()
	h.must(IssueTurnEnded{Claim: testerToken})

	wantWrittenAgain(t, h, testerToken, refused)
	h.wantCalls("AdoptWorkingCopy", 0)
	if text := h.mustTake(); text != "test the branch" {
		t.Fatalf("the take after the landing was answered %q, want the tester's task", text)
	}
	h.wantCalls("AdoptWorkingCopy", 1)
	h.must(IssueTurnEnded{Claim: testerToken})
	h.observe(runtime.Alive)
	h.wantPrompts(1)
	h.wantCalls("AdoptWorkingCopy", 1)
}

// Row 42: as row 40, then the merge forward is withdrawn, and then, in a case of its own, cleared:
// the hold ends and the value is written again, as at the landing. The machine reads the record
// through one reader and is woken by one event, so the two ends differ only in what the daemon's
// record held (internal/workflow's tests of the end).
//
// The negative control is as row 40's.
func TestTakeHeldByAMergeForwardIsWrittenAgainWhenItIsWithdrawnOrCleared(t *testing.T) {
	for _, end := range []string{"withdrawn", "cleared"} {
		t.Run(end, func(t *testing.T) {
			record := &mergeForwardRecord{}
			record.asks()
			h, refused := heldByTheMerge(t, record)

			record.ends()
			h.must(IssueTurnEnded{Claim: testerToken})

			wantWrittenAgain(t, h, testerToken, refused)
			if text := h.mustTake(); text != "test the branch" {
				t.Fatalf("the take after the merge forward was %s = %q, want the tester's task", end, text)
			}
			h.wantCalls("AdoptWorkingCopy", 1)
		})
	}
}

// Row 43: as row 40, then a daemon restart with the merge still owed, and then the take: it is
// refused in-turn from the record the restarted daemon reads, and no adoption runs. At the landing
// the value is written again and the take adopts, once.
//
// The negative control is as row 40's.
func TestTakeOfARestartedDaemonIsRefusedInTurnFromTheRestoredRecord(t *testing.T) {
	record := &mergeForwardRecord{}
	record.asks()
	h, refused := heldByTheMerge(t, record)

	h.restart()

	h.wantRefused(HoldInTurn)
	h.wantCalls("AdoptWorkingCopy", 0)
	if p := h.pending(); p.taken() {
		t.Fatalf("pending %+v after the restart, want the task untaken", p)
	}

	record.ends()
	h.must(IssueTurnEnded{Claim: testerToken})

	wantWrittenAgain(t, h, testerToken, refused)
	h.mustTake()
	h.wantCalls("AdoptWorkingCopy", 1)
	h.wantPrompts(1)
}

// Row 44: as row 40, a take that adopts nothing: a message another role sent, an operator's own
// task, anything that names no phase. It is not held by the merge forward, is decided as it always
// was, and never reads the record. The control is the same claim's next phase task, which adopts
// and is held.
//
// The negative control is a take held whatever it adopts (the read made before adopts is asked):
// the operator's task is refused in-turn.
func TestTakeThatAdoptsNothingIsNotHeldByAMergeForward(t *testing.T) {
	record := &mergeForwardRecord{}
	record.asks()
	h := mergeForwardHarness(t, testerClaim(), record)

	h.must(RequestDeliver{Claim: testerToken, Task: "the operator asks how the branch builds"})
	if text := h.mustTake(); text != "the operator asks how the branch builds" {
		t.Fatalf("the take of a task that adopts nothing = %q, want it accepted as it always was", text)
	}
	h.wantCalls("AdoptWorkingCopy", 0)
	if got := record.asked(testerClaim().Issue); got != 0 {
		t.Fatalf("the record was read %d times for a take that adopts nothing, want none", got)
	}

	h.must(StreamTurnStart{Claim: testerToken})
	h.must(StreamTurnEnd{Claim: testerToken})
	testersTask(h)
	h.wantRefused(HoldInTurn)
	h.wantCalls("AdoptWorkingCopy", 0)
}

// Row 45: in implementing, the implementer took its phase task and its turn never started, so R5's
// second wait took the take back; the merge forward is owed to it and not landed. Its own retake of
// the task is refused in-turn and the working copy is not adopted over the merge in progress, so the
// merge carries no empty commit. The merge lands, the value is written again (IssueTurnEnded), and
// the retake is accepted once and adopts then.
//
// The negative control is as row 40's: the retake is accepted while the merge is owed, and the
// second adoption runs.
func TestTakeOfTheAskedRolesOwnPhaseTaskAfterTheSecondWaitIsRefusedInTurnWhileItsMergeIsOwed(t *testing.T) {
	record := &mergeForwardRecord{}
	h := mergeForwardHarness(t, queuedClaim(), record, withWaits)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
	first := h.mustTake()
	h.wantCalls("AdoptWorkingCopy", 1)

	record.asks()
	h.advance(turnStartBound)

	h.wantCharges(WaitTurnStart)
	if p := h.pending(); p.taken() {
		t.Fatalf("pending %+v after the second bound, want the task taken back", p)
	}
	h.wantRefused(HoldInTurn)
	h.wantCalls("AdoptWorkingCopy", 1)
	refused, _ := h.values.value(testToken)

	record.ends()
	h.must(IssueTurnEnded{Claim: testToken})

	wantWrittenAgain(t, h, testToken, refused)
	if again := h.mustTake(); again != first {
		t.Fatalf("the retake after the landing was answered %q, want %q", again, first)
	}
	h.wantCalls("AdoptWorkingCopy", 2)
	h.wantPrompts(2)
}

// Row 46, the machine's side: the implementer's process is relaunched before the landing, by a
// death and, in a case of its own, by a provider error that the operator's stop and retry carried
// on; the merge forward was asked of it and is owed. The new process's take of its phase task is
// refused in-turn, and so is the tester's, with no adoption. The merge lands: every held take, the
// implementer's own included, is written again, and each is accepted once. (The ask sent again to
// the new launch is the workflow's, and the daemon's: the tests of askAgainAtLaunch.)
//
// The negative control is as row 40's: the new process's take is accepted at once.
func TestTakeOfARelaunchedProcessIsHeldWhileTheMergeForwardIsOwedAndTakenOnceAtTheLanding(t *testing.T) {
	for _, tc := range []struct {
		name string
		// relaunch ends the implementer's process with its task pending and brings the new one up.
		relaunch func(h *harness)
	}{
		{name: "a death", relaunch: func(h *harness) {
			h.must(StreamTurnStart{Claim: testToken})
			h.observe(runtime.Gone)
			h.relaunched()
		}},
		{name: "a provider error's retry", relaunch: func(h *harness) {
			h.must(StreamTurnStart{Claim: testToken})
			h.must(StreamTurnEnd{Claim: testToken})
			h.errorTurn(ProviderError{Message: "overloaded_error", Status: 529})
			h.must(RequestStop{Claim: testToken, By: SuspendedByOperator})
			h.wantState(StateRetired)
			h.must(RequestAskedRetry{Claim: testToken, By: SuspendedByOperator})
			h.relaunched()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := &mergeForwardRecord{}
			record.asks()
			implementer := mergeForwardHarness(t, queuedClaim(), record)
			implementer.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Phase: phase.Implementing, Generation: 7})
			tester, testerValue := heldByTheMerge(t, record)

			tc.relaunch(implementer)

			implementer.wantRefused(HoldInTurn)
			implementer.wantCalls("AdoptWorkingCopy", 0)
			tester.wantRefused(HoldInTurn)
			tester.wantCalls("AdoptWorkingCopy", 0)
			implementerValue, _ := implementer.values.value(testToken)

			record.ends()
			implementer.must(IssueTurnEnded{Claim: testToken})
			tester.must(IssueTurnEnded{Claim: testerToken})

			wantWrittenAgain(t, implementer, testToken, implementerValue)
			wantWrittenAgain(t, tester, testerToken, testerValue)
			// The relaunched process's take carries the note of what its relaunch ended in front of the task.
			if text := implementer.mustTake(); !strings.HasSuffix(text, "implement the plan") {
				t.Fatalf("the relaunched implementer's take after the landing = %q, want its phase task at the end", text)
			}
			if text := tester.mustTake(); text != "test the branch" {
				t.Fatalf("the tester's take after the landing = %q, want its phase task", text)
			}
			implementer.wantCalls("AdoptWorkingCopy", 1)
			tester.wantCalls("AdoptWorkingCopy", 1)
			implementer.wantPrompts(1)
			tester.wantPrompts(1)
		})
	}
}

// A take whose read of the record fails is undecided: it is answered with no outcome, which the
// take route answers 500 CLAIM_WRITE_FAILED, and it is never accepted, so no adoption runs and no
// take stands. It is recorded refused at its revision, and the value is written again under its
// number once the read answers, at the claim's next chance, so the worker asks at the new revision
// (decision 20 of dispatch://INFRA-2811). The take at that revision is accepted once.
//
// The negative control is a failed read read as no merge owed: the take is accepted and adopts
// over a merge that may be in progress.
func TestTakeWhoseMergeForwardReadFailedIsUndecidedWrittenAgainAndNeverAccepted(t *testing.T) {
	record := &mergeForwardRecord{}
	record.set(false, "", errBoom)
	h := mergeForwardHarness(t, testerClaim(), record)
	testersTask(h)
	asked := h.seen()

	answer, err := h.takeValue(asked)

	if answer.Outcome != "" || !errors.Is(err, errBoom) {
		t.Fatalf("the take while the merge forward's read fails = %+v, %v; want no answer and the read's failure", answer, err)
	}
	h.wantCalls("AdoptWorkingCopy", 0)
	h.wantPrompts(0)
	if p := h.pending(); p.taken() || h.claim().TaskTakeState != TakeRefused {
		t.Fatalf("pending %+v with take state %q, want the task untaken and its take recorded refused at its revision", p, h.claim().TaskTakeState)
	}
	if lines := h.logs.lines(takeUndecidedLine); len(lines) != 1 {
		t.Fatalf("%d lines say the take could not be decided, want one: %q", len(lines), lines)
	}

	record.ends()
	h.observe(runtime.Alive)

	wantWrittenAgain(t, h, testerToken, asked)
	written := h.records.of(valueWrittenLine)
	wantAttr(t, written[len(written)-1], "cause", TaskValueTakeFailed)
	if answer, err := h.takeValue(h.seen()); answer.Outcome != TakeOutcomeAccepted {
		t.Fatalf("the take at the newer revision = %+v, %v; want it accepted", answer, err)
	}
	h.wantCalls("AdoptWorkingCopy", 1)
	h.wantCharges()
}
