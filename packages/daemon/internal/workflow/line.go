package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Walk is the line of phases one task of an issue's run walks, and where that task stands on it. A
// run with plan rows (record.Store.Plan at the issue's generation) walks the line of each of its
// tasks' steps (plan.Line): each step runs as the phase of its role, then Legion's own retro and
// merge where the task's work merges by itself (Merges). A task with a sub-owner has no steps: its
// line starts with planning, the phase of its sub-owner's act at its parts (lineOf). A run with
// none, which is every issue that ran before plan rows existed, walks today's line, phase.Order,
// as the steps of the task default (todaysSteps), with Stored false, and its table is Table, row
// for row. Every behaviour that differs from today reads Stored and nothing else; the steps of both
// are read the same way (placeOnWalk).
//
// Line, Places and Steps are read only: an unstored walk's Line is phase.Order itself, its Places
// record.TodaysPlaces and its Steps todaysSteps itself.
type Walk struct {
	// Stored says whether the run has plan rows.
	Stored bool
	// Task is the id of the walk's task: a task of the plan, or defaultTask where the run has
	// none, which is the task its steps are recorded under (StepID).
	Task string
	// Branch is the GitHub branch the task works on (record.PlanTask.BranchOf), whose working copy
	// a start of the task names (taskWorkingCopy); "" where the run has no plan rows.
	Branch string
	// Text is the text of the task's block (record.PlanTask.BlockText), which a step's worker is
	// told (standingPart); "" where the run has no plan rows.
	Text string
	// Steps are the task's steps, by place from 1: todaysSteps where the run has no plan rows.
	Steps []record.PlanStep
	// Needs are the tasks the task needs, each by its id (record.PlanTask.Needs), which the run's
	// plan node carries (planNode); nil where it needs none or the run has no plan rows.
	Needs []string
	// Line is the phases the task walks: one for each step, then retro and merging where the task
	// merges; a task with a sub-owner's starts with planning.
	Line []phase.Phase
	// Places is the place of each phase of Line, in its order (record.LinePlaces): the one reading
	// of where a position of the line stands, which every reader of the line's places reads
	// (PlaceOf, startPlace, backwardPlace, issuePhaseOf, phaseAt and the walk's table).
	Places []record.TaskPlace
	// Merges says whether the task's work merges by itself under the plan in force
	// (record.StoredPlan.Merging: its block says merges, or it is a task of the top level that no
	// task needs), so its line ends with Legion's retro and merge; any other task is done at its
	// last step. It is true for a run with no plan rows and for a plan of one task.
	Merges bool
	// State and Place are where the task stands (record.PlanTask); zero where the run has no
	// plan rows, whose place is the issue's phase alone.
	State record.TaskState
	Place record.TaskPlace
	// StoppedReason is why a stopped task was stopped (record.PlanTask.StoppedReason), by its owner
	// or by a failed step; "" for a task that is not stopped.
	StoppedReason string
	// HoldReason is why a held task is held (record.PlanTask.HoldReason): "" for a failed claim,
	// record.HoldEscalated or record.HoldForChildren; "" for a task that is not held.
	HoldReason record.HoldReason
	// Wait and WaitSince are what the step at the task's place waits for before its start is
	// delivered, and since when (record.PlanTask.StepWait, StepWaitSince): none for a step whose
	// start was delivered, and the agent for a step whose agent does a step of another task
	// (Engine.start). A step that waits stands in no step (standingStep): nothing of it started.
	Wait      record.StepWait
	WaitSince *time.Time
	// StartReason is why the step the task stands at was started (record.PlanTask.StartReason): a
	// start again at its first step or a move back, "" for a first start and after any later move:
	// a step that starts after a wait (startAfterWait) tells it and counts as a move back, as that
	// start would have, had it waited for nothing.
	StartReason string
	// StartFrom is the phase the move into the start that waits at the task's place left
	// (record.PlanTask.StartFrom), which that start keeps when it waits (keepStartFrom): the step
	// that starts after the wait (startAfterWait) raises its entry with it (enter's from), as that
	// start would have, had it waited for nothing. Every move clears it.
	StartFrom phase.Phase
	// Version is the version of the plan in force (record.StoredPlan.Version): where the plan came
	// from, its document and its number, which the run's plan node names (planNode); zero where
	// the run has no plan rows.
	Version record.PlanVersion
}

