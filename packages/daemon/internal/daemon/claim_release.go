package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
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

// claimVerdict is what releaseClaim's transaction makes of the holder Dispatch shows, against the
// daemon's own record: whether the row sends the DELETE, or leaves the claim and writes why it
// stays (record.Issue.ClaimLeft). The DELETE goes out after that transaction and never inside it,
// so no transaction of the record waits on Dispatch. The line the executor logs once that
// transaction has committed is read off the reason it wrote: one for a claim Dispatch names no
// actor for (record.ClaimLeftUnread), which leaves nothing to judge a holder by, and one for a
// holder whose name no claim of the daemon's project recorded (record.ClaimLeftUnattributed).
type claimVerdict string

const (
	// claimStays leaves the claim where it is, and the transaction wrote why: the reason, or ""
	// for an issue Dispatch shows no claim on, which clears what an earlier row wrote.
	claimStays claimVerdict = "stays"
	// claimUnrecorded leaves the claim of an issue the daemon records nothing for: it can attribute
	// no claim of it, and there is no record to say so in.
	claimUnrecorded claimVerdict = "unrecorded"
	// claimTakeable is a claim the row asks Dispatch to release; Dispatch's answer decides what is
	// written (takeClaim).
	claimTakeable claimVerdict = "takeable"
)

