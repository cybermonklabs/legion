package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/steprecord"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// defaultTask is the one task of today's fixed phases: until an issue carries a plan of its own,
// every step of it is a step of this task.
const defaultTask = "default"

// StepAgent is what the daemon reads of a step's agent where it writes the step's end, the one
// write of a step's record that names either field (endStep): the session its claim records now,
// and the model that answered it last in that session as `<provider>/<model>`, or the model alone
// when the frame named no provider. Either is "" where the daemon reads none, or reads text outside
// the form of a step's agent text (the daemon's stepAgent), and the end then leaves that field
// out, so the record names none.
type StepAgent struct {
	Session string
	Model   string
}

// stepRole is the role a step's record names, the worker of the step as Dispatch reads it
// ("acceptance" for accepting's architect), and stepType the type of its work ("" for a retro or a
// merge). Each is a type of its own, so no call can give a step's role, its type and its agent,
// three texts, in another order (stepWrite).
type (
	stepRole string
	stepType string
)

// stepOf is the step a phase runs as a step of the task default, whose steps are DefaultSteps:
// stepIn over that list. A held issue's step is the step of the phase its hold is from
// (record.StandsAt), which the caller reads.
func stepOf(p phase.Phase) (place record.StepPlace, role stepRole, typ stepType, ok bool) {
	return stepIn(DefaultSteps(), p)
}

// standing is the step an issue stands in: its place, its role and its type (stepOf), and the
// phase it is the step of (record.StandsAt).
type standing struct {
	place record.StepPlace
	role  stepRole
	typ   stepType
	at    phase.Phase
}

// standingStep is the step issue stands in: stepAt the phase it stands in (record.StandsAt, so a
// held issue's is the step of the phase its hold is from). Every writer of a step's record takes
// the step it found, or, for a phase the issue has left, stepAt's (reportEndedStep).
func standingStep(issue record.Issue) (standing, bool) {
	return stepAt(issue, record.StandsAt(issue))
}

// stepAt is the one place that decides the step issue runs at phase at and whether its runs are
// recorded: the step of at (stepOf), and false for a phase that runs no step — admitted, awaiting
// a person, held, done — or a record key that is no issue key (recordsSteps).
func stepAt(issue record.Issue, at phase.Phase) (standing, bool) {
	place, role, typ, ok := stepOf(at)
	return standing{place: place, role: role, typ: typ, at: at}, ok && recordsSteps(issue)
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
	// ResultStopped is a run that ended under its role with neither: a head that conflicts with
	// its base, or an end no move of the table wrote (stepStopped).
	ResultStopped StepResult = steprecord.ResultStopped
)

