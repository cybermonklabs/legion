package supervise

// Every start resumes the session its claim recorded, and the two ways that can fail are not the
// same failure: dispatch://INFRA-714 R8. A session no store holds is gone, so the claim starts
// fresh, uncharged, and the loss is reported; a store that cannot be asked says nothing about the
// session, so the claim keeps it and waits.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A resume the runtime answers with runtime.ErrSessionFileMissing — which both runtimes answer for
// a session no store holds — spawns a fresh agent in the same try, charges nothing, tells the
// workflow, and builds that spawn for a claim that knows its session was lost, so the new agent is
// told in its own prompt.
func TestAResumeOfAMissingSessionStartsFreshUnchargedAndReportsTheLoss(t *testing.T) {
	h := newBareHarness(t)
	var reported []Claim
	var details []string
	h.deps.SessionLost = func(c Claim, _ SessionLoss, detail string) {
		reported = append(reported, c)
		details = append(details, detail)
	}
	h.store.PutClaim(h.ctx, queuedClaim())
	h.start(queuedClaim())
	h.reach(StateIdle)
	h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: no OMP session anywhere", testToken, runtime.ErrSessionFileMissing)})

	h.observe(runtime.Gone)

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1}) // the death's own charge, and nothing for the lost session
	spawn := h.wantCalls("Spawn", 2)[1]
	if spawn.Spec.ResumeSessionFile != "" {
		t.Errorf("the fresh start is %+v, want it to resume nothing", spawn.Spec)
	}
	if c := h.claim(); c.Session != "" || c.SessionFile != "" {
		t.Errorf("claim %+v after the lost session, want it recording none", c)
	}
	built := h.specs.last(t)
	if built.SessionLost != SessionLossGone {
		t.Error("the fresh agent's spec was built for a claim that does not know its session was lost")
	}
	if len(reported) != 1 {
		t.Fatalf("the loss was reported %d times, want once", len(reported))
	}
	if !strings.Contains(details[0], session) || !strings.Contains(details[0], "no session store") {
		t.Errorf("the loss was reported as %q, want it to name the session and why", details[0])
	}
}

// A start the session store could not answer is a store fault, never a missing session: the claim
// keeps its session, nothing is charged, the same session is started again a minute later, and the
// issue and the controller are told at once and then hourly rather than every minute.
func TestAStartTheSessionStoreCannotAnswerWaitsAndIsReportedHourly(t *testing.T) {
	h := newBareHarness(t)
	var faults []string
	h.deps.SessionStoreFault = func(_ Claim, detail string) { faults = append(faults, detail) }
	h.store.PutClaim(h.ctx, queuedClaim())
	h.start(queuedClaim())
	h.reach(StateIdle)
	storeDown := fmt.Errorf("resume %s: %w: dial tcp: connection refused", testToken, runtime.ErrSessionStore)
	h.rt.OnResume = func(context.Context) error { return storeDown }

	// The death's relaunch reports the store's refusal to its caller — the sweep, or the outbox
	// row — which is how a start that nothing charged is still visible as a failure.
	if err := h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}}); !errors.Is(err, runtime.ErrSessionStore) {
		t.Fatalf("the death's relaunch answered %v, want the store's refusal", err)
	}

	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1}) // the death's charge alone: the store fault costs nothing
	c := h.claim()
	if c.Session != session || c.SessionFile != sessionFile {
		t.Fatalf("claim %+v after the store fault, want its session kept", c)
	}
	if !strings.Contains(c.LastStartFailure, "connection refused") {
		t.Errorf("the claim records the failed start as %q, want the store's own refusal", c.LastStartFailure)
	}
	if len(faults) != 1 {
		t.Fatalf("the store fault was reported %d times, want once at the first start that met it", len(faults))
	}

	// A minute later the same session is tried again, and the second fault inside the hour is not
	// reported again.
	h.advance(sessionStoreRetry)
	h.wantCalls("Resume", 2)
	h.wantBudgets(Budgets{LaunchFailures: 1})
	if len(faults) != 1 {
		t.Fatalf("the store fault was reported %d times within the hour, want once", len(faults))
	}
	if resumed := h.calls("Resume")[1]; resumed.Spec.ResumeSessionFile != sessionFile {
		t.Errorf("the retry is %+v, want the same session %s", resumed.Spec, sessionFile)
	}

	// An hour of those retries on, the people who can fix the store are told again — once, not
	// once a minute — and the claim is still waiting for the store with nothing charged.
	h.advance(sessionStoreReport)
	if len(faults) != 2 {
		t.Fatalf("the store fault was reported %d times after an hour of retries, want twice", len(faults))
	}
	h.wantState(StateLaunching)
	h.wantBudgets(Budgets{LaunchFailures: 1})
}

