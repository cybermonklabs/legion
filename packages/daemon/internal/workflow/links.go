package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// prName is pull request number of repo as a notice of a pull request of an issue's set names it,
// owner/name#number: the set holds pull requests of several repositories, so a number alone names
// none of them (dispatch://INFRA-5910 decision 10).
func prName(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

// sameLink is the test of a link that names the pull request link names, the repository compared
// without case as the record compares it.
func sameLink(link record.PullRequestLink) func(record.PullRequestLink) bool {
	return func(other record.PullRequestLink) bool {
		return other.Number == link.Number && strings.EqualFold(other.Repo, link.Repo)
	}
}

// takeLinks writes links, the pull requests an event of issue that names its links shows the issue
// linking (intake.DispatchIssue.Links), as the whole of the issue's links
// (record.Store.ReplacePullRequestLinks): Dispatch is the source, and the record keeps its copy
// (dispatch://INFRA-5910 decision 4). A link Dispatch no longer shows is removed (linkRemoved,
// decision 18), and a link it shows that the record did not hold is added (linkAdded, decision 8).
// A link that stays keeps its row as it stands, its refusal and its told mark with it. Where a link
// went or came, the links that hold the issue's close (setHolds) may have changed, so the close is
// judged again (judgeClose, decision 9): a link that went may have held it, and one that came holds
// it while its read is owed, which the judge tells the architect of where the close waits. It
// answers whether that judge closed the issue, which then stands done out of the workflow, so its
// caller applies nothing more of the event to it. It moves no phase of its own and writes no status:
// a link changes which pull requests are the issue's, never where its work stands, so a link to a
// done issue reopens nothing (D10).
//
// The read of a pull request the issue links newly, whose row the record does not hold yet, is
// owed: the link holds no row of the issue until that read records one (pullRequestLinkRead).
func (e *Engine) takeLinks(ctx context.Context, tx pgx.Tx, issue record.Issue, links []intake.PullRequestLink) (bool, error) {
	before, err := e.store.PullRequestLinksOf(ctx, tx, issue.Key)
	if err != nil {
		return false, err
	}
	given := make([]record.PullRequestLink, len(links))
	for at, link := range links {
		given[at] = record.PullRequestLink{Issue: issue.Key, Repo: link.Repo, Number: link.Number}
	}
	if err := e.store.ReplacePullRequestLinks(ctx, tx, issue.Key, given); err != nil {
		return false, err
	}
	removed, added := 0, 0
	for _, link := range before {
		if slices.ContainsFunc(given, sameLink(link)) {
			continue
		}
		if err := e.linkRemoved(ctx, tx, issue, link); err != nil {
			return false, err
		}
		removed++
	}
	for _, link := range given {
		if slices.ContainsFunc(before, sameLink(link)) {
			continue
		}
		if err := e.linkAdded(ctx, tx, issue, link); err != nil {
			return false, err
		}
		added++
	}
	if removed == 0 && added == 0 {
		return false, nil
	}
	judged, err := e.judgeClose(ctx, tx, issue.Key, nil, fmt.Sprintf("%d of its links went and %d came", removed, added))
	return judged.closed, err
}

// linkedRow is the row the record holds of pull request number of repo, whatever its issue, read
// under the name a link or a fact carries and then under the other name of its configured pair
// (ghrepo.Under), as a row alone (record.Store.PullRequestRowByNumber), nil for none.
func (e *Engine) linkedRow(ctx context.Context, tx pgx.Tx, repo string, number int) (*record.PullRequest, error) {
	return ghrepo.Under(e.cfg.RepositoryRenames, repo, func(name string) (*record.PullRequest, error) {
		return e.store.PullRequestRowByNumber(ctx, tx, name, number)
	})
}

// linkRemoved is the removal of link, a link of issue to a pull request that Dispatch no longer
// shows (dispatch://INFRA-5910 decision 18): the link's row has gone (takeLinks). A pull request
// that no task's branch holds in the issue's repository leaves the issue's set with its link
// (D2), so its row goes too, whether it is open or merged: no fact of it moves the issue after.
// A pull request on a task's branch keeps its row and stays in the set, as the task's own or as
// another pull request of the task (decision 5), which the stack's close then reaches at the
// issue's last merge (decision 11). A production check its merge opened stays open and runs: a
// check is the issue's, not the pull request's. A link the daemon refused as a fork's or as another
// issue's held no row of the issue, and its removal deletes nothing.
func (e *Engine) linkRemoved(ctx context.Context, tx pgx.Tx, issue record.Issue, link record.PullRequestLink) error {
	pr, err := e.linkedRow(ctx, tx, link.Repo, link.Number)
	if err != nil || pr == nil || pr.Issue != issue.Key {
		return err
	}
	stored, err := e.store.Plan(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return err
	}
	if stored.HoldsBranch(issue, pr.Repo, pr.Branch) {
		e.logOnCommit(ctx, "workflow: a removed link's pull request is on a task's branch, and stays in the issue's set",
			"issue", issue.Key, "pullRequest", prName(pr.Repo, pr.Number), "branch", pr.Branch, "state", pr.State)
		return nil
	}
	if err := e.store.DeletePullRequest(ctx, tx, pr.Repo, pr.Number); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a removed link's pull request leaves the issue's set, and its row is deleted",
		"issue", issue.Key, "pullRequest", prName(pr.Repo, pr.Number), "branch", pr.Branch, "state", pr.State)
	return nil
}

