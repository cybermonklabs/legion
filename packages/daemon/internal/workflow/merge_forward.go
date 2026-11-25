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
// the head, the base, why, the role asked, that the issue stays in its phase and the read of
// GitHub the act rests on; the outbox routes that notice to the owning architect, as every notice
// of the issue is routed. In implementing only the ask goes: the implementer's own assignment and
// its completion stand, and the architect is told nothing, since the work the conflict needs is
// already where the issue stands.
//
// The read the act rests on is the newest read of GitHub the record holds of the head, its merge
// state and the moment it was read (record.PullRequest.MergeState and StateReadAt), which the ask,
// the hold and the notice each name (record.AnsweredMergeState, dispatch://INFRA-2833 R3) and the
// log line of the ask carries. On a confirmed read (mergeability) it is the read just applied,
// which confirmed the conflict or found it confirmed again, and which mergeability has just
// written to pr; a fact that carries no read time, as one a test mints, writes its state and
// leaves the stamp the record held. On a phase entry (mergeForwardOnEntry) it is the newest read
// the record holds of that head by then, which the merger's hold names too (holdMerger). The time
// is carried in UTC, as the messages name it.
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
func (e *Engine) mergeForward(ctx context.Context, tx pgx.Tx, issue record.Issue, line []phase.Phase, pr *record.PullRequest, why record.ConflictWhy) error {
	at := record.StandsAt(issue)
	if pr.State != record.PullRequestOpen || !classify.MergeForwardStands(issue, line) {
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
	if worker := RoleFor(at); worker != asker && record.MergeForwardReaches(record.MergeForwardHold, worker) {
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: worker, row: issue.Key})
	}
	addressed, err := e.addressRoles(issue.Project, recipients, nil)
	if err != nil {
		return err
	}
	readAt := pr.StateReadAt.UTC()
	for _, to := range addressed {
		role, _ := to.token.Role()
		step := record.MergeForwardHold
		if role == asker {
			step = record.MergeForwardAsk
		}
		if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: role, Step: step, Head: pr.HeadSHA, Base: pr.Base,
			Phase: at, PullRequest: pr.Number, Why: why, MergeState: pr.MergeState, StateReadAt: readAt}); err != nil {
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
		reason := fmt.Sprintf("the head %s conflicts with %s (%s): the %s is asked to merge %s forward, and the issue %s; %s",
			pr.HeadSHA, pr.Base, why, asker, pr.Base, stays, record.AnsweredMergeState(pr.MergeState, readAt, pr.HeadSHA))
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
		"pullRequest", pr.Number, "head", pr.HeadSHA, "base", pr.Base, "why", why,
		"mergeState", string(pr.MergeState), "stateReadAt", readAt)
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
//
// A move that leaves the record's open ask owed to nobody where the issue now stands (owedTo: the
// ask of implementing outside implementing that the move did not ask again, the merger's ask
// outside merging) ends the take hold that ask put on the issue's adopting takes
// (mergeForwardEnds), as the landing and the clear do (putPullRequest).
func (e *Engine) mergeForwardOnEntry(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) error {
	if issue.Phase == phase.Implementing {
		return e.clearMergeForward(ctx, tx, issue.Key)
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return err
	}
	if classify.ConflictRound(*pr) {
		asked := pr.MergeForward
		switch {
		case record.StandsIn(issue, phase.Merging):
			if asked != nil && asked.Head == pr.HeadSHA && asked.Phase != phase.Implementing && asked.Asked() != claim.RoleMerger {
				err = e.holdMerger(ctx, tx, issue, pr)
			} else {
				err = e.mergeForward(ctx, tx, issue, w.Line, pr, record.ConflictMergeNext)
			}
		case asked != nil && asked.Head == pr.HeadSHA && asked.Phase == phase.Implementing &&
			w.MovesBack(record.StandsAt(issue), phase.Implementing) && !classify.MergeComesNext(issue, w.Line):
			err = e.mergeForward(ctx, tx, issue, w.Line, pr, asked.Why)
		}
		if err != nil {
			return err
		}
	}
	if asked, open := openAsk(*pr); open && !owedTo(issue, asked) {
		e.mergeForwardEnds(ctx, issue.Key)
	}
	return nil
}

