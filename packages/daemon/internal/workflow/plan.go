package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// planRecordNamespace is the namespace of every plan record's id.
var planRecordNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("legion-plan-record"))

// PlanID is the id of the plan one run of an issue carries out: a version 5 uuid of the issue and
// its run (record.Issue.Generation), so a write of the plan sent again lands on the same plan and
// the next run writes a plan of its own.
func PlanID(issue string, generation uint64) string {
	return uuid.NewSHA1(planRecordNamespace, []byte(fmt.Sprintf("%s/%d", issue, generation))).String()
}

// planNode is the plan node of issue's run: the plan w walks, as Dispatch records it under
// the run's one id (PlanID). Its source is the version in force's (record.PlanVersion.Source):
// owner, with the document and the version of it Legion took, or default, the project's default
// steps, with neither. Its task is the run's one task, with its steps in order, each a role or
// `role:key` (plan.Step.String). The node is written at the start of the work and again, whole, at
// each later version Legion takes (planGiven): Dispatch replaces all four fields for a version
// above the one it holds. Both read w with its plan rows: the start's first step writes them
// (planFirstStep), and a version taken is one. A run with no plan rows walks today's line, so its
// node is the task default of todaysSteps: a hand-over's carry of an orphan that ran before plans
// (WritePlan).
func planNode(issue record.Issue, w Walk) record.PlanRecord {
	node := record.PlanRecord{ID: PlanID(issue.Key, issue.Generation), Generation: issue.Generation, Source: record.PlanSourceDefault}
	if w.Version.Source == record.PlanSourceOwner {
		node.Source, node.PlanDocument = record.PlanSourceOwner, &record.PlanDocument{ArtifactID: w.Version.ArtifactID, Version: w.Version.Version}
	}
	node.Tasks = []record.PlanRecordTask{{ID: w.Task, Steps: plan.Words(namedSteps(w.Steps))}}
	return node
}

