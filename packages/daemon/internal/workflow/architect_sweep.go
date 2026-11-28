package workflow

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ArchitectAtWork says whether a root's own architect is the role at work where that root stands.
// It is the entry sweep's reach over the architect, where record.AtWork is every other role's
// (admit's owedEntryRoles), and it is wider than record.AtWork deliberately. It answers true for
// three roots: the two the first-task derivation serves (ArchitectStartTask), and one past them
// that the architect is still owed a resume's work on, so the rule reaches exactly the roots it
// has work to send:
//
//   - a root standing at phase.Admitted, whatever its design gate. record.AtWork answers false
//     for such a root once its gate has opened, because no phase worker of it works there; its
//     own architect still does, since it is the only role at work on the root itself, and the
//     measured defect is a relaunched architect of such a root (dispatch://INFRA-2614).
//   - a root a hand-over carried in, in every phase that carries a run (record.Issue.CarriesRun)
//     at the generation the hand-over opened, which the record states as the hand-over row of
//     this generation (ArchitectCarried, the reading the catch-up makes). R5 names a root a
//     hand-over carried in while it stands in the phase that came with it, and this reach is
//     wider than that because the record holds no fact that says the root still stands in that
//     phase. The hand-over row names the issue, its generation and whether a gate came with it,
//     and no phase. The promotion that opens the tree raises the carried phase role's entry and
//     the architect's in one transaction, each at the engine's own clock, and a later transition
//     raises the next role's entry at a later moment, so no comparison of those moments is exact,
//     and the architect's own entry moves again with each resume. The issue's movement mark moves
//     with every fact of the issue, a Dispatch event among them, and neither its run mark
//     (record.Issue.TakenBack) nor its phase records hold a fact the promotion writes and a
//     transition changes. Holding the reach to the carried phase would take a column of its own.
//     So the task the derivation writes for such a root says
//     only what holds in every phase of that run (writeCarriedPhase), and what the reach does
//     past the carried phase holds of such a root wherever it stands: the sweep sends its
//     architect an entry the claim was not given, and the first stall arm reads the task of the
//     entry the row names. The mark on the issue (record.Issue.HandedOverFrom) is no use here:
//     the promotion that opens the root's own tree clears it, and this is read after that
//     promotion.
//   - a root past phase.Admitted under its own power while the entry its architect's phases row
//     names (entry) was raised by the resume of a person's answer (record.PhaseEntry.Resumed,
//     ResumeAwaitedRoot) and its claim has not been given it (run's record.RoleClaim.GivenEntry
//     below that entry, 0 where no claim stands). The resume puts the root back at admitted, and
//     where the answer opened the design gate the same admission pass promotes it to its first
//     phase, so a start row of that resume that reaches no claim would otherwise leave the answer
//     with nothing to send it: a lost row must lose none of it (dispatch://INFRA-2614 R4). It
//     widens nothing by phase, since it holds only until the claim is given that entry. A root
//     waiting on a person is left out: it stands there because its architect handed the work back
//     after that resume, and the answer to the new wait raises an entry of its own and queues the
//     next start of its architect, which admit's architectClaimUnreachable tells the controller.
//
// It reaches no wider. Any other root past phase.Admitted holds no hand-over row of its
// generation and stands there under its own power, with a phase role that owns its work; a root
// in accepting is told by the notice that moved it there, and a root waiting on a person by the
// resume the daemon composes when the answer lands. A child at phase.Admitted is not its tree's
// root, so a child's own sub-architect claim is outside the rule, which ArchitectHears answers
// for again before anything is sent.
//
// Whether that architect can hear what the rule would send is a question of its own
// (ArchitectHears), and whether a start is already on its way is a third (ArchitectStartPending).
// entry and run are what the caller read for the whole pass (record.Store.PhaseEntries,
// record.Store.RoleRuns). The store read is made for a slotted tree root past phase.Admitted that
// no resume's entry keeps alone: a child, a root at phase.Admitted and a root owed a resume are
// answered from the record in hand.
func ArchitectAtWork(ctx context.Context, tx pgx.Tx, store record.Store, root record.Issue, entry record.PhaseEntry,
	run record.RoleRun,
) (bool, error) {
	if !claim.IsTreeRoot(root.Key, root.Tree) {
		return false, nil
	}
	if root.Phase == phase.Admitted {
		return true, nil
	}
	if !root.CarriesRun() {
		return false, nil
	}
	if resumeOwed(root, entry, run) {
		return true, nil
	}
	handed, err := ArchitectCarried(ctx, tx, store, root)
	return handed != nil, err
}

