package workflow

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// threeTasks writes key's plan at generation of three tasks, each with the steps `implementer
// reviewer tester`: a running at the implementer's step 1, b running at the reviewer's step 2, and
// c held at the tester's step 3. It answers the issue as the store reads it, whose running steps
// are the implementer's and the reviewer's, and the tester's held.
func threeTasks(t *testing.T, pool *pgxpool.Pool, key string, generation uint64) record.Issue {
	t.Helper()
	v := record.PlanVersion{Issue: key, Generation: generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: claim.Token(treeRoleToken(t, key, claim.RoleArchitect)), GivenAt: stepClock}
	roles := plan.BuiltinRoles()
	var tasks []record.PlanTask
	var steps []record.PlanStep
	for i, task := range []string{"a", "b", "c"} {
		tasks = append(tasks, record.PlanTask{Issue: key, Generation: generation, Task: task, PlanVersion: 1, BlockText: "Do " + task + ".",
			State: record.TaskRunning, Place: record.TaskPlace{Kind: record.PlaceStep, Position: i + 1}})
		for position, role := range []string{"implementer", "reviewer", "tester"} {
			steps = append(steps, record.PlanStep{Issue: key, Generation: generation, Task: task, Position: position + 1, Role: role,
				Type: roles[role].Type})
		}
	}
	var read *record.Issue
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		if err := store.PutPlan(t.Context(), tx, v, tasks, steps); err != nil {
			return err
		}
		if err := store.HoldTask(t.Context(), tx, key, generation, "c", ""); err != nil {
			return err
		}
		var err error
		read, err = store.Issue(t.Context(), tx, key)
		return err
	})
	if roles := runningRoles(*read); !slices.Equal(roles, []string{"implementer", "reviewer", "tester (held)"}) {
		t.Fatalf("premise: %s reads the running steps %v, want the implementer's, the reviewer's and the tester's held", key, roles)
	}
	return *read
}

// runningRoles is the role of each of issue's running steps, in order, a held one marked so.
func runningRoles(issue record.Issue) []string {
	var roles []string
	for _, step := range issue.Running {
		role := string(step.Role)
		if step.Held {
			role += " (held)"
		}
		roles = append(roles, role)
	}
	return roles
}

// owedEntry records role's phases row of key at entry 2 with that entry's task, and the role's
// claim given entry 1, so the claim stands behind its entry and is sent that entry's work
// (OwedEntry, SendOwedEntry), which reads no walk of the run.
func owedEntry(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) {
	t.Helper()
	token := seedPhaseWorker(t, pool, key, role, "")
	seedPhase(t, pool, record.PhaseRow{Issue: key, Role: role, Claim: token, Entry: 2, EntryTask: "the work of entry 2 of the " + string(role),
		EntryRaisedAt: stepClock})
	if _, err := pool.Exec(t.Context(), `update claims set generation = 1, given_entry = 1 where token = $1`, string(token)); err != nil {
		t.Fatalf("give the %s claim entry 1: %v", role, err)
	}
}

