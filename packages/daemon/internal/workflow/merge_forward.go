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
	"github.com/sjawhar/legion/daemon/internal/store"
)

// mergeForward is the daemon's act on a confirmed conflict that R0 found a reason to act on, why
// (dispatch://INFRA-4374 R0, R1; classify.ConflictNeedsMerge): the issue keeps the phase it stands
// in (record.StandsAt), no phase entry starts and no supervise row is written, so an issue past
// its review takes no admission slot for the merge, and the role the phase asks
// (record.MergeForwardAsker) is asked, on its own role topic, to merge the base forward onto the
// head and push (record.MergeForwardAsk): the implementer before merging, and in merging the
// merger, whose merge it is (cybermonklabs/legion#4979). Outside implementing it also tells the
// worker of that phase, when that is the tester or the reviewer, to stop writing the workspace the
// roles share while the merge runs (record.MergeForwardHold), and the architect that owns the
// issue one checks-red notice whose phase is the phase the issue stays in and whose reason names
// the head, the base, why, the role asked and that the issue stays in its phase; the outbox routes
// that notice to the owning architect, as every notice of the issue is routed. In implementing
// only the ask goes: the implementer's own assignment and its completion stand, and the architect
// is told nothing, since the work the conflict needs is already where the issue stands.
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
// a pull request that is not open, an issue where no merge forward stands on line, the phases its
// task walks (classify.MergeForwardStands: implementing or a phase after it), and a tree that
// lingers (record.TreeLingers, where every role is suspended) are asked nothing.
//
// While a fix of the task holds the task's branch (fixHoldsBranch) nothing else pushes there, so
// the conflict waits: the record keeps the conflicting head, as it keeps one that needs no merge
// yet, one line says why, and nobody is asked or told; the way after the fix reads the conflict
// again (mergeForwardAfterFix). An ask of the head that a fix took as its own (record.MergeForward.Fix)
// is no ask once that fix no longer holds the branch, so a read of the same head asks again.
func (e *Engine) mergeForward(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, pr *record.PullRequest, why record.ConflictWhy) error {
	at := record.StandsAt(issue)
	if pr.State != record.PullRequestOpen || !classify.MergeForwardStands(issue, w.Line) {
		return nil
	}
	holds, err := e.fixHoldsBranch(ctx, tx, issue, w)
	if err != nil {
		return err
	}
	if holds && false {
		e.logOnCommit(ctx, "workflow: a confirmed conflict waits for the fix that holds the task's branch", "issue", issue.Key, "task", w.Task,
			"phase", at, "pullRequest", pr.Number, "head", pr.HeadSHA, "base", pr.Base, "why", why)
		return nil
	}
	asker := record.MergeForwardAsker(at)
	if asked := pr.MergeForward; asked != nil && asked.Head == pr.HeadSHA && asked.Fix == nil && (asked.Phase != phase.Implementing || at == phase.Implementing) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	recipients := []roleRecipient{{issue: issue.Key, role: asker, row: issue.Key}}
	if worker := RoleFor(at); worker != asker && record.MergeForwardReaches(record.MergeForwardHold, worker) {
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: worker, row: issue.Key})
	}
	addressed, err := e.addressRoles(issue.Project, recipients, nil)
	if err != nil {
		return err
	}
	for _, to := range addressed {
		role, _ := to.token.Role()
		step := record.MergeForwardHold
		if role == asker {
			step = record.MergeForwardAsk
		}
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: role, Step: step, Head: pr.HeadSHA, Base: pr.Base,
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
			next.Earlier = append(slices.Clip(before.Earlier), record.LandedMergeForward{Head: before.Head, Landed: before.Landed, Fix: before.Fix})
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

// fixHoldsBranch says whether a fix of the task of w holds the task's branch: from its opening
// until its way is taken, that is while the task's latest fix reads its job, runs, or has ended
// with its way pending (record.TaskFix.Way 0). While a fix holds it, nothing pushes to the branch
// but the fix's one end push (mergeForward). A fix that a stop, a leave or a new generation ended
// takes no way (Way 4) and holds nothing.
func (e *Engine) fixHoldsBranch(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (bool, error) {
	if !w.Stored {
		return false, nil
	}
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil {
		return false, err
	}
	return fix.Way == 0, nil
}

// mergeForwardAfterFix reads the record's conflict again once the way after a fix is taken
// (afterFix), in that way's transaction and after its act: the fix no longer holds the task's
// branch, so a conflict that waited for it is judged now. It asks nobody and writes no reason.
// Where the record holds the task's head in a confirmed conflict (classify.ConflictRound), it asks
// for a fresh read of GitHub's merge state once the transaction commits (Config.ReadMergeState),
// whose answer is judged at that head as every confirmed conflict is (mergeability, R0 of
// dispatch://INFRA-4374): a conflict R0 leaves alone stays recorded, and one it acts on is asked
// then, once. Where the conflicting head is no longer the task's head, it reads nothing: the reads
// the head's move owes decide.
func (e *Engine) mergeForwardAfterFix(ctx context.Context, tx pgx.Tx, key string) error {
	pr, err := e.store.PullRequest(ctx, tx, key)
	if err != nil || pr == nil || pr.State != record.PullRequestOpen || !classify.ConflictRound(*pr) {
		return err
	}
	if e.cfg.ReadMergeState == nil {
		return nil
	}
	e.logOnCommit(ctx, "workflow: the way after a fix reads the conflict that waited for it", "issue", key, "pullRequest", pr.Number,
		"head", pr.HeadSHA, "base", pr.Base)
	store.OnCommit(ctx, func() { e.cfg.ReadMergeState(key) })
	return nil
}

// takeOwedMerge makes the implementer's owed merge forward of the head the fix of pass opens at the
// fix's own work, in the transaction that opens it (openFix), before its job is read or its task
// is delivered: an ask of that head to the implementer that has not landed (owedMerge) takes the
// fix's name (record.MergeForward.Fix). The fix's task tells it to make that merge first, in its
// own working copy, and push it with its one end push (fixTask), so the fix stays the one writer
// of the task's branch: the ask's own messages end undelivered (leftMergeForwardAt), its landing
// is the fix's (mergeForwardLanded), and no completion fence is made of it (mergedForwardRefusal).
func (e *Engine) takeOwedMerge(ctx context.Context, tx pgx.Tx, pass *fixPass, number int) error {
	if owedMerge(pass.pr, pass.head) == nil {
		return nil
	}
	taken := *pass.pr.MergeForward
	taken.Fix = &record.MergeForwardFix{Generation: pass.issue.Generation, Task: pass.w.Task, Number: number}
	pr := *pass.pr
	pr.MergeForward = &taken
	if err := e.putPullRequest(ctx, tx, pr); err != nil {
		return err
	}
	pass.pr = &pr
	e.logOnCommit(ctx, "workflow: a fix takes the merge forward its implementer owes", "issue", pass.issue.Key, "task", pass.w.Task,
		"fix", number, "pullRequest", pr.Number, "head", taken.Head, "base", taken.Base, "askedIn", taken.Phase)
	return nil
}

// owedMerge is the merge forward the implementer owes at head on pr: the record's ask of head to
// the implementer that has not landed, which a fix of the task may take as its own (takeOwedMerge)
// or holds already; nil for none.
func owedMerge(pr *record.PullRequest, head string) *record.MergeForward {
	if pr == nil || pr.State != record.PullRequestOpen {
		return nil
	}
	asked := pr.MergeForward
	if asked == nil || asked.Landed != "" || asked.Head != head || asked.Asked() != claim.RoleImplementer {
		return nil
	}
	return asked
}

// fixOwns says whether owner names fix of task in issue's generation: the fix that took a merge
// forward (record.MergeForward.Fix) is of the task a completion reports on.
func fixOwns(owner *record.MergeForwardFix, issue record.Issue, task string) bool {
	return owner != nil && owner.Generation == issue.Generation && owner.Task == task
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
//   - At such a head, a move into a phase after implementing on the task's line (Walk.MovesBack: a
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
func (e *Engine) mergeForwardOnEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) error {
	if issue.Phase == phase.Implementing {
		return e.clearMergeForward(ctx, tx, issue.Key)
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil || !classify.ConflictRound(*pr) {
		return err
	}
	asked := pr.MergeForward
	switch {
	case record.StandsIn(issue, phase.Merging):
		if asked != nil && asked.Head == pr.HeadSHA && asked.Phase != phase.Implementing && asked.Asked() != claim.RoleMerger {
			return e.holdMerger(ctx, tx, issue, pr)
		}
		return e.mergeForward(ctx, tx, issue, w, pr, record.ConflictMergeNext)
	case asked != nil && asked.Head == pr.HeadSHA && asked.Phase == phase.Implementing &&
		w.MovesBack(record.StandsAt(issue), phase.Implementing) && !classify.MergeComesNext(issue, w.Line):
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
	addressed, err := e.addressRoles(issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleMerger, row: issue.Key}}, nil)
	if err != nil {
		return err
	}
	for _, to := range addressed {
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardHold,
			Head: asked.Head, Base: asked.Base, Phase: phase.Merging, PullRequest: pr.Number, Why: asked.Why}); err != nil {
			return err
		}
	}
	e.logOnCommit(ctx, "workflow: the merger waits for a merge forward another role was asked", "issue", issue.Key, "tree", issue.Tree,
		"pullRequest", pr.Number, "head", asked.Head, "asked", asked.Asked(), "askedIn", asked.Phase)
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
// to read what the merge changed before it goes on (record.MergeForwardLanded). An issue whose
// task's reviewer step already ran - that step stands before the place the issue stands at on the
// walk's line (Walk.MovesBack) - stands on a review of the old head, so the reviewer is asked to
// read the merge and approve the new head or request changes (record.MergeForwardReview), and the
// record keeps that the review was asked for (ReviewAsked), which the merger's task (task) and the
// landed the merger is told in merging (record.MergeForwardMessage.Message) read. In the default
// steps that is an issue in accepting, retro or merging. A reviewer step still to come reviews
// the head the issue stands at when it runs, so it is asked nothing now. A task whose plan has no
// reviewer step stands on no review of the old head, no reviewer step runs for it, and a merger
// told to wait for a reviewer's approval would wait for good, so nothing is asked and the merger
// goes on at the new head.
// The daemon records no decision outside reviewing, so that review is the roles' to hold to the
// head they merge. The architect and the role asked are told nothing more: the architect's
// checks-red notice already said the merge was asked for, and the role asked pushed it; in merging
// that role is the merger, which the landed reaches as the worker of merging, since it tells it to
// merge the new head only once the reviewer's approval of it is on the pull request, where the
// task has a reviewer step, and to go on at the new head where it has none. A tree that
// lingers (record.TreeLingers, where every role is suspended) records and tells nothing.
//
// A merge forward a fix took as its own (record.MergeForward.Fix, takeOwedMerge) lands with the
// fix's one end push: the record keeps the new head as the one it left, asks no review, and tells
// nobody, since the fix's own end decides what runs after it (afterFix) and no step beside the fix
// was held for this merge.
func (e *Engine) mergeForwardLanded(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	asked := pr.MergeForward
	if asked == nil || asked.Landed != "" || asked.Head == pr.HeadSHA {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	if asked.Fix != nil {
		landed := *asked
		landed.Landed, landed.ReviewAsked = pr.HeadSHA, false
		pr.MergeForward = &landed
		if err := e.putPullRequest(ctx, tx, *pr); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: the merge forward a fix took landed with its end push", "issue", issue.Key, "tree", issue.Tree,
			"task", asked.Fix.Task, "fix", asked.Fix.Number, "pullRequest", pr.Number, "head", asked.Head, "landed", pr.HeadSHA)
		return nil
	}
	at := record.StandsAt(issue)
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	review := w.MovesBack(at, phase.Reviewing)
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
// to role about the merge forward asked of head is no longer to be delivered (leftMergeForwardAt),
// "" while it is. An issue the record does not hold is a nil issue, no pull request and no reason.
// The daemon's executor is its one caller: it reads the rule in one transaction at each attempt of
// a row, just before it delivers (the daemon's mergeForwardMessage), so the executor judges each
// queued row by this rule, and no row outlives the state it was written for. The workflow calls
// neither this nor leftMergeForwardAt: it writes these rows only in states the rule accepts. The
// rule is kept beside mergeForward and mergeForwardLanded, which write the rows, and
// clearMergeForward, which drops the ask they are about, since it restates their states for a row
// already queued. It is exported for that executor.
func MergeForwardAt(ctx context.Context, store record.Store, tx pgx.Tx, key string, role claim.Role, step record.MergeForwardStep, head string) (*record.Issue, *record.PullRequest, string, error) {
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
	return issue, pr, leftMergeForwardAt(*issue, lingers, pr, role, step, head), nil
}

// leftMergeForwardAt says why a merge-forward message of step to role about the merge forward
// asked of head is no longer to be delivered, or "" while it is: the issue is in the workflow, its
// tree does not linger (lingers, record.TreeLingers), and the pull request the record holds for it
// (pr, nil for none) is open and holds a merge forward of head (record.PullRequest.MergeForward)
// that, for an ask or a hold, has not landed. In each other state the message would tell its role
// of work nobody wants any more. The roles of a tree that lingers are suspended. The workflow drops
// the ask on a move into implementing and when the issue leaves the workflow (clearMergeForward),
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
func leftMergeForwardAt(issue record.Issue, lingers bool, pr *record.PullRequest, role claim.Role, step record.MergeForwardStep, head string) string {
	switch {
	case !record.InWorkflow(issue):
		return fmt.Sprintf("the issue has left the workflow, at status %s in %s", issue.Status, issue.Phase)
	case lingers:
		return "the issue's tree lingers or has closed"
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
	case pr.MergeForward.Fix != nil:
		return fmt.Sprintf("the merge forward of head %s is fix %d of task %s's own work", head, pr.MergeForward.Fix.Number, pr.MergeForward.Fix.Task)
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
// completion of testing, reviewing or retro whose commit is the head a merge forward was asked of,
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
// It answers false where it does not refuse.
//
// A merge forward a fix of task took as its own (record.MergeForward.Fix, fixOwns) makes no fence:
// the fix pushed it with its one end push, and the steps beside the fix judged the head the fix
// started at, which the fix's way reads (afterFix). The fence still stands for every merge no fix
// took, among the earlier landings too, so a completion at a head such a merge replaced is
// refused as before.
func mergedForwardRefusal(issue record.Issue, task string, role claim.Role, reported phase.Phase, commit string, pr *record.PullRequest) (intake.Result, bool) {
	switch reported {
	case phase.Testing, phase.Reviewing, phase.Retro:
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
		if asked.Landed != "" && !fixOwns(asked.Fix, issue, task) {
			landed = asked.Landed
		}
		for i := len(asked.Earlier) - 1; landed == "" && i >= 0; i-- {
			if !fixOwns(asked.Earlier[i].Fix, issue, task) {
				landed = asked.Earlier[i].Landed
			}
		}
		if landed == "" {
			return intake.Result{}, false
		}
		clause = fmt.Sprintf("names no head, so it says nothing about which head its work judged, and a merge of %s forward landed at head %s",
			asked.Base, landed)
	} else {
		replaced, ok := mergedForwardOf(asked, commit, func(owner *record.MergeForwardFix) bool { return fixOwns(owner, issue, task) })
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
	pending := asked.Landed == "" && asked.Fix == nil && asked.Head == pr.HeadSHA && asked.Phase == reported && record.MergeForwardReaches(record.MergeForwardHold, role)
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
// forwards that landed before it (record.MergeForward.Earlier) whose head commit is, leaving out
// each that a fix of the completion's task took as its own (owned). It answers false where no
// other merge forward the record keeps replaced commit.
func mergedForwardOf(asked *record.MergeForward, commit string, owned func(*record.MergeForwardFix) bool) (record.LandedMergeForward, bool) {
	if asked.Landed != "" && asked.Head == commit && !owned(asked.Fix) {
		return record.LandedMergeForward{Head: asked.Head, Landed: asked.Landed, Fix: asked.Fix}, true
	}
	for i := len(asked.Earlier) - 1; i >= 0; i-- {
		if asked.Earlier[i].Head == commit && !owned(asked.Earlier[i].Fix) {
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
