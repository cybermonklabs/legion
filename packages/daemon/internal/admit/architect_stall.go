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
// selection's (readSlotHolders): a root holding a slot, a root in accepting, retro or merging and a root
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
//
// floor is the floor of the project's stall reading (stallFloor), which each caller reads once
// with the project's pause (readStallFloor) and hands in, so this reads no pause of its own. No
// caller asks it while the project is paused (promoteHolds, convergeEntries, OwesEntryWork,
// ArchitectStallArms): the pause has stopped every architect an arm would name and holds every
// start and task the daemon would send one, and the passes that tell the controller do not run
// then. After a pause, each arm whose bound is a stretch of time counts it from no earlier than
// the end of the latest pause (floor). The first arm counts no time while the architect's launch
// waits in the wake pacing (tookNothingSince). The third counts from no earlier than the let-out
// of the launch the latest pause held for the architect, whose let-outs it reads once
// (readPauseLetOuts), and, once the architect has confirmed a task of its entry, from no earlier
// than that confirmation (architectSlowBeforeGate).
func (a *Admission) architectStalls(ctx context.Context, tx pgx.Tx, read slotReading, facts record.LineFacts,
	floor stallFloor, sent map[string]bool,
) ([]architectStall, error) {
	letOuts, err := a.readPauseLetOuts(ctx, tx, floor, a.now())
	if err != nil {
		return nil, err
	}
	var stalls []architectStall
	for _, held := range read.holders {
		if held.role != claim.RoleArchitect {
			continue
		}
		root, run := held.issue, read.runs[held.token]
		row := read.entries[record.PhaseRole{Issue: root.Key, Role: claim.RoleArchitect}]
		now := a.now()
		took := architectTookNothing(root, row, run, read.chains.HoldsOwnGate(root.Key), floor, now)
		for _, arm := range []struct {
			arm record.ArchitectStallArm
			why string
		}{
			{record.ArchitectTookNothing, took},
			{record.ArchitectClaimUnreachable, architectClaimUnreachable(root, row, run, sent[root.Key], floor, now)},
			{record.ArchitectSlowBeforeGate, architectSlowBeforeGate(root, facts.Gates[root.Key], held.admittedAt, row, run, floor,
				letOuts[root.Key], now)},
		} {
			if arm.why != "" {
				stalls = append(stalls, architectStall{root: root, arm: arm.arm, why: arm.why, at: now})
			}
		}
	}
	return stalls, nil
}

// stallFloor is the moment no arm of the stall reading counts time from before: the end of the
// project's latest pause, and the zero value for a project no pause has ended. A pause stops the
// project's architects and holds every start and every task the daemon would send them (record's
// pauseHolds, supervise's sendPending), so while it stood an architect could register no step of
// its gate, take no task and be relaunched by no start. That time is no time the architect failed
// to use. So each arm whose bound is a stretch of time counts it from the later of the moment it
// names and this one, as no stuck clock starts before the resume either (decision 14 and
// departure 2 of dispatch://INFRA-3759, store.SetProjectResumeClocksTx). Without it, the resume's
// own pass (internal/daemon's applyResumePass) would tell the controller of each stall the pause
// made, and spend the arm's one mark for the run on it (record.Store.TellArchitectsStalled).
//
// The floor alone leaves the resume's wave of launches counted: the stuck clocks start at the
// let-out of each launch the wake pacing lets out (store.Store.SetResumeClock), and an architect
// far back in the wave waits longer than the first arm's bound before its own launch. So the first
// arm counts nothing while the architect's agent is not up (tookNothingSince), and the third
// counts from no earlier than the let-out of the launch the pause owed the architect and, once
// the architect has confirmed its entry's task, than that first confirmation
// (architectSlowBeforeGate, readPauseLetOuts).
type stallFloor struct {
	pause int64
	at    time.Time
}

