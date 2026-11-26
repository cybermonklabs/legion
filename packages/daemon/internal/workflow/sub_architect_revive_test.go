package workflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The tree whose child's architect a phase start revives: its root and the child under it.
const (
	reviveRoot  = "LEGION-208"
	reviveChild = "LEGION-209"
)

// reviveIssue is key, an issue of reviveRoot's tree on a repository the engine serves, standing at
// at with status: the root itself, or a child under the root.
func reviveIssue(key string, at phase.Phase, status string) record.Issue {
	issue := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: reviveRoot,
		Project: "LEGION", Title: key, Phase: at, Generation: 1, Status: status, Rank: "V"}
	if key != reviveRoot {
		issue.Parent = new(reviveRoot)
	}
	return issue
}

// seedReviveTree records reviveRoot's live tree: the root standing at rootAt, its design gate open
// and its admission slot held, as a running tree's root stands.
func seedReviveTree(t *testing.T, pool *pgxpool.Pool, rootAt phase.Phase) {
	t.Helper()
	seedIssue(t, pool, reviveIssue(reviveRoot, rootAt, PhaseStatus(rootAt)))
	seedGate(t, pool, record.DesignGate{Issue: reviveRoot, ArtifactID: gateDoc, LatestVersion: 1, ApprovedVersion: new(1)})
	seedSlot(t, pool, record.Slot{Issue: reviveRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
}

// seedRoleClaim records the claim token of issue's role in state, suspended by by when it is
// suspended, as the claim store holds it, on the session ses-<role>: the reviewer's is the
// session its reviews' footer names (reviewBody).
func seedRoleClaim(t *testing.T, pool *pgxpool.Pool, token claim.Token, issue string, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) {
	t.Helper()
	session := "ses-" + string(role)
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, 'legion', $2, $3, $4, 1, $5, $6, $7, 0, 0, 0, 0, $8)`,
		string(token), reviveRoot, issue, string(role), session, "/sessions/"+session+".jsonl", string(state), string(by)); err != nil {
		t.Fatalf("seed the %s claim of %s: %v", role, issue, err)
	}
}

// seedIssuesArchitect records issue's architect claim in state, suspended by by when it is
// suspended.
func seedIssuesArchitect(t *testing.T, pool *pgxpool.Pool, issue string, state supervise.ClaimState, by supervise.SuspendSource) {
	t.Helper()
	token, err := claim.NewToken("legion", issue, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("the architect token of %s: %v", issue, err)
	}
	seedRoleClaim(t, pool, token, issue, claim.RoleArchitect, state, by)
}

// architectStartsOf is every start of issue's architect the outbox holds, oldest first.
func architectStartsOf(t *testing.T, pool *pgxpool.Pool, issue string) []record.SuperviseRequest {
	t.Helper()
	var starts []record.SuperviseRequest
	for _, start := range superviseRowsOf(t, pool, issue, "start") {
		if start.Role == claim.RoleArchitect {
			starts = append(starts, start)
		}
	}
	return starts
}

// plainArchitectStart is the start of a child's architect in reviveRoot's tree at generation
// (childArchitectStart), with no phase, task, entry, slot mark or kept decisions.
func plainArchitectStart(generation uint64) record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Tree: reviveRoot, Role: claim.RoleArchitect, Generation: generation}
}

// reviveMove is one transition that starts a phase of an issue: the phase it moves the issue from
// and to, the rows the issue needs to stand at from (seed), and the fact that moves it (apply).
type reviveMove struct {
	name     string
	from, to phase.Phase
	seed     func(t *testing.T, pool *pgxpool.Pool, key string)
	apply    func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string)
}

// plannerCompletes is the planner's completion of planning, which moves an issue with no plan rows,
// as these are, to implementing and starts its implementer.
var plannerCompletes = reviveMove{
	name: "the planner's completion", from: phase.Planning, to: phase.Implementing,
	seed: func(t *testing.T, pool *pgxpool.Pool, key string) {
		t.Helper()
		seedPhase(t, pool, record.PhaseRow{Issue: key, Role: claim.RolePlanner, Claim: "planner-claim"})
	},
	apply: func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
		t.Helper()
		if result := applyFacts(t, pool, engine)("planned-"+key, intake.HandoffComplete{Generation: 1, Issue: key, Role: claim.RolePlanner,
			Claim: "planner-claim", Summary: "the plan", HandoffRecord: "plan-" + key, HandoffVersion: 1}); result.Refusal != nil {
			t.Fatalf("the planner's completion of %s = %+v, want it recorded", key, result.Refusal)
		}
	},
}

// reviewerApproves is the review App's approval of the head the issue's pull request stands at,
// its checks green and GitHub reporting it clean, which moves the issue to accepting, whose
// acceptance on an issue with no plan rows is the architect's, so no worker is started. The
// reviewer's claim is seeded under the
// token the daemon starts the reviewer under, on the session ses-reviewer (seedRoleClaim), which
// the approval's footer names (reviewBody): a review of the review App decides its round only when
// its footer names the session that claim records (byReviewer).
var reviewerApproves = reviveMove{
	name: "the reviewer's approval", from: phase.Reviewing, to: phase.Accepting,
	seed: func(t *testing.T, pool *pgxpool.Pool, key string) {
		t.Helper()
		reviewer, err := claim.NewToken("legion", key, claim.RoleReviewer)
		if err != nil {
			t.Fatalf("the reviewer token of %s: %v", key, err)
		}
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: 43,
			Branch: "legion/" + key, HeadSHA: headOne, CheckedHead: headOne, CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}})
		seedPhase(t, pool, record.PhaseRow{Issue: key, Role: claim.RoleReviewer, Claim: reviewer})
		seedPhase(t, pool, record.PhaseRow{Issue: key, Role: claim.RoleImplementer, Claim: "implement-claim"})
		seedRoleClaim(t, pool, reviewer, key, claim.RoleReviewer, supervise.StateWorking, "")
	},
	apply: func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
		t.Helper()
		applyFacts(t, pool, engine)("approved-"+key, intake.PullRequestReview{Repo: "sjawhar/legion", Number: 43, State: "approved",
			CommitID: headOne, Author: testReviewApp, Body: reviewBody("ship it", headOne)})
	},
}

// moved applies move to the issue key, which stands at move.from with its rows seeded, and fails
// the test unless the issue then stands at move.to.
func (move reviveMove) moved(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	move.apply(t, pool, engine, key)
	if got := phaseOf(t, pool, key); got != move.to {
		t.Fatalf("%s left %s in %s, want %s", move.name, key, got, move.to)
	}
}

// A child's own architect whose claim has failed starts again at the child's next phase start:
// every transition that starts a phase of the child, the move into accepting among them, whose
// role is the architect itself on a child with no plan rows. The start is plain (childArchitectStart),
// queued on the child in the transaction that moves it, and the outbox's start retries the failed
// claim with fresh budgets in its session, or in a new one once supervise's Machine.forgetSession
// has dropped that (this start asks for no new session). The engine reads the claim's state in that
// transaction and queues the start only when the claim has failed: a claim that runs, is
// suspended (by the daemon, or by the idle release, whose suspension keeps its role), has retired,
// or does not exist is no failed architect, and no start is queued for it. A root's architect is
// admission's, so a root's phase start revives none, even when that architect has failed. A start
// already queued is the start: a second transition while it waits queues none beside it, and so
// does the child's first phase start, which queues its architect's start whatever the claim's
// state.
func TestAChildsNextPhaseStartRestartsItsFailedArchitect(t *testing.T) {
	t.Parallel()
	for _, move := range []reviveMove{plannerCompletes, reviewerApproves} {
		for _, tc := range []struct {
			name   string
			state  supervise.ClaimState
			by     supervise.SuspendSource
			starts int
		}{
			{name: "its architect failed", state: supervise.StateFailed, starts: 1},
			{name: "its architect ready", state: supervise.StateReady},
			{name: "its architect suspended with it", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon},
			{name: "its architect released by the idle rule", state: supervise.StateSuspended, by: supervise.SuspendedByReleased},
			{name: "its architect retired", state: supervise.StateRetired},
			{name: "its architect never started"},
		} {
			t.Run(move.name+", "+tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				seedReviveTree(t, pool, phase.Implementing)
				seedIssue(t, pool, reviveIssue(reviveChild, move.from, PhaseStatus(move.from)))
				move.seed(t, pool, reviveChild)
				if tc.state != "" {
					seedIssuesArchitect(t, pool, reviveChild, tc.state, tc.by)
				}

				move.moved(t, pool, testEngine(config.DesignGateRootIssues, nil), reviveChild)
				got := architectStartsOf(t, pool, reviveChild)
				if len(got) != tc.starts {
					t.Fatalf("the child's architect starts after %s = %#v; want %d", move.name, got, tc.starts)
				}
				for _, start := range got {
					if want := plainArchitectStart(1); !reflect.DeepEqual(start, want) {
						t.Fatalf("the child's architect start = %#v; want the plain start %#v, with no phase, task, entry or slot mark", start, want)
					}
				}
				if roots := architectStartsOf(t, pool, reviveRoot); len(roots) != 0 {
					t.Fatalf("the child's move queued the root's architect starts %#v; want none", roots)
				}
			})
		}
	}

	t.Run("a root's phase start", func(t *testing.T) {
		pool := migratedPool(t)
		seedReviveTree(t, pool, phase.Planning)
		plannerCompletes.seed(t, pool, reviveRoot)
		seedIssuesArchitect(t, pool, reviveRoot, supervise.StateFailed, "")

		plannerCompletes.moved(t, pool, testEngine(config.DesignGateRootIssues, nil), reviveRoot)
		if got := architectStartsOf(t, pool, reviveRoot); len(got) != 0 {
			t.Fatalf("the root's move into implementing queued its architect starts %#v; want none: admission starts a root's architect", got)
		}
	})

	t.Run("a second phase start while the first's architect start is queued", func(t *testing.T) {
		pool := migratedPool(t)
		seedReviveTree(t, pool, phase.Implementing)
		seedIssue(t, pool, reviveIssue(reviveChild, phase.Planning, PhaseStatus(phase.Planning)))
		plannerCompletes.seed(t, pool, reviveChild)
		seedIssuesArchitect(t, pool, reviveChild, supervise.StateFailed, "")
		engine := testEngine(config.DesignGateRootIssues, nil)

		plannerCompletes.moved(t, pool, engine, reviveChild)
		if got := architectStartsOf(t, pool, reviveChild); len(got) != 1 {
			t.Fatalf("the child's architect starts after the planner's completion = %#v; want one", got)
		}
		if result := applyFacts(t, pool, engine)("moved-back", intake.BackwardMove{Issue: reviveChild, Requester: claim.RoleImplementer,
			To: phase.Planning, Reason: "the plan leaves out the migration"}); result.Refusal != nil {
			t.Fatalf("the implementer's move back = %+v, want it applied", result.Refusal)
		}
		if got := phaseOf(t, pool, reviveChild); got != phase.Planning {
			t.Fatalf("the child = %s after the implementer's move back, want planning", got)
		}
		if got, want := architectStartsOf(t, pool, reviveChild), []record.SuperviseRequest{plainArchitectStart(1)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the child's architect starts after its second phase start = %#v; want the one queued start %#v", got, want)
		}
	})

	t.Run("a child's first phase start", func(t *testing.T) {
		pool := migratedPool(t)
		seedHeldTree(t, pool, record.DesignGate{Issue: reviveRoot, ArtifactID: gateDoc, LatestVersion: 3, ApprovedVersion: new(3)})
		seedAdmittedChild(t, pool, reviveChild)
		seedIssuesArchitect(t, pool, reviveChild, supervise.StateFailed, "")

		admissionPass(t, pool, testEngine(config.DesignGateRootIssues, nil))
		if got := phaseOf(t, pool, reviveChild); got != phase.Planning {
			t.Fatalf("the child = %s after the line reached it, want planning", got)
		}
		if got, want := architectStartsOf(t, pool, reviveChild), []record.SuperviseRequest{plainArchitectStart(4)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the child's architect starts after its first phase start = %#v; want the one start %#v", got, want)
		}
	})
}