// resumeOwed is the third root of ArchitectAtWork, asked of a root that stands in a phase that
// carries a run (record.Issue.CarriesRun), which its caller has already answered: the root is not
// waiting on a person, and the entry of its architect's row was raised by a resume that the claim
// of run has not been given.
//
// The wait is read as record.Issue.AwaitingPerson, the reading the second stall arm makes (admit's
// architectClaimUnreachable), rather than as the phase alone. The two answer alike for every
// record the sweep reads: the store refuses a root in awaiting_person that names no ask and no
// gate version it waits on (issues_awaiting_shape), and it reads such a root's wait back whenever
// either is set (record's scanIssue), so a root standing in awaiting_person always carries its
// Awaiting.
func resumeOwed(root record.Issue, entry record.PhaseEntry, run record.RoleRun) bool {
	if !entry.Resumed || root.AwaitingPerson() {
		return false
	}
	return GivenEntry(run) < entry.Entry
}

// ArchitectCarried is the record of a hand-over that created root at the generation it stands at
// (record.Store.HandedOverRoot), nil where none did: the run of that generation is the one the
// hand-over carried the root into, with the phase records and the pull request it brought and,
// where the row says so, its design gate (record.HandedOverRoot.GateCarried). A row of an earlier
// generation belongs to a run that has ended, so it says nothing of this one. The row names no
// phase, so it does not say the root still stands in the phase that came with it, only that it
// stands in that run (ArchitectAtWork says why no other fact says more). It is the one reading
// of that question: the sweep's reach (ArchitectAtWork) and the reader of what a root
// architect's task is derived from (ArchitectStartOf) both make it, which is every start of a
// root architect and the daemon's narrowing of a held task. The mark on the issue
// (record.Issue.HandedOverFrom) is no such reading: the promotion that opens the root's tree
// clears it.
func ArchitectCarried(ctx context.Context, tx pgx.Tx, store record.Store, root record.Issue) (*record.HandedOverRoot, error) {
	handed, err := store.HandedOverRoot(ctx, tx, root.Key)
	if err != nil || handed == nil || handed.Generation != root.Generation {
		return nil, err
	}
	return handed, nil
}

// ArchitectStartPending says whether a start of a root architect is already on its way, and why,
// which is what stops the sweep writing a second row for the same entry while the first waits
// (admit's owedEntryRoles). It is owed, the daemon's own reading of a start a role has not
// taken, with two of its arms read for this question. owed answers "has no claim yet" for a tree
// whose architect claim does not exist, and that tree is the very one the sweep writes the start
// for, so with no claim a start still queued is the whole of what holds the row back.
//
// And owed answers "has not taken its task yet" for a task that holds work (supervise.HoldsWork)
// the claim holds unconfirmed, whatever its run and entry. A task of news of the approved design
// alone holds none, so it never holds the row back: the start of the row's entry takes its place
// in the claim's hands and carries its news itself (supervise's Machine.supersedes). Here a task
// the root's own record has superseded is no start on its way either: one that carries a run and
// an entry, of a run earlier than the root's generation, or of the root's run at an entry below
// the entry its architect's phases row names (entry, record.PhaseRow.Entry, 0 for a row that names
// none). That task is the work of a run re-admission ended or of an entry the row has moved past,
// and the start of the row's entry takes its place in the claim's hands (supervise's
// Delivery.SupersededBy, the comparison the claim's machine makes when that start's task arrives).
// Held back by it, the sweep would write nothing on every pass while a lost start of the row's
// entry left the claim with only that task, and the relaunch of the claim would send it first
// (dispatch://INFRA-2614 R4, R5). Any other task of no run or of no entry is superseded by
// nothing: an operator's own, and a wake's (wakeRow), which says why the pod came back and is no
// entry's work. The machine refuses the start's task behind it and sends it first, so it holds
// the row back as owed answers. owed's other arms hold as they are: a start still queued, and a
// claim still starting. A start still queued is any start row of the architect at the root's
// generation that brings it work (startsWork), a wake's and the resume of a cancelled idle release
// (the daemon's releaseResume) among them, though neither carries an entry: it holds the row back
// until a lane has run it, and the passes after that read the claim it brought back by the arms
// above. A start queued for news alone is no such row, and holds nothing back. owed itself answers
// as it did for its other reader, the awaiting sweep (decideAwait), which asks whether the
// architect has taken whatever it was last given.
func ArchitectStartPending(root record.Issue, entry int64, run record.RoleRun) (string, bool) {
	if run.Claim == nil {
		if queuedStart(run) {
			return "has a start still queued", true
		}
		return "", false
	}
	if p := run.Claim.Pending; p != nil && p.SupersededBy(root.Generation, entry) {
		held := *run.Claim
		held.Pending = nil
		run.Claim = &held
	}
	return owed(run)
}

