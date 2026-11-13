package workflow

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// checks applies a CI settlement to the pull request and tells the roles that work the issue what
// it says (tellChecks). It moves nothing: a failed CI check is information the worker holding the
// phase decides on - rerun, wait, or fix - and not the daemon's to read the semantics of
// (dispatch://INFRA-714/spec R1).
func (e *Engine) checks(ctx context.Context, tx pgx.Tx, fact intake.PullRequestChecks, woken claim.Role) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	// The listener settles every commit of the pull request, its head or not, and a settlement
	// stands only for the commit it names: it is for the head only when it names the head. While a
	// head run is open the settlement is recorded in it whether it stands or not
	// (classify.ApplyCheckSettlement).
	candidate := classify.SettlementCandidate{Head: fact.HeadSHA, CheckRuns: fact.CheckRuns, Generation: fact.Generation, Snapshot: fact.Snapshot, Failing: fact.Failing, Cancelled: fact.Cancelled}
	told := settledChecks(*pr)
	settled, applied := classify.ApplyCheckSettlement(*pr, candidate)
	if !applied {
		if pr.Run != nil {
			*pr = settled
			if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
				return intake.Result{}, err
			}
		}
		// The write above is one of the two pull-request writes of this package that mark no
		// movement of the issue (record.Issue.MovedAt, the rule in engine.go's dispatchIssue),
		// the other being a push onto the base branch, which is no news of the pull request
		// either (base_push.go). It records the fence the daemon read and decides nothing — no
		// run and no outcome changed — so it is the daemon learning what it already knew, which
		// is never a movement. Marking it would refresh a stopped tree's quiet time at every
		// duplicate settlement the listener sends, and a tree that stopped with a settled head
		// would never be found at all. The write is made with the store's own PutPullRequest and
		// not through putPullRequest, though that helper's own test would mark nothing for most
		// writes of this shape: the settlement that pushes an open head run past its bound of
		// steps closes the run and decides nothing, and the helper would read the run's closing
		// as news. internal/admit's TestTheRecordKeepsWhenAnIssueLastMoved drives that write in
		// its case of the listener's settlement that decides nothing, and fails where this write
		// goes through the helper.
		//
		// A settlement that does not stand for the head tells nobody, and GitHub's merge state is
		// read for the head all the same (Apply's askMergeState): a run GitHub counts for the head
		// can settle on a commit the record does not stand at, and the state is the only answer
		// that says so.
		return intake.Result{}, nil
	}
	// The settlement that applies is the other half of that pair, and it is news: the head it
	// settled on, the runs it reports and what each of them concluded are new to the record, and
	// a GitHub event of the issue's pull request is a movement of the issue
	// (dispatch://INFRA-2560 R3). So this write goes through the helper that marks it
	// (putPullRequest) while the duplicate above does not, and a tree whose last news was its CI
	// settling is measured from that settlement rather than from whatever moved the issue before
	// it.
	*pr = settled
	if err := e.putPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.tellChecks(ctx, tx, pr, told, woken)
}

// mergeability records what GitHub's last read found for whether the pull request's head can be
// merged into its base (intake.PullRequestMergeability, readPullRequest's own GitHub answer), and
// acts on the two things that read can say.
//
// A head that conflicts with its base sends the issue back to implementing from whatever later
// phase it stands in (TriggerConflict):
// GitHub computes no merge ref for a conflicting head, so no check runs on it and no CI result
// will ever arrive for it, and only the implementer merging the base forward clears it. It
// decides on every read, not only a changed one, since the recorded conflict is what a read made
// in an earlier phase left. Implementing, where the work already is, has no row for the trigger,
// so a read there records the conflict and moves nothing (transition).
//
// GitHub's own merge state for the head rides along on the same fact: it is what says whether
// GitHub would merge this head now, which the tri-state mergeability cannot, and it is what an
// approval standing on the head waits for (reviewRound). So in reviewing the round is asked again
// once the state is recorded - this read is the one that moves an approved issue to accepting, as
// often as not. The base the read names is recorded with
// them, since that read is the only one that names the base of a pull request opened before the
// record kept one: it is what a push onto that branch finds the pull request by (basePush), and a
// read that answers for a base push already recorded clears the push's mark, the two having been
// read together.
//
// A read that answers for another head than the record stands at is dropped whole, and logged: a
// push landed between GitHub's answer and this apply, so the verdict is about a tree the record
// has moved past, and an approval must never advance on it. Nothing is lost by dropping it - that
// push is itself a fact that asks for a read of the head it left (askMergeState).
//
// Whatever the read decides, it is also the judgement the conflict-round exemption is read from
// (record.PullRequest.ConflictingHead, recordRound): a CONFLICTING read holds the pull request's
// own head conflicting, a MERGEABLE read of that same head releases it, and an UNKNOWN read,
// which is what GitHub answers for a head sitting in the merge queue, leaves the mark as it is -
// GitHub has not computed an answer, so there is nothing to record and nothing to take back. The
// mark is a second column rather than a reading of Mergeability because every read rewrites that
// one from GitHub's answer: the UNKNOWN of a read on the ladder that follows would lose the
// conflict, and the next round at the conflicting head would count.
func (e *Engine) mergeability(ctx context.Context, tx pgx.Tx, fact intake.PullRequestMergeability) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	if fact.HeadSHA != pr.HeadSHA {
		e.logOnCommit(ctx, "workflow: GitHub's merge state answered for a head the pull request has moved past; the read is dropped",
			"issue", pr.Issue, "pullRequest", pr.Number, "answered", fact.HeadSHA, "head", pr.HeadSHA,
			"mergeable", string(fact.Mergeable), "mergeState", string(fact.MergeState))
		return intake.Result{}, nil
	}
	base := pr.Base
	if fact.Base != "" {
		base = fact.Base
	}
	prior := *pr
	conflicting := pr.ConflictingHead
	switch fact.Mergeable {
	case record.MergeabilityConflicting:
		conflicting = pr.HeadSHA
	case record.MergeabilityMergeable:
		if conflicting == pr.HeadSHA {
			conflicting = ""
		}
	}
	if pr.Mergeability != fact.Mergeable || pr.MergeState != fact.MergeState || pr.Base != base || pr.BaseMoved != "" || pr.ConflictingHead != conflicting {
		pr.Mergeability, pr.MergeState, pr.Base, pr.BaseMoved, pr.ConflictingHead = fact.Mergeable, fact.MergeState, base, "", conflicting
		if err := e.putPullRequest(ctx, tx, *pr); err != nil {
			return intake.Result{}, err
		}
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if classify.HeadConflicts(*pr) {
		reason := fmt.Sprintf("the head conflicts with %s: GitHub runs no checks on it; merge %s forward", base, base)
		return intake.Result{}, e.transition(ctx, tx, *issue, TriggerConflict, "", record.PhaseRow{}, pr, reason)
	}
	if issue.Phase != phase.Reviewing {
		return intake.Result{}, nil
	}
	reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return intake.Result{}, err
	}
	_, err = e.settleRound(ctx, tx, *issue, reviewer, pr, e.reviewRound(*issue, reviewer, &prior), byMergeState)
	return intake.Result{}, err
}

