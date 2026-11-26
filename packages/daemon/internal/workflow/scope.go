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

// TaskScope is one task of an issue's run that a step handler acts on, read once in the handler's
// transaction: the copy of the issue read for that task, the task's walk, and the plan both were
// read from. For a run with plan rows the copy stands where its task stands (scopeOn) and names
// the task (record.Issue.ViewTask, which the store refuses to write), whatever the plan's count of
// tasks; a run with no plan rows is its issue itself, walking today's line (noPlanScope). Every
// function that starts, moves, holds, stops or restarts a step takes one, and its fields are
// unexported, so a scope is made only by the readers of this file: a bare record.Issue reaches no
// step, and the one place a run with no plan rows becomes a scope is noPlanScope.
type TaskScope struct {
	issue record.Issue
	walk  Walk
	plan  *record.StoredPlan
}

// Issue is the copy of the issue read for s's task.
func (s TaskScope) Issue() record.Issue { return s.issue }

// Walk is the walk of s's task.
func (s TaskScope) Walk() Walk { return s.walk }

// Task is the id of s's task: a task of the plan, or defaultTask for a run with no plan rows.
func (s TaskScope) Task() string { return s.walk.Task }

// Planned says whether s's run has plan rows.
func (s TaskScope) Planned() bool { return s.plan != nil }

// textNamesTask is the one presentation rule of the workflow's texts about s's task: a text names
// the task only where s's plan holds a choice of tasks, so a plan of one task, and a run with no
// plan rows, say what they always said. heldSummary (a held notice's summary) and arrive's journal
// line read it, and nothing else does: no rule of what a step does reads the plan's count of tasks.
func textNamesTask(s TaskScope) bool { return s.plan != nil && len(s.plan.Tasks) > 1 }

// StepKey is the step of s's task the copy stands in, as a phases row keys it
// (record.PhaseRow.StepTask, StepPlace, which enter writes): the task and the place of the step
// (placeOnWalk), from 1 for a step and 0 for the retro and the merge; ("", 0) for a run with no
// plan rows and for a task at no act.
func (s TaskScope) StepKey() (string, int) {
	place, _, _, ok := placeOnWalk(s.walk, standingIn(record.StandsAt(s.issue)))
	if s.plan == nil || !ok {
		return "", 0
	}
	return s.walk.Task, place.Place
}

// scopeOn is the scope of task of stored, the plan of issue's run, which the caller read and which
// is not nil, whatever its count of tasks. False where the plan holds no such task. The copy stands
// where the task stands (scopeStanding).
func scopeOn(issue record.Issue, stored *record.StoredPlan, task string) (TaskScope, bool) {
	row, ok := stored.TaskOf(task)
	if !ok {
		return TaskScope{}, false
	}
	w := walkOfTask(issue.Key, stored, row, stored.Merging())
	issue.Phase, issue.Hold = scopeStanding(issue, w, stored)
	issue.ViewTask = task
	return TaskScope{issue: issue, walk: w, plan: stored}, true
}

// scopeStanding is where the copy of issue read for w's task, of stored, stands: the task's own
// phase and hold (taskStanding), with four readings of the issue's own standing kept over it.
//   - A running task whose issue is held from that task's phase stands held, with the issue's hold:
//     a hold the release before wrote on the issue alone, beside its running task, which
//     record.RunningOf reads as that step held.
//   - A root that waits on a person before its first step stands in awaiting_person over the
//     admitted of a task at no act.
//   - A running task whose issue waits on its open Dispatch children holding the start of that
//     task's phase (record.ChildrenWait.Start) stands in awaiting_children, where its task stood
//     through the wait (awaitChildren): no hold is from that phase, and no step of the task runs
//     there (dispatch://INFRA-3320 R11).
//   - A task waiting at no act while no other task of the plan has started stands where the issue
//     stands: plan rows written while the issue already stood in a step, before any move named a
//     place (Walk.placeIn reads such a task so). A task that waits while another task has run,
//     done among them, stands at admitted, so no step of it stands before its start: no run of it
//     is opened (OpenUncountedRuns) or ended before its first step.
func scopeStanding(issue record.Issue, w Walk, stored *record.StoredPlan) (phase.Phase, *record.Hold) {
	at, hold := taskStanding(w)
	switch {
	case w.State == record.TaskRunning && issue.Phase == phase.Held && issue.Hold != nil && issue.Hold.From == at:
		return issue.Phase, issue.Hold
	case at == phase.Admitted && issue.Phase == phase.AwaitingPerson:
		return issue.Phase, issue.Hold
	case w.State == record.TaskRunning && waitsHolding(issue, at):
		return issue.Phase, nil
	case w.State == record.TaskWaiting && w.Place.Kind == "" && !otherTaskStarted(stored, w.Task):
		return issue.Phase, issue.Hold
	}
	return at, hold
}

