package replay

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// RowKind is one kind of outbox row as the replay's worker stub tells them apart: its
// record.OutboxKind and, for a supervise row, its op, since a start, a suspend and a close of a
// role are three different acts on the issue's work.
type RowKind struct {
	Kind record.OutboxKind
	Op   record.SuperviseOp
}

// OutboxWay is what the replay's worker stub does with one kind of outbox row, which in a daemon
// an executor runs against something outside it: a worker, Dispatch, GitHub.
type OutboxWay string

const (
	// OutboxExecuted is a row the worker stub executes as the daemon's executor would, against
	// the replay's own state: a start opens the issue's next recorded segment, a stop pauses or
	// cancels it, a status write becomes the Dispatch event its executor's write makes, and a row
	// whose executor applies a fact back into the daemon applies that fact through the chain at
	// the row's own time.
	OutboxExecuted OutboxWay = "executed"
	// OutboxFinished is a row the worker stub finishes without acting, under the assumption
	// RowHandling.Assumption names: its executor changes nothing the replay decides or measures.
	OutboxFinished OutboxWay = "finished"
)

// RowHandling is what the worker stub does with one kind of outbox row: whether it executes the
// row or finishes it, what executing it does and the facts the daemon's executor of it applies
// back into the daemon, or the assumption a finished row rests on.
type RowHandling struct {
	// Way is whether the stub executes the row or finishes it.
	Way OutboxWay
	// Does is what the stub does to execute the row; empty for a row it finishes.
	Does string
	// Facts is every fact, by its type's name in package intake, the daemon's executor of the row
	// applies back into the daemon and the stub applies in its place; none for a row whose
	// executor applies none.
	Facts []string
	// Assumption is the number of the assumption (Assumptions) a finished row rests on; zero for
	// a row the stub executes.
	Assumption int
}

// OutboxHandling is what the replay's worker stub does with each kind of outbox row: every
// record.OutboxKind, and a supervise row by each of its ops. A row of a kind with no entry here
// is neither executed nor finished under an assumption, and the inventory test
// (TestEveryOutboxKindIsExecutedOrFinishedUnderAnAssumption) names it.
var OutboxHandling = map[RowKind]RowHandling{
	{Kind: record.OutboxKindDispatchStatus}: {Way: OutboxExecuted, Facts: []string{"DispatchIssue", "Handover"},
		Does: "behind the executor's fences, read against the replay's copy of Dispatch, writes the status there and applies the event that write makes; a hand-over applies the hand-over"},
	{Kind: record.OutboxKindLingerClose}: {Way: OutboxExecuted, Facts: []string{"LingerExpired"},
		Does: "applies the linger's expiry at the row's next_at"},
	{Kind: record.OutboxKindGateSeed}: {Way: OutboxExecuted, Facts: []string{"DispatchArtifact", "GateAboveDispatch"},
		Does: "reads the registered document's approval from the replay's copy of Dispatch and applies what it finds, or the reset of a gate above Dispatch's version"},
	{Kind: record.OutboxKindDesignChanges}: {Way: OutboxExecuted,
		Does: "reconciles the tree's news of its approved design as the executor does, reading from the replay's copy of Dispatch the version each review history names: records each change set the news names, read with nothing listed from version 0 and unavailable from any other, since the copy holds no document's text, raises a told row past a pair that changed no work, and queues the news-only start of each other row's holder"},
	{Kind: record.OutboxKindSupervise, Op: "start"}: {Way: OutboxExecuted,
		Does: "behind the executor's fences writes the role's claim as the outbox's start writes it, admitted on its first start, with its given entry and a confirmed delivery; a launch implies readiness, clearing a start failure and its time but not the last death; resumes the issue's paused segment or opens its next recorded one; a start queued for news alone, behind the same fences, is finished under assumption 26 where it reaches its claim with news to tell"},
	{Kind: record.OutboxKindSupervise, Op: "suspend"}: {Way: OutboxExecuted,
		Does: "suspends the role's claim, its task taken back, and pauses the issue's segment it works, or cancels it where the issue has left the workflow"},
	{Kind: record.OutboxKindSupervise, Op: "step_stop"}: {Way: OutboxExecuted,
		Does: "suspends the claim of the step a stopped task ran, as a suspend does and as the daemon's own executor runs it, its session kept and the issue's segment paused"},
	{Kind: record.OutboxKindSupervise, Op: "issue_close"}: {Way: OutboxExecuted,
		Does: "retires the role's claim of a closed issue and cancels the segments it works"},
	{Kind: record.OutboxKindSupervise, Op: "tree_close"}: {Way: OutboxExecuted,
		Does: "retires the role's claim of a tree whose linger expired and cancels the segments it works"},
	{Kind: record.OutboxKindSupervise, Op: "retire_task"}: {Way: OutboxExecuted,
		Does: "pauses the issue's segment the role works, keeping the rest of its length, and ends its turn, as the move into awaiting_children that wrote the row stops the phase (cybermonklabs/legion#4592); the claim keeps its state, and the return's start resumes the segment"},
	{Kind: record.OutboxKindSupervise, Op: "release"}:    {Way: OutboxFinished, Assumption: assumeWorkspaceInSegment},
	{Kind: record.OutboxKindSupervise, Op: "pause_stop"}: {Way: OutboxFinished, Assumption: assumeWorkspaceInSegment},

	{Kind: record.OutboxKindDispatchMessage}:  {Way: OutboxFinished, Assumption: assumeNoticesChangeNothing},
	{Kind: record.OutboxKindNotice}:           {Way: OutboxFinished, Assumption: assumeNoticesChangeNothing},
	{Kind: record.OutboxKindControllerNotice}: {Way: OutboxFinished, Assumption: assumeNoticesChangeNothing},
	{Kind: record.OutboxKindRoleMessage}:      {Way: OutboxFinished, Assumption: assumeNoticesChangeNothing},
	{Kind: record.OutboxKindHookPrompt}:       {Way: OutboxFinished, Assumption: assumeNoticesChangeNothing},
	{Kind: record.OutboxKindUpdateAsk}:        {Way: OutboxFinished, Assumption: assumeNoticesChangeNothing},
	{Kind: record.OutboxKindMergeForward}:     {Way: OutboxFinished, Assumption: assumeNoticesChangeNothing},
	{Kind: record.OutboxKindPhaseEnded}:       {Way: OutboxFinished, Assumption: assumeNoticesChangeNothing},

	{Kind: record.OutboxKindPendingBasisSeed}: {Way: OutboxFinished, Assumption: assumeNoBasis},
	{Kind: record.OutboxKindReparentSeed}:     {Way: OutboxFinished, Assumption: assumeNoBasis},

	{Kind: record.OutboxKindGatedStepsPublish}:  {Way: OutboxFinished, Assumption: assumePublicationsDecideNothing},
	{Kind: record.OutboxKindDesignGatesPublish}: {Way: OutboxFinished, Assumption: assumePublicationsDecideNothing},
	{Kind: record.OutboxKindRoleHoldersPublish}: {Way: OutboxFinished, Assumption: assumePublicationsDecideNothing},
	// The variant is part of the step publication, not a second effect that the replay executes.
	{Kind: record.OutboxKindStepRecord}: {Way: OutboxFinished, Assumption: assumePublicationsDecideNothing},
	{Kind: record.OutboxKindPlanRecord}: {Way: OutboxFinished, Assumption: assumePublicationsDecideNothing},

	{Kind: record.OutboxKindIssueBranch}:     {Way: OutboxFinished, Assumption: assumeWorkspaceInSegment},
	{Kind: record.OutboxKindIssueSuspend}:    {Way: OutboxFinished, Assumption: assumeWorkspaceInSegment},
	{Kind: record.OutboxKindIssueBundle}:     {Way: OutboxFinished, Assumption: assumeWorkspaceInSegment},
	{Kind: record.OutboxKindIssueRelease}:    {Way: OutboxFinished, Assumption: assumeWorkspaceInSegment},
	{Kind: record.OutboxKindWorkspaceRemove}: {Way: OutboxFinished, Assumption: assumeWorkspaceInSegment},
	{Kind: record.OutboxKindClaimRelease}:    {Way: OutboxFinished, Assumption: assumeWorkspaceInSegment},

	{Kind: record.OutboxKindMergeQueueRead}:    {Way: OutboxFinished, Assumption: assumeNoGitHub},
	{Kind: record.OutboxKindMergeQueueRemoval}: {Way: OutboxFinished, Assumption: assumeNoGitHub},
	{Kind: record.OutboxKindQueueTimelineRead}: {Way: OutboxFinished, Assumption: assumeNoGitHub},
	{Kind: record.OutboxKindConflictConfirm}:   {Way: OutboxFinished, Assumption: assumeNoGitHub},

	// Superseded check wakes and their replacements are both left out under the same assumption.
	{Kind: record.OutboxKindCheckWake}:  {Way: OutboxFinished, Assumption: assumeNoProductionCheck},
	{Kind: record.OutboxKindCheckLook}:  {Way: OutboxFinished, Assumption: assumeNoProductionCheck},
	{Kind: record.OutboxKindCheckBound}: {Way: OutboxFinished, Assumption: assumeNoProductionCheck},
}

