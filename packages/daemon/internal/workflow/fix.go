package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/workspace"
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
	case pass.open != nil && !e.noFixJoin:
		return e.joinFix(ctx, tx, pass, state, row, failure)
	case record.StandsIn(issue, phase.Retro, phase.Merging) && !atTaskEnd(issue):
		if first, ok := firstImplementation(w); e.fixInMerging && ok && w.Stored && pass.fixesCI && row == nil && record.StandsIn(issue, phase.Merging) {
			return e.openFix(ctx, tx, pass, state.Check, first, failure, nil)
		}
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
		entry, err := e.stepEntry(ctx, tx, issue.Key, claim.Role(w.Steps[w.Place.Position-1].Role))
		if err != nil || entry == 0 {
			return err
		}
		failure.WentTo = record.CIFailureToImplementer
		if _, err := e.store.PutCIFailure(ctx, tx, failure); err != nil {
			return err
		}
		reader := record.CIReader{Phase: w.steps()[w.Place.Position-1], Entry: entry}
		return e.assignRepair(ctx, tx, pass, state, reader, 0, " Fix it in this step.")
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
	return e.assignRepair(ctx, tx, pass, state, record.CIReader{Fix: fix.Fix, Entry: fix.Entry}, fix.Fix, " It is part of your fix.")
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
	if e.waitForOwedMerge && owedMerge(pass.pr, pass.head) != nil {
		e.logOnCommit(ctx, "workflow: a failed CI check waits for the merge forward its implementer owes", "issue", issue.Key, "task", w.Task,
			"check", check, "head", pass.head)
		return nil
	}
	latest, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil {
		return err
	}
	// The last fix ended while a step beside it runs, and the way after it waits for that step's
	// end (afterFix). A fix opened now could not take that step's commits, which stay on its
	// branch beside the last fix until its run ends, so the failure waits with no row written: the
	// fix rule reads it again at the fact that takes the way.
	if latest != nil && latest.State == record.FixEnded && latest.Way == 0 && !e.openBesidePendingWay {
		e.logOnCommit(ctx, "workflow: a failed CI check waits for the way after the last fix", "issue", issue.Key, "task", w.Task,
			"fix", latest.Fix, "check", check, "head", pass.head)
		return nil
	}
	number := 1
	if latest != nil {
		number = latest.Fix + 1
	}
	_, err = claim.BesideBranch(issue.Key, issue.Generation, w.Task, number, claim.RoleImplementer)
	if err == nil && e.failFixName {
		err = fmt.Errorf("the seam failFixName names no branch beside fix %d", number)
	}
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

// fixJobRead applies the answer of the read of a fix's failed job (record.FixJobRead): the fix
// keeps the job the read found, or the read's error where it found none, and starts (startFix). A
// fact whose fix no longer reads its job — it ended, or the issue left the run it was of — changes
// nothing, which the log says.
func (e *Engine) fixJobRead(ctx context.Context, tx pgx.Tx, fact intake.FixJobRead) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	fix, err := e.store.RunningFix(ctx, tx, fact.Key, fact.Generation, fact.Task)
	if err != nil {
		return intake.Result{}, err
	}
	if issue.Generation != fact.Generation || fix == nil || fix.Fix != fact.Fix || fix.State != record.FixReading {
		e.logOnCommit(ctx, "workflow: the read of a fix's job changes nothing: the fix no longer reads its job", "issue", fact.Key,
			"generation", fact.Generation, "task", fact.Task, "fix", fact.Fix)
		return intake.Result{}, nil
	}
	fix.JobName, fix.JobURL, fix.ReadFailed = fact.Name, fact.URL, fact.ReadFailed
	if err := e.store.UpdateFix(ctx, tx, *fix); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.startFix(ctx, tx, *issue, *fix)
}

// startFix starts fix, whose job's read has answered, in one transaction: the implementer's row
// raises an entry at the fix's place with the fix's task (enterAt), whose start writes the start
// of a new run of that step naming the fix's check; the fix runs at that entry, so the issue
// record names it (Issue.RunningFix) and the fences of a start and of a delivery hold its task
// (record.PhaseHolds); and its start is queued for the phase of its step, beside the line of
// starts that wait for a slot, with its working copy made at the fix's head, or at the head of the
// fix's own branch where origin holds it (started, fixBranchOf). The issue stays where it stands,
// and each other step of the task that was handed work in this run is told of the fix
// (tellStepsOfFix).
func (e *Engine) startFix(ctx context.Context, tx pgx.Tx, issue record.Issue, fix record.TaskFix) error {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	if !w.Stored || fix.Place < 1 || fix.Place > len(w.Steps) {
		return fmt.Errorf("start fix %d of task %s of %s: its place %d is no step of the task", fix.Fix, fix.Task, issue.Key, fix.Place)
	}
	at := w.steps()[fix.Place-1]
	role := RoleFor(at)
	task, err := e.fixTask(ctx, tx, issue, w, fix, at)
	if err != nil {
		return err
	}
	entry, sent, err := e.enterAt(ctx, tx, issue, &w, role, at, task, false, false, fix.Check)
	if err != nil {
		return err
	}
	now := e.now()
	fix.State, fix.Entry, fix.StartedAt = record.FixRunning, entry, &now
	if err := e.store.UpdateFix(ctx, tx, fix); err != nil {
		return err
	}
	fixing := issue
	fixing.Phase, fixing.Hold = at, nil
	marks := startMarks{stored: true, fix: fix.Fix, fixFrom: fix.Head, fixBranch: e.fixBranchOf(issue, fix)}
	if err := e.started(ctx, tx, fixing, role, sent, entry, marks); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a fix starts beside the task's steps", "issue", issue.Key, "task", fix.Task, "fix", fix.Fix,
		"check", fix.Check, "head", fix.Head, "job", fix.JobName, "readFailed", fix.ReadFailed, "phase", issue.Phase)
	return e.tellStepsOfFix(ctx, tx, issue, w, fix)
}

// fixTask is the task of fix, a run of the step that runs as at: the task a start of that step
// is told (task), in that step's phase, its working copy line naming the fix's own working copy
// (fixDirOf), and right after it the sentences of the fix, each built from the start's typed facts
// (startFacts): what it fixes (fixSentence), each other check it holds, the merge forward its
// implementer owes, which the fix makes first (owedMergeSentence), and how it pushes
// (fixPushSentence).
func (e *Engine) fixTask(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix, at phase.Phase) (string, error) {
	fixing := issue
	fixing.Phase, fixing.Hold = at, nil
	rows, err := e.store.Phases(ctx, tx, issue.Key)
	if err != nil {
		return "", err
	}
	facts, err := e.startFacts(ctx, tx, fixing, w)
	if err != nil {
		return "", err
	}
	if !facts.Own || facts.Fix != fix.Fix {
		return "", fmt.Errorf("compose the task of fix %d of task %s of %s: the start at its place stands beside fix %d", fix.Fix, fix.Task, issue.Key, facts.Fix)
	}
	dirOf := func(record.Issue) string { return facts.FixDir }
	standing, err := standingText(fixing, dirOf, w, rows)
	if err != nil {
		return "", err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return "", err
	}
	copyLine := WorkingCopy(fixing, dirOf).lines
	sentences := fixSentence(issue.Key, facts)
	for _, check := range facts.Failed {
		sentences += fmt.Sprintf(" Check %s failed at %s too: CI job of the check ended %s.", check, facts.Head, facts.Cases[check])
	}
	sentences += fixReports(w, rows, fix) + owedMergeSentence(facts) + fixPushSentence(issue, fix, facts.Branch, facts.FixDir)
	standing.lines = copyLine + sentences + strings.TrimPrefix(standing.lines, copyLine)
	return task(fixing, standing, record.PhaseRow{}, pr, ""), nil
}

