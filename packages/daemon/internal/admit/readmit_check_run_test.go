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
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A hand-over that carries its child's run carries the run of the child's open production checks:
// each open check of the old generation moves to the new one (record.Store.CarryProductionChecks),
// so a check opened at a merge of the carried run is still a check of the run that holds the issue.
// LEGION-209, under the lingering LEGION-208 at generation 3, has a (merges) done at its merge, with
// its check open at look 1 and a wake standing, and b at its first step; a check of an earlier merge
// of generation 3 is closed. The hand-over carries the run to generation 4: the open check is of
// generation 4, its wake starts look 2 while b goes on, and the closed check keeps generation 3.
//
// The fault it catches is a check of the carried run read as a check of an earlier run, whose wakes
// defer, whose bound closes it as not run and which the run's next merge closes as not run (its
// control: readmit without the move, under which the wake defers and look 2 never starts).
func TestACarriedHandOverKeepsTheRunOfItsOpenCheck(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour, ProductionCheckRole: claim.RoleTester, ProductionCheckBound: 48 * time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-208", "LEGION-209"
	until := fixedNow.Add(time.Hour)
	parent := root
	version := record.PlanVersion{Issue: child, Generation: 3, Version: 1, ArtifactID: "artifact-plan-209", Source: record.PlanSourceOwner,
		GivenBy: "legion-legion-legion-208-architect", GivenAt: fixedNow.Add(-3 * time.Hour)}
	tasks := []record.PlanTask{
		{Issue: child, Generation: 3, Task: "a", PlanVersion: 1, BlockText: "Do a.", State: record.TaskDone, Place: record.TaskPlace{Kind: record.PlaceMerge},
			Merges: true, Branch: "legion/LEGION-209"},
		{Issue: child, Generation: 3, Task: "b", PlanVersion: 1, BlockText: "Do b.", State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: 1},
			Branch: "legion/LEGION-209.b"},
	}
	steps := []record.PlanStep{
		{Issue: child, Generation: 3, Task: "a", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer"},
		{Issue: child, Generation: 3, Task: "b", Position: 1, Role: "implementer", Type: "implementation", Agent: "implementer-b"},
	}
	opened := fixedNow.Add(-time.Hour)
	wake := fixedNow
	open := record.ProductionCheck{Issue: child, Project: testProject, Tree: root, Repo: "acme/widgets", Number: 42, MergeSHA: "merge-a",
		OpenedAt: opened, BoundAt: opened.Add(48 * time.Hour), Role: claim.RoleTester, Looks: 1, LookWakeAt: &opened, LastWakeKind: "merge",
		LastWakeDetail: "merge-a", LastWakeAt: &opened, WakeAt: &wake, Generation: 3, Task: "a"}
	earlierOpened, earlierClosed := fixedNow.Add(-30*time.Hour), fixedNow.Add(-26*time.Hour)
	closed := record.ProductionCheck{Issue: child, Project: testProject, Tree: root, Repo: "acme/widgets", Number: 41, MergeSHA: "merge-earlier",
		OpenedAt: earlierOpened, BoundAt: earlierOpened.Add(48 * time.Hour), Role: claim.RoleTester, Looks: 1, LookWakeAt: &earlierOpened,
		LastWakeKind: "merge", LastWakeDetail: "merge-earlier", LastWakeAt: &earlierOpened, ClosedAt: &earlierClosed, Outcome: record.CheckRecorded,
		Generation: 3, Task: "a"}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
				Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
			records.PutIssue(ctx, tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
				Title: child, Parent: &parent, Phase: phase.Implementing, Generation: 3, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}),
			records.PutPullRequest(ctx, tx, record.PullRequest{Issue: child, Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head-42",
				Generation: 3, State: record.PullRequestMerged, MergeSHA: "merge-a"}),
			records.PutPullRequest(ctx, tx, record.PullRequest{Issue: child, Repo: "acme/widgets", Number: 45, Branch: "legion/LEGION-209.b", HeadSHA: "head-45",
				Generation: 3, State: record.PullRequestOpen}),
			records.PutHandover(ctx, tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
			records.PutPlan(ctx, tx, version, tasks, steps),
			records.PutProductionCheck(ctx, tx, open),
			records.PutProductionCheck(ctx, tx, closed),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo", Title: child,
		Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 4 || got.Phase != phase.Implementing {
		t.Fatalf("handed-over child = %#v, want its own tree at generation 4, still implementing: the fixture must be a carried hand-over", got)
	}
	if _, err := intake.ApplyFact(ctx, pool, "outbox", "wake", intake.CheckWakeDue{Issue: child, Look: 1, OpenedAt: opened, Reason: "the deploy has not carried merge-a"},
		engine, admission); err != nil {
		t.Fatalf("ApplyFact wake: %v", err)
	}
	checks := checksOf(t, pool, child)
	if len(checks) != 2 || !checks[0].OpenedAt.Equal(opened) || checks[0].Looks != 2 || !checks[0].LookOpen {
		t.Fatalf("the checks of %s at the open check's wake = %+v, want look 2 of the check opened at %s started while b goes on, not deferred", child, checks, opened)
	}
	if checks[0].Generation != 4 || !checks[1].OpenedAt.Equal(earlierOpened) || checks[1].Generation != 3 {
		t.Fatalf("the checks of %s after the hand-over = %+v, want the open one of generation 4 and the closed one still of generation 3", child, checks)
	}
	starts := 0
	for _, start := range checkLookStarts(t, pool, child) {
		if start.CheckLook == 2 && start.CheckOpenedAt.Equal(opened) {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("the starts of look 2 of %s's check = %d, want one", child, starts)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		stored, err := record.NewStore().Plan(ctx, tx, child, 4)
		if err != nil || stored == nil {
			t.Fatalf("the plan of %s at generation 4 = %+v, %v", child, stored, err)
		}
		if b, _ := stored.TaskOf("b"); b.State != record.TaskRunning || b.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) {
			t.Fatalf("task b after the wake is %s at %+v, want running at its first step", b.State, b.Place)
		}
	})
}

// checksOf is each production check of key, newest opened first, as recorded.
func checksOf(t *testing.T, pool *pgxpool.Pool, key string) []record.ProductionCheck {
	t.Helper()
	var checks []record.ProductionCheck
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if checks, err = record.NewStore().ProductionChecksOf(context.Background(), tx, key); err != nil {
			t.Fatalf("read the checks of %s: %v", key, err)
		}
	})
	return checks
}

// checkLookStarts is each start row of key that starts a look of a production check, oldest first.
func checkLookStarts(t *testing.T, pool *pgxpool.Pool, key string) []record.SuperviseRequest {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload ? 'checkLook' order by id`, key)
	if err != nil {
		t.Fatalf("read the look starts of %s: %v", key, err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read the look starts of %s: %v", key, err)
	}
	starts := make([]record.SuperviseRequest, len(payloads))
	for i, payload := range payloads {
		if err := json.Unmarshal(payload, &starts[i]); err != nil {
			t.Fatalf("decode a look start of %s: %v", key, err)
		}
	}
	return starts
}