// A suspension of a claim whose start is still running stops that start and suspends the claim,
// rather than waiting out a launch that can take minutes; the stopped start is charged nothing.
func TestASuspendOfAStartingWorkerStopsTheStartAndChargesNothing(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	started, cut := make(chan struct{}), make(chan error, 1)
	h.rt.OnResume = func(ctx context.Context) error {
		close(started)
		<-ctx.Done() // the start waits in the runtime until something ends its context
		cut <- ctx.Err()
		return ctx.Err()
	}

	dying := make(chan error, 1)
	go func() {
		dying <- h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}})
	}()
	<-started

	// The suspension arrives while the relaunch is in the runtime: it stops that start before it
	// waits for the machine's decision, which the start is holding.
	suspended := make(chan error, 1)
	go func() {
		suspended <- h.handle(RequestSuspend{Claim: testToken, Reason: "the operator suspended it", By: SuspendedByOperator})
	}()
	if err := <-cut; err == nil {
		t.Fatal("the start ran to completion, want it cut short by the suspension")
	}
	<-dying
	if err := <-suspended; err != nil {
		t.Fatalf("the suspend answered %v, want the claim suspended", err)
	}
	h.rt.OnResume = nil

	h.wantState(StateSuspended)
	h.wantBudgets(Budgets{LaunchFailures: 1}) // the death's charge alone: the cancelled start is charged nothing
	if c := h.claim(); c.SuspendedBy != SuspendedByOperator || c.Session != session {
		t.Fatalf("claim %+v, want it suspended by the operator with its session kept", c)
	}
}

// A process that exited on a session store that stopped answering it (runtime.Observation's
// StoreFault) is no death the claim is charged for: the claim keeps its session, waits launching
// with no process, tells the issue and the controller, and starts the same session again once the
// minute is out, still charging nothing. The control is the same death on a store that answers,
// which charges a launch failure and relaunches at once.
func TestAProcessThatExitedOnAStoreThatStoppedAnsweringIsWaitedOutUncharged(t *testing.T) {
	for name, storeFault := range map[string]bool{"on a store that stopped answering": true, "control: on a store that answers": false} {
		t.Run(name, func(t *testing.T) {
			h := newBareHarness(t)
			var faults []string
			h.deps.SessionStoreFault = func(_ Claim, detail string) { faults = append(faults, detail) }
			h.store.PutClaim(h.ctx, queuedClaim())
			h.start(queuedClaim())
			h.reach(StateIdle)
			resumes := len(h.calls("Resume"))

			h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
				Detail: "role tester reports generation 1 exited (code 1, signal )", StoreFault: storeFault}})

			if !storeFault {
				h.wantBudgets(Budgets{LaunchFailures: 1})
				h.wantCalls("Resume", resumes+1)
				if len(faults) != 0 {
					t.Fatalf("a death on a store that answers told %d store faults, want none", len(faults))
				}
				return
			}
			h.wantState(StateLaunching)
			h.wantBudgets(Budgets{})
			c := h.claim()
			if c.Session != session || c.SessionFile != sessionFile || c.Locator != nil {
				t.Fatalf("claim %+v after the store fault, want its session kept and no process recorded", c)
			}
			if len(faults) != 1 || !strings.Contains(faults[0], "exited (code 1") {
				t.Fatalf("the store fault was told %q, want it told once with the runtime's account of the exit", faults)
			}
			h.advance(sessionStoreRetry - time.Second)
			h.wantCalls("Resume", resumes)
			h.advance(time.Second)
			if resumed := h.wantCalls("Resume", resumes+1)[resumes]; resumed.Spec.ResumeSessionFile != sessionFile {
				t.Errorf("the minute's try is %+v, want the same session %s", resumed.Spec, sessionFile)
			}
			h.wantBudgets(Budgets{})
		})
	}
}

