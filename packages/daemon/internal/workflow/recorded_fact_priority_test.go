package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
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

// inherited is a priority Dispatch resolves an issue to from an ancestor outside its Legion tree,
// as a member with no priority of its own under a P0 parent carries it (J1 of
// dispatch://INFRA-5793).
var inherited = record.Resolved{Priority: record.P0, From: "OTHER-7", Known: true}

// The facts the daemon builds from its own record keep the resolved priority the record holds,
// with its source, for an issue whose own priority is null: the status call's own write, taken as
// a new run (countCallWrite) or as set_status's todo on an issue in Legion's work (applyCallWrite's
// backInWork), both through ownWrite, and a held session write the workflow takes (heldWriteFact).
// A fact that read the own null as a reason to drop the inherited P0 would make the issue wait
// under the cap after the daemon's own write (C1 of dispatch://INFRA-5636). Negative control, run
// when this was written, failing the two status call subtests and then reverted: with ownWrite
// carrying no Resolved, each record reads no resolved priority after the write.
func TestTheDaemonBuiltFactsKeepAnInheritedPriority(t *testing.T) {
	t.Parallel()
	t.Run("a status call's new run", func(t *testing.T) {
		t.Parallel()
		pool := migratedPool(t)
		root := stepRoot(t, pool, phase.Implementing)
		child := record.Issue{Key: "LEGION-209", Repo: root.Repo, DefaultBranch: root.DefaultBranch, GitHubRepo: root.GitHubRepo,
			Tree: root.Key, Parent: &root.Key, Project: root.Project, Title: "child", Phase: phase.Implementing,
			Generation: 3, Status: "backlog", DispatchStatus: "backlog", Rank: "V", LastDispatchSeq: 4, Resolved: inherited}
		seedIssue(t, pool, child)
		engine := stepEngine(nil)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			return engine.countCallWrite(ctx, tx, callWrite{issue: intake.StatusCallIssue{Key: child.Key},
				status: "todo", record: &child, observed: "backlog", newRun: true})
		})
		if got := issueRecord(t, pool, child.Key); got == nil || got.Generation != 4 || got.Priority != record.NoPriority || got.Resolved != inherited {
			t.Fatalf("new run = %+v, want generation 4 with no own priority and the inherited %+v its record held", got, inherited)
		}
	})
	t.Run("a status call's todo on a held issue in Legion's work", func(t *testing.T) {
		t.Parallel()
		pool := migratedPool(t)
		held := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
			Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Testing}, Generation: 1,
			Status: "testing", DispatchStatus: "testing", Rank: "U", HandedOver: true, LastDispatchSeq: 1, Resolved: inherited}
		seedIssue(t, pool, held)
		seedPhase(t, pool, record.PhaseRow{Issue: held.Key, Role: claim.RoleTester, Claim: "test-claim"})
		engine := testEngine(config.DesignGateRootIssues, nil)
		if result := applyFacts(t, pool, engine)("todo", intake.StatusCall{Op: intake.SetStatusCall,
			Issues: []intake.StatusCallIssue{{Key: held.Key, Status: "todo"}}}); result.Refusal != nil {
			t.Fatalf("set_status todo on the held %s was refused: %+v", held.Key, result.Refusal)
		}
		if got := issueRecord(t, pool, held.Key); got == nil || got.Phase != phase.Testing || got.Hold != nil ||
			got.Priority != record.NoPriority || got.Resolved != inherited {
			t.Fatalf("the released issue = %+v, want it back in testing with no own priority and the inherited %+v its record held", got, inherited)
		}
	})
	t.Run("a held session write", func(t *testing.T) {
		t.Parallel()
		pool := migratedPool(t)
		root := stepRoot(t, pool, phase.Implementing)
		root.Status, root.DispatchStatus, root.LastDispatchSeq, root.Resolved = "backlog", "todo", 8, inherited
		seedIssue(t, pool, root)
		engine := stepEngine(nil)
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			return engine.applyStatus(ctx, tx, root, heldWriteFact(root, record.HeldSessionWrite{Seq: 5, Status: "todo"}))
		})
		if got := issueRecord(t, pool, root.Key); got == nil || got.Status == "backlog" || got.Priority != record.NoPriority || got.Resolved != inherited {
			t.Fatalf("return from the held write = %+v, want the returned phase with no own priority and the inherited %+v its record held", got, inherited)
		}
	})
}