// fixReports is what the task of fix says of a step that failed to the fix's own step while the
// fix read its job (besideFix's GotReport): the report of each step of w after the fix's place
// whose row is completed and names a handoff record, in the words the row of a report sent to a
// running fix carries (reportToFix). Such a step's report reaches the fix no other way: its row
// is queued for the entry of a fix that has none until its start raises one, and the standing
// text names only the steps before the fix's place. "" where the fix got no report.
func fixReports(w Walk, rows []record.PhaseRow, fix record.TaskFix) string {
	if !fix.GotReport {
		return ""
	}
	sentences := ""
	for i, at := range w.steps() {
		role := RoleFor(at)
		if i+1 <= fix.Place || role == "" {
			continue
		}
		row := record.PhaseRowOf(rows, role)
		if row.HandoffRecord == "" {
			continue
		}
		sentences += fmt.Sprintf(" The %s of step %d reports: %s. Its report is %s. It is part of your fix.", role, i+1, row.Summary, row.HandoffRecord)
	}
	return sentences
}

// owedMergeSentence is what a fix's task says of the merge forward its implementer owes
// (startFacts' MergeHead and MergeBase): the fix makes it first, in its own working copy, and
// pushes it with its one end push; "" where none is owed.
func owedMergeSentence(facts startFacts) string {
	if facts.MergeHead == "" {
		return ""
	}
	return fmt.Sprintf(" Legion already asked you to merge %s into the task's branch at %s. Make that merge your first act in this fix's copy. "+
		"Include it in your fix's one end push, and push nothing from the task's shared copy.", facts.MergeBase, facts.MergeHead)
}

// fixPushSentence is how fix pushes, which its task says after the failures it holds: from its own
// working copy at dir, its work in progress only to its own branch own (F), never to the task's
// branch; once, when it ends, to the task's branch, then it completes. Before that push it takes the
// commits each step beside it pushed to a branch of its own, and the head of the task's branch
// where it moved (workspace.BesideTakeCommands): a rebase of its own commits onto them, which
// rewrites none of theirs and which a run again leaves as it stands, so the commit under its working
// copy, `@-`, which the push and the completion read, holds each of them once. A commit a step names
// by its id and could not push is copied under the working copy (`--insert-before @`), so it becomes
// `@-`; a copy `--onto @-` would be a sibling of `@`. A fix whose own branch cannot be named, which
// the fix rule refuses to open (refuseFixName), pushes to the task's branch alone.
func fixPushSentence(issue record.Issue, fix record.TaskFix, own, dir string) string {
	if dir == "" {
		dir = "."
	}
	taskBranch := claim.IssueBranch(issue.Key)
	prefix, err := claim.BesideFixPrefix(issue.Key, issue.Generation, fix.Task, fix.Fix)
	if own == "" || err != nil {
		return " Push only from this working copy, to the branch of the task, once, when your fix ends, then complete; before you push, fetch the branch and rebase your commits onto its head where it moved. " +
			"A commit a step beside you names in its report is in this clone by its id: copy it onto your head with jj duplicate <commit> --insert-before @, and never rebase or rewrite it."
	}
	return fmt.Sprintf(" Push only from this working copy. Until your fix ends, push your commits only to your own branch %s, never to the branch of the task: run %s, then %s. "+
		"When your fix ends, push it to the branch of the task once, then complete. "+
		"Before that push, take the commits of the steps beside you and the head of the branch of the task where it moved: %s. "+
		"That puts their commits under yours as they are and rebases only yours, and a run of it again changes nothing; never rebase or rewrite one of their commits, since another working copy may stand on it. "+
		"A commit that a step names by its id and could not push is in this clone by that id: copy it under your working copy once, with jj duplicate <commit> --insert-before @.",
		own, shellprefix.Command([]string{"jj", "-R", dir, "bookmark", "set", own, "-r", "@-"}),
		shellprefix.Command([]string{"jj", "-R", dir, "git", "push", "--remote", "origin", "--bookmark", own}),
		strings.Join(workspace.BesideTakeCommands(dir, prefix, own, taskBranch), ", then "))
}

// startFacts is what a start of a step of issue's task, standing in issue.Phase on w, is told of a
// fix and of the task's CI, as typed values; each sentence such a start's task says of them is
// built from these alone (fixTask, fixLines):
//
//   - the start of a fix (Own), the step at the place of the task's open fix: its number, its
//     check, the head it failed at with its job's case, its job, the job's page or the read's
//     failure, each other check it holds that fails at that head (Failed, with Cases), and the
//     head and base of the merge forward its implementer owes there, which the fix took as its own
//     first work (owedMerge, takeOwedMerge);
//   - a start of another step while a fix reads its job or runs: the fix's number, check and head,
//     and, for each role but the reviewer, which creates no commit, the step's own branch beside
//     the fix (Branch) and the head the daemon holds of a push of it (Pushed, record.TaskFix.Pushed);
//     a start whose step is bound to a branch already keeps that one (record.StepStart.PushBranch),
//     and so does a resume of a step that still runs after its fix ended with its way pending;
//   - a start of an implementation step of a task that takes a fix, with no fix open: the head and
//     each check a CI job runs that fails there (Failed).
//
// The fix's own start holds its own branch F as Branch, and the directory of its own working copy
// beside the task's (FixDir: workspace.FixDir of the directory the runtime gives the issue's working
// copy, Config.WorkspaceDir, "" where the runtime names none), which its task's working copy line
// and its push commands name. Every other start holds none.
type startFacts struct {
	Fix                                        int
	Own                                        bool
	Check, Head, CICase, Job, Page, ReadFailed string
	Branch, Pushed, FixDir                     string
	// Kept is a start of a step that a way 2 runs again after fix Fix ended, whose branch beside
	// that fix (Branch) the daemon holds a push of (Pushed): the way keeps that branch, and the step
	// takes its commits from it.
	Kept                 bool
	MergeHead, MergeBase string
	Failed               []string
	// Cases is the ci_case of each check of Failed that a fix holds.
	Cases map[string]string
}