// linkAdded is link, a link of issue to a pull request that the record did not hold for issue
// before the event that names it (takeLinks). A pull request the record holds for another issue
// stays with that issue, and the link is refused (refuseLink, record.LinkRefusedOtherIssue,
// dispatch://INFRA-5910 decision 8): a pull request belongs to one issue, and the issue column of
// its row never moves. A pull request the record holds for issue already, a task's, is in its set
// now as linked too. One the record holds no row of is a link whose read is owed.
func (e *Engine) linkAdded(ctx context.Context, tx pgx.Tx, issue record.Issue, link record.PullRequestLink) error {
	pr, err := e.linkedRow(ctx, tx, link.Repo, link.Number)
	if err != nil || pr == nil || pr.Issue == issue.Key {
		return err
	}
	return e.refuseLink(ctx, tx, issue, link, record.LinkRefusedOtherIssue, pr.Issue)
}

// refuseLinkOfAnotherIssue refuses the link another issue holds to pr, a pull request the record
// has just recorded for its own issue as a task's (pullRequestOpened), as another issue's
// (refuseLink, record.LinkRefusedOtherIssue, dispatch://INFRA-5910 decision 8): the record holds
// the pull request for the issue that recorded it first, and the link of any other issue to it
// takes it into no set. That link held the linking issue's close while its read was owed
// (setHolds), and holds nothing once refused, so that close is judged again (judgeClose). A link of
// pr's own issue, a link already refused, and no link refuse nothing.
func (e *Engine) refuseLinkOfAnotherIssue(ctx context.Context, tx pgx.Tx, pr record.PullRequest) error {
	link, err := ghrepo.Under(e.cfg.RepositoryRenames, pr.Repo, func(name string) (*record.PullRequestLink, error) {
		return e.store.PullRequestLinkOf(ctx, tx, name, pr.Number)
	})
	if err != nil || link == nil || link.Issue == pr.Issue || link.Refused != "" {
		return err
	}
	linking, err := e.store.Issue(ctx, tx, link.Issue)
	if err != nil || linking == nil {
		return err
	}
	if err := e.refuseLink(ctx, tx, *linking, *link, record.LinkRefusedOtherIssue, pr.Issue); err != nil {
		return err
	}
	_, err = e.judgeClose(ctx, tx, linking.Key, nil, fmt.Sprintf("its link of %s is refused as %s's", prName(pr.Repo, pr.Number), pr.Issue))
	return err
}

