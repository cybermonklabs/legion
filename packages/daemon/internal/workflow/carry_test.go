package workflow

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// testerPassAt is LEGION-208's tester row as its pass at commit leaves it.
func testerPassAt(commit string) record.PhaseRow {
	return record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", HandoffCommit: commit,
		HandoffRecord: "test-1", HandoffVersion: 1, Summary: "tested"}
}

// reviewedRow is LEGION-208's reviewer row as the reviewer's approve of headOne, carrying nothing,
// left it when it ended the round under reviewClaim.
func reviewedRow() record.PhaseRow {
	return record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: reviewClaim, HandoffCommit: headOne,
		HandoffRecord: "review-1", HandoffVersion: 1, Summary: "reviewed", CompletedAt: time.Date(2026, 9, 22, 23, 0, 0, 0, time.UTC),
		Decision: &record.ReviewDecision{State: "approved", Body: "reviewed", Head: headOne}}
}

// seedPastReview seeds LEGION-208 standing in at past its review: the tester passed headOne, the
// reviewer's approve of headOne ended the round, and a push since moved pull request #42 to
// headTwo. The step records name each review step's agent: the reviewer's step, passed, under
// reviewClaim, and the owner's acceptance under architectClaim, passed past accepting with its
// sign-off's approve of headOne recorded, and running in accepting.
func seedPastReview(t *testing.T, at phase.Phase, status string) *pgxpool.Pool {
	t.Helper()
	pool := seedPastReviewUnrecorded(t, at, status)
	seedStepAgent(t, pool, reviewerStep, reviewClaim, true)
	accepted := at != phase.Accepting
	seedStepAgent(t, pool, acceptanceStep, architectClaim, accepted)
	if accepted {
		seedRecord(t, pool, func(tx pgx.Tx) error {
			_, err := store.RecordVerdictTx(t.Context(), tx, store.Verdict{Repository: ghrepo.MustParse("sjawhar/legion"), Number: 42, Head: headOne,
				Issue: "LEGION-208", Task: defaultTask, Step: acceptanceStep, Verdict: store.VerdictApprove, Session: string(architectClaim),
				RecordedAt: time.Date(2026, 9, 22, 23, 30, 0, 0, time.UTC)})
			return err
		})
	}
	return pool
}

// seedPastReviewUnrecorded is seedPastReview with no step record of either review step: the
// record of a review that ran under the release before the daemon recorded each step's start.
func seedPastReviewUnrecorded(t *testing.T, at phase.Phase, status string) *pgxpool.Pool {
	t.Helper()
	pool := migratedPool(t)
	standsIn(t, pool, at, status)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", Base: "main", HeadSHA: headTwo, Failing: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, testerPassAt(headOne))
	seedPhase(t, pool, reviewedRow())
	seedClaimSession(t, pool, reviewClaim, "ses-reviewer")
	return pool
}

// A reviewer whose step started before the daemon recorded each step's start is still at work
// when the release that takes a review step's verdict from its step record deploys, so its step
// has no record. Where no start recorded the agent of the reviewer's step, the issue's reviewer
// claim, the claim that step's role runs under, is that step's agent: its verdict past review is
// taken and recorded as that step's, and any other claim's is still refused.
func TestAReviewerStepNoStartRecordedTakesTheReviewersClaim(t *testing.T) {
	pool := seedPastReviewUnrecorded(t, phase.Retro, "retro")
	var asked []string
	engine := writingEngine(&asked)
	refusedFact := pastReviewVerdict("approve", headTwo)
	refusedFact.Claim = "legion-legion-legion-208-implementer"
	if result := applyFacts(t, pool, engine)("implementer", refusedFact); refusalCode(result) != "VERDICT_CALLER_FORBIDDEN" {
		t.Fatalf("the implementer's verdict = %+v, want VERDICT_CALLER_FORBIDDEN", result.Refusal)
	}
	applyRefusingNothing(t, pool, engine, pastReviewVerdict("approve", headTwo))
	given, found := stepVerdictOf(t, pool, headTwo, reviewerStep)
	if !found || given.Verdict != store.VerdictApprove || given.Session != string(reviewClaim) {
		t.Fatalf("the verdict of the reviewer's step at %s = %+v (found %t), want the reviewer's approve under %s", headTwo, given, found, reviewClaim)
	}
	if !slices.Equal(asked, []string{headTwo}) {
		t.Fatalf("the engine asked for writes on %v, want one on %s", asked, headTwo)
	}
}

