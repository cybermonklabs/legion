package workflow

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// holdSessionWrite holds write, the status write fact of a session on issue, behind the writes the
// record already holds (record.Issue.HeldSessionWrites), in Dispatch's order: the record takes
// the event's observation with its own status kept (observedIssue), so admission, running after
// this handler, stops at the event's sequence, and nothing is taken or set back yet. While issue's
// Dispatch chain is unknown that is all, and the admission pass that follows in this fact notes
// the key for the daemon to read Dispatch again (admit's judgeHeldStatusWrites). On a known chain,
// a list held while it was unknown whose chain the copy came to know before any pass judged it,
// the whole list is judged in order at once (judgeHeld), this write last (decision 24 of
// dispatch://INFRA-4730).
func (e *Engine) holdSessionWrite(ctx context.Context, tx pgx.Tx, chains *record.Chains, issue record.Issue, fact intake.DispatchIssue, write record.HeldSessionWrite) error {
	held := observedIssue(issue, fact)
	held.HeldSessionWrites = append(slices.Clone(issue.HeldSessionWrites), write)
	if chains.Chain(issue.Key).Known() {
		return e.judgeHeld(ctx, tx, held)
	}
	e.logOnCommit(ctx, "workflow: a session wrote the status of an issue whose Dispatch chain is unknown; the daemon holds the write until the chain is known",
		"issue", issue.Key, "session", write.Session, "wrote", write.Status, "held", len(held.HeldSessionWrites))
	return e.store.PutIssue(ctx, tx, held)
}

// JudgeHeldStatusWrites judges the session writes the repair holds on issue
// (record.Issue.HeldSessionWrites) once the copy of Dispatch knows issue's chain, one by one in
// Dispatch's order (judgeHeld), and clears them in tx, the transaction it judges them in, so no
// later pass of the same transaction judges them again. Admission's decision pass calls it for
// each issue that holds any, the pass that follows an event and the boot's Reconcile alike
// (decision 24 of dispatch://INFRA-4730), with chains, its one reading of the held issues' own
// chains: whether a chain is known is the copy's to say, which no judgement here writes. While the
// chain is still unknown it judges nothing and answers false, and the caller notes the key for
// the daemon to read Dispatch again.
func (e *Engine) JudgeHeldStatusWrites(ctx context.Context, tx pgx.Tx, chains *record.Chains, issue record.Issue) (bool, error) {
	if len(issue.HeldSessionWrites) == 0 {
		return true, nil
	}
	if !chains.Chain(issue.Key).Known() {
		return false, nil
	}
	return true, e.judgeHeld(ctx, tx, issue)
}

// judgeHeld judges issue's held session writes in Dispatch's order by the repair's rule
// (judgeSessionWrite), each as if it came now, on the chain as it stands, and clears them. A write
// taken moves the record as a person's move of that status does (applyStatus): it leaves the
// workflow, or comes back to it, over every status write the daemon queued. A write set back
// leaves the record at the status the writes before it left it, and an outside session's set-back
// is told as at its event (tellSetBack). A write of the status the record already holds moves
// nothing, as its event would not. On a chain no issue of which carries the legion label every
// write is anyone's to make, so each is taken.
//
// Dispatch shows the last write, or the daemon's own echo after it, which the record holds
// (DispatchStatus), so the daemon queues at most one write for the whole list: the record's final
// status, where it differs from what Dispatch shows, at the last held write's sequence, which
// Dispatch refuses where the issue has moved since, so the row reads the issue again
// (record.ReassertStatusWrite). So an architect above's done followed by the write of a session the
// repair sets back ends at done.
func (e *Engine) judgeHeld(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	writes, shown := issue.HeldSessionWrites, issue.DispatchStatus
	current := issue
	current.HeldSessionWrites = nil
	if err := e.store.PutIssue(ctx, tx, current); err != nil {
		return err
	}
	// The chain a write is judged on is read once and again only after a write is taken, which
	// moves records (applyStatus); a set-back writes none, so the next write reads what it read.
	var chains *record.Chains
	for _, write := range writes {
		if write.Status == current.Status {
			continue
		}
		if chains == nil {
			read, err := record.ReadChains(ctx, tx, e.store, current.Project, []string{current.Key})
			if err != nil {
				return err
			}
			chains = read
		}
		var err error
		verdict, agent := writeTaken, false
		if legionOnChain(chains, current.Key) {
			if verdict, agent, err = e.judgeSessionWrite(ctx, tx, chains, current, write); err != nil {
				return err
			}
		}
		if verdict == writeSetBack {
			e.logOnCommit(ctx, "workflow: a held session write is set back", "issue", current.Key,
				"session", write.Session, "claims_tree", agent, "wrote", write.Status, "status", current.Status)
			if err := e.tellSetBack(ctx, tx, current, write, agent); err != nil {
				return err
			}
			continue
		}
		e.logOnCommit(ctx, "workflow: a held session write is taken", "issue", current.Key,
			"session", write.Session, "wrote", write.Status)
		if err := e.applyStatus(ctx, tx, current, heldWriteFact(current, write)); err != nil {
			return err
		}
		moved, err := e.store.Issue(ctx, tx, current.Key)
		if err != nil {
			return err
		}
		current, chains = *moved, nil
	}
	// A move in work reads the held write as Dispatch's latest status (observedIssue); the record
	// keeps what Dispatch shows now.
	if current.DispatchStatus != shown {
		current.DispatchStatus = shown
		if err := e.store.PutIssue(ctx, tx, current); err != nil {
			return err
		}
	}
	if current.Status == shown {
		return nil
	}
	return e.enqueue(ctx, tx, current.Key, record.ReassertStatusWrite(current.Status, shown, writes[len(writes)-1].Seq))
}

// heldWriteFact is write, a held session write of issue, as the event the workflow takes it as:
// the issue as its record observes it, at the record's own sequence, so taking the write moves no
// mark of what the record has read, and write's status by write's session.
func heldWriteFact(issue record.Issue, write record.HeldSessionWrite) intake.DispatchIssue {
	parent := ""
	if issue.Parent != nil {
		parent = *issue.Parent
	}
	return intake.DispatchIssue{Key: issue.Key, ResolvedRepo: issue.Repo, Seq: issue.LastDispatchSeq, Type: "issue.updated",
		Status: write.Status, Title: issue.Title, Rank: issue.Rank, Priority: issue.Priority, Resolved: issue.Resolved, Parent: parent, HandedOver: issue.HandedOver,
		ActorSession: write.Session}
}