// passOverEarlierOwners deletes each plan of issue that an earlier owner gave, before its first
// step reads its plan, so the walk never takes one, and answers the versions it
// passed over. An earlier owner's plan is the version in force of a generation whose giver is no
// architect of the issue's tree now (record.Store's PlansOfEarlierOwners, record.ArchitectOfTree,
// the rule the routes hold every architect's call to): the owner of an issue changes where an
// orphan is admitted again as a root of its own and where an adoption moves a member to another
// tree, and the plan the earlier owner gave is not the new owner's to run. The rows go in this
// transaction, so the next first step finds none. An issue with no plan rows, or only its
// owners', is read and nothing is written.
func (e *Engine) passOverEarlierOwners(ctx context.Context, tx pgx.Tx, issue record.Issue) ([]record.PlanVersion, error) {
	earlier, err := e.store.PlansOfEarlierOwners(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	for _, version := range earlier {
		if err := e.store.DeletePlan(ctx, tx, issue.Key, version.Generation); err != nil {
			return nil, err
		}
	}
	return earlier, nil
}

// groupOf is the group of issue: the row of its experiment assignments that says how it is done
// (experiment.GroupOf), or the zero row, an issue of no group. The row was written at the issue's
// draw, once, so the group is the one the issue was drawn into whatever the project's file says
// now, and no reader of the workflow asks that file for it.
func (e *Engine) groupOf(ctx context.Context, tx pgx.Tx, issue string) (record.Assignment, error) {
	rows, err := e.store.AssignmentsFor(ctx, tx, []string{issue})
	if err != nil {
		return record.Assignment{}, err
	}
	return experiment.GroupOf(rows[issue]), nil
}

// takesNoPlan says whether group, an issue's row of groupOf, takes no plan from the issue's owner:
// the arm runs the project's default steps. It is the one answer the three readers of the group
// read — the first step (planFirstStep), register_plan (planGiven) and stop_task (taskStopped) —
// and it holds only the word this release carries out: the loader refuses `owner-alone`
// (config's readVariants), so no draw of this release writes it.
func takesNoPlan(group record.Assignment) bool {
	return config.PlanGroup(group.Plan) == config.PlanGroupDefault
}

// planFirstStep settles the plan the first step of issue's run walks, in the transaction that
// starts it (gateOpened) and before the transition reads it (walk), so each run that starts its
// work in this release has plan rows. A plan an earlier owner gave is passed over first
// (passOverEarlierOwners), so nothing below carries one. A run whose generation then holds plan
// rows, the owner's own or a hand-over's carried plan, keeps them. One whose generation holds none
// takes the newest plan its owner gave, of any generation, generation 0 among them, which holds a
// plan given before the daemon recorded the issue (record.Store's OwnersNewestPlan), carried from
// its start (CarryPlan without carried): each task waits at no act, and the transition starts the
// first step of its line. Where the owner gave none, the run takes the project's default steps
// (defaultRows). Nothing is passed over in silence, and the plan the run takes is the one the
// telling names (tellPassedOver). Every plan_steps row of the issue is read first, before anything
// is passed over or written, so the default steps are named from the rows that show which names a
// key held, as a version an owner gives is (record.NameAgents).
//
// An issue whose group takes no plan from its owner (groupOf, takesNoPlan) takes the default
// steps: the owner's plans are passed over too (passOverOwners), and the one notice that tells
// what it passed over names the group (tellNotTaken).
func (e *Engine) planFirstStep(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	steps, err := e.store.PlanStepsOf(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	group, err := e.groupOf(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	earlier, err := e.passOverEarlierOwners(ctx, tx, issue)
	if err != nil {
		return err
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return err
	}
	if takesNoPlan(group) {
		owners, stored, err := e.passOverOwners(ctx, tx, issue, stored)
		if err != nil {
			return err
		}
		// The group's one notice tells the earlier owners' versions with its owner's, so the plans of
		// earlier owners go to tellNotTaken and none to tellPassedOver, whose text offers register_plan.
		if err := e.takeFirstPlan(ctx, tx, issue, stored, nil, steps); err != nil {
			return err
		}
		return e.tellNotTaken(ctx, tx, issue, group, append(earlier, owners...))
	}
	if stored == nil {
		if stored, err = e.store.OwnersNewestPlan(ctx, tx, issue); err != nil {
			return err
		}
	}
	return e.takeFirstPlan(ctx, tx, issue, stored, earlier, steps)
}

// takeFirstPlan writes stored, the plan the first step of issue's run walks, at the run's
// generation, and tells what passed over earlier (tellPassedOver): the project's default steps for
// none (defaultRows), named from steps, every plan_steps row of the issue as planFirstStep read
// them, a plan of another generation carried from its start (CarryPlan without carried), and the
// generation's own rows kept as they are. planFirstStep and the stop of a task before that step
// (taskStopped) take the first plan through it.
func (e *Engine) takeFirstPlan(ctx context.Context, tx pgx.Tx, issue record.Issue, stored *record.StoredPlan, earlier []record.PlanVersion,
	steps []record.PlanStep,
) error {
	switch {
	case stored == nil:
		held, err := e.store.KeyNames(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		claims, err := e.nameClaims(ctx, tx, issue)
		if err != nil {
			return err
		}
		version, tasks, named := e.defaultRows(issue, steps, held, claims)
		if err := e.store.PutPlan(ctx, tx, version, tasks, named); err != nil {
			return err
		}
		stored = &record.StoredPlan{Version: version}
	case stored.Version.Generation != issue.Generation:
		if err := e.store.CarryPlan(ctx, tx, issue.Key, stored.Version.Generation, issue.Generation, false); err != nil {
			return err
		}
	}
	return e.tellPassedOver(ctx, tx, issue, earlier, stored.Version)
}

// passOverOwners deletes every plan of issue's owner that its first step could take, for an issue
// whose group runs the project's default steps, and answers the versions it passed over, in the
// order they were given, and the plan of the run's generation that stands after it (stored, as the
// caller read it): the owner's plan in force at each generation (record.Store's PlansOfOwners), the
// run's own, generation 0, which holds a plan given before the daemon recorded and drew the issue,
// and each copy a carry wrote to a later generation. A run that passed over the newest alone would
// leave the copies before it to the next run's first step, which would tell the same version again.
// A plan of the run's generation that no owner gave, a hand-over's carried default, stands. The
// rows go in this transaction.
func (e *Engine) passOverOwners(ctx context.Context, tx pgx.Tx, issue record.Issue, stored *record.StoredPlan) ([]record.PlanVersion, *record.StoredPlan, error) {
	passed, err := e.store.PlansOfOwners(ctx, tx, issue)
	if err != nil {
		return nil, nil, err
	}
	for _, version := range passed {
		if err := e.store.DeletePlan(ctx, tx, issue.Key, version.Generation); err != nil {
			return nil, nil, err
		}
		if stored != nil && version.Generation == stored.Version.Generation {
			stored = nil
		}
	}
	return passed, stored, nil
}

// defaultRows is the plan of issue's run whose owner gave none, or whose experiment's group takes
// no plan from its owner (passOverOwners): version 0 of the source default, given now, with no
// document and no giver, and one task, default, whose block names no steps and so runs the
// project's default steps (Config.DefaultSteps), waiting at no act. Its agents are named as a given
// version's are (record.NameAgents): from earlier, every plan_steps row of the issue, held, the
// lasting record of the names its keys held (record.KeyNames), and claims, the issue's stored
// claims by name (nameClaims), so no agent of the default steps takes the name of a key an earlier
// plan gave, whatever release wrote the rows that show it, nor a name whose claim holds another
// role.
func (e *Engine) defaultRows(issue record.Issue, earlier []record.PlanStep, held []record.KeyName, claims record.NameClaims) (record.PlanVersion, []record.PlanTask, []record.PlanStep) {
	version := record.PlanVersion{Issue: issue.Key, Generation: issue.Generation, Source: record.PlanSourceDefault, GivenAt: e.now()}
	task := record.PlanTask{Issue: issue.Key, Generation: issue.Generation, Task: defaultTask, BlockText: "The project's default steps.",
		StepsByDefault: true, State: record.TaskWaiting}
	return version, []record.PlanTask{task}, record.NameAgents(earlier, held, claims, planSteps(e.cfg.Deployment, issue.Key, issue.Generation, defaultTask, e.cfg.DefaultSteps))
}

// planSteps is steps as the rows of the steps of task of issue's plan at generation: each at its
// place from 1, with its agent key and the type its role has in d, the engine's deployment
// (Config.Deployment), so a role the deployment defines is stored with its own type. It is the
// one writer of a stored step's type, which the step record names and the return of a failed
// step, the count of a review round and the commit of a review step read.
func planSteps(d plan.Deployment, issue string, generation uint64, task string, steps []plan.Step) []record.PlanStep {
	rows := make([]record.PlanStep, len(steps))
	for i, step := range steps {
		rows[i] = record.PlanStep{Issue: issue, Generation: generation, Task: task, Position: i + 1, Role: step.Role,
			Type: d.Roles[step.Role].Type, AgentKey: step.Key}
	}
	return rows
}

// tellPassedOver says what the first step of issue passed over, the plans of earlier owners
// (passOverEarlierOwners): one log line for each copy it deleted, with its generation, naming the
// plan the first step runs (runs, the version in force of its generation: the owner's, or the
// default steps), and one plan-not-taken notice to the issue's architect, which names each version
// once (namedOnce). Nothing passed over tells nothing.
func (e *Engine) tellPassedOver(ctx context.Context, tx pgx.Tx, issue record.Issue, earlier []record.PlanVersion, runs record.PlanVersion) error {
	if len(earlier) == 0 {
		return nil
	}
	ran, told := "default steps", "the default steps"
	if runs.Source == record.PlanSourceOwner {
		ran = describeVersion(runs)
		told = ran
	}
	for _, version := range earlier {
		e.logOnCommit(ctx, "workflow: a plan of an earlier owner is not taken", "issue", issue.Key, "generation", version.Generation,
			"version", version.Version, "document", version.ArtifactID, "giver", version.GivenBy, "runs", ran)
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
		"The first step of %s took no plan of an earlier owner: %s. It runs %s. To run a plan it did not take, give it again with register_plan as your own.",
		issue.Key, namedOnce(earlier), told)})
}

// tellNotTaken says what the first step of issue, whose group runs the project's default steps
// (group), passed over: every plan of an earlier owner (passOverEarlierOwners) and of its owner
// (passOverOwners), in that order. One log line for each copy it deleted, with its generation, and
// one plan-not-taken notice to the issue's architect, which names each version once (namedOnce)
// and the group, so the owner learns why no plan of its own is taken. Unlike the notice of an
// earlier owner's plan alone (tellPassedOver), it offers no register_plan: no plan given for this
// issue is taken. Nothing passed over tells nothing.
func (e *Engine) tellNotTaken(ctx context.Context, tx pgx.Tx, issue record.Issue, group record.Assignment, passed []record.PlanVersion) error {
	if len(passed) == 0 {
		return nil
	}
	for _, version := range passed {
		e.logOnCommit(ctx, "workflow: a plan is not taken: the issue's group runs the project's default steps", "issue", issue.Key,
			"generation", version.Generation, "version", version.Version, "document", version.ArtifactID, "giver", version.GivenBy,
			"experiment", group.Experiment, "variant", group.Variant)
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "plan-not-taken", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
		"The first step of %s took no plan: %s. %s", issue.Key, namedOnce(passed), intake.GroupClause(issue.Key, group))})
}

