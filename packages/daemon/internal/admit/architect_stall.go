package admit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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
// selection's (readSlotHolders): a root holding a slot and a root past its review alike. A root
// in accepting, retro or merging holds no slot since cybermonklabs/legion#3573, so the first two
// arms read it here as they read it while it held one. The third reads a root at admitted alone,
// whose slot gives it the admission time its bound is weighed from. sent names the roots whose
// architect start this pass wrote (resumeSlottedWorkers), which the second arm reads; the
// sweep's gate and the state pass none, since the gate asks only where the pass would send
// nothing (OwesEntryWork) and the state reads what the passes before it committed
// (ArchitectStallArms). Each root's arms are read at that root's own reading of the clock.
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
		took, err := a.architectTookNothing(ctx, tx, root, run, now)
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
// it so (workflow.StartFor, workflow.ArchitectStartPending).
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
		intake.OnCommit(ctx, func() {
			a.log.Warn("admission: the controller was told a root's architect has stalled",
				"issue", stall.root.Key, "arm", string(stall.arm), "phase", stall.root.Phase,
				"generation", stall.root.Generation, "reason", reason)
		})
	}
	return nil
}

// architectTookNothing is the first arm: the architect's claim holds the task of the entry it
// was given, that task was queued more than workflow.StrandedReleaseWake ago, and no turn of the
// claim has confirmed it. It answers what the record says, and "" for a root it does not read.
//
// The task is read as the delivery carrying the entry the claim was given — the task the claim's
// own handshake was written from (supervise.Claim.Handshake), whose two times the state shows
// beside the two numbers (api.Issue.EntryQueuedAt) — rather than as the entry the phases row
// names, because a claim still holding an older entry's task unconfirmed took nothing either,
// and the sweep writes no newer start for it while that task stands
// (workflow.ArchitectStartPending).
//
// The root must also be one the sweep serves (architectServed): no root outside the reach is
// sent an architect's task at all, so none can have failed to take one, and a root that cannot
// hear is one nothing a controller does reaches, so it raises nothing, while one that could not
// hear earlier and can now raises the arm, because the task it was owed never arrived. Those
// readings are asked last, so a root whose claim holds no such task costs no read.
func (a *Admission) architectTookNothing(ctx context.Context, tx pgx.Tx, root record.Issue, run record.RoleRun,
	now time.Time,
) (string, error) {
	held := run.Claim
	if held == nil || held.Pending == nil {
		return "", nil
	}
	task := held.Pending
	if task.Entry == 0 || task.Entry != held.GivenEntry || !task.ConfirmedAt.IsZero() {
		return "", nil
	}
	if task.QueuedAt.IsZero() || now.Sub(task.QueuedAt) <= workflow.StrandedReleaseWake {
		return "", nil
	}
	served, err := a.architectServed(ctx, tx, root)
	if err != nil || !served {
		return "", err
	}
	return fmt.Sprintf("the task of phase entry %d was queued to the architect's claim of %s at %s. No turn of that claim has confirmed it. The root stands at %s. The daemon writes no second start while that task stands.",
		task.Entry, root.Key, task.QueuedAt.UTC().Format(time.RFC3339), root.Phase), nil
}

// architectClaimUnreachable is the second arm: the root is in the workflow and either its
// architect's claim is failed or retired with no start of the root's run queued to relaunch it,
// or the root holds no architect claim at all while the work of its entry has stood more than
// workflow.StrandedReleaseWake. It answers what the record says, and "" for a root it does not
// read.
//
// A failed or a retired claim is read only where no start will reach it: no start of the root's
// generation waits in the queue (workflow.StartFor, the reading workflow.Stranded makes of a tree
// architect's claim, over run's queued rows, which RoleRuns reads at the root's generation), and
// the pass did not write one itself (sent). The outbox's start of a failed or a retired claim
// relaunches it on the session it kept, so a claim such a start will reach is the daemon's own
// next move rather than a stall: a controller woken for it would take `fresh_session`, which
// discards the session that start resumes, and the mark the wake writes would leave a real failure
// later in the run waking nobody. Every re-admission of a closed tree makes that shape, since the
// linger's end retires the architect's claim and the promotion queues its start in the same pass
// that reads this arm. A root waiting on a person is read the same way, as workflow.Stranded reads
// it: the person's answer resumes its architect with a start of its own (ResumeAwaitedRoot), so
// a failed or a retired claim of such a root is relaunched by the daemon's next move too.
//
// A root with no claim is read unless a start of one already waited in the outbox when the pass
// began (workflow.ArchitectStartPending, the sweep's own reading), since a queued start is the
// daemon's own next move and the claim is created by running it. run is the reading the pass made
// before its sends, so where the pass itself wrote the start again (sent) the wake still stands —
// a root with no claim that long is stalled whatever this pass writes — and its reason says the
// start was written, rather than that none waits.
func architectClaimUnreachable(root record.Issue, row record.PhaseEntry, run record.RoleRun, sent bool, now time.Time) string {
	if !record.InWorkflow(root) {
		return ""
	}
	if held := run.Claim; held != nil {
		if held.State != supervise.StateFailed && held.State != supervise.StateRetired {
			return ""
		}
		if sent || root.AwaitingPerson() || !workflow.StartFor(run, root, root.Generation, "") {
			return ""
		}
		return fmt.Sprintf("the architect claim of %s is %s, no start of its run waits in the queue to relaunch it, and the root stands at %s in the workflow.",
			root.Key, held.State, root.Phase)
	}
	if _, pending := workflow.ArchitectStartPending(run); pending {
		return ""
	}
	if row.Entry == 0 || row.RaisedAt.IsZero() || now.Sub(row.RaisedAt) <= workflow.StrandedReleaseWake {
		return ""
	}
	queue := "no start of one waits in the queue"
	if sent {
		queue = "the daemon wrote the start of its architect again in the same transaction as this wake"
	}
	return fmt.Sprintf("%s holds no architect claim at all, and %s. The work of phase entry %d has stood since %s.",
		root.Key, queue, row.Entry, row.RaisedAt.UTC().Format(time.RFC3339))
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
// It reads a root standing at admitted and no other. A root a hand-over carried in stands in the
// phase that came with it, where its tree's work is already running under a phase worker, and a
// gate it has not registered holds nothing up yet.
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
