package admit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// This file is the architect-stalled wake (dispatch://INFRA-2614, R7): the three arms the
// admission pass reads over the own architect of every root it weighs, the wake that tells the
// project's controller of each arm once per run, and the reading the state route serves of the
// same arms. The arms are stated here once: the pass, the entry sweep's gate (OwesEntryWork) and
// the state (ArchitectStallArms) all ask architectStalls.

// architectStall is one arm the stall reading raises over one root on a pass: the root,
// the arm, what the record says, which the wake's reason carries after the arm's own words, and
// the moment the pass read it.
type architectStall struct {
	root record.Issue
	arm  record.ArchitectStallArm
	why  string
	at   time.Time
}

// mark is the stall as the mark that keeps its arm to one wake keys it (record.ArchitectStall).
func (s architectStall) mark() record.ArchitectStall {
	return record.ArchitectStall{Issue: s.root.Key, Tree: s.root.Tree, Generation: s.root.Generation, Arm: s.arm, At: s.at}
}

// stallMarks is the mark of each of stalls, in their order.
func stallMarks(stalls []architectStall) []record.ArchitectStall {
	marks := make([]record.ArchitectStall, len(stalls))
	for i, stall := range stalls {
		marks[i] = stall.mark()
	}
	return marks
}

// architectStalls reads the three arms over the architect of every root of read, in the order
// the holders are read and, for each root, in the order a controller is told them: the architect
// took nothing, its claim cannot be reached, the root is slow before its gate. The domain is the
// selection's (readSlotHolders): a root holding a slot, a root past its review and a root
// released for a wait alike. A root in accepting, retro or merging holds no slot since
// cybermonklabs/legion#3573, and a root waiting on another issue holds none since
// cybermonklabs/legion#3715, so the first two arms read each of them here as they read it while
// it held one. The third reads a root at admitted with no step of its design gate registered
// alone, whose slot gives it the admission time its bound is weighed from; a root at admitted is
// released for a wait only once its gate has opened, so the third holds for no released root.
// sent names the roots whose architect start this pass wrote (resumeSlottedWorkers), which the
// second arm reads; the sweep's gate and the state pass none, since the gate asks only where the
// pass would send nothing (OwesEntryWork) and the state reads what the passes before it
// committed (ArchitectStallArms). Each root's arms are read at that root's own reading of the
// clock.
func (a *Admission) architectStalls(ctx context.Context, tx pgx.Tx, read slotReading, facts record.LineFacts,
	sent map[string]bool,
) ([]architectStall, error) {
	var stalls []architectStall
	for _, held := range read.holders {
		if held.role != claim.RoleArchitect {
			continue
		}
		root, run := held.issue, read.runs[held.token]
		row := read.entries[record.PhaseRole{Issue: root.Key, Role: claim.RoleArchitect}]
		now := a.now()
		took, err := a.architectTookNothing(ctx, tx, root, row, run, now)
		if err != nil {
			return nil, err
		}
		for _, arm := range []struct {
			arm record.ArchitectStallArm
			why string
		}{
			{record.ArchitectTookNothing, took},
			{record.ArchitectClaimUnreachable, architectClaimUnreachable(root, row, run, sent[root.Key], now)},
			{record.ArchitectSlowBeforeGate, architectSlowBeforeGate(root, facts.Gates[root.Tree], held.admittedAt, row, now)},
		} {
			if arm.why != "" {
				stalls = append(stalls, architectStall{root: root, arm: arm.arm, why: arm.why, at: now})
			}
		}
	}
	return stalls, nil
}