// startFacts is the typed facts of a start of the step of issue's task standing in issue.Phase on w
// (the type's doc).
func (e *Engine) startFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (startFacts, error) {
	place, ok := w.PlaceOf(issue.Phase)
	if !w.Stored || !ok || place.Kind != record.PlaceStep || place.Position > len(w.Steps) {
		return startFacts{}, nil
	}
	open, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil {
		return startFacts{}, err
	}
	if open != nil && open.Place != place.Position {
		return e.besideFacts(ctx, tx, issue, w, place.Position, *open)
	}
	if open == nil {
		if facts, bound, err := e.boundFacts(ctx, tx, issue, w, place.Position); err != nil || bound {
			return facts, err
		}
		if facts, kept, err := e.keptFacts(ctx, tx, issue, w, place.Position); err != nil || kept {
			return facts, err
		}
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return startFacts{}, err
	}
	if open != nil {
		return e.fixFacts(ctx, tx, issue, w, *open, pr)
	}
	if w.Steps[place.Position-1].Type != plan.TypeImplementation {
		return startFacts{}, nil
	}
	fixesCI, err := e.store.RunFixesCI(ctx, tx, issue.Key, issue.Generation)
	if err != nil || !fixesCI {
		return startFacts{}, err
	}
	repo, head := taskHead(pr)
	if head == "" {
		return startFacts{}, nil
	}
	held, err := e.store.HeadStates(ctx, tx, issue.Key, repo, head)
	if err != nil {
		return startFacts{}, err
	}
	facts := startFacts{Head: head}
	for _, state := range failedChecks(held) {
		facts.Failed = append(facts.Failed, state.Check)
	}
	return facts, nil
}

// besideFacts is the start facts of a step at place, a step of w's task of issue other than the
// fix's own, while open, a fix of the task, reads its job or runs (startFacts): the fix's number,
// check and head, and, for each role but the reviewer, the step's branch: the one its open start
// is bound to already (record.StepStart.PushBranch), which a later fix's start keeps, else its own
// branch beside open, which its start binds (bindBesideFix); and the head the daemon holds of a
// push of that branch, read from the fix the branch is of. The role is the role of the step's phase,
// a phase of a step of the plan, which admitted never is.
func (e *Engine) besideFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place int, open record.TaskFix) (startFacts, error) {
	facts := startFacts{Fix: open.Fix, Check: open.Check, Head: open.Head}
	role := RoleFor(w.steps()[place-1])
	if role == claim.RoleReviewer || role == "" {
		return facts, nil
	}
	bound, pushed, err := e.boundBranch(ctx, tx, issue, w, place)
	if err != nil {
		return startFacts{}, err
	}
	if bound != "" {
		facts.Branch, facts.Pushed = bound, pushed
		return facts, nil
	}
	if branch, ok := e.besideBranchOf(issue, w.Task, open.Fix, role); ok {
		facts.Branch, facts.Pushed = branch, open.Pushed[string(role)]
	}
	return facts, nil
}

// keptFacts is the start facts of a step at place of w's task of issue that a way 2 runs again
// after the task's latest fix ended (startFacts, while no fix of the task is open): where the
// daemon holds a push of the step's own branch beside that fix (record.TaskFix.Pushed), the way kept
// that branch (deletesAtWay), and the start takes its commits from it (Kept). The fix's Way, which
// afterFix writes before the way's start, is the record that the way ran, so no step start's moment
// is read: no step of the task runs when the way is taken (taskStepRuns), so every start and resume
// of a step after the fix's place from then on comes after the fix's end. The way's own start is
// among them, and its task is composed before that start is counted (start, stepStanding), when the
// step's latest start is still its start from before the fix ended. The role is the role of the
// step's phase, a phase of a step of the plan, which admitted never is; the implementer's branch
// beside the fix is the fix's own, which its end push took whole. false for every other start.
func (e *Engine) keptFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place int) (startFacts, bool, error) {
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.Way != wayAfterFix || place <= fix.Place {
		return startFacts{}, false, err
	}
	role := RoleFor(w.steps()[place-1])
	pushed := fix.Pushed[string(role)]
	if role == claim.RoleImplementer || pushed == "" {
		return startFacts{}, false, nil
	}
	branch, ok := e.besideBranchOf(issue, w.Task, fix.Fix, role)
	if !ok {
		return startFacts{}, false, nil
	}
	return startFacts{Fix: fix.Fix, Check: fix.Check, Head: fix.EndHead, Branch: branch, Pushed: pushed, Kept: true}, true, nil
}

// boundFacts is the start facts of a step at place of w's task of issue whose open start is bound
// to a branch beside a fix that no longer reads its job or runs (startFacts, while no fix of the task
// is open): a resume of a step that still runs after its fix ended with its way pending, told of
// that fix and of its branch until its step ends. false where the start is bound to none.
func (e *Engine) boundFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place int) (startFacts, bool, error) {
	bound, pushed, err := e.boundBranch(ctx, tx, issue, w, place)
	if err != nil || bound == "" {
		return startFacts{}, false, err
	}
	_, _, _, number, _, _ := claim.MatchBesideBranch(bound, []string{w.Task})
	fix, err := e.store.FixAt(ctx, tx, issue.Key, issue.Generation, w.Task, number)
	if err != nil || fix == nil {
		return startFacts{}, false, err
	}
	return startFacts{Fix: fix.Fix, Check: fix.Check, Head: fix.Head, Branch: bound, Pushed: pushed}, true, nil
}

// boundBranch is the branch beside a fix that the open start of the step at place of w's task of
// issue is bound to (record.StepStart.PushBranch), "" for none, and the head the daemon holds of a
// push of it, read from the fix the branch names (record.Store.FixAt, record.TaskFix.Pushed), ""
// for none.
func (e *Engine) boundBranch(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place int) (branch, pushed string, err error) {
	start, err := e.store.StepStarts(ctx, tx, issue.Key, record.StepPlace{Task: w.Task, Kind: record.PlaceStep, Place: place})
	if err != nil || start.Run != record.RunOpen || start.PushBranch == "" {
		return "", "", err
	}
	_, _, _, number, role, ok := claim.MatchBesideBranch(start.PushBranch, []string{w.Task})
	if !ok {
		return start.PushBranch, "", nil
	}
	fix, err := e.store.FixAt(ctx, tx, issue.Key, issue.Generation, w.Task, number)
	if err != nil || fix == nil {
		return start.PushBranch, "", err
	}
	return start.PushBranch, fix.Pushed[string(role)], nil
}

// fixFacts is the start facts of fix's own start (startFacts), pr being issue's pull request on
// record.
func (e *Engine) fixFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix, pr *record.PullRequest) (startFacts, error) {
	held, err := e.store.HeadStates(ctx, tx, issue.Key, prRepo(issue, pr), fix.Head)
	if err != nil {
		return startFacts{}, err
	}
	facts := startFacts{Fix: fix.Fix, Own: true, Check: fix.Check, Head: fix.Head, CICase: held.States[fix.Check].CICase,
		Job: fix.JobName, Page: fix.JobURL, ReadFailed: fix.ReadFailed}
	facts.Branch, _ = e.besideBranchOf(issue, fix.Task, fix.Fix, claim.RoleImplementer)
	if dir := fixDirOf(e.cfg.WorkspaceDir); dir != nil {
		facts.FixDir = dir(issue)
	}
	for _, state := range failedChecks(held) {
		if state.Check == fix.Check {
			continue
		}
		row, err := e.store.CIFailureAt(ctx, tx, issue.Key, issue.Generation, fix.Task, state.Check, fix.Head)
		if err != nil {
			return startFacts{}, err
		}
		if row != nil && row.Fix == fix.Fix {
			if facts.Cases == nil {
				facts.Cases = map[string]string{}
			}
			facts.Failed, facts.Cases[state.Check] = append(facts.Failed, state.Check), state.CICase
		}
	}
	ours := record.MergeForwardFix{Generation: issue.Generation, Task: w.Task, Number: fix.Fix}
	if owed := owedMerge(pr, fix.Head); owed != nil && !e.mergeOutsideFix && (owed.Fix == nil || *owed.Fix == ours) {
		facts.MergeHead, facts.MergeBase = owed.Head, owed.Base
	}
	return facts, nil
}

