package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// AdoptionRefusal says why issue cannot join tree for what its own record says, nil when it can. A
// recorded root that never ran as a tree of its own (RanAsRoot), whose tree's live root is tree's,
// is a member of that tree: whether tree is live is the caller's to read, once for every issue it
// asks about, and a release of children refuses a tree that is not live by name
// (intake.ChildNotAdoptable). A child already recorded in tree needs nothing; one recorded in any
// other tree is that tree's, not tree's to adopt (intake.ChildInOtherTree), unless that tree is
// closed by the words of C4 (closedTreesMember), which the tree that takes it adopts. Every
// refusal is a 409 whose message names the issue and why.
//
// C4 of dispatch://INFRA-3107 is the one clause that takes a member rather than a root: a member
// of a closed tree that never started a phase joins the tree whose architect releases it
// (AdoptMember), and nothing else about a member is adoptable. A member of a tree that is live is
// another architect's work and keeps its refusal, as does one of a parked tree, one of a tree
// still in its linger and one in any phase past admitted.
//
// Admission's sweep of its queue (adoptQueuedRoots) leaves a refused root queueing as a root of
// its own; a release of children answers the refusal. That sweep asks only about issues that are
// their own tree's root, so no member reaches it and nothing moves because a pass ran: only an
// architect's release adopts a member. Callers check it before Adopt or AdoptMember.
func (e *Engine) AdoptionRefusal(ctx context.Context, tx pgx.Tx, issue record.Issue, tree string) (*intake.Refusal, error) {
	if issue.Tree == tree {
		return nil, nil
	}
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		adoptable, err := e.closedTreesMember(ctx, tx, issue)
		if err != nil || adoptable {
			return nil, err
		}
		return intake.ChildInOtherTree(issue.Key, issue.Tree, tree), nil
	}
	ran, err := e.RanAsRoot(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	if ran != "" {
		return intake.ChildNotAdoptable(issue.Key, tree, ran), nil
	}
	return nil, nil
}

// closedTreesMember says whether member is the member C4 of dispatch://INFRA-3107 lets another
// tree take: it stands at phase.Admitted, so it never started a phase, and its own tree's root is
// recorded, finished (record.DoneStatus) and has no linger left to run (record.Lingering, read
// against the engine's clock). Those are the words the held list reads for tree_closed over the
// same records (record.HeldReasonOf), so an issue the tick says is held for a closed tree is the
// issue a release is accepted for.
//
// The linger is the one still running, never Issue.Lingers, which answers whether a deadline was
// ever armed: every close arms one (beginLinger) and nothing but a re-admission clears it, so
// the broader reading would refuse every member of every tree the daemon has ever closed, which
// is the whole of what C4 opens.
//
// Status is the daemon's own status field, never DispatchStatus: it is what record.OutOfWorkflow
// reads and what the linger's own hand-over of a closed tree's members reads, so the word, the
// hand-over and this agree about one root. The phase is read before the root, because a member in
// a later phase is refused whatever its root says and needs no read of its own.
//
// Whether the member holds an admission slot is the caller's to read, once for every issue it
// asks about, as the liveness of the tree that takes it is: C4 covers a member that holds none,
// and a release of children refuses one that does by name (admit's adoptChildren).
func (e *Engine) closedTreesMember(ctx context.Context, tx pgx.Tx, member record.Issue) (bool, error) {
	if member.Phase != phase.Admitted {
		return false, nil
	}
	root, err := e.store.Issue(ctx, tx, member.Tree)
	if err != nil || root == nil {
		return false, err
	}
	return root.Status == record.DoneStatus && !record.Lingering(*root, e.now()), nil
}

