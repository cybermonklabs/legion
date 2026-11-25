package workflow

import (
	"cmp"
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A head that moves past review (spec R4, R5; Design, "A head that moves in review"). A check run
// belongs to one commit, so a head a push leaves has no legion-verdict until the review steps'
// verdicts name it, and a testing step passes at that head only by the check states Dispatch
// publishes there. Past review, every move of the head asks the agents of the review steps for
// their verdicts at the new head (askVerdict, and mergeForwardLanded for a merge forward's
// landing).

// reviewVerdict applies the verdict past review (intake.ReviewVerdict, which the review-verdict
// route queues): the approve or changes, at the head the pull request moved to once a review step
// of the issue's task has passed, of the agent of a review step of that task that has passed, by
// the walk (reviewStep.passed), as the step's record names it (reviewSteps): the reviewer, or the
// architect that ran the owner's acceptance. It records that step's verdict at that head
// (recordVerdict), whose write of legion-verdict the fact's commit asks for (askMergeState), and
// the reviewer's is also the round's decision there, on the reviewer's row (decisionOf), which the
// next ask of the reviewer counts its range from (verdictSince). It writes no handoff, makes no
// completion and moves no phase.
//
// Every judgement of the call is made here, under the fact's lock, each refusal recording nothing:
// a claim the record names the agent of no review step of the issue's task
// (VERDICT_CALLER_FORBIDDEN), a caller whose own step has not passed (VERDICT_NOT_PAST_REVIEW), a
// lingering tree (TREE_LINGERING), and a head the pull request has moved off
// (VERDICT_HEAD_NOT_CURRENT). The step the issue stands in has not passed, so its agent's verdict
// is refused here too: the reviewer's in reviewing is its completion's, and the owner's in
// accepting is its sign_off or its move back out of accepting.
func (e *Engine) reviewVerdict(ctx context.Context, tx pgx.Tx, fact intake.ReviewVerdict) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	w, err := e.walk(ctx, tx, *issue)
	if err != nil {
		return intake.Result{}, err
	}
	steps, err := reviewSteps(ctx, e.store, tx, *issue, w)
	if err != nil {
		return intake.Result{}, err
	}
	step, ok := stepOfAgent(steps, fact.Claim)
	if !ok {
		return intake.Result{Refusal: &intake.Refusal{Status: 403, Code: "VERDICT_CALLER_FORBIDDEN", Message: fmt.Sprintf(
			"review_verdict is the verdict of the agent of a review step of %s's task, as its step record names it, and %s; this call is claim %s's. An architect that ran a child's acceptance names that child as issue; nothing was recorded",
			issue.Key, agentsOf(steps), fact.Claim)}}, nil
	}
	if !step.passed(w, *issue) {
		return refused("VERDICT_NOT_PAST_REVIEW", fmt.Sprintf(
			"%s stands in %s, and step %d, this claim's, has not passed: review_verdict is the verdict of a review step's agent on a head that moved after that step passed, and the step that runs now takes its verdict by its own call, the reviewer's completion or the owner's sign_off or request_backward_move; nothing was recorded",
			issue.Key, record.StandsAt(*issue), step.place.Place)), nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return refusedLingering(*issue, "the verdict past review"), err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	if pr == nil || pr.HeadSHA != fact.Commit {
		current := "the daemon records no pull request for " + issue.Key
		if pr != nil {
			current = fmt.Sprintf("the head of %s's pull request #%d is %s", issue.Key, pr.Number, pr.HeadSHA)
		}
		return refused("VERDICT_HEAD_NOT_CURRENT", fmt.Sprintf(
			"this verdict is on %s, and %s: read that head and send the verdict on it; nothing was recorded", fact.Commit, current)), nil
	}
	decision := decisionOf(intake.HandoffComplete{Verdict: fact.Verdict, Summary: fact.Summary, Commit: fact.Commit})
	if decision == nil {
		// The route takes approve and changes alone (api's reviewerVerdictOf).
		return intake.Result{}, nil
	}
	if step.at == phase.Reviewing {
		reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
		if err != nil {
			return intake.Result{}, err
		}
		reviewer.Decision = decision
		if err := e.store.PutPhase(ctx, tx, reviewer); err != nil {
			return intake.Result{}, err
		}
	}
	if err := e.recordVerdict(ctx, tx, issue.Key, pr, step.place, fact.Claim, fact.Verdict, fact.Commit); err != nil {
		return intake.Result{}, err
	}
	// A step's verdict is the record changing, so the issue moved (the rule in dispatchIssue), as
	// at the completion that wrote the round's decision before it (handoff).
	if err := e.store.MarkIssueMoved(ctx, tx, issue.Key, e.now()); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: recorded a review step's verdict past review", "issue", issue.Key, "tree", issue.Tree,
		"phase", record.StandsAt(*issue), "step", step.place.Place, "agent", fact.Claim, "verdict", fact.Verdict, "commit", fact.Commit)
	return intake.Result{}, nil
}