// fixLines is what the task of a start of a step of issue's task, standing in issue.Phase on w,
// says of a fix and of the task's CI (startFacts): beside a fix that reads its job or runs, that
// the failure is the fix's and where the step pushes until it ends (besideStepSentence); and, for
// an implementation step of a task that takes a fix, each check that a CI job runs and that fails
// at the task's head, each of which goes to that step (record.CIFailureToImplementer), so the fix
// rule tells its implementer of it no second time. Every other start says nothing more, the fix's
// own among them, whose task is the fix's (fixTask).
func (e *Engine) fixLines(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (string, error) {
	facts, err := e.startFacts(ctx, tx, issue, w)
	switch {
	case err != nil || facts.Own:
		return "", err
	case facts.Kept:
		return keptSentence(facts, claim.IssueBranch(issue.Key)), nil
	case facts.Fix > 0:
		return besideStepSentence(facts), nil
	case len(facts.Failed) == 0:
		return "", nil
	}
	for _, check := range facts.Failed {
		failure := record.CIFailure{Issue: issue.Key, Generation: issue.Generation, Task: w.Task, Check: check, Head: facts.Head,
			WentTo: record.CIFailureToImplementer, At: e.now()}
		if _, err := e.store.PutCIFailure(ctx, tx, failure); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf(" Failed CI checks at %s: %s.", facts.Head, strings.Join(facts.Failed, ", ")), nil
}

// prRepo is the repository of issue's pull request on record, or the issue's own where it has none.
func prRepo(issue record.Issue, pr *record.PullRequest) string {
	if pr != nil {
		return pr.Repo
	}
	return issue.Repo
}

// fixDirOf is dir, the directory the runtime gives an issue's working copy, as the directory of a
// fix's own working copy beside it (workspace.FixDir): nil and "" stay none.
func fixDirOf(dir func(record.Issue) string) func(record.Issue) string {
	if dir == nil {
		return nil
	}
	return func(issue record.Issue) string {
		if at := dir(issue); at != "" {
			return workspace.FixDir(at)
		}
		return ""
	}
}

// fixSentence is the sentence of a fix's task that names what it fixes, from its start facts: its
// check, the head the check failed at, and its CI job as the read found it: its name, how it ended
// (CICase) and its page; its name and how to find its run where Dispatch holds no page; and the
// read's error and how to find the run where the read found no job.
func fixSentence(key string, facts startFacts) string {
	find := fmt.Sprintf("find its run from %s with dispatch results --issue %s", facts.Head, key)
	switch {
	case facts.Job == "" && facts.ReadFailed != "":
		return fmt.Sprintf(" This is a fix: check %s failed at %s (Dispatch could not be read for its job: %s; %s).", facts.Check, facts.Head, facts.ReadFailed, find)
	case facts.Page == "":
		return fmt.Sprintf(" This is a fix: check %s failed at %s (CI job %s; Dispatch holds no address of its page, so %s).", facts.Check, facts.Head,
			jobOf(record.TaskFix{JobName: facts.Job}), find)
	}
	return fmt.Sprintf(" This is a fix: check %s failed at %s (CI job %s ended %s, page %s).", facts.Check, facts.Head, facts.Job, facts.CICase, facts.Page)
}

// jobOf is the name of fix's CI job, or "of the check" where the read found none.
func jobOf(fix record.TaskFix) string {
	if fix.JobName == "" {
		return "of the check"
	}
	return fix.JobName
}

// tellStepsOfFix tells each other step of w's task that runs or was handed work in this run (its
// phases row holds the task of an entry), and the owner where the issue stands in the owner's
// acceptance, of fix, by one fix-started row each (record.CINewsFixStarted, tellFixStarted): the fix
// runs, the failure is the fix's, and, for each role but the reviewer, which creates no commit, that
// until its step ends it pushes only to its own branch beside the fix, which the row names and the
// step's open start is bound to here (toldBranch). Each row is for the step's current work, its
// entry where its step runs, or the owner's acceptance, so the outbox wakes a released reader and
// finishes a row whose work is no longer current undelivered.
//
// A step whose current start named this fix (toldByStart: it started while the fix read its job)
// was told by its own start's task, and the owner by the row of its move into accepting
// (tellOwnerOfFix), so each is told once. A step that has not started is told by its start (task).
// The tester's testing step still judges each check a CI job runs, this one among them, so its
// report can fail for it, and a step that fails beside a fix moves nothing until the way after the
// fix (besideFix); no other step's report fails for it.
//
// Each other step's open start is marked as one the fix runs beside (record.StepStart.BesideFix),
// whether or not its step is told: what ReviewBesideFix reads of a review step that started before
// the fix, never the clock. toldByStart judges the start as it was read before the mark, as its own
// start left it.
func (e *Engine) tellStepsOfFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix) error {
	news, err := e.fixStartedOf(ctx, tx, issue, w, fix)
	if err != nil {
		return err
	}
	var opened time.Time
	if e.toldByClock {
		failure, err := e.store.CIFailureAt(ctx, tx, issue.Key, issue.Generation, w.Task, fix.Check, fix.Head)
		if err != nil {
			return err
		}
		if failure != nil {
			opened = failure.At
		}
	}
	for i, at := range w.steps() {
		role := RoleFor(at)
		if i+1 == fix.Place || role == "" {
			continue
		}
		place := record.StepPlace{Task: w.Task, Kind: record.PlaceStep, Place: i + 1}
		start, err := e.store.StepStarts(ctx, tx, issue.Key, place)
		if err != nil {
			return err
		}
		if start.Run == record.RunOpen && start.BesideFix != fix.Fix {
			if err := e.store.MarkStepBesideFix(ctx, tx, issue.Key, place, start.Starts, fix.Fix); err != nil {
				return err
			}
		}
		to := roleRecipient{issue: issue.Key, role: role, row: issue.Key}
		var reader *record.CIReader
		if role == claim.RoleArchitect {
			// The owner's acceptance is no worker's step: the owner is told while the issue
			// stands in it, the acceptance being its work.
			if record.StandsAt(issue) != at || start.Run != record.RunOpen {
				continue
			}
			if to.issue, err = e.owningArchitect(ctx, tx, issue); err != nil {
				return err
			}
			reader = &record.CIReader{Phase: at}
		} else {
			row, err := e.phaseRow(ctx, tx, issue.Key, role)
			if err != nil {
				return err
			}
			if row.Entry == 0 || row.EntryTask == "" {
				continue
			}
			if !row.Completed() && start.Run == record.RunOpen {
				reader = &record.CIReader{Phase: at, Entry: row.Entry}
			}
		}
		if e.toldByStart(start, fix, opened) {
			continue
		}
		branch, err := e.toldBranch(ctx, tx, issue, place, start, fix, role)
		if err != nil {
			return err
		}
		if err := e.tellFixStarted(ctx, tx, issue, w, news, to, reader, branch); err != nil {
			return err
		}
	}
	return nil
}

// tellOwnerOfFix tells the owner, whose acceptance step starts at step while a fix of the task
// reads its job or runs (startNamedStep), of that fix by the fix-started row tellStepsOfFix gives
// each step beside it: the acceptance has no task of its own to name the fix, so the row is its one
// notice of it, naming the owner's branch beside the fix, which the start bound (bindBesideFix). Its
// acceptance is not held by the fix's check. A fix that opens after the acceptance started tells
// the owner at its own start instead.
func (e *Engine) tellOwnerOfFix(ctx context.Context, tx pgx.Tx, issue record.Issue, step standing) error {
	if step.place.Kind != record.PlaceStep {
		return nil
	}
	fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, step.place.Task)
	if err != nil || fix == nil || fix.Place == step.place.Place {
		return err
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	news, err := e.fixStartedOf(ctx, tx, issue, w, *fix)
	if err != nil {
		return err
	}
	start, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
	if err != nil {
		return err
	}
	owner, err := e.owningArchitect(ctx, tx, issue)
	if err != nil {
		return err
	}
	return e.tellFixStarted(ctx, tx, issue, w, news, roleRecipient{issue: owner, role: claim.RoleArchitect, row: issue.Key},
		&record.CIReader{Phase: step.at}, start.PushBranch)
}

// fixStarted is what every fix-started row of fix carries beside its receiver: the repository and
// the number of the task's pull request, how the fix's check's CI job ended at the fix's head, and
// the word of the step the fix runs again in the task's plan.
type fixStarted struct {
	fix    record.TaskFix
	repo   string
	number int
	ciCase string
	step   string
}

// fixStartedOf is the fixStarted of fix, a fix of w's task of issue.
func (e *Engine) fixStartedOf(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix) (fixStarted, error) {
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return fixStarted{}, err
	}
	news := fixStarted{fix: fix, repo: prRepo(issue, pr)}
	if pr != nil {
		news.number = pr.Number
	}
	held, err := e.store.HeadStates(ctx, tx, issue.Key, news.repo, fix.Head)
	if err != nil {
		return fixStarted{}, err
	}
	news.ciCase = held.States[fix.Check].CICase
	if words := plan.Words(namedSteps(w.Steps)); fix.Place >= 1 && fix.Place <= len(words) {
		news.step = words[fix.Place-1]
	}
	return news, nil
}