// settledChecks is what the CI settlement standing for the pull request's head says: the head it
// settled on, and each check it reports with the conclusion it reports for it, in name order; ""
// while no settlement stands. A check the settlement names as failed failed, one it names as
// cancelled was cancelled, and one it reports as a run and names neither way succeeded - the
// listener settles a commit once every check it has seen is terminal. Nothing here judges those
// conclusions: a job GitHub never started reads like any other, and what they mean for the work is
// the reading worker's (dispatch://INFRA-714/spec R1). The line is the whole of what the daemon
// knows of that head's CI, so two settlements that leave it the same say nothing new (tellChecks).
//
// Each name's conclusion is the newest run of it among the settlements of the head the fence
// stands at, which is what the record holds (classify.SettlementFor): a settlement stands only
// for the head it names, so a check re-run there and passed is no longer red however its earlier
// run ended. A check still running on that head is in no settlement of it - the listener settles
// a commit only once every check it has seen there is terminal - so this line names it not at
// all: a pending check is no verdict, and never a failure.
//
// A check run's name is whoever wrote the workflow's text, and this line is read into an agent's
// prompt, so each name is quoted as a Go string (strconv.Quote): the quotes bound it, and a quote
// or a newline inside it is escaped rather than closing the one around it. A name of ordinary
// characters reads as itself inside the quotes.
func settledChecks(pr record.PullRequest) string {
	if pr.CheckedHead == "" {
		return ""
	}
	conclusions := make(map[string]record.RunConclusion, len(pr.CheckRuns)+len(pr.Failing)+len(pr.Cancelled))
	for _, run := range pr.CheckRuns {
		conclusions[run.Name] = record.RunSucceeded
	}
	for _, name := range pr.Cancelled {
		conclusions[name] = record.RunCancelled
	}
	for _, name := range pr.Failing {
		conclusions[name] = record.RunFailed
	}
	if len(conclusions) == 0 {
		return "CI settled at " + pr.CheckedHead + " reporting no check"
	}
	return "CI settled at " + pr.CheckedHead + ": " + namedConclusions(conclusions)
}

// headChecks is every check run GitHub reported on the pull request's current head with what that
// run concluded, in name order; "" when the record holds none. It is the line the notice of a head
// GitHub will not merge from names (stuckMergeStateReason), where the settlement's own line would
// not be true: a settlement standing for the head can be of the code head below it, and a name
// whose newest run is there ran on other heads of the tree and not on this one. What GitHub
// weighs for the merge is the current head, so that is the head whose checks the notice names,
// and a required check whose run is missing here is the thing its reader looks for.
//
// Each run of the fence carries the head it ran on and its conclusion (record.AttemptRun), so
// this is a reading of the fence and not of a settlement: a check run on this head that a newer
// run of the same name on another head has replaced in the fence is not named, and nor is a run
// stored before the fence carried its head.
func headChecks(pr record.PullRequest) string {
	conclusions := make(map[string]record.RunConclusion, len(pr.CheckRuns))
	for _, run := range pr.CheckRuns {
		if run.Head != "" && run.Head == pr.HeadSHA {
			conclusions[run.Name] = run.Conclusion
		}
	}
	if len(conclusions) == 0 {
		return ""
	}
	return "GitHub reported on head " + pr.HeadSHA + ": " + namedConclusions(conclusions)
}

// namedConclusions is each check name with what it came to, in name order, as both lines above
// spell it.
func namedConclusions(conclusions map[string]record.RunConclusion) string {
	names := make([]string, 0, len(conclusions))
	for name := range conclusions {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, name := range names {
		names[i] = strconv.Quote(name) + " " + string(conclusions[name])
	}
	return strings.Join(names, ", ")
}

// tellChecks tells the roles that work the issue what the settlement now standing for the pull
// request's head says (settledChecks), and moves nothing: the daemon passes the CI result on, and
// the worker that holds the phase decides whether to re-run, wait or fix
// (dispatch://INFRA-714/spec R1). One message goes to three roles - the worker holding the issue's
// phase, the implementer, whose code the checks ran on, and the tree's architect - each on its own
// role topic (record.RoleMessage), the way the daemon reaches a role holder with a message. A role
// named twice, the implementer holding the phase, is told once, and a phase no worker holds names
// none (addressRoles). told is what the settlement standing for the head said before this one, so
// a second settlement of the same head that leaves the same conclusions tells nobody again, and so
// does a settlement that stands for no head. Linger holds a member of a closed tree where it stood
// (record.TreeLingers): every role of the tree is suspended there, so the settlement is recorded
// and nobody is told.
//
// A role whose released pod this same settlement woke is told nothing: its wake task already
// names the settlement (WakeTask with pods.WakeChecks), and a message about the same settlement
// would reach it as a second wake of one release, described to the worker as something somebody
// published to its role topic. woken is that role, "" when the settlement woke nobody, and every
// other role — a live one, and one the wake did not reach — is told as before.
//
// A holder this daemon cannot name is an error here, and with it the settlement's whole
// transaction: the settlement moves no issue, so there is nothing kept by telling the rest, and
// the next settlement of the head asks again.
func (e *Engine) tellChecks(ctx context.Context, tx pgx.Tx, pr *record.PullRequest, told string, woken claim.Role) error {
	settled := settledChecks(*pr)
	if settled == "" || settled == told {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return err
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	// The tree's architect owns the tree, so its claim is the tree root's; a phase the architect
	// itself holds (accepting) is that same claim, and is told once with it.
	recipients := []roleRecipient{}
	if woken != claim.RoleArchitect {
		recipients = append(recipients, roleRecipient{issue: issue.Tree, role: claim.RoleArchitect, row: issue.Key})
	}
	for _, role := range []claim.Role{RoleFor(issue.Phase), claim.RoleImplementer} {
		if role == claim.RoleArchitect || role == woken {
			continue
		}
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: role, row: issue.Key})
	}
	addressed, err := e.addressRoles(issue.Project, recipients, nil)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("Pull request #%d: %s. Nothing moved on it: decide whether to re-run the checks, wait for them, or fix the code.", pr.Number, settled)
	for _, to := range addressed {
		if err := e.enqueue(ctx, tx, to.issue, record.RoleMessage{Role: string(to.token), Body: body}); err != nil {
			return err
		}
	}
	return nil
}

