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
// head's set that the judgement reads (judgeStates): the checks whose state the move waits for, and
// each check that does not pass with why, as the implementer's task and the architect's notice name
// it. A judgement with neither passes.
type testingJudgement struct {
	// Waiting is each check whose state is not conclusive: one with no state, or, reading every
	// check, one with no result while its CI run goes on (ci_case not_completed with the reason
	// no_result).
	Waiting []string
	// Failing is each check with a conclusive state that is not yes, with that state and its
	// reason: k2 no for fail, k3 unknown for pass_and_fail, k4 unknown for read_failed.
	Failing []record.FailingCheck
}

// judgeStates judges the states the daemon holds of the head's checks, and is the one place the
// rule of a passing check is written: a check passes when it reads yes at its head, whichever
// results Dispatch counted for it. Every check of the head's set is judged in the set's order, and
// a state of a check outside the set is no part of it, so a check removed from the Verification
// document holds nothing. A check with no state waits: it has no state it ends at, and it is not
// known yet whether a CI job runs it, since Dispatch tells that only in a state, by naming the
// job's case (ci_case) there.
//
// leaveCI leaves out each check a CI job runs, the one whose held state names its job's case,
// whatever that state is. That is the testing move once the end of a task holds its CI checks: a
// failure of such a check reaches the implementer at once by a message of its own (tellCIFailure),
// and the review, the acceptance and the end of the task read it. The testing move passes
// Config.TestingLeavesCI, which the daemon wires false until the change that builds that hold at
// the end of a task turns it on, since a task whose steps after testing hold no review and no
// acceptance would otherwise end with no gate on its CI checks. Reading every check, one with no result while
// its CI run goes on waits as well, since that is no state the check ends at, and a fail or a pass
// and a fail is a fail whatever its CI run says.
//
// Every other state of a check judged is conclusive, and one that is not yes fails: a fail, a pass
// and a fail, and a state Dispatch published because a read it needed failed (read_failed), which
// is a state of the check all the same. false is no set held at the head, which judges nothing.
func judgeStates(head record.HeadStates, leaveCI bool) (testingJudgement, bool) {
	if head.Checks == nil {
		return testingJudgement{}, false
	}
	var judgement testingJudgement
	for _, check := range head.Checks {
		state, held := head.States[check]
		switch {
		case !held:
			judgement.Waiting = append(judgement.Waiting, check)
		case leaveCI && state.CICase != "":
			continue
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
// before those results. At the fence it decides only once every check it reads holds a conclusive
// state there (judgeStates with Config.TestingLeavesCI: every check, those a CI job runs among
// them, as the daemon wires it, until the end of a task holds the checks a CI job runs; only the
// checks that no CI job runs, once the change that builds that hold turns it on): a head with no
// set held, or a check the judgement waits for, a check with no state among them, moves nothing,
// whatever the other checks read, since a later state of the waiting check changes what the task
// must name. Then every check it reads passing moves the issue on to the task's next step, and the
// step the completion named for its task to return to (row.ReturnTo) is not used, which the log
// says once, when the pass moves the issue out of testing. Otherwise the work goes back, the task
// naming each check it reads that does not pass with its state and its reason: to the step the
// completion named (namedReturn), or, where it named none, by the walk's default, the first
// implementation step before the tester (transition's returnOf), implementing for a run with no
// plan rows. Each wait and each move back is one testingMove, which writes the words of the task
// of the step the work goes back to and the architect's notice and the notice's fields from the
// same values. atCompletion is whether the tester's completion is the fact deciding: only that one
// tells the architect of a wait, in one phase-finished notice that names what the move waits for,
// so each later state event that leaves the move waiting tells nothing more.
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
	judgement, judged := judgeStates(held, e.cfg.TestingLeavesCI)
	switch {
	case !judged:
		return e.testingWaits(ctx, tx, issue, row, atCompletion, testingMove{Head: head, WaitsFor: waitsForSet})
	case len(judgement.Waiting) > 0:
		return e.testingWaits(ctx, tx, issue, row, atCompletion, testingMove{Head: head, WaitsFor: waitsForChecks, Waiting: judgement.Waiting})
	case len(judgement.Failing) > 0:
		target, err := e.namedReturn(ctx, tx, issue, row)
		if err != nil {
			return err
		}
		move := testingMove{Head: head, Failing: judgement.Failing}
		return e.transition(ctx, tx, issue, TriggerTesterFailed, target, row, pr, moveWhy{Testing: &move})
	}
	if err := e.transition(ctx, tx, issue, TriggerTesterPassed, "", row, pr, moveWhy{}); err != nil || row.ReturnTo <= 0 {
		return err
	}
	// The line that the step named is not used is the pass's move's: it is written once the issue
	// stands out of testing in this transaction, so a pass that moves nothing (a lingering tree, or
	// a line with no testing row) writes none, and a later state event that decides the same pass
	// writes it only with the move it makes. No state event decides a move once the issue has left
	// testing (checkState), so the line is written once.
	moved, err := e.store.Issue(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if moved != nil && moved.Phase != phase.Testing {
		e.logOnCommit(ctx, "workflow: the testing step passed, so the step the tester's completion named is not used",
			"issue", issue.Key, "head", head, "returnTo", row.ReturnTo)
	}
	return nil
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
// that does not pass (Failing). Its sentence is the words of the task of the step the work goes
// back to and of the architect's notice, and its notice carries the sentence and these values, so
// the words and the fields are one value.
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

// namedReturn is the phase of the step the tester's completion, row, named for its task to
// return to (row.ReturnTo), which a fail of the check states returns the work to, and "" where
// it named none, which leaves the walk's default (transition's returnOf). The completion's step
// was judged against the task's steps when it was taken (refusedReturn). Should a later version
// of the plan have changed those steps while the move waited, a step they no longer take is
// logged and left, and the default holds.
func (e *Engine) namedReturn(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow) (phase.Phase, error) {
	if row.ReturnTo <= 0 {
		return "", nil
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return "", err
	}
	target, why := returnStep(issue, w, issue.Phase, row.ReturnTo)
	if why != "" {
		e.logOnCommit(ctx, "workflow: the step the tester's completion named is no step its task can go back to now, so the walk's default takes the fail",
			"issue", issue.Key, "returnTo", row.ReturnTo, "why", why)
		return "", nil
	}
	return target, nil
}

// testingWaits is the testing move holding issue in testing for wait. At the tester's completion
// it tells the architect, in wait's phase-finished notice of the tester's, and logs the wait from
// the same move (logTestingMove); at a later state event it does neither, since the architect was
// told.
func (e *Engine) testingWaits(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, atCompletion bool, wait testingMove) error {
	if !atCompletion {
		return nil
	}
	if err := e.notice(ctx, tx, issue.Key, wait.notice(row)); err != nil {
		return err
	}
	e.logTestingMove(ctx, issue.Key, "waits", wait.Head, &wait)
	return nil
}

// logTestingMove logs `workflow: the testing move decided` once the fact commits, from the move
// the agent's objects are written from, where they are written: transition calls it beside
// noticeFor for the tester's moves back and on (TriggerTesterFailed, TriggerTesterPassed), with
// the phase moved to, its head and why.Testing, and testingWaits, at the completion, with waits,
// the wait's head and the move it gives notice. Its attributes are issue, head, leave_ci (the
// engine's TestingLeavesCI), move, and, only where m is set, waits_for, waiting and failing (the
// ids of m's failing checks), so a move with no testingMove logs no check and reads no field of a
// nil m. decideTesting logs nothing itself.
func (e *Engine) logTestingMove(ctx context.Context, issue, move, head string, m *testingMove) {
	args := []any{"issue", issue, "head", head, "leave_ci", e.cfg.TestingLeavesCI, "move", move}
	if m != nil {
		failing := make([]string, 0, len(m.Failing))
		for _, check := range m.Failing {
			failing = append(failing, check.Check)
		}
		args = append(args, "waits_for", m.WaitsFor, "waiting", m.Waiting, "failing", failing)
	}
	e.logOnCommit(ctx, "workflow: the testing move decided", args...)
}

// checkState holds one of Dispatch's check.state_changed events of an issue the workflow records,
// in the fact's own transaction, so the state, the head's set and the serial commit together with
// the event's record as processed, and never apart from it: the head's set of checks every event
// carries (record.Store.PutCheckSet), the state of the check a check event names
// (PutCheckState), each behind the store's guard that a lower Dispatch sequence changes nothing,
// and the event's serial, which the testing move's fence counts (PutCheckStateSerial). An event of
// an issue the workflow does not record is no state of its work and is held nowhere.
//
// A newer state of a check a CI job runs that says its job ended at the head without a pass
// (ciJobFailed), where the state the daemon held there before said no such failure, is told to the
// implementer at once, in the same transaction and in any phase (tellCIFailure): the failure does
// not wait for the tester's or the reviewer's step to end, and it moves nothing.
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
		failed := ciJobFailed(fact.State, fact.CICase)
		var before record.CheckState
		var wasHeld bool
		if failed {
			held, err := e.store.HeadStates(ctx, tx, fact.Key, fact.Repo, fact.Head)
			if err != nil {
				return intake.Result{}, err
			}
			before, wasHeld = held.States[fact.Check]
		}
		state := record.CheckState{Issue: fact.Key, Repo: fact.Repo, Head: fact.Head, Check: fact.Check,
			State: fact.State, Reason: fact.Reason, CICase: fact.CICase, Roles: fact.Roles, Seq: fact.Mark.Seq}
		newer, err := e.store.PutCheckState(ctx, tx, state)
		if err != nil {
			return intake.Result{}, err
		}
		if failed && newer && !(wasHeld && sameFailure(before, state)) {
			if err := e.tellCIFailure(ctx, tx, *issue, state); err != nil {
				return intake.Result{}, err
			}
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

// ciJobNotEnded are the CI cases Dispatch names for a check a CI job runs whose job has not ended
// at the head, or whose end Dispatch could not read (envoy's results_ci.go): its run has not
// concluded, no run of its name is on the head, Dispatch holds no head to read at, GitHub did not
// answer, a result of the job named no repository Dispatch could resolve, or the read had not
// started when its batch's bound ended. Every other case names an end: a conclusion as GitHub wrote
// it, or a result the job recorded of its own.
var ciJobNotEnded = map[string]bool{
	ciNotCompleted: true, "absent": true, "no_head": true, "github_unreadable": true, "read_failed": true, "not_started": true,
}

// ciJobFailed says whether a state of a check is one of a check a CI job runs whose job ended at
// its head without a pass: the state names a CI case that is an end, the case is not a success,
// and the state is not yes. A cancelled or skipped job ended without a pass too.
func ciJobFailed(state, ciCase string) bool {
	return ciCase != "" && !ciJobNotEnded[ciCase] && ciCase != "success" && state != stateYes
}

// sameFailure says whether two states of one check at one head say the same failure: the same
// answer, the same reason and the same case of its job, whatever roles the results came from.
func sameFailure(a, b record.CheckState) bool {
	return a.State == b.State && a.Reason == b.Reason && a.CICase == b.CICase
}

// tellCIFailure tells the implementer of issue that state, a check a CI job runs, fails at its
// head: one message on the implementer's role topic (ciFailureMessage) naming the pull request,
// the check, the head, the job's case and the state, written in the state event's own transaction,
// and once it is queued the line `workflow: the implementer is told of a failed CI job`, written
// from that row. The implementer reads the job's log and decides whose failure it is. Only a state
// at the head of the open pull request on record, in its repository, is told: a head the pull
// request has left is no head the implementer works on. It tells no other role, starts no
// implementer and moves nothing, so the tester or the reviewer that works goes on. Linger holds a
// member of a closed tree where it stood (record.TreeLingers), so nobody is told there, as
// tellChecks tells nobody.
func (e *Engine) tellCIFailure(ctx context.Context, tx pgx.Tx, issue record.Issue, state record.CheckState) error {
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return err
	}
	if pr.State != record.PullRequestOpen || pr.Repo != state.Repo || pr.HeadSHA != state.Head {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	addressed, err := e.addressRoles(issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleImplementer, row: issue.Key}}, nil)
	if err != nil {
		return err
	}
	for _, to := range addressed {
		row := ciFailureMessage(string(to.token), pr.Number, state)
		if err := e.enqueue(ctx, tx, to.issue, row); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: the implementer is told of a failed CI job", "issue", issue.Key, "pull_request", row.PullRequest,
			"check", row.Check, "repo", state.Repo, "head", row.Head, "ci_case", row.CICase)
	}
	return nil
}

// ciFailureMessage is the implementer's message that state fails at the head of pull request
// number, sent to the role holder token: its fields are the pull request, the state's head, check
// and job's case, and its body is written from those fields and the state's answer (its state, and
// its reason where it has one).
func ciFailureMessage(token string, number int, state record.CheckState) record.RoleMessage {
	message := record.RoleMessage{Role: token, PullRequest: number, Head: state.Head, Check: state.Check, CICase: state.CICase}
	answer := state.State
	if state.Reason != "" {
		answer += " (" + state.Reason + ")"
	}
	message.Body = fmt.Sprintf("Pull request #%d: check %s fails at %s, where its CI job ended %s; its state there is %s. Nothing moved on it: read the job's log and decide whose failure it is.",
		message.PullRequest, message.Check, message.Head, message.CICase, answer)
	return message
}
