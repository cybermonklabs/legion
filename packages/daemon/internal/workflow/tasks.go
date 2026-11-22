package workflow

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// view is the copy of issue read for task of its run's plan, and the walk of that task: the copy
// stands in the task's own phase and hold (taskStanding), not the issue's, and names the task
// (record.Issue.ViewTask), so a handler of a fact that concerns one step reads it once and then
// runs as it runs on an issue of one task. An issue whose plan holds one task, or none, is its own
// view and walks as it always did, whatever task is named: its walk is its run's (walk). A task a
// plan of several does not hold is an error.
func (e *Engine) view(ctx context.Context, tx pgx.Tx, issue record.Issue, task string) (record.Issue, Walk, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return record.Issue{}, Walk{}, err
	}
	view, w, ok, err := viewOn(issue, stored, task)
	if err == nil && !ok {
		err = fmt.Errorf("read %s at generation %d for task %s: its plan holds no such task", issue.Key, issue.Generation, task)
	}
	return view, w, err
}

// viewOn is view on stored, the plan of issue's run, which the caller read: ok is false where its
// plan holds several tasks and none is task.
func viewOn(issue record.Issue, stored *record.StoredPlan, task string) (record.Issue, Walk, bool, error) {
	issue.ViewTask = ""
	if stored == nil || len(stored.Tasks) <= 1 {
		if stored == nil {
			return issue, todaysWalk(), true, nil
		}
		w, err := walkIn(issue, stored)
		return issue, w, err == nil, err
	}
	at := slices.IndexFunc(stored.Tasks, func(row record.PlanTask) bool { return row.Task == task })
	if at < 0 {
		return issue, Walk{}, false, nil
	}
	w := walkOfTask(issue.Key, stored, at, mergingTask(stored))
	issue.ViewTask = task
	issue.Phase, issue.Hold = taskStanding(w)
	return issue, w, true, nil
}

// viewsOf is the copy of issue read for each task of its run's plan, in task id order, with the
// walk of that task: a reader that reads every step of the issue reads each of them. An issue
// whose plan holds one task, or none, is its one view.
func (e *Engine) viewsOf(ctx context.Context, tx pgx.Tx, issue record.Issue) ([]record.Issue, []Walk, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return nil, nil, err
	}
	if stored == nil || len(stored.Tasks) <= 1 {
		view, w, _, err := viewOn(issue, stored, "")
		if err != nil {
			return nil, nil, err
		}
		return []record.Issue{view}, []Walk{w}, nil
	}
	views, walks := make([]record.Issue, len(stored.Tasks)), make([]Walk, len(stored.Tasks))
	for at, task := range stored.Tasks {
		if views[at], walks[at], _, err = viewOn(issue, stored, task.Task); err != nil {
			return nil, nil, err
		}
	}
	return views, walks, nil
}

// pullRequestOf is the pull request of the work issue is read for: for a copy read for one task
// of a plan of several (view), that task's own row (record.Store.PullRequestOfTask), nil where it
// has none; for the issue itself, the issue's (record.Store.PullRequest), as every reader read it
// before tasks had pull requests of their own.
func (e *Engine) pullRequestOf(ctx context.Context, tx pgx.Tx, issue record.Issue) (*record.PullRequest, error) {
	if issue.ViewTask != "" {
		return e.store.PullRequestOfTask(ctx, tx, issue.Key, issue.ViewTask)
	}
	return e.store.PullRequest(ctx, tx, issue.Key)
}

// viewOfPullRequest is the copy of issue read for the task whose branch pr is on
// (record.StoredPlan.TaskOfBranch, the rule every read of a pull request's task reads by,
// record.PullRequest.Task), and the walk of that task: the one reading of a fact of a pull request
// that reaches its task, so a fact of one task's pull request moves that task and no other, and
// one of a done task's moves none, its copy standing in done. An issue whose plan holds one task,
// or none, is its own view, whatever branch the pull request is on. ok is false where its plan
// holds several tasks and the branch is no one task's: a pull request whose facts move nothing,
// which is logged.
func (e *Engine) viewOfPullRequest(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest) (record.Issue, Walk, bool, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return record.Issue{}, Walk{}, false, err
	}
	if stored == nil || len(stored.Tasks) <= 1 {
		return viewOn(issue, stored, "")
	}
	task, ok := stored.TaskOfBranch(issue.Key, pr.Branch)
	if !ok {
		e.logOnCommit(ctx, "workflow: a fact of a pull request on a branch that no one task of its issue holds moves no task",
			"issue", issue.Key, "pullRequest", pr.Number, "branch", pr.Branch, "tasks", stored.TasksOfBranch(issue.Key, pr.Branch))
		return issue, Walk{}, false, nil
	}
	return viewOn(issue, stored, task)
}

