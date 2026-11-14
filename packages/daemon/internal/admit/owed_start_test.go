package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
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

// A3's eighth return, which is admission's own start rather than a transition: a re-admitted
// tree whose two children stand mid-phase. The child short of its review waits for a slot of
// its own, because the cap counts the issues whose change is still to write and the tree's one
// root slot no longer carries its children; the child past its review waits for none
// (record.KeepsSlot), so its start is claimable as soon as the branch row ahead of it is run.
//
// The promotion writes everything but the tasks in the fact's own transaction: the root's new
// generation and Dispatch status, each child's branch row, and a task-less start of each role of
// a member that holds a claim row, the durable record that the role has run (startMidPhaseMembers),
// so the sessions a member has are brought back and answer while its work waits. Each child of
// this fixture has run one role, the one whose phase it stands in, so no task-less start is
// written for either of them. Each child then has exactly one start, the phase worker's, which
// carries that phase's task, and nothing names its worker as a claim nothing will wake.
//
// Two assertions of the table in internal/workflow (owed_start_test.go) read differently here,
// and the rule is the re-admission's. The re-admission cleared the work each role's entry named
// with the rest of the generation (record.ClearTreeGeneration), so the promotion raises the
// entry and writes the resume task it hands the role as the work of that entry
// (workflow.StartPromotedWorker): the row names the entry the start carries, and no completion
// in flight is refused for an entry nothing ever delivered, since every earlier entry is below
// it. And the claim this start brings back is the suspended one the tree's close left: the
// promotion itself stops and resumes nothing, and the start row is what resumes it, when the
// child's slot comes.
func TestAReadmittedTreesMidPhaseChildrenEachWaitForTheirOwnSlot(t *testing.T) {
	const root, testing_, retro = "LEGION-208", "LEGION-209", "LEGION-211"
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Linger: time.Hour,
		Clock: func() time.Time { return fixedNow }}, nil)
	putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: "root", Tree: root, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", LastDispatchSeq: 1})
	tester := midPhaseChild(t, pool, root, testing_, phase.Testing, "in_progress")
	implementer := midPhaseChild(t, pool, root, retro, phase.Retro, "retro")

	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "A", HandedOver: handed}, engine)

	// The re-admission applied: the root carries its new generation and its Dispatch status write,
	// and each child stands in the phase its own worker left it in.
	if again := issue(t, pool, root); again.Generation != 2 || again.Phase != phase.Admitted || again.Status != "in_progress" {
		t.Fatalf("the re-admitted root = %#v, want generation 2 at admitted, in_progress", again)
	}
	var written string
	if err := pool.QueryRow(context.Background(), `select coalesce((select payload->>'status' from outbox
		where kind = 'dispatch_status' and issue = $1 order by id desc limit 1), '')`, root).Scan(&written); err != nil {
		t.Fatalf("read the root's Dispatch status write: %v", err)
	}
	if written != "in_progress" {
		t.Fatalf("the root's Dispatch status write says %q, want in_progress", written)
	}
	// The tree's one slot is the root's, so neither child holds one.
	if !holdsSlot(t, pool, root) || holdsSlot(t, pool, testing_) || holdsSlot(t, pool, retro) {
		t.Fatalf("the slots are %#v, want the root's alone while the cap is one", slots(t, pool))
	}
	// Every start of an issue waits behind that issue's own branch row, which the promotion
	// queued first so that no role starts before its branch exists on GitHub (store.go's
	// outboxInOrder). Running those rows, as their lane does, leaves the slot as the one thing
	// still holding a task back.
	runBranchRows(t, pool, testing_, retro)
	for child, want := range map[string]struct {
		role  claim.Role
		phase phase.Phase
		// waits says the child's start waits for an admission slot, which the phase it stands
		// in decides (record.PhaseKeepsSlot, read by workflow.StartOrOwe): the tester's start
		// waits, and the retro implementer's does not, since an issue past its review holds no
		// row for that start to wait for.
		waits bool
	}{testing_: {role: claim.RoleTester, phase: phase.Testing, waits: true}, retro: {role: claim.RoleImplementer, phase: phase.Retro}} {
		// The re-admission rewrites no phase: the phase a member's record carries is the phase it
		// stands in, since the merge is what ends merging and no phase of this workflow waits for
		// one.
		if stands := issue(t, pool, child); stands.Phase != want.phase || stands.Generation != 1 {
			t.Fatalf("%s stands at %s on generation %d, want its own phase on its own generation", child, stands.Phase, stands.Generation)
		}
		entry, work := entryOf(t, pool, child, want.role)
		if entry != 1 || work == "" {
			t.Fatalf("the %s's row of %s is at entry %d naming the work %q, want the first entry of this run carrying the task its start hands the role",
				want.role, child, entry, work)
		}
		starts := startsOf(t, pool, child)
		if len(starts) != 1 || starts[0].role != want.role || starts[0].task == "" {
			t.Fatalf("the starts of %s = %+v, want the %s one, carrying its resume task: the only role of it that has run is the one whose phase it stands in",
				child, starts, want.role)
		}
		if starts[0].needsSlot != want.waits || starts[0].claimable == want.waits {
			t.Fatalf("the start of %s = %+v, want it marked for a slot = %t and claimable = %t: a start waits for the row its phase keeps, and runs at once where its phase keeps none",
				child, starts[0], want.waits, !want.waits)
		}
	}
	for token, state := range claimStates(t, pool) {
		if state != string(supervise.StateSuspended) {
			t.Fatalf("the claim %s is %s, want it as the tree's close left it: the promotion resumes nothing itself, its start rows do", token, state)
		}
	}
	var pending int
	if err := pool.QueryRow(context.Background(), "select count(*) from pending_task_deliveries").Scan(&pending); err != nil {
		t.Fatalf("count the pending deliveries: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d deliveries are pending, want none: each child's task waits in the outbox", pending)
	}
	assertMidPhaseChildrenAreNotStranded(t, pool, root, map[string]claim.Token{testing_: tester, retro: implementer})

	// The slot the testing child takes delivers that child's one task. The retro child's start
	// waited for no row at all, so the slot of another issue neither frees it nor holds it.
	takeSlot(t, pool, testing_)
	for _, row := range startsOf(t, pool, testing_) {
		if !row.claimable {
			t.Fatalf("the start %+v of %s is still held, want its slot to have freed it", row, testing_)
		}
	}
}

// assertMidPhaseChildrenAreNotStranded is A3's fourth assertion for the re-admission: nothing
// names a mid-phase child's worker as a claim nothing will wake, whether it is live or suspended
// next, because the start it waits for is queued and StartFor counts a row the slot holds.
func assertMidPhaseChildrenAreNotStranded(t *testing.T, pool *pgxpool.Pool, root string, workers map[string]claim.Token) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	var claims []supervise.Claim
	for child, token := range workers {
		stands := issue(t, pool, child)
		c := supervise.Claim{Token: token, Project: project, Tree: root, Issue: child, Role: phase.RoleFor(stands.Phase),
			Generation: 1, State: supervise.StateSuspended}
		claims = append(claims, c)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		stranded, err := workflow.Stranded(context.Background(), tx, record.NewStore(), testProject, nil, claims, nil, time.Now().UTC())
		if err != nil {
			t.Fatalf("read the stranded claims: %v", err)
		}
		if len(stranded) != 0 {
			t.Fatalf("Stranded names %+v, want nothing: each start is queued, held for its issue's slot", stranded)
		}
	})
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