// An outage that ends, an agent registering once its process opened its session, and returns ten
// minutes later is an outage of its own: the issue and the controller are told of it at once, not
// an hour after the first report. The hourly rule holds while one outage lasts
// (TestAStartTheSessionStoreCannotAnswerWaitsAndIsReportedHourly,
// TestOneOutageWhoseTriesLaunchIsToldOnceUntilAnAgentRegisters).
func TestAStoreFaultAfterTheStoreAnsweredAStartIsToldAtOnce(t *testing.T) {
	h := newBareHarness(t)
	var faults []string
	h.deps.SessionStoreFault = func(_ Claim, detail string) { faults = append(faults, detail) }
	h.store.PutClaim(h.ctx, queuedClaim())
	h.start(queuedClaim())
	h.reach(StateIdle)
	diesOnTheStore := func() {
		h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
			Detail: "role tester reports generation exited (code 1, signal )", StoreFault: true}})
	}

	diesOnTheStore()
	if len(faults) != 1 {
		t.Fatalf("the first outage was told %d times, want once", len(faults))
	}
	// The minute's try finds the store answering: the same session launches and its agent registers.
	h.advance(sessionStoreRetry)
	h.wantState(StateLaunching)
	h.reach(StateReady)

	h.advance(10 * time.Minute)
	diesOnTheStore()
	if len(faults) != 2 {
		t.Fatalf("an outage ten minutes after the store answered a start was told %d times in all, want twice: at once", len(faults))
	}
}

// One outage in which each minute's try launches and its process dies on the store before its
// agent registers is one outage, told at once and then hourly: a fresh Oh My Pi start asks the
// store nothing before its child does, so a start that launches says nothing of the store, and a
// store that answers each start's lookup and fails each child's own open is down all the same. An
// agent that registers has opened its session, which the store answered: that ends the outage, so
// the next store fault is told at once.
func TestOneOutageWhoseTriesLaunchIsToldOnceUntilAnAgentRegisters(t *testing.T) {
	for name, tc := range map[string]struct{ first, each ClaimState }{
		"each try's process dies once its shim connected":                              {first: StateShimConnected, each: StateShimConnected},
		"the first process ran, then each try's process dies before its shim connects": {first: StateIdle, each: StateLaunching},
	} {
		t.Run(name, func(t *testing.T) {
			h := newBareHarness(t)
			var faults []string
			h.deps.SessionStoreFault = func(_ Claim, detail string) { faults = append(faults, detail) }
			h.store.PutClaim(h.ctx, queuedClaim())
			h.start(queuedClaim())
			h.reach(tc.first)
			diesOnTheStore := func() {
				h.must(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
					Detail: "role tester reports generation exited (code 1, signal )", StoreFault: true}})
				h.wantState(StateLaunching)
				h.wantBudgets(Budgets{})
			}

			diesOnTheStore()
			for range 5 {
				h.advance(sessionStoreRetry)
				h.wantState(StateLaunching)
				h.reach(tc.each)
				diesOnTheStore()
			}
			if len(faults) != 1 {
				t.Fatalf("one outage of six tries over five minutes, each try launched and each process died on the store before its agent registered, was told %d times, want once", len(faults))
			}

			h.advance(sessionStoreRetry)
			h.wantState(StateLaunching)
			h.reach(StateRegistered)
			diesOnTheStore()
			if len(faults) != 2 {
				t.Fatalf("a store fault after an agent registered was told %d times in all, want twice: at once", len(faults))
			}
		})
	}
}

// The loss of a recorded session is reported before the fresh agent starts, so a fresh spawn that
// fails, charged as a launch failure, still leaves the loss reported once: the retry is a spawn of
// a claim that records no session, which reports nothing.
func TestALostSessionIsReportedOnceEvenWhenItsFreshSpawnFails(t *testing.T) {
	h := newBareHarness(t)
	var reported []string
	h.deps.SessionLost = func(_ Claim, _ SessionLoss, detail string) { reported = append(reported, detail) }
	h.store.PutClaim(h.ctx, queuedClaim())
	h.start(queuedClaim())
	h.reach(StateIdle)
	h.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: no OMP session anywhere", testToken, runtime.ErrSessionFileMissing)})
	h.rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("the pod never came")})

	h.observe(runtime.Gone)

	if len(reported) != 1 || !strings.Contains(reported[0], session) {
		t.Fatalf("the loss was reported %q, want it reported once, naming %s", reported, session)
	}
	if c := h.claim(); c.Session != "" || c.Budgets.LaunchFailures < 2 {
		t.Fatalf("claim %+v, want the session forgotten and the failed spawn charged beside the death", c)
	}
}