// issueOfPullRequest is the issue pr is recorded for, read for the task whose branch pr is on
// (viewOfPullRequest): the issue each handler of a fact of the pull request acts on. It is nil
// where the record holds no such issue, and where the issue's plan holds several tasks and the
// branch is no one task's, so the fact records the pull request and moves nothing.
func (e *Engine) issueOfPullRequest(ctx context.Context, tx pgx.Tx, pr record.PullRequest) (*record.Issue, error) {
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return nil, err
	}
	view, _, ok, err := e.viewOfPullRequest(ctx, tx, *issue, pr)
	if err != nil || !ok {
		return nil, err
	}
	return &view, nil
}

// openStepsOfPullRequest is the steps of issue that take work (OpenStepsOf) of each task whose
// branch pr is on (record.StoredPlan.TasksOfBranch): the steps a settlement of the pull request's
// checks is the business of, and no step of a task on another branch. Every step of an issue whose
// plan holds one task, or none, is.
func (e *Engine) openStepsOfPullRequest(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest) ([]record.RunningStep, error) {
	open := OpenStepsOf(issue)
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil || len(stored.Tasks) <= 1 {
		return open, err
	}
	tasks := stored.TasksOfBranch(issue.Key, pr.Branch)
	return slices.DeleteFunc(open, func(step record.RunningStep) bool { return !slices.Contains(tasks, step.Task) }), nil
}

// viewOfAgent is the copy of issue read for the task whose running step agent runs
// (record.Issue.StepOfAgent, a held step among them), and the walk of that task: the one reading
// of a fact of a step that reaches its task by the agent of the claim it names. An issue whose
// plan holds one task, or none, is its own view, whatever agent is named. ok is false where its
// plan holds several tasks and agent runs none of their steps: a step that waits for its agent is
// none of them.
func (e *Engine) viewOfAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, agent claim.Agent) (record.Issue, Walk, bool, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return record.Issue{}, Walk{}, false, err
	}
	task := ""
	if stored != nil && len(stored.Tasks) > 1 {
		step, ok := issue.StepOfAgent(agent)
		if !ok {
			return issue, Walk{}, false, nil
		}
		task = step.Task
	}
	return viewOn(issue, stored, task)
}

// agentOf is the agent whose claim token is token (claim.Token.Cut), "" for a token that names
// none.
func agentOf(token claim.Token) claim.Agent {
	_, agent, _ := token.Cut()
	return agent
}

// handoffView is the copy of issue a completion is read on (fact, intake.HandoffComplete): for a
// plan of several tasks, the copy read for the task the route named, the task of the step the
// claim's agent runs (fact.Task), or, for a fact that names none, the task the claim's agent runs a
// step of now (viewOfAgent); for a plan of one task, or none, the issue itself. ok is false where
// the plan holds several tasks and neither names one of them: the completion is of no step.
func (e *Engine) handoffView(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.HandoffComplete) (record.Issue, bool, error) {
	if fact.Task == "" {
		view, _, ok, err := e.viewOfAgent(ctx, tx, issue, agentOf(fact.Claim))
		return view, ok, err
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return record.Issue{}, false, err
	}
	view, _, ok, err := viewOn(issue, stored, fact.Task)
	return view, ok, err
}

// actView is the copy of issue an owner's call on the act at answers is read on (sign_off and the
// architect's move back, at accepting): for a plan of several tasks, the copy read for task, the
// task the call names, or, where it names none, for the one task that stands at at; for a plan of
// one task, or none, the issue itself, as before tasks, whatever the call names. A named task that
// does not stand at at is refused (intake.TaskNotAtPhase), and so is a call that names none while
// two or more tasks stand there (intake.TaskRequired); with none there, the issue itself is
// answered, which stands at another act and which the call's own guard refuses.
func (e *Engine) actView(ctx context.Context, tx pgx.Tx, issue record.Issue, task string, at phase.Phase) (record.Issue, *intake.Refusal, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil || len(stored.Tasks) <= 1 {
		return issue, nil, err
	}
	if task != "" {
		view, _, ok, err := viewOn(issue, stored, task)
		switch {
		case err != nil:
			return record.Issue{}, nil, err
		case !ok:
			return record.Issue{}, intake.TaskNotAtPhase(issue.Key, task, "", at), nil
		case view.Phase != at:
			return record.Issue{}, intake.TaskNotAtPhase(issue.Key, task, view.Phase, at), nil
		}
		return view, nil, nil
	}
	var standing []record.Issue
	var ids []string
	for _, row := range stored.Tasks {
		view, _, _, err := viewOn(issue, stored, row.Task)
		if err != nil {
			return record.Issue{}, nil, err
		}
		if view.Phase == at {
			standing, ids = append(standing, view), append(ids, row.Task)
		}
	}
	switch len(standing) {
	case 0:
		return issue, nil, nil
	case 1:
		return standing[0], nil, nil
	}
	return record.Issue{}, intake.TaskRequired(issue.Key, ids, at), nil
}

