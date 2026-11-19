package workflow

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergeForwardPhases is the phases a merge forward is asked in: implementing, where the work
// already is, and every phase after it up to the merge, which the issue keeps while the
// implementer merges (dispatch://INFRA-4374 R1). An issue that stands in none of them - before
// its implementation, or out of the workflow - has no pull request work to merge forward.
var mergeForwardPhases = []phase.Phase{phase.Implementing, phase.Testing, phase.Reviewing, phase.Accepting, phase.Retro, phase.Merging}

// mergeForward is the daemon's act on a confirmed conflict that R0 found a reason to act on, why
// (dispatch://INFRA-4374 R0, R1; classify.ConflictNeedsMerge): the issue keeps the phase it stands
// in (record.StandsAt), no phase entry starts and no supervise row is written, so an issue past
// its review takes no admission slot for the merge, and the implementer is asked, on its own role
// topic, to merge the base forward onto the head and push (record.MergeForwardAsk). Outside
// implementing it also tells the worker of that phase, when that is the tester, the reviewer or
// the merger, to stop writing the workspace the roles share while the merge runs
// (record.MergeForwardHold), and the architect that owns the issue one checks-red notice whose
// phase is the phase the issue stays in and whose reason names the head, the base, why and that
// the issue stays in its phase; the outbox routes that notice to the owning architect, as every
// notice of the issue is routed. In implementing only the ask goes: the implementer's own
// assignment and its completion stand, and the architect is told nothing, since the work the
// conflict needs is already where the issue stands.
//
// It asks once for each head: the record keeps the ask (record.PullRequest.MergeForward), and a
// later confirmed read, or a move into another phase, at the head it names sends nothing. The one
// exception is an ask made in implementing, which covers implementing only: an implementer that
// completes at that head without the merge leaves the issue's next phase to ask again, with its
// hold and its notice.
//
// The roles are addressed as every role message of the daemon is (addressRoles), each once, and
// a pull request that is not open, an issue in no phase of mergeForwardPhases and a tree that
// lingers (record.TreeLingers, where every role is suspended) are asked nothing.
func (e *Engine) mergeForward(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, why string) error {
	at := record.StandsAt(issue)
	if pr.State != record.PullRequestOpen || !slices.Contains(mergeForwardPhases, at) {
		return nil
	}
	if asked := pr.MergeForward; asked != nil && asked.Head == pr.HeadSHA && (asked.Phase != phase.Implementing || at == phase.Implementing) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	recipients := []roleRecipient{{issue: issue.Key, role: claim.RoleImplementer, row: issue.Key}}
	if worker := RoleFor(at); at != phase.Implementing && holdsForMergeForward(worker) {
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: worker, row: issue.Key})
	}
	addressed, err := e.addressRoles(issue.Project, recipients, nil)
	if err != nil {
		return err
	}
	for _, to := range addressed {
		role, _ := to.token.Role()
		step := record.MergeForwardHold
		if role == claim.RoleImplementer {
			step = record.MergeForwardAsk
		}
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: role, Step: step, Head: pr.HeadSHA, Base: pr.Base,
			Phase: at, PullRequest: pr.Number, Why: why}); err != nil {
			return err
		}
	}
	if at != phase.Implementing {
		reason := fmt.Sprintf("the head %s conflicts with %s (%s): the implementer is asked to merge %s forward, and the issue stays in %s",
			pr.HeadSHA, pr.Base, why, pr.Base, at)
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "checks-red", Role: claim.RoleArchitect, Phase: at, Reason: reason}); err != nil {
			return err
		}
	}
	pr.MergeForward = &record.MergeForward{Head: pr.HeadSHA, Base: pr.Base, Phase: at, Why: why, AskedAt: e.now()}
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: merge forward asked", "issue", issue.Key, "tree", issue.Tree, "phase", at,
		"pullRequest", pr.Number, "head", pr.HeadSHA, "base", pr.Base, "why", why)
	return nil
}

// holdsForMergeForward says whether the worker of a phase is told to hold while the implementer
// merges forward (record.MergeForwardHold): the tester, the reviewer and the merger, which work
// the shared workspace beside the implementer. The implementer is the one the ask tells, and the
// architect is told by the checks-red notice.
func holdsForMergeForward(role claim.Role) bool {
	return role == claim.RoleTester || role == claim.RoleReviewer || role == claim.RoleMerger
}