// todaysSteps is the steps of the task default of a run with no plan rows, which walks today's
// line, phase.Order: the default steps of a project that names none (config.Project.RunsByDefault),
// never the configured project's, so such a run walks today's line whatever `default_steps` says,
// and its places, and the ids of its records, stay where the release before put them. Each takes
// the type of its role among Legion's own (plan.BuiltinRoles), whatever the deployment defines.
// They are read once.
var todaysSteps = planSteps(plan.Deployment{Roles: plan.BuiltinRoles()}, "", 0, defaultTask, config.Project{}.RunsByDefault())

// todaysWalk is the walk of a run with no plan rows: the task default walking todaysSteps on
// today's line, phase.Order, at today's places (record.TodaysPlaces), which merges, with Stored
// false.
func todaysWalk() Walk {
	return Walk{Task: defaultTask, Steps: todaysSteps, Line: phase.Order, Places: record.TodaysPlaces, Merges: true}
}

// walksOf is the walk of each task of stored, the plan of the run of the issue keyed key, in task
// id order (record.StoredPlan.Tasks): today's walk alone for a run with no plan rows (todaysWalk).
func walksOf(key string, stored *record.StoredPlan) []Walk {
	if stored == nil {
		return []Walk{todaysWalk()}
	}
	merging := stored.Merging()
	walks := make([]Walk, len(stored.Tasks))
	for at, task := range stored.Tasks {
		walks[at] = walkOfTask(key, stored, task, merging)
	}
	return walks
}

// walkOfTask is the walk of task, a task of stored, the plan of the issue keyed key, whose tasks
// whose work merges by itself are merging (record.StoredPlan.Merging). Its line and the places of
// that line are read here once.
func walkOfTask(key string, stored *record.StoredPlan, task record.PlanTask, merging []string) Walk {
	steps, merges, subOwned := stored.Steps[task.Task], slices.Contains(merging, task.Task), task.OwnerRole != ""
	line := lineOf(subOwned, steps, merges)
	return Walk{Stored: true, Task: task.Task, Branch: task.BranchOf(key), Text: task.BlockText, Steps: steps, Needs: task.Needs,
		Line: line, Places: record.LinePlaces(line, subOwned), Merges: merges, State: task.State, Place: task.Place,
		StoppedReason: task.StoppedReason, HoldReason: task.HoldReason, Wait: task.StepWait, WaitSince: task.StepWaitSince,
		StartReason: task.StartReason, StartFrom: task.StartFrom, Version: stored.Version}
}

// lineOf is the line of a task whose steps are steps, as its walk reads it: the phase each step
// runs as, then Legion's own retro and merge where merges says the task merges (plan.Line). The
// line of a task with a sub-owner (subOwned, its block names `owner`) starts with planning, which
// no plan names: the phase of its sub-owner's act at its parts (record.LinePlaces).
func lineOf(subOwned bool, steps []record.PlanStep, merges bool) []phase.Phase {
	line := plan.Line(namedSteps(steps), merges)
	if !subOwned {
		return line
	}
	return slices.Insert(line, 0, phase.Planning)
}

// namedSteps is steps as a plan names them (plan.Step), each its role and the key of the agent it
// names: what a walk's line is read from (plan.Line), what its plan node and a started worker's
// task write (plan.Words) and what a later version is compared with (heldPlan).
func namedSteps(steps []record.PlanStep) []plan.Step {
	named := make([]plan.Step, len(steps))
	for i, step := range steps {
		named[i] = plan.Step{Role: step.Role, Key: step.AgentKey}
	}
	return named
}

// PlaceOf is the place of the first position of w's line that runs as p (Places): a step at its
// place from 1, the retro or the merge at place 0, or the parts of a task with a sub-owner for its
// planning, where its sub-owner acts. A phase the line does not hold has no place. Two steps that
// run as step are two places, so a reader of where the task stands reads placeIn, and PlaceOf
// serves a phase that stands once in its line.
func (w Walk) PlaceOf(p phase.Phase) (record.TaskPlace, bool) {
	if at := slices.Index(w.Line, p); at >= 0 {
		return w.Places[at], true
	}
	return record.TaskPlace{}, false
}

// startPlace is the place a start of w's task moves it to, and the phase it stands in there: the
// place of the first position of its line (Places), which is its first step, or the parts of a task
// with a sub-owner. ok is false for a line that holds no phase before its retro.
func (w Walk) startPlace() (phase.Phase, record.TaskPlace, bool) {
	if len(w.steps()) == 0 {
		return "", record.TaskPlace{}, false
	}
	return w.Line[0], w.Places[0], true
}

