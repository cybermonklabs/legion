package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// stopTask stops w's task of issue where it stands, in the caller's transaction, or answers why
// the task cannot stop there (stopRefusal) and writes nothing. Every stop of a task comes here, the
// owner's (taskStopped) and a failed step's with no step to take its work back (stopUnreturned), so
// no stop writes a hold the record cannot hold. A run with no plan rows stands before its first step,
// the one place taskStopped stops such a run: once the stop is judged, the plan that step would walk
// is settled (planFirstStep), the newest plan its owner gave or the project's default steps, and the
// task stops there, standing stopped before its first step (waitForFirstStep brings it back).
//
// The stop itself: the end of the step the issue stands in is named as end says (nameEnd), which
// the fact's settle writes, since the stopped task stands in no step (standingStep reads its plan's
// hold), the task keeps its place and becomes stopped with why (record.Store.StopTask), and the
// issue is held from the phase that place derives for the stopped task (record.HoldTaskStopped), as
// a failed claim's hold is written (claimFailed). The hold keeps the task's place (moveTask) and the
// slot of the phase it is from (record.KeepsSlot reads record.StandsAt), and it writes no Dispatch
// status, since held has none (PhaseStatus). The process of the step's role is stopped with its
// session kept (the op step_stop, whose source is the daemon's, said as its reason), except for
// accepting, whose architect is the owner itself and runs no process of Legion's for the step, and a
// phase no role works. The stop's row names the claim of the agent that ran the step, read here in
// the fact's transaction (workerClaim), and the outbox stops that claim: a restart of the task before
// the row runs enters the first step and writes its agent on the role's row, and a row resolved when
// it runs would stop that agent and leave the stopped step's running. The task's state is the
// caller's to have read: a task done or left is no task to stop (StopTask refuses it).
//
// issue may be the copy read for the stopped task of a plan of several (view): the issue is then
// written where its tasks put it (putIssue), held for the stop only while no other task runs, so
// the stop holds the tasks that need the stopped one and each other task goes on; and the stop's
// row names the agent of that task's step (stepToken).
func (e *Engine) stopTask(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, why, said string, end StepEnd) (*intake.Refusal, error) {
	if refusal := stopRefusal(issue, w.Task); refusal != nil {
		return refusal, nil
	}
	if !w.Stored {
		if _, err := e.planFirstStep(ctx, tx, issue); err != nil {
			return nil, err
		}
		// The walk of the stopped task of the plan the first step settled, named, since a plan of
		// several tasks has no one walk.
		read := issue
		read.ViewTask = w.Task
		var err error
		if w, err = e.walk(ctx, tx, read); err != nil {
			return nil, err
		}
	}
	from := record.StandsAt(issue)
	if step, ok := standingStep(w, issue); ok {
		if err := e.nameEnd(ctx, issue, step, end); err != nil {
			return nil, err
		}
	}
	if err := e.store.StopTask(ctx, tx, issue.Key, issue.Generation, w.Task, why); err != nil {
		return nil, err
	}
	issue.Phase, issue.Hold = phase.Held, &record.Hold{From: from, Reason: record.HoldTaskStopped}
	if err := e.WriteIssue(ctx, tx, issue); err != nil {
		return nil, err
	}
	// The stop takes the issue into held, which is a phase change and so a movement (the rule in
	// dispatchIssue), as a failed claim's hold is.
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return nil, err
	}
	e.logOnCommit(ctx, "workflow: a task is stopped", "issue", issue.Key, "tree", issue.Tree, "task", w.Task, "from", from, "reason", why)
	role, err := StoppedStepRole(ctx, e.store, tx, issue)
	if err != nil || role == "" {
		return nil, err
	}
	// The agent of the stopped step: on a copy of the issue read for one task of a plan of
	// several, the agent of that task's step (stepToken), since the role's phases row names the
	// agent its latest entry reached, which may be another task's.
	var agent claim.Token
	if issue.ViewTask != "" {
		agent, err = e.stepToken(issue, w, w.Place, role)
	} else {
		agent, err = e.workerClaim(ctx, tx, issue, role)
	}
	if err != nil {
		return nil, err
	}
	return nil, e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "step_stop", Tree: issue.Tree, Role: role, Claim: agent,
		Generation: issue.Generation, Reason: said})
}

