package daemon

// The released daemon starts nothing (dispatch://INFRA-3583 R7). Each gate the project release's
// mark is read by is driven here twice, the mark held and the mark clear, over the same act: the
// launch check every machine launch makes, the runtime refusal a launch that raced the mark meets,
// the task a turn's end would hand, the periodic work, and the outbox. The mark is set directly:
// the release that sets it is projectrelease.go's, which project_release_rig_test.go runs.
//
// The delivery, the notice and the backstop that come due during the release are held here at the
// unit level: each waits in the database while the mark holds and acts in a daemon whose mark is
// clear, which is the next process. That the next boot's starts come before them is the rig's to
// show, with a whole boot (project_move_test.go).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// releasedMark is a project release mark already set, as the release sets it before its record.
func releasedMark(t *testing.T) *projectRelease {
	t.Helper()
	release := &projectRelease{}
	if !release.mark() {
		t.Fatal("a fresh mark was already set")
	}
	return release
}

// isReleaseRefusal is whether err is the project release's refusal of a start: a wait, in the
// sentence the routes give.
func isReleaseRefusal(err error) bool {
	return errors.Is(err, wait.ErrWaiting) && strings.Contains(err.Error(), api.ProjectReleaseHeldSentence)
}

// The launch check every launch makes, and the admission of every claim started again, refuse
// while the mark holds, whatever the claim, the project's controller included; the store behind
// is asked nothing. With the mark clear both reach the store. Through a machine, a spawn the mark
// refuses launches nothing, and a death's relaunch it refuses starts nothing and is charged
// nothing.
func TestProjectReleaseHoldsEveryLaunchCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		claim supervise.Claim
	}{
		{"an issue's claim", supervise.Claim{Token: "legion-legion-legion-208-implementer", Project: "legion", Tree: "LEGION-208",
			Issue: "LEGION-208", Role: claim.RoleImplementer, State: supervise.StateQueued}},
		{"the project's controller", supervise.Claim{Token: claim.ControllerToken("legion"), Project: "legion",
			Role: claim.RoleController, State: supervise.StateQueued}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			release := releasedMark(t)
			behind := &outboxClaimStore{}
			gated := gateLaunch(behind, release, nil)
			if _, err := gated.AdmitClaim(ctx, tc.claim); !isReleaseRefusal(err) {
				t.Fatalf("AdmitClaim while released = %v, want the release's wait", err)
			}
			if err := gated.CheckLaunch(ctx, tc.claim); !isReleaseRefusal(err) {
				t.Fatalf("CheckLaunch while released = %v, want the release's wait", err)
			}
			if len(behind.claims) != 0 {
				t.Fatalf("the store behind the refused admission holds %v, want nothing", behind.claims)
			}
			release.unmark()
			if _, err := gated.AdmitClaim(ctx, tc.claim); err != nil {
				t.Fatalf("AdmitClaim with the mark clear = %v, want the store's admission", err)
			}
			if err := gated.CheckLaunch(ctx, tc.claim); err != nil {
				t.Fatalf("CheckLaunch with the mark clear = %v, want the store's check", err)
			}
			if _, ok := behind.claims[tc.claim.Token]; !ok {
				t.Fatal("the admission with the mark clear did not reach the store")
			}
		})
	}

	t.Run("a test's own refusal still runs behind the mark", func(t *testing.T) {
		asked := 0
		gated := gateLaunch(&outboxClaimStore{}, &projectRelease{}, func(context.Context, supervise.Claim) error {
			asked++
			return errors.New("refused by the test")
		})
		if err := gated.CheckLaunch(context.Background(), supervise.Claim{Token: "legion-legion-legion-208-tester"}); err == nil || asked != 1 {
			t.Fatalf("CheckLaunch with the mark clear = %v after %d asks, want the test's refusal once", err, asked)
		}
	})

	t.Run("a spawn the mark refuses launches nothing", func(t *testing.T) {
		sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
		release := &projectRelease{}
		sup.deps.Store = gateLaunch(&outboxClaimStore{}, release, nil)
		token := mustClaimToken(t, "LEGION-208", claim.RoleArchitect)
		m, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208",
			Issue: "LEGION-208", Role: claim.RoleArchitect, State: supervise.StateQueued}, "")
		if err != nil {
			t.Fatal(err)
		}
		release.mark()
		if err := m.Handle(context.Background(), supervise.RequestSpawn{Claim: token}); !isReleaseRefusal(err) {
			t.Fatalf("spawn while released = %v, want the release's wait", err)
		}
		if got := m.Claim(); got.State != supervise.StateQueued || len(rt.CallsOf("Spawn")) != 0 {
			t.Fatalf("after the refused spawn the claim is %s with %d spawns, want queued and none", got.State, len(rt.CallsOf("Spawn")))
		}
		release.unmark()
		if err := m.Handle(context.Background(), supervise.RequestSpawn{Claim: token}); err != nil {
			t.Fatalf("spawn with the mark clear: %v", err)
		}
		if spawns := len(rt.CallsOf("Spawn")); spawns != 1 {
			t.Fatalf("spawns with the mark clear = %d, want 1", spawns)
		}
	})

	t.Run("a death's relaunch the mark refuses starts nothing and is charged nothing", func(t *testing.T) {
		sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
		release := &projectRelease{}
		sup.deps.Store = gateLaunch(&outboxClaimStore{}, release, nil)
		token := launchedClaim(t, sup, "LEGION-208", "LEGION-208", claim.RoleArchitect)
		m, _ := sup.Machine(token)
		loc := *m.Claim().Locator
		release.mark()
		_ = m.Handle(context.Background(), supervise.RuntimeObservation{Observation: runtime.Observation{Locator: loc, Kind: runtime.Gone}})
		if got := m.Claim(); got.Budgets.LaunchFailures != 0 || len(resumesOf(rt, token)) != 0 {
			t.Fatalf("after the refused relaunch the claim holds %d launch failures and %d resumes, want none of either",
				got.Budgets.LaunchFailures, len(resumesOf(rt, token)))
		}
	})
}

