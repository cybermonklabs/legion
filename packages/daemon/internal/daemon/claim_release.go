package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// claimReleaseWait caps how long a claim release row rides out Dispatch's refusal to take the
// claim from its holder (dispatch.Claimed), measured from the row's own creation: the row carries
// no moment of its own, and row.CreatedAt is when the daemon decided the holder should let the
// issue go. The usual refusal clears well inside it, because Dispatch makes a claim takeable at
// once for a holder its registry has lost and 20 minutes after an unresponsive one was last seen
// (packages/envoy/internal/dispatch/api/issue_claim.go, judgeClaim). The same rule leaves a
// holder the registry still calls live, and an unresponsive one it cannot date, holding the claim
// for as long as that lasts, so the hour is a cap on a refusal that may never clear rather than a
// bound those 20 minutes give.
const claimReleaseWait = time.Hour

// claimVerdict is what the daemon's own record makes of the holder Dispatch shows for an issue a
// claim release row answers: the one thing releaseClaim's transaction reads out of the record,
// and what it acts on once the transaction has committed.
type claimVerdict string

const (
	// claimLeft leaves the claim where it is with nothing logged: the issue runs again, the
	// daemon records nothing for it, its tree is the operator's, or the holder is a session the
	// daemon records in a tree of its own.
	claimLeft claimVerdict = "left"
	// claimTakeable is a claim held by a session the daemon records in the issue's own tree,
	// which is the one holder it takes a claim from.
	claimTakeable claimVerdict = "takeable"
	// claimUnattributed is a holder no claims row of the daemon's accounts for.
	claimUnattributed claimVerdict = "unattributed"
)

// releaseClaim releases the Dispatch claim of an issue the daemon took out of the workflow
// without closing it (record.ClaimRelease). The row carries the issue key alone, and the order
// below is what the row stands on: the holder comes from Dispatch and the issue from the
// daemon's own record when the row runs, never from when it was queued, because that is the
// reading that is current when the DELETE is sent. A person who claimed the issue since, a
// re-admission, a second park, or a holder that let the claim go on its own each end the row on
// their own, with no generation or holder to fence it.
//
// Only a claim held by a session the daemon records in that issue's own tree is released. A
// person's claim is theirs; the daemon's own session is a legitimate holder, as
// workflow.Unresumable treats it; a session that claims in another tree owns the issue instead;
// and a holder no claims row accounts for is one the daemon cannot attribute, which it leaves
// and logs rather than take a claim from a session it knows nothing about.
//
// A tree the operator holds is left alone whoever holds the claim, on the predicate a park of
// such a root reads (record.OperatorHoldsTree): the operator's tree shares the root's key, so its
// own session reads as a session of the issue's tree here, and the workflow may record nothing
// that acts on such a tree's claims (treelifecycle.Lifecycle.HeldByOperator). The test is here
// rather than where the row is queued because the operator can open the tree in the hour a row
// waits out a holder.
func (r *outbox) releaseClaim(ctx context.Context, row record.OutboxRow) error {
	if r.dispatch == nil {
		return errors.New("claim release executor has no Dispatch client")
	}
	issue, err := r.dispatch.GetIssue(ctx, row.Issue)
	if err != nil {
		return fmt.Errorf("read Dispatch issue %s before its claim release: %w", row.Issue, err)
	}
	switch {
	case issue.ClaimUnread:
		// Dispatch showed a claim naming no actor, so the read says neither who holds it nor
		// that nobody does: there is nothing to hold the tree test against.
		r.log.Warn("outbox claim release left a claim Dispatch names no actor for", "issue", row.Issue, "row", row.ID)
		return nil
	case issue.Claim == nil:
		return nil
	// The id a Dispatch claim carries is the session id the claims table holds, which is why the
	// store reads below take Claim.ID and the daemon's own session is tested against it
	// (workflow.Unresumable).
	case issue.Claim.Kind != "session", issue.Claim.ID == dispatch.DaemonSession(row.Issue):
		return nil
	}
	holder := issue.Claim
	// The record, the tree's lifecycle and the claims the holder is judged against are read in
	// one transaction, so they are one state of the record.
	verdict := claimLeft
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		recorded, err := r.records.Issue(ctx, tx, row.Issue)
		if err != nil || recorded == nil {
			return err
		}
		// The issue is live again, or its tree runs again at a later generation while Dispatch
		// still shows it parked: the claim belongs to the run, not to the park this row answered.
		if !record.Stopped(*recorded) {
			return nil
		}
		held, err := record.OperatorHoldsTree(ctx, r.records, tx, *recorded)
		if err != nil || held {
			return err
		}
		inTree, err := r.records.SessionClaimsTree(ctx, tx, recorded.Tree, holder.ID)
		if err != nil {
			return err
		}
		if inTree {
			verdict = claimTakeable
			return nil
		}
		elsewhere, err := r.records.SessionClaimsAnywhere(ctx, tx, holder.ID)
		if err != nil || elsewhere {
			return err
		}
		verdict = claimUnattributed
		return nil
	}); err != nil {
		return fmt.Errorf("read the record of %s before its claim release: %w", row.Issue, err)
	}
	switch verdict {
	case claimTakeable:
		if err := r.dispatch.ReleaseClaim(ctx, row.Issue); err != nil {
			if dispatch.Contended(err) {
				return claimContendedWait(row.Issue)
			}
			if dispatch.Claimed(err) {
				if r.now().UTC().Sub(row.CreatedAt) >= claimReleaseWait {
					r.log.Warn("outbox claim release gave up: the holder still holds the claim an hour after the row was written",
						"issue", row.Issue, "holder", holder.Holder())
					return nil
				}
				return claimHeldWait(holder.Holder())
			}
			return fmt.Errorf("release the Dispatch claim of %s: %w", row.Issue, err)
		}
	case claimUnattributed:
		r.log.Warn("outbox claim release left a claim the daemon cannot attribute to the issue's tree",
			"issue", row.Issue, "holder", holder.Holder())
	}
	return nil
}