// askVerdict asks the agent of each review step of issue's task that has passed for its verdict at
// pr's head, which a push moved there from from while issue stands past its review (spec R5, "The
// verdict past review"): every push once a review step has passed, a retro commit and an
// implementer's correction among them, leaves a head with no legion-verdict, which cannot merge
// until the review steps' verdicts there are written. Past review is a review step of the task that
// has passed, by the walk (askReviewSteps asks each such step and no other): on the default steps
// that is accepting, retro or merging, and on a task whose review step comes before its tester it
// is testing too. A merge forward's landing asks in
// mergeForwardLanded, so this is every other move (askReviewSteps). A tree that lingers, where
// every role is suspended, asks nothing.
func (e *Engine) askVerdict(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, from string) error {
	if from == "" || from == pr.HeadSHA {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	_, err := e.askReviewSteps(ctx, tx, issue, pr, from, cmp.Or(pr.Base, issue.DefaultBranch), "")
	return err
}

// askReviewSteps asks the agent of each review step of issue's task that has passed (passed) for
// its verdict at pr's head, which the push from from moved there, the landing of a merge forward
// of base where why names its reason, and says whether it asked any. Each ask names the range that
// agent has not judged: from the head of its step's last verdict (verdictSince for the reviewer,
// whose row's decision its verdicts write; the store's last verdict of the step for every other),
// so a second push before it answers still asks about every commit since that verdict; where none
// is recorded, from from. A push back to the head of that verdict asks nothing of that agent; a
// landing asks all the same, since its merge is what the agent reads.
//
// The reviewer is asked by the merge-forward review (record.MergeForwardReview) on its role's
// topic, which wakes a released reviewer: a new outbox kind or payload key would be one the release
// before it cannot read (spec R15). Its executor delivers it while the issue stands past its review
// at that head (leftMergeForwardAt). The agent of every other review step, the architect that ran
// the owner's acceptance, is asked by a role message to the claim its step's record names
// (record.RoleMessage, stepVerdictAsk), the kind every release reads. The daemon's executor
// delivers it held while the ask stands (VerdictAskStands): a released claim is woken for it, and
// the row waits until a session holds the claim's role, since with the default steps legion-verdict
// at the new head waits on that agent's verdict and nothing else tells it of the push. A step whose
// record names no claim is asked nothing.
func (e *Engine) askReviewSteps(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, from, base string, why record.ConflictWhy) (bool, error) {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return false, err
	}
	steps, err := reviewSteps(ctx, e.store, tx, issue, w)
	if err != nil {
		return false, err
	}
	at, asked := record.StandsAt(issue), false
	for _, step := range steps {
		if !step.passed(w, issue) || step.at != phase.Reviewing && step.agent == "" {
			continue
		}
		since, err := e.stepVerdictSince(ctx, tx, issue, pr, step, from)
		if err != nil {
			return false, err
		}
		if why != "" {
			since = cmp.Or(since, from)
		}
		if since == "" {
			continue
		}
		if step.at == phase.Reviewing {
			addressed, err := e.addressRoles(issue.Project, []roleRecipient{{issue: issue.Key, role: claim.RoleReviewer, row: issue.Key}}, nil)
			if err != nil {
				return false, err
			}
			for _, to := range addressed {
				if err := e.enqueue(ctx, tx, to.issue, record.MergeForwardMessage{Role: claim.RoleReviewer, Step: record.MergeForwardReview,
					Head: since, Landed: pr.HeadSHA, Base: base, Phase: at, PullRequest: pr.Number, Why: why}); err != nil {
					return false, err
				}
			}
		} else if err := e.enqueue(ctx, tx, issue.Key, record.RoleMessage{Role: string(step.agent),
			Body: stepVerdictAsk(issue.Key, pr, at, since, base, why)}); err != nil {
			return false, err
		}
		asked = true
		e.logOnCommit(ctx, "workflow: the head moved past review; a review step's agent is asked for its verdict", "issue", issue.Key,
			"tree", issue.Tree, "phase", at, "step", step.place.Place, "agent", step.agent, "pullRequest", pr.Number, "head", since, "landed", pr.HeadSHA)
	}
	return asked, nil
}

