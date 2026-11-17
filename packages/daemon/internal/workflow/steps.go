package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// defaultTask is the one task of today's fixed phases: until an issue carries a plan of its own,
// every step of it is a step of this task.
const defaultTask = "default"

// StepAgent is what the daemon reads of a step's agent where it writes the step's record: the
// session its claim records now, and the model that answered it last in that session as
// `<provider>/<model>`. Either is "" where the daemon reads none, and a record leaves out a field
// read empty, so a later write can still fill it (record.StepRecord).
type StepAgent struct {
	Session string
	Model   string
}

// stepOf is the step a phase runs as a step of the task default, whose steps are DefaultSteps
// (stepIn). A held issue's step is the step of the phase its hold is from (record.StandsAt), which
// the caller reads.
func stepOf(p phase.Phase) (place record.StepPlace, step int, role, typ string, ok bool) {
	return stepIn(DefaultSteps(), p)
}

// endOf is how a transition on trigger ends the step it moves the issue out of. A move back a role
// or a report asked for — the tester's fail, a review's request for changes, a role's or the
// architect's backward move — ends it failed; a move back nobody asked for, a head that conflicts
// with its base, ends it stopped. Both return the work to the step the issue moves into, which the
// record names. Every other move ends the step passed and returns nowhere.
func endOf(trigger TriggerKind) (result string, returns bool) {
	switch trigger {
	case TriggerTesterFailed, TriggerReviewRejected, TriggerBackward:
		return "failed", true
	case TriggerConflict:
		return "stopped", true
	}
	return "passed", false
}

// stepRecordNamespace is the namespace of every step record's id.
var stepRecordNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("legion-step-record"))

// StepID is the id of the record of one run of one step: a version 5 uuid of the issue, its run
// (record.Issue.Generation), the task, the kind, the place and the count of starts of that place
// (record.Store.StepStarts). The start of the run raises the count and the end reads it unchanged,
// so the two writes compute one id apart, across a restart too, and the next start of the same
// place, which raises the count again, writes another record.
func StepID(issue string, generation uint64, task, kind string, step int, starts int64) string {
	name := fmt.Sprintf("%s/%d/%s/%s/%d/%d", issue, generation, task, kind, step, starts)
	return uuid.NewSHA1(stepRecordNamespace, []byte(name)).String()
}

// StepEnd is how a step ends: its result (passed, failed or stopped), the step the work returns
// to (nil where it returns nowhere; a retro's or a merge's place is 0) and the handoff record the
// ending role's completion named ("" for none).
type StepEnd struct {
	Result     string
	ReturnedTo *int
	Report     string
}

// stepStopped is the end of a step that no completion and no move of the table ended: the issue
// left the workflow, its tree lingered, or its run ended under it.
var stepStopped = StepEnd{Result: "stopped"}

// startStep writes the start of the step issue stands in (record.StandsAt), run by agent: one more
// start counted at its place (record.Store.RaiseStepStarts), and a step record row, committed with
// the start, carrying every identity field, the moment it started and the session the daemon
// reads of agent. The record never makes the start wait: the row reaches Dispatch on a lane of its
// own (record.LaneRecord). A phase that runs no step writes nothing, and neither does a record key
// that is no issue key (recordsSteps).
func (e *Engine) startStep(ctx context.Context, tx pgx.Tx, issue record.Issue, agent claim.Token) error {
	place, step, role, typ, ok := stepOf(record.StandsAt(issue))
	if !ok || !recordsSteps(issue) {
		return nil
	}
	now := e.now()
	starts, err := e.store.RaiseStepStarts(ctx, tx, issue.Key, place, string(agent), now)
	if err != nil {
		return err
	}
	write := stepWrite(issue, place, step, role, typ, string(agent), starts)
	write.StartedAt = new(now)
	if read := e.readAgent(agent); read.Session != "" {
		write.Session = new(read.Session)
	}
	return e.enqueue(ctx, tx, issue.Key, write)
}

// endStep writes the end of the step before stands in (record.StandsAt, so a held issue ends the
// step of the phase its hold is from), before being the issue as it stood before the write that
// ends the step. The record is the one its start wrote: the same generation and the count of
// starts unchanged, and the agent and the moment that start wrote, so the two writes agree
// whichever reaches Dispatch first. One run gets one end: the end marks the run ended in the
// store, in its own transaction (record.Store.EndStepStart), and an end of a run already marked
// writes nothing, so a run two writes end — the linger's and the hand-over's of one member — keeps
// the first end, and Dispatch never takes the later one for it. A step running when this release
// deployed has no count, and its end writes the record with the count 0, the agent its start would
// have named and, for a phase worker, the moment its role's entry was raised, and marks that run
// ended too (record.Store.EndUncountedStep): the route makes the node from the end alone. The end
// carries the moment now, end's fields, and the session and the model the daemon reads of the
// agent now. A phase that runs no step writes nothing, and neither does a record key that is no
// issue key (recordsSteps).
func (e *Engine) endStep(ctx context.Context, tx pgx.Tx, before record.Issue, end StepEnd) error {
	at := record.StandsAt(before)
	place, step, role, typ, ok := stepOf(at)
	if !ok || !recordsSteps(before) {
		return nil
	}
	start, open, err := e.store.EndStepStart(ctx, tx, before.Key, place)
	if err != nil {
		return err
	}
	agent, startedAt := claim.Token(start.Agent), start.StartedAt
	if !open {
		if agent, startedAt, err = e.upgradedStart(ctx, tx, before, at); err != nil {
			return err
		}
		first, err := e.store.EndUncountedStep(ctx, tx, before.Key, place, string(agent), startedAt)
		if err != nil || !first {
			return err
		}
	}
	write := stepWrite(before, place, step, role, typ, string(agent), start.Starts)
	if !startedAt.IsZero() {
		write.StartedAt = new(startedAt)
	}
	write.EndedAt, write.Result, write.ReturnedTo = new(e.now()), new(end.Result), end.ReturnedTo
	if end.Report != "" {
		write.Report = new(end.Report)
	}
	read := e.readAgent(agent)
	if read.Session != "" {
		write.Session = new(read.Session)
	}
	if read.Model != "" {
		write.Model = new(read.Model)
	}
	return e.enqueue(ctx, tx, before.Key, write)
}