// ArchitectRelaunch is what the queued starts of a root's run say of its architect's claim
// (ArchitectRelaunchOf).
type ArchitectRelaunch int

const (
	// RelaunchNone says that no start of the run is the daemon's next move for the claim, and that
	// none is failing.
	RelaunchNone ArchitectRelaunch = iota
	// RelaunchFailing says that a start of the run has run against the claim and a run of it ended
	// without success, and that no start is the daemon's next move.
	RelaunchFailing
	// RelaunchNext says that a start of the run is the daemon's next move for the claim.
	RelaunchNext
)

// ArchitectRelaunchOf says whether a start queued for root's architect is the daemon's next move
// for its claim, which is what the second arm of admit's architect-stalled reading asks before it
// wakes the controller for a failed or a retired claim, or for a root with no claim. A start of
// the run is one startActs counts for the architect, whose start names no phase: one above the
// newest stop that still acts (actingStop).
//
// A start a lane holds under a live lease (record.QueuedSupervise.Running) is the next move,
// whatever else the record says of it: the outbox is running it now. The outbox writes
// LastStartRow when a run begins (internal/daemon's outbox, supervise's Machine.StartedBy) and only
// then relaunches the claim, so for that moment the record shows the claim still failed or retired
// beside the start that is relaunching it, and the start's Attempts may be above zero from an
// earlier run that waited. A lapsed lease holds nothing, and record.QueuedSupervise.Running says
// why and which clock it reads, so a start under a lapsed lease is read below as one no lane
// holds.
//
// A start no lane holds is failing when its id is at or below the claim's LastStartRow, which says
// it has run against the claim, and its Attempts are above zero, which says a run of it ended
// without success and left it queued, a failure or a wait (record.QueuedSupervise.Attempts). The
// outbox only runs it again on its backoff, so a failing start is never the next move, however
// recently it was queued. Any other start is the next move while it was queued at most
// StrandedReleaseWake ago (record.QueuedSupervise.Age, in the store's own clock); one queued
// earlier is neither the next move nor failing. That is why the floor is the acting stop alone and
// not the later of it and LastStartRow: a start at or below LastStartRow can still be the next
// move. A next move wins over a failing start, so RelaunchFailing means that no start is the next
// move.
//
// A start's age is counted from no earlier than floor, which admit's stall reading passes as the
// end of the project's latest pause (its stallFloor): a pause holds every start (record's
// pauseHolds), so a start queued before the pause ended has been the outbox's to run only since
// then, as the stuck clocks restart at the resume (decision 14 and departure 2 of
// dispatch://INFRA-3759). So the age read is the lesser of the start's Age and now less floor.
// It is read as the lesser of two durations rather than from the later of two moments because
// each duration is taken in one clock: Age in the store's, and now less floor in the daemon's,
// which is the clock now is read in and the clock the resume wrote the pause's end in
// (internal/pause's Service.Resume). A project no pause has ended passes the zero floor, and now
// less the zero time is the largest duration (time.Time.Sub), so that floor bounds no age.
//
// Stranded reads the same claim through StartFor, which counts every start above the acting stop
// whatever its age, its lease and its attempts, and the two readings answer different questions.
// Stranded asks whether nothing will start the claim again, and a failing start will, since the
// outbox runs it again on its backoff, so Stranded leaves such a claim out. The arm asks whether a
// start can still cure the claim, and a start that has run against it and left it as it was cannot
// (the architect's reading of R7 at dispatch://INFRA-2614).
func ArchitectRelaunchOf(run record.RoleRun, root record.Issue, floor, now time.Time) ArchitectRelaunch {
	stop := actingStop(run, root)
	lastStart := lastStartRow(run)
	relaunch := RelaunchNone
	for _, queued := range run.Queued {
		if !startActs(queued, stop, "") {
			continue
		}
		if queued.Running {
			return RelaunchNext
		}
		if queued.ID <= lastStart && queued.Attempts > 0 {
			relaunch = RelaunchFailing
			continue
		}
		if min(queued.Age, now.Sub(floor)) <= StrandedReleaseWake {
			return RelaunchNext
		}
	}
	return relaunch
}

