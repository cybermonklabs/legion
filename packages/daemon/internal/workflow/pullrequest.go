package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// putPullRequest records pr and marks its issue moved where the write is news of the pull
// request rather than the daemon reconciling what it already knew (pullRequestNews, and the rule
// in engine.go's dispatchIssue). Every write of a pull request row in this package goes through
// it but two: the row is written from thirteen places — a pull request opened, reopened,
// synchronized, pushed to, closed or merged, a CI settlement, a mergeability read and the end of
// its second read (conflictConfirmDone), a review, a merge forward's ask (mergeForward), its
// landing (mergeForwardLanded) and its clearing (clearTaskMergeForward, clearMergeForwards) — and a write site added
// later would otherwise have to remember the mark. Routing them through
// one function is what makes forgetting it hard rather than easy.
//
// The two exceptions write the row through the store themselves, and each says why where it
// stands. The first is the listener's duplicate of a settlement the pass already built
// (review.go's checks), which writes the fence the daemon read with record.Store.PutPullRequest and
// decides nothing: the fence is part of the judgement, so the test below would read that write as
// news, and the daemon recording what it has read of GitHub is no movement of the work. The second
// is a push onto the base branch (base_push.go's basePush), which records the commit that push left
// the base at and nothing else (record.Store.SetPullRequestBaseMoved): a base that moved is no fact
// of the pull request, so the test below names that field nowhere and this helper would mark
// nothing for it either, and the walk the write sits in touches every pull request recorded on the
// base with each row in hand already, so a second read apiece would buy nothing. A new write site
// that only records a read belongs with those two; every write that changes what the record knows
// of the pull request belongs here.
//
// The row the write replaces is read first, by its own key in the caller's transaction
// (replacedPullRequest), so the test has both sides. That read is the cost of never mistaking a
// repeat for news: GitHub redelivers a webhook, and the daemon's own passes rebuild a push or a
// settlement whose webhook was lost, so the same fact reaches this code twice as a matter of
// course. It is read as its row alone (record.Store.TaskPullRequestRows), with none of what the
// pull request remembers of its settled heads: the test reads none of that, and the record written
// is pr, never this one.
//
// The issue and its branch are the row's own (record.PullRequest.Issue and Branch, the table's
// key), so the helper needs no second argument and cannot be handed the wrong key. The moment is
// the engine's clock, as every other site's is: the fact is being applied now, whatever clock
// GitHub stamped it with, and the mark says when the daemon saw the issue move.
func (e *Engine) putPullRequest(ctx context.Context, tx pgx.Tx, pr record.PullRequest) error {
	recorded, err := e.replacedPullRequest(ctx, tx, e.store.TaskPullRequestRows, pr.Issue, pr.Branch)
	if err != nil {
		return err
	}
	if err := e.store.PutPullRequest(ctx, tx, pr); err != nil {
		return err
	}
	if recorded != nil && !pullRequestNews(*recorded, pr) {
		return nil
	}
	return e.store.MarkIssueMoved(ctx, tx, pr.Issue, e.now())
}

