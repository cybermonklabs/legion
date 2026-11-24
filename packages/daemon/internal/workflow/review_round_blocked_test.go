package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/verdict"
)

// githubFake is GitHub as the real engine reaches it for LEGION-208's pull request #42, through the
// two calls the daemon makes on the commit of a fact: a read of the head's merge state
// (Config.ReadMergeState) and a write of legion-verdict on a head (Config.WriteVerdict). The engine
// asks for each on the commit, and the fake answers once the fact's transaction is over, as the
// daemon's reader and writer run after it (serve): first every write asked for, since the daemon
// holds a read until the write before it ends (askVerdictWrite), then every read, each applied as
// the daemon's read applies GitHub's answer (intake.PullRequestMergeability).
type githubFake struct {
	t      *testing.T
	pool   *pgxpool.Pool
	engine *Engine
	// mergeable and state are GitHub's answer for the head the branch stands at, unless
	// legion-verdict is success there: the base requires that check, so GitHub answers `blocked`
	// until the run exists, and `clean` once it does (answerFor).
	mergeable record.Mergeability
	state     record.MergeState
	// reads is how many reads the engine has asked for that GitHub has not answered, and writes
	// each head the engine has asked legion-verdict written on and the writer has not decided yet.
	reads  int
	writes []string
	// answered is each state GitHub answered a read with, in order, and at the moment of the last
	// answer: each read is a minute after the one before, so the record takes every one of them
	// (StateReadAt) and two conflicting reads are a confirmation apart (conflictConfirmWait).
	answered []record.MergeState
	at       time.Time
	// verdicts is legion-verdict's conclusion on each head the writer wrote it on.
	verdicts map[string]verdict.Conclusion
}

// newGitHubFake is the fake GitHub of pool's LEGION-208, wired into engine as its reader of the
// merge state and its writer of legion-verdict. GitHub answers MERGEABLE and state until a test
// says otherwise.
func newGitHubFake(t *testing.T, pool *pgxpool.Pool, engine *Engine, state record.MergeState) *githubFake {
	g := &githubFake{t: t, pool: pool, engine: engine, mergeable: record.MergeabilityMergeable, state: state,
		at: time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC), verdicts: map[string]verdict.Conclusion{}}
	engine.cfg.ReadMergeState = func(string) { g.reads++ }
	engine.cfg.WriteVerdict = func(_, _ string, _ int, head string) { g.writes = append(g.writes, head) }
	return g
}

// apply applies fact through the engine, fails the test on a refusal, and then serves what the
// fact's commit asked GitHub for.
func (g *githubFake) apply(id string, fact intake.Fact) {
	g.t.Helper()
	if result := applyFacts(g.t, g.pool, g.engine)(id, fact); result.Refusal != nil {
		g.t.Fatalf("%s was refused: %+v", id, result.Refusal)
	}
	g.serve()
}

// serve writes legion-verdict on each head asked for, then answers each read asked for, until
// nothing the engine asked is left: a read that GitHub answers is a fact of its own, which can ask
// again.
func (g *githubFake) serve() {
	g.t.Helper()
	for len(g.writes) > 0 || g.reads > 0 {
		if len(g.writes) > 0 {
			head := g.writes[0]
			g.writes = g.writes[1:]
			if conclusion := g.decide(head); conclusion != verdict.None {
				g.verdicts[head] = conclusion
			}
			continue
		}
		g.reads--
		g.read()
	}
}

// answerFor is what GitHub answers for head: clean once legion-verdict is success on it, and the
// fake's state otherwise.
func (g *githubFake) answerFor(head string) record.MergeState {
	if g.verdicts[head] == verdict.Success {
		return record.MergeStateClean
	}
	return g.state
}