// waitsHolding says whether issue stands in awaiting_children holding the start of at
// (record.ChildrenWait.Start), the phase its task stands at through the wait (awaitChildren).
func waitsHolding(issue record.Issue, at phase.Phase) bool {
	return issue.Phase == phase.AwaitingChildren && issue.ChildrenWait != nil && issue.ChildrenWait.Start == at
}

// otherTaskStarted says whether a task of stored other than task has left its wait: running,
// held, stopped, done or left.
func otherTaskStarted(stored *record.StoredPlan, task string) bool {
	return slices.ContainsFunc(stored.Tasks, func(row record.PlanTask) bool {
		return row.Task != task && row.State != record.TaskWaiting
	})
}

// noPlanScope is the scope of a run with no plan rows: the issue itself, walking today's line as
// the task default (todaysWalk). It is the one adapter of such a run, which reads and runs exactly
// as before plans: a run from before plans, a generation not started, and a run resumed after a
// rollback.
func noPlanScope(issue record.Issue) TaskScope {
	issue.ViewTask = ""
	return TaskScope{issue: issue, walk: todaysWalk()}
}

// scopesOn is the scope of each task of stored, the plan of issue's run, in task id order, and
// the one scope noPlanScope makes where stored is nil.
func scopesOn(issue record.Issue, stored *record.StoredPlan) []TaskScope {
	if stored == nil {
		return []TaskScope{noPlanScope(issue)}
	}
	scopes := make([]TaskScope, 0, len(stored.Tasks))
	for _, row := range stored.Tasks {
		s, _ := scopeOn(issue, stored, row.Task)
		scopes = append(scopes, s)
	}
	return scopes
}

// scope is the scope of task of issue's run, read in tx: noPlanScope for a run with no plan rows,
// whatever task is named; false where the plan holds no such task.
func (e *Engine) scope(ctx context.Context, tx pgx.Tx, issue record.Issue, task string) (TaskScope, bool, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return TaskScope{}, false, err
	}
	if stored == nil {
		return noPlanScope(issue), true, nil
	}
	s, ok := scopeOn(issue, stored, task)
	return s, ok, nil
}

// ScopeOf is scope for a task the caller holds from the plan (a task of a fact, a ready task): a
// task the plan does not hold is an error. It is exported for the tests of other packages, which
// hand a scope to the engine's exported starts as production's readers do.
func (e *Engine) ScopeOf(ctx context.Context, tx pgx.Tx, issue record.Issue, task string) (TaskScope, error) {
	s, ok, err := e.scope(ctx, tx, issue, task)
	if err == nil && !ok {
		err = fmt.Errorf("read %s at generation %d for task %s: its plan holds no such task", issue.Key, issue.Generation, task)
	}
	return s, err
}

// scopes is the scope of each task of issue's run, in task id order, read in tx (scopesOn): a
// reader that reads every step of the issue reads each of them.
func (e *Engine) scopes(ctx context.Context, tx pgx.Tx, issue record.Issue) ([]TaskScope, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return nil, err
	}
	return scopesOn(issue, stored), nil
}

// ScopeOfStep is the scope a reader that weighs or gives step its work reads, read in tx from its
// run's plan rows (ScopeOfStepOn). It is the one read of each reader that gives one step its work:
// the boot's resume (suspendedPhaseOwners), the ready of a claim (readyWorkerTask) and the silence
// repair (phaseWorkerRecord); admission's repairs over phase workers (admit's phaseWorkersOf), which
// weigh every issue of a project on every fact, read the plans of all of them at once
// (record.Store.PlansOf) and take each scope from ScopeOfStepOn.
func ScopeOfStep(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue, step record.RunningStep) (TaskScope, error) {
	stored, err := store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return TaskScope{}, err
	}
	return ScopeOfStepOn(issue, stored, step)
}

