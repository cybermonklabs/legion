package prompts

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A step of the type review is the reviewer's, or the owner's acceptance at `acceptance:owner`,
// and the daemon takes each step's verdict only from the claim its step record names (workflow's
// reviewStepRefusal and reviewVerdict). Each agent sends its verdict at the head it read: the
// reviewer `approve` or `changes` with its completion's `commit`, and past review with
// `review_verdict` and no `issue`; the owner with `sign_off`, its `approve`, or a move back out of
// `accepting`, its `changes`, at the head the record holds for the pull request
// (recordAcceptance), and past review with `review_verdict` naming `issue`. No completion names a
// tested commit, and an `approve` carries no tester's pass to a later head: the states Dispatch
// publishes at each head decide the testing step there, and the tester's completion carries no
// verdict. legion-verdict is success only where each commit of the change has a reader, so each
// review step's agent is told what a reader is and that it reads no commit of its own, and the
// merger names both review steps and the readers where it says what the check is written from.
// Each composed prompt that carries a rule
// holds it in one clause, and no composed prompt still teaches the carry, the reviewer as the one
// review step, or the check written from the tester's verdict. This shows the text is loaded,
// never that a role follows it.
func TestEachReviewStepSendsItsVerdictAtTheHeadItRead(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The removed field's own name is left to C3's grep over these trees, so this file names it nowhere.
	carry := []string{"tester's pass", "handoffCommit", "back to `testing` by itself",
		"sends the issue back to testing", "the reviewer whose completion ended the round"}
	owner := [][]string{
		{"`sign_off` (`issue`, and `commit`, the head you read)"},
		{"your `sign_off` with `commit` set to that head is your `approve` there",
			"your `request_backward_move` out of `accepting` with `commit` set to it is your `changes` there",
			"read that head (`pullRequest.head` in `read_record`) and its evidence before you decide"},
		{"The daemon refuses a `commit` that is not the pull request's current head (`VERDICT_HEAD_NOT_CURRENT`)"},
		{"send `review_verdict` with `issue` the issue you accepted", "`commit` the new head"},
		{"your acceptance reads no commit that a session of your claim made", "a commit you push to the issue's branch is one"},
		{"`sign_off` is then your `approve` at the new head"},
		{"each check reads yes there, whatever steps the plan names"},
		{"a merge state the round does not end at", "`behind` its base, `draft`, or `dirty` before a second read confirms it"},
		{"and at `blocked` once every check run GitHub reported on that head has concluded"},
		{"`legion-verdict: failure (a commit with no reader)`", "no worker's change cures it", "only a person merges that head, by a break-glass merge",
			"a GitHub owner turns the ruleset's rule off for that one merge"},
	}
	ownerRetired := append([]string{
		"the daemon also asks the reviewer for its verdict on the new head",
		"that it sent `approve` on the new head and `legion-verdict` at that head reads `success`",
		"which the daemon writes from the reviewer's verdict",
		"no push earns it",
		"a `blocked` one the required check run",
		"your `sign_off` is your `approve` there",
		"makes your acceptance no independent review",
		"each check reads yes there where the task has a tester step",
		"(no independent review)",
	}, carry...)
	for _, tc := range []struct {
		name    string
		role    claim.Role
		isRoot  bool
		holds   [][]string
		retired []string
	}{
		{"reviewer", claim.RoleReviewer, false, [][]string{
			{"It takes your step's verdict only from the claim your step's record names as its agent", "(`VERDICT_CALLER_FORBIDDEN`)"},
			{"Send it with `op: \"review_verdict\"`", "`commit` that head", "leave `issue` out"},
			{"The daemon takes it only from the agent of a review step of the issue's task", "you for your step"},
			{"only once your step has passed, never for the step that runs now (`VERDICT_NOT_PAST_REVIEW`)"},
			{"each commit of the change has a reader", "an agent of a review step whose verdict at that head is `approve`, or of a testing step that passed there, whose claim records a session and none of whose sessions made that commit"},
			{"A commit's makers are the sessions its `Omp-Session` lines name", "a commit with no such line has no agent maker"},
			{"A pass or a verdict at an earlier commit counts at no later head", "your `approve` carries no pass of the testing step"},
			{"past review, the old head the daemon's message names", "the head your last verdict named"},
			{"each check reads yes there, whatever steps the plan names"},
			{"or at `blocked` once every check run GitHub reported on that head has concluded"},
			{"The round still waits while that state is unread or `unknown`, and while a check run of that head runs"},
			{"a push past review, such as a merge forward the daemon asks for, makes a new head"},
			{"each push past review asks the agent of each review step that has passed for a verdict on the new head"},
		}, append([]string{"from your verdict there", "a head your `approve` names", "the reviewer is asked as well",
			"the head the push left", "No other phase completion carries a `verdict` (`VERDICT_NOT_TAKEN`)",
			"leaves the issue in `reviewing` until GitHub's own merge state for it comes clean",
			"nothing of this tree pushes above it", "at least one of those review steps is independent", "A claim wrote part of the change",
			"each check reads yes where the task has a tester step", "counts as no independent review",
			"only while the issue stands past its review"}, carry...)},
		{"root architect", claim.RoleArchitect, true, owner, ownerRetired},
		{"sub-architect", claim.RoleArchitect, false, owner, ownerRetired},
		{"tester", claim.RoleTester, false, [][]string{
			{"You post no check run, and your completion carries no verdict", "the daemon writes `legion-verdict` from the states Dispatch publishes of the issue's checks at the head"},
		}, append([]string{"from your verdict and the reviewer's"}, carry...)},
		{"merger", claim.RoleMerger, false, [][]string{
			{"The daemon writes the required check `legion-verdict` on each head from the verdicts there of the task's review steps, the reviewer's and the owner's acceptance",
				"the states Dispatch publishes of the issue's checks there, whatever steps the plan names", "a reader of each commit of the change"},
			{"each push past review asks the agent of each review step that has passed for a verdict on the new head"},
			{"Where a review step of your task has passed, the daemon also asks its agent for its verdict on that head"},
			{"If the base's rules require an approving review, no step of this task gives one"},
		}, append([]string{"from the reviewer's and the tester's verdicts", "a head the reviewer's `approve` names",
			"Where your task has a reviewer step, the daemon also asks the reviewer", "the task's steps have no review step",
			"where the task has a tester step, the states Dispatch publishes", "no independent review"}, carry...)},
	} {
		text := foldWhitespace(composedText(t, composer, tc.role, tc.isRoot))
		for _, terms := range tc.holds {
			if !clauseHolds(text, terms...) {
				t.Errorf("no clause of the composed %s prompt holds %q, in that order", tc.name, terms)
			}
		}
		for _, phrase := range tc.retired {
			if strings.Contains(text, phrase) {
				t.Errorf("the composed %s prompt still says %q", tc.name, phrase)
			}
		}
	}
}

