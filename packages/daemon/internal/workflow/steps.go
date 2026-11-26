package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/steprecord"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// defaultTask is the id of the one task of an issue whose owner gives no plan, or whose
// experiment's group takes none (plan.DefaultTask): the task of the project's default steps, and
// of today's steps of a run with no plan rows (todaysSteps), under which the steps of each are
// recorded.
const defaultTask = plan.DefaultTask

// StepAgent is what the daemon reads of a step's agent where it writes the step's end, the one
// write of a step's record that names either field (endRun): the session its claim records now,
// and the model that answered it last in that session as `<provider>/<model>`, or the model alone
// when the frame named no provider. Either is "" where the daemon reads none, or reads text outside
// the form of a step's agent text (the daemon's stepAgent), and the end then leaves that field
// out, so the record names none.
type StepAgent struct {
	Session string
	Model   string
}

// stepRole is the role a step's record names, the worker of the step as Dispatch reads it
// ("acceptance" for accepting's architect), a type of its own, so no call can give a step's role,
// its type (plan.Type, "" for a retro or a merge) and its agent, three texts, in another order
// (stepWrite).
type stepRole string

// stepOfWalk is the step w's task runs at place, a place of w's line (Walk.PlaceOf): the role and
// the type the plan's rows record for the step (record.PlanStep), or the role that runs Legion's
// own retro or merge, which no plan names and which has no type. A step place the task's steps do
// not hold, and a task at no act, run no step.
func stepOfWalk(w Walk, place record.TaskPlace) (role stepRole, typ plan.Type, ok bool) {
	switch place.Kind {
	case record.PlaceRetro:
		return "implementer", "", true
	case record.PlaceMerge:
		return "merger", "", true
	case record.PlaceStep:
		if place.Position >= 1 && place.Position <= len(w.Steps) {
			named := w.Steps[place.Position-1]
			return stepRole(named.Role), named.Type, true
		}
	}
	return "", "", false
}

// standing is the step an issue stands in: its place, its role and its type (stepAt), the phase
// it is the step of (record.StandsAt), and whether its agent is the owner of the task (owner).
type standing struct {
	place record.StepPlace
	role  stepRole
	typ   plan.Type
	at    phase.Phase
	// owner is whether the owner of the task does this step or retro (Walk.PlaceRole is the
	// architect) and the issue works it now, held from it by nothing, which standingStep alone
	// reads: a start of such a run tells the owner (startStep). A held issue's owner is told when
	// the hold's retry puts the issue back in the step (restart), as a held worker is started then.
	owner bool
}

// standingStep is the step issue stands in on w, the walk of its run: stepAt where it stands
// (record.StandsAt, so a held issue's is the step of the phase its hold is from), with whether the
// owner of the task does it now (standing.owner). Every writer of a step's record takes the step
// it found, or, for a phase the issue has left, stepAt's (reportEndedStep). An issue its plan
// holds stands in no step (planHold): a stopped task's run ended with its stop, and a parent held
// for its children has not started its retro, so the end rule ends the run each stood in (runOver)
// and no start is made for either.
func standingStep(w Walk, issue record.Issue) (standing, bool) {
	if planHold(issue, w.Stored) {
		return standing{}, false
	}
	step, ok := stepAt(w, issue, standingIn(record.StandsAt(issue)))
	step.owner = ok && issue.Phase != phase.Held && w.OwnerAt(w.Place)
	return step, ok
}

// stepSite is the place a caller of stepAt asks for, made by standingIn or placeNamed, with the
// phase at, which that place runs as. The zero stepSite is no place: stepAt answers false for it.
type stepSite struct {
	at    phase.Phase
	place record.TaskPlace
	named bool
}

// standingIn is where the task stands while its issue stands in at (Walk.placeIn): on a stored
// walk the task's own place where it runs as at, so of two steps that run as step the one the
// task stands at, and on a walk with no plan rows the place of at on today's line.
func standingIn(at phase.Phase) stepSite { return stepSite{at: at} }

// placeNamed is a place the caller names, which runs as at: place on a stored walk; on a walk with
// no plan rows, whose line holds each phase once, the place of at on today's line, so an issue
// that ran before plan rows keeps the ids of its records.
func placeNamed(place record.TaskPlace, at phase.Phase) stepSite {
	return stepSite{at: at, place: place, named: true}
}

