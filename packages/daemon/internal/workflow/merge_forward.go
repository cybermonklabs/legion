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
// its review takes no admission slot for the merge, and the role asked is asked, on its claim's
// topic, to merge the base forward onto the head and push (record.MergeForwardAsk): before merging
// the agent of the task's step that does its implementation work where it stands
// (implementationAgent: the task's last step of the type implementation before the place the issue
// stands at, whatever its role is called; the owner, where that step's key is owner; the
// implementer, for a run with no plan rows), and in merging the merger, whose merge it is
// (record.MergeForwardAsker, cybermonklabs/legion#4979). The record keeps the role asked
// (record.MergeForward.Role) and the claim the ask reached (record.MergeForward.Agent), which says
// whose ask it is; the merger is asked by its role, and its ask keeps no claim. Away from the
// implementation step it also tells the worker of the step the task stands at to stop writing the
// workspace the roles share while the merge runs (record.MergeForwardHold), when that worker is the
// tester, the reviewer, the agent of an acceptance step (a fresh agent of acceptance, or the owner
// at `acceptance:owner`) or, at step, the role of the running step, a role the deployment or the
// plan defines (mergeForwardHold, record.MergeForwardReaches), unless that worker's role is the
// role asked. That worker is addressed as the step's worker (record.Issue.StepWorker), so the owner
// at its own step is told by its own claim, as a fresh worker of that step is by its claim. Where
// that worker's claim is the claim asked, it gets the ask alone, which covers the hold and says
// what the hold asked (tellAsk, addressMergeForward). The architect that owns the issue gets one
// checks-red notice whose phase is the phase the issue stays in and whose reason names the head,
// the base, why, the role asked and that the issue stays in its phase; the outbox routes that
// notice to the owning architect, as every notice of the issue but an owner-step is routed. At that
// step itself (Walk.atImplementation: implementing on today's steps) only the ask goes, and the
// record marks it so (record.MergeForward.Own): the agent's own assignment and its completion
// stand, and the architect is told nothing, since the work the conflict needs is already where the
// task stands.
//
// It asks once for each head: the record keeps the ask (record.PullRequest.MergeForward), and a
// later confirmed read, or a move into another phase, at the head it names sends nothing, so the
// role first asked of a head keeps that merge until it lands and two roles are never sent to merge
// one base forward. The one exception is an ask made at the implementation step, which covers that
// step alone: once the task stands past that step at that head - its agent completed there without
// the merge - the step it stands at asks again, with its hold and its notice, where a step of the
// task still runs there before the merge, and the move into merging asks the merger where none does
// (mergeForwardOnEntry). An ask of the implementation step's agent made away from that step that
// has not landed when the issue reaches merging stays that agent's, and the merger is held instead
// (mergeForwardOnEntry).
//
// The ask replaces the one the record holds, and keeps what that one kept of the merge forwards
// that landed before it in the same stretch of work (record.MergeForward.Earlier), with that ask
// itself added once it landed: when the head a merge left conflicts in its turn and is merged
// forward again, a completion at the head the first merge replaced is still refused
// (mergedForwardRefusal). That holds for an ask of a new head and for the same head asked again
// on a phase entry alike.
//
// Each claim gets one message (addressMergeForward): the ask where it is both the agent asked and
// the worker held. A pull request that is not open, an issue where no merge forward stands at the
// place its task stands at on w's line (mergeForwardStands, classify.MergeForwardStands: the task's
// implementation step or a place after it, and any place once the task's run has reached that step,
// read by the place, since every step of a role the deployment or the plan defines runs as step),
// and a tree that lingers (record.TreeLingers, where every role is suspended) are asked nothing.
// Nor is an issue whose claim of the role asked runs in the slot of a role the deployment no longer
// defines (lostSlot): nobody can make the merge, so no role is told, the architect is not told
// that role is asked, and the record keeps no ask.
func (e *Engine) mergeForward(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, pr *record.PullRequest, why record.ConflictWhy) error {
	at := record.StandsAt(issue)
	place, _ := w.placeIn(at)
	if pr.State != record.PullRequestOpen {
		return nil
	}
	if start, reached, err := e.mergeForwardStands(ctx, tx, issue, w, place); err != nil || !classify.MergeForwardStands(start, place, reached) {
		return err
	}
	// An ask the record holds of this head covers the head wherever the task stands, but an ask made
	// at the implementation step (record.MergeForward.Own), which covers that step alone.
	own := w.atImplementation(at)
	if made := pr.MergeForward; made != nil && made.Head == pr.HeadSHA && (!made.Own || own) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	// The role asked: the merger in merging (record.MergeForwardAsker), by its role, and before
	// merging the agent of the task's implementation step, on the claim its step's start names
	// (implementationAgent), the owner's where that step's key is owner. A task with no step of that
	// type has no agent to ask: the record keeps no Agent and no Role then (the zero recipient), and
	// tellAsk asks nobody.
	asked, named := roleRecipient{issue: issue.Key, role: claim.RoleMerger, row: issue.Key}, true
	if record.MergeForwardAsker(at) != claim.RoleMerger {
		var err error
		if asked, named, err = e.implementationAgent(ctx, tx, issue, w, at); err != nil {
			return err
		}
	}
	next := &record.MergeForward{Head: pr.HeadSHA, Base: pr.Base, Phase: at, Own: own, Agent: asked.claim, Why: why, AskedAt: e.now()}
	if named {
		next.Role = asked.role
	}
	if before := pr.MergeForward; before != nil {
		next.Earlier = before.Earlier
		if before.Landed != "" {
			// Clipped, so the append writes a new array: a copy of the pull request shares the old one.
			next.Earlier = append(slices.Clip(before.Earlier), record.LandedMergeForward{Head: before.Head, Landed: before.Landed})
		}
	}
	// The ask and the hold are queued before the record holds the ask: the write of the record opens
	// a merge forward asked of the owner, which asks the owner again (ownersMergeForwardWritten,
	// askOwnerAgain) unless an ask at that head is queued already, and this one is. Where the claim
	// of the role asked runs in the slot of a role the deployment lost (lostSlot), tellAsk queues
	// nothing and says so, and nothing more is done: no notice, and no ask on the record.
	asking, err := e.tellAsk(ctx, tx, issue, *next, pr.Number, w.deployment(e.cfg.Deployment))
	if err != nil || !asking {
		return err
	}
	if !own {
		// A held issue stands in held, not in the phase its hold came from (StandsAt), such as an
		// issue held from testing whose conflict a read confirms (mergeability).
		stays := "stays in " + string(at)
		if issue.Phase == phase.Held {
			stays = "stays held from " + string(at)
		}
		who := "agent of the task's implementation step"
		if named {
			who = string(asked.role)
		}
		reason := fmt.Sprintf("the head %s conflicts with %s (%s): the %s is asked to merge %s forward, and the issue %s",
			pr.HeadSHA, pr.Base, why, who, pr.Base, stays)
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "checks-red", Role: claim.RoleArchitect, Phase: at, Reason: reason}); err != nil {
			return err
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

// tellAsk queues the messages mergeForward's ask asked (a record.MergeForward of issue's pull
// request numbered pull) sends: the ask, built by the one builder of it (record.MergeForward.Ask),
// to the claim it reached (asked.Agent), or, for the merger, to the merger by its role, as the role
// it was made as (record.MergeForward.Role, "" where no task step does that work, and then nobody
// is asked); and the hold to the worker of the step the task stands at (mergeForwardHold), away
// from the implementation step, where d, the deployment as the issue's walk reads it
// (Walk.deployment), still defines that worker's role (lostRole). Each claim gets one message
// (addressMergeForward): where the agent asked works the step the issue stands at, it gets the ask
// alone, which covers the hold. The ask again of the owner (askOwnerAgain) sends the ask alone, its
// hold sent with the first ask, and builds it itself.
//
// The claim of a keyed agent runs in the slot of its name, which can be a role the deployment lost
// while its step's role is not (lostSlot): its task is stopped as the lost role's is, and a message
// to it would wake a claim no pod can launch. Where that claim is the one the ask reaches (the
// implementer's, or in merging the merger's), nobody can make the merge, so nothing is queued: no
// hold, which would keep the worker waiting for a merge nobody makes, and tellAsk answers false, so
// mergeForward sends no notice saying that role is asked and writes no ask on the record, and a
// later confirmed read of the head asks again once the role is defined again. Any other message to
// a claim in such a slot is left out. It answers true where the ask goes.
func (e *Engine) tellAsk(ctx context.Context, tx pgx.Tx, issue record.Issue, asked record.MergeForward, pull int, d plan.Deployment) (bool, error) {
	ask := asked.Ask(pull, "")
	var sends []mergeForwardSend
	if asked.Role != "" {
		sends = append(sends, mergeForwardSend{step: record.MergeForwardAsk, to: roleRecipient{issue: issue.Key, role: ask.Role, row: issue.Key, claim: asked.Agent}})
	}
	hold, held, err := mergeForwardHold(issue, asked)
	if err != nil {
		return false, err
	}
	if held && !lostRole(hold.role, d) {
		sends = append(sends, mergeForwardSend{step: record.MergeForwardHold, to: hold})
	}
	messages, err := e.addressMergeForward(ctx, tx, issue.Project, ask, sends, nil)
	if err != nil {
		return false, err
	}
	for _, message := range messages {
		if (message.Step == record.MergeForwardAsk || message.Covers == record.MergeForwardAsk) && lostSlot(message.Claim, d) {
			slot, _ := message.Claim.Slot()
			e.logOnCommit(ctx, "workflow: no merge forward is asked: the claim of the role asked runs in the slot of a role the deployment no longer defines",
				"issue", issue.Key, "pullRequest", pull, "head", asked.Head, "role", ask.Role, "claim", message.Claim, "slot", slot)
			return false, nil
		}
	}
	for _, message := range messages {
		if lostSlot(message.Claim, d) {
			continue
		}
		if err := e.enqueue(ctx, tx, issue.Key, message); err != nil {
			return false, err
		}
	}
	return true, nil
}

// mergeForwardHold is the recipient of the hold the ask asked sends where issue stands
// (record.StandsAt): the worker of the step the task stands at, addressed as that step's worker
// (record.MergeForwardWorker: the owner's claim at a place the owner works, and elsewhere the claim
// the role's phases row names), and false where no hold goes: at the implementation step
// (asked.Own), where the hold does not reach that worker, and where that worker's role is the role
// asked (record.MergeForward.Role), as the merger is in merging. tellAsk sends it, askOwnerAgain
// reads it for the ask's cover, and mergedForwardRefusal for whether the hold told the claim that
// completes to wait for the landing.
func mergeForwardHold(issue record.Issue, asked record.MergeForward) (roleRecipient, bool, error) {
	if asked.Own {
		return roleRecipient{}, false, nil
	}
	worker, token, reaches, err := record.MergeForwardWorker(record.MergeForwardHold, issue, record.StandsAt(issue))
	if err != nil || !reaches || worker == asked.Role {
		return roleRecipient{}, false, err
	}
	return roleRecipient{issue: issue.Key, role: worker, row: issue.Key, claim: token}, true, nil
}

// mergeForwardSend is one message one event of a merge forward sends: its step and the recipient
// it is addressed to.
type mergeForwardSend struct {
	step record.MergeForwardStep
	to   roleRecipient
}

// addressMergeForward addresses the messages one event of a merge forward sends (sends), each as
// every role message of the daemon is addressed (addressRoles), and completes each from template,
// the event's message with its heads, base, phase, pull request and why: its role, claim and step
// are its send's. It gives each claim one message for the event: of two messages whose recipients
// one claim holds and of which one covers the other (record.MergeForwardCovers), the claim gets the
// covering one alone, whichever of the two sends names first, and it carries in Covers the step of
// the one it stands in for, so the agent gets each thing it is asked for, once. A pair no cover
// joins is sent whole. A message skip refuses for its claim is not sent and is covered by none: the
// landed, which is never asked of the agent that pushed the merge (mergeForwardLanded). It answers
// the messages in the order of sends, a covering message in the place of the first of its two, for
// its sender to queue.
func (e *Engine) addressMergeForward(ctx context.Context, tx pgx.Tx, project string, template record.MergeForwardMessage,
	sends []mergeForwardSend, skip func(record.MergeForwardStep, claim.Token) bool) ([]record.MergeForwardMessage, error) {
	var messages []record.MergeForwardMessage
	for _, send := range sends {
		addressed, err := e.addressRoles(ctx, tx, project, []roleRecipient{send.to}, nil)
		if err != nil {
			return nil, err
		}
		if len(addressed) == 0 {
			continue
		}
		to := addressed[0]
		if skip != nil && skip(send.step, to.token) {
			continue
		}
		message := template
		message.Role, message.Claim, message.Step, message.Covers = to.role, to.token, send.step, ""
		i := slices.IndexFunc(messages, func(m record.MergeForwardMessage) bool {
			return m.Claim == to.token && (record.MergeForwardCovers(m.Step, send.step) || record.MergeForwardCovers(send.step, m.Step))
		})
		switch {
		case i < 0:
			messages = append(messages, message)
		case record.MergeForwardCovers(messages[i].Step, send.step):
			messages[i].Covers = send.step
		default:
			message.Covers = messages[i].Step
			messages[i] = message
		}
	}
	return messages, nil
}

// mergeForwardOnEntry is what a move does with the merge forward of the issue's pull request once
// the move has put issue in the phase it now stands in and written that phase's start
// (transition). It reads where the move put the issue, never the phase the move left. w is the
// walk of issue's run, as the move left it. The pull request is read again in the transaction,
// since the copy a transition is handed can be none, or one read before the transaction's other
// writes.
//
//   - A move into the implementation step (implementing on today's steps) drops the record's merge
//     forward (clearMergeForward): the agent of that step works the head there, and a head it
//     pushes is read for a conflict of its own.
//   - At a head the record holds in a confirmed conflict (classify.ConflictRound), a move into
//     merging asks the merger for the merge forward for record.ConflictMergeNext: the merge comes
//     next, GitHub merges no head that conflicts with its base, and the merge is the merger's
//     (cybermonklabs/legion#4979). This is where an approval of a conflicting head, which ends its
//     round (reviewRound), is acted on once every step of the task has run: a move to where the
//     merge comes next before merging (classify.MergeComesNext: retro, or accepting where no step
//     of the task follows the acceptance) asks nothing, since the conflict waits for the merger
//     (classify.ConflictNeedsMerge). Where the record still holds an ask of that head, made of
//     another role away from the implementation step and not landed, that role keeps the merge,
//     since it may already be at work on it, and the merger is held until it lands (holdMerger)
//     rather than asked. On a task where a step follows the review or the acceptance, a move into
//     that step asks nothing either: the conflict was decided by R0's reasons where it was
//     confirmed, and the step goes on at the head as a tester or a reviewer after the implementer
//     does.
//   - At such a head, a move to a place past the task's implementation step on its line
//     (Walk.passedImplementation: a planning after the implementer too, a planning before it
//     never) where a step of the task still runs before the merge (not classify.MergeComesNext),
//     while the record's ask of that head is still the one made at the implementation step
//     (record.MergeForward.Own), asks again for the reason it was asked for
//     (record.MergeForward.Why): an ask made there covers that step alone, so an agent that
//     completes it at that head without the merge leaves the step the task moves to to ask, with
//     its hold and its notice. It is keyed on the ask, not on the phase the move left, so no such
//     step starts at that head with the ask of the implementation step alone. A move to where the
//     merge comes next asks nothing, as any move there at a conflicting head does: a plan with no
//     tester and no reviewer step moves out of implementing into its retro, or into an acceptance
//     no step follows, the ask of implementing stays on the record, and the move into merging asks
//     the merger. A parent held before its retro, and its release into it, ask nothing either and
//     make no call here (holdBeforeRetro, startHeldRetro).
//
// mergeForward asks once for each head, so a move into merging at a head the merger was already
// asked of asks nothing more.
func (e *Engine) mergeForwardOnEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) error {
	if w.atImplementation(issue.Phase) {
		return e.clearMergeForward(ctx, tx, issue.Key)
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil || !classify.ConflictRound(*pr) {
		return err
	}
	asked := pr.MergeForward
	switch {
	case record.StandsIn(issue, phase.Merging):
		if asked != nil && asked.Head == pr.HeadSHA && !asked.Own && asked.Asked() != claim.RoleMerger {
			return e.holdMerger(ctx, tx, issue, pr)
		}
		return e.mergeForward(ctx, tx, issue, w, pr, record.ConflictMergeNext)
	case asked != nil && asked.Head == pr.HeadSHA && asked.Own &&
		w.passedImplementation(record.StandsAt(issue)) && !classify.MergeComesNext(issue, w.Line):
		return e.mergeForward(ctx, tx, issue, w, pr, asked.Why)
	}
	return nil
}

// holdMerger tells the merger of an issue that has just entered merging that another role's merge
// forward of the head stands and has not landed (mergeForwardOnEntry): the hold
// (record.MergeForwardHold) has it merge nothing and wait for the landed message, which
// mergeForwardLanded sends it as the worker of merging, with the reviewer's review of the new head.
// The record's ask is left as it is. A tree that lingers tells nobody.
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
		// As in mergeForward: no message to a claim whose slot is a role the deployment lost.
		if lostSlot(to.token, e.cfg.Deployment) {
			continue
		}
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: claim.RoleMerger, Claim: to.token, Step: record.MergeForwardHold,
			Head: asked.Head, Base: asked.Base, Phase: phase.Merging, PullRequest: pr.Number, Why: asked.Why}); err != nil {
			return err
		}
	}
	e.logOnCommit(ctx, "workflow: the merger waits for a merge forward another role was asked", "issue", issue.Key, "tree", issue.Tree,
		"pullRequest", pr.Number, "head", asked.Head, "asked", asked.Asked(), "askedIn", asked.Phase)
	return nil
}

