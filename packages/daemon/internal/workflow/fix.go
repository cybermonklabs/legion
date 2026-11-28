package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A fix is the first implementation step of a task started again beside the task's other steps,
// because a check that a CI job runs failed at the task's head (record.TaskFix). The fix rule
// (fixRule) is the one place that decides what a failure of such a check does: it runs at the
// tail of every fact that names an issue (apply), over the check states the daemon holds
// (checkState), so a failure is judged once, in the transaction of the fact that made it known.

// taskHead is where the task of pr's issue stands: the repository and the head of its open pull
// request, which the task's checks are read at, and "" where the issue has no open pull request.
func taskHead(pr *record.PullRequest) (repo, head string) {
	if pr == nil || pr.State != record.PullRequestOpen {
		return "", ""
	}
	return pr.Repo, pr.HeadSHA
}

// failedChecks is each check of head's set that fails there by the rule of a CI check
// (record.CIVerdict's CIFailed), in the set's order, with its state. A set not held fails nothing.
func failedChecks(head record.HeadStates) []record.CheckState {
	var failed []record.CheckState
	for _, check := range head.Checks {
		state, held := head.States[check]
		if record.CIVerdict(state, held) == record.CIFailed {
			failed = append(failed, state)
		}
	}
	return failed
}

// firstImplementation is the place of the first step of w's task whose recorded type is
// implementation (record.PlanStep.Type), which a fix runs again; false for a task with none.
func firstImplementation(w Walk) (int, bool) {
	for i, step := range w.Steps {
		if step.Type == plan.TypeImplementation {
			return i + 1, true
		}
	}
	return 0, false
}

// taskRuns says whether the task of w's run stands running: a task of a stored walk in
// record.TaskRunning, and the task of a run with no plan rows while its issue is in the workflow.
func taskRuns(issue record.Issue, w Walk) bool {
	if w.Stored {
		return w.State == record.TaskRunning
	}
	return InWorkflow(issue)
}

// atTaskEnd says whether issue stands at its task's end: held before its retro for its fix or
// for its CI. A hold for the fix from a step before the end (besideFix) is no end.
func atTaskEnd(issue record.Issue) bool {
	return fixOrCIHold(issue) && issue.Hold.From == phase.Retro
}

// fixPass is what one run of the fix rule read of an issue: its record, its walk, its pull request
// (nil for none), its task's head, the task's open fix (nil for none) and whether its run takes a
// fix.
type fixPass struct {
	issue   record.Issue
	w       Walk
	pr      *record.PullRequest
	head    string
	open    *record.TaskFix
	fixesCI bool
}

// fixRule is the one place a failure of a check that a CI job runs at the task's head is decided,
// applied at the tail of every fact that names the issue key. It takes each failure at the head
// (fixFailures), then reads again the end of a task held before its retro (releaseRetroHold),
// which the fact's failures, its states and its fix's end can all change.
func (e *Engine) fixRule(ctx context.Context, tx pgx.Tx, key string) error {
	if err := e.fixFailures(ctx, tx, key); err != nil {
		return err
	}
	return e.releaseRetroHold(ctx, tx, key)
}

// fixFailures takes the failures at the head of the issue key's task. It returns where the task
// does not run (taskRuns) or has no head (taskHead), and otherwise takes each check that fails at
// the head (failedChecks), in the set's order, to the first of these that applies, by the check's
// failure row at that head (record.CIFailure), which says whom the failure went to:
//
//   - a fix of the task is open (reading its job or running), wherever the task stands: the
//     failure joins that fix, and the implementer gets it as a repair assignment of its fix where
//     the fix runs (assignRepair; a fix still reading its job names it in its task). A failure
//     some fix took already needs nothing. No second fix starts while one is open.
//   - the task stands at its retro or its merge: the failure goes to the merger, which reads
//     GitHub, and nothing else is done.
//   - the task is held at its end (atTaskEnd): where no fix ran for the check at that head and
//     the task takes a fix and has an implementation step, a fix starts (openFix); else its owner
//     is told once (endFailure).
//   - before the task's end, for a task that takes a fix (a stored walk, record.PlanTask.FixesCI),
//     a failure no one was given yet: where the task's implementation step runs, it goes to that
//     step, whose implementer gets it as a repair assignment of that step (assignRepair); where a
//     step after the task's first implementation step runs or waits, a fix starts (openFix);
//     before the first implementation step nothing is written, and that step's start names the
//     failure.
//
// So a failure is assigned once for each check at each head, by the fix's start or by one
// assignment row, whichever of Dispatch's state and the listener's settlement came first: the
// settlement's report assigns nothing (reportRuns).
//
// A task that takes no fix gets nothing of this rule before its end, and its CI settlements are
// told as before (tellChecks).
func (e *Engine) fixFailures(ctx context.Context, tx pgx.Tx, key string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, key)
	if err != nil {
		return err
	}
	repo, head := taskHead(pr)
	if head == "" {
		return nil
	}
	held, err := e.store.HeadStates(ctx, tx, key, repo, head)
	if err != nil {
		return err
	}
	failed := failedChecks(held)
	if len(failed) == 0 {
		return nil
	}
	w, err := e.walk(ctx, tx, *issue)
	if err != nil || !taskRuns(*issue, w) {
		return err
	}
	pass := &fixPass{issue: *issue, w: w, pr: pr, head: head}
	if w.Stored {
		if pass.open, err = e.store.RunningFix(ctx, tx, key, issue.Generation, w.Task); err != nil {
			return err
		}
		if pass.fixesCI, err = e.store.RunFixesCI(ctx, tx, key, issue.Generation); err != nil {
			return err
		}
	}
	for _, state := range failed {
		if err := e.fixFailure(ctx, tx, pass, state); err != nil {
			return err
		}
	}
	return nil
}