// resolveSite uses the named place on a stored walk. A legacy walk locates the phase on its line.
func (w Walk) resolveSite(site stepSite) (record.TaskPlace, bool) {
	if site == (stepSite{}) {
		return record.TaskPlace{}, false
	}
	if site.named && w.Stored {
		return site.place, true
	}
	return w.placeIn(site.at)
}

// stepAt is the one place that decides the step issue runs at site on w, the walk of its run, and
// whether its runs are recorded (placeOnWalk), and false for no place and for a phase that runs no
// step — admitted, awaiting a person, held, done, a phase the walk's line does not hold — or a
// record key that is no issue key (recordsSteps).
func stepAt(w Walk, issue record.Issue, site stepSite) (standing, bool) {
	place, role, typ, ok := placeOnWalk(w, site)
	return standing{place: place, role: role, typ: typ, at: site.at}, ok && recordsSteps(issue)
}

// placeOnWalk is the step the run whose walk is w runs at site: the place its count of starts is
// kept under, whose Place is its place in the task, its role and its type: the step of its one
// task at the site's place on the task's line (stepOfWalk), under the task's id. An owner's plan
// of `implementer reviewer` records its implementer at step 1 of its own task and has no step 3,
// 4 or 5, of two steps that run as step it is the one the site names, and a run with no plan rows
// runs today's steps of the task default (todaysSteps), so an issue that ran before plan rows
// keeps the ids of its records.
func placeOnWalk(w Walk, site stepSite) (place record.StepPlace, role stepRole, typ plan.Type, ok bool) {
	stands, ok := w.resolveSite(site)
	if !ok {
		return record.StepPlace{}, "", "", false
	}
	if role, typ, ok = stepOfWalk(w, stands); !ok {
		return record.StepPlace{}, "", "", false
	}
	return stands.StepPlace(w.Task), role, typ, true
}

// StepResult is how a run of a step ended: one of the three results Dispatch takes for a step
// record, each the word contracts gives it (steprecord). Any other text is refused there as
// 400 STEP_INPUT, which the outbox drops for good (internal/daemon's recordRefused), so every
// end names its result by one of the three constants below.
type StepResult string

const (
	// ResultPassed is a run whose step's work was done: the move that ended it went forward, or
	// its merge landed in a lingering tree (closeMerged).
	ResultPassed StepResult = steprecord.ResultPassed
	// ResultFailed is a run a role or a report sent back: the tester's fail, a review's request
	// for changes, a role's or the architect's backward move.
	ResultFailed StepResult = steprecord.ResultFailed
	// ResultStopped is a run that ended under its role with neither: an end no move of the table
	// wrote (stepStopped).
	ResultStopped StepResult = steprecord.ResultStopped
)

// endOf is how a transition on trigger ends the step it moves the issue out of. A move back a role
// or a report asked for — the tester's fail, a review's request for changes, the fail of a step
// that runs as step, a role's or the architect's backward move — ends it failed and returns the
// work to the step the issue moves into, which the record names. Every other move ends the step
// passed and returns nowhere. A head that conflicts with its base moves no issue
// (dispatch://INFRA-4374), so it ends no step: the run goes on through the merge forward.
func endOf(trigger TriggerKind) (result StepResult, returns bool) {
	switch trigger {
	case TriggerTesterFailed, TriggerReviewRejected, TriggerStepFailed, TriggerBackward:
		return ResultFailed, true
	}
	return ResultPassed, false
}

// stepRecordNamespace is the namespace of every step record's id.
var stepRecordNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("legion-step-record"))

// StepID is the id of the record of one run of one step: a version 5 uuid of the issue, its run
// (record.Issue.Generation), the place — its task, kind and place in the task, the key the count
// of starts is kept under, taken whole so no call can give its fields in another order — and the
// count of starts of that place (record.Store.StepStarts). The start of the run raises the count
// and the end reads it unchanged, so the two writes compute one id apart, across a restart too,
// and the next start of the same place, which raises the count again, writes another record.
func StepID(issue string, generation uint64, place record.StepPlace, starts int64) string {
	name := fmt.Sprintf("%s/%d/%s/%s/%d/%d", issue, generation, place.Task, place.Kind, place.Place, starts)
	return uuid.NewSHA1(stepRecordNamespace, []byte(name)).String()
}

// StepEnd is how a step ends: its result, the step the work returns to (nil where it returns
// nowhere; a retro's or a merge's place is 0) and the handoff record the ending role's completion
// named ("" for none).
type StepEnd struct {
	Result     StepResult
	ReturnedTo *int
	Report     string
}