// AgentAt is the name of the agent that does the work of place on w (record.PlanStep.AgentName), the
// one place the workflow names an act's agent: for a step, the agent of that step, so the steps of
// one key name one agent; for the retro, the agent of the task's last step of the type
// implementation, which runs it; and for the merge, the merger, one agent of the issue that does
// the merge of each task (its role's own name, whose token is the merger's claim, as
// record.RunningStep names it), so two merges of one issue run one at a time (Engine.start). It is
// "" for a walk with no plan rows, for a step of the key owner, whose claim is no name on the
// issue, and for a place the task does not hold: there the role stands for the agent
// (Engine.stepToken).
func (w Walk) AgentAt(place record.TaskPlace) claim.Agent {
	if !w.Stored {
		return ""
	}
	switch place.Kind {
	case record.PlaceStep:
		if place.Position >= 1 && place.Position <= len(w.Steps) {
			return w.Steps[place.Position-1].AgentName()
		}
	case record.PlaceRetro:
		for i := len(w.Steps) - 1; i >= 0; i-- {
			if w.Steps[i].Type == plan.TypeImplementation {
				return w.Steps[i].AgentName()
			}
		}
	case record.PlaceMerge:
		return claim.Agent(claim.RoleMerger)
	}
	return ""
}

// placeIn is where w's task stands while its issue stands in p: for a stored walk, w.Place where
// the phase it derives for a running task (issuePhaseOf) is p, and no place otherwise; for a walk
// with no plan rows, and for a stored task at no act whose issue already stands in a step (plan
// rows written while the issue stood there, before any move named a place), the place of p on the
// line (PlaceOf), the first that runs as p. moveTask keeps w.Place with the issue's phase, so a
// caller that reads the walk and the issue in one transaction finds one place.
func (w Walk) placeIn(p phase.Phase) (record.TaskPlace, bool) {
	if !w.Stored || w.Place.Kind == "" {
		return w.PlaceOf(p)
	}
	if issuePhaseOf(w, w.Place, record.TaskRunning) != p {
		return record.TaskPlace{}, false
	}
	return w.Place, true
}

// steps is the part of the line each step runs: all of it but the retro and the merge that end
// the line of a task that merges.
func (w Walk) steps() []phase.Phase {
	if n := len(w.Line); n >= 2 && w.Line[n-2] == phase.Retro && w.Line[n-1] == phase.Merging {
		return w.Line[:n-2]
	}
	return w.Line
}

// MovesBack says whether row moves w's task back by the task's own order. A stored walk compares
// the row's two places (placeRank): an earlier step by its place, every step before the retro, the
// retro before the merge, so a move from step 3 back to step 2 goes back though both run as step.
// A run with no plan rows walks today's line, so this is phase.MovesBack of the row's two phases
// for it. A task in another order than today's so counts its move back from testing to reviewing
// (the steps `implementer reviewer tester`), and not the review's approval from reviewing to
// testing, which moves it on.
func (w Walk) MovesBack(row Row) bool {
	if !w.Stored {
		return phase.MovesBack(row.From, row.To)
	}
	return row.ToPlace.Kind != "" && placeRank(row.ToPlace) < placeRank(row.FromPlace)
}

// standsPast says whether w's task, standing where its issue stands in at (placeIn), stands past
// the place of p on its line (PlaceOf): a move from there to p would move it back (MovesBack). A
// phase the line does not hold has no place, so no task stands past it. The merge forward reads
// it: whether the task stands past its implementing (mergeForwardOnEntry), and whether its
// reviewer step already ran (mergeForwardLanded).
func (w Walk) standsPast(at, p phase.Phase) bool {
	from, _ := w.placeIn(at)
	to, _ := w.PlaceOf(p)
	return w.MovesBack(Row{From: at, To: p, FromPlace: from, ToPlace: to})
}

// issuePhaseOf is the phase w's task derives at place in state, which an issue of that one task
// stands in: done for a task that is done or left the workflow before it was done
// (record.TaskLeft), whatever its place, which is the phase of an issue out of the workflow; retro
// or merging at the task's retro or merge; planning where its sub-owner acts, at its parts and its
// report (record.TaskPlace.SubOwnerActs); the phase of the position of the line whose place is a
// step place (Places); and admitted for a task at no act, which has not started. A task stopped or
// held keeps its place, and the hold overlays this phase (record.Hold.From, taskStanding). A step
// place the line does not give derives no phase. The phase of an issue of several tasks is
// issuePhaseOfTasks's, over each.
func issuePhaseOf(w Walk, place record.TaskPlace, state record.TaskState) phase.Phase {
	if state == record.TaskDone || state == record.TaskLeft {
		return phase.Done
	}
	if place.SubOwnerActs() {
		return phase.Planning
	}
	switch place.Kind {
	case record.PlaceRetro:
		return phase.Retro
	case record.PlaceMerge:
		return phase.Merging
	case record.PlaceStep:
		if at := slices.Index(w.Places, place); at >= 0 {
			return w.Line[at]
		}
		return ""
	}
	return phase.Admitted
}