// ScopeOfStepOn is ScopeOfStep on stored, the plan of issue's run, which the caller read: the scope
// of step's task, whose copy stands in that task's phase, so a walk of it and the resume composed on
// it (ResumeTask) are that task's; for a run with no plan rows the issue itself standing at the
// phase step runs as. What the reader queues for the step names the step's phase, so two steps of
// one issue at two phases are two starts rather than one start of the issue's phase.
func ScopeOfStepOn(issue record.Issue, stored *record.StoredPlan, step record.RunningStep) (TaskScope, error) {
	issue.Phase = step.Phase
	if stored == nil {
		return noPlanScope(issue), nil
	}
	s, ok := scopeOn(issue, stored, step.Task)
	if !ok {
		return TaskScope{}, fmt.Errorf("read %s at generation %d for the step of task %s: its plan holds no such task", issue.Key, issue.Generation, step.Task)
	}
	return s, nil
}

// scopeOfAgent is the scope of the task whose step a fact of role's claim token is of: the task
// whose step the claim's agent runs (record.Issue.StepOfAgent, a held step among them), else the
// held or stopped task whose step at its place has that claim (rowClaim), first in task id order,
// since a stopped task's step is no running step: the one reading of a fact of a step that reaches
// its task by the claim it names, whatever the plan's count of tasks. A fact whose token names no
// agent is of role's own claim (factAgent, factClaim). ok is false where the claim runs no step of
// the plan and stands at no held or stopped one: a step that waits for its agent is none of them. A
// run with no plan rows is its issue, whatever claim is named.
func (e *Engine) scopeOfAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, token claim.Token) (TaskScope, bool, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return noPlanScope(issue), err == nil, err
	}
	if step, ok := issue.StepOfAgent(factAgent(role, token)); ok {
		s, ok := scopeOn(issue, stored, step.Task)
		return s, ok, nil
	}
	if token = factClaim(issue, role, token); token == "" {
		return TaskScope{}, false, nil
	}
	return e.heldOrStoppedOf(issue, stored, role, token)
}

// heldOrStoppedOf is the scope of the held or stopped task of stored, the plan of issue's run, whose
// step at its place has the claim token for role (rowClaim), first in task id order: a stopped
// task's step is no running step, so a fact of its claim reaches it here (scopeOfAgent,
// completionScope). ok is false where no such task stands.
func (e *Engine) heldOrStoppedOf(issue record.Issue, stored *record.StoredPlan, role claim.Role, token claim.Token) (TaskScope, bool, error) {
	for _, s := range scopesOn(issue, stored) {
		if s.walk.State != record.TaskHeld && s.walk.State != record.TaskStopped {
			continue
		}
		at, err := e.rowClaim(s, role)
		if err != nil {
			return TaskScope{}, false, err
		}
		if at == token {
			return s, true, nil
		}
	}
	return TaskScope{}, false, nil
}

// agentOf is the agent whose claim token is token (claim.Token.Cut), "" for a token that names
// none.
func agentOf(token claim.Token) claim.Agent {
	_, agent, _ := token.Cut()
	return agent
}

// factAgent is the agent a fact of role's claim token is of: the agent token names (agentOf), and
// role's own where it names none, the agent of a step with no agent of its own (as every step of a
// run with no plan rows is, record.RunningOf): a fact built without its claim is of the role's own.
func factAgent(role claim.Role, token claim.Token) claim.Agent {
	if agent := agentOf(token); agent != "" {
		return agent
	}
	return claim.Agent(role)
}

// factClaim is the claim a fact of role's claim token is of on issue, as a phases row and a step's
// start name it (rowClaim): token where it names an agent, and role's own token (claim.NewToken)
// where it names none, as factAgent reads it; none where issue names no claims (recordsSteps) or
// role is no role, which claim.NewToken refuses.
func factClaim(issue record.Issue, role claim.Role, token claim.Token) claim.Token {
	if agentOf(token) != "" {
		return token
	}
	if !recordsSteps(issue) {
		return ""
	}
	own, err := stepClaim(issue, issue.Key, role)
	if err != nil {
		return ""
	}
	return own
}

