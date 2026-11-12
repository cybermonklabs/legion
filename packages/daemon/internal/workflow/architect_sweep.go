package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// ArchitectAtWork says whether a root's own architect is the role at work where that root stands.
// It is the entry sweep's reach over the architect, where record.AtWork is every other role's
// (admit's resumeSlottedWorkers), and it is wider than record.AtWork deliberately. The two roots
// it answers true for are the two the first-task derivation serves (ArchitectStartTask), so the
// rule reaches exactly the roots it has work to send:
//
//   - a root standing at phase.Admitted, whatever its design gate. record.AtWork answers false
//     for such a root once its gate has opened, because no phase worker of it works there; its
//     own architect still does, since it is the only role of the tree at work, and the measured
//     defect is a relaunched architect of such a root (dispatch://INFRA-2614).
//   - a root a hand-over carried in while it stands in the phase that came with it, which the
//     record states as the hand-over row of this generation (record.Store.HandedOverRoot, the
//     reading the catch-up makes) and a phase that carries a run (record.Issue.CarriesRun). The
//     mark on the issue is no use here: the promotion that opens the root's own tree clears it
//     (record.Issue.HandedOverFrom), and this is read after that promotion.
//
// It reaches no wider. A root past phase.Admitted under its own power holds no hand-over row and
// has a phase role that owns its work; a root in accepting is told by the notice that moved it
// there, and a root waiting on a person by the resume the daemon composes when the answer lands,
// and neither is at phase.Admitted. A child at phase.Admitted is not its tree's root, so a
// child's own sub-architect claim is outside the rule, which ArchitectHears answers for again
// before anything is sent.
//
// Whether that architect can hear what the rule would send is a question of its own
// (ArchitectHears), and whether a start is already on its way is a third (ArchitectStartPending).
// The store read is made for a slotted tree root past phase.Admitted alone: a child and a root at
// phase.Admitted are answered from the record in hand.
func ArchitectAtWork(ctx context.Context, tx pgx.Tx, store record.Store, root record.Issue) (bool, error) {
	if !claim.IsTreeRoot(root.Key, root.Tree) {
		return false, nil
	}
	if root.Phase == phase.Admitted {
		return true, nil
	}
	if !root.CarriesRun() {
		return false, nil
	}
	return architectCarried(ctx, tx, store, root)
}

// architectCarried is whether a hand-over created root at the generation it stands at
// (record.Store.HandedOverRoot), which is what says the phase it stands in came with it. A row of
// an earlier generation belongs to a run that has ended, so it says nothing of this one.
func architectCarried(ctx context.Context, tx pgx.Tx, store record.Store, root record.Issue) (bool, error) {
	handed, err := store.HandedOverRoot(ctx, tx, root.Key)
	if err != nil {
		return false, err
	}
	return handed != nil && handed.Generation == root.Generation, nil
}

// ArchitectStartPending says whether a start of a root architect is already on its way, and why,
// which is what stops the sweep writing a second row for the same entry while the first waits
// (admit's resumeSlottedWorkers). It is owed, the daemon's own reading of a start a role has not
// taken, with its one arm that does not hold here taken off: owed answers "has no claim yet" for
// a tree whose architect claim does not exist, and that tree is the very one the sweep writes the
// start for. With no claim, a start still queued is the whole of what holds the row back.
func ArchitectStartPending(run record.RoleRun) (string, bool) {
	if run.Claim == nil {
		if queuedStart(run) {
			return "has a start still queued", true
		}
		return "", false
	}
	return owed(run)
}

// SendArchitectStart writes the start of root's architect again, for the entry its phases row
// already names: the same row admission's queued-root branch writes, enqueued directly rather
// than through StartOrOwe, so it waits for no admission slot (the root holds the row the cap
// counts from its promotion until its work ends, record.KeepsSlot). It raises no entry, as no
// resume of the work an entry already names does (enter, SendOwedEntry).
//
// The task is derived again from the record as it stands rather than taken from the row's
// EntryTask, which is what every other role is sent. The reason is the kept decisions a root
// architect's task names: a kept decision is claimed by the outbox row that names it, and
// record.Postgres.UnclaimedKept locks and reads only what nothing claims, so a text composed once
// and sent again could name a decision another telling has already claimed. The derivation reads
// what is still unclaimed now, and EnqueueNaming claims exactly what this row names in the same
// transaction.
func (e *Engine) SendArchitectStart(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.TreeGate, entry int64) error {
	decided, kept, err := e.ParkedDecisionTask(ctx, tx, root)
	if err != nil {
		return err
	}
	launches, err := e.store.ArchitectLaunches(ctx, tx, []string{root.Tree})
	if err != nil {
		return err
	}
	carried, err := architectCarried(ctx, tx, e.store, root)
	if err != nil {
		return err
	}
	task := ArchitectStartTask(ArchitectStart{Root: root, Carried: carried, Gate: gate,
		Launch: launches[root.Tree], Decided: decided})
	return EnqueueNaming(ctx, tx, e.store, root.Key, root.Tree, record.SuperviseRequest{Op: "start", Tree: root.Tree,
		Role: claim.RoleArchitect, Generation: root.Generation, Task: task, Entry: entry, Kept: kept}, kept, e.now())
}