// read is one read of #42 answered by GitHub, a minute after the read before it, about the head
// the branch stands at, applied as the daemon applies its read. It is also the second read a first
// conflicting read queues (record.ConflictConfirm), which the daemon makes from its outbox and not
// from a fact's ask.
func (g *githubFake) read() {
	g.t.Helper()
	head := recordedPR(g.t, g.pool, "LEGION-208").HeadSHA
	g.at = g.at.Add(conflictConfirmWait)
	state := g.answerFor(head)
	g.answered = append(g.answered, state)
	if result := applyFacts(g.t, g.pool, g.engine)("read-"+g.at.Format(time.RFC3339), intake.PullRequestMergeability{
		Repo: "sjawhar/legion", Number: 42, Base: "main", HeadSHA: head, Mergeable: g.mergeable, MergeState: state, ReadAt: g.at}); result.Refusal != nil {
		g.t.Fatalf("GitHub's answer %s for %s was refused: %+v", state, head, result.Refusal)
	}
}

// decide is the writer's conclusion of legion-verdict on head, read from what the engine recorded
// there as the daemon's writer reads it (its legionInputs): each step of the task the issue's run
// walks (Engine.VerdictSteps), each review step's verdict at head, and the issue's check states at
// head, decided by verdict.Decide. It is a stand-in for that writer, which lives in
// internal/daemon, and it leaves out the reads the writer makes outside the engine's records: the
// change's commits from GitHub's compare and the sessions claim_sessions records for each claim
// (readCommits). In their place the change is the head alone, made by the claims that ran an
// implementation step, and each claim's one session is its token, so a review step's approve reads
// the change unless its own claim ran the implementation. The daemon's TestLegionVerdict tests hold
// the reads themselves.
func (g *githubFake) decide(head string) verdict.Conclusion {
	g.t.Helper()
	in := verdict.Inputs{Head: head, Legion: true}
	repository := ghrepo.MustParse("sjawhar/legion")
	seedRecord(g.t, g.pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		issue, err := g.engine.store.Issue(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		steps, err := g.engine.VerdictSteps(ctx, tx, *issue)
		if err != nil {
			return err
		}
		var implementers []string
		for _, step := range steps {
			if step.Type == plan.TypeImplementation && step.Agent != "" {
				implementers = append(implementers, string(step.Agent))
			}
		}
		in.Commits = []verdict.Commit{{SHA: head, Makers: implementers}}
		for _, step := range steps {
			decided := verdict.Step{Type: step.Type}
			switch step.Type {
			case plan.TypeReview:
				given, found, err := store.StepVerdictTx(ctx, tx, repository, 42, head, step.Place.Task, step.Place.Place)
				if err != nil {
					return err
				}
				if found {
					decided.Verdict, decided.Sessions = given.Verdict, []string{given.Session}
				}
			case plan.TypeTesting:
				if step.Agent != "" {
					decided.Sessions = []string{string(step.Agent)}
				}
			}
			in.Steps = append(in.Steps, decided)
		}
		held, err := g.engine.store.HeadStates(ctx, tx, "LEGION-208", "sjawhar/legion", head)
		for _, check := range held.Checks {
			in.Checks = append(in.Checks, verdict.Check{Name: check, State: held.States[check].State})
		}
		if held.Checks != nil && in.Checks == nil {
			in.Checks = []verdict.Check{}
		}
		return err
	})
	conclusion, _ := verdict.Decide(in)
	return conclusion
}

// merge is the merger's merge of #42 at head: GitHub merges only a head it answers a state it
// would merge from, and the merge is applied as its webhook delivers it.
func (g *githubFake) merge(head string) {
	g.t.Helper()
	if state := g.answerFor(head); !state.Mergeable() {
		g.t.Fatalf("GitHub refuses the merger's merge of #42 at %s: it answers %s, legion-verdict there is %q", head, state, g.verdicts[head])
	}
	g.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, MergeSHA: "merge-42"})
}

