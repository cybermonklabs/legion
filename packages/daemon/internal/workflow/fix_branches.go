package workflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// While a fix of a task reads its job or runs, each other step of the task pushes its commits to a
// branch of its own beside the fix, never to the task's branch (claim.BesideBranch): the step's
// start or the fix's fix-started row names that branch (startFacts, tellStepsOfFix), the step's
// open start keeps it (record.StepStart.PushBranch), an update round's ask names it
// (UpdatePushes), the daemon keeps the head each push left (RecordBesidePush, basePush), and the
// fix takes the commits from there before its one push to the task's branch. The daemon deletes
// the branches once the fix took their commits, or once no step of the task can run again
// (queueBesideDelete).

// offHeadWaitReason is why the testing move waits where the tester's completion named head, at
// which Legion holds no set of check states, and the open pull request stands at prHead, another
// commit: Dispatch computes states only at a pull request's heads and former heads, so no state
// comes at a commit that is neither, such as one of a step's own branch beside a fix, and the
// tester is to record its results at the pull request's head and complete again.
func offHeadWaitReason(head, prHead string) string {
	return fmt.Sprintf("the move waits for Dispatch's states at %s, which is not the head of the pull request (%s) and at which Legion holds no state. "+
		"Dispatch computes states only at a pull request's heads and former heads, so where %s is neither, none comes: "+
		"the tester records its results at the pull request's head and completes again", head, prHead, head)
}

// besideBranchOf is the branch beside fix of task in issue's generation that role's step pushes to
// (claim.BesideBranch), and true. Where no such name can be made, which only an issue key and a
// generation beyond Dispatch's key rule can cause, it logs that one task by name with the error
// and answers false, and the caller gives that task nothing of the branch, so every other fact of
// the issue goes on. The fix rule refuses to open such a task's fix at all (openFix), so no fix
// that runs meets this.
func (e *Engine) besideBranchOf(issue record.Issue, task string, fix int, role claim.Role) (string, bool) {
	branch, err := claim.BesideBranch(issue.Key, issue.Generation, task, fix, role)
	if err != nil {
		e.log.Error("workflow: no branch beside a fix can be named for this task, which gets none", "issue", issue.Key,
			"generation", issue.Generation, "task", task, "fix", fix, "role", role, "error", err)
		return "", false
	}
	return branch, true
}

// refuseFixName refuses to open a fix of the task of pass for check, failing at the pass's head,
// where the name of the fix's own branch cannot be made (claim.BesideBranch's err): the failure is
// the owner's, its row written where none was, and the owner is told once for the check at that
// head, by one ci-failed notice naming the task and the error; no fix opens, nothing of a fix is
// delivered, and the fact goes on.
func (e *Engine) refuseFixName(ctx context.Context, tx pgx.Tx, pass *fixPass, check string, failure record.CIFailure, row *record.CIFailure, err error) error {
	issue, w := pass.issue, pass.w
	e.log.Error("workflow: a fix of this task cannot be opened: no branch beside it can be named", "issue", issue.Key,
		"generation", issue.Generation, "task", w.Task, "check", check, "head", pass.head, "error", err)
	if row == nil {
		failure.WentTo = record.CIFailureToOwner
		if _, err := e.store.PutCIFailure(ctx, tx, failure); err != nil {
			return err
		}
	}
	told, err2 := e.store.TellOwnerOnce(ctx, tx, issue.Key, issue.Generation, w.Task, check, pass.head)
	if err2 != nil || !told {
		return err2
	}
	acts, err2 := e.ciActsOf(ctx, tx, issue, w)
	if err2 != nil {
		return err2
	}
	notice := ciFailedNotice(issue, w, check, pass.head, false, acts)
	notice.Reason = fmt.Sprintf("Check %s fails at %s of task %s of %s, and no fix of task %s can run: Legion cannot name a branch beside it (%v). "+
		"Decide with the person: %s.", check, pass.head, w.Task, issue.Key, w.Task, err, acts.decide())
	return e.notice(ctx, tx, issue.Key, notice)
}

