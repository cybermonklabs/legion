package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Walk is the line of phases an issue's run walks, and where its task stands on it. A run with
// plan rows (record.Store.Plan at the issue's generation) walks the line of its one task's steps
// (plan.Line): each step runs as the phase of its role, then Legion's own retro and merge. A run
// with none, which is every issue that ran before plan rows existed, walks today's line,
// phase.Order, as the steps of the task default (todaysSteps), with Stored false, and its table is
// Table, row for row. Every behaviour that differs from today reads Stored and nothing else; the
// steps of both are read the same way (placeOnWalk).
//
// Line and Steps are read only: an unstored walk's Line is phase.Order itself, and its Steps are
// todaysSteps itself.
type Walk struct {
	// Stored says whether the run has plan rows.
	Stored bool
	// Task is the id of the run's one task: the plan's, or defaultTask where the run has none,
	// which is the task its steps are recorded under (StepID).
	Task string
	// Text is the text of the task's block (record.PlanTask.BlockText), which a step's worker is
	// told (standingPart); "" where the run has no plan rows.
	Text string
	// Steps are the task's steps, by place from 1: todaysSteps where the run has no plan rows.
	Steps []record.PlanStep
	// Needs are the tasks the task needs, each by its id (record.PlanTask.Needs), which the run's
	// plan node carries (planNode); nil where it needs none or the run has no plan rows.
	Needs []string
	// Line is the phases the task walks: one for each step, then retro and merging.
	Line []phase.Phase
	// State and Place are where the task stands (record.PlanTask); zero where the run has no
	// plan rows, whose place is the issue's phase alone.
	State record.TaskState
	Place record.TaskPlace
	// StoppedReason is why a stopped task was stopped (record.PlanTask.StoppedReason), by its owner
	// or by a failed step; "" for a task that is not stopped.
	StoppedReason string
	// Version is the version of the plan in force (record.StoredPlan.Version): where the plan came
	// from, its document and its number, which the run's plan node names (planNode); zero where
	// the run has no plan rows.
	Version record.PlanVersion
}

// builtinDeployment is the roles a step of this release runs as (plan.BuiltinRoles): the one
// Deployment the walk reads a stored step's phase from.
var builtinDeployment = plan.Deployment{Roles: plan.BuiltinRoles()}

// todaysSteps is the steps of the task default of a run with no plan rows, which walks today's
// line, phase.Order: the default steps of a project that names none (config.Project.RunsByDefault),
// never the configured project's, so such a run walks today's line whatever `default_steps` says,
// and its places, and the ids of its records, stay where the release before put them. They are
// read once.
var todaysSteps = planSteps("", 0, defaultTask, config.Project{}.RunsByDefault())

// todaysWalk is the walk of a run with no plan rows: the task default walking todaysSteps on
// today's line, phase.Order, with Stored false.
func todaysWalk() Walk { return Walk{Task: defaultTask, Steps: todaysSteps, Line: phase.Order} }

// walk is the walk of issue's run: its plan rows, or today's line with Stored false
// (todaysWalk). A run whose rows hold no task or more than one (record.StoredPlan.Task, the one
// rule for the task a run is on), or a step whose role runs as no phase, is an error: no plan
// this release takes writes one (plan.Validate, plan.Supported).
func (e *Engine) walk(ctx context.Context, tx pgx.Tx, issue record.Issue) (Walk, error) {
	return readWalk(ctx, e.store, tx, issue)
}

// LineOf is the line of phases issue's run walks (Walk.Line), as store holds it in tx: the one
// order a reader outside the engine names the task's steps in, such as the news of an approved
// design, whose answer for an issue in accepting names the phases a move back reaches
// (acceptingAnswer).
func LineOf(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue) ([]phase.Phase, error) {
	w, err := readWalk(ctx, store, tx, issue)
	return w.Line, err
}

// readWalk is the walk of issue's run as store holds it in tx (walk).
func readWalk(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue) (Walk, error) {
	stored, err := store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return todaysWalk(), err
	}
	task, err := stored.Task()
	if err != nil {
		return Walk{}, fmt.Errorf("walk %s: %w", issue.Key, err)
	}
	steps := stored.Steps[task.Task]
	line := plan.Line(namedSteps(steps), builtinDeployment)
	if at := slices.Index(line, ""); at >= 0 {
		return Walk{}, fmt.Errorf("walk %s at generation %d: step %d of task %s names the role %q, which runs as no phase", issue.Key, issue.Generation, at+1, task.Task, steps[at].Role)
	}
	return Walk{Stored: true, Task: task.Task, Text: task.BlockText, Steps: steps, Needs: task.Needs, Line: line, State: task.State, Place: task.Place,
		StoppedReason: task.StoppedReason, Version: stored.Version}, nil
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

