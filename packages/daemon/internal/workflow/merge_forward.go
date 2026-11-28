package workflow

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// mergeForward is the daemon's act on a confirmed conflict that R0 found a reason to act on, why
// (dispatch://INFRA-4374 R0, R1; classify.ConflictNeedsMerge): the issue keeps the phase it stands
// in (record.StandsAt), no phase entry starts and no supervise row is written, so an issue past
// its review takes no admission slot for the merge, and the role the phase asks
// (record.MergeForwardAsker) is asked, on its own role topic, to merge the base forward onto the
// head and push (record.MergeForwardAsk): the implementer before merging, and in merging the
// merger, whose merge it is (cybermonklabs/legion#4979). Outside implementing it also tells the
// worker of that phase (mergeForwardWorker, record.MergeForwardWorker: the planner, the tester,
// the reviewer, the merger or, at step, the role of the running step) that is not the role asked
// to stop writing the workspace the roles share while the merge runs (record.MergeForwardHold),
// and the architect that owns the issue one checks-red notice whose phase is the phase the issue
// stays in and whose reason names the head, the base, why, the role asked and that the issue stays
// in its phase; the outbox routes that notice to the owning architect, as every notice of the
// issue is routed. In implementing only the ask goes: the implementer's own assignment and its
// completion stand, and the architect is told nothing, since the work the conflict needs is
// already where the issue stands.
//
// It asks once for each head: the record keeps the ask (record.PullRequest.MergeForward), and a
// later confirmed read, or a move into another phase, at the head it names asks no role anew, so
// the role first asked of a head keeps that merge until it lands; the move into merging only tells
// the merger's launch the merger's ask again where no row of it reached that launch
// (askMergerAtEntry). The one exception is an ask made in implementing, which covers implementing
// only: an implementer that completes at that head without the merge leaves the issue's next
// phase to ask again, with its hold and its notice, where a step of the task still runs there
// before the merge, and the move into merging to ask the merger where none does
// (mergeForwardOnEntry). Any other ask of the implementer that has not landed stays the
// implementer's into each phase the issue moves to, and the worker that starts there is held
// instead (mergeForwardOnEntry, holdStartedWorker).
//
// So one merge of one head has one asked role at a time, and two roles are never asked to merge
// one base forward onto one head. Where the merger is asked for a head the record holds an
// implementer's ask of that has not landed - the ask of implementing, which the move into merging
// does not keep - the merger's ask replaces it on the record, an ask row of the implementer's still
// queued is delivered no more (leftMergeForwardAt), and the implementer, which may already hold
// that ask, is told once that the merge is no longer its work (record.MergeForwardWithdrawn). A
// merge the implementer pushes after that is a new head, which the merger's ask follows as any
// landing does (mergeForwardLanded).
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
// tree that lingers (record.TreeLingers, where every role is suspended) are asked nothing. Nor is
// an issue whose claim of the role asked runs in the slot of a role the deployment no longer
// defines (lostSlot): nobody can make the merge, so no role is told, the architect is not told
// that role is asked, and the record keeps no ask.
func (e *Engine) mergeForward(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, pr *record.PullRequest, why record.ConflictWhy) error {
	at := record.StandsAt(issue)
	place, _ := w.placeIn(at)
	if pr.State != record.PullRequestOpen || !classify.MergeForwardStands(w.Line, place) {
		return nil
	}
	asker := record.MergeForwardAsker(at)
	if _, owed := owedAsk(issue, *pr); owed {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	steps := map[claim.Role]record.MergeForwardStep{asker: record.MergeForwardAsk}
	recipients := []roleRecipient{{issue: issue.Key, role: asker, row: issue.Key}}
	if worker, held := e.mergeForwardWorker(issue, at, record.MergeForwardHold); held && worker != asker {
		steps[worker] = record.MergeForwardHold
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: worker, row: issue.Key})
	}
	if withdrawnBy(*pr, asker) {
		steps[claim.RoleImplementer] = record.MergeForwardWithdrawn
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: claim.RoleImplementer, row: issue.Key})
	}
	addressed, err := e.addressRoles(ctx, tx, issue.Project, recipients, nil)
	if err != nil {
		return err
	}
	// The claim of a keyed agent runs in the slot of its name, which can be a role the deployment
	// lost while its step's role is not (lostSlot): its task is stopped as the lost role's is, and a
	// message to it would wake a claim no pod can launch. Where that claim is the role asked's (the
	// implementer, or in merging the merger), nobody can make the merge, so nothing is asked: no
	// hold, which would keep the worker waiting for a merge nobody makes, no notice saying that role
	// is asked, and no ask on the record, so a later confirmed read of the head asks again once the
	// role is defined again.
	for _, to := range addressed {
		if to.role == asker && lostSlot(to.token, e.cfg.Deployment) {
			slot, _ := to.token.Slot()
			e.logOnCommit(ctx, "workflow: no merge forward is asked: the claim of the role asked runs in the slot of a role the deployment no longer defines",
				"issue", issue.Key, "pullRequest", pr.Number, "head", pr.HeadSHA, "role", asker, "claim", to.token, "slot", slot)
			return nil
		}
	}
	for _, to := range addressed {
		if lostSlot(to.token, e.cfg.Deployment) {
			continue
		}
		role := to.role
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: role, Claim: to.token, Step: steps[role], Head: pr.HeadSHA, Base: pr.Base,
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

// mergeForwardOnEntry is what a move does with the merge forward of the issue's pull request once
// the move has put issue in the phase it now stands in and written that phase's start
// (transition). It reads where the move put the issue, never the phase the move left. w is the
// walk of issue's run, as the move left it. The pull request is read again in the transaction,
// since the copy a transition is handed can be none, or one read before the transaction's other
// writes.
//
//   - A move into implementing drops the record's merge forward (clearMergeForward): the
//     implementer works the head there, and a head it pushes is read for a conflict of its own.
//   - Where the merge of the head the pull request stands at is still owed to a role where the
//     move put the issue (owedAsk: an ask of the implementer made outside implementing, the
//     merger's ask in merging), the move asks no other role, since that role may already be at
//     work on the merge, and tells the worker of the phase it entered to wait for that merge
//     (holdStartedWorker), on a forward move and on a backward move alike: every role works in
//     the one workspace the merge writes. So the merger that enters merging while the
//     implementer owes the merge is held rather than asked, and so is the planner, the tester or
//     the reviewer of any step the move starts, in whatever order the task's steps run. The
//     merger that owes the merge itself is held by nothing (heldWorker): the move tells its
//     current launch the ask where no row of it reached that launch and none is queued
//     (askMergerAtEntry), since a row of it that ended while the issue stood outside merging,
//     where the merger owes nothing, told it nothing. The architect in accepting writes no
//     workspace and is told by the ask's checks-red notice, and the implementer in retro is the
//     role asked, so neither is held. The owed ask decides it, as it decides the completion
//     fence's wait (mergedForwardRefusal) and the delivery of each row (leftMergeForwardAt),
//     whatever GitHub's last read of the head said, since a MERGEABLE read leaves the ask and its
//     role may still merge.
//   - Otherwise the merge of the head is owed to nobody where the issue now stands: no ask of it,
//     or an ask its role no longer owes there (owedTo: the ask of implementing outside
//     implementing, the merger's ask outside merging, or a withdrawn one). At a head the record
//     holds in a confirmed conflict (classify.ConflictRound), the move asks again where the merge
//     is owed in the phase it entered. A move into merging asks the merger for
//     record.ConflictMergeNext: the merge comes next, GitHub merges no head that conflicts with
//     its base, and the merge is the merger's (cybermonklabs/legion#4979). A move into a phase
//     where a step of the task still runs before the merge (not classify.MergeComesNext) asks the
//     role of that phase for the reason the open ask was asked for (record.MergeForward.Why): an
//     ask made in implementing covers implementing alone, so an implementer that completes at
//     that head without the merge leaves the next phase to ask, with its hold and its notice; and
//     the merger's merge-next, after a move back out of merging, is asked again for the reason R0
//     gives with no rules read at the move (classify.ConflictNeedsMerge: record.ConflictRulesUnread
//     in every phase where the merge does not come next). mergeForward asks nothing before the implementer
//     (classify.MergeForwardStands). A move to where the merge comes next before merging
//     (classify.MergeComesNext: retro, or accepting where no step of the task follows the
//     acceptance) asks nothing, since the conflict waits for the merger
//     (classify.ConflictNeedsMerge); an approval of a conflicting head, which ends its round
//     (reviewRound), is acted on there once every step of the task has run. With no open ask, a
//     move into a step that follows the review or the acceptance asks nothing either: the conflict
//     was decided by R0's reasons where it was confirmed, and the step goes on at the head as a
//     tester or a reviewer after the implementer does.
//   - Where the move leaves the implementer's open ask owed by nobody and nobody was asked again,
//     the implementer is told once that the merge is no longer its work (withdrawUnowed), and the
//     record keeps the ask, marked withdrawn, for its completion fence. A parent held before its
//     retro, and its release into it, make no call here (holdBeforeRetro, StartHeldRetro), so its
//     ask of implementing is withdrawn when the merger is asked (mergeForward).
//
// mergeForward asks nobody while the merge of the head is owed, so a move into merging at a head
// the merger was already asked of writes no new ask: it tells the merger's current launch the ask
// the record holds where no row of it reached that launch and none is queued (askMergerAtEntry),
// and nothing more.
func (e *Engine) mergeForwardOnEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) error {
	if issue.Phase == phase.Implementing {
		_, err := e.clearMergeForward(ctx, tx, issue.Key)
		return err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return err
	}
	asked, open := openAsk(*pr)
	if open && owedTo(issue, asked) {
		if asked.Asked() == claim.RoleMerger {
			if err := e.askMergerAtEntry(ctx, tx, issue); err != nil {
				return err
			}
		}
		return e.holdStartedWorker(ctx, tx, issue, pr)
	}
	if classify.ConflictRound(*pr) {
		switch {
		case record.StandsIn(issue, phase.Merging):
			if err := e.mergeForward(ctx, tx, issue, w, pr, record.ConflictMergeNext); err != nil {
				return err
			}
		case open && !classify.MergeComesNext(issue, w.Line):
			// The ask of the phase the move left is asked again of the role of the phase it entered,
			// for the reason it was asked for, save the merger's merge-next, which no phase before
			// merging holds: R0 with no rules read at the move answers base-rules-unread there.
			why := asked.Why
			if why == record.ConflictMergeNext {
				why = record.ConflictRulesUnread
			}
			if err := e.mergeForward(ctx, tx, issue, w, pr, why); err != nil {
				return err
			}
		}
	}
	return e.withdrawUnowed(ctx, tx, issue, pr)
}