// StoppedStepRole is the role whose process the stop of issue's task stopped (stopTask's
// step_stop): the role that works the step the task stands at while a task-stopped hold holds the
// issue (record.Store.StoppedStep, read in tx on s, since a stopped task's step is no running
// step), at step the role of the deployment that runs it, and no role for an issue held for any
// other reason or not held, for a stop at no act, and for a stop at accepting, whose architect is
// the owner itself and runs no process of Legion's for the step. That role starts again only when
// its task does: a later version of the plan that starts the task again, or a person's todo on a
// stopped root, clears the hold in the same write that queues the role's start (restartTask), and
// every other start of that role while the hold stands finishes without acting where every start
// of a role is run (the daemon outbox's start).
func StoppedStepRole(ctx context.Context, s record.Store, tx pgx.Tx, issue record.Issue) (claim.Role, error) {
	stopped, ok, err := s.StoppedStep(ctx, tx, issue)
	if err != nil || !ok || stopped.Role == claim.RoleArchitect {
		return "", err
	}
	return stopped.Role, nil
}

// rolesAt is the role of each step that works issue at p: each running step there, held or not
// (record.Issue.RunningAt), and, where issue is held at p for its stopped task, the step the stop
// ended (record.Store.StoppedStep, the architect at accepting among them). A stopped task's step
// is no running step, and its role still answers for the step the stop ended: a late report of
// it, its claim's failure and an operator's retry of it each meet that stop rather than a phase
// another role works.
func rolesAt(ctx context.Context, s record.Store, tx pgx.Tx, issue record.Issue, p phase.Phase) ([]claim.Role, error) {
	var roles []claim.Role
	for _, step := range issue.RunningAt(p) {
		roles = append(roles, step.Role)
	}
	if record.StandsAt(issue) != p {
		return roles, nil
	}
	stopped, ok, err := s.StoppedStep(ctx, tx, issue)
	if err != nil || !ok {
		return roles, err
	}
	return append(roles, stopped.Role), nil
}

// worksAt says whether role works a step of issue at p (rolesAt).
func worksAt(ctx context.Context, s record.Store, tx pgx.Tx, issue record.Issue, p phase.Phase, role claim.Role) (bool, error) {
	roles, err := rolesAt(ctx, s, tx, issue, p)
	return slices.Contains(roles, role), err
}

// stopRefusal is the refusal of a stop of task of issue where issue stands, and nil where the
// stop's hold can stand there. A root that waits on a person before its first step stands in
// awaiting_person, which no hold is from: the issues table refuses such a hold
// (issues_held_from_check), and a wait's columns outside awaiting_person (issues_awaiting_shape).
// So its task cannot stop there, and the owner changes it with a later version of its plan, or
// stops it once the person has answered (TaskAwaitsPerson). Every other place an issue of the
// workflow stands is one a hold is from, held itself among them, since a held issue's place is
// the phase its hold is from (record.StandsAt).
func stopRefusal(issue record.Issue, task string) *intake.Refusal {
	if record.StandsAt(issue) == phase.AwaitingPerson {
		return intake.TaskAwaitsPerson(issue.Key, task)
	}
	return nil
}