// pastReviewVerdict is the recorded reviewer's verdict past review at commit, as the review-verdict
// route queues it (api's reviewVerdict).
func pastReviewVerdict(verdict, commit string) intake.ReviewVerdict {
	return intake.ReviewVerdict{Issue: "LEGION-208", Claim: reviewClaim, Verdict: verdict, Commit: commit, Summary: "read the push"}
}

// ownerVerdict is the owner's verdict past review at commit, from the architect that ran its
// acceptance, as the review-verdict route queues it.
func ownerVerdict(verdict, commit string) intake.ReviewVerdict {
	return intake.ReviewVerdict{Issue: "LEGION-208", Claim: architectClaim, Verdict: verdict, Commit: commit, Summary: "read the push as the owner"}
}

// writingEngine is testEngine with a writer of legion-verdict that keeps the head of each write the
// engine asks for (Config.WriteVerdict), once the fact's transaction commits.
func writingEngine(asked *[]string) *Engine {
	engine := testEngine(config.DesignGateRootIssues, nil)
	engine.cfg.WriteVerdict = func(_, _ string, _ int, head string) { *asked = append(*asked, head) }
	return engine
}

// handoffFields is what of a phase row a completion writes and a verdict past review does not.
func handoffFields(row record.PhaseRow) []any {
	return []any{row.Claim, row.HandoffCommit, row.HandoffRecord, row.HandoffVersion, row.Summary, row.CompletedAt.UTC()}
}

// The review-verdict route queues one intake.ReviewVerdict (api's reviewVerdict), and Engine.Apply
// applies it: the round's decision at the call's head on the reviewer's row, the verdict of the
// reviewer's step there, and the write of legion-verdict asked at that head on the fact's commit.
// Without its case in Engine.apply's switch, the fact falls to the default, which records nothing.
func TestReviewVerdictFactIsRouted(t *testing.T) {
	pool := seedPastReview(t, phase.Retro, "retro")
	var asked []string
	applyRefusingNothing(t, pool, writingEngine(&asked), pastReviewVerdict("approve", headTwo))
	if state, head := storedDecision(t, pool); state != "approved" || head != headTwo {
		t.Fatalf("the round's decision = %q at %q, want approved at %s", state, head, headTwo)
	}
	given, found := stepVerdictOf(t, pool, headTwo, reviewerStep)
	if !found || given.Verdict != store.VerdictApprove || given.Issue != "LEGION-208" || given.Session != string(reviewClaim) {
		t.Fatalf("the verdict of the reviewer's step at %s = %+v (found %t), want the reviewer's approve", headTwo, given, found)
	}
	if !slices.Equal(asked, []string{headTwo}) {
		t.Fatalf("the engine asked for writes on %v, want one on %s", asked, headTwo)
	}
}