// backwardView is the copy of issue a move back (fact) is read on: the owner's, at accepting, on
// the task it names or the one task there (actView); a phase worker's on the task of the step its
// claim's agent runs, which its route names (intake.BackwardMove.Task). Of a plan of one task, or
// none, it is the issue itself, as before tasks. A worker's move of a plan of several tasks that
// names no task of it, its agent running no step there, is refused and changes nothing.
func (e *Engine) backwardView(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.BackwardMove) (record.Issue, *intake.Refusal, error) {
	if fact.Requester == claim.RoleArchitect {
		return e.actView(ctx, tx, issue, fact.Task, phase.Accepting)
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil || len(stored.Tasks) <= 1 {
		return issue, nil, err
	}
	view, _, ok, err := viewOn(issue, stored, fact.Task)
	if err != nil || ok {
		return view, nil, err
	}
	return record.Issue{}, refused("BACKWARD_REFUSED", fmt.Sprintf("the %s's claim runs no step of %s, so its move back to %s changed nothing",
		fact.Requester, issue.Key, fact.To)).Refusal, nil
}

// heldView is the copy of issue read for the held task an owner's decision on a hold is of
// (retryOrEscalate): the task it names; else the task whose step the claim it names runs, the
// operator's retry of one claim; else the one held task of the plan. With none held, it is the
// first stopped task in task id order, whose hold its stop is (planHoldRefusal refuses its
// release), and with none of either the issue as it stands, which stands in no hold. A named task
// that is not held is refused (intake.TaskNotAtPhase), and so is a decision that names neither a
// task nor a claim while two or more tasks are held (intake.TaskRequired). An issue whose plan
// holds one task, or none, is its own view, as before tasks were held, whatever the decision names.
func (e *Engine) heldView(ctx context.Context, tx pgx.Tx, issue record.Issue, task string, by claim.Token) (record.Issue, *intake.Refusal, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil || len(stored.Tasks) <= 1 {
		return issue, nil, err
	}
	switch {
	case task != "":
		view, w, ok, err := viewOn(issue, stored, task)
		switch {
		case err != nil:
			return record.Issue{}, nil, err
		case !ok:
			return record.Issue{}, intake.TaskNotAtPhase(issue.Key, task, "", phase.Held), nil
		case w.State != record.TaskHeld:
			return record.Issue{}, intake.TaskNotAtPhase(issue.Key, task, view.Phase, phase.Held), nil
		}
		return view, nil, nil
	case by != "":
		view, _, ok, err := e.viewOfAgent(ctx, tx, issue, agentOf(by))
		if err != nil || ok {
			return view, nil, err
		}
		return issue, nil, nil
	}
	var held, stopped []string
	for _, row := range stored.Tasks {
		switch row.State {
		case record.TaskHeld:
			held = append(held, row.Task)
		case record.TaskStopped:
			stopped = append(stopped, row.Task)
		}
	}
	switch {
	case len(held) > 1:
		return record.Issue{}, intake.TaskRequired(issue.Key, held, phase.Held), nil
	case len(held) == 0 && len(stopped) == 0:
		return issue, nil, nil
	}
	view, _, _, err := viewOn(issue, stored, slices.Concat(held, stopped)[0])
	return view, nil, err
}

// putIssue writes issue, the issue itself or a copy of it read for one of its tasks (view), and
// answers the issue as written, which a caller that then asks where the issue stands reads. An
// issue whose plan holds one task, or none, is written as it is given (writeIssueOn's check). One
// whose plan holds several stands where its tasks put it (issuePhaseOfTasks): a copy read for a
// task is written as the issue in that phase and hold, with the Dispatch status of that phase
// where it has one, and read for none; and the issue itself, read for none, is refused where its
// phase or hold is not theirs, since issues.phase is derived from its tasks and from nothing else.
// A root that waits on a person before its first step stands in awaiting_person over the admitted
// its tasks derive.
func (e *Engine) putIssue(ctx context.Context, tx pgx.Tx, issue record.Issue) (record.Issue, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return record.Issue{}, err
	}
	if stored == nil || len(stored.Tasks) <= 1 {
		issue.ViewTask = ""
		w := todaysWalk()
		if stored != nil {
			if w, err = walkIn(issue, stored); err != nil {
				return record.Issue{}, err
			}
		}
		return issue, e.writeIssueOn(ctx, tx, issue, w)
	}
	at, hold := issuePhaseOfTasks(walksOf(issue.Key, stored))
	if at == phase.Admitted && issue.Phase == phase.AwaitingPerson {
		at = phase.AwaitingPerson
	}
	if issue.ViewTask == "" {
		if issue.Phase != at || !sameHold(issue.Hold, hold) {
			return record.Issue{}, fmt.Errorf("put %s at generation %d: it stands in %s (hold %+v), and its %d tasks put it in %s (hold %+v)",
				issue.Key, issue.Generation, issue.Phase, issue.Hold, len(stored.Tasks), at, hold)
		}
		return issue, e.store.PutIssue(ctx, tx, issue)
	}
	issue.ViewTask, issue.Phase, issue.Hold = "", at, hold
	if status := PhaseStatus(at); status != "" && status != issue.Status {
		if err := e.status(ctx, tx, issue, status, ""); err != nil {
			return record.Issue{}, err
		}
		issue.Status = status
	}
	if issue.CarriesRun() {
		issue.TakenBack = true
	}
	return issue, e.store.PutIssue(ctx, tx, issue)
}