// replacedPullRequest is the row a write of the pull request of issue on branch replaces
// (record.Store.PutPullRequest, keyed by the issue and the branch): the issue's row on branch, nil
// for none, among the rows read reads of issue. A caller that carries what the row remembers of
// its settled heads into the record it writes reads the rows whole (record.Store.TaskPullRequests:
// pullRequestOpened's reopen); one that only compares the two reads them alone
// (TaskPullRequestRows: putPullRequest).
func (e *Engine) replacedPullRequest(ctx context.Context, tx pgx.Tx, read func(context.Context, pgx.Tx, string) ([]record.PullRequest, error), issue, branch string) (*record.PullRequest, error) {
	rows, err := read(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	if at := slices.IndexFunc(rows, func(pr record.PullRequest) bool { return pr.Branch == branch }); at >= 0 {
		return &rows[at], nil
	}
	return nil, nil
}

// pullRequestNews says whether the write of after over before is news of the pull request: the
// daemon's judgement of it changed (classify.JudgementChanged — its head, the head it checked,
// its failing and cancelled checks, its check runs, the commit statuses of the head it checked,
// and the settlement fence those were read from), its head run opened or closed, the pushes it is
// still holding changed, or one of the facts GitHub reports about the pull request itself
// changed: its state, the branch it merges into, the newest deciding review, GitHub's
// mergeability verdict and the merge state beside it, the head the daemon holds conflicting, and
// the merge commit.
//
// GitHub's merge state is named for the reason its mergeability is, and the two are read from
// one answer: the state says whether GitHub would merge this head now, which the tri-state
// cannot, and it is what an approval standing on the head waits for (review.go's reviewRound). A
// read that moves it is GitHub answering something other than it answered last time — a check
// the base requires that has since run, a base that has moved under the head — and the ones that
// hold an approved head are told to the architect and the reviewer. So a read that changes the
// state alone and a read that changes the verdict beside it are one kind of fact, and are marked
// alike. The base the pull request merges into is named for the same reason: a pull request
// retargeted onto another branch, and the first read to fill the base of one opened before the
// record kept one, each leave the record holding something of GitHub's it did not hold before.
//
// What it leaves out is every part of the row that is the daemon's own bookkeeping of a fact it has
// already taken: the moves a push spends as it is reconciled (record.PullRequest.Moves,
// classify.ApplyPushDropping), GitHub's clock of its last change to the pull request
// (HeadUpdatedAt), and the commit the latest push left the base at (BaseMoved), which is a fact of
// the base branch and says only which base the recorded mergeability was read against — the push
// that writes it is no news of the pull request (basePush), and the read that clears it clears a
// note the daemon left itself. A redelivered webhook, and a webhook of a push or a settlement one
// of the daemon's own passes rebuilt first, write those and nothing else; marked as movements they
// would refresh the quiet time of a tree that has not moved since its work ended, and that tree
// would never be found (record.QuietTrees). The moment of the daemon's last answering read
// (StateReadAt) is left out too: a read that only confirms what is recorded writes that field
// alone, and it says how old the daemon's knowledge of the head is, never that the pull request
// moved. Clearing it is no news on its own account either: a write that clears it is judged by what
// else it changes (record.PullRequest.StateReadAt says what stamps, clears and carries it). The
// daemon's own suites hold that line: a late push webhook is asserted to change nothing of the
// issue record (internal/daemon's lateWebhookPushDecidesNothing), and a settlement that decides
// nothing — the listener's redelivery and its settlement of a commit that is not the head while a
// head run is open, the one that closes that run among them — is asserted to move no issue
// (internal/admit's TestTheRecordKeepsWhenAnIssueLastMoved, its case of the listener's settlement
// that decides nothing). The same test holds that a merge-state read that only confirms what is
// recorded moves no issue either.
//
// Two fields are left out because they are the daemon's bookkeeping of its own reads and asks, not
// news of the pull request: the first conflicting read still waiting for its second
// (record.PullRequest.ConflictRead) and the merge forward the daemon asked for (MergeForward). A
// read that writes or releases the first read writes GitHub's verdict with it, which is named
// above whenever it changed, and the confirmation writes the head the daemon holds conflicting,
// which is named too; the ask is the daemon's own act on a conflict it already recorded, its
// landing rides on the synchronize that moved the head, which is named above, its clearing on a
// move of the issue or its leave, each a movement of its own, and the end of a second read that
// decided nothing (workflow's conflictConfirmDone) is a note the daemon leaves itself, as
// clearing BaseMoved is.
//
// A field added to record.PullRequest later is not news to this test until it is named here. The
// direction that costs something is this one — an unnamed field that is news leaves a tree
// reading quieter than it is, which is one notice to a controller — while naming a field that is
// not news loses the stopped tree the measure exists to find.
func pullRequestNews(before, after record.PullRequest) bool {
	return classify.JudgementChanged(before, after) ||
		(before.Run == nil) != (after.Run == nil) ||
		!slices.Equal(before.Pushes, after.Pushes) ||
		before.State != after.State ||
		before.Base != after.Base ||
		before.ReviewSeen != after.ReviewSeen ||
		before.Mergeability != after.Mergeability ||
		before.MergeState != after.MergeState ||
		before.ConflictingHead != after.ConflictingHead ||
		before.MergeSHA != after.MergeSHA
}

// pullRequestOpened records the pull request a branch opened, and a reopen is the same pull
// request: it keeps its newest review, which orders every review it will have, and its pushes and
// head run, which are facts about commits. A reopen at
// another head than the recorded one - commits pushed while the pull request was closed, which no
// synchronize reports - is judged as a synchronize to that head would be
// (classify.AdvancePullRequestHead). A new generation
// deletes a pull request that is not open, and all of them with it. An issue that waits to close on
// its recorded merge (awaitsMerge) keeps that merge: GitHub never sends the merge again, so a pull
// request opened from its branch is not recorded, while an issue set back to todo reruns and its
// branch's new pull request replaces the merged one. The pull request is recorded on its branch,
// in place of the row of that branch (replacedPullRequest), so the pull request of one task of an
// issue never takes the place of another's, and it is the pull request of the task whose branch it
// is on (viewOfPullRequest): the merge it waits on is the task's, and it moves the task where its
// implementer's completion waits for it. It is recorded only on a branch a task of its issue holds
// (issueForBranch): one on any other branch of an issue's shape, a reopen or a redelivered open of
// a pull request whose row the record deleted among them, is none of the issue's. And it is
// recorded only when its head is a branch of its own repository (fact.HeadRepo is fact.Repo, as
// sameRepository compares names): a fork's pull request, which anyone may open and whose branch
// may carry any name, an issue's or a task's among them, is none of the issue's, and neither is
// one whose source named no repository for the head, which no source can show is the issue's.
func (e *Engine) pullRequestOpened(ctx context.Context, tx pgx.Tx, fact intake.PullRequestOpened) (intake.Result, error) {
	if !e.sameRepository(fact.HeadRepo, fact.Repo) {
		e.logOnCommit(ctx, "workflow: a pull request whose head is no branch of its own repository is not recorded",
			"repo", fact.Repo, "pullRequest", fact.Number, "branch", fact.Branch, "headRepo", fact.HeadRepo)
		return intake.Result{}, nil
	}
	issue, err := e.issueForBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	pr := record.PullRequest{Issue: issue.Key, Repo: fact.Repo, Number: fact.Number, Branch: fact.Branch, Base: fact.Base, HeadSHA: fact.HeadSHA,
		HeadUpdatedAt: fact.UpdatedAt, Failing: []string{}, State: record.PullRequestOpen}
	vs, ok, err := e.scopeOfPullRequest(ctx, tx, *issue, pr)
	view := vs.issue
	if err != nil {
		return intake.Result{}, err
	}
	recorded, err := e.replacedPullRequest(ctx, tx, e.store.TaskPullRequests, issue.Key, fact.Branch)
	if err != nil {
		return intake.Result{}, err
	}
	// An opened or reopened older than the recorded pull request's clock is a late redelivery,
	// whichever pull request it names: a branch can carry a newer pull request after a merge or a
	// park and rerun, and an earlier one's late event must not replace it. The workflow keeps one
	// pull request per branch and assumes one is open at a time; two open at once from one branch,
	// on different bases, are not modelled, and this fence can then keep the earlier one, whose
	// close still tells the architect.
	if recorded != nil && classify.LateLifecycle(fact.UpdatedAt, recorded.HeadUpdatedAt) {
		return intake.Result{}, nil
	}
	// A new generation deletes a pull request that is not open and keeps its clock
	// (record.Store.ClearedPullRequestClock), so an opened or reopened of it no newer than that
	// clock - late, or replayed from a message the intake gave up on, however late - finds no row
	// for the fence above and would bring it back open at the head it had then. It records nothing.
	// A reopen newer than the clock is a person reopening it since, and applies. The clock is read
	// under the name the fact carries and then under the other name of a configured rename
	// (ghrepo.Under): its row keeps the name it was cleared under until the one-time rename pass
	// writes the pair's `to` over it with the issue's other rows (record.RenameIssueRepository). A
	// clock missing on either side fences nothing, as LateLifecycle's does.
	if recorded == nil {
		cleared, err := ghrepo.Under(e.cfg.RepositoryRenames, fact.Repo, func(name string) (time.Time, error) {
			return e.store.ClearedPullRequestClock(ctx, tx, name, fact.Number)
		})
		if err != nil {
			return intake.Result{}, err
		}
		if !fact.UpdatedAt.IsZero() && !cleared.IsZero() && !fact.UpdatedAt.After(cleared) {
			e.logOnCommit(ctx, "workflow: a late opened of a pull request a new generation cleared records nothing",
				"issue", issue.Key, "repo", fact.Repo, "pull_request", fact.Number, "updated_at", fact.UpdatedAt, "cleared_clock", cleared)
			return intake.Result{}, nil
		}
	}
	if recorded != nil && recorded.State == record.PullRequestMerged && recorded.Number != fact.Number && awaitsMerge(view) {
		e.logOnCommit(ctx, "workflow: a pull request opened from a branch whose issue waits to close on its merge is not recorded",
			"issue", issue.Key, "merged", recorded.Number, "opened", fact.Number)
		return intake.Result{}, nil
	}
	if recorded != nil && e.sameRepository(recorded.Repo, fact.Repo) && recorded.Number == fact.Number {
		// GitHub sends opened once per pull request, so an opened for one already recorded is a
		// redelivery whatever its clock; a reopen's clock is left to the fence above. GitHub never
		// reopens a merged pull request, so a reopen of one is older than the merge, which carries
		// no clock to fence it. A reopen of the pull request the record holds open, at the record's
		// clock or before it, is the reopen that opened it delivered again - its webhook after the
		// pass applied its read, or a redelivery - since a close between the two would have closed
		// the record: applied again it would start the CI settlement afresh and disarm auto-merge
		// once more. A reopen newer than an open record follows a close that never arrived, and
		// applies.
		if !fact.Reopened || recorded.State == record.PullRequestMerged ||
			(recorded.State == record.PullRequestOpen && !fact.UpdatedAt.After(recorded.HeadUpdatedAt)) {
			return intake.Result{}, nil
		}
		// The reopen keeps the pushes - facts about commits - and the head run, so a push that
		// arrives after the reopen settles the move the reopen made as it would have had it
		// arrived first (classify.Reopen); the check fence is emptied. It keeps what the record
		// remembers of the heads settled before (record.PullRequest.SettledHeads), so the next
		// settlement of the head it reopens at is weighed against that head's fence as it stood
		// before the close, and an earlier one, delivered late, is still stale (classify's
		// SettlementFor).
		carried := classify.Reopen(*recorded, fact.HeadSHA)
		pr.ReviewSeen = recorded.ReviewSeen
		pr.Pushes, pr.Moves, pr.Run, pr.SettledHeads = carried.Pushes, carried.Moves, carried.Run, carried.SettledHeads
		// The reopen keeps the base and the push onto it the next read still owes an answer for: a
		// pull request reopens onto the base it was opened on, and a fact that names none (a
		// listener that predates the field) must not forget the recorded one.
		pr.BaseMoved = recorded.BaseMoved
		if pr.Base == "" {
			pr.Base = recorded.Base
		}
		pr.HeadUpdatedAt = classify.LatestClock(recorded.HeadUpdatedAt, fact.UpdatedAt)
	}
	if err := e.putPullRequest(ctx, tx, pr); err != nil {
		return intake.Result{}, err
	}
	if !ok || view.Phase != phase.Implementing {
		// A reopen lands under whatever phase the issue stands in, a reviewing round with an
		// approval standing among them, and GitHub recomputes the merge state of a pull request
		// it reopens; the read for it is asked for once, after this handler (Apply's
		// askMergeState).
		return intake.Result{}, nil
	}
	row, err := e.stepRow(ctx, tx, vs, claim.RoleImplementer)
	if err != nil || !row.Completed() {
		return intake.Result{}, err
	}
	return intake.Result{}, e.implementationReady(ctx, tx, vs, row, &pr)
}

// pullRequestSynchronized records the pull request's new head and what the move leaves standing
// (classify.AdvancePullRequestHead). It reads no CI result: a settlement moves nothing, in any
// phase, and the roles that work the issue were told what it said when it arrived (checks,
// dispatch://INFRA-714/spec R1). In reviewing the head's arrival re-asks the round (settleRound):
// any push may change code, so an approval of the previous head does not approve the new one
// (classify.ApprovalStands), whether the push that left it is known yet or not. The push's own
// handler re-asks it again when it arrives, and a round stuck the same way at the same head is
// told once (stuckAs). A synchronize that moves nothing - a redelivery, or one carrying no head -
// changes nothing the round reads, and applies and re-asks nothing. A synchronize of a pull
// request recorded merged changes nothing either: GitHub synchronizes only an open pull request,
// so one observed after the merge is older than it, a late redelivery the merge's missing clock
// cannot fence, as a close or a reopen of a merged pull request is (closed, pullRequestOpened). A
// synchronize that moves no head because it is older than the newest applied
// (classify.LateLifecycle, by GitHub's clock alone, as the release before), or because it returned
// the branch to the recorded head, still names the push its move took, by its sender, while a head
// run is open (classify.NameMove): an arrival whose synchronize said it moved from a head the
// record never held waits for it. Late, it records that alone, and moves and sets nothing else. In
// implementing a head that arrives once the implementer has completed is the pull request its
// completion waited for.
//
// A head that moves off the one a merge forward was asked of is that merge landing, in whatever
// phase the issue stands (mergeForwardLanded, dispatch://INFRA-4374 R1): the record keeps the new
// head as the merge's, and the phase's worker and, where the task's reviewer step already ran, the
// reviewer are told. It is
// recorded before the phase's own handling below, which goes on as it would for any move.
func (e *Engine) pullRequestSynchronized(ctx context.Context, tx pgx.Tx, fact intake.PullRequestSynchronized) (intake.Result, error) {
	pr, err := e.pullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || pr == nil || pr.State == record.PullRequestMerged {
		return intake.Result{}, err
	}
	prior := *pr
	named := record.Named{Before: fact.Before, SHA: fact.HeadSHA, Sender: fact.Sender, At: fact.UpdatedAt}
	if classify.LateLifecycle(fact.UpdatedAt, pr.HeadUpdatedAt) {
		if *pr = classify.NameMove(*pr, named); pr.Run == prior.Run {
			return intake.Result{}, nil
		}
		return intake.Result{}, e.putPullRequest(ctx, tx, *pr)
	}
	// GitHub names the base on every pull-request event, so a synchronize is also where a base
	// retargeted on GitHub reaches the record; one that names none (a listener that predates the
	// field, or the daemon's own read of the head) leaves the recorded base as it is, for the
	// daemon's next read of the pull request to fill. A late synchronize - a redelivery, or one
	// replayed from a message the intake gave up on, however late - records no base, even where it
	// names the move a head run waits for above: the base it names is the one the pull request had
	// when it was sent, which a newer event may have retargeted since.
	if fact.Base != "" {
		pr.Base = fact.Base
	}
	arrival := classify.Arrival{Before: fact.Before, Sender: fact.Sender, At: fact.UpdatedAt}
	moved := classify.MovesBy(*pr, fact.HeadSHA)
	if moved {
		*pr = classify.AdvancePullRequestHead(*pr, arrival, fact.HeadSHA)
	} else {
		*pr = classify.NameMove(*pr, named)
	}
	pr.HeadUpdatedAt = classify.LatestClock(pr.HeadUpdatedAt, fact.UpdatedAt)
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	scope, err := e.issueOfPullRequest(ctx, tx, *pr)
	if err != nil || scope == nil {
		return intake.Result{}, err
	}
	issue := &scope.issue
	if moved {
		if err := e.mergeForwardLanded(ctx, tx, *scope, pr); err != nil {
			return intake.Result{}, err
		}
	}
	switch issue.Phase {
	case phase.Reviewing:
		// A head that moved re-asks the round, whether its push is recorded yet or not: any push
		// may change code, so an approval of the head before it does not approve this one. GitHub
		// recomputes the merge state of a pull request whose head moved, and the read for it is
		// asked for after this handler (Apply's askMergeState).
		if !moved {
			return intake.Result{}, nil
		}
		return intake.Result{}, e.reaskRound(ctx, tx, *scope, pr, prior, byPush)
	case phase.Implementing:
		row, err := e.stepRow(ctx, tx, *scope, claim.RoleImplementer)
		if err != nil || !row.Completed() {
			return intake.Result{}, err
		}
		return intake.Result{}, e.implementationReady(ctx, tx, *scope, row, pr)
	}
	return intake.Result{}, nil
}

// push records a push to the pull request's branch (classify.ApplyPushDropping) and re-asks the
// review round (reaskRound). It records nothing for a push whose After is record.NoCommit: that is
// GitHub's deletion of the branch, which names no code. intake's decodePush makes no fact of a
// deletion, and this test holds for a Push from any other source too. It records nothing for a
// push to a pull request recorded merged either, since the merge has landed: a push after it is
// not code that merged, and once the merge named its head that head is final (merged). The limit
// is a merge that names no head, which no source sends in this release (intake's decodePullRequest
// and daemon's mergeQueueRead both name it): a push made before such a merge but delivered after
// it is not recorded, so it does not take back an approval of the head the record holds, although
// it may be the code that merged.
func (e *Engine) push(ctx context.Context, tx pgx.Tx, fact intake.Push) (intake.Result, error) {
	if fact.After == record.NoCommit {
		return intake.Result{}, nil
	}
	pr, err := e.pullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || pr == nil || pr.State == record.PullRequestMerged {
		return intake.Result{}, err
	}
	prior := *pr
	var dropped classify.Dropped
	*pr, dropped = classify.ApplyPushDropping(*pr, record.ClassifiedPush{SHA: fact.After, Before: fact.Before,
		ByReviewApp: e.byReviewApp(fact.Pusher), Sender: fact.Sender})
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	// Past its bound the record lets the oldest push waiting for a link go (classify.ApplyPush): a
	// webhook behind it never arrived, and the move it would have linked into is judged without it.
	// A move past its bound on the pushes of its two heads made again lets the oldest go too: the
	// next arrival by that move takes the oldest kept. Past the bound on pushes of the same two heads
	// on their way, before their move is made, the oldest after the first goes.
	for _, gone := range dropped.Waiting {
		e.warnOnCommit(ctx, "workflow: a pull request holds the most pushes waiting for a link; the oldest is let go",
			"issue", pr.Issue, "pullRequest", pr.Number, "before", gone.Before, "after", gone.SHA)
	}
	if gone := dropped.Repeat; gone != nil {
		e.warnOnCommit(ctx, "workflow: a move holds the most pushes of its two heads made again; the oldest is let go",
			"issue", pr.Issue, "pullRequest", pr.Number, "before", gone.Before, "after", gone.SHA)
	}
	if gone := dropped.Copy; gone != nil {
		e.warnOnCommit(ctx, "workflow: a pull request holds the most pushes of the same two heads on their way; the oldest after the first is let go",
			"issue", pr.Issue, "pullRequest", pr.Number, "before", gone.Before, "after", gone.SHA)
	}
	// A push changes which settlement stands for the head, and that moves nothing: a CI result
	// moves no issue, and the roles that work it were told what the settlement said when it
	// arrived (checks, dispatch://INFRA-714/spec R1). In reviewing the push can be what shows the
	// head the reviewer approved is about to be replaced, so the round is settled again; the
	// round's re-ask is reviewing's alone.
	scope, err := e.issueOfPullRequest(ctx, tx, *pr)
	if err != nil || scope == nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.reaskRound(ctx, tx, *scope, pr, prior, byPush)
}

// reaskRound re-asks issue's review round (settleRound) after a fact that changed the pull
// request - a push, the head it left arriving, GitHub's merge state, the merge - with by the
// fact's summary. pr is the pull request as the fact left it, and prior is the pull request as
// the fact found it. The round as prior had it is the before a round stuck the same way is
// deduped against (stuckAs), so a fact at the head the round was already told stuck at tells
// nothing again. An issue outside reviewing has no round to re-ask, and nothing is read for it.
func (e *Engine) reaskRound(ctx context.Context, tx pgx.Tx, s TaskScope, pr *record.PullRequest, prior record.PullRequest, by string) error {
	issue := s.issue
	if issue.Phase != phase.Reviewing {
		return nil
	}
	reviewer, err := e.stepRow(ctx, tx, s, claim.RoleReviewer)
	if err != nil {
		return err
	}
	_, err = e.settleRound(ctx, tx, s, reviewer, pr, e.reviewRound(issue, reviewer, &prior), by)
	return err
}

// merged records the pull request merged, with its merge commit, whatever the issue's phase, and
// closes an issue that awaits its merge (awaitsMerge; closeMerged): one in merging, where the
// merger merges and the merge is what ends that phase, and one held from merging, by a failed
// merger's claim, an escalation, its owner's stop of its task at its merge or the upgrade's data
// move, since the merge is the one fact GitHub never sends again and it puts the work on main. A
// merge before merging is the architect's to act on, as the shipped daemon routes it: it is told,
// and the merge closes the issue once it reaches merging (transition). A redelivered merge
// changes nothing.
//
// It records the head the merge names, as a close does (closed). A synchronize or a push of that
// head can be lost or wait behind a backlog, and after the merge nothing else moves the record's
// head: the synchronize handler drops a merged record, and the daemon's reads follow only an open
// pull request's head (daemon's followHead). It then drops the pushes the record holds, since the
// head the merge names is final and no push from it is code on its way (classify.CodeOnItsWay). A
// merge that names no head leaves the recorded head and its pushes.
//
// In reviewing, after the architect's notice, the merge settles the review round (settleRound):
// an approval that stands on the head the pull request merged at ends the round and moves the
// issue to its task's next step (accepting, in the default steps), since a merged pull request
// leaves the round nothing to wait on (reviewRound). Without this, an approval that came before
// the merge would hold the issue in
// reviewing: no read of the merged pull request ever comes (mergeStateOwed), and once the
// reviewer has completed no other fact settles the round. An approval of a head other than the
// one the merge names ends no round: once its reviewer has completed, the round is stuck on other
// code (stuckApprovedOtherCode), and the architect is told once.
func (e *Engine) merged(ctx context.Context, tx pgx.Tx, fact intake.PullRequestMerged) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil || pr.State == record.PullRequestMerged {
		return intake.Result{}, err
	}
	prior := *pr
	if fact.HeadSHA != "" {
		if fact.HeadSHA != pr.HeadSHA {
			*pr = classify.AdvancePullRequestHead(*pr, classify.Arrival{}, fact.HeadSHA)
		}
		pr.Pushes = nil
	}
	pr.State, pr.MergeSHA = record.PullRequestMerged, fact.MergeSHA
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	if err := e.closeOtherTaskPullRequests(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	scope, err := e.issueOfPullRequest(ctx, tx, *pr)
	if err != nil || scope == nil {
		return intake.Result{}, err
	}
	issue := &scope.issue
	if awaitsMerge(*issue) {
		return intake.Result{}, e.closeMergedTelling(ctx, tx, *scope, pr)
	}
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-merged", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("pull request #%d merged while %s was in %s, not merging", pr.Number, issue.Key, issue.Phase)}); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.reaskRound(ctx, tx, *scope, pr, prior, byMerge)
}

// closeOtherTaskPullRequests queues the close of each other open pull request of merged's issue
// (record.TaskPullRequestsClose), each with the comment `Merged in #<n>.`, where merged, the pull
// request the fact just recorded merged, is the pull request of the task that merges
// (record.Store.MergePullRequest): its merge put the work of every task on the default branch, and
// GitHub leaves the pull request of each other task open. It closes only the pull request of a
// task of the issue's plan, on a branch a task holds (record.StoredPlan.HoldsBranch, the rule the
// recorder reads, issueForBranch): a row on any other branch is no task's, and the implement App
// closes no pull request Legion did not open. The merge of any other task's pull request closes
// nothing, and neither does the merge of an issue whose other pull requests are closed or that has
// none, as an issue of one task has none. It is written in the merge's own transaction, so the row
// is queued once, as GitHub sends the merge once.
func (e *Engine) closeOtherTaskPullRequests(ctx context.Context, tx pgx.Tx, merged record.PullRequest) error {
	sink, err := e.store.MergePullRequest(ctx, tx, merged.Issue)
	if err != nil || sink == nil || sink.Number != merged.Number || !e.sameRepository(sink.Repo, merged.Repo) {
		return err
	}
	issue, err := e.store.Issue(ctx, tx, merged.Issue)
	if err != nil || issue == nil {
		return err
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return err
	}
	prs, err := e.store.TaskPullRequests(ctx, tx, merged.Issue)
	if err != nil {
		return err
	}
	var open []int
	for _, pr := range prs {
		if pr.State == record.PullRequestOpen && pr.Number != merged.Number && stored.HoldsBranch(issue.Key, pr.Branch) {
			open = append(open, pr.Number)
		}
	}
	if len(open) == 0 {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, record.TaskPullRequestsClose{Generation: issue.Generation, Numbers: open,
		Comment: fmt.Sprintf("Merged in #%d.", merged.Number)})
}

// waitingForTree is what the architect that owns a merged issue is told when the merge of its pull
// request finds issues beneath it still in the workflow (for a root, the rest of its tree): the
// close is waiting, and for whom. It also says how a child leaves without its own merge, which is
// the architect's status write (record.EndChildRule), and that such a write counts once the daemon
// has taken it (record.StatusNotTakenYet).
func waitingForTree(issue string, number int, waiting []string) string {
	return fmt.Sprintf("pull request #%d merged, and %s's close is waiting for %s, still in the workflow. %s closes once the last of them leaves the workflow.",
		number, issue, strings.Join(waiting, ", "), issue) + " " + record.StatusNotTakenYet + " " + record.EndChildRule
}

// closeMergedTelling closes issue on its merged pull request (closeMerged) and, when the close
// waited for the issues beneath it instead, tells the architect that owns the issue once. It is
// the two places a wait is newly entered — the merge arriving at an issue that awaits it (merged),
// and an issue reaching merging on a pull request already merged (transition) — and nowhere else:
// notices never de-duplicate (notice writes a row per call) and closeMerged is re-entered on the
// departure of every issue beneath it (closeAwaitingMerge) and on every boot (sweepMerged), so
// those two tell nobody.
func (e *Engine) closeMergedTelling(ctx context.Context, tx pgx.Tx, s TaskScope, pr *record.PullRequest) error {
	issue := s.issue
	waiting, err := e.closeMerged(ctx, tx, s, pr)
	if err != nil || len(waiting) == 0 {
		return err
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-merged", Role: claim.RoleArchitect,
		Reason: waitingForTree(issue.Key, pr.Number, waiting)})
}

