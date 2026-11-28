package admit

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// checker is a role the deployment defines: no phase of Legion's is its, so a step of it runs as
// the phase step, and the plan rows name it as the issue's running role (record.Issue.StepRole).
const checker claim.Role = "checker"

// runAtCheckerStep writes key's plan `implementer checker` at generation, its task a running at
// step 2, so the role the store reads with key is the checker wherever key stands at step or is
// held from it.
func runAtCheckerStep(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		project, err := claim.ProjectToken(testProject)
		if err != nil {
			t.Fatal(err)
		}
		architect, err := claim.NewToken(project, key, claim.RoleArchitect)
		if err != nil {
			t.Fatal(err)
		}
		v := record.PlanVersion{Issue: key, Generation: generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner, GivenBy: architect, GivenAt: fixedNow}
		task := record.PlanTask{Issue: key, Generation: generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: record.TaskWaiting}
		steps := []record.PlanStep{
			{Issue: key, Generation: generation, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: key, Generation: generation, Task: "a", Position: 2, Role: string(checker), Type: "testing"},
		}
		for _, put := range []error{
			records.PutPlan(ctx, tx, v, []record.PlanTask{task}, steps),
			records.MoveTask(ctx, tx, key, generation, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning),
		} {
			if put != nil {
				t.Fatalf("seed the checker's step of %s: %v", key, put)
			}
		}
	})
}

// standInFinishingTree moves key, a member of the finishing tree (seedFinishingTree), to stand at
// step, or held from it, with the checker running its step.
func standInFinishingTree(t *testing.T, pool *pgxpool.Pool, key string, held bool) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		member, err := records.Issue(ctx, tx, key)
		if err != nil || member == nil {
			t.Fatalf("read %s: %v", key, err)
		}
		member.Phase, member.Hold, member.Status, member.DispatchStatus = phase.Step, nil, "in_progress", "in_progress"
		if held {
			member.Phase, member.Hold = phase.Held, &record.Hold{From: phase.Step}
		}
		if err := records.PutIssue(ctx, tx, *member); err != nil {
			t.Fatalf("put %s at step: %v", key, err)
		}
	})
	runAtCheckerStep(t, pool, key, 1)
}

