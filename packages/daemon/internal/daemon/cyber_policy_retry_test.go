package daemon

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The cases of this file are the daemon's automatic relaunches of a failed claim, each held to one
// rule: a claim failed for OpenAI's cyber-policy refusal of its session's content is relaunched by
// a retry someone decided alone. The relaunch resumes the recorded session, and the daemon's first
// prompt to it is input from outside the session, which ends Oh My Pi's own hold, so the refused
// content would go to OpenAI again and add a refusal to the organization's record.

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
// not retry a controller held for a cyber-policy refusal, after any wait: it logs one error naming
// the cause and the operator's `legion claims retry`, and asks the controller's machine for
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
			k := newControllerKeeper(context.Background(), sup, "legion", time.Minute, slog.New(slog.NewJSONHandler(&logs, nil)))
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
				held := strings.Split(strings.TrimSpace(logs.String()), "\n")
				var lines []string
				for _, line := range held {
					if strings.Contains(line, "the daemon's controller is held for OpenAI's cyber_policy refusal") {
						lines = append(lines, line)
					}
				}
				if len(lines) != 1 || !strings.Contains(lines[0], `"level":"ERROR"`) || !strings.Contains(lines[0], "legion claims retry") ||
					!strings.Contains(lines[0], "--fresh-session") || !strings.Contains(lines[0], `"cause":"cyber_policy"`) {
					t.Fatalf("the keeper logged %q, want one error naming the cause and the operator's retry:\n%s", lines, logs.String())
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

// A start row the workflow or admission writes for a failed claim retries it (the outbox's start).
// Every such start but one is the daemon's own — the next phase start of a role that fails outside
// its phase, a child architect's next phase start, a re-admission, a production check's look — and
// none of them relaunches a claim held for a cyber-policy refusal: the row finishes without acting
// and the claim stays failed with its session. The one start someone decided is the held phase's
// retry, which carries ResendEntry (workflow's restart: the architect's retry_or_escalate, the
// operator's `legion claims retry`, a person's todo on a held root), and it relaunches the claim.
func TestAStartRowRelaunchesAClaimHeldForCyberPolicyOnlyForTheHeldPhasesRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		state      supervise.ClaimState
		resend     bool
		relaunched bool
	}{
		{"a start of the daemon's own, of a failed claim", supervise.StateFailed, false, false},
		{"a start of the daemon's own, of a claim an operator stopped after it failed", supervise.StateRetired, false, false},
		{"the held phase's retry", supervise.StateFailed, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
			putOutboxIssue(t, pool, records, issue)
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			tester := mustClaimToken(t, issue.Key, claim.RoleTester)
			held := heldForCyberPolicy(supervise.Claim{Token: tester, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleTester})
			held.State = tc.state
			if _, _, err := sup.Create(context.Background(), held, ""); err != nil {
				t.Fatalf("create the held tester: %v", err)
			}
			clock := time.Now()
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
				now: func() time.Time { return clock }, stateDir: t.TempDir()}
			enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester,
				Generation: 1, Phase: phase.Testing, Task: "Test it.", ResendEntry: tc.resend}, clock))

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run the start: %v", err)
			}
			m, _ := sup.Machine(tester)
			m.Wait()
			got, launches := m.Claim(), len(resumesOf(rt, tester))+len(rt.CallsOf("Spawn"))
			if tc.relaunched {
				if launches != 1 || got.State != supervise.StateLaunching || outboxRows(t, pool) != 0 {
					t.Fatalf("after the held phase's retry the tester is %s with %d launches and %d rows, want it relaunched once and the row run",
						got.State, launches, outboxRows(t, pool))
				}
				return
			}
			if launches != 0 || got.State != tc.state || got.FailureCause != supervise.CauseCyberPolicy || got.Session != "ses-refused" {
				t.Fatalf("after the daemon's own start the tester is %s (%q, %q) with %d launches, want it %s for cyber policy with its session and never launched",
					got.State, got.FailureCause, got.Session, launches, tc.state)
			}
			if rows := outboxRows(t, pool); rows != 0 {
				t.Fatalf("the start left %d rows, want it finished: nothing later lifts the hold, so a row kept would retry for ever", rows)
			}
		})
	}
}
