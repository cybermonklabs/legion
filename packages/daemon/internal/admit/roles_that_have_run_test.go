package admit

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A re-admitted tree brings back the roles of its members that have run, and no others. The claim
// row is what says a role has run: a re-admission clears the generation's facts and never a claim
// (record.ClearTreeGeneration touches pull_requests, design_gates, phases and issues), and a
// claim's token carries no generation (claim.NewToken), so the row outlives the generation that
// wrote it — which is why every state here asserts that the rows it seeded are still there after
// the re-admission. A role that holds none has never run, and a session started for it would hold
// no transcript and no task, so nothing but a message could ever wake it: it is not started at all.
//
// The four states are the member mid-phase, the member in a phase with no worker of its own, the
// re-admitted root itself, and the member whose phase worker never ran.
//
// What the old behaviour was: the first two states' assertion that a role holding no claim row has
// no start row at all ("want no start row at all"). Before this rule, startMidPhaseMembers
// enqueued a task-less start for every role of every mid-phase member with no claim test at all,
// so each of those states found rows for the reviewer, the merger and the member's own
// sub-architect too. The third state passed then as it does now, because the pass over members
// already skips one standing at admitted, and the fourth exercises the owner's branch, which this
// rule leaves gated by workflow.StartFor alone.
func TestAReAdmissionStartsOnlyRolesThatHaveRun(t *testing.T) {
	const root, member = "LEGION-208", "LEGION-209"
	readmission := intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "A", HandedOver: handed}

	// seedTree records the tree a re-admission opens: the root its linger closed at generation 1,
	// and, where stands names a phase, one member of it left standing there by the close.
	seedTree := func(t *testing.T, pool *pgxpool.Pool, stands phase.Phase, status string) {
		t.Helper()
		putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: "root", Tree: root, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", LastDispatchSeq: 1})
		if stands == "" {
			return
		}
		parent := root
		putIssue(t, pool, record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: "member", Tree: root, Parent: &parent, Phase: stands, Generation: 1, Status: status, DispatchStatus: status,
			Rank: "B", LastDispatchSeq: 1})
	}

	// hasRun records each role of key as one that has run: the claim row the tree's close left
	// suspended, the durable record of the run before this generation.
	hasRun := func(t *testing.T, pool *pgxpool.Pool, key string, roles ...claim.Role) []claim.Token {
		t.Helper()
		tokens := make([]claim.Token, 0, len(roles))
		for _, role := range roles {
			tokens = append(tokens, putWorkerClaim(t, pool, root, key, role, 1, supervise.StateSuspended, supervise.SuspendedByDaemon))
		}
		return tokens
	}

	// survived is the assertion the rule itself reads: every claim row seeded before the
	// re-admission is still there, in the state the tree's close left it.
	survived := func(t *testing.T, pool *pgxpool.Pool, tokens []claim.Token) {
		t.Helper()
		states := claimStates(t, pool)
		for _, token := range tokens {
			state, kept := states[string(token)]
			if !kept || state != string(supervise.StateSuspended) {
				t.Fatalf("the claim %s reads %q, kept=%v after the re-admission; want the suspended row the close left, which no generation clears", token, state, kept)
			}
		}
	}

	// startRows is every start row of key by role: each state names at most one row per role, and
	// a role with no row at all.
	startRows := func(t *testing.T, pool *pgxpool.Pool, key string) map[claim.Role]midPhaseStart {
		t.Helper()
		rows := make(map[claim.Role]midPhaseStart)
		for _, row := range startsOf(t, pool, key) {
			if _, twice := rows[row.role]; twice {
				t.Fatalf("%s holds more than one start row for its %s: %+v", key, row.role, startsOf(t, pool, key))
			}
			rows[row.role] = row
		}
		return rows
	}

	// assertBroughtBack is the shape of a start that brings a role's session back: it carries no
	// task, since the role is asked nothing, and waits for no slot, since it puts nobody to work,
	// so a lane may run it at once.
	assertBroughtBack := func(t *testing.T, rows map[claim.Role]midPhaseStart, roles ...claim.Role) {
		t.Helper()
		for _, role := range roles {
			row, started := rows[role]
			if !started || row.task != "" || row.needsSlot || !row.claimable {
				t.Fatalf("the %s of %s was started with %+v (started=%v), want one start carrying no task and claimable at once", role, member, row, started)
			}
		}
	}

	// assertNeverStarted is the rule: a role that holds no claim row has never run, and a
	// re-admission starts it with nothing.
	assertNeverStarted := func(t *testing.T, rows map[claim.Role]midPhaseStart, roles ...claim.Role) {
		t.Helper()
		for _, role := range roles {
			if row, started := rows[role]; started {
				t.Fatalf("the %s of %s was started with %+v, want no start row at all: it holds no claim row, so it has never run", role, member, row)
			}
		}
	}

	// assertCarriesThePhase is the phase worker's own start: the one row that carries the resume
	// task and waits for the member's admission slot, which the root holds while the cap is one.
	assertCarriesThePhase := func(t *testing.T, rows map[claim.Role]midPhaseStart, role claim.Role) {
		t.Helper()
		row, started := rows[role]
		if !started || row.task == "" || !row.needsSlot || row.claimable {
			t.Fatalf("the %s of %s was started with %+v (started=%v), want its resume task on the one row that waits for the slot", role, member, row, started)
		}
	}

	// assertArchitectStarted is the tree architect's start as the promotion has always made it:
	// the root's one start row, which names no phase and carries no task.
	assertArchitectStarted := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		rows := startRows(t, pool, root)
		architect, started := rows[claim.RoleArchitect]
		if len(rows) != 1 || !started || architect.task != "" || architect.needsSlot {
			t.Fatalf("the re-admitted root's starts are %+v, want the architect's alone, carrying no task and waiting for no slot", rows)
		}
	}

	newEngine := func() *workflow.Engine {
		return workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
			Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	}

	t.Run("a mid-phase member brings back the roles that have run", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		seedTree(t, pool, phase.Testing, "in_progress")
		// The member has run three of its six roles: the tester, whose phase it stands in, and two
		// that have handed their phases on. The reviewer, the merger and its own sub-architect
		// have never run.
		tokens := hasRun(t, pool, member, claim.RoleTester, claim.RolePlanner, claim.RoleImplementer)

		apply(t, pool, admission, "readmit", readmission, newEngine())

		// Every start of an issue waits behind that issue's branch row until the branch exists on
		// GitHub (store.go's outboxInOrder); running it, as its lane does, leaves the slot as the
		// one thing still holding a start back.
		runBranchRows(t, pool, member)
		rows := startRows(t, pool, member)
		assertBroughtBack(t, rows, claim.RolePlanner, claim.RoleImplementer)
		assertNeverStarted(t, rows, claim.RoleReviewer, claim.RoleMerger, claim.RoleArchitect)
		assertCarriesThePhase(t, rows, claim.RoleTester)
		assertArchitectStarted(t, pool)
		survived(t, pool, tokens)
	})

	t.Run("a member in a phase with no worker of its own", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		// Accepting waits on the tree's architect, which the promotion starts, so the member's own
		// phase has no worker and no row of its own waits for a slot. The roles that have run come
		// back all the same, each start claimable at once, because such a start waits for nothing.
		seedTree(t, pool, phase.Accepting, "needs_review")
		tokens := hasRun(t, pool, member, claim.RoleTester, claim.RolePlanner, claim.RoleImplementer)

		apply(t, pool, admission, "readmit", readmission, newEngine())

		runBranchRows(t, pool, member)
		rows := startRows(t, pool, member)
		assertBroughtBack(t, rows, claim.RoleTester, claim.RolePlanner, claim.RoleImplementer)
		assertNeverStarted(t, rows, claim.RoleReviewer, claim.RoleMerger, claim.RoleArchitect)
		if len(rows) != 3 {
			t.Fatalf("the member's starts in accepting are %+v, want the three roles that have run and nothing waiting for a slot", rows)
		}
		assertArchitectStarted(t, pool)
		survived(t, pool, tokens)
	})

	t.Run("the re-admitted root standing at admitted", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		seedTree(t, pool, "", "")
		// Every role of the root has run under the generation before, and the re-admission still
		// starts none of them: a root standing at admitted has run nothing in this generation, and
		// its first phase is the waiting line's to start.
		tokens := hasRun(t, pool, root, claim.Roles...)

		apply(t, pool, admission, "readmit", readmission, newEngine())

		if again := issue(t, pool, root); again.Generation != 2 || again.Phase != phase.Admitted {
			t.Fatalf("the re-admitted root = %#v, want generation 2 standing at admitted", again)
		}
		assertArchitectStarted(t, pool)
		survived(t, pool, tokens)
	})

	t.Run("a member whose phase worker has never run", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		seedTree(t, pool, phase.Testing, "in_progress")
		// The member's tester holds no claim row, which is the ordinary case of a phase whose
		// worker never started before the tree left the workflow. Its start is the member's work
		// rather than a session brought back, so the claim row decides nothing for it: the owner's
		// branch is gated by workflow.StartFor alone, and the phase runs again. The planner, which
		// has run, comes back beside it. Which roles hold no row at all is the first state's
		// assertion, not this one's.
		tokens := hasRun(t, pool, member, claim.RolePlanner)

		apply(t, pool, admission, "readmit", readmission, newEngine())

		runBranchRows(t, pool, member)
		rows := startRows(t, pool, member)
		assertCarriesThePhase(t, rows, claim.RoleTester)
		assertBroughtBack(t, rows, claim.RolePlanner)
		assertArchitectStarted(t, pool)
		survived(t, pool, tokens)
	})
}