// answerSkew bounds how far GitHub's clock, which stamps a review's submission, and the daemon's,
// which stamps the reviewer's completion (record.PhaseRow.CompletedAt), may disagree. No ordering
// between the two avoids comparing those clocks: the delivery order is the race itself, and the
// completion has no GitHub-side stamp. GitHub's review ids come closest, but GitHub assigns one
// when a review is created, not when it is submitted, so a draft created before the completion
// and submitted after it would read as already seen; reading them would also put a GitHub call on
// the completion path. The bound is sized to skew alone, since a real answer follows the completion
// within the minute the stuck notice, the architect's request and the reviewer's review take. Its
// two costs: a GitHub clock more than answerSkew ahead makes a review submitted before the
// completion read as the reviewer's answer, and tell again; an answer submitted within answerSkew
// of the completion reads as delivered late, and tells only when the round's stuck cause or head
// changes (stuckAs).
const answerSkew = 10 * time.Second

// byReviewer says whether review is the reviewer's own review-App session, not merely a review the
// review App submitted: the controller now holds the review App's token too, so every role's
// review arrives under the same bot login (Config.ReviewAppLogin). What tells the reviewer's
// review from any other review-App session's - the controller's, an architect's, a sibling tree's
// role - is the Legion footer every Legion role appends to a pull-request review
// (intake.PullRequestReview.LegionSession), matched against reviewerSession, the session recorded
// on the reviewer's own claim (record.Store.ClaimSession). With no session recorded - a daemon
// whose claims predate this, or a reviewer claim with no row - the login decides by itself, as it
// always has, since there is nothing recorded to tell the sessions apart.
func (e *Engine) byReviewer(review intake.PullRequestReview, reviewerSession string) bool {
	return e.byReviewApp(review.Author) && (reviewerSession == "" || review.LegionSession() == reviewerSession)
}

// DecidesRound says whether review may decide a review round: it is the reviewer's own review-App
// session (byReviewer, with reviewerSession as ReviewerSession reads it), or its author has write
// access or higher to the repository, which intake read from GitHub before the fact reached this
// transaction (intake.ResolveAuthorCanWrite, for a webhook's review and the daemon's own read of a
// pull request's reviews alike). On a public repository any account can review a pull request, so
// anyone else's review decides nothing. GitHub's 404, or a 403 that is not its rate limit, reads as
// no write access; any other failed read is retried before the review reaches here. A review-App
// review that is not the reviewer's own session decides nothing either: it is set aside, not
// counted as a writer's or dropped as a stranger's. It is the half of the whole judgement
// (RefusesRound) that answers for who wrote a review, and a refusal it leaves stands at every head
// (RoundRefusal.AtEveryHead): an account's standing and a session's claim are not things the
// record's next head changes.
func (e *Engine) DecidesRound(review intake.PullRequestReview, reviewerSession string) bool {
	e = e.decision()
	return e.byReviewer(review, reviewerSession) || review.AuthorCanWrite
}

// RoundRefusal is RefusesRound's answer: why a review must not decide a review round, and how long
// that refusal stands. Both places that judge a review must know how long one stands - the webhook
// path keeps on the reviewer's row only a refusal the round could still be spared (review), and
// the daemon's read of GitHub's list remembers only one that will stand on the read after it
// (daemon's reviewToApply) - and they read it here rather than judging the review a second time.
type RoundRefusal struct {
	// Reason is the whole of why the review must not decide the round, "" when nothing refuses it.
	Reason string
	// AtEveryHead is whether the refusal stands whatever head the record comes to hold: who wrote
	// the review (DecidesRound), and a person's review read from GitHub's list, whose commit_id is
	// GitHub's own and moves with the pull request. A review-App review refused for the head its
	// footer names is refused at the head the record holds now and no further, since the head the
	// record holds changes.
	AtEveryHead bool
}

// RefusesRound is the whole of why review, one that decides (intake.PullRequestReview.Decides),
// must not decide pr's round, a RoundRefusal with no reason when nothing refuses it: who wrote it
// (DecidesRound, with reviewerSession as ReviewerSession reads it), then the head it says it read
// (refusesRound). Two places judge a review - this engine, for the review a webhook delivers, and
// the daemon's read of GitHub's list of a pull request's reviews (daemon's reviewToApply), which
// applies only a review this would take - and both ask this one judgement, so neither judges by a
// narrower rule than the other, and neither asks a second one to learn how long a refusal stands:
// the answer carries that (RoundRefusal.AtEveryHead).
func (e *Engine) RefusesRound(review intake.PullRequestReview, pr record.PullRequest, reviewerSession string) RoundRefusal {
	e = e.decision()
	if !e.DecidesRound(review, reviewerSession) {
		if e.byReviewApp(review.Author) {
			return RoundRefusal{Reason: "the review App submitted it from a session that is not the reviewer's", AtEveryHead: true}
		}
		return RoundRefusal{Reason: "its author is neither the review App nor an account with write access to the repository", AtEveryHead: true}
	}
	return e.refusesRound(review, pr)
}

// refusesRound is the second half of that judgement (RefusesRound): why a review that decides
// (intake.PullRequestReview.Decides) and that DecidesRound counts must still not decide pr's
// round, a RoundRefusal with no reason when nothing refuses it. The whole rule, in this order:
//
//   - A review the daemon read from GitHub's list of pr's reviews
//     (intake.PullRequestReview.ReadFromList) that the review App did not submit decides nothing,
//     at every head the record can come to hold. The list answers with GitHub's own commit_id for
//     every review it holds, and that id moves to the pull request's current head
//     (dispatch://INFRA-1557), so a person's listed approval would stand for code it never read,
//     and no head the record reaches makes the list's answer evidence of the code it read. The
//     same person's review by webhook decides as it always has, on the commit_id the event
//     carried.
//   - A review the review App submitted, listed or by webhook, decides only when the head its
//     Legion footer names (intake.PullRequestReview.LegionHead) is pr's current head, compared
//     exactly, so its refusal stands at the head the record holds now and no further. The footer
//     is the review's own word for the code it read, written in the body it was submitted with; no
//     clock answers for a force push that puts an earlier head back, and GitHub's commit_id for
//     the review moves with the pull request.
//   - Every other review - a person's, delivered by its webhook - decides as it did before.
func (e *Engine) refusesRound(review intake.PullRequestReview, pr record.PullRequest) RoundRefusal {
	if !e.byReviewApp(review.Author) {
		if review.ReadFromList {
			return RoundRefusal{Reason: "a person's review read from the list", AtEveryHead: true}
		}
		return RoundRefusal{}
	}
	switch head := review.LegionHead(); {
	case head == "":
		return RoundRefusal{Reason: "footer names no head"}
	case head != pr.HeadSHA:
		return RoundRefusal{Reason: fmt.Sprintf("footer names %s, not the head %s", head, pr.HeadSHA)}
	}
	return RoundRefusal{}
}