// refuseLink holds link, a link of issue, refused for refusal (record.Store.SetPullRequestLinkRefused,
// dispatch://INFRA-5910 decision 8), and tells issue's architect of it once for each refusal
// (record.PullRequestLink.ToldAt, decision 10): a link already refused for refusal whose refusal
// was told tells nothing again, so a read made again at a boot repeats nothing, and a link told for
// another reason before, its read owed, is told its refusal. A refusal that takes the pull request
// into no set is told with the notice `pr-link-refused` and the way out: a fork's pull request
// (record.LinkRefusedFork, detail the repository its head is in) and another issue's
// (record.LinkRefusedOtherIssue, detail that issue's key). A link held as unserved (detail the App
// that is missing) or unreadable (detail GitHub's answer) holds the issue's close instead, since
// Legion cannot see its pull request merge (setHolds), and is told with the close's notice,
// `pr-holds-close`, with what is missing and the way out: give Legion what it lacks there, or
// remove the link.
func (e *Engine) refuseLink(ctx context.Context, tx pgx.Tx, issue record.Issue, link record.PullRequestLink, refusal record.LinkRefusal, detail string) error {
	if err := e.store.SetPullRequestLinkRefused(ctx, tx, link.Repo, link.Number, refusal); err != nil {
		return err
	}
	name := prName(link.Repo, link.Number)
	e.logOnCommit(ctx, "workflow: a link of an issue to a pull request is refused", "issue", issue.Key, "pullRequest", name,
		"refused", refusal, "detail", detail)
	var reason string
	holds := false
	switch refusal {
	case record.LinkRefusedFork:
		fork := "in the fork " + detail
		if detail == "" {
			fork = "in a fork GitHub no longer has"
		}
		reason = fmt.Sprintf("%s links pull request %s, whose head is %s: Legion takes no fork's pull request into an issue's pull requests, so it neither works it nor waits for it. "+
			"Remove the link; to make the change part of %s, open it from a branch of %s and link that pull request.",
			issue.Key, name, fork, issue.Key, link.Repo)
	case record.LinkRefusedOtherIssue:
		reason = fmt.Sprintf("%s links pull request %s, which Legion holds as %s's: a pull request is one issue's, so it stays %s's and is none of %s's pull requests. "+
			"Remove the link from %s.", issue.Key, name, detail, detail, issue.Key, issue.Key)
	case record.LinkRefusedUnserved:
		holds = true
		reason = fmt.Sprintf("%s links pull request %s, and Legion does not serve %s: %s. Legion cannot read the pull request or see it merge, so it holds the close of %s. "+
			"Install what is missing on %s, or remove the link.", issue.Key, name, link.Repo, detail, issue.Key, link.Repo)
	case record.LinkRefusedUnreadable:
		holds = true
		reason = fmt.Sprintf("%s links pull request %s, and Legion cannot read it: %s. Legion cannot see it merge, so it holds the close of %s. "+
			"Give Legion's Apps access to it, or remove the link.", issue.Key, name, detail, issue.Key)
	default:
		return nil
	}
	if link.ToldAt != nil && link.Refused == refusal {
		return nil
	}
	notice := record.Notice{Kind: "pr-link-refused", Role: claim.RoleArchitect, Reason: reason}
	if holds {
		notice = record.Notice{Kind: "pr-holds-close", Role: claim.RoleArchitect, Reason: reason}
	}
	if err := e.notice(ctx, tx, issue.Key, notice); err != nil {
		return err
	}
	told := e.now()
	return e.store.SetPullRequestLinkTold(ctx, tx, link.Repo, link.Number, &told)
}