// readStallFloor is whether the project stands paused and, where it does not, the floor of its
// stall reading, in one read through tx (store.OpenOrLastPauseTx, the project's open pause or
// else its newest ended one); a paused project has no floor. Each of its readers reads it once,
// before anything else, and hands the floor to what counts from it (tellStalledTrees,
// architectStalls): the admission pass (promoteHolds), the entry sweep's pass (convergeEntries),
// the sweep's gate (OwesEntryWork) and the state's reading of the arms (ArchitectStallArms).
//
// The pause is read on the reader's own transaction rather than from the daemon's flag
// (internal/pause's Service.Paused): the reader then judges the pause in the snapshot it judges
// everything else in, a pause another daemon of a rolling restart made counts before this
// daemon's next sweep reads it, and the read is of the table and the key the outbox's claim reads
// (record's outboxPauseFree).
func (a *Admission) readStallFloor(ctx context.Context, tx pgx.Tx) (stallFloor, bool, error) {
	p, found, err := store.OpenOrLastPauseTx(ctx, tx, a.project)
	if err != nil {
		return stallFloor{}, false, fmt.Errorf("read whether %s is paused and when its last pause ended: %w", a.project, err)
	}
	if !found {
		return stallFloor{}, false, nil
	}
	if !p.Ended() {
		return stallFloor{}, true, nil
	}
	return stallFloor{pause: p.ID, at: p.EndedAt}, false, nil
}

// readPauseLetOuts is, by root key, when the wake pacing let out the launch of the root's
// architect that the floor's pause held (store.PauseLetOutsTx, the stuck clock of the architect's
// resume of that pause), read once per reading through tx, and nothing for a project no pause has
// ended. A root whose architect the pause held no launch of is absent. The pause owes an
// architect it stops a launch where the architect holds a task it did not finish or a launch the
// pacing had parked, and it holds each wake of an architect that arrives while it stands; the
// resume lets each such launch out through the pacing, so the architect could take no turn
// before that let-out. A launch that still waits in the pacing's queue has no let-out yet, and
// reads as let out at now, the reading's own moment, so no time counts while it waits.
func (a *Admission) readPauseLetOuts(ctx context.Context, tx pgx.Tx, floor stallFloor, now time.Time) (map[string]time.Time, error) {
	if floor.pause == 0 {
		return nil, nil
	}
	project, err := claim.ProjectToken(a.project)
	if err != nil {
		return nil, err
	}
	letOuts, err := store.PauseLetOutsTx(ctx, tx, project, floor.pause, claim.RoleArchitect, now)
	if err != nil {
		return nil, fmt.Errorf("read the architect let-outs of %s for the architect-stalled arms: %w", a.project, err)
	}
	return letOuts, nil
}

// count is the moment a wait of the project counts its bound from, given the moment the wait
// names (since): since, or the floor where the floor is later. Each arm counts its bound so, and
// the stalled-tree notice a tree's quiet time (tellStalledTrees); the second arm hands the
// floor's moment to workflow.ArchitectRelaunchOf, which counts a queued start's age from no
// earlier than it. said is what the arm's reason adds where the floor is later, and "" where it
// is not, so the reason keeps naming the moment the arm names and says where the count began.
func (f stallFloor) count(since time.Time) (from time.Time, said string) {
	if !f.at.After(since) {
		return since, ""
	}
	return f.at, fmt.Sprintf(" That time is counted from the end of pause %d at %s, which held the project.",
		f.pause, f.at.UTC().Format(time.RFC3339))
}

