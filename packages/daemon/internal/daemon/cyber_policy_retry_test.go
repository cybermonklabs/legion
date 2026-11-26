package daemon

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The cases of this file are the daemon's automatic starts of a failed claim, each held to one
// rule: the agent of a claim failed for OpenAI's cyber-policy refusal of its session's content is
// launched again by a start someone decided alone, and in a new session. A resume of the recorded
// session would take the daemon's first prompt as input from outside the session, which ends Oh My
// Pi's own hold, so the refused content would go to OpenAI again and add a refusal to the
// organization's record.

// heldForCyberPolicy is the stored claim of a role that failed on OpenAI's cyber-policy refusal,
// with the sentence the daemon writes for it, and its recorded session.
func heldForCyberPolicy(c supervise.Claim) supervise.Claim {
	c.Generation, c.State, c.Session, c.SessionFile = 1, supervise.StateFailed, "ses-refused", "/tmp/refused.jsonl"
	c.FailureCause = supervise.CauseCyberPolicy
	c.Failure = "a provider error that cannot clear ended a turn this claim's agent ran with no prompt of the daemon's: OpenAI's cyber_policy refusal of this session's content"
	c.FailedAt = time.Now()
	return c
}

// The controller keeper retries a failed controller after a wait that doubles (controllerKeeper),
// and the operator's retry of the controller is refused, so the keeper is the only door. It does
// not retry a controller held for a cyber-policy refusal, after any wait
// (supervise.Claim.KeeperRetries): it logs one error whose reason is the machine's refusal, which
// names the operator's `legion claims retry --claim <token>`, and asks the controller's machine for
// nothing. A controller failed for any other provider error that cannot clear is retried as
// before, which is the control: the same keeper over the same clock does retry it.
func TestTheKeeperDoesNotRetryAControllerHeldForCyberPolicy(t *testing.T) {
	t.Parallel()
	token := claim.ControllerToken("legion")
	controller := supervise.Claim{Token: token, Project: "legion", Role: claim.RoleController}
	for _, tc := range []struct {
		name    string
		claim   supervise.Claim
		retried bool
	}{
		{"held for cyber policy", heldForCyberPolicy(controller), false},
		{"failed for a provider error (HTTP 400)", func() supervise.Claim {
			c := heldForCyberPolicy(controller)
			c.FailureCause = supervise.CauseProviderError
			c.Failure = "a provider error that cannot clear ended this claim's turn: a provider error (HTTP 400)"
			return c
		}(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			if _, _, err := sup.Create(context.Background(), tc.claim, ""); err != nil {
				t.Fatalf("create the failed controller: %v", err)
			}
			var logs syncBuffer
			k := newControllerKeeper(context.Background(), sup, "legion", time.Minute, 0, slog.New(slog.NewJSONHandler(&logs, nil)))
			clock := time.Now()
			k.now = func() time.Time { return clock }
			m, _ := sup.Machine(token)
			// Five looks, each past the longest wait the keeper ever takes.
			for range 5 {
				if err := k.keep(); err != nil {
					t.Fatalf("keep: %v", err)
				}
				m.Wait()
				if m.Claim().State != supervise.StateFailed {
					break
				}
				clock = clock.Add(2 * controllerRetryMax)
			}
			launches := len(resumesOf(rt, token)) + len(rt.CallsOf("Spawn"))
			if !tc.retried {
				if c := m.Claim(); c.State != supervise.StateFailed || c.FailureCause != supervise.CauseCyberPolicy || c.Session != "ses-refused" {
					t.Fatalf("the keeper left the controller %s (%q) recording %q, want it failed for cyber policy with its session", c.State, c.FailureCause, c.Session)
				}
				if launches != 0 {
					t.Fatalf("the keeper launched the held controller %d times, want never", launches)
				}
				var lines []string
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					if strings.Contains(line, cyberPolicyHeldLine) {
						lines = append(lines, line)
					}
				}
				if want := "`" + supervise.RetryCommand(token) + "`"; len(lines) != 1 || !strings.Contains(lines[0], `"level":"ERROR"`) ||
					!strings.Contains(lines[0], want) || strings.Contains(lines[0], "--fresh-session") || !strings.Contains(lines[0], `"cause":"cyber_policy"`) {
					t.Fatalf("the keeper logged %q, want one error naming the cause and the operator's %s:\n%s", lines, want, logs.String())
				}
				if strings.Contains(logs.String(), "retrying the daemon's controller") {
					t.Fatalf("the keeper logged a retry of the held controller:\n%s", logs.String())
				}
				return
			}
			if launches != 1 || m.Claim().State != supervise.StateLaunching {
				t.Fatalf("the keeper left the controller %s after %d launches, want it retried once", m.Claim().State, launches)
			}
		})
	}
}