// ArchitectStallArms is, by root key, every arm of the stall reading that holds for that root's
// own architect now, in the order a controller is told them, and nothing for a root no arm holds
// for. It is the reading the admission pass makes before it wakes the controller — the same
// reads of the project (reread), the same holders (readSlotHolders) and the same arms
// (architectStalls) — and it reads nothing of the marks, of the controller's registration or of
// a wake, so an arm is in the answer whether or not the controller was told of it. It writes
// nothing, so the state route asks it in its own read-only snapshot (internal/daemon's
// source.State), which serves the answer as each root's `architectStalls`
// (api.Issue.ArchitectStalls) and is what `legion state`'s plain line prints.
//
// It reads the record as the passes before it committed it, which is what the next pass reads
// before its sends: a start an earlier pass wrote is a queued start here, and the second arm reads
// it so (workflow.ArchitectRelaunchOf).
func (a *Admission) ArchitectStallArms(ctx context.Context, tx pgx.Tx) (map[string][]record.ArchitectStallArm, error) {
	issues, slots, facts, err := a.reread(ctx, tx)
	if err != nil {
		return nil, err
	}
	read, err := a.readSlotHolders(ctx, tx, issues, slots, facts)
	if err != nil {
		return nil, err
	}
	stalls, err := a.architectStalls(ctx, tx, read, facts, nil)
	if err != nil || len(stalls) == 0 {
		return nil, err
	}
	arms := make(map[string][]record.ArchitectStallArm, len(stalls))
	for _, stall := range stalls {
		arms[stall.root.Key] = append(arms[stall.root.Key], stall.arm)
	}
	return arms, nil
}

// wakeStalledArchitects wakes the project's controller once for each of stalls no mark has told
// yet, once per run of its root's tree (record.Store.TellArchitectsStalled). The pass calls it
// after it has sent every role the work its phase entry names, so each arm reads a root the
// daemon has already done what it can for.
//
// It marks nothing and queues nothing while no controller is registered
// (record.Store.ControllerRegistered, one read for the whole pass), which is the daemon's rule
// for every wake that keeps a mark (wakeController, tellStalledTrees): a controller that
// registers later reads its notices from the end of the stream, so a wake queued before it
// reaches nobody, and the mark written beside it would keep that arm from being told again in
// the run. So a stall read while no controller runs is told at the first pass after one
// registers.
//
// Every stall of the pass is marked in one statement, and a stall marked already answers false,
// so a root that stays stalled costs no statement of its own on the facts after its telling. The
// wakes are queued for the stalls that statement told, in the order the pass read them, and per
// issue rather than through wakeController, which keeps one unpublished notice per project and
// kind: every root of a project whose architects are stuck owes a wake of its own, and the
// project-wide rule would let the first root's wake hide every other root's. What keeps one
// root's arm to one wake is the mark instead, which is keyed by the arm as well, so a root two
// arms read wakes the controller twice and neither case hides the other.
//
// The reason each wake carries begins with the arm's own words (record.ArchitectStallArm.Words)
// and then says what the record says, so a controller reads which of the three raised the wake
// it holds without opening anything.
func (a *Admission) wakeStalledArchitects(ctx context.Context, tx pgx.Tx, stalls []architectStall) error {
	if len(stalls) == 0 {
		return nil
	}
	registered, err := a.store.ControllerRegistered(ctx, tx, a.project)
	if err != nil || !registered {
		return err
	}
	told, err := a.store.TellArchitectsStalled(ctx, tx, stallMarks(stalls))
	if err != nil {
		return err
	}
	for i, stall := range stalls {
		if !told[i] {
			continue
		}
		reason := stall.arm.Words() + ": " + stall.why
		if err := a.enqueue(ctx, tx, stall.root.Key, record.ControllerNotice{Kind: record.ArchitectStalledNotice,
			Role: claim.RoleArchitect, Reason: reason}, stall.at); err != nil {
			return err
		}
		store.OnCommit(ctx, func() {
			a.log.Warn("admission: the controller was told a root's architect has stalled",
				"issue", stall.root.Key, "arm", string(stall.arm), "phase", stall.root.Phase,
				"generation", stall.root.Generation, "reason", reason)
		})
	}
	return nil
}

