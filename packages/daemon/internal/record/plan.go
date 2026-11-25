package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// The plan of an issue is four tables (store migrations 20261117034837_issue_plans and
// 20261125114700_plan_roles): the versions given for it (issue_plans), the tasks of the plan
// (plan_tasks), each with where it stands and its counts of returns, the steps of each task
// (plan_steps), and the roles the plan defines (plan_roles). Every row is keyed by the issue and
// its generation, and no row names an issues row, because an owner can give a plan before the
// daemon records its issue: such a plan is written at generation 0. An issue that ran before plans
// has no rows, and goes on as it did then. The count of starts of a step is not the plan's: it lives
// in step_starts alone, at the place TaskPlace.StepPlace names.

const (
	// PlanSourceOwner is a version the issue's owner gave (register_plan), at the version of its
	// document.
	PlanSourceOwner = "owner"
	// PlanSourceDefault is the project's default steps as one task, written at the start of the
	// work of a generation no owner's plan reached, or whose experiment's group takes no plan from
	// its owner: version 0, no document and no giver.
	PlanSourceDefault = "default"
)

// TaskState is the state of a task (PlanTask.State). A task whose issue leaves the workflow before
// the task is done is left: it keeps its place, and the reason it was stopped for where it was
// stopped, and nothing starts it again, since a todo opens a new run with rows of its own.
type TaskState string

// The states of a task, each the word plan_tasks.state stores.
const (
	TaskWaiting TaskState = "waiting"
	TaskRunning TaskState = "running"
	TaskDone    TaskState = "done"
	TaskStopped TaskState = "stopped"
	TaskLeft    TaskState = "left"
)

// The kinds of act a task stands at (TaskPlace.Kind), which are the kinds the count of starts keys
// a place by (StepPlace.Kind) and a step record names: a step of the task, from place 1, and the
// task's retro and its merge, at place 0, which no plan names. Each is the word contracts gives
// the kind of a step record (steprecord). A task at no act has the kind "" at place 0.
const (
	PlaceStep  = steprecord.KindStep
	PlaceRetro = steprecord.KindRetro
	PlaceMerge = steprecord.KindMerge
)

// PlanVersion is one version of the plan of an issue at one generation. The highest version of a
// generation is the plan in force. An owner's version is the version of its document (ArtifactID)
// it gave, and GivenBy the claim token of the owner that gave it; a default plan has version 0, no
// document and no giver.
type PlanVersion struct {
	Issue      string
	Generation uint64
	Version    int
	ArtifactID string
	Source     string
	GivenBy    claim.Token
	GivenAt    time.Time
}

// TaskPlace is where a task stands: at a step (PlaceStep, Position from 1), at its retro or its
// merge (Position 0), or at no act (Kind "", Position 0).
type TaskPlace struct {
	Kind     string
	Position int
}

// StepPlace is the place task stands at as the count of starts keys it: the start of the step
// running there raises that count (RaiseStepStarts), and its record's id names it.
func (p TaskPlace) StepPlace(task string) StepPlace {
	return StepPlace{Task: task, Kind: p.Kind, Place: p.Position}
}

// Started says whether a task standing at p has started its work: it stands at a step, its retro
// or its merge. A task at no act has not: it waits there for its first step, and a stop keeps it
// there (StopTask keeps the place), so a task stopped at no act is one whose work never ran.
func (p TaskPlace) Started() bool {
	return p != TaskPlace{}
}

// Rank orders the places of one task's line: no act first, then each step by its place, then the
// retro, then the merge. It is the one order of a task's places: a move back is a move to a lower
// rank (workflow's Walk.MovesBack), a step that ran stands at a lower rank than the place the task
// stands at, whatever phase each runs as (every step of a role the deployment defines runs as
// step, so only the place tells two of them apart), and the place of a role nearest where a task
// stands is read by it (NearestPlace).
func (p TaskPlace) Rank() int {
	switch p.Kind {
	case PlaceStep:
		return p.Position
	case PlaceRetro:
		return math.MaxInt - 1
	case PlaceMerge:
		return math.MaxInt
	}
	return 0
}

// NearestPlace is the place of places, in line order, nearest at: the latest at or before at
// (Rank), else the first after it; ok false where places is empty. A role can stand at two places
// of a task, and the one a reader with no run to go by means is the one the task stands at or left
// last, or, before the first, the first: where a role's row is read and written (RolePlace) and
// which acceptance the task stands at (workflow's acceptedRole).
func NearestPlace(places []TaskPlace, at TaskPlace) (TaskPlace, bool) {
	if len(places) == 0 {
		return TaskPlace{}, false
	}
	nearest := places[0]
	for _, place := range places {
		if place.Rank() <= at.Rank() {
			nearest = place
		}
	}
	return nearest, true
}

// valid says whether p is a place a task can stand at.
func (p TaskPlace) valid() bool {
	switch p.Kind {
	case PlaceStep:
		return p.Position >= 1
	case "", PlaceRetro, PlaceMerge:
		return p.Position == 0
	}
	return false
}

// ReturnCounts are the counts of returns of one task, which an issue with no plan rows keeps on its
// implementer's phases row (PhaseRow's Pass, Rounds, ConflictRounds, BackwardMoves and CapTold, each
// documented there). BackwardMoves is nil where no move back was counted.
type ReturnCounts struct {
	Pass, Rounds, ConflictRounds int
	BackwardMoves                map[string]int
	CapTold                      int
}