// outboxLease is how long the stub leases a row it takes, as the daemon's lanes lease one
// (internal/daemon's outboxLease): the stub finishes every row it takes before it takes the next,
// so no lease it takes is ever live when another row of the issue is listed.
const outboxLease = 30 * time.Second

// replaySession is the session a claim the stub launches runs where the record shows no session
// running that claim (sessionOf): the replay's workers are the record's segments.
const replaySession = "replay"

// sessionOf is the session c runs once the stub launches it now: the one the record shows running
// that claim at the clock's instant (IssueTrace.sessionAt), so the daemon's reads that compare a
// session with a claim's — the holder of a Dispatch claim, the author of an ask, the writer of a
// status — answer as they did in the record; replaySession where the record shows none. It reads
// the instant rather than the clock's next moment, which moves the clock (simClock.Now).
func (d *driver) sessionOf(c supervise.Claim) string {
	if iss, ok := d.trace.Issues[c.Issue]; ok {
		if session := iss.sessionAt(c.Role, d.clock.Instant()); session != "" {
			return session
		}
	}
	return replaySession
}

// dueSlack is how far past the clock's moment the stub reads a row as due. The simulated clock
// moves a microsecond at each read within one instant (simClock), so a moment a pass wrote from
// its clock — a linger's close, two hours after the leave — stands those few microseconds after the
// moment the record shows; the stub wakes at the millisecond (wake) and takes a row due within
// the slack of it, so the act the row brings happens at the record's moment.
const dueSlack = time.Millisecond

// dueNow is the moment the stub reads the outbox's rows at: the clock's next moment and the slack.
func (d *driver) dueNow() time.Time { return d.now().Add(dueSlack) }

// drain runs the worker stub between facts, on connections of its own and never inside a handler:
// the outbox rows the daemon's lanes would take now, in the store's own order and under its own
// rules (record.Postgres.DueRows: due, in order, one status write of an issue at a time, a start
// behind an older issue_branch, a start that waits for a slot not before it holds one, nothing a
// pause holds), but a start a wait holds under the run's rule (waitHolds), one at a time and
// listed again after each, since a row run changes what is due, until none is due; then a wake at
// the next row that comes due later (wake). A start a wait holds stays due, and the drain after
// the fact whose pass ends the wait takes it. Within one listing nothing changes the slots until a
// row is taken, which ends the listing, so the slots a wait holds are read once in it, at its first
// start that may be held.
func (d *driver) drain() error {
	for {
		var due []record.OutboxRow
		if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			due, err = d.records.DueRows(ctx, tx, d.window.Project, "", nil, d.dueNow())
			return err
		}); err != nil {
			return fmt.Errorf("replay: list the outbox rows due at %s: %w", stamp(d.clock.Instant()), err)
		}
		ran := false
		var waiting map[string]bool
		for _, row := range due {
			held, err := d.waitHolds(row, &waiting)
			if err != nil {
				return err
			}
			if held {
				continue
			}
			took, err := d.take(row)
			if err != nil {
				return err
			}
			if took {
				ran = true
				break
			}
		}
		if !ran {
			return d.wake()
		}
	}
}

// waitHolds says whether row is a start that the wait of its issue on another issue holds under
// the run's rule. Under a rule whose start may take a cover (record.TakeCover, the rule of the
// tool's spec at version 4) a started issue keeps its slot through a wait, and the wait holds the
// start of its next phase worker by a hold of its own, which reads the wait and not the slot, while
// the phase under way goes on; no release of the daemon holds a start so, so the stub holds it in
// the lanes' place. The start held is one that waits for a slot (record.SuperviseRequest.NeedsSlot,
// a phase worker's) and brings its worker a phase entry its claim has not been given, of an issue
// whose slot the admission pass marks waiting (record.Slot.WaitsSince: admit's markSlotWaits over
// blockers.KeptWaits, the waits a cover reads). A covered waiter holds no slot, so the lanes' own
// rule holds its start. Under every other rule nothing is held here. waiting is the issues whose
// slot is marked waiting, read the first time a start needs them where it is nil.
func (d *driver) waitHolds(row record.OutboxRow, waiting *map[string]bool) (bool, error) {
	if d.rule.Take != record.TakeCover || row.Kind != record.OutboxKindSupervise {
		return false, nil
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		// The row's own run reports what it cannot decode (take).
		return false, nil
	}
	start, ok := payload.(record.SuperviseRequest)
	if !ok || start.Op != "start" || !start.NeedsSlot || start.Entry <= 0 {
		return false, nil
	}
	token, err := claim.NewToken(d.token, row.Issue, start.Role)
	if err != nil {
		return false, err
	}
	if c, found := d.claims[token]; found && start.Entry <= c.GivenEntry {
		return false, nil
	}
	if *waiting == nil {
		read := map[string]bool{}
		if err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			slots, err := d.records.Slots(ctx, tx)
			for _, slot := range slots {
				if slot.WaitsSince != nil {
					read[slot.Issue] = true
				}
			}
			return err
		}); err != nil {
			return false, fmt.Errorf("replay: read whether a wait holds the start row %d of %s: %w", row.ID, row.Issue, err)
		}
		*waiting = read
	}
	return (*waiting)[row.Issue], nil
}