// sameHold says whether a and b are one hold: none, or one from the same phase for the same reason.
func sameHold(a, b *record.Hold) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// needsDone says whether each task that task needs is done in stored, the plan of its run. A need
// the plan does not hold is never done: a later version that removes a task another needs is
// refused (plan.Validate).
func needsDone(stored *record.StoredPlan, task record.PlanTask) bool {
	for _, need := range task.Needs {
		at := slices.IndexFunc(stored.Tasks, func(row record.PlanTask) bool { return row.Task == need })
		if at < 0 || stored.Tasks[at].State != record.TaskDone {
			return false
		}
	}
	return true
}

// tasksWaitingOn is each task of stored, the plan of a run, that needs task, itself or through
// other tasks, and is neither done nor left, in task id order: the tasks a stop of task holds,
// while each other task goes on. None for no plan.
func tasksWaitingOn(stored *record.StoredPlan, task string) []string {
	if stored == nil {
		return nil
	}
	needing := map[string]bool{task: true}
	for grew := true; grew; {
		grew = false
		for _, row := range stored.Tasks {
			if needing[row.Task] {
				continue
			}
			if slices.ContainsFunc(row.Needs, func(need string) bool { return needing[need] }) {
				needing[row.Task], grew = true, true
			}
		}
	}
	var waiting []string
	for _, row := range stored.Tasks {
		if row.Task != task && needing[row.Task] && row.State != record.TaskDone && row.State != record.TaskLeft {
			waiting = append(waiting, row.Task)
		}
	}
	return waiting
}