// PlaceOf is where the task stands while its issue stands in p: the step of the line that runs
// as p, at its place from 1, or the retro or the merge at place 0. A phase the line does not hold
// has no place.
func (w Walk) PlaceOf(p phase.Phase) (record.TaskPlace, bool) {
	switch p {
	case phase.Retro:
		return record.TaskPlace{Kind: record.PlaceRetro}, true
	case phase.Merging:
		return record.TaskPlace{Kind: record.PlaceMerge}, true
	}
	if at := slices.Index(w.steps(), p); at >= 0 {
		return record.TaskPlace{Kind: record.PlaceStep, Position: at + 1}, true
	}
	return record.TaskPlace{}, false
}

// steps is the part of the line each step runs: all of it but the retro and the merge.
func (w Walk) steps() []phase.Phase {
	if len(w.Line) < 2 {
		return nil
	}
	return w.Line[:len(w.Line)-2]
}

// MovesBack says whether a move of w's task from the phase from to the phase to goes back by the
// task's own order: to stands before from on w's line, its steps in the task's order, then the
// retro, then the merge. Each role runs at most one step of a task, so each phase stands at one
// place of the line. A phase the line does not hold goes back nowhere, and nothing goes back into
// it. A run with no plan rows walks today's line, phase.Order, so this is phase.MovesBack for it.
// A task in another order than today's so counts its move back from testing to reviewing (the
// steps `implementer reviewer tester`), and not the review's approval from reviewing to testing,
// which moves it on.
func (w Walk) MovesBack(from, to phase.Phase) bool {
	return phase.MovesBackOn(w.Line, from, to)
}

// issuePhaseOf is the phase an issue whose one task stands at place in state stands in: done
// for a task that is done or left the workflow before it was done (record.TaskLeft), whatever its
// place, which is the phase of an issue out of the workflow; the phase of the step at place, retro
// or merging at the task's retro or merge; and admitted for a task at no act, which has not
// started. A task stopped or held keeps its place, and the hold overlays this phase
// (record.Hold.From). A step place the line does not hold derives no phase.
func issuePhaseOf(w Walk, place record.TaskPlace, state record.TaskState) phase.Phase {
	if state == record.TaskDone || state == record.TaskLeft {
		return phase.Done
	}
	switch place.Kind {
	case record.PlaceRetro:
		return phase.Retro
	case record.PlaceMerge:
		return phase.Merging
	case record.PlaceStep:
		if steps := w.steps(); place.Position >= 1 && place.Position <= len(steps) {
			return steps[place.Position-1]
		}
		return ""
	}
	return phase.Admitted
}

// moveTask is the one writer of where a task stands: it moves w's task to the place of to in state
// (record.Store.MoveTask), keeping its place on a move to done and standing it at no act on a move
// to admitted, where a task waits for its first step (waitForFirstStep), and sets issue.Phase to the
// phase that place derives (issuePhaseOf), in the caller's transaction and before the caller's write
// of issue (WriteIssue). w then stands where the task does, for whatever the caller reads of it after
// the move. A move to verifying keeps the task's place too, done at its merge, and the issue stands
// in verifying over it: the task's work merged, and what the issue waits for is its close
// (enterVerifying). A run with no plan rows has no task to move, and issue.Phase is to, as it always
// was. A move to where the task already stands writes nothing. The caller clears or keeps
// issue.Hold itself.
func (e *Engine) moveTask(ctx context.Context, tx pgx.Tx, issue *record.Issue, w *Walk, to phase.Phase, state record.TaskState) error {
	if !w.Stored {
		issue.Phase = to
		return nil
	}
	place := w.Place
	switch to {
	case phase.Done, phase.Verifying:
		// The task keeps its place.
	case phase.Admitted:
		place = record.TaskPlace{}
	default:
		var ok bool
		if place, ok = w.PlaceOf(to); !ok {
			return fmt.Errorf("move task %s of %s to %s: its line %v holds no such phase", w.Task, issue.Key, to, w.Line)
		}
	}
	if place != w.Place || state != w.State {
		if err := e.store.MoveTask(ctx, tx, issue.Key, issue.Generation, w.Task, place, state); err != nil {
			return err
		}
		w.Place, w.State = place, state
	}
	issue.Phase = issuePhaseOf(*w, place, state)
	if to == phase.Verifying && issue.Phase == phase.Done {
		issue.Phase = phase.Verifying
	}
	return nil
}