// ReviewerSession is the session recorded on the claim of issue's reviewer, the session a review-App
// review's Legion footer must name to be the reviewer's own (byReviewer): "" when the reviewer's
// row holds no claim, the claim has no row, or its session is empty, in which case the login decides
// by itself. The daemon's read of a pull request's reviews reads it before it judges one
// (RefusesRound), as review does, so both judge a review-App review alike.
func (e *Engine) ReviewerSession(ctx context.Context, tx pgx.Tx, issue string) (string, error) {
	e = e.decision()
	reviewer, err := e.phaseRow(ctx, tx, issue, claim.RoleReviewer)
	if err != nil {
		return "", err
	}
	return e.claimSession(ctx, tx, reviewer)
}

// claimSession is the session recorded on row's claim, "" when the row holds no claim.
func (e *Engine) claimSession(ctx context.Context, tx pgx.Tx, row record.PhaseRow) (string, error) {
	if row.Claim == "" {
		return "", nil
	}
	return e.store.ClaimSession(ctx, tx, row.Claim)
}

// reviewersAnswer says whether fact is the reviewer's answer to a round it completed undecided: a
// review from the reviewer's own review-App session (byReviewer), with a body - GitHub records a
// reply on a review thread as a review with no body - more than answerSkew after the completion
// was applied. A review submitted before the completion but delivered after it is not, since the
// completion comes through the API and the review by webhook; nor is one with no submission time,
// or anyone else's, the review App's other sessions included.
func (e *Engine) reviewersAnswer(fact intake.PullRequestReview, reviewer record.PhaseRow, reviewerSession string) bool {
	return e.byReviewer(fact, reviewerSession) && fact.Body != "" && fact.SubmittedAt.After(reviewer.CompletedAt.Add(answerSkew))
}

func (e *Engine) review(ctx context.Context, tx pgx.Tx, fact intake.PullRequestReview) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return intake.Result{}, err
	}
	// reviewerSession is the session recorded on the reviewer's claim (claimSession), "" when none
	// is, in which case byReviewer decides by the login alone. It is read only for a review the
	// review App submitted: no other author's review ever reaches the compare.
	var reviewerSession string
	if e.byReviewApp(fact.Author) {
		reviewerSession, err = e.claimSession(ctx, tx, reviewer)
		if err != nil {
			return intake.Result{}, err
		}
	}
	// The reviewer's answer tells whenever it leaves the round stuck; any other review, only when it
	// changes how the round is stuck.
	before, by := e.reviewRound(*issue, reviewer, pr), byOtherReview
	if e.reviewersAnswer(fact, reviewer, reviewerSession) {
		before, by = round{}, byAnswer
	}
	// Only changes_requested and approved decide anything, and one judgement says whether such a
	// review may decide this round (RefusesRound): who wrote it, and the head it says it read.
	// Any other review orders nothing either, so a comment written after a decision but
	// delivered before it, or an outsider's review, cannot make the decision look old.
	state := strings.ToLower(fact.State)
	decides := fact.Decides()
	// refused is why a review the round's rules otherwise count was refused the round at the head
	// the record holds now rather than at every head (RoundRefusal.AtEveryHead). It is kept on the
	// reviewer's row below, since nothing else would say why the round has no decision. A refusal
	// that stands at every head leaves none: a stranger's review is no decision the round lost,
	// and the round is undecided the way one nobody reviewed is.
	refused := ""
	if decides {
		if refusal := e.RefusesRound(fact, *pr, reviewerSession); refusal.Reason != "" {
			args := []any{"issue", issue.Key, "pull_request", pr.Number, "author", fact.Author, "state", state,
				"footer_session", fact.LegionSession(), "reviewer_session", reviewerSession, "reviewer_claim", string(reviewer.Claim),
				"footer_head", fact.LegionHead(), "head", pr.HeadSHA, "read_from_list", fact.ReadFromList,
				"body_truncated", fact.BodyTruncated}
			if e.byReviewApp(fact.Author) {
				e.warnOnCommit(ctx, "workflow: a review decides nothing: "+refusal.Reason, args...)
			} else {
				e.logOnCommit(ctx, "workflow: a review decides nothing: "+refusal.Reason, args...)
			}
			decides = false
			if !refusal.AtEveryHead {
				refused = refusal.Reason
			}
		}
	}
	if decides && reviewerSession == "" && e.byReviewApp(fact.Author) {
		e.logOnCommit(ctx, "workflow: the reviewer's session is not recorded; the review App's login decides",
			"issue", issue.Key, "pull_request", pr.Number, "reviewer_claim", string(reviewer.Claim))
	}
	if !decides {
		// The stuck notice of a round nothing decided is built from the record when the reviewer
		// completes (reviewRound, stuckUndecided), which is after the refused review arrived, so
		// the refusal is kept on the reviewer's row for it to name. It is written only on a row
		// that holds no deciding decision, so a refusal reaching a round an approval already
		// decided cannot erase it, and it lives with that decision, which every entry into
		// reviewing clears (clearHandoff): no later round inherits the refusal of an earlier one.
		if refused != "" && (reviewer.Decision == nil || reviewer.Decision.State == "") {
			reviewer.Decision = &record.ReviewDecision{Refused: refused}
			if err := e.store.PutPhase(ctx, tx, reviewer); err != nil {
				return intake.Result{}, err
			}
		}
		_, err := e.settleRound(ctx, tx, *issue, reviewer, pr, before, by)
		return intake.Result{}, err
	}
	// Deciding reviews are ordered by when they were submitted, then by GitHub's review id
	// (record.ReviewOrder). Among reviews that all carry a time and arrive before the round ends,
	// the newest decides whatever order they are delivered in; with a review without a time in
	// play, the outcome can depend on delivery order. A round ends once both of its halves are in,
	// so a review arriving after that decides nothing for it. The pull request keeps the newest it
	// has had across rounds, so a review submitted before one already processed - redelivered, or
	// from an earlier round - records nothing. A review without an id is ordered by when it
	// arrives.
	if fact.ID != 0 {
		order := fact.Order()
		if !order.After(pr.ReviewSeen) {
			return intake.Result{}, nil
		}
		pr.ReviewSeen = order
		if err := e.putPullRequest(ctx, tx, *pr); err != nil {
			return intake.Result{}, err
		}
	}
	// The decision belongs to the review round, not to the pull request's head: the round must
	// still know, when the reviewer completes, what the review decided, on which head, and what
	// it said for the next round's implementer. The round is open while the issue stands in
	// reviewing, a hold from reviewing counted as standing there (record.StandsIn, the one shape
	// every question about where an issue stands is written over), since the retry puts it back
	// there with the reviewer's work kept. A review outside the round decides nothing and counts
	// no round. An approval is judged against the head it names when the review ends
	// (classify.ApprovalStands).
	if !record.StandsIn(*issue, phase.Reviewing) {
		return intake.Result{}, nil
	}
	reviewer.Decision = &record.ReviewDecision{State: state, Body: fact.Body, Head: e.reviewHead(fact)}
	if err := e.store.PutPhase(ctx, tx, reviewer); err != nil {
		return intake.Result{}, err
	}
	_, err = e.settleRound(ctx, tx, *issue, reviewer, pr, before, by)
	return intake.Result{}, err
}