// stopUnreturned stops w's task of issue, whose step standing in issue.Phase failed with no step
// before it of the type implementation to take its work back (returnTarget), as stop_task
// stops it (stopTask), its step ending as end says; and tells the task's owner once, in a notice
// of kind task-stopped whose reason names the task and the step. No return happened, so no pass,
// pair, round or hook is counted. A failed step stands in the phase of its role, where a hold
// always stands, so the stop's refusal is an error.
func (e *Engine) stopUnreturned(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, end StepEnd) error {
	place := w.Place
	why := fmt.Sprintf("step %d (%s) failed and no earlier step of the type implementation can take the work", place.Position, w.Steps[place.Position-1].Role)
	said := fmt.Sprintf("task %s of %s was stopped: %s", w.Task, issue.Key, why)
	refusal, err := e.stopTask(ctx, tx, issue, w, why, said, end)
	if err != nil {
		return err
	}
	if refusal != nil {
		return fmt.Errorf("stop task %s of %s after its failed step: %s", w.Task, issue.Key, refusal.Message)
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "task-stopped", Role: claim.Role(w.Steps[place.Position-1].Role), Phase: issue.Phase, Reason: said})
}

// taskStopped is the owner's stop_task of one task of a recorded issue: the task
// stops where it stands, in the fact's own transaction, as stopTask stops it, its running step
// ending stopped, with the reason that its owner stopped it. The architect the issue's notices
// reach is told in a task-stopped notice that names the architect that made the call
// (Notice.StoppedBy): the daemon's notice executor publishes it only where that is another
// claim, since the caller is answered by its own call.
//
// Apply judged the stop already on the record as the fact reads it (requestRefusal): an
// architect that owns the issue no longer, or a stop of a run the issue has left since its route
// read it (TaskStaleGeneration), is refused there, unspent, before this reads anything.
//
// An issue the record does not hold, or one with no plan rows whose work started before Legion
// took plans, has no task to stop, and a task the plan does not hold is none either (TaskNotFound,
// which lists the plan's tasks only where it has some): an issue with no plan rows changes
// nothing. An issue that stands before its first step has no plan rows yet, and its task is one
// that is not done: its task is one of the plan that step would walk, the newest plan its owner
// gave or the project's default steps, and stopTask settles that plan as it stops the task. A task
// that plan does not hold is refused first, naming the plan's tasks, and nothing is written. A
// stop of a task already stopped changes nothing and is no refusal, so a retry of a stop already
// applied finds it stopped. A task that is done, or left the workflow with its issue before it was
// done, has nothing left to stop (FinishedTask): each is refused here, before the store's
// stop, which refuses both states itself and would fail the whole fact, and before the linger a
// root's leave begins, since the task's own state is the truer answer. A stop in a lingering tree
// changes nothing and says so, and so does a stop where its hold cannot stand: a root that waits on
// a person before its first step (stopRefusal).
func (e *Engine) taskStopped(ctx context.Context, tx pgx.Tx, fact intake.TaskStopped) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	if issue == nil {
		return intake.Result{Refusal: intake.TaskNotFound(fact.Issue, fact.Task, nil)}, nil
	}
	// The stopped task is read on the copy of the issue read for it (viewOn), which for a plan of
	// one task, or none, is the issue itself, walking its one task.
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return intake.Result{}, err
	}
	target, w, onPlan, err := viewOn(*issue, stored, fact.Task)
	if err != nil {
		return intake.Result{}, err
	}
	if !onPlan {
		var tasks []string
		for _, row := range stored.Tasks {
			tasks = append(tasks, row.Task)
		}
		return intake.Result{Refusal: intake.TaskNotFound(issue.Key, fact.Task, tasks)}, nil
	}
	switch {
	case !w.Stored && beforeItsWork(*issue):
		// The stop's task is one of the plan the issue's first step would walk: the newest its
		// owner gave, or the task default of the project's default steps.
		waiting, err := e.store.OwnersNewestPlan(ctx, tx, *issue)
		if err != nil {
			return intake.Result{}, err
		}
		tasks := []string{defaultTask}
		if waiting != nil {
			tasks = tasks[:0]
			for _, task := range waiting.Tasks {
				tasks = append(tasks, task.Task)
			}
		}
		if !slices.Contains(tasks, fact.Task) {
			return intake.Result{Refusal: intake.TaskNotFound(issue.Key, fact.Task, tasks)}, nil
		}
		w.Task = fact.Task
	case !w.Stored:
		return intake.Result{Refusal: intake.TaskNotFound(issue.Key, fact.Task, nil)}, nil
	case fact.Task != w.Task:
		return intake.Result{Refusal: intake.TaskNotFound(issue.Key, fact.Task, []string{w.Task})}, nil
	case w.State == record.TaskStopped:
		return intake.Result{}, nil
	}
	if refusal := intake.FinishedTask(issue.Key, w.Task, w.State, intake.StopOfTask); refusal != nil {
		return intake.Result{Refusal: refusal}, nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	if lingers {
		return refusedLingering(*issue, fmt.Sprintf("the owner's stop of task %s", w.Task)), nil
	}
	if target.ViewTask != "" {
		// A copy read for one task stands in that task's phase: whether a hold can stand where the
		// issue stands is the issue's own (a root waiting on a person before its first step).
		if refusal := stopRefusal(*issue, w.Task); refusal != nil {
			return intake.Result{Refusal: refusal}, nil
		}
	}
	from := record.StandsAt(target)
	why := fmt.Sprintf("task %s of %s was stopped by its owner", w.Task, issue.Key)
	refusal, err := e.stopTask(ctx, tx, target, w, why, why, stepStopped)
	if err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	// The role of the step the stop ended, read on the issue as it stood before the stop: its running
	// step where the issue stood (record.Issue.RunningAt), the stopped task's own of a plan of several,
	// and where no step of it runs there, the role of that phase (RoleFor): the architect at admitted,
	// before the task's first step. Where tasks of a plan of several wait on the stopped task, the
	// notice names them: each other task goes on (tasksWaitingOn).
	notice := record.Notice{Kind: "task-stopped", Role: RoleFor(from), Phase: from, StoppedBy: fact.By, Reason: why + ", the architect " + string(fact.By)}
	for _, step := range target.RunningAt(from) {
		if target.ViewTask == "" || step.Task == w.Task {
			notice.Role = step.Role
			break
		}
	}
	if waiting := tasksWaitingOn(stored, w.Task); target.ViewTask != "" && len(waiting) > 0 {
		notice.Reason += fmt.Sprintf(". Tasks %s need it and wait; each other task goes on.", strings.Join(waiting, ", "))
	}
	return intake.Result{}, e.notice(ctx, tx, issue.Key, notice)
}