// holdMerger tells the merger of an issue that has just entered merging that another role's merge
// forward of the head stands and has not landed (mergeForwardOnEntry): the hold
// (record.MergeForwardHold) has it merge nothing and wait for the landed message, which
// mergeForwardLanded sends it as the worker of merging, with the reviewer's review of the new head.
// The hold names the read of GitHub it rests on, as mergeForward's hold does: the newest read the
// record holds of the head (record.PullRequest.MergeState and StateReadAt, dispatch://INFRA-2833
// R3), which is the head asked of. The record's ask is left as it is. A tree that lingers tells
// nobody.
func (e *Engine) holdMerger(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	asked := pr.MergeForward
	addressed, err := e.addressRoles(issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleMerger, row: issue.Key}}, nil)
	if err != nil {
		return err
	}
	// The issue stands in merging here (mergeForwardOnEntry), so the message names merging.
	hold := mergeForwardMessageOf(issue, *pr, claim.RoleMerger, record.MergeForwardHold, asked.Why)
	for _, to := range addressed {
		if err := e.enqueue(ctx, tx, to.issue, hold); err != nil {
			return err
		}
	}
	e.logOnCommit(ctx, "workflow: the merger waits for a merge forward another role was asked", "issue", issue.Key, "tree", issue.Tree,
		"pullRequest", pr.Number, "head", asked.Head, "asked", asked.Asked(), "askedIn", asked.Phase,
		"mergeState", string(pr.MergeState), "stateReadAt", hold.StateReadAt)
	return nil
}

// clearMergeForward drops the merge forward the record holds for issue's pull request
// (record.PullRequest.MergeForward), on a move into implementing (mergeForwardOnEntry) and when
// the issue leaves the workflow (leave): the ask, its landing and the review it asked for are of
// a stretch of work that is over, and a later conflict is asked about afresh. A record that holds
// none is not written. A clear of an ask that was open ends the take hold it put on the issue's
// adopting takes (putPullRequest, mergeForwardEnds): a move back into implementing is no take of
// the asked role's held by its own merge, since the move clears the ask in its own transaction.
func (e *Engine) clearMergeForward(ctx context.Context, tx pgx.Tx, issue string) error {
	pr, err := e.store.PullRequest(ctx, tx, issue)
	if err != nil || pr == nil || pr.MergeForward == nil {
		return err
	}
	pr.MergeForward = nil
	return e.putPullRequest(ctx, tx, *pr)
}

// openAsk is the merge forward pr's record holds while it is the open ask of the head pr stands
// at - an ask of that head that has not landed - and whether it is.
func openAsk(pr record.PullRequest) (record.MergeForward, bool) {
	asked := pr.MergeForward
	if asked == nil || asked.Head != pr.HeadSHA || asked.Landed != "" {
		return record.MergeForward{}, false
	}
	return *asked, true
}

// openMergeForward says whether the merge forward the record holds, nil for none, is one the take
// hold may stand over: it has neither landed nor been withdrawn. putPullRequest reads the end of
// the hold off the two sides of a write by it.
func openMergeForward(asked *record.MergeForward) bool {
	return asked != nil && asked.Landed == "" && !asked.Withdrawn
}

// owedTo says whether the role asked names still owes the merge while issue stands where it does:
// the merger's ask while the issue stands in merging, where the merge is its own, since a message
// to the merger outside merging is delivered no more (leftMergeForwardAt); the implementer's ask
// made in implementing while it stands in implementing, which that ask covers alone, where a later
// phase that still owes the merge asks again (mergeForwardOnEntry); the implementer's ask made in
// any later phase wherever the issue stands, merging included, where the merger is held for it
// (holdMerger); and never once the ask was withdrawn (record.MergeForward.Withdrawn). An asked role
// whose claim ended still owes it: the head still conflicts, and the ready of that role's next
// launch asks it again (askAgainAtLaunch).
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
// that head is owed to a role, which the take hold reads (MergeForwardOwed), the ask sent again to
// the asked role's next launch (askAgain) and the ask queued at a reopen (askAtReopen), so the
// issue's adopting takes are held exactly while a launch of the asked role is owed the ask. It
// reads no state of the pull request: a close does not stop a merge already running in the
// workspace, so the merge stays owed, and the hold stands, through a close at the same head; the
// sends alone wait for the pull request to be open (askAgain, the daemon's delivery).
func owedAsk(issue record.Issue, pr record.PullRequest) (record.MergeForward, bool) {
	asked, open := openAsk(pr)
	return asked, open && owedTo(issue, asked)
}