// waitsOnGitHub is whether LEGION-208's round waits on GitHub's answer for its head as the record
// holds it now: the rule the state build reads for the issue's mergeWait
// (classify.RoundWaitsOnMergeState, projection's mergeWaitView), so it is what `legion state`
// shows.
func waitsOnGitHub(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	pr := recordedPR(t, pool, "LEGION-208")
	return classify.RoundWaitsOnMergeState(*issueRecord(t, pool, "LEGION-208"), reviewerRow(t, pool), &pr)
}

// standsUntold fails the test unless LEGION-208 stands in want and no review-stuck notice has gone
// out.
func standsUntold(t *testing.T, pool *pgxpool.Pool, want phase.Phase, why string) {
	t.Helper()
	if got := issuePhase(t, pool); got != want {
		t.Fatalf("LEGION-208 is in %s, want %s: %s", got, want, why)
	}
	if told := reviewStuckNotices(t, pool); len(told) != 0 {
		t.Fatalf("the architect was told the round was stuck %+v, want nothing: %s", told, why)
	}
}

// waits fails the test unless LEGION-208's round waits in reviewing on GitHub, untold, and the
// issue's view would carry the wait.
func waits(t *testing.T, pool *pgxpool.Pool, why string) {
	t.Helper()
	standsUntold(t, pool, phase.Reviewing, why)
	if !waitsOnGitHub(t, pool) {
		t.Fatalf("the round waits on no answer of GitHub's, want the wait the state shows: %s", why)
	}
}