// stepStopped is the end of a step that no completion and no move of the table ended: the issue
// left the workflow, its tree lingered, or its run ended under it.
var stepStopped = StepEnd{Result: ResultStopped}

// startStep writes the start of the step issue stands in (record.StandsAt), run by agent: one more
// start counted at its place (record.Store.RaiseStepStarts), and a step record row, committed with
// the start, carrying every identity field and the moment it started, and no session: Dispatch
// keeps the first value it is given of each field and only the end names the model, so the end
// writes the session and the model as one read's pair (endRun). The start first ends each run of
// the issue that is over, the run of its own place among them (settleStart), so the end of the run
// the issue leaves, or that its place held, reaches Dispatch before this start. The record never
// makes the start wait: the row reaches Dispatch on a lane of its own (record.LaneRecord). A phase
// that runs no step and a record key that is no issue key are no step its callers find
// (standingStep).
//
// It is the one writer of the start of every run, so it is the one place a run of a step or a
// retro the owner does is told to the owner: where step.owner, it queues the notice owner-step
// with task, the text the owner's entry holds (tellOwner), in the same transaction as the record.
// No path that starts such a run can start it untold: a transition's start, a task started again,
// a held retro and a re-admission's promotion all come here through enter (startOwnersStep).
func (e *Engine) startStep(ctx context.Context, tx pgx.Tx, issue record.Issue, step standing, agent claim.Token, task string) error {
	if err := e.settleStart(ctx, tx, issue.Key, step.place); err != nil {
		return err
	}
	// The moment is the one the count of starts keeps, which PostgreSQL holds to the microsecond
	// and the end reads back, so the start and the end write one moment (endRun).
	now := e.now().Truncate(time.Microsecond)
	drawn, err := e.variantOf(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	var starts int64
	write, err := e.store.RaiseStepStarts(ctx, tx, issue.Key, step.place, func(raised int64) record.StepRecord {
		starts = raised
		write := stepWrite(issue, drawn, step.place, step.role, step.typ, string(agent), raised)
		write.StartedAt = new(now)
		return write
	})
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, write); err != nil {
		return err
	}
	if !step.owner {
		return nil
	}
	return e.tellOwner(ctx, tx, issue, step, starts, task)
}

// endStep writes the end of the run of the step at site on w, the walk of that run, before being
// the issue as it stood in that run. It writes the end of each run a step of its walk holds, and
// the fact's settle is its one caller (settleRuns), which decides that an open run is over, reads
// w at the run's own generation and names the run's own place (placeNamed), so the end of a run
// the fact left names the step of that run's plan, and of two steps that run as step, the one the
// run stood at, wherever the task stands now; a run no step of that walk holds is ended from its
// kept start instead (endOffWalk). The record is the one its start wrote: the same generation and
// the count of starts unchanged, and the agent and the moment that start wrote, so the two writes
// agree whichever reaches Dispatch first. A run no start of this release counted has an open row
// from the daemon's boot (OpenUncountedRuns), which wrote the count and the agent its start would
// have named, so its end is written the same way. endRun writes it. A phase that runs no step
// writes nothing, and neither does a record key that is no issue key (recordsSteps).
//
// It is the one writer of the end of each step on a walk, so it is the one place an owner's slot is
// given its own App back at a step's end: where the run it ends is a step of a stored plan whose
// start named an architect (the owner, or the architect above that ran the step for it,
// ownerAgent), and the one rule of what the owner's place switches the slot to gives a role for
// the place (record.OwnersPlaceActsAs, which the daemon's ActsAs reads while the place runs) that
// acts as another App than the owner's own (actsAsOwnApp), it queues a supervise slot_credential of
// the owner's claim in the same transaction, which writes the owner's slot back to its own App
// when it runs, and writes nothing where another act of the owner's of another App runs by then
// (ownerSlotCredential; internal/daemon's slotCredential). So the owner's retro of a task with no
// step of the type implementation, done as itself, queues none: its slot held the owner's own App
// throughout. Nor does the end of the owner's tester, reviewer or acceptance step, each of which
// acts as the review App, the owner's own. A merge's start names the merger, so its end queues
// none. A run on no step of its walk is ended, and its write-back queued, by endOffWalk.
func (e *Engine) endStep(ctx context.Context, tx pgx.Tx, before record.Issue, w Walk, site stepSite, end StepEnd) error {
	step, ok := stepAt(w, before, site)
	if !ok {
		return nil
	}
	drawn, err := e.variantOf(ctx, tx, before.Key)
	if err != nil {
		return err
	}
	var agent claim.Token
	ended, err := e.endRun(ctx, tx, before.Key, step.place, end, func(start record.StepStart) record.StepRecord {
		agent = claim.Token(start.Agent)
		return stepWrite(before, drawn, step.place, step.role, step.typ, start.Agent, start.Starts)
	})
	if err != nil || !ended || !w.Stored {
		return err
	}
	if role, switched := record.OwnersPlaceActsAs(step.at, claim.Role(step.role), w.Steps); !switched || actsAsOwnApp(role) {
		return nil
	}
	return e.ownerSlotCredential(ctx, tx, before, agent)
}