// endOf is how a transition on trigger ends the step it moves the issue out of. A move back a role
// or a report asked for — the tester's fail, a review's request for changes, a role's or the
// architect's backward move — ends it failed; a move back nobody asked for, a head that conflicts
// with its base, ends it stopped. Both return the work to the step the issue moves into, which the
// record names. Every other move ends the step passed and returns nowhere.
func endOf(trigger TriggerKind) (result StepResult, returns bool) {
	switch trigger {
	case TriggerTesterFailed, TriggerReviewRejected, TriggerBackward:
		return ResultFailed, true
	case TriggerConflict:
		return ResultStopped, true
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
// writes the session and the model as one read's pair (endStep). The start first ends each run of
// the issue that is over, the run of its own place among them (settleStart), so the end of the run
// the issue leaves, or that its place held, reaches Dispatch before this start. The record never
// makes the start wait: the row reaches Dispatch on a lane of its own (record.LaneRecord). A phase
// that runs no step and a record key that is no issue key are no step its callers find
// (standingStep).
func (e *Engine) startStep(ctx context.Context, tx pgx.Tx, issue record.Issue, step standing, agent claim.Token) error {
	if err := e.settleStart(ctx, tx, issue.Key, step.place); err != nil {
		return err
	}
	// The moment is the one the count of starts keeps, which PostgreSQL holds to the microsecond
	// and the end reads back, so the start and the end write one moment (endStep).
	now := e.now().Truncate(time.Microsecond)
	write, err := e.store.RaiseStepStarts(ctx, tx, issue.Key, step.place, func(starts int64) record.StepRecord {
		write := stepWrite(issue, step.place, step.role, step.typ, string(agent), starts)
		write.StartedAt = new(now)
		return write
	})
	if err != nil {
		return err
	}
	return e.enqueue(ctx, tx, issue.Key, write)
}

// endStep writes the end of the run of the step before stands in (record.StandsAt, so a held issue
// ends the step of the phase its hold is from), before being the issue as it stood in that run. It
// is the one writer of an end, and the fact's settle its one caller (settleRuns), which decides
// that an open run is over. The record is the one its start wrote: the same generation and the
// count of starts unchanged, and the agent and the moment that start wrote, so the two writes agree
// whichever reaches Dispatch first. A run no start of this release counted has an open row from
// the daemon's boot (OpenUncountedRuns), which wrote the count and the agent its start would have
// named, so its end is written the same way. One run gets one end: the end marks the run ended in
// the store, in its own transaction (record.Store.EndStepStart), and an end of a run already
// marked writes nothing. The end carries the moment now, end's fields, and the session and the
// model of one read of the agent now, the only write that names either (startStep). A phase that
// runs no step writes nothing, and neither does a record key that is no issue key (recordsSteps).
func (e *Engine) endStep(ctx context.Context, tx pgx.Tx, before record.Issue, end StepEnd) error {
	step, ok := standingStep(before)
	if !ok {
		return nil
	}
	start, ended, err := e.store.EndStepStart(ctx, tx, before.Key, step.place)
	if err != nil || !ended {
		return err
	}
	agent, startedAt := claim.Token(start.Agent), start.StartedAt
	write := stepWrite(before, step.place, step.role, step.typ, string(agent), start.Starts)
	if !startedAt.IsZero() {
		write.StartedAt = new(startedAt)
	}
	write.EndedAt, write.Result, write.ReturnedTo = new(e.now()), new(string(end.Result)), end.ReturnedTo
	if end.Report != "" {
		write.Report = new(end.Report)
	}
	read := e.readAgent(ctx, tx, agent)
	if read.Session != "" {
		write.Session = new(read.Session)
	}
	if read.Model != "" {
		write.Model = new(read.Model)
	}
	return e.enqueue(ctx, tx, before.Key, write)
}

// reportEndedStep writes report, the handoff a role's completion of at named, onto the record of
// the run of at's step that ended before that completion landed: a review the review event moved
// on, whose end the transition wrote with no report, since the reviewer had not completed yet. A
// record's report is the handoff the ending role's completion named, whichever of the two lands
// first. The run is the latest of at's place, ended, since the issue has left at and no start of
// it came since (handoff's current). The write carries the run's identity and the report alone,
// and Dispatch writes each field once, so it and the end merge in either order
// (record.StepRecord). A completion that names no handoff writes nothing, and neither does a
// place whose latest run is not marked ended (record.RunEnded) — none ran, one is open, or one an
// earlier release started after a rollback, whose record this release has not written — nor a
// record key that is no issue key.
func (e *Engine) reportEndedStep(ctx context.Context, tx pgx.Tx, issue record.Issue, at phase.Phase, report string) error {
	step, ok := stepAt(issue, at)
	if !ok || report == "" {
		return nil
	}
	latest, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
	if err != nil || latest.Run != record.RunEnded {
		return err
	}
	write := stepWrite(issue, step.place, step.role, step.typ, latest.Agent, latest.Starts)
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
// (startAgent, so accepting's is the architect that owns the issue now, acceptingAgent), and the
// moment its phase worker's entry was raised. Accepting's architect raises no entry, and so the
// moment is unknown there. A merge whose merger's entry this visit to merging did not raise is one
// GitHub had already made when the issue reached merging, which started no merger: an entry never
// raised, or one raised before the retro's completion that brought the issue there, the one move
// into merging (the table's retro row), which is an earlier visit's.
func (e *Engine) upgradedStart(ctx context.Context, tx pgx.Tx, issue record.Issue, at phase.Phase) (claim.Token, time.Time, error) {
	worker := phase.RoleFor(at)
	if worker == claim.RoleArchitect {
		agent, err := e.startAgent(ctx, tx, issue, at)
		return agent, time.Time{}, err
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, worker)
	if err != nil {
		return "", time.Time{}, err
	}
	if at == phase.Merging {
		// The retro's completion is recorded on the row of the role that works the retro.
		retro, err := e.phaseRow(ctx, tx, issue.Key, phase.RoleFor(phase.Retro))
		if err != nil {
			return "", time.Time{}, err
		}
		if row.EntryRaisedAt.IsZero() || row.EntryRaisedAt.Before(retro.CompletedAt) {
			return "", time.Time{}, nil
		}
	}
	agent, err := stepClaim(issue, issue.Key, worker)
	return agent, row.EntryRaisedAt.UTC(), err
}

// startAgent is the agent a start of the step issue stands in at names: for accepting, the
// architect that owns issue now (acceptingAgent), nobody for a merge GitHub has already made,
// where no merger starts, and the claim of the phase's worker otherwise.
func (e *Engine) startAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, at phase.Phase) (claim.Token, error) {
	switch at {
	case phase.Accepting:
		return e.acceptingAgent(ctx, tx, issue)
	case phase.Merging:
		standing := issue
		standing.Phase = at
		merged, err := PhaseFinished(ctx, e.store, tx, standing)
		if err != nil || merged {
			return "", err
		}
	}
	return stepClaim(issue, issue.Key, phase.RoleFor(at))
}

// acceptingAgent is the agent of issue's accepting step: the claim of the architect that owns
// issue now, the one that signs it off or moves it back. That is a child's own architect while its
// claim holds its role (owningArchitect, the owner walk over the claims this transaction reads), or
// while a start of it that will run waits in the outbox (ownArchitectStarting), else the nearest
// architect above it whose claim holds its role, else the tree's. It is the one place both a start
// of accepting (startAgent) and the boot's open of a run of accepting no start counted (upgradedStart)
// read, so the record names the architect whose request ends the step, and that request waits for
// that architect's stream (api's apply, record.RunsAStepOn).
func (e *Engine) acceptingAgent(ctx context.Context, tx pgx.Tx, issue record.Issue) (claim.Token, error) {
	starting, err := e.ownArchitectStarting(ctx, tx, issue)
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

// ownArchitectStarting is whether issue's own architect, a child's, owns its accepting step by a
// start the outbox will act on, which the owner walk does not see: the walk reads a claim as the
// store records it, and a claim whose start is only queued does not hold its role yet
// (record.HoldsRole). Two moves queue such a start in the transaction that starts accepting: the
// move into accepting, which starts a failed child architect again (reviveArchitect), and a
// re-admission's promotion, which brings back a member's suspended architect (admit's
// startMidPhaseMembers). Once that start runs, the child's own architect owns the child, and it
// signs it off. The start is one of no phase or of accepting (architectStartQueued).
//
// Two such starts do not count. One queued for a claim the operator suspended: that suspension is
// woken by no start, queued or not, since each waits for the operator's resume
// (supervise.Claim.RuleHolds). And one queued once the issue has left accepting, which the store's
// record of the issue says: the start is then the next phase's (reviveArchitect at that phase's
// start).
func (e *Engine) ownArchitectStarting(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	run, child, err := e.ownArchitectRun(ctx, tx, issue)
	if err != nil || !child || !architectStartQueued(run, phase.Accepting) {
		return false, err
	}
	if run.Claim != nil && (supervise.Claim{State: run.Claim.State, SuspendedBy: run.Claim.SuspendedBy}).RuleHolds() {
		return false, nil
	}
	stands, err := e.store.Issue(ctx, tx, issue.Key)
	if err != nil || stands == nil {
		return false, err
	}
	return record.StandsAt(*stands) == phase.Accepting && stands.Generation == issue.Generation, nil
}

// startNamedStep writes the start of step, the step issue stands in (standingStep), under the agent
// that step names (startAgent), the one place that decides who runs a start no phase worker's entry
// names: a move into accepting, whose role is the architect, for which start starts no worker, or
// into a merge GitHub has already made, which nobody runs and whose close ends it (transition), and
// admission's promotion of a member whose worker it does not start (StartStepRecord).
func (e *Engine) startNamedStep(ctx context.Context, tx pgx.Tx, issue record.Issue, step standing) error {
	agent, err := e.startAgent(ctx, tx, issue, step.at)
	if err != nil {
		return err
	}
	return e.startStep(ctx, tx, issue, step, agent)
}

// stepClaim is the claim token of role on key, in issue's project.
func stepClaim(issue record.Issue, key string, role claim.Role) (claim.Token, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", err
	}
	return claim.NewToken(project, key, role)
}

// stepWrite is the identity of one run of a step of issue: the fields every write of its record
// carries.
func stepWrite(issue record.Issue, place record.StepPlace, role stepRole, typ stepType, agent string, starts int64) record.StepRecord {
	return record.StepRecord{ID: StepID(issue.Key, issue.Generation, place, starts), Task: place.Task,
		Step: place.Place, Kind: place.Kind, Role: string(role), Type: string(typ), Agent: agent, Generation: issue.Generation}
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
// it into to with handoff as the completion's row: endOf's result, the step of to where the work
// goes back, and the handoff record handoff names when its role is the role of from's phase. Its
// one caller names it for the step it found the issue in (nameMoveEnd), so from runs a step, whose
// phase has a role, and a completion of no role names no report.
func endOfTransition(trigger TriggerKind, from, to phase.Phase, handoff record.PhaseRow) StepEnd {
	result, returns := endOf(trigger)
	end := StepEnd{Result: result}
	if into, _, _, ok := stepOf(to); returns && ok {
		end.ReturnedTo = new(into.Place)
	}
	if handoff.Role == phase.RoleFor(from) {
		end.Report = handoff.HandoffRecord
	}
	return end
}

// StartStepRecord writes the start of the step issue stands in, under the agent that step names
// (startNamedStep): admission's promotion of a member whose phase worker it does not start (admit's
// startMidPhaseMembers), whose step starts again all the same. A step whose latest run is still
// open at the issue's own generation goes on, and nothing is written: a role the promotion found
// already started for this run (workflow.StartFor), or a member held from merging whose tree is
// admitted again, whose merge step the linger left open (beginLinger) and whose retry goes on with
// it. A second start there would open a run while the one before it has no end, and the count
// would leave that one none. A run still open at an earlier generation is another run's: a member
// held from merging and then handed over at the linger's close stands at the generation its
// readmit raised, while the settle that ends its old merge runs only at the fact's end, so the
// start goes on, and its own settle (settleStart) ends that old run before it opens the new one.
func (e *Engine) StartStepRecord(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	step, ok := standingStep(issue)
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
	return e.startNamedStep(ctx, tx, issue, step)
}