// openAsk is the merge forward pr's record holds while it is the open ask of the head pr stands
// at - an ask of that head that has not landed, on a pull request that is open - and whether it is.
// A close keeps the record's ask (closed), but a closed pull request gets no push and no landing,
// so nothing it holds is owed and no worker waits for it; a reopen drops the ask
// (pullRequestOpened). Every reader of the record's ask reads this one rule, awaitChildren's drop
// of it among them. Two checks of the state stand apart, on purpose, since neither reads the
// record's ask: mergeForward's guards the writing of a new ask, so a closed pull request is asked
// nothing, and leftMergeForwardAt's ends every queued row of a pull request that is not open, the
// landed and the review among them.
func openAsk(pr record.PullRequest) (record.MergeForward, bool) {
	asked := pr.MergeForward
	if pr.State != record.PullRequestOpen || asked == nil || asked.Head != pr.HeadSHA || asked.Landed != "" {
		return record.MergeForward{}, false
	}
	return *asked, true
}

// owedTo says whether the role asked names still owes the merge while issue stands where
// it does: the merger's ask while the issue stands in merging, where the merge is its own; the
// implementer's ask made in implementing while it stands in implementing, which that ask covers
// alone; the implementer's ask made in any later phase wherever the issue stands, merging included,
// where the merger is held for it; and never once the implementer was told that the ask is no
// longer its work (record.MergeForward.Withdrawn). An asked role whose claim ended still owes it:
// the head still conflicts, a worker that goes on at it makes work the merge replaces, and the
// ready of that role's next launch asks it again (askAgainAtLaunch).
func owedTo(issue record.Issue, asked record.MergeForward) bool {
	switch {
	case asked.Withdrawn:
		return false
	case asked.Asked() == claim.RoleMerger:
		return record.StandsIn(issue, phase.Merging)
	case asked.Phase == phase.Implementing:
		return record.StandsIn(issue, phase.Implementing)
	}
	return true
}

// owedAsk is the record's open ask of the head pr stands at (openAsk) while its asked role owes the
// merge where issue stands (owedTo), and whether it is. It is the one rule of whether the merge of
// that head is owed to a role, which every reader of the ask reads: mergeForward, which asks no
// second role while it is; the hold at each start and the completion fence (heldWorker); the
// landed message (mergeForwardLanded); the delivery of each row (leftMergeForwardAt); and the ask
// sent again at the asked role's next launch (askAgainAtLaunch) and to the merger's launch at the
// move into merging (askMergerAtEntry).
func owedAsk(issue record.Issue, pr record.PullRequest) (record.MergeForward, bool) {
	asked, open := openAsk(pr)
	return asked, open && owedTo(issue, asked)
}

// heldWorker is the worker of the phase issue stands at that the merge owed at the head pr stands
// at keeps waiting (owedAsk), and whether there is one: the worker of that phase where the hold
// reaches it (record.MergeForwardWorker) and it is not the asked role itself. So the implementer's
// ask of implementing holds nobody, and neither does the merger's ask, each owed only in its
// role's own phase. A worker that starts there while the merge is owed is held
// (holdStartedWorker), and one that works there when the ask is made is held by the ask itself
// (mergeForward), so it is the one rule of which worker a hold has told to wait, which the
// completion fence reads too (mergedForwardRefusal). It compares roles, as the issue alone says
// them; where the claim of that worker is the asked role's, one agent on both steps as the steps
// of one key make it, the merge is that agent's own and no hold reaches it, which each reader that
// holds the claims compares (oneAgent): holdStartedWorker, owedAskClause and
// mergedForwardRefusalOf.
func heldWorker(issue record.Issue, pr record.PullRequest) (claim.Role, bool) {
	asked, owed := owedAsk(issue, pr)
	if !owed {
		return "", false
	}
	worker, held := record.MergeForwardWorker(record.MergeForwardHold, issue, record.StandsAt(issue))
	return worker, held && worker != asked.Asked()
}

// withdrawnBy says whether an ask of asker ends the record's open ask: the merger asked where the
// record holds the implementer's open ask that was never withdrawn, the ask of implementing, which
// is all that reaches the merger's ask (mergeForward asks nobody while a merge is owed). One merge
// of one head has one asked role at a time, so the implementer is told that the merge is no longer
// its work (record.MergeForwardWithdrawn).
func withdrawnBy(pr record.PullRequest, asker claim.Role) bool {
	before, open := openAsk(pr)
	return asker == claim.RoleMerger && open && !before.Withdrawn && before.Asked() == claim.RoleImplementer
}