// UpdatePushes is where each role of issue key pushes at an update round's ask (supervise.UpdatePush,
// the pod update's askRound): a step whose open start is bound to a branch beside a fix
// (record.StepStart.PushBranch) pushes there, the accepting owner's named start among them, and its
// binding stands until that step's run ends, also after the fix ended with its way pending; the
// implementer pushes to its own branch F while its fix runs (OwnFix), and pushes nothing while the
// fix reads its job (FixReads), since neither its copy nor F exists yet and the task's branch has
// one writer then. Every other role gets the zero push, main's ask: the reviewer, which creates no
// commit, and a step whose start no fix named. An issue with no fix, and one whose run has no plan
// rows, gets nothing but zero pushes. The role of each step is the role of its phase, a phase of a
// step of the plan, which admitted, the architect's phase of the tree's design, never is.
//
// The seam updateByRunningFix reads no binding: a step's branch comes from the fix that reads its
// job or runs alone, so once that fix ended each step beside it gets the zero push.
func (e *Engine) UpdatePushes(ctx context.Context, tx pgx.Tx, key string) (map[claim.Role]supervise.UpdatePush, error) {
	pushes := map[claim.Role]supervise.UpdatePush{}
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return pushes, err
	}
	w, err := e.walk(ctx, tx, *issue)
	if err != nil || !w.Stored {
		return pushes, err
	}
	fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil {
		return nil, err
	}
	for i, at := range w.steps() {
		start, err := e.store.StepStarts(ctx, tx, issue.Key, record.StepPlace{Task: w.Task, Kind: record.PlaceStep, Place: i + 1})
		if err != nil {
			return nil, err
		}
		role := RoleFor(at)
		if start.Run != record.RunOpen || role == claim.RoleReviewer {
			continue
		}
		branch := start.PushBranch
		if e.updateByRunningFix {
			branch = ""
			if fix != nil && fix.Place != i+1 {
				branch, _ = e.besideBranchOf(*issue, w.Task, fix.Fix, role)
			}
		}
		if branch != "" {
			pushes[role] = supervise.UpdatePush{Branch: branch}
		}
	}
	if fix == nil {
		return pushes, nil
	}
	switch fix.State {
	case record.FixReading:
		pushes[claim.RoleImplementer] = supervise.UpdatePush{FixReads: true}
	case record.FixRunning:
		if own, ok := e.besideBranchOf(*issue, w.Task, fix.Fix, claim.RoleImplementer); ok {
			pushes[claim.RoleImplementer] = supervise.UpdatePush{Branch: own, OwnFix: true}
		}
	}
	return pushes, nil
}

// UpdateByRunningFix sets the seam updateByRunningFix on e: an update round's ask reads no step's
// binding to a branch beside a fix, only the fix that reads its job or runs (UpdatePushes), so after
// the fix ends each step beside it gets the zero push. Only tests call it, as the control of the
// binding on an engine another package builds.
func (e *Engine) UpdateByRunningFix() { e.updateByRunningFix = true }

// WithoutEndBesideDelete sets the seam noEndBesideDelete on e: a task's move into its retro with no
// hold, at a transition and at the release of an end hold, queues no deletion of the branches beside
// its fixes (queueTaskEnd). Only tests call it, as the control of that deletion on an engine another
// package builds.
func (e *Engine) WithoutEndBesideDelete() { e.noEndBesideDelete = true }

// FixBranchFromRecordedPush sets the seam fixBranchFromRecordedPush on e: a start or a resume of a
// fix names the fix's own branch only where the daemon holds a push of it (fixBranchOf). Only tests
// call it, as the control of the fix's branch on a new pod on an engine another package builds.
func (e *Engine) FixBranchFromRecordedPush() { e.fixBranchFromRecordedPush = true }

// fixBranchOf is the fix's own branch F (claim.BesideBranch of the implementer) that every start and
// resume of fix carries (startMarks.fixBranch), whether or not a push of it is recorded: the shim
// reads origin for it, and makes the fix's working copy at its head where origin holds it, so a fix
// that pushed at an update round's ask goes on from its own commits on a new pod. The seam
// fixBranchFromRecordedPush names it only where the daemon holds the implementer's push of it
// (record.TaskFix.Pushed).
func (e *Engine) fixBranchOf(issue record.Issue, fix record.TaskFix) string {
	if e.fixBranchFromRecordedPush && fix.Pushed[string(claim.RoleImplementer)] == "" {
		return ""
	}
	branch, _ := e.besideBranchOf(issue, fix.Task, fix.Fix, claim.RoleImplementer)
	return branch
}

