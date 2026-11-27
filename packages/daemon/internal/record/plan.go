package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// The plan of an issue is three tables (store migration 20261117034837_issue_plans): the versions
// given for it (issue_plans), the tasks of the plan (plan_tasks), each with where it stands and its
// counts of returns, and the steps of each task (plan_steps). Every row is keyed by the issue and
// its generation, and no row names an issues row, because an owner can give a plan before the
// daemon records its issue: such a plan is written at generation 0. An issue that ran before plans
// has no rows, and goes on as it did then. The count of starts of a step is not the plan's: it lives
// in step_starts alone, at the place TaskPlace.StepPlace names. Beside them, plan_branches holds
// each branch name a generation of the issue gave (store migration 20261121165047_task_copies):
// AssignTaskBranch writes it at each name it gives and decides by every generation's rows,
// RecordBranchGiven writes the branch of each pull request of an earlier run whose row a run
// started again deletes, a carried CarryPlan copies it, BranchGiven reads one name of one
// generation, and neither DeleteTasks nor DeletePlan deletes it. A task's hold and the wait of the
// step at its place are columns of its row (store migration 20261117224125_task_held).

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
// stopped, and nothing starts it again, since a todo opens a new run with rows of its own. A held
// task stands where it stood, for the reason its hold reads (PlanTask.HoldReason), until its
// owner's retry releases it at the same place (HoldTask, ReleaseTask).
type TaskState string

// The states of a task, each the word plan_tasks.state stores.
const (
	TaskWaiting TaskState = "waiting"
	TaskRunning TaskState = "running"
	TaskHeld    TaskState = "held"
	TaskDone    TaskState = "done"
	TaskStopped TaskState = "stopped"
	TaskLeft    TaskState = "left"
)

// StepWait is what the step at a running task's place waits for before its start is delivered
// (PlanTask.StepWait): nothing, the end of its task's join, or its agent, which runs a step of
// another task. A step that waits is no running step: nothing of it has started.
type StepWait string

