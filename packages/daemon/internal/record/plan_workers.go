package record

import (
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// StepRecipient addresses a message to role's worker. Where the owner does that work, the
// recipient is the issue's own architect, whatever claim the step's phases row names. Otherwise
// the claim is left for that row to name (WorkerClaim). Callers judge owner from the issue's
// current place or the plan step whose worker they need.
func StepRecipient(issue Issue, role claim.Role, owner bool) (claim.Role, claim.Token, error) {
	if owner {
		token, err := OwnerClaim(issue)
		return role, token, err
	}
	return role, "", nil
}

// Worker is who works one place of a task on its plan rows (PlaceWorker): the role of the place,
// the agent that does it, and the place, from 1, of the step that agent was given.
type Worker struct {
	// Role is the role of the place: a step's own role, and at the retro the role of its run.
	Role claim.Role
	// Agent is the agent that does the place (plan.AgentOf): the owner of the task where its Key is
	// owner (Worker.Owner).
	Agent plan.Agent
	// Step is the place, from 1, of the step whose agent Agent is: the step itself, and at the
	// retro the task's last step of the type implementation; 0 for the owner's retro of a task with
	// no such step.
	Step int
}

// Owner says whether the owner of the task does the place.
func (w Worker) Owner() bool { return w.Agent.Owner() }

// ClaimRole is the role of the claim that works the place: the architect where the owner does it,
// whose claim keeps its role at a step of its own, and Role otherwise.
func (w Worker) ClaimRole() claim.Role {
	if w.Owner() {
		return claim.RoleArchitect
	}
	return w.Role
}

// Name is the name of the agent that does the place (PlanStep.AgentName), of its step on steps,
// the task's rows PlaceWorker read: none where the owner does it, whose claim is no name on the
// issue.
func (w Worker) Name(steps []PlanStep) claim.Agent {
	if w.Owner() || w.Step < 1 || w.Step > len(steps) {
		return ""
	}
	return steps[w.Step-1].AgentName()
}

// ownersRetroRole is the role of the retro the owner of a task does (PlaceWorker): the implementer,
// the role of the phase retro, on whose phases row the owner's retro is recorded, since the claim
// that works it keeps the architect's role. PlaceWorker gives it to every retro of the owner's and
// Issue.OwnersRowRole reads it for the row of the owner's retro, so the two never name two rows.
const ownersRetroRole = claim.RoleImplementer

// PlaceWorker is who works place of task, whose plan rows are steps, and ok false at the merge, at
// no act and at a step the task does not hold, which no agent of the plan works. At a step it is
// the step's own role and agent. At the retro it is read from the type each row records: the agent
// and the role of the task's last step of the type implementation, else, where the task has no such
// step or that step's key is owner, the owner, with the role implementer, the role of the phase
// retro, on whose phases row the owner's retro is recorded. It reads each step's type, never its
// role's name, so a role of the deployment or of the plan of the type implementation does the retro
// of a task whose last such step it is.
//
// It is the one statement of who works a place of a task: the workflow's walk (Walk.PlaceRole,
// AgentAt, OwnerAt and RoleAt), the route complete_step, the sentences of OWNER_STEP_NOT_RUNNING and
// SIGNOFF_NOT_THE_OWNERS, and the place of a role's phases row (PhaseRowAt) all read it, and the
// store states it again in SQL (plansql.PlaceRole), which a test holds to this one.
func PlaceWorker(task string, place TaskPlace, steps []PlanStep) (Worker, bool) {
	agentOf := func(position int) plan.Agent {
		step := steps[position-1]
		return plan.AgentOf(task, position, plan.Step{Role: step.Role, Key: step.AgentKey})
	}
	switch place.Kind {
	case PlaceStep:
		if place.Position < 1 || place.Position > len(steps) {
			return Worker{}, false
		}
		return Worker{Role: claim.Role(steps[place.Position-1].Role), Agent: agentOf(place.Position), Step: place.Position}, true
	case PlaceRetro:
		for i := len(steps) - 1; i >= 0; i-- {
			if steps[i].Type != plan.TypeImplementation {
				continue
			}
			agent := agentOf(i + 1)
			if agent.Owner() {
				return Worker{Role: ownersRetroRole, Agent: agent, Step: i + 1}, true
			}
			return Worker{Role: claim.Role(steps[i].Role), Agent: agent, Step: i + 1}, true
		}
		return Worker{Role: ownersRetroRole, Agent: plan.Agent{Key: "owner"}}, true
	}
	return Worker{}, false
}

// PhaseLeftBy is the phase role has left when its issue, standing at at, no longer stands where
// role works, on an issue whose one task is task with the plan rows steps (task "" for an issue
// with none): the phase its role last worked in the task. Once the issue stands past the retro,
// the agent of the task's retro (PlaceWorker at the retro) has left the retro, whichever role it
// is, as the implementer has under phase.LeftBy, so the retro of a role of the deployment or of the
// plan of the type implementation is the retro it reports and not a step; every other role, and
// every role with no plan rows, has left what phase.LeftBy answers, which reads the phase alone.
// It is the one reading of a completion that is not the work of where the task stands: the
// workflow's (completionOf, judgedReport) and the completion route's (api's answerCompletion),
// which judges by it what the report must carry, so the two judge a late report of a retro alike.
func PhaseLeftBy(role claim.Role, at phase.Phase, task string, steps []PlanStep) phase.Phase {
	if task != "" && phase.MovesBack(at, phase.Retro) {
		if retro, ok := PlaceWorker(task, TaskPlace{Kind: PlaceRetro}, steps); ok && retro.Role == role {
			return phase.Retro
		}
	}
	return phase.LeftBy(role, at)
}

// ReturnTarget is the step, from 1, that the failed step at position of a task on steps returns
// its work to when its report names none: the first step before it whose recorded type is
// implementation. ok is false where no step before it has that type, and for a position that is no
// step of steps: the work then returns nowhere. A step of a role the deployment defines may stand
// before its task's implementer, and a stored type that no plan gives may stand anywhere; each
// reaches that answer. The workflow's return of a failed step (Walk.returnTarget) and the owner's
// fail of its acceptance (api's ownersAcceptance) both read it.
func ReturnTarget(steps []PlanStep, position int) (int, bool) {
	if position < 1 || position > len(steps) {
		return 0, false
	}
	for i, step := range steps[:position-1] {
		if step.Type == plan.TypeImplementation {
			return i + 1, true
		}
	}
	return 0, false
}
