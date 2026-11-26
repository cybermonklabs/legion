package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// planGiven is the plan an owner gave with register_plan, stored at the generation the route gave
// it at: the issue's own, or 0 for an issue the daemon had not recorded, whose plan waits there for
// the issue's first step. It has three parts, each its own function: the judgement of the version
// (versionRefusal), its write (writeVersion), and what the write starts on a recorded issue
// (afterVersion). Each refusal writes nothing and leaves the fact unspent, so the same
// registration, whose id names the issue, the generation, the document and the version (api's
// registerPlan), applies when it is given again: a version refused while its task ran is taken
// after stop_task, and the owner's plan given again at the generation the record holds meets the
// rules of a later version there. A version taken answers the tasks it adds over the version in
// force that versionRefusal read (intake.Result.Added, record.PlanVersion.Added), which the route
// answers, and which the write records on the version, since a give of a version the store holds
// never reaches this apply (intake.ApplyFact) and is answered that, not what the version would add
// over the version in force then.
//
// Apply judged the plan already on the record as the fact reads it, since the route read the
// record, or Dispatch, before the fact lock (requestRefusal): a giver that owns the issue no
// longer is refused ISSUE_OUTSIDE_TREE there, and a plan of a run the issue has left
// PLAN_STALE_GENERATION.
func (e *Engine) planGiven(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven) (intake.Result, error) {
	given, refusal, err := e.versionRefusal(ctx, tx, fact)
	if err != nil || refusal != nil {
		return intake.Result{Refusal: refusal, Unspent: refusal != nil}, err
	}
	written, err := e.writeVersion(ctx, tx, fact, given)
	if err == nil && given.recorded {
		err = e.afterVersion(ctx, tx, fact, given, written)
	}
	return intake.Result{Added: given.version.Added}, err
}

// givenVersion is what versionRefusal read of a version it takes: the issue the record holds (nil
// for one it does not), whether the version is of the issue's own generation (recorded, false for
// a plan waiting at generation 0), the plan in force at the fact's generation (nil for none), the
// tasks the version changes, adds or removes over it (plan.ChangedTasks), and the rows the write
// stores (givenRows), each step named with its agent (record.NameAgents).
type givenVersion struct {
	issue    *record.Issue
	recorded bool
	stored   *record.StoredPlan
	changed  []string
	version  record.PlanVersion
	tasks    []record.PlanTask
	steps    []record.PlanStep
}