// actsAsOwnApp is whether role acts as the App of the owner's own role, the architect's, by the one
// role-to-App rule (appauth.AppRoleFor), asked with no deployment and no plan: it names the App of
// a name of Legion's whatever the deployment's settings or a plan say, and answers any other name
// ErrRoleNotDefined, which is false here. Such a role is one a plan or the deployment defines,
// whose App the daemon reads (appauth.RoleApps), so its end queues the write-back, and the runtime
// rewrites nothing where the slot holds that App already (runtime.SlotCredentials).
func actsAsOwnApp(role claim.Role) bool {
	app, err := appauth.AppRoleFor(role, nil, appauth.Planned{})
	if err != nil {
		return false
	}
	own, err := appauth.AppRoleFor(claim.RoleArchitect, nil, appauth.Planned{})
	return err == nil && app == own
}

// ownerSlotCredential queues, where agent, the agent the start of an ended step or retro named, is
// an architect, the supervise slot_credential that writes the slot of the owner of issue's task
// (record.OwnerClaim) back to the App of the owner's own role (internal/daemon's slotCredential).
// While the owner does a step, or the retro of its own implementation step, its slot holds the App
// of the role it is done as, unless a merge forward asked of the owner holds it on the implement
// App (internal/daemon's ownerActsAs). So the end of that step or retro queues the write-back
// rather than leave the slot to its next refresh. The write is made only where the record then
// answers the owner's own App, so a step, a retro or a merge forward of another App that runs by
// then keeps its App, and the next notice to the owner waits for that write (internal/daemon's
// deliverNotice). The
// slot written is always the owner's, whichever architect the start named: the owner-step goes to
// the owner, and only the owner's claim acts as another role. Any other agent's slot never held
// another App, and an issue whose key makes no claim token has no owner's slot.
func (e *Engine) ownerSlotCredential(ctx context.Context, tx pgx.Tx, issue record.Issue, agent claim.Token) error {
	if _, of, ok := agent.Cut(); !ok || of != claim.Agent(claim.RoleArchitect) {
		return nil
	}
	row, ok := record.OwnerSlotWriteBack(issue)
	if !ok {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, row)
}

// endRun is the one writer of the end of a run, the run at place on issue, for both of its callers:
// endStep, for a run a step of its walk holds, and endOffWalk, for a run on no step of it. Each
// gives identity, which builds the record's identity from the start the end reads. One run gets one
// end: endRun marks the run ended in the store, in its own transaction (record.Store.EndStepStart),
// and an end of a run already marked writes nothing and answers false. The end carries that
// identity, the moment the start wrote, the moment now, end's fields, and the session and the model
// of one read of the start's agent now, the only write that names either (startStep).
func (e *Engine) endRun(ctx context.Context, tx pgx.Tx, issue string, place record.StepPlace, end StepEnd, identity func(start record.StepStart) record.StepRecord) (bool, error) {
	start, ended, err := e.store.EndStepStart(ctx, tx, issue, place)
	if err != nil || !ended {
		return false, err
	}
	write := identity(start)
	if !start.StartedAt.IsZero() {
		write.StartedAt = new(start.StartedAt)
	}
	write.EndedAt, write.Result, write.ReturnedTo = new(e.now()), new(string(end.Result)), end.ReturnedTo
	if end.Report != "" {
		write.Report = new(end.Report)
	}
	read := e.readAgent(ctx, tx, claim.Token(start.Agent))
	if read.Session != "" {
		write.Session = new(read.Session)
	}
	if read.Model != "" {
		write.Model = new(read.Model)
	}
	return true, e.enqueue(ctx, tx, issue, write)
}