// R5 and C57: past review, the verdict of the agent of a review step of the task that has passed,
// at the current head, is that step's verdict there, and it writes no handoff and moves no phase:
// the reviewer's approve or changes is also the round's decision there, and the owner's, from the
// architect that ran its acceptance, is its acceptance's verdict and leaves the round's decision as
// it stood. Each asks for the write of legion-verdict at that head in its own pass. In accepting
// the owner's acceptance is the step that runs, which review_verdict refuses
// (TestReviewVerdictRefusesTheStepRunningNow), so the owner's verdict is sent here in retro and in
// merging alone.
func TestAReviewVerdictPastReviewMovesNoPhase(t *testing.T) {
	for _, stand := range []struct {
		at     phase.Phase
		status string
	}{{phase.Accepting, "needs_review"}, {phase.Retro, "retro"}, {phase.Merging, "retro"}} {
		for _, tc := range []struct {
			name  string
			fact  intake.ReviewVerdict
			step  int
			by    claim.Token
			state string
		}{
			{name: "the reviewer's approve", fact: pastReviewVerdict("approve", headTwo), step: reviewerStep, by: reviewClaim, state: "approved"},
			{name: "the reviewer's changes", fact: pastReviewVerdict("changes", headTwo), step: reviewerStep, by: reviewClaim, state: "changes_requested"},
			{name: "the owner's approve", fact: ownerVerdict("approve", headTwo), step: acceptanceStep, by: architectClaim, state: "approved"},
			{name: "the owner's changes", fact: ownerVerdict("changes", headTwo), step: acceptanceStep, by: architectClaim, state: "approved"},
		} {
			if stand.at == phase.Accepting && tc.by == architectClaim {
				continue
			}
			t.Run(string(stand.at)+", "+tc.name, func(t *testing.T) {
				pool := seedPastReview(t, stand.at, stand.status)
				var asked []string
				applyRefusingNothing(t, pool, writingEngine(&asked), tc.fact)
				wantHead := headTwo
				if tc.step != reviewerStep {
					wantHead = headOne
				}
				if state, head := storedDecision(t, pool); state != tc.state || head != wantHead {
					t.Fatalf("the round's decision = %q at %q, want %s at %s", state, head, tc.state, wantHead)
				}
				if got, want := handoffFields(reviewerRow(t, pool)), handoffFields(reviewedRow()); !slices.Equal(got, want) {
					t.Fatalf("the reviewer's handoff = %v, want it as its completion left it, %v: the verdict writes no handoff", got, want)
				}
				if given, found := stepVerdictOf(t, pool, headTwo, tc.step); !found || string(given.Verdict) != tc.fact.Verdict || given.Session != string(tc.by) {
					t.Fatalf("the verdict of step %d at %s = %+v (found %t), want %s's %s", tc.step, headTwo, given, found, tc.by, tc.fact.Verdict)
				}
				if !slices.Equal(asked, []string{headTwo}) {
					t.Fatalf("the engine asked for writes on %v, want one on %s", asked, headTwo)
				}
				if got := issuePhase(t, pool); got != stand.at {
					t.Fatalf("after the verdict the issue is in %s, want %s: a verdict past review moves no phase", got, stand.at)
				}
				if tester := roleRow(t, pool, claim.RoleTester); tester.HandoffCommit != headOne {
					t.Fatalf("the tester's row = %+v, want its completion at %s kept", tester, headOne)
				}
			})
		}
	}
}

// The route reads the record before the fact's transaction, and takes the call's caller, its
// issue's place and its head only here, under the fact's lock: a claim the record names the agent
// of no review step the issue has reached, an issue not past its review, and a head the pull
// request has moved off are refused, and a lingering tree records nothing. Each refusal records
// nothing and asks for no write.
func TestAReviewVerdictTheRecordNoLongerTakesIsRefused(t *testing.T) {
	withClaim := func(fact intake.ReviewVerdict, by claim.Token) intake.ReviewVerdict {
		fact.Claim = by
		return fact
	}
	for _, tc := range []struct {
		name   string
		seed   func(t *testing.T, pool *pgxpool.Pool)
		fact   intake.ReviewVerdict
		status int
		code   string
	}{
		{name: "a head the pull request has moved off", fact: pastReviewVerdict("approve", headOne), status: 409, code: "VERDICT_HEAD_NOT_CURRENT"},
		{name: "another reviewer claim", fact: withClaim(pastReviewVerdict("approve", headTwo), "legion-legion-legion-209-reviewer"),
			status: 403, code: "VERDICT_CALLER_FORBIDDEN"},
		{name: "the implementer's claim", fact: withClaim(pastReviewVerdict("approve", headTwo), "legion-legion-legion-208-implementer"),
			status: 403, code: "VERDICT_CALLER_FORBIDDEN"},
		{name: "the tester's claim", fact: withClaim(pastReviewVerdict("approve", headTwo), "legion-legion-legion-208-tester"),
			status: 403, code: "VERDICT_CALLER_FORBIDDEN"},
		{name: "an issue sent back to testing", seed: func(t *testing.T, pool *pgxpool.Pool) { standsIn(t, pool, phase.Testing, "testing") },
			fact: pastReviewVerdict("approve", headTwo), status: 409, code: "VERDICT_NOT_PAST_REVIEW"},
		{name: "a lingering tree", seed: func(t *testing.T, pool *pgxpool.Pool) {
			until := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Retro, Generation: 1, Status: "retro", Rank: "U", LingerUntil: &until})
		}, fact: pastReviewVerdict("approve", headTwo), status: 409, code: "TREE_LINGERING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedPastReview(t, phase.Retro, "retro")
			if tc.seed != nil {
				tc.seed(t, pool)
			}
			var asked []string
			result := applyFacts(t, pool, writingEngine(&asked))("verdict", tc.fact)
			if result.Refusal == nil || result.Refusal.Code != tc.code || result.Refusal.Status != tc.status ||
				!strings.Contains(result.Refusal.Message, "nothing was recorded") && !strings.Contains(result.Refusal.Message, "changed nothing") {
				t.Fatalf("the verdict was answered %+v, want %d %s saying nothing was recorded", result.Refusal, tc.status, tc.code)
			}
			if state, head := storedDecision(t, pool); state != "approved" || head != headOne {
				t.Fatalf("the round's decision = %q at %q, want the approve of %s as it stood", state, head, headOne)
			}
			for _, step := range []int{reviewerStep, acceptanceStep} {
				if given, found := stepVerdictOf(t, pool, tc.fact.Commit, step); found && tc.fact.Commit != headOne {
					t.Fatalf("the store holds the verdict %+v of step %d at %s, want none: the refusal records nothing", given, step, tc.fact.Commit)
				}
			}
			if len(asked) != 0 {
				t.Fatalf("the engine asked for writes on %v, want none", asked)
			}
		})
	}
}