// PlanTask is one task of a plan. PlanVersion is the version that wrote its text; BlockText, Needs,
// PartOf, OwnerRole, Merges and StepsByDefault are what that version's block says. State,
// StoppedReason, Place and Counts are where the task stands, which the walk writes (MoveTask,
// StopTask, PutReturnCounts). Needs is nil where the block names none.
type PlanTask struct {
	Issue       string
	Generation  uint64
	Task        string
	PlanVersion int
	BlockText   string
	Needs       []string
	PartOf      string
	OwnerRole   string
	Merges      bool
	// StepsByDefault says that the block names no step, so the task's steps (PlanStep) are the
	// default steps it runs (plan.StepsOf), not steps its block names: a later version is compared
	// with the block as it was given, which names none (plan.ChangedTasks).
	StepsByDefault bool
	State          TaskState
	StoppedReason  string
	Place          TaskPlace
	Counts         ReturnCounts
}

// PlanStep is one step of a task, at its place from 1: the role that runs it, the type of its
// role's work (plan.Type), the agent key the plan names for it ("" for none) and the name of its
// agent (Agent, read through AgentName).
type PlanStep struct {
	Issue      string
	Generation uint64
	Task       string
	Position   int
	Role       string
	Type       plan.Type
	AgentKey   string
	// Agent is the name of the step's agent, the last part of its claim token, which NameAgents
	// gives before the step is written and nothing else writes. A step of the key owner holds none.
	// 20261124213820 named every other step a release before the column wrote, except where the
	// first form of that migration ran: a row it left holds "" and reads as no name, and the third
	// pass of NameAgents leaves such a step to its fourth.
	Agent claim.Agent
	// LeaveSession is the session the claim of the step's agent must not resume: every start of
	// the step's agent carries it (SuperviseRequest.LeaveSession), and the claim leaves it where it
	// still records it (supervise's revive). No writer sets it now: NameAgents gives no agent a name
	// a key held, so every step is written with "", and only a stored row that already holds a
	// session has its starts carry one.
	LeaveSession string
}

// PlanRole is one role a plan defines with a role block: its name (the block's id), the version of
// the plan that wrote it, its type, its model ("" for the default model) and its prompt, the
// block's body.
type PlanRole struct {
	Issue       string
	Generation  uint64
	Role        string
	PlanVersion int
	Type        string
	Model       string
	Prompt      string
}

// PlanAgent is the agent of the step (plan.AgentOf): the agent of its key, which every step of that
// key shares in any task, or for a step with no key the agent of that step alone.
func (s PlanStep) PlanAgent() plan.Agent {
	return plan.AgentOf(s.Task, s.Position, plan.Step{Role: s.Role, Key: s.AgentKey})
}

// AgentName is the name of the step's agent, the last part of its claim token, by the one rule
// every reader of a stored step names it by (plan.StoredAgentName), and the one reader of Agent:
// none for a step of the key owner, since its agent is the tree's architect, whose token names the
// tree and no name on the issue, and the stored name otherwise.
func (s PlanStep) AgentName() claim.Agent {
	return claim.Agent(plan.StoredAgentName(s.AgentKey, string(s.Agent)))
}

// StepToken is the claim token of the agent of step, a step of issue, in project (already a
// claim.ProjectToken): the token a launch's addressing names for the step. A step of the key owner
// is the owner's, the issue's own architect by rule (OwnerClaim), whose token names the issue and
// no name of an agent; every other step's is the token of its agent's name (AgentName), so the
// steps of one key are one claim.
func StepToken(project, issue string, step PlanStep) (claim.Token, error) {
	if step.PlanAgent().Owner() {
		return claim.NewToken(project, issue, claim.RoleArchitect)
	}
	return claim.AgentToken(project, issue, step.AgentName())
}

// StoredPlan is the plan of an issue at one generation: the version in force, every task ordered by
// its id, the steps of each task by its id, ordered by place, and the roles the plan defines,
// ordered by name. Steps and Roles are never nil.
type StoredPlan struct {
	Version PlanVersion
	Tasks   []PlanTask
	Steps   map[string][]PlanStep
	Roles   []PlanRole
}

// Task is the plan's one task, the task its run is on whatever that task's state: this release's
// plans hold one (plan.Supported). A plan that holds none, or more than one, is an error.
func (p *StoredPlan) Task() (PlanTask, error) {
	if len(p.Tasks) != 1 {
		return PlanTask{}, fmt.Errorf("the plan of %s at generation %d holds %d tasks, and this release walks one", p.Version.Issue, p.Version.Generation, len(p.Tasks))
	}
	return p.Tasks[0], nil
}

