package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// relaunchProber is the role the plan of stepStartKey defines in the tests of a changed role.
const relaunchProber claim.Role = "prober"

// seedProberVersion records version v of the plan of stepStartKey at generation 2: one task whose
// one step is relaunchProber, of type and model and prompt as given, running at that step, with
// the prober's phases row on entry v, the entry the start of that version's step carries. The
// version is given now.
func seedProberVersion(t *testing.T, pool *store.Pool, records record.Store, v int, typ, model, prompt string) {
	t.Helper()
	seedProberVersionAt(t, pool, records, v, typ, model, prompt, time.Now().UTC())
}

// seedProberVersionAt is seedProberVersion with the version given at givenAt.
func seedProberVersionAt(t *testing.T, pool *store.Pool, records record.Store, v int, typ, model, prompt string, givenAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		version := record.PlanVersion{Issue: stepStartKey, Generation: 2, Version: v, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
			GivenBy: mustClaimToken(t, stepStartKey, claim.RoleArchitect), GivenAt: givenAt}
		task := record.PlanTask{Issue: stepStartKey, Generation: 2, Task: "a", PlanVersion: v, BlockText: "Probe the widget.", State: record.TaskWaiting}
		steps := []record.PlanStep{{Issue: stepStartKey, Generation: 2, Task: "a", Position: 1, Role: string(relaunchProber), Type: stepplan.Type(typ)}}
		if err := records.PutPlan(ctx, tx, version, []record.PlanTask{task}, steps); err != nil {
			return err
		}
		if err := records.PutPlanRoles(ctx, tx, stepStartKey, 2, v, []record.PlanRole{{Issue: stepStartKey, Generation: 2, Role: string(relaunchProber),
			PlanVersion: v, Type: typ, Model: model, Prompt: prompt}}); err != nil {
			return err
		}
		if err := records.MoveTask(ctx, tx, stepStartKey, 2, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 1}, record.TaskRunning); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: stepStartKey, Role: relaunchProber, Entry: int64(v),
			EntryTask: "Probe it. Issue: LEGION-208. Phase: step.", EntryRaisedAt: time.Now()})
	}); err != nil {
		t.Fatalf("seed version %d of the plan of %s: %v", v, stepStartKey, err)
	}
}

// poolPlanRole reads the role of an issue's plan in force from pool, as the daemon's reader does.
func poolPlanRole(pool *store.Pool, records record.Store) func(context.Context, string, claim.Role) (record.PlanRole, bool, error) {
	return func(ctx context.Context, issue string, role claim.Role) (record.PlanRole, bool, error) {
		var defined record.PlanRole
		var found bool
		err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			defined, found, err = records.PlanRoleOf(ctx, tx, issue, string(role))
			return err
		})
		return defined, found, err
	}
}