// scopeOfPullRequest is the scope of the task whose branch pr is on (record.StoredPlan.TaskOfBranch,
// the rule every read of a pull request's task reads by, record.PullRequest.Task): the one reading
// of a fact of a pull request that reaches its task, so a fact of one task's pull request moves that
// task and no other, and one of a done task's moves none, its copy standing in done. ok is false
// where the branch is no one task's: a pull request whose facts move nothing, which is logged. A
// run with no plan rows is its issue, whatever branch the pull request is on.
func (e *Engine) scopeOfPullRequest(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest) (TaskScope, bool, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return noPlanScope(issue), err == nil, err
	}
	task, ok := stored.TaskOfBranch(issue.Key, pr.Branch)
	if !ok {
		e.logOnCommit(ctx, "workflow: a fact of a pull request on a branch that no one task of its issue holds moves no task",
			"issue", issue.Key, "pullRequest", pr.Number, "branch", pr.Branch, "tasks", stored.TasksOfBranch(issue.Key, pr.Branch))
		return TaskScope{}, false, nil
	}
	s, ok := scopeOn(issue, stored, task)
	return s, ok, nil
}

// issueOfPullRequest is the scope of the issue pr is recorded for, read for the task whose branch
// pr is on (scopeOfPullRequest): what each handler of a fact of the pull request acts on. It is nil
// where the record holds no such issue, and where the branch is no one task's, so the fact records
// the pull request and moves nothing.
func (e *Engine) issueOfPullRequest(ctx context.Context, tx pgx.Tx, pr record.PullRequest) (*TaskScope, error) {
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return nil, err
	}
	s, ok, err := e.scopeOfPullRequest(ctx, tx, *issue, pr)
	if err != nil || !ok {
		return nil, err
	}
	return &s, nil
}

// openStepsOfPullRequest is the steps of issue that take work (OpenStepsOf) of each task whose
// branch pr is on (record.StoredPlan.TasksOfBranch): the steps a settlement of the pull request's
// checks is the business of, and no step of a task on another branch. Every step of a run with no
// plan rows is.
func (e *Engine) openStepsOfPullRequest(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest) ([]record.RunningStep, error) {
	open := OpenStepsOf(issue)
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return open, err
	}
	tasks := stored.TasksOfBranch(issue.Key, pr.Branch)
	return slices.DeleteFunc(open, func(step record.RunningStep) bool { return !slices.Contains(tasks, step.Task) }), nil
}

// completionScope is the scope a completion (fact, intake.HandoffComplete) is read on: the task the
// route named (fact.Task); for a fact that names none, the task of the step the claim's agent runs
// now (record.Issue.StepOfAgent), else the task of the latest phases row that names the claim (a
// completion of a step the task has left), else the held or stopped task whose step has that claim
// (heldOrStoppedOf: the report of the run a stop ended), the claim and its agent read as factClaim
// and factAgent read them. A completion of the role's own claim (claim.NewToken) that none of them
// reaches is of the one task of the plan that has a merge (record.StoredPlan.OnlyTaskThatMerges):
// the claim of a row raised before the plan, or of a step a later version took off the task's line;
// where several tasks have a merge it is of none. A keyed agent's claim that none of them reaches
// runs no step of the plan, and ok is false: the completion is of no step, and moves no task. A run
// with no plan rows is its issue.
func (e *Engine) completionScope(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.HandoffComplete) (TaskScope, bool, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return noPlanScope(issue), err == nil, err
	}
	task := fact.Task
	if task == "" {
		if step, ok := issue.StepOfAgent(factAgent(fact.Role, fact.Claim)); ok {
			task = step.Task
		}
	}
	token := factClaim(issue, fact.Role, fact.Claim)
	if task == "" && token != "" {
		rows, err := e.store.Phases(ctx, tx, issue.Key)
		if err != nil {
			return TaskScope{}, false, err
		}
		// A row names the claim its completion carried (handoff writes it) or the claim its entry
		// wrote (enter), so either reading of the fact's claim finds its row.
		var latest int64
		for _, row := range rows {
			named := row.Claim == token || fact.Claim != "" && row.Claim == fact.Claim
			if named && row.StepTask != "" && row.Entry > latest {
				task, latest = row.StepTask, row.Entry
			}
		}
	}
	if task == "" && token != "" {
		if s, ok, err := e.heldOrStoppedOf(issue, stored, fact.Role, token); err != nil || ok {
			return s, ok, err
		}
		if own, err := stepClaim(issue, issue.Key, fact.Role); err == nil && token == own {
			task, _ = stored.OnlyTaskThatMerges()
		}
	}
	s, ok := scopeOn(issue, stored, task)
	return s, ok, nil
}