// withdrawUnowed tells the implementer once that the merge of the head is no longer its work where
// a move left the record's open ask of the implementer owed by nobody and no role was asked again
// (mergeForwardOnEntry): the ask of implementing on a move into the retro or an acceptance the merge
// comes next after, or into a planning before the implementer, where no merge forward stands. The
// record keeps the ask, marked withdrawn, for its completion fence, and asks again on a later move
// or read that owes the merge. A tree that lingers tells nobody.
func (e *Engine) withdrawUnowed(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	asked, open := openAsk(*pr)
	if !open || asked.Withdrawn || asked.Asked() != claim.RoleImplementer || owedTo(issue, asked) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	if err := e.tellRole(ctx, tx, issue, pr, claim.RoleImplementer, record.MergeForwardWithdrawn, asked.Why); err != nil {
		return err
	}
	withdrawn := asked
	withdrawn.Withdrawn = true
	pr.MergeForward = &withdrawn
	return e.putPullRequest(ctx, tx, *pr)
}

// askAgainAtLaunch sends the record's ask again at the ready of a launch of the asked role's claim
// (askAgain). The ready is a fact (intake.ClaimReady) the daemon applies once for each launch of a
// claim (claimReady), and it names the role the claim's step has now: one agent can work the asked
// role's step and the step of the role the ready names, as the steps of one key make it, so the
// ready of that claim asks again whichever of its roles it names (dispatch://INFRA-4112 R5). The
// ready of a claim no merge forward asks reads nothing more.
//
// The asked role's claim can end while the ask is owed: it fails (claimFailed), an operator stops
// it, or its agent reports its own exit (supervise's stop and exit, which write no workflow fact).
// A row of the ask queued for that claim then ends undelivered (the daemon's deliverHeld), and the
// next launch of the role holds no ask, so the ready of every later launch asks again, one rule for
// every cause: the merger's retry in merging, the implementer's next start in its retro, or an
// operator's retry of either. The held workers stay held meanwhile, since the merge stays owed.
// Each launch is told the ask once. While a row of the ask is still queued the ready sends nothing
// and leaves the ask to that row - the row that woke a released role among them, whose wake is
// that launch, and the row the move into merging sent the merger at the launch before it
// (askMergerAtEntry) - and the row's end, delivered or not, settles under the fact lock with the
// claim's launch read again (MergeForwardAskEnded): it records the launch the row reached, so the
// ready of that launch, where it comes after the delivery, sends nothing either, and it sends the
// ask to a later launch the row did not reach, whose ready left the ask to the row. A launch that
// resumes the session an earlier launch was told the ask in is asked once more at its ready: the
// workflow cannot tell that session from a new one, and an owed ask is still true when it is told
// again. An ask that landed or was withdrawn, the ask of implementing outside implementing and
// the merger's ask outside merging are owed to nobody, so a ready that comes there sends nothing;
// the merger's launch whose ready came while the issue stood outside merging is told at the move
// back into merging (askMergerAtEntry). A pull request that is not open or a tree that lingers
// tells nobody.
func (e *Engine) askAgainAtLaunch(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.ClaimReady) error {
	project, rows, err := e.workerRows(ctx, tx, issue)
	if err != nil {
		return err
	}
	token, err := record.WorkerClaim(project, issue.Key, fact.Role, rows)
	if err != nil {
		return err
	}
	// An ask names the implementer or the merger (record.MergeForward.Asked), so the ready of a
	// claim that works neither one's step is owed no ask (askAgain) and reads nothing more.
	asked := false
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleMerger} {
		worker, err := record.WorkerClaim(project, issue.Key, role, rows)
		if err != nil {
			return err
		}
		asked = asked || worker == token
	}
	if !asked {
		return nil
	}
	pr, err := askAgain(ctx, e.store, tx, e.cfg.Deployment, issue, fact.Role, token, fact.Launch, e.now())
	if err != nil || pr == nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a merge forward still owed is asked again at its role's new launch", "issue", issue.Key,
		"tree", issue.Tree, "role", fact.Role, "asked", pr.MergeForward.Asked(), "launch", fact.Launch, "phase", record.StandsAt(issue),
		"pullRequest", pr.Number, "head", pr.MergeForward.Head, "askedIn", pr.MergeForward.Phase)
	return nil
}

// mergerAskedAtEntryLine is the line askMergerAtEntry logs where the move into merging tells the
// merger's launch the merge forward it owes. The production check of that send (c-production-ask
// of dispatch://INFRA-5792) reads the line whole, so it must not change.
const mergerAskedAtEntryLine = "workflow: a merge forward still owed is asked again at the move into merging"

// askMergerAtEntry sends the record's ask again to the merger's current launch at a move into
// merging where the merger owes it (mergeForwardOnEntry), by the rule a launch's ready and a row's
// end send it by (askAgain): only to a launch later than the one the record names as told
// (record.MergeForward.SentLaunch), and nothing while a row of it is queued. The merger owes its
// ask in merging alone (owedTo), so a row of it that ended while the issue stood outside merging
// sent nothing (MergeForwardAskEnded), and neither did a ready that came there
// (askAgainAtLaunch); a move back at the same launch brings no ready, and this send is what tells
// that launch. The implementer's asks are not sent here: they stay owed in every phase until the
// daemon withdraws them, so their rows do not end for this cause.
//
// The launch is the merger's claim as the claim store holds it (record.RoleClaim.Launch,
// claims.generation), under the token the daemon addresses the merger by (addressRoles), and it
// is the launch whose session gets the row. The move runs in a fact's transaction, which holds
// the fact lock (intake.SerializeFacts) at READ COMMITTED, so the read sees every claim write
// committed before the move. The claim's machine publishes a launch before it stores it
// (supervise's putClaim) and a claim's generation only grows, so the launch read is never later
// than the one the executor publishes to, which publishes only once the launch its view names
// registered its session and credits that launch (the daemon's deliverHeld): the record never
// names a launch later than one a row reached (recordAskEnd). The machine reports a ready only
// once that ready's claim write landed (supervise's durableWrite), so the launch read is never
// earlier than one whose ready came before the move, the ready that sent nothing outside merging
// among them. A launch whose ready comes after the move finds this row queued and leaves the ask
// to it, and the row's end sends the ask to a later launch the row did not reach
// (MergeForwardAskEnded). Where the launch read is behind the live one - a relaunch in flight, or
// a claim write still owed - the row waits for the later launch's session and is credited to the
// launch it reaches. A claim the store holds failed or retired, and one it holds none of, read as
// launch 0, as the executor reads them (the daemon's claimEnded), so nothing is sent and the ready
// of the next launch asks; a released claim reads as its launch, and the row wakes it.
//
// The exception is R1's (dispatch://INFRA-5792): a delivery whose listener answer names no holder
// credits no launch the listener stored it for, so the record can name a launch earlier than one a
// row reached, and a later send tells that launch a second time. It happens in two cases. Where the
// launch read is behind a relaunch in flight, the delivery is credited to the launch the row was
// sent at, and the row's end sends the later launch a second row. And a first ask's row carries no
// launch (record.MergeForwardMessage.SentLaunch), so its delivery records none (recordAskEnd), and
// each later send at the launch it reached tells that launch again: the row's end
// (MergeForwardAskEnded), and, where the row that end queued itself ends while the issue stands
// outside merging, this send at the move back. So a launch can be told twice, never zero times, as
// with every row of a merge-forward ask (the daemon's deliverHeld).
func (e *Engine) askMergerAtEntry(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
	token, err := e.workerClaim(ctx, tx, issue, claim.RoleMerger)
	if err != nil {
		return err
	}
	held, err := e.store.RoleClaims(ctx, tx, []claim.Token{token})
	if err != nil {
		return err
	}
	var launch uint64
	if merger := held[token]; merger != nil && !supervise.FailedOrRetired(merger.State) {
		launch = merger.Launch
	}
	pr, err := askAgain(ctx, e.store, tx, e.cfg.Deployment, issue, claim.RoleMerger, token, launch, e.now())
	if err != nil || pr == nil {
		return err
	}
	e.logOnCommit(ctx, mergerAskedAtEntryLine, "issue", issue.Key, "tree", issue.Tree, "role", claim.RoleMerger,
		"launch", launch, "sentLaunch", pr.MergeForward.SentLaunch, "phase", record.StandsAt(issue), "pullRequest", pr.Number,
		"head", pr.MergeForward.Head, "askedIn", pr.MergeForward.Phase)
	return nil
}