// claimHeldWait is the wait a claim release answers while Dispatch still holds the claim for its
// holder (dispatch.Claimed): the holder has not stopped yet, and the row tries again on its
// backoff until claimReleaseWait is up. It deliberately wraps nothing of Dispatch's refusal, only
// its holder's name: settle computes permanentRefusal before it tests wait.ErrWaiting, so a wait
// errors.As could reach the *dispatch.Error through would be finished rather than retried.
func claimHeldWait(holder string) error {
	return claimWait{err: wait.Errorf("the Dispatch claim is still held by %s", holder)}
}

// claimContendedWait is the wait a claim release answers a contended refusal with
// (dispatch.Contended): the issue's holder changed twice while the request ran, so Dispatch
// judged nobody and applied nothing (issue_claim.go's answerContendedClaim). That is a routine
// retry rather than a failure, and it says nothing about the holder, so no contended answer ends
// the row by itself. It does not hold the row open either: the hour claimReleaseWait caps a
// refused release at is wall-clock time from row.CreatedAt, so the time these retries take
// counts toward it like any other, and a refusal naming the holder (dispatch.Claimed) after them
// can end the row at once. It wraps nothing of Dispatch's refusal, for the reason claimHeldWait
// gives.
func claimContendedWait(issue string) error {
	return claimWait{err: wait.Errorf("the Dispatch claim of %s was contended while it was released", issue)}
}

// claimWait is the wait a claim release row rides out, whichever refusal it answers: the claim
// still held by its holder (claimHeldWait) or a contended answer (claimContendedWait). It is
// routine (routineWait), so the hour's attempts stay at debug: the backoff caps at 64 seconds,
// so an hour costs about 57 of them, and every attempt from the seventh would otherwise be
// logged as a warning (waitWarnAttempts). A holder that has not stopped yet is not something an
// operator should be woken for; the hour running out is, and that line is its own. A contended
// refusal rides the same type for the same reason: Dispatch applied nothing, so the row has only
// to ask again.
type claimWait struct{ err error }

func (c claimWait) Error() string { return c.err.Error() }

func (c claimWait) Unwrap() error { return c.err }

func (claimWait) Routine() bool { return true }

// releaseParkedClaims queues one Dispatch claim release (record.ClaimRelease) for each issue of
// the boot's listing that still carries a claim while the daemon's own record says it stopped.
// Dispatch releases a claim at a close alone (its issue PATCH), so a park leaves the claim of the
// session the daemon stopped standing on the board.
//
// It is the boot's pass and not a periodic sweep because every park the daemon sees queues its
// own row (workflow.Engine.leave), so the only claims left for this to find are the ones parked
// before that row existed and any row lost with a restore of the database. The recovery caller
// passes boot false (recoverRepositories) and starts no pass.
//
// A summary carrying no claim is passed over before anything is read, which is the whole of the
// pass's cost: of the 1,585 issues of the live board, 1,426 carry none. What is left is read in
// one batch of the records, because the pass runs inside the boot's serialized transaction,
// holding the fact lock every boot fact waits on (intake.SerializeFacts), where a read per issue
// would be hundreds of round trips. Whether the issue stopped is the record's to say and never
// the listing's, because an unslotted root and a lingering root both keep an active Dispatch
// status (workflow.Engine.LeaveUnslotted leaves the root at the status it stood at). An issue the
// daemon does not record is left alone with nothing logged: it could attribute no claim of it.
//
// The pass attributes no claim itself. The row's executor re-reads Dispatch, the record and the
// tree's lifecycle when it runs (outbox.releaseClaim), so a person's claim, a session of another
// tree, a holder no claims row accounts for, a tree the operator holds and an issue running again
// all end the row there, on the reading that is current when the DELETE would be sent — which is
// why the operator-held test is the executor's alone and the pass makes none. The rows are
// drained by the outbox's own sweep rather than by a commit wake: attach stores the outbox the
// records' wake reaches (wakeOutbox) after the reconcile probe has run (daemon.go's boot stages).
func (w *workflowRuntime) releaseParkedClaims(ctx context.Context, tx pgx.Tx, scope []dispatch.IssueSummary) error {
	keys := make([]string, 0, len(scope))
	for _, summary := range scope {
		if summary.ClaimedBy == "" && !summary.ClaimUnread {
			continue
		}
		if summary.Status == record.DoneStatus {
			continue
		}
		keys = append(keys, summary.Key)
	}
	issues, err := w.records.IssuesOf(ctx, tx, w.dispatchProject, keys)
	if err != nil {
		return fmt.Errorf("read the records of the claimed issues of %s before their claim release: %w", w.dispatchProject, err)
	}
	recorded := make(map[string]*record.Issue, len(issues))
	for i := range issues {
		recorded[issues[i].Key] = &issues[i]
	}
	for _, key := range keys {
		issue := recorded[key]
		if issue == nil || !record.Stopped(*issue) {
			continue
		}
		row, err := record.NewOutboxRow(key, record.ClaimRelease{}, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := w.records.Enqueue(ctx, tx, row); err != nil {
			return fmt.Errorf("queue the Dispatch claim release of %s: %w", key, err)
		}
		intake.OnCommit(ctx, func() {
			w.log.Info("workflow: boot queues a Dispatch claim release for an issue out of the workflow",
				"issue", issue.Key, "tree", issue.Tree, "status", issue.Status)
		})
	}
	return nil
}