// pullRequestLinkRead applies the daemon's read of a pull request an issue links
// (intake.PullRequestLinkReadDone, record.PullRequestLinkRead): it records the pull request the
// link names, or holds the link refused (dispatch://INFRA-5910 decisions 4 and 8). A read of a link
// the issue no longer holds, or that moved to another issue, records nothing, and so does a read of
// a link refused as a fork's or another issue's, which stays refused. A read that found no pull
// request is held refused: unserved where the daemon reads nothing in the repository
// (record.LinkRefusedUnserved), unreadable where GitHub answered 404 (record.LinkRefusedUnreadable);
// either is read again at the next boot and at the next change of the issue's links. A pull
// request whose head is in a fork, or whose fork GitHub no longer has, is refused as a fork's (D2),
// by the rule the open of a pull request reads (pullRequestOpened). One the record holds for
// another issue is refused as that issue's. A read that found the pull request takes back an
// unserved or unreadable refusal, and the told mark of the notice that told it, which named the
// refusal and not the pull request the read found (refuseLink), and records it: a pull request the
// record holds no row of yet as a new row of the issue, at the state, head, branch, base and merge
// commit GitHub showed, and one it holds already through the facts a webhook of it would apply
// (linkReadOf), so each fence of a late or repeated fact holds for a read too. It moves no phase of
// its own. A read that refuses the link as a fork's or another issue's, or that records a new row,
// changes which links hold the issue's close (setHolds): a link whose read is owed holds it, and
// once read it holds it only while its pull request is open. So the close is judged again
// (judgeClose, decision 9), as each fact judges it that changes the set; the facts linkReadOf
// applies judge it themselves where they change it (merged, closed).
func (e *Engine) pullRequestLinkRead(ctx context.Context, tx pgx.Tx, fact intake.PullRequestLinkReadDone) (intake.Result, error) {
	link, err := ghrepo.Under(e.cfg.RepositoryRenames, fact.Repo, func(name string) (*record.PullRequestLink, error) {
		return e.store.PullRequestLinkOf(ctx, tx, name, fact.Number)
	})
	if err != nil {
		return intake.Result{}, err
	}
	if link == nil || link.Issue != fact.Issue || link.Refused == record.LinkRefusedFork || link.Refused == record.LinkRefusedOtherIssue {
		e.logOnCommit(ctx, "workflow: the read of a pull request its issue no longer takes as linked records nothing",
			"issue", fact.Issue, "pullRequest", prName(fact.Repo, fact.Number), "link", link)
		return intake.Result{}, nil
	}
	issue, err := e.store.Issue(ctx, tx, link.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	name := prName(fact.Repo, fact.Number)
	switch {
	case fact.Unserved != "":
		return intake.Result{}, e.refuseLink(ctx, tx, *issue, *link, record.LinkRefusedUnserved, fact.Unserved)
	case fact.NotFound:
		return intake.Result{}, e.refuseLink(ctx, tx, *issue, *link, record.LinkRefusedUnreadable, "GitHub answered 404")
	case !e.sameRepository(fact.HeadRepo, fact.Repo):
		if err := e.refuseLink(ctx, tx, *issue, *link, record.LinkRefusedFork, fact.HeadRepo); err != nil {
			return intake.Result{}, err
		}
		_, err := e.judgeClose(ctx, tx, issue.Key, nil, fmt.Sprintf("its link of %s is refused as a fork's", name))
		return intake.Result{}, err
	}
	recorded, err := e.replacedPullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil {
		return intake.Result{}, err
	}
	if recorded != nil && recorded.Issue != issue.Key {
		if err := e.refuseLink(ctx, tx, *issue, *link, record.LinkRefusedOtherIssue, recorded.Issue); err != nil {
			return intake.Result{}, err
		}
		_, err := e.judgeClose(ctx, tx, issue.Key, nil, fmt.Sprintf("its link of %s is refused as %s's", name, recorded.Issue))
		return intake.Result{}, err
	}
	if link.Refused != "" {
		if err := e.store.SetPullRequestLinkRefused(ctx, tx, link.Repo, link.Number, ""); err != nil {
			return intake.Result{}, err
		}
		if err := e.store.SetPullRequestLinkTold(ctx, tx, link.Repo, link.Number, nil); err != nil {
			return intake.Result{}, err
		}
	}
	if recorded != nil {
		return e.linkReadOf(ctx, tx, fact, *recorded)
	}
	pr := record.PullRequest{Issue: issue.Key, Repo: fact.Repo, Number: fact.Number, Branch: fact.Branch, Base: fact.Base, HeadSHA: fact.HeadSHA,
		HeadUpdatedAt: fact.UpdatedAt, Failing: []string{}, State: fact.State, MergeSHA: fact.MergeSHA}
	if err := e.putPullRequest(ctx, tx, pr); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: a pull request the issue links is recorded", "issue", issue.Key, "pullRequest", prName(pr.Repo, pr.Number),
		"branch", pr.Branch, "base", pr.Base, "state", pr.State, "head", pr.HeadSHA)
	var merge *record.PullRequest
	if pr.State == record.PullRequestMerged {
		merge = &pr
	}
	_, err = e.judgeClose(ctx, tx, issue.Key, merge, fmt.Sprintf("its linked %s is read %s", name, pr.State))
	return intake.Result{}, err
}