// PutPlan stores version v of the plan of v.Issue at v.Generation, with the tasks it gives and the
// steps of those tasks. The same version given twice is refused, and so is a task or a step of
// another issue or generation, or a step of a task v does not give, whose step the write would
// leave beside a task that keeps its own; each refusal is made before anything is written, so the
// transaction stays usable. A task the generation holds already takes v's text and attributes and
// keeps where it stands, which the walk writes; a new task is written as given. The steps of each
// task v gives are replaced by the steps v gives it, each with the name of its agent as it is given
// (NameAgents names them before this write; PutPlan has no rule of its own).
func (s *Postgres) PutPlan(ctx context.Context, tx pgx.Tx, v PlanVersion, tasks []PlanTask, steps []PlanStep) error {
	describe := fmt.Sprintf("put version %d of the plan of %s at generation %d", v.Version, v.Issue, v.Generation)
	given := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if task.Issue != v.Issue || task.Generation != v.Generation {
			return fmt.Errorf("%s: task %s is of %s at generation %d", describe, task.Task, task.Issue, task.Generation)
		}
		given = append(given, task.Task)
	}
	for _, step := range steps {
		if step.Issue != v.Issue || step.Generation != v.Generation {
			return fmt.Errorf("%s: step %d of task %s is of %s at generation %d", describe, step.Position, step.Task, step.Issue, step.Generation)
		}
		if !slices.Contains(given, step.Task) {
			return fmt.Errorf("%s: step %d names task %s, which the version does not give", describe, step.Position, step.Task)
		}
	}
	tag, err := tx.Exec(ctx, `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ($1, $2, $3, $4, $5, $6, $7) on conflict (issue, generation, version) do nothing`,
		v.Issue, int64(v.Generation), v.Version, v.Source, v.ArtifactID, string(v.GivenBy), v.GivenAt)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: that version is stored already", describe)
	}
	for _, task := range tasks {
		moves, err := encodeBackwardMoves(task.Counts.BackwardMoves)
		if err != nil {
			return fmt.Errorf("%s: task %s: %w", describe, task.Task, err)
		}
		needs := task.Needs
		if needs == nil {
			needs = []string{}
		}
		if _, err := tx.Exec(ctx, `insert into plan_tasks (issue, generation, task, plan_version, block_text, needs, part_of, owner_role, merges,
			steps_by_default, state, stopped_reason, running_kind, position, pass, rounds, conflict_rounds, backward_moves, cap_told)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
			on conflict (issue, generation, task) do update set plan_version = excluded.plan_version, block_text = excluded.block_text,
			needs = excluded.needs, part_of = excluded.part_of, owner_role = excluded.owner_role, merges = excluded.merges,
			steps_by_default = excluded.steps_by_default`,
			v.Issue, int64(v.Generation), task.Task, task.PlanVersion, task.BlockText, needs, task.PartOf, task.OwnerRole, task.Merges,
			task.StepsByDefault, task.State, task.StoppedReason, task.Place.Kind, task.Place.Position,
			task.Counts.Pass, task.Counts.Rounds, task.Counts.ConflictRounds, moves, task.Counts.CapTold); err != nil {
			return fmt.Errorf("%s: task %s: %w", describe, task.Task, err)
		}
	}
	if _, err := tx.Exec(ctx, `delete from plan_steps where issue = $1 and generation = $2 and task = any($3)`,
		v.Issue, int64(v.Generation), given); err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	for _, step := range steps {
		if _, err := tx.Exec(ctx, `insert into plan_steps (issue, generation, task, position, role, type, agent_key, agent, leave_session)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			v.Issue, int64(v.Generation), step.Task, step.Position, step.Role, step.Type, step.AgentKey, string(step.Agent), step.LeaveSession); err != nil {
			return fmt.Errorf("%s: step %d of task %s: %w", describe, step.Position, step.Task, err)
		}
		// The name a key's agent holds is kept in the lasting record too (KeyNames), with the
		// latest generation that gave it: this write deletes the task's earlier rows, and the first
		// step of a run deletes an earlier owner's plan, but the name's claim records the key's
		// session in every later generation.
		if name := step.AgentName(); step.AgentKey != "" && name != "" {
			if _, err := tx.Exec(ctx, `insert into plan_key_names (issue, name, agent_key, generation) values ($1, $2, $3, $4)
				on conflict (issue, name) do update set agent_key = excluded.agent_key,
				generation = greatest(plan_key_names.generation, excluded.generation)`, v.Issue, string(name), step.AgentKey, int64(v.Generation)); err != nil {
				return fmt.Errorf("%s: the name %s of key %s: %w", describe, name, step.AgentKey, err)
			}
		}
	}
	return nil
}

// PutPlanRoles writes roles, the roles version of the plan of issue at generation defines, over
// every role that generation holds, so a role an earlier version defined and this one does not is
// gone, and nil leaves the generation with none. A role of another issue, generation or version is
// refused before anything is written, so the transaction stays usable; a version the issue's plans
// do not hold is refused by the store (the foreign key on issue_plans).
func (s *Postgres) PutPlanRoles(ctx context.Context, tx pgx.Tx, issue string, generation uint64, version int, roles []PlanRole) error {
	describe := fmt.Sprintf("put the roles of version %d of the plan of %s at generation %d", version, issue, generation)
	for _, role := range roles {
		if role.Issue != issue || role.Generation != generation || role.PlanVersion != version {
			return fmt.Errorf("%s: role %s is of version %d of %s at generation %d", describe, role.Role, role.PlanVersion, role.Issue, role.Generation)
		}
	}
	if _, err := tx.Exec(ctx, `delete from plan_roles where issue = $1 and generation = $2`, issue, int64(generation)); err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	for _, role := range roles {
		if _, err := tx.Exec(ctx, `insert into plan_roles (issue, generation, role, plan_version, type, model, prompt)
			values ($1, $2, $3, $4, $5, $6, $7)`,
			issue, int64(generation), role.Role, version, role.Type, role.Model, role.Prompt); err != nil {
			return fmt.Errorf("%s: role %s: %w", describe, role.Role, err)
		}
	}
	return nil
}

// planRoleColumns are the columns of a plan_roles row as scanPlanRole reads them.
const planRoleColumns = `issue, generation, role, plan_version, type, model, prompt`

func scanPlanRole(row pgx.Row) (PlanRole, error) {
	var role PlanRole
	var generation int64
	if err := row.Scan(&role.Issue, &generation, &role.Role, &role.PlanVersion, &role.Type, &role.Model, &role.Prompt); err != nil {
		return PlanRole{}, err
	}
	role.Generation = uint64(generation)
	return role, nil
}

// PlanRoleOf is the role named role that the plan of issue defines at the issue's current
// generation, and false, with no error, where that plan defines none of that name or the daemon
// records no issue of that key: what a launch of a role of a plan reads.
func (s *Postgres) PlanRoleOf(ctx context.Context, tx pgx.Tx, issue, role string) (PlanRole, bool, error) {
	found, err := scanPlanRole(tx.QueryRow(ctx, `select plan_roles.issue, plan_roles.generation, plan_roles.role, plan_roles.plan_version,
		plan_roles.type, plan_roles.model, plan_roles.prompt from plan_roles
		join issues on issues.key = plan_roles.issue and issues.generation = plan_roles.generation
		where plan_roles.issue = $1 and plan_roles.role = $2`, issue, role))
	if errors.Is(err, pgx.ErrNoRows) {
		return PlanRole{}, false, nil
	}
	if err != nil {
		return PlanRole{}, false, fmt.Errorf("read the role %s of the plan of %s: %w", role, issue, err)
	}
	return found, true, nil
}

const planVersionColumns = `issue, generation, version, source, artifact_id, given_by, given_at`

// Plan is the plan of issue at generation, or nil where that generation has no plan rows.
func (s *Postgres) Plan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (*StoredPlan, error) {
	describe := fmt.Sprintf("read the plan of %s at generation %d", issue, generation)
	version, err := scanPlanVersion(tx.QueryRow(ctx, "select "+planVersionColumns+` from issue_plans
		where issue = $1 and generation = $2 order by version desc limit 1`, issue, int64(generation)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	plan := &StoredPlan{Version: version, Steps: map[string][]PlanStep{}}
	rows, err := tx.Query(ctx, `select `+planTaskColumns+` from plan_tasks
		where plan_tasks.issue = $1 and plan_tasks.generation = $2 order by plan_tasks.task`, issue, int64(generation))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	if plan.Tasks, err = scanPlanTasks(rows); err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	steps, err := tx.Query(ctx, `select task, position, role, type, agent_key, agent, leave_session
		from plan_steps where issue = $1 and generation = $2 order by task, position`, issue, int64(generation))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	defer steps.Close()
	for steps.Next() {
		step := PlanStep{Issue: issue, Generation: generation}
		if err := steps.Scan(&step.Task, &step.Position, &step.Role, &step.Type, &step.AgentKey, &step.Agent, &step.LeaveSession); err != nil {
			return nil, fmt.Errorf("%s: %w", describe, err)
		}
		plan.Steps[step.Task] = append(plan.Steps[step.Task], step)
	}
	if err := steps.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	roles, err := tx.Query(ctx, `select `+planRoleColumns+` from plan_roles
		where issue = $1 and generation = $2 order by role`, issue, int64(generation))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	if plan.Roles, err = pgx.CollectRows(roles, func(row pgx.CollectableRow) (PlanRole, error) { return scanPlanRole(row) }); err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return plan, nil
}

// PlanStepsOf is every plan_steps row of issue, of every generation, each with the name of its agent,
// ordered by generation, task and place: what NameAgents reads as earlier before a version's steps
// are written (PutPlan deletes the steps of each task it gives), so a key keeps the name an earlier
// version or generation gave it.
func (s *Postgres) PlanStepsOf(ctx context.Context, tx pgx.Tx, issue string) ([]PlanStep, error) {
	describe := fmt.Sprintf("read every step of the plans of %s", issue)
	rows, err := tx.Query(ctx, `select generation, task, position, role, type, agent_key, agent, leave_session
		from plan_steps where issue = $1 order by generation, task, position`, issue)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	defer rows.Close()
	var steps []PlanStep
	for rows.Next() {
		step := PlanStep{Issue: issue}
		var generation int64
		if err := rows.Scan(&generation, &step.Task, &step.Position, &step.Role, &step.Type, &step.AgentKey, &step.Agent, &step.LeaveSession); err != nil {
			return nil, fmt.Errorf("%s: %w", describe, err)
		}
		step.Generation = uint64(generation)
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return steps, nil
}

// KeyNames is every name a key's agent held on issue, by name (plan_key_names): the lasting record
// PutPlan writes for each step of a key, which no deletion of a plan removes. No code removes a
// row: the table names no issues row (a plan can be given before its issue is recorded), and a
// stored claim keeps the
// session it records across the issue's generations (supervise's launch resumes it), so a name a
// key held names that key's session for as long as the issue does. NameAgents reserves each name it
// answers for its key, beside the names the issue's plan_steps rows hold.
func (s *Postgres) KeyNames(ctx context.Context, tx pgx.Tx, issue string) ([]KeyName, error) {
	describe := fmt.Sprintf("read the names the keys of %s held", issue)
	rows, err := tx.Query(ctx, `select name, agent_key, generation from plan_key_names where issue = $1 order by name`, issue)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (KeyName, error) {
		var h KeyName
		var generation int64
		err := row.Scan(&h.Name, &h.AgentKey, &generation)
		h.Generation = uint64(generation)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return held, nil
}

// PlanTasksOf is every task of the plan of each issue of the Dispatch project at the issue's
// current generation, ordered by issue and then task, in one read: the tasks a generation holds
// are the tasks of its plan in force (PutPlan, DeleteTasks). An issue with no plan rows at its
// generation has none, whether it ran before plans or its rows are of an earlier generation, a run
// that is over. As Assignments does, the read is joined to the issue records of project, since the
// database is shared by every project's daemon. The state's projection reads it once for every
// issue it serves.
func (s *Postgres) PlanTasksOf(ctx context.Context, tx pgx.Tx, project string) ([]PlanTask, error) {
	rows, err := tx.Query(ctx, `select `+planTaskColumns+` from plan_tasks
		join issues on issues.key = plan_tasks.issue and issues.generation = plan_tasks.generation
		where issues.project = $1
		order by plan_tasks.issue, plan_tasks.task`, project)
	if err != nil {
		return nil, fmt.Errorf("read the plan tasks of every issue of %s: %w", project, err)
	}
	tasks, err := scanPlanTasks(rows)
	if err != nil {
		return nil, fmt.Errorf("read the plan tasks of every issue of %s: %w", project, err)
	}
	return tasks, nil
}

// planTaskColumns is the columns of a plan_tasks row, named by their table so a read joined to
// another table names no column twice, in the order scanPlanTasks reads them.
const planTaskColumns = `plan_tasks.issue, plan_tasks.generation, plan_tasks.task, plan_tasks.plan_version,
	plan_tasks.block_text, plan_tasks.needs, plan_tasks.part_of, plan_tasks.owner_role, plan_tasks.merges, plan_tasks.steps_by_default,
	plan_tasks.state, plan_tasks.stopped_reason, plan_tasks.running_kind, plan_tasks.position,
	plan_tasks.pass, plan_tasks.rounds, plan_tasks.conflict_rounds, plan_tasks.backward_moves, plan_tasks.cap_told`

// scanPlanTasks reads every row of a query of planTaskColumns and closes rows.
func scanPlanTasks(rows pgx.Rows) ([]PlanTask, error) {
	defer rows.Close()
	var tasks []PlanTask
	for rows.Next() {
		var task PlanTask
		var generation int64
		var moves []byte
		if err := rows.Scan(&task.Issue, &generation, &task.Task, &task.PlanVersion, &task.BlockText, &task.Needs, &task.PartOf,
			&task.OwnerRole, &task.Merges, &task.StepsByDefault, &task.State, &task.StoppedReason, &task.Place.Kind, &task.Place.Position,
			&task.Counts.Pass, &task.Counts.Rounds, &task.Counts.ConflictRounds, &moves, &task.Counts.CapTold); err != nil {
			return nil, err
		}
		task.Generation = uint64(generation)
		if len(task.Needs) == 0 {
			task.Needs = nil
		}
		var err error
		if task.Counts.BackwardMoves, err = decodeBackwardMoves(moves); err != nil {
			return nil, fmt.Errorf("task %s of %s: %w", task.Task, task.Issue, err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

// inForce selects, of issue $1, each owner's version that is the plan in force of its generation:
// the highest version there.
const inForce = `source = 'owner' and issue = $1
	and version = (select max(version) from issue_plans newest where newest.issue = issue_plans.issue and newest.generation = issue_plans.generation)`

// OwnersNewestPlan is the newest plan in force that an owner of issue gave, of any generation,
// generation 0 among them, which holds a plan given before the daemon recorded the issue: the
// latest given whose giver is an architect of the issue's tree now (ArchitectOfTree), and of two
// given at one moment the later generation's. nil where no owner gave a plan that is in force; a
// default plan is nobody's. The versions are read, and their rows closed, before each giver is
// judged, since a transaction runs one statement at a time.
func (s *Postgres) OwnersNewestPlan(ctx context.Context, tx pgx.Tx, issue Issue) (*StoredPlan, error) {
	describe := fmt.Sprintf("read the newest plan an owner gave for %s", issue.Key)
	rows, err := tx.Query(ctx, "select "+planVersionColumns+` from issue_plans where `+inForce+`
		order by given_at desc, generation desc`, issue.Key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	given, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PlanVersion, error) { return scanPlanVersion(row) })
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	for _, version := range given {
		owner, err := ArchitectOfTree(ctx, s, tx, version.GivenBy, issue)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", describe, err)
		}
		if owner {
			return s.Plan(ctx, tx, issue.Key, version.Generation)
		}
	}
	return nil, nil
}

// PlansOfEarlierOwners is the version in force of each generation of issue whose giver is no
// architect of the issue's tree now (ArchitectOfTree), in the order they were given: the plans an
// earlier owner gave, which the issue's first step passes over. The architect of the tree's root
// and each sub-architect of the tree are owners, so none of their plans is among them, and a
// default plan is nobody's, so none is either.
func (s *Postgres) PlansOfEarlierOwners(ctx context.Context, tx pgx.Tx, issue Issue) ([]PlanVersion, error) {
	return s.plansInForce(ctx, tx, issue, false, fmt.Sprintf("read the plans of %s that earlier owners gave", issue.Key))
}

// PlansOfOwners is the version in force of each generation of issue whose giver is an architect of
// the issue's tree now (ArchitectOfTree), in the order they were given, generation 0 among them,
// which holds a plan given before the daemon recorded the issue: every plan an owner gave that a
// first step could take, each copy a carry wrote to a later generation (CarryPlan) among them. A
// default plan is nobody's, so none is among them.
func (s *Postgres) PlansOfOwners(ctx context.Context, tx pgx.Tx, issue Issue) ([]PlanVersion, error) {
	return s.plansInForce(ctx, tx, issue, true, fmt.Sprintf("read the plans of %s that its owners gave", issue.Key))
}

// plansInForce is the owner's version in force of each generation of issue, in the order they were
// given, whose giver is an architect of the issue's tree now where owners is true, and is none where
// it is false. The versions are read, and their rows closed, before each giver is judged, since a
// transaction runs one statement at a time.
func (s *Postgres) plansInForce(ctx context.Context, tx pgx.Tx, issue Issue, owners bool, describe string) ([]PlanVersion, error) {
	rows, err := tx.Query(ctx, "select "+planVersionColumns+` from issue_plans where `+inForce+`
		order by given_at, generation`, issue.Key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	given, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PlanVersion, error) { return scanPlanVersion(row) })
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	var plans []PlanVersion
	for _, version := range given {
		owner, err := ArchitectOfTree(ctx, s, tx, version.GivenBy, issue)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", describe, err)
		}
		if owner == owners {
			plans = append(plans, version)
		}
	}
	return plans, nil
}

// PlanInForce is the version in force of one generation of an issue's plan (PlanVersion, the
// highest version there, whatever its source) and the role of the first step of its task, "" where
// the task names none: what the phase the issue's run starts in is judged from before its first
// step settles the plan (ReadFirstPhases).
type PlanInForce struct {
	Version   PlanVersion
	FirstRole string
}

// PlansInForce is the version in force of each generation of each issue of keys, with the role of
// its task's first step, in one read: each issue's in the order OwnersNewestPlan judges them, the
// latest given first and, of two given at one moment, the later generation's. A generation with no
// plan rows is none of them. A plan of this release holds one task (StoredPlan.Task), so the first
// step of the first task by id is the first step of the task its run walks.
func (s *Postgres) PlansInForce(ctx context.Context, tx pgx.Tx, keys []string) ([]PlanInForce, error) {
	describe := fmt.Sprintf("read the plans in force of %d issues", len(keys))
	rows, err := tx.Query(ctx, `select v.issue, v.generation, v.version, v.source, v.artifact_id, v.given_by, v.given_at,
		coalesce((select s.role from plan_steps s where s.issue = v.issue and s.generation = v.generation
			order by s.task, s.position limit 1), '')
		from issue_plans v
		where v.issue = any($1) and v.version = (select max(version) from issue_plans newest
			where newest.issue = v.issue and newest.generation = v.generation)
		order by v.issue, v.given_at desc, v.generation desc`, keys)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	plans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PlanInForce, error) {
		var inForce PlanInForce
		version, err := scanPlanVersion(scanThen{row: row, after: []any{&inForce.FirstRole}})
		inForce.Version = version
		return inForce, err
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	return plans, nil
}

// ReadFirstPhases is the phase the run of each issue of issues starts in, by key, for each issue
// whose promotion moves it into the first step of its plan: one standing at admitted in the
// workflow under its tree's open design gate (gates, by root key, as LineFacts.Gates holds them),
// whose first phase the waiting line offers (LineFirstPhase) or whose row it gives back first, in
// the same pass (LineWaitingOnIssue). Every other issue has no entry: a root at admitted under a
// gate that is not open is its architect writing the design, and its promotion leaves it there.
//
// The phase is the one the first step of the plan that step walks runs as, the first of its line
// (plan.Line), read before the step settles the plan, from the sources the step settles it from
// and in their order (workflow's planFirstStep): the plan in force of the issue's own generation,
// unless an earlier owner gave it, which the step passes over (PlansOfEarlierOwners); else the
// newest plan in force an owner of the issue gave, of any generation (OwnersNewestPlan); else
// defaults, the project's default steps, which the caller passes because this package holds no
// project's settings. An owner is an architect of the issue's tree now (ArchitectOfTree).
//
// It is one read for every issue (Store.PlansInForce), and none where no issue stands so: the line
// is judged on every fact, and three reads for each child waiting for a slot would cost a round
// trip apiece on each of them. The giver of a plan is judged as the first step judges it, from the
// issue alone for the tree root's architect and from a read of the tree for a sub-architect.
func ReadFirstPhases(ctx context.Context, tx pgx.Tx, store Store, issues []Issue, gates map[string]TreeGate, defaults []plan.Step) (map[string]phase.Phase, error) {
	starting := make(map[string]Issue)
	for _, issue := range issues {
		if issue.Phase == phase.Admitted && InWorkflow(issue) && gates[issue.Tree].Open() {
			starting[issue.Key] = issue
		}
	}
	if len(starting) == 0 {
		return nil, nil
	}
	plans, err := store.PlansInForce(ctx, tx, slices.Sorted(maps.Keys(starting)))
	if err != nil {
		return nil, err
	}
	own, owners := make(map[string]PlanInForce), make(map[string]PlanInForce)
	for _, inForce := range plans {
		issue := starting[inForce.Version.Issue]
		if inForce.Version.Source == PlanSourceOwner {
			owner, err := ArchitectOfTree(ctx, store, tx, inForce.Version.GivenBy, issue)
			if err != nil {
				return nil, fmt.Errorf("judge the giver of the plan of %s at generation %d: %w", issue.Key, inForce.Version.Generation, err)
			}
			if !owner {
				continue
			}
			if _, seen := owners[issue.Key]; !seen {
				owners[issue.Key] = inForce
			}
		}
		if inForce.Version.Generation == issue.Generation {
			own[issue.Key] = inForce
		}
	}
	byDefault := phase.Retro
	if len(defaults) > 0 {
		byDefault = firstOfLine(defaults[0].Role)
	}
	phases := make(map[string]phase.Phase, len(starting))
	for key := range starting {
		taken, ok := own[key]
		if !ok {
			taken, ok = owners[key]
		}
		phases[key] = byDefault
		if ok {
			phases[key] = firstOfLine(taken.FirstRole)
		}
	}
	return phases, nil
}

// firstOfLine is the first phase of the line of a task whose first step names role (plan.Line):
// the phase that step runs as (plan.RunsAs), or the retro of a task that names no step, role "".
func firstOfLine(role string) phase.Phase {
	if role == "" {
		return phase.Retro
	}
	return plan.RunsAs(role)
}

// CarryPlan copies the plan of issue at generation from to generation to: every version, every
// task, every step and every role it defines. carried is a hand-over that carries its run, whose
// tasks go on where they stood, so each task keeps its state, its place and its counts. Without
// carried, the plan starts from its start: each task waits at no act with every count 0. Neither
// copies a count of starts, which step_starts keeps by place alone. Where from has no plan rows it
// writes nothing, so an issue that ran before plans goes on as it did; a generation to that holds
// a plan already is refused.
func (s *Postgres) CarryPlan(ctx context.Context, tx pgx.Tx, issue string, from, to uint64, carried bool) error {
	describe := fmt.Sprintf("carry the plan of %s from generation %d to %d", issue, from, to)
	if from == to {
		return fmt.Errorf("%s: a generation is not carried onto itself", describe)
	}
	var held, taken bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from issue_plans where issue = $1 and generation = $2),
		exists (select 1 from issue_plans where issue = $1 and generation = $3)`, issue, int64(from), int64(to)).Scan(&held, &taken); err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if !held {
		return nil
	}
	if taken {
		return fmt.Errorf("%s: generation %d holds a plan already", describe, to)
	}
	tasks := `insert into plan_tasks (issue, generation, task, plan_version, block_text, needs, part_of, owner_role, merges, steps_by_default, state)
		select issue, $3, task, plan_version, block_text, needs, part_of, owner_role, merges, steps_by_default, 'waiting'
		from plan_tasks where issue = $1 and generation = $2`
	if carried {
		tasks = `insert into plan_tasks (issue, generation, task, plan_version, block_text, needs, part_of, owner_role, merges, steps_by_default,
			state, stopped_reason, running_kind, position, pass, rounds, conflict_rounds, backward_moves, cap_told)
			select issue, $3, task, plan_version, block_text, needs, part_of, owner_role, merges, steps_by_default,
			state, stopped_reason, running_kind, position, pass, rounds, conflict_rounds, backward_moves, cap_told
			from plan_tasks where issue = $1 and generation = $2`
	}
	for _, statement := range []string{
		`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
			select issue, $3, version, source, artifact_id, given_by, given_at from issue_plans where issue = $1 and generation = $2`,
		tasks,
		// Every column of plan_steps but the generation, leave_session among them: a default step
		// whose agent took a key's name carries the key's session to leave into the generation it is
		// carried to, or that agent's first start there resumes the key's session
		// (TestACarriedPlanHoldsEveryColumnOfItsSteps fails on a column the copy leaves out).
		`insert into plan_steps (issue, generation, task, position, role, type, agent_key, agent, leave_session)
			select issue, $3, task, position, role, type, agent_key, agent, leave_session
			from plan_steps where issue = $1 and generation = $2`,
		`insert into plan_roles (issue, generation, role, plan_version, type, model, prompt)
			select issue, $3, role, plan_version, type, model, prompt
			from plan_roles where issue = $1 and generation = $2`,
	} {
		if _, err := tx.Exec(ctx, statement, issue, int64(from), int64(to)); err != nil {
			return fmt.Errorf("%s: %w", describe, err)
		}
	}
	return nil
}

