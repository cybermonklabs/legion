package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// everyClaimRow is one supervise row the walk over every claim of an issue wrote: its op, the role
// it names and the claim it names.
type everyClaimRow struct {
	op, role, claim string
}

// everyClaimRows is every supervise row of issue, oldest first.
func everyClaimRows(t *testing.T, pool *pgxpool.Pool, issue string) []everyClaimRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload->>'op', payload->>'role', coalesce(payload->>'claim', '')
		from outbox where issue = $1 and kind = 'supervise' order by id`, issue)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []everyClaimRow
	for rows.Next() {
		var row everyClaimRow
		if err := rows.Scan(&row.op, &row.role, &row.claim); err != nil {
			t.Fatal(err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

// seedRoleClaims stores, for each issue of keys as the record holds it, an idle claim of each role
// of Legion, in its tree, as the supervisor stores the claim of each role it ran there: what a walk
// over every claim of an issue reads (record.Store.IssueClaims), which writes a row naming a claim
// only for a claim the issue holds, and a row by the role alone for a role whose worker claim is
// none of those (claimTargets).
func seedRoleClaims(t *testing.T, pool *pgxpool.Pool, keys ...string) {
	t.Helper()
	for _, key := range keys {
		var dispatchProject, tree string
		var generation int64
		if err := pool.QueryRow(t.Context(), `select project, tree, generation from issues where key = $1`, key).Scan(&dispatchProject, &tree, &generation); err != nil {
			t.Fatalf("read %s for its claims: %v", key, err)
		}
		project, err := claim.ProjectToken(dispatchProject)
		if err != nil {
			t.Fatal(err)
		}
		for _, role := range claim.Roles {
			token, err := claim.NewToken(project, key, role)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
				state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
				values ($1, $2, $3, $4, $5, $6, '', '', 'idle', 0, 0, 0, 0) on conflict (token) do nothing`,
				string(token), project, tree, key, string(role), generation); err != nil {
				t.Fatalf("store the %s's claim of %s: %v", role, key, err)
			}
		}
	}
}

// An issue can hold two stored claims of one role: key a's agent, `…-implementer`, works the
// tester's step of version 2 (`implementer:a tester:a reviewer`) in the claim it started as
// implementer, and the fresh tester of version 1, `…-tester`, which ran and was stopped, still
// holds its own. A walk over every claim of the issue writes one row for each stored claim, each
// naming it with its stored role, so each gets its close; rows by role reached the claim of the
// worker on the tester's step twice and left `…-tester` with none. Each role whose step's claim is
// none of those gets one row naming no claim, which reaches a claim that comes up after the walk.
// A walk that leaves out the production check's role leaves out the claim that the check's own
// close, which names that role, reaches: the claim of the worker on the role's step, by name and
// by role.
func TestEveryClaimWritesARowForEachStoredClaim(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	store := record.NewStore()
	engine := New(store, Config{Project: "LEGION"}, nil)
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Title: "Workflow", Tree: "LEGION-208", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U", LastDispatchSeq: 1}
	const keyed, fresh = "legion-legion-legion-208-implementer", "legion-legion-legion-208-tester"
	for _, seed := range []struct{ token, state string }{{keyed, "idle"}, {fresh, "suspended"}} {
		if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', $2, $3, 'tester', 1, '', '', $4, 0, 0, 0, 0)`, seed.token, issue.Tree, issue.Key, seed.state); err != nil {
			t.Fatalf("seed the claim %s: %v", seed.token, err)
		}
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := store.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		return store.PutPhase(ctx, tx, record.PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: keyed, StepTask: "a", StepPlace: 2, Entry: 1})
	}); err != nil {
		t.Fatalf("write the tester's phases row: %v", err)
	}
	walk := func(op record.SuperviseOp, except claim.Role) []everyClaimRow {
		t.Helper()
		if _, err := pool.Exec(ctx, `delete from outbox where issue = $1`, issue.Key); err != nil {
			t.Fatal(err)
		}
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return engine.everyClaim(ctx, tx, issue, op, "the issue is done", except)
		}); err != nil {
			t.Fatalf("walk every claim of %s: %v", issue.Key, err)
		}
		return everyClaimRows(t, pool, issue.Key)
	}

	others := func(op string, roles ...claim.Role) []everyClaimRow {
		var rows []everyClaimRow
		for _, role := range roles {
			rows = append(rows, everyClaimRow{op, string(role), ""})
		}
		return rows
	}
	want := append([]everyClaimRow{{"issue_close", "tester", keyed}, {"issue_close", "tester", fresh}},
		others("issue_close", claim.RoleArchitect, claim.RolePlanner, claim.RoleReviewer, claim.RoleMerger)...)
	if got := walk("issue_close", ""); !slices.Equal(got, want) {
		t.Fatalf("the issue's close wrote %+v, want %+v: one row for each stored claim, naming it, and one for each other role", got, want)
	}
	want = append([]everyClaimRow{{"suspend", "tester", fresh}},
		others("suspend", claim.RoleArchitect, claim.RolePlanner, claim.RoleReviewer, claim.RoleMerger)...)
	if got := walk("suspend", claim.RoleTester); !slices.Equal(got, want) {
		t.Fatalf("the walk that leaves out the check's role wrote %+v, want %+v: the claim that role's close reaches left out, by name and by role", got, want)
	}
}
