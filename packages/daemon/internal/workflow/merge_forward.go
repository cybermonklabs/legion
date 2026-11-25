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
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergeForward is the daemon's act on a confirmed conflict that R0 found a reason to act on, why
// (dispatch://INFRA-4374 R0, R1; classify.ConflictNeedsMerge): the issue keeps the phase it stands
// in (record.StandsAt), no phase entry starts and no supervise row is written, so an issue past
// its review takes no admission slot for the merge, and the role the phase asks
// (record.MergeForwardAsker) is asked, on its own role topic, to merge the base forward onto the
// head and push (record.MergeForwardAsk): the implementer before merging, and in merging the
// merger, whose merge it is (cybermonklabs/legion#4979). Outside implementing it also tells the
// step, to stop writing the workspace the roles share while the merge runs
// (record.MergeForwardHold), and the architect that owns the issue one checks-red notice whose
// phase is the phase the issue stays in and whose reason names the head, the base, why, the role
// asked and that the issue stays in its phase; the outbox routes that notice to the owning
// architect, as every notice of the issue is routed. In implementing only the ask goes: the
// implementer's own assignment and its completion stand, and the architect is told nothing, since
// the work the conflict needs is already where the issue stands.
//
// It asks once for each head: the record keeps the ask (record.PullRequest.MergeForward), and a
// later confirmed read, or a move into another phase, at the head it names sends nothing, so the
// role first asked of a head keeps that merge until it lands and two roles are never sent to merge
// one base forward. The one exception is an ask made in implementing, which covers implementing
// only: an implementer that completes at that head without the merge leaves the issue's next phase
// to ask again, with its hold and its notice, where a step of the task still runs there before the
// merge, and the move into merging to ask the merger where none does (mergeForwardOnEntry). An ask
// of the implementer made after implementing that has not landed when the issue reaches merging
// stays the implementer's, and the merger is held instead (mergeForwardOnEntry).
//
// The ask replaces the one the record holds, and keeps what that one kept of the merge forwards
// that landed before it in the same stretch of work (record.MergeForward.Earlier), with that ask
// itself added once it landed: when the head a merge left conflicts in its turn and is merged
// forward again, a completion at the head the first merge replaced is still refused
// (mergedForwardRefusal). That holds for an ask of a new head and for the same head asked again
// on a phase entry alike.
//
// The roles are addressed as every role message of the daemon is (addressRoles), each once, and
// a pull request that is not open, an issue where no merge forward stands at the place its task
// stands at on w's line (classify.MergeForwardStands: the implementer's place or a place after
// it, read by the place, since every step of a role the deployment defines runs as step), and a
// tree that lingers (record.TreeLingers, where every role is suspended) are asked nothing.
func (e *Engine) mergeForward(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, pr *record.PullRequest, why record.ConflictWhy) error {
	at := record.StandsAt(issue)
	place, _ := w.placeIn(at)
	if pr.State != record.PullRequestOpen || !classify.MergeForwardStands(w.Line, place) {
		return nil
	}
	asker := record.MergeForwardAsker(at)
	if asked := pr.MergeForward; asked != nil && asked.Head == pr.HeadSHA && (asked.Phase != phase.Implementing || at == phase.Implementing) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	recipients := []roleRecipient{{issue: issue.Key, role: asker, row: issue.Key}}
	for _, step := range issue.RunningAt(at) {
		if step.Role != asker && record.MergeForwardReaches(record.MergeForwardHold, step.Role) {
			recipients = append(recipients, roleRecipient{issue: issue.Key, role: step.Role, row: issue.Key})
		}
	}
	addressed, err := e.addressRoles(ctx, tx, issue.Project, recipients, nil)
	if err != nil {
		return err
	}
	for _, to := range addressed {
		role := to.role
		step := record.MergeForwardHold
		if role == asker {
			step = record.MergeForwardAsk
		}
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: role, Claim: to.token, Step: step, Head: pr.HeadSHA, Base: pr.Base,
			Phase: at, PullRequest: pr.Number, Why: why}); err != nil {
			return err
		}
	}
	if at != phase.Implementing {
		// A held issue stands in held, not in the phase its hold came from (StandsAt), such as an
		// issue held from testing whose conflict a read confirms (mergeability).
		stays := "stays in " + string(at)
		if issue.Phase == phase.Held {
			stays = "stays held from " + string(at)
		}
		reason := fmt.Sprintf("the head %s conflicts with %s (%s): the %s is asked to merge %s forward, and the issue %s",
			pr.HeadSHA, pr.Base, why, asker, pr.Base, stays)
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "checks-red", Role: claim.RoleArchitect, Phase: at, Reason: reason}); err != nil {
			return err
		}
	}
	next := &record.MergeForward{Head: pr.HeadSHA, Base: pr.Base, Role: asker, Phase: at, Why: why, AskedAt: e.now()}
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