// actScope is the scope an owner's call on the act at answers is read on (sign_off and the
// architect's move back, at accepting): the task the call names, or, where it names none, the one
// task that stands at at. A named task that does not stand at at is refused (intake.TaskNotAtPhase),
// and so is a call that names none while two or more tasks stand there (intake.TaskRequired). With
// none there ok is false: the issue stands at another act, and each call refuses it with the
// issue's own phase. A run with no plan rows is its issue, whatever the call names.
func (e *Engine) actScope(ctx context.Context, tx pgx.Tx, issue record.Issue, task string, at phase.Phase) (TaskScope, bool, *intake.Refusal, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return noPlanScope(issue), err == nil, nil, err
	}
	if task != "" {
		s, ok := scopeOn(issue, stored, task)
		switch {
		case !ok:
			return TaskScope{}, false, intake.TaskNotAtPhase(issue.Key, task, "", at), nil
		case s.issue.Phase != at:
			return TaskScope{}, false, intake.TaskNotAtPhase(issue.Key, task, s.issue.Phase, at), nil
		}
		return s, true, nil, nil
	}
	var standing []TaskScope
	var ids []string
	for _, s := range scopesOn(issue, stored) {
		if s.issue.Phase == at {
			standing, ids = append(standing, s), append(ids, s.walk.Task)
		}
	}
	switch len(standing) {
	case 0:
		return TaskScope{}, false, nil, nil
	case 1:
		return standing[0], true, nil, nil
	}
	return TaskScope{}, false, intake.TaskRequired(issue.Key, ids, at), nil
}

// backwardScope is the scope a move back (fact) is read on: the owner's, at accepting, on the task
// it names or the one task there (actScope); a phase worker's on the task its route names
// (intake.BackwardMove.Task), else the task of the step its claim's agent runs, else the one task of
// the plan that has a merge (record.StoredPlan.OnlyTaskThatMerges). The owner's move that names no
// task while none stands at accepting, and a worker's whose task the plan does not hold, are
// refused and change nothing. A run with no plan rows is its issue.
func (e *Engine) backwardScope(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.BackwardMove) (TaskScope, *intake.Refusal, error) {
	if fact.Requester == claim.RoleArchitect {
		s, ok, refusal, err := e.actScope(ctx, tx, issue, fact.Task, phase.Accepting)
		if err != nil || refusal != nil || ok {
			return s, refusal, err
		}
		return TaskScope{}, refused("BACKWARD_REFUSED", fmt.Sprintf("no task of %s stands at %s, the act the owner's move back is of, so the %s's move to %s changed nothing",
			issue.Key, phase.Accepting, fact.Requester, fact.To)).Refusal, nil
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return noPlanScope(issue), nil, err
	}
	task := fact.Task
	if task == "" {
		if step, ok := issue.StepOfAgent(agentOf(fact.By)); ok {
			task = step.Task
		} else {
			task, _ = stored.OnlyTaskThatMerges()
		}
	}
	if s, ok := scopeOn(issue, stored, task); ok {
		return s, nil, nil
	}
	return TaskScope{}, refused("BACKWARD_REFUSED", fmt.Sprintf("the %s's claim runs no step of %s, so its move back to %s changed nothing",
		fact.Requester, issue.Key, fact.To)).Refusal, nil
}

