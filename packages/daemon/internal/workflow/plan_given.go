package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// planGiven is the plan an owner gave with register_plan, stored at the generation the route gave
// it at: the issue's own, or 0 for an issue the daemon had not recorded, whose plan waits there for
// the issue's first step. It has three parts, each its own function: the judgement of the version
// (versionRefusal), its write (writeVersion), and what the write starts on a recorded issue
// (afterVersion). Each refusal writes nothing. intake.ApplyFact runs this for every give, whatever
// its id's mark, so a give is judged against the store as this transaction reads it: a version
// refused while its task ran is taken when given again after stop_task, the owner's plan given
// again at the generation the record holds meets the rules of a later version there, and a version
// a first step passed over since it was taken is judged again. A give of the version that runs, or
// that the generation's first step will run (versionRefusal), is answered taken: it writes nothing,
// but adds its giver to the givers of that version where the version names another
// (record.Store.AddPlanGiver). So a second delivery of a give taken writes nothing.
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
	if given.taken {
		return intake.Result{}, e.addGiver(ctx, tx, fact, given.stored.Version)
	}
	if earlier := given.passedOver; earlier != nil {
		if err := e.store.DeletePlan(ctx, tx, fact.Issue, earlier.Generation); err != nil {
			return intake.Result{}, err
		}
		e.logOnCommit(ctx, "workflow: a plan of an earlier owner is set aside for a plan its owner gave", "issue", fact.Issue,
			"generation", earlier.Generation, "version", earlier.Version, "document", earlier.ArtifactID, "giver", giversOf(*earlier))
	}
	written, err := e.writeVersion(ctx, tx, fact, given)
	if err != nil || !given.recorded {
		return intake.Result{}, err
	}
	return intake.Result{}, e.afterVersion(ctx, tx, fact, given, written)
}

// addGiver adds fact's giver to the givers of held, the version in force that fact gives again
// (record.Store.AddPlanGiver), which writes nothing where held names that giver already: a give by
// an owner makes it a giver of the version, so the version is that owner's while it owns the issue
// (record.GivenByOwner), whatever becomes of the other givers. An add that changed the row is
// logged with the owner that gave it again (given_by) and the givers before it (giver, giversOf).
func (e *Engine) addGiver(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven, held record.PlanVersion) error {
	added, err := e.store.AddPlanGiver(ctx, tx, fact.Issue, fact.Generation, fact.Plan.Version, fact.GivenBy, e.now())
	if err != nil || !added {
		return err
	}
	e.logOnCommit(ctx, "workflow: an owner's plan is given again by another owner", "issue", fact.Issue, "generation", fact.Generation,
		"version", fact.Plan.Version, "document", fact.Plan.ArtifactID, "given_by", fact.GivenBy, "giver", giversOf(held))
	return nil
}

