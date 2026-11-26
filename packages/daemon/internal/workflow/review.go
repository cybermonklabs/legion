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
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// checks applies a CI settlement to the pull request and tells the roles that work the issue what
// it says (tellChecks). It moves nothing: the settlement is information, not the daemon's to
// read the semantics of (dispatch://INFRA-714/spec R1), and the message asks no role to act on it:
// the tester reads no CI, the implementer is told of a failed check a CI job runs by its own
// message (tellCIFailure, INFRA-4843 R15a), and the reviewer and the architect read CI's state. A
// failed check that a CI job runs is judged by Dispatch's check states instead, and for a task
// that takes a fix the fix rule starts the fix (fixRule). The record as it stood before the
// settlement is kept for the report of a task that takes a fix, which names only what the
// settlement newly put on it (reportRuns).
//
// It runs for every settlement fact, after the wake (wakeOnSettlement), and each settlement it
// does not apply is named by one INFO line once the fact commits, since a pull request whose head
// got no CI message otherwise leaves nothing to say whether the daemon dropped its settlement or
// never heard it (dispatch://INFRA-4135 R6): one that no recorded pull request matches
// (intake.NoRecordedPullRequestLine, which the consumer writes for nearly every such settlement
// before any transaction, so here it names a record gone by the time the fact applies); one that
// stands for no head the record holds (classify.SettlementFor), with the record's head, the head
// its checks stand at and whether a head run is open; and one of the head that the fence takes as
// stale or conflicting (classify.ClassifySettlement), with the generation and snapshot the record
// holds. A redelivery of a settlement already applied is no drop and logs nothing. Each line
// begins with the settlement's identity (intake.SettlementIdentity), by which it is matched to
// the listener's line that published the settlement, and is written once for each identity
// (logDroppedSettlement).
func (e *Engine) checks(ctx context.Context, tx pgx.Tx, fact intake.PullRequestChecks, woken claim.Role) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil {
		return intake.Result{}, err
	}
	if pr == nil {
		e.logDroppedSettlement(ctx, fact, intake.NoRecordedPullRequestLine)
		return intake.Result{}, nil
	}
	// The listener settles every commit of the pull request, its head or not, and a settlement
	// stands only for the commit it names: it is for the head only when it names the head. While a
	// head run is open the settlement is recorded in it whether it stands or not
	// (classify.ApplyCheckSettlement). A list of commit statuses the decoder could not read costs
	// the settlement nothing else: the fact names no status, and the pull request keeps the
	// statuses it holds for the head, since a settlement's statuses are merged into those per
	// context and a context it leaves out stays as it is kept (classify.ApplySettlement,
	// intake.PullRequestChecks.StatusesUnread).
	candidate := settlementCandidate(fact)
	told := settledChecks(*pr)
	before := *pr
	settled, applied := classify.ApplyCheckSettlement(*pr, candidate)
	if !applied {
		e.logUnappliedSettlement(ctx, fact, *pr, candidate)
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
	return intake.Result{}, e.tellChecks(ctx, tx, before, pr, told, woken)
}

// settlementCandidate is the settlement fact as the apply step reads it, and the one place it is
// built from the fact: checks applies it, and the wake (wakeOnSettlement) asks the apply step
// whether it applies before it wakes anyone, so the two read one candidate and cannot drift. It is
// the fact as delivered: a list of commit statuses the decoder could not read is no status here,
// whatever the wake's task names in its place.
func settlementCandidate(fact intake.PullRequestChecks) classify.SettlementCandidate {
	return classify.SettlementCandidate{Head: fact.HeadSHA, CheckRuns: fact.CheckRuns, Generation: fact.Generation, Snapshot: fact.Snapshot,
		Failing: fact.Failing, Cancelled: fact.Cancelled, Statuses: fact.Statuses}
}

// logUnappliedSettlement names a settlement checks does not apply to pr, the record as it stood
// before the settlement: one that stands for no head the record holds, and one of its head the
// fence takes as stale or conflicting. Every other settlement not applied, the redelivery of one
// applied (classify.SettlementDuplicate) among them, logs nothing.
func (e *Engine) logUnappliedSettlement(ctx context.Context, fact intake.PullRequestChecks, pr record.PullRequest, candidate classify.SettlementCandidate) {
	stood, stands := classify.SettlementFor(pr, candidate)
	if !stands {
		e.logDroppedSettlement(ctx, fact, "workflow: a checks settlement stands for no head the record holds",
			"recorded_head", pr.HeadSHA, "checked_head", pr.CheckedHead, "head_run_open", pr.Run != nil, "issue", pr.Issue)
		return
	}
	switch classified := classify.ClassifySettlement(stood, candidate); classified {
	case classify.SettlementStale, classify.SettlementConflict:
		e.logDroppedSettlement(ctx, fact, "workflow: a checks settlement of the head was not applied",
			"classification", string(classified), "recorded_generation", stood.Generation, "recorded_snapshot", stood.Snapshot, "issue", pr.Issue)
	}
}

// logDroppedSettlement writes msg at INFO once the fact's transaction commits, after the
// settlement's identity and then args, unless the engine has logged a drop of the same settlement
// already: one settlement delivered many times writes one line, and the identity is marked in the
// commit's own hook, so a fact rolled back neither writes the line nor keeps its retry from
// writing it (Engine.droppedSettlements).
func (e *Engine) logDroppedSettlement(ctx context.Context, fact intake.PullRequestChecks, msg string, args ...any) {
	identity := fact.Identity()
	args = append(identity.Attributes(), args...)
	store.OnCommit(ctx, func() {
		if e.droppedSettlements.Add(identity) {
			e.log.Info(msg, args...)
		}
	})
}

// conflictConfirmWait is how long after a first CONFLICTING read of a head the read that can
// confirm it comes, at the least (dispatch://INFRA-4374 R2). GitHub documents that `mergeable` is
// computed by a background job and that a client asks again "after giving the job time to
// complete", and it documents no time for the job (https://docs.github.com/en/rest/pulls/pulls,
// "Get a pull request"). The daemon already waits 30 seconds between reads of a head GitHub
// answers `unknown` for (the daemon's mergeStateWait); the spec's floor of one minute is two of
// those waits, and this is that floor. It is compared with the reads' own times, and with the
// engine's clock where a fact carries none, so tests control it.
const conflictConfirmWait = time.Minute