// Version 1 of the plan defines prober (type testing, model m-a, prompt P1), whose step's start
// launches its claim with P1 and m-a; the prober's turn ends and its process stays up, idle. The
// owner then gives version 2, which changes the role block (type implementation, model m-b,
// prompt P2), and the task's step of prober starts again. The claim still runs the process
// launched with P1 and m-a, so the start ends that process with its session kept and the claim is
// relaunched on its session with P2 and m-b: no task of version 2 goes to a process of version 1,
// whose next mint would hand it the App of the new type. The control is a version 2 that changes
// no part of the role: the start hands the running process its task and relaunches nothing.
//
// Before the fix the start found a live claim of the same role and handed it the task, and the
// prober went on as P1 on m-a.
func TestAStartAfterAChangedRoleBlockRelaunchesTheRolesLiveClaim(t *testing.T) {
	const p1, p2 = "# Prober\n\nProbe the change, first wording.", "# Prober\n\nProbe the change, second wording."
	for _, tc := range []struct {
		name               string
		typ, model, prompt string
		relaunched         bool
	}{
		{"version 2 changes the role's type, model and prompt", "implementation", "m-b", p2, true},
		{"the control: version 2 leaves the role as it was", "testing", "m-a", p1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: stepStartKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: stepStartKey, Title: "Workflow", Phase: phase.Step, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			openTree(t, pool, "LEGION", stepStartKey)
			holdSlot(t, pool, stepStartKey)
			seedProberVersion(t, pool, records, 1, "testing", "m-a", p1)
			composer, err := prompts.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			prober := specs{stateDir: t.TempDir(), project: "legion", prompts: composer, planRole: poolPlanRole(pool, records)}
			sup.deps.Specs = prober
			token := mustClaimToken(t, stepStartKey, relaunchProber)
			conn := fake.NewConn()
			sup.deps.Conns.(*fake.Conns).Register(token, conn)
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, roleDefinition: prober.roleDefinition,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}

			first := mustOutboxRow(t, stepStartKey, record.SuperviseRequest{Op: "start", Tree: stepStartKey, Role: relaunchProber, Generation: 2,
				Phase: phase.Step, Task: "Probe it. Issue: LEGION-208. Phase: step.", Entry: 1}, time.Now())
			first.ID = 95
			if err := runner.execute(ctx, first); err != nil {
				t.Fatalf("the prober's start of version 1: %v", err)
			}
			machine, found := sup.Machine(token)
			if !found {
				t.Fatal("the prober's start of version 1 created no claim")
			}
			machine.Wait()
			launches := append(rt.CallsOf("Spawn"), rt.CallsOf("Resume")...)
			if len(launches) != 1 || launches[0].Spec.Model != "m-a" || !composedHolds(t, launches[0].Spec, p1) {
				t.Fatalf("the launches of version 1 = %d, the first on %q; want one on m-a with P1: the case's premise", len(launches), modelOf(launches))
			}
			generation := machine.Claim().Generation
			for _, ev := range []supervise.Event{
				supervise.StreamHello{Claim: token, Generation: generation},
				supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-prober", SessionFile: "/tmp/prober.jsonl", CapabilityHash: []byte("prober")},
				supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-prober"},
			} {
				if err := machine.Handle(ctx, ev); err != nil {
					t.Fatalf("bring the prober's agent up: handle %T: %v", ev, err)
				}
			}
			machine.Wait()
			if pending := machine.Claim().Pending; pending != nil {
				for _, ev := range []supervise.Event{
					supervise.StreamTurnStart{Claim: token, DeliveryID: pending.ID},
					supervise.StreamTurnEnd{Claim: token},
					supervise.StreamPromptResult{Claim: token, DeliveryID: pending.ID, AgentInvoked: true},
				} {
					if err := machine.Handle(ctx, ev); err != nil {
						t.Fatalf("the prober's turn of version 1: handle %T: %v", ev, err)
					}
				}
			}
			machine.Wait()
			if held := machine.Claim(); held.RunsNothing() || held.Pending != nil || held.State == supervise.StateWorking {
				t.Fatalf("the prober after its turn of version 1 is %s holding %+v, want its process up, idle, with no task: the case's premise", held.State, held.Pending)
			}

			seedProberVersion(t, pool, records, 2, tc.typ, tc.model, tc.prompt)
			second := mustOutboxRow(t, stepStartKey, record.SuperviseRequest{Op: "start", Tree: stepStartKey, Role: relaunchProber, Generation: 2,
				Phase: phase.Step, Task: "Probe it again. Issue: LEGION-208. Phase: step.", Entry: 2}, time.Now())
			second.ID = 96
			if err := runner.execute(ctx, second); err != nil {
				t.Fatalf("the prober's start of version 2: %v", err)
			}
			machine.Wait()
			relaunches := append(rt.CallsOf("Spawn"), rt.CallsOf("Resume")...)[1:]
			if !tc.relaunched {
				if len(relaunches) != 0 {
					t.Fatalf("a version that leaves the role as it was relaunched the prober %d times (on %q), want none", len(relaunches), modelOf(relaunches))
				}
				return
			}
			if len(relaunches) != 1 {
				t.Fatalf("the prober's start of version 2 relaunched its claim %d times, want once: its process runs P1 on m-a, which version 2 changed", len(relaunches))
			}
			spec := relaunches[0].Spec
			if spec.Model != "m-b" || !composedHolds(t, spec, p2) || composedHolds(t, spec, p1) || spec.ResumeSessionFile != "/tmp/prober.jsonl" {
				t.Fatalf("the prober was relaunched on %q, its prompt holding P2 %t and P1 %t, on the session %q; want m-b with P2 alone, on its own session /tmp/prober.jsonl",
					spec.Model, composedHolds(t, spec, p2), composedHolds(t, spec, p1), spec.ResumeSessionFile)
			}
		})
	}
}