// RanAsRoot says how issue ran as a tree of its own, "" when it never did: it opened a tree
// lifecycle under its key (only its promotion opens one, or an operator's root spawn), or its
// record carries a run of its own (record.Issue.CarriesRun, or the run's mark
// record.Issue.TakenBack: the record blockers.Exempt reads for work already under way). The
// lifecycle is the fact that the root took a slot and an architect, which a run's record does not
// hold: promotion opens it while the root still stands at phase.Admitted, and the mark is written
// only when a phase begins. The run's record covers a root that carries a run handed over to it: a
// child carries its run only from a phase, whose start wrote the mark, and re-admission keeps the
// mark with that run; one handed over without its run has it cleared, having run nothing as a
// root. The members of a root that never ran cannot have started a phase either: a phase needs its
// gate, the gate needs the root's architect, and the architect needs a slot, whose promotion opens
// the lifecycle. A root that ran is never adopted (AdoptionRefusal) nor held under an unreleased
// child of another tree (Ancestry).
//
// A root that left another tree as a root of its own (record.Issue.HandedOverFrom: a child its
// finished tree handed over, or an orphan admitted, carrying a run or not) is the same: it stays
// its own tree, so neither re-admitting the root it left nor any other tree's release takes it
// back, though it never took a slot. Only a labelled root that never left a tree and never ran
// (one a person made a child of a live tree) is adopted.
func (e *Engine) RanAsRoot(ctx context.Context, tx pgx.Tx, issue record.Issue) (string, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", err
	}
	lifecycle, found, err := e.store.TreeLifecycle(ctx, tx, project, issue.Key)
	if err != nil {
		return "", err
	}
	if found {
		if lifecycle.HeldByOperator() {
			return "the operator runs a tree under its key", nil
		}
		return "it ran as a tree of its own", nil
	}
	if issue.CarriesRun() || issue.TakenBack {
		return "it carries a run handed over to it as a root of its own", nil
	}
	if issue.HandedOverFrom != "" {
		return "it was handed over as a root of its own", nil
	}
	return "", nil
}

// Ancestry is the Dispatch chain issue's record takes when an observation's preparation read chain
// (record.Issue.Ancestry): chain, or nil when issue ran as a tree of its own (RanAsRoot). The
// waiting line holds a root out while a live root is on its chain (the ancestry hold, under an
// unreleased child of that tree) until that root's release adopts it, and a release refuses a root
// that ran, so such a root keeps queueing as its own instead: held, nothing would ever bring it in.
// Every write of an observation's chain to a record goes through this, with the record as it will
// be written: RanAsRoot reads its run's mark.
func (e *Engine) Ancestry(ctx context.Context, tx pgx.Tx, issue record.Issue, chain []string) ([]string, error) {
	if len(chain) == 0 {
		return nil, nil
	}
	ran, err := e.RanAsRoot(ctx, tx, issue)
	if err != nil || ran != "" {
		return nil, err
	}
	return chain, nil
}

// Adopt moves issue, a recorded root AdoptionRefusal found adoptable into root's live tree, into
// that tree with every record of its own tree, its parent recorded as parent; each member keeps
// its own. Each record takes its next generation, the fence every row of the old tree already
// checks (its linger's close, its supervise rows, its issue suspend), with no linger, hold,
// pending READY or Dispatch chain (record.Issue.Ancestry: a member waits in no line), no
// hand-over fence and its generation's facts cleared. A todo record enters the tree's run
// admitted, as an unrecorded todo child does (enterChild), so it starts its first phase at the
// slot the waiting line gives it, unless it waits on an unfinished Dispatch issue, which keeps it
// out of that line until the last of them closes. Any other status
// keeps it as a parked child at done, which a todo runs again (ReenterChild).
// The tree's architect is told with a child-status notice. Everything is written in the caller's
// transaction.
func (e *Engine) Adopt(ctx context.Context, tx pgx.Tx, issue, root record.Issue, parent string) error {
	members, err := e.store.TreeIssues(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.Generation == ^uint64(0) {
			return fmt.Errorf("adopt %s into %s: generation overflows", member.Key, root.Tree)
		}
		if member.Key == issue.Key {
			member.Parent = record.ParentOf(parent)
		}
		next := member.Generation + 1
		if err := e.store.DeleteHandover(ctx, tx, member.Key); err != nil {
			return err
		}
		if err := e.store.ClearGeneration(ctx, tx, member.Key, e.now()); err != nil {
			return err
		}
		if member.Status == "todo" {
			if err := e.enterChild(ctx, tx, root, RecordedFact(member), next, recordedResolution(member)); err != nil {
				return err
			}
			continue
		}
		member.Tree, member.Project, member.Generation, member.Phase = root.Tree, root.Project, next, phase.Done
		member.LingerUntil, member.Hold, member.ReadyPending, member.Ancestry = nil, nil, nil, nil
		if err := e.store.PutIssue(ctx, tx, member); err != nil {
			return err
		}
	}
	if err := e.catchUpOnWhatTheTreeIsOwed(ctx, tx, root); err != nil {
		return err
	}
	if err := e.notice(ctx, tx, issue.Key, joinedTheTree(issue.Key, root.Key)); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a queued root joined a live tree", "issue", issue.Key, "tree", root.Tree, "generation", issue.Generation+1)
	return nil
}