// A launch that passed the launch check just before the mark was set meets it at the runtime
// (runtime.ErrProjectReleased): that refusal charges no launch failure, and the claim waits to be
// launched again, its process recorded nowhere. Any other refusal of the runtime is charged, so
// what this reads is the sentinel's.
func TestProjectReleaseRuntimeRefusalChargesNoLaunchFailure(t *testing.T) {
	t.Parallel()
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	ctx := context.Background()
	create := func(role claim.Role) *supervise.Machine {
		t.Helper()
		m, _, err := sup.Create(ctx, supervise.Claim{Token: mustClaimToken(t, "LEGION-208", role), Project: "legion",
			Tree: "LEGION-208", Issue: "LEGION-208", Role: role, State: supervise.StateQueued}, "")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("the release's refusal", func(t *testing.T) {
		m := create(claim.RoleArchitect)
		rt.ScriptSpawn(fake.SpawnResult{Err: fmt.Errorf("sandbox runtime: create sandbox: %w", runtime.ErrProjectReleased)})
		_ = m.Handle(ctx, supervise.RequestSpawn{Claim: m.Claim().Token})
		got := m.Claim()
		if got.Budgets.LaunchFailures != 0 || got.State != supervise.StateLaunching || got.Locator != nil {
			t.Fatalf("after the release's refusal the claim is %s on %v with %d launch failures, want launching, no process and none charged",
				got.State, got.Locator, got.Budgets.LaunchFailures)
		}
		if !strings.Contains(got.LastStartFailure, runtime.ErrProjectReleased.Error()) {
			t.Fatalf("the claim's last start failure is %q, want the release's refusal named", got.LastStartFailure)
		}
	})

	t.Run("any other refusal is charged", func(t *testing.T) {
		m := create(claim.RoleTester)
		refused := fake.SpawnResult{Err: errors.New("the runtime refused the launch")}
		rt.ScriptSpawn(refused, refused)
		_ = m.Handle(ctx, supervise.RequestSpawn{Claim: m.Claim().Token})
		if got := m.Claim(); got.Budgets.LaunchFailures == 0 {
			t.Fatalf("after an ordinary refusal the claim is %s with no launch failure charged, want it charged", got.State)
		}
	})
}

// A role whose turn ends while the release holds is handed no task: the task stays pending on the
// claim, unsent. A release whose record did not commit clears the mark, and the task the machine
// held is sent then, with no other event reaching the claim.
func TestProjectReleaseHandsNoTaskWhenATurnEnds(t *testing.T) {
	t.Parallel()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	release := &projectRelease{}
	sup.deps.ProjectReleaseHolds = release.held
	release.unmarked = sup.sendHeldTasks
	ctx := context.Background()
	token := launchedClaim(t, sup, "LEGION-208", "LEGION-208", claim.RoleArchitect)
	m, _ := sup.Machine(token)
	registered, _ := sup.deps.Conns.Conn(token)
	conn := registered.(*fake.Conn)
	if err := m.Handle(ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start the turn: %v", err)
	}
	release.mark()
	if err := m.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: "Carry on with the tree."}); err != nil {
		t.Fatalf("deliver during the turn: %v", err)
	}

	t.Run("the turn's end hands no task while the release holds", func(t *testing.T) {
		if err := m.Handle(ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
			t.Fatalf("end the turn: %v", err)
		}
		m.Wait()
		if prompts := conn.Prompts(); len(prompts) != 0 {
			t.Fatalf("the role was sent %d prompts while the release holds, want none", len(prompts))
		}
		if got := m.Claim(); got.State != supervise.StateIdle || got.Pending == nil || got.Pending.Task != "Carry on with the tree." {
			t.Fatalf("after the turn's end the claim is %s holding %+v, want it idle with the task still pending", got.State, got.Pending)
		}
	})

	t.Run("a failed commit's unmark sends the task the machine held", func(t *testing.T) {
		release.unmark()
		testwait.Eventually(t, "the held task sent once the mark is cleared", func() bool {
			return len(conn.Prompts()) == 1
		})
		if prompt := conn.Prompts()[0]; !strings.Contains(prompt.Message, "Carry on with the tree.") {
			t.Fatalf("the role was sent %q, want the task it held", prompt.Message)
		}
	})
}