// closeMerged closes issue, which awaits its merge in merging (or is held from merging, a hold the
// close clears: a failed merger's claim, an escalation, a stopped task's or the upgrade's data
// move's), on its merged pull request pr, and reports
// the issues it waited for instead, empty when it closed. In a running tree the close is the
// table's row to done (transition), which leaves: a child's close retires every claim of it but
// that of the role the project's `production_check` names, and opens its production check in that
// role's live session, and retires every claim and opens no check where that setting is off; a root's close lingers
// its tree and opens its check behind the linger's rows (leave). A merge is the one fact GitHub
// never sends again, so one that lands while the tree lingers, where no transition runs, still
// closes the issue: it writes what leave would that a lingering tree allows — the phase done, the
// status done, the pull request disarmed, the architect above a child told child-closed — and
// opens the check, whose first look is a wake, since the linger suspended that role. It retires
// nobody and suspends nothing: the linger's own rows already did, and the tree's close retires
// every member's claims. The boot's sweep closes a migrated issue the same way (booted).
//
// A merged issue's close waits for the issues beneath it, on the one set both acceptance rules
// read (OpenBeneath): for a tree root that is every other member of the tree still in the
// workflow, read from the flat tree rather than the parent relation because the linger its close
// begins stops every member whatever it hangs under — a child's finished phase would be refused
// 409 TREE_LINGERING and its work lost — and for an issue below the root it is the issues whose
// chain of recorded parents reaches it, whose merges its own head is meant to carry. The set
// leaves out an issue the daemon itself holds at admitted beneath this one — without that the
// two would wait on each other for ever, as OpenBeneath's own comment sets out. The departure of
// any of them re-reads this wait (leave's closeAwaitingMerge), so the close happens as soon as
// the last one leaves. The wait is kept inside the branch where the tree does not linger: a
// member of a tree that already lingers must still close at its merge, which suspends nothing
// further and is the one thing that still moves there.
func (e *Engine) closeMerged(ctx context.Context, tx pgx.Tx, s TaskScope, pr *record.PullRequest) ([]string, error) {
	// An issue held from merging is awaiting its merge still (awaitsMerge, the one
	// predicate written over that wait), and the close goes on from the phase itself, so every
	// row below reads one shape.
	if !awaitsMerge(s.issue) {
		return nil, nil
	}
	// Only the copy in hand leaves the hold here: the task keeps its place, its merge, which a hold
	// keeps too (moveTask), and the close below writes the move to done.
	s.issue.Phase, s.issue.Hold = phase.Merging, nil
	issue := s.issue
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return nil, err
	}
	if !lingers {
		waiting, err := e.openBeneath(ctx, tx, issue)
		if err != nil {
			return nil, err
		}
		if len(waiting) > 0 {
			e.logOnCommit(ctx, "workflow: a merged issue's close waits for the issues beneath it", "issue", issue.Key, "tree", issue.Tree,
				"pullRequest", pr.Number, "waitingFor", strings.Join(waiting, ", "))
			return waiting, nil
		}
		return nil, e.transition(ctx, tx, s, TriggerPullRequestMerged, record.TaskPlace{}, record.PhaseRow{}, pr, "")
	}
	w := s.walk
	row, ok := w.row(issue.Phase, TriggerPullRequestMerged, record.TaskPlace{}, Snapshot{Phase: issue.Phase})
	if !ok {
		return nil, nil
	}
	status := PhaseStatus(row.To)
	if err := e.status(ctx, tx, issue, status, ""); err != nil {
		return nil, err
	}
	// The merge ends the merge step the linger left open, passed: no transition runs in a lingering
	// tree, so this move names that end itself, as a move of the table does (nameMoveEnd).
	if err := e.nameMoveEnd(ctx, w, issue, TriggerPullRequestMerged, placeNamed(row.ToPlace, row.To), record.PhaseRow{}); err != nil {
		return nil, err
	}
	if err := e.moveTask(ctx, tx, &s, row.To, row.ToPlace, record.TaskDone); err != nil {
		return nil, err
	}
	s.issue.Hold = nil
	if status != "" {
		s.issue.Status = status
	}
	if _, err := e.putScope(ctx, tx, &s); err != nil {
		return nil, err
	}
	issue = s.issue
	e.logOnCommit(ctx, "workflow: a lingering tree's member closed at its merge", "issue", issue.Key, "tree", issue.Tree, "pullRequest", pr.Number, "status", issue.Status)
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is done", issue.Key)}); err != nil {
			return nil, err
		}
	}
	return nil, e.openCheck(ctx, tx, issue, *pr, true)
}