// reportEndedStep writes report, the handoff a completion of at named, onto the record of the run
// of at's step that ended before that completion landed: a review the review event moved on, whose
// end the transition wrote with no report, since the reviewer had not completed yet. A record's
// report is the handoff the ending role's completion named, whichever of the two lands first. The
// run is the latest of the place of the step whose run row holds (Walk.runPlace), row being the
// role's row of the run the completion reports (Walk.completionRow): a role can stand at two steps,
// and the issue has left the one this completion is of, which its served entry names, wherever the
// task stands now, and no start of it came since (handoff's current). w is the walk of issue's run.
// The write carries the run's identity and the report alone, and Dispatch writes each field once,
// so it and the end merge in either order (record.StepRecord). A completion that names no handoff
// writes nothing, and neither does a place whose latest run is not marked ended (record.RunEnded)
// — none ran, one is open, or one an earlier release started after a rollback, whose record this
// release has not written — nor a record key that is no issue key.
func (e *Engine) reportEndedStep(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, at phase.Phase, row record.PhaseRow, report string) error {
	if report == "" {
		return nil
	}
	place, ok := w.runPlace(row, at)
	if !ok {
		return nil
	}
	step, ok := stepAt(w, issue, placeNamed(place, at))
	if !ok {
		return nil
	}
	latest, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
	if err != nil || latest.Run != record.RunEnded {
		return err
	}
	drawn, err := e.variantOf(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	write := stepWrite(issue, drawn, step.place, step.role, step.typ, latest.Agent, latest.Starts)
	write.Report = new(report)
	return e.enqueue(ctx, tx, issue.Key, write)
}

// recordsSteps says whether issue's steps are recorded: its key is an issue key, which every key
// Dispatch gives is, and so names claims (claim.NewToken) and an issue the route can hold. The
// record has keys that are none only where a test wrote them, and those have no claims either,
// as the promotion's own walk passes over them (admit's startMidPhaseMembers).
func recordsSteps(issue record.Issue) bool { return claim.IsIssueKey(issue.Key) }

// upgradedStart is the agent and the moment of the start of a step at that no start of this
// release counted, which ran under the release before: the agent its start would have named
// (startAgent, so a step of the owner's is the architect that runs it now, ownerAgent), and the
// moment its phase worker's entry was raised. At a place the owner works (RoleAt is the
// architect) the moment is unknown: the release before raised no entry for the architect's
// acceptance, the one step of the owner's it ran. A merge whose merger's entry this visit to
// merging did not raise is one GitHub had already made when the issue reached merging, which
// started no merger: an entry never raised, or one raised before the retro's completion that
// brought the issue there, the one move into merging (the table's retro row), which is an
// earlier visit's.
func (e *Engine) upgradedStart(ctx context.Context, tx pgx.Tx, issue record.Issue, at phase.Phase) (claim.Token, time.Time, error) {
	worker := issue.RoleAt(at)
	if worker == claim.RoleArchitect {
		agent, err := e.startAgent(ctx, tx, issue, at)
		return agent, time.Time{}, err
	}
	row, err := e.phaseRow(ctx, tx, issue, worker)
	if err != nil {
		return "", time.Time{}, err
	}
	if at == phase.Merging {
		// The retro's completion is recorded on the row of the role that works the retro.
		w, err := e.walk(ctx, tx, issue)
		if err != nil {
			return "", time.Time{}, err
		}
		retro, err := e.phaseRow(ctx, tx, issue, w.RoleAt(record.TaskPlace{Kind: record.PlaceRetro}))
		if err != nil {
			return "", time.Time{}, err
		}
		if row.EntryRaisedAt.IsZero() || row.EntryRaisedAt.Before(retro.CompletedAt) {
			return "", time.Time{}, nil
		}
	}
	agent, err := e.placeToken(ctx, tx, issue, at, worker)
	return agent, row.EntryRaisedAt.UTC(), err
}

// startAgent is the agent a start of the step issue stands in at names: for an acceptance the owner
// does (an issue with no plan rows, or a step acceptance:owner: RoleAt is the architect, the rule
// of the owner's place, record.Issue.OwnersPlace), the architect that runs the owner's step now
// (ownerAgent), nobody for a merge GitHub has already made, where no merger starts, and the agent
// of the step's place otherwise (placeToken), which is ownerAgent's answer too for a step of the
// key owner, and the fresh agent of acceptance for a step acceptance with no key.
func (e *Engine) startAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, at phase.Phase) (claim.Token, error) {
	switch {
	case at == phase.Accepting && issue.RoleAt(at) == claim.RoleArchitect:
		return e.ownerAgent(ctx, tx, issue, at)
	case at == phase.Merging:
		standing := issue
		standing.Phase = at
		merged, err := PhaseFinished(ctx, e.store, tx, standing)
		if err != nil || merged {
			return "", err
		}
	}
	return e.placeToken(ctx, tx, issue, at, issue.RoleAt(at))
}

