package supervise

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A claim the daemon fails carries one word for what failed it, beside the sentence it already
// wrote and the moment it failed, so the architect that must choose between a retry and an
// escalation reads a word it can decide on rather than building its own classifier out of a
// sentence that covers four different events.
//
// The first case drives every event that can fail a claim to its end and reads the record: the
// word is the failing site's own, the sentence is the one the daemon writes today, and the four
// counters are read rather than copied — after the failure they hold whatever the failing path
// left, which is what a reader is served.
//
// The three that follow are the record's own lifetime. A claim no failure stands over carries
// none, which is every claim the daemon ever ran but one it failed and has not been given
// another run: the retry clears the record with the counts it names, and the launch of that run
// clears it again. A claim an operator retired after it failed is the one that keeps it, since
// nothing has run it again. And a daemon replaced between the event that asked for the relaunch
// and the budget running out writes the cause of that event, not the refusal that happened to
// spend the last of the budget: the relaunch lives in the replaced daemon's memory and the
// cause has to live in the claim (Claim.PendingCause).
func TestAFailedClaimCarriesItsCause(t *testing.T) {
	limit := testLimits().LaunchFailures

	t.Run("every event that fails a claim", func(t *testing.T) {
		for _, row := range []struct {
			// name is the event as the issue's own check names it.
			name string
			// drive fails a claim and answers it with the clock reading it took immediately
			// before the step that failed it, which bounds the moment the record must carry.
			drive   func(*testing.T) (*harness, time.Time)
			cause   FailureCause
			detail  string
			budgets Budgets
		}{{
			name: "a launch the runtime refuses until the launch budget runs out",
			drive: func(t *testing.T) (*harness, time.Time) {
				h := newHarness(t)
				h.rt.ScriptSpawn(fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom})
				at := h.clock.Now()
				if err := h.handle(RequestSpawn{Claim: h.token}); !errors.Is(err, errBoom) {
					t.Fatalf("the spawn returned %v, want the runtime's last refusal", err)
				}
				return h, at
			},
			cause:   CauseLaunchRefused,
			detail:  "launch failures ran out",
			budgets: Budgets{LaunchFailures: limit},
		}, {
			name: "a process that dies with work outstanding until the deaths run out",
			drive: func(t *testing.T) (*harness, time.Time) {
				h := newHarness(t)
				h.reach(StateReady)
				h.must(RequestDeliver{Claim: h.token, Task: "the task"})
				h.must(StreamTurnStart{Claim: h.token})
				for death := 1; death < limit; death++ {
					h.observe(runtime.Gone)
					h.relaunched()
					h.must(StreamTurnStart{Claim: h.token})
				}
				at := h.clock.Now()
				h.observe(runtime.Gone)
				return h, at
			},
			cause:   CauseDied,
			detail:  "deaths with work outstanding ran out",
			budgets: Budgets{Deaths: limit},
		}, {
			name: "a process reported dead again after its charge, until the launch budget runs out",
			drive: func(t *testing.T) (*harness, time.Time) {
				h := newHarness(t)
				h.launch()
				for range limit - 1 {
					h.observe(runtime.Gone)
				}
				h.wantBudgets(Budgets{LaunchFailures: limit - 1})
				// The relaunch this death raises is refused by a lifecycle check the store will
				// not answer, which leaves the claim standing over its dead process with that
				// death already charged. What brings such a claim back is the orphan sweep
				// reporting the same process again (Claim.DeathCharged, died).
				dead := h.locator()
				store := &unansweredCheckLaunchStore{memStore: h.store, failing: true}
				h.m.deps.Store = store
				gone := RuntimeObservation{Observation: runtime.Observation{Locator: dead, Kind: runtime.Gone, At: h.clock.Now()}}
				if err := h.handle(gone); err == nil {
					t.Fatal("the relaunch the death raised was taken, want the lifecycle recheck's refusal of it")
				}
				store.failing = false
				at := h.clock.Now()
				h.must(gone)
				return h, at
			},
			cause:   CauseDied,
			detail:  "launch failures ran out",
			budgets: Budgets{LaunchFailures: limit},
		}, {
			name: "an agent that never registers before its deadline, until the launch budget runs out",
			drive: func(t *testing.T) (*harness, time.Time) {
				h := newHarness(t)
				h.reach(StateRegistered)
				for range limit - 1 {
					h.advance(deadline)
					h.wantState(StateLaunching)
					h.connect()
					h.register()
				}
				at := h.clock.Now()
				h.advance(deadline)
				return h, at
			},
			cause:   CauseNeverRegistered,
			detail:  "launch failures ran out",
			budgets: Budgets{LaunchFailures: limit},
		}, {
			name: "a relaunch onto the daemon's own plugin contract that runs out",
			drive: func(t *testing.T) (*harness, time.Time) {
				h := newHarness(t)
				h.launch()
				h.m.deps.PluginRelaunchable = true
				register := func() {
					t.Helper()
					err := h.registerStale(RequestRegister{
						Claim: h.token, Generation: h.generation(), Session: session, SessionFile: sessionFile,
						CapabilityHash: []byte("capability"), PluginContract: 1,
					})
					var refusal claim.Refusal
					if !errors.As(err, &refusal) || refusal != claim.StalePluginContract(1, h.deps.PluginContract) {
						t.Fatalf("the stale registration returned %v, want the refusal naming both contracts", err)
					}
				}
				// The first relaunch is free and marks the claim; every one after it, still
				// finding a plugin behind this daemon, is charged. The relaunch is the one the
				// refusal posts, run as the daemon's inbox runs it (RelaunchStaleRegistration).
				for range limit {
					register()
				}
				at := h.clock.Now()
				register()
				return h, at
			},
			cause:   CauseStalePlugin,
			detail:  "launch failures ran out",
			budgets: Budgets{LaunchFailures: limit},
		}, {
			name: "prompt retirements that run out",
			drive: func(t *testing.T) (*harness, time.Time) {
				h := newHarness(t)
				h.reach(StateReady)
				h.must(RequestDeliver{Claim: h.token, Task: "the task"})
				// Two prompts the agent acknowledges and starts no turn on, then the third,
				// which retires the process and relaunches the same session.
				for range testLimits().PromptFailures - 1 {
					h.advance(testRPC)
					h.observe(runtime.Alive)
				}
				h.advance(testRPC)
				h.relaunched()
				for range testLimits().PromptFailures - 1 {
					h.advance(testRPC)
					h.observe(runtime.Alive)
				}
				at := h.clock.Now()
				h.advance(testRPC)
				return h, at
			},
			cause:   CausePromptRetiresExhausted,
			detail:  "prompt retirements ran out",
			budgets: Budgets{PromptFailures: 3, PromptRetires: 2},
		}, {
			name: "a provider error that cannot clear",
			drive: func(t *testing.T) (*harness, time.Time) {
				h := workingClaim(t)
				at := h.clock.Now()
				h.errorTurn(StreamPromptResult{Error: "invalid_request_error", HTTPStatus: 400})
				return h, at
			},
			cause:   CauseProviderError,
			detail:  "a provider error that cannot clear ended this claim's turn",
			budgets: Budgets{},
		}} {
			t.Run(row.name, func(t *testing.T) {
				h, at := row.drive(t)

				h.wantState(StateFailed)
				h.wantFailure(row.cause, row.detail, at)
				h.wantBudgets(row.budgets)
				if stored := h.store.load(h.token); stored.FailureCause != row.cause || stored.PendingCause != "" {
					t.Errorf("the stored claim carries cause %q with pending cause %q, want %q with the pending cause consumed",
						stored.FailureCause, stored.PendingCause, row.cause)
				}
			})
		}
	})

	t.Run("a claim no failure stands over carries no record", func(t *testing.T) {
		for _, row := range []struct {
			name  string
			drive func(*testing.T) *harness
		}{{
			name:  "a fresh claim nothing has launched",
			drive: func(t *testing.T) *harness { return newHarness(t) },
		}, {
			name: "a claim whose agent is ready",
			drive: func(t *testing.T) *harness {
				h := newHarness(t)
				h.reach(StateReady)
				return h
			},
		}, {
			name: "a suspended claim",
			drive: func(t *testing.T) *harness {
				h := newHarness(t)
				h.reach(StateSuspended)
				return h
			},
		}, {
			name: "a claim that failed and was launched again",
			drive: func(t *testing.T) *harness {
				h := newHarness(t)
				h.launch()
				for range limit {
					h.observe(runtime.Gone)
				}
				h.wantState(StateFailed)
				h.wantFailure(CauseDied, "launch failures ran out", epoch)
				h.must(RequestRetry{Claim: h.token})
				h.wantState(StateLaunching)
				return h
			},
		}} {
			t.Run(row.name, func(t *testing.T) {
				h := row.drive(t)

				h.wantNoFailure()
				if got := h.claim().PendingCause; got != "" {
					t.Errorf("the claim carries the pending cause %q, want none", got)
				}
				if stored := h.store.load(h.token); stored.FailureCause != "" || !stored.FailedAt.IsZero() {
					t.Errorf("the stored claim carries cause %q at %s, want no record of a failure",
						stored.FailureCause, stored.FailedAt)
				}
			})
		}
	})

	// A failed claim an operator stops is retired with its record standing: nothing has launched
	// it, so what failed it is still what a reader of it must act on.
	t.Run("an operator's retirement of a failed claim keeps the record", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.must(RequestDeliver{Claim: h.token, Task: "the task"})
		h.must(StreamTurnStart{Claim: h.token})
		for death := 1; death < limit; death++ {
			h.observe(runtime.Gone)
			h.relaunched()
			h.must(StreamTurnStart{Claim: h.token})
		}
		at := h.clock.Now()
		h.observe(runtime.Gone)
		h.wantState(StateFailed)

		h.must(RequestStop{Claim: h.token})

		h.wantState(StateRetired)
		h.wantFailure(CauseDied, "deaths with work outstanding ran out", at)
		h.wantBudgets(Budgets{Deaths: limit})
	})

	// The daemon is replaced between the death that asked for the relaunch and the budget running
	// out. The relaunch itself was in the replaced daemon's memory, and the claim the new one
	// builds from the store is what the cause has to survive in: without it the refusal that
	// spends the last of the launch budget would write launch_refused for a claim that died.
	t.Run("a daemon restart between the death and the budget running out", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)

		h.observe(runtime.Gone)

		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{LaunchFailures: 1})
		if stored := h.store.load(h.token).PendingCause; stored != CauseDied {
			t.Fatalf("the stored claim carries the pending cause %q, want %q: the restart reads this and nothing else", stored, CauseDied)
		}

		h.restart()
		// The pod the relaunch started moved, and the repoint's own launches — charged like any
		// other, and asked for by nothing that failed — are refused until the budget is spent.
		h.rt.ScriptResume(fake.SpawnResult{Err: errBoom}, fake.SpawnResult{Err: errBoom})
		at := h.clock.Now()
		if err := h.handle(RuntimeObservation{Observation: runtime.Observation{
			Locator: h.locator(), Kind: runtime.StaleAddress, At: h.clock.Now(),
		}}); !errors.Is(err, errBoom) {
			t.Fatalf("the repoint returned %v, want the runtime's last refusal", err)
		}

		h.wantState(StateFailed)
		h.wantFailure(CauseDied, "launch failures ran out", at)
		h.wantBudgets(Budgets{LaunchFailures: limit})
	})
}
