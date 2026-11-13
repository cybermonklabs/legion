package workflow

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// appendOnlyIssue is the issue whose two transitions land before its slot does.
const appendOnlyIssue = "LEGION-208"

// owedStart is one supervise start row of an issue as the transition that wrote it left the
// table: the row's own identity, when it entered the outbox, and what it asks for.
type owedStart struct {
	id        int64
	createdAt time.Time
	role      claim.Role
	task      string
	entry     int64
}

// owedStartsOf is every supervise start row of issue, oldest first by the pair the waiting line
// orders them on (record.OwedStart). It reads the table directly, so what it reports is the rows
// as the transitions left them.
func owedStartsOf(t *testing.T, pool *pgxpool.Pool, issue string) []owedStart {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select id, created_at, coalesce(payload->>'role', ''), coalesce(payload->>'task', ''),
		coalesce((payload->>'entry')::bigint, 0)
		from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by created_at, id`, issue)
	if err != nil {
		t.Fatalf("read the start rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var starts []owedStart
	for rows.Next() {
		var start owedStart
		if err := rows.Scan(&start.id, &start.createdAt, &start.role, &start.task, &start.entry); err != nil {
			t.Fatalf("scan a start row of %s: %v", issue, err)
		}
		starts = append(starts, start)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the start rows of %s: %v", issue, err)
	}
	return starts
}

// seedAppendOnlyRoot records a root of its own whose planner has finished, so the fact below
// moves it into implementing, with the pull request its implementing round ends over: the
// implementer's completion moves the issue on only while its change is open on GitHub
// (record.LetsImplementationMove).
func seedAppendOnlyRoot(t *testing.T, pool *pgxpool.Pool, key string, number int) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: key, Project: "LEGION", Title: "root " + key, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})
	seedTreeLifecycle(t, pool, key)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: number,
		Branch: "legion/" + key, HeadSHA: "head", CheckedHead: "head", Failing: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: key, Role: claim.RolePlanner, Claim: claim.Token("plan-claim-" + key)})
}

// plannerDone is the planner's completion of key, which starts the implementer.
func plannerDone(key string) intake.HandoffComplete {
	return intake.HandoffComplete{Generation: 1, Issue: key, Role: claim.RolePlanner, Claim: claim.Token("plan-claim-" + key),
		HandoffRecord: "plan-" + key, HandoffVersion: 1}
}

// Two transitions before the issue's admission slot leave one start row, and the later one is a
// row of its own: the earlier row is deleted and a new row written carrying its creation time,
// never the earlier row's payload rewritten in place (record.Store.EnqueueStart).
//
// One outbox row is one task, which is what a claim's record of the rows it has already taken a
// task from rests on (supervise.Claim.DeliveredRows). A row rewritten in place could be one whose
// earlier payload had already reached the claim, and the lane running it would then recognise the
// row, drop the later task and finish clean: a phase with no task, no error and no row behind it
// (dispatch://INFRA-2800).
func TestTwoTransitionsBeforeTheSlotLeaveTheLaterStartAppendOnly(t *testing.T) {
	pool := migratedPool(t)
	seedAppendOnlyRoot(t, pool, appendOnlyIssue, 42)
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyRefusingNothing(t, pool, engine, plannerDone(appendOnlyIssue))
	first := owedStartsOf(t, pool, appendOnlyIssue)
	if len(first) != 1 || first[0].role != claim.RoleImplementer {
		t.Fatalf("the start rows after the planner's completion = %+v, want the one the implementer is owed", first)
	}

	// The implementer completes before the slot comes, which sends the issue to testing and
	// writes the tester's start over the one the issue owes.
	applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: appendOnlyIssue, Role: claim.RoleImplementer,
		Claim: "implement-claim", Commit: "fix-1", HandoffRecord: "implement-1", HandoffVersion: 1})

	second := owedStartsOf(t, pool, appendOnlyIssue)
	if len(second) != 1 {
		t.Fatalf("the start rows after the second transition = %+v, want the one start the issue owes: two transitions before the slot start the worker once", second)
	}
	later := second[0]
	if later.role != claim.RoleTester || later.task == first[0].task {
		t.Fatalf("the owed start = %+v, want the tester's, carrying the later transition's task rather than the %q the implementer was given",
			later, first[0].task)
	}
	if later.id == first[0].id {
		t.Fatalf("the later task rides on row %d, the row the earlier task was written in: one outbox row is one task, so the later start is a row of its own", later.id)
	}
	if !later.createdAt.Equal(first[0].createdAt) {
		t.Fatalf("the later start entered the outbox at %s, want the %s the earlier row carried: the creation time holds the issue's place in the waiting line",
			later.createdAt, first[0].createdAt)
	}
	var left int
	if err := pool.QueryRow(t.Context(), "select count(*) from outbox where id = $1", first[0].id).Scan(&left); err != nil {
		t.Fatalf("read what became of row %d: %v", first[0].id, err)
	}
	if left != 0 {
		t.Fatalf("row %d is still in the outbox, want it deleted with the start it carried: the issue owes one start", first[0].id)
	}
}

// The issue's place in the admission waiting line is the same before the second transition and
// after it: the start written over the one it owed carries that row's creation time, and the line
// orders the issues whose work came back on that time (record.Waiting over record.OwedStarts). An
// issue that took a new creation time would go to the back of the line for finishing a phase,
// behind issues that came to wait after it.
func TestASecondTransitionBeforeTheSlotKeepsTheIssuesPlaceInTheWaitingLine(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	const ahead, behind = "LEGION-100", "LEGION-300"
	for i, key := range []string{ahead, appendOnlyIssue, behind} {
		seedAppendOnlyRoot(t, pool, key, 40+i)
	}

	// Each issue comes to wait in its own transaction, so the three creation times run in the
	// order their work came back.
	applyRefusingNothing(t, pool, engine, plannerDone(ahead))
	applyRefusingNothing(t, pool, engine, plannerDone(appendOnlyIssue))
	applyRefusingNothing(t, pool, engine, plannerDone(behind))

	want := []string{ahead, appendOnlyIssue, behind}
	if got := waitingLine(t, pool, engine); !slices.Equal(got, want) {
		t.Fatalf("the waiting line is %q, want %q: each issue stands where its work came back", got, want)
	}

	applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: appendOnlyIssue, Role: claim.RoleImplementer,
		Claim: "implement-claim", Commit: "fix-1", HandoffRecord: "implement-1", HandoffVersion: 1})

	if got := waitingLine(t, pool, engine); !slices.Equal(got, want) {
		t.Fatalf("the waiting line after the second transition is %q, want the %q it was: a second transition is the same wait carrying a newer task",
			got, want)
	}
}

// waitingLine is the keys of the admission waiting line, read through the two statements the
// real pass reads it through (record.Waiting over record.ReadLineFacts).
func waitingLine(t *testing.T, pool *pgxpool.Pool, engine *Engine) []string {
	t.Helper()
	var keys []string
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		issues, err := store.Issues(t.Context(), tx)
		if err != nil {
			return err
		}
		slots, err := store.Slots(t.Context(), tx)
		if err != nil {
			return err
		}
		facts, err := record.ReadLineFacts(t.Context(), tx, store, "LEGION", issues, engine.cfg.Steps)
		if err != nil {
			return err
		}
		for _, entry := range record.Waiting(issues, slots, facts) {
			keys = append(keys, entry.Issue.Key)
		}
		return nil
	})
	return keys
}