// reviewHead is the head review read, which is the head its decision is recorded against and the
// head an approval of it is weighed at when the round ends (classify.ApprovalStands). The two
// sources differ because only one of them is the review's own word. A review the review App
// submitted names the head it read in its Legion footer (intake.PullRequestReview.LegionHead),
// which is why it decides the round at all (refusesRound); GitHub's commit_id for that same
// review is the pull request's head as GitHub holds it, and GitHub moves it onto each later head
// (dispatch://INFRA-1557). Recorded from commit_id, an approval of the head its footer names
// would stand for the head after it the moment that head arrived, approving code the reviewer
// never read. A person's review carries no footer, so it is recorded on the commit_id its
// webhook was delivered with, the head it decides on and the one GitHub had not yet moved.
func (e *Engine) reviewHead(review intake.PullRequestReview) string {
	if e.byReviewApp(review.Author) {
		return review.LegionHead()
	}
	return review.CommitID
}

// The fact that wrote a review-stuck notice, its summary. The architect prompt
// (prompts/go/architect-common.md) reads a notice the reviewer's completion or answer wrote after
// the architect asked as the reviewer answering, and every other as news about the round.
const (
	byCompletion  = "the reviewer's completion"
	byAnswer      = "the reviewer's review"
	byOtherReview = "a review that is not the reviewer's answer"
	byPush        = "a push"
	byMergeState  = "GitHub's merge state for the head"
)

// roundOutcome is what a review round comes to after a fact (reviewRound).
type roundOutcome int

const (
	// roundOpen is a round still owed its reviewer's completion or its decision, or waiting on the
	// head a push is bringing; or an issue not in reviewing, since one held
	// from reviewing ends only once the retry restores it.
	roundOpen roundOutcome = iota
	// roundApproved ends the round: its reviewer completed, and its approval approves the head's code
	// (classify.ApprovalStands), which GitHub answers for with a merge state the issue may advance
	// on. No CI verdict is read: the reviewer reads the head's checks on GitHub before it approves,
	// and a settlement moves no issue (dispatch://INFRA-714/spec R1).
	roundApproved
	// roundRejected ends the round: its reviewer completed, and it requested changes.
	roundRejected
	// roundStuck is a round that nothing on its way will end (stuckCause): one its reviewer
	// completed without a decision of the head, or one whose approval GitHub will not merge from.
	roundStuck
)

// stuckCause is why a round that nothing on its way will end is stuck.
type stuckCause int

const (
	// stuckUndecided: no review decided it. A COMMENT decides nothing, and neither does a review in
	// any state but approved or changes_requested, nor one DecidesRound does not count.
	stuckUndecided stuckCause = iota + 1
	// stuckApprovedOtherCode: its approval is of a head whose code the current head may not carry.
	stuckApprovedOtherCode
	// stuckMergeState: its approval approves the head's code, and GitHub answers for that head
	// with a merge state it would not merge from - blocked on a check or a review the repository
	// requires, behind its base, a draft. Nothing the daemon does moves it: GitHub's answer is
	// about the head on the branch, and only a push that earns the answer the base wants changes
	// it. This cause alone does not wait for the reviewer's completion: the approval has already
	// decided the round, and the completion is no part of what holds it.
	stuckMergeState
)

// round is reviewRound's account of a review round. A stuck round names its cause, the head it is
// stuck at, the merge state GitHub gave for that head, and what the architect, the reviewer and a
// restarted reviewer are told of it. waitsOnMergeState is the one thing the round is owed from
// outside the record: an approval that stands on the head with GitHub's answer for it not one the
// issue may advance on, which the daemon reads GitHub for (settleRound, daemon's readMergeState).
// It is set for the round that waits for a first answer and for the stuck one alike, since the
// state GitHub gives a head changes under it - a base push clears `behind`, a required check that
// runs clears `blocked` - and a read is the only thing that would ever say so.
type round struct {
	outcome           roundOutcome
	cause             stuckCause
	head              string
	state             record.MergeState
	reason            string
	waitsOnMergeState bool
}

// stuckAs says whether r is stuck in the same way as other: the same cause at the same head. A
// fact that is not the reviewer's own tells only when the round is stuck in a new way, so a
// second push classified late at the same head tells nothing. It is read for the two causes a
// review decides - the round nothing decided, and the approval of other code - and not for a
// round GitHub will not merge from, which is deduped against the mark the notice leaves on the
// decision instead (settleRound, record.ReviewDecision.ToldStuckHead): other is recomputed from
// the record as the fact found it, so it remembers nothing of an answer GitHub gave and took
// back, and `blocked`, `unknown`, `blocked` would tell twice.
func (r round) stuckAs(other round) bool {
	return r.outcome == roundStuck && other.outcome == roundStuck &&
		r.cause == other.cause && r.head == other.head
}