// composedHolds says whether the prompt spec composes holds text.
func composedHolds(t *testing.T, spec runtime.SpawnSpec, text string) bool {
	t.Helper()
	composed, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		t.Fatalf("compose the prompt of %s: %v", spec.Claim, err)
	}
	return strings.Contains(composed, text)
}

// modelOf is the model of each launch, in order.
func modelOf(launches []fake.Call) []string {
	models := make([]string, len(launches))
	for i, launch := range launches {
		models[i] = launch.Spec.Model
	}
	return models
}

// A daemon that starts while a prober's agent runs restores the prober's claim over that process,
// and the start of the prober's next step compares the definition of the role the process was
// launched with, which the claim keeps over the restart (supervise.Claim.LaunchedDefinition), with
// the definition of the plan in force. The earlier daemon launched the prober under version 1 of
// the plan (type testing, model m-a, prompt P1). Where version 2 changed the role block (type
// implementation, model m-b, prompt P2), whenever it was given — after the agent came up, or while
// it came up, after its launch was built and before it said it was ready — the start relaunches
// the process once, on its own session, with the definition of the plan in force, and its line
// says the definition changed. Where the block is unchanged, the start hands the running process
// its task and relaunches nothing, whether or not the claim records a ready time — unless a
// release that keeps no definition launched the process after this one did, which a rollback that
// skipped both ways back leaves: that launch moved the claim's generation on and left the
// definition as this release wrote it, so the definition names a process that no longer runs, and
// the start relaunches the process once, with the plan's definition, its line saying why.
//
// Before the claim kept the definition, the restarted daemon compared the time the plan in force
// was given with the agent's last ready: a version given while the agent came up relaunched
// nothing, so a process of P1 on m-a ran the step while its mints acted as the implement App, and
// a claim that recorded no ready time was relaunched though nothing had changed. Before the claim
// kept the generation of that launch too, the definition the release before left was taken as the
// running process's, and nothing was relaunched.
func TestARestartedDaemonRelaunchesAPlanRolesAgentWhoseLaunchRanAnotherDefinition(t *testing.T) {
	const p1, p2 = "# Prober\n\nProbe the change, first wording.", "# Prober\n\nProbe the change, second wording."
	const changed = "the definition of its role prober on LEGION-208 changed since its process launched"
	const unrecorded = "a release that records no definition of its role prober launched its process on LEGION-208"
	for _, tc := range []struct {
		name string
		// v2 is when version 2 changes the block: "after" the agent came up, while it was
		// "booting", or "" for no version 2, the task entering the prober's step again.
		v2 string
		// ready is how long before the restart the agent last said it was ready, 0 for none.
		ready time.Duration
		// releaseBefore is whether a release that keeps no definition launched the process after
		// the earlier daemon did, moving the claim's generation on and leaving its definition.
		releaseBefore bool
		relaunched    bool
	}{
		{"version 2 was given after the agent came up", "after", time.Hour, false, true},
		{"version 2 was given while the agent came up", "booting", time.Hour, false, true},
		{"the control: the block is unchanged", "", time.Hour, false, false},
		{"the block is unchanged and the claim records no ready time", "", 0, false, false},
		{"the block is unchanged and a release that keeps no definition launched the process since", "", time.Hour, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			base := time.Now().UTC().Truncate(time.Second)
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: stepStartKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: stepStartKey, Title: "Workflow", Phase: phase.Step, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			openTree(t, pool, "LEGION", stepStartKey)
			holdSlot(t, pool, stepStartKey)
			seedProberVersionAt(t, pool, records, 1, "testing", "m-a", p1, base.Add(-2*time.Hour))
			composer, err := prompts.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			prober := specs{stateDir: t.TempDir(), project: "legion", prompts: composer, planRole: poolPlanRole(pool, records)}
			provision := func(context.Context, workspace.Request) (workspace.Workspace, error) {
				return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
			}
			token := mustClaimToken(t, stepStartKey, relaunchProber)

			// The earlier daemon: it launches the prober under version 1, the agent comes up, and its
			// turn ends with the process up and idle.
			earlier, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			earlier.deps.Specs = prober
			earlier.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
			first := mustOutboxRow(t, stepStartKey, record.SuperviseRequest{Op: "start", Tree: stepStartKey, Role: relaunchProber, Generation: 2,
				Phase: phase.Step, Task: "Probe it. Issue: LEGION-208. Phase: step.", Entry: 1}, time.Now())
			first.ID = 95
			if err := (&outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: earlier,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now, provision: provision}).execute(ctx, first); err != nil {
				t.Fatalf("the earlier daemon's start of the prober: %v", err)
			}
			machine, found := earlier.Machine(token)
			if !found {
				t.Fatal("the earlier daemon's start of the prober created no claim")
			}
			machine.Wait()
			if tc.v2 == "booting" {
				// Its launch is built under version 1 and its agent is not up yet.
				seedProberVersionAt(t, pool, records, 2, "implementation", "m-b", p2, base.Add(-90*time.Minute))
			}
			generation := machine.Claim().Generation
			for _, ev := range []supervise.Event{
				supervise.StreamHello{Claim: token, Generation: generation},
				supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-prober", SessionFile: "/tmp/prober.jsonl", CapabilityHash: []byte("prober")},
				supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-prober"},
			} {
				if err := machine.Handle(ctx, ev); err != nil {
					t.Fatalf("bring the prober's agent up: handle %T: %v", ev, err)
				}
			}
			machine.Wait()
			if pending := machine.Claim().Pending; pending != nil {
				for _, ev := range []supervise.Event{
					supervise.StreamTurnStart{Claim: token, DeliveryID: pending.ID},
					supervise.StreamTurnEnd{Claim: token},
					supervise.StreamPromptResult{Claim: token, DeliveryID: pending.ID, AgentInvoked: true},
				} {
					if err := machine.Handle(ctx, ev); err != nil {
						t.Fatalf("the prober's turn under the earlier daemon: handle %T: %v", ev, err)
					}
				}
			}
			machine.Wait()
			launched := append(rt.CallsOf("Spawn"), rt.CallsOf("Resume")...)
			if held := machine.Claim(); held.RunsNothing() || held.Pending != nil || held.State == supervise.StateWorking || len(launched) != 1 || launched[0].Spec.Model != "m-a" {
				t.Fatalf("the prober under the earlier daemon is %s holding %+v after %d launches on %q, want its process up, idle, with no task, launched once on m-a: the case's premise",
					held.State, held.Pending, len(launched), modelOf(launched))
			}
			earlier.stop()

			// The store keeps the claim over the restart, with the agent's last ready as the case
			// gives it. A launch of a release that keeps no definition writes the claim's next
			// generation and leaves the definition column as it stands.
			claims := earlier.deps.Store.(*outboxClaimStore)
			claims.mu.Lock()
			stored := claims.claims[token]
			claims.mu.Unlock()
			stored.LastReady = time.Time{}
			if tc.ready != 0 {
				stored.LastReady = base.Add(-tc.ready)
			}
			if tc.releaseBefore {
				stored.Generation++
			}
			if err := claims.PutClaim(ctx, stored); err != nil {
				t.Fatal(err)
			}
			switch tc.v2 {
			case "after":
				seedProberVersionAt(t, pool, records, 2, "implementation", "m-b", p2, base.Add(-30*time.Minute))
			case "":
				if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
					return records.PutPhase(ctx, tx, record.PhaseRow{Issue: stepStartKey, Role: relaunchProber, Entry: 2,
						EntryTask: "Probe it again. Issue: LEGION-208. Phase: step.", EntryRaisedAt: time.Now()})
				}); err != nil {
					t.Fatalf("enter the prober's step again under version 1: %v", err)
				}
			}

			// This daemon: its supervisor restores the claim over the process the earlier one
			// launched, which still runs, and has built no launch of it.
			logs := &syncBuffer{}
			restarted, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			restarted.deps.Runtime, restarted.deps.Store, restarted.deps.Conns = rt, claims, earlier.deps.Conns
			restarted.deps.Specs, restarted.deps.Log = prober, jsonLogger(logs)
			if _, err := restarted.restore(ctx, []supervise.Claim{stored}); err != nil {
				t.Fatalf("restore the prober's claim: %v", err)
			}
			machine, found = restarted.Machine(token)
			if !found || machine.Claim().RunsNothing() || machine.Claim().LaunchedDefinition == "" {
				t.Fatalf("the restore made a machine of the prober's claim %t, running a process, with its launch's definition %q; want one, running, with the definition of version 1: the case's premise",
					found, machine.Claim().LaunchedDefinition)
			}
			second := mustOutboxRow(t, stepStartKey, record.SuperviseRequest{Op: "start", Tree: stepStartKey, Role: relaunchProber, Generation: 2,
				Phase: phase.Step, Task: "Probe it again. Issue: LEGION-208. Phase: step.", Entry: 2}, time.Now())
			second.ID = 96
			if err := (&outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: restarted, roleDefinition: prober.roleDefinition,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now, provision: provision}).execute(ctx, second); err != nil {
				t.Fatalf("the restarted daemon's start of the prober's next step: %v", err)
			}
			machine.Wait()
			relaunches := append(rt.CallsOf("Spawn"), rt.CallsOf("Resume")...)[1:]
			if !tc.relaunched {
				if len(relaunches) != 0 {
					t.Fatalf("a restart over an unchanged block relaunched the prober %d times (on %q), want none", len(relaunches), modelOf(relaunches))
				}
				if lines := logs.String(); strings.Contains(lines, "since its process launched") || strings.Contains(lines, "records no definition of its role") {
					t.Fatalf("the restarted daemon logged a relaunch's reason over an unchanged block:\n%s", lines)
				}
				return
			}
			why, model, prompt, other := changed, "m-b", p2, p1
			if tc.releaseBefore {
				why, model, prompt, other = unrecorded, "m-a", p1, p2
			}
			if len(relaunches) != 1 {
				t.Fatalf("the restarted daemon's start relaunched the prober %d times, want once: %s", len(relaunches), why)
			}
			spec := relaunches[0].Spec
			if spec.Model != model || !composedHolds(t, spec, prompt) || composedHolds(t, spec, other) || spec.ResumeSessionFile != "/tmp/prober.jsonl" {
				t.Fatalf("the prober was relaunched on %q, its prompt holding the plan's %t and the other %t, on the session %q; want %s with the plan's prompt alone, on its own session /tmp/prober.jsonl",
					spec.Model, composedHolds(t, spec, prompt), composedHolds(t, spec, other), spec.ResumeSessionFile, model)
			}
			if !strings.Contains(logs.String(), why) {
				t.Fatalf("the restarted daemon's lines do not say %q:\n%s", why, logs.String())
			}
		})
	}
}