// The daemon's periodic work does nothing that makes a pod or starts a role while the release
// holds, and does it with the mark clear: the sweep's orphan reconciliation (with its cache sweep),
// the relaunch of the launches the last process left unfinished, the idle and unowned ticks, and
// the controller keeper. The wakes' hold is the delivery test's below (wakePass).
func TestProjectReleaseHoldsThePeriodicWork(t *testing.T) {
	t.Parallel()
	t.Run("the sweep", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		sup := newSupervisor(context.Background(), outboxTreeStore(t, pool), "legion", t.TempDir(), quietLogger())
		t.Cleanup(sup.stop)
		rt := fake.NewRuntime()
		release := releasedMark(t)
		s := &supervision{log: quietLogger(), plan: plan{clock: stillClock{}, project: "legion", orphanSweep: time.Hour, release: release},
			runtime: rt, supervisor: sup, refreshGate: supervise.NewRefreshGate(0)}
		s.sweepOnce(context.Background())
		if calls := rt.CallsOf("ReconcileOrphans"); len(calls) != 0 {
			t.Fatalf("the sweep reconciled orphans %d times while the release holds, want never", len(calls))
		}
		release.unmark()
		s.sweepOnce(context.Background())
		if calls := rt.CallsOf("ReconcileOrphans"); len(calls) != 1 {
			t.Fatalf("the sweep reconciled orphans %d times with the mark clear, want once", len(calls))
		}
	})

	t.Run("the relaunch of the unfinished launches", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		ctx, cancel := context.WithCancel(context.Background())
		sup := newSupervisor(ctx, outboxTreeStore(t, pool), "legion", t.TempDir(), quietLogger())
		// The relaunch reads the machines' clock on every pass past the hold, even one with no
		// launch to reserve a turn for (reserveUnfinishedTurns), and a bare supervisor holds none.
		sup.deps.Clock = stillClock{}
		rt := fake.NewRuntime()
		release := releasedMark(t)
		s := &supervision{log: quietLogger(), plan: plan{clock: stillClock{}, project: "legion", orphanSweep: 5 * time.Millisecond, release: release},
			runtime: rt, supervisor: sup, refreshGate: supervise.NewRefreshGate(0)}
		t.Cleanup(func() {
			cancel()
			s.wg.Wait()
			sup.stop()
		})
		s.relaunchUnfinished([]claim.Token{mustClaimToken(t, "LEGION-208", claim.RoleTester)}, nil, false)
		time.Sleep(100 * time.Millisecond)
		if calls := rt.CallsOf("ReconcileOrphans"); len(calls) != 0 {
			t.Fatalf("the relaunch reconciled the boot's orphans %d times while the release holds, want never", len(calls))
		}
		release.unmark()
		testwait.Eventually(t, "the relaunch's tick with the mark clear", func() bool {
			return len(rt.CallsOf("ReconcileOrphans")) > 0
		})
	})

	t.Run("the idle and unowned ticks", func(t *testing.T) {
		w, _, logged := wakeRuntime(t)
		w.release = releasedMark(t)
		w.idlePass(context.Background())
		if strings.Contains(logged.String(), "idle release") || strings.Contains(logged.String(), "unowned release") {
			t.Fatalf("the idle tick decided pods while the release holds; it logged %q", logged)
		}
		w.release.unmark()
		w.idlePass(context.Background())
		if !strings.Contains(logged.String(), "has not once reported itself ready") {
			t.Fatalf("the idle tick with the mark clear logged %q, want it to have read the listener's report", logged)
		}
	})

	t.Run("the controller keeper", func(t *testing.T) {
		sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
		keeper := newControllerKeeper(context.Background(), sup, "legion", time.Minute, 0, quietLogger())
		keeper.release = releasedMark(t)
		if err := keeper.keep(); err != nil {
			t.Fatalf("keep while the release holds: %v", err)
		}
		if _, ok := sup.Machine(claim.ControllerToken("legion")); ok || len(rt.CallsOf("Spawn")) != 0 {
			t.Fatalf("the keeper created the controller's claim (%t) and spawned %d times while the release holds, want neither",
				ok, len(rt.CallsOf("Spawn")))
		}
		keeper.release.unmark()
		if err := keeper.keep(); err != nil {
			t.Fatalf("keep with the mark clear: %v", err)
		}
		if _, ok := sup.Machine(claim.ControllerToken("legion")); !ok || len(rt.CallsOf("Spawn")) != 1 {
			t.Fatalf("with the mark clear the keeper created the claim (%t) and spawned %d times, want it created and spawned once",
				ok, len(rt.CallsOf("Spawn")))
		}
	})
}