// fixFailure takes the failure of state's check at the pass's head to the first branch of the
// fix rule that applies (fixFailures).
func (e *Engine) fixFailure(ctx context.Context, tx pgx.Tx, pass *fixPass, state record.CheckState) error {
	issue, w := pass.issue, pass.w
	row, err := e.store.CIFailureAt(ctx, tx, issue.Key, issue.Generation, w.Task, state.Check, pass.head)
	if err != nil {
		return err
	}
	failure := record.CIFailure{Issue: issue.Key, Generation: issue.Generation, Task: w.Task, Check: state.Check, Head: pass.head, At: e.now()}
	switch {
	case pass.open != nil:
		return e.joinFix(ctx, tx, pass, state, row, failure)
	case record.StandsIn(issue, phase.Retro, phase.Merging) && !atTaskEnd(issue):
		if row != nil {
			return nil
		}
		failure.WentTo = record.CIFailureToMerger
		_, err := e.store.PutCIFailure(ctx, tx, failure)
		return err
	case atTaskEnd(issue):
		return e.endFailure(ctx, tx, pass, state, row, failure)
	case !w.Stored || !pass.fixesCI || row != nil:
		return nil
	}
	first, hasImplementation := firstImplementation(w)
	switch {
	case w.Place.Kind == record.PlaceStep && w.Place.Position >= 1 && w.Place.Position <= len(w.Steps) &&
		w.Steps[w.Place.Position-1].Type == plan.TypeImplementation:
		readers, err := e.ciReaders(ctx, tx, issue, w)
		if err != nil {
			return err
		}
		reader := stepReaderOf(readers, roleRecipient{issue: issue.Key, role: claim.Role(w.Steps[w.Place.Position-1].Role)})
		if reader == nil {
			return nil
		}
		failure.WentTo = record.CIFailureToImplementer
		if _, err := e.store.PutCIFailure(ctx, tx, failure); err != nil {
			return err
		}
		return e.assignRepair(ctx, tx, pass, state, *reader, 0, " Fix it in this step.")
	case hasImplementation && w.Place.Kind == record.PlaceStep && w.Place.Position > first:
		return e.openFix(ctx, tx, pass, state.Check, first, failure, nil)
	}
	return nil
}

// endFailure is the fix rule's branch of a task held at its end (atTaskEnd) with no fix open,
// row being the failure's row at the head, nil for none. A fix runs once for each check at each
// head, and the end is judged by the row's fix and whether its owner was told, never by whether a
// row exists, so a row of the implementer's or the merger's blocks neither: where no fix ran for
// the check at the head (no row, or a row of no fix) and the task takes a fix and has an
// implementation step, a fix starts there (openFix), the row taking its number; else its owner is
// told once, by one ci-failed notice (record.Store.TellOwnerOnce, ciFailedNotice), the row the
// owner's where none was written.
func (e *Engine) endFailure(ctx context.Context, tx pgx.Tx, pass *fixPass, state record.CheckState, row *record.CIFailure, failure record.CIFailure) error {
	issue, w := pass.issue, pass.w
	fixed := row != nil && row.Fix > 0
	if first, hasImplementation := firstImplementation(w); !fixed && w.Stored && pass.fixesCI && hasImplementation {
		return e.openFix(ctx, tx, pass, state.Check, first, failure, row)
	}
	if row == nil {
		failure.WentTo = record.CIFailureToOwner
		if _, err := e.store.PutCIFailure(ctx, tx, failure); err != nil {
			return err
		}
	}
	told, err := e.store.TellOwnerOnce(ctx, tx, issue.Key, issue.Generation, w.Task, state.Check, pass.head)
	if err != nil || !told {
		return err
	}
	e.logOnCommit(ctx, "workflow: a failed CI check at a task's end that no fix takes is told to its owner", "issue", issue.Key, "task", w.Task,
		"check", state.Check, "head", pass.head, "fixEnded", fixed)
	acts, err := e.ciActsOf(ctx, tx, issue, w)
	if err != nil {
		return err
	}
	return e.notice(ctx, tx, issue.Key, ciFailedNotice(issue, w, state.Check, pass.head, fixed, acts))
}

