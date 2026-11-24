package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The words of a check state the testing move reads, as Dispatch publishes them in
// check.state_changed (intake.DispatchCheckState).
const (
	stateYes = "yes"
	// reasonNoResult is a check with no result at the head.
	reasonNoResult = "no_result"
	// ciNotCompleted is the case of a check CI proves whose run at the head has not concluded.
	ciNotCompleted = "not_completed"
)

// testingJudgement is what the states the daemon holds at the tested head say of each check of the
// head's set (judgeStates): the checks whose state the move waits for, and each check that does
// not pass with why, as the implementer's task and the architect's notice name it. A judgement
// with neither passes.
type testingJudgement struct {
	// Waiting is each check whose state is not conclusive: one with no state, or with no result
	// while its CI run goes on (ci_case not_completed with the reason no_result).
	Waiting []string
	// Failing is each check with a conclusive state that is not yes, with that state and its
	// reason: k2 no for fail, k3 unknown for pass_and_fail, k4 unknown for read_failed.
	Failing []record.FailingCheck
}

// judgeStates judges the states the daemon holds of the head's checks, and is the one place the
// rule of a passing check is written: a check passes when it reads yes at its head, whichever
// results Dispatch counted for it. Every check of the head's set is judged in the set's order, and
// a state of a check outside the set is no part of it, so a check removed from the Verification
// document holds nothing. A check with no state, and one with no result while its CI run goes on,
// wait: neither is a state the check ends at. Every other state is conclusive, and one that is not
// yes fails: a fail, a pass and a fail whatever its CI run says, and a state Dispatch published
// because a read it needed failed (read_failed), which is a state of the check all the same. false
// is no set held at the head, which judges nothing.
func judgeStates(head record.HeadStates) (testingJudgement, bool) {
	if head.Checks == nil {
		return testingJudgement{}, false
	}
	var judgement testingJudgement
	for _, check := range head.Checks {
		state, held := head.States[check]
		switch {
		case !held:
			judgement.Waiting = append(judgement.Waiting, check)
		case state.State == stateYes:
			continue
		case state.Reason == reasonNoResult && state.CICase == ciNotCompleted:
			judgement.Waiting = append(judgement.Waiting, check)
		default:
			judgement.Failing = append(judgement.Failing, record.FailingCheck{Check: check, State: state.State, Reason: state.Reason})
		}
	}
	return judgement, true
}

// testedHead is where the testing move reads the states: the commit the tester's completion
// reported, the head its test record names, in the repository of the pull request on record. The
// record is the one witness of the head the tester tested: the pull request's head can have moved
// while the tester worked (a merge forward asked of the implementer while the issue stays in
// testing, say), so a completion that reported no commit has no tested head, "", and the move
// reads no state for it (decideTesting). The completion route refuses such a completion
// (TESTED_HEAD_REQUIRED), so only a row the engine was handed directly can name none. Dispatch
// keeps publishing the states at a head the pull request has left while the pull request is open,
// so a result the tester records at the tested head after such a move reaches the move as any
// other state event does.
func testedHead(issue record.Issue, row record.PhaseRow, pr *record.PullRequest) (repo, head string) {
	repo, head = issue.Repo, row.HandoffCommit
	if pr != nil {
		repo = pr.Repo
	}
	return repo, head
}

// decideTesting moves issue out of testing by the states the daemon holds of its checks at the
// tested head, the tester's completion row being row. Nothing of Dispatch is read here: the states
// arrive as check.state_changed events (checkState), and the serial of the tester's test record
// came with its completion (api's handoffRecord).
//
// A completion whose test record named no head (testedHead's "") holds the move and reads no
// state, whatever any head holds: the tester's head is the record's to say, and the tester's
// completing again with a record that names it decides the move then. Otherwise
// it moves nothing until the daemon holds every state event of the issue from the lowest serial it
// holds through the record's (record.Store.StatesFenceReached), since each event the tester's
// results caused has a serial at or below it: below that fence the states held may be the ones from
// before those results. At the fence it decides only once every check of the head's set holds a
// conclusive state there (judgeStates): a head with no set held, or a check the judgement waits
// for, moves nothing, whatever the other checks read, since a later state of the waiting check
// changes what the task must name. Then every check passing moves the issue to reviewing, and
// otherwise it goes back to implementing, the task naming each check that does not pass with its
// state and its reason. Each wait and each move back is one testingMove, which writes the words of
// the implementer's task and the architect's notice and the notice's fields from the same values.
// atCompletion is whether the tester's completion is the fact deciding: only that one tells the
// architect of a wait, in one phase-finished notice that names what the move waits for, so each
// later state event that leaves the move waiting tells nothing more.
func (e *Engine) decideTesting(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, pr *record.PullRequest, atCompletion bool) error {
	repo, head := testedHead(issue, row, pr)
	if head == "" {
		return e.testingWaits(ctx, tx, issue, row, atCompletion, testingMove{WaitsFor: waitsForTestedHead})
	}
	reached, err := e.store.StatesFenceReached(ctx, tx, issue.Key, row.StatesSerial)
	if err != nil {
		return err
	}
	if !reached {
		return e.testingWaits(ctx, tx, issue, row, atCompletion, testingMove{Head: head, WaitsFor: waitsForFence, Serial: row.StatesSerial})
	}
	held, err := e.store.HeadStates(ctx, tx, issue.Key, repo, head)
	if err != nil {
		return err
	}
	judgement, judged := judgeStates(held)
	switch {
	case !judged:
		return e.testingWaits(ctx, tx, issue, row, atCompletion, testingMove{Head: head, WaitsFor: waitsForSet})
	case len(judgement.Waiting) > 0:
		return e.testingWaits(ctx, tx, issue, row, atCompletion, testingMove{Head: head, WaitsFor: waitsForChecks, Waiting: judgement.Waiting})
	case len(judgement.Failing) > 0:
		move := testingMove{Head: head, Failing: judgement.Failing}
		return e.transition(ctx, tx, issue, TriggerTesterFailed, "", row, pr, moveWhy{Testing: &move})
	}
	return e.transition(ctx, tx, issue, TriggerTesterPassed, "", row, pr, moveWhy{})
}