// wake queues the stub's next run at the earliest next_at of the project's rows that come due
// after now, as a lane sleeps until its next row is due (record.Postgres.NextDue, asked of every
// issue of the project, since the stub runs every lane): a linger's close, a start backing off; at
// its millisecond, which the slack reads it due at (dueSlack). Nothing is queued where a wake at
// that moment or earlier already is.
func (d *driver) wake() error {
	var next time.Time
	var due bool
	now := d.dueNow()
	if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		next, due, err = d.records.NextDue(ctx, tx, d.window.Project, "", nil, now)
		return err
	}); err != nil {
		return fmt.Errorf("replay: read when the outbox is next due after %s: %w", stamp(now), err)
	}
	if !due {
		return nil
	}
	at := next.UTC().Truncate(time.Millisecond)
	if !d.wakeAt.IsZero() && !d.wakeAt.After(at) && d.wakeAt.After(d.clock.Instant()) {
		return nil
	}
	d.wakeAt = at
	d.push(&item{at: at, class: classWake, rec: at, run: func() error {
		if d.wakeAt.Equal(at) {
			d.wakeAt = time.Time{}
		}
		return nil
	}})
	return nil
}

// take leases row, if a lane may still run it now (record.Postgres.ClaimRow), and runs it as its
// kind's handling says (OutboxHandling). It answers whether it took the row.
func (d *driver) take(row record.OutboxRow) (bool, error) {
	var leased record.OutboxRow
	var ok bool
	if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		leased, ok, err = d.records.ClaimRow(ctx, tx, row.ID, d.dueNow(), outboxLease)
		return err
	}); err != nil || !ok {
		return false, err
	}
	payload, err := record.DecodeOutboxPayload(leased)
	if err != nil {
		return true, fmt.Errorf("replay: decode outbox row %d: %w", leased.ID, err)
	}
	switch value := payload.(type) {
	case record.StatusWrite:
		err = d.statusRow(leased, value)
	case record.LingerClose:
		err = d.lingerRow(leased, value)
	case record.GateSeed:
		err = d.gateSeedRow(leased, value)
	case record.SuperviseRequest:
		err = d.superviseRow(leased, value)
	case record.DesignNewsReconcile:
		err = d.designChangesRow(leased)
	default:
		err = d.finishUnder(leased, RowKind{Kind: leased.Kind})
	}
	if err != nil {
		return true, fmt.Errorf("replay: run outbox row %d (%s of %s) at %s: %w", leased.ID, leased.Kind, leased.Issue,
			stamp(d.clock.Instant()), err)
	}
	return true, nil
}

// finish deletes row, the stub's run of it over (record.Postgres.FinishOutbox).
func (d *driver) finish(row record.OutboxRow) error {
	return store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		return d.records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
	})
}

// finishUnder finishes row without acting, under the assumption its kind's handling names, and
// counts it.
func (d *driver) finishUnder(row record.OutboxRow, kind RowKind) error {
	handling, handled := OutboxHandling[kind]
	if !handled || handling.Way != OutboxFinished {
		return fmt.Errorf("the stub holds no way to finish a row of %s %s", kind.Kind, kind.Op)
	}
	return d.finishAssumed(row, handling.Assumption)
}

// finishAssumed finishes row without acting, under assumption, and counts it: the way of a row
// its kind's handling finishes (finishUnder), and of a start queued for news alone that reaches
// its claim, which a start's handling executes (start).
func (d *driver) finishAssumed(row record.OutboxRow, assumption int) error {
	d.counters.Finished[assumption]++
	d.used[assumption] = true
	return d.finish(row)
}

// retry gives row back to the outbox to run again on its backoff, as a lane gives back a row
// whose executor waits on something (record.RetryBackoff): a first start of a child's own
// architect while its tree lingers, a merger behind its tree's closed design gate or behind news
// of its approved design not yet recorded, a claim its tree's lifecycle refuses.
func (d *driver) retry(row record.OutboxRow, why error) error {
	next := d.clock.Instant().Add(record.RetryBackoff(row.Attempts))
	if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		return d.records.RetryOutbox(ctx, tx, row.ID, row.LeaseToken, next, why.Error())
	}); err != nil {
		return err
	}
	d.counters.Retried++
	return nil
}

// lingerRow runs a linger's close at its next_at: the linger's expiry (intake.LingerExpired), as
// the daemon's executor applies it.
func (d *driver) lingerRow(row record.OutboxRow, payload record.LingerClose) error {
	if _, err := d.apply("outbox", fmt.Sprintf("linger:%s:%d", row.Issue, payload.Generation),
		intake.LingerExpired{Issue: row.Issue, Generation: payload.Generation}); err != nil {
		return err
	}
	return d.finish(row)
}

// gateSeedRow runs a gate seed as the daemon's executor does (internal/daemon's seedGate), reading
// the registered document's approval from the replay's copy of Dispatch: approved on its latest
// version, the gate opens there; changes requested on it, it closes with the reason; a later
// version moves the gate to it; a document below the seeded version resets the gate to the
// version Dispatch holds (intake.GateAboveDispatch). A read that changes nothing applies nothing.
// A document the window holds no event of, which a registration names all the same (a version
// written before the inputs reach, or one a deployment that gates no step writes no event for), is
// read at the version the registration names: the architect registered the version it wrote.
func (d *driver) gateSeedRow(row record.OutboxRow, payload record.GateSeed) error {
	read := d.shadow.approvalOf(row.Issue, payload.ArtifactID)
	latest := read.latest
	if latest == 0 {
		latest = payload.Version
	}
	fact := intake.DispatchArtifact{Key: row.Issue, ArtifactID: payload.ArtifactID, Kind: intake.DispatchArtifactVersion,
		Version: latest, RequestWaitsOnAgent: read.ask != "" && read.waitingOn == "agent"}
	switch {
	case read.approved != nil:
		fact.Kind, fact.Mark, fact.Actor = intake.DispatchArtifactApproved, read.approved.mark, read.approved.actor
	case read.changes != nil:
		reason := read.changes.reason
		if reason == "" {
			reason = "Dispatch reports changes requested"
		}
		fact.Kind, fact.Reason, fact.Mark, fact.Actor = intake.DispatchArtifactChangesRequested, reason, read.changes.mark, read.changes.actor
	case latest == payload.Version:
		return d.finish(row)
	}
	if latest < payload.Version {
		id := fmt.Sprintf("gate-above:%s:%d:%s:%s:%d:%s:%d:%d", row.Issue, payload.Generation, payload.Step, payload.ArtifactID,
			payload.Version, fact.Kind, fact.Version, fact.Mark.ID())
		if _, err := d.apply("outbox", id, intake.GateAboveDispatch{Generation: payload.Generation, Above: payload.Version, Read: fact}); err != nil {
			return err
		}
		return d.finish(row)
	}
	id := fmt.Sprintf("gate-seed:%s:%d:%s:%s:%s:%d:%d", row.Issue, payload.Generation, payload.Step, payload.ArtifactID, fact.Kind,
		fact.Version, fact.Mark.ID())
	if fact.Kind == intake.DispatchArtifactVersion && fact.RequestWaitsOnAgent {
		id += ":with-agent"
	}
	if _, err := d.apply("outbox", id, fact); err != nil {
		return err
	}
	return d.finish(row)
}