// architectTookNothing is the first arm: the architect's claim holds the task of the entry its
// phases row names (row), of the root's run, no turn of the claim has confirmed it, and it has
// stood so more than workflow.StrandedReleaseWake since the moment the arm counts it from
// (tookNothingSince): when a task of that entry was first queued to the claim, or, for a task a
// let-go took back from a turn, the later of that moment and the relaunched agent's ready. It
// answers what the record says, and "" for a root it does not read.
//
// The task is read as the delivery of the entry the row names, of the root's generation, which is
// R7's "its entry's task" (dispatch://INFRA-2614), and no other. A task of an earlier run, or of
// an entry the row has moved past, that carries an entry is one the row has superseded: the
// sweep's send passes over it and writes the start of the row's entry, whose task takes its place
// in the claim's hands (workflow.ArchitectStartPending), so it is no work the architect failed to
// take. The arms are read after the pass's sends, from the reading made before them, so an arm
// that read such a task — the task of the entry the claim was given, say, with the row above it —
// would tell the controller that the daemon writes no second start in the very pass that writes
// it. A task of no entry, an operator's own or a wake's, is no entry's task either, so this arm
// reads nothing of it, while the sweep holds the start back behind it until a turn takes it. A
// task of the row's own entry is one the claim was given that entry with (supervise's queue), so
// the sweep writes nothing for it (workflow.OwedEntry) and the wake is the one thing left. The
// claim's handshake of that entry (supervise.Claim.Handshake) holds the two times the state shows
// beside the two numbers (api.Issue.EntryQueuedAt), and its queued time is the first queueing
// this arm counts from.
//
// The root must also be one the sweep serves (architectServed): no root outside the reach is
// sent an architect's task at all, so none can have failed to take one, and a root that cannot
// hear is one nothing a controller does reaches, so it raises nothing, while one that could not
// hear earlier and can now raises the arm, because the task it was owed never arrived. The reach
// is read over the entry the architect's row names (row) and the run, as the sweep's send reads
// it, so the arm reads the roots the rule of R5 reaches and no others, as R7 says. Those
// readings are asked last, so a root whose claim holds no such task costs no read.
func (a *Admission) architectTookNothing(ctx context.Context, tx pgx.Tx, root record.Issue, row record.PhaseEntry,
	run record.RoleRun, now time.Time,
) (string, error) {
	held := run.Claim
	if held == nil || held.Pending == nil {
		return "", nil
	}
	task := held.Pending
	if task.Entry == 0 || task.Entry != row.Entry || task.Generation != root.Generation || !task.ConfirmedAt.IsZero() ||
		task.QueuedAt.IsZero() {
		return "", nil
	}
	since, counted := tookNothingSince(*held)
	if !counted || now.Sub(since) <= workflow.StrandedReleaseWake {
		return "", nil
	}
	served, err := a.architectServed(ctx, tx, root, row, run)
	if err != nil || !served {
		return "", err
	}
	unconfirmed := "No turn of that claim has confirmed it."
	if task.Interrupted {
		unconfirmed = fmt.Sprintf("A let-go of the claim's process took it back from a turn of it, and no turn has confirmed it since %s, "+
			"the later of its first queueing and the moment the claim's relaunched agent was ready.", since.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("the task of phase entry %d was first queued to the architect's claim of %s at %s. %s The root stands at %s. The daemon writes no second start while that task stands.",
		task.Entry, root.Key, firstQueued(*held).UTC().Format(time.RFC3339), unconfirmed, root.Phase), nil
}

// tookNothingSince is the moment the first arm counts the pending task of held from, and false
// where it does not count that task at all. held holds a pending task.
//
// A task no let-go took back is counted from the first queueing of its entry (firstQueued), in
// whatever state the claim stands: no turn ever confirmed it, so the architect took nothing of
// it, and a failed claim that holds it raises the second arm beside this one. The task's own
// queueing is not that moment where the entry was queued before: a task retired unconfirmed puts
// its entry back (supervise's retirePending), the sweep sends that entry again, and the new
// delivery is queued at the moment of that send. Counted from it, an architect that has left the
// entry's work untaken past the bound would have its count started again at every send.
//
// A task a let-go took back from a turn (supervise.Delivery.Interrupted) is one a turn of the
// architect did confirm. Every relaunch takes the claim's task back so (supervise's
// Machine.interrupted): a death, an out-of-memory kill, the stale-plugin relaunch, a repoint, the
// stall backstop and a held suspension's deadline. The take-back copies the task under a new id,
// keeps its QueuedAt, clears its confirmation and marks it, and the task waits for the relaunch to
// send it again. Counted from its entry's first queueing, it would raise the arm on the first pass
// of the relaunch of an architect whose task was first queued more than ten minutes ago, about an
// architect that worked the task, and spend the arm's one mark for the run.
//
// So such a task is counted only while the claim's agent can take a prompt, which is the states
// ready, working and idle. A claim enters them only through its agent's ready (supervise's ready,
// the one move out of registered into ready), which writes the claim's last ready
// (record.RoleClaim.LastReady) on every launch, a relaunch's included, so the task is counted from
// the later of its entry's first queueing and that ready: a task the relaunched agent has been
// able to take for more than the bound with no turn confirming it raises the arm. The mark is
// never cleared (supervise.Delivery.Interrupted), so a task a later refusal takes back again is
// still counted from that ready, which is then the later moment anyway. The other states count
// nothing. In queued, launch_uncertain, launching, shim_connected and registered the relaunch has
// not yet brought up an agent that can take a prompt. A suspended claim has no process: its task
// waits for the resume, whose ready counts it again. A failed or a retired claim has no process
// either, and no start reaches it without a relaunch, which is the second arm's reading.
func tookNothingSince(held record.RoleClaim) (time.Time, bool) {
	queued := firstQueued(held)
	if !held.Pending.Interrupted {
		return queued, true
	}
	switch held.State {
	case supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		if held.LastReady.After(queued) {
			return held.LastReady, true
		}
		return queued, true
	}
	return time.Time{}, false
}

// firstQueued is when a task of the entry of held's pending task was first queued to the claim:
// the queued moment of the claim's handshake where that handshake is of the task's entry and run
// (record.RoleClaim.Handshake, which the claim's machine writes at the first queue of an entry and
// keeps through every later task of it), and the task's own queueing otherwise. held holds a
// pending task.
func firstQueued(held record.RoleClaim) time.Time {
	task, handshake := held.Pending, held.Handshake
	if handshake.Entry == task.Entry && handshake.Generation == task.Generation && !handshake.QueuedAt.IsZero() {
		return handshake.QueuedAt
	}
	return task.QueuedAt
}

// architectClaimUnreachable is the second arm: the root is in the workflow, no start of its run is
// the daemon's next move for its architect (workflow.ArchitectRelaunchOf), and either its
// architect's claim is failed or retired, or the root holds no architect claim at all while the
// work of its entry has stood more than workflow.StrandedReleaseWake. It answers what the record
// says, and "" for a root it does not read.
//
// workflow.ArchitectRelaunchOf decides which start of the run is the daemon's next move, and the
// pass's own start (sent) is one too. The arm waits while such a start stands because the
// outbox's start of a failed or a retired claim relaunches it, on the session it kept, or a new
// one once supervise's Machine.forgetSession has dropped that (an architect's start asks for no
// new session): a
// controller woken for that claim would take `fresh_session`, which discards the session that
// start resumes, and the mark the wake writes would leave a real failure later in the run waking
// nobody. Every re-admission of a closed tree makes that shape, since the linger's end retires the
// architect's claim and the promotion queues its start in the same pass that reads this arm. A
// claim whose start has run against it and failed, or whose start has stood owed past the bound,
// is the failure no further send cures, which R7's second arm names (the architect's reading of R7
// at dispatch://INFRA-2614, measured by its tester on the whole daemon).
//
// A root waiting on a person is in the workflow, holding its slot through the wait
// (record.KeepsSlot), so the arm reads it as it reads any root, and its reason says that it waits.
// The daemon queues no start of its architect while it waits, since the sweep's reach leaves such
// a root out (workflow.ArchitectAtWork), and the answer to the wait queues the next one
// (workflow.Engine.ResumeAwaitedRoot), which the outbox runs against a failed claim with a retry
// and against a retired one with its workspace provisioned again and a retry. Until that answer
// no send cures the claim, so the controller is told the tree has no architect that can run while
// it waits. workflow.Stranded leaves such a root out, which is a different question: it asks
// whether anything will start the claim again, and the answer will.
//
// That wake spends the arm's one mark for the root's run (record.Store.TellArchitectsStalled keys
// it by issue, tree, generation and arm), and the answer starts no new run: the resume raises the
// architect's entry and not the root's generation. So when the start the answer queues runs
// against the claim and leaves it failed or retired (workflow.RelaunchFailing), the arm holds
// again and wakes nobody, as R7's once per root and generation says. The state still lists it
// (ArchitectStallArms reads no mark), which is what the controller's guidance has it read on its
// ticks once the wait is gone (skills/legion-controller/references/wakes.md).
//
// The reason of a claim says which case holds: the start of its run has run against it and left
// it failed or retired, or no start of its run queued inside the bound waits to relaunch it. A
// root with no claim has no start that ran against a claim, so its reason says only that no start
// queued inside the bound waits to create one.
//
// run is the reading the pass made before its sends, so where the pass itself wrote the start of
// a root with no claim (sent) the wake still stands — a root with no claim that long is stalled
// whatever this pass writes — and its reason says the start was written, rather than that none
// waits.
func architectClaimUnreachable(root record.Issue, row record.PhaseEntry, run record.RoleRun, sent bool, now time.Time) string {
	if !record.InWorkflow(root) {
		return ""
	}
	held := run.Claim
	if held != nil && held.State != supervise.StateFailed && held.State != supervise.StateRetired {
		return ""
	}
	relaunch := workflow.ArchitectRelaunchOf(run, root)
	if relaunch == workflow.RelaunchNext {
		return ""
	}
	var waits string
	if root.AwaitingPerson() {
		waits = " The root waits on a person, and the daemon queues the next start of its architect when that wait is answered."
	}
	if held != nil {
		if sent {
			return ""
		}
		if relaunch == workflow.RelaunchFailing {
			return fmt.Sprintf("the architect claim of %s is %s. The start of its run has run against it and left it %s, and the outbox only runs that start again on its backoff. The root stands at %s in the workflow.%s",
				root.Key, held.State, held.State, root.Phase, waits)
		}
		return fmt.Sprintf("the architect claim of %s is %s, no start of its run queued in the last %s waits to relaunch it, and the root stands at %s in the workflow.%s",
			root.Key, held.State, workflow.StrandedReleaseWake, root.Phase, waits)
	}
	if row.Entry == 0 || row.RaisedAt.IsZero() || now.Sub(row.RaisedAt) <= workflow.StrandedReleaseWake {
		return ""
	}
	queue := "no start of one queued in the last " + workflow.StrandedReleaseWake.String() + " waits to create it"
	if sent {
		queue = "the daemon wrote the start of its architect again in the same transaction as this wake"
	}
	return fmt.Sprintf("%s holds no architect claim at all, and %s. The work of phase entry %d has stood since %s.%s",
		root.Key, queue, row.Entry, row.RaisedAt.UTC().Format(time.RFC3339), waits)
}

// architectSlowBeforeGate is the third arm: the root stands at phase.Admitted with no step of
// its design gate registered, more than workflow.ArchitectGateWake after the later of its
// admission and the moment its architect's entry was last raised. It answers what the record
// says, and "" for a root it does not read.
//
// The later of the two moments is the issue's own decision. A root a person answered after a six
// hour wait comes back to admitted holding the admission row it has held all along, so its
// admitted moment is six hours old and the bound would be past the moment it resumed. The entry
// its resume raised is what says when the work came back to it. For a first admission and for a
// re-admission the two are one moment, since the promotion that takes the slot raises the entry
// in the same transaction (promoteHolds).
//
// It reads a root standing at admitted and no other. A root a hand-over carried in stands in a
// phase of the run that carried it, where its tree's work is already running under a phase
// worker, and a gate it has not registered holds nothing up yet.
func architectSlowBeforeGate(root record.Issue, gate record.TreeGate, admittedAt time.Time, row record.PhaseEntry, now time.Time) string {
	if root.Phase != phase.Admitted || len(gate.Rows) > 0 {
		return ""
	}
	since := admittedAt
	if row.RaisedAt.After(since) {
		since = row.RaisedAt
	}
	if since.IsZero() || now.Sub(since) <= workflow.ArchitectGateWake {
		return ""
	}
	return fmt.Sprintf("%s has stood at admitted since %s with no step of its design gate registered, which is more than %s.",
		root.Key, since.UTC().Format(time.RFC3339), workflow.ArchitectGateWake)
}