// bindBesideFix writes what start number starts of place, a step of issue's task that role runs,
// names of the fix of its task that reads its job or runs: the fix (record.Store.MarkStepBesideFix)
// and, for every role but the reviewer, which creates no commit, the branch beside the fix it pushes
// to until its run ends (SetStepPushBranch). The task of that start named the same (startFacts), and
// the fix's start tells no fix-started row to a step whose start named the fix (tellStepsOfFix). A
// start of no step, of the fix's own place or of no role (a fix's own start), and one while no fix is
// open, names no fix: the store wrote none at the start, so a binding of an earlier start never
// reaches it.
func (e *Engine) bindBesideFix(ctx context.Context, tx pgx.Tx, issue record.Issue, place record.StepPlace, starts int64, role claim.Role) error {
	if role == "" || place.Kind != record.PlaceStep {
		return nil
	}
	open, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, place.Task)
	if err != nil || open == nil || open.Place == place.Place {
		return err
	}
	if err := e.store.MarkStepBesideFix(ctx, tx, issue.Key, place, starts, open.Fix); err != nil {
		return err
	}
	if role == claim.RoleReviewer {
		return nil
	}
	branch, ok := e.besideBranchOf(issue, place.Task, open.Fix, role)
	if !ok {
		return nil
	}
	return e.store.SetStepPushBranch(ctx, tx, issue.Key, place, starts, branch)
}

// toldByStart says whether start, the latest start of a step beside fix, told that step of fix
// already: it named fix (record.StepStart.BesideFix, written in the transaction that counted the
// start), so the fix's start sends that step no fix-started row. A start before the fix opened, in
// an earlier fact or earlier in the fact that opened it, named none. The seams: tellEveryStepOfFix
// reads no start, and toldByClock reads the clock instead, a start at or after the moment the fix
// opened (opened) taken as told, which a fixed clock or one shared microsecond confuses.
func (e *Engine) toldByStart(start record.StepStart, fix record.TaskFix, opened time.Time) bool {
	switch {
	case e.tellEveryStepOfFix:
		return false
	case e.toldByClock:
		return start.Starts > 0 && !start.StartedAt.Before(opened)
	}
	return start.BesideFix == fix.Fix
}

// toldBranch is the branch beside fix that role's step at place pushes to, which the fix-started row
// to it names, "" for the reviewer, which creates no commit: the branch the step's open start is
// bound to already, which a later fix's start keeps, else the step's branch beside fix, which the
// open start is bound to here (SetStepPushBranch). A step whose run has ended is bound to nothing.
func (e *Engine) toldBranch(ctx context.Context, tx pgx.Tx, issue record.Issue, place record.StepPlace, start record.StepStart, fix record.TaskFix,
	role claim.Role) (string, error) {
	if role == claim.RoleReviewer {
		return "", nil
	}
	open := start.Run == record.RunOpen
	if open && start.PushBranch != "" {
		return start.PushBranch, nil
	}
	branch, ok := e.besideBranchOf(issue, place.Task, fix.Fix, role)
	if !ok || !open {
		return branch, nil
	}
	return branch, e.store.SetStepPushBranch(ctx, tx, issue.Key, place, start.Starts, branch)
}

