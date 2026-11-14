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
// (admit's owedEntryRoles), and it is wider than record.AtWork deliberately. The two roots
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
	return ArchitectCarried(ctx, tx, store, root)
}

// ArchitectCarried is whether a hand-over created root at the generation it stands at
// (record.Store.HandedOverRoot), which is what says the phase it stands in came with it. A row of
// an earlier generation belongs to a run that has ended, so it says nothing of this one. It is
// the reading every caller past the promotion makes, since that promotion clears the mark on the
// issue (record.Issue.HandedOverFrom): the sweep's reach (ArchitectAtWork), the task its start
// derives (architectStartWork) and the daemon's narrowing of a held task (its rootTask).
func ArchitectCarried(ctx context.Context, tx pgx.Tx, store record.Store, root record.Issue) (bool, error) {
	handed, err := store.HandedOverRoot(ctx, tx, root.Key)
	if err != nil {
		return false, err
	}
	return handed != nil && handed.Generation == root.Generation, nil
}

// ArchitectStartPending says whether a start of a root architect is already on its way, and why,
// which is what stops the sweep writing a second row for the same entry while the first waits
// (admit's owedEntryRoles). It is owed, the daemon's own reading of a start a role has not
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
// already names (row): the same row admission's queued-root branch writes, enqueued directly
// rather than through StartOrOwe, so it waits for no admission slot and marks no movement (the
// root holds the row the cap counts from its promotion until its work passes its review, and
// holds none after it, record.KeepsSlot). It raises no entry, as no resume of the work an entry
// already names does (enter, SendOwedEntry).
//
// What it sends depends on what raised the entry, which the row's own task says. An entry the
// derivation raised (admission's promotion, RaiseArchitectEntryAndStart) carries a task that
// ends in architectStartTail (CarriesArchitectStart), and that task is derived again from the
// record as it stands rather than sent as written. The reason is the kept decisions such a task
// names: a kept decision is claimed by the outbox row that names it, and
// record.Postgres.UnclaimedKept locks and reads only what nothing claims, so a text composed once
// and sent again could name a decision another telling has already claimed. The derivation reads
// what is still unclaimed now, and EnqueueNaming claims exactly what this row names in the same
// transaction.
//
// An entry the resume of a root a person answered raised (ResumeAwaitedRoot) carries the
// resume's own task, which names the answer — which ask or which review, and where to read it —
// and no kept decision, so that text is sent as written. The answer is the work that entry owes,
// and the derivation cannot give it: its shapes are a tree's first act, which is false of a root
// whose architect has run in this generation, and they end in "no message is coming", which is
// false of a root whose person's answer stands (dispatch://INFRA-2614/comment/03dd9487-b44d-4508-bc0e-268fe951b17e).
// An entry with no task, which the upgrade gives every architect of the release before, is
// derived as an entry of the derivation's.
func (e *Engine) SendArchitectStart(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.TreeGate, row record.PhaseRow) error {
	if row.EntryTask != "" && !CarriesArchitectStart(row.EntryTask) {
		return e.enqueueArchitectStart(ctx, tx, root, row.EntryTask, nil, row.Entry)
	}
	task, kept, err := e.architectStartWork(ctx, tx, root, gate)
	if err != nil {
		return err
	}
	return e.enqueueArchitectStart(ctx, tx, root, task, kept, row.Entry)
}

// RaiseArchitectEntryAndStart hands root's architect work it was not already on: it raises that
// architect's phase entry with the task the derivation gives over the record as it stands, writes
// that task as the work of the entry on the architect's phases row (EnterArchitect, the call
// admission's promotion makes), and writes the start of that entry by SendArchitectStart's own
// path. It answers the entry it raised, and 0 where it wrote nothing.
//
// It is the sweep's one raise, for the root whose architect's row names no entry at all (admit's
// resumeSlottedWorkers). Every other start of this file resumes the work an entry already names and
// raises none.
//
// The derivation is read once and the row and the start carry that one text, because a kept
// decision is claimed by the outbox row that names it: deriving twice would compose a task from
// decisions the first text had already claimed. A derivation that gives no task writes nothing at
// all — an entry raised with no work on it would stand above every entry its claim was ever
// given, with nothing to send it — though the reach this is called under is the two roots the
// derivation serves (ArchitectAtWork, ArchitectStartTask).
func (e *Engine) RaiseArchitectEntryAndStart(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.TreeGate) (int64, error) {
	task, kept, err := e.architectStartWork(ctx, tx, root, gate)
	if err != nil {
		return 0, err
	}
	if task == "" {
		return 0, nil
	}
	entry, err := e.EnterArchitect(ctx, tx, root, task)
	if err != nil {
		return 0, err
	}
	return entry, e.enqueueArchitectStart(ctx, tx, root, task, kept, entry)
}

// architectStartWork is the task a root architect's start carries, derived from the record as it
// stands, and the kept decisions that task names, which the row that carries it claims.
func (e *Engine) architectStartWork(ctx context.Context, tx pgx.Tx, root record.Issue,
	gate record.TreeGate,
) (string, *record.KeptDecisions, error) {
	decided, kept, err := e.ParkedDecisionTask(ctx, tx, root)
	if err != nil {
		return "", nil, err
	}
	launches, err := e.store.ArchitectLaunches(ctx, tx, []string{root.Tree})
	if err != nil {
		return "", nil, err
	}
	// The derivation reads whether the root was carried only past phase.Admitted (its rule 1),
	// so the hand-over row is read there alone; a root at admitted, which is almost every start
	// the sweep writes, is given the same task either way.
	carried := false
	if root.Phase != phase.Admitted {
		if carried, err = ArchitectCarried(ctx, tx, e.store, root); err != nil {
			return "", nil, err
		}
	}
	return ArchitectStartTask(ArchitectStart{Root: root, Carried: carried, Gate: gate,
		Launch: launches[root.Tree], Decided: decided}), kept, nil
}

// enqueueArchitectStart writes the start row of root's architect for entry, carrying task and
// claiming the kept decisions that task names in this same transaction.
func (e *Engine) enqueueArchitectStart(ctx context.Context, tx pgx.Tx, root record.Issue, task string,
	kept *record.KeptDecisions, entry int64,
) error {
	return e.EnqueueNaming(ctx, tx, root.Key, root.Tree, record.SuperviseRequest{Op: "start", Tree: root.Tree,
		Role: claim.RoleArchitect, Generation: root.Generation, Task: task, Entry: entry, Kept: kept}, kept, e.now())
}