// taskStanding is where an issue stands for w's task alone (record.TaskStanding) at the phase its
// place derives (issuePhaseOf). It is the phase and the hold of a copy of the issue read for that
// task (view), and the one reading of each task that issuePhaseOfTasks ranks.
func taskStanding(w Walk) (phase.Phase, *record.Hold) {
	return record.TaskStanding(issuePhaseOf(w, w.Place, w.State), w.State, w.HoldReason)
}

// issuePhaseOfTasks is the one function that derives where an issue whose run has plan rows stands
// from tasks, the walk of each of its tasks in task id order: issues.phase and the issue's hold. In
// this order:
//
//  1. Each task is done or left: done, with no hold.
//  2. A task runs: the phase of the running task whose phase comes first in today's order
//     (phase.Order), a step of a role the deployment defines at the place of implementing, with
//     no hold. The slot (record.KeepsSlot), the Dispatch status (PhaseStatus) and the issue's work
//     (record.AtWork) each read the issue's phase as one value, and each reads the least finished
//     work: one task implementing and one in review keeps the slot and shows in_progress.
//  3. No task runs, and a task is held or stopped: held, from the phase of the first such task in
//     that order, for its hold's reason, task-stopped for a stopped task (taskStanding).
//  4. Else each task that is not done waits: admitted.
//
// Of two tasks at one phase the first in task id order is taken. For one task it is where that
// task stands (taskStanding). issues.phase so names the phase of no step but one: a reader that
// asks which steps run reads the issue's running steps (record.Issue.Running).
func issuePhaseOfTasks(tasks []Walk) (phase.Phase, *record.Hold) {
	at, hold, _ := standingOfTasks(tasks)
	return at, hold
}

// standingOfTasks is issuePhaseOfTasks with the index in tasks of the task whose standing decides
// it: the running task taken, else the held or stopped task taken, else the first task that waits,
// and the first task where each is done or left; -1 for no task. A refusal of a write that
// disagrees with the tasks names that task (WriteIssue).
func standingOfTasks(tasks []Walk) (phase.Phase, *record.Hold, int) {
	running, holding := -1, -1
	runner, holder, waiter, done := -1, -1, -1, -1
	var runs phase.Phase
	var hold *record.Hold
	for i, w := range tasks {
		at, held := taskStanding(w)
		switch w.State {
		case record.TaskDone, record.TaskLeft:
			if done < 0 {
				done = i
			}
		case record.TaskRunning:
			if rank := orderRank(at); running < 0 || rank < running {
				running, runs, runner = rank, at, i
			}
		case record.TaskHeld, record.TaskStopped:
			if rank := orderRank(held.From); holding < 0 || rank < holding {
				holding, hold, holder = rank, held, i
			}
		default:
			if waiter < 0 {
				waiter = i
			}
		}
	}
	switch {
	case runner >= 0:
		return runs, nil, runner
	case holder >= 0:
		return phase.Held, hold, holder
	case waiter >= 0:
		return phase.Admitted, nil, waiter
	}
	return phase.Done, nil, done
}

// orderRank is where p stands in today's order (phase.Order), from 1, for issuePhaseOfTasks's rule:
// a step of a role the deployment defines at the place of implementing, and 0 for a phase before
// that order, the admitted of a task at no act.
func orderRank(p phase.Phase) int {
	if p == phase.Step {
		p = phase.Implementing
	}
	return slices.Index(phase.Order, p) + 1
}