// placeToken is the claim token of the agent of the work issue stands in at, read on the walk of
// issue's run (walkToken).
func (e *Engine) placeToken(ctx context.Context, tx pgx.Tx, issue record.Issue, at phase.Phase, role claim.Role) (claim.Token, error) {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return "", err
	}
	return e.walkToken(ctx, tx, issue, w, at, role)
}

// walkToken is the claim token of the agent of the work issue stands in at, read on w, issue's
// walk (stepToken), and role's own token where w holds no place for at.
func (e *Engine) walkToken(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, at phase.Phase, role claim.Role) (claim.Token, error) {
	place, ok := w.placeIn(at)
	if !ok {
		return stepClaim(issue, issue.Key, role)
	}
	return e.stepToken(ctx, tx, issue, w, place, at, role)
}

// stepToken is the claim token of the agent that does the work of place on w, issue's walk, whose
// role is role and which runs as at: the workflow's one function that makes the token of a step's
// agent, which each entry takes (enter) and writes to the step's phases row (record.PhaseRow.Claim)
// and to its step record (startStep). A place whose agent is the owner (Walk.OwnerAt: a step of the
// key owner, or the retro of a task whose retro is the owner's) is run by the architect that owns
// the issue now (ownerAgent), as an acceptance the owner does is (startAgent). Where AgentAt names
// an agent, it is that agent's token (claim.AgentToken), so the steps of one key are one claim;
// where it names none — the merge, and every place of a run with no plan rows — it is the role's
// own token (R20). A launch's addressing makes a stored step's token apart from this, by
// record.StepToken (the daemon's stepWorkersOf): the same agent's token where the step's agent is
// not the owner, and for a step of the key owner the issue's own architect by rule
// (record.OwnerClaim), where this names the architect that runs the owner's step now, which can be
// the architect above.
func (e *Engine) stepToken(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place record.TaskPlace, at phase.Phase, role claim.Role) (claim.Token, error) {
	if w.OwnerAt(place) {
		return e.ownerAgent(ctx, tx, issue, at)
	}
	name := w.AgentAt(place)
	if name == "" {
		return stepClaim(issue, issue.Key, role)
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", err
	}
	return claim.AgentToken(project, issue.Key, name)
}

// implementationAgent is the agent that does the implementation work of issue's task where the
// issue stands in at, on w, issue's walk, as a recipient of a role message: workAgent of the role
// implementer, the agent of the step Walk.implementationPlace names. So the agent is the same
// whatever its role is called - the implementer of a run with no plan rows, or a role the
// deployment or the plan defines of the type implementation - and a role whose step no task holds
// is never told. ok is false for a task with no step of that type.
func (e *Engine) implementationAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, at phase.Phase) (roleRecipient, bool, error) {
	return e.workAgent(ctx, tx, issue, w, at, claim.RoleImplementer)
}

// workAgent is the agent that does the work of role, one of Legion's roles of a step, in issue's
// task where the issue stands in at, on w, issue's walk, as a recipient of a role message: the
// agent of the step Walk.workPlace names from the place of at (placeIn, none where the walk holds
// no place for at), with that step's role and the claim stepToken makes for it, the claim the start
// of that step names. Where that step's agent is the owner (Walk.OwnerAt), the claim is the owner's
// (record.OwnerClaim), as for every message to the worker of an owner's step
// (record.Issue.StepWorker): the owner does that step, since its owner-step and its report are the
// owner's, while the start names the architect above where the issue's own architect holds no role
// (ownerAgent). ok is false for a task with no step that does that work.
func (e *Engine) workAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, at phase.Phase, work claim.Role) (roleRecipient, bool, error) {
	place, _ := w.placeIn(at)
	step, ok := w.workPlace(place, work)
	if !ok {
		return roleRecipient{}, false, nil
	}
	role := claim.Role(w.Steps[step.Position-1].Role)
	_, token, err := record.StepRecipient(issue, role, w.OwnerAt(step))
	if err == nil && token == "" {
		token, err = e.stepToken(ctx, tx, issue, w, step, at, role)
	}
	if err != nil {
		return roleRecipient{}, false, err
	}
	return roleRecipient{issue: issue.Key, role: role, row: issue.Key, claim: token}, true, nil
}

