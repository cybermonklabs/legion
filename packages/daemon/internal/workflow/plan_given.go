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
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// planGiven is the plan an owner, or the sub-owner of one of its tasks, gave with register_plan,
// stored at the generation the route gave it at: the issue's own, or 0 for an issue the daemon had
// not recorded, whose plan waits there for the issue's first step. It has four parts, each its own
// function: the judgement of the version (versionRefusal), its write (writeVersion), what the
// write starts on a recorded issue (afterVersion), and the end of the issue where the version
// leaves only tasks that are done or left (leaveWhereDone). Each refusal writes nothing and leaves
// the fact unspent, so the same registration, whose id names the issue, the generation, the
// document and the version (api's registerPlan), applies when it is given again: a version refused
// while its task ran is taken after stop_task, and the owner's plan given again at the generation
// the record holds meets the rules of a later version there. A version taken answers the tasks it
// adds over the version in force that versionRefusal read (intake.Result.Added,
// record.PlanVersion.Added), which the route answers, and which the write records on the version,
// since a give of a version the store holds never reaches this apply (intake.ApplyFact) and is
// answered that, not what the version would add over the version in force then.
//
// Apply judged an architect's plan already on the record as the fact reads it, since the route
// read the record, or Dispatch, before the fact lock (requestRefusal): a giver that owns the issue
// no longer is refused ISSUE_OUTSIDE_TREE there, and a plan of a run the issue has left
// PLAN_STALE_GENERATION. A sub-owner's version is judged here (giverOf).
func (e *Engine) planGiven(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven) (intake.Result, error) {
	given, refusal, err := e.versionRefusal(ctx, tx, fact)
	if err != nil || refusal != nil {
		return intake.Result{Refusal: refusal, Unspent: refusal != nil}, err
	}
	written, err := e.writeVersion(ctx, tx, fact, given)
	if err == nil && given.recorded {
		err = e.afterVersion(ctx, tx, fact, given, written)
	}
	if err != nil {
		return intake.Result{}, err
	}
	if given.recorded {
		err = e.leaveWhereDone(ctx, tx, fact.Issue)
	}
	return intake.Result{Added: given.version.Added}, err
}

// leaveWhereDone ends the run of the issue keyed key where the version just taken left each of its
// tasks done or left the workflow (issuePhaseOfTasks rule 1): one that removes the last task that
// was not done, a stopped or a waiting one. The issue is written where its tasks put it, done, with
// the Dispatch status done (putScope), and leaves the workflow with the status done through the one
// place of that leave (leaveByTasks), as the close at a merge does: a root lingers its tree, and a
// child tells the architect above it. Its leave opens the check of each merged pull request of its
// run that has none, and a merge that opened its own check gets no second one. An issue that is
// done already, or whose tasks put it anywhere else, is left as it stands.
func (e *Engine) leaveWhereDone(ctx context.Context, tx pgx.Tx, key string) error {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil || issue.Phase == phase.Done {
		return err
	}
	scopes, err := e.scopes(ctx, tx, *issue)
	if err != nil {
		return err
	}
	var walks []Walk
	for _, s := range scopes {
		if !s.Planned() {
			return nil
		}
		walks = append(walks, s.walk)
	}
	if at, _ := issuePhaseOfTasks(walks); walks == nil || at != phase.Done {
		return nil
	}
	written, err := e.putScope(ctx, tx, &scopes[0])
	if err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a version left only tasks that are done, and the issue leaves the workflow", "issue", key, "tree", written.Tree,
		"status", written.Status)
	return e.leaveByTasks(ctx, tx, written)
}