// moveTask is the one writer of where a task stands: it moves w's task to place in state
// (record.Store.MoveTask), the place of the row that moves it: a move to done passes the place the
// task keeps, and a move to admitted, where a task waits for its first step (waitForFirstStep), no
// place, the task at no act. It sets issue.Phase to the phase that place derives (issuePhaseOf),
// which must be to, and issue.Running to the running steps a fresh read gives
// (record.Store.RunningSteps), in the caller's transaction and before the caller's write of issue
// (WriteIssue, or putIssue for a copy read for one task of a plan of several, which writes the
// issue in the phase its tasks derive), so a caller that reads the issue after the move reads the
// steps a fresh read gives: the moved task's step at its new place, with the role and the phase of
// that place, and none for a task moved to no act, done or left. A move into running from waiting
// or stopped is a start of the task: the store writes that its step waits for the join of its
// needs (record.StepWaitJoin), and the start that follows the move (Engine.start) joins them or ends
// that wait, and writes the wait of a step whose agent does a step of another task. Every other
// move ends any wait the task had, so the moved task's step is a running step at once. w then
// stands where the task does, its wait among it but not the moment the store wrote, which no
// reader of a walk reads, for whatever the caller reads of it after the move. A run with no plan
// rows has no task to move: its copy's phase is to, as it always was, and its one running step
// stands at to (record.RunningOf). A move to where the task already stands writes nothing. The
// caller clears or keeps the copy's hold itself. s's copy and walk are moved together, so no move
// of a task is written on a bare issue.
func (e *Engine) moveTask(ctx context.Context, tx pgx.Tx, s *TaskScope, to phase.Phase, place record.TaskPlace, state record.TaskState) error {
	issue, w := &s.issue, &s.walk
	if !w.Stored {
		issue.Phase = to
		issue.Running = record.RunningOf(*issue, false, nil)
		return nil
	}
	if derived := issuePhaseOf(*w, place, state); derived != to {
		return fmt.Errorf("move task %s of %s to %s at %+v: that place of its line %v runs as %q", w.Task, issue.Key, to, place, w.Line, derived)
	}
	if place != w.Place || state != w.State {
		if err := e.store.MoveTask(ctx, tx, issue.Key, issue.Generation, w.Task, place, state); err != nil {
			return err
		}
		wait := record.StepWaitNone
		if state == record.TaskRunning && (w.State == record.TaskWaiting || w.State == record.TaskStopped) {
			wait = record.StepWaitJoin
		}
		w.Place, w.State, w.Wait, w.WaitSince, w.StartReason, w.StartFrom = place, state, wait, nil, "", ""
	}
	issue.Phase = to
	running, err := e.store.RunningSteps(ctx, tx, *issue)
	if err != nil {
		return err
	}
	issue.Running = running
	return nil
}

// WriteIssue writes issue (record.Store.PutIssue), the issue itself and never a copy read for one
// of its tasks, which a step handler writes through putScope: the aggregate write of the workflow
// and of admission, and no other code of either package calls a store's PutIssue. issues.phase and
// the places of the run's tasks are two records of where the issue stands, and WriteIssue refuses
// a write in which they disagree: a run with plan rows stands where its tasks put it, whatever the
// plan's count of tasks (derivedStanding: issuePhaseOfTasks, a root that waits on a person before
// its first step in awaiting_person over admitted, a parent that waits on its open Dispatch
// children in awaiting_children over the phase of the start it holds, and a hold the release before
// wrote on the issue alone, where a task runs and none is held or stopped, heldBeforeOver), a hold
// from the phase its tasks' hold is from. The refusal names the phase the write stands in (a hold
// by the phase it is from, a wait on children by the start it holds) and the task whose standing
// decides the issue's (standingOfTasks): its state, its place and the phase that place stands in.
// It reads the plan in the caller's transaction, after any move of a task (moveTask), so a write
// that names its phase itself is checked against the store and not against the caller's copy. A
// run with no plan rows has only its phase, and is written as given.
func (e *Engine) WriteIssue(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return err
	}
	if stored != nil {
		walks := walksOf(issue.Key, stored)
		at, hold := derivedStanding(issue, walks, heldBeforeOver(walks))
		if issue.Phase != at || at == phase.Held && (issue.Hold == nil || hold == nil || issue.Hold.From != hold.From) {
			return writeRefusal(issue, walks)
		}
	}
	return e.store.PutIssue(ctx, tx, issue)
}

// writeRefusal is WriteIssue's refusal of issue, whose phase disagrees with tasks, the walk of each
// task of its run: the phase issue stands in, held from the phase its hold is from or in
// awaiting_children holding the start of the phase its wait holds (record.ChildrenWait.Start), and
// the state, the place and the phase of that place of the task whose standing decides the issue's
// (standingOfTasks).
func writeRefusal(issue record.Issue, tasks []Walk) error {
	at := record.StandsAt(issue)
	stands := string(at)
	switch {
	case issue.Phase == phase.AwaitingChildren && issue.ChildrenWait != nil:
		stands = "awaiting_children, holding the start of " + string(issue.ChildrenWait.Start)
	case at != issue.Phase:
		stands = "held from " + stands
	}
	_, _, decides := standingOfTasks(tasks)
	if decides < 0 {
		return fmt.Errorf("put %s at generation %d: it stands in %s, and its plan holds no task", issue.Key, issue.Generation, stands)
	}
	w := tasks[decides]
	return fmt.Errorf("put %s at generation %d: it stands in %s, and its task %s is %s at %+v, which stands in %s",
		issue.Key, issue.Generation, stands, w.Task, w.State, w.Place, issuePhaseOf(w, w.Place, w.State))
}