// ownerAgent is the claim that runs a step of the owner of issue's task at at (a step of the key
// owner, the owner's retro, or the acceptance of a run with no plan rows), and that the step's
// record names: the architect that owns issue now, the one that does the step. The owner by rule
// is the issue's own architect (record.OwnerClaim), and it
// runs the step while its claim holds its role (owningArchitect, the owner walk over the claims
// this transaction reads), or while a start of it that will run waits in the outbox
// (ownArchitectStarting); else the nearest architect above it whose claim holds its role does the
// step, else the tree's, so a record never names an architect that did not act. It is the one
// place every start of an owner's step (startAgent, stepToken) and the boot's open of a run of one
// no start counted (upgradedStart) read, so the record names the architect whose request ends the
// step, and that request waits for that architect's stream (api's apply, record.RunsAStepOn).
func (e *Engine) ownerAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, at phase.Phase) (claim.Token, error) {
	starting, err := e.ownArchitectStarting(ctx, tx, issue, at)
	if err != nil {
		return "", err
	}
	owner := issue.Key
	if !starting {
		if owner, err = e.owningArchitect(ctx, tx, issue); err != nil {
			return "", err
		}
	}
	return stepClaim(issue, owner, claim.RoleArchitect)
}

// ownArchitectStarting is whether issue's own architect, a child's, runs its owner's step at at by
// a start the outbox will act on, which the owner walk does not see: the walk reads a claim as the
// store records it, and a claim whose start is only queued does not hold its role yet
// (record.HoldsRole). Two moves queue such a start in the transaction that starts the step: the
// move into it, which starts a failed child architect again (reviveArchitect), and a re-admitted
// tree's first run, which brings back a member's suspended architect (admit's
// startMidPhaseMembers). Once that start runs, the child's own architect owns the child, and it
// does the step. The start is one of no phase or of at (architectStartQueued).
//
// Two such starts do not count. One queued for a claim the operator suspended: that suspension is
// woken by no start, queued or not, since each waits for the operator's resume
// (supervise.Claim.RuleHolds). And one queued once the issue has left at, which the store's
// record of the issue says: the start is then the next phase's (reviveArchitect at that phase's
// start).
func (e *Engine) ownArchitectStarting(ctx context.Context, tx pgx.Tx, issue record.Issue, at phase.Phase) (bool, error) {
	run, child, err := e.ownArchitectRun(ctx, tx, issue)
	if err != nil || !child || !architectStartQueued(run, at) {
		return false, err
	}
	if run.Claim != nil && (supervise.Claim{State: run.Claim.State, SuspendedBy: run.Claim.SuspendedBy}).RuleHolds() {
		return false, nil
	}
	stands, err := e.store.Issue(ctx, tx, issue.Key)
	if err != nil || stands == nil {
		return false, err
	}
	return record.StandsAt(*stands) == at && stands.Generation == issue.Generation, nil
}

// startNamedStep writes the start of step, the step issue stands in (standingStep), under the agent
// that step names (startAgent), the one place that decides who runs a start no phase worker's entry
// names: a move into accepting of a run with no plan rows, whose role is the architect, which
// starts no worker (Engine.start), a move into a merge GitHub has already made, which nobody runs
// and whose close ends it (transition), and admission's promotion of a member whose worker it does
// not start (StartStepRecord). None of them is a step the owner does, whose start raises the
// owner's entry and tells the owner its task (startOwnersStep), so it tells no task.
func (e *Engine) startNamedStep(ctx context.Context, tx pgx.Tx, issue record.Issue, step standing) error {
	agent, err := e.startAgent(ctx, tx, issue, step.at)
	if err != nil {
		return err
	}
	return e.startStep(ctx, tx, issue, step, agent, "")
}

// stepClaim is the claim token of role on key, in issue's project.
func stepClaim(issue record.Issue, key string, role claim.Role) (claim.Token, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", err
	}
	return claim.NewToken(project, key, role)
}