// A held issue moves only when its hold ends, and a verdict past review moves no phase at all: the
// recorded reviewer's verdict taken while the issue is held is recorded at its head, and the hold's
// end - the architect's retry, or the operator's restart of the phase's worker - releases the
// issue to the phase it was held from, whatever the verdict says.
func TestAVerdictTakenWhileHeldMovesNothing(t *testing.T) {
	retry := intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision}
	restart := intake.RestartPhase{Issue: "LEGION-208", Role: claim.RoleImplementer}
	for _, tc := range []struct {
		name     string
		verdict  intake.ReviewVerdict
		ends     intake.Fact
		decision string
	}{
		{name: "an approve, then the architect's retry", verdict: pastReviewVerdict("approve", headTwo), ends: retry, decision: "approved"},
		{name: "a changes, then the operator's restart", verdict: pastReviewVerdict("changes", headTwo), ends: restart, decision: "changes_requested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedPastReview(t, phase.Retro, "retro")
			holdFromRetro(t, pool)
			var asked []string
			engine := writingEngine(&asked)
			applyRefusingNothing(t, pool, engine, tc.verdict)
			if got := issuePhase(t, pool); got != phase.Held {
				t.Fatalf("after the verdict the issue is in %s, want held: a held issue moves only when its hold ends", got)
			}
			if state, head := storedDecision(t, pool); state != tc.decision || head != headTwo {
				t.Fatalf("the round's decision = %q at %q, want %s at %s", state, head, tc.decision, headTwo)
			}
			if given, found := stepVerdictOf(t, pool, headTwo, reviewerStep); !found || given.Verdict != store.VerdictWord(tc.verdict.Verdict) {
				t.Fatalf("the verdict of the reviewer's step at %s = %+v (found %t), want the reviewer's %s", headTwo, given, found, tc.verdict.Verdict)
			}
			if !slices.Equal(asked, []string{headTwo}) {
				t.Fatalf("the engine asked for writes on %v, want one on %s", asked, headTwo)
			}
			applyRefusingNothing(t, pool, engine, tc.ends)
			if got := issuePhase(t, pool); got != phase.Retro {
				t.Fatalf("after the hold ended the issue is in %s, want retro: no verdict moves a phase", got)
			}
		})
	}
}

// holdFromRetro holds LEGION-208 from retro, as its implementer's failed claim does.
func holdFromRetro(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Retro}, Generation: 1,
		Status: "retro", Rank: "U"})
}