// DeletePlan deletes the plan of issue at generation, every row of it in each of the four tables
// (the tasks, the steps and the roles go with their versions, on delete cascade).
func (s *Postgres) DeletePlan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) error {
	if _, err := tx.Exec(ctx, `delete from issue_plans where issue = $1 and generation = $2`, issue, int64(generation)); err != nil {
		return fmt.Errorf("delete the plan of %s at generation %d: %w", issue, generation, err)
	}
	return nil
}

// DeleteTasks deletes each of tasks from the plan of issue at generation, with its steps: what a
// later version that removes a task writes. Only a waiting or a stopped task is removed, since a
// later version changes nothing that runs or ran; a name that is neither, or that the plan does not
// hold, is refused, naming it.
func (s *Postgres) DeleteTasks(ctx context.Context, tx pgx.Tx, issue string, generation uint64, tasks []string) error {
	describe := fmt.Sprintf("delete tasks %v of the plan of %s at generation %d", tasks, issue, generation)
	rows, err := tx.Query(ctx, `delete from plan_tasks where issue = $1 and generation = $2 and task = any($3)
		and state in ('waiting', 'stopped') returning task`, issue, int64(generation), tasks)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	deleted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	var kept []string
	for _, task := range tasks {
		if !slices.Contains(deleted, task) && !slices.Contains(kept, task) {
			kept = append(kept, task)
		}
	}
	if len(kept) > 0 {
		return fmt.Errorf("%s: the plan holds no waiting or stopped task %v", describe, kept)
	}
	return nil
}