// closed records the pull request closed unmerged, which a re-admitted generation drops, and tells
// the architect, whose decision it is whether the work is reopened, reassigned, or cancelled, as the
// shipped daemon routes it. It records the head the close carries, which every synchronize before
// it left, and keeps the close's clock when it is the later one, so a reopen or a synchronize older
// than the close, redelivered late, changes nothing. A close that finds the pull request already
// closed records its head and clock too. When it is newer than the record, it is a second close
// whose reopen has not been delivered yet, and the architect is told; at the record's clock or with
// no clock it is a redelivery, and it tells the architect nothing more. A close of a merged pull
// request changes nothing: GitHub never closes one, so the close is older than the merge, which
// carries no clock to fence it.
func (e *Engine) closed(ctx context.Context, tx pgx.Tx, fact intake.PullRequestClosed) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil || pr.State == record.PullRequestMerged || classify.LateLifecycle(fact.UpdatedAt, pr.HeadUpdatedAt) {
		return intake.Result{}, err
	}
	repeated := classify.RepeatedClose(*pr, fact.UpdatedAt)
	if fact.HeadSHA != "" && fact.HeadSHA != pr.HeadSHA {
		*pr = classify.AdvancePullRequestHead(*pr, classify.Arrival{}, fact.HeadSHA)
	}
	pr.State = record.PullRequestClosed
	pr.HeadUpdatedAt = classify.LatestClock(pr.HeadUpdatedAt, fact.UpdatedAt)
	if err := e.putPullRequest(ctx, tx, *pr); err != nil || repeated {
		return intake.Result{}, err
	}
	scope, err := e.issueOfPullRequest(ctx, tx, *pr)
	if err != nil || scope == nil {
		return intake.Result{}, err
	}
	issue := &scope.issue
	return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-closed-unmerged", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("pull request #%d closed without merging while %s was in %s", pr.Number, issue.Key, issue.Phase)})
}