// With the default steps legion-verdict waits for the owner's acceptance, which comes after the
// review round, and GitHub answers `blocked` until that required check exists. So an approval that
// stands on the head ends the round on `blocked` as it does on a state GitHub would merge from,
// once GitHub has reported a check run on that head and every one of them has concluded
// (classify.ChecksConcludedOnHead): the daemon reads no ruleset, so it cannot tell a missing
// legion-verdict from another required check, and the merger answers the merge state at merging.
// The round still waits while GitHub has given no answer, unread or `unknown`, and while a check
// run of the head is still running, with the wait on the issue's view. `behind` and `draft` keep
// today's stuck round and its notice, told once, and a head that conflicts with its base keeps
// today's rule: its first conflicting read waits for the second, and a confirmed conflict ends the
// round with the merge forward asked (cybermonklabs/legion#4464).
func TestAnApprovalOfABlockedHeadWhoseChecksConcludedMovesToAccepting(t *testing.T) {
	// rig is LEGION-208 in reviewing, its pull request #42 at headOne with CI settled there and no
	// merge state read (seedMergeStateRound), applied through the real engine and the fake GitHub.
	rig := func(t *testing.T, state record.MergeState) (*pgxpool.Pool, *githubFake) {
		t.Helper()
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateUnread)
		return pool, newGitHubFake(t, pool, testEngine(config.DesignGateRootIssues, nil), state)
	}
	approve := reviewAt(1, "approved", headOne)

	t.Run("GitHub answers blocked once every check run on the head concluded", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateBlocked)
		github.apply("approve", approve)
		if len(github.answered) != 1 || github.answered[0] != record.MergeStateBlocked {
			t.Fatalf("GitHub answered %v, want the one read the approval asked for, answered blocked", github.answered)
		}
		standsUntold(t, pool, phase.Accepting, "the approval stands on a blocked head whose checks concluded")
	})

	t.Run("GitHub's answer unread", func(t *testing.T) {
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateUnread)
		engine := testEngine(config.DesignGateRootIssues, nil)
		github := newGitHubFake(t, pool, engine, record.MergeStateBlocked)
		if result := applyFacts(t, pool, engine)("approve", approve); result.Refusal != nil {
			t.Fatalf("the approval was refused: %+v", result.Refusal)
		}
		if github.reads != 1 {
			t.Fatalf("the approval asked GitHub for %d reads, want one: the round waits on GitHub's answer", github.reads)
		}
		waits(t, pool, "GitHub has answered nothing for the head")
		github.serve()
		standsUntold(t, pool, phase.Accepting, "GitHub answered blocked for a head whose checks concluded")
	})

	t.Run("GitHub answers unknown", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateUnknown)
		github.apply("approve", approve)
		waits(t, pool, "GitHub is still computing the merge state")
		github.state = record.MergeStateBlocked
		github.read()
		standsUntold(t, pool, phase.Accepting, "the read after unknown answered blocked")
	})

	t.Run("a check run of the head is still running", func(t *testing.T) {
		pool := migratedPool(t)
		seedMergeStateRound(t, pool, record.MergeStateUnread)
		// CI settled the head before the reviewer's push; the head the approval stands on has no
		// settlement of its own, which is what a CI run still going there leaves.
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: headOne, Failing: []string{}, Cancelled: []string{},
			Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateBlocked,
			CheckedHead: headEarlier, CheckRuns: []record.AttemptRun{{Name: "unit tests", ID: 1, Head: headEarlier, Conclusion: record.RunSucceeded}}})
		github := newGitHubFake(t, pool, testEngine(config.DesignGateRootIssues, nil), record.MergeStateBlocked)
		github.apply("approve", approve)
		if len(github.answered) != 1 {
			t.Fatalf("GitHub answered %v, want the one read the approval asked for", github.answered)
		}
		waits(t, pool, "GitHub answers blocked while a check run of the head runs")
		// The last check concludes: the settlement asks for a read, and GitHub still answers blocked.
		github.apply("settled", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: headOne, Generation: 1, Snapshot: "settled",
			CheckRuns: []record.AttemptRun{{Name: "unit tests", ID: 2}}, Failing: []string{}, Cancelled: []string{}})
		if len(github.answered) != 2 || github.answered[1] != record.MergeStateBlocked {
			t.Fatalf("GitHub answered %v, want a second read, asked by the settlement, answered blocked", github.answered)
		}
		standsUntold(t, pool, phase.Accepting, "every check run of the head has concluded and GitHub answers blocked")
	})

	for _, state := range []record.MergeState{record.MergeStateBehind, record.MergeStateDraft} {
		t.Run("GitHub answers "+string(state), func(t *testing.T) {
			pool, github := rig(t, state)
			github.apply("approve", approve)
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("LEGION-208 is in %s, want reviewing: GitHub will not merge a head it answers %s", got, state)
			}
			told := reviewStuckNotices(t, pool)
			if len(told) != 1 || !strings.Contains(told[0].Reason, "GitHub reports that head "+string(state)+" rather than one it would merge") {
				t.Fatalf("review-stuck notices %+v, want one naming %s (stuckMergeState)", told, state)
			}
			if stuck := stuckRoleMessages(t, pool); len(stuck) != 1 {
				t.Fatalf("the reviewer was told %d times %+v, want once", len(stuck), stuck)
			}
			if !waitsOnGitHub(t, pool) {
				t.Fatalf("the stuck round waits on no answer of GitHub's, want the wait the state shows")
			}
			github.read()
			if again := reviewStuckNotices(t, pool); len(again) != 1 || issuePhase(t, pool) != phase.Reviewing {
				t.Fatalf("after a second %s read the notices are %+v in %s, want the one notice and reviewing", state, again, issuePhase(t, pool))
			}
		})
	}

	t.Run("a head that conflicts with its base", func(t *testing.T) {
		pool, github := rig(t, record.MergeStateDirty)
		github.mergeable = record.MergeabilityConflicting
		github.apply("approve", approve)
		if confirms := issueOutboxOf(t, pool, "LEGION-208", string(record.OutboxKindConflictConfirm)); len(confirms) != 1 {
			t.Fatalf("the outbox holds %d second reads, want the one the first conflicting read queued", len(confirms))
		}
		waits(t, pool, "a first conflicting read waits for its second")
		// The second read, a minute after the first, confirms the conflict (dispatch://INFRA-4374 R2).
		github.read()
		standsUntold(t, pool, phase.Accepting, "a confirmed conflict ends the round whatever GitHub's state")
		if pr := recordedPR(t, pool, "LEGION-208"); pr.ConflictingHead != headOne || pr.MergeForward == nil || pr.MergeForward.Head != headOne {
			t.Fatalf("the record holds %q conflicting and the merge forward %+v, want %s held and its merge forward asked", pr.ConflictingHead, pr.MergeForward, headOne)
		}
	})
}