// The admission reads the role that works an issue with the issue (record.Issue.RoleAt,
// workflow.OpenRoleOf), so at step it is the role of the running step, which no phase of
// Legion's names (phase.RoleFor answers none for step):
//   - a re-admission's promotion (startMidPhaseMembers) starts the checker of a member at step
//     with its phase's work;
//   - the roles a promotion brings back (promotedRoles) take the checker of a member at step and
//     of one held from it, after Legion's six, so a checker that has run on a held member comes
//     back with no task, as each role of Legion's that has run on a held member does;
//   - the tick's silent list (silentWorkers, through phaseWorkerOf) names a silent checker of an
//     issue at step, and the entry repair (owedEntryRoles, through the same selection) sends the
//     checker of a slotted issue at step the work of the entry its phases row names;
//   - the stalled-tree notice (stalledTreeReason) gives the checker the turn of a member at step
//     and of one held from it, as it gives the tester the turn of a member at testing.
//
// The promotion of a member held from step is read through promotedRoles alone: the promotion
// then starts that member's step record again (workflow.Engine.StartStepRecord), whose agent is
// still read by phase (workflow's startAgent) and is none for step until the workflow's own task
// reads the role there.
func TestTheAdmissionReadsTheRunningDeploymentRole(t *testing.T) {
	t.Run("a promotion starts the checker", func(t *testing.T) {
		pool, admission, engine := seedFinishingTree(t)
		const atStep = planning
		closeFinishingTree(t, pool, admission, engine)
		standInFinishingTree(t, pool, atStep, false)
		reopenFinishingTree(t, pool, admission, engine)

		if root := issue(t, pool, finishedRoot); root.Generation != 2 || root.Lingers() {
			t.Fatalf("the reopened root = %#v, want generation 2, no longer lingering: the promotion this case reads did not run", root)
		}
		if got := issue(t, pool, atStep); got.Phase != phase.Step || got.StepRole != checker {
			t.Fatalf("%s after the promotion = %s with StepRole %q, want step with checker", atStep, got.Phase, got.StepRole)
		}
		starts := roleStarts(t, pool, atStep, checker)
		if len(starts) != 1 || starts[0].Phase != phase.Step || starts[0].Task == "" {
			t.Errorf("the promotion's starts of %s's checker = %+v, want one at step carrying its phase's work", atStep, starts)
		}
	})

	t.Run("the roles a promotion brings back", func(t *testing.T) {
		withChecker := append(slices.Clone(claim.Roles), checker)
		for _, c := range []struct {
			name   string
			member record.Issue
			want   []claim.Role
		}{
			{"at step", record.Issue{Phase: phase.Step, StepRole: checker}, withChecker},
			{"held from step", record.Issue{Phase: phase.Held, Hold: &record.Hold{From: phase.Step}, StepRole: checker}, withChecker},
			{"at testing", record.Issue{Phase: phase.Testing, StepRole: claim.RoleTester}, claim.Roles},
			{"held from testing", record.Issue{Phase: phase.Held, Hold: &record.Hold{From: phase.Testing}}, claim.Roles},
			{"at step with no plan rows", record.Issue{Phase: phase.Step}, claim.Roles},
		} {
			if got := promotedRoles(c.member); !slices.Equal(got, c.want) {
				t.Errorf("%s: the roles a promotion brings back = %v, want %v", c.name, got, c.want)
			}
		}
		if len(claim.Roles) != 6 {
			t.Errorf("claim.Roles = %v after promotedRoles, want Legion's six: promotedRoles must not append to it", claim.Roles)
		}
	})

	t.Run("the silent list names the checker", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 2)
		admission.UseWorkerSilence(silenceBound, backgroundBound)
		registerController(t, pool)
		const key = "LEGION-720"
		working := seedQuietRoot(t, pool, key, "A", phase.Step)
		runAtCheckerStep(t, pool, key, working.Generation)
		silentStep := fixedNow.Add(-2 * time.Hour)
		steppedAt(t, pool, putWorkerClaim(t, pool, key, key, checker, working.Generation, supervise.StateReady, ""), silentStep)

		tick(t, pool, admission, "tick-1")

		silent := tickNotice(t, pool).Silent
		if len(silent) != 1 || silent[0].Issue != key || silent[0].Role != checker || !silent[0].LastStep.Equal(silentStep) {
			t.Fatalf("the tick names %#v as silent, want %s's checker, last stepped at %s: the role that runs the issue's step", silent, key, silentStep)
		}
	})

	t.Run("the entry repair sends the checker its entry's work", func(t *testing.T) {
		pool := migratedPool(t)
		const root, key = "LEGION-740", "LEGION-741"
		seedTreeAndMember(t, pool, root, key, phase.Step, 1, false)
		takeSlot(t, pool, key)
		runAtCheckerStep(t, pool, key, 1)
		project, err := claim.ProjectToken(testProject)
		if err != nil {
			t.Fatal(err)
		}
		token, err := claim.NewToken(project, key, checker)
		if err != nil {
			t.Fatal(err)
		}
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: key, Role: checker, Claim: token,
				Entry: seededEntry, EntryTask: seededEntryTask(key), EntryRaisedAt: fixedNow}); err != nil {
				t.Fatalf("record the checker's phases row of %s: %v", key, err)
			}
		})

		tick(t, pool, quiet(t, 1), "converge-pass-1")

		starts := convergeStarts(t, pool, key)
		if want := (convergeStart{role: checker, task: seededEntryTask(key), entry: seededEntry}); len(starts) != 1 ||
			(convergeStart{role: starts[0].role, task: starts[0].task, entry: starts[0].entry}) != want {
			t.Fatalf("the start rows of %s = %+v, want the one start of its checker carrying the work of entry %d: the checker holds no claim, so it was given nothing", key, starts, seededEntry)
		}
	})

	t.Run("the stalled text gives the checker the turn", func(t *testing.T) {
		members := []record.Issue{
			{Key: "LEGION-730", Phase: phase.Step, StepRole: checker},
			{Key: "LEGION-731", Phase: phase.Held, Hold: &record.Hold{From: phase.Step}, StepRole: checker},
			{Key: "LEGION-732", Phase: phase.Testing},
		}
		reason := stalledTreeReason("LEGION-730", record.TreeQuiet{Quiet: 3 * time.Hour}, members, nil)
		for _, want := range []string{
			"\nLEGION-730 is in step. The checker has the turn. ",
			"\nLEGION-731 is in held. The checker has the turn. ",
			"\nLEGION-732 is in testing. The tester has the turn. ",
		} {
			if !strings.Contains(reason, want) {
				t.Errorf("the stalled-tree notice is\n%s\nwant it to hold %q", reason, want)
			}
		}
	})
}