// The liveness sweep writes its line about the daemon's own controller again at every worker boot
// timeout while no session holds the controller's record (watchController), and the keeper writes
// its line once per process. So for a controller held for a cyber-policy refusal, which the keeper
// does not retry, the sweep names the operator's retry rather than a relaunch that never comes. A
// controller failed for another cause, and one not yet created, keep the keeper's relaunch, which
// is the control.
func TestTheLivenessSweepNamesTheOperatorsRetryForAHeldController(t *testing.T) {
	t.Parallel()
	token := claim.ControllerToken("legion")
	held := heldForCyberPolicy(supervise.Claim{Token: token, Project: "legion", Role: claim.RoleController})
	other := held
	other.FailureCause = supervise.CauseProviderError
	const relaunch = "this daemon launches its own controller and relaunches it"
	for _, tc := range []struct {
		name  string
		claim supervise.Claim
		held  bool
	}{
		{"held for cyber policy", held, true},
		{"failed for a provider error", other, false},
		{"not created yet", supervise.Claim{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := launchedControllerLine(tc.claim)
			if !strings.HasPrefix(line, controllerNotRegistered) {
				t.Fatalf("the sweep's line is %q, want it to begin with %q, which one log query counts in either mode", line, controllerNotRegistered)
			}
			names := strings.Contains(line, supervise.RetryCommand(token))
			if names != tc.held || strings.Contains(line, relaunch) == tc.held {
				t.Fatalf("the sweep's line is %q, want the operator's retry named %t and the keeper's relaunch named %t", line, tc.held, !tc.held)
			}
		})
	}
}