// versionRefusal judges fact's version, and answers what it read for the write, or the refusal.
// A generation's first plan is taken where its task has not started: an issue the daemon has not
// recorded, and one standing before its first step (beforeItsWork); each task waits at no act, and
// the opening of the gate starts the first step of its line (walk). An issue recorded with no plan
// rows whose work started ran before Legion took plans, and walks today's line to its end: its plan
// is refused. A plan given at generation 0, for an issue the route read unrecorded, of an issue the
// daemon recorded since, waits at generation 0 only while that issue stands before its first step
// with no plan rows at its own generation (waitsAtZero); once its first step took a plan, it is of
// a run that started without it, and is refused PLAN_STALE_GENERATION.
//
// A later version is compared, task by task, with the version in force as its owner gave it
// (heldPlan, plan.ChangedTasks). A version of another document than the one in force is refused,
// but over the default steps, and so is a version at or below it (laterVersionRefusal). A task the
// version changes or removes must wait or be stopped: a running task is refused, and so is a held
// task, one reason for each, and so is a task that is done or left the workflow. A version that
// adds a need of the task that merges is refused while that task stands at its retro or its merge
// or is done (needsOfMerging). A version that moves the base of a task that has started and is not
// stopped is refused too, so a task's later steps are never told a base its start did not make
// (startedBasesMoved). A plan given in a lingering tree is refused too.
//
// A recorded issue whose group takes no plan from its owner (groupOf, takesNoPlan) is answered
// PLAN_NOT_TAKEN, right after the generation is judged, so no plan of it is stored, also one given
// at generation 0 for an issue the daemon recorded meanwhile. The fact is left unspent as well:
// the group never changes, so the same registration sent again gets the same answer, never the
// answer of a registration applied, which the route gives as the plan taken. An issue the daemon
// has not recorded has no draw yet; its plan waits at generation 0, and its first step passes it
// over where the issue's group takes none (planFirstStep).
//
// Each step of the version is named from every row of the issue as it stands now
// (record.NameAgents, Store.PlanStepsOf), before anything is written, since the write deletes the
// steps of each task it gives: so a key keeps its name, and a version in which a fresh agent would
// need the name a key holds from an earlier version is refused as a plan (intake.AgentUnnamed),
// never by the write.
func (e *Engine) versionRefusal(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven) (givenVersion, *intake.Refusal, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil {
		return givenVersion{}, nil, err
	}
	// The route gives generation 0 to an issue it found unrecorded, and such a plan waits at
	// generation 0 for the issue's first step; recorded is a plan of the issue's own generation.
	given := givenVersion{issue: issue, recorded: issue != nil && fact.Generation != 0, changed: []string{}}
	if issue != nil && !given.recorded {
		waits, err := e.waitsAtZero(ctx, tx, *issue)
		if err != nil || !waits {
			return givenVersion{}, refusalUnless(err, intake.PlanStaleGeneration(fact.Issue, fact.Generation, issue.Generation)), err
		}
	}
	if issue != nil {
		group, err := e.groupOf(ctx, tx, issue.Key)
		if err != nil {
			return givenVersion{}, nil, err
		}
		if takesNoPlan(group) {
			return givenVersion{}, intake.PlanNotTaken(issue.Key, group), nil
		}
	}
	if given.stored, err = e.store.Plan(ctx, tx, fact.Issue, fact.Generation); err != nil {
		return givenVersion{}, nil, err
	}
	if given.stored == nil && given.recorded && !beforeItsWork(*issue) {
		return givenVersion{}, intake.PlanRefused([]plan.Refusal{intake.StartedBeforePlans(issue.Key)}), nil
	}
	if given.stored != nil {
		if refusal := laterVersionRefusal(fact.Issue, given.stored.Version, fact.Plan); refusal != nil {
			return givenVersion{}, refusal, nil
		}
		given.changed = plan.ChangedTasks(heldPlan(given.stored), fact.Plan)
	}
	given.version, given.tasks, given.steps = givenRows(fact, given.stored, given.changed, e.cfg.DefaultSteps, e.cfg.Deployment, e.now())
	if given.stored != nil {
		var refused []plan.Refusal
		for _, id := range given.changed {
			task, ok := given.stored.TaskOf(id)
			if !ok {
				continue
			}
			if refusal := intake.FinishedTask(fact.Issue, id, task.State, intake.ChangeOfTask); refusal != nil {
				return givenVersion{}, refusal, nil
			}
			switch task.State {
			case record.TaskRunning:
				refused = append(refused, intake.RunningTaskChanged(id))
			case record.TaskHeld:
				refused = append(refused, intake.HeldTaskChanged(id, heldAt(given.stored, task)))
			}
		}
		refused = append(refused, needsOfMerging(given.stored, fact.Plan, given.changed)...)
		if issue != nil {
			moved, err := startedBasesMoved(*issue, given.stored, nextPlan(given.stored, given.version, given.tasks, given.steps), given.changed)
			if err != nil {
				return givenVersion{}, nil, err
			}
			refused = append(refused, moved...)
		}
		if len(refused) > 0 {
			return givenVersion{}, intake.PlanRefused(refused), nil
		}
	}
	if issue != nil {
		lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
		if err != nil || lingers {
			return givenVersion{}, refusalUnless(err, refusedLingering(*issue, fmt.Sprintf("version %d of the owner's plan", fact.Plan.Version)).Refusal), err
		}
	}
	earlier, err := e.store.PlanStepsOf(ctx, tx, fact.Issue)
	if err != nil {
		return givenVersion{}, nil, err
	}
	held, err := e.store.KeyNames(ctx, tx, fact.Issue)
	if err != nil {
		return givenVersion{}, nil, err
	}
	given.steps = record.NameAgents(earlier, given.steps, held)
	if unnamed := record.UnnamedAgents(earlier, given.steps, held); len(unnamed) > 0 {
		reasons := make([]plan.Refusal, len(unnamed))
		for i, agent := range unnamed {
			reasons[i] = intake.AgentUnnamed(agent.Step.Task, agent.Step.Position, string(agent.Name), agent.Holder)
		}
		return givenVersion{}, intake.PlanRefused(reasons), nil
	}
	return given, nil, nil
}