// MergeForwardOwed reads in tx whether the merge of the head issue key's pull request stands at is
// owed to a role now (owedAsk), and which role: supervise's take hold (Deps.MergeForwardOwed),
// which the daemon answers through it (workflowRuntime.mergeForwardOwed). While it is, every take
// of the issue's roles that would adopt the working copy they share is refused, the asked role's
// own included: a merge forward is a role message and no take (mergeForward), so between two runs
// of the merge, or before its first run starts, no role of the issue is in a turn, and an adoption
// made then would leave an empty commit on the merge in progress. An issue or a pull request the
// record does not hold owes no merge. The hold ends where the merge stops being owed: the landing
// (a reopen at a head pushed while the pull request was closed among them, pullRequestOpened), the
// clear, and a move that leaves the ask owed to nobody (mergeForwardEnds). A close at the same head
// does not end it, whatever the pull request's state: the close sends the architect
// pr-closed-unmerged, whose decision ends the wait, and a reopen keeps the ask and sends it where
// it did not reach the asked role's live launch (askAtReopen).
func MergeForwardOwed(ctx context.Context, store record.Store, tx pgx.Tx, key string) (claim.Role, bool, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return "", false, err
	}
	pr, err := store.PullRequestRow(ctx, tx, key)
	if err != nil || pr == nil {
		return "", false, err
	}
	asked, owed := owedAsk(*issue, *pr)
	if !owed {
		return "", false, nil
	}
	return asked.Asked(), true, nil
}

// mergeForwardEnds tells the daemon, once tx commits, that the merge of issue's head may be owed
// to no role any more (Config.MergeForwardEnded): the merge forward landed (mergeForwardLanded),
// was withdrawn or cleared (clearMergeForward), which putPullRequest finds by the open ask its
// write takes from the record, or a move left the open ask owed to nobody where the issue now
// stands (mergeForwardOnEntry). What that end does is internal/daemon's supervisor.wakeIssueClaims.
func (e *Engine) mergeForwardEnds(ctx context.Context, issue string) {
	if e.cfg.MergeForwardEnded == nil {
		return
	}
	store.OnCommit(ctx, func() { e.cfg.MergeForwardEnded(issue) })
}

// askAgainAtLaunch sends the record's ask again at the ready of a launch of the asked role's claim
// (askAgain). The ready is a fact (intake.ClaimReady) the daemon applies once for each launch of a
// claim (claimReady); the ready of a role no merge forward asks reads nothing more.
//
// The asked role's process can be relaunched while the ask is owed - a death, a provider error's
// retry - and its claim can end: it fails (claimFailed), an operator stops it, or its agent reports
// its own exit. The new process holds no ask, and a row of the ask queued for an ended claim ends
// undelivered (the daemon's deliverHeld), so the ready of every later launch asks again, one rule
// for every cause. The issue's adopting takes stay held meanwhile, the asked role's own phase task
// included (MergeForwardOwed), since the merge stays owed, so without this the hold would wait for
// a merge no process was told of. Each launch is told the ask once. While a row of the ask is still
// queued the ready sends nothing and leaves the ask to that row - the row that woke a released
// role among them, whose wake is that launch - and the row's end, delivered or not, settles under
// the fact lock with the claim's launch read again (MergeForwardAskEnded): it records the launch
// the row reached, so the ready of that launch, where it comes after the delivery, sends nothing
// either, and it sends the ask to a later launch the row did not reach, whose ready left the ask to
// the row. A launch that resumes the session an earlier launch was told the ask in is asked once
// more at its ready: the workflow cannot tell that session from a new one, and an owed ask is
// still true when it is told again. An ask that landed or was withdrawn, and the ask of
// implementing outside implementing, are owed to nobody, and a pull request that is not open or a
// tree that lingers tells nobody.
func (e *Engine) askAgainAtLaunch(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.ClaimReady) error {
	pr, err := askAgain(ctx, e.store, tx, issue, fact.Role, fact.Launch, e.now())
	if err != nil || pr == nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a merge forward still owed is asked again at its role's new launch", "issue", issue.Key,
		"tree", issue.Tree, "role", fact.Role, "launch", fact.Launch, "phase", record.StandsAt(issue), "pullRequest", pr.Number,
		"head", pr.MergeForward.Head, "askedIn", pr.MergeForward.Phase)
	return nil
}