// mergeability records what GitHub's last read found for whether the pull request's head can be
// merged into its base (intake.PullRequestMergeability, readPullRequest's own GitHub answer), and
// acts on a conflict only once a second read has confirmed it and R0's decision says the head
// must be merged forward now (classify.ConflictNeedsMerge).
//
// A head that conflicts with its base is judged in two reads (dispatch://INFRA-4374 R2). GitHub
// computes mergeability in a background job and can answer CONFLICTING about a base it has not
// caught up with, so the first CONFLICTING read of an open pull request's head records itself
// (record.PullRequest.ConflictRead: the head, the base branch and the time of the read), queues the
// read that asks GitHub again once conflictConfirmWait has passed (record.ConflictConfirm), logs
// itself, and acts on nothing. A later CONFLICTING read of the same head and the same base branch,
// made conflictConfirmWait or more after the first, confirms the conflict: the record holds the
// head conflicting (ConflictingHead) from then on, drops the first read, and decides. A
// CONFLICTING read sooner than the wait changes nothing of the first read, and one of another
// head or another base branch is a first read of its own. The base branch is compared by name and
// never by commit: GitHub's base.sha does not name the base it computed against, and a conflict
// with a newer base is still a conflict. A MERGEABLE read releases the first read and logs both
// reads. An UNKNOWN read leaves it as it is, and the second read's own row releases it once its
// last attempt still answers UNKNOWN, while the first read is still the one the row was queued for
// (conflictConfirmDone, record.ConflictConfirm.FirstReadAt). After a release, the next CONFLICTING
// read is a first read again, with a row of its own.
//
// The decision is R0's (dispatch://INFRA-4374 R0, classify.ConflictNeedsMerge), read on the
// base's rules for its required checks, which the confirming read carries
// (intake.PullRequestMergeability.Rules). A reason to act asks the implementer, or in merging the
// merger, to merge the base forward, and the issue keeps its phase (mergeForward, R1); no reason
// leaves the conflict recorded and logged with the checks the base requires and those green on
// the head, and the task's steps go on at that head. A CONFLICTING read of a head the
// record already holds conflicting decides again in the same way, since the phase the issue has
// reached since can need what the phase before did not, and mergeForward asks nothing twice for
// one head.
//
// GitHub's own merge state for the head rides along on the same fact: it is what says whether
// GitHub would merge this head now, which the tri-state mergeability cannot, and it is what an
// approval standing on the head waits for (reviewRound). So in reviewing the round is asked again
// once the state is recorded, whatever the read decided - this read is the one that moves an
// approved issue on to its task's next step (accepting, in the default steps), as often as not.
// The base the read names is recorded with
// them, since that read is the only one that names the base of a pull request opened before the
// record kept one: it is what a push onto that branch finds the pull request by (basePush), and a
// read that answers for a base push already recorded clears the push's mark, the two having been
// read together.
//
// A read that answers for another head than the record stands at is dropped whole, and logged: a
// push landed between GitHub's answer and this apply, so the verdict is about a tree the record
// has moved past, and an approval must never advance on it. Nothing is lost by dropping it - that
// push is itself a fact that asks for a read of the head it left (askMergeState). The read time
// goes with it: a stamp is never recorded against a head the record has already left, since it
// would say that head's state was read when it was another head's that was.
//
// A read GitHub answered before the read the record holds of its head
// (record.PullRequest.StateReadAt) is dropped whole as well, and logged: nothing is written,
// nothing decided and no round asked again. The readers of the pull request run apart, and the
// per-issue read dates its answer before it follows the head under the fact lock (daemon's
// readIssuePullRequest), so a MERGEABLE answer made before a push to the base can be applied after
// the CONFLICTING answer of a read made since. Applied, it would release that conflict's first
// read, and the record would say mergeable while GitHub says CONFLICTING. A fact that carries no
// read time is never dropped for its time.
//
// The read time the fact carries (intake.PullRequestMergeability.ReadAt) is what
// record.PullRequest.StateReadAt is written from, and it is the one thing a read that confirms
// what is recorded still moves: the row is written for it alone. A fact that carries none - every
// one minted by something that did not read GitHub, as a test's PullRequestMergeability is -
// writes no stamp and erases none, so the recorded one stands as the last real read left it. The
// two reads of a conflict are timed by the same read time, and by the engine's clock for a fact
// that carries none.
//
// Whatever the read decides, it is also the judgement the conflict-round exemption is read from
// (record.PullRequest.ConflictingHead, recordRound): a confirmed conflict holds the pull request's
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
	if !fact.ReadAt.IsZero() && fact.ReadAt.Before(pr.StateReadAt) {
		e.logOnCommit(ctx, "workflow: GitHub's merge state answered before the read the record holds; the read is dropped",
			"issue", pr.Issue, "pullRequest", pr.Number, "head", pr.HeadSHA, "readAt", fact.ReadAt, "stateReadAt", pr.StateReadAt,
			"mergeable", string(fact.Mergeable), "mergeState", string(fact.MergeState))
		return intake.Result{}, nil
	}
	base := pr.Base
	if fact.Base != "" {
		base = fact.Base
	}
	readAt := fact.ReadAt
	if readAt.IsZero() {
		readAt = e.now()
	}
	prior := *pr
	conflicting, first, decide := pr.ConflictingHead, pr.ConflictRead, false
	switch fact.Mergeable {
	case record.MergeabilityConflicting:
		switch {
		case pr.State != record.PullRequestOpen:
			// A pull request that is not open merges nothing, so its conflict is nothing to act on.
		case conflicting == pr.HeadSHA:
			decide = true
		case first != nil && first.Head == pr.HeadSHA && first.Base == base:
			if readAt.Sub(first.At) >= conflictConfirmWait {
				conflicting, first, decide = pr.HeadSHA, nil, true
			}
		default:
			first = &record.ConflictRead{Head: pr.HeadSHA, Base: base, At: readAt}
			if err := e.enqueueAt(ctx, tx, pr.Issue, record.ConflictConfirm{Number: pr.Number, Head: pr.HeadSHA, FirstReadAt: readAt}, readAt.Add(conflictConfirmWait)); err != nil {
				return intake.Result{}, err
			}
			e.logOnCommit(ctx, "workflow: a conflict read waits for a second read",
				"issue", pr.Issue, "pullRequest", pr.Number, "head", pr.HeadSHA, "base", base, "readAt", readAt)
		}
	case record.MergeabilityMergeable:
		if conflicting == pr.HeadSHA {
			conflicting = ""
		}
		if first != nil {
			e.logOnCommit(ctx, "workflow: a second read did not confirm the conflict",
				"issue", pr.Issue, "pullRequest", pr.Number, "head", first.Head, "base", first.Base,
				"firstReadAt", first.At, "readAt", readAt, "mergeable", string(fact.Mergeable), "mergeState", string(fact.MergeState))
			first = nil
		}
	}
	stateReadAt := pr.StateReadAt
	if !fact.ReadAt.IsZero() {
		stateReadAt = fact.ReadAt
	}
	// The last clause is the confirming read's: it answers what is already recorded, and the time
	// it was read is the whole of what it has to say.
	if pr.Mergeability != fact.Mergeable || pr.MergeState != fact.MergeState || pr.Base != base || pr.BaseMoved != "" ||
		pr.ConflictingHead != conflicting || pr.ConflictRead != first || !stateReadAt.Equal(pr.StateReadAt) {
		pr.Mergeability, pr.MergeState, pr.Base, pr.BaseMoved, pr.ConflictingHead, pr.ConflictRead, pr.StateReadAt =
			fact.Mergeable, fact.MergeState, base, "", conflicting, first, stateReadAt
		if err := e.putPullRequest(ctx, tx, *pr); err != nil {
			return intake.Result{}, err
		}
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if decide {
		w, err := e.walk(ctx, tx, *issue)
		if err != nil {
			return intake.Result{}, err
		}
		green := classify.GreenContexts(*pr)
		if why := classify.ConflictNeedsMerge(*issue, w.Line, fact.Rules, green); why != "" {
			if err := e.mergeForward(ctx, tx, *issue, w, pr, why); err != nil {
				return intake.Result{}, err
			}
		} else {
			// Rules is nil for a read that carried none, which a place where the merge comes next
			// before merging decides on too: the conflict waits there for merging whatever the rules
			// (classify.ConflictNeedsMerge, classify.MergeComesNext).
			var required []string
			if fact.Rules != nil {
				required = fact.Rules.Required
			}
			e.logOnCommit(ctx, "workflow: a confirmed conflict needs no merge yet",
				"issue", pr.Issue, "pullRequest", pr.Number, "head", pr.HeadSHA, "base", base, "phase", issue.Phase,
				"required", required, "green", green)
		}
	}
	return intake.Result{}, e.reaskRound(ctx, tx, *issue, pr, prior, byMergeState)
}