const (
	// StepWaitNone is a step that waits for nothing.
	StepWaitNone StepWait = ""
	// StepWaitJoin is a step that waits for the end of its task's join: the task_branch row
	// (TaskBranch) that makes the task's branch on GitHub at the joined work of its needs.
	StepWaitJoin StepWait = "join"
	// StepWaitAgent is a step that waits for its agent, which runs a step of another task.
	StepWaitAgent StepWait = "agent"
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
// it gave; a default plan has version 0, no document and no giver. GivenBy is the claim token of
// the giver the store added last, and GivenAt the time of that giver's first give of the version.
// A give by an owner the version names already, as GivenBy or among EarlierGivers, changes nothing
// (AddPlanGiver), so an earlier giver that gives the version again stays an earlier giver: GivenBy
// is not always the owner that gave the version last, nor GivenAt the time of its last give.
// EarlierGivers is each other owner the store added as a giver of the same version at the same
// generation before GivenBy (store migration 20261124033010_plan_earlier_givers), nil for none: a
// version's givers are a set, and an owner's give of a version in force that names another giver
// adds that owner (AddPlanGiver). A version is its owners' while one of its givers is an architect
// of the issue's tree (GivenByOwner).
//
// Added is the ids of the tasks an owner's version added over the version in force when it was
// taken, in the plan's order (workflow's addedTasks): what register_plan answered then, which
// PutPlan records. Every read of a version gives it back (planVersionColumns), nil where the
// version added none and where it was taken before its write recorded what it added (store
// migration 20261123200630_plan_added), so a give of a version the store holds is answered what
// its first give was answered (workflow's planGiven). A default plan records none.
type PlanVersion struct {
	Issue         string
	Generation    uint64
	Version       int
	ArtifactID    string
	Source        string
	GivenBy       claim.Token
	GivenAt       time.Time
	EarlierGivers []claim.Token
	Added         []string
}

// Givers is every owner that gave v, the giver the store added last (GivenBy) first, and none for a
// default plan.
func (v PlanVersion) Givers() []claim.Token {
	if v.GivenBy == "" {
		return v.EarlierGivers
	}
	return slices.Concat([]claim.Token{v.GivenBy}, v.EarlierGivers)
}

// GivenByOwner says whether a giver of version (PlanVersion.Givers) is an architect of issue's tree
// now, as store reads the tree (ArchitectOfTree): the one rule by which a version is its owners'.
// A default plan has no giver and is nobody's. Each reader of an owner's plans judges by it through
// the store its caller hands (GivenByEarlierOwners, OwnersNewestPlan, PlansOfEarlierOwners,
// PlansOfOwners), so the workflow, which hands its own store, judges a give of a plan and the first
// step of a run by the one tree the routes read too.
func GivenByOwner(ctx context.Context, store Store, tx pgx.Tx, version PlanVersion, issue Issue) (bool, error) {
	for _, giver := range version.Givers() {
		owner, err := ArchitectOfTree(ctx, store, tx, giver, issue)
		if err != nil || owner {
			return owner, err
		}
	}
	return false, nil
}

// GivenByEarlierOwners says whether version, the plan in force of a generation of issue, is one
// that only earlier owners gave: an owner's version (PlanSourceOwner) of which no giver is an
// architect of issue's tree now (GivenByOwner). The owner of an issue changes where an orphan is
// admitted again as a root of its own and where an adoption moves a member to another tree, and a
// version that only earlier owners gave is not the new owner's to run: the issue's first step
// passes it over (PlansOfEarlierOwners, workflow's passOverEarlierOwners), so a give of a plan
// before that step is judged as though the version were gone (workflow's versionRefusal), and a
// plan given at generation 0 still waits there while the issue's own generation holds only such a
// version (workflow's waitsAtZero).
func GivenByEarlierOwners(ctx context.Context, store Store, tx pgx.Tx, version PlanVersion, issue Issue) (bool, error) {
	if version.Source != PlanSourceOwner {
		return false, nil
	}
	owners, err := GivenByOwner(ctx, store, tx, version, issue)
	return !owners && err == nil, err
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

// Rank orders the places of one task's line: no place first, then each step by its place, then
// the retro, then the merge. A move back goes to a place of a lower rank, and a step that ran
// stands at a lower rank than the place the task stands at, whatever phase each runs as: every
// step of a role the deployment defines runs as step, so only the place tells two of them apart.
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
// StoppedReason, HoldReason, Place and Counts are where the task stands, which the walk writes
// (MoveTask, StopTask, HoldTask, ReleaseTask, PutReturnCounts): HoldReason is why a held task is
// held, "" for one whose agent's claim failed and for every task that is not held. StepWait and
// StepWaitSince are what the step at a running task's place waits for and since when, nil where it
// waits for nothing (SetStepWait), and StepWaitFrom is the phase the start that waits moved the
// issue from, the phase the entry it raises names (PhaseRow.EntryFrom), "" for none: the start
// writes it when its step waits (SetStepWaitFrom), the start after the wait reads it (the
// workflow's startAfterWait), and every move and stop clears it. Branch is the GitHub branch the
// task works on, "" until its first start gives it one (AssignTaskBranch), and JoinConflicts is
// the id of each need whose merge GitHub refused as a conflict, into the branch the join of its
// needs is made on: its join branch where it has one (StoredPlan.BaseOf), else its branch
// (SetJoinConflicts). JoinUnmerged says GitHub refused as a conflict the merge of its join branch
// into its branch, which the task's own work there makes at a start again (SetJoinUnmerged).
// StartReason is why the step the task stands at was started: a start again at its first step, or
// a move back to an earlier step (SetStartReason), "" for a first start and after any later move:
// the step that starts after a wait reads it (the workflow's startAfterWait). StartReport is the
// report of the step a move forward ended (the workflow's handoffReport), which the start of the
// next step tells before the pull request (SetStartReport), "" for a move back, whose report is in
// its reason, for every other start and after any later move: the step that starts after a wait
// for its agent is told it in the same place. StartTask is the task the return out of
// awaiting_children hands the start of the step it takes the task to (SetStartTask), "" for every
// other start and after any later move: the step that starts after a wait for its agent is told it
// as written. Needs and JoinConflicts are nil where they hold none.
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
	Branch         string
	JoinConflicts  []string
	JoinUnmerged   bool
	HoldReason     HoldReason
	StepWait       StepWait
	StepWaitSince  *time.Time
	StepWaitFrom   phase.Phase
	StartReason    string
	StartReport    string
	StartTask      string
}

// BranchOf is the GitHub branch task works on, for the task of the issue keyed issueKey: the branch
// its row holds, or the issue's own branch (claim.IssueBranch) where it holds none, which is the
// branch of every task of a release before tasks had branches of their own. Each reader of a task's
// branch reads it here.
func (t PlanTask) BranchOf(issueKey string) string {
	if t.Branch == "" {
		return claim.IssueBranch(issueKey)
	}
	return t.Branch
}

// BranchGiven says whether generation of issue gave branch (plan_branches): to a task of it
// (AssignTaskBranch), as the branch of a pull request of an earlier run (RecordBranchGiven), or
// with the run a hand-over carried (CarryPlan). The workflow reads it where it records a pull
// request on the issue's own branch that the one task of a plan of one holds before its start
// (StoredPlan.HoldsBranchBeforeItsStart): that task takes the issue's branch at its start only
// where its generation gave that branch to no task, so a pull request there is not its own where
// the generation gave it to a task a later version removed.
func (s *Postgres) BranchGiven(ctx context.Context, tx pgx.Tx, issue string, generation uint64, branch string) (bool, error) {
	var given bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from plan_branches where issue = $1 and generation = $2 and branch = $3)`,
		issue, int64(generation), branch).Scan(&given); err != nil {
		return false, fmt.Errorf("read whether %s gave %s at generation %d: %w", issue, branch, generation, err)
	}
	return given, nil
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
	// gives before the step is written and nothing else writes. A step a release before the column
	// wrote holds none.
	Agent claim.Agent
}

// AgentName is the name of the step's agent: the last part of its claim token, and the one reader
// of Agent. A step of the key owner has none, since its agent is the tree's architect, whose token
// names the tree and no name on the issue. A step a release before the column wrote holds none and
// reads as its role: that release ran each role at most once in a task and refused every key but
// the owner's, so its role named its agent.
func (s PlanStep) AgentName() claim.Agent {
	switch {
	case plan.AgentOf(s.Task, s.Position, plan.Step{Role: s.Role, Key: s.AgentKey}).Owner():
		return ""
	case s.Agent != "":
		return s.Agent
	}
	return claim.Agent(s.Role)
}

// StoredPlan is the plan of an issue at one generation: the version in force, every task ordered by
// its id, and the steps of each task by its id, ordered by place. Steps is never nil.
type StoredPlan struct {
	Version PlanVersion
	Tasks   []PlanTask
	Steps   map[string][]PlanStep
}

// MergingTask is the task of p whose work is the issue's that merges: the one task no task needs
// (plan.Sinks), whatever the plan's count of tasks, and none (false) where the plan holds no task,
// or more than one task no task needs, which plan.Supported refuses. It is the one rule every
// reader of the task that merges reads by: the walk of each task (its retro and merge), the base of
// a task's pull request (BaseOf) and the read of an issue's merge pull request (MergePullRequest).
func (p *StoredPlan) MergingTask() (string, bool) {
	tasks := make([]plan.Task, len(p.Tasks))
	for i, row := range p.Tasks {
		tasks[i] = plan.Task{ID: row.Task, Needs: row.Needs}
	}
	if sinks := plan.Sinks(tasks); len(sinks) == 1 {
		return sinks[0], true
	}
	return "", false
}

// TaskOf is the task of p whose id is id, and false where p holds none, or where there is no plan
// (p nil): the one lookup of a plan's task by its id.
func (p *StoredPlan) TaskOf(id string) (PlanTask, bool) {
	if p == nil {
		return PlanTask{}, false
	}
	if at := slices.IndexFunc(p.Tasks, func(row PlanTask) bool { return row.Task == id }); at >= 0 {
		return p.Tasks[at], true
	}
	return PlanTask{}, false
}

// TasksOfBranch is each task of p whose branch branch is, for the issue keyed issueKey, in task id
// order: the one rule by which a pull request on branch, and each fact of it, is a task's
// (PullRequest.Task, TaskOfBranch, HoldsBranch). A task whose row holds branch is its only task. A
// task whose row holds none (BranchOf reads the issue's own branch for it, claim.IssueBranch)
// holds that branch, while no task's row holds it, in two cases alone. It started (unstarted), as
// a task a release before tasks had branches of their own did, on that branch; each start now
// gives a task its branch (AssignTaskBranch, which counts the issue's branch given while a started
// task holds none). Or it has not started and takes the issue's branch at its first start
// (takesIssueBranch), as an issue always did. Any other task that has not started was given no
// branch, and its first start can give it another than the issue's, so it holds none: no pull
// request on the issue's branch is its own before then, neither one a person opened there nor one
// of an earlier run that a run started again closed. Started tasks hold the branch before one that
// has not. In the daemon a row of a started task holds no branch only for that task of the release
// before, which ran plans of one task (plan.Supported), so the answer is one task or none; a plan
// whose rows name several such started tasks names each of them.
func (p StoredPlan) TasksOfBranch(issueKey, branch string) []string {
	if branch == "" {
		return nil
	}
	var started, unstarted []string
	for _, task := range p.Tasks {
		switch {
		case task.Branch == branch:
			return []string{task.Task}
		case task.Branch != "" || branch != claim.IssueBranch(issueKey):
		case !task.unstarted():
			started = append(started, task.Task)
		case p.takesIssueBranch(task):
			unstarted = append(unstarted, task.Task)
		}
	}
	if len(started) > 0 {
		return started
	}
	return unstarted
}

// takesIssueBranch says whether task, a task of p, takes the issue's own branch at its first start
// before any other task can start: it is the task that merges (MergingTask) and needs none, so it
// is the whole of the issue's work and its first start is the generation's first, which
// AssignTaskBranch gives the issue's branch. A plan whose task that merges needs another starts
// that other first, and which of its tasks starts first is not known before then.
func (p StoredPlan) takesIssueBranch(task PlanTask) bool {
	merging, ok := p.MergingTask()
	return ok && merging == task.Task && len(task.Needs) == 0
}

// unstarted says whether t has not started its work: it waits, or a stop took it before its first
// step, at no act (TaskPlace.Started).
func (t PlanTask) unstarted() bool {
	return t.State == TaskWaiting || t.State == TaskStopped && !t.Place.Started()
}

// TaskOfBranch is the task of p whose branch branch is (TasksOfBranch), for the issue keyed
// issueKey, and whether one is: none where no task's branch it is, and none where it is several
// tasks' branch, whose facts are no one task's.
func (p StoredPlan) TaskOfBranch(issueKey, branch string) (string, bool) {
	if tasks := p.TasksOfBranch(issueKey, branch); len(tasks) == 1 {
		return tasks[0], true
	}
	return "", false
}

// HoldsBranch says whether a pull request on branch is one of the issue keyed issueKey, whose plan
// at its generation p is (nil for an issue with no plan rows there): the one rule by which the
// workflow records a pull request (its issueForBranch), and by which the merge of the pull request
// of the task that merges closes the others (closeOtherTaskPullRequests). It is where a task of the
// plan holds branch (TasksOfBranch), and the issue's own branch (claim.IssueBranch) where there is
// no plan, as an issue that ran before plans did. A pull request on any other branch of an issue's
// shape (claim.BranchIssue) is none of the issue's: a person's or a fork's on a name no task was
// given, and one whose row the record deleted when it let the pull request go, whose task a later
// version removed or whose run was started again. The recorder reads one fact more for the one case
// HoldsBranchBeforeItsStart names.
func (p *StoredPlan) HoldsBranch(issueKey, branch string) bool {
	if p == nil {
		return branch == claim.IssueBranch(issueKey)
	}
	return len(p.TasksOfBranch(issueKey, branch)) > 0
}

// HoldsBranchBeforeItsStart says whether a task of p holds branch (TasksOfBranch) only as a task
// that has not started and takes the issue's branch at its first start (takesIssueBranch): branch
// is the issue's own, and the task's row holds no branch. Its first start gives it the issue's
// branch only where its generation gave that branch to no task (AssignTaskBranch), which the plan's
// rows cannot say once a later version removed the task that held it, so the workflow reads that
// too where it records a pull request (Store.BranchGiven).
func (p *StoredPlan) HoldsBranchBeforeItsStart(issueKey, branch string) bool {
	if p == nil || branch != claim.IssueBranch(issueKey) {
		return false
	}
	return slices.ContainsFunc(p.Tasks, func(task PlanTask) bool {
		return task.Branch == "" && task.unstarted() && p.takesIssueBranch(task)
	})
}

// BaseOf is the branch the pull request of task of p, the plan of issue, is based on, and whether
// it is the task's join branch, which the join of its needs makes (join true): the one rule the
// join's executor, the opener of a task's pull request, the task text and the judgement of a later
// version of the plan read. It is issue's default branch for the task that merges (MergingTask), for
// a task that needs none and for a task none of whose steps is of the type implementation, whose
// pull request shows the joined work of its needs, which its steps read. A task with one need is
// based on that need's branch (BranchOf). A task with several is based on its join branch, its own
// branch and claim.JoinBranchSuffix, where the work of its needs is joined and from which its branch
// starts, so its pull request shows its own work alone. It is read for a task its first start gave a
// branch (AssignTaskBranch). A task p does not hold, and a need it does not hold, are an error naming
// it: plan.Validate refuses such a plan, so p holding one is a defect, which no base would answer
// truly.
func (p StoredPlan) BaseOf(issue Issue, task string) (base string, join bool, err error) {
	of, ok := p.TaskOf(task)
	if !ok {
		return "", false, fmt.Errorf("the base of task %s of %s: its plan holds no such task", task, issue.Key)
	}
	var first PlanTask
	for i, id := range of.Needs {
		need, ok := p.TaskOf(id)
		if !ok {
			return "", false, fmt.Errorf("the base of task %s of %s: it needs %s, which its plan holds no task of", task, issue.Key, id)
		}
		if i == 0 {
			first = need
		}
	}
	if merging, merges := p.MergingTask(); len(of.Needs) == 0 || merges && merging == task || !p.Implements(task) {
		return issue.DefaultBranch, false, nil
	}
	if len(of.Needs) > 1 {
		return of.BranchOf(issue.Key) + claim.JoinBranchSuffix, true, nil
	}
	return first.BranchOf(issue.Key), false, nil
}

// Implements says whether a step of task of p is of the type implementation: the step that opens
// the task's pull request. A task with no such step gets its pull request from Legion.
func (p StoredPlan) Implements(task string) bool {
	return slices.ContainsFunc(p.Steps[task], func(step PlanStep) bool { return step.Type == plan.TypeImplementation })
}

// PutPlan stores version v of the plan of v.Issue at v.Generation, with the tasks it gives and the
// steps of those tasks, and the tasks v records it added (PlanVersion.Added, none for nil). The
// same version given twice is refused, and so is a task or a step of another issue or generation,
// or a step of a task v does not give, whose step the write would leave beside a task that keeps
// its own; each refusal is made before anything is written, so the transaction stays usable. A
// task the generation holds already takes v's text and attributes and keeps where it stands, which
// the walk writes; a new task is written as given. The steps of each task v gives are replaced by
// the steps v gives it, each with the name of its agent as it is given (NameAgents names them
// before this write; PutPlan has no rule of its own).
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
	added := v.Added
	if added == nil {
		added = []string{}
	}
	tag, err := tx.Exec(ctx, `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at, added)
		values ($1, $2, $3, $4, $5, $6, $7, $8) on conflict (issue, generation, version) do nothing`,
		v.Issue, int64(v.Generation), v.Version, v.Source, v.ArtifactID, string(v.GivenBy), v.GivenAt, added)
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
		if _, err := tx.Exec(ctx, `insert into plan_steps (issue, generation, task, position, role, type, agent_key, agent)
			values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			v.Issue, int64(v.Generation), step.Task, step.Position, step.Role, step.Type, step.AgentKey, string(step.Agent)); err != nil {
			return fmt.Errorf("%s: step %d of task %s: %w", describe, step.Position, step.Task, err)
		}
	}
	noteRecordWrite(ctx)
	return nil
}