// Every outbox row that would start or prompt a role waits while the release holds, and every other
// kind runs (record.HeldByProjectRelease). Every kind of every lane is named here, so a kind added
// to the outbox without a decision for the release fails this test, and so is every supervise op, a
// project pause's `pause_stop` and a stopped task's `step_stop` (stops, which run), the retirement
// of a stopped phase's task (`retire_task`, which runs: it writes the claim's record alone, since
// the machine hands no claim anything while the release holds), the launch the pause owes a role
// it stopped and a start queued for news of the approved design alone (two starts, which wait)
// among them, and the pause's two notices to the controller, which wait as every controller notice
// does. The end of a parent's worker's phase (phase_ended) waits, as a hook's prompt does: it
// tells a role and wakes a released one. So do the merger's message that the project's merge
// holds changed (merge_holds_changed, #4065), which wakes a released merger as the merge-queue
// message does, the architect's notices of that change and of a run of provider errors, and the
// ask of the update round the controller's pod update opens (`pod:<pod uid>`), as every round's
// ask but the release's own does. The reconcile of a tree's news (design_changes) and the two
// reads of Dispatch for a root's design-gate basis and for a re-parented root
// (pending_basis_seed, reparent_seed) run: each tells a role only through a start or a notice,
// which waits.
func TestProjectReleaseHoldsEveryOutboxKindThatStartsOrPromptsARole(t *testing.T) {
	t.Parallel()
	const ownRound = "release:7"
	type row struct {
		name    string
		payload record.OutboxPayload
		held    bool
	}
	cases := map[record.OutboxKind][]row{
		record.OutboxKindSupervise: {
			{"a start", record.SuperviseRequest{Op: "start"}, true},
			{"a start queued for news of the approved design alone", record.SuperviseRequest{Op: "start", Phase: phase.Reviewing, News: true}, true},
			{"the launch a project pause owed a role it stopped", record.SuperviseRequest{Op: "start", Pause: 3, WakeKind: pods.WakePause}, true},
			{"an idle release's role stop", record.SuperviseRequest{Op: "release"}, true},
			{"a suspend", record.SuperviseRequest{Op: "suspend"}, false},
			{"an issue's close", record.SuperviseRequest{Op: "issue_close"}, false},
			{"a tree's close", record.SuperviseRequest{Op: "tree_close"}, false},
			{"a project pause's role stop", record.SuperviseRequest{Op: "pause_stop", Pause: 3}, false},
			{"a stopped task's step stop", record.SuperviseRequest{Op: "step_stop"}, false},
			{"the retirement of a stopped phase's task", record.SuperviseRequest{Op: "retire_task"}, false},
		},
		record.OutboxKindUpdateAsk: {
			{"the release's own ask", record.UpdateAsk{Round: ownRound}, false},
			{"a pod update round's ask", record.UpdateAsk{Round: "update:3"}, true},
			{"the ask of the round the controller's pod update opened", record.UpdateAsk{Round: "pod:0f1e2d3c"}, true},
		},
		record.OutboxKindNotice: {
			{"a notice", record.Notice{}, true},
			{"an architect's notice that the merge holds changed", record.Notice{Kind: "merge-holds-changed", Role: claim.RoleArchitect}, true},
			{"an architect's notice of a run of provider errors", record.Notice{Kind: record.ProviderErrorsNotice, Role: claim.RoleArchitect}, true},
		},
		record.OutboxKindControllerNotice: {
			{"a controller notice", record.ControllerNotice{}, true},
			{"a project-paused notice", record.ControllerNotice{Kind: record.ProjectPausedNotice}, true},
			{"a project-resumed notice", record.ControllerNotice{Kind: record.ProjectResumedNotice}, true},
		},
		record.OutboxKindRoleMessage:        {{"a role message", record.RoleMessage{}, true}},
		record.OutboxKindPhaseEnded:         {{"the end of a parent's worker's phase", record.PhaseEnded{}, true}},
		record.OutboxKindHookPrompt:         {{"a hook's prompt", record.HookPrompt{}, true}},
		record.OutboxKindMergeQueueRead:     {{"a merge-queue read", record.MergeQueueRead{}, true}},
		record.OutboxKindMergeQueueRemoval:  {{"a merge-queue removal", record.MergeQueueRemoval{}, true}},
		record.OutboxKindMergeHoldsChanged:  {{"the merger's message that the merge holds changed", record.MergeHoldsChanged{Project: "legion"}, true}},
		record.OutboxKindConflictConfirm:    {{"a conflicting head's second read", record.ConflictConfirm{}, true}},
		record.OutboxKindMergeForward:       {{"a merge forward's message", record.MergeForwardMessage{}, true}},
		record.OutboxKindCheckWake:          {{"a check's wake", record.CheckWakeDue{}, true}},
		record.OutboxKindCheckLook:          {{"a check's look", record.CheckLook{}, true}},
		record.OutboxKindCheckBound:         {{"a check's bound", record.CheckBound{}, true}},
		record.OutboxKindIssueBundle:        {{"an idle release's workspace check", record.IssueWorkspaceCheck{}, true}},
		record.OutboxKindIssueRelease:       {{"an idle release's teardown", record.IssueRelease{}, true}},
		record.OutboxKindDispatchStatus:     {{"a status write", record.StatusWrite{}, false}},
		record.OutboxKindDispatchMessage:    {{"a message post", record.MessagePost{}, false}},
		record.OutboxKindClaimRelease:       {{"a Dispatch claim's release", record.ClaimRelease{}, false}},
		record.OutboxKindGatedStepsPublish:  {{"the gated steps' publish", record.GatedStepsPublish{}, false}},
		record.OutboxKindDesignGatesPublish: {{"an issue's design gates' publish", record.DesignGatesPublish{}, false}},
		record.OutboxKindRoleHoldersPublish: {{"an issue's role holders' publish", record.RoleHoldersPublish{}, false}},
		record.OutboxKindStepRecord:         {{"a step record's write", record.StepRecord{}, false}},
		record.OutboxKindPlanRecord:         {{"a plan record's write", record.PlanRecord{}, false}},
		record.OutboxKindIssueSuspend:       {{"a closed issue's pod deleted", record.IssueSuspend{}, false}},
		record.OutboxKindLingerClose:        {{"a linger's close", record.LingerClose{}, false}},
		record.OutboxKindWorkspaceRemove:    {{"a workspace's removal", record.WorkspaceRemove{}, false}},
		record.OutboxKindGateSeed:           {{"a gate's seed", record.GateSeed{}, false}},
		record.OutboxKindPendingBasisSeed:   {{"a pending basis's seed", record.PendingBasisSeed{}, false}},
		record.OutboxKindReparentSeed:       {{"a re-parented root's seed", record.ReparentSeed{}, false}},
		record.OutboxKindIssueBranch:        {{"an issue's branch", record.IssueBranch{}, false}},
		record.OutboxKindDesignChanges:      {{"a tree's reconcile of its news of the approved design", record.DesignNewsReconcile{}, false}},
	}
	for _, lane := range record.OutboxLanes() {
		for _, kind := range record.LaneKinds(lane) {
			rows, ok := cases[kind]
			if !ok {
				t.Errorf("the outbox kind %q has no row here: decide whether a project release holds it", kind)
				continue
			}
			for _, tc := range rows {
				t.Run(string(kind)+": "+tc.name, func(t *testing.T) {
					if tc.payload.OutboxKind() != kind {
						t.Fatalf("the case's payload is of kind %q, want %q", tc.payload.OutboxKind(), kind)
					}
					if got := record.HeldByProjectRelease(tc.payload, ownRound); got != tc.held {
						t.Fatalf("held = %t, want %t", got, tc.held)
					}
				})
			}
		}
	}
	t.Run("every ask while the release's record has not committed", func(t *testing.T) {
		if !record.HeldByProjectRelease(record.UpdateAsk{Round: ownRound}, "") {
			t.Fatal("an ask was let through before the release's record named its round")
		}
	})
	t.Run("the mark names the release's own round once its record commits", func(t *testing.T) {
		release := releasedMark(t)
		if round := release.askRound(); round != "" {
			t.Fatalf("the round before the record = %q, want none", round)
		}
		release.recorded(releaseAskHook{id: 7})
		if round := release.askRound(); round != ownRound {
			t.Fatalf("the round once release 7 is recorded = %q, want %q", round, ownRound)
		}
		// Once the release finished, its own asks still run, where they meet the claim-round
		// read, and no ask is published without that read.
		release.finish()
		if ask, own := release.askRound(), release.ownRound(); ask != "" || own != ownRound || !release.held() {
			t.Fatalf("once release 7 finished the ask round is %q and its own %q, held %t; want none, %q and the mark held",
				ask, own, release.held(), ownRound)
		}
		if record.HeldByProjectRelease(record.UpdateAsk{Round: ownRound}, release.ownRound()) {
			t.Fatal("an ask of the finished release is held, want it run to meet the claim-round read")
		}
	})
}