// statusRow runs a status write as the daemon's executor does (internal/daemon's status and the
// rule of its kind), every fence ported against the replay's copy of Dispatch (the shadow): a
// person's hand-over stands only while the label does; a hand-over of a closed tree's child is
// fenced on its hand-over record and its root's linger, hands the child over (intake.Handover),
// and writes the status with the label; the daemon's own close stands only at the generation it
// closed; and every kind finishes without writing where somebody moved the issue since the row
// read it, or where Dispatch already shows the status. A write the fences let through is the
// shadow's status write, whose event the replay applies at once as the Dispatch event it makes.
// The reason a write carries is posted nowhere: no measure or decision reads it.
func (d *driver) statusRow(row record.OutboxRow, payload record.StatusWrite) error {
	iss, ok := d.shadow.issues[row.Issue]
	if !ok || iss.payload == nil {
		// Dispatch holds no such issue, and its executor's read is refused.
		return d.finish(row)
	}
	read, handedOver := iss.status(), dispatch.CarriesLegionLabel(iss.labels())
	moved := read != payload.ObservedStatus && read != payload.Status
	switch payload.Kind {
	case record.PersonHandoverStatusRow:
		if !moved && !handedOver {
			return d.finish(row)
		}
	case record.HandOverStatusRow:
		return d.handOverRow(row, payload, read, handedOver, moved)
	case record.CloseStatusRow:
		if payload.CloseGeneration != 0 {
			issue, err := d.recorded(row.Issue)
			if err != nil {
				return err
			}
			if issue != nil && issue.Generation != payload.CloseGeneration {
				return d.finish(row)
			}
		}
	}
	if moved || read == payload.Status {
		return d.finish(row)
	}
	if err := d.writeStatus(row.Issue, payload.Status); err != nil {
		return err
	}
	return d.finish(row)
}

// handOverRow runs a hand-over's status write (internal/daemon's handOverRule): a move that
// superseded it drops its fence; a fence that no longer holds writes nothing; otherwise the child
// is written todo with the label where Dispatch does not show it so already, and handed over.
func (d *driver) handOverRow(row record.OutboxRow, payload record.StatusWrite, read string, handedOver, moved bool) error {
	if moved {
		if err := d.serialized(func(ctx context.Context, tx pgx.Tx) error {
			return d.records.DeleteHandoverAt(ctx, tx, row.Issue, payload.HandOverTree, payload.HandOverLinger)
		}); err != nil {
			return err
		}
		return d.finish(row)
	}
	holds, err := d.handOverHolds(row, payload)
	if err != nil || !holds {
		if err != nil {
			return err
		}
		return d.finish(row)
	}
	handOver := func() error {
		_, err := d.apply("outbox", fmt.Sprintf("handover:%s:%d", row.Issue, row.ID),
			intake.Handover{Issue: row.Issue, Tree: payload.HandOverTree, Generation: payload.HandOverLinger})
		return err
	}
	if read == payload.Status && handedOver {
		if err := handOver(); err != nil {
			return err
		}
		return d.finish(row)
	}
	raw, wrote, err := d.shadow.statusWrite(row.Issue, payload.Status, true, d.clock.Instant())
	if err != nil {
		return err
	}
	if err := handOver(); err != nil {
		return err
	}
	if wrote {
		if err := d.applyEcho(row.Issue, raw); err != nil {
			return err
		}
	}
	return d.finish(row)
}

// handOverHolds says whether a hand-over's fence still holds, as the daemon's executor reads it
// (internal/daemon's handOverHolds, by record.HandOverHolds): the child still in the tree the row
// names, its hand-over record of that tree and linger uncancelled, and the tree's root lingering at
// that generation. A cancelled record is dropped on the way.
func (d *driver) handOverHolds(row record.OutboxRow, payload record.StatusWrite) (bool, error) {
	var issue, root *record.Issue
	var handover *record.Handover
	if err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if issue, err = d.records.Issue(ctx, tx, row.Issue); err != nil || issue == nil {
			return err
		}
		if handover, err = d.records.Handover(ctx, tx, row.Issue); err != nil || handover == nil || handover.Cancelled {
			return err
		}
		root, err = d.records.Issue(ctx, tx, handover.Tree)
		return err
	}); err != nil {
		return false, fmt.Errorf("read the hand-over of %s: %w", row.Issue, err)
	}
	if handover != nil && handover.Cancelled {
		if err := d.serialized(func(ctx context.Context, tx pgx.Tx) error {
			return d.records.DeleteHandoverAt(ctx, tx, row.Issue, payload.HandOverTree, payload.HandOverLinger)
		}); err != nil {
			return false, err
		}
	}
	return record.HandOverHolds(issue, root, handover, payload), nil
}

// writeStatus is the daemon's write of key's status to Dispatch, made in the shadow, and the event
// that write makes applied at once through the decoder and the chain (applyEcho).
func (d *driver) writeStatus(key, status string) error {
	raw, wrote, err := d.shadow.statusWrite(key, status, false, d.clock.Instant())
	if err != nil || !wrote {
		return err
	}
	return d.applyEcho(key, raw)
}

// applyEcho applies raw, the event a write of the replay's own made in the shadow (a status write,
// or a move of a root's rank, rerank), as the Dispatch consumer applies the event that write makes:
// decoded, prepared and applied as its id.
func (d *driver) applyEcho(key string, raw json.RawMessage) error {
	var head struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("read the event the replay wrote of %s: %w", key, err)
	}
	fact, _, err := intake.DecodeDispatchEvent(d.window.Project, key, raw)
	if err != nil || fact == nil {
		return err
	}
	prepared, recorded, err := d.prepare(fact)
	if err != nil {
		return err
	}
	d.countFence(prepared, recorded)
	_, err = d.apply("dispatch", "dispatch-"+strconv.FormatInt(head.ID, 10), prepared)
	return err
}