// refusalUnless is refusal where err is nil, and nil where a read failed, which answers the error
// in its place.
func refusalUnless(err error, refusal *intake.Refusal) *intake.Refusal {
	if err != nil {
		return nil
	}
	return refusal
}

// writtenVersion is what writeVersion read of the version in force before it wrote fact's over
// it: each task of it that was stopped and that the version changes (stopped), and whether the
// run's work started (started): a task of the version in force stood at an act of its line. A first
// plan is taken only before the work (beforeItsWork), so without stored rows it did not.
type writtenVersion struct {
	stopped []string
	started bool
}

// writeVersion stores fact's version as the version in force, the rows versionRefusal built and
// named (given): each task it leaves as it was keeps its steps and where it stands, each task it
// changes takes the steps its block now names, and each task it removes is deleted, with the row of
// its pull request, which is closed where it is open (closeRemovedPullRequests), and its working
// copy is queued for removal (removeTaskCopies). It answers what it read of the version before.
func (e *Engine) writeVersion(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven, given givenVersion) (writtenVersion, error) {
	version, tasks, steps := given.version, given.tasks, given.steps
	var removed []string
	var written writtenVersion
	if given.stored != nil {
		for _, task := range given.stored.Tasks {
			if !slices.ContainsFunc(fact.Plan.Tasks, func(next plan.Task) bool { return next.ID == task.Task }) {
				removed = append(removed, task.Task)
			}
			if task.State == record.TaskStopped && slices.Contains(given.changed, task.Task) {
				written.stopped = append(written.stopped, task.Task)
			}
			written.started = written.started || task.Place.Started()
		}
	}
	if len(removed) > 0 {
		if given.recorded {
			if err := e.closeRemovedPullRequests(ctx, tx, fact, removed); err != nil {
				return writtenVersion{}, err
			}
			if err := e.removeTaskCopies(ctx, tx, fact, given.stored, removed); err != nil {
				return writtenVersion{}, err
			}
		}
		if err := e.store.DeleteTasks(ctx, tx, fact.Issue, fact.Generation, removed); err != nil {
			return writtenVersion{}, err
		}
	}
	if err := e.store.PutPlan(ctx, tx, version, tasks, steps); err != nil {
		return writtenVersion{}, err
	}
	e.logOnCommit(ctx, "workflow: an owner's plan is taken", "issue", fact.Issue, "generation", fact.Generation, "version", fact.Plan.Version,
		"document", fact.Plan.ArtifactID, "given_by", fact.GivenBy, "changed", given.changed)
	return written, nil
}

// closeRemovedPullRequests deletes the row of the pull request of each task of fact's issue that
// fact's version removes (removed), and queues the close of each such pull request that is open,
// one row for each (record.TaskPullRequestsClose), with the comment `Task <id> was removed from
// the plan.`, in the version's transaction: GitHub keeps a pull request open until someone closes
// it. A row's task is read from its branch against the plan in force (record.PullRequest.Task), so
// the rows are read before the removed tasks go (DeleteTasks). A row left behind would be no
// task's, and one on the issue's own branch would read as the pull request of the task a version
// leaves alone, which holds that branch before its start as the one task of a plan of one
// (record.StoredPlan.TasksOfBranch), and as the issue's merge; so a closed or merged row goes too.
// A task a later version adds back starts on a branch of its own (record.Store.AssignTaskBranch),
// and no fact of the closed pull request finds a row. It runs for a version of the issue's own
// generation alone (givenVersion.recorded): a row's task is read at that generation, and a plan
// waiting at generation 0 has no task that started.
func (e *Engine) closeRemovedPullRequests(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven, removed []string) error {
	prs, err := e.store.TaskPullRequests(ctx, tx, fact.Issue)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if !slices.Contains(removed, pr.Task) {
			continue
		}
		if pr.State == record.PullRequestOpen {
			if err := e.enqueue(ctx, tx, fact.Issue, record.TaskPullRequestsClose{Generation: fact.Generation, Numbers: []int{pr.Number},
				Comment: fmt.Sprintf("Task %s was removed from the plan.", pr.Task)}); err != nil {
				return err
			}
		}
		if err := e.store.DeletePullRequest(ctx, tx, fact.Issue, pr.Branch); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: the pull request of a task the plan removed is deleted from the record, and closed where it was open",
			"issue", fact.Issue, "task", pr.Task, "pullRequest", pr.Number, "branch", pr.Branch, "state", pr.State, "version", fact.Plan.Version)
	}
	return nil
}