// MoveTask writes where task stands in the plan of issue at generation, and its state: the one
// write of the walk's moveTask. A stop is StopTask's, which records why, so the state stopped is
// refused here, and a move takes a stop's reason away, but a move to left (TaskLeft): a stopped
// task whose issue leaves the workflow keeps why it was stopped, the one record of why it was not
// done. A place no task can stand at, a step the task does not have, a task the plan does not
// hold and a task that left the workflow, which nothing moves again, are refused.
func (s *Postgres) MoveTask(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, place TaskPlace, state TaskState) error {
	describe := fmt.Sprintf("move task %s of %s at generation %d to %s %d, %s", task, issue, generation, place.Kind, place.Position, state)
	switch state {
	case TaskWaiting, TaskRunning, TaskDone, TaskLeft:
	case TaskStopped:
		return fmt.Errorf("%s: a stop is StopTask's, which records why", describe)
	default:
		return fmt.Errorf("%s: no task has that state", describe)
	}
	if !place.valid() {
		return fmt.Errorf("%s: no task stands at that place", describe)
	}
	tag, err := tx.Exec(ctx, `update plan_tasks set running_kind = $4, position = $5, state = $6,
		stopped_reason = case when $6 = 'left' then stopped_reason else '' end
		where issue = $1 and generation = $2 and task = $3 and state <> 'left'
		and ($4 <> 'step' or exists (select 1 from plan_steps where plan_steps.issue = $1 and plan_steps.generation = $2
			and plan_steps.task = $3 and plan_steps.position = $5))`,
		issue, int64(generation), task, place.Kind, place.Position, state)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: the plan holds no such task that is in the workflow, or the task no such step", describe)
	}
	return nil
}