// clearMergeForward drops the merge forward the record holds for issue's pull request
// (record.PullRequest.MergeForward), on a move into the task's implementation step
// (mergeForwardOnEntry) and when the issue leaves the workflow (leave): the ask, its landing and
// the review it asked for are of a stretch of work that is over, and a later conflict is asked
// about afresh. A record that holds none is not written.
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
// The worker of the step the task stands at, when the landed reaches it - the tester, the
// reviewer, the agent of an acceptance step (a fresh agent of acceptance, or the owner at
// `acceptance:owner`), the merger or, at step, the role of the running step, a role the
// deployment or the plan defines (mergeForwardWorker, record.MergeForwardReaches) - addressed as
// that step's worker (record.Issue.StepWorker), the owner at its own step among them, is told the
// old head and the new one and to read what the merge changed before it goes on
// (record.MergeForwardLanded), unless that worker's claim is the one the ask reached
// (record.MergeForward.Agent): that agent pushed the merge, at its own step or at another step its
// key works. A record of the release before keeps no agent, and nor does an ask of the merger,
// which is asked by its role; the agent skipped then is the one mergeForward asks before merging,
// the agent of the task's implementation step where the ask was made (implementationAgent at
// record.MergeForward.Phase), so that agent is told no landed either. An issue where a reviewer
// step of its task already ran - its first reviewer step stands before the place the issue stands
// at on the walk's line (Walk.passedStep) - stands on a review of the old head, so the reviewer is
// asked to read the merge and approve the new head or request changes (record.MergeForwardReview),
// whoever pushed the merge, since the merger waits for that approval. In the default steps that is
// an issue in accepting, retro or merging. A reviewer step still to come reviews the head the issue
// stands at when it runs, so it is asked nothing now. The review is addressed as the reviewer, to
// the claim of the agent of the task's reviewer step (workAgent): the owner's own claim where that
// step's key is owner (record.OwnerClaim), as every message to the worker of an owner's step is,
// and never the claim the reviewer's phases row names, which can be the architect above
// (ownerAgent), an architect that does not do the step. A task whose plan has no reviewer step
// stands on no review of the old head, no reviewer step runs for it, and a merger told to wait for
// a reviewer's approval would wait for good, so nothing is asked and the merger goes on at the new
// head. No message goes to a claim whose slot is a role the deployment lost (lostSlot).
//
// The review covers the landed (record.MergeForwardCovers, addressMergeForward), so a reviewer that
// is also the worker of the step the issue stands at, the owner or one keyed agent at both, gets the
// review alone, which also tells it to go on with its step at the new head. The record keeps that
// the review was asked (ReviewAsked) only where a review was queued, so the merger's task (task) and
// the landed the merger is told in merging (record.MergeForwardMessage.Message), which read it,
// never wait for an approval nobody was asked for.
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
	d := w.deployment(e.cfg.Deployment)
	var sends []mergeForwardSend
	if w.passedStep(at, phase.Reviewing) {
		// The review goes to the agent of the task's reviewer step (workAgent): the owner's claim where
		// that step's key is owner, and never the reviewer's row, which can name the architect above.
		reviewer, named, err := e.workAgent(ctx, tx, issue, w, at, claim.RoleReviewer)
		if err != nil {
			return err
		}
		if named {
			sends = append(sends, mergeForwardSend{step: record.MergeForwardReview,
				to: roleRecipient{issue: issue.Key, role: claim.RoleReviewer, row: issue.Key, claim: reviewer.claim}})
		}
	}
	worker, told, err := mergeForwardWorker(issue, at, record.MergeForwardLanded, d)
	if err != nil {
		return err
	}
	if told {
		sends = append(sends, mergeForwardSend{step: record.MergeForwardLanded, to: worker})
	}
	// The ask names no claim where it keeps none: a record of the release before, and an ask of the
	// merger, which is addressed by its role. The one the landed then skips as the agent that pushed
	// the merge is the agent mergeForward asks before merging where the ask was made, so the merger,
	// which the landed reaches as the worker of merging, is told the new head.
	pusher := asked.Agent
	if pusher == "" {
		agent, _, err := e.implementationAgent(ctx, tx, issue, w, asked.Phase)
		if err != nil {
			return err
		}
		pusher = agent.claim
	}
	// As in mergeForward: no message to a claim whose slot is a role the deployment lost.
	skip := func(step record.MergeForwardStep, to claim.Token) bool {
		return lostSlot(to, d) || step == record.MergeForwardLanded && pusher != "" && to == pusher
	}
	template := record.MergeForwardMessage{Head: asked.Head, Landed: pr.HeadSHA, Base: asked.Base, Phase: at, PullRequest: pr.Number, Why: asked.Why}
	messages, err := e.addressMergeForward(ctx, tx, issue.Project, template, sends, skip)
	if err != nil {
		return err
	}
	review := false
	for _, message := range messages {
		if err := e.enqueue(ctx, tx, issue.Key, message); err != nil {
			return err
		}
		review = review || message.Step == record.MergeForwardReview
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

// mergeForwardWorker is the worker of the step issue stands at (at) that a merge forward's message
// of step reaches (record.MergeForwardReaches), addressed as that step's worker
// (record.MergeForwardWorker: the owner's claim at a place the owner works, and elsewhere the claim
// the role's phases row names), and false where none does: a phase whose role is told nothing, and
// a step whose role d, the engine's deployment as the issue's walk reads it (Walk.deployment: with
// each role the plan in force defines), no longer defines (lostRole). Such a role's task is stopped
// (stopLostRole) and starts again only once the role is defined again, so a landed message to it is
// no work of the issue's, and a message held for its released claim would bring that claim back
// (the daemon's deliverHeld). The landed (mergeForwardLanded) reads it, and the hold (tellAsk) reads
// the same rule through mergeForwardHold; a row of either queued before a restart that dropped the
// role is ended by leftMergeForwardAt.
func mergeForwardWorker(issue record.Issue, at phase.Phase, step record.MergeForwardStep, d plan.Deployment) (roleRecipient, bool, error) {
	worker, token, reaches, err := record.MergeForwardWorker(step, issue, at)
	if err != nil || !reaches || lostRole(worker, d) {
		return roleRecipient{}, false, err
	}
	return roleRecipient{issue: issue.Key, role: worker, row: issue.Key, claim: token}, true, nil
}

// deploymentOf is d as issue reads it at its generation: d's roles and each role the plan of that
// generation defines (planDeployment over record.Store.Plan's roles), so neither a role of the plan
// in force nor an agent named for one is a role the deployment lost (lostRole, lostSlot), as a walk
// of the issue reads it (Walk.deployment). A generation with no plan rows is d.
func deploymentOf(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue, d plan.Deployment) (plan.Deployment, error) {
	stored, err := store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil || stored == nil {
		return d, err
	}
	return planDeployment(stored.Roles, d), nil
}

// lostRole says whether role is a role nobody in deployment defines: one a daemon restarted
// without, whose task is stopped (stopLostRole) and whose claim no message of a merge forward
// reaches. Legion's own roles are never lost, and a caller that reads a role of an issue's plan
// passes the deployment with the plan's roles (Walk.deployment, deploymentOf).
func lostRole(role claim.Role, deployment plan.Deployment) bool {
	return claim.IsDeploymentRole(role) && !deployment.Defines(string(role))
}

// lostAgent says whether name, the name of a step's agent (record.PlanStep.AgentName), is named
// for a role deployment lost (lostRole): the agent runs in the slot of its name, which it takes
// from the role of its first step (record.NameAgents), so a pod of the deployment has no slot for
// it, whatever role its step has now. A second or later agent of a role takes a numbered name of
// that role (claim.NumberedAgentRole), such as `implementer-2`, which is no role itself: it is
// lost only where that role is, and a numbered name of no role the deployment defines, such as
// that of a role it lost, is lost. A step of the key owner has no name, and is never lost this way.
func lostAgent(name claim.Agent, deployment plan.Deployment) bool {
	if name == "" || !lostRole(claim.Role(name), deployment) {
		return false
	}
	roles := slices.Clone(claim.Roles)
	for role := range deployment.Roles {
		roles = append(roles, claim.Role(role))
	}
	of, numbered := claim.NumberedAgentRole(string(name), roles)
	return !numbered || lostRole(of, deployment)
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
// settings define, which its engine reads too, and the roles the plan of the issue's generation
// defines are read here beside them (deploymentOf). An issue the record does not hold is a nil issue,
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
	if deployment, err = deploymentOf(ctx, store, tx, *issue, deployment); err != nil {
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
// nobody wants any more. The roles of a tree that lingers are suspended. A role nobody defines for
// the issue - neither the deployment nor the plan in force of the issue - is one a restart dropped
// after the row was queued: mergeForwardWorker writes no row to it, the boot stopped its task
// (stopLostRoles), and a delivery would wake its released claim, whose launch then fails for want
// of the role; a claim whose slot is such a role is one a keyed agent named for it runs in,
// whatever its step's role, whose launch waits for a pod with that slot, which the deployment
// builds none of. The workflow drops the ask on a move into the task's implementation step and
// when the issue leaves the workflow (clearMergeForward), and a merge forward of another head is a
// newer ask, which has its own messages. A pull request that is not open ends every step:
// mergeForward asks nothing for one, and once it closed or merged nothing is left to merge forward,
// to hold for or to review. The ask and the hold end at the landing
// (record.MergeForward.Landed): role-message rows run in no order of the issue's, so a hold still
// queued would reach the worker after its landed and tell it to wait for a head that already came,
// and an ask would ask for a merge already pushed. A message to the merger ends when the issue no
// longer stands in merging: a backward move gives the workspace to an earlier phase, and the
// merger must not be woken to write or merge there. The record keeps the ask and its earlier
// landings, so their completion fences and review still stand. The landed and the review are the
// landing's own messages, and they still go after it while their role applies.
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
// hold went to the worker of the step the task stands at (mergeForwardHold, the rule tellAsk sends
// it by), which is the claim that completes, since the step's own fences hold a completion to that
// worker (intake.StepsWorkerFence, or the owner's own report) - since that hold tells the role not
// to complete at that head (record.MergeForwardHold). Then the refusal names the head the pull
// request stands at, says its merge forward is asked and has not landed, and tells the role to wait
// for the daemon's landed message (record.MergeForwardLanded) and do the phase's work at the head
// that message names (awaitedMergeForward). That wait is told only to a role the ask's hold was
// sent to: every other completion is answered as above, at the head the pull request stands at,
// since no hold told its role not to complete there. The retro's agent (the agent of the task's
// last step of the type implementation, or the owner), whose completion of retro is fenced, is
// sent no hold, since the hold reaches no worker of the retro (record.MergeForwardReaches); an ask
// made at the implementation step (record.MergeForward.Own) sends none, and stands into the next
// step when the move there does not ask again (mergeForwardOnEntry), as at a head the record no
// longer holds in a confirmed conflict; and where by, the claim that completes, is the claim the
// ask reached (record.MergeForward.Agent), its ask covered the hold (addressMergeForward,
// record.MergeForwardCovers) and it is sent no landed, since it pushed the merge itself: an owner
// that does both the task's implementation step and the step that completes, or one keyed agent
// that does both.
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
func mergedForwardRefusal(issue record.Issue, role claim.Role, by claim.Token, reported phase.Phase, commit string, pr *record.PullRequest) (intake.Result, bool) {
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
	_, held, err := mergeForwardHold(issue, *asked)
	pending := err == nil && held && asked.Landed == "" && asked.Head == pr.HeadSHA && asked.Phase == reported &&
		(asked.Agent == "" || by != asked.Agent)
	var next string
	if pending {
		next = awaitedMergeForward(pr, again)
	} else {
		next = fmt.Sprintf("Pull request #%d stands at head %s now. Read what the merge changed with `git show --remerge-diff %s`, do the phase's work at head %s, then %s; this completion changed nothing",
			pr.Number, pr.HeadSHA, landed, pr.HeadSHA, again)
	}
	return refused("HANDOFF_HEAD_MERGED_FORWARD", fmt.Sprintf("the %s's completion of phase %s of %s %s. %s", role, reported, issue.Key, clause, next)), true
}

// awaitedMergeForward ends a refusal when the merge forward of the current head is pending and
// the completion's role is eligible to wait for it (mergedForwardRefusal). It names that head and
// tells the role to work at the head in the daemon's landed message, then complete as again says.
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