// askAgain sends the record's ask of issue's pull request again to role at launch, a launch of
// role's claim later than the one the ask was last told (record.MergeForward.SentLaunch), while
// the merge of the head is still owed to role where issue stands (owedAsk) and no row of that ask
// is queued for role and head (queueAsk), and records launch on the ask. The row carries launch and
// the SentLaunch it replaced (record.MergeForwardMessage's Launch and SentLaunch), so its delivery
// tells launch, and a row that ends undelivered sets the ask's SentLaunch back
// (MergeForwardAskEnded). It answers the pull request it wrote, nil where it sent nothing. Launch 0
// is no launch: a claim that failed or retired, or one the daemon holds none of, is asked at the
// ready of its next launch. A pull request that is not open and a tree that lingers tell nobody.
// It decides from the pull request's row (record.Store.PullRequestRow), and reads the whole pull
// request only to write it, once the ask is sent: at nearly every ready of the asked role, which
// reaches it under the fact lock, nothing is sent. Its callers are a launch's ready
// (askAgainAtLaunch) and the end of a row of the ask (MergeForwardAskEnded), which the daemon's
// executor makes without an engine, so it writes through store: the row through queuedRow, and the
// pull request through record.Store.PutPullRequest rather than putPullRequest, since it changes the
// merge forward alone, which is no news of the pull request (pullRequestNews).
func askAgain(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue, role claim.Role, launch uint64, now time.Time) (*record.PullRequest, error) {
	if launch == 0 || !record.MergeForwardReaches(record.MergeForwardAsk, role) || !record.InWorkflow(issue) {
		return nil, nil
	}
	recorded, err := store.PullRequestRow(ctx, tx, issue.Key)
	if err != nil || recorded == nil || recorded.State != record.PullRequestOpen {
		return nil, err
	}
	asked, owed := owedAsk(issue, *recorded)
	if !owed || asked.Asked() != role || launch <= asked.SentLaunch {
		return nil, nil
	}
	if queued, err := queueAsk(ctx, store, tx, issue, *recorded, asked, launch, now); err != nil || !queued {
		return nil, err
	}
	pr, err := store.PullRequest(ctx, tx, issue.Key)
	if err != nil || pr == nil {
		return nil, err
	}
	sent := asked
	sent.SentLaunch = launch
	pr.MergeForward = &sent
	if err := store.PutPullRequest(ctx, tx, *pr); err != nil {
		return nil, err
	}
	return pr, nil
}

// queueAsk queues a row of asked, the record's ask of pr's head, to its asked role
// (mergeForwardMessageOf), unless a row of that ask is still queued for that role and head
// (record.Store.QueuedMergeForwardAsk), which reaches the role by its own delivery, or issue's tree
// lingers (record.TreeLingers), where every role is suspended; it answers whether it queued one.
// The row carries launch, the launch it is queued for (0 for none), and the SentLaunch the ask
// holds as it is queued (record.MergeForwardMessage's Launch and SentLaunch). Its callers are the
// re-send at a launch's ready (askAgain), which then writes launch as the ask's SentLaunch, and the
// reopen (askAtReopen), which writes none. It writes through store, as askAgain must.
func queueAsk(ctx context.Context, store record.Store, tx pgx.Tx, issue record.Issue, pr record.PullRequest, asked record.MergeForward, launch uint64, now time.Time) (bool, error) {
	role := asked.Asked()
	if queued, err := store.QueuedMergeForwardAsk(ctx, tx, issue.Key, role, asked.Head); err != nil || queued {
		return false, err
	}
	if lingers, err := record.TreeLingers(ctx, store, tx, issue.Tree); err != nil || lingers {
		return false, err
	}
	ask := mergeForwardMessageOf(issue, pr, role, record.MergeForwardAsk, asked.Why)
	ask.Launch, ask.SentLaunch = launch, asked.SentLaunch
	row, err := queuedRow(ctx, tx, store, issue.Key, ask, now)
	if err != nil {
		return false, err
	}
	if err := store.Enqueue(ctx, tx, row); err != nil {
		return false, err
	}
	return true, nil
}