// planVersionColumns is the columns of one issue_plans row as scanPlanVersion reads them.
const planVersionColumns = `issue, generation, version, source, artifact_id, given_by, given_at, earlier_givers, added`

// AddPlanGiver adds giver to the givers of version of the plan of issue at generation
// (PlanVersion.EarlierGivers): the giver it named moves to the end of the earlier givers, and
// giver, at at, becomes GivenBy, which a release before this column reads as the one giver. It
// answers whether it changed the row: a giver the version names already, as GivenBy or among the
// earlier givers, changes nothing, so an earlier giver never becomes GivenBy again; and so does a
// version the store does not hold.
func (s *Postgres) AddPlanGiver(ctx context.Context, tx pgx.Tx, issue string, generation uint64, version int, giver claim.Token, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `update issue_plans set earlier_givers = earlier_givers || given_by, given_by = $4, given_at = $5
		where issue = $1 and generation = $2 and version = $3 and given_by <> $4 and not ($4 = any(earlier_givers))`,
		issue, int64(generation), version, string(giver), at)
	if err != nil {
		return false, fmt.Errorf("add %s to the givers of version %d of the plan of %s at generation %d: %w", giver, version, issue, generation, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Plan is the plan of issue at generation, or nil where that generation has no plan rows.
func (s *Postgres) Plan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (*StoredPlan, error) {
	plans, err := plansOf(ctx, tx, []string{issue}, []int64{int64(generation)})
	if err != nil {
		return nil, fmt.Errorf("read the plan of %s at generation %d: %w", issue, generation, err)
	}
	return plans[issue], nil
}

// PlansOf is the plan of each of issues at the issue's own generation, by key, in the three
// statements Plan asks whatever the number of issues (one where none of them has plan rows, and
// none for no issue): a pass over many issues that reads each one's plan pays no round trip per
// issue. An issue whose generation has no plan rows has no entry.
func (s *Postgres) PlansOf(ctx context.Context, tx pgx.Tx, issues []Issue) (map[string]*StoredPlan, error) {
	if len(issues) == 0 {
		return map[string]*StoredPlan{}, nil
	}
	keys, generations := make([]string, len(issues)), make([]int64, len(issues))
	for i, issue := range issues {
		keys[i], generations[i] = issue.Key, int64(issue.Generation)
	}
	plans, err := plansOf(ctx, tx, keys, generations)
	if err != nil {
		return nil, fmt.Errorf("read the plans of %d issues: %w", len(issues), err)
	}
	return plans, nil
}

// plansOf is the body of Plan and PlansOf: the plan of the issue keys[i] at generations[i], for
// each i, by key. Each plan is its highest version, every task ordered by its id and the steps of
// each task ordered by place; the tasks and the steps are not read where no issue has a version.
func plansOf(ctx context.Context, tx pgx.Tx, keys []string, generations []int64) (map[string]*StoredPlan, error) {
	const read = `unnest($1::text[], $2::bigint[]) as read(key, gen)`
	versions, err := tx.Query(ctx, `select distinct on (issue_plans.issue) `+planVersionColumns+` from issue_plans
		join `+read+` on issue_plans.issue = read.key and issue_plans.generation = read.gen
		order by issue_plans.issue, issue_plans.version desc`, keys, generations)
	if err != nil {
		return nil, err
	}
	plans := map[string]*StoredPlan{}
	for versions.Next() {
		version, err := scanPlanVersion(versions)
		if err != nil {
			versions.Close()
			return nil, err
		}
		plans[version.Issue] = &StoredPlan{Version: version, Steps: map[string][]PlanStep{}}
	}
	versions.Close()
	if err := versions.Err(); err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return plans, nil
	}
	rows, err := tx.Query(ctx, `select `+planTaskColumns+` from plan_tasks
		join `+read+` on plan_tasks.issue = read.key and plan_tasks.generation = read.gen
		order by plan_tasks.issue, plan_tasks.task`, keys, generations)
	if err != nil {
		return nil, err
	}
	tasks, err := scanPlanTasks(rows)
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if plan := plans[task.Issue]; plan != nil {
			plan.Tasks = append(plan.Tasks, task)
		}
	}
	steps, err := tx.Query(ctx, `select plan_steps.issue, plan_steps.generation, plan_steps.task, plan_steps.position, plan_steps.role,
		plan_steps.type, plan_steps.agent_key, plan_steps.agent from plan_steps
		join `+read+` on plan_steps.issue = read.key and plan_steps.generation = read.gen
		order by plan_steps.issue, plan_steps.task, plan_steps.position`, keys, generations)
	if err != nil {
		return nil, err
	}
	defer steps.Close()
	for steps.Next() {
		var step PlanStep
		var generation int64
		if err := steps.Scan(&step.Issue, &generation, &step.Task, &step.Position, &step.Role, &step.Type, &step.AgentKey, &step.Agent); err != nil {
			return nil, err
		}
		step.Generation = uint64(generation)
		if plan := plans[step.Issue]; plan != nil {
			plan.Steps[step.Task] = append(plan.Steps[step.Task], step)
		}
	}
	if err := steps.Err(); err != nil {
		return nil, err
	}
	return plans, nil
}

