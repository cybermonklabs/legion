package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// seedSubArchitectTree records a live tree with root LEGION-1, its child LEGION-2 at generation
// 2, LEGION-2's own child LEGION-3, and LEGION-2's sibling LEGION-4, each in the workflow. edit
// changes LEGION-2's record and root the root's before they are written.
func seedSubArchitectTree(t *testing.T, pool *pgxpool.Pool, root, edit func(*record.Issue)) {
	t.Helper()
	issue := func(key, parent string, at phase.Phase, generation uint64, status, rank string) record.Issue {
		recorded := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: "LEGION", Title: key,
			Phase: at, Generation: generation, Status: status, Rank: rank}
		if parent != "" {
			recorded.Parent = &parent
		}
		return recorded
	}
	rootIssue := issue("LEGION-1", "", phase.Implementing, 1, "in_progress", "U")
	child := issue("LEGION-2", "LEGION-1", phase.Planning, 2, "in_progress", "V")
	if root != nil {
		root(&rootIssue)
	}
	if edit != nil {
		edit(&child)
	}
	for _, recorded := range []record.Issue{rootIssue, child,
		issue("LEGION-3", "LEGION-2", phase.Admitted, 2, "todo", "W"),
		issue("LEGION-4", "LEGION-1", phase.Implementing, 1, "in_progress", "X"),
	} {
		seedIssue(t, pool, recorded)
	}
}

// catchUpIssues is the issue each queued catch-up row is queued on, in order.
func catchUpIssues(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select issue from outbox where kind = 'notice' and payload->>'kind' = 'catch-up' order by id")
	if err != nil {
		t.Fatalf("read the catch-up rows: %v", err)
	}
	issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the catch-up rows: %v", err)
	}
	return issues
}

