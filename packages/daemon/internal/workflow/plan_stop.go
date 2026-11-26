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

// stopTask stops s's task where it stands, in the caller's transaction, or answers why the task
// cannot stop there (stopRefusal) and writes nothing. Every stop of a task comes here, the owner's
// (taskStopped) and a failed step's with no step to take its work back (stopUnreturned), so no stop
// writes a hold the record cannot hold. A run with no plan rows stands before its first step, the
// one place taskStopped stops such a run: once the stop is judged, the plan that step would walk is
// settled (planFirstStep), the newest plan its owner gave or the project's default steps, and the
// task stops there, standing stopped before its first step (waitForFirstStep brings it back).
//
// The stop itself: the end of the step the copy stands in is named as end says (nameEnd), which
// the fact's settle writes, since the stopped task stands in no step (standingStep reads its plan's
// hold), the task keeps its place and becomes stopped with why (record.Store.StopTask), and the
// task's copy is held from the phase that place derives for the stopped task
// (record.HoldTaskStopped), as a failed claim's hold is written (claimFailed). The hold keeps the
// task's place (moveTask) and the slot of the phase it is from (record.KeepsSlot reads
// record.StandsAt), and it writes no Dispatch status, since held has none (PhaseStatus). The issue
// is written where its tasks put it (putScope), held for the stop only while no other task runs, so
// the stop holds the tasks that need the stopped one and each other task goes on. The process of
// the step's role is stopped with its session kept (the op step_stop, whose source is the daemon's,
// said as its reason), except for accepting, whose architect is the owner itself and runs no
// process of Legion's for the step, and a phase no role works. The stop's row names the claim of
// the agent that ran the step, read here in the fact's transaction: the claim the step's entry
// wrote (rowClaim) for a run with plan rows, and the claim of the role's row (workerClaim) for one
// with none. The outbox stops that claim: a restart of the task before the row runs enters the
// first step and writes its agent on the role's row, and a row resolved when it runs would stop that
// agent and leave the stopped step's running. A step that waits for its join or its agent
// (Walk.Wait, read before the stop, which ends the wait) was never delivered: its agent runs no step
// of this task, or runs another task's, so its stop queues no step_stop, which would end that other
// step. The task's state is the caller's to have read: a task done or left is no task to stop
// (StopTask refuses it).
func (e *Engine) stopTask(ctx context.Context, tx pgx.Tx, s TaskScope, why, said string, end StepEnd) (*intake.Refusal, error) {
	if refusal := stopRefusal(s.issue, s.walk.Task); refusal != nil {
		return refusal, nil
	}
	if !s.walk.Stored {
		if _, err := e.planFirstStep(ctx, tx, s.issue); err != nil {
			return nil, err
		}
		// The scope of the stopped task of the plan the first step settled.
		var err error
		if s, err = e.ScopeOf(ctx, tx, s.issue, s.walk.Task); err != nil {
			return nil, err
		}
	}
	from := record.StandsAt(s.issue)
	if step, ok := standingStep(s.walk, s.issue); ok {
		if err := e.nameEnd(ctx, s.issue, step, end); err != nil {
			return nil, err
		}
	}
	if err := e.store.StopTask(ctx, tx, s.issue.Key, s.issue.Generation, s.walk.Task, why); err != nil {
		return nil, err
	}
	wait := s.walk.Wait
	s.walk.State, s.walk.StoppedReason, s.walk.Wait, s.walk.WaitSince = record.TaskStopped, why, record.StepWaitNone, nil
	s.issue.Phase, s.issue.Hold = phase.Held, &record.Hold{From: from, Reason: record.HoldTaskStopped}
	if _, err := e.putScope(ctx, tx, &s); err != nil {
		return nil, err
	}
	issue := s.issue
	// The stop takes the issue into held, which is a phase change and so a movement (the rule in
	// dispatchIssue), as a failed claim's hold is.
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return nil, err
	}
	e.logOnCommit(ctx, "workflow: a task is stopped", "issue", issue.Key, "tree", issue.Tree, "task", s.walk.Task, "from", from, "reason", why,
		"wait", wait)
	if wait != record.StepWaitNone {
		return nil, nil
	}
	role, err := StoppedStepRole(ctx, e.store, tx, issue)
	if err != nil || role == "" {
		return nil, err
	}
	// The agent of the stopped step: the claim its entry wrote on the role's row (rowClaim), since
	// the role's phases row names the agent its latest entry reached, which may be another task's;
	// the claim of the role's row for a run with no plan rows, and for an issue whose key names no
	// claims, whose entry wrote none (enter, recordsSteps).
	var agent claim.Token
	if s.Planned() && recordsSteps(issue) {
		agent, err = e.rowClaim(s, role)
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
// stopped root, clears the hold in the same write that queues the role's start
// (startAtFirstStep), and every other start of that role while the hold stands finishes without
// acting where every start of a role is run (the daemon outbox's start).
func StoppedStepRole(ctx context.Context, s record.Store, tx pgx.Tx, issue record.Issue) (claim.Role, error) {
	stopped, ok, err := copyStoppedStep(ctx, s, tx, issue)
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
	stopped, ok, err := copyStoppedStep(ctx, s, tx, issue)
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
// the phase its hold is from (record.StandsAt), except awaiting_children: no hold is from it
// either (issues_held_from_check), and taskStopped refuses a stop there first, before it calls
// stopTask (waitingRefusal).
func stopRefusal(issue record.Issue, task string) *intake.Refusal {
	if record.StandsAt(issue) == phase.AwaitingPerson {
		return intake.TaskAwaitsPerson(issue.Key, task)
	}
	return nil
}

// stopUnreturned stops s's task, whose step standing in its copy's phase failed with no step
// before it of the type implementation to take its work back (returnTarget), as stop_task
// stops it (stopTask), its step ending as end says; and tells the task's owner once, in a notice
// of kind task-stopped whose reason names the task and the step. No return happened, so no pass,
// pair, round or hook is counted. A failed step stands in the phase of its role, where a hold
// always stands, so the stop's refusal is an error.
func (e *Engine) stopUnreturned(ctx context.Context, tx pgx.Tx, s TaskScope, end StepEnd) error {
	w, issue := s.walk, s.issue
	place := w.Place
	why := fmt.Sprintf("step %d (%s) failed and no earlier step of the type implementation can take the work", place.Position, w.Steps[place.Position-1].Role)
	said := fmt.Sprintf("task %s of %s was stopped: %s", w.Task, issue.Key, why)
	refusal, err := e.stopTask(ctx, tx, s, why, said, end)
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
// a person before its first step (stopRefusal), and a parent waiting in awaiting_children, which no
// hold is from either (issues_held_from_check): its stop is refused WAITING_ON_CHILDREN, naming
// its open children and the architect's levers over them, as its sign-off and its move back are
// (waitingRefusal, dispatch://INFRA-3320 R11).
//
// An issue whose group takes no plan from its owner (groupOf, takesNoPlan) takes no stop of its
// tasks from that owner either: the experiment decides how the issue is done. The stop is
// answered PLAN_NOT_TAKEN before anything else is read, and the task goes on as it stood, so a
// parent of such a group in awaiting_children is answered PLAN_NOT_TAKEN, not WAITING_ON_CHILDREN.
func (e *Engine) taskStopped(ctx context.Context, tx pgx.Tx, fact intake.TaskStopped) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	if issue == nil {
		return intake.Result{Refusal: intake.TaskNotFound(fact.Issue, fact.Task, nil)}, nil
	}
	group, err := e.groupOf(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	if takesNoPlan(group) {
		return intake.Result{Refusal: intake.TaskNotStopped(issue.Key, group)}, nil
	}
	// The stopped task is read on its own scope (scopeOn), whatever the plan's count of tasks; a run
	// with no plan rows is its issue (noPlanScope).
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return intake.Result{}, err
	}
	target, onPlan := noPlanScope(*issue), true
	if stored != nil {
		target, onPlan = scopeOn(*issue, stored, fact.Task)
	}
	if !onPlan {
		var tasks []string
		for _, row := range stored.Tasks {
			tasks = append(tasks, row.Task)
		}
		return intake.Result{Refusal: intake.TaskNotFound(issue.Key, fact.Task, tasks)}, nil
	}
	switch {
	case !target.walk.Stored && beforeItsWork(*issue):
		// The stop's task is one of the plan the issue's first step would walk: the newest its
		// owner gave, or the task default of the project's default steps.
		waiting, err := record.OwnersNewestPlan(ctx, e.store, tx, *issue)
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
		target.walk.Task = fact.Task
	case !target.walk.Stored:
		return intake.Result{Refusal: intake.TaskNotFound(issue.Key, fact.Task, nil)}, nil
	case target.walk.State == record.TaskStopped:
		return intake.Result{}, nil
	}
	w := target.walk
	// The task whose start the wait holds stands in awaiting_children (scopeStanding), and its stop
	// is refused with the wait's own code (dispatch://INFRA-3320 R11).
	if target.issue.Phase == phase.AwaitingChildren {
		return e.waitingRefusal(ctx, tx, target, actStopTask, claim.RoleArchitect)
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
	// The copy stands in its task's phase: whether a hold can stand where the issue stands is the
	// issue's own (a root waiting on a person before its first step).
	if refusal := stopRefusal(*issue, w.Task); refusal != nil {
		return intake.Result{Refusal: refusal}, nil
	}
	from := record.StandsAt(target.issue)
	why := fmt.Sprintf("task %s of %s was stopped by its owner", w.Task, issue.Key)
	refusal, err := e.stopTask(ctx, tx, target, why, why, stepStopped)
	if err != nil || refusal != nil {
		return intake.Result{Refusal: refusal}, err
	}
	// The role of the step the stop ended, read on the copy as it stood before the stop: its running
	// step where the copy stood (record.Issue.RunningAt), the stopped task's own, and where no step
	// of it runs there, the role of that phase (RoleFor): the architect at admitted, before the
	// task's first step. Where tasks of the plan wait on the stopped task, the notice names them:
	// each other task goes on (tasksWaitingOn).
	notice := record.Notice{Kind: "task-stopped", Role: RoleFor(from), Phase: from, StoppedBy: fact.By, Reason: why + ", the architect " + string(fact.By)}
	for _, step := range target.issue.RunningAt(from) {
		if !target.Planned() || step.Task == w.Task {
			notice.Role = step.Role
			break
		}
	}
	if waiting := tasksWaitingOn(stored, w.Task); len(waiting) > 0 {
		notice.Reason += fmt.Sprintf(". Tasks %s need it and wait; each other task goes on.", strings.Join(waiting, ", "))
	}
	return intake.Result{}, e.notice(ctx, tx, issue.Key, notice)
}

// startAtFirstStep starts s's task again at its first step, in the caller's transaction, for
// reason, which that step's worker is told, whatever the plan's count of tasks. It runs on a
// person's todo on a root held for the stopped task (releaseHeldRoot), and for a later version of
// the plan that changes the stopped task or replaces it with a task of another id, where the work
// of the stopped task ran: it stood at an act of its line (record.TaskPlace.Started). A task
// stopped before its first step went out takes the way into that step instead (waitForFirstStep).
// s is the scope the caller read, its copy carrying any observation the caller keeps: the stopped
// task, or the new task of a later version, waiting at no act.
//
// Where its work ran (record.TaskPlace.Started) its pass is raised, so a completion of this run
// takes a key of its own (the completion key names the task and its pass), and no pair, round or
// hook is counted, since no step failed; a new task keeps its counts of zero. Where a task it needs
// is not done, it waits at no act, its stop's reason gone, and starts when its needs are done
// (startReady). Else the hold is cleared, the task runs at step 1 with its stop's reason gone
// (moveTask), and the issue stands where its tasks then put it (putScope). The Dispatch status of
// that phase is queued over the status Dispatch shows, not through Engine.status: a hold changes no
// status, so the record still reads the status of the phase the hold is from (releaseHeldRoot says
// the same of a release). Step 1's role is started at a raised entry (start), which writes the
// start of its step, and its entry counts as a move back, since it asks for the work again from the
// first step. The step the task stood in ended when it stopped (stopTask), so none is ended here.
// After the start, the restart does what a move does after its start: it queues the reconcile of
// the tree's news (queueMovedNews), and it acts on the merge forward of the task's pull request as
// a move from the phase the hold is from into step 1's phase does (mergeForwardOnEntry), so a
// restart into implementing drops an ask made before the hold, which would otherwise hold back
// every later ask of its head (mergeForward asks once for each head). The act is task_restarted,
// which the log line names: no row of the table moves a held issue.
//
// The start of step 1 is a phase start like any other (dispatch://INFRA-3320 R9): where a Dispatch
// child of the issue is open and its pull request has not merged, the task moves to step 1 all the
// same, and the issue waits in awaiting_children holding that start, with the task step 1's worker
// would have been given and the restart's reason as its cause (awaitChildren), in place of the
// start, so no role is started and Dispatch is written the status of awaiting_children over the
// one it shows. The guard reads the start alone (childrenHoldingStart), since the restart raises
// it from held whatever place the stop stood at, the retro or the merge among them. The return
// starts step 1 once no child is open (ReturnFromChildren).
//
// Its callers hand it a stopped task, or the new task of a later version, waiting: a task held
// for its stop is never done or left, since each write of done or left clears the issue's hold in
// the same write (endTaskWithLeave, and the merge of an issue that awaits it).
func (e *Engine) startAtFirstStep(ctx context.Context, tx pgx.Tx, s TaskScope, reason string) error {
	steps := s.walk.steps()
	if !s.walk.Stored || len(steps) == 0 {
		return fmt.Errorf("restart task %s of %s: its line %v holds no first step", s.walk.Task, s.issue.Key, s.walk.Line)
	}
	row, ok := s.plan.TaskOf(s.walk.Task)
	if !ok {
		return fmt.Errorf("restart task %s of %s: its plan holds no such task", s.walk.Task, s.issue.Key)
	}
	if s.walk.Place.Started() {
		counts, err := e.store.ReturnCounts(ctx, tx, s.issue.Key, s.issue.Generation, s.walk.Task)
		if err != nil {
			return err
		}
		counts.Pass++
		if err := e.store.PutReturnCounts(ctx, tx, s.issue.Key, s.issue.Generation, s.walk.Task, counts); err != nil {
			return err
		}
	}
	from := record.StandsAt(s.issue)
	if !needsDone(s.plan, row) {
		if err := e.moveTask(ctx, tx, &s, phase.Admitted, record.TaskPlace{}, record.TaskWaiting); err != nil {
			return err
		}
		s.issue.Hold = nil
		if _, err := e.putScope(ctx, tx, &s); err != nil {
			return err
		}
		if err := e.store.MarkIssueMoved(ctx, tx, s.issue.Key, e.now()); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: a stopped task waits for the tasks it needs", "issue", s.issue.Key, "tree", s.issue.Tree, "task", s.walk.Task,
			"act", "task_released", "needs", s.walk.Needs, "reason", reason)
		return nil
	}
	// The pull request of the task, read once: the guard of the start reads it, the start's task
	// names it (readPull), and the merge forward of the release acts on it (mergeForwardOnEntry).
	pr, err := e.taskPullRequest(ctx, tx, s)
	if err != nil {
		return err
	}
	open, err := e.childrenHoldingStart(ctx, tx, s.issue, steps[0], pr)
	if err != nil {
		return err
	}
	first := record.TaskPlace{Kind: record.PlaceStep, Position: 1}
	if err := e.moveTask(ctx, tx, &s, steps[0], first, record.TaskRunning); err != nil {
		return err
	}
	s.issue.Hold = nil
	// The held start keeps its task as the restart composed it (childrenHold.Task), and the task
	// row keeps no start reason for it: the return gives the task of its own.
	held := s.issue
	told := func(at Standing) string { return task(held, at, record.PhaseRow{}, reason) }
	if len(open) > 0 {
		if status := PhaseStatus(phase.AwaitingChildren); status != s.issue.DispatchStatus {
			if err := e.enqueue(ctx, tx, s.issue.Key, record.OrdinaryStatusWrite(status, s.issue.DispatchStatus, "")); err != nil {
				return err
			}
			s.issue.Status = status
		}
		at, err := e.standingPart(ctx, tx, s, readPull(pr))
		if err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: a stopped task's restart at its first step waits on the issue's open Dispatch children", "issue", s.issue.Key,
			"tree", s.issue.Tree, "task", s.walk.Task, "act", "task_restarted", "from", from, "holds", steps[0], "reason", reason)
		waiting := s
		waiting.issue.Phase = from
		return e.awaitChildren(ctx, tx, waiting, childrenHold{Start: steps[0], Task: told(at), Cause: reason, Open: open})
	}
	if err := e.store.SetStartReason(ctx, tx, s.issue.Key, s.issue.Generation, s.walk.Task, reason); err != nil {
		return err
	}
	s.walk.StartReason = reason
	if at, _ := derivedStanding(s.issue, scopeWalks(s), false); PhaseStatus(at) != "" && PhaseStatus(at) != s.issue.DispatchStatus {
		status := PhaseStatus(at)
		if err := e.enqueue(ctx, tx, s.issue.Key, record.OrdinaryStatusWrite(status, s.issue.DispatchStatus, "")); err != nil {
			return err
		}
		s.issue.Status = status
	}
	written, err := e.putScope(ctx, tx, &s)
	if err != nil {
		return err
	}
	// The restart takes the issue out of held into a phase, which is a movement (the rule in
	// dispatchIssue).
	if err := e.store.MarkIssueMoved(ctx, tx, s.issue.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a stopped task starts again at its first step", "issue", s.issue.Key, "tree", s.issue.Tree, "task", s.walk.Task,
		"act", "task_restarted", "from", from, "to", s.issue.Phase, "issuePhase", written.Phase, "reason", reason)
	// The role of the task's first step (Walk.RoleAt), so a first step of a role the deployment
	// defines starts that role again.
	role := s.walk.RoleAt(first)
	if err := e.clearHandoff(ctx, tx, s.issue.Key, role); err != nil {
		return err
	}
	issue := s.issue
	if err := e.start(ctx, tx, s, role, readPull(pr), func(at Standing) string {
		return task(issue, at, record.PhaseRow{}, reason)
	}, phase.Held, true); err != nil {
		return err
	}
	if err := e.queueMovedNews(ctx, tx, issue); err != nil {
		return err
	}
	return e.mergeForwardOnEntry(ctx, tx, s, pr)
}

// scopeWalks is the walk of each task of s's plan, in task id order, with s's own walk in place of
// its task's as the plan was read: what the issue's derivation reads once s's task has moved.
func scopeWalks(s TaskScope) []Walk {
	walks := walksOf(s.issue.Key, s.plan)
	for i := range walks {
		if walks[i].Task == s.walk.Task {
			walks[i] = s.walk
		}
	}
	return walks
}

// waitForFirstStep ends the stop of s's task, a task stopped before its first step went out, which
// stands at no act (record.TaskPlace.Started), in the caller's transaction, for reason, which its
// log line names. It runs where startAtFirstStep runs for a task whose work ran. Nothing of the task
// ran, so it goes back the way a task that was never stopped goes into its first step: it waits at
// no act again with its stop's reason gone (moveTask), the hold is cleared, and the issue stands
// where its tasks put it (putScope): admitted, where the hold is from, for a task alone, since no
// stop holds a root that waits on a person (stopRefusal). Nothing starts here: the design gate, the
// waiting line and admission decide when the first step starts (StartFirstPhase), and no count is
// raised, since no run of the task ended. Admitted has no Dispatch status of its own
// (PhaseStatus), so none is written. The issue leaves held, which is a movement (the rule in
// dispatchIssue). The act is task_released, which the log line names.
func (e *Engine) waitForFirstStep(ctx context.Context, tx pgx.Tx, s TaskScope, reason string) error {
	if err := e.moveTask(ctx, tx, &s, phase.Admitted, record.TaskPlace{}, record.TaskWaiting); err != nil {
		return err
	}
	s.issue.Hold = nil
	if _, err := e.putScope(ctx, tx, &s); err != nil {
		return err
	}
	if err := e.store.MarkIssueMoved(ctx, tx, s.issue.Key, e.now()); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a task stopped before its first step waits for that step again", "issue", s.issue.Key, "tree", s.issue.Tree,
		"task", s.walk.Task, "act", "task_released", "to", s.issue.Phase, "reason", reason)
	return nil
}
