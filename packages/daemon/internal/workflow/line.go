package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
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

// todaysSteps is the steps of the task default of a run with no plan rows, which walks today's
// line, phase.Order: the default steps of a project that names none (config.Project.RunsByDefault),
// never the configured project's, so such a run walks today's line whatever `default_steps` says,
// and its places, and the ids of its records, stay where the release before put them. Each takes
// the type of its role among Legion's own (plan.BuiltinRoles), whatever the deployment defines.
// They are read once.
var todaysSteps = planSteps(plan.Deployment{Roles: plan.BuiltinRoles()}, "", 0, defaultTask, config.Project{}.RunsByDefault())

// todaysWalk is the walk of a run with no plan rows: the task default walking todaysSteps on
// today's line, phase.Order, with Stored false.
func todaysWalk() Walk { return Walk{Task: defaultTask, Steps: todaysSteps, Line: phase.Order} }

// walk is the walk of issue's run: its plan rows, or today's line with Stored false
// (todaysWalk). Each step runs as the phase of its role (plan.RunsAs). A run whose rows hold no
// task or more than one (record.StoredPlan.Task, the one rule for the task a run is on) is an
// error: no plan this release takes writes one (plan.Validate, plan.Supported).
func (e *Engine) walk(ctx context.Context, tx pgx.Tx, issue record.Issue) (Walk, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return todaysWalk(), err
	}
	task, err := stored.Task()
	if err != nil {
		return Walk{}, fmt.Errorf("walk %s: %w", issue.Key, err)
	}
	steps := stored.Steps[task.Task]
	return Walk{Stored: true, Task: task.Task, Text: task.BlockText, Steps: steps, Line: plan.Line(namedSteps(steps)), State: task.State,
		Place: task.Place, StoppedReason: task.StoppedReason, Version: stored.Version}, nil
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

// PlaceOf is the first place of w's line that runs as p: the step of the line that runs as p, at
// its place from 1, or the retro or the merge at place 0. A phase the line does not hold has no
// place. Two steps that run as step are two places, so a reader of where the task stands reads
// placeIn, and PlaceOf serves a phase that stands once in its line.
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