// stuckMergeStateReason is what the architect and the reviewer are told of an approval standing
// on a head GitHub answers for with a merge state it would not merge from: the head, the pull
// request, the state in GitHub's own spelling, and every check run GitHub reported on that head
// with the conclusion it came to (headChecks), which is the whole of what the daemon knows of
// the head's own CI. There is always one to name: the only caller builds this reason for a round
// it has already reported a head for, which it does only once GitHub has reported a check run on
// that head itself and every one of them has concluded (reviewRound,
// classify.ChecksConcludedOnHead), and that gate reads the same fence for the same thing -
// a run stamped with the current head - so headChecks names at least the run the gate found.
//
// It names the head's own check runs rather than everything the settlement standing for the head
// reports, which can be the code head's below it: GitHub weighs the current head for the merge,
// so a name whose newest run ran on another head of the tree says nothing about why this head is
// held, and naming it here would read as a run GitHub made on this one.
//
// The daemon reads no repository ruleset, so it does not know which checks the base requires
// (dispatch://INFRA-714/spec R1): a required check that has never run is in no list GitHub gives
// for the head, so it is simply absent from the checks named here. The reason says so, because a
// reader who does not know that would take a short list for the daemon leaving something out
// rather than for the finding it is - the check that is missing is the one to look for. It says
// nothing of what clears the state: a push that earns what the base requires is one way, and a
// check the base requires finishing on the head as it stands is another.
func stuckMergeStateReason(pr record.PullRequest) string {
	return fmt.Sprintf("the reviewer approved head %s on pull request #%d, and GitHub reports that head %s rather than one it would merge. %s. The daemon reads no ruleset and so names no required check: one the base requires that has never run is in nothing GitHub reports for the head, so it is absent here rather than named as missing, which makes the missing check the thing to look for. The issue waits in reviewing until GitHub reports the head clean, unstable or has_hooks",
		pr.HeadSHA, pr.Number, pr.MergeState, headChecks(pr))
}

// reviewRound is what issue's review round comes to, row being its reviewer's and pr its pull
// request as a fact left them. The review event decides it, and the reviewer's handoff records its
// findings (dispatch://INFRA-714/spec R1): a request for changes ends the round whether or not the
// reviewer has completed, and the completion that follows is recorded on the reviewer's own row
// (engine's leftItsPhase). An approval ends it once it approves the head's code
// (classify.ApprovalStands) and GitHub reports that head one the issue may advance on
// (record.MergeState.Mergeable). An approval GitHub has given no answer for yet - the state
// unread, or `unknown` while GitHub computes it - waits where it stands, and the read that lands
// moves it (mergeability, the ladder behind daemon's readMergeState). So does an approval whose
// head still has a check running: a check the base requires holds the head `blocked` while it
// runs, which says nothing about the round. What tells the two apart is the head's own check
// runs (classify.ChecksConcludedOnHead): the round reports a head only once GitHub has reported
// a check run on that head itself and every one of them has concluded. A settlement of an
// earlier head never satisfies it, and must not: the push that left this head starts CI of its
// own, and GitHub answers `blocked` while it runs. Read off an earlier head's settlement, the
// round would be reported stuck at once and the head marked told before the checks that would
// clear it had finished.
// An approval GitHub answers with a state it would not merge from, on a head whose own checks
// have all concluded, is stuck (stuckMergeState): the architect and the reviewer are told, once
// for each head and state (record.ReviewDecision.ToldStuckHead), since the checks that would
// have cleared it are over and nothing else is on its way to move the issue.
// The round reads no CI verdict: what a check concluded moves nothing and
// decides no round - only whether every check reported has concluded, which is what GitHub's own
// state is waiting for - and the reviewer reads the head's checks on GitHub itself. A completed round
// that nothing on its way would end is stuck, its reason naming the
// head, since the decision it needs is of the head: no review decided it (stuckUndecided), or its
// approval is of other code than the head's (stuckApprovedOtherCode). An approval of the head a
// push is still on its way from (classify.CodeOnItsWay) keeps the round open until the push's new
// head arrives.
func (e *Engine) reviewRound(issue record.Issue, row record.PhaseRow, pr *record.PullRequest) round {
	if issue.Phase != phase.Reviewing {
		return round{}
	}
	completed := row.Completed()
	decided := ""
	if row.Decision != nil {
		decided = row.Decision.State
	}
	if decided == "changes_requested" {
		return round{outcome: roundRejected}
	}
	if pr == nil {
		return round{}
	}
	switch {
	case decided == "approved" && classify.ApprovalStands(*pr, row.Decision.Head):
		if pr.MergeState.Mergeable() {
			return round{outcome: roundApproved}
		}
		// Everything below here waits on GitHub's answer for this head: the unread state and
		// `unknown` because GitHub has given none yet, every other because the one it gave can
		// change under the head, and a head GitHub has reported no concluded check run on
		// because a check still running there is what the state is waiting for. The daemon reads
		// for each (settleRound), and the settlement that ends the last check is itself a fact
		// that asks for a read.
		if pr.MergeState == record.MergeStateUnread || pr.MergeState == record.MergeStateUnknown || !classify.ChecksConcludedOnHead(*pr) {
			return round{waitsOnMergeState: true}
		}
		return round{outcome: roundStuck, cause: stuckMergeState, head: pr.HeadSHA, state: pr.MergeState,
			waitsOnMergeState: true, reason: stuckMergeStateReason(*pr)}
	case !completed:
		return round{}
	case decided == "":
		return round{outcome: roundStuck, cause: stuckUndecided, head: pr.HeadSHA, reason: undecidedReason(row, *pr)}
	case classify.ApprovalSetAside(*pr, row.Decision.Head):
		return round{outcome: roundStuck, cause: stuckApprovedOtherCode, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer approved %s on pull request #%d, which does not approve head %s: a push since may have changed code, so only an APPROVE of %s or a REQUEST_CHANGES ends the round",
				row.Decision.Head, pr.Number, pr.HeadSHA, pr.HeadSHA)}
	}
	return round{}
}

// undecidedReason is what the architect is told of a round its reviewer completed with no review
// that decided it (stuckUndecided): what would end the round, and, when the daemon set a review
// aside that would otherwise have decided it, why it did (record.ReviewDecision.Refused). The
// refusal is the only thing that says why the round has no decision - the review is on GitHub,
// approving what looks like the head - so a reader who was not told it would look for a review
// that never came.
func undecidedReason(row record.PhaseRow, pr record.PullRequest) string {
	reason := fmt.Sprintf("the reviewer completed its round on pull request #%d with no review that decides it: only an APPROVE of head %s or a REQUEST_CHANGES ends the round, from an account with write access to the repository or from the reviewer's own session as the review App (its Legion footer naming the session recorded on the reviewer's claim, and naming %s as the head it read: a review-App review whose footer names no head, or another head, is set aside and decides nothing, since GitHub's commit_id for a review moves with the pull request), and a COMMENT decides nothing",
		pr.Number, pr.HeadSHA, pr.HeadSHA)
	if row.Decision != nil && row.Decision.Refused != "" {
		reason += "; a review that would otherwise have decided this round was set aside - " + row.Decision.Refused
	}
	return reason
}

