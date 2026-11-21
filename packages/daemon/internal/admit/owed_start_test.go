package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// midPhaseStart is one supervise start row of an issue, with whether a lane may run it now
// (record.Store.DueRows, the predicate the outbox claims by).
type midPhaseStart struct {
	role      claim.Role
	task      string
	needsSlot bool
	claimable bool
}

// startsOf is every supervise start row issue holds, oldest first.
func startsOf(t *testing.T, pool *pgxpool.Pool, issue string) []midPhaseStart {
	t.Helper()
	var rows []midPhaseStart
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		due, err := record.NewStore().DueRows(ctx, tx, testProject, issue, []record.OutboxKind{record.OutboxKindSupervise}, fixedNow)
		if err != nil {
			t.Fatalf("list what a lane may run for %s: %v", issue, err)
		}
		runnable := make(map[int64]struct{}, len(due))
		for _, row := range due {
			runnable[row.ID] = struct{}{}
		}
		all, err := tx.Query(ctx, `select id, coalesce(payload->>'role', ''), coalesce(payload->>'task', ''),
			coalesce(payload->>'needsSlot', '') = 'true'
			from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
		if err != nil {
			t.Fatalf("list the starts of %s: %v", issue, err)
		}
		defer all.Close()
		for all.Next() {
			var id int64
			var row midPhaseStart
			if err := all.Scan(&id, &row.role, &row.task, &row.needsSlot); err != nil {
				t.Fatalf("scan a start of %s: %v", issue, err)
			}
			_, row.claimable = runnable[id]
			rows = append(rows, row)
		}
		if err := all.Err(); err != nil {
			t.Fatalf("read the starts of %s: %v", issue, err)
		}
	})
	return rows
}

// runBranchRows finishes the issue_branch row of each of issues, as that row's lane does once
// GitHub holds the branch: the starts queued behind it are held by it until it is gone.
func runBranchRows(t *testing.T, pool *pgxpool.Pool, issues ...string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		rows, err := tx.Query(ctx, `select id from outbox where kind = 'issue_branch' and issue = any($1) order by id`, issues)
		if err != nil {
			t.Fatalf("list the branch rows of %v: %v", issues, err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan a branch row: %v", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read the branch rows of %v: %v", issues, err)
		}
		if len(ids) != len(issues) {
			t.Fatalf("the branch rows of %v are %v, want one each: every member past admitted gets one", issues, ids)
		}
		for _, id := range ids {
			if err := record.NewStore().FinishOutbox(ctx, tx, id, ""); err != nil {
				t.Fatalf("finish the branch row %d: %v", id, err)
			}
		}
	})
}

// midPhaseChild records key in root's tree standing mid-phase at stands, with the phases row and
// the claim of the role that works it as the tree's close left it: suspended by the daemon's own
// suspend of every claim, since a close retires nothing of the record and only a worker's own
// handoff moves a phase on.
func midPhaseChild(t *testing.T, pool *pgxpool.Pool, root, key string, stands phase.Phase, status string) claim.Token {
	t.Helper()
	parent := root
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: key, Tree: root, Parent: &parent, Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: key, LastDispatchSeq: 1})
	role := phase.RoleFor(stands)
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		if err := record.NewStore().PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: role, Claim: token}); err != nil {
			t.Fatalf("seed the %s's row of %s: %v", role, key, err)
		}
		if _, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, $2, $3, $4, $5, 1, 'ses', '/sessions/ses.jsonl', 'suspended', 1, 0, 0, 0, 0, 'daemon')`,
			string(token), project, root, key, string(role)); err != nil {
			t.Fatalf("seed the %s of %s: %v", role, key, err)
		}
	})
	return token
}

// entryOf is the phase entry role's phases row of issue is on and the work that entry names.
func entryOf(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role) (int64, string) {
	t.Helper()
	var entry int64
	var task string
	if err := pool.QueryRow(context.Background(), `select entry, entry_task from phases where issue = $1 and role = $2`,
		issue, string(role)).Scan(&entry, &task); err != nil {
		t.Fatalf("read the %s's entry on %s: %v", role, issue, err)
	}
	return entry, task
}