// ArchitectStallArms is, by root key, every arm of the stall reading that holds for that root's
// own architect now, in the order a controller is told them, and nothing for a root no arm holds
// for. It is the reading the admission pass makes before it wakes the controller — the same
// reads of the project (readPass), the same holders (readSlotHolders) and the same arms
// (architectStalls) — and it reads nothing of the marks, of the controller's registration or of
// a wake, so an arm is in the answer whether or not the controller was told of it. It writes
// nothing, so the state route asks it in its own read-only snapshot (internal/daemon's
// source.State), which serves the answer as each root's `architectStalls`
// (api.Issue.ArchitectStalls) and is what `legion state`'s plain line prints.
//
// It reads the record as the passes before it committed it, which is what the next pass reads
// before its sends: a start an earlier pass wrote is a queued start here, and the second arm reads
// it so (workflow.ArchitectRelaunchOf).
//
// While the project is paused it answers no arm and reads nothing past the pause, which it reads
// once, first, with the floor it hands the arms (readStallFloor). The pause has stopped every
// architect an arm would name, and holds every start and task the daemon would send one, so no
// arm says anything of the architect, and `legion state` lists no stall the pause made. After the
// resume it reads the arms as the pass does: from no earlier than the pause's end (stallFloor),
// with no time counted by the first arm while an architect waits for its launch in the resume's
// wave of launches, and with the third counting from no earlier than the let-out of a launch the
// pause held, and than the first confirmation of the architect's entry where there is one, as
// the stuck clocks start at each launch's let-out (decision 14 and departure 2 of
// dispatch://INFRA-3759).
func (a *Admission) ArchitectStallArms(ctx context.Context, tx pgx.Tx) (map[string][]record.ArchitectStallArm, error) {
	floor, paused, err := a.readStallFloor(ctx, tx)
	if err != nil || paused {
		return nil, err
	}
	issues, slots, facts, err := a.readShared(ctx, tx)
	if err != nil {
		return nil, err
	}
	read, err := a.readSlotHolders(ctx, tx, issues, slots, facts)
	if err != nil {
		return nil, err
	}
	stalls, err := a.architectStalls(ctx, tx, read, facts, floor, nil)
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
// (tookNothingSince): while the claim's agent is up to take a prompt, the later of when a task of
// that entry was first queued to the claim and that agent's last ready; for a task no let-go took
// back, held by a claim that is suspended, failed or retired, when that task was first queued;
// and in every case no earlier than the end of the project's latest pause (floor). While the
// claim's agent is not up, which is where a launch the wake pacing has parked stands, it counts
// nothing. The pause keeps such a task unsent (supervise's sendPending), and stops at its ready
// an architect launched before the task went, so the task stands pending, unconfirmed and not
// taken back, through the whole pause. It answers what the record says, and "" for a root it does
// not read.
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
// readings are asked last, though they read nothing either.
func architectTookNothing(root record.Issue, row record.PhaseEntry, run record.RoleRun, holdsGate bool, floor stallFloor, now time.Time) string {
	held := run.Claim
	if held == nil || held.Pending == nil {
		return ""
	}
	task := held.Pending
	if task.Entry == 0 || task.Entry != row.Entry || task.Generation != root.Generation || !task.ConfirmedAt.IsZero() ||
		task.QueuedAt.IsZero() {
		return ""
	}
	queued := firstQueued(*held)
	since, counted := tookNothingSince(*held, queued)
	if !counted {
		return ""
	}
	from, floored := floor.count(since)
	if now.Sub(from) <= workflow.StrandedReleaseWake {
		return ""
	}
	if !architectServed(root, row, run, holdsGate) {
		return ""
	}
	unconfirmed := "No turn of that claim has confirmed it."
	switch {
	case task.Interrupted:
		unconfirmed = fmt.Sprintf("A let-go of the claim's process took it back from a turn of it, and no turn has confirmed it since %s, "+
			"the later of its first queueing and the moment the claim's relaunched agent was ready.", since.UTC().Format(time.RFC3339))
	case since.After(queued):
		unconfirmed = fmt.Sprintf("No turn of that claim has confirmed it since %s, when the claim's agent was last ready to take it.",
			since.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("the task of phase entry %d was first queued to the architect's claim of %s at %s. %s%s The root stands at %s. The daemon writes no second start while that task stands.",
		task.Entry, root.Key, queued.UTC().Format(time.RFC3339), unconfirmed, floored, root.Phase)
}

// tookNothingSince is the moment the first arm counts the pending task of held from, given when
// a task of that task's entry was first queued to the claim (queued, firstQueued), and false
// where it counts nothing of the task at this reading. held holds a pending task.
//
// The arm counts only time in which the architect could have taken the task. A claim's agent can
// take a prompt in the states ready, working and idle (supervise.SaidReady). A claim enters them
// only through its agent's ready (supervise's ready, the one move out of registered into ready),
// which writes the claim's last ready (record.RoleClaim.LastReady) on every launch, a relaunch's
// included. So in those states the task is counted from the later of its entry's first queueing
// and that ready: a task the agent has been able to take for more than the bound with no turn
// confirming it raises the arm. A task first queued while the agent was up is counted from that
// queueing, and one queued before a relaunch is counted from the relaunched agent's ready.
//
// In queued, launch_uncertain, launching, shim_connected and registered no agent is up to take a
// prompt, so the task counts nothing. A launch the wake pacing has parked stands there
// (supervise's waitOnMachine persists the claim launching), and the pacing lets launches out at
// its rate (worker_wakes_per_minute, 8 by default, one every 7.5 seconds). After a pause, the
// resume lets every launch the pause held out through it, so an architect ninety launches back
// waits about eleven minutes for its own, past the bound. Counted through that wait, its task
// would raise the arm before its agent was up, about a stall the resume's queue made, and spend
// the arm's one mark for the run. A wave of wakes no pause held queues launches the same way, and
// the rule is the same for it.
//
// A task a let-go took back from a turn (supervise.Delivery.Interrupted) is one a turn of the
// architect did confirm. Every relaunch takes the claim's task back so (supervise's
// Machine.interrupted): a death, an out-of-memory kill, the stale-plugin relaunch, a repoint, the
// stall backstop and a held suspension's deadline. The take-back copies the task under a new id,
// keeps its QueuedAt, clears its confirmation and marks it, and the task waits for the relaunch to
// send it again, whose ready counts it again. The mark is never cleared, so a task a later refusal
// takes back again is still counted from that ready, which is then the later moment anyway. In
// suspended, failed and retired such a task counts nothing: a suspended claim has no process, and
// its task waits for the resume, whose ready counts it again; a failed or a retired claim has no
// process either, and no start reaches it without a relaunch, which is the second arm's reading.
//
// A task no let-go took back, held by a suspended, failed or retired claim, is counted from the
// first queueing of its entry: no turn ever confirmed it, so the architect took nothing of it,
// and a failed claim that holds it raises the second arm beside this one.
//
// The first queueing of the entry is the moment rather than the task's own queueing where the
// entry was queued before: a task retired unconfirmed puts its entry back (supervise's
// retirePending), the sweep sends that entry again, and the new delivery is queued at the moment
// of that send. Counted from it, an architect that has left the entry's work untaken past the
// bound would have its count started again at every send.
func tookNothingSince(held record.RoleClaim, queued time.Time) (time.Time, bool) {
	switch {
	case supervise.SaidReady(held.State):
		if held.LastReady.After(queued) {
			return held.LastReady, true
		}
		return queued, true
	case held.Pending.Interrupted:
		return time.Time{}, false
	case supervise.IsGone(held.State):
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
// says, and "" for a root it does not read. Both stretches of time it reads, a queued start's age
// and the time the work of the entry has stood, are counted from no earlier than the end of the
// project's latest pause (floor): the pause held every start, so none of them could run while it
// stood.
//
// workflow.ArchitectRelaunchOf decides which start of the run is the daemon's next move, and the
// pass's own start (sent) is one too. The arm waits while such a start stands because the
// outbox's start of a failed or a retired claim relaunches it, on the session it kept, or a new
// one once supervise's Machine.forgetSession has dropped that (an architect's start asks for no
// new session): a
// controller woken for that claim would take `fresh_session`, which discards the session that
// start resumes, and the mark the wake writes would leave a real failure later in the run waking
// nobody. Every new run of a closed root makes that shape, since its close retires the
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
func architectClaimUnreachable(root record.Issue, row record.PhaseEntry, run record.RoleRun, sent bool, floor stallFloor, now time.Time) string {
	if !record.InWorkflow(root) {
		return ""
	}
	held := run.Claim
	if held != nil && held.State != supervise.StateFailed && held.State != supervise.StateRetired {
		return ""
	}
	relaunch := workflow.ArchitectRelaunchOf(run, floor.at, now)
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
	if row.Entry == 0 || row.RaisedAt.IsZero() {
		return ""
	}
	from, floored := floor.count(row.RaisedAt)
	if now.Sub(from) <= workflow.StrandedReleaseWake {
		return ""
	}
	queue := "no start of one queued in the last " + workflow.StrandedReleaseWake.String() + " waits to create it"
	if sent {
		queue = "the daemon wrote the start of its architect again in the same transaction as this wake"
	}
	return fmt.Sprintf("%s holds no architect claim at all, and %s. The work of phase entry %d has stood since %s.%s%s",
		root.Key, queue, row.Entry, row.RaisedAt.UTC().Format(time.RFC3339), floored, waits)
}

// architectSlowBeforeGate is the third arm: the root stands at phase.Admitted with no step of
// its design gate registered, more than workflow.ArchitectGateWake after the latest of these
// moments: its admission or the moment its architect's entry was last raised, whichever is later
// (since); the end of the project's latest pause (floor), since the pause stopped the architect,
// which could register no step while it stood; the let-out of the launch the latest pause held
// for its architect (letOut, the zero time for none); and, where a turn of its architect has
// confirmed a task of that entry, the moment it first did. It answers what the record says, with
// the moment it counted from, and "" for a root it does not read.
//
// The later of the admission and the raised entry is the issue's own decision. A root a person
// answered after a six hour wait comes back to admitted holding the admission row it has held all
// along, so its admitted moment is six hours old and the bound would be past the moment it
// resumed. The entry its resume raised is what says when the work came back to it. For a first
// admission and for a re-admission the two are one moment, since the promotion that takes the
// slot raises the entry in the same transaction (promoteHolds).
//
// The architect can take no turn of the entry before the wake pacing lets its launch out, and a
// resume lets every launch a pause held out through the pacing, 8 a minute by default. So a root
// the resume's own pass admits, or one whose architect the pause stopped, would otherwise count
// the architect's wait in that queue, about eleven minutes at ninety launches and longer in a
// larger project, against its 90 minutes. Two moments say when that wait ended. An architect
// that confirmed its entry before a pause, and that the pause stopped owing a launch, was let out
// at the stuck clock of its resume of that pause (readPauseLetOuts), which the reservation of
// that launch sets. And the claim's handshake of the entry (record.RoleClaim.Handshake) keeps
// when a turn first confirmed a task of it, which is after the let-out of the launch that turn
// ran in; that moment does not move at a later relaunch, since the handshake keeps the first
// confirmation of an entry that a turn of the worker made (supervise's EntryHandshake).
//
// While the handshake of the row's entry and the root's run holds no confirmation, the arm counts
// from since, the floor and the let-out alone, so the pacing's queue can feed it only then. A
// root whose architect has not confirmed a task of its entry 90 minutes after that moment is one
// where something other than the queue is wrong — a start that never runs, a task of no entry
// held ahead of the entry's, an architect that took nothing — and the arm tells of it.
//
// It reads a root standing at admitted and no other. A root a hand-over carried in stands in a
// phase of the run that carried it, where its tree's work is already running under a phase
// worker, and a gate it has not registered holds nothing up yet.
func architectSlowBeforeGate(root record.Issue, gate record.TreeGate, admittedAt time.Time, row record.PhaseEntry, run record.RoleRun,
	floor stallFloor, letOut, now time.Time,
) string {
	if root.Phase != phase.Admitted || len(gate.Rows) > 0 {
		return ""
	}
	since := admittedAt
	if row.RaisedAt.After(since) {
		since = row.RaisedAt
	}
	if since.IsZero() {
		return ""
	}
	from, counted := floor.count(since)
	if letOut.After(from) {
		from, counted = letOut, fmt.Sprintf(" That time is counted from %s, when the wake pacing let out its architect's launch, which pause %d held.",
			letOut.UTC().Format(time.RFC3339), floor.pause)
	}
	if held := run.Claim; held != nil && row.Entry != 0 && held.Handshake.Entry == row.Entry &&
		held.Handshake.Generation == root.Generation && held.Handshake.ConfirmedAt.After(from) {
		from, counted = held.Handshake.ConfirmedAt, fmt.Sprintf(" That time is counted from %s, when a turn of its architect first confirmed a task of phase entry %d.",
			held.Handshake.ConfirmedAt.UTC().Format(time.RFC3339), row.Entry)
	}
	if now.Sub(from) <= workflow.ArchitectGateWake {
		return ""
	}
	return fmt.Sprintf("%s has stood at admitted since %s with no step of its design gate registered, which is more than %s.%s",
		root.Key, since.UTC().Format(time.RFC3339), workflow.ArchitectGateWake, counted)
}