// holdSentWorker holds role, where it is the worker of the phase issue stands at that the merge of
// the head owed to another role keeps waiting (heldWorker), by the rule that holds a worker a move
// or a retry starts (holdStartedWorker). Its callers are every other way the daemon brings a worker
// back to its phase's work: the ready of each launch of the worker's claim (claimReady), which the
// daemon makes on its own - a process that died and is relaunched within its budgets, a released
// pod a message wakes, a session the runtime cannot resume and replaces with a fresh one in the
// same launch (supervise's startFresh) - and whose fresh session holds none of its predecessor's
// messages, the hold among them; the silence repair's send of the work to a worker up between
// turns with no task (repairSilence), which a held worker that stopped as its hold says is; and
// admission's send of the entry a worker is behind (SendOwedEntry). A worker that already holds
// the hold reads it again, which changes nothing it does: a launch that resumes the session an
// earlier hold reached, and a worker a move starts, which the move's hold (mergeForwardOnEntry) and
// its launch's ready each hold once, both reaching the launch's one session. The asked role is
// never held (heldWorker), and a pull request that is not open holds nobody (openAsk).
func (e *Engine) holdSentWorker(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role) error {
	if !record.InWorkflow(issue) {
		return nil
	}
	// Only the worker a hold of the phase reaches can be held, which the issue alone says, so the
	// pull request is read for that worker alone.
	if worker, reaches := e.mergeForwardWorker(issue, record.StandsAt(issue), record.MergeForwardHold); !reaches || worker != role {
		return nil
	}
	pr, err := e.store.PullRequestRow(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	return e.holdStartedWorker(ctx, tx, issue, pr)
}

// askAgain sends the record's ask of issue's pull request again to the role it asks, at launch, a
// launch of token later than the one the ask was last told (record.MergeForward.SentLaunch), while
// the merge of the head is still owed to that role where issue stands (owedAsk) and no row of that
// ask is queued for that role and head (record.Store.QueuedMergeForwardAsk). token is the claim of
// role's worker. Where role is the role asked, that is the claim the ask goes to. Where role is
// another, the ask goes to token only where token is also the claim of the worker on the asked
// role's step (record.ReadWorkerClaim): one agent works both steps, as the steps of one key make
// it, so a launch of that claim is a launch of the asked role's (dispatch://INFRA-4112 R5).
// The row names token (record.MergeForwardMessage.Claim), the claim launch is of,
// and carries launch (record.MergeForwardMessage.SentLaunch), and the record's ask is left as it
// is: the launch is recorded only once a row of the ask is delivered (MergeForwardAskEnded), and
// while the row is queued no second one is written. It answers the pull request as its row alone
// (record.Store.PullRequestRow), which no caller writes back, nil where it sent nothing. Launch 0
// is no launch: a claim that failed or retired, or one the daemon holds none of, is asked at the
// ready of its next launch. A claim that runs in the slot of a role deployment no longer defines
// (lostSlot) is asked nothing, as tellRole tells it nothing: its launch waits for a pod with that
// slot, which the deployment builds none of, and a row of the ask to it ends undelivered
// (leftMergeForwardAt), whose end would otherwise queue the next row to it, and so on without end.
// A pull request that is not open and a tree that lingers tell nobody. Its callers are a launch's
// ready (askAgainAtLaunch) and the move into merging (askMergerAtEntry), each with the claim of
// the worker on role's step (record.WorkerClaim) and the engine's deployment, and the end of a row
// of the ask (MergeForwardAskEnded), with the row's role, the claim the row reached and the
// executing daemon's deployment, which the daemon's executor makes without an engine, so it writes
// the row through store (queuedRow).
func askAgain(ctx context.Context, store record.Store, tx pgx.Tx, deployment plan.Deployment, issue record.Issue, role claim.Role, token claim.Token, launch uint64,
	now time.Time) (*record.PullRequest, error) {
	if launch == 0 || !record.InWorkflow(issue) || lostSlot(token, deployment) {
		return nil, nil
	}
	pr, err := store.PullRequestRow(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return nil, err
	}
	asked, owed := owedAsk(issue, *pr)
	if !owed || launch <= asked.SentLaunch {
		return nil, nil
	}
	if asked.Asked() != role {
		if owner, err := workerClaimOf(ctx, store, tx, issue, asked.Asked()); err != nil || owner != token {
			return nil, err
		}
	}
	if queued, err := store.QueuedMergeForwardAsk(ctx, tx, issue.Key, asked.Asked(), asked.Head); err != nil || queued {
		return nil, err
	}
	if lingers, err := record.TreeLingers(ctx, store, tx, issue.Tree); err != nil || lingers {
		return nil, err
	}
	ask := mergeForwardMessageOf(issue, *pr, asked.Asked(), token, record.MergeForwardAsk, asked.Why)
	ask.SentLaunch = launch
	row, err := queuedRow(ctx, tx, store, issue.Key, ask, now)
	if err != nil {
		return nil, err
	}
	if err := store.Enqueue(ctx, tx, row); err != nil {
		return nil, err
	}
	return pr, nil
}

// MergeForwardAskEnded settles the end of ask, a row of the merge-forward ask of its head to its
// role on issue key, which the daemon's executor deletes in tx under the fact lock tx holds
// (intake.SerializeFacts), so the end is ordered with every fact, the ready of each launch of the
// role's claim among them (askAgainAtLaunch). token is the claim the row reached (the row's own
// claim, else the worker on its role's step as the executor read it). delivered says whether the
// row was delivered; told is the launch of that claim the delivery reached, which the executor
// reads before the publish, and 0 for a row that ends undelivered; launch is the claim's launch as
// the executor reads it in tx, 0 where the claim has failed or retired or the daemon holds none;
// deployment is the roles of a step the executing daemon's settings define, as MergeForwardAt
// reads them, so a claim in the slot of a role it lost is asked nothing again (askAgain). It
// answers whether it sent the ask again.
//
// Where the record's ask is the row's own - of its head, naming its role, not landed - a delivered
// row records on it (record.MergeForward.SentLaunch) the latest of the launch it names, told, and
// the launch the row was sent at (record.MergeForwardMessage.SentLaunch), so the ready of told,
// which can come after the delivery, sends nothing again, and a row the listener named no holder of
// is still taken as told to the launch it was sent at, or the ask would be sent to that launch at
// every end. A row that ends undelivered records nothing: no launch is recorded before a row
// reaches it, so the record never names a launch nothing told and an end has nothing to take back.
// A newer ask, of another head or to another role, is not the row's own and is left as it is; one
// of the same head and role takes the row's delivery as its own, since the row tells the same merge
// of the same head.
//
// Then, delivered or not, the ask is sent again at launch (askAgain): a launch whose ready came
// while the row was still queued sent nothing and left the ask to the row, which reached an earlier
// launch or none. So each launch is told the ask once, whichever of the row's end and the launch's
// ready comes first. told is a launch whose own session the listener stored the message for, read
// before the publish, and 0 where the listener named no holder: the executor publishes the ask
// only once the launch it reads has registered its session, and does not end the row as delivered
// where the listener stored it for another session (the daemon's deliverHeld), so a copy an
// earlier launch's session took credits no later launch, and a launch that came up and took the
// publish between that read and the publish is told the ask again here. No launch is left untold.
func MergeForwardAskEnded(ctx context.Context, store record.Store, tx pgx.Tx, deployment plan.Deployment, key string, ask record.MergeForwardMessage, token claim.Token,
	delivered bool, told, launch uint64, now time.Time) (bool, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return false, err
	}
	if err := recordAskEnd(ctx, store, tx, key, ask, delivered, told); err != nil {
		return false, err
	}
	pr, err := askAgain(ctx, store, tx, deployment, *issue, ask.Role, token, launch, now)
	return pr != nil, err
}