// heldScope is the scope of the held task an owner's decision on a hold is of (retryOrEscalate):
// the task it names; else the task of the claim it names, the operator's retry of one claim: the
// task whose step that claim's agent runs (record.Issue.StepOfAgent), or, where its agent runs
// none, the one held or stopped task whose step at its place is that agent's; else the one held
// task of the plan, else the first stopped task in task id order, whose hold its stop is
// (planHoldRefusal refuses its release). A task stands held where its copy does (scopeStanding): a
// held task, and a running task whose issue the release before held from its phase. A named task
// that does not stand held is refused (intake.TaskNotAtPhase), and so is a decision that names no
// task while two or more tasks are held, or a claim whose agent stands at no held or stopped task
// or at two of them (intake.TaskRequired). With no task held or stopped the scope is the zero one,
// which stands in no hold, and the decision changes nothing. A run with no plan rows is its issue.
func (e *Engine) heldScope(ctx context.Context, tx pgx.Tx, issue record.Issue, task string, by claim.Token) (TaskScope, *intake.Refusal, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return noPlanScope(issue), nil, err
	}
	if task != "" {
		s, ok := scopeOn(issue, stored, task)
		switch {
		case !ok:
			return TaskScope{}, intake.TaskNotAtPhase(issue.Key, task, "", phase.Held), nil
		case s.issue.Phase != phase.Held:
			return TaskScope{}, intake.TaskNotAtPhase(issue.Key, task, s.issue.Phase, phase.Held), nil
		}
		return s, nil, nil
	}
	if by != "" {
		if step, ok := issue.StepOfAgent(agentOf(by)); ok {
			s, _ := scopeOn(issue, stored, step.Task)
			return s, nil, nil
		}
	}
	var held, stopped []TaskScope
	var heldIDs, holding, agents []string
	var agentsScope TaskScope
	for _, s := range scopesOn(issue, stored) {
		if s.issue.Phase != phase.Held {
			continue
		}
		if s.walk.State == record.TaskStopped {
			stopped = append(stopped, s)
		} else {
			held, heldIDs = append(held, s), append(heldIDs, s.walk.Task)
		}
		holding = append(holding, s.walk.Task)
		if by != "" && s.walk.AgentAt(s.walk.Place) == agentOf(by) {
			agents, agentsScope = append(agents, s.walk.Task), s
		}
	}
	switch {
	case len(holding) == 0:
		return TaskScope{}, nil, nil
	case by != "" && len(agents) == 0:
		return TaskScope{}, intake.TaskRequired(issue.Key, holding, phase.Held), nil
	case by != "" && len(agents) == 1:
		return agentsScope, nil, nil
	case by != "":
		return TaskScope{}, intake.TaskRequired(issue.Key, agents, phase.Held), nil
	case len(held) > 1:
		return TaskScope{}, intake.TaskRequired(issue.Key, heldIDs, phase.Held), nil
	}
	return slices.Concat(held, stopped)[0], nil, nil
}

// PromotedScope is the scope a re-admitted tree's promotion starts role on (admit's
// startMidPhaseMembers): the scope of the task whose step role works among the steps that take
// work (OpenStepsOf, the first in task id order, as the promotion takes each role once), else the
// one task of the plan that has a merge (record.StoredPlan.OnlyTaskThatMerges). The promotion reads
// its start's claim (PromotedWorkerClaim), weighs that claim's run against the copy's phase
// (StartFor) and starts the role (StartPromotedWorker) on it. A run with no plan rows is its issue.
func (e *Engine) PromotedScope(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role) (TaskScope, error) {
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return noPlanScope(issue), err
	}
	task, _ := stored.OnlyTaskThatMerges()
	open := OpenStepsOf(issue)
	if at := slices.IndexFunc(open, func(step record.RunningStep) bool { return step.Role == role }); at >= 0 {
		task = open[at].Task
	}
	s, ok := scopeOn(issue, stored, task)
	if !ok {
		return TaskScope{}, fmt.Errorf("read %s at generation %d for the promotion of its %s: its plan holds no task %q", issue.Key, issue.Generation, role, task)
	}
	return s, nil
}

// putScope writes the issue s's copy was read from where its tasks put it, and answers the issue
// as written. A run with no plan rows is written as given. A run with plan rows stands where its
// tasks put it (issuePhaseOfTasks over s's plan with s's walk in place of its task's row,
// planWithWalk), with the Dispatch status of that phase where it has one and the issue shows
// another, whatever the plan's count of tasks; a root that waits on a person before its first step
// stands in awaiting_person over the admitted its tasks derive, and a hold the release before wrote
// on the issue alone beside s's running task stands (heldBefore). It reads no plan: every writer of
// s's task sets what it wrote on s's walk before it calls, and every reader that writes several
// tasks in one transaction reads each next scope on the plan the write before left (startReady) or
// again from the store (releaseHeldRoot, afterVersion, stopLostRoles), so s's plan holds every write
// of the transaction. s's copy keeps standing where its task stands, takes the status and the run's
// mark (record.Issue.TakenBack) the write gave the issue, and its plan takes its task's row as
// written.
func (e *Engine) putScope(ctx context.Context, tx pgx.Tx, s *TaskScope) (record.Issue, error) {
	written := s.issue
	written.ViewTask = ""
	if s.plan == nil {
		return written, e.store.PutIssue(ctx, tx, written)
	}
	stored := planWithWalk(s.plan, s.walk)
	written.Phase, written.Hold = derivedStanding(s.issue, walksOf(written.Key, stored), heldBefore(*s))
	if status := PhaseStatus(written.Phase); status != "" && status != written.Status {
		if err := e.status(ctx, tx, written, status, ""); err != nil {
			return record.Issue{}, err
		}
		written.Status = status
	}
	if written.CarriesRun() {
		written.TakenBack = true
	}
	if err := e.store.PutIssue(ctx, tx, written); err != nil {
		return record.Issue{}, err
	}
	s.issue.Status, s.issue.TakenBack, s.plan = written.Status, written.TakenBack, stored
	return written, nil
}