// mergeForwardOnEntry is what a move does with the merge forward of the pull request of s's task
// once the move has put its copy in the phase it now stands in and written that phase's start
// (transition). It reads where the move put the copy, never the phase the move left, and s's walk
// as the move left it. pr is that pull request as the move's handler read it in the transaction,
// after the handler's own writes of it, nil where the task has none: the transition reads the row
// once, and nothing it does before this writes the row.
//
//   - A move into implementing drops the record's merge forward of the task's pull request
//     (clearTaskMergeForward): the implementer works the head there, and a head it pushes is read
//     for a conflict of its own.
//   - At a head the record holds in a confirmed conflict (classify.ConflictRound), a move into
//     merging asks the merger for the merge forward for record.ConflictMergeNext: the merge comes
//     next, GitHub merges no head that conflicts with its base, and the merge is the merger's
//     (cybermonklabs/legion#4979). This is where an approval of a conflicting head, which ends its
//     round (reviewRound), is acted on once every step of the task has run: a move to where the
//     merge comes next before merging (classify.MergeComesNext: retro, or accepting where no step
//     of the task follows the acceptance) asks nothing, since the conflict waits for the merger
//     (classify.ConflictNeedsMerge). Where the record still holds an ask of that head, made of
//     another role in a phase before merging and not landed, that role keeps the merge, since it
//     may already be at work on it, and the merger is held until it lands (holdMerger) rather than
//     asked. On a task where a step follows the review or the acceptance, a move into that step
//     asks nothing either: the conflict was decided by R0's reasons where it was confirmed, and
//     the step goes on at the head as a tester or a reviewer after the implementer does.
//   - At such a head, a move into a phase after implementing on the task's line (standsBefore: a
//     planning after the implementer too, a planning before it never) where a step of the task
//     still runs before the merge (not classify.MergeComesNext), while the record's ask of that
//     head is still the one made in implementing, asks again for the reason it was asked for
//     (record.MergeForward.Why): an ask made there covers implementing alone, so an implementer
//     that completes at that head without the merge leaves the next phase to ask, with its hold
//     and its notice. It is keyed on the ask, not on the phase the move left, so no such phase
//     starts at that head with the ask of implementing alone. A move to where the merge comes next
//     asks nothing, as any move there at a conflicting head does: a plan with no tester and no
//     reviewer step moves out of implementing into its retro, or into an acceptance no step
//     follows, the ask of implementing stays on the record, and the move into merging asks the
//     merger. A parent held before its retro, and its release into it, ask nothing either and
//     make no call here (holdBeforeRetro, startHeldRetro).
//
// mergeForward asks once for each head, so a move into merging at a head the merger was already
// asked of asks nothing more.
func (e *Engine) mergeForwardOnEntry(ctx context.Context, tx pgx.Tx, s TaskScope, pr *record.PullRequest) error {
	issue, w := s.issue, s.walk
	if issue.Phase == phase.Implementing {
		return e.clearTaskMergeForward(ctx, tx, s, pr)
	}
	if pr == nil || !classify.ConflictRound(*pr) {
		return nil
	}
	asked := pr.MergeForward
	switch {
	case record.StandsIn(issue, phase.Merging):
		if asked != nil && asked.Head == pr.HeadSHA && asked.Phase != phase.Implementing && asked.Asked() != claim.RoleMerger {
			return e.holdMerger(ctx, tx, issue, pr)
		}
		return e.mergeForward(ctx, tx, issue, w, pr, record.ConflictMergeNext)
	case asked != nil && asked.Head == pr.HeadSHA && asked.Phase == phase.Implementing &&
		w.standsBefore(phase.Implementing, record.StandsAt(issue)) && !classify.MergeComesNext(issue, w.Line):
		return e.mergeForward(ctx, tx, issue, w, pr, asked.Why)
	}
	return nil
}