// recordAskEnd records on the record's ask of issue key the launch a delivered row of ask reached,
// where that ask is the row's own - of its head, naming its role, not landed (MergeForwardAskEnded):
// the latest of the launch the ask names, told, and the launch the row was sent at. A row that
// ends undelivered, and a delivery that names no later launch, write nothing.
func recordAskEnd(ctx context.Context, store record.Store, tx pgx.Tx, key string, ask record.MergeForwardMessage, delivered bool, told uint64) error {
	if !delivered || max(told, ask.SentLaunch) == 0 {
		return nil
	}
	pr, err := store.PullRequest(ctx, tx, key)
	if err != nil || pr == nil || pr.MergeForward == nil {
		return err
	}
	own := *pr.MergeForward
	if own.Head != ask.Head || own.Landed != "" || own.Asked() != ask.Role {
		return nil
	}
	sent := max(own.SentLaunch, told, ask.SentLaunch)
	if sent == own.SentLaunch {
		return nil
	}
	own.SentLaunch = sent
	pr.MergeForward = &own
	return store.PutPullRequest(ctx, tx, *pr)
}

// owedAskClause is the clause the notice of role's failed claim names the merge owed to role with
// (claimFailed): the merge of the head, owed to role, which is asked again at the ready of role's
// next launch (askAgainAtLaunch), and the worker held for it, which stays held, since the head
// still conflicts and the merge is still owed; "" where no merge is owed to role. So the
// architect, who can start the role again, is told once, in that notice. Where the claim of the
// worker held is role's own, one agent on both steps as the steps of one key make it, nobody is
// held (holdStartedWorker), and the clause says the merge is that agent's own to make
// (dispatch://INFRA-4112).
func (e *Engine) owedAskClause(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role) (string, error) {
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return "", err
	}
	asked, owed := owedAsk(issue, *pr)
	if !owed || asked.Asked() != role {
		return "", nil
	}
	clause := fmt.Sprintf(". The merge of %s forward onto head %s is owed to the %s, and its ask is sent again when the %s starts again",
		asked.Base, asked.Head, role, role)
	if worker, held := heldWorker(issue, *pr); held && !lostRole(worker, e.cfg.Deployment) {
		one, err := e.oneAgent(ctx, tx, issue, worker, role)
		if err != nil {
			return "", err
		}
		if one {
			clause += fmt.Sprintf("; the %s's step is that agent's too, so the merge is its own to make and no worker is held for it", worker)
		} else {
			clause += fmt.Sprintf("; the %s is held for it", worker)
		}
	}
	return clause, nil
}

// tellRole queues the merge-forward message of step to role about the record's ask of pr's head
// (mergeForwardMessageOf) to the claim of the worker on role's step (sendToWorker).
func (e *Engine) tellRole(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, role claim.Role, step record.MergeForwardStep, why record.ConflictWhy) error {
	_, err := e.sendToWorker(ctx, tx, issue, mergeForwardMessageOf(issue, *pr, role, "", step, why))
	return err
}

// sendToWorker queues message, a merge-forward message to its role on issue, to the claim of the
// worker on that role's step (addressRoles), naming that claim (record.MergeForwardMessage.Claim),
// and to no claim whose slot is a role the deployment lost, as in mergeForward. It answers whether
// it queued a row: what tellRole and mergeForwardLanded send each message through.
func (e *Engine) sendToWorker(ctx context.Context, tx pgx.Tx, issue record.Issue, message record.MergeForwardMessage) (bool, error) {
	addressed, err := e.addressRoles(ctx, tx, issue.Project, []roleRecipient{{issue: issue.Key, role: message.Role, row: issue.Key}}, nil)
	if err != nil {
		return false, err
	}
	sent := false
	for _, to := range addressed {
		if lostSlot(to.token, e.cfg.Deployment) {
			continue
		}
		message.Claim = to.token
		if err := e.enqueue(ctx, tx, to.issue, message); err != nil {
			return false, err
		}
		sent = true
	}
	return sent, nil
}

// mergeForwardMessageOf is the merge-forward message of step to role about the record's ask of pr's
// head, for the claim token, naming the phase issue stands at and why: what tellRole queues, and
// askAgain.
func mergeForwardMessageOf(issue record.Issue, pr record.PullRequest, role claim.Role, token claim.Token, step record.MergeForwardStep, why record.ConflictWhy) record.MergeForwardMessage {
	return record.MergeForwardMessage{Role: role, Claim: token, Step: step, Head: pr.MergeForward.Head, Base: pr.MergeForward.Base,
		Phase: record.StandsAt(issue), PullRequest: pr.Number, Why: why}
}

// holdStartedWorker holds the worker that has just started in the phase issue stands at, where
// the merge of the head pr stands at is owed to another role (heldWorker). It is the one rule of
// every start of a phase's worker: a move's (mergeForwardOnEntry), the
// architect's retry of a held phase and the operator's restart of a phase's worker (retryPhase),
// and every other way the daemon brings that worker back to its phase's work (holdSentWorker), whose
// worker the first hold may never have reached, since a hold to a claim that failed ends
// undelivered, or reached in a session the new launch does not keep. The hold
// (record.MergeForwardHold) names the phase the issue stands at and has the worker stop writing
// the workspace, merge nothing where it is the merger, and wait for the landed message, which
// mergeForwardLanded sends it as the worker of that phase. The record's ask is left as it is. A
// tree that lingers tells nobody, and neither does a step whose role the engine's deployment no
// longer defines (lostRole), as mergeForwardWorker tells it nothing. Nor is a worker held whose
// claim is the asked role's, one agent on both steps as the steps of one key make it
// (dispatch://INFRA-4112 R5): the ask covers the hold, which would tell that agent to wait for a
// merge that is its own to make, as mergeForward names that claim once, with the ask
// (addressRoles), and the ready of each launch of that claim asks it again (askAgainAtLaunch).
func (e *Engine) holdStartedWorker(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	if pr == nil {
		return nil
	}
	worker, held := heldWorker(issue, *pr)
	if !held || lostRole(worker, e.cfg.Deployment) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	asked := pr.MergeForward
	if one, err := e.oneAgent(ctx, tx, issue, worker, asked.Asked()); err != nil || one {
		return err
	}
	if err := e.tellRole(ctx, tx, issue, pr, worker, record.MergeForwardHold, asked.Why); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a worker waits for a merge forward another role was asked", "issue", issue.Key, "tree", issue.Tree,
		"role", worker, "phase", record.StandsAt(issue), "pullRequest", pr.Number, "head", asked.Head, "asked", asked.Asked(), "askedIn", asked.Phase)
	return nil
}