// restartTask starts w's task of issue again at its first step, in the
// caller's transaction, for reason, which that step's worker is told. It runs on a person's todo
// on a root held for the stopped task (releaseHeldRoot), and for a later version of the plan that
// changes the stopped task or replaces it with a task of another id, where the work of the stopped
// task ran: it stood at an act of its line (record.TaskPlace.Started). A task stopped before its
// first step went out takes the way into that step instead (waitForFirstStep). issue is the record
// the caller writes, with any observation it keeps, and w is the walk of the task to start, which
// the caller read: the stopped task, or the new task of a later version, waiting at no act.
//
// The hold is cleared, the task runs at step 1 with its stop's reason gone (moveTask), and the
// issue stands in the phase of step 1. The Dispatch status of that phase is queued over the
// status Dispatch shows, not through Engine.status: a hold changes no status, so the record still
// reads the status of the phase the hold is from (releaseHeldRoot says the same of a release).
// Step 1's role is started at a raised entry (start), which writes the start of its step, and its
// entry counts as a move back, since it asks for the work again from the first step. The step the
// issue stood in ended when the task stopped (stopTask), so none is ended here. A task that ran
// raises its pass, so that a completion of this run takes a key of its own (the completion key
// names the task and its pass), and no pair, round or hook is counted, since no step failed; a
// new task keeps its counts of zero. The act is task_restarted, which the log line names: no row
// of the table moves a held issue.
//
// Its callers hand it a stopped task, or the new task of a later version, waiting: a task held
// for its stop is never done or left, since each write of done or left clears the issue's hold in
// the same write (endTaskWithLeave, and the merge of an issue that awaits it). A copy of the issue
// read for one task of a plan of several (view) is started again by restartViewed, which starts
// the task only where its needs are done.
func (e *Engine) restartTask(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, reason string) error {
	steps := w.steps()
	if !w.Stored || len(steps) == 0 {
		return fmt.Errorf("restart task %s of %s: its line %v holds no first step", w.Task, issue.Key, w.Line)
	}
	if issue.ViewTask != "" {
		return e.restartViewed(ctx, tx, issue, w, reason)
	}
	if w.State != record.TaskWaiting {
		counts, err := e.store.ReturnCounts(ctx, tx, issue.Key, issue.Generation, w.Task)
		if err != nil {
			return err
		}
		counts.Pass++
		if err := e.store.PutReturnCounts(ctx, tx, issue.Key, issue.Generation, w.Task, counts); err != nil {
			return err
		}
	}
	from, first := record.StandsAt(issue), steps[0]
	status := PhaseStatus(first)
	if status != issue.DispatchStatus {
		if err := e.enqueue(ctx, tx, issue.Key, record.OrdinaryStatusWrite(status, issue.DispatchStatus, "")); err != nil {
			return err
		}
	}
	if err := e.moveTask(ctx, tx, &issue, &w, first, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning); err != nil {
		return err
	}
	if err := e.store.SetStartReason(ctx, tx, issue.Key, issue.Generation, w.Task, reason); err != nil {
		return err
	}
	w.StartReason = reason
	issue.Hold, issue.Status = nil, status
	if err := e.WriteIssue(ctx, tx, issue); err != nil {
		return err
	}
	// The restart takes the issue out of held into a phase, which is a movement (the rule in
	// dispatchIssue).
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a stopped task starts again at its first step", "issue", issue.Key, "tree", issue.Tree, "task", w.Task,
		"act", "task_restarted", "from", from, "to", first, "reason", reason)
	// The role of the task's first step (Walk.RoleAt), so a first step of a role the deployment
	// defines starts that role again.
	role := w.RoleAt(record.TaskPlace{Kind: record.PlaceStep, Position: 1})
	if err := e.clearHandoff(ctx, tx, issue.Key, role); err != nil {
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	return e.start(ctx, tx, issue, w, role, func(at Standing) string {
		return task(issue, at, record.PhaseRow{}, pr, reason)
	}, true)
}