// joinedTheTree is the child-status notice an adoption sends the architect of the tree that
// takes an issue, so that architect reads that the issue joined. Both routes send it — a queued
// root with its own tree's members (Adopt) and the one member of a closed tree a release takes
// (AdoptMember, C4 of dispatch://INFRA-3107) — and it is written here once, so the architect
// reads one sentence whichever route moved the issue.
func joinedTheTree(issue, root string) record.Notice {
	return record.Notice{Kind: "child-status", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("%s joined the tree of %s from the admission queue", issue, root)}
}

// catchUpOnWhatTheTreeIsOwed writes root's architect a catch-up when a person's decision is kept
// for the tree and nobody in it has heard it. What a person decided on an adopted issue while it
// waited is kept under that issue, so the tree it now stands in owes it (Store.UnclaimedKept
// reads by the issue's tree now). The tree's architect already runs and no ready is coming, so
// it is written a catch-up now, as a release's hand-over does (CatchUpTheTreesOwed); an
// architect that does not hear yet is written nothing, and its next start or ready names them.
//
// Both adoption routes call it, the queued root's (Adopt) and the closed tree's member's
// (AdoptMember): the issue each takes can carry a decision a person made on it while its own
// tree ran nothing, and a tree that hears it only at its architect's next launch hears it hours
// later or not at all.
func (e *Engine) catchUpOnWhatTheTreeIsOwed(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	decisions, approval, err := e.store.UnclaimedKept(ctx, tx, root.Tree)
	if err != nil {
		return err
	}
	if len(decisions) == 0 && approval.Approved == 0 {
		return nil
	}
	return CatchUpTheTreesOwed(ctx, tx, e.store, e.cfg.Steps, []string{root.Tree}, e.now())
}

// AdoptMember moves member, a member of a closed tree AdoptionRefusal found adoptable
// (closedTreesMember), into root's live tree under the Dispatch parent parent: C4 of
// dispatch://INFRA-3107, the route a member of a tree nobody runs again takes into the tree whose
// architect releases it. It is Adopt's body for the one issue the release names, and moves no
// other member of the old tree: the route takes the issue the architect released, and each other
// member stands in admission.held with tree_closed until an architect releases it too.
//
// The record takes its next generation, the fence every row of the old tree already checks (its
// linger's close, its supervise rows, its issue suspend), with its hand-over fence deleted and
// its generation's facts cleared, and enters the tree admitted on the repository it was first
// recorded with (enterChild, recordedResolution), as an unrecorded todo child does: no generation
// is reset, and nothing starts it but the slot the waiting line gives it. The tree's architect is
// told with a child-status notice (joinedTheTree), and with a catch-up where a person's decision
// on the member is kept for it (catchUpOnWhatTheTreeIsOwed). Everything is written in the
// caller's transaction.
func (e *Engine) AdoptMember(ctx context.Context, tx pgx.Tx, member record.Issue, root record.Issue, parent string) error {
	if member.Generation == ^uint64(0) {
		return fmt.Errorf("adopt %s into %s: generation overflows", member.Key, root.Tree)
	}
	fact := RecordedFact(member)
	fact.Parent = parent
	if err := e.store.DeleteHandover(ctx, tx, member.Key); err != nil {
		return err
	}
	if err := e.store.ClearGeneration(ctx, tx, member.Key, e.now()); err != nil {
		return err
	}
	if err := e.enterChild(ctx, tx, root, fact, member.Generation+1, recordedResolution(member)); err != nil {
		return err
	}
	if err := e.catchUpOnWhatTheTreeIsOwed(ctx, tx, root); err != nil {
		return err
	}
	if err := e.notice(ctx, tx, member.Key, joinedTheTree(member.Key, root.Key)); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a closed tree's member joined a live tree", "issue", member.Key,
		"tree", root.Tree, "from", member.Tree, "generation", member.Generation+1)
	return nil
}