// Each path that resumes work reaches each running step of an issue: with task a at the
// implementer's step, task b at the reviewer's and task c held at the tester's, the boot's resume
// by rule names the suspended claims of the implementer and the reviewer as owners and not the
// tester's (suspendedPhaseOwners); the ready of each claim sends the implementer and the reviewer
// the work they are owed and sends the tester nothing (readyWorkerTask); the silence rule names
// the implementer and the reviewer as unprompted workers and not the tester (unpromptedPhaseWorker,
// which phaseWorkerRecord's guard shares); and a CI settlement wakes the released claims of the
// implementer and the reviewer and not the tester's (wakeOnSettlement). An issue with one task
// reaches its one role, as TestTheResumePathsGiveTheRunningDeploymentRoleItsWork holds. The wake
// of the listener names a claim by its token and reads no step, so it is not among them.
//
// The boot is held at its selection: its start of each owner reads the walk of the issue's run,
// which a plan of several tasks has none of yet.
//
// The fault it catches is a running step that no resume path reaches (its control: OpenStepsOf
// answering its first step alone, which gives the reviewer nothing).
func TestEachResumePathReachesEachRunningStep(t *testing.T) {
	const key = "LEGION-209"
	stand := func(t *testing.T, pool *pgxpool.Pool) record.Issue {
		t.Helper()
		seedIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: key, Project: "LEGION", Title: "a root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true})
		seedSlot(t, pool, record.Slot{Issue: key, Index: 0})
		return threeTasks(t, pool, key, 1)
	}

	t.Run("the boot's resume by rule", func(t *testing.T) {
		pool := migratedPool(t)
		stand(t, pool)
		project, err := claim.ProjectToken("LEGION")
		if err != nil {
			t.Fatal(err)
		}
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester} {
			seedRuleSuspendedOwner(t, pool, project, key, role)
		}
		engine := testEngine(config.DesignGateRootIssues, nil)
		var owners phaseOwners
		if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
			var err error
			owners, err = engine.suspendedPhaseOwners(t.Context(), tx)
			return err
		}); err != nil {
			t.Fatalf("read the boot's owners: %v", err)
		}
		var roles []claim.Role
		for _, owner := range owners.suspended {
			roles = append(roles, owner.role)
		}
		if want := []claim.Role{claim.RoleImplementer, claim.RoleReviewer}; !slices.Equal(roles, want) {
			t.Fatalf("the boot's owners of %s = %v, want %v: each running step's worker, and no held step's", key, roles, want)
		}
	})

	// Each running step's start names the claim of its step's agent, and the outbox takes out a
	// pending start only for a start that names the same claim (record.Store.EnqueueStart), so one
	// ready's start takes out no other step's: the implementer's ready queues its start, the
	// reviewer's queues its own beside it, each naming its claim and the phase its step runs as and
	// carrying the work that claim is owed, and the tester's ready queues nothing.
	t.Run("the ready of each claim", func(t *testing.T) {
		pool := migratedPool(t)
		stand(t, pool)
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester} {
			owedEntry(t, pool, key, role)
		}
		// started is each start of role's in the outbox, oldest first: the claim it names, the
		// phase it runs as and the work it carries.
		started := func(role claim.Role) []string {
			var starts []string
			for _, start := range roleStartsOf(t, pool, key, role) {
				starts = append(starts, fmt.Sprintf("naming %s at %s carrying %q", start.Claim, start.Phase, start.Task))
			}
			return starts
		}
		owed := func(role claim.Role, at phase.Phase) []string {
			return []string{fmt.Sprintf("naming %s at %s carrying %q", treeRoleToken(t, key, role), at, "the work of entry 2 of the "+string(role))}
		}
		implementer, reviewer := owed(claim.RoleImplementer, phase.Implementing), owed(claim.RoleReviewer, phase.Reviewing)
		for _, ready := range []struct {
			role   claim.Role
			starts map[claim.Role][]string
		}{
			{claim.RoleImplementer, map[claim.Role][]string{claim.RoleImplementer: implementer}},
			{claim.RoleReviewer, map[claim.Role][]string{claim.RoleImplementer: implementer, claim.RoleReviewer: reviewer}},
			{claim.RoleTester, map[claim.Role][]string{claim.RoleImplementer: implementer, claim.RoleReviewer: reviewer}},
		} {
			if _, err := intake.ApplyFact(t.Context(), pool, "supervise", "supervise:ready:"+string(ready.role), intake.ClaimReady{Issue: key, Role: ready.role, Launch: 2},
				testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("apply the ready of the %s: %v", ready.role, err)
			}
			for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester} {
				if got := started(role); !slices.Equal(got, ready.starts[role]) {
					t.Fatalf("after the ready of the %s the %s's starts of %s are %q, want %q: each running step's owed work on its own claim, and no held step's",
						ready.role, role, key, got, ready.starts[role])
				}
			}
		}
	})

	t.Run("the silence of each worker", func(t *testing.T) {
		pool := migratedPool(t)
		stored := stand(t, pool)
		for role, want := range map[claim.Role]bool{claim.RoleImplementer: true, claim.RoleReviewer: true, claim.RoleTester: false} {
			s := aSilentImplementer()
			s.issue.Running, s.role = stored.Running, role
			if got := unpromptedPhaseWorker(s.issue, s.root, s.gate, s.role, s.run); got != want {
				t.Errorf("the %s, up between turns with no task, is named the unprompted worker of a running step %t, want %t", role, got, want)
			}
		}
	})

	t.Run("a CI settlement's wake", func(t *testing.T) {
		rig := newWakeRig(t)
		threeTasks(t, rig.pool, "LEGION-208", wakeIssueGeneration)
		seedReleasedClaim(t, rig.pool, claim.RoleReviewer, "session-reviewer")
		seedReleasedClaim(t, rig.pool, claim.RoleTester, "session-tester")

		if _, err := intake.ApplyFact(t.Context(), rig.pool, "api", "settled", settlement("head1", "build"), rig.engine, admissionStub{}); err != nil {
			t.Fatalf("apply the settlement: %v", err)
		}

		var woken []claim.Role
		for _, start := range wakeStarts(t, rig.pool) {
			woken = append(woken, start.Role)
		}
		slices.Sort(woken)
		if want := []claim.Role{claim.RoleImplementer, claim.RoleReviewer}; !slices.Equal(woken, want) {
			t.Fatalf("the settlement woke %v, want %v: each running step's released worker, and no held step's", woken, want)
		}
	})
}

