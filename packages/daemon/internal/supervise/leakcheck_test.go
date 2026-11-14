package supervise

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// leakCheckSentence is a cause the sandbox runtime refuses a start with when the shared public
// leak check could not be put in place in the role's pod (leakguard.Cause).
const leakCheckSentence = "the shared public leak check could not be installed: GitHub answered the read of theorem-labs/agent-infra at leak-check-v3 with HTTP 404: Not Found"

// leakCheckRefused is a start the runtime refused on the leak check, wrapped as the sandbox
// runtime's relaunch wraps every start error.
func leakCheckRefused(h *harness) error {
	return fmt.Errorf("launch %s: start through its authenticated launcher: %w",
		h.token, runtime.LeakCheckRefusal{Cause: leakCheckSentence})
}

// launchingWrites is every write of the claim that recorded a launch try: the write launchNow
// makes before it asks the runtime for the process, in order.
func launchingWrites(h *harness) []Claim {
	var tries []Claim
	for _, c := range h.store.history() {
		if c.State == StateLaunching && c.Locator == nil {
			tries = append(tries, c)
		}
	}
	return tries
}

// A start the runtime refuses because the shared public leak check could not be installed is a
// launch failure like any other, and the claim carries the refusal's sentence in a field of its
// own beside the failure record, so a reader of the claim is told which check refused it rather
// than having to find it inside the failure's sentence. The field follows the latest try: it
// reads the same across the backoff between two refused tries, and goes as soon as a try starts
// its process or fails for another reason.
func TestALaunchTheLeakCheckRefusedCarriesItsCause(t *testing.T) {
	limit := testLimits().LaunchFailures

	t.Run("refused tries run out and fail the claim with the cause beside the failure", func(t *testing.T) {
		h := newHarness(t)
		refused := leakCheckRefused(h)
		for range limit {
			h.rt.ScriptSpawn(fake.SpawnResult{Err: refused})
		}

		err := h.handle(RequestSpawn{Claim: h.token})
		if got := runtime.LeakCheckCause(err); got != leakCheckSentence {
			t.Fatalf("the spawn returned %v, carrying the leak-check cause %q; want the runtime's refusal", err, got)
		}

		h.wantCalls("Spawn", limit)
		// Each refused try is charged one launch failure, and every try after the first is
		// written carrying the cause of the one before it.
		tries := launchingWrites(h)
		if len(tries) != limit {
			t.Fatalf("the claim was written launching %d times, want once per try (%d)", len(tries), limit)
		}
		for i, try := range tries {
			want := leakCheckSentence
			if i == 0 {
				want = ""
			}
			if try.Budgets.LaunchFailures != i || try.LeakCheckCause != want {
				t.Errorf("try %d was written with %d launch failures and the cause %q, want %d and %q",
					i+1, try.Budgets.LaunchFailures, try.LeakCheckCause, i, want)
			}
		}

		h.wantState(StateFailed)
		h.wantBudgets(Budgets{LaunchFailures: limit})
		got := h.claim()
		if want := "launch failures ran out: " + refused.Error(); got.Failure != want {
			t.Errorf("the claim's failure is %q, want %q", got.Failure, want)
		}
		if got.FailureCause != CauseLaunchRefused || got.LeakCheckCause != leakCheckSentence {
			t.Errorf("the claim carries cause %q and leak-check cause %q, want %q and %q",
				got.FailureCause, got.LeakCheckCause, CauseLaunchRefused, leakCheckSentence)
		}
		// A refusal started no agent: no process is recorded, and nothing was ever ready.
		if got.Locator != nil {
			t.Errorf("the claim records the process %+v, want none: the runtime started nothing", *got.Locator)
		}
		if stored := h.store.load(h.token); stored.LeakCheckCause != leakCheckSentence || stored.Locator != nil {
			t.Errorf("the stored claim carries the leak-check cause %q and the process %v, want %q and none",
				stored.LeakCheckCause, stored.Locator, leakCheckSentence)
		}
	})

	t.Run("a later successful launch clears it", func(t *testing.T) {
		h := newHarness(t)
		h.rt.ScriptSpawn(fake.SpawnResult{Err: leakCheckRefused(h)})

		h.launch()

		h.wantCalls("Spawn", 2)
		if tries := launchingWrites(h); len(tries) != 2 || tries[1].LeakCheckCause != leakCheckSentence {
			t.Fatalf("the launching writes are %+v, want two, the second carrying the first try's cause", tries)
		}
		got := h.claim()
		if got.Locator == nil || got.LeakCheckCause != "" {
			t.Fatalf("the claim records the process %v and the leak-check cause %q, want a process and no cause",
				got.Locator, got.LeakCheckCause)
		}
		if stored := h.store.load(h.token); stored.LeakCheckCause != "" {
			t.Errorf("the stored claim carries the leak-check cause %q, want none once a launch started its process", stored.LeakCheckCause)
		}
		h.wantBudgets(Budgets{LaunchFailures: 1})
	})

	t.Run("a retry of the failed claim clears it, and a try refused for another reason carries none", func(t *testing.T) {
		h := newHarness(t)
		refused := leakCheckRefused(h)
		for range limit {
			h.rt.ScriptSpawn(fake.SpawnResult{Err: refused})
		}
		if err := h.handle(RequestSpawn{Claim: h.token}); runtime.LeakCheckCause(err) == "" {
			t.Fatalf("the spawn returned %v, want the runtime's refusal", err)
		}
		h.wantState(StateFailed)
		before := len(launchingWrites(h))

		h.rt.ScriptSpawn(fake.SpawnResult{Err: errBoom})
		h.must(RequestRetry{Claim: h.token})

		h.wantState(StateLaunching)
		tries := launchingWrites(h)[before:]
		if len(tries) != 2 {
			t.Fatalf("the retry wrote %d launch tries, want 2", len(tries))
		}
		for i, try := range tries {
			if try.LeakCheckCause != "" {
				t.Errorf("try %d of the retry was written with the leak-check cause %q, want none", i+1, try.LeakCheckCause)
			}
		}
		h.wantNoFailure()
		if got := h.claim().LeakCheckCause; got != "" {
			t.Errorf("the claim carries the leak-check cause %q, want none", got)
		}
	})

	// A death relaunches the same session through relaunchAfterFailure, which records the death
	// as the cause the claim fails under. A relaunch whose every try the leak check refuses runs
	// out in launchNow, under that cause, with the sentence beside it.
	t.Run("a relaunch after a death whose refused tries run out", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		refused := leakCheckRefused(h)
		for range limit - 1 {
			h.rt.ScriptResume(fake.SpawnResult{Err: refused})
		}

		err := h.handle(RuntimeObservation{Observation: runtime.Observation{
			Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(),
		}})
		if runtime.LeakCheckCause(err) != leakCheckSentence {
			t.Fatalf("the relaunch returned %v, want the runtime's refusal", err)
		}

		h.wantState(StateFailed)
		h.wantBudgets(Budgets{LaunchFailures: limit})
		got := h.claim()
		if want := "launch failures ran out: " + refused.Error(); got.Failure != want {
			t.Errorf("the claim's failure is %q, want %q", got.Failure, want)
		}
		if got.FailureCause != CauseDied || got.LeakCheckCause != leakCheckSentence {
			t.Errorf("the claim carries cause %q and leak-check cause %q, want %q and %q",
				got.FailureCause, got.LeakCheckCause, CauseDied, leakCheckSentence)
		}
		if got.Locator != nil {
			t.Errorf("the claim records the process %+v, want none: the runtime started nothing", *got.Locator)
		}
	})

	// A relaunch that runs out in relaunchAfterFailure itself fails with the bare sentence, and a
	// process had to start for there to be anything to die: that start is the latest try, so the
	// cause of the refused try before it is gone.
	t.Run("a relaunch after a death that runs out through relaunchAfterFailure", func(t *testing.T) {
		h := newHarness(t)
		h.reach(StateReady)
		h.rt.ScriptResume(fake.SpawnResult{Err: leakCheckRefused(h)})

		h.observe(runtime.Gone)

		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{LaunchFailures: limit - 1})
		if tries := launchingWrites(h); tries[len(tries)-1].LeakCheckCause != leakCheckSentence {
			t.Fatalf("the try after the refused one was written with the leak-check cause %q, want %q",
				tries[len(tries)-1].LeakCheckCause, leakCheckSentence)
		}

		h.observe(runtime.Gone)

		h.wantState(StateFailed)
		h.wantBudgets(Budgets{LaunchFailures: limit})
		got := h.claim()
		if got.Failure != "launch failures ran out" || got.FailureCause != CauseDied || got.LeakCheckCause != "" {
			t.Errorf("the claim carries failure %q, cause %q and leak-check cause %q; want %q, %q and none",
				got.Failure, got.FailureCause, got.LeakCheckCause, "launch failures ran out", CauseDied)
		}
	})
}