// stepWrite is the identity of one run of a step of issue, the fields every write of its record
// carries, and drawn, the experiment and the variant of issue's group (variantOf), nil for an issue
// of no group. Every write of a step record is built here, and each caller reads drawn with
// variantOf first, so each write names its issue's variant. A nil drawn compiles, so the compiler
// holds no caller to that read: TestEachStepRecordNamesTheVariantOfItsIssue holds the start, the
// end, the rating and the late report, and TestTheStartTheBootOpensNamesTheVariantOfItsIssue the
// start the boot opens (OpenUncountedRuns).
func stepWrite(issue record.Issue, drawn *record.StepExperiment, place record.StepPlace, role stepRole, typ plan.Type, agent string, starts int64) record.StepRecord {
	return record.StepRecord{ID: StepID(issue.Key, issue.Generation, place, starts), Task: place.Task,
		Step: place.Place, Kind: place.Kind, Role: string(role), Type: string(typ), Agent: agent, Generation: issue.Generation,
		StepExperiment: drawn}
}

// variantOf is the experiment and the variant of issue's group, the row of its draw that says how
// it is done (groupOf), as a step record names them: nil for an issue of no group, whose records
// name neither. The row was written once, at the issue's draw, so every record of the issue names
// the one variant it was drawn into, whatever the project's settings say now.
func (e *Engine) variantOf(ctx context.Context, tx pgx.Tx, issue string) (*record.StepExperiment, error) {
	group, err := e.groupOf(ctx, tx, issue)
	if err != nil || group.Experiment == "" || group.Variant == "" {
		return nil, err
	}
	return &record.StepExperiment{Experiment: group.Experiment, Variant: group.Variant}, nil
}

// readAgent is what the daemon reads of agent now (Config.StepAgent), in the fact's transaction
// tx: nothing for no agent or no reader.
func (e *Engine) readAgent(ctx context.Context, tx pgx.Tx, agent claim.Token) StepAgent {
	if agent == "" || e.cfg.StepAgent == nil {
		return StepAgent{}
	}
	return e.cfg.StepAgent(ctx, tx, agent)
}

// endOfTransition is how a transition on trigger ends the step from, where the issue stood, moving
// it into to with handoff as the completion's row: endOf's result, the place of the step at to on
// w, the walk of the issue's run, where the work goes back (placeOnWalk; to is the place the
// transition's row moves the task to), and the handoff record handoff names when its role is the
// role whose step the move ends (Walk.endingRole). Its callers name it for the step they found the
// issue in (nameMoveEnd, stopUnreturned), so from runs a step, which has a role, and a completion
// of no role names no report.
func endOfTransition(w Walk, trigger TriggerKind, from phase.Phase, to stepSite, handoff record.PhaseRow) StepEnd {
	result, returns := endOf(trigger)
	end := StepEnd{Result: result}
	if into, _, _, ok := placeOnWalk(w, to); returns && ok {
		end.ReturnedTo = new(into.Place)
	}
	if handoff.Role == w.endingRole(from) {
		end.Report = handoff.HandoffRecord
	}
	return end
}

// StartStepRecord writes the start of the step issue stands in on the walk of its run
// (standingStep), under the agent that step names (startNamedStep): admission's promotion of a
// member whose phase worker it does not start (admit's startMidPhaseMembers), whose step starts
// again all the same. A step whose latest run is still open at the issue's own generation goes
// on, and nothing is written: a role the promotion found already started for this run
// (workflow.StartFor), or a member held from merging whose tree is admitted again, whose merge
// step the linger left open (beginLinger) and whose retry goes on with it. A second start there
// would open a run while the one before it has no end, and the count would leave that one none. A
// run still open at an earlier generation is another run's: a member held from merging and then
// handed over at the linger's close stands at the generation its readmit raised, while the settle
// that ends its old merge runs only at the fact's end, so the start goes on, and its own settle
// (settleStart) ends that old run before it opens the new one. A member its plan holds starts no
// step (standingStep): its stopped task runs again only when a later version of the plan starts
// the task at its first step, and a parent held for its children has started no retro.
//
// A step or a retro the owner does starts as every start of the owner's does (startOwnersStep):
// its entry raised under the owner's claim with the phase's resume task, as the promotion's start
// of a mid-phase worker raises one (StartPromotedWorker), its run's start, and one notice
// owner-step that carries that task (startStep).
func (e *Engine) StartStepRecord(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	step, ok := standingStep(w, issue)
	if !ok {
		return nil
	}
	runs, err := e.store.StepRuns(ctx, tx, []string{issue.Key})
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Place == step.place && run.Run == record.RunOpen && run.Generation == issue.Generation {
			return nil
		}
	}
	if step.owner {
		return e.startOwnersStep(ctx, tx, issue, w, func(at Standing) string { return resumeText(issue, at) }, false)
	}
	return e.startNamedStep(ctx, tx, issue, step)
}