// superviseRow runs a supervise row as the daemon's executor does (internal/daemon's supervise),
// its fences first: a pause's stop and an idle pod's release are finished under the assumption of
// a pod's own work; a row of an issue the replay does not record finishes; a close of a tree runs
// on its linger's fence and a stop of a tree the issue has left on that tree; every other row
// finishes where it serves an earlier generation, a phase the issue has left or an entry its role
// has left, or names another tree.
func (d *driver) superviseRow(row record.OutboxRow, payload record.SuperviseRequest) error {
	if payload.Op == "pause_stop" || payload.Op == "release" {
		return d.finishUnder(row, RowKind{Kind: record.OutboxKindSupervise, Op: payload.Op})
	}
	issue, err := d.recorded(row.Issue)
	if err != nil {
		return err
	}
	if issue == nil {
		return d.finish(row)
	}
	token, err := claim.NewToken(d.token, row.Issue, payload.Role)
	if err != nil {
		return err
	}
	switch {
	case payload.Op == "tree_close":
		return d.closeTree(row, payload, token)
	case payload.Op == "retire_task":
		return d.retireStoppedTask(row, payload, *issue)
	case payload.Op == "suspend" && payload.Tree != issue.Tree:
		c, found := d.claims[token]
		if !found || c.Tree != payload.Tree {
			return d.finish(row)
		}
		return d.suspend(row, payload, *issue, token)
	case payload.CheckLook > 0:
		// A production check's look: no check runs in a replay (assumption 30).
		return d.finishUnder(row, RowKind{Kind: record.OutboxKindCheckLook})
	}
	if payload.Generation != issue.Generation || payload.Phase != "" && payload.Phase != issue.Phase || payload.Tree != issue.Tree {
		return d.finish(row)
	}
	if payload.Entry > 0 && payload.Phase != "" {
		entry, err := d.phaseEntry(row.Issue, payload.Role)
		if err != nil {
			return err
		}
		if payload.Entry < entry {
			return d.finish(row)
		}
	}
	switch payload.Op {
	case "start":
		return d.start(row, payload, *issue, token)
	case "suspend", "step_stop":
		// A step stop is the suspend of the process of the step a stopped task ran (workflow's
		// stopTask), which the daemon's own executor runs as a suspend, so the stub runs it as
		// one too.
		return d.suspend(row, payload, *issue, token)
	case "issue_close":
		c, found := d.claims[token]
		if !found || !workflow.StopActs(row.ID, payload, c.LastStartRow, nil) {
			return d.finish(row)
		}
		return d.retireClaim(row, token)
	}
	return fmt.Errorf("outbox row %d has unknown supervise operation %q", row.ID, payload.Op)
}

// phaseEntry is the entry role's phases row of issue names (record.PhaseRow.Entry).
func (d *driver) phaseEntry(issue string, role claim.Role) (int64, error) {
	var entry int64
	err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := d.records.Phases(ctx, tx, issue)
		if err != nil {
			return err
		}
		entry = record.PhaseRowOf(rows, role).Entry
		return nil
	})
	return entry, err
}