// releaseClaim releases the Dispatch claim (record.ClaimRelease) of a session the daemon records on
// the issue itself, or in the tree of the gate holder the issue stands under on the Dispatch chain
// (record.Chains.StateTree and GateTree, R18 of dispatch://INFRA-3320), once nothing of the
// daemon's runs in that session for the issue. Dispatch releases a claim at a close alone, so a
// claim the daemon gave up any other way would otherwise stand on the board, and the next agent of
// the issue would find it claimed by a session that is gone. The row comes in two shapes, and each
// reads the holder from Dispatch and the issue from the daemon's own record when it runs, never
// from when it was queued, because that is the reading that is current when the DELETE is sent: a
// person who claimed the issue since, a second move or a holder that let the claim go on its own
// each end the row on their own. The row of a move carries its fence, so a return to work or a new
// run stands at a later phase entry or run, at whose fence the row finishes unrun before this runs
// (outbox.outlived).
//
// The row of a move out of the workflow, and of the boot pass, names no session. It releases a
// session of the issue or of that gate tree while the record says the issue stopped: a session a
// claim of one of those issues names now, or one claim_sessions holds for such a claim that left
// it behind (record.Store.SessionOf), which is how it reaches a session left before its own row
// existed, or whose own row Dispatch refused until after the move. A claim of an issue in the
// workflow again belongs to that run (issue-runs). A person's claim is theirs; the daemon's own
// session is a legitimate holder, as workflow.Unresumable treats it; a session the project
// controller's claim or a claim under another gate root recorded, its current one or one it left
// behind, owns the issue instead (record.Store.SessionClaim); and a name no claim of the project
// recorded is one the daemon cannot attribute, which it leaves and logs rather than take a claim
// from a session it knows nothing about. The issue and its gate tree are asked first, so a name a
// claim of one of them names as its session now is theirs whatever claim_sessions holds the name
// for.
//
// The row of a session the daemon left behind names that session and no fence: store's putClaim
// queued it in the claim write that moved the claim off the session. It judges that session's
// claim alone. Any other holder is a person who took the issue since or the new session's own
// claim, of which the row judges nothing, so it writes nothing either. A left session is dead to
// the daemon whatever the issue does, so this row does not ask whether the issue stopped. It
// releases the claim unless the operator holds the issue, or a claim names the session as its
// current one again (a name an operator's release of a claim's names freed and another claim
// took): a claim of the issue or of its gate tree (works-now), the project controller's or one
// under another gate root.
//
// Each outcome that leaves a claim on purpose is written into the issue's record with its reason
// and holder (record.Store.RecordClaimLeft), in the transaction that read what decided it, so a
// role that reads its record learns why the claim stands without the daemon's log. A release
// Dispatch took and a claim that is gone clear an earlier entry. What Dispatch answers the DELETE
// is written in a transaction of its own (takeClaim), and a refusal no retry repairs by
// finishRefused.
//
// An issue the operator holds is left alone whoever holds the claim, on the predicate the leave of
// such an issue reads (record.OperatorHolds): the operator's lifetime shares the issue's key, so
// its own session reads as a session of the issue here, and the workflow may record nothing that
// acts on such an issue's claims (treelifecycle.Lifecycle.HeldByOperator). The test is here rather
// than where the row is queued because the operator can take the issue in the hour a row waits out
// a holder.
func (r *outbox) releaseClaim(ctx context.Context, row record.OutboxRow, release record.ClaimRelease) error {
	if r.dispatch == nil {
		return errors.New("claim release executor has no Dispatch client")
	}
	issue, err := r.dispatch.GetIssue(ctx, row.Issue)
	if err != nil {
		return fmt.Errorf("read Dispatch issue %s before its claim release: %w", row.Issue, err)
	}
	if release.Session != "" && !issue.ClaimUnread && issue.Claim != nil &&
		(issue.Claim.Kind != "session" || issue.Claim.ID != release.Session) {
		// Another holder than the left session: this row judged nothing of it, so the issue's
		// record keeps whatever a row that did judge it wrote.
		return nil
	}
	holder := ""
	if issue.Claim != nil && !issue.ClaimUnread {
		holder = issue.Claim.Holder()
	}
	// The record, the issue's lifecycle, the Dispatch chain and the claims the holder is judged
	// against are read in one transaction, so they are one state of the record, and the entry that
	// says why the claim stays is written from that state in the same transaction.
	var verdict claimVerdict
	var reason record.ClaimLeftReason
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if verdict, reason, err = r.judgeClaim(ctx, tx, row.Issue, release, issue); err != nil {
			return err
		}
		switch verdict {
		case claimTakeable, claimUnrecorded:
			return nil
		}
		return r.recordClaimLeft(ctx, tx, row.Issue, holder, reason)
	}); err != nil {
		return fmt.Errorf("judge the Dispatch claim of %s against its record: %w", row.Issue, err)
	}
	switch {
	case reason == record.ClaimLeftUnread:
		r.log.Warn("outbox claim release left a claim Dispatch names no actor for", "issue", row.Issue, "row", row.ID)
	case reason == record.ClaimLeftUnattributed:
		r.log.Warn("outbox claim release left a claim the daemon cannot attribute to the issue's tree",
			"issue", row.Issue, "holder", holder)
	case verdict == claimTakeable:
		return r.takeClaim(ctx, row, holder)
	}
	return nil
}