// When each check reads yes and every testing and review step of the task passed at a head, and a
// commit of the change has no reader there, or the daemon could not read every commit of the change,
// the daemon writes legion-verdict `failure` there with its own title (spec R4), and the run's
// summary names each commit with no reader. No worker's change cures that, so the merger neither
// merges nor sends the issue back: it tells the owner of the task once for that head, naming each
// commit with no reader and each check with no counted pass, and records that message in a
// `dispatch comment` that keeps it from sending a second. The message asks no person for an
// approval, since only a person merges that head, by a break-glass merge. The composed merger
// prompt keys that case on the run's title, gives the read that prints it, and says each of those
// acts. This shows the text is loaded, never that a merger follows it.
func TestTheMergerTellsTheOwnerOfACommitWithNoReaderOncePerHead(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := foldWhitespace(composedText(t, composer, claim.RoleMerger, false))
	const title = "`legion-verdict: failure (a commit with no reader)`"
	for _, terms := range [][]string{
		{"`output.title` reads " + title, "is a head no worker can fix"},
		{"a commit of the change has no reader there", "or the daemon could not read every commit of the change"},
		{"Do not merge, do not enqueue, do not send the issue back, and do not complete"},
		{"Tell the owner of the task, your architect, once for that head", "where one already names that head", "send nothing more"},
		{"with `envoy_publish` to its encoded role token", "name the pull request, the head, each commit the summary names",
			"each check of the task with no counted pass at that head"},
		{"record that message in a `dispatch comment` on the issue that names the head"},
		{"The message asks no person for an approval", "only a person merges that head, by a break-glass merge",
			"a GitHub owner turns the ruleset's rule off for that one merge"},
		{"unless the run's title names a commit with no reader"},
	} {
		if !clauseHolds(text, terms...) {
			t.Errorf("no clause of the composed merger prompt holds %q, in that order", terms)
		}
	}
	for _, phrase := range []string{"Only a person can merge that head", "(no independent review)", "names no independent review"} {
		if strings.Contains(text, phrase) {
			t.Errorf("the composed merger prompt still says %q", phrase)
		}
	}
	const read = `gh api 'repos/<o>/<r>/commits/<head>/check-runs?check_name=legion-verdict' --jq '[.check_runs[] | select(.app.slug == "<slug>") | {conclusion, title: .output.title, summary: .output.summary}]'`
	if !strings.Contains(text, read) {
		t.Errorf("the composed merger prompt omits the read of the run's title and summary, %q", read)
	}
}
