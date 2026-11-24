package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// quietRunningByDefault is quiet for a project whose default_steps are defaults
// (workflow.Config.DefaultSteps, which the daemon wires from config.Project.RunsByDefault).
func quietRunningByDefault(t *testing.T, cap int, defaults []plan.Step) *Admission {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, DefaultSteps: defaults,
		Clock: func() time.Time { return fixedNow }}, log)
	admission := New(record.NewStore(), engine, cap, testProject, log)
	admission.now = func() time.Time { return fixedNow }
	return admission
}

// ownersSteps is register_plan's fact for key at generation 1: version 1 of pathsArtifact, its one
// task a with steps, given by the architect of pathsRoot, the owner of every issue of these tests.
func ownersSteps(t *testing.T, key string, steps ...plan.Step) intake.PlanGiven {
	t.Helper()
	return intake.PlanGiven{Issue: key, Generation: 1, GivenBy: architectClaim(t, pathsRoot), Plan: plan.Plan{ArtifactID: pathsArtifact, Version: 1,
		Tasks: []plan.Task{{ID: "a", Text: "Read the widget.", Steps: steps}}}}
}

// seedPlanOf records version 1 of an owner's plan of key at generation, given by givenBy, its one
// task a waiting at no act with the steps roles, each with the type of its role among Legion's
// own: a plan the store holds before key's first step, which no fact of the test gives.
func seedPlanOf(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, givenBy claim.Token, roles ...string) {
	t.Helper()
	version := record.PlanVersion{Issue: key, Generation: generation, Version: 1, ArtifactID: pathsArtifact, Source: record.PlanSourceOwner,
		GivenBy: givenBy, GivenAt: fixedNow.Add(-time.Hour)}
	task := record.PlanTask{Issue: key, Generation: generation, Task: "a", PlanVersion: 1, BlockText: "Read the widget.", State: record.TaskWaiting}
	builtin := plan.BuiltinRoles()
	steps := make([]record.PlanStep, len(roles))
	for i, role := range roles {
		steps[i] = record.PlanStep{Issue: key, Generation: generation, Task: "a", Position: i + 1, Role: role, Type: builtin[role].Type}
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPlan(context.Background(), tx, version, []record.PlanTask{task}, steps); err != nil {
			t.Fatalf("seed the plan of %s at generation %d: %v", key, generation, err)
		}
	})
}

// rootWhoseWaitEnded records key as a root of its own running tree, standing at admitted under its
// open design gate, as the pass left it that gave its row back while it waited on blocker
// (record.Store.ReleaseSlotForWait), with Dispatch now showing blocker done: its wait has ended,
// and its first step waits in the line.
func rootWhoseWaitEnded(t *testing.T, pool *pgxpool.Pool, key, blocker string) {
	t.Helper()
	seedSlotted(t, pool, key, "A")
	seedGate(t, pool, record.DesignGate{Issue: key, ArtifactID: "artifact-" + key, LatestVersion: 1, ApprovedVersion: new(1)})
	seedMirror(t, pool, key, "in_progress", blocker)
	seedMirror(t, pool, blocker, "done")
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().ReleaseSlotForWait(context.Background(), tx, key, fixedNow); err != nil {
			t.Fatalf("give back the row of %s for its wait: %v", key, err)
		}
	})
}

// releasedChild records child at admitted, released under root, whose design gate is open and
// which stands in accepting, where it holds no row (record.KeepsSlot): the child holds none until
// the daemon starts it, and its first step waits in the line.
func releasedChild(t *testing.T, pool *pgxpool.Pool, root, child string) {
	t.Helper()
	seedSlotted(t, pool, root, "A")
	seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: "artifact-" + root, LatestVersion: 1, ApprovedVersion: new(1)})
	acceptingRoot(t, pool, root)
	capMember(t, pool, root, child, phase.Admitted, "todo", "A1")
}