// stepAt is the step whose agent does the work of place on w, the one place the workflow reads a
// step's agent from (Engine.stepToken): for a step, that step, so the steps of one key are one
// agent; for the retro, the task's last step of the type implementation, which runs it. A walk with
// no plan rows holds none, and neither does the merge or a place the task does not hold: there the
// role stands for the agent.
func (w Walk) stepAt(place record.TaskPlace) (record.PlanStep, bool) {
	if !w.Stored {
		return record.PlanStep{}, false
	}
	switch place.Kind {
	case record.PlaceStep:
		return w.planStepAt(place)
	case record.PlaceRetro:
		for i := len(w.Steps) - 1; i >= 0; i-- {
			if w.Steps[i].Type == plan.TypeImplementation {
				return w.Steps[i], true
			}
		}
	}
	return record.PlanStep{}, false
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

// rowRead is the task and the place of w a role's row is read at (record.PhaseRowAt): w's task
// and place, or, for a stored task at no act whose issue stands in standsIn, the first place of
// standsIn on its line (PlaceOf), as placeIn reads it. A walk with no plan rows reads no task, so
// each role reads its one row (record.PhaseRowOf).
func (w Walk) rowRead(standsIn phase.Phase) (string, record.TaskPlace) {
	if !w.Stored {
		return "", record.TaskPlace{}
	}
	if w.Place.Kind == "" {
		at, _ := w.PlaceOf(standsIn)
		return w.Task, at
	}
	return w.Task, w.Place
}

// completionRow is the row of rows of the run role's completion serving entry reports
// (record.CompletionRow), read where w's task stands (rowRead): where role works that place
// (works), its row there; otherwise the row of the entry it serves, else role's row now.
func (w Walk) completionRow(rows []record.PhaseRow, role claim.Role, entry int64, works bool, standsIn phase.Phase) record.PhaseRow {
	task, at := w.rowRead(standsIn)
	return record.CompletionRow(rows, role, entry, works, task, at, w.Steps)
}

// runPlace is the place on w of the run row holds, a run of reported: the retro and the merge at
// their places, and a step at the step the row ran (record.PhaseRow.StepPlace), so a report of a
// role that stands at two steps is the report of the step whose run it is, whichever place the
// task stands at now. A row of no step, one a new generation cleared or the release before wrote,
// is the role's one row, at its place nearest where the task stands (record.RunPlace). ok is
// false for a row of another task, and for a step w's line no longer runs as reported with the
// row's role there, as after a later version of the plan took it away. A walk with no plan rows
// holds each phase once (PlaceOf).
func (w Walk) runPlace(row record.PhaseRow, reported phase.Phase) (record.TaskPlace, bool) {
	if !w.Stored {
		return w.PlaceOf(reported)
	}
	switch {
	case row.StepTask != "" && row.StepTask != w.Task:
		return record.TaskPlace{}, false
	case reported == phase.Retro:
		return record.TaskPlace{Kind: record.PlaceRetro}, true
	case reported == phase.Merging:
		return record.TaskPlace{Kind: record.PlaceMerge}, true
	}
	place := record.RunPlace(row, w.Place, w.Steps)
	if place < 1 || place > len(w.Steps) || w.Line[place-1] != reported || claim.Role(w.Steps[place-1].Role) != row.Role {
		return record.TaskPlace{}, false
	}
	return record.TaskPlace{Kind: record.PlaceStep, Position: place}, true
}

// steps is the part of the line each step runs: all of it but the retro and the merge.
func (w Walk) steps() []phase.Phase {
	if len(w.Line) < 2 {
		return nil
	}
	return w.Line[:len(w.Line)-2]
}

// MovesBack says whether row moves w's task back by the task's own order. A stored walk compares
// the row's two places (record.TaskPlace.Rank): an earlier step by its place, every step before the
// retro, the retro before the merge, so a move from step 3 back to step 2 goes back though both run
// as step.
// A run with no plan rows walks today's line, so this is phase.MovesBack of the row's two phases
// for it. A task in another order than today's so counts its move back from testing to reviewing
// (the steps `implementer reviewer tester`), and not the review's approval from reviewing to
// testing, which moves it on.
func (w Walk) MovesBack(row Row) bool {
	if !w.Stored {
		return phase.MovesBack(row.From, row.To)
	}
	return row.ToPlace.Kind != "" && row.ToPlace.Rank() < row.FromPlace.Rank()
}

// passedStep says whether the step of w's task that runs as p stands before the place the task
// stands at while its issue stands in at (placeIn): the implementer's work behind a later step, or
// the reviewer's review an issue past it stands on. It compares places (record.TaskPlace.Rank), so
// it reads a task whose steps of roles the deployment defines all run as step as the stored table
// does (MovesBack). A phase the line does not hold, and an issue that stands at no place of the
// line, passed nothing. Where two steps run as p it reads the first (PlaceOf), which is what its
// readers ask of implementing and reviewing, either of which can stand at two steps: whether any
// such step stands behind the place the task stands at.
func (w Walk) passedStep(at, p phase.Phase) bool {
	here, standing := w.placeIn(at)
	there, held := w.PlaceOf(p)
	return standing && held && there.Rank() < here.Rank()
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

// moveTask is the one writer of where a task stands: it moves w's task to place in state
// (record.Store.MoveTask), the place of the row that moves it: a move to done passes the place the
// task keeps, and a move to admitted, where a task waits for its first step (waitForFirstStep), no
// place, the task at no act. It sets issue.Phase to the phase that place derives (issuePhaseOf),
// which must be to, and issue.StepRole to the role of the step at that place ("" at the retro, the
// merge or no act), in the caller's transaction and before the caller's write of issue
// (WriteIssue), so a caller that reads the issue after the move reads the role a fresh read gives.
// w then stands where the task does, for whatever the caller reads of it after the move. A run
// with no plan rows has no task to move, and issue.Phase is to, as it always was. A move to where
// the task already stands writes nothing. The caller clears or keeps issue.Hold itself.
func (e *Engine) moveTask(ctx context.Context, tx pgx.Tx, issue *record.Issue, w *Walk, to phase.Phase, place record.TaskPlace, state record.TaskState) error {
	if !w.Stored {
		issue.Phase = to
		return nil
	}
	if derived := issuePhaseOf(*w, place, state); derived != to {
		return fmt.Errorf("move task %s of %s to %s at %+v: that place of its line %v runs as %q", w.Task, issue.Key, to, place, w.Line, derived)
	}
	if place != w.Place || state != w.State {
		if err := e.store.MoveTask(ctx, tx, issue.Key, issue.Generation, w.Task, place, state); err != nil {
			return err
		}
		w.Place, w.State = place, state
	}
	issue.Phase = to
	issue.StepRole = ""
	if step, ok := w.planStepAt(place); ok {
		issue.StepRole = claim.Role(step.Role)
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
		if at != task && (issue.Phase != phase.AwaitingPerson || task != phase.Admitted) {
			return fmt.Errorf("put %s at generation %d: it stands in %s, and its task %s is %s at %+v, which stands in %s",
				issue.Key, issue.Generation, stands, w.Task, w.State, w.Place, task)
		}
	}
	return e.store.PutIssue(ctx, tx, issue)
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
	at, ok := w.resolveSite(from)
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

// lostAt says whether the work of place on w's task is lost to d: a step whose role d does not
// define, a role the deployment lost, whose step no role may start (stopLostRole); or a step, or
// the retro, whose agent's name is such a role (lostAgent). An agent runs in the slot of its name
// (claim.Token.Slot), the role of its first step (record.NameAgents), whatever role its step has
// now, so an agent named for a role the deployment lost has no slot in a pod the deployment builds,
// and its launch waits for a pod that never comes. Legion's own roles are in every deployment
// (config.Config.Deployment, and New's default), so only a role of a deployment's own can be lost.
func (w Walk) lostAt(place record.TaskPlace, d plan.Deployment) (record.TaskPlace, bool) {
	if !w.Stored {
		return record.TaskPlace{}, false
	}
	if step, ok := w.planStepAt(place); ok && !d.Defines(step.Role) {
		return place, true
	}
	if step, ok := w.stepAt(place); ok && lostAgent(step.AgentName(), d) {
		return place, true
	}
	return record.TaskPlace{}, false
}

// lostStep is the first work w's task still has to do that d lost (lostAt): the step it stands at
// and each after it, then the retro, or all of them for a task at no act; the retro alone for a
// task at its retro; none for a task at its merge, done, left or stopped.
func (w Walk) lostStep(d plan.Deployment) (record.TaskPlace, bool) {
	if !w.Stored || w.State != record.TaskRunning && w.State != record.TaskWaiting {
		return record.TaskPlace{}, false
	}
	first := 1
	switch w.Place.Kind {
	case record.PlaceStep:
		first = w.Place.Position
	case record.PlaceRetro:
		return w.lostAt(w.Place, d)
	case record.PlaceMerge:
		return record.TaskPlace{}, false
	}
	return w.lostFrom(first, d)
}

// lostFrom is the first work of w's task from step first on that d lost (lostAt): a step, or after
// the last step the retro. lostStep reads it from where a task that runs stands; a stopped task's
// end of its stop reads it from step 1, since all of the task's work is still to do once it goes on
// (keepLostStop).
func (w Walk) lostFrom(first int, d plan.Deployment) (record.TaskPlace, bool) {
	for n := first; n <= len(w.Steps); n++ {
		if lost, ok := w.lostAt(record.TaskPlace{Kind: record.PlaceStep, Position: n}, d); ok {
			return lost, true
		}
	}
	return w.lostAt(record.TaskPlace{Kind: record.PlaceRetro}, d)
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
// retro and the merger at the merge; and "" at no act or at a place the task does not hold. On a
// task whose roles are Legion's own it answers phase.RoleFor of the phase the place derives.
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

// backwardPlace is the place a move back of w's task to the phase to reaches, ok false where none
// serves it: for a walk with no plan rows the place of to on today's line, whose phases each stand
// once (PlaceOf); for a stored walk the place before w.Place whose step, retro or merge runs as to,
// the nearest of them where a role that stands at two steps runs to at two places before w.Place.
// A failed step's work does not go to the nearest: it returns to the first step of the type
// implementation before it (returnTarget). Where no earlier place runs as to, no backward row
// serves the move. Where to is step and two or more places before w.Place run as step, they are
// steps of roles the move does not tell apart, and its third answer is the two of them nearest
// w.Place, the lower first, which the move's refusal names (backwardRefusal); it is nil in every
// other case.
func (w Walk) backwardPlace(to phase.Phase) (record.TaskPlace, bool, []record.TaskPlace) {
	if !w.Stored {
		place, ok := w.PlaceOf(to)
		return place, ok, nil
	}
	var found []record.TaskPlace
	for at, p := range w.Line {
		if place := placeAt(w.Line, at); p == to && place.Rank() < w.Place.Rank() {
			found = append(found, place)
		}
	}
	switch {
	case len(found) >= 2 && to == phase.Step:
		return record.TaskPlace{}, false, found[len(found)-2:]
	case len(found) >= 1:
		return found[len(found)-1], true, nil
	}
	return record.TaskPlace{}, false, nil
}

// table is the table of w's line: Table for a run with no plan rows, which is TableOf(phase.Order)
// built once, and the table of the task's line with its places otherwise (StoredTableOf).
func (w Walk) table() []Row {
	if !w.Stored {
		return Table
	}
	return StoredTableOf(w.Line)
}