// start runs a start row as the outbox's start does (internal/daemon's supervise, op start):
// finished without acting for an issue out of the workflow or a member of a lingering tree, but
// the first start of a child's own architect, which waits on its backoff while its root lingers.
// Then what the start reads of the news of the approved design, in internal/daemon's startNewsOf's
// order: a start queued for news alone (record.SuperviseRequest.News) that reaches no claim
// (newsOnlyReaches) finishes without acting; a merger's start waits while its tree's design gate
// is closed (gateClosed) or its member is owed news no start can carry yet
// (mergerNewsUnrecorded); and a start queued for news alone finishes without acting where its news
// says nothing now (creditNews), waits on its backoff while its claim holds a task, as the claim's
// machine refuses a task of news alone behind any task (supervise's queue), and is otherwise
// finished under assumption 26, since it hands the worker a task of news and no work: the replay
// credits that news (creditNews), but writes nothing to its claim and launches no claim for it.
// Then the claim is written as the start and the turn's confirmation write it: admitted on its
// first start, re-pointed at the issue's tree where it stands in another, launched again where it
// was queued, suspended, failed or retired (`supervise: launched`), and given the row's task unless
// it holds that phase's task already or was given the row's entry (supervise's queue), with a
// confirmed delivery, in one transaction with the row's finish; a task of an entry given so carries
// the news its claim is owed, as the daemon's start attaches it (newsForStart), which the replay
// credits (creditNews). Last, the work the start brings: the issue's paused segment of the role
// resumes; a start whose entry is above the one the claim was given, or that resends its entry,
// opens the issue's next recorded segment of the role (and an issue the record never started takes
// the never-started scenario's work); a start that re-sends an entry already given is counted as a
// reassertion. The settling of a root architect's claim whose root handed work to a person is told
// the pass (Settled).
func (d *driver) start(row record.OutboxRow, payload record.SuperviseRequest, issue record.Issue, token claim.Token) error {
	c, found := d.claims[token]
	if payload.WakeKind != "" && found && c.State == supervise.StateSuspended && c.SuspendedBy == supervise.SuspendedByDaemon {
		return d.finish(row)
	}
	if record.OutOfWorkflow(issue.Status) {
		return d.finish(row)
	}
	root, err := d.recorded(issue.Tree)
	if err != nil {
		return err
	}
	if root != nil && root.Lingers() {
		if !found && payload.Role == claim.RoleArchitect && !claim.IsTreeRoot(issue.Key, issue.Tree) && payload.Phase == "" && payload.Task == "" &&
			!payload.News {
			return d.retry(row, fmt.Errorf("the first start of the own architect of %s waits for the tree of %s to run again", issue.Key, root.Key))
		}
		return d.finish(row)
	}
	if payload.News {
		entry, err := d.phaseEntry(row.Issue, payload.Role)
		if err != nil {
			return err
		}
		if !newsOnlyReaches(c, found, entry) {
			return d.finish(row)
		}
	}
	if payload.Role == claim.RoleMerger {
		closed, err := d.gateClosed(issue)
		if err != nil {
			return err
		}
		if closed {
			return d.retry(row, fmt.Errorf("the merger of %s waits for the design gate of %s to open", issue.Key, issue.Tree))
		}
		unrecorded, err := d.mergerNewsUnrecorded(issue)
		if err != nil {
			return err
		}
		if unrecorded {
			return d.retry(row, fmt.Errorf("the merger of %s waits for what changed in the approved design of %s to be recorded", issue.Key, issue.Tree))
		}
	}
	if payload.News {
		says, err := d.creditNews(issue, payload.Role, token, false)
		switch {
		case err != nil:
			return err
		case !says:
			return d.finish(row)
		case supervise.HoldsTask(c.Pending):
			return d.retry(row, fmt.Errorf("the task of news alone of %s's %s waits behind the task its claim holds", issue.Key, payload.Role))
		}
		if _, err := d.creditNews(issue, payload.Role, token, true); err != nil {
			return err
		}
		return d.finishAssumed(row, assumeNoticesChangeNothing)
	}
	if !found || c.Tree != issue.Tree {
		admit := c
		if !found {
			admit = supervise.Claim{Token: token, Project: d.token, Tree: issue.Tree, Issue: issue.Key, Role: payload.Role,
				State: supervise.StateQueued}
		}
		admit.Tree = issue.Tree
		admitted, err := d.st.AdmitClaim(d.ctx, admit)
		if err != nil {
			return d.retry(row, err)
		}
		c = admitted
	}
	given, state := c.GivenEntry, c.State
	if state == supervise.StateFailed && payload.RuleResume {
		return d.finish(row)
	}
	c.LastStartRow = row.ID
	if c.RunsNothing() {
		d.launch(&c, state == supervise.StateSuspended)
	}
	ranNothing := state == supervise.StateFailed || state == supervise.StateRetired
	delivered := false
	if (payload.ResumeTask || ranNothing) && c.Pending != nil && payload.Phase != "" &&
		c.Pending.StillWorked(payload.Generation, payload.Phase, c.State) {
		if payload.Entry > 0 && c.Pending.Entry == payload.Entry && payload.Entry > c.GivenEntry {
			c.GivenEntry = payload.Entry
		}
	} else if payload.Task != "" && !queuedAlready(c, row.ID, payload) {
		now := d.now()
		if c.Pending != nil {
			// The task the claim held was taken by a turn, or is superseded: the newer one takes
			// its place (supervise's queue), the older retired as a turn retires it.
			c = retired(c)
		}
		kept, err := json.Marshal(payload.Kept)
		if err != nil {
			return err
		}
		if payload.Kept == nil {
			kept = nil
		}
		c.Pending = &supervise.Delivery{ID: fmt.Sprintf("outbox:%d", row.ID), Task: payload.Task, Phase: payload.Phase,
			Generation: payload.Generation, Entry: payload.Entry, Kept: kept, RoundNamed: payload.RoundNamed,
			QueuedAt: now, DeliveredAt: now, ConfirmedAt: now, Prompted: true}
		c.DeliveredRows = append(slices.Clone(c.DeliveredRows), row.ID)
		if payload.Entry > 0 {
			c.GivenEntry = payload.Entry
			c.Handshake = supervise.EntryHandshake{Entry: payload.Entry, Generation: payload.Generation, QueuedAt: now, ConfirmedAt: now}
		}
		delivered = true
	}
	if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if c.Pending != nil {
			err = d.st.PutClaimAndDeliveryTx(ctx, tx, c, *c.Pending)
		} else {
			err = d.st.PutClaimTx(ctx, tx, c)
		}
		if err != nil {
			return err
		}
		return d.records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
	}); err != nil {
		return fmt.Errorf("write the claim %s of the start row %d: %w", token, row.ID, err)
	}
	d.claims[token] = c
	d.used[assumeSupervisionSettles] = true
	if delivered && payload.Entry != 0 {
		if _, err := d.creditNews(issue, payload.Role, token, true); err != nil {
			return err
		}
	}
	if err := d.startWork(issue, payload, given, delivered); err != nil {
		return err
	}
	return d.settled(issue.Key, payload.Role)
}

// queuedAlready says whether c was given the work of payload's start already (supervise's
// Machine.given): an entry at or below the one the claim was given, unless the start resends it,
// or, for a start of no entry, the row itself delivered once.
func queuedAlready(c supervise.Claim, row int64, payload record.SuperviseRequest) bool {
	if payload.Entry > 0 && !payload.ResendEntry {
		return payload.Entry <= c.GivenEntry
	}
	return slices.Contains(c.DeliveredRows, row)
}

// launch brings c's worker up as the supervisor's launch does: the next launch of the claim, its
// failure and suspension cleared, ready to be prompted, running the session the record shows
// running the claim now (sessionOf), and the line the launch writes (`supervise: launched`).
// resumed says whether the launch resumes a suspended claim's session.
func (d *driver) launch(c *supervise.Claim, resumed bool) {
	c.Generation++
	c.State, c.SuspendedBy, c.Session = supervise.StateIdle, "", d.sessionOf(*c)
	c.Failure, c.FailureCause, c.FailedAt = "", "", time.Time{}
	c.LastReady = d.now()
	c.LastStartFailure, c.LastStartFailureAt = "", time.Time{}
	resumedBy := "none"
	if resumed {
		resumedBy = "id"
	}
	d.log.Info(launchedLine, "claim", string(c.Token), "generation", c.Generation,
		"incarnation", fmt.Sprintf("replay%08x", c.Generation), "resumed", resumed, "resumedBy", resumedBy)
}

// retired is c with its pending task retired as the end of the turn that worked it retires it
// (supervise's retirePending): what it was left serving read by supervise's own rule where the task
// was confirmed (supervise.Claim's ServingRun, ServingEntry and ServingCheck), and the entry it was
// given put back to the one it served where it was not.
func retired(c supervise.Claim) supervise.Claim {
	p := c.Pending
	if p == nil {
		return c
	}
	switch {
	case !p.ConfirmedAt.IsZero():
		c.ServingGeneration, c.ServedEntry, c.ServingCheckLook = c.ServingRun(), c.ServingEntry(), c.ServingCheck()
	case p.Entry != 0 && p.Entry == c.GivenEntry:
		c.GivenEntry = c.ServedEntry
	}
	c.Pending = nil
	c.Budgets.Deaths = 0
	return c
}

// startWork is the work a start the stub ran brings to issue: the paused segment of the role, or
// its next recorded segment where the start's entry is above the one the claim was given (given)
// or the start resends its entry, a reassertion counted where the start re-sent an entry the
// claim was given already.
func (d *driver) startWork(issue record.Issue, payload record.SuperviseRequest, given int64, delivered bool) error {
	if p := d.pausedPiece(issue.Key, payload.Role); p != nil {
		return d.resumePiece(p)
	}
	opens := payload.ResendEntry || payload.Entry > 0 && payload.Entry > given
	switch {
	case opens && delivered:
		served := cmp.Or(payload.Phase, issue.Phase)
		if d.startPiece(issue.Key, payload.Role, served, payload.ResendEntry) != nil {
			return nil
		}
		if d.neverStarted(issue.Key) {
			return d.neverStartedWork(issue.Key, payload.Role, served)
		}
		d.counters.NoSegment++
	case payload.Entry > 0 && !opens:
		d.counters.Reassertions++
	}
	return nil
}

