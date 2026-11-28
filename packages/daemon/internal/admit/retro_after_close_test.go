package admit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// retroAdmission is the engine and the admission the tests of the retro after a close drive, on
// the clock now points at: the default steps with the design gate off, a one-hour linger and the
// tester as the role of the production check.
func retroAdmission(now *time.Time) (*workflow.Engine, *Admission) {
	clock := func() time.Time { return *now }
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateOff.Steps(),
		ReviewRoundCap: 3, Linger: time.Hour, ProductionCheckRole: claim.RoleTester, Clock: clock}, nil)
	admission := New(record.NewStore(), engine, 3, testProject, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admission.now = clock
	return engine, admission
}

// startFirstPhase starts key's first phase as the admission's promotion does, in a fact of its
// own, inside the fact's transaction and settle (Admission.OpenFact and SettleFact are the
// engine's): the start of the issue's run, and of the run of each issue above it.
func startFirstPhase(t *testing.T, pool *pgxpool.Pool, engine *workflow.Engine, admission *Admission, key string) {
	t.Helper()
	stored := issue(t, pool, key)
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx = admission.OpenFact(ctx)
		if err := engine.StartFirstPhase(ctx, tx, stored, nil); err != nil {
			return err
		}
		return admission.SettleFact(ctx, tx)
	}); err != nil {
		t.Fatalf("start the first phase of %s: %v", key, err)
	}
	if got := issue(t, pool, key); got.Phase != phase.Planning {
		t.Fatalf("%s after its first phase started stands in %s, want planning", key, got.Phase)
	}
}

// roleToken is the claim token of role on key in the test project.
func roleToken(t *testing.T, key string, role claim.Role) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", testProject, err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatalf("%s token of %s: %v", role, key, err)
	}
	return token
}

// seedWokenArchitect records root's architect claim working in its own tree at generation 1, as
// the start of a retro's wake leaves it once it has relaunched the architect the root's close
// suspended: nothing of the wake is queued any longer.
func seedWokenArchitect(t *testing.T, pool *pgxpool.Pool, root string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		suspended_by, serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'legion', $2, $2, 'architect', 1, 'ses-architect', '', 'working', '', 1, 0, 0, 0, 0)`,
		string(roleToken(t, root, claim.RoleArchitect)), root); err != nil {
		t.Fatalf("seed the woken architect of %s: %v", root, err)
	}
}

// outboxMark is the id of the newest outbox row, 0 for none: every row a later fact writes is
// newer.
func outboxMark(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from outbox`).Scan(&id); err != nil {
		t.Fatalf("read the newest outbox row: %v", err)
	}
	return id
}

// architectSuspendsSince counts the supervise rows newer than mark that suspend key's architect.
func architectSuspendsSince(t *testing.T, pool *pgxpool.Pool, key string, mark int64) int {
	t.Helper()
	var suspends int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and id > $2 and kind = $3
		and payload->>'op' = 'suspend' and payload->>'role' = 'architect'`, key, mark, string(record.OutboxKindSupervise)).Scan(&suspends); err != nil {
		t.Fatalf("count the suspends of %s's architect: %v", key, err)
	}
	return suspends
}

// retrosEndedRow is a linger_close row that names the end of a tree's last retro after a close
// (record.LingerClose with RetrosEnded), and when it is due.
type retrosEndedRow struct {
	due   time.Time
	close record.LingerClose
}

// retrosEndedRowsSince is key's linger_close rows newer than mark that name the end of its tree's
// last retro after a close, oldest first: the stop or the close the workflow queues for the hour
// after that end.
func retrosEndedRowsSince(t *testing.T, pool *pgxpool.Pool, key string, mark int64) []retrosEndedRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select next_at, payload from outbox where issue = $1 and id > $2 and kind = $3 order by id`,
		key, mark, string(record.OutboxKindLingerClose))
	if err != nil {
		t.Fatalf("read the linger_close rows of %s: %v", key, err)
	}
	defer rows.Close()
	var got []retrosEndedRow
	for rows.Next() {
		var row retrosEndedRow
		var payload []byte
		if err := rows.Scan(&row.due, &payload); err != nil {
			t.Fatalf("scan a linger_close row of %s: %v", key, err)
		}
		if err := json.Unmarshal(payload, &row.close); err != nil {
			t.Fatalf("decode the linger_close row %s of %s: %v", payload, key, err)
		}
		if row.close.RetrosEnded != nil {
			got = append(got, row)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the linger_close rows of %s: %v", key, err)
	}
	return got
}