// judgeClaim is releaseClaim's decision table over what Dispatch shows of key's claim (shown), in
// the order the rows of the table hold: the verdict, and the reason the issue's record gives for a
// claim the row leaves ("" for none).
func (r *outbox) judgeClaim(ctx context.Context, tx pgx.Tx, key string, release record.ClaimRelease, shown dispatch.Issue) (claimVerdict, record.ClaimLeftReason, error) {
	switch {
	case shown.ClaimUnread:
		// Dispatch showed a claim naming no actor, so the read says neither who holds it nor
		// that nobody does: there is nothing to judge a holder by.
		return claimStays, record.ClaimLeftUnread, nil
	case shown.Claim == nil:
		return claimStays, "", nil
	}
	holder := shown.Claim
	// The id a Dispatch claim carries is the session id the claims table holds, which is why the
	// store reads below take Claim.ID and the daemon's own session, the one of its own Dispatch
	// project, is tested against it (workflow.Unresumable). A left session's row has already
	// found its own session holding the claim, which is neither. RecordClaimLeft writes nothing for
	// an issue the daemon does not record, so these two need no read of the record.
	if release.Session == "" {
		switch {
		case holder.Kind != "session":
			return claimStays, record.ClaimLeftPerson, nil
		case holder.ID == dispatch.DaemonSession(r.dispatchProject):
			return claimStays, record.ClaimLeftDaemon, nil
		}
	}
	recorded, err := r.records.Issue(ctx, tx, key)
	if err != nil || recorded == nil {
		return claimUnrecorded, "", err
	}
	// The issue is live again, or it runs again at a later generation while Dispatch still shows it
	// out of the workflow: the claim belongs to the run, not to the move this row answered.
	if release.Session == "" && !record.Stopped(*recorded) {
		return claimStays, record.ClaimLeftIssueRuns, nil
	}
	held, err := record.OperatorHolds(ctx, r.records, tx, *recorded)
	if err != nil {
		return "", "", err
	}
	if held {
		return claimStays, record.ClaimLeftOperatorTree, nil
	}
	// The holder is judged against the issue's own claims and the tree of the gate holder it
	// stands under on the Dispatch chain (record.Chains.StateTree, GateTree), never the tree a
	// claim or the record stores: an issue out of Legion's work is in no gate root's tree of its
	// own, and one in it stands in the tree of its gate root.
	chains, err := record.ReadChains(ctx, tx, r.records, recorded.Project, []string{recorded.Key})
	if err != nil {
		return "", "", err
	}
	issues := append([]string{recorded.Key}, chains.GateTree(chains.StateTree(recorded.Key))...)
	if release.Session != "" {
		return r.judgeLeftSession(ctx, tx, issues, holder.ID)
	}
	of, err := r.records.SessionOf(ctx, tx, issues, holder.ID)
	if err != nil {
		return "", "", err
	}
	if of {
		return claimTakeable, "", nil
	}
	owner, found, err := r.records.SessionClaim(ctx, tx, r.project, holder.ID)
	switch {
	case err != nil:
		return "", "", err
	case !found:
		return claimStays, record.ClaimLeftUnattributed, nil
	}
	return claimStays, ownerLeft(owner), nil
}

// judgeLeftSession is the left session's row once its session holds the claim of an issue that
// the operator does not hold, judged against issues, the issue and the tree of the gate holder it
// stands under on the Dispatch chain: the session stays only where a claim names it as its current
// session again, and is released otherwise. Whether the claim claim_sessions holds the name for
// left it behind is read off that claim's current session, as the claim that recorded the name
// first is the only one that can name it now. The row finds that claim through the hold alone
// (record.Store.SessionClaim), where the move's row asks the claims of issues first
// (record.Store.SessionOf). So where the name is held for no claim (an empty claim), the row
// releases in every case: also where a claim names the session as its current one now, and where
// the move's row leaves the Dispatch claim as unattributed. The only claims that name such a name
// as their current session are the conflicts the boot logs (suspendSessionConflicts), and the hold
// refuses every launch of each of them. So for each claim that runs, the hold names that claim, and
// the two rows agree on whose session it is.
func (r *outbox) judgeLeftSession(ctx context.Context, tx pgx.Tx, issues []string, session string) (claimVerdict, record.ClaimLeftReason, error) {
	owner, found, err := r.records.SessionClaim(ctx, tx, r.project, session)
	switch {
	case err != nil:
		return "", "", err
	case !found || owner.Session != session:
		return claimTakeable, "", nil
	case slices.Contains(issues, owner.Issue):
		// The project controller's claim stands on no issue (0032_controller_claim's shape check),
		// and each of issues is an issue's key, so that claim is never among them.
		return claimStays, record.ClaimLeftWorksNow, nil
	}
	return claimStays, ownerLeft(owner), nil
}