// SendArchitectStart writes the start of root's architect again, for the entry its phases row
// already names (row): the start StartArchitect writes, enqueued directly rather than through
// StartOrOwe, so it waits for no admission slot and marks no movement: a root architect's start
// takes no slot, whether the root holds its row, holds none in accepting, retro or merging (record.KeepsSlot) or
// gave it back for a wait (admit's releaseSlotsForWaits). It raises no entry, as no resume of the
// work an entry already names does (enter, SendOwedEntry).
//
// What it sends turns on what the row says raised the entry, and never on a reading of that
// entry's task. An entry no resume raised (record.PhaseRow.EntryAwaiting nil) is one
// StartArchitect raised, or one the upgrade gave no task, and its task is derived again from the
// record as it stands (architectStartWork) rather than sent as written. The reason is the kept
// decisions such a task names: a kept decision is claimed by the outbox row that names it, and
// record.Postgres.UnclaimedKept locks and reads only what nothing claims, so a text composed once
// and sent again could name a decision another telling has already claimed. The derivation reads
// what is still unclaimed now, and EnqueueNaming claims exactly what this row names in the same
// transaction. That holds for every task the derivation writes, the one with no first-task
// instruction among them: for a root past phase.Admitted under its own power the derivation's
// answer is what a person decided on the tree and the roots that rest on its approval, and
// nothing else.
//
// An entry that holds a wait is one the resume of a root a person answered raised
// (ResumeAwaitedRoot), and the answer is the work that entry owes, which the derivation cannot
// give: its shapes are a tree's first act, which is false of a root whose architect has run in
// this generation, none of them names the answer, and the fresh tree's closes on "no message is
// coming", which is false of a root whose person's answer stands
// (dispatch://INFRA-2614/comment/03dd9487-b44d-4508-bc0e-268fe951b17e). So it writes the start the
// resume wrote: the wait it answers, no text, and the entry. The daemon renders that start's task
// at its send from the record as it stands then (ResumeAwaitedRootTask, through its rootTask),
// which asks whether the person's review the answer names is still their word
// (SupersedingApproval). The text the row keeps is not sent: it was rendered at the raise, and a
// person who approved the reviewed version since has answered the changes request it names
// (dispatch://INFRA-3136/spec R2). The sweep sends it while the claim has not been given it,
// whether the root stands at admitted or in a phase past admitted that the pass of the answer
// promoted it to (ArchitectAtWork).
func (e *Engine) SendArchitectStart(ctx context.Context, tx pgx.Tx, root record.Issue, row record.PhaseRow) error {
	if row.EntryAwaiting != nil {
		return e.enqueue(ctx, tx, root.Key, record.SuperviseRequest{
			Op: "start", Tree: root.Tree, Role: claim.RoleArchitect, Generation: root.Generation, Awaiting: row.EntryAwaiting, Entry: row.Entry,
		})
	}
	task, kept, err := e.architectStartWork(ctx, tx, root)
	if err != nil {
		return err
	}
	return e.enqueueArchitectStart(ctx, tx, root, task, kept, row.Entry, false)
}