// C65: the default steps, planner implementer tester reviewer acceptance:owner, run end to end on
// one head while GitHub answers blocked for it until legion-verdict exists there: the tester's
// checks read yes, the reviewer approves, the round ends on blocked once the head's checks have
// concluded and the issue reaches accepting, the owner accepts, the writer writes legion-verdict
// success, GitHub answers clean, and the merger merges. A round that waited for GitHub to answer
// that the head can merge would wait for the owner's acceptance, which comes after it, so the issue
// would never leave reviewing.
func TestTheDefaultStepsReachLegionVerdictSuccess(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	seedTreeLifecycle(t, pool, root.Key)
	// The pull request stands at headOne with no merge state read: GitHub's answers are the fake's.
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(t.Context(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key,
			Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Base: "main", HeadSHA: headOne, Failing: []string{}})
	})
	github := newGitHubFake(t, pool, engine, record.MergeStateBlocked)
	token := func(role claim.Role) claim.Token { return claim.Token(treeRoleToken(t, root.Key, role)) }
	completion := func(role claim.Role, handoff, commit, word string) intake.HandoffComplete {
		return intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: role, Claim: token(role), Summary: string(role) + " done",
			Commit: commit, Verdict: word, HandoffRecord: handoff, HandoffVersion: 1}
	}
	at := func(want phase.Phase, after string) {
		t.Helper()
		if got := issuePhase(t, pool); got != want {
			t.Fatalf("after %s LEGION-208 is in %s, want %s", after, got, want)
		}
	}

	firstStep(t, pool, engine, root.Key)
	at(phase.Planning, "its first step")
	github.apply("planner", completion(claim.RolePlanner, "plan-1", "", ""))
	at(phase.Implementing, "the planner's completion")
	github.apply("implementer", completion(claim.RoleImplementer, "implement-1", headOne, ""))
	at(phase.Testing, "the implementer's completion")
	github.apply("settled", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: headOne, Generation: 1, Snapshot: "settled",
		CheckRuns: []record.AttemptRun{{Name: "unit tests", ID: 1}}, Failing: []string{}, Cancelled: []string{}})
	state, id := testedFact(t, pool, root.Key, headOne, true)
	github.apply(id, state)
	github.apply("tester", completion(claim.RoleTester, "test-1", headOne, ""))
	at(phase.Reviewing, "the tester's checks read yes")

	github.apply("reviewer", completion(claim.RoleReviewer, "review-1", headOne, "approve"))
	if len(github.verdicts) != 0 {
		t.Fatalf("legion-verdict after the reviewer's approval = %v, want none: the owner's acceptance has not passed", github.verdicts)
	}
	if n := len(github.answered); n == 0 || github.answered[n-1] != record.MergeStateBlocked {
		t.Fatalf("GitHub answered %v, want blocked for the approved head: legion-verdict does not exist there", github.answered)
	}
	standsUntold(t, pool, phase.Accepting, "the reviewer's approval stands on a blocked head whose checks concluded")

	github.apply("sign-off", rootsSignOff(t, pool, engine, root.Key))
	if got := github.verdicts; len(got) != 1 || got[headOne] != verdict.Success {
		t.Fatalf("legion-verdict after the owner's acceptance = %v, want success on %s alone", got, headOne)
	}
	at(phase.Retro, "the owner's acceptance")
	github.apply("retro", intake.HandoffComplete{Generation: 1, Issue: root.Key, Role: claim.RoleImplementer, Claim: token(claim.RoleImplementer),
		Summary: "retro done", Commit: headOne})
	at(phase.Merging, "the retro's completion")

	github.merge(headOne)
	at(phase.Done, "the merger's merge")
}