// implementationReady moves issue from implementing to testing on the implementer's completion of
// the round that row records, once the pull request pr is recorded open or merged
// (TriggerImplementationReady: a record closed without merging, or none, moves nothing). It
// reads no CI result: a settlement that arrived while the implementer worked moved nothing and
// moves nothing now, since the roles were told what it said when it arrived (checks,
// dispatch://INFRA-714/spec R1).
func (e *Engine) implementationReady(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, pr *record.PullRequest) error {
	return e.transition(ctx, tx, issue, TriggerImplementationReady, "", row, pr, "")
}

// settleRound acts on what issue's review round comes to (reviewRound, with row its reviewer's and
// pr its pull request as the fact left them), and says whether it moved the issue. An ended round
// moves on, its request for changes back to implementing; a stuck round is told to the architect,
// in a notice whose summary is by, the fact that wrote it - unless it was told already. For the
// two causes a review decides, that is the round having been stuck the same way (stuckAs) before
// that fact, which is before: each caller passes the round as its own fact found it, so a round
// stuck at a head the architect was told of is told once and no fact about the same head tells it
// again. The issue stays in reviewing, and the notice names the reviewer's role topic, which the
// architect asks the decision on. Linger holds a member of a closed tree where it stood
// (record.TreeLingers), and leaves no mark: nothing was told, so the head is told when the tree
// runs again.
//
// A round GitHub will not merge from (stuckMergeState) is the one cause the reviewer cannot
// decide, and the whole of its path - its own dedup, its own message to the reviewer, a notice
// with no topic - is tellStuckMergeState's, which this asks for once.
func (e *Engine) settleRound(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, pr *record.PullRequest, before round, by string) (bool, error) {
	r := e.reviewRound(issue, row, pr)
	switch r.outcome {
	case roundRejected:
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return false, err
		}
		return true, e.transition(ctx, tx, issue, TriggerReviewRejected, "", row, pr, row.Decision.Body)
	case roundApproved:
		return true, e.transition(ctx, tx, issue, TriggerReviewApproved, "", row, pr, "")
	case roundStuck:
		if r.cause == stuckMergeState {
			return false, e.tellStuckMergeState(ctx, tx, issue, row, r, by)
		}
		// Every other cause is deduped against the round as this fact found it, which the record
		// answers for.
		if r.stuckAs(before) {
			return false, nil
		}
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return false, err
		}
		reviewer, err := reviewerToken(issue)
		if err != nil {
			return false, err
		}
		return false, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "review-stuck", Role: claim.RoleReviewer, Phase: phase.Reviewing,
			Summary: by, Reason: r.reason, Topic: notify.RoleTopicPrefix + string(reviewer)})
	}
	return false, nil
}

// tellStuckMergeState tells the architect and the reviewer of the one stuck cause a review
// cannot decide: an approval of the head's code that GitHub answers for with a merge state it
// would not merge from (stuckMergeState). It is the whole of that cause's path through
// settleRound, which the rest of the function shares nothing with but the notice it ends in -
// the dedup is against another thing, the reviewer is told as well as the architect, and the
// notice names no topic - so settleRound asks which cause this is once and leaves it here.
//
// It is deduped against the mark the last notice left on the reviewer's decision (told), which
// outlives the answers GitHub gives in between: the state is read back from GitHub every time,
// and `blocked`, `unknown`, `blocked` is one finding told once. The mark is written with the
// notice, so a notice linger holds (record.TreeLingers) leaves none and the head is told when
// the tree runs again.
//
// The reviewer's own message is a role message (record.RoleMessage, the way the daemon reaches a
// role holder with something to read): the reviewer approved that head and would otherwise watch
// a round it decided go nowhere, and it is the role reading the head's checks on GitHub, which
// is where what the notice names is answered. The architect's notice names no role topic, since
// no decision of the reviewer's would move this round.
func (e *Engine) tellStuckMergeState(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, r round, by string) error {
	if told(row, r) {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	reviewer, err := reviewerToken(issue)
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.RoleMessage{Role: string(reviewer),
		Body: "The round you approved is stuck: " + r.reason + "."}); err != nil {
		return err
	}
	row.Decision.ToldStuckHead, row.Decision.ToldStuckState = r.head, r.state
	if err := e.store.PutPhase(ctx, tx, row); err != nil {
		return err
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "review-stuck", Role: claim.RoleReviewer, Phase: phase.Reviewing,
		Summary: by, Reason: r.reason})
}

// reviewerToken is the claim token of issue's reviewer, whose role topic the architect is given
// to ask the decision on and whose role a message addresses.
func reviewerToken(issue record.Issue) (claim.Token, error) {
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", err
	}
	return claim.NewToken(project, issue.Key, claim.RoleReviewer)
}

// told says whether the notice a round GitHub will not merge from would write has already gone
// out: the reviewer's decision carries the head and state it was last reported stuck on
// (record.ReviewDecision.ToldStuckHead). It answers for that cause alone, which is the only
// caller (tellStuckMergeState); every other is deduped against the round as the fact found it
// (stuckAs), which the record answers for them.
func told(row record.PhaseRow, r round) bool {
	return row.Decision != nil &&
		row.Decision.ToldStuckHead == r.head && row.Decision.ToldStuckState == r.state
}

// askMergeState asks the daemon to read GitHub's merge state for the pull request fact is of,
// when the round that fact has just been applied to leaves waiting on GitHub's answer for the
// head (round.waitsOnMergeState, Config.ReadMergeState). It is the engine's own account of what
// the round is owed rather than a second reading of the record beside it, and it runs on the
// commit, so the fact's transaction waits for no GitHub call.
//
// Every fact that can change GitHub's answer asks, and the list is this function's switch rather
// than a sentence in a comment and nine calls spread through the handlers: the pull request's own
// events, a push to it, a review, a CI settlement, and a push to the base it sits on. Apply calls
// it once, after the handler, so a handler's own early return cannot drop the read and no handler
// recomputes the round settleRound has just computed (dispatch://INFRA-714 R3). GitHub's own
// answer does not ask - the fact a read applies is mergeability, and a read that asked for
// another would read the same head for as long as GitHub kept answering the same way. What a read
// answers is the ladder's to decide (daemon's mergeStateLadder), and the next fact of the issue
// asks again.
//
// A base push is the one fact of many pull requests, and the one this leaves alone: its handler
// walks the open pull requests recorded on the base once, and asks for each read inside that
// walk (basePush), since the record it overwrites as it goes is what tells a redelivered push
// from a first delivery.
func (e *Engine) askMergeState(ctx context.Context, tx pgx.Tx, fact intake.Fact) error {
	if e.settings().ReadMergeState == nil {
		return nil
	}
	var pr *record.PullRequest
	var err error
	switch fact := fact.(type) {
	case intake.PullRequestOpened:
		pr, err = e.pullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	case intake.PullRequestSynchronized:
		pr, err = e.pullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	case intake.Push:
		pr, err = e.pullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	case intake.PullRequestChecks:
		pr, err = e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	case intake.PullRequestReview:
		pr, err = e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	case intake.BasePush:
		// Asked for inside the walk that records the push, which is the last reader of what
		// tells a redelivery from a first delivery (basePush).
		return nil
	default:
		return nil
	}
	if err != nil || pr == nil {
		return err
	}
	return e.askMergeStateFor(ctx, tx, pr)
}

