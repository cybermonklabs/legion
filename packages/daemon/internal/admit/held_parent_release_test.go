package admit

import (
	"context"
	"fmt"
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

// assertHeldForChildren holds that key stands held before its retro for the issues beneath it,
// with no retro started: why is the step of the case it is read after.
func assertHeldForChildren(t *testing.T, pool *pgxpool.Pool, key, why string) {
	t.Helper()
	if got := issue(t, pool, key); !record.HeldForChildren(got) {
		t.Fatalf("%s %s = %s with hold %+v, want held before its retro for its children", key, why, got.Phase, got.Hold)
	}
	if n := phaseStarts(t, pool, key, claim.RoleImplementer, phase.Retro); n != 0 {
		t.Fatalf("%s %s started its retro %d times, want none yet", key, why, n)
	}
}

// assertRetroStarted holds that key's hold before its retro ended and its retro started once:
// the issue stands in retro with no hold, and its implementer is started in retro once.
func assertRetroStarted(t *testing.T, pool *pgxpool.Pool, key, why string) {
	t.Helper()
	if got := issue(t, pool, key); got.Phase != phase.Retro || got.Hold != nil {
		t.Fatalf("%s %s = %s with hold %+v, want its retro started: nothing beneath it is in the workflow", key, why, got.Phase, got.Hold)
	}
	if n := phaseStarts(t, pool, key, claim.RoleImplementer, phase.Retro); n != 1 {
		t.Fatalf("%s %s started its implementer in retro %d times, want once", key, why, n)
	}
}

// A parent whose plan has no acceptance step is held before its retro while an issue beneath it
// on its Dispatch chain is in the workflow (record.HoldForChildren), and its retro starts once
// nothing beneath it is and its gate root runs (workflow's StartHeldRetro). A leave of the last
// issue beneath it is one way the set empties; a change of parent in Dispatch that moves the last
// issue beneath it under another issue is another (admission's recordObservation), and the moved
// issue's new chain no longer reaches the parent, which the settle reads by project
// (workflow's releaseHeldParents). The retro starts in the fact that leaves nothing open beneath
// the parent, and not before: the premise is read at the step before it.
func TestAParentHeldForItsChildrenIsReleasedWheneverNothingBeneathItIsOpen(t *testing.T) {
	t.Run("a re-parent in Dispatch moves the last issue beneath it away", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		planByOwner(t, pool, inRetro, record.TaskPlace{Kind: record.PlaceRetro})
		holdForChildren(t, pool, inRetro)
		assertHeldForChildren(t, pool, inRetro, "with "+heldBeneath+" beneath it")

		apply(t, pool, admission, "reparent", intake.DispatchIssue{Key: heldBeneath, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated",
			Status: "in_progress", Title: heldBeneath, Parent: finishedRoot, Rank: "C"}, engine)

		if got := issue(t, pool, heldBeneath); got.Parent == nil || *got.Parent != finishedRoot || !record.InWorkflow(got) {
			t.Fatalf("%s after the re-parent = %#v, want it in the workflow under %s: the fixture must move it, not end it", heldBeneath, got, finishedRoot)
		}
		assertRetroStarted(t, pool, inRetro, "after "+heldBeneath+" moved under "+finishedRoot)
	})
}

// assertRetriesRefused applies to key, a parent held before its retro for its children, the
// operator's retry of the hold (`legion claims retry`, which names no architect) and the operator's
// restart of its implementer, and holds that each is refused 409 HELD_FOR_CHILDREN with want and
// leaves key held with no retro started.
func assertRetriesRefused(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler, key, want string) {
	t.Helper()
	for i, fact := range []intake.Fact{
		intake.RetryOrEscalate{Issue: key, Decision: intake.RetryDecision, Reason: "try again"},
		intake.RestartPhase{Issue: key, Role: claim.RoleImplementer},
	} {
		result, err := intake.ApplyFact(context.Background(), pool, "api", fmt.Sprintf("retry-%s-%d", key, i), fact, engine, admission)
		if err != nil || result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "HELD_FOR_CHILDREN" || result.Refusal.Message != want {
			t.Fatalf("the release %T of %s = %+v, %v; want 409 HELD_FOR_CHILDREN %q", fact, key, result.Refusal, err, want)
		}
		assertHeldForChildren(t, pool, key, fmt.Sprintf("after the refused %T", fact))
	}
}