// standsBefore says whether p stands before at, where w's task stands, on the task's own order: a
// move of the task from at back to p goes back (Walk.MovesBack). A stored walk compares the place
// of p (PlaceOf) with the task's place at at (placeIn), and a run with no plan rows compares the two
// phases on today's line. A phase the line does not hold stands before nothing, and nothing stands
// before it.
func (w Walk) standsBefore(p, at phase.Phase) bool {
	from, here := w.placeIn(at)
	to, there := w.PlaceOf(p)
	return here && there && w.MovesBack(Row{From: at, To: p, FromPlace: from, ToPlace: to})
}

// holdMerger tells the merger of an issue that has just entered merging that another role's merge
// forward of the head stands and has not landed (mergeForwardOnEntry): the hold
// (record.MergeForwardHold) has it merge nothing and wait for the landed message, which
// mergeForwardLanded sends it as the worker of merging, with the reviewer's review of the new head.
// The hold names the claim of the merger's step, as every merge-forward message does
// (addressRoles). The record's ask is left as it is. A tree that lingers tells nobody.
func (e *Engine) holdMerger(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	asked := pr.MergeForward
	addressed, err := e.addressRoles(ctx, tx, issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleMerger, row: issue.Key}}, nil)
	if err != nil {
		return err
	}
	for _, to := range addressed {
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: claim.RoleMerger, Claim: to.token, Step: record.MergeForwardHold,
			Head: asked.Head, Base: asked.Base, Phase: phase.Merging, PullRequest: pr.Number, Why: asked.Why}); err != nil {
			return err
		}
	}
	e.logOnCommit(ctx, "workflow: the merger waits for a merge forward another role was asked", "issue", issue.Key, "tree", issue.Tree,
		"pullRequest", pr.Number, "head", asked.Head, "asked", asked.Asked(), "askedIn", asked.Phase)
	return nil
}

// clearTaskMergeForward drops the merge forward the record holds for pr, the pull request of s's
// task as the move read it (record.PullRequest.MergeForward), on a move into implementing
// (mergeForwardOnEntry): the ask, its landing and the review it asked for are of a stretch of work
// that is over, and a later conflict is asked about afresh. A run with no plan rows drops each of
// its pull requests' (clearMergeForwards). A record that holds none is not written.
func (e *Engine) clearTaskMergeForward(ctx context.Context, tx pgx.Tx, s TaskScope, pr *record.PullRequest) error {
	if !s.Planned() {
		return e.clearMergeForwards(ctx, tx, s.issue)
	}
	if pr == nil {
		return nil
	}
	return e.clearMergeForwardsOf(ctx, tx, []record.PullRequest{*pr})
}