// What a testing move that waits waits for, as the tester's notice names it (record.Notice's
// WaitsFor).
const (
	// waitsForTestedHead is the head the tester tested: its test record named none.
	waitsForTestedHead = "tested_head"
	// waitsForFence is Dispatch's events through the serial of the tester's test record, some of
	// which the daemon does not hold yet.
	waitsForFence = "fence"
	// waitsForSet is the head's set of checks, which the daemon does not hold.
	waitsForSet = "set"
	// waitsForChecks is the states of the checks the judgement waits for.
	waitsForChecks = "checks"
)

// testingMove is one wait or one move back of the testing move at the tested head Head
// (decideTesting): a wait names what it waits for (WaitsFor), the fence's serial (Serial) and the
// checks whose states it waits for (Waiting); a move back has no WaitsFor, and names each check
// that does not pass (Failing). Its sentence is the words of the implementer's task and of the
// architect's notice, and its notice carries the sentence and these values, so the words and the
// fields are one value.
type testingMove struct {
	Head     string
	WaitsFor string
	Serial   int64
	Waiting  []string
	Failing  []record.FailingCheck
}

// sentence is the move in words, written from its fields alone.
func (m testingMove) sentence() string {
	switch m.WaitsFor {
	case waitsForTestedHead:
		return "the move waits for a tested head: the tester's test record named none, so no check state can decide it; " +
			"the tester writes a new test handoff naming under head the commit it tested and completes again"
	case waitsForFence:
		return fmt.Sprintf("the move waits for Dispatch's check states through serial %d at %s", m.Serial, m.Head)
	case waitsForSet:
		return "the move waits for Dispatch's states at " + m.Head
	case waitsForChecks:
		return "the move waits for the states of " + strings.Join(m.Waiting, ", ") + " at " + m.Head
	}
	named := make([]string, len(m.Failing))
	for i, check := range m.Failing {
		named[i] = check.String()
	}
	return "these checks do not pass at " + m.Head + ": " + strings.Join(named, "; ")
}

// notice is the tester's phase-finished notice of the move, row being the tester's completion:
// the tester's summary, the move's sentence as its reason, and the move's head, wait and checks as
// its fields.
func (m testingMove) notice(row record.PhaseRow) record.Notice {
	return record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing, Summary: row.Summary,
		Reason: m.sentence(), Head: m.Head, WaitsFor: m.WaitsFor, Waiting: m.Waiting, Failing: m.Failing}
}

// testingWaits is the testing move holding issue in testing for wait. At the tester's completion
// it logs the wait and tells the architect, in wait's phase-finished notice of the tester's; at a
// later state event it does neither, since the architect was told.
func (e *Engine) testingWaits(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, atCompletion bool, wait testingMove) error {
	if !atCompletion {
		return nil
	}
	e.logOnCommit(ctx, "workflow: the testing move waits for check states", "issue", issue.Key, "head", wait.Head,
		"statesSerial", row.StatesSerial, "reason", wait.sentence())
	return e.notice(ctx, tx, issue.Key, wait.notice(row))
}

// checkState holds one of Dispatch's check.state_changed events of an issue the workflow records,
// in the fact's own transaction, so the state, the head's set and the serial commit together with
// the event's record as processed, and never apart from it: the head's set of checks every event
// carries (record.Store.PutCheckSet), the state of the check a check event names
// (PutCheckState), each behind the store's guard that a lower Dispatch sequence changes nothing,
// and the event's serial, which the testing move's fence counts (PutCheckStateSerial). An event of
// an issue the workflow does not record is no state of its work and is held nowhere.
//
// While the issue is in testing and its tester has completed, the event decides the testing move
// again (decideTesting), whatever head it names: an event at another head can still be the one the
// fence waits for. In any other phase, and before the tester's completion, it is held and moves
// nothing, so a fail that arrives while the tester works starts no implementer.
func (e *Engine) checkState(ctx context.Context, tx pgx.Tx, fact intake.DispatchCheckState) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if _, err := e.store.PutCheckSet(ctx, tx, fact.Key, fact.Repo, fact.Head, fact.Checks, fact.Mark.Seq); err != nil {
		return intake.Result{}, err
	}
	if fact.Check != "" {
		if _, err := e.store.PutCheckState(ctx, tx, record.CheckState{Issue: fact.Key, Repo: fact.Repo, Head: fact.Head, Check: fact.Check,
			State: fact.State, Reason: fact.Reason, CICase: fact.CICase, Roles: fact.Roles, Seq: fact.Mark.Seq}); err != nil {
			return intake.Result{}, err
		}
	}
	if err := e.store.PutCheckStateSerial(ctx, tx, fact.Key, fact.Serial); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: a check state is held", "issue", fact.Key, "check", fact.Check, "repo", fact.Repo, "head", fact.Head,
		"state", fact.State, "reason", fact.Reason)
	if issue.Phase != phase.Testing {
		return intake.Result{}, nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleTester)
	if err != nil || !row.Completed() {
		return intake.Result{}, err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.decideTesting(ctx, tx, *issue, row, pr, false)
}