// dequeued writes the daemon's one read of a pull request that GitHub reports left the merge
// queue (intake.PullRequestDequeued, record.MergeQueueRead), while the issue stands in merging at
// the head the event names (MergingAt). The event names no reason for the removal, since the
// listener drops GitHub's, and GitHub's merge queue reports a removal in the same second as the
// merge it makes, so the read decides and the event does not: a merged answer applies the merge,
// and any other answer tells the merger (mergeQueueReadDone). The read is an outbox row, as every
// effect of a fact is, so the event is acknowledged without waiting for GitHub and a restart
// before the read loses nothing.
//
// It moves no phase, writes no slot and wakes no claim: what a removal means is the merger's to
// decide (dispatch://INFRA-3274/spec R1). A removal at a head other than the one the record holds,
// of an issue that has left merging, or of a pull request that is not open writes nothing and is
// logged, as a read of GitHub's merge state for a head the record has moved past is
// (mergeability): the record has moved on since GitHub reported it.
func (e *Engine) dequeued(ctx context.Context, tx pgx.Tx, fact intake.PullRequestDequeued) (intake.Result, error) {
	pr, err := e.pullRequestRow(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	issue, _, why, err := MergingAt(ctx, e.store, tx, pr.Issue, fact.Number, fact.HeadSHA)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if why != "" {
		e.logOnCommit(ctx, "workflow: a pull request left the merge queue where its issue does not merge at that head; nothing is read",
			"issue", issue.Key, "pullRequest", fact.Number, "head", fact.HeadSHA, "why", why)
		return intake.Result{}, nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.MergeQueueRead{Number: fact.Number, Head: fact.HeadSHA}); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once",
		"issue", issue.Key, "pullRequest", fact.Number, "head", fact.HeadSHA)
	return intake.Result{}, nil
}