// clearMergeForwards drops the merge forward of each pull request of issue
// (record.Store.TaskPullRequests) when the issue leaves the workflow (leave), since its leave ends
// the work of every task, and on a move into implementing of a run with no plan rows.
func (e *Engine) clearMergeForwards(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	prs, err := e.store.TaskPullRequests(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	return e.clearMergeForwardsOf(ctx, tx, prs)
}

// clearMergeForwardsOf drops the merge forward of each of prs that holds one.
func (e *Engine) clearMergeForwardsOf(ctx context.Context, tx pgx.Tx, prs []record.PullRequest) error {
	for _, pr := range prs {
		if pr.MergeForward == nil {
			continue
		}
		pr.MergeForward = nil
		if err := e.putPullRequest(ctx, tx, pr); err != nil {
			return err
		}
	}
	return nil
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
// to read what the merge changed before it goes on (record.MergeForwardLanded). An issue whose
// task's reviewer step already ran - that step stands before the place the issue stands at on the
// walk's line (standsBefore) - stands on a review of the old head, so the reviewer is asked to
// read the merge and approve the new head or request changes (record.MergeForwardReview), and the
// record keeps that the review was asked for (ReviewAsked), which the merger's task
// (pullRequestPart) and the landed the merger is told in merging
// (record.MergeForwardMessage.Message) read. In the default steps that is an issue in accepting,
// retro or merging. A reviewer step still to come reviews the head the issue stands at when it
// runs, so it is asked nothing now. A task whose plan has no reviewer step stands on no review of
// the old head, no reviewer step runs for it, and a merger told to wait for a reviewer's approval
// would wait for good, so nothing is asked and the merger goes on at the new head.
// The daemon records no decision outside reviewing, so that review is the roles' to hold to the
// head they merge. The architect and the role asked are told nothing more: the architect's
// checks-red notice already said the merge was asked for, and the role asked pushed it; in merging
// that role is the merger, which the landed reaches as the worker of merging, since it tells it to
// merge the new head only once the reviewer's approval of it is on the pull request, where the
// task has a reviewer step, and to go on at the new head where it has none. A tree that
// lingers (record.TreeLingers, where every role is suspended) records and tells nothing.
func (e *Engine) mergeForwardLanded(ctx context.Context, tx pgx.Tx, s TaskScope, pr *record.PullRequest) error {
	issue, w := s.issue, s.walk
	asked := pr.MergeForward
	if asked == nil || asked.Landed != "" || asked.Head == pr.HeadSHA {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	at := record.StandsAt(issue)
	review := w.standsBefore(phase.Reviewing, at)
	var recipients []roleRecipient
	for _, step := range issue.RunningAt(at) {
		if record.MergeForwardReaches(record.MergeForwardLanded, step.Role) {
			recipients = append(recipients, roleRecipient{issue: issue.Key, role: step.Role, row: issue.Key})
		}
	}
	if review {
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: claim.RoleReviewer, row: issue.Key})
	}
	addressed, err := e.addressRoles(ctx, tx, issue.Project, recipients, nil)
	if err != nil {
		return err
	}
	for _, to := range addressed {
		role := to.role
		step := record.MergeForwardLanded
		if review && role == claim.RoleReviewer {
			step = record.MergeForwardReview
		}
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: role, Claim: to.token, Step: step, Head: asked.Head, Landed: pr.HeadSHA,
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

// lostRole says whether role is a role of the deployment that deployment no longer defines: one
// a daemon restarted without, whose task is stopped (stopLostRole) and whose claim no message of a
// merge forward reaches. Legion's own roles are never lost.
func lostRole(role claim.Role, deployment plan.Deployment) bool {
	return !claim.IsBuiltin(role) && !deployment.Defines(string(role))
}

// MergeForwardAt reads in tx what a merge-forward message (record.MergeForwardMessage) of issue
// key is judged by: the issue, whether its tree lingers (record.TreeLingers) and the pull request
// the message is about: the issue's pull request that holds a merge forward of head, whichever
// task's it is, and the pull request of the issue's task that merges where none does
// (record.Store.MergePullRequest). It returns the issue and that pull request, with why a message
// of step to role about the merge forward asked of head is no longer to be delivered
// (leftMergeForwardAt), "" while it is. An issue the record does not hold is a nil issue, no pull
// request and no reason.
// The daemon's executor is its one caller: it reads the rule in one transaction at each attempt of
// a row, just before it delivers (the daemon's mergeForwardMessage), so the executor judges each
// queued row by this rule, and no row outlives the state it was written for. The workflow calls
// neither this nor leftMergeForwardAt: it writes these rows only in states the rule accepts. The
// rule is kept beside mergeForward and mergeForwardLanded, which write the rows, and
// clearTaskMergeForward, which drops the ask they are about, since it restates their states for a row
// already queued. It is exported for that executor. The pull request is its row alone
// (record.Store.TaskPullRequestRows, MergePullRequestRow): the executor names its repository and
// writes nothing of it.
func MergeForwardAt(ctx context.Context, store record.Store, tx pgx.Tx, deployment plan.Deployment, key string, role claim.Role, step record.MergeForwardStep, head string) (*record.Issue, *record.PullRequest, string, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, nil, "", err
	}
	lingers, err := record.TreeLingers(ctx, store, tx, issue.Tree)
	if err != nil {
		return nil, nil, "", err
	}
	prs, err := store.TaskPullRequestRows(ctx, tx, key)
	if err != nil {
		return nil, nil, "", err
	}
	at := slices.IndexFunc(prs, func(pr record.PullRequest) bool { return pr.MergeForward != nil && pr.MergeForward.Head == head })
	if at >= 0 {
		return issue, &prs[at], leftMergeForwardAt(*issue, lingers, &prs[at], deployment, role, step, head), nil
	}
	pr, err := store.MergePullRequestRow(ctx, tx, key)
	if err != nil {
		return nil, nil, "", err
	}
	return issue, pr, leftMergeForwardAt(*issue, lingers, pr, deployment, role, step, head), nil
}

// leftMergeForwardAt says why a merge-forward message of step to role about the merge forward
// asked of head is no longer to be delivered, or "" while it is: the issue is in the workflow, its
// tree does not linger (lingers, record.TreeLingers), and the pull request the record holds for it
// (pr, nil for none) is open and holds a merge forward of head (record.PullRequest.MergeForward)
// that, for an ask or a hold, has not landed. In each other state the message would tell its role
// of work nobody wants any more. The roles of a tree that lingers are suspended. The workflow drops
// the ask on a move into implementing and when the issue leaves the workflow (clearTaskMergeForward and clearMergeForwards),
// and a merge forward of another head is a newer ask, which has its own messages. A pull request
// that is not open ends every step: mergeForward asks nothing for one, and once it closed or
// merged nothing is left to merge forward, to hold for or to review. The ask and the hold end at
// the landing (record.MergeForward.Landed): role-message rows run in no order of the issue's, so a
// hold still queued would reach the worker after its landed and tell it to wait for a head that
// already came, and an ask would ask for a merge already pushed. A message to the merger ends when
// the issue no longer stands in merging: a backward move gives the workspace to an earlier phase,
// and the merger must not be woken to write or merge there. The record keeps the ask and its
// earlier landings, so their completion fences and review still stand. The landed and the review
// are the landing's own messages, and they still go after it while their role applies.
func leftMergeForwardAt(issue record.Issue, lingers bool, pr *record.PullRequest, deployment plan.Deployment, role claim.Role, step record.MergeForwardStep, head string) string {
	switch {
	case !record.InWorkflow(issue):
		return fmt.Sprintf("the issue has left the workflow, at status %s in %s", issue.Status, issue.Phase)
	case lingers:
		return "the issue's tree lingers or has closed"
	case lostRole(role, deployment):
		return fmt.Sprintf("the deployment no longer defines the role %s", role)
	case pr == nil:
		return "the record holds no pull request for the issue"
	case pr.State != record.PullRequestOpen:
		return fmt.Sprintf("the pull request is %s", pr.State)
	case role == claim.RoleMerger && !record.StandsIn(issue, phase.Merging):
		return fmt.Sprintf("the issue no longer stands in merging, at phase %s", record.StandsAt(issue))
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
// left. The merger's task reads it (pullRequestPart), started or resumed; the landed message reads
// ReviewAsked alone, since it names the head the merge left (record.MergeForwardMessage.Message).
func mergeForwardReviewOwed(pr record.PullRequest) bool {
	asked := pr.MergeForward
	return asked != nil && asked.ReviewAsked && asked.Landed == pr.HeadSHA
}

// mergedForwardRefusal is the completion fence of a merge forward (dispatch://INFRA-4374 R1): a
// completion of testing, reviewing, step or retro whose commit is the head a merge forward was asked of,
// once that merge landed and the pull request stands at another head, reports work on code the
// issue no longer stands on, and is refused with HANDOFF_HEAD_MERGED_FORWARD before anything is
// recorded (handoff). The merge forward is the record's landed ask (record.MergeForward.Landed)
// or any that landed before it in the same stretch of work (record.MergeForward.Earlier,
// mergedForwardOf), so a head the first merge replaced stays fenced after the head that merge
// left conflicts in its turn and is merged forward again. The refusal names the head that merge
// replaced, the head it left and the head the pull request stands at now, and tells the role to
// read what the merge changed and complete again at that head.
//
// A completion that names no commit is refused the same way once a merge forward of the record's
// stretch of work landed: the tester and the reviewer take their commit from the head their
// handoff record names, and a record that holds no head that parses reports none
// (packages/pi-shared/src/handoff-record.ts), so such a completion says nothing about which head
// its work judged, as the completion route's entry fence holds of a completion behind its entry
// (api's behindEntryClause), and the head it judged may be the one the merge replaced. That
// refusal names the head the pull request stands at now and the head the newest merge forward
// that landed left, whose merge the role can read, and tells the role to do the phase's work at
// the head the pull request stands at and complete again at it.
//
// Neither refusal sends the role to the head the pull request stands at while the record's ask of
// that head has not landed and its hold was sent to that role - the head an earlier merge left
// conflicts in its turn, its merge forward is asked in the phase the completion reports, and the
// hold reaches the role that completes it (record.MergeForwardReaches) - since that hold tells
// the role not to complete at that head (record.MergeForwardHold). Then the refusal names the
// head the pull request stands at, says its merge forward is asked and has not landed, and tells
// the role to wait for the daemon's landed message (record.MergeForwardLanded) and do the phase's
// work at the head that message names (awaitedMergeForward). That wait is told only to a role the
// ask's hold was sent to: every other completion is answered as above, at the head the pull
// request stands at, since no hold told its role not to complete there. The implementer, whose
// completion of retro is fenced, is sent no hold, the merge being its own to make; and an ask made
// in implementing sends none, and stands into the next phase when the move there does not ask
// again (mergeForwardOnEntry), as at a head the record no longer holds in a confirmed conflict.
//
// A phase that ends with a handoff record is told to write it again, with its `head` set to the
// head the role is told to complete at, since a completion naming the record it already named is
// the one the route answers as received (api/handoff.go) and the commit the completion reports is
// the head the record names.
//
// The fence is the merge forward's alone, not every push's: a completion at the current head, at
// a head no merge forward replaced, naming no commit while no merge forward has landed, or of any
// other phase passes it. handoff asks it only of a completion of the phase the issue stands in,
// so a completion of a phase the issue has left passes it too: that completion moves nothing, and
// its handoff is the report of the work the role did before the move, which the next phase reads
// (leftItsPhase). The reviewer whose approval moved the issue on completes reviewing after the
// move, at the head it reviewed, and the review a landed merge forward then asks of it is made
// with no completion (record.MergeForwardReview), so a refusal would lose that report for good.
//
// The fence knows a head by its name alone, which leaves one completion it cannot judge. A role of
// the deployment reports the commit its workspace stands on (`@-`, pi-shared's handoff-record.ts),
// so where that role made a commit of its own above a head a merge forward replaced, its
// completion names that commit: neither the replaced head nor the head the pull request stands at,
// it passes. Refusing it would take the commit's ancestry, which the daemon does not hold: the
// record keeps each merge forward's replaced head and the head it left, not a commit graph, and a
// commit the role did not push is on no remote that GitHub could be asked about. Such a
// completion is taken, and the troubleshooting guide's "The work loops or stops" says how an
// operator finds it and sends the task back to the step.
//
// It answers false where it does not refuse.
func mergedForwardRefusal(issue record.Issue, role claim.Role, reported phase.Phase, commit string, pr *record.PullRequest) (intake.Result, bool) {
	switch reported {
	case phase.Testing, phase.Reviewing, phase.Step, phase.Retro:
	default:
		return intake.Result{}, false
	}
	if pr == nil || pr.MergeForward == nil || commit == pr.HeadSHA {
		return intake.Result{}, false
	}
	asked := pr.MergeForward
	// clause is what the completion reports - the head it is at, or that it names none - with the
	// merge forward that replaced that head, or the newest that landed, and landed the head that
	// merge left, whose merge the role can read.
	var clause, landed string
	if commit == "" {
		landed = asked.Landed
		if landed == "" && len(asked.Earlier) > 0 {
			landed = asked.Earlier[len(asked.Earlier)-1].Landed
		}
		if landed == "" {
			return intake.Result{}, false
		}
		clause = fmt.Sprintf("names no head, so it says nothing about which head its work judged, and a merge of %s forward landed at head %s",
			asked.Base, landed)
	} else {
		replaced, ok := mergedForwardOf(asked, commit)
		if !ok {
			return intake.Result{}, false
		}
		landed = replaced.Landed
		clause = fmt.Sprintf("is at head %s, and a merge of %s forward replaced that head with head %s", replaced.Head, asked.Base, landed)
	}
	again := "complete again at that head"
	if word, _, ok := phase.Handoff(reported); ok {
		again = fmt.Sprintf("write this phase's handoff again with `dispatch handoff-write --issue %s --phase %s --body-file <file>`, its `head` set to that head, and complete again naming it",
			issue.Key, word)
	}
	pending := asked.Landed == "" && asked.Head == pr.HeadSHA && asked.Phase == reported && record.MergeForwardReaches(record.MergeForwardHold, role)
	var next string
	if pending {
		next = awaitedMergeForward(pr, again)
	} else {
		next = fmt.Sprintf("Pull request #%d stands at head %s now. Read what the merge changed with `git show --remerge-diff %s`, do the phase's work at head %s, then %s; this completion changed nothing",
			pr.Number, pr.HeadSHA, landed, pr.HeadSHA, again)
	}
	return refused("HANDOFF_HEAD_MERGED_FORWARD", fmt.Sprintf("the %s's completion of phase %s of %s %s. %s", role, reported, issue.Key, clause, next)), true
}

// awaitedMergeForward ends a refusal of mergedForwardRefusal while the record's ask is of the head
// pr stands at, has not landed, and sent its hold to the role that completes, which tells that
// role not to complete at that head (record.MergeForwardHold). It names that head, says the merge
// of the ask's base forward onto it is asked and has not landed, and tells the role to wait for
// the daemon's landed message (record.MergeForwardLanded), do the phase's work at the new head
// that message names, and then complete at it as again says.
func awaitedMergeForward(pr *record.PullRequest, again string) string {
	return fmt.Sprintf("Pull request #%d stands at head %s now, and the merge of %s forward onto head %s is asked and has not landed. Wait for the daemon's message that this merge landed, do the phase's work at the new head that message names, then %s; this completion changed nothing",
		pr.Number, pr.HeadSHA, pr.MergeForward.Base, pr.HeadSHA, again)
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
	prs, err := e.store.TaskPullRequests(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	at := slices.IndexFunc(prs, func(pr record.PullRequest) bool { return pr.Number == fact.Number })
	if at < 0 {
		return intake.Result{}, nil
	}
	pr := &prs[at]
	if pr.ConflictRead == nil || pr.ConflictRead.Head != fact.Head || !pr.ConflictRead.At.Equal(fact.FirstReadAt) {
		return intake.Result{}, nil
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