// planWithWalk is stored with the row of w's task standing where w stands: its state, its place,
// its stop's and its hold's reasons (each kept only in the state it belongs to), the wait of its
// step and the reason that step started, the fields the writers of a task write
// (record.Store.MoveTask, HoldTask, ReleaseTask, StopTask, SetStepWait, SetStartReason). The plan
// is a copy: stored is left as it was.
func planWithWalk(stored *record.StoredPlan, w Walk) *record.StoredPlan {
	next := *stored
	next.Tasks = slices.Clone(stored.Tasks)
	for i := range next.Tasks {
		row := &next.Tasks[i]
		if row.Task != w.Task {
			continue
		}
		row.State, row.Place, row.StepWait, row.StepWaitSince, row.StartReason = w.State, w.Place, w.Wait, w.WaitSince, w.StartReason
		row.StoppedReason, row.HoldReason = "", ""
		if w.State == record.TaskStopped {
			row.StoppedReason = w.StoppedReason
		}
		if w.State == record.TaskHeld {
			row.HoldReason = w.HoldReason
		}
	}
	return &next
}

// derivedStanding is where an issue standing as issue stands for its tasks, the walk of each
// (issuePhaseOfTasks), with the three readings of the issue's own standing kept over it: a root
// that waits on a person before its first step stands in awaiting_person over admitted, a parent
// that waits on its open Dispatch children stands in awaiting_children over the phase of the start
// it holds where its tasks derive that phase and no hold (waitsHolding), and, where keepsHold says
// issue's hold is one the release before wrote on the issue alone beside its running task
// (heldBefore, heldBeforeOver), that hold stands where the tasks derive none and put the issue in
// the phase it is from.
func derivedStanding(issue record.Issue, tasks []Walk, keepsHold bool) (phase.Phase, *record.Hold) {
	at, hold := issuePhaseOfTasks(tasks)
	switch {
	case at == phase.Admitted && issue.Phase == phase.AwaitingPerson:
		return issue.Phase, nil
	case hold == nil && waitsHolding(issue, at):
		return issue.Phase, nil
	case keepsHold && hold == nil && issue.Phase == phase.Held && issue.Hold != nil && issue.Hold.From == at:
		return issue.Phase, issue.Hold
	}
	return at, hold
}

// heldBefore says whether s's copy holds a hold the release before wrote on the issue alone beside
// s's task, which the store holds running (scopeStanding's first reading): s's task runs, and the
// copy is held from that task's own phase. A hold of s's task itself holds or stops the task
// (record.Store.HoldTask, StopTask), and its writer sets that state on s's walk, so a stop of one
// task holds the issue only where no other task runs.
func heldBefore(s TaskScope) bool {
	return s.walk.State == record.TaskRunning && s.issue.Phase == phase.Held && s.issue.Hold != nil &&
		s.issue.Hold.From == issuePhaseOf(s.walk, s.walk.Place, record.TaskRunning)
}

// heldBeforeOver says whether a hold of an issue whose tasks are tasks can be one the release
// before wrote on the issue alone, for a write of the issue itself (WriteIssue), which holds no
// task of its own: a task runs, and no task is held or stopped, so the phase the hold is from is a
// running task's.
func heldBeforeOver(tasks []Walk) bool {
	running := false
	for _, w := range tasks {
		switch w.State {
		case record.TaskHeld, record.TaskStopped:
			return false
		case record.TaskRunning:
			running = true
		}
	}
	return running
}

