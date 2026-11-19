package workflow

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
