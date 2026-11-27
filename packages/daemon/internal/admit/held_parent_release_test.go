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
// is in the workflow (record.HoldForChildren), and its retro starts once nothing beneath it is.
// A leave of the last issue beneath it is one way the set empties; each row here is another,
// and none of them is a leave beneath the parent in a tree that runs:
//   - a re-parent in Dispatch moves the last issue beneath it under another issue of the tree
//     (admission's recordObservation);
//   - the last issue beneath it leaves while the tree lingers after its root's close, and a person
//     reopens the root, whose promotion runs the tree again;
//   - the linger of its closed root ends, the parent is handed over as a root of its own with its
//     hold and its plan (readmit), and the promotion of that root opens its tree.
//
// In each the retro starts in the fact that leaves nothing open beneath the parent in a tree
// that runs, and not before: the premise of each row is read at the step before it.
func TestAParentHeldForItsChildrenIsReleasedWheneverNothingBeneathItIsOpen(t *testing.T) {
	t.Run("a re-parent in Dispatch moves the last issue beneath it away", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		planByOwner(t, pool, inRetro, record.TaskPlace{Kind: record.PlaceRetro})
		holdForChildren(t, pool, inRetro)
		assertHeldForChildren(t, pool, inRetro, "with "+heldBeneath+" beneath it")

		apply(t, pool, admission, "reparent", intake.DispatchIssue{Key: heldBeneath, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated",
			Status: "in_progress", Title: heldBeneath, Parent: finishedRoot, Rank: "C"}, engine)

		if got := issue(t, pool, heldBeneath); got.Parent == nil || *got.Parent != finishedRoot || !workflow.InWorkflow(got) {
			t.Fatalf("%s after the re-parent = %#v, want it in the workflow under %s: the fixture must move it, not end it", heldBeneath, got, finishedRoot)
		}
		assertRetroStarted(t, pool, inRetro, "after "+heldBeneath+" moved under "+finishedRoot)
	})

	t.Run("the last issue beneath it leaves while the tree lingers, and the root is reopened", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		planByOwner(t, pool, inRetro, record.TaskPlace{Kind: record.PlaceRetro})
		holdForChildren(t, pool, inRetro)
		closeFinishingTree(t, pool, admission, engine)
		apply(t, pool, admission, "beneath-done", intake.DispatchIssue{Key: heldBeneath, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated",
			Status: "done", Title: heldBeneath, Parent: inRetro, Rank: "C"}, engine)
		assertHeldForChildren(t, pool, inRetro, "while its tree lingers")

		reopenFinishingTree(t, pool, admission, engine)

		if root := issue(t, pool, finishedRoot); root.Generation != 2 || root.Lingers() {
			t.Fatalf("the reopened root = %#v, want generation 2, no longer lingering: the promotion this case reads did not run", root)
		}
		assertRetroStarted(t, pool, inRetro, "once its tree runs again")
	})

	t.Run("the parent is handed over as a root of its own with its hold, and its tree opens", func(t *testing.T) {
		const root, parent, beneath = "LEGION-208", "LEGION-209", "LEGION-210"
		ctx := context.Background()
		pool := migratedPool(t)
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		admission := newAdmission(t, 1, quiet)
		engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
			Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, quiet)
		until := fixedNow.Add(time.Hour)
		rootKey, parentKey := root, parent
		version := record.PlanVersion{Issue: parent, Generation: 1, Version: 1, ArtifactID: "artifact-plan-209", Source: record.PlanSourceOwner,
			GivenBy: "legion-legion-legion-208-architect", GivenAt: fixedNow.Add(-time.Hour)}
		task := record.PlanTask{Issue: parent, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskRunning,
			Place: record.TaskPlace{Kind: record.PlaceRetro}}
		steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
			{Issue: parent, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: parent, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
		})
		inTx(t, pool, func(tx pgx.Tx) {
			records := record.NewStore()
			for _, put := range []error{
				records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
					Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
				records.PutIssue(ctx, tx, record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
					Title: parent, Parent: &rootKey, Phase: phase.Held, Hold: &record.Hold{From: phase.Retro, Reason: record.HoldForChildren}, Generation: 1,
					Status: "retro", DispatchStatus: "retro", Rank: "B", LastDispatchSeq: 1}),
				records.PutIssue(ctx, tx, record.Issue{Key: beneath, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
					Title: beneath, Parent: &parentKey, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "C", LastDispatchSeq: 1}),
				records.PutHandover(ctx, tx, record.Handover{Issue: parent, Tree: root, Generation: 1}),
				records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps),
			} {
				if put != nil {
					t.Fatalf("seed: %v", put)
				}
			}
		})

		// The hand-over's own todo re-admits the parent as a root of its own, carrying its hold and
		// its plan, and promotes it, which opens its tree: the parent's former children stand in the
		// old tree, so nothing stands beneath it there.
		apply(t, pool, admission, "handover", intake.DispatchIssue{Key: parent, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
			Title: parent, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
		if got := issue(t, pool, parent); got.Tree != parent || got.Generation != 2 || got.HandedOverFrom != "" {
			t.Fatalf("the handed-over parent = %#v, want a root of its own at generation 2 whose tree its promotion opened: the fixture must be a carried hand-over", got)
		}
		if got := issue(t, pool, beneath); got.Tree != root {
			t.Fatalf("%s after the hand-over stands in the tree of %s, want %s: it is no issue beneath the handed-over parent", beneath, got.Tree, root)
		}
		assertRetroStarted(t, pool, parent, "handed over as a root of its own with nothing beneath it")
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
// beneath the parent is open, the retro waits only for the parent's tree to run (workflow's
// StartHeldRetro). Then the refusal says why the tree runs nothing, and lists no issue:
//   - the last issue beneath it left while the tree lingers after its root's close;
//   - the parent, handed over as a root of its own with its hold and its plan, waits in the line
//     for its tree to be opened while its project is paused, which promotes nothing.
func TestARetryOfAParentHeldForItsChildrenSaysWhyItsRetroWaits(t *testing.T) {
	t.Run("its tree lingers with nothing open beneath it", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		planByOwner(t, pool, inRetro, record.TaskPlace{Kind: record.PlaceRetro})
		holdForChildren(t, pool, inRetro)
		closeFinishingTree(t, pool, admission, engine)
		apply(t, pool, admission, "beneath-done", intake.DispatchIssue{Key: heldBeneath, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated",
			Status: "done", Title: heldBeneath, Parent: inRetro, Rank: "C"}, engine)
		if root := issue(t, pool, finishedRoot); !root.Lingers() || workflow.InWorkflow(issue(t, pool, heldBeneath)) {
			t.Fatalf("the root = %#v and %s is in the workflow: %t; want the tree lingering with nothing open beneath %s", root, heldBeneath,
				workflow.InWorkflow(issue(t, pool, heldBeneath)), inRetro)
		}
		assertHeldForChildren(t, pool, inRetro, "while its tree lingers")

		assertRetriesRefused(t, pool, admission, engine, inRetro, inRetro+" waits before its retro, and no issue holds it any longer: its tree "+finishedRoot+
			" lingers after its root left the workflow, and Legion starts the retro once the tree runs again. This retry changed nothing.")
	})

	t.Run("it waits in the line as a root of its own with nothing beneath it", func(t *testing.T) {
		const root, parent, beneath = "LEGION-208", "LEGION-209", "LEGION-210"
		ctx := context.Background()
		pool := migratedPool(t)
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		admission := newAdmission(t, 1, quiet)
		engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
			Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, quiet)
		until := fixedNow.Add(time.Hour)
		rootKey, parentKey := root, parent
		version := record.PlanVersion{Issue: parent, Generation: 1, Version: 1, ArtifactID: "artifact-plan-209", Source: record.PlanSourceOwner,
			GivenBy: "legion-legion-legion-208-architect", GivenAt: fixedNow.Add(-time.Hour)}
		task := record.PlanTask{Issue: parent, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskRunning,
			Place: record.TaskPlace{Kind: record.PlaceRetro}}
		steps := record.NameAgents(nil, nil, nil, []record.PlanStep{
			{Issue: parent, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: parent, Generation: 1, Task: "a", Position: 2, Role: "reviewer", Type: "review"},
		})
		// The operator paused the project, so nothing promotes the parent once it is a root.
		pauseTestProject(t, pool, fixedNow)
		inTx(t, pool, func(tx pgx.Tx) {
			records := record.NewStore()
			for _, put := range []error{
				records.PutIssue(ctx, tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
					Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
				records.PutIssue(ctx, tx, record.Issue{Key: parent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
					Title: parent, Parent: &rootKey, Phase: phase.Held, Hold: &record.Hold{From: phase.Retro, Reason: record.HoldForChildren}, Generation: 1,
					Status: "retro", DispatchStatus: "retro", Rank: "B", LastDispatchSeq: 1}),
				records.PutIssue(ctx, tx, record.Issue{Key: beneath, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject,
					Title: beneath, Parent: &parentKey, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "C", LastDispatchSeq: 1}),
				records.PutHandover(ctx, tx, record.Handover{Issue: parent, Tree: root, Generation: 1}),
				records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps),
			} {
				if put != nil {
					t.Fatalf("seed: %v", put)
				}
			}
		})

		apply(t, pool, admission, "handover", intake.DispatchIssue{Key: parent, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
			Title: parent, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
		if got := issue(t, pool, parent); got.Tree != parent || got.Lingers() || got.HandedOverFrom == "" {
			t.Fatalf("the handed-over parent = %#v, want a root of its own that still waits for the promotion that opens its tree", got)
		}
		assertHeldForChildren(t, pool, parent, "while it waits in the line")

		waits := parent + " waits before its retro, and no issue holds it any longer: its tree " + parent +
			" waits in the line to be opened, and Legion starts the retro once the tree runs."
		assertRetriesRefused(t, pool, admission, engine, parent, waits+" This retry changed nothing.")

		// A person moves the waiting root and sets it to todo again. The todo leaves the hold, and
		// the board is set back at the root's status with the same why beside it, so the person
		// reads why the todo went back (workflow's heldParentWait).
		apply(t, pool, admission, "person-moves", intake.DispatchIssue{Key: parent, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "in_progress",
			Title: parent, Parent: root, Rank: "B", HandedOver: true}, engine)
		apply(t, pool, admission, "person-todo", intake.DispatchIssue{Key: parent, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated", Status: "todo",
			Title: parent, Parent: root, Rank: "B", HandedOver: true}, engine)
		assertHeldForChildren(t, pool, parent, "after a person's todo")
		writes := statusWritesOf(t, pool, parent)
		if len(writes) == 0 || writes[len(writes)-1].ObservedStatus != "todo" || writes[len(writes)-1].Reason != waits {
			t.Fatalf("the status writes of %s after a person's todo = %+v, want the last one over the todo with the reason %q", parent, writes, waits)
		}
	})
}