// taskPullRequest is the pull request of s's task: a planned task's own row
// (record.Store.PullRequestOfTask), nil where it has none, and for a run with no plan rows the row
// of its merge (record.Store.MergePullRequest). It reads the record whole, with what it remembers
// of its settled heads, for a handler that writes it back or hands it to a move that may
// (transition, whose mergeForwardOnEntry writes the merge forward).
func (e *Engine) taskPullRequest(ctx context.Context, tx pgx.Tx, s TaskScope) (*record.PullRequest, error) {
	if s.plan == nil {
		return e.store.MergePullRequest(ctx, tx, s.issue.Key)
	}
	return e.store.PullRequestOfTask(ctx, tx, s.issue.Key, s.walk.Task)
}

// taskPullRequestRow is the pull request taskPullRequest answers, by the same rule, as its row
// alone (record.Store.PullRequestRowOfTask, MergePullRequestRow): with none of what it remembers
// of its settled heads, for a reader that never writes the record back, hands it to no move, and
// reads neither its SettledHeads nor its head run's Settled.
func (e *Engine) taskPullRequestRow(ctx context.Context, tx pgx.Tx, s TaskScope) (*record.PullRequest, error) {
	if s.plan == nil {
		return e.store.MergePullRequestRow(ctx, tx, s.issue.Key)
	}
	return e.store.PullRequestRowOfTask(ctx, tx, s.issue.Key, s.walk.Task)
}

// stepRow is the phases row of role's step of s, the one read of it in a step handler: the row of
// role (record.PhaseRowOf), which keys no step yet.
func (e *Engine) stepRow(ctx context.Context, tx pgx.Tx, s TaskScope, role claim.Role) (record.PhaseRow, error) {
	return e.phaseRow(ctx, tx, s.issue.Key, role)
}

// heldSummary is what a held notice of s's task says of the step it holds (heldTaskSummary): its
// owner reads which of its tasks is held only where the plan holds a choice of them
// (textNamesTask), so a plan of one task, and a run with no plan rows, say none.
func heldSummary(s TaskScope) string {
	if !textNamesTask(s) {
		return ""
	}
	return heldTaskSummary(s.walk)
}

// rowClaim is the claim of the agent of the step s's copy stands in for role (placeToken): the
// claim an entry of that step writes on role's phases row (enter), which record.WorkerClaim reads
// back; role's own token where the walk holds no place there.
func (e *Engine) rowClaim(s TaskScope, role claim.Role) (claim.Token, error) {
	return e.placeToken(s, record.StandsAt(s.issue), role)
}

// startClaim is the claim a start of role's step of s names (record.SuperviseRequest.Claim): for a
// run with plan rows, whatever the plan's count of tasks, the claim the step's entry writes on the
// row (rowClaim), so a start of one task's step takes out no start of another's; none for a run
// with no plan rows, whose start reaches the claim of its role's row when it runs, as before plans,
// and none for an issue whose key names no claims (recordsSteps), whose entry writes none (enter).
func (e *Engine) startClaim(s TaskScope, role claim.Role) (claim.Token, error) {
	if s.plan == nil || !recordsSteps(s.issue) {
		return "", nil
	}
	return e.rowClaim(s, role)
}

// scopeOrNoPlan is the scope of task of stored, the plan of issue's run at the generation issue
// names, which the caller read: noPlanScope where stored is nil, whatever task is named; false
// where the plan holds no such task.
func scopeOrNoPlan(issue record.Issue, stored *record.StoredPlan, task string) (TaskScope, bool) {
	if stored == nil {
		return noPlanScope(issue), true
	}
	return scopeOn(issue, stored, task)
}

// copyStoppedStep is the step the stop of issue's task ended (record.Store.StoppedStep): for a copy
// of the issue read for one task (scopeOn), that task's own (record.Store.StoppedStepOf), and for
// the issue itself the first stopped task's. It is the one reader of the task a copy is read for
// outside the readers of this file.
func copyStoppedStep(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue) (record.RunningStep, bool, error) {
	if issue.ViewTask != "" {
		return store.StoppedStepOf(ctx, tx, issue, issue.ViewTask)
	}
	return store.StoppedStep(ctx, tx, issue)
}