// StartArchitect hands root's architect work it was not on, which two starts do: admission's
// promotion of a queued root (admit's LineQueuedRoot branch, through StartPromotedArchitect) and
// the sweep's raise for an architect whose phases row names no entry at all (admit's
// resumeSlottedWorkers). It derives the architect's task from the record as it stands
// (architectStartWork), raises the architect's phase entry with that task as the entry's work
// where the task is not empty (EnterArchitect), and writes the start, carrying the task and the
// entry and claiming the kept decisions the task names, all in the caller's transaction. Every
// other start of this file resumes the work an entry already names and raises none.
//
// The derivation is read once and the row and the start carry that one text, because a kept
// decision is claimed by the outbox row that names it: deriving twice would compose a task from
// decisions the first text had already claimed.
//
// A task the derivation leaves empty raises no entry, and the start carries no task and no entry,
// so the outbox delivers nothing for it and the start only launches the claim. That is the
// promotion of a root past phase.Admitted under its own power on which nobody decided anything
// and on whose approval no root still in the workflow rests (ArchitectStartTask's second rule):
// an entry raised with no work on it would stand above every entry its claim was ever given, with
// nothing to send it. The sweep's raise never meets that case, because the roots it raises an
// entry for are inside its reach (ArchitectAtWork) with a row that names no entry, which no resume
// raised, and those are the two roots the derivation writes a first task for.
//
// The start waits for no admission slot, so it is enqueued directly rather than through
// StartOrOwe, and it marks no movement of its own: the promotion marks the movement of the tree it
// opens, and the sweep's raise is the daemon sending work the record already owes.
func (e *Engine) StartArchitect(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	return e.startArchitect(ctx, tx, root, false)
}

// StartPromotedArchitect is StartArchitect for admission's promotion of a queued root, whose start
// is marked decided (record.SuperviseRequest.Decided). A root reaches the line only by a person's
// or the controller's todo on it, which hands it to Legion or opens its tree again — the
// controller's restart of a tree, a backlog and then a todo on its root, among them — so the start
// of its architect is theirs, as the held phase's retry is (restart). It is the one start that
// brings back a root architect held for OpenAI's cyber-policy refusal, which it starts in a new
// session (supervise's freshSession): an ordinary restart of the tree, which asks nothing of the
// agent that makes it but the restart. A root admitted for the first time has no claim, and the
// mark changes nothing for it.
func (e *Engine) StartPromotedArchitect(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	return e.startArchitect(ctx, tx, root, true)
}

// startArchitect is StartArchitect's and StartPromotedArchitect's one body, decided being the mark
// the start carries.
func (e *Engine) startArchitect(ctx context.Context, tx pgx.Tx, root record.Issue, decided bool) error {
	task, kept, err := e.architectStartWork(ctx, tx, root)
	if err != nil {
		return err
	}
	var entry int64
	if task != "" {
		if entry, err = e.EnterArchitect(ctx, tx, root, task, nil); err != nil {
			return err
		}
	}
	return e.enqueueArchitectStart(ctx, tx, root, task, kept, entry, decided)
}

// architectStartWork is the task a root architect's start carries, derived from the record as it
// stands (ArchitectStartOf, which reads the roots that rest on its approval too) with what its tree
// is owed and nothing claims (ParkedDecisionTask), and the kept decisions that task names, which
// the row that carries it claims.
func (e *Engine) architectStartWork(ctx context.Context, tx pgx.Tx, root record.Issue) (string, *record.KeptDecisions, error) {
	decided, kept, err := e.ParkedDecisionTask(ctx, tx, root)
	if err != nil {
		return "", nil, err
	}
	start, err := ArchitectStartOf(ctx, tx, e.store, e.cfg.Steps, root)
	if err != nil {
		return "", nil, err
	}
	start.Decided = decided
	return ArchitectStartTask(start), kept, nil
}

// enqueueArchitectStart writes the start row of root's architect for entry, carrying task and
// claiming the kept decisions that task names in this same transaction, marked decided where a
// person or the controller asked for it (StartPromotedArchitect).
func (e *Engine) enqueueArchitectStart(ctx context.Context, tx pgx.Tx, root record.Issue, task string,
	kept *record.KeptDecisions, entry int64, decided bool,
) error {
	return e.EnqueueNaming(ctx, tx, root.Key, root.Tree, record.SuperviseRequest{Op: "start", Tree: root.Tree,
		Role: claim.RoleArchitect, Generation: root.Generation, Task: task, Entry: entry, Kept: kept, Decided: decided}, kept, e.now())
}