// An ask past review (record.MergeForwardReview) is delivered while the issue stands past its
// review and its pull request stands at the head the ask is about, whether a merge forward or any
// other push moved it there; a later push, which asks again, or a move out of the phases past
// review ends it undelivered. The other steps of a merge forward keep their rule: an ask with no
// merge forward recorded is no longer delivered.
func TestAnAskPastReviewIsDeliveredWhileItsHeadStands(t *testing.T) {
	left := func(t *testing.T, pool *pgxpool.Pool, step record.MergeForwardStep, head, landed string) string {
		t.Helper()
		// The review goes to the reviewer and the ask to the implementer (record.MergeForwardReaches).
		role := claim.RoleImplementer
		if step == record.MergeForwardReview {
			role = claim.RoleReviewer
		}
		var why string
		seedRecord(t, pool, func(tx pgx.Tx) (err error) {
			_, _, why, err = MergeForwardAt(t.Context(), record.NewStore(), tx, "LEGION-208", role, step, head, landed)
			return err
		})
		return why
	}
	pool := seedPastReview(t, phase.Accepting, "needs_review")
	if why := left(t, pool, record.MergeForwardReview, headOne, headTwo); why != "" {
		t.Fatalf("the ask of a verdict at %s is not delivered (%s), want it delivered", headTwo, why)
	}
	if why := left(t, pool, record.MergeForwardAsk, headTwo, ""); why != "the record holds no merge forward" {
		t.Fatalf("an ask of a merge forward the record does not hold is left as %q, want the merge forward's rule", why)
	}
	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", Base: "main", Before: headTwo, HeadSHA: headThree})
	if why := left(t, pool, record.MergeForwardReview, headOne, headTwo); !strings.Contains(why, headThree) {
		t.Fatalf("the ask at %s after a push to %s is left as %q, want it undelivered, naming %s", headTwo, headThree, why, headThree)
	}
	standsIn(t, pool, phase.Testing, "testing")
	if why := left(t, pool, record.MergeForwardReview, headTwo, headThree); !strings.Contains(why, "past its review") {
		t.Fatalf("the ask at %s once the issue went back to testing is left as %q, want it undelivered", headThree, why)
	}
}

// verdictAsks is every ask of a verdict past review LEGION-208's outbox holds (the merge-forward
// review rows), in the order written.
func verdictAsks(t *testing.T, pool *pgxpool.Pool) []record.MergeForwardMessage {
	t.Helper()
	asks := []record.MergeForwardMessage{}
	for _, message := range mergeForwardMessages(t, pool) {
		if message.Step == record.MergeForwardReview {
			asks = append(asks, message)
		}
	}
	return asks
}