// oneAgent says whether one agent works the steps of the roles a and b of issue now: the claim of
// the worker on each one's step (record.WorkerClaim, over one read of the issue's phases rows) is
// one claim, as the steps of one key make it.
func (e *Engine) oneAgent(ctx context.Context, tx pgx.Tx, issue record.Issue, a, b claim.Role) (bool, error) {
	project, rows, err := e.workerRows(ctx, tx, issue)
	if err != nil {
		return false, err
	}
	first, err := record.WorkerClaim(project, issue.Key, a, rows)
	if err != nil {
		return false, err
	}
	second, err := record.WorkerClaim(project, issue.Key, b, rows)
	return err == nil && first == second, err
}

// clearMergeForward drops the merge forward the record holds for issue's pull request
// (record.PullRequest.MergeForward), on a move into implementing (mergeForwardOnEntry) or into
// awaiting_children (awaitChildren) and when the issue leaves the workflow (leave): the ask, its
// landing and the review it asked for are of a stretch of work that is over, and a later conflict
// is asked about afresh. A record that holds none is not written. It answers the pull request as
// the record held it before the clear, nil where it held no merge forward, so the move into
// awaiting_children reads the dropped ask by the one rule every reader of the record's ask reads
// (openAsk).
func (e *Engine) clearMergeForward(ctx context.Context, tx pgx.Tx, issue string) (*record.PullRequest, error) {
	pr, err := e.store.PullRequest(ctx, tx, issue)
	if err != nil || pr == nil || pr.MergeForward == nil {
		return nil, err
	}
	before := *pr
	pr.MergeForward = nil
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return nil, err
	}
	return &before, nil
}

// mergeForwardLanded records that a merge forward landed (dispatch://INFRA-4374 R1): pr's head,
// just moved by a synchronize (pullRequestSynchronized), is the first head off the one the merge
// forward was asked of. It runs in whatever phase issue stands in, and once for each ask: the
// record keeps the new head as the one the merge left (record.MergeForward.Landed), which is a
// new value rather than a change of the one the record holds, since a copy of the pull request
// shares it.
//
// The worker of the phase the issue stands in, when the landed reaches it - the planner, the
// tester, the reviewer, the merger or, at step, the role of the running step (mergeForwardWorker,
// record.MergeForwardWorker) - is told the old head and the new one and to read what the merge
// changed before it goes on (record.MergeForwardLanded). An issue whose task's reviewer step
// already ran - that step stands before the place the issue stands at on the walk's line
// (Walk.passedStep) - stands on a review of the old head, so the reviewer is asked to read the
// merge and approve the new head or request changes (record.MergeForwardReview), and the record
// keeps that the review was asked for (ReviewAsked) where a row of that review is queued, which
// the merger's task (task) and the landed the merger is told in merging
// (record.MergeForwardMessage.Message) read: a reviewer whose claim runs in the slot of a role the
// deployment lost is asked nothing, and no merger waits for an approval nobody was asked for. In
// the default steps that is an issue in accepting, retro or merging. At step the review names the
// role of the running step, the worker the landed reaches, which it tells beside the architect
// (record.MergeForwardMessage.Worker), since no phase names that role. A reviewer step still to
// come reviews the head the issue stands at when it runs, so it is asked nothing now. A task whose
// plan has no reviewer step stands on no review of the old head, no reviewer step runs for it, and
// a merger told to wait for a reviewer's approval would wait for good, so nothing is asked and the
// merger goes on at the new head.
//
// The landed and the review say different things: the landed tells the worker to go on with its
// step at the new head, and the review asks for an approval or a request for changes, with no
// completion. Neither covers the other, so each is addressed on its own (addressRoles, which
// names each claim once within one message's recipients), and one agent that works the worker's
// step and the reviewer's, as the steps of one key make it, gets both (dispatch://INFRA-4112 R5).
// The reviewer that is itself the worker the landed reaches is told the review alone, which names
// the new head as the landed does.
//
// The daemon records no decision outside reviewing, so that review is the roles' to hold to the
// head they merge. The architect and the role asked are told nothing more: the architect's
// checks-red notice already said the merge was asked for, and the role asked pushed it; in merging
// that role is the merger, which the landed reaches as the worker of merging, since it tells it to
// merge the new head only once the reviewer's approval of it is on the pull request, where the
// task has a reviewer step, and to go on at the new head where it has none. A tree that
// lingers (record.TreeLingers, where every role is suspended) records and tells nothing.
func (e *Engine) mergeForwardLanded(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	asked := pr.MergeForward
	if asked == nil || asked.Landed != "" || asked.Head == pr.HeadSHA {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	at := record.StandsAt(issue)
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	review := w.passedStep(at, phase.Reviewing)
	landing := record.MergeForwardMessage{Head: asked.Head, Landed: pr.HeadSHA, Base: asked.Base, Phase: at, PullRequest: pr.Number, Why: asked.Why}
	messages := make([]record.MergeForwardMessage, 0, 2)
	worker, told := e.mergeForwardWorker(issue, at, record.MergeForwardLanded)
	// The reviewer as the worker is told the review alone, which covers the landed.
	if told && owedTo(issue, *asked) && (!review || worker != claim.RoleReviewer) {
		toWorker := landing
		toWorker.Role, toWorker.Step = worker, record.MergeForwardLanded
		messages = append(messages, toWorker)
	}
	if review {
		toReviewer := landing
		toReviewer.Role, toReviewer.Step = claim.RoleReviewer, record.MergeForwardReview
		if at == phase.Step {
			toReviewer.Worker = worker
		}
		messages = append(messages, toReviewer)
	}
	reviewAsked := false
	for _, message := range messages {
		sent, err := e.sendToWorker(ctx, tx, issue, message)
		if err != nil {
			return err
		}
		reviewAsked = reviewAsked || (sent && message.Step == record.MergeForwardReview)
	}
	landed := *asked
	landed.Landed, landed.ReviewAsked = pr.HeadSHA, reviewAsked
	pr.MergeForward = &landed
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: the merge forward landed", "issue", issue.Key, "tree", issue.Tree, "phase", at,
		"pullRequest", pr.Number, "head", asked.Head, "landed", pr.HeadSHA)
	return nil
}

// mergeForwardWorker is the worker of the phase issue stands at (at) that a merge forward's
// message of step reaches (record.MergeForwardWorker: at step, the role of the running step), and
// false where none does: a phase whose role is told nothing, and a step whose role the engine's
// deployment no longer defines (lostRole). Such a role's task is stopped (stopLostRole) and starts
// again only once the role is defined again, so a hold or a landed message to it is no work of the
// issue's, and a message held for its released claim would bring that claim back (the daemon's
// deliverHeld). The messages a merge forward sends the step's role all read it: its hold when the
// ask is made (mergeForward) and at each start (holdSentWorker, and holdStartedWorker through the
// same lostRole), and its landed (mergeForwardLanded); a row of any of them queued before a
// restart that dropped the role is ended by leftMergeForwardAt.
func (e *Engine) mergeForwardWorker(issue record.Issue, at phase.Phase, step record.MergeForwardStep) (claim.Role, bool) {
	worker, reaches := record.MergeForwardWorker(step, issue, at)
	if !reaches || lostRole(worker, e.cfg.Deployment) {
		return "", false
	}
	return worker, true
}