// tellFixStarted writes the fix-started row of news to to, a step beside the fix or the owner, for
// reader, its current work, nil where its step does not run, with branch, the branch beside the fix
// it pushes to, "" for the reviewer, which creates no commit. The row names the fix's check, head,
// job's case and number (Assigned) and that branch, which a test reads; its body
// (fixStartedBody) is built from the same values.
func (e *Engine) tellFixStarted(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, news fixStarted, to roleRecipient,
	reader *record.CIReader, branch string) error {
	addressed, err := e.addressRoles(issue.Project, []roleRecipient{to}, nil)
	if err != nil {
		return err
	}
	fix := news.fix
	for _, a := range addressed {
		if err := e.enqueue(ctx, tx, a.issue, record.CINews{Purpose: record.CINewsFixStarted, Role: string(a.token), Generation: issue.Generation,
			Task: w.Task, Repo: news.repo, PullRequest: news.number, Head: fix.Head, Reader: reader, Branch: branch,
			Assigned: &record.CIAssigned{Check: fix.Check, Head: fix.Head, CICase: news.ciCase, Fix: fix.Fix},
			Body:     fixStartedBody(issue.Key, news, to.role, branch)}); err != nil {
			return err
		}
	}
	return nil
}

// fixStartedBody is the body of the fix-started row to role, a step beside the fix of news, or the
// owner for the architect, branch being its branch beside the fix ("" for the reviewer): the fix,
// its check, head and job, and the step it runs again in its own working copy; that the failure is
// the fix's, so the step's report does not fail for it (the testing step leaves each check a CI job
// runs to its task's end); and how it pushes until its step ends, where it records each result and
// what it does where GitHub refuses its push. The owner is told what it accepts on instead of where
// it records.
func fixStartedBody(key string, news fixStarted, role claim.Role, branch string) string {
	fix := news.fix
	body := fmt.Sprintf("A fix of task %s of %s runs: check %s failed at %s (CI job %s). It is step %d (%s) again, in a working copy of its own.",
		fix.Task, key, fix.Check, fix.Head, jobOf(fix), fix.Place, news.step)
	body += " Leave that failure to the fix: your report does not fail for it, and you run its job no more."
	if branch == "" {
		return body + " Until your step ends, push nothing to the branch of the task."
	}
	body += pushBesideSentence(branch)
	if role == claim.RoleArchitect {
		return body + fmt.Sprintf(" Accept only where each check but %s reads yes at the head of the pull request. %s is the fix's, and it does not hold your acceptance. "+
			"A result at a commit of your own branch counts for nothing.", fix.Check, fix.Check) + refusedPushSentence
	}
	return body + recordAtHead(fix.Head)
}

// runningFixOf is the walk of issue's run and its task's fix number n, where that fix runs (the
// fix the issue record names, record.Issue.RunningFix, with its entry raised): the fix a
// completion or a move back of the implementer's claim names ends it. A fix that reads its job
// still, has ended, or was never run is no fix a completion ends: the completion is refused as one
// of an entry the issue has moved past (HANDOFF_STALE_ENTRY), changing nothing.
func (e *Engine) runningFixOf(ctx context.Context, tx pgx.Tx, issue record.Issue, n int) (Walk, *record.TaskFix, *intake.Refusal, error) {
	w, fix, err := e.fixNamed(ctx, tx, issue, n)
	if err != nil || fix != nil {
		return w, fix, nil, err
	}
	return w, nil, refused("HANDOFF_STALE_ENTRY", fmt.Sprintf("the implementer's completion of fix %d of task %s of %s is of a fix that runs no more; this completion changed nothing",
		n, w.Task, issue.Key)).Refusal, nil
}

// fixNamed is the walk of issue's run and its task's fix number n where that fix runs, nil where
// it does not (runningFixOf).
func (e *Engine) fixNamed(ctx context.Context, tx pgx.Tx, issue record.Issue, n int) (Walk, *record.TaskFix, error) {
	w, err := e.walk(ctx, tx, issue)
	if err != nil || !w.Stored || issue.RunningFix != n {
		return w, nil, err
	}
	fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.Fix != n || fix.State != record.FixRunning {
		return w, nil, err
	}
	return w, fix, nil
}