// ownerLeft is the reason a claim stays with a session a claim outside the judged issues recorded:
// the project controller's claim, which stands on no issue, or a claim under another gate root.
// SessionClaim reads no stored tree, and 0032_controller_claim's shape check holds a controller
// claim's tree empty with its issue, so the tree claim.IsController is handed is that empty one.
func ownerLeft(owner record.SessionClaim) record.ClaimLeftReason {
	if claim.IsController(owner.Role, owner.Issue, "") {
		return record.ClaimLeftController
	}
	return record.ClaimLeftOtherTree
}

// takeClaim sends the DELETE of a claim releaseClaim judged takeable, held by holder, and writes
// what Dispatch's answer leaves in the issue's record, in a transaction of its own: a release
// Dispatch took clears the entry, and a holder Dispatch kept for the whole hour claimReleaseWait
// gives is written as such (held-an-hour) before its line is logged, so a failed write logs nothing
// and the row asks again. A wait writes nothing, since the row has not ended. Any other failure
// carries the holder (claimReleaseFailure) for finishRefused to write where no retry repairs it.
func (r *outbox) takeClaim(ctx context.Context, row record.OutboxRow, holder string) error {
	err := r.dispatch.ReleaseClaim(ctx, row.Issue)
	var reason record.ClaimLeftReason
	switch {
	case err == nil:
	case dispatch.Contended(err):
		return claimContendedWait(row.Issue)
	case !dispatch.Claimed(err):
		return &claimReleaseFailure{holder: holder, err: fmt.Errorf("release the Dispatch claim of %s: %w", row.Issue, err)}
	case r.now().UTC().Sub(row.CreatedAt) < claimReleaseWait:
		return claimHeldWait(holder)
	default:
		reason = record.ClaimLeftHeldAnHour
	}
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		return r.recordClaimLeft(ctx, tx, row.Issue, holder, reason)
	}); err != nil {
		return fmt.Errorf("record what the Dispatch claim release of %s left: %w", row.Issue, err)
	}
	if reason == record.ClaimLeftHeldAnHour {
		r.log.Warn("outbox claim release gave up: the holder still holds the claim an hour after the row was written",
			"issue", row.Issue, "holder", holder)
	}
	return nil
}

// recordClaimLeft writes issue's entry of the claim a release row left with holder, for reason, at
// the executor's clock; reason "" clears the entry.
func (r *outbox) recordClaimLeft(ctx context.Context, tx pgx.Tx, issue, holder string, reason record.ClaimLeftReason) error {
	var left *record.ClaimLeft
	if reason != "" {
		left = &record.ClaimLeft{Holder: holder, Reason: reason, At: r.now().UTC()}
	}
	return r.records.RecordClaimLeft(ctx, tx, issue, left)
}

// claimReleaseFailure is a claim release's DELETE that failed, with the holder the row judged when
// it sent it. It unwraps to the failure, so permanentRefusal and settle read Dispatch's own refusal
// through it exactly as they would without it, and finishRefused reads the holder off it when it
// writes the refusal into the issue's record (ClaimLeftRefused). A read Dispatch refused is
// returned as it is and carries no holder, since Dispatch named none.
type claimReleaseFailure struct {
	holder string
	err    error
}

func (f *claimReleaseFailure) Error() string { return f.err.Error() }

func (f *claimReleaseFailure) Unwrap() error { return f.err }

// claimHeldWait is the wait a claim release answers while Dispatch still holds the claim for its
// holder (dispatch.Claimed): the holder has not stopped yet, and the row tries again on its
// backoff until claimReleaseWait is up. It deliberately wraps nothing of Dispatch's refusal, only
// its holder's name: settle computes permanentRefusal before it tests wait.ErrWaiting, so a wait
// errors.As could reach the *dispatch.Error through would be finished rather than retried.
//
// It is routine (routineWaiting), so the hour's attempts stay at debug: the backoff caps at 64
// seconds, so an hour costs about 57 of them, and every attempt from the seventh would otherwise
// be logged as a warning (waitWarnAttempts). A holder that has not stopped yet is not something an
// operator should be woken for; the hour running out is, and that line is its own.
func claimHeldWait(holder string) error {
	return routineWaiting{err: wait.Errorf("the Dispatch claim is still held by %s", holder)}
}