// WriteIssue writes issue (record.Store.PutIssue), and is the one write of an issue of the workflow
// and of admission: no other code of either package calls a store's PutIssue. issues.phase and the
// place of the run's task are two records of where the issue stands, and WriteIssue refuses a
// write in which they disagree: the issue stands in the phase its task's place and state derive
// (issuePhaseOf), a hold overlays that phase and names it (record.StandsAt), and a root that
// waits on a person
// before its first step stands in awaiting_person over the admitted of a task at no act. That root
// stands there itself: no hold is from awaiting_person, which the issues table refuses
// (issues_held_from_check), so a hold that names it is refused here too. A parent that waits on its
// open Dispatch children stands in awaiting_children over the phase of the start it holds
// (record.ChildrenWait.Start), where its task stands (awaitChildren), as a hold overlays the phase
// it is from: no hold is from awaiting_children either (issues_children_wait_shape). It reads
// the walk in the caller's transaction, after any move of the task (moveTask), so a write that
// names its phase itself is checked against the store and not against the caller's copy: one read
// of issue_plans for a run with no plan rows, which has only its phase and is not checked, and
// three reads (issue_plans, plan_tasks, plan_steps) for a run with plan rows. A workflow move
// writes through writeIssueOn instead, on the walk it read once and moved.
func (e *Engine) WriteIssue(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	return e.writeIssueOn(ctx, tx, issue, w)
}

// writeIssueOn is WriteIssue's check and write on w, the walk of issue's run as the caller holds
// it: WriteIssue's own read, or a workflow move, which read w and moved its task on it (moveTask),
// so w is where the store holds the task: moveTask writes the place and the state it sets on w,
// in the same transaction.
func (e *Engine) writeIssueOn(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) error {
	if w.Stored {
		at, task := record.StandsAt(issue), issuePhaseOf(w, w.Place, w.State)
		stands := string(at)
		switch {
		case issue.Phase == phase.AwaitingChildren && issue.ChildrenWait != nil:
			at = issue.ChildrenWait.Start
			stands = "awaiting_children, holding the start of " + string(at)
		case at != issue.Phase:
			stands = "held from " + stands
		}
		// Two phases stand over a task that derives another: a root waiting on a person before its
		// first step, over the admitted of a task at no act, and an issue whose merged task is done,
		// waiting in verifying for its close.
		waits := issue.Phase == phase.AwaitingPerson && task == phase.Admitted || issue.Phase == phase.Verifying && task == phase.Done
		if at != task && !waits {
			return fmt.Errorf("put %s at generation %d: it stands in %s, and its task %s is %s at %+v, which stands in %s",
				issue.Key, issue.Generation, stands, w.Task, w.State, w.Place, task)
		}
	}
	return e.store.PutIssue(ctx, tx, issue)
}

// row is the row of w's table that moves an issue standing in from on trigger, into target where
// a target is named, whose guard admits snapshot. A failed step of a task with plan rows returns
// its work to the step its plan reads (returnTarget, or the step a completion names), which is
// an earlier step of the task's line but not always implementing, the one target of the table's
// two returns (stepRows). Such a return is the table's row of the failed step's trigger with that
// step as its target (returnRow).
func (w Walk) row(from phase.Phase, trigger TriggerKind, target phase.Phase, snapshot Snapshot) (Row, bool) {
	for _, row := range w.table() {
		if row.From == from && row.Trigger == trigger && (target == "" || row.To == target) && (row.Guard == nil || row.Guard(snapshot)) {
			return row, true
		}
	}
	if w.Stored && returnsWork(trigger) && target != "" {
		return w.returnRow(from, trigger, target, snapshot)
	}
	return Row{}, false
}

// returnsWork says whether trigger is a failed step returning its task's work to an earlier step:
// the tester's fail and a review's request for changes. A backward move names its own
// target, which a backward row of the line serves.
func returnsWork(trigger TriggerKind) bool {
	return trigger == TriggerTesterFailed || trigger == TriggerReviewRejected
}

// returnRow is the return of the failed step standing at from to the earlier step of w's task
// that runs as target: the table's row of from on trigger, whose target becomes target and whose
// effects become the effects of a move into target (effectsInto). A target that is not a step of
// the task before from returns nothing.
func (w Walk) returnRow(from phase.Phase, trigger TriggerKind, target phase.Phase, snapshot Snapshot) (Row, bool) {
	at, _ := w.PlaceOf(from)
	to, _ := w.PlaceOf(target)
	if at.Kind != record.PlaceStep || to.Kind != record.PlaceStep || to.Position >= at.Position {
		return Row{}, false
	}
	for _, row := range w.table() {
		if row.From == from && row.Trigger == trigger && (row.Guard == nil || row.Guard(snapshot)) {
			row.To, row.Effects = target, effectsInto(target)
			return row, true
		}
	}
	return Row{}, false
}