// row is the row of w's table that moves an issue standing in from on trigger, into target where
// a target is named (the zero place names none), whose guard admits snapshot. A stored walk takes
// the row out of the place its task stands at (Row.FromPlace) and into target (Row.ToPlace), so
// two steps that run as one phase take their own rows; its table holds a return from each step
// to each step before it (StoredTableOf), so a failed step returns its work to the step its plan
// reads (returnTarget, or the step a completion names), which is an earlier step of the task's
// line but not always implementing. A walk with no plan rows reads no place of a row: its target
// is the phase that place derives on today's line.
func (w Walk) row(from phase.Phase, trigger TriggerKind, target record.TaskPlace, snapshot Snapshot) (Row, bool) {
	named := target != (record.TaskPlace{})
	for _, row := range w.table() {
		if row.From != from || row.Trigger != trigger || (row.Guard != nil && !row.Guard(snapshot)) {
			continue
		}
		if w.Stored && (row.FromPlace != w.Place || named && row.ToPlace != target) {
			continue
		}
		if !w.Stored && named && row.To != issuePhaseOf(w, target, record.TaskRunning) {
			continue
		}
		return row, true
	}
	return Row{}, false
}

// returnsWork says whether trigger is a failed step returning its task's work to an earlier step:
// the tester's fail, a review's request for changes and the fail of a step that runs as step. A
// backward move names its own target, which a backward row of the line serves.
func returnsWork(trigger TriggerKind) bool {
	return trigger == TriggerTesterFailed || trigger == TriggerReviewRejected || trigger == TriggerStepFailed
}

// failsIn says whether trigger is the return of a step that runs as p (stepRows): the one fail a
// worker of p reports.
func failsIn(p phase.Phase, trigger TriggerKind) bool {
	for _, step := range stepRows {
		if step.from == p && step.trigger == trigger && step.to != "" {
			return true
		}
	}
	return false
}

// returnTarget is the place the failed step of w's task, standing at w.Place, returns the work to
// when the completion names none: the first step before it whose recorded type is implementation
// (record.PlanStep.Type). ok is false where no step before it has that type: the task then stops
// (stopUnreturned). A step of a role the deployment defines may stand before its task's
// implementer, and a stored type that no plan gives may stand anywhere; each reaches that answer.
func (w Walk) returnTarget() (record.TaskPlace, bool) {
	if w.Place.Kind != record.PlaceStep || w.Place.Position < 1 || w.Place.Position > len(w.Steps) {
		return record.TaskPlace{}, false
	}
	for i, step := range w.Steps[:w.Place.Position-1] {
		if step.Type == plan.TypeImplementation {
			return record.TaskPlace{Kind: record.PlaceStep, Position: i + 1}, true
		}
	}
	return record.TaskPlace{}, false
}

// returnsUnnamed says whether trigger is the fail of the step of w's stored task that issue stands
// in, whose report names no step to return to (target the zero place): its work returns to the
// step its plan reads from the steps' recorded types (returnOf). A step with no step before it has
// no row of its fail in the table at all, and its task stops the same way.
func (w Walk) returnsUnnamed(issue record.Issue, trigger TriggerKind, target record.TaskPlace) bool {
	return w.Stored && returnsWork(trigger) && target == (record.TaskPlace{}) && failsIn(issue.Phase, trigger)
}

// returnOf is the row the failed step of w's task standing where issue stands moves by on trigger
// when its completion names no step to return to (returnsUnnamed): the return of its work to the
// step its plan reads from the steps' recorded types (returnTarget), or stops, where no step
// before it can take the work and the task stops instead (stopUnreturned). A run with no plan
// rows, and every other move, keeps row, whose target for a failed step is implementing, as
// today's line's.
func (w Walk) returnOf(issue record.Issue, trigger TriggerKind, target record.TaskPlace, row Row, snapshot Snapshot) (Row, bool, error) {
	if !w.returnsUnnamed(issue, trigger, target) {
		return row, false, nil
	}
	to, found := w.returnTarget()
	if !found {
		return Row{}, true, nil
	}
	returned, ok := w.row(issue.Phase, trigger, to, snapshot)
	if !ok {
		return Row{}, false, fmt.Errorf("return the work of %s from %+v to %+v: its line %v holds no such return", issue.Key, w.Place, to, w.Line)
	}
	return returned, false, nil
}