// pausedPiece is the piece of key's work role's claim was suspended in, nil for none.
func (d *driver) pausedPiece(key string, role claim.Role) *piece {
	for _, p := range d.script.pieces[key] {
		if p.state == piecePaused && p.role == role {
			return p
		}
	}
	return nil
}

// neverStarted says whether the record never started key's work: an issue of the trace with no
// slot and no work segment (neverStartedIn).
func (d *driver) neverStarted(key string) bool {
	return neverStartedIn(d.trace, key)
}

// neverStartedWork is the work of an issue the record never started, which the replay starts and
// whose script holds no work the branch's scenario gave it (Branch.NeverStarted, assumption 8:
// drawNeverStarted gives it the drawn work under drawn and drawn-done): under none, or where the
// window holds no issue to draw from, the start opens a segment with no turn and no end, which
// holds what the start took to the window's end. The run counts the issue among its admissions with
// no work (Divergence.NoWorkAdmissions).
func (d *driver) neverStartedWork(key string, role claim.Role, served phase.Phase) error {
	d.used[8] = true
	d.noWork[key] = true
	p := &piece{issue: key, index: len(d.script.pieces[key]), phase: served, role: role, opens: opensOnStart, ends: endOpen,
		start: d.clock.Instant(), m: d.script.inFlightAt(d.clock.Instant())}
	d.script.pieces[key] = append(d.script.pieces[key], p)
	d.openPiece(p, d.clock.Instant(), false)
	return nil
}

// gateClosed says whether the design gate of issue's tree is closed, which holds the merger's
// start (internal/daemon's designGateClosed): a step the tree is gated on holds a row no approval
// has opened (classify.StandingGate over the deployment's steps and the tree root's rows, which
// adds a criteria row the root registered beside a deployment that gates spec and not criteria). A
// step with no row holds nothing here: nobody can be waiting on a document the record does not
// know.
func (d *driver) gateClosed(issue record.Issue) (bool, error) {
	var gates map[record.DesignStep]record.DesignGate
	err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gates, err = d.records.GatesOf(ctx, tx, issue.Tree)
		return err
	})
	if err != nil {
		return false, err
	}
	_, closed := classify.StandingGate(d.window.GateSteps, gates)
	return closed, nil
}

// suspend runs a suspend as the outbox's does: a claim that runs nothing, or that a newer start
// superseded (workflow.StopActs), is left; otherwise the turn under way ends, the task it held is
// taken back unconfirmed, so the entry it was given falls to the one it served and the admission
// pass sends that entry again once the issue holds its slot again (admit's resumeSlottedWorkers),
// and the claim stands suspended by the daemon (`supervise: suspended`). The issue's segment the
// role works pauses with the rest of its length, or is cancelled (cancelPiece) where the issue has
// left the workflow.
func (d *driver) suspend(row record.OutboxRow, payload record.SuperviseRequest, issue record.Issue, token claim.Token) error {
	c, found := d.claims[token]
	if !found || c.Gone() ||
		!workflow.StopActs(row.ID, payload, c.LastStartRow, nil) {
		return d.finish(row)
	}
	if p := d.runningPiece(issue.Key, payload.Role); p != nil && p.state == pieceRunning {
		if record.InWorkflow(issue) {
			if err := d.pausePiece(p); err != nil {
				return err
			}
		} else {
			d.cancelPiece(p)
		}
	}
	if d.turnOpen(d.claims[token]) {
		if err := d.writeTurnEnd(issue.Key, payload.Role); err != nil {
			return err
		}
	}
	c = d.claims[token]
	pending := c.Pending
	if pending != nil {
		taken := *pending
		taken.ConfirmedAt = time.Time{}
		c.Pending = &taken
	}
	c = retired(c)
	c.State, c.SuspendedBy = supervise.StateSuspended, supervise.SuspendedByDaemon
	if err := d.writeClaim(c, pending); err != nil {
		return err
	}
	d.log.Info(suspendedLine, "claim", string(token), "reason", payload.Reason, "by", string(supervise.SuspendedByDaemon))
	return d.finish(row)
}

// retireStoppedTask runs the retirement of the task a stopped worker's claim holds for its phase, which
// the admission pass writes when it moves a started parent into awaiting_children in the middle of
// that phase (workflow's StopForChildren, supervise.Machine.RetireStoppedTask): the row names the
// phase the issue stood in, which the move has left, so it is read against the issue's generation
// and tree alone. The stub pauses the segment the role works, which keeps the rest of its length
// for the start the return out of awaiting_children writes (workflow's ReturnFromChildren) and
// ends its turn, and marks the pause a stop (piece.stopped), so the issue's acts placed from its
// start wait for the return too (holdForReturn); the claim keeps its state, as the daemon's
// executor retires the task and leaves the claim, and the worker is told to stop by a phase-ended
// message the stub finishes (assumption 26).
func (d *driver) retireStoppedTask(row record.OutboxRow, payload record.SuperviseRequest, issue record.Issue) error {
	if payload.Generation != issue.Generation || payload.Tree != issue.Tree {
		return d.finish(row)
	}
	if p := d.runningPiece(issue.Key, payload.Role); p != nil && p.state == pieceRunning {
		if err := d.pausePiece(p); err != nil {
			return err
		}
		p.stopped = true
	}
	return d.finish(row)
}

// closeTree runs a tree's close as the outbox's does: it acts only while the tree's root still
// lingers at the generation the close names (workflow.StopActs), and then retires the claim and
// cancels the segments it works.
func (d *driver) closeTree(row record.OutboxRow, payload record.SuperviseRequest, token claim.Token) error {
	c, found := d.claims[token]
	if !found {
		return d.finish(row)
	}
	root, err := d.recorded(payload.Tree)
	if err != nil {
		return err
	}
	if !workflow.StopActs(row.ID, payload, c.LastStartRow, root) {
		return d.finish(row)
	}
	return d.retireClaim(row, token)
}

// retireClaim retires the claim token names, its issue closed or its tree's linger expired: the
// turn under way ends, the task it held goes with it, the session is kept, and every segment of the
// role's work on the issue is cancelled (cancelPiece).
func (d *driver) retireClaim(row record.OutboxRow, token claim.Token) error {
	c := d.claims[token]
	for _, p := range d.script.pieces[c.Issue] {
		if p.role == c.Role && (p.state == pieceRunning || p.state == piecePaused) {
			d.cancelPiece(p)
		}
	}
	if d.turnOpen(c) {
		if err := d.writeTurnEnd(c.Issue, c.Role); err != nil {
			return err
		}
	}
	c = d.claims[token]
	pending := c.Pending
	c = retired(c)
	c.State = supervise.StateRetired
	if err := d.writeClaim(c, pending); err != nil {
		return err
	}
	return d.finish(row)
}

