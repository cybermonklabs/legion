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
// The ask replaces the one the record holds, and keeps what that one kept of the merge forwards
// that landed before it in the same stretch of work (record.MergeForward.Earlier), with that ask
// itself added once it landed: when the head a merge left conflicts in its turn and is merged
// forward again, a completion at the head the first merge replaced is still refused
// (mergedForwardRefusal). That holds for an ask of a new head and for the same head asked again
// on a phase entry alike.
//
// The roles are addressed as every role message of the daemon is (addressRoles), each once, and
// a pull request that is not open, an issue in no phase of record.MergeForwardPhases and a tree
// that lingers (record.TreeLingers, where every role is suspended) are asked nothing.
func (e *Engine) mergeForward(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, why record.ConflictWhy) error {
	at := record.StandsAt(issue)
	if pr.State != record.PullRequestOpen || !record.StandsIn(issue, record.MergeForwardPhases...) {
		return nil
	}
	if asked := pr.MergeForward; asked != nil && asked.Head == pr.HeadSHA && (asked.Phase != phase.Implementing || at == phase.Implementing) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	recipients := []roleRecipient{{issue: issue.Key, role: claim.RoleImplementer, row: issue.Key}}
	if worker := RoleFor(at); at != phase.Implementing && record.MergeForwardReaches(record.MergeForwardHold, worker) {
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
	next := &record.MergeForward{Head: pr.HeadSHA, Base: pr.Base, Phase: at, Why: why, AskedAt: e.now()}
	if before := pr.MergeForward; before != nil {
		next.Earlier = before.Earlier
		if before.Landed != "" {
			// Clipped, so the append writes a new array: a copy of the pull request shares the old one.
			next.Earlier = append(slices.Clip(before.Earlier), record.LandedMergeForward{Head: before.Head, Landed: before.Landed})
		}
	}
	pr.MergeForward = next
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: merge forward asked", "issue", issue.Key, "tree", issue.Tree, "phase", at,
		"pullRequest", pr.Number, "head", pr.HeadSHA, "base", pr.Base, "why", why)
	return nil
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
// The worker of the phase the issue stands in, when the landed reaches it - the tester, the
// reviewer or the merger (record.MergeForwardReaches) - is told the old head and the new one and
// to read what the merge changed before it goes on (record.MergeForwardLanded). An issue past its
// review (record.PastReview) stands on a review of the old head, so the reviewer is asked to read
// the merge and approve the new head or request changes (record.MergeForwardReview), and the
// record keeps that the review was asked for (ReviewAsked), which the merger's task reads (task).
// The daemon records no decision outside reviewing, so that review is the roles' to hold to the
// head they merge. The architect and the implementer are told nothing: the architect's checks-red
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
	if worker := RoleFor(at); record.MergeForwardReaches(record.MergeForwardLanded, worker) {
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

// MergeForwardAt reads in tx what a merge-forward message (record.MergeForwardMessage) of issue
// key is judged by: the issue, whether its tree lingers (record.TreeLingers) and the pull request
// the record holds for it. It returns the issue and that pull request, with why a message of step
// about the merge forward asked of head is no longer to be delivered (leftMergeForwardAt), "" while
// it is. An issue the record does not hold is a nil issue, no pull request and no reason. The rule
// states, for a row already queued, the states the workflow writes these rows in (mergeForward,
// mergeForwardLanded) and drops the ask they are about in (clearMergeForward), and the daemon's
// executor reads it in one transaction at each attempt of a row, just before it delivers (the
// daemon's mergeForwardMessage): so the workflow and the executor judge a row by this one rule,
// kept beside the code that writes the rows, and no row outlives the state it was written for. It
// is exported for that executor.
func MergeForwardAt(ctx context.Context, store record.Store, tx pgx.Tx, key string, step record.MergeForwardStep, head string) (*record.Issue, *record.PullRequest, string, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, nil, "", err
	}
	lingers, err := record.TreeLingers(ctx, store, tx, issue.Tree)
	if err != nil {
		return nil, nil, "", err
	}
	pr, err := store.PullRequest(ctx, tx, key)
	if err != nil {
		return nil, nil, "", err
	}
	return issue, pr, leftMergeForwardAt(*issue, lingers, pr, step, head), nil
}

// leftMergeForwardAt says why a merge-forward message of step about the merge forward asked of
// head is no longer to be delivered, or "" while it is: the issue is in the workflow, its tree does
// not linger (lingers, record.TreeLingers), and the pull request the record holds for it (pr, nil
// for none) is open and holds a merge forward of head (record.PullRequest.MergeForward) that, for
// an ask or a hold, has not landed. In each other state the message would tell its role of work
// nobody wants any more. The roles of a tree that lingers are suspended. The workflow drops the ask
// on a move into implementing and when the issue leaves the workflow (clearMergeForward), and a
// merge forward of another head is a newer ask, which has its own messages. A pull request that is
// not open ends every step: mergeForward asks nothing for one, and once it closed or merged nothing
// is left to merge forward, to hold for or to review. The ask and the hold end at the landing
// (record.MergeForward.Landed): role-message rows run in no order of the issue's, so a hold still
// queued would reach the worker after its landed and tell it to wait for a head that already came,
// and an ask would ask for a merge already pushed. The landed and the review are the landing's own
// messages, and they still go after it.
func leftMergeForwardAt(issue record.Issue, lingers bool, pr *record.PullRequest, step record.MergeForwardStep, head string) string {
	switch {
	case !record.InWorkflow(issue):
		return fmt.Sprintf("the issue has left the workflow, at status %s in %s", issue.Status, issue.Phase)
	case lingers:
		return "the issue's tree lingers or has closed"
	case pr == nil:
		return "the record holds no pull request for the issue"
	case pr.State != record.PullRequestOpen:
		return fmt.Sprintf("the pull request is %s", pr.State)
	case pr.MergeForward == nil:
		return "the record holds no merge forward"
	case pr.MergeForward.Head != head:
		return fmt.Sprintf("the merge forward the record holds is of head %s", pr.MergeForward.Head)
	case (step == record.MergeForwardAsk || step == record.MergeForwardHold) && pr.MergeForward.Landed != "":
		return fmt.Sprintf("the merge forward landed at head %s", pr.MergeForward.Landed)
	}
	return ""
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
// once that merge landed and the pull request stands at another head, reports work on code the
// issue no longer stands on, and is refused with HANDOFF_HEAD_MERGED_FORWARD before anything is
// recorded (handoff). The merge forward is the record's landed ask (record.MergeForward.Landed)
// or any that landed before it in the same stretch of work (record.MergeForward.Earlier,
// mergedForwardOf), so a head the first merge replaced stays fenced after the head that merge
// left conflicts in its turn and is merged forward again. The refusal names the head that merge
// replaced, the head it left and the head the pull request stands at now, and tells the role to
// read what the merge changed and complete again at that head; a phase that ends with a handoff
// record is told to write it again, since a completion naming the record it already named is the
// one the route answers as received (api/handoff.go). The fence is the merge forward's alone, not
// every push's: a completion at the current head, at a head no merge forward replaced, or of any
// other phase passes it. It answers false where it does not refuse.
func mergedForwardRefusal(issue record.Issue, role claim.Role, reported phase.Phase, commit string, pr *record.PullRequest) (intake.Result, bool) {
	switch reported {
	case phase.Testing, phase.Reviewing, phase.Retro:
	default:
		return intake.Result{}, false
	}
	if pr == nil || pr.MergeForward == nil || commit == pr.HeadSHA {
		return intake.Result{}, false
	}
	replaced, ok := mergedForwardOf(pr.MergeForward, commit)
	if !ok {
		return intake.Result{}, false
	}
	again := "complete again at that head"
	if word, _, ok := phase.Handoff(reported); ok {
		again = fmt.Sprintf("write this phase's handoff again with `dispatch handoff-write --issue %s --phase %s --body-file <file>`, and complete again at that head naming it",
			issue.Key, word)
	}
	return refused("HANDOFF_HEAD_MERGED_FORWARD", fmt.Sprintf("the %s's completion of phase %s of %s is at head %s, and the merge of %s forward replaced that head: pull request #%d stands at head %s now. Read what the merge changed with `git show --remerge-diff %s`, do the phase's work at head %s, then %s; this completion changed nothing",
		role, reported, issue.Key, replaced.Head, pr.MergeForward.Base, pr.Number, pr.HeadSHA, replaced.Landed, pr.HeadSHA, again)), true
}

// mergedForwardOf is the merge forward of the record's stretch of work that replaced commit: the
// ask the record holds, when it landed and commit is its head, or else the newest of the merge
// forwards that landed before it (record.MergeForward.Earlier) whose head commit is. It answers
// false where no merge forward the record keeps replaced commit.
func mergedForwardOf(asked *record.MergeForward, commit string) (record.LandedMergeForward, bool) {
	if asked.Landed != "" && asked.Head == commit {
		return record.LandedMergeForward{Head: asked.Head, Landed: asked.Landed}, true
	}
	for i := len(asked.Earlier) - 1; i >= 0; i-- {
		if asked.Earlier[i].Head == commit {
			return asked.Earlier[i], true
		}
	}
	return record.LandedMergeForward{}, false
}

// conflictConfirmDone releases a first conflicting read that no second read confirmed or
// released (intake.ConflictConfirmDone): the second read's row (record.ConflictConfirm) read
// GitHub's UNKNOWN up to its last attempt, or no read of it succeeded (Unread). The first read is
// cleared while the record still holds the row's own, of the fact's head and made at its
// FirstReadAt, so the next read that finds the head conflicting starts the two reads again
// (mergeability), and the release is logged with both reads as a MERGEABLE release is. A record
// that holds no first read of that head and time - a later read already confirmed or released
// it, the head moved and a read of the new head made one of its own, or a MERGEABLE read released
// it and a later CONFLICTING read of the same head made a newer one, whose own row reads it - is
// left as it is. It acts on nothing and moves nothing.
func (e *Engine) conflictConfirmDone(ctx context.Context, tx pgx.Tx, fact intake.ConflictConfirmDone) (intake.Result, error) {
	pr, err := e.store.PullRequest(ctx, tx, fact.Issue)
	if err != nil || pr == nil || pr.Number != fact.Number || pr.ConflictRead == nil || pr.ConflictRead.Head != fact.Head ||
		!pr.ConflictRead.At.Equal(fact.FirstReadAt) {
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