// returnAct is what names a step to go back to: a failed step's report (failReport) or a move back
// (backwardMove). This is the act as a refusal names it, Again what the caller sends again, and
// Verb what the step at its place did.
type returnAct struct{ This, Again, Verb string }

var (
	failReport   = returnAct{This: "this fail", Again: "the fail", Verb: "failed"}
	backwardMove = returnAct{This: "this move", Again: "the move", Verb: "moves back"}
)

// returnStep is the one judge of a named place: the place of step n of w's task, which the report
// or the move of the step standing at from names as the one its task returns to (returnTo), or why
// it is refused, which changes nothing: an issue with no plan rows has no step to name, a step that
// runs no step of the task's line now names no step to return from, a place past the task's last
// step names no step of it, and the step at from itself or a later one is not earlier (the spec's
// words: a report that names a step of another task, or a step that is not earlier). The sentences
// take the route's shape for a returnTo it refuses before the workflow reads one (api's
// INVALID_RETURN_TO). from is off the line where a later version of the plan took its step away
// while the report was on its way: the report is of a run the daemon ended without it
// (leftItsPhase), and the route takes returnTo on every fail of a step's worker.
func returnStep(issue record.Issue, w Walk, from stepSite, n int, act returnAct) (record.TaskPlace, string) {
	if !w.Stored {
		return record.TaskPlace{}, fmt.Sprintf("returnTo is %d, and %s has no plan of tasks in Legion's record, so it has no step to name; send %s again without returnTo; nothing was recorded",
			n, issue.Key, act.Again)
	}
	at, ok := from.place, from.named
	if !from.named {
		at, ok = w.placeIn(from.at)
	}
	if _, step := w.planStepAt(at); !ok || !step {
		return record.TaskPlace{}, fmt.Sprintf("returnTo is %d, and task %s of %s has no step that runs as %s now (its steps are %s), so %s names no step of the task to return to; send %s again without returnTo; nothing was recorded",
			n, w.Task, issue.Key, from.at, strings.Join(plan.Words(namedSteps(w.Steps)), " "), act.This, act.Again)
	}
	role := w.Steps[at.Position-1].Role
	switch {
	case n > len(w.Steps):
		return record.TaskPlace{}, fmt.Sprintf("returnTo is %d, and task %s of %s has steps 1 to %d, so it names no step of the task whose step %d (%s) %s; send %s again naming an earlier step of that task, or without returnTo; nothing was recorded",
			n, w.Task, issue.Key, len(w.Steps), at.Position, role, act.Verb, act.Again)
	case n >= at.Position:
		return record.TaskPlace{}, fmt.Sprintf("returnTo is %d, and step %d (%s) of task %s of %s is not before step %d (%s), the step that %s; send %s again naming an earlier step, or without returnTo; nothing was recorded",
			n, n, w.Steps[n-1].Role, w.Task, issue.Key, at.Position, role, act.Verb, act.Again)
	}
	return record.TaskPlace{Kind: record.PlaceStep, Position: n}, ""
}

// lostAt says whether place is a step of w's task whose role d does not define: a role the
// deployment lost, whose step no role may start (stopLostRole). Legion's own roles are in every
// deployment (config.Config.Deployment, and New's default), so only a role of a deployment's own
// can be lost.
func (w Walk) lostAt(place record.TaskPlace, d plan.Deployment) (record.TaskPlace, bool) {
	step, ok := w.planStepAt(place)
	if !w.Stored || !ok {
		return record.TaskPlace{}, false
	}
	if _, defined := d.Roles[step.Role]; defined {
		return record.TaskPlace{}, false
	}
	return place, true
}

// lostStep is the first step w's task still has to run whose role d does not define (lostAt): the
// step it stands at and each after it, or every step of a task at no act; none for a task at its
// retro or its merge, done, left or stopped.
func (w Walk) lostStep(d plan.Deployment) (record.TaskPlace, bool) {
	if !w.Stored || w.State != record.TaskRunning && w.State != record.TaskWaiting {
		return record.TaskPlace{}, false
	}
	first := 1
	switch w.Place.Kind {
	case record.PlaceStep:
		first = w.Place.Position
	case record.PlaceRetro, record.PlaceMerge:
		return record.TaskPlace{}, false
	}
	for n := first; n <= len(w.Steps); n++ {
		if lost, ok := w.lostAt(record.TaskPlace{Kind: record.PlaceStep, Position: n}, d); ok {
			return lost, true
		}
	}
	return record.TaskPlace{}, false
}