// A retry of a parent held before its retro for its children is refused, and where nothing
// beneath the parent on its Dispatch chain is open, the retro waits only for the parent's gate
// root to run (workflow's StartHeldRetro, record.TreeRuns): here a root whose work came back to
// implementing without the slot it needs waits in the line for that slot, and its project is
// paused, which grants none. Then the refusal says why the tree runs nothing, naming the tree the
// state names for the parent (record.Chains.StateTree), and lists no issue. A person's todo on the
// held parent leaves the hold, and the board is set back at its status with the same why beside it.
func TestARetryOfAParentHeldForItsChildrenSaysWhyItsRetroWaits(t *testing.T) {
	const root, parent = "LEGION-208", "LEGION-209"
	ctx := context.Background()
	pool := migratedPool(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	admission := newAdmission(t, 1, quiet)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Clock: func() time.Time { return fixedNow }}, quiet)
	rootKey := root
	version := record.PlanVersion{Issue: parent, Generation: 1, Version: 1, ArtifactID: "artifact-plan-209", Source: record.PlanSourceOwner,
		GivenBy: "legion-legion-legion-208-architect", GivenAt: fixedNow.Add(-time.Hour)}
	task := record.PlanTask{Issue: parent, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskRunning,
		Place: record.TaskPlace{Kind: record.PlaceRetro}}
	steps := []record.PlanStep{
		{Issue: parent, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
		{Issue: parent, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
	}
	// The operator paused the project, so the root waits in the line for its slot.
	pauseTestProject(t, pool, fixedNow)
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
				Title: root, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", LastDispatchSeq: 1}),
			records.PutIssue(ctx, tx, record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
				Title: parent, Parent: &rootKey, Phase: phase.Held, Hold: &record.Hold{From: phase.Retro, Reason: record.HoldForChildren}, Generation: 1,
				Status: "retro", DispatchStatus: "retro", Rank: "B", LastDispatchSeq: 1}),
			records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: root, Project: testProject, Seq: 1,
				Status: record.Carry("in_progress"), Parent: record.Carry(""), Labels: record.Carry([]string{"legion"})}),
			records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: parent, Project: testProject, Seq: 1,
				Status: record.Carry("retro"), Parent: record.Carry(root), Labels: record.Carry([]string{"legion"})}),
			records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})
	assertHeldForChildren(t, pool, parent, "while its gate root waits in the line")

	waits := parent + " waits before its retro, and no issue holds it any longer: its tree " + root +
		" waits in the line to be opened, and Legion starts the retro once the tree runs."
	assertRetriesRefused(t, pool, admission, engine, parent, waits+" This retry changed nothing.")

	// A person moves the held parent and sets it to todo again. The todo leaves the hold, and the
	// board is set back at its status with the same why beside it, so the person reads why the
	// todo went back (workflow's heldParentWait).
	apply(t, pool, admission, "person-moves", intake.DispatchIssue{Key: parent, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "in_progress",
		Title: parent, Parent: root, Rank: "B"}, engine)
	apply(t, pool, admission, "person-todo", intake.DispatchIssue{Key: parent, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: "todo",
		Title: parent, Parent: root, Rank: "B"}, engine)
	assertHeldForChildren(t, pool, parent, "after a person's todo")
	writes := statusWritesOf(t, pool, parent)
	if len(writes) == 0 || writes[len(writes)-1].ObservedStatus != "todo" || writes[len(writes)-1].Reason != waits {
		t.Fatalf("the status writes of %s after a person's todo = %+v, want the last one over the todo with the reason %q", parent, writes, waits)
	}
}