// A prober launched under version 1 of the plan (type testing, model m-a, prompt P1) is up and
// idle when its process dies, and the relaunch's try starts nothing: the session store did not
// answer (runtime.ErrSessionStore), so the claim waits launching, records no process, and tries
// again a minute later (supervise's startWaitsForStore). Version 2 changes the role block (type
// implementation, model m-b, prompt P2), given after the failed try or before it, and the task's
// step of prober starts. The claim runs no process, and the try that starts its next one builds its
// spec then, from the plan in force, so the start suspends nothing and says no definition changed:
// it hands the claim its task, the retry launches the prober once, with P2 and m-b, on its own
// session, and the agent that comes up is not suspended.
//
// Before the fix the start compared the definition of the process the claim had let go with the
// plan in force and asked the launching claim to suspend: the start waited, its line said the
// definition changed since the process launched though no process ran, and the suspension held for
// the retry's agent stopped it once it came up, to be launched again.
func TestAStartWhileAFailedLaunchWaitsToBeTriedAgainSuspendsNothing(t *testing.T) {
	const p1, p2 = "# Prober\n\nProbe the change, first wording.", "# Prober\n\nProbe the change, second wording."
	storeDown := fmt.Errorf("resume the prober: %w: dial tcp: connection refused", runtime.ErrSessionStore)
	for _, tc := range []struct {
		name string
		// beforeTry is whether version 2 is given before the failed try, which then built its
		// spec from version 2, rather than after it.
		beforeTry bool
	}{
		{"version 2 is given after the failed try", false},
		{"version 2 is given before the failed try", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			putOutboxIssue(t, pool, records, record.Issue{Key: stepStartKey, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: stepStartKey, Title: "Workflow", Phase: phase.Step, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			openTree(t, pool, "LEGION", stepStartKey)
			holdSlot(t, pool, stepStartKey)
			seedProberVersion(t, pool, records, 1, "testing", "m-a", p1)
			composer, err := prompts.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			clock, logs := &heldClock{}, &syncBuffer{}
			prober := specs{stateDir: t.TempDir(), project: "legion", prompts: composer, planRole: poolPlanRole(pool, records)}
			sup.deps.Specs, sup.deps.Clock, sup.deps.Log = prober, clock, jsonLogger(logs)
			token := mustClaimToken(t, stepStartKey, relaunchProber)
			sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, roleDefinition: prober.roleDefinition,
				tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			comeUp := func(machine *supervise.Machine, when string) {
				t.Helper()
				generation := machine.Claim().Generation
				for _, ev := range []supervise.Event{
					supervise.StreamHello{Claim: token, Generation: generation},
					supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-prober", SessionFile: "/tmp/prober.jsonl", CapabilityHash: []byte("prober")},
					supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-prober"},
				} {
					if err := machine.Handle(ctx, ev); err != nil {
						t.Fatalf("bring the prober's agent up %s: handle %T: %v", when, ev, err)
					}
				}
				machine.Wait()
			}

			first := mustOutboxRow(t, stepStartKey, record.SuperviseRequest{Op: "start", Tree: stepStartKey, Role: relaunchProber, Generation: 2,
				Phase: phase.Step, Task: "Probe it. Issue: LEGION-208. Phase: step.", Entry: 1}, time.Now())
			first.ID = 95
			if err := runner.execute(ctx, first); err != nil {
				t.Fatalf("the prober's start of version 1: %v", err)
			}
			machine, found := sup.Machine(token)
			if !found {
				t.Fatal("the prober's start of version 1 created no claim")
			}
			machine.Wait()
			comeUp(machine, "under version 1")
			if pending := machine.Claim().Pending; pending != nil {
				for _, ev := range []supervise.Event{
					supervise.StreamTurnStart{Claim: token, DeliveryID: pending.ID},
					supervise.StreamTurnEnd{Claim: token},
					supervise.StreamPromptResult{Claim: token, DeliveryID: pending.ID, AgentInvoked: true},
				} {
					if err := machine.Handle(ctx, ev); err != nil {
						t.Fatalf("the prober's turn of version 1: handle %T: %v", ev, err)
					}
				}
			}
			machine.Wait()
			launched := append(rt.CallsOf("Spawn"), rt.CallsOf("Resume")...)
			if held := machine.Claim(); held.RunsNothing() || held.Pending != nil || held.State == supervise.StateWorking || len(launched) != 1 || launched[0].Spec.Model != "m-a" {
				t.Fatalf("the prober after its turn of version 1 is %s holding %+v after %d launches on %q, want its process up, idle, with no task, launched once on m-a: the case's premise",
					held.State, held.Pending, len(launched), modelOf(launched))
			}

			if tc.beforeTry {
				seedProberVersion(t, pool, records, 2, "implementation", "m-b", p2)
			}
			rt.ScriptResume(fake.SpawnResult{Err: storeDown})
			died := runtime.Observation{Locator: *machine.Claim().Locator, Kind: runtime.Gone, At: time.Now()}
			if err := machine.Handle(ctx, supervise.RuntimeObservation{Observation: died}); !errors.Is(err, runtime.ErrSessionStore) {
				t.Fatalf("the relaunch after the prober's death answered %v, want the session store's refusal: the case's premise", err)
			}
			machine.Wait()
			if held := machine.Claim(); held.State != supervise.StateLaunching || held.Locator != nil || len(rt.CallsOf("Resume")) != 1 {
				t.Fatalf("the prober after its failed try is %s recording the process %+v after %d tries, want launching, recording none, after one: the case's premise",
					held.State, held.Locator, len(rt.CallsOf("Resume")))
			}
			// The failed try stopped the process its let-go ended (supervise's stopPrevious), a
			// suspension of the dead process; any later one would stop the retry's agent.
			stopped := len(rt.CallsOf("Suspend"))
			if !tc.beforeTry {
				seedProberVersion(t, pool, records, 2, "implementation", "m-b", p2)
			}

			second := mustOutboxRow(t, stepStartKey, record.SuperviseRequest{Op: "start", Tree: stepStartKey, Role: relaunchProber, Generation: 2,
				Phase: phase.Step, Task: "Probe it again. Issue: LEGION-208. Phase: step.", Entry: 2}, time.Now())
			second.ID = 96
			if err := runner.execute(ctx, second); err != nil {
				t.Fatalf("the prober's start of version 2 while its failed launch waits to be tried again = %v, want the task handed over: the claim runs no process to suspend", err)
			}
			machine.Wait()
			if lines := logs.String(); strings.Contains(lines, "since its process launched") || strings.Contains(lines, "records no definition of its role") {
				t.Fatalf("the start logged a relaunch's reason for a claim that runs no process:\n%s", lines)
			}

			if fired := clock.fire(time.Minute); fired != 1 {
				t.Fatalf("fired %d timers of a minute, want the one retry of the failed try", fired)
			}
			machine.Wait()
			resumes := rt.CallsOf("Resume")
			if len(resumes) != 2 {
				t.Fatalf("the prober was tried %d times after its death, want twice: the failed try and its retry", len(resumes))
			}
			spec := resumes[1].Spec
			if spec.Model != "m-b" || !composedHolds(t, spec, p2) || composedHolds(t, spec, p1) || spec.ResumeSessionFile != "/tmp/prober.jsonl" {
				t.Fatalf("the retry launched the prober on %q, its prompt holding P2 %t and P1 %t, on the session %q; want m-b with P2 alone, on its own session /tmp/prober.jsonl",
					spec.Model, composedHolds(t, spec, p2), composedHolds(t, spec, p1), spec.ResumeSessionFile)
			}
			comeUp(machine, "after the retry")
			if held, suspends := machine.Claim(), rt.CallsOf("Suspend")[stopped:]; held.State == supervise.StateSuspended || len(suspends) != 0 {
				t.Fatalf("the retry's agent is %s after %d suspensions since the failed try, want it up and not suspended: it runs version 2", held.State, len(suspends))
			}
			if launches := append(rt.CallsOf("Spawn"), rt.CallsOf("Resume")...); len(launches) != 3 {
				t.Fatalf("the prober was launched %d times (on %q), want three: version 1's, the failed try and its retry", len(launches), modelOf(launches))
			}
		})
	}
}
