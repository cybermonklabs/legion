package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// RecordedFact is a fact the daemon builds from its own record, for a run no live event brought,
// and it copies the stored priority: the record its run writes keeps the issue's own priority
// rather than reading as none until the next Dispatch observation. Its two callers are the
// re-entry of a child set back to todo (ReenterChild, which admission's promotion of a stranded
// child runs) and the adoption of a queued root's todo members into a live tree (Adopt).

func TestAReenteredChildKeepsItsOwnPriority(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Implementing)
	parent := "LEGION-208"
	child := record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: parent, Project: "LEGION",
		Title: "LEGION-209", Parent: &parent, Phase: phase.Implementing, Generation: 3, Status: "in_progress", Rank: "V", LastDispatchSeq: 1, Priority: record.P1}
	seedIssue(t, pool, child)
	child.Status = "todo"
	engine := stepEngine(nil)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.ReenterChild(ctx, tx, child, RecordedFact(child))
	})
	if got := issueRecord(t, pool, "LEGION-209"); got == nil || got.Generation != 4 || got.Priority != record.P1 {
		t.Fatalf("the re-entered child = %+v, want generation 4 at P1, the priority its record held", got)
	}
}

func TestAnAdoptedTodoMemberKeepsItsOwnPriority(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Implementing)
	queued := record.Issue{Key: "LEGION-300", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-300", Project: "LEGION",
		Title: "queued", Phase: phase.Admitted, Generation: 2, Status: "todo", Rank: "W", LastDispatchSeq: 1, Priority: record.P2}
	seedIssue(t, pool, queued)
	engine := stepEngine(nil)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		return engine.Adopt(ctx, tx, queued, root, intake.AdoptedChild{Issue: queued.Key, Parent: root.Key})
	})
	if got := issueRecord(t, pool, "LEGION-300"); got == nil || got.Tree != root.Tree || got.Generation != 3 || got.Priority != record.P2 {
		t.Fatalf("the adopted member = %+v, want it in %s at generation 3 at P2, the priority its record held", got, root.Tree)
	}
}

// inherited is a priority Dispatch resolves an issue to from an ancestor outside its Legion tree,
// as a member with no priority of its own under a P0 parent carries it (J1 of
// dispatch://INFRA-5793).
var inherited = record.Resolved{Priority: record.P0, From: "OTHER-7", Known: true}

// The two daemon-built facts keep the resolved priority a record holds, with its source, for a
// member whose own priority is null: a fact that read the own null as a reason to drop the
// inherited P0 would make the member wait under the cap after its re-entry or its adoption (C1 of
// dispatch://INFRA-5636).
func TestTheDaemonBuiltFactsKeepAnInheritedPriority(t *testing.T) {
	t.Run("a re-entered child", func(t *testing.T) {
		t.Parallel()
		pool := migratedPool(t)
		stepRoot(t, pool, phase.Implementing)
		parent := "LEGION-208"
		child := record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: parent, Project: "LEGION",
			Title: "LEGION-209", Parent: &parent, Phase: phase.Implementing, Generation: 3, Status: "in_progress", Rank: "V", LastDispatchSeq: 1, Resolved: inherited}
		seedIssue(t, pool, child)
		child.Status = "todo"
		engine := stepEngine(nil)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			return engine.ReenterChild(ctx, tx, child, RecordedFact(child))
		})
		if got := issueRecord(t, pool, "LEGION-209"); got == nil || got.Generation != 4 || got.Priority != record.NoPriority || got.Resolved != inherited {
			t.Fatalf("the re-entered child = %+v, want generation 4 with no own priority and the inherited %+v its record held", got, inherited)
		}
	})
	t.Run("an adopted todo member", func(t *testing.T) {
		t.Parallel()
		pool := migratedPool(t)
		root := stepRoot(t, pool, phase.Implementing)
		queued := record.Issue{Key: "LEGION-300", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-300", Project: "LEGION",
			Title: "queued", Phase: phase.Admitted, Generation: 2, Status: "todo", Rank: "W", LastDispatchSeq: 1, Resolved: inherited}
		seedIssue(t, pool, queued)
		engine := stepEngine(nil)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			return engine.Adopt(ctx, tx, queued, root, intake.AdoptedChild{Issue: queued.Key, Parent: root.Key})
		})
		if got := issueRecord(t, pool, "LEGION-300"); got == nil || got.Tree != root.Tree || got.Resolved != inherited {
			t.Fatalf("the adopted member = %+v, want it in %s with the inherited %+v its record held", got, root.Tree, inherited)
		}
	})
}