// linkReadOf applies fact, the read of a linked pull request the record holds as recorded, as the
// one webhook fact of GitHub's that would bring the record to what the read showed, through that
// fact's own handler, so its fences hold: a merge GitHub made (merged), a close without one
// (closed), a reopen (pullRequestOpened), or a head that moved while the pull request stayed open
// (pullRequestSynchronized). A read that shows what the record holds applies nothing.
func (e *Engine) linkReadOf(ctx context.Context, tx pgx.Tx, fact intake.PullRequestLinkReadDone, recorded record.PullRequest) (intake.Result, error) {
	switch {
	case fact.State == record.PullRequestMerged && recorded.State != record.PullRequestMerged:
		return e.merged(ctx, tx, intake.PullRequestMerged{Repo: recorded.Repo, Number: recorded.Number, MergeSHA: fact.MergeSHA, HeadSHA: fact.HeadSHA})
	case fact.State == record.PullRequestClosed && recorded.State == record.PullRequestOpen:
		return e.closed(ctx, tx, intake.PullRequestClosed{Repo: recorded.Repo, Number: recorded.Number, HeadSHA: fact.HeadSHA, UpdatedAt: fact.UpdatedAt})
	case fact.State == record.PullRequestOpen && recorded.State == record.PullRequestClosed:
		return e.pullRequestOpened(ctx, tx, intake.PullRequestOpened{Repo: recorded.Repo, Number: recorded.Number, Branch: fact.Branch, Base: fact.Base,
			HeadSHA: fact.HeadSHA, HeadRepo: fact.HeadRepo, URL: fact.URL, UpdatedAt: fact.UpdatedAt, Reopened: true})
	case fact.State == record.PullRequestOpen && recorded.State == record.PullRequestOpen && fact.HeadSHA != recorded.HeadSHA:
		return e.pullRequestSynchronized(ctx, tx, intake.PullRequestSynchronized{Repo: recorded.Repo, Number: recorded.Number, Branch: fact.Branch,
			Base: fact.Base, HeadSHA: fact.HeadSHA, UpdatedAt: fact.UpdatedAt})
	}
	return intake.Result{}, nil
}

// mergedNotOwn is the merge of pr, a pull request that is no task's own (merged): another pull
// request of a task on its branch, or a pull request the issue links that is no task's own, in any
// repository (dispatch://INFRA-5910 decisions 5 and 9). Its issue is the issue column of its row
// (record.PullRequest.Issue), never a plan task's, and the merge, which merged recorded already,
// moves no task: it queues no close of the task pull requests (closeHeldPullRequests) and runs no
// transition. The architect is told (`pr-merged`), and an issue in reviewing has its round asked
// again (reaskRound), so the merge ends the review of that pull request alone. The links that hold
// the issue's close lose one where pr is linked (setHolds), so the close is judged again
// (judgeClose), with pr the merge it follows. scope is the task pr is of (issueOfPullRequest), nil
// where it is no one task's; such a pull request that the issue does not link either is none of its
// set, and its merge tells nobody.
func (e *Engine) mergedNotOwn(ctx context.Context, tx pgx.Tx, scope *TaskScope, pr *record.PullRequest, prior record.PullRequest) error {
	if scope == nil && !pr.Linked {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return err
	}
	name := prName(pr.Repo, pr.Number)
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-merged", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("pull request %s merged while %s was in %s; it is no task's own pull request of %s, so its merge moves no task", name, issue.Key,
			issue.Phase, issue.Key)}); err != nil {
		return err
	}
	if scope != nil {
		if err := e.reaskRound(ctx, tx, *scope, pr, prior, byMerge); err != nil {
			return err
		}
	}
	_, err = e.judgeClose(ctx, tx, issue.Key, pr, fmt.Sprintf("pull request %s merged", name))
	return err
}

// heldLink is a link of an issue that holds the issue's close (setHolds): the link, and the row the
// record holds of the pull request it names, nil where its read is still owed or it is held refused
// as unserved or unreadable.
type heldLink struct {
	link record.PullRequestLink
	row  *record.PullRequest
}