// startReady is the one place tasks of a plan of several start, in the caller's transaction: it
// moves each task that waits and each of whose needs is done, in task id order, to its first step,
// running (moveTask, which writes that the step waits for its join), writes the issue (putIssue)
// and starts the step's role through Engine.start, the start of every step, which gives the task
// its branch and joins its needs (startTaskCopy). It answers the issue as written. A
// plan of one task, or none, starts nothing here: its one task starts at the gate's opening as
// before (transition), and so the issue is answered as given. It runs at the first step of the
// work (gateOpened), at each move of a task to done (transition), at each version of the plan taken
// after the work started (afterVersion), and after a stopped task starts again or waits
// (restartTask, through afterVersion). No setting bounds how many tasks run at one time: the plan
// bounds it with each task's needs.
func (e *Engine) startReady(ctx context.Context, tx pgx.Tx, issue record.Issue) (record.Issue, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil || len(stored.Tasks) <= 1 {
		return issue, err
	}
	var ready []string
	for _, row := range stored.Tasks {
		if row.State == record.TaskWaiting && needsDone(stored, row) {
			ready = append(ready, row.Task)
		}
	}
	if len(ready) == 0 {
		return issue, nil
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return record.Issue{}, err
	}
	written := issue
	for _, id := range ready {
		view, w, err := e.view(ctx, tx, written, id)
		if err != nil {
			return record.Issue{}, err
		}
		steps := w.steps()
		if len(steps) == 0 {
			return record.Issue{}, fmt.Errorf("start task %s of %s: its line %v holds no first step", id, issue.Key, w.Line)
		}
		first := record.TaskPlace{Kind: record.PlaceStep, Position: 1}
		if err := e.moveTask(ctx, tx, &view, &w, steps[0], first, record.TaskRunning); err != nil {
			return record.Issue{}, err
		}
		view.Hold = nil
		if written, err = e.putIssue(ctx, tx, view); err != nil {
			return record.Issue{}, err
		}
		if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
			return record.Issue{}, err
		}
		e.logOnCommit(ctx, "workflow: a task whose needs are done starts at its first step", "issue", issue.Key, "tree", issue.Tree, "task", id,
			"needs", w.Needs, "to", steps[0], "issuePhase", written.Phase)
		role := w.RoleAt(first)
		if err := e.clearHandoff(ctx, tx, issue.Key, role); err != nil {
			return record.Issue{}, err
		}
		view.Status, view.TakenBack = written.Status, written.TakenBack
		if err := e.start(ctx, tx, view, w, role, func(at Standing) string {
			return task(view, at, record.PhaseRow{}, pr, "")
		}, false); err != nil {
			return record.Issue{}, err
		}
	}
	return written, nil
}

// restartViewed is restartTask of w's task of a plan of several tasks, read for that task (view),
// for reason: the task stopped by its owner, or by a failed step, that a later version changes.
// Where its work ran (record.TaskPlace.Started) its pass is raised, so a completion of its next run
// takes a key of its own. Where each task it needs is done, it starts again at its first step,
// running, its stop's reason gone, and that step's role is started at a raised entry that counts as
// a move back (start). Else it waits at no act, its stop's reason gone, and starts when its needs
// are done (startReady). Either way the issue is written where its tasks put it (putIssue): the
// stop of one task holds the issue only while no other task runs.
func (e *Engine) restartViewed(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, reason string) error {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return err
	}
	at := slices.IndexFunc(stored.Tasks, func(row record.PlanTask) bool { return row.Task == w.Task })
	if at < 0 {
		return fmt.Errorf("restart task %s of %s: its plan holds no such task", w.Task, issue.Key)
	}
	if w.Place.Started() {
		counts, err := e.store.ReturnCounts(ctx, tx, issue.Key, issue.Generation, w.Task)
		if err != nil {
			return err
		}
		counts.Pass++
		if err := e.store.PutReturnCounts(ctx, tx, issue.Key, issue.Generation, w.Task, counts); err != nil {
			return err
		}
	}
	from := record.StandsAt(issue)
	if !needsDone(stored, stored.Tasks[at]) {
		if err := e.moveTask(ctx, tx, &issue, &w, phase.Admitted, record.TaskPlace{}, record.TaskWaiting); err != nil {
			return err
		}
		issue.Hold = nil
		if _, err := e.putIssue(ctx, tx, issue); err != nil {
			return err
		}
		if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: a stopped task waits for the tasks it needs", "issue", issue.Key, "tree", issue.Tree, "task", w.Task,
			"act", "task_released", "needs", w.Needs, "reason", reason)
		return nil
	}
	first := record.TaskPlace{Kind: record.PlaceStep, Position: 1}
	if err := e.moveTask(ctx, tx, &issue, &w, w.steps()[0], first, record.TaskRunning); err != nil {
		return err
	}
	if err := e.store.SetStartReason(ctx, tx, issue.Key, issue.Generation, w.Task, reason); err != nil {
		return err
	}
	w.StartReason = reason
	issue.Hold = nil
	written, err := e.putIssue(ctx, tx, issue)
	if err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a stopped task starts again at its first step", "issue", issue.Key, "tree", issue.Tree, "task", w.Task,
		"act", "task_restarted", "from", from, "to", issue.Phase, "reason", reason)
	role := w.RoleAt(first)
	if err := e.clearHandoff(ctx, tx, issue.Key, role); err != nil {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	issue.Status, issue.TakenBack = written.Status, written.TakenBack
	return e.start(ctx, tx, issue, w, role, func(at Standing) string {
		return task(issue, at, record.PhaseRow{}, pr, reason)
	}, true)
}