// VerdictAskStands reads in tx whether agent is still owed its ask of a verdict past review on
// issue key (askReviewSteps): the issue is in the workflow, its tree does not linger, the pull
// request the record holds for it is open, and agent is the agent the record names of a review step
// of the issue's task that has passed (past review, by the walk),
// the reviewer's aside, whose last verdict on the pull request names no head or another head than
// the pull request's current one (storedVerdictSince). It also returns the issue the record holds,
// nil where it holds none. The reviewer is left out because its ask is the merge-forward review,
// held by a rule of its own (leftMergeForwardAt).
//
// The daemon's role-message executor is its one caller: a role message to such an agent is
// delivered held, waking a released claim, where every other role message to a released claim ends
// undelivered (internal/daemon's outbox.roleMessage). The executor reads it at each attempt, just
// before it delivers, so the row is held only while the agent owes that verdict: its verdict at the
// current head, the issue's move out of the phases past review or out of the workflow, and the pull
// request's close each end the hold. Every role message of the issue to that agent is held while
// the ask stands, the ask and a CI settlement's message alike, since the claim is woken for the ask
// and reads both. It reads records alone, through the readers an engine reads with (readWalk,
// reviewSteps), and builds no engine.
func VerdictAskStands(ctx context.Context, records record.Store, tx pgx.Tx, key string, agent claim.Token) (*record.Issue, bool, error) {
	issue, err := records.Issue(ctx, tx, key)
	if err != nil || issue == nil || agent == "" || !record.InWorkflow(*issue) {
		return issue, false, err
	}
	if lingers, err := record.TreeLingers(ctx, records, tx, issue.Tree); err != nil || lingers {
		return issue, false, err
	}
	pr, err := records.PullRequest(ctx, tx, key)
	if err != nil || pr == nil || pr.State != record.PullRequestOpen {
		return issue, false, err
	}
	w, err := readWalk(ctx, records, tx, *issue)
	if err != nil {
		return issue, false, err
	}
	steps, err := reviewSteps(ctx, records, tx, *issue, w)
	if err != nil {
		return issue, false, err
	}
	for _, step := range steps {
		if step.at == phase.Reviewing || step.agent != agent || !step.passed(w, *issue) {
			continue
		}
		since, err := storedVerdictSince(ctx, tx, *issue, pr, step, pr.HeadSHA)
		return issue, since != "", err
	}
	return issue, false, nil
}

// stepVerdictSince is the head of step's last verdict on pr, "" where it names pr's head, so no
// verdict of that step is owed there, and otherwise where none is recorded: the reviewer's row's
// decision for the reviewer's step (verdictSince), and the store's last verdict of the step for
// every other (storedVerdictSince).
func (e *Engine) stepVerdictSince(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, step reviewStep, otherwise string) (string, error) {
	if step.at == phase.Reviewing {
		reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
		return verdictSince(reviewer, pr.HeadSHA, otherwise), err
	}
	return storedVerdictSince(ctx, tx, issue, pr, step, otherwise)
}