// askAtReopen queues the record's ask of pr's head to its asked role at a reopen of pr
// (pullRequestOpened), which keeps the ask, where the merge of that head is still owed to that role
// (owedAsk). While the pull request was closed the ask may have reached no live launch of the role:
// the re-send at a launch's ready sends nothing to a pull request that is not open (askAgain), and
// the delivery of a row of the ask refuses one (MergeForwardAt), so a row that ended undelivered
// during the close, or a launch that came up during it, left the role untold. The workflow knows no
// launch of the role's claim, so the row carries the SentLaunch it is queued under
// (record.MergeForwardMessage.SentLaunch), and its delivery, which reads the launch, tells only a
// later one: a live launch already told, whose merge may still be running, is told nothing again
// (the daemon's mergeForwardMessage). A row of the ask still queued for the role and head is not
// doubled (queueAsk), and a ready that comes while this row is queued leaves the ask to it
// (askAgainAtLaunch). The ask of implementing reopened outside implementing is owed to nobody and
// gets none, and an issue out of the workflow and a tree that lingers tell nobody, as the re-send
// tells nobody there.
func (e *Engine) askAtReopen(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest) error {
	asked, owed := owedAsk(issue, pr)
	if !owed || !record.MergeForwardReaches(record.MergeForwardAsk, asked.Asked()) || !record.InWorkflow(issue) {
		return nil
	}
	if queued, err := queueAsk(ctx, e.store, tx, issue, pr, asked, 0, e.now()); err != nil || !queued {
		return err
	}
	e.logOnCommit(ctx, "workflow: a merge forward still owed is asked again at the reopen of its pull request", "issue", issue.Key,
		"tree", issue.Tree, "role", asked.Asked(), "phase", record.StandsAt(issue), "pullRequest", pr.Number, "head", asked.Head,
		"askedIn", asked.Phase, "sentLaunch", asked.SentLaunch)
	return nil
}

// MergeForwardAskEnded settles the end of ask, a row of the merge-forward ask of ask.Head to
// ask.Role on issue key, which the daemon's executor deletes in tx under the fact lock tx holds
// (intake.SerializeFacts), so the end is ordered with every fact, the ready of each launch of the
// role's claim among them (askAgainAtLaunch). told is the launch of the role's claim the row was
// delivered to, which the executor reads before the publish, and 0 for a row that ends
// undelivered; launch is the claim's launch as the executor reads it in tx, 0 where the claim has
// failed or retired or the daemon holds none. It answers whether it sent the ask again.
//
// The record's ask first keeps what the row's end says of the launches told (settleAskRow): a row
// delivered to told keeps told, so the ready of told, which can come after the delivery, sends
// nothing again, and a re-send's row that ends undelivered sets the ask's SentLaunch back to the
// one it replaced. Then, delivered or not, the ask is sent again at launch (askAgain): a launch
// whose ready came while the row was still queued sent nothing and left the ask to the row, which
// reached an earlier launch or none. So each launch is told the ask once, whichever of the row's
// end and the launch's ready comes first, with one exception, so that no launch is left untold:
// read before the publish, told is never a launch later than the one whose session took the
// message, so a launch that came up and took the publish between that read and the publish is told
// the ask again here. Where the record holds no merge forward, or one that launch is no later than
// the last told of, askAgain would send nothing, and is not asked.
func MergeForwardAskEnded(ctx context.Context, store record.Store, tx pgx.Tx, key string, ask record.MergeForwardMessage, told, launch uint64, now time.Time) (bool, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return false, err
	}
	asked, err := settleAskRow(ctx, store, tx, key, ask, told)
	if err != nil || asked == nil || launch <= asked.SentLaunch {
		return false, err
	}
	pr, err := askAgain(ctx, store, tx, *issue, ask.Role, launch, now)
	return pr != nil, err
}