// settledChecks is what the CI settlement standing for the pull request's head says: the head it
// settled on, and each check it reports with what it came to, in name order; "" while no
// settlement stands. A check the settlement names as failed failed, one it names as cancelled was
// cancelled, one whose run GitHub skipped (record.RunSkipped) was skipped, and one it reports as a
// run and names no other way succeeded - the listener settles a commit once every check it has
// seen is terminal. Nothing here judges those conclusions: a job GitHub never started is named
// skipped and decides nothing, and what they mean for the work is the reading worker's
// (dispatch://INFRA-714/spec R1). The line is the whole of what the daemon knows of that head's
// CI, so two settlements that leave it the same say nothing new (tellChecks).
//
// Each check is named by the word GitHub gave its run (record.AttemptRun.GitHubConclusion) where
// the settlement carried one, and by the daemon's conclusion where it did not (shownAs). GitHub's
// word is what tells a check that ran from one that did not, where the daemon's conclusion takes
// every run that neither failed nor was cancelled as succeeded or skipped. A skipped check ran
// nothing on the head, and a role that read it as a pass would believe a suite held back for the
// merge queue passed on a head where it never ran (dispatch://INFRA-4135 R1), so where the line
// names any check skipped it says once, after the commit statuses, that a skipped check ran
// nothing (skippedClause). A settlement that carries no word and names no run skipped, as a
// listener of an earlier release sends, gives the line the daemon gave before it read either.
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
//
// After the check runs it names each commit status the record keeps for the checked head, with
// its state, in context order (namedStatuses), and nothing where it keeps none, so the line of a
// head that carries no status reads as it did before the daemon kept any. A status is an entry of
// its own kind, apart from the check runs: a context that shares a check run's name is named in
// both places.
func settledChecks(pr record.PullRequest) string {
	if pr.CheckedHead == "" {
		return ""
	}
	conclusions := make(map[string]record.RunConclusion, len(pr.CheckRuns)+len(pr.Failing)+len(pr.Cancelled))
	for _, run := range pr.CheckRuns {
		conclusions[run.Name] = record.RunSucceeded
		if run.Conclusion == record.RunSkipped {
			conclusions[run.Name] = record.RunSkipped
		}
	}
	for _, name := range pr.Cancelled {
		conclusions[name] = record.RunCancelled
	}
	for _, name := range pr.Failing {
		conclusions[name] = record.RunFailed
	}
	shown := make(map[string]string, len(conclusions))
	for name, conclusion := range conclusions {
		shown[name] = string(conclusion)
	}
	// The fence holds one run per name (classify's mergeAttemptSets), the run that decided it.
	for _, run := range pr.CheckRuns {
		shown[run.Name] = shownAs(run, conclusions[run.Name])
	}
	statuses := namedStatuses(keptStatuses(pr, pr.CheckedHead))
	if len(shown) == 0 {
		return "CI settled at " + pr.CheckedHead + " reporting no check" + statuses
	}
	return "CI settled at " + pr.CheckedHead + ": " + namedConclusions(shown) + statuses + skippedClause(shown)
}

// headChecks is every check run GitHub reported on the pull request's current head with what that
// run concluded, in name order; "" when the record holds none. It is the line the notice of a head
// GitHub will not merge from names (stuckMergeStateReason), where the settlement's own line would
// not be true: a settlement standing for the head can be of the code head below it, and a name
// whose newest run is there ran on other heads of the tree and not on this one. What GitHub
// weighs for the merge is the current head, so that is the head whose checks the notice names,
// and a required check whose run is missing here is the thing its reader looks for.
//
// Each run is named by the word GitHub gave it where the settlement carried one, and by its
// conclusion where it did not (shownAs), and where any run is named skipped the line says once,
// after the commit statuses, that a skipped check ran nothing on this head (skippedClause), as
// settledChecks does and for the same reason: a required check GitHub skipped is one that never
// ran there, and its reader must not take it for a pass (dispatch://INFRA-4135 R1).
//
// Each run of the fence carries the head it ran on and its conclusion (record.AttemptRun), so
// this is a reading of the fence and not of a settlement, through classify.HeadRuns, the one test
// of a run's head the daemon's readers share: a check run on this head that a newer run of the
// same name on another head has replaced in the fence is not named, and nor is a run stored before
// the fence carried its head.
//
// After the runs it names each commit status the record keeps for the current head, with its
// state (namedStatuses), when the checked head is the current head (classify.Settled), and none
// otherwise: a status the record keeps is stamped with the head whose settlement named it, and one
// of another head says nothing of this one.
func headChecks(pr record.PullRequest) string {
	runs := classify.HeadRuns(pr)
	shown := make(map[string]string, len(runs))
	for _, run := range runs {
		shown[run.Name] = shownAs(run, run.Conclusion)
	}
	if len(shown) == 0 {
		return ""
	}
	var statuses []record.CommitStatus
	if classify.Settled(pr) {
		statuses = keptStatuses(pr, pr.HeadSHA)
	}
	return "GitHub reported on head " + pr.HeadSHA + ": " + namedConclusions(shown) + namedStatuses(statuses) + skippedClause(shown)
}

// shownAs is what a text names run's check as, conclusion being what the text concludes the check
// came to: the word GitHub gave the run (record.AttemptRun.GitHubConclusion) where the settlement
// carried one and the run's own conclusion is that conclusion, and the conclusion otherwise. The
// word is the run's, so it names the check only where that run is what decided it: a name the
// settlement's lists give another conclusion than its run's is named by the lists' conclusion, and
// a run with no word - from a listener that sends none, or a word the decoder refused - by the
// daemon's conclusion, as before the daemon kept a word. Unlike a check's name, the word is shown
// unquoted: the decoder keeps only a word of lower-case letters and underscores (intake's
// conclusionWord), so it can carry nothing else into the agent's prompt the line is read into.
func shownAs(run record.AttemptRun, conclusion record.RunConclusion) string {
	if run.GitHubConclusion != "" && run.Conclusion == conclusion {
		return run.GitHubConclusion
	}
	return string(conclusion)
}