// The idle release keeps the claim of each running step: with task a at the implementer's step
// and task b at the reviewer's while the issue stands in implementing, the phase reminder of each
// one's claim, due now, holds the pod (idlePhaseOpen over the view's running steps), and a due
// reminder of the tester's claim, which runs no step, holds nothing, so the pod is released. The
// checks' message reaches the agent of each step that takes work and no held step's: with a at the
// reviewer's step and c held at the tester's, the message of a settlement goes to the architect,
// the reviewer and the implementer, and not to the tester.
//
// The fault it catches is a reader of the issue's phase alone, which takes no step of another
// phase for its own (its controls: idlePhaseOpen on RoleFor(view.Phase), which releases the
// reviewer's claim under its due reminder, and the checks' reader on RunningAt of the issue's
// phase, which tells the held tester and not the reviewer).
func TestTheIdleReleaseTakesNoClaimOfARunningStep(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	view := pods.IdleView{Issue: "LEGION-208", Tree: "LEGION-208", Found: true, Phase: phase.Implementing, Status: "in_progress",
		Generation: 3, Root: pods.IdleRoot{Phase: phase.Implementing, Status: "in_progress", Generation: 3}, Planned: true,
		Running: []pods.RunningRow{
			{Task: "a", Kind: "step", Position: 1, Role: "implementer", At: 1},
			{Task: "b", Kind: "step", Position: 2, Role: "reviewer", At: 2},
		}}
	idleClaim := func(role claim.Role, due bool) pods.IdleClaim {
		c := pods.IdleClaim{Token: claim.Token("legion-legion-208-" + string(role)), Role: role, State: supervise.StateIdle,
			ServingGeneration: 3, PluginContract: 1, ActivityAt: now.Add(-time.Hour), LastTurnAt: now.Add(-2 * time.Hour),
			UpdatedAt: now.Add(-2 * time.Hour)}
		if due {
			c.ActivityNextReminderAt = now.Add(-time.Minute)
		}
		return c
	}
	for _, tc := range []struct {
		name   string
		claims []pods.IdleClaim
		hold   IdleHold
	}{
		{"the implementer's reminder is due", []pods.IdleClaim{idleClaim(claim.RoleImplementer, true), idleClaim(claim.RoleReviewer, false)}, IdleHoldReminder},
		{"the reviewer's reminder is due", []pods.IdleClaim{idleClaim(claim.RoleImplementer, false), idleClaim(claim.RoleReviewer, true)}, IdleHoldReminder},
		{"the tester's reminder is due", []pods.IdleClaim{idleClaim(claim.RoleImplementer, false), idleClaim(claim.RoleReviewer, false), idleClaim(claim.RoleTester, true)}, IdleHoldNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := view
			view.Claims = tc.claims
			decision := IdleRelease(view, now, time.Hour, true, false, 1)
			if decision.Hold != tc.hold || decision.Release != (tc.hold == IdleHoldNone) {
				t.Fatalf("the idle release = %+v, want the hold %s (a release where none)", decision, tc.hold)
			}
		})
	}

	t.Run("the checks' message", func(t *testing.T) {
		pool := migratedPool(t)
		seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
			Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "U"})
		stored := threeTasks(t, pool, "LEGION-208", 1)
		seedRecord(t, pool, func(tx pgx.Tx) error {
			return record.NewStore().MoveTask(t.Context(), tx, stored.Key, stored.Generation, "a", record.TaskPlace{}, record.TaskWaiting)
		})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester} {
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: claim.Token(string(role) + "-claim")})
		}

		// The fact is applied without the boot's open of the runs no start counted, which reads
		// the walk of each issue's run, and a plan of several tasks has none yet.
		if result, err := intake.ApplyFact(t.Context(), pool, "api", "settled", settledAt("head", 1, "unit"), mergingEngine(), admissionStub{}); err != nil || result.Refusal != nil {
			t.Fatalf("apply the settlement: %+v, %v", result.Refusal, err)
		}

		body := toldBody("CI settled at head: \"build\" succeeded, \"unit\" failed")
		assertRoleMessages(t, pool, "LEGION-208", []record.RoleMessage{
			{Role: roleToken(t, claim.RoleArchitect), Body: body},
			{Role: roleToken(t, claim.RoleReviewer), Body: body},
			{Role: roleToken(t, claim.RoleImplementer), Body: body},
		})
	})
}