// mergeForwardOnEntry is what a move does with the merge forward of the issue's pull request once
// the move has put issue in the phase it now stands in and written that phase's start
// (transition), from being the phase the move left. The pull request is read again in the
// transaction, since the copy a transition is handed can be none, or one read before the
// transaction's other writes.
//
//   - A move into implementing drops the record's merge forward (clearMergeForward): the
//     implementer works the head there, and a head it pushes is read for a conflict of its own.
//   - At a head the record holds in a confirmed conflict (classify.ConflictRound), a move into
//     accepting, retro or merging asks for the merge forward for classify.ConflictMergeNext: the
//     merge comes next, and GitHub merges no head that conflicts with its base. This is where an
//     approval of a conflicting head, which ends its round (reviewRound), is acted on
//     (dispatch://INFRA-4374 R0).
//   - At such a head, a move out of implementing into a later phase asks again what was asked in
//     implementing, for the reason it was asked for (record.MergeForward.Why): an ask made there
//     covers implementing alone, so an implementer that completes at that head without the merge
//     leaves the next phase to ask, with its hold and its notice.
//
// mergeForward asks once for each head, so a move at a head already asked of in a phase past
// implementing - an ask in reviewing, then the round's end into accepting - asks nothing more.
func (e *Engine) mergeForwardOnEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, from phase.Phase) error {
	if issue.Phase == phase.Implementing {
		return e.clearMergeForward(ctx, tx, issue.Key)
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil || !classify.ConflictRound(*pr) {
		return err
	}
	asked := pr.MergeForward
	switch {
	case record.PastReview(issue):
		return e.mergeForward(ctx, tx, issue, pr, classify.ConflictMergeNext)
	case from == phase.Implementing && !phase.MovesBack(from, issue.Phase) &&
		asked != nil && asked.Head == pr.HeadSHA && asked.Phase == phase.Implementing:
		return e.mergeForward(ctx, tx, issue, pr, asked.Why)
	}
	return nil
}

// clearMergeForward drops the merge forward the record holds for issue's pull request
// (record.PullRequest.MergeForward), on a move into implementing (mergeForwardOnEntry) and when
// the issue leaves the workflow (leave): the ask, its landing and the review it asked for are of
// a stretch of work that is over, and a later conflict is asked about afresh. A record that holds
// none is not written.
func (e *Engine) clearMergeForward(ctx context.Context, tx pgx.Tx, issue string) error {
	pr, err := e.store.PullRequest(ctx, tx, issue)
	if err != nil || pr == nil || pr.MergeForward == nil {
		return err
	}
	pr.MergeForward = nil
	return e.putPullRequest(ctx, tx, *pr)
}

// mergeForwardLanded records that a merge forward landed (dispatch://INFRA-4374 R1): pr's head,
// just moved by a synchronize (pullRequestSynchronized), is the first head off the one the merge
// forward was asked of. It runs in whatever phase issue stands in, and once for each ask: the
// record keeps the new head as the one the merge left (record.MergeForward.Landed), which is a
// new value rather than a change of the one the record holds, since a copy of the pull request
// shares it.
//
// The worker of the phase the issue stands in, when that is the tester, the reviewer or the
// merger (holdsForMergeForward), is told the old head and the new one and to read what the merge
// changed before it goes on (record.MergeForwardLanded). An issue past its review
// (record.PastReview) stands on a review of the old head, so the reviewer is asked to read the
// merge and approve the new head or request changes (record.MergeForwardReview), and the record
// keeps that the review was asked for (ReviewAsked), which the merger's task reads (task). The
// daemon records no decision outside reviewing, so that review is the roles' to hold to the head
// they merge. The architect and the implementer are told nothing: the architect's checks-red
// notice already said the merge was asked for, and the implementer pushed it. A tree that lingers
// (record.TreeLingers, where every role is suspended) records and tells nothing.
func (e *Engine) mergeForwardLanded(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	asked := pr.MergeForward
	if asked == nil || asked.Landed != "" || asked.Head == pr.HeadSHA {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	at := record.StandsAt(issue)
	review := record.PastReview(issue)
	var recipients []roleRecipient
	if worker := RoleFor(at); holdsForMergeForward(worker) {
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: worker, row: issue.Key})
	}
	if review {
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: claim.RoleReviewer, row: issue.Key})
	}
	addressed, err := e.addressRoles(issue.Project, recipients, nil)
	if err != nil {
		return err
	}
	for _, to := range addressed {
		role, _ := to.token.Role()
		step := record.MergeForwardLanded
		if review && role == claim.RoleReviewer {
			step = record.MergeForwardReview
		}
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: role, Step: step, Head: asked.Head, Landed: pr.HeadSHA,
			Base: asked.Base, Phase: at, PullRequest: pr.Number, Why: asked.Why}); err != nil {
			return err
		}
	}
	landed := *asked
	landed.Landed, landed.ReviewAsked = pr.HeadSHA, review
	pr.MergeForward = &landed
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the merge forward landed", "issue", issue.Key, "tree", issue.Tree, "phase", at,
		"pullRequest", pr.Number, "head", asked.Head, "landed", pr.HeadSHA)
	return nil
}

