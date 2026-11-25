package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The claim of the name tester ran key k's agent, in session ses-k, and still runs it when the
// start of a step whose agent took that name arrives, marked with the session to leave
// (record.SuperviseRequest.LeaveSession). The start suspends the claim first, keeping its session,
// and then brings it back in a session of its own: the runtime spawns a fresh agent, resumes none,
// and the claim records ses-k as abandoned. A claim in a turn holds the suspension for the turn's
// end, and the start waits until then. The control is the same start naming a session the claim
// does not record: nothing stops the running claim, and nothing starts it again.
func TestAStartThatLeavesAKeysSessionSuspendsItsRunningClaimAndStartsAFreshAgent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		leave  string
		inTurn bool
		leaves bool
	}{
		{"the claim is ready", "ses-k", false, true},
		{"the claim is in a turn", "ses-k", true, true},
		{"the control: the start leaves another session", "ses-other", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
			putOutboxIssue(t, pool, records, issue)
			named := mustClaimToken(t, issue.Key, claim.RoleTester)
			putPhaseRow(t, pool, records, record.PhaseRow{Issue: issue.Key, Role: claim.RoleTester, Claim: named, StepTask: "default", StepPlace: 2, Entry: 2})
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: named, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
				Role: claim.RoleTester, State: supervise.StateQueued}, "")
			if err != nil {
				t.Fatalf("create the claim of the name tester: %v", err)
			}
			runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, notices: &outboxPublisher{},
				trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: named, Exempt: true}); err != nil {
				t.Fatalf("spawn key k's agent: %v", err)
			}
			launch := machine.Claim().Generation
			if err := machine.Handle(ctx, supervise.RequestRegister{Claim: named, Generation: launch, Session: "ses-k", SessionFile: "/tmp/k.jsonl"}); err != nil {
				t.Fatalf("register key k's agent: %v", err)
			}
			if err := machine.Handle(ctx, supervise.RequestReady{Claim: named, Generation: launch, Session: "ses-k"}); err != nil {
				t.Fatalf("ready key k's agent: %v", err)
			}
			if tc.inTurn {
				if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: named}); err != nil {
					t.Fatalf("key k's agent's turn: %v", err)
				}
			}
			suspends, spawns, resumes := len(rt.CallsOf("Suspend")), len(rt.CallsOf("Spawn")), len(rt.CallsOf("Resume"))

			start := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester, Phase: phase.Testing,
				Entry: 2, Task: "Test the change.", Generation: 1, LeaveSession: tc.leave}, time.Now())
			start.ID = 81
			if tc.inTurn {
				if err := runner.execute(ctx, start); !errors.Is(err, wait.ErrWaiting) {
					t.Fatalf("the start during key k's turn = %v, want a wait", err)
				}
				if view := machine.View(); !view.SuspensionHeld || view.State != supervise.StateWorking || machine.Claim().Session != "ses-k" {
					t.Fatalf("the claim is %s in session %q with a suspension held = %t, want working in ses-k with the suspension held",
						view.State, machine.Claim().Session, view.SuspensionHeld)
				}
				if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: named}); err != nil {
					t.Fatalf("end key k's agent's turn: %v", err)
				}
			}
			if err := runner.execute(ctx, start); err != nil {
				t.Fatalf("the start: %v", err)
			}

			suspended, spawned, resumed := len(rt.CallsOf("Suspend"))-suspends, len(rt.CallsOf("Spawn"))-spawns, len(rt.CallsOf("Resume"))-resumes
			got := machine.Claim()
			if !tc.leaves {
				if suspended != 0 || spawned != 0 || resumed != 0 || got.Session != "ses-k" || got.Abandoned.Session != "" {
					t.Fatalf("the start stopped the claim %d times, spawned %d and resumed %d, and the claim records %q and abandoned %q; want the running claim left in ses-k",
						suspended, spawned, resumed, got.Session, got.Abandoned.Session)
				}
				return
			}
			if suspended != 1 || spawned != 1 || resumed != 0 {
				t.Fatalf("the start stopped the claim %d times, spawned %d and resumed %d; want one suspension and one fresh agent, and no resume of ses-k",
					suspended, spawned, resumed)
			}
			if got.Session != "" || got.Abandoned.Session != "ses-k" || got.State != supervise.StateLaunching {
				t.Fatalf("the claim is %s, records %q and abandoned %q; want it launching with no session and ses-k abandoned", got.State, got.Session, got.Abandoned.Session)
			}
		})
	}
}