// namedOnce names the versions a first step passed over as its plan-not-taken notice names them:
// each version once (describeVersion), in the order it was first passed over. A rerun carries the
// owner's plan to its new generation with the time it was given (record.Store's CarryPlan), so the
// versions passed over hold one copy of a version for each generation it ran, and the notice names
// the version, not its copies.
func namedOnce(versions []record.PlanVersion) string {
	named := make([]string, 0, len(versions))
	for _, version := range versions {
		if described := describeVersion(version); !slices.Contains(named, described) {
			named = append(named, described)
		}
	}
	return strings.Join(named, "; ")
}

// describeVersion names an owner's version of a plan as a log line and a notice name it: its
// version, its document and who gave it.
func describeVersion(version record.PlanVersion) string {
	return fmt.Sprintf("version %d of document %s, given by %s", version.Version, version.ArtifactID, version.GivenBy)
}

// WritePlan queues the plan node of issue's run (planNode), the plan it carries out, read from its
// walk, where the run's work starts: at the run's move out of admitted (transition's
// TriggerGateOpened, which queues it on the walk it read, writePlanOn), after the first step has
// settled the plan (planFirstStep), and at a hand-over's carry, which re-admits an orphan at a new
// generation in the phase it ran in elsewhere (admit's readmit), so that run never leaves admitted,
// after the carry has written its plan rows (record.Store.CarryPlan). A record key that is no
// issue key writes nothing (recordsSteps), and reads no walk.
func (e *Engine) WritePlan(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	if !recordsSteps(issue) {
		return nil
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	return e.writePlanOn(ctx, tx, issue, w)
}

// writePlanOn is WritePlan on w, the walk of issue's run as its caller read it.
func (e *Engine) writePlanOn(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) error {
	if !recordsSteps(issue) {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, planNode(issue, w))
}

// rateRetro writes the rating the retro of issue's run gave the plan onto that retro's record, in
// the transaction of the completion that ended the retro: a step record row whose id is the id of
// the retro's latest run (StepID, from the count of its starts, which the retro's end left as it
// was), at the retro's place of the run's walk (stepAt: its task's retro for a run with plan rows,
// the task default's for one with none), carrying every identity field the run's start and end
// carry, the agent its start named among them, so the write makes the record where neither has
// landed yet, and the rating and its reason. It carries no moment and no result, which are the
// end's. Dispatch sets each field once, so the end and the rating merge in either order. The
// retro's step comes from stepAt, which every writer of a step's record takes, so a record key that
// is no issue key writes nothing.
func (e *Engine) rateRetro(ctx context.Context, tx pgx.Tx, issue record.Issue, rating record.PlanRating) error {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	step, recorded := stepAt(w, issue, placeNamed(record.TaskPlace{Kind: record.PlaceRetro}, phase.Retro))
	if !recorded {
		return nil
	}
	start, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
	if err != nil {
		return err
	}
	write := stepWrite(issue, step.place, step.role, step.typ, start.Agent, start.Starts)
	write.PlanRating = &rating
	return e.enqueue(ctx, tx, issue.Key, write)
}

// planHold says whether issue is held by its plan rather than by a failure: its owner stopped its
// task (record.HoldTaskStopped), or, as a parent whose last step is not the owner's acceptance, it waits for
// the issues beneath it in place of its retro (record.HoldForChildren). No path that resumes the
// work of a held issue resumes such a hold: a later version of the plan or a person's todo on a
// root ends a task's stop (restartTask, or waitForFirstStep for a task stopped before its first
// step), and the parent's retro starts once nothing beneath it is open (releaseHeldParents). stored says whether the
// issue's run has plan rows: no plan holds a run that has none, so a hold of either reason on such
// a run, which the roll-forward of a way back that left its reason finds, is a plain hold that a
// retry releases.
func planHold(issue record.Issue, stored bool) bool {
	return stored && (record.HeldFor(issue, record.HoldTaskStopped) || record.HeldForChildren(issue))
}

// planHoldRefusal is the answer to a release of issue's hold — the architect's retry or
// escalation (retryOrEscalate), the operator's restart (restartPhase) — where its plan holds it
// (planHold), and nil for any other hold. A retry would start the role of the phase the hold is
// from and an escalation would overwrite the reason, so each is refused with what starts the work
// again: for a stopped task a later version of its plan (RetryOfStoppedTask), for a parent held
// for its children why it still waits (heldParentWait's refusal).
func (e *Engine) planHoldRefusal(ctx context.Context, tx pgx.Tx, issue record.Issue) (*intake.Refusal, error) {
	if issue.Phase != phase.Held || issue.Hold == nil {
		return nil, nil
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil || !planHold(issue, w.Stored) {
		return nil, err
	}
	if record.HeldForChildren(issue) {
		wait, err := e.heldParentWaitOf(ctx, tx, issue)
		if err != nil {
			return nil, err
		}
		return wait.refusal(), nil
	}
	return intake.RetryOfStoppedTask(issue.Key, w.Task, w.StoppedReason), nil
}

// pastReport is the run that a completion of a phase its issue no longer stands in reports, which
// pastReportOf decides once: a run of a phase the daemon moved the issue on from, the run of the
// step that the stop of its task ended, or no run that handoff answers.
type pastReport int

const (
	// noRunReported is a completion of no run: handoff refuses it as one of a phase the issue
	// does not stand in (NotCurrentPhase).
	noRunReported pastReport = iota
	// leftPhaseReported is the first report of a run of a phase the daemon moved the issue on from
	// without it (leftItsPhase), whatever event moved it, which handoff records on the role's row
	// and writes onto the ended step (reportEndedStep), moving nothing.
	leftPhaseReported
	// stoppedStepReported is a completion of the phase the issue is held from because its task
	// was stopped there (record.HoldTaskStopped).
	stoppedStepReported
)

// pastReportOf is the run that role's completion of reported, a phase issue no longer stands in,
// reports. A held issue stands where no role works, so no role has left its phase there
// (leftItsPhase), but a hold of an owner's plan, which stands where its issue stands: a parent held
// for its children at its retro, and a stopped task at the step the stop ended, whose report of
// that step's run is the run the stop ended and is no left phase's, so a run of a left phase is
// never also that run. w is the walk of issue's run and row the role's row of the run the
// completion reports (Walk.completionRow).
func (e *Engine) pastReportOf(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fact intake.HandoffComplete, reported phase.Phase,
	row record.PhaseRow) (pastReport, error) {
	left, err := e.leftItsPhase(ctx, tx, issue, w, fact.Role, fact.Claim, reported, row)
	switch {
	case err != nil:
		return noRunReported, err
	case left:
		return leftPhaseReported, nil
	case record.HeldFor(issue, record.HoldTaskStopped) && issue.Hold.From == reported && planHold(issue, w.Stored):
		return stoppedStepReported, nil
	}
	return noRunReported, nil
}

// lateReport is the refusal of fact, role's report of reported, a phase issue no longer stands in,
// where issue's run has plan rows and a stop of its task ended the run the report is of. past is
// the run the report is of (pastReportOf), and row the role's row of that run
// (Walk.completionRow). The report of the phase a stop holds the issue from is late where it is of
// the run the stop ended (ranUnreported); its first sentence is the reason the task stores
// (LateReportOfStoppedTask). Every other report gets nil: the first report of a run an event
// ended, whatever step the task stands at now, which handoff takes as it takes one on an issue
// with no plan rows (leftPhaseReported); one of no run; one of a run whose plan rows are gone; one
// of the retro or the merge; one of a task whose issue left the workflow
// (HANDOFF_NOT_CURRENT_PHASE); and one of a step other than the one the stop ended. The refusal
// writes nothing itself: the caller answers it after the lingering tree's and keeps the report's
// handoff record (spendLateHandoff).
func (e *Engine) lateReport(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fact intake.HandoffComplete, reported phase.Phase, past pastReport,
	row record.PhaseRow) (*intake.Refusal, error) {
	if past != stoppedStepReported || !w.Stored {
		return nil, nil
	}
	// The report is of the step whose run row holds, so of two steps that run as step, or two
	// steps of one role, only the one the stop ended (the task keeps its place at its stop) is
	// late.
	at, ok := w.runPlace(row, reported)
	if !ok || at != w.Place || at.Kind != record.PlaceStep {
		return nil, nil
	}
	ran, err := e.ranUnreported(ctx, tx, issue, fact.Role, fact.Claim, row)
	if err != nil || !ran {
		return nil, err
	}
	return intake.LateReportOfStoppedTask(issue.Key, w.Task, w.StoppedReason, at.Position, w.Steps[at.Position-1].Role, fact.HandoffRecord != ""), nil
}

// spendLateHandoff keeps handoff, the record a late report lateReport refused names, as its role's
// last one on row, the role's row of the run the report is of (record.PhaseRow.LastHandoff), and
// writes nothing else: the issue, its task and the rest of the row stand as they were, and no
// movement is marked. The record counts as used, so the step's next run is refused it
// (HANDOFF_NOT_NEW) and writes a handoff of its own, as the refusal tells the agent. A report that
// names no record writes nothing.
func (e *Engine) spendLateHandoff(ctx context.Context, tx pgx.Tx, row record.PhaseRow, handoff string) error {
	if handoff == "" || row.LastHandoff == handoff {
		return nil
	}
	row.LastHandoff = handoff
	return e.store.PutPhase(ctx, tx, row)
}

// beforeItsWork says whether issue stands before its first step: admitted, or a root waiting
// there on a person (enterAwaiting). Its task has not started, so a first plan takes it.
func beforeItsWork(issue record.Issue) bool {
	return issue.Phase == phase.Admitted || issue.Phase == phase.AwaitingPerson
}

// planRatingRefusal is the rule of which completion carries the rating of the plan, judged where
// the retro ends: handoff reads it on the issue as the completion's own transaction reads it, under
// the lock every fact applies under, which is the read its TriggerRetroCompleted ends the retro on.
// The route held what no state of the issue changes: the rating's form (api's planRatingOf) and
// whether the caller must rate (intake.HandoffComplete.MustRate). endsRetro is whether this
// completion ends the retro, and reported the phase it is of, which the refusal names. The
// completion that ends the retro with no rating from a caller that must rate is refused
// PLAN_RATING_REQUIRED, and any other completion that carries one PLAN_RATING_UNEXPECTED; nil is
// no refusal.
func planRatingRefusal(fact intake.HandoffComplete, endsRetro bool, reported phase.Phase) *intake.Refusal {
	what := fmt.Sprintf("this completion of phase %s of %s", reported, fact.Issue)
	switch {
	case endsRetro && fact.PlanRating == nil && fact.MustRate:
		return intake.PlanRatingRequired(what)
	case !endsRetro && fact.PlanRating != nil:
		return intake.PlanRatingUnexpected(what)
	}
	return nil
}