// lostRole says whether role is a role of the deployment that deployment no longer defines: one
// a daemon restarted without, whose task is stopped (stopLostRole) and whose claim no message of a
// merge forward reaches. Legion's own roles are never lost.
func lostRole(role claim.Role, deployment plan.Deployment) bool {
	return claim.IsDeploymentRole(role) && !deployment.Defines(string(role))
}

// lostAgent says whether name, the name of a step's agent (record.PlanStep.AgentName), is a role
// deployment lost (lostRole): the agent runs in the slot of its name, the role of its first step
// (record.NameAgents), so a pod of the deployment has no slot for it, whatever role its step has
// now. A step of the key owner has no name, and is never lost this way.
func lostAgent(name claim.Agent, deployment plan.Deployment) bool {
	return name != "" && lostRole(claim.Role(name), deployment)
}

// lostSlot says whether token's claim runs in a slot (claim.Token.Slot) that is a role deployment
// lost (lostAgent): its launch waits for a pod with that slot, which the deployment builds none of.
// A token that names no slot is not lost this way.
func lostSlot(token claim.Token, deployment plan.Deployment) bool {
	slot, ok := token.Slot()
	return ok && lostAgent(slot, deployment)
}

// MergeForwardAt reads in tx what a merge-forward message (record.MergeForwardMessage) of issue
// key to role, for the claim token where the row names one, is judged by: the issue, whether its
// tree lingers (record.TreeLingers) and the pull
// request the record holds for it. It returns the issue and that pull request, with why a message
// of step to role about the merge forward asked of head is no longer to be delivered
// (leftMergeForwardAt), "" while it is; deployment is the roles of a step the reading daemon's
// settings define, which its engine reads too. An issue the record does not hold is a nil issue,
// no pull request and no reason. The daemon's executor is its one caller: it reads the rule in one
// transaction at each attempt of a row, just before it delivers (the daemon's mergeForwardMessage),
// so the executor judges each queued row by this rule, and no row outlives the state it was written
// for. The workflow calls neither this nor leftMergeForwardAt: it writes these rows only in states
// the rule accepts. The rule is kept beside mergeForward and mergeForwardLanded, which write the
// rows, mergeForwardWorker, which picks the worker they tell, and clearMergeForward, which drops
// the ask they are about, since it restates their states for a row already queued. It is exported
// for that executor. The pull request is its row alone (record.Store.PullRequestRow): the executor
// names its repository and writes nothing of it.
func MergeForwardAt(ctx context.Context, store record.Store, tx pgx.Tx, deployment plan.Deployment, key string, role claim.Role, token claim.Token, step record.MergeForwardStep, head string) (*record.Issue, *record.PullRequest, string, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, nil, "", err
	}
	lingers, err := record.TreeLingers(ctx, store, tx, issue.Tree)
	if err != nil {
		return nil, nil, "", err
	}
	pr, err := store.PullRequestRow(ctx, tx, key)
	if err != nil {
		return nil, nil, "", err
	}
	return issue, pr, leftMergeForwardAt(*issue, lingers, pr, deployment, role, token, step, head), nil
}

// leftMergeForwardAt says why a merge-forward message of step to role, for the claim token, about
// the merge forward asked of head is no longer to be delivered, or "" while it is: the issue is in
// the workflow, its tree does not linger (lingers, record.TreeLingers), neither role nor the slot
// token runs in (lostSlot) is a role of the deployment that deployment no longer defines
// (lostRole), and the pull request the record holds for it (pr, nil
// for none) is open and holds a merge forward of head (record.PullRequest.MergeForward) that, for
// an ask or a hold, has not landed. In each other state the message would tell its role of work
// nobody wants any more. The roles of a tree that lingers are suspended. A role the deployment no
// longer defines is one a restart dropped after the row was queued: mergeForwardWorker writes no
// row to it, the boot stopped its task (stopLostRoles), and a delivery would wake its released
// claim, whose launch then fails for want of the role; a claim whose slot is such a role is one a
// keyed agent named for it runs in, whatever its step's role, whose launch waits for a pod with
// that slot, which the deployment builds none of. The workflow drops the ask on a move into
// implementing or awaiting_children and when the issue leaves the workflow (clearMergeForward),
// and a merge forward of another head is a newer ask, which has its own messages. A pull request
// that is not open ends every step: mergeForward asks nothing for one, and once it closed or
// merged nothing is left to merge forward, to hold for or to review. The ask and the hold end at
// the landing (record.MergeForward.Landed): role-message rows run in no order of the issue's, so a
// hold still queued would reach the worker after its landed and tell it to wait for a head that
// already came, and an ask would ask for a merge already pushed. A message to the merger ends when
// the issue no longer stands in merging: a backward move gives the workspace to an earlier phase,
// and the merger must not be woken to write or merge there. An ask and a hold end once the merge
// of their head is owed to nobody where the issue stands (owedTo), the one rule the hold at a
// start, the fence and the landed message read too, and an ask ends once the record's ask of its
// head names another role (record.MergeForward.Asked): one merge of one head has one asked role at
// a time, and the merger asked in merging ends an implementer's ask of the same head made in
// implementing, which the withdrawal tells it (record.MergeForwardWithdrawn). An ask to a claim
// that ended is the daemon's to end (its deliverHeld), and the ready of the role's next launch asks
// again (askAgainAtLaunch). The record keeps the ask and its earlier landings, so their completion
// fences and review still stand. The landed and the review are the landing's own messages, and they
// still go after it while their role applies, and so does the withdrawal while the record holds an
// ask of its head that the implementer does not owe again.
func leftMergeForwardAt(issue record.Issue, lingers bool, pr *record.PullRequest, deployment plan.Deployment, role claim.Role, token claim.Token, step record.MergeForwardStep, head string) string {
	switch {
	case !record.InWorkflow(issue):
		return fmt.Sprintf("the issue has left the workflow, at status %s in %s", issue.Status, issue.Phase)
	case lingers:
		return "the issue's tree lingers or has closed"
	case lostRole(role, deployment):
		return fmt.Sprintf("the deployment no longer defines the role %s", role)
	case lostSlot(token, deployment):
		slot, _ := token.Slot()
		return fmt.Sprintf("the deployment no longer defines the role %s, the slot of the claim %s", slot, token)
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
	case step == record.MergeForwardAsk && role != pr.MergeForward.Asked():
		return fmt.Sprintf("the merge forward of head %s is asked of the %s", head, pr.MergeForward.Asked())
	case (step == record.MergeForwardAsk || step == record.MergeForwardHold) && !owedTo(issue, *pr.MergeForward):
		return fmt.Sprintf("the merge of head %s is owed to no role while the issue stands at phase %s", head, record.StandsAt(issue))
	case step == record.MergeForwardWithdrawn && pr.MergeForward.Asked() == claim.RoleImplementer && owedTo(issue, *pr.MergeForward):
		return fmt.Sprintf("the implementer is asked for head %s again", head)
	}
	return ""
}

// mergeForwardReviewOwed says whether the review a landed merge forward asked of the reviewer
// stands for pr's head (mergeForwardLanded): it was asked, and the head is still the one the merge
// left. The merger's task reads it (task); the landed message reads ReviewAsked alone, since it
// names the head the merge left (record.MergeForwardMessage.Message).
func mergeForwardReviewOwed(pr record.PullRequest) bool {
	asked := pr.MergeForward
	return asked != nil && asked.ReviewAsked && asked.Landed == pr.HeadSHA
}