// waitForFirstStep ends the stop of w's task of issue, a task stopped before its first step went
// out, which stands at no act (record.TaskPlace.Started), in the caller's transaction, for reason,
// which its log line names. It runs where restartTask runs for a task whose work ran. Nothing of the
// task ran, so it goes back the way a task that was never stopped goes into its first step: it waits
// at no act again with its stop's reason gone (moveTask), the hold is cleared, and the issue stands
// in admitted, where the hold is from: no stop holds a root that waits on a person (stopRefusal).
// Nothing starts here: the design gate, the waiting line and admission decide when the first step
// starts (StartFirstPhase), and no count is raised, since no run of the task ended. Admitted has no
// Dispatch status of its own (PhaseStatus), so none is written. The issue leaves held, which is a
// movement (the rule in dispatchIssue). The act is task_released, which the log line names.
func (e *Engine) waitForFirstStep(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, reason string) error {
	if err := e.moveTask(ctx, tx, &issue, &w, phase.Admitted, record.TaskPlace{}, record.TaskWaiting); err != nil {
		return err
	}
	issue.Hold = nil
	if err := e.WriteIssue(ctx, tx, issue); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a task stopped before its first step waits for that step again", "issue", issue.Key, "tree", issue.Tree,
		"task", w.Task, "act", "task_released", "to", issue.Phase, "reason", reason)
	return nil
}