// recordsSteps says whether issue's steps are recorded: its key is an issue key, which every key
// Dispatch gives is, and so names claims (claim.NewToken) and an issue the route can hold. The
// record has keys that are none only where a test wrote them, and those have no claims either,
// as the promotion's own walk passes over them (admit's startMidPhaseMembers).
func recordsSteps(issue record.Issue) bool { return claim.IsIssueKey(issue.Key) }

// upgradedStart is the agent and the moment of the start of a step at that no start of this
// release counted, which ran under the release before: the agent its start would have named
// (startAgent), and the moment its phase worker's entry was raised. Accepting's architect raises
// no entry, and so the moment is unknown there. A merge whose merger's entry was never raised is
// one GitHub had already made when the issue reached merging, which started no merger.
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
	if at == phase.Merging && row.EntryRaisedAt.IsZero() {
		return "", time.Time{}, nil
	}
	agent, err := stepClaim(issue, issue.Key, worker)
	return agent, row.EntryRaisedAt.UTC(), err
}

// startAgent is the agent a start of the step issue stands in at names: the tree architect for
// accepting (open point 4 of the plan: a sub-architect's own claim is runtime state the record does
// not hold), nobody for a merge GitHub has already made, where no merger starts, and the claim of
// the phase's worker otherwise.
func (e *Engine) startAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, at phase.Phase) (claim.Token, error) {
	switch at {
	case phase.Accepting:
		return stepClaim(issue, issue.Tree, claim.RoleArchitect)
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
func stepWrite(issue record.Issue, place record.StepPlace, step int, role, typ, agent string, starts int64) record.StepRecord {
	return record.StepRecord{ID: StepID(issue.Key, issue.Generation, place.Task, place.Kind, step, starts), Task: place.Task,
		Step: step, Kind: place.Kind, Role: role, Type: typ, Agent: agent, Generation: issue.Generation}
}

// readAgent is what the daemon reads of agent now (Config.StepAgent): nothing for no agent or no
// reader.
func (e *Engine) readAgent(agent claim.Token) StepAgent {
	if agent == "" || e.cfg.StepAgent == nil {
		return StepAgent{}
	}
	return e.cfg.StepAgent(agent)
}

// endOfTransition is how a transition on trigger ends the step of from, moving the issue into to
// with handoff as the completion's row: endOf's result, the step of to where the work goes back,
// and the handoff record handoff names when its role is the role of from. A phase that runs no
// step, admitted among them, whose role is the architect's, ends no step and names no report.
func endOfTransition(trigger TriggerKind, from, to phase.Phase, handoff record.PhaseRow) StepEnd {
	result, returns := endOf(trigger)
	end := StepEnd{Result: result}
	if _, _, _, _, ok := stepOf(from); !ok {
		return end
	}
	if _, step, _, _, ok := stepOf(to); returns && ok {
		end.ReturnedTo = new(step)
	}
	if handoff.Role != "" && handoff.Role == phase.RoleFor(from) {
		end.Report = handoff.HandoffRecord
	}
	return end
}

// EndRunningStep ends, stopped, the step before stands in: admission's re-admission of an issue
// whose run ends under it (admit's readmit), and its promotion's end of a member's merge a linger
// left open (admit's startMidPhaseMembers). A run another write ended already gets no second end
// (endStep).
func (e *Engine) EndRunningStep(ctx context.Context, tx pgx.Tx, before record.Issue) error {
	return e.endStep(ctx, tx, before, stepStopped)
}

// StartStepRecord writes the start of the step issue stands in, under the agent that step names
// (startAgent): admission's promotion of a member whose phase worker it does not start (admit's
// startMidPhaseMembers), whose step starts again all the same. A step whose latest run is still
// open goes on, and nothing is written: a member held from merging, whose merge step the linger
// left open (beginLinger) and whose retry goes on with it, or a role the promotion found already
// started for this run (workflow.StartFor). A second start there would open a run while the one
// before it has no end, and the count would leave that one none.
func (e *Engine) StartStepRecord(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	at := record.StandsAt(issue)
	place, _, _, _, ok := stepOf(at)
	if !ok || !recordsSteps(issue) {
		return nil
	}
	latest, err := e.store.StepStarts(ctx, tx, issue.Key, place)
	if err != nil || latest.Starts > 0 && !latest.Ended {
		return err
	}
	agent, err := e.startAgent(ctx, tx, issue, at)
	if err != nil {
		return err
	}
	return e.startStep(ctx, tx, issue, agent)
}