// reviewFails says whether the step at w.Place, whose fail is the move, counts a review round
// toward review_round_cap: a step of the type review whose agent is not the owner, since the
// owner's own step reviews nothing another agent wrote.
func (w Walk) reviewFails() bool {
	step, ok := w.planStepAt(w.Place)
	return w.Stored && ok && step.Type == plan.TypeReview && step.AgentKey != "owner"
}

// planStepAt is the plan row of the step of w's task at place, ok false where place is no step
// the task holds (the retro, the merge, no act).
func (w Walk) planStepAt(place record.TaskPlace) (record.PlanStep, bool) {
	if place.Kind != record.PlaceStep || place.Position < 1 || place.Position > len(w.Steps) {
		return record.PlanStep{}, false
	}
	return w.Steps[place.Position-1], true
}

// RoleAt is the role that works the step of w's task at place: for a step, the worker of the
// phase its role runs as (phase.RoleFor of plan.RunsAs: the architect for the owner's acceptance),
// and the role itself where it runs as step, a role the deployment defines; the implementer at the
// retro and the merger at the merge; and "" at no act, at a place the task does not hold, and at
// the parts and the report of a task with a sub-owner, which no step's worker works, so a start of
// the task there (Engine.start) starts no role. On a task whose roles are Legion's own it answers
// phase.RoleFor of the phase the place derives.
func (w Walk) RoleAt(place record.TaskPlace) claim.Role {
	switch place.Kind {
	case record.PlaceRetro:
		return phase.RoleFor(phase.Retro)
	case record.PlaceMerge:
		return phase.RoleFor(phase.Merging)
	}
	step, ok := w.planStepAt(place)
	if !ok {
		return ""
	}
	if runs := plan.RunsAs(step.Role); runs != phase.Step {
		return phase.RoleFor(runs)
	}
	return claim.Role(step.Role)
}

// endingRole is the role whose step a move of w's task out of from ends, read on the walk before
// the move: on a task with plan rows the role of the step at the task's place (RoleAt), which names
// a role the deployment defines at step, and on an issue with none the role of the phase.
func (w Walk) endingRole(from phase.Phase) claim.Role {
	if w.Stored {
		return w.RoleAt(w.Place)
	}
	return phase.RoleFor(from)
}

// placeOfRole is the place of the step of w's task whose role is role, ok false where none is: a
// role runs at most one step of a task.
func (w Walk) placeOfRole(role claim.Role) (record.TaskPlace, bool) {
	for i, step := range w.Steps {
		if claim.Role(step.Role) == role {
			return record.TaskPlace{Kind: record.PlaceStep, Position: i + 1}, true
		}
	}
	return record.TaskPlace{}, false
}

// backwardPlace is the place a move back of w's task to the phase to reaches, ok false where none
// serves it: for a walk with no plan rows the place of to on today's line, whose phases each stand
// once (PlaceOf); for a stored walk the one place before w.Place of a position of the line that
// runs as to (Places). Where no earlier place runs as to, or more than one does (two steps that run
// as step), no backward row serves the move. Where to is step and two or more places before w.Place
// run as step, its third answer is the two of them nearest w.Place, the lower first, which the
// move's refusal names (backwardRefusal); it is nil in every other case.
func (w Walk) backwardPlace(to phase.Phase) (record.TaskPlace, bool, []record.TaskPlace) {
	if !w.Stored {
		place, ok := w.PlaceOf(to)
		return place, ok, nil
	}
	var found []record.TaskPlace
	for at, p := range w.Line {
		if place := w.Places[at]; p == to && placeRank(place) < placeRank(w.Place) {
			found = append(found, place)
		}
	}
	switch {
	case len(found) == 1:
		return found[0], true, nil
	case len(found) >= 2 && to == phase.Step:
		return record.TaskPlace{}, false, found[len(found)-2:]
	}
	return record.TaskPlace{}, false, nil
}

// table is the table of w's line: Table for a run with no plan rows, which is the table of today's
// line at today's places built once, and the table of the task's line at its places otherwise
// (StoredTableOf over Places), so the gate's opening puts a task with a sub-owner at its parts and
// a move back to planning returns it there.
func (w Walk) table() []Row {
	if !w.Stored {
		return Table
	}
	return StoredTableOf(w.Line, w.Places)
}