// A start cut short by its own context — a suspension that stops it, or the daemon's own stop —
// was refused by nothing, so it leaves the claim no leak-check cause even where the runtime's
// answer was a LeakCheckRefusal: the sandbox runtime answers a read or a push of the check that
// its start context ended with one (launchers.serveLeakCheck, leakguard.Source.Get), and its
// sentence ends in that context's cancellation, not in anything wrong with the pod's check. A
// claim suspended in the middle of that push that carried the sentence would tell an operator
// reading `legion claims list` or the state that the pod's check could not be installed, when
// all that happened is that the start was stopped.
func TestAStartCutShortByItsContextLeavesNoLeakCheckCause(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	started, cut := make(chan struct{}), make(chan error, 1)
	h.rt.OnResume = func(ctx context.Context) error {
		close(started)
		<-ctx.Done() // the push of the check waits in the runtime until something ends its context
		cut <- ctx.Err()
		return fmt.Errorf("launch %s: start through its authenticated launcher: %w", h.token, runtime.LeakCheckRefusal{
			Cause: "the shared public leak check could not be installed: the start stopped waiting for the daemon's read of theorem-labs/agent-infra at leak-check-v3: " + ctx.Err().Error(),
		})
	}

	dying := make(chan error, 1)
	go func() {
		dying <- h.handle(RuntimeObservation{Observation: runtime.Observation{Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now()}})
	}()
	<-started
	suspended := make(chan error, 1)
	go func() {
		suspended <- h.handle(RequestSuspend{Claim: h.token, Reason: "the operator suspended it", By: SuspendedByOperator})
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
	if got := h.claim().LeakCheckCause; got != "" {
		t.Errorf("the suspended claim carries the leak-check cause %q, want none: its start was stopped, not refused", got)
	}
	history := h.store.history()
	if got := history[len(history)-1].LeakCheckCause; got != "" {
		t.Errorf("the suspended claim was written with the leak-check cause %q, want none: its start was stopped, not refused", got)
	}
}