// why is the clause the notice that tells a held link (tellHolds) names it with: why the link holds
// the close, and the way out of that hold.
func (h heldLink) why() string {
	switch {
	case h.link.Refused == record.LinkRefusedUnserved:
		return fmt.Sprintf("Legion does not serve %s, so it cannot read the pull request or see it merge. Install what Legion lacks on %s, or remove its link.",
			h.link.Repo, h.link.Repo)
	case h.link.Refused == record.LinkRefusedUnreadable:
		return "GitHub does not show it to Legion, so Legion cannot see it merge. Give Legion's Apps access to it, or remove its link."
	case h.row == nil:
		return "Legion has not read it from GitHub yet. Remove its link, or have it merged."
	}
	return "it is open. Remove its link, or have it merged."
}

// setHolds is each link of issue that holds its close (dispatch://INFRA-5910 decision 9), in the
// order of their repositories, compared without case, and numbers: each pull request the issue
// links that is open, whether or not a task holds its branch, since the stack's close of the
// pull requests of tasks never reaches a pull request the issue links (closeHeldPullRequests); each
// link whose read is still owed, whose pull request the record holds no row of yet, which counts as
// open so that no close happens between a link and its read (decision 4); and each link held refused
// as unserved or unreadable, whose pull request Legion cannot read or see merge (decision 8). A
// pull request of a task that the issue does not link holds nothing, since the stack's close
// reaches it at the issue's last merge, and neither does a merged or a closed pull request, which
// the architect left closed (D5), nor a link refused as a fork's or another issue's, which takes its
// pull request into no set. It is the one statement of which pull requests hold a close, a read of
// the store alone, as PhaseFinished reads it outside the engine: the issue's links
// (record.Store.PullRequestLinksOf) and the row of each, read under the link's own name as rows
// alone (record.Store.PullRequestRowByNumber), the repository compared without case.
func setHolds(ctx context.Context, store record.Store, tx pgx.Tx, issue string) ([]heldLink, error) {
	links, err := store.PullRequestLinksOf(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	var holds []heldLink
	for _, link := range links {
		switch link.Refused {
		case record.LinkRefusedFork, record.LinkRefusedOtherIssue:
			continue
		case record.LinkRefusedUnserved, record.LinkRefusedUnreadable:
			holds = append(holds, heldLink{link: link})
			continue
		}
		row, err := store.PullRequestRowByNumber(ctx, tx, link.Repo, link.Number)
		if err != nil {
			return nil, err
		}
		if row == nil || row.Issue == issue && row.State == record.PullRequestOpen {
			holds = append(holds, heldLink{link: link, row: row})
		}
	}
	return holds, nil
}

// tellHolds tells issue's architect of each link of holds, the links that hold its close (setHolds),
// that no notice named yet (record.PullRequestLink.ToldAt, dispatch://INFRA-5910 decision 10), in
// one `pr-holds-close` that names each such pull request as owner/name#number, why it holds the
// close and the way out of the hold (heldLink.why), and marks each link it named told, so no later
// pass names it again: the close of a merge in hand (closeMerged), at the merge, at a child's
// leave, at each boot, and the hold of an issue with no merge in hand (holdForPullRequests,
// leaveWhereDone, judgeClose) all call it while the close waits, and the architect hears each link
// once. A new generation clears the marks (record.Store.ClearGeneration). Holds whose links were
// all named tell nothing.
func (e *Engine) tellHolds(ctx context.Context, tx pgx.Tx, issue record.Issue, holds []heldLink) error {
	var untold []heldLink
	for _, held := range holds {
		if held.link.ToldAt == nil {
			untold = append(untold, held)
		}
	}
	if len(untold) == 0 {
		return nil
	}
	var reason strings.Builder
	for at, held := range untold {
		if at > 0 {
			reason.WriteString(" ")
		}
		fmt.Fprintf(&reason, "%s holds the close of %s, which links it: %s", prName(held.link.Repo, held.link.Number), issue.Key, held.why())
	}
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-holds-close", Role: claim.RoleArchitect, Reason: reason.String()}); err != nil {
		return err
	}
	told := e.now()
	for _, held := range untold {
		if err := e.store.SetPullRequestLinkTold(ctx, tx, held.link.Repo, held.link.Number, &told); err != nil {
			return err
		}
	}
	e.logOnCommit(ctx, "workflow: the architect is told the pull requests that hold an issue's close", "issue", issue.Key, "holds", len(untold))
	return nil
}