// Spec R5: each push past review asks the round's recorded reviewer for its verdict at the new
// head, from the head the reviewer last gave its verdict on, so two pushes before the reviewer
// answers each ask about the whole range the reviewer has not judged: from headOne, where its
// approve stands, to headTwo and then to headThree, and never from headTwo, which no verdict
// named. Once the reviewer's verdict names headThree, the next push asks from headThree.
func TestAnAskPastReviewNamesTheHeadOfTheReviewersLastVerdict(t *testing.T) {
	pool := seedPastReview(t, phase.Retro, "retro")
	engine := testEngine(config.DesignGateRootIssues, nil)
	push := func(from, to string) {
		t.Helper()
		applyRefusingNothing(t, pool, engine, intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/LEGION-208", Base: "main", Before: from, HeadSHA: to})
	}
	ask := func(from, to string) record.MergeForwardMessage {
		return record.MergeForwardMessage{Role: claim.RoleReviewer, Step: record.MergeForwardReview, Head: from, Landed: to, Base: "main",
			Phase: phase.Retro, PullRequest: 42}
	}
	push(headTwo, headThree)
	if got, want := verdictAsks(t, pool), []record.MergeForwardMessage{ask(headOne, headThree)}; !slices.Equal(got, want) {
		t.Fatalf("the second push asked %+v, want %+v: the range from the head of the reviewer's last verdict", got, want)
	}
	applyRefusingNothing(t, pool, engine, pastReviewVerdict("approve", headThree))
	push(headThree, headEarlier)
	if got, want := verdictAsks(t, pool), []record.MergeForwardMessage{ask(headOne, headThree), ask(headThree, headEarlier)}; !slices.Equal(got, want) {
		t.Fatalf("the push after the verdict at %s asked %+v, want %+v", headThree, got, want)
	}
}

// architectAsks is the body of every role message LEGION-208's outbox holds for architectClaim, the
// agent of the owner's acceptance, in the order written.
func architectAsks(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'body' from outbox where kind = 'role_message' and issue = 'LEGION-208'
		and payload->>'role' = $1 order by id`, string(architectClaim))
	if err != nil {
		t.Fatalf("read the architect's role messages: %v", err)
	}
	bodies, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the architect's role messages: %v", err)
	}
	return bodies
}

// R5 and C57: each push that moves the head while the issue stands past its review asks the agent
// of each review step of the task that has passed for its verdict at the new head, with or without
// a merge forward: the reviewer by the merge-forward review (record.MergeForwardReview), and the
// architect that ran the owner's acceptance by a role message to its claim, each from the head of
// that step's last verdict. In retro and merging both have passed; in accepting the owner's
// acceptance is the step the issue stands in, which its own sign-off or move back decides at the
// head it stands at, so the reviewer alone is asked. In merging, the two agents' verdicts at the
// new head each ask for the write of legion-verdict there. A push in reviewing asks no verdict past
// review: it re-asks the round, as the round's own rule does.
func TestAHeadThatMovesPastReviewAsksEachReviewStepsAgent(t *testing.T) {
	push := intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Base: "main", Before: headTwo, HeadSHA: headThree}
	reviewerAsk := func(at phase.Phase, why record.ConflictWhy) []record.MergeForwardMessage {
		return []record.MergeForwardMessage{{Role: claim.RoleReviewer, Step: record.MergeForwardReview, Head: headOne, Landed: headThree, Base: "main",
			Phase: at, PullRequest: 42, Why: why}}
	}
	for _, tc := range []struct {
		name    string
		at      phase.Phase
		status  string
		forward bool
		owner   bool
		moved   string
	}{
		{name: "a push in retro", at: phase.Retro, status: "retro", owner: true, moved: "a push moved the head of pull request sjawhar/legion#42"},
		{name: "a push in accepting", at: phase.Accepting, status: "needs_review"},
		{name: "a push in merging", at: phase.Merging, status: "retro", owner: true, moved: "a push moved the head of pull request sjawhar/legion#42"},
		{name: "a merge forward's landing in retro", at: phase.Retro, status: "retro", forward: true, owner: true,
			moved: "the merge of main forward landed on pull request sjawhar/legion#42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedPastReview(t, tc.at, tc.status)
			why := record.ConflictWhy("")
			if tc.forward {
				why = record.ConflictMergeNext
				seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
					Branch: "legion/LEGION-208", Base: "main", HeadSHA: headTwo, Failing: []string{},
					MergeForward: &record.MergeForward{Head: headTwo, Base: "main", Phase: tc.at, Why: why, AskedAt: time.Date(2026, 9, 22, 23, 40, 0, 0, time.UTC)}})
			}
			var asked []string
			engine := writingEngine(&asked)
			applyRefusingNothing(t, pool, engine, push)
			if got, want := verdictAsks(t, pool), reviewerAsk(tc.at, why); !slices.Equal(got, want) {
				t.Fatalf("the push in %s asked the reviewer %+v, want %+v", tc.at, got, want)
			}
			asks := architectAsks(t, pool)
			if !tc.owner {
				if len(asks) != 0 {
					t.Fatalf("the push in %s asked the architect %q, want nothing: its own acceptance is the step the issue stands in", tc.at, asks)
				}
				return
			}
			if len(asks) != 1 {
				t.Fatalf("the push in %s asked the architect %q, want one ask", tc.at, asks)
			}
			for _, want := range []string{"LEGION-208: " + tc.moved, "The old head is " + headOne + ", and the new head is " + headThree,
				"`git diff " + headOne + " " + headThree + "`", "review_verdict: verdict approve or changes, commit " + headThree + ", issue LEGION-208."} {
				if !strings.Contains(asks[0], want) {
					t.Fatalf("the architect's ask reads %q, want it to say %q", asks[0], want)
				}
			}
			if tc.at != phase.Merging {
				return
			}
			applyRefusingNothing(t, pool, engine, pastReviewVerdict("approve", headThree))
			applyRefusingNothing(t, pool, engine, ownerVerdict("approve", headThree))
			if !slices.Equal(asked, []string{headThree, headThree}) {
				t.Fatalf("the two agents' approves asked for writes of legion-verdict on %v, want one on %s in each one's pass", asked, headThree)
			}
			if got := issuePhase(t, pool); got != phase.Merging {
				t.Fatalf("after the two approves the issue is in %s, want merging: a verdict moves no phase", got)
			}
		})
	}

	t.Run("a push in reviewing", func(t *testing.T) {
		pool := seedPastReview(t, phase.Reviewing, "needs_review")
		applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), push)
		if asks := verdictAsks(t, pool); len(asks) != 0 {
			t.Fatalf("the push in reviewing asked the reviewer %+v, want no ask of a verdict past review", asks)
		}
		if asks := architectAsks(t, pool); len(asks) != 0 {
			t.Fatalf("the push in reviewing asked the architect %q, want nothing", asks)
		}
		told := reviewStuckNotices(t, pool)
		if !slices.ContainsFunc(told, func(n record.Notice) bool { return n.Summary == byPush && strings.Contains(n.Reason, headThree) }) {
			t.Fatalf("the push in reviewing told the round %+v, want the round re-asked at %s by the push", told, headThree)
		}
	})
}

// askStands is workflow.VerdictAskStands of agent on LEGION-208 as the store holds it now.
func askStands(t *testing.T, pool *pgxpool.Pool, agent claim.Token) bool {
	t.Helper()
	var stands bool
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		_, stands, err = VerdictAskStands(t.Context(), record.NewStore(), tx, "LEGION-208", agent)
		return err
	})
	return stands
}

// Spec R5: the ask of a verdict past review stands for the agent of a review step of the task that
// has passed, the reviewer's aside, while that agent owes its verdict at the pull request's current
// head: the daemon's role-message executor holds a role message to it while the ask stands, waking
// a released claim (internal/daemon's outbox.roleMessage). In retro, after a push to headTwo, the
// architect that ran the owner's acceptance, whose verdict names headOne, owes one; the reviewer,
// asked by the merge-forward review under a rule of its own, and any other claim do not. Its verdict
// at headTwo ends the ask, and a push to headThree raises it again. A pull request that closed ends
// it, and so does an issue in accepting, whose acceptance is the step it stands in and has not
// passed. The negative control is the reader without the read of the step's last verdict (every
// passed step's agent owing one): the case after the architect's verdict at headTwo fails.
func TestAnAskPastReviewStandsWhileTheStepsAgentOwesItsVerdict(t *testing.T) {
	t.Run("in retro, until the agent's verdict at the current head", func(t *testing.T) {
		pool := seedPastReview(t, phase.Retro, "retro")
		for _, tc := range []struct {
			agent claim.Token
			want  bool
		}{{architectClaim, true}, {reviewClaim, false}, {"legion-legion-legion-209-architect", false}, {"", false}} {
			if got := askStands(t, pool, tc.agent); got != tc.want {
				t.Fatalf("the ask of %q stands = %t at %s, want %t", tc.agent, got, headTwo, tc.want)
			}
		}
		engine := testEngine(config.DesignGateRootIssues, nil)
		applyRefusingNothing(t, pool, engine, ownerVerdict("approve", headTwo))
		if askStands(t, pool, architectClaim) {
			t.Fatalf("the ask of %s stands after its verdict at %s, want it ended", architectClaim, headTwo)
		}
		applyRefusingNothing(t, pool, engine, intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
			Base: "main", Before: headTwo, HeadSHA: headThree})
		if !askStands(t, pool, architectClaim) {
			t.Fatalf("the ask of %s does not stand after the push to %s, want it raised again", architectClaim, headThree)
		}
	})
	t.Run("a closed pull request ends it", func(t *testing.T) {
		pool := seedPastReview(t, phase.Merging, "retro")
		seedPR(t, pool, record.PullRequest{State: record.PullRequestClosed, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/LEGION-208", Base: "main", HeadSHA: headTwo, Failing: []string{}})
		if askStands(t, pool, architectClaim) {
			t.Fatalf("the ask of %s stands on a closed pull request, want none", architectClaim)
		}
	})
	t.Run("in accepting the acceptance has not passed", func(t *testing.T) {
		pool := seedPastReview(t, phase.Accepting, "needs_review")
		if askStands(t, pool, architectClaim) {
			t.Fatalf("the ask of %s stands in accepting, want none: its acceptance is the step the issue stands in", architectClaim)
		}
	})
}

// Spec R5: the merger merges a head that moved past review only once legion-verdict at that head
// is success. So the merger started in merging is told so whenever the head it starts at is not
// the head of the reviewer's last verdict, a push of any kind having moved it there, and that the
// agent of each review step is asked for its verdict on it, the owner's acceptance among them (spec
// R4: success there takes every review step's approve at that head); and it is told nothing of it
// once the reviewer's verdict names that head.
func TestTheMergerWaitsForTheVerdictAtAHeadAnyPushMovedPastReview(t *testing.T) {
	const waits = " only once legion-verdict at that head is success."
	for _, tc := range []struct {
		name    string
		verdict bool
	}{{name: "a push past review the reviewer has not judged"}, {name: "the reviewer's verdict at the pushed head", verdict: true}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedPastReview(t, phase.Retro, "retro")
			engine := testEngine(config.DesignGateRootIssues, nil)
			if tc.verdict {
				applyRefusingNothing(t, pool, engine, pastReviewVerdict("approve", headTwo))
			}
			applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer,
				Claim: "implement-claim", Summary: "retro done", Commit: headTwo})
			if got := issuePhase(t, pool); got != phase.Merging {
				t.Fatalf("after retro's completion the issue is in %s, want merging", got)
			}
			task := startTask(t, pool, claim.RoleMerger)
			if told := strings.Contains(task, "merge head "+headTwo+waits); told == tc.verdict {
				t.Fatalf("the merger's task reads %q; told to wait for legion-verdict at %s = %t, want %t", task, headTwo, told, !tc.verdict)
			}
			if !tc.verdict && (!strings.Contains(task, "the reviewer's verdict at head "+headOne) ||
				!strings.Contains(task, "the agent of each review step is asked for its verdict on it")) {
				t.Fatalf("the merger's task reads %q, want it to name %s, the head of the reviewer's last verdict, and the ask of each review step", task, headOne)
			}
		})
	}
}

// A round held from reviewing can become stuck on GitHub's merge state while held: the reviewer
// approved the head, and the read GitHub answers `behind` for it, a state the round does not end
// on, lands during the hold. The retry
// that ends the hold is then the fact that tells the architect, and its review-stuck notice names
// the round's facts as its summary: the head, the reviewer, and how long ago the reviewer gave its
// verdict, never an empty summary.
func TestARetriedRoundStuckOnItsMergeStateTellsTheRoundsFacts(t *testing.T) {
	pool := migratedPool(t)
	seedMergeStateRound(t, pool, record.MergeStateUnread)
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	apply("verdict", reviewerVerdict("approve", headOne))
	apply("failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleReviewer})
	apply("behind", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: headOne,
		Mergeable: record.MergeabilityMergeable, MergeState: record.MergeStateBehind})
	if told := reviewStuckNotices(t, pool); len(told) != 0 {
		t.Fatalf("review-stuck notices before the retry = %+v, want none: the round was held", told)
	}
	later := applyFacts(t, pool, mergingEngineAt(time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)))
	later("retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision})
	told := reviewStuckNotices(t, pool)
	if len(told) != 1 {
		t.Fatalf("review-stuck notices after the retry = %+v, want one", told)
	}
	for _, want := range []string{"head " + headOne, "reviewer " + string(reviewClaim), "2h0m ago"} {
		if !strings.Contains(told[0].Summary, want) {
			t.Fatalf("the notice's summary is %q, want it to name %q", told[0].Summary, want)
		}
	}
}