// storedVerdictSince is the head of the last verdict of step, a review step other than the
// reviewer's, that the store records on pr (store.LastStepVerdictTx), "" where it names pr's head,
// and otherwise where none of issue's is recorded.
func storedVerdictSince(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, step reviewStep, otherwise string) (string, error) {
	repository, err := ghrepo.Parse("the repository of "+issue.Key+"'s pull request", pr.Repo)
	if err != nil {
		return "", err
	}
	given, found, err := store.LastStepVerdictTx(ctx, tx, repository, pr.Number, step.place.Task, step.place.Place)
	switch {
	case err != nil || !found || given.Issue != issue.Key:
		return otherwise, err
	case given.Head == pr.HeadSHA:
		return "", nil
	}
	return given.Head, nil
}

// stepVerdictAsk is the role message that asks the agent of a review step other than the
// reviewer's for its verdict at pr's head (askReviewSteps): issue, what moved the head (the merge of
// base forward where why names a reason, any other push where it names none), the phase the issue
// stands in at, both heads, how to read what changed since the old head, since, and the call that
// answers: review_verdict with its verdict, the new head as its commit and the issue it is for,
// since the agent's own grant is of its own issue. The sentences are short and plain, one
// instruction to a sentence, because an agent reads them (ASD-STE100).
func stepVerdictAsk(issue string, pr *record.PullRequest, at phase.Phase, since, base string, why record.ConflictWhy) string {
	pull := fmt.Sprintf("pull request %s#%d", pr.Repo, pr.Number)
	moved, read := "a push moved the head of "+pull, "Read what changed since the old head"
	if why != "" {
		moved, read = fmt.Sprintf("the merge of %s forward landed on %s", base, pull), "Read what the merge changed since the old head"
	}
	return fmt.Sprintf("%s: %s, and the issue stands in %s. The old head is %s, and the new head is %s. %s: run `git diff %s %s`."+
		" Then send your verdict on the new head with review_verdict: verdict approve or changes, commit %s, issue %s."+
		" Write no handoff for this verdict, and make no sign_off and no request_backward_move for it.",
		issue, moved, at, since, pr.HeadSHA, read, since, pr.HeadSHA, pr.HeadSHA, issue)
}

// verdictSince is the head the round's recorded reviewer last gave its verdict on (its row's
// decision, which its completion of reviewing and each verdict past review write), and "" where
// that verdict names head itself, so no verdict is owed there. A row that holds no decision - a
// round a release before the reviewer's verdict decided - answers otherwise, the head the caller
// knows the reviewer was last asked about.
func verdictSince(reviewer record.PhaseRow, head, otherwise string) string {
	if decided := reviewer.Decision; decided != nil && decided.Head != "" {
		if decided.Head == head {
			return ""
		}
		return decided.Head
	}
	return otherwise
}

// owedVerdict is the head of the round's recorded reviewer's last verdict where the head of
// issue's open pull request pr is another one, so the reviewer's verdict at pr's head is owed
// before it merges (verdictSince), and "" where none is owed. Where the reviewer's row holds no
// decision, a landed merge forward's ask of the reviewer stands in (mergeForwardReviewOwed). The
// merger's task reads it (task).
func (e *Engine) owedVerdict(ctx context.Context, tx pgx.Tx, issue string, pr *record.PullRequest) (string, error) {
	if pr == nil || pr.State != record.PullRequestOpen {
		return "", nil
	}
	reviewer, err := e.phaseRow(ctx, tx, issue, claim.RoleReviewer)
	if err != nil {
		return "", err
	}
	otherwise := ""
	if mergeForwardReviewOwed(*pr) {
		otherwise = pr.MergeForward.Head
	}
	return verdictSince(reviewer, pr.HeadSHA, otherwise), nil
}