// fixCompleted ends fix, which the implementer's completion fact ended: the fix passed, at the
// commit the completion reports, or, for one that names none, at the task's head as it stands
// (taskHead), since only the fix pushes while it runs. Its run ends with the completion's report.
func (e *Engine) fixCompleted(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix *record.TaskFix, fact intake.HandoffComplete, pr *record.PullRequest) error {
	endHead := fact.Commit
	if endHead == "" {
		_, endHead = taskHead(pr)
	}
	return e.endFix(ctx, tx, issue, w, fix, StepEnd{Result: ResultPassed, Report: fact.HandoffRecord}, endHead)
}

// fixMovedBack applies the implementer's move back while it runs fix fact.Fix: the fix's report of
// a failure, taken whatever phase the issue stands in. It ends the fix failed at the task's head,
// returning the task to the step the move names, which stands before the fix's own step on the
// task's walk; the issue does not move now, since the step beside the fix goes on. It is refused
// BACKWARD_REFUSED, changing nothing, where the fix it names runs no more, where the step it names
// is not before the fix's, and where the pull request has merged, and it is refused as every move
// back is for a reason Dispatch would refuse and on a lingering tree.
func (e *Engine) fixMovedBack(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.BackwardMove) (intake.Result, error) {
	w, fix, err := e.fixNamed(ctx, tx, issue, fact.Fix)
	if err != nil {
		return intake.Result{}, err
	}
	if fix == nil {
		return refused("BACKWARD_REFUSED", fmt.Sprintf("the implementer's move of %s to %s names fix %d of task %s, which runs no more, so it changed nothing",
			issue.Key, fact.To, fact.Fix, w.Task)), nil
	}
	to, ok := w.PlaceOf(fact.To)
	if !ok || to.Kind != record.PlaceStep || to.Position >= fix.Place {
		return refused("BACKWARD_REFUSED", fmt.Sprintf("a fix's move back is its report of a failure, which returns task %s of %s to a step before the fix's own, step %d; %s is no such step, so the implementer's move of fix %d changed nothing",
			w.Task, issue.Key, fix.Place, fact.To, fix.Fix)), nil
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	if pr != nil && pr.State == record.PullRequestMerged {
		return refused("BACKWARD_REFUSED", fmt.Sprintf("the implementer's move of fix %d of %s to %s changed nothing: its pull request has merged; a merged change never moves back",
			fix.Fix, issue.Key, fact.To)), nil
	}
	if code, why := backwardReasonRefusal(phase.Implementing, fact.To, mover{role: claim.RoleImplementer}, fact.Reason); code != "" {
		return refused(code, why), nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if lingers {
		return refusedLingering(issue, fmt.Sprintf("the implementer's move of fix %d to %s", fix.Fix, fact.To)), nil
	}
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return intake.Result{}, err
	}
	_, head := taskHead(pr)
	return intake.Result{}, e.endFix(ctx, tx, issue, w, fix, StepEnd{Result: ResultFailed, ReturnedTo: &to.Position}, head)
}

// endFix is the one writer of a fix's end: the fix row ends (record.FixEnded) with end's result,
// at endHead, the commit the fix's own completion reports, or the task's head where its end
// reports none, so "the fix made new commits" is its end against its own start, EndHead against
// Head, and never a commit another step pushed. A fix that failed returns the task to the earlier
// of the step a step beside it returned the task to and the step the fix's own report names
// (earlierReturn). The run of the step the fix ran again ends with end (endRun), its report and
// its return among it. The fix's own end decides what runs after it (afterFix), which waits while
// another step of the task runs.
func (e *Engine) endFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix *record.TaskFix, end StepEnd, endHead string) error {
	now := e.now()
	fix.State, fix.Result, fix.EndHead, fix.EndedAt = record.FixEnded, string(end.Result), endHead, &now
	end.Commit = endHead
	if end.Result == ResultFailed {
		fix.Failed = true
		if end.ReturnedTo != nil {
			fix.ReturnTo = earlierReturn(fix.ReturnTo, *end.ReturnedTo)
		}
	}
	if err := e.store.UpdateFix(ctx, tx, *fix); err != nil {
		return err
	}
	place := record.TaskPlace{Kind: record.PlaceStep, Position: fix.Place}
	if role, typ, ok := stepOfWalk(w, place); ok && recordsSteps(issue) {
		at := place.StepPlace(w.Task)
		if _, err := e.endRun(ctx, tx, issue.Key, at, end, func(start record.StepStart) record.StepRecord {
			return stepWrite(issue, at, role, typ, start.Agent, start.Starts)
		}); err != nil {
			return err
		}
	}
	e.logOnCommit(ctx, "workflow: a fix ends", "issue", issue.Key, "task", fix.Task, "fix", fix.Fix, "result", fix.Result,
		"check", fix.Check, "head", fix.Head, "endHead", fix.EndHead, "returnTo", fix.ReturnTo, "phase", issue.Phase)
	return e.afterFix(ctx, tx, issue, w, nil)
}

// earlierReturn is the earlier of two places a task returns to, by place from 1, where 0 is none.
func earlierReturn(stored, reported int) int {
	if stored == 0 || (reported > 0 && reported < stored) {
		return reported
	}
	return stored
}

// The ways a fix's end takes (record.TaskFix.Way), after R7a's three: wayReturn returns the task
// to an earlier step, wayAfterFix runs again the steps after the fix's own, and wayOn leaves each
// step its result.
const (
	wayReturn   = 1
	wayAfterFix = 2
	wayOn       = 3
)

// rowMove is a move of the table that transition took no row for while a fix's way was pending
// (besideFix): afterFix makes it where the way leaves each step its result.
type rowMove struct {
	trigger TriggerKind
	target  phase.Phase
	handoff record.PhaseRow
	pr      *record.PullRequest
	why     moveWhy
}

// wayMove is a move the way after a fix makes (afterFix), which its context carries: moveTo is the
// head the working copy of the task moves to before the started step's task (moveToOf), the task's
// head for the first two ways and none for the third, and its presence marks the move as the
// way's own, which besideFix lets through.
type wayMove struct{ moveTo string }

// wayMoveKey is the context key of a wayMove.
type wayMoveKey struct{}

// moveToOf is the head the start of a step moves the task's working copy to: the task's head for
// a start the first two ways after a fix make, "" for every other start.
func moveToOf(ctx context.Context) string {
	move, _ := ctx.Value(wayMoveKey{}).(wayMove)
	return move.moveTo
}