// mergeQueueReadDone writes the message that tells the issue's merger its pull request left the
// merge queue (record.MergeQueueRemoval), once the daemon's read of the pull request found no
// merge (intake.MergeQueueReadDone): GitHub answered it open or closed unmerged, or no read
// succeeded (Unread), which the message then says. The read ran outside any transaction, for up
// to two minutes while GitHub failed it, so the issue is judged again here by the rule the read
// row judged it by before it read (MergingAt): an issue that has left merging at that head
// since writes nothing, and is logged. The message is a row of its own rather than a publish of
// the read row's, because it waits until the merger's session holds its role, and a row that
// waited there would read GitHub again at each try (record.MergeQueueRead).
//
// It moves no phase, writes no slot and wakes no claim: the message's own row wakes a released
// merger when it runs (the daemon's merge-queue removal executor).
func (e *Engine) mergeQueueReadDone(ctx context.Context, tx pgx.Tx, fact intake.MergeQueueReadDone) (intake.Result, error) {
	issue, _, why, err := MergingAt(ctx, e.store, tx, fact.Issue, fact.Number, fact.Head)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if why != "" {
		e.logOnCommit(ctx, "workflow: the merge-queue read finished after the issue left merging at that head; the merger is not told",
			"issue", issue.Key, "pullRequest", fact.Number, "head", fact.Head, "unread", fact.Unread, "why", why)
		return intake.Result{}, nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.MergeQueueRemoval{Number: fact.Number, Head: fact.Head, Unread: fact.Unread}); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: the merger is owed word that its pull request left the merge queue",
		"issue", issue.Key, "pullRequest", fact.Number, "head", fact.Head, "unread", fact.Unread)
	return intake.Result{}, nil
}

