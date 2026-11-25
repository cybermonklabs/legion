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
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "ci-failed", Role: claim.RoleArchitect, Phase: record.StandsAt(issue), Reason: fmt.Sprintf(
		"Check %s fails at %s of task %s of %s, and no fix of task %s can run: Legion cannot name a branch beside it (%v). Decide with the person: send the task back with request_backward_move, or stop it with stop_task.",
		check, pass.head, w.Task, issue.Key, w.Task, err)})
}

// UpdatePushes is where each role of issue key pushes at an update round's ask (supervise.UpdatePush,
// the pod update's askRound): a step whose open start is bound to a branch beside a fix
// (record.StepStart.PushBranch) pushes there, the accepting owner's named start among them, and its
// binding stands until that step's run ends, also after the fix ended with its way pending; the
// implementer pushes to its own branch F while its fix runs (OwnFix), and pushes nothing while the
// fix reads its job (FixReads), since neither its copy nor F exists yet and the task's branch has
// one writer then. Every other role gets the zero push, main's ask. An issue with no fix, and one
// whose run has no plan rows, gets nothing but zero pushes.
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
	for i, at := range w.steps() {
		start, err := e.store.StepStarts(ctx, tx, issue.Key, record.StepPlace{Task: w.Task, Kind: record.PlaceStep, Place: i + 1})
		if err != nil {
			return nil, err
		}
		if role := RoleFor(at); start.Run == record.RunOpen && start.PushBranch != "" && role != claim.RoleReviewer {
			pushes[role] = supervise.UpdatePush{Branch: start.PushBranch}
		}
	}
	fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil {
		return pushes, err
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

// besideStepSentence is what the task of a step that starts beside fix says of it (fixLines): the
// fix runs, its failure is the fix's, and, where the step has a branch of its own (branch, ""
// for the reviewer, which creates no commit), that it pushes only there, names that branch and its
// commits in its report, records each result at the pull request's head, and what it does where
// GitHub refuses the push; and, where the daemon holds a push of that branch (TaskFix.Pushed, the
// role the branch is of), where those commits stand for a new pod.
func besideStepSentence(key string, fix record.TaskFix, branch string) string {
	sentence := fmt.Sprintf(" A fix of this task runs: check %s failed at %s. Leave that failure to the fix.", fix.Check, fix.Head)
	if branch == "" {
		return sentence
	}
	sentence += fmt.Sprintf(" Until this step ends, push nothing to the branch of the task: push your commits to your own branch %s with %s, and name that branch and each commit in your report.",
		branch, workspace.BesidePushCommand(branch)) + recordAtHead(fix.Head)
	if _, _, _, _, role, ok := claim.MatchBesideBranch(branch, []string{fix.Task}); ok {
		if pushed := fix.Pushed[string(role)]; pushed != "" {
			sentence += fmt.Sprintf(" Your pushed commits are on %s at %s: on a new pod, run jj -R \"$LEGION_WORKSPACE\" git fetch --remote origin --branch %s first and go on at %s@origin.",
				branch, pushed, branch, branch)
		}
	}
	return sentence
}

// recordAtHead is the two sentences a step beside a fix that pushes is told of where it records
// and what it does where GitHub refuses its push: each result, and its head, at head, the pull
// request's, never at a commit of its own branch.
func recordAtHead(head string) string {
	return fmt.Sprintf(" Record each result, and name your head, at %s, the head of the pull request, never at a commit of your own branch, where a result counts for nothing. "+
		"If GitHub refuses that push, push nothing else, and name each commit by its id, with GitHub's answer, in your report.", head)
}
