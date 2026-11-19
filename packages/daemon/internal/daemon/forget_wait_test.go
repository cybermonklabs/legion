package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The daemon's own retries of a claim in a new session meet the hold's refusal of that new session
// (supervise's mayForget, review rounds 18 and 19 of #4220), and they log it by its cause. While
// the Envoy listener names the recorded session, which another claim's live process runs under, as
// the claim's role holder, the retry waits until that process stops, which in the case the refusal
// exists for is an operator's suspend of that claim: the controller keeper writes it once per
// episode at Warn, naming the claim to suspend and the bound, and each later retry's Warn carries
// the reason; a start row answers it as a wait (wait.ErrWaiting, the one wait contract), which
// settle raises to Warn from waitWarnAttempts. A lookup the listener refuses the same way every
// time, a wrong bearer's 401 among them, is a fault no suspend fixes: the keeper writes it once per
// episode at Error with the listener's status, and a start row answers it as a failure. Neither
// returns anything the keeper's loop logs at Error at each retry.
//
// The controls (the pull request records each run): the containment line written at Info fails
// the Warn row; the refused lookup written at Warn, as a wait, fails the Error row; forgetWait not
// matching wait.ErrWaiting fails the start row's wait.
func TestARetryHeldForItsRoleIsAWait(t *testing.T) {
	const taken = "ses-taken"
	refusedTwice := []supervise.TurnOutcome{{Generation: 1, Sequence: 1, Refused: true}, {Generation: 1, Sequence: 2, Refused: true}}
	// shared is a supervisor whose session taken another claim, legion-943's implementer,
	// recorded first, and whose listener answers every role lookup with lookup.
	shared := func(t *testing.T, lookup func(context.Context, string) (string, string, error)) *supervisor {
		t.Helper()
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		if _, err := sup.deps.Store.(*outboxClaimStore).HoldSessions(context.Background(), "legion",
			mustClaimToken(t, "LEGION-943", claim.RoleImplementer), supervise.RecordedHolds(taken, "")); err != nil {
			t.Fatalf("hold %s for the claim that recorded it first: %v", taken, err)
		}
		sup.deps.RoleHolder = lookup
		return sup
	}
	namesTaken := func(context.Context, string) (string, string, error) { return taken, "live", nil }
	unauthorized := func(context.Context, string) (string, string, error) {
		return "", "", fmt.Errorf("look up the holder: %w", &notify.RoleLookupRefused{Status: 401, Reason: `listener returned 401 Unauthorized: {"error":"unauthorized"}`})
	}
	// keeper runs a failed daemon controller, whose provider refused its recorded session out of
	// its budget, through the keeper's first look and three retries, an hour apart.
	keeper := func(t *testing.T, lookup func(context.Context, string) (string, string, error)) string {
		t.Helper()
		sup := shared(t, lookup)
		token := claim.ControllerToken("legion")
		if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Role: claim.RoleController,
			Generation: 1, State: supervise.StateFailed, Session: taken, SessionFile: "/tmp/controller.jsonl", TurnOutcomes: refusedTwice}, ""); err != nil {
			t.Fatalf("create the failed controller: %v", err)
		}
		var logs syncBuffer
		k := newControllerKeeper(context.Background(), sup, "legion", time.Minute, slog.New(slog.NewJSONHandler(&logs, nil)))
		clock := time.Now()
		k.now = func() time.Time { return clock }
		for i := range 4 {
			if err := k.keep(); err != nil {
				t.Fatalf("keep %d returned %v, which the keeper's loop logs at Error at each retry", i, err)
			}
			clock = clock.Add(time.Hour)
		}
		if m, _ := sup.Machine(token); m.Claim().Session != taken {
			t.Fatalf("the controller records %q, want %s kept", m.Claim().Session, taken)
		}
		return logs.String()
	}
	lineOf := func(logs, msg string) []string {
		var lines []string
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, msg) {
				lines = append(lines, line)
			}
		}
		return lines
	}

	t.Run("the controller keeper, the role held under the session", func(t *testing.T) {
		logs := keeper(t, namesTaken)
		held := lineOf(logs, "waits for the Envoy listener to free its role, or to answer who holds it")
		if len(held) != 1 {
			t.Fatalf("the keeper logged the wait %d times over three retries, want once:\n%s", len(held), logs)
		}
		for _, want := range []string{`"level":"WARN"`, "legion claims suspend --claim legion-legion-legion-943-implementer", "ten minutes"} {
			if !strings.Contains(held[0], want) {
				t.Errorf("the wait's line %s does not hold %s", held[0], want)
			}
		}
		retries := lineOf(logs, "controller: retrying the daemon's controller")
		if len(retries) != 3 || strings.Contains(retries[0], `"reason"`) || !strings.Contains(retries[2], `"reason"`) {
			t.Fatalf("the retries' Warn lines are %q, want three, each after the first held one naming its reason", retries)
		}
	})

	t.Run("the controller keeper, a lookup the listener refuses", func(t *testing.T) {
		logs := keeper(t, unauthorized)
		refused := lineOf(logs, "the Envoy listener refuses the lookup of its role's holder")
		if len(refused) != 1 {
			t.Fatalf("the keeper logged the refused lookup %d times over three retries, want once:\n%s", len(refused), logs)
		}
		for _, want := range []string{`"level":"ERROR"`, `"status":401`} {
			if !strings.Contains(refused[0], want) {
				t.Errorf("the refused lookup's line %s does not hold %s", refused[0], want)
			}
		}
		if held := lineOf(logs, "waits for the Envoy listener to free its role"); len(held) != 0 {
			t.Fatalf("the refused lookup was logged as a wait: %q", held)
		}
	})

	// The episode ends at its way out, not only once the claim is seen running (leaveHeld, review
	// round 19 of #4220): the role is freed, the next retry is admitted, and the launch fails, so
	// the claim fails again before the keeper ever sees it running. The retry after that is no
	// longer held, and its Warn names no reason, so it sends the operator to suspend no claim.
	// The control (the pull request records the run): the reason cleared only where the claim is
	// seen running; the later retry's Warn still names the freed role's holder.
	t.Run("the controller keeper, a retry admitted and then a launch that fails", func(t *testing.T) {
		var mu sync.Mutex
		freed := false
		sup := shared(t, func(context.Context, string) (string, string, error) {
			mu.Lock()
			defer mu.Unlock()
			if freed {
				return "", "", nil
			}
			return taken, "live", nil
		})
		runtime := sup.deps.Runtime.(*fake.Runtime)
		sup.deps.Limits.LaunchFailures = 1
		token := claim.ControllerToken("legion")
		if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Role: claim.RoleController,
			Generation: 1, State: supervise.StateFailed, Session: taken, SessionFile: "/tmp/controller.jsonl", TurnOutcomes: refusedTwice}, ""); err != nil {
			t.Fatalf("create the failed controller: %v", err)
		}
		var logs syncBuffer
		k := newControllerKeeper(context.Background(), sup, "legion", time.Minute, slog.New(slog.NewJSONHandler(&logs, nil)))
		clock := time.Now()
		k.now = func() time.Time { return clock }
		step := func() {
			t.Helper()
			if err := k.keep(); err != nil {
				t.Fatalf("keep: %v", err)
			}
			clock = clock.Add(time.Hour)
		}
		step() // the failure is seen, and the first retry is due
		step() // the retry is held: the listener names the session
		if held := lineOf(logs.String(), "waits for the Envoy listener to free its role"); len(held) != 1 {
			t.Fatalf("the held retry was logged %d times, want once:\n%s", len(held), logs.String())
		}
		mu.Lock()
		freed = true
		mu.Unlock()
		runtime.ScriptSpawn(fake.SpawnResult{Err: errors.New("pull the controller's image: not found")})
		// The admitted retry's failed launch is an error of the request, not a held retry.
		if err := k.keep(); err == nil || errors.Is(err, supervise.ErrForgetWaitsForRole) {
			t.Logf("the admitted retry answered %v", err)
		}
		clock = clock.Add(time.Hour)
		m, _ := sup.Machine(token)
		if state := m.Claim().State; state != supervise.StateFailed {
			t.Fatalf("after the admitted retry's failed launch the controller is %s, want failed: the row needs the claim failed again", state)
		}
		step() // the next retry
		retries := lineOf(logs.String(), "controller: retrying the daemon's controller")
		if len(retries) != 3 {
			t.Fatalf("the retries' Warn lines are %q, want three", retries)
		}
		if strings.Contains(retries[2], `"reason"`) {
			t.Fatalf("the retry after an admitted one names a reason: %s; the held episode ended when the retry was admitted", retries[2])
		}
	})

	t.Run("a start row", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			lookup func(context.Context, string) (string, string, error)
			wait   bool
		}{
			{"the role held under the session", namesTaken, true},
			{"a lookup the listener refuses", unauthorized, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := isolatedOutboxPool(t)
				records := record.NewStore()
				issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
				putOutboxIssue(t, pool, records, issue)
				sup := shared(t, tc.lookup)
				tester := mustClaimToken(t, issue.Key, claim.RoleTester)
				if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: tester, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleTester,
					Generation: 1, State: supervise.StateFailed, Session: taken, SessionFile: "/tmp/tester.jsonl"}, ""); err != nil {
					t.Fatalf("create the failed tester: %v", err)
				}
				clock := time.Now()
				runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
					now: func() time.Time { return clock }}
				row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester,
					Generation: 1, Phase: phase.Testing, Task: "Test it.", FreshSession: true}, clock)
				err := runner.execute(context.Background(), row)
				var refused *supervise.RefusedError
				if !errors.As(err, &refused) {
					t.Fatalf("the start row = %v, want the hold's refusal", err)
				}
				if got := errors.Is(err, wait.ErrWaiting); got != tc.wait {
					t.Fatalf("the start row = %v, a wait %t; want a wait %t", err, got, tc.wait)
				}
				if m, _ := sup.Machine(tester); m.Claim().Session != taken {
					t.Fatalf("the tester records %q, want %s kept", m.Claim().Session, taken)
				}
			})
		}
	})
}