// queueBesideDelete queues the deletion of the branches beside fix of task in issue's generation,
// or beside every fix of the task there where fix is 0 (record.BesideBranchesDelete), in the
// caller's transaction: after the way of a fix whose own completion made commits (afterFix), and
// where no step of the task can run again (the move into its retro, the release of an end hold
// into it, a leave, a new generation, a version that removes the task). A task that never had a
// fix writes nothing, and so does one whose branches cannot be named (besideBranchOf). It takes the
// task's id, so it names a task a version removes.
func (e *Engine) queueBesideDelete(ctx context.Context, tx pgx.Tx, issue record.Issue, task string, fix int) error {
	latest, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, task)
	if err != nil || latest == nil {
		return err
	}
	if _, err := claim.BesideTaskPrefix(issue.Key, issue.Generation, task); err != nil {
		e.log.Error("workflow: the branches beside this task's fixes cannot be named, so none is deleted", "issue", issue.Key,
			"generation", issue.Generation, "task", task, "error", err)
		return nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.BesideBranchesDelete{Generation: issue.Generation, Task: task, Fix: fix}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the branches beside a fix are to be deleted", "issue", issue.Key, "generation", issue.Generation,
		"task", task, "fix", fix)
	return nil
}

// queueTaskEnd queues the deletion of the branches beside every fix of w's task (queueBesideDelete,
// fix 0) where no step of the task can run again because it moves into its retro with no hold
// (transition, startHeldRetro). The seam noEndBesideDelete takes these two out.
func (e *Engine) queueTaskEnd(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) error {
	if !w.Stored || e.noEndBesideDelete {
		return nil
	}
	return e.queueBesideDelete(ctx, tx, issue, w.Task, 0)
}

// recordBesidePush keeps the head a push left a branch beside a fix at (record.Store.RecordBesidePush),
// for the fix and the role the branch is of: fact is the push of a branch that is no issue's
// (basePush), and a branch beside a fix is one. The branch is matched to its task among the tasks
// of its issue's plan at the name's generation whose fix of the name's number the store holds
// (claim.MatchBesideBranch); the name is read for nothing more. A deleted branch, whose push leaves
// no commit, removes that role's key alone. A branch of no issue, generation, fix or task the
// record holds is left alone.
func (e *Engine) recordBesidePush(ctx context.Context, tx pgx.Tx, fact intake.BasePush) error {
	key, generation, number, ok := besideBranchFix(fact.Branch)
	if !ok {
		return nil
	}
	plan, err := e.store.Plan(ctx, tx, key, generation)
	if err != nil || plan == nil {
		return err
	}
	var tasks []string
	for _, task := range plan.Tasks {
		fix, err := e.store.FixAt(ctx, tx, key, generation, task.Task, number)
		if err != nil {
			return err
		}
		if fix != nil {
			tasks = append(tasks, task.Task)
		}
	}
	issue, _, task, fix, role, ok := claim.MatchBesideBranch(fact.Branch, tasks)
	if !ok {
		return nil
	}
	head := fact.After
	if head == record.NoCommit {
		head = ""
	}
	if err := e.store.RecordBesidePush(ctx, tx, issue, generation, task, fix, string(role), head); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a push beside a fix is recorded", "issue", issue, "generation", generation, "task", task, "fix", fix,
		"role", role, "branch", fact.Branch, "head", head)
	return nil
}

// besideBranchFix is the issue, the generation and the fix's number a branch beside a fix names
// in its first, second and fourth parts (claim.BesideBranch's form), the parts a match reads its
// task by (claim.MatchBesideBranch), and false for a branch of another form.
func besideBranchFix(branch string) (issue string, generation uint64, fix int, ok bool) {
	if !claim.IsBesideBranch(branch) {
		return "", 0, 0, false
	}
	parts := strings.Split(branch, "/")
	if len(parts) != 6 || !strings.HasPrefix(parts[4], "fix-") {
		return "", 0, 0, false
	}
	generation, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	if fix, err = strconv.Atoi(strings.TrimPrefix(parts[4], "fix-")); err != nil {
		return "", 0, 0, false
	}
	return parts[1], generation, fix, true
}

// besideWorkSentence is what the owner is told of the work beside fix of task, at a stop that
// ends the fix (dropFixWay) and at a failed claim of the fix (fixClaimFailed): where the commits of
// the fix and of each step beside it are, the fix's own branch, and that Legion keeps them until no
// step of the task can run again, which is so, since neither end deletes a branch. A task whose
// branches cannot be named says so instead.
func besideWorkSentence(issue string, generation uint64, task string, fix int) string {
	prefix, err := claim.BesideFixPrefix(issue, generation, task, fix)
	if err != nil {
		return fmt.Sprintf(" No branch beside fix %d of task %s can be named (%v), so its work stands only where its working copies hold it.", fix, task, err)
	}
	own, err := claim.BesideBranch(issue, generation, task, fix, claim.RoleImplementer)
	if err != nil {
		return fmt.Sprintf(" No branch beside fix %d of task %s can be named (%v), so its work stands only where its working copies hold it.", fix, task, err)
	}
	return fmt.Sprintf(" The commits of fix %d and of each step beside it, where they pushed them, are on the branches under %s on GitHub. The fix's own is %s. "+
		"Legion keeps them until no step of the task can run again.", fix, prefix, own)
}