// askMergeStateFor queues the read for one pull request whose round waits on GitHub's answer. The
// guard on a daemon with no reader is here as well as in askMergeState, because a base push
// reaches this from its handler's walk rather than through that switch (basePush).
func (e *Engine) askMergeStateFor(ctx context.Context, tx pgx.Tx, pr *record.PullRequest) error {
	read := e.settings().ReadMergeState
	if read == nil || pr.State != record.PullRequestOpen {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return err
	}
	reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return err
	}
	if !e.reviewRound(*issue, reviewer, pr).waitsOnMergeState {
		return nil
	}
	key := issue.Key
	intake.OnCommit(ctx, func() { read(key) })
	return nil
}

// recordPass counts the implementer one more pass through the implementing, testing and reviewing
// stretch, which every move back into an earlier phase opens, counts the move itself under its
// own pair, and fires the deployment's hooks that stand on the move (fireWorkBackHooks).
//
// The pass bounds nothing: it is the counter the handoff completion key carries
// (api/handoff.go), and its whole job is to make a completion at a commit an earlier pass
// reported a fact of its own. BackwardMoves is what a hook counts, under the move's own
// "<from>-<to>" pair, spelled as phase.BackwardPairs() spells the pairs (phase.Pair): a review
// that sends the work back and a test that sends it back are two counts and never one. Both rise
// here, where every backward move passes, rather than beside the review round, which only a
// review requesting changes opens (recordRound): a hook standing on that count would never see a
// tester's fail or a worker's own move back.
//
// A move the record holds as a conflict with the base raises no pair and fires no hook: nobody
// sent the work back, so there is no move of a kind for a hook to count, and the pass it still
// raises is the completion key's alone. Everything is written in the move's own transaction, so
// a move the record holds and a prompt nobody wrote cannot happen, and nothing here holds the
// move back: the transition starts the next role whatever a hook said.
func (e *Engine) recordPass(ctx context.Context, tx pgx.Tx, issue string, from, to phase.Phase, pr *record.PullRequest) error {
	row, err := e.phaseRow(ctx, tx, issue, claim.RoleImplementer)
	if err != nil {
		return err
	}
	row.Pass++
	if pr != nil && classify.ConflictRound(*pr) {
		return e.store.PutPhase(ctx, tx, row)
	}
	if row.BackwardMoves == nil {
		row.BackwardMoves = make(map[string]int, 1)
	}
	row.BackwardMoves[phase.Pair(from, to)]++
	if err := e.store.PutPhase(ctx, tx, row); err != nil {
		return err
	}
	return e.fireWorkBackHooks(ctx, tx, issue, from, to, row.BackwardMoves, pr)
}

// recordRound counts the implementer one more review round, which only a review that requested
// changes ends, and tells the architect once for each value review_round_cap holds. The move's
// own pair and the deployment's hooks are counted where every backward move passes (recordPass),
// so a round and a move are counted apart: this one is the cap's, and everything it writes it
// writes in the move's own transaction.
//
// What the cap reads is Rounds less the rounds a conflict with the base caused
// (classify.ConflictRound, record.PhaseRow.ConflictRounds). A round opened while the record holds
// the pull request's own head as conflicting is answered by the implementer's forward merge,
// whatever else was asked of it, so it costs the issue nothing - which is what
// dispatch://INFRA-1442 asked for, on a tree that reached the cap on three rounds of which two
// were conflicts. Rounds rises for both kinds, so the record keeps what the issue spent beside
// what it was excused, and the message names both numbers: an architect woken by it reads how
// many rounds were counted against the cap and how many were not.
//
// A conflict round posts neither the cap's message nor its notice, whatever the counted rounds
// stand at: it leaves that number as the round before it left it, and a notice is itself a way
// of counting, waking the architect to decide a round nobody spent.
//
// The cap's message and its pr-blocked notice go once for each value the cap holds, and not on
// every counted round at or above it: the round that tells writes the cap it read into
// record.PhaseRow.CapTold, and a later round compares the two. The comparison is != and never <,
// because a cap lowered under a row that already told a higher value must tell once at the new
// value, which < would never do. A raised cap tells again at its new number, and a new
// generation tells again from its own first crossing, because clearGeneration clears the mark.
//
// Nothing here holds the move back: the transition starts the next role whatever the counts read. A write that fails is still an error of the move's transaction, as
// every other write of that transaction is, and is returned rather than swallowed.
func (e *Engine) recordRound(ctx context.Context, tx pgx.Tx, issue string, pr *record.PullRequest) error {
	row, err := e.phaseRow(ctx, tx, issue, claim.RoleImplementer)
	if err != nil {
		return err
	}
	row.Rounds++
	if pr != nil && classify.ConflictRound(*pr) {
		// A conflict round ends here: it tells nobody, so the write of the two counts is all it
		// has to do.
		row.ConflictRounds++
		return e.store.PutPhase(ctx, tx, row)
	}
	counted := row.Rounds - row.ConflictRounds
	roundCap := e.settings().ReviewRoundCap
	tell := counted >= roundCap && row.CapTold != roundCap
	if tell {
		row.CapTold = roundCap
	}
	if err := e.store.PutPhase(ctx, tx, row); err != nil {
		return err
	}
	if tell {
		message := fmt.Sprintf("Issue reached review_round_cap=%d: %d rounds counted, %d conflict rounds not counted.",
			roundCap, counted, row.ConflictRounds)
		if err := e.enqueue(ctx, tx, issue, record.MessagePost{Body: message}); err != nil {
			return err
		}
		if err := e.notice(ctx, tx, issue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: message}); err != nil {
			return err
		}
	}
	return nil
}