// removeTaskCopies queues the removal of the working copy of each task of fact's issue that fact's
// version removes (removed), one row for each (record.TaskCopyRemove), in the version's
// transaction: the deletion of the task's rows (DeleteTasks) leaves the jj workspace on its branch
// beside the issue's own working copy, and nothing else removes it, since the issue's own removal
// takes the working copies on the daemon's host only once the issue's tree has closed (the
// daemon's removeWorkspace). The branch is read from stored, the plan in force, before the tasks
// go. A task that never started holds no branch and has no working copy, and the task that holds
// the issue's own branch (record.PlanTask.BranchOf) works in the issue's own working copy, which
// goes only with the issue: neither queues a row. A task a later version adds back takes a branch
// of its own (record.Store.AssignTaskBranch), so no removal reaches its working copy. It runs for
// a version of the issue's own generation alone (givenVersion.recorded), as the close of a removed
// task's pull request does: a plan waiting at generation 0 has no task that started.
func (e *Engine) removeTaskCopies(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven, stored *record.StoredPlan, removed []string) error {
	for _, task := range stored.Tasks {
		branch := task.BranchOf(fact.Issue)
		if !slices.Contains(removed, task.Task) || branch == claim.IssueBranch(fact.Issue) {
			continue
		}
		if err := e.enqueue(ctx, tx, fact.Issue, record.TaskCopyRemove{Generation: fact.Generation, Task: task.Task, Branch: branch}); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: the working copy of a task the plan removed is queued for removal",
			"issue", fact.Issue, "task", task.Task, "branch", branch, "version", fact.Plan.Version)
	}
	return nil
}