// PlanStepsOf is every plan_steps row of issue, of every generation, each with the name of its agent,
// ordered by generation, task and place: what NameAgents reads as earlier before a version's steps
// are written (PutPlan deletes the steps of each task it gives), so a key keeps the name an earlier
// version or generation gave it.
func (s *Postgres) PlanStepsOf(ctx context.Context, tx pgx.Tx, issue string) ([]PlanStep, error) {
	describe := fmt.Sprintf("read every step of the plans of %s", issue)
	rows, err := tx.Query(ctx, `select generation, task, position, role, type, agent_key, agent
		from plan_steps where issue = $1 order by generation, task, position`, issue)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", describe, err)
	}
	defer rows.Close()
	var steps []PlanStep
	for rows.Next() {
		step := PlanStep{Issue: issue}
		var generation int64
		if err := rows.Scan(&generation, &step.Task, &step.Position, &step.Role, &step.Type, &step.AgentKey, &step.Agent); err != nil {
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
	plan_tasks.pass, plan_tasks.rounds, plan_tasks.conflict_rounds, plan_tasks.backward_moves, plan_tasks.cap_told,
	plan_tasks.branch, plan_tasks.join_conflicts, plan_tasks.join_unmerged, plan_tasks.hold_reason, plan_tasks.step_wait,
	plan_tasks.step_wait_since, plan_tasks.step_wait_from, plan_tasks.start_reason, plan_tasks.start_report, plan_tasks.start_task`

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
			&task.Counts.Pass, &task.Counts.Rounds, &task.Counts.ConflictRounds, &moves, &task.Counts.CapTold,
			&task.Branch, &task.JoinConflicts, &task.JoinUnmerged, &task.HoldReason, &task.StepWait, &task.StepWaitSince, &task.StepWaitFrom,
			&task.StartReason, &task.StartReport, &task.StartTask); err != nil {
			return nil, err
		}
		task.Generation = uint64(generation)
		if len(task.JoinConflicts) == 0 {
			task.JoinConflicts = nil
		}
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

// PlansInForce is each owner's version of the plan of issue that is the plan in force of its
// generation, generation 0 among them, which holds a plan given before the daemon recorded the
// issue, and each copy a carry wrote to a later generation (CarryPlan) among them, in the order
// they were given (PlanVersion.GivenAt, the first give of the giver the store added last), and of
// two given at one moment the earlier generation's first. A default plan is nobody's, so none is
// among them. It judges no giver: OwnersNewestPlan, PlansOfEarlierOwners and PlansOfOwners judge
// each version through the store their caller hands (GivenByOwner).
func (s *Postgres) PlansInForce(ctx context.Context, tx pgx.Tx, issue string) ([]PlanVersion, error) {
	rows, err := tx.Query(ctx, "select "+planVersionColumns+` from issue_plans where `+inForce+`
		order by given_at, generation`, issue)
	if err != nil {
		return nil, fmt.Errorf("read the plans in force of %s: %w", issue, err)
	}
	given, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PlanVersion, error) { return scanPlanVersion(row) })
	if err != nil {
		return nil, fmt.Errorf("read the plans in force of %s: %w", issue, err)
	}
	return given, nil
}

// OwnersNewestPlan is the newest plan in force that an owner of issue gave, of any generation
// (Store.PlansInForce), generation 0 among them, which holds a plan given before the daemon
// recorded the issue: the latest given of which a giver is an architect of the issue's tree now,
// as store reads the tree (GivenByOwner), and of two given at one moment the later generation's.
// nil where no owner gave a plan that is in force. The versions are read, and their rows closed,
// before each giver is judged, since a transaction runs one statement at a time.
func OwnersNewestPlan(ctx context.Context, store Store, tx pgx.Tx, issue Issue) (*StoredPlan, error) {
	given, err := store.PlansInForce(ctx, tx, issue.Key)
	if err != nil {
		return nil, err
	}
	for _, version := range slices.Backward(given) {
		owner, err := GivenByOwner(ctx, store, tx, version, issue)
		if err != nil {
			return nil, fmt.Errorf("read the newest plan an owner gave for %s: %w", issue.Key, err)
		}
		if owner {
			return store.Plan(ctx, tx, issue.Key, version.Generation)
		}
	}
	return nil, nil
}

// PlansOfEarlierOwners is each plan in force of issue (Store.PlansInForce) that only earlier owners
// gave, as store reads the issue's tree (GivenByEarlierOwners), in the order they were given: the
// plans the issue's first step passes over. The architect of the tree's root and each
// sub-architect of the tree are owners, so no plan one of them gave is among them.
func PlansOfEarlierOwners(ctx context.Context, store Store, tx pgx.Tx, issue Issue) ([]PlanVersion, error) {
	return plansInForceWhere(ctx, store, tx, issue, fmt.Sprintf("read the plans of %s that earlier owners gave", issue.Key), GivenByEarlierOwners)
}

// PlansOfOwners is each plan in force of issue (Store.PlansInForce) of which a giver is an
// architect of the issue's tree now, as store reads the tree (GivenByOwner), in the order they
// were given: every plan an owner gave that a first step could take, generation 0 and each copy a
// carry wrote among them.
func PlansOfOwners(ctx context.Context, store Store, tx pgx.Tx, issue Issue) ([]PlanVersion, error) {
	return plansInForceWhere(ctx, store, tx, issue, fmt.Sprintf("read the plans of %s that its owners gave", issue.Key), GivenByOwner)
}

// plansInForceWhere is each plan in force of issue (Store.PlansInForce) that keep keeps, judged
// through store, in the order they were given; describe names the read in an error. The versions
// are read, and their rows closed, before keep judges any, since a transaction runs one statement
// at a time.
func plansInForceWhere(ctx context.Context, store Store, tx pgx.Tx, issue Issue, describe string,
	keep func(context.Context, Store, pgx.Tx, PlanVersion, Issue) (bool, error)) ([]PlanVersion, error) {
	given, err := store.PlansInForce(ctx, tx, issue.Key)
	if err != nil {
		return nil, err
	}
	var plans []PlanVersion
	for _, version := range given {
		kept, err := keep(ctx, store, tx, version, issue)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", describe, err)
		}
		if kept {
			plans = append(plans, version)
		}
	}
	return plans, nil
}

// CarryPlan copies the plan of issue at generation from to generation to: every version, with its
// givers and the tasks it recorded it added, every task and every step. carried is a hand-over
// that carries its run, whose tasks go on where they stood, so each task keeps its state, its
// hold's reason, its place, its step's wait, since when and the phase the start that waits moved
// the issue from, why its step started, the report of the step before its start tells and the task
// a return hands that start, its counts, its branch and the needs whose join conflicted, and the
// generation every name it gave a task (AssignTaskBranch), since the run goes on in those branches.
// Without
// carried, the plan starts from its start: each task waits at no act with no hold, no step wait,
// every count 0 and no branch, and to has given no name yet. The names from gave stay given to the
// issue (AssignTaskBranch reads them at every generation), so no task of a plan of several takes
// one of them, the issue's branch among them, while the one task of a plan of one takes the
// issue's branch, as an issue always did. Neither copies a count of starts, which step_starts
// keeps by place alone. Where from has no plan rows it writes nothing, so an issue that ran before
// plans goes on as it did. A generation to that holds a plan already is refused.
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
	var names []string
	if carried {
		tasks = `insert into plan_tasks (issue, generation, task, plan_version, block_text, needs, part_of, owner_role, merges, steps_by_default,
			state, stopped_reason, hold_reason, running_kind, position, step_wait, step_wait_since, step_wait_from, pass, rounds,
			conflict_rounds, backward_moves, cap_told, branch, join_conflicts, join_unmerged, start_reason, start_report, start_task)
			select issue, $3, task, plan_version, block_text, needs, part_of, owner_role, merges, steps_by_default,
			state, stopped_reason, hold_reason, running_kind, position, step_wait, step_wait_since, step_wait_from, pass, rounds,
			conflict_rounds, backward_moves, cap_told, branch, join_conflicts, join_unmerged, start_reason, start_report, start_task
			from plan_tasks where issue = $1 and generation = $2`
		names = []string{`insert into plan_branches (issue, generation, branch)
			select issue, $3, branch from plan_branches where issue = $1 and generation = $2 on conflict do nothing`}
	}
	for _, statement := range append([]string{
		`insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at, earlier_givers, added)
			select issue, $3, version, source, artifact_id, given_by, given_at, earlier_givers, added from issue_plans where issue = $1 and generation = $2`,
		tasks,
		`insert into plan_steps (issue, generation, task, position, role, type, agent_key, agent)
			select issue, $3, task, position, role, type, agent_key, agent
			from plan_steps where issue = $1 and generation = $2`,
	}, names...) {
		if _, err := tx.Exec(ctx, statement, issue, int64(from), int64(to)); err != nil {
			return fmt.Errorf("%s: %w", describe, err)
		}
	}
	noteRecordWrite(ctx)
	return nil
}

// DeletePlan deletes the plan of issue at generation, every row of it in each of the three tables
// (the tasks and the steps go with their versions, on delete cascade).
func (s *Postgres) DeletePlan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) error {
	tag, err := tx.Exec(ctx, `delete from issue_plans where issue = $1 and generation = $2`, issue, int64(generation))
	if err != nil {
		return fmt.Errorf("delete the plan of %s at generation %d: %w", issue, generation, err)
	}
	noteRowsWritten(ctx, tag)
	return nil
}

// AgentWaiter is the task of the plan of issue at generation in the state running whose step has
// waited longest for agent: of the steps that wait for their agent (StepWaitAgent) and whose agent
// is agent, as a running step's is read (stepOfRow, PlanStep.AgentName), the one whose wait began
// first (PlanTask.StepWaitSince), and of two that began at one moment the first in task id order.
// It is "" where no step waits for agent. A step that waits for its join waits for no agent, and a
// task that is not running has no step that waits, whatever its row still holds: a freed agent
// goes to this step, and an agent with a step waiting since earlier is busy for every other step.
func (s *Postgres) AgentWaiter(ctx context.Context, tx pgx.Tx, issue string, generation uint64, agent claim.Agent) (string, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, "select "+pods.AgentWaitersColumn("$1::text", "$2::bigint"), issue, int64(generation)).Scan(&raw); err != nil {
		return "", fmt.Errorf("read the step of %s at generation %d that waited longest for agent %s: %w", issue, generation, agent, err)
	}
	rows, _, err := pods.DecodeRunning(raw)
	if err != nil {
		return "", fmt.Errorf("read the step of %s at generation %d that waited longest for agent %s: %w", issue, generation, agent, err)
	}
	for _, row := range rows {
		if agent != "" && stepOfRow(row, false).Agent == agent {
			return row.Task, nil
		}
	}
	return "", nil
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
	noteRecordWrite(ctx)
	return nil
}

// scanPlanVersion reads one issue_plans row of planVersionColumns.
func scanPlanVersion(row scanner) (PlanVersion, error) {
	var version PlanVersion
	var generation int64
	var givenBy string
	var earlier, added []string
	if err := row.Scan(&version.Issue, &generation, &version.Version, &version.Source, &version.ArtifactID, &givenBy, &version.GivenAt, &earlier, &added); err != nil {
		return PlanVersion{}, err
	}
	version.Generation, version.GivenBy, version.GivenAt = uint64(generation), claim.Token(givenBy), version.GivenAt.UTC()
	for _, giver := range earlier {
		version.EarlierGivers = append(version.EarlierGivers, claim.Token(giver))
	}
	if len(added) > 0 {
		version.Added = added
	}
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