// givenVersion is what versionRefusal read of a version it takes: the issue the record holds (nil
// for one it does not), whether the version is of the issue's own generation (recorded, false for
// a plan waiting at generation 0), who gave it (giverOf), the plan in force at the fact's
// generation (nil for none), the tasks the version changes, adds or removes over it
// (plan.ChangedTasks), and the rows the write stores (givenRows), each step named with its agent
// (record.NameAgents).
type givenVersion struct {
	issue    *record.Issue
	recorded bool
	giver    plan.Giver
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
// A later version is compared, task by task, with the version in force as it was given
// (heldPlan, plan.ChangedTasks). A version of another document than the one in force is refused,
// but over the default steps, and so is a version at or below it (laterVersionRefusal). Each task
// the version changes, adds or removes must be its giver's (giverOf, plan.OwnershipRefusals),
// before any rule of the task's state: the owner of the issue changes the tasks of the top level,
// and the sub-owner of a task changes its parts. A first version is held to it over no plan, so
// the owner's first version that writes a part is refused as a later one is. A task the version
// changes or removes that is done or left the workflow is refused first (intake.FinishedTask). A
// sub-owner's version is then refused while its task stands at its report, its retro or its merge
// (partsPlaceRefusal). A task the version changes or removes must wait or be stopped: a running
// task is refused, and so is a held task, one reason for each. A version that changes whether the
// work of a task it leaves as it is merges by itself is refused where that task stands at its
// retro or its merge, and wherever that task stands where a sub-owner gave it and does not write
// that task (mergeChangeRefusals). Such a version moves no base: a task that has started keeps the
// base its start fixed (record.PlanTask.Base, BaseOf), and only the start of its merge moves it. A
// plan given in a lingering tree is refused too.
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
	giver, refusal := giverOf(fact, issue, given.stored)
	if refusal != nil {
		return givenVersion{}, refusal, nil
	}
	given.giver = giver
	if given.stored == nil && given.recorded && !beforeItsWork(*issue) {
		return givenVersion{}, intake.PlanRefused([]plan.Refusal{intake.StartedBeforePlans(issue.Key)}), nil
	}
	// A first version is held to the tasks its giver writes as a later one is, over no plan, so each
	// of its tasks is a change. given.changed keeps the changes over a plan in force alone, as the
	// rules of a later version and the write's log line read them, so a first version names none.
	held := plan.Plan{}
	if given.stored != nil {
		if refusal := laterVersionRefusal(fact.Issue, given.stored.Version, fact.Plan); refusal != nil {
			return givenVersion{}, refusal, nil
		}
		held = heldPlan(given.stored)
	}
	changed := plan.ChangedTasks(held, fact.Plan)
	if given.stored != nil {
		given.changed = changed
	}
	if refused := plan.OwnershipRefusals(held, fact.Plan, changed, giver); len(refused) > 0 {
		return givenVersion{}, intake.PlanRefused(refused), nil
	}
	given.version, given.tasks, given.steps = givenRows(fact, given.stored, given.changed, e.cfg.DefaultSteps, e.cfg.Deployment, e.now())
	if given.stored != nil {
		// A done or left task the version changes or removes is refused first, for good: a part at
		// its task's report, retro or merge is done, so a version that changes one meets this
		// refusal before the rule of where its task stands.
		for _, id := range given.changed {
			if task, ok := given.stored.TaskOf(id); ok {
				if refusal := intake.FinishedTask(fact.Issue, id, task.State, intake.ChangeOfTask); refusal != nil {
					return givenVersion{}, refusal, nil
				}
			}
		}
		if refusal := partsPlaceRefusal(fact.Issue, given.stored, giver); refusal != nil {
			return givenVersion{}, intake.PlanRefused([]plan.Refusal{*refusal}), nil
		}
		var refused []plan.Refusal
		for _, id := range given.changed {
			task, ok := given.stored.TaskOf(id)
			switch {
			case !ok:
			case task.State == record.TaskRunning:
				refused = append(refused, intake.RunningTaskChanged(id))
			case task.State == record.TaskHeld:
				refused = append(refused, intake.HeldTaskChanged(id, taskPlaceWords(given.stored, task)))
			}
		}
		refused = append(refused, mergeChangeRefusals(*given.stored, nextPlan(given.stored, given.version, given.tasks, given.steps), given.changed, giver)...)
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
	given.steps = record.NameAgents(earlier, given.steps)
	if unnamed := record.UnnamedAgents(given.steps); len(unnamed) > 0 {
		reasons := make([]plan.Refusal, len(unnamed))
		for i, step := range unnamed {
			reasons[i] = intake.AgentUnnamed(step.Task, step.Position, string(record.RoleName(step.Role)))
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

// giverOf is who gives fact's version (plan.Giver), read in the fact's transaction from the claim
// that gave it and stored, the plan in force at the fact's generation (nil for none). An
// architect's version is the owner's, "", whose claim requestRefusal judged. A sub-owner's
// (intake.PlanGiven.SubOwner) is the task of stored that names a sub-owner and whose sub-owner's
// agent (plan.SubOwnerAgent) is the agent of the claim that gave it; the route held that claim to
// fact's issue (SUB_OWNER_OUTSIDE_ISSUE), a claim's issue never changes, and plan.Validate holds
// each sub-owner's agent to a name of its own. requestRefusal judges no sub-owner, so its version of
// a run the issue has left is refused PLAN_STALE_GENERATION here, and one whose claim is the
// sub-owner of no task of stored intake.NoSubOwnedTask. A sub-owner of a task that is done, or that
// left the workflow with its issue, is refused TASK_DONE, naming the task and where it stands and
// sending the missing work to the owner of the issue (intake.FinishedTask, intake.ChangeOfParts):
// no part it gives could start, since a part starts only while its task runs at its parts. A
// sub-owner starts only once its task runs, so its route gives its version at the generation of the
// recorded issue.
func giverOf(fact intake.PlanGiven, issue *record.Issue, stored *record.StoredPlan) (plan.Giver, *intake.Refusal) {
	agent, ok := fact.SubOwner()
	if !ok {
		return "", nil
	}
	if issue != nil && fact.Generation != issue.Generation {
		return "", intake.PlanStaleGeneration(fact.Issue, fact.Generation, issue.Generation)
	}
	if issue != nil && stored != nil {
		for _, task := range stored.Tasks {
			if task.OwnerRole == "" || plan.SubOwnerAgent(task.Task) != agent {
				continue
			}
			if refusal := intake.FinishedTask(fact.Issue, task.Task, task.State, intake.ChangeOfParts); refusal != nil {
				return "", refusal
			}
			return plan.Giver(task.Task), nil
		}
	}
	return "", intake.PlanRefused([]plan.Refusal{intake.NoSubOwnedTask(fact.Issue, agent)})
}

// partsPlaceRefusal is the reason a version that giver, the task whose sub-owner gave it ("" for
// the owner's), gives over stored, the plan in force, is refused for where that task stands, and
// nil where it is taken. A sub-owner gives a version while its task waits at no act, stands at its
// parts, running or held, or is stopped: a part it adds starts once its task runs at its parts with
// no wait (mayStart), which a start, a release and the end of a join each reach. At its report, its
// retro or its merge, running or held, the task moves forward only, so a part given then would never
// start: the version is refused, naming the task and where it stands (intake.PartsOfReportedTask,
// intake.PartsOfTaskAtAct). A done or left task is giverOf's to refuse. The owner's version meets
// no such rule.
func partsPlaceRefusal(issue string, stored *record.StoredPlan, giver plan.Giver) *plan.Refusal {
	if giver == "" {
		return nil
	}
	task, ok := stored.TaskOf(string(giver))
	if !ok || task.State != record.TaskRunning && task.State != record.TaskHeld {
		return nil
	}
	var refusal plan.Refusal
	switch task.Place.Kind {
	case record.PlaceReported:
		refusal = intake.PartsOfReportedTask(issue, task.Task)
	case record.PlaceRetro, record.PlaceMerge:
		refusal = intake.PartsOfTaskAtAct(issue, task.Task, task.Place.Kind)
	default:
		return nil
	}
	return &refusal
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
	prs, err := e.store.TaskPullRequestRows(ctx, tx, fact.Issue)
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
// did it and who gave it (stoppedReason); before the work starts it waits for its first step, as a
// task never stopped does (waitForFirstStep). Then, once the work started, each task that may start
// starts (startReadyNow, once for the version), a task the version adds among them. Before the work
// starts nothing starts: the gate's opening writes the node and starts each task whose needs are
// done (gateOpened).
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
		reason := stoppedReason(given.giver, fact.Plan.Version, ended.stopped...)
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
	return e.startReadyNow(ctx, tx, issue.Key)
}

// stoppedReason is the reason a stopped task's worker is told when version of the plan, given by
// giver, ends the stop of tasks: it names the giver as it is, the owner of the issue or the
// sub-owner of a task.
func stoppedReason(giver plan.Giver, version int, tasks ...string) string {
	by := "the owner"
	if giver != "" {
		by = "the sub-owner of task " + string(giver)
	}
	return fmt.Sprintf("%s gave version %d of the plan while task %s was stopped", by, version, strings.Join(tasks, ", "))
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
// none (planFirstStep). Whose plan is an earlier owner's is the store's one rule (record.Store's
// PlansOfEarlierOwners), which judges a sub-owner's version as its owner's, as the step does. Once
// a plan of the generation is the one its run walks, the default steps the step wrote or a plan of
// its owner's, the run went on without the one given at generation 0. versionRefusal reads the
// group only after this judgement, so a plan given at generation 0 for an issue whose group takes
// none is answered PLAN_STALE_GENERATION once its run went on, and PLAN_NOT_TAKEN while it still
// waits.
func (e *Engine) waitsAtZero(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	if !beforeItsWork(issue) {
		return false, nil
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return err == nil, err
	}
	// PlansOfEarlierOwners answers the version in force of each generation, so the generation
	// names the one stored holds.
	earlier, err := e.store.PlansOfEarlierOwners(ctx, tx, issue)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(earlier, func(v record.PlanVersion) bool { return v.Generation == issue.Generation }), nil
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

// actWords is where a task stands at kind, a place that is no step, as a text to an agent names it:
// its parts or its report, where its sub-owner acts, its retro or its merge; "" for a step and for
// no act. It is the one name of such a place in a refusal (taskPlaceWords) and in the held notice
// of a failed claim (heldTaskSummary), so neither names a step a task does not stand at.
func actWords(kind string) string {
	switch kind {
	case record.PlaceParts:
		return "its parts"
	case record.PlaceReported:
		return "its report"
	case record.PlaceRetro:
		return "its retro"
	case record.PlaceMerge:
		return "its merge"
	}
	return ""
}

// taskPlaceWords is where task of stored stands, as a refusal names it (intake.HeldTaskChanged,
// taskMergingAt): `step <i> (<role>)`, the place of an act (actWords), or no act for a task that has
// not started.
func taskPlaceWords(stored *record.StoredPlan, task record.PlanTask) string {
	if words := actWords(task.Place.Kind); words != "" {
		return words
	}
	if task.Place.Kind == "" {
		return "no act"
	}
	role := ""
	if steps := stored.Steps[task.Task]; task.Place.Position >= 1 && task.Place.Position <= len(steps) {
		role = steps[task.Place.Position-1].Role
	}
	return fmt.Sprintf("step %d (%s)", task.Place.Position, role)
}

// mergeChangeRefusals is one refusal for each task of before, the plan in force, that the version
// giver gave ("" for the owner's) leaves as it is and whose work after, the plan the version makes
// (nextPlan), merges by itself where before's does not, or the other way
// (record.StoredPlan.Merging): the rule each walk reads (walksOf), which reads every task's needs,
// a part's among them. A sub-owner acts only inside the parts of its task, and which tasks merge is
// the owner's to decide, so a sub-owner's version that changes whether a task it does not write
// merges is refused wherever that task stands (intake.MergeOfOwnersTask): a part's need of a task
// of the top level is taken where it changes no task's merging. For the owner's version, where the
// task stands decides. A task that waits or is stopped, and a task running or held at a step, its
// last included, take the version: its line goes on to its retro and its merge, or ends at its last
// step, by the answer in force when that step passes (plan.Line). A task running or held at its
// retro or its merge is refused (intake.MergeOfStartedActChanged): that act has started, or a hold
// keeps it back, and neither a retro whose task would no longer merge nor a task past its last step
// that would now merge can be carried out. A done task whose work merged
// (record.StoredPlan.WorkMerged, the fact its merge wrote, which this version does not change, as it
// changes Merging) takes the version, and nothing of it runs again. A done task whose work did not
// merge, which after leaves needed by no task, is refused for good (intake.DoneTaskWouldMerge): its
// work would merge by itself, and a done task does not start again. A task the version changes or
// removes (changed) is left to the refusal that names it already (intake.RunningTaskChanged,
// intake.HeldTaskChanged, intake.FinishedTask), so no task is named twice.
//
// Of the other answers a version could change for a task its giver does not write, none reads
// another task's blocks but through Merging: the base the plan gives a task
// (record.StoredPlan.PlannedBase) reads Merging and the task's own needs, steps and task above it,
// and the start of a task (mayStart) reads its own needs and the place of its own task.
func mergeChangeRefusals(before, after record.StoredPlan, changed []string, giver plan.Giver) []plan.Refusal {
	was, will := before.Merging(), after.Merging()
	var refused []plan.Refusal
	for _, task := range before.Tasks {
		if slices.Contains(changed, task.Task) || slices.Contains(was, task.Task) == slices.Contains(will, task.Task) {
			continue
		}
		atAct := task.Place.Kind == record.PlaceRetro || task.Place.Kind == record.PlaceMerge
		switch {
		case giver != "" && task.PartOf != string(giver):
			refused = append(refused, intake.MergeOfOwnersTask(before.Version.Issue, task.Task, string(giver), slices.Contains(was, task.Task)))
		case atAct && (task.State == record.TaskRunning || task.State == record.TaskHeld):
			refused = append(refused, intake.MergeOfStartedActChanged(task.Task, task.Place.Kind, task.State == record.TaskHeld))
		case task.State == record.TaskDone && !before.WorkMerged(task.Task) && slices.Contains(will, task.Task):
			refused = append(refused, intake.DoneTaskWouldMerge(task.Task))
		}
	}
	return refused
}

// nextPlan is the plan stored becomes once the rows of a version given over it (givenRows) are
// written (record.Store.PutPlan): version, each of tasks, and the steps of each task. A task the
// generation holds already takes its block's columns from tasks and keeps the rest of its row as
// stored holds it, as the write keeps it: where it stands, its branch, its counts and its waits. A
// task new to the generation waits at no act and holds no branch until its first start gives it
// one (record.Store.AssignTaskBranch).
func nextPlan(stored *record.StoredPlan, version record.PlanVersion, tasks []record.PlanTask, steps []record.PlanStep) record.StoredPlan {
	next := record.StoredPlan{Version: version, Tasks: make([]record.PlanTask, 0, len(tasks)), Steps: map[string][]record.PlanStep{}}
	for _, task := range tasks {
		if kept, ok := stored.TaskOf(task.Task); ok {
			kept.PlanVersion, kept.BlockText, kept.Needs, kept.PartOf, kept.OwnerRole, kept.Merges, kept.StepsByDefault =
				task.PlanVersion, task.BlockText, task.Needs, task.PartOf, task.OwnerRole, task.Merges, task.StepsByDefault
			task = kept
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