// mergeForwardReviewOwed says whether the review a landed merge forward asked of the reviewer
// stands for pr's head (mergeForwardLanded): it was asked, and the head is still the one the merge
// left. The merger's task reads it (task).
func mergeForwardReviewOwed(pr record.PullRequest) bool {
	asked := pr.MergeForward
	return asked != nil && asked.ReviewAsked && asked.Landed == pr.HeadSHA
}

// mergedForwardRefusal is the completion fence of a merge forward (dispatch://INFRA-4374 R1): a
// completion of testing, reviewing or retro whose commit is the head a merge forward was asked of,
// once that merge landed (record.MergeForward.Landed) and the pull request stands at another head,
// reports work on code the issue no longer stands on, and is refused with
// HANDOFF_HEAD_MERGED_FORWARD before anything is recorded (handoff). The refusal names both heads
// and tells the role to read what the merge changed and complete again at the head the pull
// request stands at now; a phase that ends with a handoff record is told to write it again, since
// a completion naming the record it already named is the one the route answers as received
// (api/handoff.go). The fence is the merge forward's alone, not every push's: a completion at the
// current head, at a head no merge forward replaced, or of any other phase passes it. It answers
// false where it does not refuse.
func mergedForwardRefusal(issue record.Issue, role claim.Role, reported phase.Phase, commit string, pr *record.PullRequest) (intake.Result, bool) {
	switch reported {
	case phase.Testing, phase.Reviewing, phase.Retro:
	default:
		return intake.Result{}, false
	}
	if pr == nil || pr.MergeForward == nil {
		return intake.Result{}, false
	}
	asked := pr.MergeForward
	if asked.Landed == "" || commit != asked.Head || commit == pr.HeadSHA {
		return intake.Result{}, false
	}
	again := "complete again at that head"
	if word, _, ok := phase.Handoff(reported); ok {
		again = fmt.Sprintf("write this phase's handoff again with `dispatch handoff-write --issue %s --phase %s --body-file <file>`, and complete again at that head naming it",
			issue.Key, word)
	}
	return refused("HANDOFF_HEAD_MERGED_FORWARD", fmt.Sprintf("the %s's completion of phase %s of %s is at head %s, and the merge of %s forward replaced that head: pull request #%d stands at head %s now. Read what the merge changed with `git show --remerge-diff %s`, do the phase's work at head %s, then %s; this completion changed nothing",
		role, reported, issue.Key, asked.Head, asked.Base, pr.Number, pr.HeadSHA, asked.Landed, pr.HeadSHA, again)), true
}

// conflictConfirmDone releases a first conflicting read that no second read confirmed or
// released (intake.ConflictConfirmDone): the second read's row (record.ConflictConfirm) read
// GitHub's UNKNOWN up to its last attempt, or no read of it succeeded (Unread). The first read is
// cleared while the record still holds it for the fact's head, so the next read that finds the head
// conflicting starts the two reads again (mergeability), and the release is logged with both reads
// as a MERGEABLE release is. A record that holds no first read of that head - a later read already
// confirmed or released it, or the head moved and a read of the new head made one of its own - is
// left as it is. It acts on nothing and moves nothing.
func (e *Engine) conflictConfirmDone(ctx context.Context, tx pgx.Tx, fact intake.ConflictConfirmDone) (intake.Result, error) {
	pr, err := e.store.PullRequest(ctx, tx, fact.Issue)
	if err != nil || pr == nil || pr.Number != fact.Number || pr.ConflictRead == nil || pr.ConflictRead.Head != fact.Head {
		return intake.Result{}, err
	}
	first := *pr.ConflictRead
	pr.ConflictRead = nil
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: a second read did not confirm the conflict",
		"issue", pr.Issue, "pullRequest", pr.Number, "head", first.Head, "base", first.Base,
		"firstReadAt", first.At, "readAt", e.now(), "unread", fact.Unread)
	return intake.Result{}, nil
}