// seedArchitectClaimIn records the architect claim of issue, a member of tree at generation, in
// state, suspended by by when it is suspended, as the claim store holds it.
func seedArchitectClaimIn(t *testing.T, pool *pgxpool.Pool, tree, issue string, generation uint64, state supervise.ClaimState, by supervise.SuspendSource) {
	t.Helper()
	token, err := claim.NewToken("legion", issue, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, 'legion', $2, $3, 'architect', $4, 'ses', '/sessions/ses.jsonl', $5, 0, 0, 0, 0, $6)`,
		string(token), tree, issue, int64(generation), string(state), string(by)); err != nil {
		t.Fatalf("seed the architect claim of %s: %v", issue, err)
	}
}

// A child's own architect is told, at each launch's ready, the child and every issue under it:
// the catch-up is queued on the child, states the child's generation and the launch, names no
// gate (a child registers none) and no decision (a decision kept while nobody could hear it is the
// tree's, and stays its root architect's), and its reason names the child. Its parent and its
// sibling are not its own. A relaunch drops the catch-up its earlier launch never had delivered,
// so the agent is given one, the current one. A child that is out of the workflow or done, and a
// child of a lingering tree, is told nothing.
func TestASubArchitectsReadyIsToldItsOwnSubtree(t *testing.T) {
	const told = "gen=2 launch=%d gates= issues=LEGION-2,LEGION-3 reason=the issues under LEGION-2, which you own, at generation 2 as the daemon records them now; start or resume your issue as your role says"
	t.Run("a live child", func(t *testing.T) {
		ctx := context.Background()
		pool := migratedPool(t)
		seedSubArchitectTree(t, pool, nil, nil)
		// A decision kept for the tree is the root architect's: the child's catch-up neither
		// names it nor claims it.
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return record.NewStore().AddParkedDecision(ctx, tx, record.ParkedDecision{Issue: "LEGION-1", Ref: "dispatch://LEGION-1/spec",
				Decision: record.DecisionDesignApproved, Actor: "user:alice", At: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)})
		}); err != nil {
			t.Fatalf("keep a decision: %v", err)
		}
		engine := testEngine(config.DesignGateRootIssues, nil)

		claimReady(t, pool, engine, "LEGION-2", claim.RoleArchitect, 1)
		if got, want := catchUps(t, pool), []string{fmt.Sprintf(told, 1)}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("after the sub-architect's first ready, catch-ups %v; want %v", got, want)
		}
		if got := catchUpIssues(t, pool); fmt.Sprint(got) != "[LEGION-2]" {
			t.Fatalf("the catch-up is queued on %v; want on LEGION-2, the child", got)
		}
		got := lastCatchUp(t, pool)
		if got.Gates == nil || len(got.Gates) != 0 || got.Decided != "" || got.Kept != nil || got.HandedOver || got.Carried {
			t.Fatalf("the sub-architect's catch-up states gates %#v, decided %q, kept %#v, handed over %t, carried %t; want an empty gate list and nothing else",
				got.Gates, got.Decided, got.Kept, got.HandedOver, got.Carried)
		}
		for _, listed := range got.Issues {
			if listed.Key == "LEGION-3" && (listed.Parent != "LEGION-2" || listed.Phase != phase.Admitted || listed.Status != "todo") {
				t.Fatalf("the grandchild is listed as %#v; want under LEGION-2, admitted, todo", listed)
			}
		}

		first := catchUpRows(t, pool)
		claimReady(t, pool, engine, "LEGION-2", claim.RoleArchitect, 2)
		if got, want := catchUps(t, pool), []string{fmt.Sprintf(told, 2)}; fmt.Sprint(got) != fmt.Sprint(want) || fmt.Sprint(catchUpRows(t, pool)) == fmt.Sprint(first) {
			t.Fatalf("after a relaunch, catch-ups %v in rows %v (before, %v); want the relaunch's own, alone", got, catchUpRows(t, pool), first)
		}

		// The root's architect still hears the decision the child's catch-up left alone.
		claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 1)
		if got := lastCatchUp(t, pool); got.Kept == nil || len(got.Kept.Decisions) != 1 {
			t.Fatalf("the root architect's catch-up keeps %#v; want the one decision kept for the tree", got.Kept)
		}
	})

	until := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		root  func(*record.Issue)
		child func(*record.Issue)
	}{
		{"a child out of the workflow", nil, func(child *record.Issue) { child.Status = "backlog" }},
		{"a child done", nil, func(child *record.Issue) { child.Phase, child.Status = phase.Done, "done" }},
		{"a child whose work is done before its status says so", nil, func(child *record.Issue) { child.Phase = phase.Done }},
		{"a child of a lingering tree", func(root *record.Issue) { root.Phase, root.Status, root.LingerUntil = phase.Done, "done", &until }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedSubArchitectTree(t, pool, tc.root, tc.child)
			claimReady(t, pool, testEngine(config.DesignGateRootIssues, nil), "LEGION-2", claim.RoleArchitect, 1)
			if got := catchUps(t, pool); len(got) != 0 {
				t.Fatalf("catch-ups %v; want none", got)
			}
		})
	}

	t.Run("an issue the daemon has not recorded", func(t *testing.T) {
		pool := migratedPool(t)
		seedSubArchitectTree(t, pool, nil, nil)
		claimReady(t, pool, testEngine(config.DesignGateRootIssues, nil), "LEGION-9", claim.RoleArchitect, 1)
		if got := catchUps(t, pool); len(got) != 0 {
			t.Fatalf("catch-ups %v; want none", got)
		}
	})
}

// An issue in accepting waits for the acceptance of the architect that owns it: the one the
// phase-finished notice that moved it there reaches, which is its own architect while that claim
// holds its role, and else the nearest architect above it that does. So a root's catch-up names
// no accepting child whose own architect holds its role — running, or released by the idle rule
// and woken by the next wake — and that architect's catch-up names it instead; and the root's
// does name a child directly under it whose architect has failed, retired or been suspended, or
// never started, since that child's acceptance then walks up to the root's.
func TestARootsCatchUpNamesOnlyTheAcceptancesItsArchitectOwns(t *testing.T) {
	const root = "the tree of LEGION-1 at generation 1 as the daemon records it now; start or resume it as your role says"
	const child = "the issues under LEGION-2, which you own, at generation 2 as the daemon records them now; start or resume your issue as your role says"
	awaiting := func(base, keys string) string {
		return base + "; in accepting, waiting for your acceptance, which no worker gives: " + keys + " (read the evidence, then sign_off or request_backward_move)"
	}
	accepting := func(child *record.Issue) { child.Phase, child.Status = phase.Accepting, "needs_review" }
	for _, tc := range []struct {
		name        string
		state       supervise.ClaimState
		by          supervise.SuspendSource
		rootReason  string
		childReason string
	}{
		{name: "its architect working", state: supervise.StateWorking, rootReason: root, childReason: awaiting(child, "LEGION-2")},
		{name: "its architect released by the idle rule", state: supervise.StateSuspended, by: supervise.SuspendedByReleased, rootReason: root, childReason: awaiting(child, "LEGION-2")},
		{name: "its architect failed", state: supervise.StateFailed, rootReason: awaiting(root, "LEGION-2"), childReason: child},
		{name: "its architect retired", state: supervise.StateRetired, rootReason: awaiting(root, "LEGION-2"), childReason: child},
		{name: "its architect suspended with it", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon, rootReason: awaiting(root, "LEGION-2"), childReason: child},
		{name: "its architect never started", rootReason: awaiting(root, "LEGION-2"), childReason: child},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedSubArchitectTree(t, pool, nil, accepting)
			seedArchitectClaimIn(t, pool, "LEGION-1", "LEGION-1", 1, supervise.StateWorking, "")
			if tc.state != "" {
				seedArchitectClaimIn(t, pool, "LEGION-1", "LEGION-2", 2, tc.state, tc.by)
			}
			engine := testEngine(config.DesignGateRootIssues, nil)
			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 1)
			want := fmt.Sprintf("gen=1 launch=1 gates=spec(gated=true,unregistered=true) issues=LEGION-1,LEGION-2,LEGION-3,LEGION-4 reason=%s", tc.rootReason)
			if got := catchUps(t, pool); len(got) != 1 || got[0] != want {
				t.Fatalf("the root's catch-up %v; want [%s]", got, want)
			}
			claimReady(t, pool, engine, "LEGION-2", claim.RoleArchitect, 1)
			want = "gen=2 launch=1 gates= issues=LEGION-2,LEGION-3 reason=" + tc.childReason
			if got := catchUps(t, pool); len(got) != 2 || got[1] != want {
				t.Fatalf("the catch-ups %v; want the child's second, [%s]", got, want)
			}
		})
	}
}