// MergingAt reads in tx what the merging rule judges issue key by: the issue, whether its tree
// lingers (record.TreeLingers) and the pull request of its work's merge, the one of the task that
// merges (record.Store.MergePullRequest), which no other task's pull request is. It returns the
// issue and that pull request, with why the workflow no longer works the issue's merging phase at
// head of pull request number (leftMergingAt), "" while it does. An issue the record does not hold
// is a nil issue, no pull request and no reason. The facts that write the read and the merger's
// message (dequeued, mergeQueueReadDone) and the daemon's outbox rows that read the pull request
// and deliver the message read the issue through this one read, so all four judge it from the
// same records. It is exported for those outbox rows (the daemon's mergingAt). The pull request
// is its row alone (record.Store.MergePullRequestRow): no caller writes it back, and none reads
// what it remembers of its settled heads.
func MergingAt(ctx context.Context, store record.Store, tx pgx.Tx, key string, number int, head string) (*record.Issue, *record.PullRequest, string, error) {
	issue, err := store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, nil, "", err
	}
	lingers, err := record.TreeLingers(ctx, store, tx, issue.Tree)
	if err != nil {
		return nil, nil, "", err
	}
	pr, err := store.MergePullRequestRow(ctx, tx, key)
	if err != nil {
		return nil, nil, "", err
	}
	return issue, pr, leftMergingAt(*issue, lingers, pr, number, head), nil
}