// StopTask writes task of the plan of issue at generation stopped for reason, at the place it
// stands, which a later start of the task reads. A stop without its reason, of a task that is done
// or left the workflow, or of a task the plan does not hold, is refused.
func (s *Postgres) StopTask(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task, reason string) error {
	describe := fmt.Sprintf("stop task %s of %s at generation %d", task, issue, generation)
	if reason == "" {
		return fmt.Errorf("%s: a stop records why", describe)
	}
	tag, err := tx.Exec(ctx, `update plan_tasks set state = 'stopped', stopped_reason = $4
		where issue = $1 and generation = $2 and task = $3 and state not in ('done', 'left')`, issue, int64(generation), task, reason)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: the plan holds no such task that is not done and in the workflow", describe)
	}
	return nil
}

// hasPlan says whether issue has plan rows at generation.
func (s *Postgres) hasPlan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (bool, error) {
	var has bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from issue_plans where issue = $1 and generation = $2)`,
		issue, int64(generation)).Scan(&has); err != nil {
		return false, fmt.Errorf("read whether %s has a plan at generation %d: %w", issue, generation, err)
	}
	return has, nil
}

// ReturnCounts is the counts of returns of task: on the task, for an issue with plan rows at
// generation, where a task the plan does not hold is refused; for an issue with none, on its
// implementer's phases row (ImplementerCounts).
func (s *Postgres) ReturnCounts(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string) (ReturnCounts, error) {
	describe := fmt.Sprintf("read the counts of returns of task %s of %s at generation %d", task, issue, generation)
	has, err := s.hasPlan(ctx, tx, issue, generation)
	if err != nil {
		return ReturnCounts{}, err
	}
	if !has {
		rows, err := s.Phases(ctx, tx, issue)
		if err != nil {
			return ReturnCounts{}, fmt.Errorf("%s: %w", describe, err)
		}
		return ImplementerCounts(rows), nil
	}
	var counts ReturnCounts
	var moves []byte
	err = tx.QueryRow(ctx, `select pass, rounds, conflict_rounds, backward_moves, cap_told from plan_tasks
		where issue = $1 and generation = $2 and task = $3`, issue, int64(generation), task).
		Scan(&counts.Pass, &counts.Rounds, &counts.ConflictRounds, &moves, &counts.CapTold)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReturnCounts{}, fmt.Errorf("%s: the plan holds no such task", describe)
	}
	if err != nil {
		return ReturnCounts{}, fmt.Errorf("%s: %w", describe, err)
	}
	if counts.BackwardMoves, err = decodeBackwardMoves(moves); err != nil {
		return ReturnCounts{}, fmt.Errorf("%s: %w", describe, err)
	}
	return counts, nil
}

// ImplementerCounts is the counts of returns of an issue with no plan rows, on its implementer's
// phases row among rows, where they lived before plans; the zero counts where rows hold no such
// row. An issue with plan rows keeps them on its task instead (PlanTask.Counts).
func ImplementerCounts(rows []PhaseRow) ReturnCounts {
	row := PhaseRowOf(rows, claim.RoleImplementer)
	return ReturnCounts{Pass: row.Pass, Rounds: row.Rounds, ConflictRounds: row.ConflictRounds, BackwardMoves: row.BackwardMoves, CapTold: row.CapTold}
}

// PutReturnCounts writes the counts of returns of task where ReturnCounts reads them, and nothing
// else: the task's other columns, or every other column of the implementer's phases row, which it
// reads and writes back whole (a row the record does not hold yet is written for the implementer
// with the counts alone).
func (s *Postgres) PutReturnCounts(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, c ReturnCounts) error {
	describe := fmt.Sprintf("put the counts of returns of task %s of %s at generation %d", task, issue, generation)
	has, err := s.hasPlan(ctx, tx, issue, generation)
	if err != nil {
		return err
	}
	if !has {
		rows, err := s.Phases(ctx, tx, issue)
		if err != nil {
			return fmt.Errorf("%s: %w", describe, err)
		}
		row := PhaseRowOf(rows, claim.RoleImplementer)
		row.Issue, row.Role = issue, claim.RoleImplementer
		row.Pass, row.Rounds, row.ConflictRounds, row.BackwardMoves, row.CapTold = c.Pass, c.Rounds, c.ConflictRounds, c.BackwardMoves, c.CapTold
		return s.PutPhase(ctx, tx, row)
	}
	moves, err := encodeBackwardMoves(c.BackwardMoves)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	tag, err := tx.Exec(ctx, `update plan_tasks set pass = $4, rounds = $5, conflict_rounds = $6, backward_moves = $7, cap_told = $8
		where issue = $1 and generation = $2 and task = $3`,
		issue, int64(generation), task, c.Pass, c.Rounds, c.ConflictRounds, moves, c.CapTold)
	if err != nil {
		return fmt.Errorf("%s: %w", describe, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: the plan holds no such task", describe)
	}
	return nil
}

// scanPlanVersion reads one issue_plans row of planVersionColumns.
func scanPlanVersion(row scanner) (PlanVersion, error) {
	var version PlanVersion
	var generation int64
	var givenBy string
	if err := row.Scan(&version.Issue, &generation, &version.Version, &version.Source, &version.ArtifactID, &givenBy, &version.GivenAt); err != nil {
		return PlanVersion{}, err
	}
	version.Generation, version.GivenBy, version.GivenAt = uint64(generation), claim.Token(givenBy), version.GivenAt.UTC()
	return version, nil
}

// encodeBackwardMoves is the jsonb a column of backward move counts holds: the empty object for no
// move counted, as PutPhase writes it.
func encodeBackwardMoves(moves map[string]int) ([]byte, error) {
	if len(moves) == 0 {
		return []byte("{}"), nil
	}
	encoded, err := json.Marshal(moves)
	if err != nil {
		return nil, fmt.Errorf("encode the backward move counts: %w", err)
	}
	return encoded, nil
}

// decodeBackwardMoves reads such a column back: nil for the empty object, as scanPhase reads it.
func decodeBackwardMoves(raw []byte) (map[string]int, error) {
	if bytes.Equal(raw, []byte("{}")) {
		return nil, nil
	}
	moves := map[string]int{}
	if err := json.Unmarshal(raw, &moves); err != nil {
		return nil, fmt.Errorf("decode the backward move counts: %w", err)
	}
	if len(moves) == 0 {
		return nil, nil
	}
	return moves, nil
}