// mergedForwardRefusal is the completion fence of a merge forward (dispatch://INFRA-4374 R1): a
// completion of planning, testing, reviewing, step or retro whose commit is the head a merge
// forward was asked of, once that merge landed and the pull request stands at another head,
// reports work on code the issue no longer stands on, and is refused with
// HANDOFF_HEAD_MERGED_FORWARD before anything is
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
// that head has not landed and its hold told that role to wait (heldWorker) - the head an earlier
// merge left conflicts in its turn, its merge forward is asked in the phase the completion reports
// or before the move that started that phase, and the hold reaches the role that completes it
// (record.MergeForwardWorker) - since that hold tells the role not to complete at that head
// (record.MergeForwardHold). At step that role is the role of the running step
// (record.Issue.RoleAt), the one mergeForwardLanded tells at the landing, whichever role of the
// deployment the hold first reached. Then the refusal names the head the pull request stands at,
// says its merge forward is asked and has not landed, and tells the role to wait for the daemon's
// landed message (record.MergeForwardLanded) and do the phase's work at the head that message
// names (mergedForwardFence.Wait). That wait is told only to a role the ask's hold was sent to:
// every other completion is answered as above, at the head the pull request stands at, since no
// hold told its role not to complete there. The implementer, whose completion of retro is fenced,
// is sent no hold, the merge being its own to make; and an ask made in implementing sends none,
// and stands into the next phase when the move there does not ask again (mergeForwardOnEntry), as
// at a head the record no longer holds in a confirmed conflict. Nor is the role held whose worker
// is the agent the merge was asked of, one agent on both steps as the steps of one key make it
// (holdStartedWorker): the completion route reads the claims (mergedForwardRefusalOf) and tells it
// that merge is its own to make (mergedForwardFence.Own) in place of the wait.
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
func mergedForwardRefusal(issue record.Issue, role claim.Role, reported phase.Phase, commit string, pr *record.PullRequest) (mergedForwardFence, bool) {
	switch reported {
	case phase.Planning, phase.Testing, phase.Reviewing, phase.Step, phase.Retro:
	default:
		return mergedForwardFence{}, false
	}
	if pr == nil || pr.MergeForward == nil || commit == pr.HeadSHA {
		return mergedForwardFence{}, false
	}
	asked := pr.MergeForward
	fence := mergedForwardFence{Issue: issue.Key, Role: role, Reported: reported, Number: pr.Number, Head: pr.HeadSHA, Base: asked.Base}
	if commit == "" {
		fence.Landed = asked.Landed
		if fence.Landed == "" && len(asked.Earlier) > 0 {
			fence.Landed = asked.Earlier[len(asked.Earlier)-1].Landed
		}
		if fence.Landed == "" {
			return mergedForwardFence{}, false
		}
	} else {
		replaced, ok := mergedForwardOf(asked, commit)
		if !ok {
			return mergedForwardFence{}, false
		}
		fence.Replaced, fence.Landed = replaced.Head, replaced.Landed
	}
	held, holds := heldWorker(issue, *pr)
	fence.Wait = holds && held == role
	return fence, true
}

// mergedForwardRefusalOf is mergedForwardRefusal as the completion route judges it (handoff), with
// the claims read in tx: where the fence tells role to wait for the open merge of the head the
// pull request stands at (mergedForwardFence.Wait), and the claim of role's worker is the claim
// of the worker on the asked role's step, one agent on both steps as the steps of one key make
// it, that merge is the completing agent's own to make, and no hold told it to wait
// (holdStartedWorker holds no such worker): the fence tells it to make the merge
// (mergedForwardFence.Own) in place of the wait (dispatch://INFRA-4112).
func (e *Engine) mergedForwardRefusalOf(ctx context.Context, tx pgx.Tx, issue record.Issue, role claim.Role, reported phase.Phase, commit string,
	pr *record.PullRequest) (mergedForwardFence, bool, error) {
	fence, fenced := mergedForwardRefusal(issue, role, reported, commit, pr)
	if !fenced || !fence.Wait {
		return fence, fenced, nil
	}
	asked := pr.MergeForward.Asked()
	one, err := e.oneAgent(ctx, tx, issue, role, asked)
	if err != nil {
		return mergedForwardFence{}, false, err
	}
	if one {
		fence.Wait, fence.Own = false, asked
	}
	return fence, true, nil
}

// mergedForwardFence is a refusal of mergedForwardRefusal as the values its sentence is composed
// of (Message), so what the refusal tells its role is a value, not a reading of its words: the role
// whose completion is refused, which the refusal answers; the phase it reported; the pull request
// and the head it stands at; the base of the record's merge forward; the head the completion is
// at, which a merge forward replaced, "" for a completion that names none; the head that merge
// left, or the newest one that landed for a completion naming none, whose merge the role can read;
// and the instruction. Wait is the instruction to wait for the daemon's message that the open
// merge of Base onto Head landed and then do the phase's work at the new head that message names,
// told only to the role the ask's hold told to wait (heldWorker). Own is the role that open merge
// was asked of where the completing agent works that role's step too (mergedForwardRefusalOf),
// "" otherwise: that agent is told to make the merge itself and push it, and do the phase's work
// at the head it leaves. Otherwise the role is told to do the phase's work at Head now.
type mergedForwardFence struct {
	Issue    string
	Role     claim.Role
	Reported phase.Phase
	Number   int
	Head     string
	Base     string
	Replaced string
	Landed   string
	Wait     bool
	Own      claim.Role
}

// Message is the refusal's sentence: what the completion reports, the merge forward that replaced
// that head or the newest that landed, and the instruction Wait or Own names. A phase that ends
// with a handoff record is told to write it again, its `head` set to the head it completes at.
func (f mergedForwardFence) Message() string {
	var clause string
	if f.Replaced == "" {
		clause = fmt.Sprintf("names no head, so it says nothing about which head its work judged, and a merge of %s forward landed at head %s",
			f.Base, f.Landed)
	} else {
		clause = fmt.Sprintf("is at head %s, and a merge of %s forward replaced that head with head %s", f.Replaced, f.Base, f.Landed)
	}
	again := "complete again at that head"
	if word, _, ok := phase.Handoff(f.Reported); ok {
		again = fmt.Sprintf("write this phase's handoff again with `dispatch handoff-write --issue %s --phase %s --body-file <file>`, its `head` set to that head, and complete again naming it",
			f.Issue, word)
	}
	var next string
	switch {
	case f.Wait:
		next = fmt.Sprintf("Pull request #%d stands at head %s now, and the merge of %s forward onto head %s is asked and has not landed. Wait for the daemon's message that this merge landed, do the phase's work at the new head that message names, then %s; this completion changed nothing",
			f.Number, f.Head, f.Base, f.Head, again)
	case f.Own != "":
		next = fmt.Sprintf("Pull request #%d stands at head %s now, and the merge of %s forward onto head %s is asked of you, as the %s, and has not landed: the merge is yours to make. Merge %s forward and push it, do the phase's work at the head that merge leaves, then %s; this completion changed nothing",
			f.Number, f.Head, f.Base, f.Head, f.Own, f.Base, again)
	default:
		next = fmt.Sprintf("Pull request #%d stands at head %s now. Read what the merge changed with `git show --remerge-diff %s`, do the phase's work at head %s, then %s; this completion changed nothing",
			f.Number, f.Head, f.Landed, f.Head, again)
	}
	return fmt.Sprintf("the %s's completion of phase %s of %s %s. %s", f.Role, f.Reported, f.Issue, clause, next)
}

// Result is the refusal as the completion route answers it: HANDOFF_HEAD_MERGED_FORWARD with
// Message.
func (f mergedForwardFence) Result() intake.Result {
	return refused("HANDOFF_HEAD_MERGED_FORWARD", f.Message())
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