// givenVersion is what versionRefusal read of a version it judged: the issue the record holds (nil
// for one it does not), whether the version is of the issue's own generation (recorded, false for
// a plan waiting at generation 0), the plan in force at the fact's generation (nil for none, and
// for one passedOver), the tasks the version changes, adds or removes over it
// (plan.ChangedTasks), whether the version is the one in force (taken), which planGiven writes
// nothing of but its giver, and the version in force that only earlier owners gave, which
// planGiven deletes before it writes (passedOver, nil for none).
type givenVersion struct {
	issue      *record.Issue
	recorded   bool
	stored     *record.StoredPlan
	changed    []string
	taken      bool
	passedOver *record.PlanVersion
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
// Before the first step of the generation the version is given at, a version in force there that
// only earlier owners gave is one that step passes over (record.GivenByEarlierOwners, read through
// the engine's store as the first step reads it, passOverEarlierOwners), so the give is judged as
// the generation's first plan, and planGiven deletes that version before it writes (passedOver).
// The version in force as the judgement then reads it is the one the generation runs or the first
// step will run, and a give of that very version is answered taken (taken), whoever gave it
// before: it is no later version, and nothing of it is written but its giver (addGiver). An issue
// the daemon has not recorded has no tree to judge an earlier owner by, so its plan at generation
// 0 is judged as it stands, and its first step judges the givers once the issue is recorded.
//
// A later version is compared, task by task, with the version in force as its owner gave it
// (heldPlan, plan.ChangedTasks). A version of another document than the one in force is refused,
// but over the default steps, and so is a version below it (laterVersionRefusal). A task the
// version changes or removes must wait or be stopped: a running task is refused, one reason for
// each, and so is a task that is done or left the workflow. A plan given in a lingering tree is
// refused too, the version in force among them.
//
// A recorded issue whose group takes no plan from its owner (groupOf, takesNoPlan) is answered
// PLAN_NOT_TAKEN, right after the generation is judged, so no plan of it is stored, also one given
// at generation 0 for an issue the daemon recorded meanwhile. The group never changes, and the
// refusal writes nothing, so the same registration sent again gets the same answer, never the
// answer of a registration applied, which the route gives as the plan taken. An issue the daemon
// has not recorded has no draw yet; its plan waits at generation 0, and its first step passes it
// over where the issue's group takes none (planFirstStep).
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
	if given.stored != nil && issue != nil && beforeItsWork(*issue) {
		earlier, err := record.GivenByEarlierOwners(ctx, e.store, tx, given.stored.Version, *issue)
		if err != nil {
			return givenVersion{}, nil, err
		}
		if earlier {
			given.passedOver, given.stored = &given.stored.Version, nil
		}
	}
	if given.stored == nil && given.recorded && !beforeItsWork(*issue) {
		return givenVersion{}, intake.PlanRefused([]plan.Refusal{intake.StartedBeforePlans(issue.Key)}), nil
	}
	given.taken = given.stored != nil && sameVersion(given.stored.Version, fact.Plan)
	if given.stored != nil && !given.taken {
		if refusal := laterVersionRefusal(fact.Issue, given.stored.Version, fact.Plan); refusal != nil {
			return givenVersion{}, refusal, nil
		}
		given.changed = plan.ChangedTasks(heldPlan(given.stored), fact.Plan)
		var running []plan.Refusal
		for _, id := range given.changed {
			at := slices.IndexFunc(given.stored.Tasks, func(task record.PlanTask) bool { return task.Task == id })
			if at < 0 {
				continue
			}
			state := given.stored.Tasks[at].State
			if refusal := intake.FinishedTask(fact.Issue, id, state, intake.ChangeOfTask); refusal != nil {
				return givenVersion{}, refusal, nil
			}
			if state == record.TaskRunning {
				running = append(running, intake.RunningTaskChanged(id))
			}
		}
		if len(running) > 0 {
			return givenVersion{}, intake.PlanRefused(running), nil
		}
	}
	if issue != nil {
		lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
		if err != nil || lingers {
			return givenVersion{}, refusalUnless(err, refusedLingering(*issue, fmt.Sprintf("version %d of the owner's plan", fact.Plan.Version)).Refusal), err
		}
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

// writeVersion stores fact's version as the version in force (givenRows): each task it leaves as
// it was keeps its steps and where it stands, each task it changes takes the steps its block now
// names, and each task it removes is deleted. It answers what it read of the version before.
func (e *Engine) writeVersion(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven, given givenVersion) (writtenVersion, error) {
	version, tasks, steps := givenRows(fact, given.stored, given.changed, e.cfg.DefaultSteps, e.cfg.Deployment, e.now())
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

// afterVersion is what a version written on a recorded issue starts. Where it changes the stopped
// task of an issue held for that task, the stop ends: where the work of the stopped task ran, which
// the place it stood at says (record.TaskPlace.Started), the task the plan now holds starts again
// at its first step (restartTask), and its worker is told which version did it; a task stopped
// before its first step went out waits for that step again, as a task never stopped does
// (waitForFirstStep). A version taken after the work started writes the run's plan node again,
// whole (planNode): the start wrote it at the version then in force, and the node names the plan
// the run walks now. A version taken before the work starts writes none, since the start writes
// the version in force.
func (e *Engine) afterVersion(ctx context.Context, tx pgx.Tx, fact intake.PlanGiven, given givenVersion, written writtenVersion) error {
	issue := *given.issue
	ends := len(written.stopped) > 0 && given.stored != nil && record.HeldFor(issue, record.HoldTaskStopped)
	if !written.started && !ends {
		return nil
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	if written.started && recordsSteps(issue) {
		if err := e.enqueue(ctx, tx, issue.Key, planNode(issue, w)); err != nil {
			return err
		}
	}
	if !ends {
		return nil
	}
	reason := fmt.Sprintf("the owner gave version %d of the plan while task %s was stopped", fact.Plan.Version, strings.Join(written.stopped, ", "))
	if !written.started {
		return e.waitForFirstStep(ctx, tx, issue, w, reason)
	}
	return e.restartTask(ctx, tx, issue, w, reason)
}

// waitsAtZero says whether a plan given at generation 0 for issue, which the daemon recorded after
// the route read it unrecorded, still waits there for the issue's first step: the issue stands
// before that step (beforeItsWork), and its generation holds no plan the step keeps — none, or
// only one earlier owners gave (record.GivenByEarlierOwners), which the step passes over
// (passOverEarlierOwners) — so the step takes the newest plan its owner gave, or passes it over
// too for an issue whose group takes none (planFirstStep). Once a plan of the generation is the
// one its run walks, the default steps the step wrote or a plan of its owner's, the run went on
// without the one given at generation 0.
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
	return record.GivenByEarlierOwners(ctx, e.store, tx, stored.Version, issue)
}

// sameVersion says whether next is held, the version in force, as its owner gave it: an owner's
// version of the same document, at the same number. A give of it is no later version: it is the
// version that runs or that the first step will run, and versionRefusal answers it taken.
func sameVersion(held record.PlanVersion, next plan.Plan) bool {
	return held.Source == record.PlanSourceOwner && held.ArtifactID == next.ArtifactID && held.Version == next.Version
}

// laterVersionRefusal is the refusal of next, given for issue over held, the version in force:
// a version of another document (PlanDocumentChanged), but over the project's default steps,
// which no document wrote, and a version at or below held (PlanVersionOld), which versionRefusal
// asks only of a version that is not held itself (sameVersion). nil for a later version of the same
// document.
func laterVersionRefusal(issue string, held record.PlanVersion, next plan.Plan) *intake.Refusal {
	if held.Source != record.PlanSourceDefault && next.ArtifactID != held.ArtifactID {
		return intake.PlanDocumentChanged(issue, held.ArtifactID)
	}
	if next.Version <= held.Version {
		return intake.PlanVersionOld(issue, held.Version, next.Version)
	}
	return nil
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
// none), of which changed are the tasks it changes, adds or removes. Each task takes the
// version's block, and a task new to the generation waits at no act (PutPlan keeps where a task
// the generation holds stands). A task the version leaves as it was keeps its stored steps; any
// other task takes the steps its block names (StepsOf), or, for a block that names none, defaults,
// the project's default steps (Config.DefaultSteps), each with the type its role has in d, the
// engine's deployment (planSteps).
func givenRows(fact intake.PlanGiven, stored *record.StoredPlan, changed []string, defaults []plan.Step, d plan.Deployment, now time.Time) (record.PlanVersion, []record.PlanTask, []record.PlanStep) {
	version := record.PlanVersion{Issue: fact.Issue, Generation: fact.Generation, Version: fact.Plan.Version, ArtifactID: fact.Plan.ArtifactID,
		Source: record.PlanSourceOwner, GivenBy: fact.GivenBy, GivenAt: now}
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