// outboxRowState is a row's attempts and last error, and whether it is still in the outbox, of
// the one reading of the row (readHeldRow).
func outboxRowState(t *testing.T, pool *store.Pool, issue string) (attempts int, lastError string, present bool) {
	t.Helper()
	row, present := readHeldRow(t, pool, issue)
	return row.attempts, row.lastError, present
}

// runDue runs every row of the project due at now through runner, as a lane runs each.
func runDue(t *testing.T, runner *outbox, now time.Time) int {
	t.Helper()
	rows, err := runner.listDue(context.Background(), "", nil, now)
	if err != nil {
		t.Fatalf("list the due rows: %v", err)
	}
	for _, row := range rows {
		if err := runner.runRow(context.Background(), row); err != nil {
			t.Fatalf("run row %d: %v", row.ID, err)
		}
	}
	return len(rows)
}

// dispatch://INFRA-3583 R7: a delivery, a notice and a backstop that come due during the release
// wait in the database while it holds, and reach their claims in the next process, whose mark is
// clear.
func TestProjectReleaseHoldsADeliveryANoticeAndABackstopUntilTheNextBoot(t *testing.T) {
	t.Parallel()
	t.Run("a delivery", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		ctx := context.Background()
		root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
			Tree: "LEGION-208", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"}
		putOutboxIssue(t, pool, records, root)
		sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
		token := mustClaimToken(t, root.Key, claim.RoleTester)
		machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: root.Key, Issue: root.Key, Role: claim.RoleTester,
			Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Session: "ses_tester", SessionFile: "/sessions/tester.jsonl"}, "")
		if err != nil {
			t.Fatal(err)
		}
		runner := func(release *projectRelease, now func() time.Time) *outbox {
			return &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion",
				stateDir: t.TempDir(), log: quietLogger(), now: now, release: release}
		}
		released := runner(releasedMark(t), time.Now)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Key, Role: claim.RoleTester,
			Generation: 1, Phase: phase.Testing, Task: "Carry on testing."}, time.Now()))

		if ran := runDue(t, released, time.Now().UTC()); ran != 1 {
			t.Fatalf("rows run while released = %d, want the start", ran)
		}
		if got := machine.Claim(); got.State != supervise.StateSuspended || len(rt.CallsOf("Resume")) != 0 || got.Pending != nil {
			t.Fatalf("while released the tester is %s with %d resumes, holding %+v; want it suspended, unresumed, given nothing",
				got.State, len(rt.CallsOf("Resume")), got.Pending)
		}
		if attempts, lastError, present := outboxRowState(t, pool, root.Key); !present || attempts != 0 || !strings.Contains(lastError, api.ProjectReleaseHeldSentence) {
			t.Fatalf("the start row is present=%t after %d attempts with %q, want it waiting in the outbox on the release with no attempt counted", present, attempts, lastError)
		}

		later := func() time.Time { return time.Now().Add(time.Hour) }
		if ran := runDue(t, runner(&projectRelease{}, later), later().UTC()); ran != 1 {
			t.Fatalf("rows run by the next process = %d, want the start", ran)
		}
		machine.Wait()
		if got := machine.Claim(); len(rt.CallsOf("Resume")) != 1 || got.Pending == nil || got.Pending.Task != "Carry on testing." {
			t.Fatalf("in the next process the tester has %d resumes, holding %+v; want it resumed once with the delivered task",
				len(rt.CallsOf("Resume")), got.Pending)
		}
		if _, _, present := outboxRowState(t, pool, root.Key); present {
			t.Fatal("the start row is still in the outbox after the next process ran it")
		}
	})

	t.Run("a notice", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: "LEGION-2", Title: "Root", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		publisher := &outboxPublisher{}
		runner := func(release *projectRelease, now func() time.Time) *outbox {
			return &outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
				log: quietLogger(), now: now, release: release}
		}
		released := runner(releasedMark(t), time.Now)
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))

		runDue(t, released, time.Now().UTC())
		if topics := publisher.topics(); len(topics) != 0 {
			t.Fatalf("the notice was published to %v while released, want nowhere", topics)
		}
		if _, lastError, present := outboxRowState(t, pool, "LEGION-2"); !present || !strings.Contains(lastError, api.ProjectReleaseHeldSentence) {
			t.Fatalf("the notice row is present=%t with %q, want it waiting in the outbox on the release", present, lastError)
		}

		later := func() time.Time { return time.Now().Add(time.Hour) }
		runDue(t, runner(&projectRelease{}, later), later().UTC())
		if topics := publisher.topics(); len(topics) != 1 {
			t.Fatalf("the next process published the notice to %v, want one topic", topics)
		}
		if _, _, present := outboxRowState(t, pool, "LEGION-2"); present {
			t.Fatal("the notice row is still in the outbox after the next process published it")
		}
	})

	t.Run("a backstop", func(t *testing.T) {
		w, pool, _ := wakeRuntime(t)
		ctx := context.Background()
		if err := w.idleStore.ArmPodBackstop(ctx, store.PodWakeBackstop{Project: "legion", Issue: "LEGION-208",
			Role: claim.RoleImplementer, DueAt: time.Now().UTC().Add(-time.Minute)}); err != nil {
			t.Fatalf("arm the backstop: %v", err)
		}
		records, err := w.idleStore.PodRecordsOf("legion")
		if err != nil {
			t.Fatal(err)
		}
		fired := &backstopHandler{records: records}
		w.handlers = []intake.Handler{fired}
		w.release = releasedMark(t)
		w.wakePass(ctx, time.Now().UTC())
		if fired.facts != 0 {
			t.Fatalf("backstops fired while released = %d, want none", fired.facts)
		}
		if due, err := w.idleStore.DuePodBackstops(ctx, "legion", time.Now().UTC()); err != nil || len(due) != 1 {
			t.Fatalf("backstops still due after the held pass = %d, %v; want the one, waiting in its table", len(due), err)
		}

		booted := &workflowRuntime{pool: pool, projectID: "legion", idleStore: w.idleStore, idlePeriod: w.idlePeriod,
			log: quietLogger(), handlers: w.handlers, release: &projectRelease{}}
		booted.wakePass(ctx, time.Now().UTC())
		if fired.facts != 1 {
			t.Fatalf("backstops fired by the next process = %d, want the one", fired.facts)
		}
	})
}