// copyMoveAfterFix is the head a start of a step of w's task moves the task's working copy to, and
// the task's latest fix whose move that start carries, nil for none, which start marks carried
// (record.TaskFix.MoveCarried) in the transaction that writes the start. The fix pushes from a
// working copy of its own, so the task's copy stands where the fix started until a start after the
// fix moves it; a later start finds the copy there or past it, where a move could take it back from
// a commit made since. So one start carries each fix's move, and the fix's mark, not any moment,
// records which: the way's own start, which moves the copy to the head the way chose (moveToOf, the
// first two ways), and otherwise the first start after the fix ended with commits of its own (its
// end is not the head it started at), which moves the copy to that end. That is a restart of the
// task after a stop, a leave or a new generation ended the fix (wayNone), the retro, the merge or a
// step after a way that left each step its result (wayOn), and a start while the way is pending.
// A start after the one that carried the move answers "", and so does every start of a task that
// has had no fix, or whose latest fix is open or ended with no commit of its own outside a way's
// start.
func (e *Engine) copyMoveAfterFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (string, *record.TaskFix, error) {
	moveTo := moveToOf(ctx)
	if !w.Stored {
		return moveTo, nil, nil
	}
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	switch {
	case err != nil || fix == nil || fix.State != record.FixEnded:
		return moveTo, nil, err
	case fix.MoveCarried:
		return "", nil, nil
	case moveTo != "":
		return moveTo, fix, nil
	case fix.EndHead == "" || fix.EndHead == fix.Head:
		return "", nil, nil
	}
	return fix.EndHead, fix, nil
}

// besideFix takes, in place of the row transition found, a move of a task whose latest fix is open
// or has ended with its way not taken, and answers whether it took the move; every other move is
// the row's.
//
// Beside an open fix, a step that passed moves the task on as the row says. A step that failed
// starts no step: its run ends failed, returning to the step the row returns to; where that step
// is the fix's own, the fix takes the report (GotReport) and the implementer is told of it, and
// otherwise the fix keeps the earliest step the task returns to (ReturnTo). The task is then held
// for its fix from the phase of the step that failed, which keeps that phase's slot, stops no
// claim, and moves nothing: the way after the fix makes the move (afterFix).
//
// After a fix ended while another step ran, the step that ends now has its end written as the row
// would write it, a failure's return kept on the fix, and then afterFix decides the way in place
// of the row, which it makes itself where each step keeps its result. So no step starts, and no
// retro starts, before the way is decided.
//
// Either way a review that requested changes counts its round here, as transition counts it; its
// return is counted by the way after the fix, which is the one return. A move the way after a fix
// makes itself is the row's (wayMove).
func (e *Engine) besideFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, trigger TriggerKind, target phase.Phase, row Row,
	handoff record.PhaseRow, pr *record.PullRequest, why moveWhy) (bool, error) {
	if _, way := ctx.Value(wayMoveKey{}).(wayMove); way || !w.Stored {
		return false, nil
	}
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.Way != 0 {
		return false, err
	}
	open := fix.State == record.FixReading || fix.State == record.FixRunning
	_, returns := endOf(trigger)
	step, stands := standingStep(w, issue)
	if (open && !returns) || !stands {
		return false, nil
	}
	if trigger == TriggerReviewRejected || (returns && step.typ == plan.TypeReview) {
		if err := e.recordRound(ctx, tx, issue, w.Task, pr); err != nil {
			return false, err
		}
	}
	end := endOfTransition(w, trigger, step.at, row.To, handoff)
	gotReport := false
	if end.ReturnedTo != nil {
		if gotReport = open && *end.ReturnedTo == fix.Place; gotReport {
			fix.GotReport = true
		} else {
			fix.ReturnTo = earlierReturn(fix.ReturnTo, *end.ReturnedTo)
		}
		if err := e.store.UpdateFix(ctx, tx, *fix); err != nil {
			return false, err
		}
	}
	// The failed step's branch beside the fix is the one its start was bound to, read before its
	// run ends.
	stepStart, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
	if err != nil {
		return false, err
	}
	if _, err := e.endRun(ctx, tx, issue.Key, step.place, end, func(start record.StepStart) record.StepRecord {
		return stepWrite(issue, step.place, step.role, step.typ, start.Agent, start.Starts)
	}); err != nil {
		return false, err
	}
	if !open {
		return true, e.afterFix(ctx, tx, issue, w, &rowMove{trigger: trigger, target: target, handoff: handoff, pr: pr, why: why})
	}
	if gotReport {
		// The fix's entry is the one its start raised, and a fix that still reads its job has
		// none: its start, in the fact of its job's read, raises the entry and composes the task
		// that carries this report (fixReports). So a report that comes while the fix reads its
		// job is queued for nobody, and the start speaks it instead.
		if fix.State == record.FixRunning {
			if err := e.reportToFix(ctx, tx, issue, w, *fix, step, handoff, pr, stepStart.PushBranch); err != nil {
				return false, err
			}
		}
	}
	from := issue.Phase
	issue.Phase, issue.Hold = phase.Held, &record.Hold{From: from, Reason: record.HoldForFix}
	if err := e.WriteIssue(ctx, tx, issue); err != nil {
		return false, err
	}
	e.logOnCommit(ctx, "workflow: a failed step beside a fix waits for the fix", "issue", issue.Key, "task", w.Task, "fix", fix.Fix,
		"from", from, "trigger", trigger, "gotReport", gotReport, "returnTo", fix.ReturnTo)
	return true, nil
}

// reportToFix gives fix, which runs, the report of step, a step beside it that failed and returned
// the task to the fix's own step (besideFix), handoff being that step's completion: one step-report
// row to the implementer, for the fix's work (its number and entry), so the outbox wakes a released
// fix and finishes the row undelivered once the fix ended, naming branch, the step's own branch beside
// the fix that its start was bound to, where its commits are ("" for none), which the fix takes
// before its end push.
func (e *Engine) reportToFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix, step standing, handoff record.PhaseRow,
	pr *record.PullRequest, branch string) error {
	addressed, err := e.addressRoles(issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleImplementer, row: issue.Key}}, nil)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("The %s of step %d reports: %s. Its report is %s. It is part of your fix.", step.role, step.place.Place, handoff.Summary, handoff.HandoffRecord)
	if branch != "" {
		body += fmt.Sprintf(" Its commits are on its own branch %s: take them before your end push, as your task says.", branch)
	}
	number := 0
	if pr != nil {
		number = pr.Number
	}
	for _, to := range addressed {
		if err := e.enqueue(ctx, tx, to.issue, record.CINews{Purpose: record.CINewsStepReport, Role: string(to.token), Generation: issue.Generation,
			Task: w.Task, Repo: prRepo(issue, pr), PullRequest: number, Head: fix.Head, Reader: &record.CIReader{Fix: fix.Fix, Entry: fix.Entry},
			Branch: branch, Body: body}); err != nil {
			return err
		}
	}
	return nil
}