// A candidate of the waiting line standing at admitted, holding no row, whose promotion moves it
// into the first step of its plan, takes a row only where the phase that step runs as keeps one
// (record.PhaseKeepsSlot): the first step of the plan that step walks (workflow's planFirstStep) —
// the plan in force of its own generation, unless an earlier owner gave it, else the newest plan
// its owner gave, else the project's default steps. Where that first step is an acceptance, the
// promotion starts it on no row and the cap's one row goes to the root waiting behind it in the
// same pass; the pass after it gives nothing back and takes nothing, since the issue in accepting
// holds none. Read from the phase the candidate's record carries, admitted, the promotion gave the
// row to the acceptance and the next pass took it back.
//
// Each case is one shape of such a candidate: a root whose row a wait on another issue took, now
// that the wait has ended; a child its architect released; a child whose owner gave its plan before
// the daemon recorded it (generation 0); and a child whose owner gave none, in a project whose
// default steps start with the owner's acceptance. Its controls are a first step that keeps a
// slot — the owner's `implementer reviewer`, and a plan of the child's own generation an earlier
// owner gave, which the first step passes over for the default steps: each of those takes the row,
// and the root behind it waits.
func TestAFirstStepThatKeepsNoSlotStartsOnNoRow(t *testing.T) {
	const second, blocker = "LEGION-300", "LEGION-BLOCKER"
	acceptance := []plan.Step{{Role: "acceptance"}}
	for _, tc := range []struct {
		name string
		key  string
		seed func(*testing.T, *pgxpool.Pool)
		// defaults is the project's default_steps, nil for those of a project that names none.
		defaults []plan.Step
		// given is the plan the owner gives key in the pass the case is about (register_plan);
		// nil where none is given and that pass is a tick.
		given []plan.Step
		// stands is the phase key's first step moves it into, and takes whether that step takes
		// the cap's one row, which leaves second waiting.
		stands phase.Phase
		takes  bool
	}{
		{name: "a root whose wait on another issue has ended", key: pathsRoot, given: acceptance, stands: phase.Accepting,
			seed: func(t *testing.T, pool *pgxpool.Pool) { rootWhoseWaitEnded(t, pool, pathsRoot, blocker) }},
		{name: "a child its architect released", key: pathsChild, given: acceptance, stands: phase.Accepting,
			seed: func(t *testing.T, pool *pgxpool.Pool) { releasedChild(t, pool, pathsRoot, pathsChild) }},
		{name: "a child whose owner gave its plan before the daemon recorded it", key: pathsChild, stands: phase.Accepting,
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				seedPlanOf(t, pool, pathsChild, 0, architectClaim(t, pathsRoot), "acceptance")
				releasedChild(t, pool, pathsRoot, pathsChild)
			}},
		{name: "a child on default steps that start with the owner's acceptance", key: pathsChild, stands: phase.Accepting,
			defaults: []plan.Step{{Role: "acceptance", Key: "owner"}, {Role: "implementer"}},
			seed:     func(t *testing.T, pool *pgxpool.Pool) { releasedChild(t, pool, pathsRoot, pathsChild) }},
		{name: "control: a root whose first step keeps a slot", key: pathsRoot, given: []plan.Step{{Role: "implementer"}, {Role: "reviewer"}},
			stands: phase.Implementing, takes: true,
			seed: func(t *testing.T, pool *pgxpool.Pool) { rootWhoseWaitEnded(t, pool, pathsRoot, blocker) }},
		{name: "control: a child whose own generation holds an earlier owner's acceptance", key: pathsChild, stands: phase.Planning, takes: true,
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				releasedChild(t, pool, pathsRoot, pathsChild)
				seedPlanOf(t, pool, pathsChild, 1, architectClaim(t, "LEGION-900"), "acceptance")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quietRunningByDefault(t, 1, tc.defaults)
			tc.seed(t, pool)
			seedWaiting(t, pool, second, "B")

			if tc.given != nil {
				applyOwnersCall(t, pool, admission, "plan-v1", ownersSteps(t, tc.key, tc.given...))
			} else {
				tick(t, pool, admission, "first")
			}
			assertFirstStepOnItsRow(t, pool, tc.key, second, tc.stands, tc.takes, "after the pass that starts it")

			before := slots(t, pool)
			tick(t, pool, admission, "again")
			assertFirstStepOnItsRow(t, pool, tc.key, second, tc.stands, tc.takes, "after the pass after it")
			assertSlots(t, pool, before)
		})
	}
}

// assertFirstStepOnItsRow fails unless key stands in stands with no hold, and the cap's one row is
// key's where takes says its first step takes it, with second waiting in the line, and second's
// otherwise, second promoted and nothing waiting.
func assertFirstStepOnItsRow(t *testing.T, pool *pgxpool.Pool, key, second string, stands phase.Phase, takes bool, when string) {
	t.Helper()
	if got := issue(t, pool, key); got.Phase != stands || got.Hold != nil {
		t.Fatalf("%s %s stands in %s (hold %+v), want %s, the first step of its plan", key, when, got.Phase, got.Hold, stands)
	}
	if takes {
		assertRowHolders(t, pool, key)
		assertWaiting(t, pool, []string{second})
		return
	}
	assertRowHolders(t, pool, second)
	if got := issue(t, pool, second); got.Status != "in_progress" {
		t.Fatalf("%s %s shows %q, want in_progress: the row its first step left free promotes it", second, when, got.Status)
	}
	assertWaiting(t, pool, nil)
}