// afterVersion is what a version written on a recorded issue starts, whatever the plan's count of
// tasks. A version taken after the work started writes the run's plan node again, whole, with each
// task the version holds (WritePlan), before any task of it starts: the start wrote it at the
// version then in force, and the node names the plan the run walks now. A version taken before the
// work starts writes none, since the start writes the version in force. Each stop the version ends
// (stopsEnded) ends on the task read for it: where the work started, which the place of a task of
// the version before says (record.TaskPlace.Started), the task starts again at its first step where
// its needs are done, or waits for them (startAtFirstStep), and its worker is told which version
// did it; before the work starts it waits for its first step, as a task never stopped does
// (waitForFirstStep). Then, once the work started, each task whose needs are done starts
// (startReady), a task the version adds among them. Before the work starts nothing starts: the
// gate's opening writes the node and starts each task whose needs are done (gateOpened).
func (e *Engine) afterVersion(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven, given givenVersion, written writtenVersion) error {
	issue := *given.issue
	if written.started {
		if err := e.WritePlan(ctx, tx, issue); err != nil {
			return err
		}
	}
	for _, ended := range stopsEnded(fact, given, written) {
		now, err := e.store.Issue(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		s, err := e.ScopeOf(ctx, tx, *now, ended.task)
		if err != nil {
			return err
		}
		reason := fmt.Sprintf("the owner gave version %d of the plan while task %s was stopped", fact.Plan.Version, strings.Join(ended.stopped, ", "))
		if !written.started {
			err = e.waitForFirstStep(ctx, tx, s, reason)
		} else {
			err = e.startAtFirstStep(ctx, tx, s, reason)
		}
		if err != nil {
			return err
		}
	}
	if !written.started {
		return nil
	}
	now, err := e.store.Issue(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return err
	}
	_, err = e.startReady(ctx, tx, *now, stored, "")
	return err
}

// endedStop is a stop a version ends: the task of the version it ends on, and the stopped tasks of
// the version before that it ends, which the task's worker is told.
type endedStop struct {
	task    string
	stopped []string
}

// stopsEnded is each stop fact's version ends, in task id order, whatever the plan's count of
// tasks: a stopped task the version changes and keeps ends its own stop; and a version that keeps
// no task of the version before replaces the plan whole, so each task it gives ends the stop of
// every stopped task it removes and starts as that one would have, told the version (the task
// default that a person's plan replaces among them). A version that keeps a task gives its new
// tasks no stop to end: each starts when its needs are done (startReady). A stopped task the
// version removes is deleted (writeVersion) and starts nothing itself.
func stopsEnded(fact intake.PlanGiven, given givenVersion, written writtenVersion) []endedStop {
	holds := func(id string) bool {
		return slices.ContainsFunc(fact.Plan.Tasks, func(next plan.Task) bool { return next.ID == id })
	}
	var ended []endedStop
	var removed []string
	for _, task := range written.stopped {
		if holds(task) {
			ended = append(ended, endedStop{task: task, stopped: []string{task}})
		} else {
			removed = append(removed, task)
		}
	}
	if len(removed) > 0 && !slices.ContainsFunc(given.stored.Tasks, func(row record.PlanTask) bool { return holds(row.Task) }) {
		for _, next := range fact.Plan.Tasks {
			ended = append(ended, endedStop{task: next.ID, stopped: removed})
		}
	}
	slices.SortFunc(ended, func(a, b endedStop) int { return strings.Compare(a.task, b.task) })
	return ended
}

// waitsAtZero says whether a plan given at generation 0 for issue, which the daemon recorded after
// the route read it unrecorded, still waits there for the issue's first step: the issue stands
// before that step (beforeItsWork), and its generation holds no plan the step keeps — none, or
// only one an earlier owner gave, which the step passes over (passOverEarlierOwners) — so the
// step takes the newest plan its owner gave, or passes it over too for an issue whose group takes
// none (planFirstStep). Once a plan of the generation is the one its run walks, the default steps
// the step wrote or a plan of its owner's, the run went on without the one given at generation 0.
// versionRefusal reads the group only after this judgement, so a plan given at generation 0 for an
// issue whose group takes none is answered PLAN_STALE_GENERATION once its run went on, and
// PLAN_NOT_TAKEN while it still waits.
func (e *Engine) waitsAtZero(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	if !beforeItsWork(issue) {
		return false, nil
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return err == nil, err
	}
	if stored.Version.Source != record.PlanSourceOwner {
		return false, nil
	}
	owners, err := record.ArchitectOfTree(ctx, e.store, tx, stored.Version.GivenBy, issue)
	return !owners && err == nil, err
}

// laterVersionRefusal is the refusal of next, given for issue over held, the version in force:
// a version of another document (PlanDocumentChanged), but over the project's default steps,
// which no document wrote, and a version at or below held (PlanVersionOld). nil for a later
// version of the same document.
func laterVersionRefusal(issue string, held record.PlanVersion, next plan.Plan) *intake.Refusal {
	if held.Source != record.PlanSourceDefault && next.ArtifactID != held.ArtifactID {
		return intake.PlanDocumentChanged(issue, held.ArtifactID)
	}
	if next.Version <= held.Version {
		return intake.PlanVersionOld(issue, held.Version, next.Version)
	}
	return nil
}

// heldAt is where task, a held task of stored, stands, as the refusal of a change of it names it
// (intake.HeldTaskChanged): `step <i> (<role>)`, or its retro or its merge.
func heldAt(stored *record.StoredPlan, task record.PlanTask) string {
	switch task.Place.Kind {
	case record.PlaceRetro:
		return "its retro"
	case record.PlaceMerge:
		return "its merge"
	}
	role := ""
	if steps := stored.Steps[task.Task]; task.Place.Position >= 1 && task.Place.Position <= len(steps) {
		role = steps[task.Place.Position-1].Role
	}
	return fmt.Sprintf("step %d (%s)", task.Place.Position, role)
}

// needsOfMerging is one refusal (intake.NeedOfMergingTask) for each task next adds or changes
// (changed) that needs the task of stored that merges (record.StoredPlan.MergingTask) where that
// task stands at its retro or its merge, or is done: the work of the issue merges there, and this
// release carries out no need of it then. None while that task stands at a step or waits.
func needsOfMerging(stored *record.StoredPlan, next plan.Plan, changed []string) []plan.Refusal {
	merging, merges := stored.MergingTask()
	row, ok := stored.TaskOf(merging)
	if !merges || !ok {
		return nil
	}
	var stands string
	switch {
	case row.Place.Kind == record.PlaceRetro:
		stands = "retro"
	case row.Place.Kind == record.PlaceMerge || row.State == record.TaskDone:
		stands = "merge"
	default:
		return nil
	}
	var refused []plan.Refusal
	for _, task := range next.Tasks {
		if task.ID != merging && slices.Contains(changed, task.ID) && slices.Contains(task.Needs, merging) {
			refused = append(refused, intake.NeedOfMergingTask(task.ID, merging, stands))
		}
	}
	return refused
}

// startedBasesMoved is one refusal (intake.StartedTaskBaseMoved) for each task of stored that has
// started and is not stopped, running or held, whose base (record.StoredPlan.BaseOf) next, the plan
// the version makes (nextPlan), moves. The task started on its base: its join, its branch and its
// pull request stand on it, and its later steps would be told the base next gives (planToldOf),
// which no start of the task made. A version moves the base of a task it leaves as it is where it
// moves the merge: one that adds the first task that needs the task that merges, which then merges
// no more and, with several needs, is based on its join branch; and one that removes the one task
// that needed a task, which then merges, based on the default branch. A task that waits, and a
// stopped task when it starts again, joins its needs on the base the plan gives at its start
// (startTaskCopy), so neither is refused.
//
// Two tasks are left to the refusal that names them already, so no task is named twice: a task the
// version changes (intake.RunningTaskChanged, intake.HeldTaskChanged), and the task that merges at
// its retro or its merge, whose base only a need of it moves (needsOfMerging), where no stop lets
// the version through. needsOfMerging is not a case of this rule: it also refuses a need of the
// task that merges once it is done, and a need of one at its retro or its merge whose base the need
// does not move (a task that needs none, or none of whose steps is of the type implementation,
// based on the default branch either way). A plan that holds no task or no need a base reads is an
// error (record.StoredPlan.BaseOf), which plan.Validate leaves no version to reach.
func startedBasesMoved(issue record.Issue, stored *record.StoredPlan, next record.StoredPlan, changed []string) ([]plan.Refusal, error) {
	var refused []plan.Refusal
	for _, task := range stored.Tasks {
		if task.State != record.TaskRunning && task.State != record.TaskHeld || slices.Contains(changed, task.Task) ||
			task.Place.Kind == record.PlaceRetro || task.Place.Kind == record.PlaceMerge {
			continue
		}
		base, _, err := stored.BaseOf(issue, task.Task)
		if err != nil {
			return nil, err
		}
		moved, _, err := next.BaseOf(issue, task.Task)
		if err != nil {
			return nil, fmt.Errorf("version %d of the plan of %s: %w", next.Version.Version, issue.Key, err)
		}
		if moved != base {
			refused = append(refused, intake.StartedTaskBaseMoved(task.Task, base, moved))
		}
	}
	return refused, nil
}

// nextPlan is the plan stored becomes once the rows of a version given over it (givenRows) are
// written (record.Store.PutPlan): version, and each of tasks, with the branch stored holds for a
// task the generation holds already, which the write keeps (a task new to the generation holds none
// until its first start gives it one, record.Store.AssignTaskBranch), and the steps of each task.
func nextPlan(stored *record.StoredPlan, version record.PlanVersion, tasks []record.PlanTask, steps []record.PlanStep) record.StoredPlan {
	next := record.StoredPlan{Version: version, Tasks: make([]record.PlanTask, 0, len(tasks)), Steps: map[string][]record.PlanStep{}}
	for _, task := range tasks {
		if at := slices.IndexFunc(stored.Tasks, func(row record.PlanTask) bool { return row.Task == task.Task }); at >= 0 {
			task.Branch = stored.Tasks[at].Branch
		}
		next.Tasks = append(next.Tasks, task)
	}
	for _, step := range steps {
		next.Steps[step.Task] = append(next.Steps[step.Task], step)
	}
	return next
}

// heldPlan is the plan in force of stored as its owner's blocks gave it, which a later version is
// compared with (plan.ChangedTasks): each task's block, with the steps its block names. Those are
// its stored steps, but for a block that named none (record.PlanTask.StepsByDefault), whose stored
// steps are the default steps it runs, which a block naming them would differ from.
func heldPlan(stored *record.StoredPlan) plan.Plan {
	held := plan.Plan{ArtifactID: stored.Version.ArtifactID, Version: stored.Version.Version}
	for _, task := range stored.Tasks {
		block := plan.Task{ID: task.Task, Needs: task.Needs, PartOf: task.PartOf, Owner: task.OwnerRole, Merges: task.Merges, Text: task.BlockText}
		if !task.StepsByDefault {
			block.Steps = namedSteps(stored.Steps[task.Task])
		}
		held.Tasks = append(held.Tasks, block)
	}
	return held
}

// givenRows is the rows of fact's version, given at now over stored, the plan in force (nil for
// none), of which changed are the tasks it changes, adds or removes. The version records the tasks
// it adds (addedTasks). Each task takes the version's block, and a task new to the generation
// waits at no act (PutPlan keeps where a task the generation holds stands). A task the version
// leaves as it was keeps its stored steps; any other task takes the steps its block names
// (StepsOf), or, for a block that names none, defaults, the project's default steps
// (Config.DefaultSteps), each with the type its role has in d, the engine's deployment
// (planSteps).
func givenRows(fact intake.PlanGiven, stored *record.StoredPlan, changed []string, defaults []plan.Step, d plan.Deployment, now time.Time) (record.PlanVersion, []record.PlanTask, []record.PlanStep) {
	version := record.PlanVersion{Issue: fact.Issue, Generation: fact.Generation, Version: fact.Plan.Version, ArtifactID: fact.Plan.ArtifactID,
		Source: record.PlanSourceOwner, GivenBy: fact.GivenBy, GivenAt: now, Added: addedTasks(stored, fact.Plan)}
	var tasks []record.PlanTask
	var steps []record.PlanStep
	for _, given := range fact.Plan.Tasks {
		tasks = append(tasks, record.PlanTask{Issue: fact.Issue, Generation: fact.Generation, Task: given.ID, PlanVersion: fact.Plan.Version,
			BlockText: given.Text, Needs: given.Needs, PartOf: given.PartOf, OwnerRole: given.Owner, Merges: given.Merges,
			StepsByDefault: !given.NamesSteps() && given.Owner == "", State: record.TaskWaiting})
		if stored != nil && !slices.Contains(changed, given.ID) {
			steps = append(steps, stored.Steps[given.ID]...)
			continue
		}
		steps = append(steps, planSteps(d, fact.Issue, fact.Generation, given.ID, plan.StepsOf(given, defaults))...)
	}
	return version, tasks, steps
}

// addedTasks is the id of each task of next that stored, the plan in force, does not hold, in
// next's order: every task of next where stored is nil, a generation's first version, and [] for a
// version that adds none, never nil. A task next changes or keeps is not added, and a task it
// removes is in no list.
func addedTasks(stored *record.StoredPlan, next plan.Plan) []string {
	added := []string{}
	for _, task := range next.Tasks {
		if stored == nil {
			added = append(added, task.ID)
			continue
		}
		if _, held := stored.TaskOf(task.ID); !held {
			added = append(added, task.ID)
		}
	}
	return added
}