// returnTarget is the step the failed step of w's task standing at from returns the work to when
// the completion names none: the first step before it whose recorded type is implementation
// (record.PlanStep.Type), as the phase that step runs as. ok is false where no step before it has
// that type: the task then stops (stopUnreturned). Each failed step a plan of this release can
// hold comes after its implementer (plan.Supported), so only a stored type that no plan gives
// reaches that answer.
func (w Walk) returnTarget(from phase.Phase) (phase.Phase, bool) {
	at, _ := w.PlaceOf(from)
	if at.Kind != record.PlaceStep {
		return "", false
	}
	for i, step := range w.Steps[:at.Position-1] {
		if step.Type == plan.TypeImplementation {
			return w.steps()[i], true
		}
	}
	return "", false
}

// returnOf is the row the failed step of w's task standing where issue stands moves by on trigger
// when its completion names no step to return to (target ""): the return of its work to the step
// its plan reads from the steps' recorded types (returnTarget), or stops, where no step before it
// can take the work and the task stops instead (stopUnreturned). A run with no plan rows, and
// every other move, keeps row, whose target for a failed step is implementing, as today's line's.
func (w Walk) returnOf(issue record.Issue, trigger TriggerKind, target phase.Phase, row Row, snapshot Snapshot) (Row, bool, error) {
	if !w.Stored || !returnsWork(trigger) || target != "" {
		return row, false, nil
	}
	to, found := w.returnTarget(issue.Phase)
	if !found {
		return Row{}, true, nil
	}
	returned, ok := w.row(issue.Phase, trigger, to, snapshot)
	if !ok {
		return Row{}, false, fmt.Errorf("return the work of %s from %s to %s: its line %v holds no such return", issue.Key, issue.Phase, to, w.Line)
	}
	return returned, false, nil
}

// returnStep is the phase of step n of w's task, the step that the report of the failed step
// standing at failed names as the one its task returns to (returnTo), or why that report is
// refused, which changes nothing: an issue with no plan rows has no step to name, a failed phase
// that runs no step of the task's line now names no step to return from, a place past the task's
// last step names no step of it, and the failed step itself or a later one is not earlier (the
// spec's words: a report that names a step of another task, or a step that is not earlier). The
// sentences take the route's shape for a returnTo it refuses before the workflow reads one (api's
// INVALID_RETURN_TO). The failed phase is off the line where a later version of the plan took its
// step away while the report was on its way: the report is of a run the daemon ended without it
// (leftItsPhase), and the route takes returnTo on every tester's fail.
func returnStep(issue record.Issue, w Walk, failed phase.Phase, n int) (phase.Phase, string) {
	if !w.Stored {
		return "", fmt.Sprintf("returnTo is %d, and %s has no plan of tasks in Legion's record, so it has no step to name; send the fail again without returnTo; nothing was recorded",
			n, issue.Key)
	}
	at, _ := w.PlaceOf(failed)
	if at.Kind != record.PlaceStep {
		return "", fmt.Sprintf("returnTo is %d, and task %s of %s has no step that runs as %s now (its steps are %s), so this fail names no step of the task to return to; send the fail again without returnTo; nothing was recorded",
			n, w.Task, issue.Key, failed, strings.Join(plan.Words(namedSteps(w.Steps)), " "))
	}
	role := w.Steps[at.Position-1].Role
	switch {
	case n > len(w.Steps):
		return "", fmt.Sprintf("returnTo is %d, and task %s of %s has steps 1 to %d, so it names no step of the task whose step %d (%s) failed; send the fail again naming an earlier step of that task, or without returnTo; nothing was recorded",
			n, w.Task, issue.Key, len(w.Steps), at.Position, role)
	case n >= at.Position:
		return "", fmt.Sprintf("returnTo is %d, and step %d (%s) of task %s of %s is not before step %d (%s), the step that failed; send the fail again naming an earlier step, or without returnTo; nothing was recorded",
			n, n, w.Steps[n-1].Role, w.Task, issue.Key, at.Position, role)
	}
	return w.steps()[n-1], ""
}

// table is the table of w's line: Table for a run with no plan rows, which is TableOf(phase.Order)
// built once, and the table of the task's line otherwise.
func (w Walk) table() []Row {
	if !w.Stored {
		return Table
	}
	return TableOf(w.Line)
}