// leftMergingAt says why the workflow no longer works issue's merging phase at head of pull
// request number, or "" while it does: the issue stands in merging itself, not held from it; it
// is in the workflow; its tree does not linger (lingers, record.TreeLingers); and the pull request
// the record holds for it (pr, nil for none) is number, open, at head. In each other state no
// merger is told of a removal from the merge queue at head: the workflow no longer works that
// phase at that head, and the merger of a lingering tree is suspended by its close and would
// never hold its role to hear it. The facts that write the read and the merger's message
// (dequeued, mergeQueueReadDone) and the daemon's outbox rows that read the pull request and
// deliver the message all judge by this one rule, through MergingAt, so no two of them disagree
// about an issue that left merging.
func leftMergingAt(issue record.Issue, lingers bool, pr *record.PullRequest, number int, head string) string {
	switch {
	case issue.Phase != phase.Merging:
		return fmt.Sprintf("the issue stands in %s, not merging", issue.Phase)
	case record.OutOfWorkflow(issue.Status):
		return fmt.Sprintf("the issue is out of the workflow at status %s", issue.Status)
	case lingers:
		return "the issue's tree lingers or has closed"
	case pr == nil:
		return "the record holds no pull request for the issue"
	case pr.Number != number:
		return fmt.Sprintf("the record holds pull request #%d for the issue", pr.Number)
	case pr.State != record.PullRequestOpen:
		return fmt.Sprintf("the pull request is %s", pr.State)
	case pr.HeadSHA != head:
		return fmt.Sprintf("the record holds the pull request at %s", pr.HeadSHA)
	}
	return ""
}

// pullRequestMissing tells the architect that the issue's implementer completed and GitHub shows
// no open pull request on its branch (intake.PullRequestMissing, the daemon's per-issue read of
// GitHub): the issue waits in implementing for a pull request record (implementationReady), and
// nothing else tells anybody it is missing - the implementer's completion was applied with GitHub
// unread, or the pull request it read was closed without merging since. The notice names the
// issue, the repository, the branch and the handoff commit, and says what to do. It tells nothing
// when the issue has left implementing, its implementer's handoff is another commit, or an open or
// merged pull request is recorded: the read asked GitHub before the record changed, or the merge
// told the architect (merged) and a completion moves the issue on it. The fact is of the task whose
// branch it names, read for that task (viewOfPullRequest), whose own pull request is the one that
// lets it move (pullRequestOf).
func (e *Engine) pullRequestMissing(ctx context.Context, tx pgx.Tx, fact intake.PullRequestMissing) (intake.Result, error) {
	recorded, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || recorded == nil {
		return intake.Result{}, err
	}
	// The pull request the fact says is missing is the one on the branch the fact names.
	missing := record.PullRequest{Issue: recorded.Key, Repo: fact.Repo, Branch: fact.Branch}
	ms, ok, err := e.scopeOfPullRequest(ctx, tx, *recorded, missing)
	issue := ms.issue
	if err != nil || !ok || issue.Phase != phase.Implementing {
		return intake.Result{}, err
	}
	row, err := e.stepRow(ctx, tx, ms, claim.RoleImplementer)
	if err != nil || row.HandoffCommit != fact.HandoffCommit {
		return intake.Result{}, err
	}
	pr, err := e.taskPullRequestRow(ctx, tx, ms)
	if err != nil || record.LetsImplementationMove(pr) {
		return intake.Result{}, err
	}
	return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pull-request-missing", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("%s's implementer completed at %s, and GitHub shows no open pull request on %s %s, so %s stays in implementing. "+
			"Tell its implementer (envoy_publish to its role topic) to open the pull request from %s; the daemon records it and moves %s to the next step of its task once it is open. "+
			"When the work should not go on, decide %s's phase as for a pull request closed without merging.",
			issue.Key, fact.HandoffCommit, fact.Repo, fact.Branch, issue.Key, fact.Branch, issue.Key, issue.Key)})
}