// taskStepRuns says whether a step of w's task runs: the latest run of one of its places is open.
func (e *Engine) taskStepRuns(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (bool, error) {
	for place := range w.Steps {
		start, err := e.store.StepStarts(ctx, tx, issue.Key, record.StepPlace{Task: w.Task, Kind: record.PlaceStep, Place: place + 1})
		if err != nil || start.Run == record.RunOpen {
			return err == nil, err
		}
	}
	return false, nil
}

// fixWayOf is the way fix's end takes (R7a's three ways) on a task of steps steps, and the place
// of the step the task returns to for the first two. taskHead is the head of the task's branch
// when the way is taken, "" where the record holds none.
//
// The commits the way counts are the task's, not the fix's own alone: a branch that no longer
// stands at the head the fix's check failed at carries work the steps after the fix have not
// seen, whether the fix pushed it or anyone else did while the fix ran. Two readings say so, and
// either is enough. The fix's own end (EndHead) answers before the synchronize of its end push
// has moved the record's head, and taskHead, the head of the task's branch when the way is taken
// ("" where the record holds none), answers for a push the fix did not make. The first way: the
// fix failed, or a step beside it failed to a step other than the fix's, so the task returns to
// the earliest such step, or, where the branch holds new commits, to the step after the fix where
// that is earlier. The second: the branch holds new commits, or the fix got a failed step's
// report, so the steps after it run again. The third, in each other case, and where no step comes
// after the fix's: each step keeps its result.
func fixWayOf(fix record.TaskFix, steps int, taskHead string) (way, place int) {
	next := fix.Place + 1
	madeCommits := (fix.EndHead != "" && fix.EndHead != fix.Head) || (taskHead != "" && taskHead != fix.Head)
	switch {
	case fix.Failed || fix.ReturnTo > 0:
		place = fix.ReturnTo
		if place == 0 || (madeCommits && next < place) {
			place = next
		}
		way = wayReturn
	case madeCommits || fix.GotReport:
		way, place = wayAfterFix, next
	}
	if way == 0 || place > steps {
		return wayOn, 0
	}
	return way, place
}

// afterFix takes the way after the latest fix of w's task once (R7a's three ways): only where that
// fix has ended, its way is not taken (Way 0, never wayNone), and no step of the task runs; it
// does nothing otherwise, and the end of the last step running beside the fix calls it again
// (besideFix). It writes the way on the fix row in the transaction of its act, so the way is taken
// once. The first two ways return the task, held or not, by the one move out of a hold
// (moveFromHold), at the task's head, which the started step's working copy moves to. The third
// makes pending, the move of the table transition took no row for, where there is one. After the
// way's act, in its transaction, the conflict that waited while the fix held the task's branch is
// read again (mergeForwardAfterFix).
func (e *Engine) afterFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, pending *rowMove) error {
	if !w.Stored {
		return nil
	}
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.State != record.FixEnded || fix.Way != 0 {
		return err
	}
	if runs, err := e.taskStepRuns(ctx, tx, issue, w); err != nil || runs {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	_, head := taskHead(pr)
	way, place := fixWayOf(*fix, len(w.Steps), head)
	if !e.noWayKept {
		fix.Way = way
		if err := e.store.UpdateFix(ctx, tx, *fix); err != nil {
			return err
		}
	}
	if e.deletesAtWay(*fix, way) {
		if err := e.queueBesideDelete(ctx, tx, issue, w.Task, fix.Fix); err != nil {
			return err
		}
	}
	// The head the started step's working copy moves to is the fix's own end where it made commits:
	// its completion reports the commit it pushed, and it reaches the engine before the synchronize
	// of that push may have moved the record's head. Where the record's head is further on, it is
	// that head, which holds the fix's commits and every commit pushed after them.
	moveTo := head
	if fix.EndHead != "" && fix.EndHead != fix.Head && head == fix.Head && !e.wayFromRecordedHead {
		moveTo = fix.EndHead
	}
	e.logOnCommit(ctx, "workflow: the way after a fix is taken", "issue", issue.Key, "task", w.Task, "fix", fix.Fix, "way", way,
		"place", place, "moveTo", moveTo)
	if way == wayOn {
		if pending != nil {
			if err := e.transition(context.WithValue(ctx, wayMoveKey{}, wayMove{}), tx, issue, pending.trigger, pending.target, pending.handoff, pending.pr, pending.why); err != nil {
				return err
			}
		}
		return e.mergeForwardAfterFix(ctx, tx, issue.Key)
	}
	ctx = context.WithValue(context.WithValue(ctx, wayMoveKey{}, wayMove{moveTo: moveTo}), moverKey{}, mover{fix: fix.Fix})
	handoff, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleImplementer)
	if err != nil {
		return err
	}
	steps := w.steps()
	reason := fmt.Sprintf("fix %d of task %s, for check %s failed at %s, ended at %s", fix.Fix, w.Task, fix.Check, fix.Head, fix.EndHead)
	if err := e.moveFromHold(ctx, tx, issue, w, steps[place-1], steps[fix.Place-1], handoff, pr, reason); err != nil {
		return err
	}
	return e.mergeForwardAfterFix(ctx, tx, issue.Key)
}

// deletesAtWay says whether the way after fix, way, deletes the branches beside it
// (queueBesideDelete): never. A step beside the fix can push to its branch after the fix's end
// push, and nothing records what that push took, so each branch stays until no step of the task
// can run again (queueTaskEnd, a leave, a new generation, a version that removes the task), and the
// step a way 2 runs again takes its commits from its own (keptFacts). The seams:
// besideDeleteAsBefore deletes after every way 2 or 3, and deleteAfterEveryCompletion after every
// way 2 or 3 of the fix's own completion.
func (e *Engine) deletesAtWay(fix record.TaskFix, way int) bool {
	switch {
	case way != wayAfterFix && way != wayOn:
		return false
	case e.besideDeleteAsBefore:
		return true
	}
	return e.deleteAfterEveryCompletion && fix.ClaimFailed == "" && fix.Result == string(ResultPassed)
}

// moveFromHold is the one move that returns a held task, and the move of the way after a fix
// (afterFix): where issue is held for its fix or its CI (fixOrCIHold), the phase the hold stands
// in is put back and the hold cleared in one write; then, where target is another phase than
// that one, the task moves back to target on TriggerBackward, which transition counts as a return
// by the pair of the two phases, firing the deployment's hooks there. Where target is the phase
// the issue stands in, no row moves the task: the step there starts again as a return to it, its
// task naming handoff's report, and the return is counted by the pair (rerunFrom, target), the act
// that sends the task back being the fix's, out of its step into the step after it.
func (e *Engine) moveFromHold(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, target, rerunFrom phase.Phase, handoff record.PhaseRow,
	pr *record.PullRequest, reason string) error {
	current, err := e.store.Issue(ctx, tx, issue.Key)
	if err != nil || current == nil {
		return err
	}
	issue = *current
	if fixOrCIHold(issue) {
		issue.Phase, issue.Hold = record.StandsAt(issue), nil
		if err := e.WriteIssue(ctx, tx, issue); err != nil {
			return err
		}
	}
	if target != issue.Phase {
		return e.transition(ctx, tx, issue, TriggerBackward, target, handoff, pr, moveWhy{Reason: reason})
	}
	if err := e.recordPass(ctx, tx, issue, w.Task, rerunFrom, target, pr); err != nil {
		return err
	}
	role := RoleFor(target)
	if err := e.clearHandoff(ctx, tx, issue.Key, role); err != nil {
		return err
	}
	if err := e.start(ctx, tx, issue, w, role, func(at Standing) string { return task(issue, at, handoff, pr, reason) }, true); err != nil {
		return err
	}
	return e.queueMovedNews(ctx, tx, issue)
}
