package workflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// While a fix of a task reads its job or runs, each other step of the task pushes its commits to a
// branch of its own beside the fix, never to the task's branch (claim.BesideBranch): the step's
// start or the fix's fix-started row names that branch (startFacts, tellStepsOfFix), the step's
// open start keeps it (record.StepStart.PushBranch), an update round's ask names it
// (UpdatePushes), the daemon keeps the head each push left (RecordBesidePush, basePush), and the
// fix takes the commits from there before its one push to the task's branch. The daemon deletes
// the branches once no step of the task can run again (queueBesideDelete).

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

// pushesBesideFix says whether role's step pushes commits of its own while a fix of its task runs,
// and so has a branch beside the fix: every role of a step but the reviewer, which reads the task's
// head and creates no commit. It is the one owner of that rule: besideBranchOf names no branch for
// any other role, so no start, binding, fix-started row or update ask names one.
func pushesBesideFix(role claim.Role) bool {
	return role != "" && role != claim.RoleReviewer
}

// besideBranchOf is the branch beside fix of task in issue's generation that role's step pushes to
// (claim.BesideBranch), and true. A role whose step pushes nothing beside a fix (pushesBesideFix)
// has none, and answers false. Where no such name can be made, which only an issue key and a
// generation beyond Dispatch's key rule can cause, it logs that one task by name with the error
// and answers false, and the caller gives that task nothing of the branch, so every other fact of
// the issue goes on. The fix rule refuses to open such a task's fix at all (openFix), so no fix
// that runs meets this.
func (e *Engine) besideBranchOf(issue record.Issue, task string, fix int, role claim.Role) (string, bool) {
	if !pushesBesideFix(role) {
		return "", false
	}
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
		// A step's open start is bound to a branch beside a fix only where its role pushes one
		// (bindBesideFix, toldBranch, by besideBranchOf), so the reviewer's names none.
		if start.Run != record.RunOpen {
			continue
		}
		branch := start.PushBranch
		if branch != "" {
			pushes[RoleFor(at)] = supervise.UpdatePush{Branch: branch}
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

// fixBranchOf is the fix's own branch F (claim.BesideBranch of the implementer) that every start and
// resume of fix carries (startMarks.fixBranch), whether or not a push of it is recorded: the shim
// reads origin for it, and makes the fix's working copy at its head where origin holds it, so a fix
// that pushed at an update round's ask goes on from its own commits on a new pod.
func (e *Engine) fixBranchOf(issue record.Issue, fix record.TaskFix) string {
	branch, _ := e.besideBranchOf(issue, fix.Task, fix.Fix, claim.RoleImplementer)
	return branch
}

// bindBesideFix writes what start number starts of place, a step of issue's task that role runs,
// names of the fix of its task that reads its job or runs: the fix (record.Store.MarkStepBesideFix)
// and, for a role whose step pushes beside a fix (besideBranchOf, which names the reviewer none),
// the branch beside the fix it pushes to until its run ends (SetStepPushBranch). The task of that
// start named the same (startFacts), and the fix's start tells no fix-started row to a step whose
// start named the fix (tellStepsOfFix). A start of no step, of the fix's own place or of no role (a
// fix's own start), and one while no fix is open, names no fix: the store wrote none at the start,
// so a binding of an earlier start never reaches it.
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
	branch, ok := e.besideBranchOf(issue, place.Task, open.Fix, role)
	if !ok {
		return nil
	}
	return e.store.SetStepPushBranch(ctx, tx, issue.Key, place, starts, branch)
}

// toldByStart says whether start, the latest start of a step beside fix, told that step of fix
// already: it named fix (record.StepStart.BesideFix, written in the transaction that counted the
// start), so the fix's start sends that step no fix-started row. A start before the fix opened, in
// an earlier fact or earlier in the fact that opened it, named none; the fix's start marks it beside
// the fix only after this has read it (tellStepsOfFix). It reads no clock: under a fixed clock, or
// with the start and the fix's opening in one microsecond, a start before the fix reads as one after.
func (e *Engine) toldByStart(start record.StepStart, fix record.TaskFix) bool {
	return start.BesideFix == fix.Fix
}

// toldBranch is the branch beside fix that role's step at place pushes to, which the fix-started row
// to it names, "" for a role whose step pushes none (besideBranchOf): the branch the step's open
// start is bound to already, which a later fix's start keeps, else the step's branch beside fix,
// which the open start is bound to here (SetStepPushBranch). A step whose run has ended is bound to
// nothing.
func (e *Engine) toldBranch(ctx context.Context, tx pgx.Tx, issue record.Issue, place record.StepPlace, start record.StepStart, fix record.TaskFix,
	role claim.Role) (string, error) {
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

// queueBesideDelete queues the deletion of the branches beside each fix of task in issue's
// generation that the task holds now, through its latest (record.BesideBranchesDelete's Through), in
// the caller's transaction, where no step of the task can run again (the move into its retro, the
// release of an end hold into it, a leave, a new generation, a version that removes the task). No
// way after a fix and no stop deletes them: a step beside the fix can push to its branch after the
// fix's end push, and nothing records what that push took, so each branch stays until then; the step
// a way 2 runs again takes its commits from its own (keptFacts), and after a stop that work can be
// the only copy. The row names the latest fix and not the whole task, because the row can wait on
// GitHub while the same task runs again in the generation: a later version adds it again, or a move
// back takes it out of its retro. Each fix it opens then numbers above that latest fix
// (record.Store.PutFix), so the row never deletes the branches of a fix it did not see. A task that
// never had a fix writes nothing, and so does one whose branches cannot be named (besideBranchOf). It
// takes the task's id, so it names a task a version removes.
func (e *Engine) queueBesideDelete(ctx context.Context, tx pgx.Tx, issue record.Issue, task string) error {
	latest, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, task)
	if err != nil || latest == nil {
		return err
	}
	if _, err := claim.BesideTaskPrefix(issue.Key, issue.Generation, task); err != nil {
		e.log.Error("workflow: the branches beside this task's fixes cannot be named, so none is deleted", "issue", issue.Key,
			"generation", issue.Generation, "task", task, "error", err)
		return nil
	}
	deletion := record.BesideBranchesDelete{Generation: issue.Generation, Task: task, Through: latest.Fix}
	if err := e.enqueue(ctx, tx, issue.Key, deletion); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the branches beside a fix are to be deleted", "issue", issue.Key, "generation", issue.Generation,
		"task", task, "through", deletion.Through)
	return nil
}

// queueTaskEnd queues the deletion of the branches beside each fix w's task holds now
// (queueBesideDelete) where no step of the task can run again because it moves into its retro with
// no hold (transition, StartHeldRetro).
func (e *Engine) queueTaskEnd(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) error {
	if !w.Stored {
		return nil
	}
	return e.queueBesideDelete(ctx, tx, issue, w.Task)
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