// namedConclusions is each check name with what the line names it as (shownAs: GitHub's word where
// the settlement carried one, else the daemon's conclusion), in name order, as both lines above
// spell it.
func namedConclusions(shown map[string]string) string {
	names := make([]string, 0, len(shown))
	for name := range shown {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, name := range names {
		names[i] = strconv.Quote(name) + " " + shown[name]
	}
	return strings.Join(names, ", ")
}

// skippedRanNothing is what each text the roles read of a head's CI says once where it names a
// check skipped: GitHub skipped the check, so it ran nothing on the head, and a role must not read
// it as a pass (dispatch://INFRA-4135 R1).
const skippedRanNothing = "a skipped check ran nothing on this head"

// skippedClause is what settledChecks and headChecks add after the commit statuses where they name
// any check skipped, by GitHub's word or by the daemon's conclusion, the two being spelled alike
// (record.RunSkipped); "" where they name none, so a line with no skipped check reads as before.
func skippedClause(shown map[string]string) string {
	for _, word := range shown {
		if word == string(record.RunSkipped) {
			return "; " + skippedRanNothing
		}
	}
	return ""
}

// keptStatuses is every commit status pr keeps for head (record.PullRequest.Statuses), in context
// order; none when what pr keeps is another head's. The stamp is what keeps a list out of a text
// about another head (record.CommitStatus.StampedWith): a daemon of an earlier release moves
// checked_head after a rollback and leaves the list as this one left it (record.CommitStatus.Head).
func keptStatuses(pr record.PullRequest, head string) []record.CommitStatus {
	var kept []record.CommitStatus
	for _, status := range pr.Statuses {
		if status.StampedWith(head) {
			kept = append(kept, status)
		}
	}
	slices.SortFunc(kept, func(a, b record.CommitStatus) int { return strings.Compare(a.Context, b.Context) })
	return kept
}

// namedStatuses is what settledChecks and headChecks add after the check runs for statuses: each
// context, quoted as a check name is (namedConclusions), with its state as GitHub spells it, in
// the order given; "" for none.
func namedStatuses(statuses []record.CommitStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	named := make([]string, len(statuses))
	for i, status := range statuses {
		named[i] = strconv.Quote(status.Context) + " " + string(status.State)
	}
	return "; commit statuses: " + strings.Join(named, ", ")
}

// tellChecks tells the roles that work the issue what the settlement now standing for the pull
// request's head says, and moves nothing: the daemon passes the CI result on and asks nobody
// anything of it, no re-run, no wait and no fix, since the tester reads no CI (INFRA-4843 R17a)
// and each other role's prompt says what it does with CI's state (dispatch://INFRA-714/spec R1).
// before is the record as it stood before the settlement was applied, pr the record after it.
// The receivers are three roles (ciReceivers) - the worker holding the issue's phase, the
// implementer, whose code the checks ran on, and the architect that owns the issue
// (owningArchitect) - each on its own role topic, the way the daemon reaches a role holder with a
// message. In merging the implementer is not told: the merge and what breaks it are the merger's
// to decide, and the merger asks the implementer when it needs it (cybermonklabs/legion#4979). A
// held issue keeps the worker of Hold.From (record.StandsAt). A role named twice, the implementer
// holding the phase, is told once, and a phase no worker holds names none (addressRoles). Linger
// holds a member of a closed tree where it stood (record.TreeLingers): every role of the tree is
// suspended there, so the settlement is recorded and nobody is told.
//
// A task whose run takes a fix (record.PlanTask.FixesCI) gets the listener's report instead
// (reportRuns): each receiver one row of the runs and statuses the settlement newly put on the
// record, which assigns nothing, since Dispatch's state of each check decides and the fix rule
// alone assigns a repair (fixRule).
//
// Every other task is told as main tells it: told is what the settlement standing for the head
// said before this one (settledChecks), so a second settlement of the same head that leaves the
// same conclusions tells nobody again, and so does a settlement that stands for no head. Each
// receiver gets the settlement's line in one role message (record.RoleMessage), but where Legion
// itself acts next for the task (ciNext: the task held at its end for its CI or for its fix, a fix
// that runs, the testing move waiting for Dispatch's states), which then gets one report of the
// settlement's runs and statuses naming that act (record.CINews with Next) in its place, so no
// receiver is told to decide what Legion decides. A settlement that reports no run and no status
// has nothing such a report can carry, and is told by the role message.
//
// A role whose released pod this same settlement woke is told no role message: its wake task
// already names the settlement (WakeTask with pods.WakeChecks), and a message about the same
// settlement would reach it as a second wake of one release, described to the worker as something
// somebody published to its role topic. woken is that role, "" when the settlement woke nobody,
// and every other role — a live one, and one the wake did not reach — is told as before. A
// report that names what Legion does next reaches the woken role all the same: it carries the act
// the wake's detail names (settlementDetail), and as a row of no reader it is finished undelivered
// for a released claim, as a role message is.
//
// A holder this daemon cannot name is an error here, and with it the settlement's whole
// transaction: the settlement moves no issue, so there is nothing kept by telling the rest, and
// the next settlement of the head asks again.
func (e *Engine) tellChecks(ctx context.Context, tx pgx.Tx, before record.PullRequest, pr *record.PullRequest, told string, woken claim.Role) error {
	settled := settledChecks(*pr)
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return err
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	w, err := e.walk(ctx, tx, *issue)
	if err != nil {
		return err
	}
	fixesCI, err := e.store.RunFixesCI(ctx, tx, issue.Key, issue.Generation)
	if err != nil {
		return err
	}
	if fixesCI {
		if err := e.reportRuns(ctx, tx, *issue, w, before, *pr); err != nil {
			return err
		}
	}
	if settled == "" || settled == told {
		return nil
	}
	recipients, err := e.ciReceivers(ctx, tx, *issue)
	if err != nil {
		return err
	}
	runs, statuses := settledRuns(*pr)
	body := fmt.Sprintf("Pull request #%d: %s. Nothing moved on it.", pr.Number, settled)
	return e.eachRecipient(issue.Project, recipients, func(to roleRecipient, token claim.Token) error {
		next, err := e.ciNext(ctx, tx, *issue, w, to.role)
		if err != nil {
			return err
		}
		if next.Next == record.CINextNone || (len(runs) == 0 && len(statuses) == 0) {
			if to.role == woken {
				return nil
			}
			return e.enqueue(ctx, tx, to.row, record.RoleMessage{Role: string(token), Body: body})
		}
		if next.Tested == pr.CheckedHead {
			next.Tested = ""
		}
		return e.enqueue(ctx, tx, to.row, record.CINews{Purpose: record.CINewsRuns, Role: string(token), Generation: issue.Generation,
			Task: w.Task, Repo: pr.Repo, PullRequest: pr.Number, Head: pr.CheckedHead, Next: next.Next, Tested: next.Tested,
			Runs: runs, Statuses: statuses, Body: runsBody(issue.Key, pr.Number, pr.CheckedHead, runs, statuses, next)})
	})
}

// owningArchitect is the key of the issue whose architect owns issue now: the end of the owner
// walk from issue itself, as the outbox routes a notice of issue's phase, over the architect
// claims of issue's tree this transaction reads (record.ReadOwnerWalk). That is issue's own
// architect while its claim holds its role, else the nearest one above it whose claim does, else
// the tree root's. A tree root is its own without a read.
func (e *Engine) owningArchitect(ctx context.Context, tx pgx.Tx, issue record.Issue) (string, error) {
	if claim.IsTreeRoot(issue.Key, issue.Tree) {
		return issue.Tree, nil
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return "", err
	}
	walk, err := record.ReadOwnerWalk(ctx, e.store, tx, project, issue.Tree)
	if err != nil {
		return "", err
	}
	return walk.Owner(issue, false), nil
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
// (intake.PullRequestReview.LegionSession), matched against reviewerSession, the session the
// claims record holds for the claim the daemon starts the reviewer under (reviewersClaim), in the
// reviewer's first round as in every later one. It is the one predicate of the reviewer's own
// review: it says who decides a round (DecidesRound), whose review is the reviewer's answer
// (reviewersAnswer), and whose review moves the commit the reviewer read
// (record.PhaseRow.ReviewedHead). With no session recorded, no review is the reviewer's, since
// nothing recorded says one is: the outbox makes the reviewer's claim with none, and the claim
// holds none until the reviewer's agent registers, nor again after its session is forgotten until
// the next one registers. A review-App review that comes then waits (RefusedWhileNoSession).
func (e *Engine) byReviewer(review intake.PullRequestReview, reviewerSession string) bool {
	return e.byReviewApp(review.Author) && reviewerSession != "" && review.LegionSession() == reviewerSession
}

// DecidesRound says whether review may decide a review round: it is the reviewer's own review-App
// session (byReviewer, with reviewerSession as ReviewerSession reads it), or its author has write
// access or higher to the repository, which intake read from GitHub before the fact reached this
// transaction (intake.ResolveAuthorCanWrite, for a webhook's review and the daemon's own read of a
// pull request's reviews alike). On a public repository any account can review a pull request, so
// anyone else's review decides nothing. GitHub's 404, or a 403 that is not its rate limit, reads as
// no write access; any other failed read is retried before the review reaches here. A review-App
// review that is not the reviewer's own session decides nothing either, in the reviewer's first
// round as in every later one: it is set aside, not counted as a writer's - the daemon never asks
// GitHub about the review App's own login (daemon's reviewerCanWrite) - or dropped as a
// stranger's. It is the half of the whole judgement (RefusesRound) that answers for who wrote a
// review. A refusal it leaves stands at every head (RefusedAtEveryHead), since an account's
// standing and a session's claim are not things the record's next head changes, save one: a
// review-App review that comes while the reviewer's claim records no session waits
// (RefusedWhileNoSession), since the session the claim records later says whose it is.
func (e *Engine) DecidesRound(review intake.PullRequestReview, reviewerSession string) bool {
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
	// Lasts is how long the refusal stands, read only where Reason is not "".
	Lasts RefusalLasts
}

// RefusalLasts is how long a refusal of a review stands (RoundRefusal.Lasts), one of its three
// values. The zero value is none of them, so a refusal whose writer names no Lasts stands at no
// head: the reviewer's row keeps nothing of it (review), and the daemon's read does not remember
// it (daemon's reviewToApply).
type RefusalLasts int

const (
	// RefusedAtThisHead is a refusal at the head the record holds now and no further: a review-App
	// review refused for the head its footer names, since the head the record holds changes. It is
	// the one refusal the reviewer's row keeps for the stuck notice (review), as a decision the
	// round could still have had, and the next read of the pull request's reviews judges the review
	// again at the head the record holds then (daemon's reviewToApply).
	RefusedAtThisHead RefusalLasts = iota + 1
	// RefusedAtEveryHead is a refusal that stands whatever the record comes to hold: who wrote the
	// review (DecidesRound), and a person's review read from GitHub's list, whose commit_id is
	// GitHub's own and moves with the pull request. It is no decision the round lost, so the
	// reviewer's row keeps nothing of it, and the daemon's read remembers it (reviewToApply).
	RefusedAtEveryHead
	// RefusedWhileNoSession is the refusal of a review App's review only because the reviewer's
	// claim records no session yet, so that nothing recorded says whether it is the reviewer's own:
	// the review waits. It is no decision the round lost, so the reviewer's row keeps nothing of it
	// for the stuck notice (review), and it does not stand at every head: the next read of the pull
	// request's reviews (daemon's reviewToApply) judges it again by the session recorded then.
	RefusedWhileNoSession
)

// RefusesRound is the whole of why review, one that decides (intake.PullRequestReview.Decides),
// must not decide pr's round, a RoundRefusal with no reason when nothing refuses it: who wrote it
// (DecidesRound, with reviewerSession as ReviewerSession reads it), then the head it says it read
// (refusesRound), where besideFix, the commit the running review step started at beside a fix of
// its task as ReviewBesideFix reads it, decides beside pr's head. A review-App review judged while
// reviewerSession is "" waits (RefusedWhileNoSession). Two places judge a review - this engine,
// for the review a webhook delivers, and the daemon's read of GitHub's list of a pull request's
// reviews (daemon's reviewToApply), which applies only a review this would take - and both ask
// this one judgement, so neither judges by a narrower rule than the other, and neither asks a
// second one to learn how long a refusal stands: the answer carries that (RoundRefusal.Lasts).
func (e *Engine) RefusesRound(review intake.PullRequestReview, pr record.PullRequest, reviewerSession, besideFix string) RoundRefusal {
	if !e.DecidesRound(review, reviewerSession) {
		switch {
		case !e.byReviewApp(review.Author):
			return RoundRefusal{Reason: "its author is neither the review App nor an account with write access to the repository", Lasts: RefusedAtEveryHead}
		case reviewerSession == "":
			return RoundRefusal{Reason: "the reviewer's claim records no session, so the next read of the pull request's reviews judges it again", Lasts: RefusedWhileNoSession}
		}
		return RoundRefusal{Reason: "the review App submitted it from a session that is not the reviewer's", Lasts: RefusedAtEveryHead}
	}
	return e.refusesRound(review, pr, besideFix)
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
//     exactly, or besideFix, the commit the running review step started at where a fix of its
//     task ran beside it ("" for none, ReviewBesideFix), so its refusal stands at the head the
//     record holds now and no further. The footer is the review's own word for the code it read,
//     written in the body it was submitted with; no clock answers for a force push that puts an
//     earlier head back, and GitHub's commit_id for the review moves with the pull request. A
//     review step that started at X ends with the review of X, also after the fix beside it
//     pushed, since only the fix pushes while it runs.
//   - Every other review - a person's, delivered by its webhook - decides as it did before.
func (e *Engine) refusesRound(review intake.PullRequestReview, pr record.PullRequest, besideFix string) RoundRefusal {
	if !e.byReviewApp(review.Author) {
		if review.ReadFromList {
			return RoundRefusal{Reason: "a person's review read from the list", Lasts: RefusedAtEveryHead}
		}
		return RoundRefusal{}
	}
	head := review.LegionHead()
	switch {
	case head == "":
		return RoundRefusal{Reason: "footer names no head", Lasts: RefusedAtThisHead}
	case head == pr.HeadSHA || head == besideFix:
		return RoundRefusal{}
	case besideFix != "":
		return RoundRefusal{Reason: fmt.Sprintf("footer names %s, not the head %s nor %s, the commit its review step started at beside a fix", head, pr.HeadSHA, besideFix),
			Lasts: RefusedAtThisHead}
	}
	return RoundRefusal{Reason: fmt.Sprintf("footer names %s, not the head %s", head, pr.HeadSHA), Lasts: RefusedAtThisHead}
}

// ReviewBesideFix is the commit the running review step of issue's task started at, where the
// task's latest fix (record.Store.LatestFix) has started and ran beside that step's run: the step
// started while the fix read its job or ran (bindBesideFix), or the fix started while the step ran
// (tellStepsOfFix). Both are marked on the step's start (record.StepStart.BesideFix), so no clock
// decides it. It is "" otherwise: an issue with no record or no plan rows, one that stands in no
// step of type review, a run of that step that has ended or names no start commit, a task whose
// latest fix has not started, and a step that started after that fix ended. While a fix runs only
// the fix pushes to the task's branch (fixPushSentence, tellStepsOfFix), so a head that moved since
// that start is the fix's: the review of that commit decides the step (refusesRound, settleRound),
// the commit the reviewer read moves to it (record.PhaseRow.ReviewedHead), and the fix's push
// re-asks no round (reaskRound). The daemon's read of a pull request's reviews reads it in the
// transaction that reads the reviewer's session, before it judges one (RefusesRound), so both
// judge a review beside a fix alike.
func (e *Engine) ReviewBesideFix(ctx context.Context, tx pgx.Tx, issue string) (string, error) {
	recorded, err := e.store.Issue(ctx, tx, issue)
	if err != nil || recorded == nil {
		return "", err
	}
	return e.reviewBesideFix(ctx, tx, *recorded)
}

// reviewBesideFix is ReviewBesideFix of issue as the fact's transaction holds it.
func (e *Engine) reviewBesideFix(ctx context.Context, tx pgx.Tx, issue record.Issue) (string, error) {
	w, err := e.walk(ctx, tx, issue)
	if err != nil || !w.Stored {
		return "", err
	}
	step, ok := standingStep(w, issue)
	if !ok || step.typ != plan.TypeReview || step.place.Kind != record.PlaceStep {
		return "", nil
	}
	start, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
	if err != nil || start.Run != record.RunOpen || start.StartCommit == "" {
		return "", err
	}
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.StartedAt == nil || start.BesideFix != fix.Fix {
		return "", err
	}
	return start.StartCommit, nil
}

// ReviewerSession is the session of issue's reviewer, the session a review-App review's Legion
// footer must name to be the reviewer's own (byReviewer), read as review reads it
// (reviewersClaim): "" when the issue has no record, the reviewer's claim has no row, or its
// session is empty, in which case no review-App review is the reviewer's, and each waits
// (RefusedWhileNoSession). The daemon's read of a pull request's reviews reads it before it judges
// one (RefusesRound), as review does, so both judge a review-App review alike, in the reviewer's
// first round as in every later one.
func (e *Engine) ReviewerSession(ctx context.Context, tx pgx.Tx, issue string) (string, error) {
	recorded, err := e.store.Issue(ctx, tx, issue)
	if err != nil || recorded == nil {
		return "", err
	}
	_, session, err := e.reviewersClaim(ctx, tx, *recorded)
	return session, err
}

// reviewersClaim is the claim the daemon starts issue's reviewer under (reviewerToken) and the
// session the claims record holds for it (record.Store.ClaimSession), the session of that claim's
// current launch: the one reading of who issue's reviewer is, for every judgement of a review-App
// review (byReviewer). It finds the claim by that token, never by the claim the reviewer's row
// names: the completion handler is the one writer of record.PhaseRow.Claim, and the reviewer
// submits the review of its first round before it completes, so read from the row no session
// would be recorded in round 1, and no review-App review, the reviewer's own among them, could
// decide that round. The session is "" when the claim has no row or records none: the outbox makes
// the claim with none, and only the registration of the reviewer's agent writes one.
func (e *Engine) reviewersClaim(ctx context.Context, tx pgx.Tx, issue record.Issue) (claim.Token, string, error) {
	token, err := reviewerToken(issue)
	if err != nil {
		return "", "", err
	}
	session, err := e.store.ClaimSession(ctx, tx, token)
	return token, session, err
}

// reviewersAnswer says whether fact is the reviewer's answer to a round it completed undecided: a
// review from the reviewer's own review-App session (byReviewer), more than answerSkew after the
// completion was applied. A review submitted before the completion but delivered after it is not,
// since the completion comes through the API and the review by webhook; nor is one with no
// submission time, or anyone else's, the review App's other sessions included.
func (e *Engine) reviewersAnswer(fact intake.PullRequestReview, reviewer record.PhaseRow, reviewerSession string) bool {
	return e.byReviewer(fact, reviewerSession) && fact.SubmittedAt.After(reviewer.CompletedAt.Add(answerSkew))
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
	resolvedIssue(ctx, issue.Key)
	reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return intake.Result{}, err
	}
	// reviewerSession is the session of the reviewer's claim (reviewersClaim), "" when none is
	// recorded, in which case a review-App review waits (RefusedWhileNoSession). It is read only for
	// a review the review App submitted: no other author's review ever reaches the compare.
	var reviewerClaim claim.Token
	var reviewerSession string
	if e.byReviewApp(fact.Author) {
		reviewerClaim, reviewerSession, err = e.reviewersClaim(ctx, tx, *issue)
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
	// review may decide this round (RefusesRound): who wrote it, and the head it says it read, which
	// beside a fix may be the commit the review step started at (ReviewBesideFix).
	// Any other review orders nothing either, so a comment written after a decision but
	// delivered before it, or an outsider's review, cannot make the decision look old.
	state := strings.ToLower(fact.State)
	decides := fact.Decides()
	// refused is why a review the round's rules otherwise count was refused the round at the head
	// the record holds now (RefusedAtThisHead). It is kept on the reviewer's row below, since nothing
	// else would say why the round has no decision. A refusal that stands at every head
	// (RefusedAtEveryHead) leaves none: a stranger's review is no decision the round lost, and the
	// round is undecided the way one nobody reviewed is. Nor does a review that waits for the
	// reviewer's session (RefusedWhileNoSession): no rule of the round counts it yet, and the read
	// that judges it again by the session recorded then either takes it or refuses it afresh.
	refused := ""
	if decides {
		// besideFix is read only for a review the review App submitted: no other author's review is
		// held to the head its footer names.
		besideFix := ""
		if e.byReviewApp(fact.Author) {
			if besideFix, err = e.reviewBesideFix(ctx, tx, *issue); err != nil {
				return intake.Result{}, err
			}
		}
		if refusal := e.RefusesRound(fact, *pr, reviewerSession, besideFix); refusal.Reason != "" {
			args := []any{"issue", issue.Key, "pull_request", pr.Number, "author", fact.Author, "state", state,
				"footer_session", fact.LegionSession(), "reviewer_session", reviewerSession, "reviewer_claim", string(reviewerClaim),
				"footer_head", fact.LegionHead(), "head", pr.HeadSHA, "beside_fix", besideFix, "read_from_list", fact.ReadFromList,
				"body_truncated", fact.BodyTruncated}
			switch {
			case refusal.Lasts == RefusedWhileNoSession:
				e.warnOnCommit(ctx, "workflow: a review of the review App waits: "+refusal.Reason, args...)
			case e.byReviewApp(fact.Author):
				e.warnOnCommit(ctx, "workflow: a review decides nothing: "+refusal.Reason, args...)
			default:
				e.logOnCommit(ctx, "workflow: a review decides nothing: "+refusal.Reason, args...)
			}
			decides = false
			if refusal.Lasts == RefusedAtThisHead {
				refused = refusal.Reason
			}
		}
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
	// The commit the reviewer's own review read outlives the round, where the decision does not
	// (record.PhaseRow.ReviewedHead): the task of the reviewer's next round names it
	// (namingTheReviewRound). Only the reviewer's own review moves it: a review-App review whose
	// Legion footer names the session the reviewer's claim records (byReviewer). A person's review
	// decides the round and leaves the commit where it stands, since the reviewer never read the
	// commit the person did; while the reviewer's claim records no session, no review-App review
	// gets here, since it waits (RefusedWhileNoSession). And the review must be of the entry that
	// stands (reviewedCommitStays).
	if e.byReviewer(fact, reviewerSession) {
		if why := reviewedCommitStays(fact, reviewer); why != "" {
			e.logOnCommit(ctx, "workflow: the reviewer's own review moves no reviewed commit: "+why,
				"issue", issue.Key, "pull_request", pr.Number, "head", reviewer.Decision.Head, "footer_entry", fact.LegionEntry(), "entry", reviewer.Entry)
		} else {
			reviewer.ReviewedHead = reviewer.Decision.Head
		}
	}
	if err := e.store.PutPhase(ctx, tx, reviewer); err != nil {
		return intake.Result{}, err
	}
	_, err = e.settleRound(ctx, tx, *issue, reviewer, pr, before, by)
	return intake.Result{}, err
}

// reviewedCommitStays is why review, a deciding review of the reviewer's own session (byReviewer)
// that its round has just taken, leaves the commit the reviewer read where it stands
// (record.PhaseRow.ReviewedHead), reviewer being the reviewer's row, and "" where the review moves
// it to the head its footer names. That head is never empty here: the round took the review, so
// RefusesRound gave no reason for it, and it refuses a review-App review whose footer names no
// head (refusesRound).
//
// A review moves the commit only when it is of the entry that stands. The reviewer's row holds the
// task of an entry, which a new generation's clear empties until the next entry is raised
// (record.Store.ClearTreeGeneration, StartPromotedWorker), and the review's footer names that
// entry (intake.PullRequestReview.LegionEntry), the number every task the reviewer is given in
// reviewing ends with (reviewround.EntryLabel). The number is never reset and each entry raises it, so a
// review of an earlier entry - submitted before a re-admission and applied after it, however
// late, or before a carried hand-over's promotion and applied after it - names a number the row
// has left and moves nothing, and the first review of a new generation reads the whole change. No
// time is compared: a review's submission time is GitHub's clock and an entry's is the daemon's,
// and nothing orders the two. A row that the entry upgrade filled (20261111225552_phase_entry) can
// hold no task, and a review of that round moves nothing. Where the commit stays, the cost is a
// next round that reads more: from an older commit, or the whole change. Who decides the round is
// not changed by any of this.
func reviewedCommitStays(review intake.PullRequestReview, reviewer record.PhaseRow) string {
	switch named := review.LegionEntry(); {
	case reviewer.EntryTask == "":
		return "the reviewer's row holds no entry's task"
	case named == 0:
		return "its footer names no entry"
	case named != reviewer.Entry:
		return fmt.Sprintf("its footer names entry %d and the reviewer's row stands at entry %d", named, reviewer.Entry)
	}
	return ""
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
	byMerge       = "the pull request's merge"
)

// roundOutcome is what a review round comes to after a fact (reviewRound).
type roundOutcome int

const (
	// roundOpen is a round still owed its reviewer's completion or its decision, or waiting on the
	// head a push is bringing; or an issue not in reviewing, since one held
	// from reviewing ends only once the retry restores it; or an approval that stands on the head
	// of a pull request closed without merging, which the architect decides (reviewRound).
	roundOpen roundOutcome = iota
	// roundApproved ends the round: its approval approves the head's code (classify.ApprovalStands),
	// which GitHub answers for with a merge state the issue may advance on, or which the pull
	// request has already merged at. No CI verdict is read: the reviewer reads the head's checks on
	// GitHub before it approves, and a settlement moves no issue (dispatch://INFRA-714/spec R1).
	roundApproved
	// roundRejected ends the round: its reviewer requested changes, whether or not it has
	// completed (reviewRound).
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
// runs clears `blocked` - and a read is the only thing that would ever say so. Which round that
// is, is classify.RoundWaitsOnMergeState: the engine and the state build read that one rule, so
// the issue's view carries a mergeWait exactly for the rounds the daemon reads GitHub for.
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
// request, the state in GitHub's own spelling, every check run GitHub reported on that head
// with the conclusion it came to, and every commit status the record keeps for that head with
// its state (headChecks), which is the whole of what the daemon knows of the head's own CI. There
// is always a run to name: the only caller builds this reason for a round it has already reported
// a head for, which it does only once GitHub has reported a check run on that head itself and
// every one of them has concluded (reviewRound, classify.ChecksConcludedOnHead), and that gate
// and headChecks read the same runs of the fence (classify.HeadRuns), so headChecks names at least
// the run the gate found.
//
// It names the head's own check runs rather than everything the settlement standing for the head
// reports, which can be the code head's below it: GitHub weighs the current head for the merge,
// so a name whose newest run ran on another head of the tree says nothing about why this head is
// held, and naming it here would read as a run GitHub made on this one.
//
// The daemon reads the base's rules for its required checks only on a read that finds the head
// conflicting with its base (intake.PullRequestMergeability.Rules, dispatch://INFRA-4374 R0), and
// a head this reason is built for is not one: an approval of a head held in a confirmed conflict
// ends its round, and one whose first conflicting read waits for its second waits (reviewRound).
// So here it does not know which checks the base requires: a required check that has never run is
// in no list GitHub gives for the head, so it is simply absent from the checks named here. The
// reason says so, because a reader who does not know that would take a short list for the daemon
// leaving something out rather than for the finding it is - the check that is missing is the one
// to look for. It says nothing of what clears the state: a push that earns what the base requires
// is one way, and a check the base requires finishing on the head as it stands is another.
func stuckMergeStateReason(pr record.PullRequest) string {
	return fmt.Sprintf("the reviewer approved head %s on pull request #%d, and GitHub reports that head %s rather than one it would merge. %s. The daemon reads the base's rules only on a conflicting read and so names no required check: one the base requires that has never run is in nothing GitHub reports for the head, so it is absent here rather than named as missing, which makes the missing check the thing to look for. The issue waits in reviewing until GitHub reports the head clean, unstable or has_hooks",
		pr.HeadSHA, pr.Number, pr.MergeState, headChecks(pr))
}

// reviewRound is what issue's review round comes to, row being its reviewer's and pr its pull
// request as a fact left them. The review event decides it, and the reviewer's handoff records its
// findings (dispatch://INFRA-714/spec R1): a request for changes ends the round whether or not the
// reviewer has completed, and the completion that follows is recorded on the reviewer's own row
// (engine's leftItsPhase). An approval ends it once it approves the head's code
// (classify.ApprovalStands) and GitHub reports that head one the issue may advance on
// (record.MergeState.Mergeable), or once the pull request has merged at that head: a merged pull
// request has nothing left to merge, GitHub answers `unknown` for it for good, and the daemon
// never reads its state again (askMergeStateFor), so nothing it could wait on would ever come.
// An approval of a head the record holds in a confirmed conflict with its base
// (classify.ConflictRound) ends the round too, whatever GitHub's merge state for it
// (dispatch://INFRA-4374 R0): nothing GitHub answers about that head changes until the base is
// merged forward, and that merge is the merger's once the issue reaches merging, so the move into
// merging asks the merger for it (transition, mergeForwardOnEntry, record.ConflictMergeNext,
// cybermonklabs/legion#4979). A step of the task that follows the review goes on at that head
// first, as the conflict's confirmed read decided (classify.ConflictNeedsMerge). While the first
// conflicting read of the head waits for its second
// (record.PullRequest.ConflictRead), the approval waits where it stands and is never reported
// stuck: GitHub's `dirty` is not yet a conflict the daemon holds, and the second read decides it.
// An approval that stands on the head of a pull request closed without merging keeps the round
// open: the architect has the pr-closed-unmerged notice and decides what comes next, and a
// reopen asks GitHub for the head's merge state again (the opened handler, then askMergeState).
// An approval that stands on an open pull request's head with any other answer waits on GitHub's
// next answer about that head, which is classify.RoundWaitsOnMergeState - the rule the state
// build reads too, so the two never drift. An approval with no merge state on record for its
// head - no read of the head on record, or a read whose answer carried no state
// (record.PullRequest.StateReadAt) - or with `unknown` while GitHub computes it, waits where it
// stands, and the read that lands moves it (mergeability, the ladder behind daemon's
// readMergeState). So does an approval
// whose head still has a check running: a check the base requires holds the head `blocked` while
// it runs, which says nothing about the round. What tells the two apart is the head's own check
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
// approval is of other code than the head's (stuckApprovedOtherCode). An approval of a head that
// code is on its way from keeps the round open, and classify.CodeOnItsWay says when that is.
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
		switch pr.State {
		case record.PullRequestClosed:
			// A closed pull request merges nothing until a reopen, and the architect decides what
			// comes of it (the pr-closed-unmerged notice), so its approval ends no round.
			return round{}
		case record.PullRequestMerged:
			// A merged pull request has nothing left to merge, and nothing the round could wait
			// on ever comes, so its approval ends the round.
			return round{outcome: roundApproved}
		}
		// Only an open pull request is left, and only its round can wait on GitHub's answer. An
		// approval of a head held in a confirmed conflict waits on no answer of GitHub's, since
		// none changes until the base is merged forward (classify.RoundWaitsOnMergeState does
		// not hold for it), so it ends the round here, whatever the merge state.
		if !classify.RoundWaitsOnMergeState(issue, row, pr) {
			return round{outcome: roundApproved}
		}
		// The approval stands on the head of an open pull request and GitHub's answer for that
		// head is not one the issue may advance on, so the round waits on GitHub's answer for it:
		// the unread state and `unknown` because GitHub has given none yet, every other because
		// the one it gave can change under the head, a head GitHub has reported no concluded
		// check run on because a check still running there is what the state is waiting for,
		// and a head whose first conflicting read waits for its second because GitHub's `dirty`
		// is no conflict until that read confirms it (mergeability), which then ends the round,
		// or releases it. The daemon reads for each (settleRound), and the settlement that ends
		// the last check is itself a fact that asks for a read. Which approval waits is the rule
		// the state build reads too (classify.RoundWaitsOnMergeState), so the view carries a
		// mergeWait for exactly this round.
		firstReadWaits := pr.ConflictRead != nil && pr.ConflictRead.Head == pr.HeadSHA
		if pr.MergeState == record.MergeStateUnread || pr.MergeState == record.MergeStateUnknown || !classify.ChecksConcludedOnHead(*pr) || firstReadWaits {
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

// implementationReady moves issue from implementing to its task's next step (testing, in the
// default steps) on the implementer's completion of
// the round that row records, once the pull request pr is recorded open or merged
// (TriggerImplementationReady: a record closed without merging, or none, moves nothing). It
// reads no CI result: a settlement that arrived while the implementer worked moved nothing and
// moves nothing now, since the roles were told what it said when it arrived (checks,
// dispatch://INFRA-714/spec R1).
func (e *Engine) implementationReady(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, pr *record.PullRequest) error {
	return e.transition(ctx, tx, issue, TriggerImplementationReady, "", row, pr, moveWhy{})
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
// An approval of the commit the running review step started at beside a fix of its task
// (ReviewBesideFix) ends the round as approved where it does not stand at the open pull request's
// head, which the fix's push moved or is moving: the review step that started at X ends with the
// review of X, and what runs after the fix is the fix's way (besideFix). An approval of the head
// itself is weighed as every approval is.
//
// A round GitHub will not merge from (stuckMergeState) is the one cause the reviewer cannot
// decide, and the whole of its path - its own dedup, its own message to the reviewer, a notice
// with no topic - is tellStuckMergeState's, which this asks for once.
func (e *Engine) settleRound(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, pr *record.PullRequest, before round, by string) (bool, error) {
	r := e.reviewRound(issue, row, pr)
	if issue.Phase == phase.Reviewing && pr != nil && pr.State == record.PullRequestOpen && row.Decision != nil &&
		row.Decision.State == "approved" && !classify.ApprovalStands(*pr, row.Decision.Head) {
		besideFix, err := e.reviewBesideFix(ctx, tx, issue)
		if err != nil {
			return false, err
		}
		if besideFix != "" && row.Decision.Head == besideFix {
			r = round{outcome: roundApproved}
		}
	}
	switch r.outcome {
	case roundRejected:
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return false, err
		}
		return true, e.transition(ctx, tx, issue, TriggerReviewRejected, "", row, pr, moveWhy{Reason: row.Decision.Body})
	case roundApproved:
		return true, e.transition(ctx, tx, issue, TriggerReviewApproved, "", row, pr, moveWhy{})
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
// when the fact owes that read (askMergeStateFor, Config.ReadMergeState): the round the fact has
// just been applied to leaves waiting on GitHub's answer for the head (round.waitsOnMergeState),
// or the fact moves the head of an issue in implementing through merging, which a conflict with
// the base needs read (the gate's reason, classify.MergeStateReadOwed, dispatch://INFRA-4374 R0).
// It is the engine's own account of what the round is owed rather than a second reading of
// the record beside it, and it runs on the commit, so the fact's transaction waits for no GitHub
// call.
//
// Every fact that can change GitHub's answer asks, and the list is this function's switch rather
// than a sentence in a comment and nine calls spread through the handlers: the pull request's own
// events, a push to it, a review, a CI settlement, and a push to the base it sits on. The switch
// also names which of them move the head (classify.MergeStateFactHeadMove): the pull request
// opened or reopened, and a synchronize, which is the fact that moves the head a push to the
// branch carries (its case says why the push is not one). Apply calls it once, after the handler,
// so a handler's own early return cannot drop the read and no handler recomputes the round
// settleRound has just computed (dispatch://INFRA-714 R3). GitHub's own answer does not ask - the
// fact a read applies is mergeability, and a read that asked for another would read the same head
// for as long as GitHub kept answering the same way. What a read answers is the ladder's to decide
// (daemon's mergeStateLadder), and the next fact of the issue asks again.
//
// A base push is the one fact of many pull requests, and the one this leaves alone: its handler
// walks the open pull requests recorded on the base once, and asks for each read inside that
// walk (basePush), since the record it overwrites as it goes is what tells a redelivered push
// from a first delivery.
func (e *Engine) askMergeState(ctx context.Context, tx pgx.Tx, fact intake.Fact) error {
	if e.cfg.ReadMergeState == nil {
		return nil
	}
	var pr *record.PullRequest
	var err error
	kind := classify.MergeStateFactOther
	switch fact := fact.(type) {
	case intake.PullRequestOpened:
		pr, err = e.pullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
		kind = classify.MergeStateFactHeadMove
	case intake.PullRequestSynchronized:
		pr, err = e.pullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
		kind = classify.MergeStateFactHeadMove
	case intake.Push:
		// The push moves no head of the record: its synchronize, from GitHub or from the per-issue
		// read (daemon's followHead), is the fact that moves the head, and the one a head move is
		// read for. So the push asks only as a review or a settlement does, for a round it leaves
		// waiting on GitHub, and one push to a branch asks for no ladder beside its synchronize's.
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
	return e.askMergeStateFor(ctx, tx, pr, kind)
}

// askMergeStateFor queues the read for one pull request when a fact of kind fact asks for it, by
// one of the gate's two reasons (classify.MergeStateReadOwed, on the walk's line: a head move of
// an issue in implementing or a phase after it, a base push of an issue in a phase after it) or by
// the
// third, which the gate does not hold: the issue's review round waiting on GitHub's answer for the
// head (reviewRound's waitsOnMergeState), worked out here only for a fact the gate owes no read
// for. The guard on a daemon with no reader is here as well as in askMergeState, because a base
// push reaches this from its handler's walk rather than through that switch (basePush), and that
// walk is the one caller that names classify.MergeStateFactBasePush. A pull request that is not
// open is read for nothing: it merges nothing until a reopen, which asks again, and its round
// waits on no answer (classify.RoundWaitsOnMergeState holds only for an open one), so the test of
// its state here saves the store reads below.
func (e *Engine) askMergeStateFor(ctx context.Context, tx pgx.Tx, pr *record.PullRequest, fact classify.MergeStateFact) error {
	if e.cfg.ReadMergeState == nil || pr.State != record.PullRequestOpen {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return err
	}
	w, err := e.walk(ctx, tx, *issue)
	if err != nil {
		return err
	}
	owed := classify.MergeStateReadOwed(*issue, w.Line, fact)
	if !owed {
		// The round's own answer matters only where the fact and the phase owe no read: every
		// phase a round can wait on GitHub in already owes one on a head move or a base push.
		reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
		if err != nil {
			return err
		}
		owed = e.reviewRound(*issue, reviewer, pr).waitsOnMergeState
	}
	if !owed {
		return nil
	}
	key := issue.Key
	store.OnCommit(ctx, func() { e.cfg.ReadMergeState(key) })
	return nil
}

// recordPass counts task one more pass through its steps from the one a move back goes to, which
// every move back to an earlier step of the task opens (Walk.MovesBack), counts the move itself
// under its own pair,
// and fires the deployment's hooks that stand on the move (fireWorkBackHooks). The counts are the
// task's (record.Store.ReturnCounts): on its plan_tasks row for an issue with plan rows, and on the
// implementer's phases row for an issue with none, where they lived before plans.
//
// The pass bounds nothing: it is the counter the handoff completion key carries
// (api/handoff.go), and its whole job is to make a completion at a commit an earlier pass
// reported a fact of its own. BackwardMoves is what a hook counts, under the move's own
// "<from>-<to>" pair, spelled as phase.MovePairs() spells the pairs (phase.Pair): a review
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
func (e *Engine) recordPass(ctx context.Context, tx pgx.Tx, issue record.Issue, task string, from, to phase.Phase, pr *record.PullRequest) error {
	counts, err := e.store.ReturnCounts(ctx, tx, issue.Key, issue.Generation, task)
	if err != nil {
		return err
	}
	counts.Pass++
	if pr != nil && classify.ConflictRound(*pr) {
		return e.store.PutReturnCounts(ctx, tx, issue.Key, issue.Generation, task, counts)
	}
	if counts.BackwardMoves == nil {
		counts.BackwardMoves = make(map[string]int, 1)
	}
	counts.BackwardMoves[phase.Pair(from, to)]++
	if err := e.store.PutReturnCounts(ctx, tx, issue.Key, issue.Generation, task, counts); err != nil {
		return err
	}
	return e.fireWorkBackHooks(ctx, tx, issue.Key, from, to, counts.BackwardMoves, pr)
}

// recordRound counts task one more review round, which only a review that requested changes
// ends, and tells the architect once for each value review_round_cap holds. The counts are the
// task's, where recordPass keeps them. The move's own pair and the deployment's hooks are counted
// where every backward move passes (recordPass), so a round and a move are counted apart: this
// one is the cap's, and everything it writes it writes in the move's own transaction.
//
// What the cap reads is Rounds less the rounds a conflict with the base caused
// (classify.ConflictRound, record.ReturnCounts.ConflictRounds). A round opened while the record
// holds the pull request's own head as conflicting is answered by the implementer's forward
// merge, whatever else was asked of it, so it costs the issue nothing - which is what
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
// record.ReturnCounts.CapTold, and a later round compares the two. The comparison is != and never
// <, because a cap lowered under a row that already told a higher value must tell once at the new
// value, which < would never do. A raised cap tells again at its new number, and a new
// generation tells again from its own first crossing, because clearGeneration clears the mark.
//
// Nothing here holds the move back: the transition starts the next role whatever the counts read. A write that fails is still an error of the move's transaction, as
// every other write of that transaction is, and is returned rather than swallowed.
func (e *Engine) recordRound(ctx context.Context, tx pgx.Tx, issue record.Issue, task string, pr *record.PullRequest) error {
	counts, err := e.store.ReturnCounts(ctx, tx, issue.Key, issue.Generation, task)
	if err != nil {
		return err
	}
	counts.Rounds++
	if pr != nil && classify.ConflictRound(*pr) {
		// A conflict round ends here: it tells nobody, so the write of the two counts is all it
		// has to do.
		counts.ConflictRounds++
		return e.store.PutReturnCounts(ctx, tx, issue.Key, issue.Generation, task, counts)
	}
	counted := counts.Rounds - counts.ConflictRounds
	tell := counted >= e.cfg.ReviewRoundCap && counts.CapTold != e.cfg.ReviewRoundCap
	if tell {
		counts.CapTold = e.cfg.ReviewRoundCap
	}
	if err := e.store.PutReturnCounts(ctx, tx, issue.Key, issue.Generation, task, counts); err != nil {
		return err
	}
	if tell {
		message := fmt.Sprintf("Issue reached review_round_cap=%d: %d rounds counted, %d conflict rounds not counted.",
			e.cfg.ReviewRoundCap, counted, counts.ConflictRounds)
		if err := e.enqueue(ctx, tx, issue.Key, record.MessagePost{Body: message}); err != nil {
			return err
		}
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: message}); err != nil {
			return err
		}
	}
	return nil
}