// ciFailedNotice is the ci-failed notice to the owner of issue, on w, the walk of its run, of
// check failing at head where no fix takes it, fixEnded where its one fix ran and ended: what
// failed, and the owner's acts (acts, ciActsOf): the move back, and the stop of its task only
// where that stop is taken.
func ciFailedNotice(issue record.Issue, w Walk, check, head string, fixEnded bool, acts ciActs) record.Notice {
	why := "no fix takes it"
	if fixEnded {
		why += ": its one fix ended"
	}
	return record.Notice{Kind: "ci-failed", Role: claim.RoleArchitect, Phase: record.StandsAt(issue), Reason: fmt.Sprintf(
		"Check %s fails at %s of task %s of %s, and %s. Decide with the person: %s.", check, head, w.Task, issue.Key, why, acts.decide())}
}

// stepEntry is the open entry of role's phases row of issue, one raised that its completion has
// not ended, and 0 where the row holds none: whether role's step runs, and the work it runs.
func (e *Engine) stepEntry(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (int64, error) {
	row, err := e.phaseRow(ctx, tx, issue, role)
	if err != nil || row.Completed() {
		return 0, err
	}
	return row.Entry, nil
}

// joinFix is the fix rule's branch of an open fix of the task, pass.open: a failure that no fix
// took (no row, or a row of the implementer, the merger or the owner) joins it, and the
// implementer gets its repair assignment for the fix where the fix runs (assignRepair). A failure
// some fix took already needs nothing.
func (e *Engine) joinFix(ctx context.Context, tx pgx.Tx, pass *fixPass, state record.CheckState, row *record.CIFailure, failure record.CIFailure) error {
	fix := pass.open
	switch {
	case row != nil && row.Fix > 0:
		return nil
	case row == nil:
		failure.WentTo, failure.Fix = record.CIFailureToFix, fix.Fix
		if _, err := e.store.PutCIFailure(ctx, tx, failure); err != nil {
			return err
		}
	default:
		if err := e.store.MarkCIFailureFix(ctx, tx, failure.Issue, failure.Generation, failure.Task, failure.Check, failure.Head, fix.Fix); err != nil {
			return err
		}
	}
	if fix.State != record.FixRunning {
		return nil
	}
	return e.assignRepair(ctx, tx, pass, state, *fixReader(*fix), fix.Fix, " It is part of your fix.")
}

// openFix starts fix of the task of the pass for check at the pass's head, at place, the task's
// first implementation step: the fix reads its failed job first (record.FixReading), the failure
// names it, its row written, or row, the one there already, given the fix's number
// (record.Store.MarkCIFailureFix), and one read of the job is queued (record.FixJobRead), whose
// answer starts the fix's step. A task held at its end for its CI is held for its fix from then
// on, from retro still. Each other check that fails at the head in the same pass joins it
// (joinFix), since the pass now holds it open. A merge forward of the head the implementer owes
// does not delay it: the fix takes that merge as its own first work (takeOwedMerge). A fix whose own
// branch cannot be named (claim.BesideBranch) opens not at all: the owner is told once instead
// (refuseFixName), and every other fact of the issue goes on.
func (e *Engine) openFix(ctx context.Context, tx pgx.Tx, pass *fixPass, check string, place int, failure record.CIFailure, row *record.CIFailure) error {
	issue, w := pass.issue, pass.w
	latest, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil {
		return err
	}
	// The last fix ended while a step beside it runs, and the way after it waits for that step's
	// end (afterFix). A fix opened now could not take that step's commits, which stay on its
	// branch beside the last fix until its run ends, so the failure waits with no row written: the
	// fix rule reads it again at the fact that takes the way.
	if latest != nil && latest.State == record.FixEnded && latest.Way == 0 {
		e.logOnCommit(ctx, "workflow: a failed CI check waits for the way after the last fix", "issue", issue.Key, "task", w.Task,
			"fix", latest.Fix, "check", check, "head", pass.head)
		return nil
	}
	number := 1
	if latest != nil {
		number = latest.Fix + 1
	}
	_, err = claim.BesideBranch(issue.Key, issue.Generation, w.Task, number, claim.RoleImplementer)
	if err != nil {
		return e.refuseFixName(ctx, tx, pass, check, failure, row, err)
	}
	fix := record.TaskFix{Issue: issue.Key, Generation: issue.Generation, Task: w.Task, Fix: number, State: record.FixReading,
		Check: check, Head: pass.head, Place: place}
	if err := e.store.PutFix(ctx, tx, fix); err != nil {
		return err
	}
	if err := e.takeOwedMerge(ctx, tx, pass, number); err != nil {
		return err
	}
	if row != nil {
		if err := e.store.MarkCIFailureFix(ctx, tx, issue.Key, issue.Generation, w.Task, check, pass.head, number); err != nil {
			return err
		}
	} else {
		failure.WentTo, failure.Fix = record.CIFailureToFix, number
		if _, err := e.store.PutCIFailure(ctx, tx, failure); err != nil {
			return err
		}
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.FixJobRead{Generation: issue.Generation, Task: w.Task, Fix: number, Check: check, Head: pass.head}); err != nil {
		return err
	}
	if record.HeldFor(issue, record.HoldForCI) {
		hold := *issue.Hold
		hold.Reason = record.HoldForFix
		issue.Hold = &hold
		if err := e.WriteIssue(ctx, tx, issue); err != nil {
			return err
		}
		pass.issue = issue
	}
	pass.open = &fix
	e.logOnCommit(ctx, "workflow: a failed CI check starts a fix beside the task's steps", "issue", issue.Key, "task", w.Task,
		"fix", number, "check", check, "head", pass.head, "place", place, "phase", issue.Phase)
	return nil
}

// ciFailureSentence is the sentence that names a failure of state's check at head: the check, the
// head and how its CI job ended (its ci_case). The daemon holds no name of the job before a fix
// reads it, so the job is the check's.
func ciFailureSentence(state record.CheckState, head string) string {
	return fmt.Sprintf("Check %s fails at %s: CI job of the check ended %s.", state.Check, head, state.CICase)
}

// assignRepair gives the implementer its one repair assignment of the failure of state's check at
// the pass's head (record.CINewsAssignment), for reader, the implementer's current work: the fix
// it runs, fix being that fix's number, or its own implementation step, fix 0. The assignment's
// sentence names the failure (ciFailureSentence) and ends with act. It is the fix rule's one way
// to assign a repair besides the fix's own start task, so a failure is assigned once for each
// check at each head, whichever of Dispatch's state and the listener's settlement came first.
func (e *Engine) assignRepair(ctx context.Context, tx pgx.Tx, pass *fixPass, state record.CheckState, reader record.CIReader, fix int, act string) error {
	issue := pass.issue
	addressed, err := e.addressRoles(issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleImplementer, row: issue.Key}}, nil)
	if err != nil {
		return err
	}
	for _, to := range addressed {
		if err := e.enqueue(ctx, tx, to.issue, record.CINews{Purpose: record.CINewsAssignment, Role: string(to.token), Generation: issue.Generation,
			Task: pass.w.Task, Repo: pass.pr.Repo, PullRequest: pass.pr.Number, Head: pass.head, Reader: &reader,
			Assigned: &record.CIAssigned{Check: state.Check, Head: pass.head, CICase: state.CICase, Fix: fix},
			Body:     ciFailureSentence(state, pass.head) + act}); err != nil {
			return err
		}
	}
	return nil
}

// fixRunsAt says whether the run at place is the run of the step the running fix of issue's task
// runs again (Issue.RunningFix): that run stands beside the issue's own step while the fix runs,
// and the fix's end ends it (endFix). A fix still reading its job has started no run.
func (e *Engine) fixRunsAt(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place record.StepPlace) (bool, error) {
	if issue.RunningFix == 0 || !w.Stored || place.Kind != record.PlaceStep || place.Task != w.Task {
		return false, nil
	}
	fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil {
		return false, err
	}
	return fix.State == record.FixRunning && fix.Place == place.Place, nil
}