// claimContendedWait is the wait a claim release answers a contended refusal with
// (dispatch.Contended): the issue's holder changed twice while the request ran, so Dispatch
// judged nobody and applied nothing (issue_claim.go's answerContendedClaim). That is a routine
// retry rather than a failure, and it says nothing about the holder, so no contended answer ends
// the row by itself. It does not hold the row open either: the hour claimReleaseWait caps a
// refused release at is wall-clock time from row.CreatedAt, so the time these retries take
// counts toward it like any other, and a refusal naming the holder (dispatch.Claimed) after them
// can end the row at once. It wraps nothing of Dispatch's refusal, for the reason claimHeldWait
// gives, and it is routine (routineWaiting), as claimHeldWait's wait is and for the same reason:
// Dispatch applied nothing, so the row has only to ask again.
func claimContendedWait(issue string) error {
	return routineWaiting{err: wait.Errorf("the Dispatch claim of %s was contended while it was released", issue)}
}

// releaseParkedClaims queues one Dispatch claim release (record.ClaimRelease) for each issue of
// the boot's listing that still carries a claim while the daemon's own record says it stopped.
// Dispatch releases a claim at a close alone (its issue PATCH), so a park leaves the claim of the
// session the daemon stopped standing on the board.
//
// It is the boot's pass and not a periodic sweep because every park the daemon sees queues its
// own row (workflow.Engine.leave), and every claim write that leaves a session behind queues that
// session's (store.Store.ReleaseLeftSessionClaims), so the only claims left for this to find are
// the ones parked before that row existed, those of a session left behind before its own row
// existed, which the executor reaches through claim_sessions, and any row lost with a restore of
// the database. The recovery caller passes boot false (recoverRepositories) and starts no pass.
//
// A summary carrying no claim is passed over before anything is read, which is the whole of the
// pass's cost: of the 1,585 issues of the live board, 1,426 carry none. What is left is read in
// one batch of the records, because the pass runs inside the boot's serialized transaction,
// holding the fact lock every boot fact waits on (intake.SerializeFacts), where a read per issue
// would be hundreds of round trips. Whether the issue stopped is the record's to say and never
// the listing's, because an unslotted root keeps an active Dispatch status
// (workflow.Engine.LeaveUnslotted leaves the root at the status it stood at). An issue the
// daemon does not record is left alone with nothing logged: it could attribute no claim of it.
//
// The pass attributes no claim itself. The row's executor re-reads Dispatch, the record, the
// issue's lifecycle and the Dispatch chain when it runs (outbox.releaseClaim), so a person's claim,
// a session the project controller's claim or a claim under another gate root recorded, a holder
// whose name no claim of the project recorded in claim_sessions, an issue the operator holds and
// an issue running again all end the row there, each written into the issue's record with its
// reason, on the reading that is current when the DELETE would be sent — which is why the
// operator-held test is the executor's alone and the pass makes none. The rows are drained by the
// outbox's own sweep rather than by a commit wake: attach stores the outbox the records' wake
// reaches (wakeOutbox) after the reconcile probe has run (daemon.go's boot stages).
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
	recorded := recordsByKey(issues)
	for _, key := range keys {
		issue := recorded[key]
		if issue == nil || !record.Stopped(*issue) {
			continue
		}
		row, err := record.NewFencedRow(ctx, tx, w.records, key, record.ClaimRelease{}, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := w.records.Enqueue(ctx, tx, row); err != nil {
			return fmt.Errorf("queue the Dispatch claim release of %s: %w", key, err)
		}
		store.OnCommit(ctx, func() {
			w.log.Info("workflow: boot queues a Dispatch claim release for an issue out of the workflow",
				"issue", issue.Key, "tree", issue.Tree, "status", issue.Status)
		})
	}
	return nil
}