// settleAskRow writes on the record's ask of issue key what the end of ask's row says of the
// launches it was told (MergeForwardAskEnded), and answers the record's merge forward as it stands
// then, nil where the record holds no pull request or no merge forward. It writes only where the
// record's ask is still the row's own: of the row's head, naming the row's role, not landed. A row
// delivered to told, a launch later than the ask was last told, keeps told
// (record.MergeForward.SentLaunch). A row that ends undelivered (told 0) and was queued for a launch
// (record.MergeForwardMessage.Launch: the re-send at that launch's ready, which wrote it as the
// ask's SentLaunch as it queued the row) told that launch nothing: where the ask still names that
// launch, it is set back to the SentLaunch the row replaced, so the ask names the launch last told
// and a launch whose row was refused, by a close before its delivery among them, is told at the
// reopen (askAtReopen). A row queued for no launch wrote none and sets none back, and a launch
// recorded since, or a new ask that replaced the row's, is left as it stands. It decides from the
// pull request's row and reads the whole pull request only to write it.
func settleAskRow(ctx context.Context, store record.Store, tx pgx.Tx, key string, ask record.MergeForwardMessage, told uint64) (*record.MergeForward, error) {
	recorded, err := store.PullRequestRow(ctx, tx, key)
	if err != nil || recorded == nil || recorded.MergeForward == nil {
		return nil, err
	}
	asked := *recorded.MergeForward
	if asked.Head != ask.Head || asked.Asked() != ask.Role || asked.Landed != "" {
		return &asked, nil
	}
	switch {
	case told > asked.SentLaunch:
		asked.SentLaunch = told
	case told == 0 && ask.Launch != 0 && asked.SentLaunch == ask.Launch:
		asked.SentLaunch = ask.SentLaunch
	default:
		return &asked, nil
	}
	pr, err := store.PullRequest(ctx, tx, key)
	if err != nil || pr == nil {
		return nil, err
	}
	pr.MergeForward = &asked
	if err := store.PutPullRequest(ctx, tx, *pr); err != nil {
		return nil, err
	}
	return &asked, nil
}

// mergeForwardMessageOf is the merge-forward message of step to role about the record's ask of pr's
// head, naming the phase issue stands at, why, and the newest read of GitHub the record holds of
// the head (record.PullRequest.MergeState and StateReadAt, dispatch://INFRA-2833 R3): the ask
// queueAsk queues and the merger's hold holdMerger queues.
func mergeForwardMessageOf(issue record.Issue, pr record.PullRequest, role claim.Role, step record.MergeForwardStep, why record.ConflictWhy) record.MergeForwardMessage {
	return record.MergeForwardMessage{Role: role, Step: step, Head: pr.MergeForward.Head, Base: pr.MergeForward.Base,
		Phase: record.StandsAt(issue), PullRequest: pr.Number, Why: why, MergeState: pr.MergeState, StateReadAt: pr.StateReadAt.UTC()}
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
// lingers (record.TreeLingers, where every role is suspended) records and tells nothing. The
// landing ends the take hold the owed merge put on the issue's adopting takes (putPullRequest,
// mergeForwardEnds), so each take it refused, the asked role's own among them, is offered again.
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
// already queued. It is exported for that executor. The pull request is its row alone
// (record.Store.PullRequestRow): the executor names its repository and writes nothing of it.
func MergeForwardAt(ctx context.Context, store record.Store, tx pgx.Tx, key string, role claim.Role, step record.MergeForwardStep, head string) (*record.Issue, *record.PullRequest, string, error) {
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
func mergedForwardRefusal(issue record.Issue, role claim.Role, reported phase.Phase, commit string, pr *record.PullRequest) (intake.Result, bool) {
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