// besideStepSentence is what the task of a step that starts beside a fix says of it (fixLines),
// from the start's facts (startFacts): the fix runs, its failure is the fix's, and, where the step
// has a branch of its own (facts.Branch, "" for the reviewer, which creates no commit), that it
// pushes only there, names that branch and its commits in its report, records each result at the
// pull request's head, and what it does where GitHub refuses the push; and, where the daemon holds a
// push of that branch (facts.Pushed), where those commits stand for a new pod. No text names a ref
// the daemon holds no push of.
func besideStepSentence(facts startFacts) string {
	sentence := fmt.Sprintf(" A fix of this task runs: check %s failed at %s. Leave that failure to the fix.", facts.Check, facts.Head)
	if facts.Branch == "" {
		return sentence
	}
	sentence += pushBesideSentence(facts.Branch) + recordAtHead(facts.Head)
	if facts.Pushed != "" {
		sentence += fmt.Sprintf(" Your pushed commits are on %s at %s: on a new pod, run jj -R \"$LEGION_WORKSPACE\" git fetch --remote origin --branch %s first and go on at %s@origin.",
			facts.Branch, facts.Pushed, facts.Branch, facts.Branch)
	}
	return sentence
}

// pushBesideSentence is the sentence that tells a step beside a fix where it pushes until its step
// ends: to branch, its own branch beside the fix, with the command that pushes there
// (workspace.BesidePushCommand), never to the task's branch, naming that branch and each commit in
// its report.
func pushBesideSentence(branch string) string {
	return fmt.Sprintf(" Until your step ends, push nothing to the branch of the task: push your commits to your own branch %s with %s, and name that branch and each commit in your report.",
		branch, workspace.BesidePushCommand(branch))
}

// recordAtHead is the two sentences a step beside a fix that pushes is told of where it records
// and what it does where GitHub refuses its push: each result, and its head, at head, the pull
// request's, never at a commit of its own branch.
func recordAtHead(head string) string {
	return fmt.Sprintf(" Record each result, and name your head, at %s, the head of the pull request, never at a commit of your own branch, where a result counts for nothing.", head) +
		refusedPushSentence
}

// refusedPushSentence is what a step beside a fix does where GitHub refuses the push to its own
// branch: it pushes nothing else, and names each commit by its id, with GitHub's answer.
const refusedPushSentence = " If GitHub refuses that push, push nothing else, and name each commit by its id, with GitHub's answer, in your report."

// keptSentence is what the task of a step that a way 2 runs again says of its kept branch beside
// the fix (fixLines), from the start's facts (startFacts, Kept): the fix ended, and the commits the
// step pushed to its own branch while the fix ran stay there, at the pushed head, kept by the way.
// The step takes first, by ancestry, those of them that the task's branch taskBranch does not hold
// (workspace.KeptTakeCommands), since the fix's end push took some as they are, and a take run
// again on a resumed turn takes nothing more; then it pushes to the task's branch as its task says.
func keptSentence(facts startFacts, taskBranch string) string {
	return fmt.Sprintf(" Fix %d of this task ended at %s, and the steps after it run again. Your own branch %s keeps the commits you pushed there beside the fix, at %s. "+
		"Take first those of them that the branch of the task does not hold: run %s. "+
		"That puts them under your working copy as they are, and a run of it again changes nothing; never rebase or rewrite one. Then push to the branch of the task as this task says.",
		facts.Fix, facts.Head, facts.Branch, facts.Pushed, strings.Join(workspace.KeptTakeCommands(facts.Branch, taskBranch), ", then "))
}