// A start row the workflow or admission writes for a failed claim retries it (the outbox's start).
// Every such start but those someone decided is the daemon's own — the next phase start of a role,
// a child architect's next phase start, a return to work's start, a production check's look, the
// sweep's start of a root architect — and none of them launches the agent of a claim held for a
// cyber-policy refusal again: the row finishes without acting and the claim stays failed with its
// session. A start of the issue's phase by the role that works it tells the workflow of the
// failure again (heldStartRefused), so the issue is held for its architect; a start of no phase
// tells nothing. A row an older daemon wrote for the held phase's retry carries ResendEntry and no
// Decided, and is the daemon's own too. The starts someone decided carry Decided — the held
// phase's retry (workflow's restart) and a root architect's start of no phase: a promoted root's
// (workflow's StartPromotedArchitect), and a failed root architect's that a todo on its root asks
// for, the controller's restart of a stuck tree among them (workflow's retryFailedArchitect) — and
// each starts the agent again in a new session. A root's architect starts under the gate root the
// outbox reads on the Dispatch chain, its own key, whatever gate root it last ran under: the held
// architect here last ran under another, as a member's does before its issue becomes a root.
func TestAStartRowLaunchesTheAgentOfAHeldClaimAgainOnlyWhenSomeoneDecidedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		role     claim.Role
		state    supervise.ClaimState
		phase    phase.Phase
		resend   bool
		decided  bool
		launched bool
		told     bool
	}{
		{"a start of the daemon's own, of a failed worker", claim.RoleTester, supervise.StateFailed, phase.Testing, false, false, false, true},
		{"a start of the daemon's own, of a worker an operator stopped after it failed", claim.RoleTester, supervise.StateRetired, phase.Testing, false, false, false, true},
		{"an older daemon's row of the held phase's retry", claim.RoleTester, supervise.StateFailed, phase.Testing, true, false, false, true},
		{"the held phase's retry", claim.RoleTester, supervise.StateFailed, phase.Testing, true, true, true, false},
		{"a start of the daemon's own, of the root's architect", claim.RoleArchitect, supervise.StateFailed, "", false, false, false, false},
		{"a promoted root's architect start", claim.RoleArchitect, supervise.StateFailed, "", false, true, true, false},
		// RoleFor answers the architect at admitted, which works no phase: a start naming admitted
		// tells the workflow nothing.
		{"a start of the daemon's own, of the architect, naming admitted", claim.RoleArchitect, supervise.StateFailed, phase.Admitted, false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issuePhase := phase.Testing
			if tc.phase != "" {
				issuePhase = tc.phase
			}
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: issuePhase, Generation: 1, Status: "testing"}
			putOutboxIssue(t, pool, records, issue)
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			token := mustClaimToken(t, issue.Key, tc.role)
			held := heldForCyberPolicy(supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: tc.role})
			held.State = tc.state
			if tc.role == claim.RoleArchitect {
				held.LaunchTree = "LEGION-100"
				if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
					return records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: issue.Key, Project: "LEGION", Seq: 1,
						Status: record.Carry(issue.Status), Parent: record.Carry("")})
				}); err != nil {
					t.Fatalf("seed the copy's row of %s under no parent: %v", issue.Key, err)
				}
			}
			if _, _, err := sup.Create(context.Background(), held, ""); err != nil {
				t.Fatalf("create the held %s: %v", tc.role, err)
			}
			failures := &claimFailures{}
			clock := time.Now()
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
				now: func() time.Time { return clock }, stateDir: t.TempDir(), handlers: []intake.Handler{failures}}
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: tc.role,
				Generation: 1, Phase: tc.phase, Task: "Work on it.", ResendEntry: tc.resend, Decided: tc.decided}, clock))

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run the start: %v", err)
			}
			m, _ := sup.Machine(token)
			m.Wait()
			got := m.Claim()
			if rows := outboxRows(t, pool); rows != 0 {
				t.Fatalf("the start left %d rows, want it finished: nothing later lifts the hold, so a row kept would retry for ever", rows)
			}
			facts := failures.applied()
			if told := len(facts) == 1 && facts[0].Issue == issue.Key && facts[0].Role == tc.role && facts[0].Cause == supervise.CauseCyberPolicy; told != tc.told ||
				len(facts) > 1 {
				t.Fatalf("the start told the workflow %+v, want one cyber_policy failure of the %s %t", facts, tc.role, tc.told)
			}
			if tc.launched {
				if spawns := len(rt.CallsOf("Spawn")); spawns != 1 || len(resumesOf(rt, token)) != 0 || got.State != supervise.StateLaunching ||
					got.Session != "" || got.Abandoned.Session != "ses-refused" {
					t.Fatalf("after the decided start the %s is %s, session %q (abandoned %q), with %d spawns and %d resumes; want it launching once in a new session",
						tc.role, got.State, got.Session, got.Abandoned.Session, spawns, len(resumesOf(rt, token)))
				}
				if spawned := rt.CallsOf("Spawn")[0].Spec.Tree; tc.role == claim.RoleArchitect && spawned != issue.Key {
					t.Fatalf("the decided start launched the root's architect under %q, want its own key %s, the gate root on the Dispatch chain", spawned, issue.Key)
				}
				return
			}
			if launches := len(resumesOf(rt, token)) + len(rt.CallsOf("Spawn")); launches != 0 || got.State != tc.state ||
				got.FailureCause != supervise.CauseCyberPolicy || got.Session != "ses-refused" {
				t.Fatalf("after the daemon's own start the %s is %s (%q, %q) with %d launches, want it %s for cyber policy with its session and never launched",
					tc.role, got.State, got.FailureCause, got.Session, launches, tc.state)
			}
		})
	}
}