// retroEnd is how the retro after the close of key's run generation ended, as run_retros holds it:
// when (nil while it is open), with what outcome, by what, and in which tree the retro ran.
func retroEnd(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) (ended *time.Time, outcome record.RetroOutcome, endedBy record.RetroEndCause, tree string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `select ended_at, outcome, ended_by, tree from run_retros
		where issue = $1 and generation = $2 and opened_at is not null`, key, int64(generation)).Scan(&ended, &outcome, &endedBy, &tree); err != nil {
		t.Fatalf("read the retro after the close of %s run %d: %v", key, generation, err)
	}
	return ended, outcome, endedBy, tree
}

// A person's todo on a closed root while its tree lingers re-admits the root at its next
// generation (readmit), and the retro Legion runs after the root's close, still open, ends there
// (P10, the readmit half of s7): its step row ends stopped with no rating, as a child's todo ends
// it (the workflow's TestATodoOnAClosedIssueEndsItsOpenRetro), and the run's record ends stopped
// by the todo. The root's run started at its first phase, which the admission's own start of it
// (workflow.Engine.StartFirstPhase) began, and its close owed that run its retro. The re-admitted
// root stands at admitted at generation 2 with its linger cleared, as every re-admission leaves it.
//
// The retro is its tree's last, and a retro's wake has brought the root architect back, so its end
// would stop that architect and queue its stop for the hour after, were the tree still read as
// lingering (review round 4 of #5630). readmit ends the retro once it has written the re-admitted
// root, so the end finds the tree running again and stops nothing of it: no suspend of the root
// architect and no linger_close row for the hour after the end.
func TestAReadmissionEndsAnOpenRetroAfterAClose(t *testing.T) {
	pool := migratedPool(t)
	now := fixedNow
	engine, admission := retroAdmission(&now)
	const root = "LEGION-1"
	seedSlotted(t, pool, root, "A")
	startFirstPhase(t, pool, engine, admission, root)
	apply(t, pool, admission, "root-closed", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done",
		Title: root, Rank: "A", HandedOver: true}, engine)
	if got := issue(t, pool, root); got.Phase != phase.Done || !got.Lingers() {
		t.Fatalf("%s after a person's done = %s, lingering %t; want it done with its tree lingering", root, got.Phase, got.Lingers())
	}
	seedWokenArchitect(t, pool, root)

	before := len(stepWrites(t, pool, root))
	mark := outboxMark(t, pool)
	apply(t, pool, admission, "root-reopened", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo",
		Title: root, Rank: "A", HandedOver: true}, engine)
	if got := issue(t, pool, root); got.Generation != 2 || got.Phase != phase.Admitted || got.Lingers() {
		t.Fatalf("%s after a person's todo = generation %d in %s, lingering %t; want it re-admitted at generation 2, at admitted", root, got.Generation, got.Phase, got.Lingers())
	}

	var ends, rated []record.StepRecord
	for _, write := range stepWrites(t, pool, root)[before:] {
		switch {
		case write.PlanRating != nil:
			rated = append(rated, write)
		case write.Kind == "retro" && write.EndedAt != nil:
			ends = append(ends, write)
		}
	}
	if len(ends) != 1 || ends[0].Result == nil || *ends[0].Result != string(workflow.ResultStopped) || ends[0].Generation != 1 || len(rated) != 0 {
		t.Fatalf("the retro's step row at the re-admission: ends %+v, ratings %+v; want one end of run 1, stopped, and no rating", ends, rated)
	}
	if ended, outcome, endedBy, _ := retroEnd(t, pool, root, 1); ended == nil || outcome != record.RetroOutcomeStopped || endedBy != record.RetroEndTodo {
		t.Fatalf("the run_retros row of %s run 1 = ended %v, outcome %q, by %q; want it ended stopped by the todo", root, ended, outcome, endedBy)
	}
	if suspends := architectSuspendsSince(t, pool, root, mark); suspends != 0 {
		t.Fatalf("suspends of the root architect at the re-admission = %d, want none: the tree runs again", suspends)
	}
	if rows := retrosEndedRowsSince(t, pool, root, mark); len(rows) != 0 {
		t.Fatalf("linger_close rows naming the retro's end at the re-admission = %+v, want none: the tree runs again", rows)
	}
}

