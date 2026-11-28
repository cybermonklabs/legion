package admit

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A waiting root moved to another in-work status keeps its phase, and gets no slot until
// Dispatch shows it todo again. No promotion or new run is triggered by another status.
func TestAWaitingRootMovedOutOfTodoWaitsForTodo(t *testing.T) {
	for _, status := range []string{"in_progress", "testing", "needs_review", "retro"} {
		t.Run(status, func(t *testing.T) {
			const key = "LEGION-5"
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := priorityEngine()
			seedSlotted(t, pool, "LEGION-1", "A")
			seedWaiting(t, pool, key, "B")
			assertWaiting(t, pool, []string{key})
			fact := intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: status, Title: key, Rank: "B", HandedOver: true}
			apply(t, pool, admission, "other-status", fact, engine)
			if got := issue(t, pool, key); got.Phase != phase.Admitted || got.Status != status || got.Generation != 1 {
				t.Fatalf("waiting root after status change = %+v; want admitted at %s in generation 1", got, status)
			}
			assertWaiting(t, pool, nil)
			apply(t, pool, admission, "slot-freed", intake.DispatchIssue{Key: "LEGION-1", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "done", Title: "LEGION-1", Rank: "A", HandedOver: true}, engine)
			assertSlots(t, pool, nil)
			if starts := architectStarts(t, pool, key); starts != 0 {
				t.Fatalf("architect starts before the todo = %d; want none", starts)
			}
			fact.Seq, fact.Status = 3, "todo"
			apply(t, pool, admission, "todo", fact, engine)
			assertSlots(t, pool, []record.Slot{{Issue: key, Index: 0, AdmittedAt: fixedNow}})
			if got := issue(t, pool, key); got.Generation != 1 {
				t.Fatalf("return to todo opened generation %d; want generation 1", got.Generation)
			}
		})
	}
}

// An unknown status is recorded without ending the issue's run.
func TestAnUnknownStatusDoesNotReleaseAnUnslottedRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Clock: func() time.Time { return fixedNow }}, nil)
	stranded := record.Issue{Key: "LEGION-UNKNOWN", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "unknown status", Tree: "LEGION-UNKNOWN",
		Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "A", HandedOver: true, LastDispatchSeq: 1}
	putIssue(t, pool, stranded)

	apply(t, pool, admission, "unknown-status", intake.DispatchIssue{Key: stranded.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "blocked", Title: stranded.Title, Rank: stranded.Rank, HandedOver: true}, engine)

	got := issue(t, pool, stranded.Key)
	if got.Phase != phase.Admitted || got.Status != "blocked" {
		t.Fatalf("unknown-status root = %#v, want it left admitted with the status recorded", got)
	}
	if notices := controllerNotices(t, pool); len(notices) != 0 {
		t.Fatalf("controller notices = %#v, want none for an unknown status", notices)
	}
}

func controllerNotices(t *testing.T, pool *pgxpool.Pool) []effect {
	t.Helper()
	var got []effect
	for _, row := range effects(t, pool) {
		if row.kind == record.OutboxKindControllerNotice {
			got = append(got, row)
		}
	}
	return got
}
