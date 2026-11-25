package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestAQueuedStatusWriteKeepsPriorityThroughANewRun(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Implementing)
	child := record.Issue{Key: "LEGION-209", Repo: root.Repo, DefaultBranch: root.DefaultBranch, GitHubRepo: root.GitHubRepo,
		Tree: root.Key, Parent: &root.Key, Project: root.Project, Title: "child", Phase: phase.Implementing,
		Generation: 3, Status: "backlog", DispatchStatus: "backlog", Rank: "V", LastDispatchSeq: 4, Priority: record.P1}
	seedIssue(t, pool, child)
	engine := stepEngine(nil)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.countCallWrite(ctx, tx, callWrite{issue: intake.StatusCallIssue{Key: child.Key},
			status: "todo", record: &child, observed: "backlog", newRun: true})
	})
	if got := issueRecord(t, pool, child.Key); got == nil || got.Generation != 4 || got.Priority != record.P1 {
		t.Fatalf("new run = %+v, want generation 4 with the stored P1", got)
	}
}

func TestAHeldStatusWriteUsesTheLatestRecordedPriority(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Implementing)
	root.Status, root.DispatchStatus, root.LastDispatchSeq, root.Priority = "backlog", "todo", 8, record.P2
	seedIssue(t, pool, root)
	engine := stepEngine(nil)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.applyStatus(ctx, tx, root, heldWriteFact(root, record.HeldSessionWrite{Seq: 5, Status: "todo"}))
	})
	if got := issueRecord(t, pool, root.Key); got == nil || got.Status == "backlog" || got.LastDispatchSeq != 8 || got.Priority != record.P2 {
		t.Fatalf("return from the held write = %+v, want the returned phase and the P2 recorded at sequence 8", got)
	}
}
