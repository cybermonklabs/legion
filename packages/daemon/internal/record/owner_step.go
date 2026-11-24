package record

import (
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// ReportedPlace is the place of a task that the owner's report of step names (complete_step's
// step): the step at that place, from 1, and the retro for 0.
func ReportedPlace(step int) TaskPlace {
	if step == 0 {
		return TaskPlace{Kind: PlaceRetro}
	}
	return TaskPlace{Kind: PlaceStep, Position: step}
}

// PlacePhase is the phase an issue stands in while its task runs at place on steps: the phase the
// step's role runs as at a step (plan.RunsAs, the phase plan.Line gives the step), retro at the
// retro, merging at the merge and admitted at no act, where a task waits for its first step; ""
// for a step the task does not hold.
func PlacePhase(place TaskPlace, steps []PlanStep) phase.Phase {
	switch place.Kind {
	case PlaceStep:
		if place.Position >= 1 && place.Position <= len(steps) {
			return plan.RunsAs(steps[place.Position-1].Role)
		}
		return ""
	case PlaceRetro:
		return phase.Retro
	case PlaceMerge:
		return phase.Merging
	}
	return phase.Admitted
}

// RunsAt says whether a task in state, standing at place on steps, runs there now: it runs, and
// issue stands, unheld, in the phase of that place (PlacePhase). The owner's report of its own step
// (OwnerStepRuns) and the workflow's judgement of whether a completion is the work of the place
// (workflow's completionOf) both read it.
func RunsAt(issue Issue, state TaskState, place TaskPlace, steps []PlanStep) bool {
	return state == TaskRunning && issue.Phase == PlacePhase(place, steps)
}

// OwnerStepRuns says whether the owner's report of step (from 1, and 0 for the retro:
// ReportedPlace) of task on issue finds that step running with the owner as its agent: the task has
// plan rows (stored), stands at that place in state and runs there now (RunsAt), and the owner does
// the place (PlaceWorker). The route complete_step and the workflow's check of the report under the
// fact's lock both read it (api's completeStep, workflow's ownerStepRefusal), and each refuses a
// report where it does not hold with OwnerStepNotRunning, whose arguments are these.
func OwnerStepRuns(issue Issue, task string, stored bool, state TaskState, step int, place TaskPlace, steps []PlanStep) bool {
	if !stored || place != ReportedPlace(step) || !RunsAt(issue, state, place, steps) {
		return false
	}
	worker, ok := PlaceWorker(task, place, steps)
	return ok && worker.Owner()
}

// OwnerStepNotRunning is the sentence of OWNER_STEP_NOT_RUNNING, the refusal of the owner's report
// of step (from 1, and 0 for the retro) of task on issue while the task, in state, stands at place
// on steps, its plan's rows (no rows where stored is false, an issue with no plan rows), where
// OwnerStepRuns does not hold. It names where the task stands and who works that place, so the
// owner reads which report, if any, is its own now; where the task stands at the step named and the
// owner does it, what keeps that step from running is the task's state or the issue's phase (a
// hold), and it names those. The route complete_step and the workflow's check of the owner's report
// both write it (api's completeStep, workflow's ownerStepRefusal), so the two say one sentence.
func OwnerStepNotRunning(issue Issue, task string, stored bool, state TaskState, step int, place TaskPlace, steps []PlanStep) string {
	named := ReportedPlace(step)
	key := issue.Key
	reported := fmt.Sprintf("%s of task %s of %s", PlaceWords(named), task, key)
	if !stored {
		return fmt.Sprintf("%s is not a step of yours that runs now: %s runs with no plan rows, so no step of it is the owner's. This report changed nothing.", reported, key)
	}
	worker, ok := PlaceWorker(task, place, steps)
	switch {
	case !ok:
		return fmt.Sprintf("%s is not a step of yours that runs now: %s stands at %s, which no agent works. This report changed nothing.", reported, key, PlaceWords(place))
	case place == named && worker.Owner():
		return fmt.Sprintf("%s is not a step of yours that runs now: %s stands in %s, and its task %s is %s at %s, so that step does not run. This report changed nothing.",
			reported, key, issue.Phase, task, state, PlaceWords(place))
	}
	return fmt.Sprintf("%s is not a step of yours that runs now: %s stands at %s (%s), whose agent is %s. This report changed nothing.",
		reported, key, PlaceWords(place), worker.Role, agentWords(worker, steps))
}

// SignOffNotTheOwners is the sentence of SIGNOFF_NOT_THE_OWNERS, the refusal of a sign-off of
// issue in accepting where the acceptance its task stands at, place of task on steps, is not the
// owner's own step (Issue.OwnersPlace): a step `acceptance` with no key, or with another key, whose
// agent ends it by its own verdict. The workflow's signOff writes it.
func SignOffNotTheOwners(issue Issue, task string, place TaskPlace, steps []PlanStep) string {
	worker, _ := PlaceWorker(task, place, steps)
	return fmt.Sprintf("%s (%s) of task %s of %s is not a step of yours: its agent is %s, which ends it with its own verdict, and a sign-off takes only an acceptance whose agent is the owner. This sign-off changed nothing.",
		PlaceWords(place), worker.Role, task, issue.Key, agentWords(worker, steps))
}

// OwnersPlaceWords is the owner's place of task, place on steps, as the architect's catch-up names
// it: `step <n> (<role>) of task <task>`, or `the retro of task <task>`.
func OwnersPlaceWords(task string, place TaskPlace, steps []PlanStep) string {
	if worker, ok := PlaceWorker(task, place, steps); ok && place.Kind == PlaceStep {
		return fmt.Sprintf("%s (%s) of task %s", PlaceWords(place), worker.Role, task)
	}
	return fmt.Sprintf("%s of task %s", PlaceWords(place), task)
}

// agentWords is the agent of worker, a place of a task whose rows are steps (PlaceWorker), as a
// sentence names it: `the owner` where the owner does the place, and its name (Worker.Name)
// otherwise.
func agentWords(worker Worker, steps []PlanStep) string {
	if worker.Owner() {
		return "the owner"
	}
	return string(worker.Name(steps))
}

// PlaceWords is place as a sentence names it: step <n>, the retro, the merge, or no step for a
// task at no act.
func PlaceWords(place TaskPlace) string {
	switch place.Kind {
	case PlaceStep:
		return fmt.Sprintf("step %d", place.Position)
	case PlaceRetro:
		return "the retro"
	case PlaceMerge:
		return "the merge"
	}
	return "no step"
}