// A person's todo on a done child while its tree is past its linger re-admits the child into a tree
// of its own (readmit, an orphan of a tree that is not live), and ends the retro after the child's
// close, the last open retro of the tree it left (review thread PRRT_kwDORFy7ds69DTRE of #5630).
// The linger's end applied at its deadline with that retro open and left the child's claims and
// workspace to it, and reserved none of the tree's cleanup (lingerExpired). The end of that retro
// is decided by the old tree's own state, read from the ended row: the tree is past its linger, so
// it queues the old tree's close for the hour after the end on the root (a linger_close row naming
// the end), whose rows the workflow's TestEveryEndOfATreesLastRetroStopsWhatTheTreeKeptByItsState
// asserts.
func TestAChildsReadmissionPastItsTreesLingerQueuesTheClose(t *testing.T) {
	pool := migratedPool(t)
	now := fixedNow
	engine, admission := retroAdmission(&now)
	const root, child = "LEGION-1", "LEGION-2"
	seedSlotted(t, pool, root, "A")
	startFirstPhase(t, pool, engine, admission, root)
	parent := root
	putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: child, Tree: root, Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B", HandedOver: true})
	startFirstPhase(t, pool, engine, admission, child)
	apply(t, pool, admission, "child-closed", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.closed", Status: "done",
		Title: child, Parent: root, Rank: "B", HandedOver: true}, engine)
	apply(t, pool, admission, "root-closed", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.closed", Status: "done",
		Title: root, Rank: "A", HandedOver: true}, engine)
	closed := issue(t, pool, root)
	if closed.Phase != phase.Done || !closed.Lingers() {
		t.Fatalf("%s after a person's done = %s, lingering %t; want it done with its tree lingering", root, closed.Phase, closed.Lingers())
	}
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "retro-after-close:LEGION-1:1", intake.RetroReport{Issue: root,
		Claim: roleToken(t, root, claim.RoleImplementer), Generation: 1, PlanRating: record.PlanRating{Rating: 4, Reason: "The plan put the tests first."}},
		engine, admission); err != nil {
		t.Fatalf("the report of the root's retro: %v", err)
	}
	now = closed.LingerUntil.Add(time.Minute)
	if _, err := intake.ApplyFact(context.Background(), pool, "outbox", workflow.LingerEventID(root, 1), intake.LingerExpired{Issue: root, Generation: 1},
		engine, admission); err != nil {
		t.Fatalf("the linger's end with the child's retro open: %v", err)
	}
	if ended, _, _, tree := retroEnd(t, pool, child, 1); ended != nil || tree != root {
		t.Fatalf("the child's retro after the linger's end = ended %v in %s's tree; want it open in %s's, the tree's last", ended, tree, root)
	}

	now = now.Add(10 * time.Minute)
	mark := outboxMark(t, pool)
	apply(t, pool, admission, "child-reopened", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: "todo",
		Title: child, Parent: root, Rank: "B", HandedOver: true}, engine)
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 2 {
		t.Fatalf("%s after a person's todo = tree %s, generation %d; want it re-admitted into a tree of its own at generation 2", child, got.Tree, got.Generation)
	}
	ended, outcome, endedBy, tree := retroEnd(t, pool, child, 1)
	if ended == nil || outcome != record.RetroOutcomeStopped || endedBy != record.RetroEndTodo || tree != root {
		t.Fatalf("the child's retro after the todo = ended %v, outcome %q, by %q, in %s's tree; want it ended stopped by the todo in %s's", ended, outcome, endedBy, tree, root)
	}
	rows := retrosEndedRowsSince(t, pool, root, mark)
	if len(rows) != 1 || rows[0].close.Generation != 1 || !rows[0].close.RetrosEnded.Equal(*ended) || !rows[0].due.Equal(ended.Add(pods.RetroWakeWindow)) {
		t.Fatalf("linger_close rows of %s naming the retro's end at the child's todo = %+v, want one at generation 1 naming the end %s, due the hour after it",
			root, rows, ended)
	}
}