// writeClaim writes c, and retires the delivery pending was where c no longer holds it; where it
// still holds one, or there was none, it writes c as putClaim does.
func (d *driver) writeClaim(c supervise.Claim, pending *supervise.Delivery) error {
	if pending == nil || c.Pending != nil {
		return d.putClaim(c)
	}
	if err := d.st.RetireDelivery(d.ctx, c, pending.ID); err != nil {
		return fmt.Errorf("write the claim %s: %w", c.Token, err)
	}
	d.claims[c.Token] = c
	return nil
}

// putClaim writes c as the claim's machine writes its state (store.Store.PutClaim), with its
// pending delivery where it holds one.
func (d *driver) putClaim(c supervise.Claim) error {
	var err error
	if c.Pending != nil {
		err = d.st.PutClaimAndDelivery(d.ctx, c, *c.Pending)
	} else {
		err = d.st.PutClaim(d.ctx, c)
	}
	if err != nil {
		return fmt.Errorf("write the claim %s: %w", c.Token, err)
	}
	d.claims[c.Token] = c
	return nil
}

// retireTask retires the task key's role's claim holds once the work of its segment is over, as
// the end of the worker's last turn of it retires it (supervise's retirePending): the entry it
// served recorded on the claim.
func (d *driver) retireTask(key string, role claim.Role) error {
	token, err := claim.NewToken(d.token, key, role)
	if err != nil {
		return err
	}
	c, ok := d.claims[token]
	if !ok || c.Pending == nil {
		return nil
	}
	pending := c.Pending
	return d.writeClaim(retired(c), pending)
}

// turnOpen says whether c is in a turn (supervise.StateWorking), the one state projection reads as
// a worker in a turn.
func (d *driver) turnOpen(c supervise.Claim) bool {
	return c.State == supervise.StateWorking
}

// ensureClaim is key's role's claim, admitted where the stub holds none: a segment under way at
// the window's start whose claim the start state did not write, since no turn of it stood then.
// It answers false, with no claim and no error, where no claim can be admitted for the turn: the
// replay does not record the issue, or the issue's tree runs no lifecycle in the replay now —
// none opened, its cleanup reserved or its cleanup confirmed (treelifecycle.BindWork) — which a
// recorded turn in a tree the record never opened meets. Such a turn is counted, under assumption
// 30 where the issue stands outside the workflow, a production check's turn, and otherwise as a
// turn begun with no lifecycle (Divergence.NoLifecycleTurns), and the replay goes on.
func (d *driver) ensureClaim(key string, role claim.Role) (supervise.Claim, bool, error) {
	token, err := claim.NewToken(d.token, key, role)
	if err != nil {
		return supervise.Claim{}, false, err
	}
	if c, ok := d.claims[token]; ok {
		return c, true, nil
	}
	issue, err := d.recorded(key)
	if err != nil {
		return supervise.Claim{}, false, err
	}
	if issue == nil {
		d.counters.NoLifecycleTurns++
		return supervise.Claim{}, false, nil
	}
	c, err := d.st.AdmitClaim(d.ctx, supervise.Claim{Token: token, Project: d.token, Tree: issue.Tree, Issue: key, Role: role,
		State: supervise.StateQueued})
	switch {
	case errors.Is(err, treelifecycle.ErrLifecycleAbsent) || errors.Is(err, treelifecycle.ErrCleanupReserved) ||
		errors.Is(err, treelifecycle.ErrLifecycleClosed):
		if record.InWorkflow(*issue) {
			d.counters.NoLifecycleTurns++
		} else {
			d.counters.TurnsLeftOut[assumeNoProductionCheck]++
			d.used[assumeNoProductionCheck] = true
		}
		return supervise.Claim{}, false, nil
	case err != nil:
		return supervise.Claim{}, false, fmt.Errorf("a turn of %s's %s at %s: %w", key, role, stamp(d.clock.Instant()), err)
	}
	d.launch(&c, false)
	if err := d.putClaim(c); err != nil {
		return supervise.Claim{}, false, err
	}
	return c, true, nil
}

// writeTurnStart is a turn of key's role's worker beginning: its claim in the turn
// (supervise.StateWorking) from now, the step the turn records, and the line the turn writes
// (`supervise: a turn started`, with the delivery the turn works where the claim holds one). A turn
// no claim can be admitted for writes nothing (ensureClaim).
func (d *driver) writeTurnStart(key string, role claim.Role) error {
	c, held, err := d.ensureClaim(key, role)
	if err != nil || !held {
		return err
	}
	if d.turnOpen(c) || c.State == supervise.StateFailed || c.State == supervise.StateRetired {
		return nil
	}
	now := d.now()
	c.State, c.LastTurnStart = supervise.StateWorking, now
	if err := d.putClaim(c); err != nil {
		return err
	}
	if _, err := d.st.PutClaimStep(d.ctx, c.Token, supervise.StepRecord{At: now, TurnStartedAt: now}); err != nil {
		return fmt.Errorf("write the step of the claim %s: %w", c.Token, err)
	}
	d.log.Info(turnStartedLine, turnLineArgs(c)...)
	if err := d.settled(key, role); err != nil {
		return err
	}
	return d.idleMoved()
}

// writeTurnEnd is the turn of key's role's worker under way ending: its claim idle from now, and
// the line the turn writes (`supervise: a turn ended`).
func (d *driver) writeTurnEnd(key string, role claim.Role) error {
	token, err := claim.NewToken(d.token, key, role)
	if err != nil {
		return err
	}
	c, ok := d.claims[token]
	if !ok || !d.turnOpen(c) {
		return nil
	}
	c.State, c.LastTurnEnd = supervise.StateIdle, d.now()
	if err := d.putClaim(c); err != nil {
		return err
	}
	d.log.Info(turnEndedLine, turnLineArgs(c)...)
	if err := d.settled(key, role); err != nil {
		return err
	}
	return d.idleMoved()
}

// turnLineArgs is the attributes of the line a turn of c writes as it starts or ends: the claim, its
// generation, and the delivery the turn works where the claim holds one.
func turnLineArgs(c supervise.Claim) []any {
	args := []any{"claim", string(c.Token), "generation", c.Generation}
	if c.Pending != nil {
		args = append(args, "delivery", c.Pending.ID)
	}
	return args
}

// settled applies Settled where the claim the stub just changed is a root architect's and its root
// has handed work to a person (record.Issue.DeclaredSeq), so the pass that enters the root's wait
// on that person runs at the moment the claim settled, as supervision's own facts make it run in a
// daemon.
func (d *driver) settled(key string, role claim.Role) error {
	if role != claim.RoleArchitect {
		return nil
	}
	issue, err := d.recorded(key)
	if err != nil || issue == nil || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.DeclaredSeq == 0 {
		return err
	}
	_, err = d.apply("replay", d.factID("settled", key), Settled{Issue: key})
	return err
}
