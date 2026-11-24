package supervise

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A launch the daemon's store of saved workspace copies held (runtime.ErrWorkspaceCopyStore: the
// take or the drop of the issue's copies for the pod's restore did not answer) started nothing and
// says nothing about the claim: it is charged no launch failure and is tried again at the probe
// interval, as a launch whose session hold the store did not answer is
// (TestAnUnansweredHoldAtAResumeChargesNothing). Every issue pod's first start takes, so a charge
// here would fail every claim a store outage of three launches met. It records why the claim waits
// as its last start failure, naming the store that did not answer, and the line it logs names that
// store too. A launch held because a copy that was taken did not reach the pod's launcher, a slice
// lost, is the start's own failure: it is charged, and the next try follows at its backoff.
//
// The controls (the pull request records the runs): the sentinel's branch taken out of
// launchTries, which charges the first row a launch failure and fails its claim at a limit of one;
// the branch widened to every error that names the saved workspace copy, which charges the second
// row nothing; and the branch setting no LastStartFailure, as before, which fails the first row and
// TestAnUnansweredHoldAtAResumeChargesNothing.
func TestALaunchTheWorkspaceCopyStoreHeldIsChargedNothingAndOneWhoseCopyDidNotReachThePodIsCharged(t *testing.T) {
	t.Run("the store did not answer the take", func(t *testing.T) {
		h := newHarness(t)
		h.deps.Limits.LaunchFailures = 1
		held := fmt.Errorf("launcher %s start generation 1: issue INFRA-1, pod pod-1: the issue's saved workspace copies could not be taken for this pod's restore, so no role of the pod starts until a launch takes them: %w: %w",
			testToken, runtime.ErrWorkspaceCopyStore, errors.New("dial tcp: connection refused"))
		h.rt.ScriptSpawn(fake.SpawnResult{Err: held})

		if err := h.handle(RequestSpawn{Claim: h.token}); !errors.Is(err, runtime.ErrWorkspaceCopyStore) {
			t.Fatalf("the launch the copy store held answered %v, want runtime.ErrWorkspaceCopyStore", err)
		}
		c := h.claim()
		if c.State != StateLaunching || c.Budgets.LaunchFailures != 0 {
			t.Fatalf("the claim is %s with %d launch failures charged, want it launching with none", c.State, c.Budgets.LaunchFailures)
		}
		requireStartFailureNames(t, h, c, runtime.ErrWorkspaceCopyStore.Error())
		h.wantCalls("Spawn", 1)
		if lines := h.logs.lines(runtime.ErrWorkspaceCopyStore.Error() + "; trying the launch again at the probe interval, charging nothing"); len(lines) != 1 {
			t.Fatalf("logged %v, want the store's hold named once", lines)
		}

		h.advance(testProbe)
		h.wantCalls("Spawn", 2)
		if c := h.claim(); c.State != StateLaunching || c.Budgets.LaunchFailures != 0 || c.Locator == nil {
			t.Fatalf("after the retry at the probe interval the claim is %s with %d launch failures charged and process %v, want it launched with none charged",
				c.State, c.Budgets.LaunchFailures, c.Locator)
		}
	})

	t.Run("a slice of the taken copy did not reach the launcher", func(t *testing.T) {
		h := newHarness(t)
		lost := fmt.Errorf("launcher %s start generation 1: issue INFRA-1, pod pod-1: a slice of the saved workspace copy did not reach the launcher, so no role of the pod starts until a launch sends the copy again: %w",
			testToken, io.ErrClosedPipe)
		h.rt.ScriptSpawn(fake.SpawnResult{Err: lost})

		h.must(RequestSpawn{Claim: h.token})
		h.wantState(StateLaunching)
		h.wantBudgets(Budgets{LaunchFailures: 1})
		h.wantCalls("Spawn", 2)
	})
}

// requireStartFailureNames wants c, h's claim after a launch a store did not answer, and the claim
// the store holds to record that launch's failure as the claim's last start failure, naming store,
// the store that did not answer.
func requireStartFailureNames(t *testing.T, h *harness, c Claim, store string) {
	t.Helper()
	if !strings.Contains(c.LastStartFailure, store) {
		t.Fatalf("the claim records its last start failure as %q, want it to name %q", c.LastStartFailure, store)
	}
	if stored := h.store.load(h.token); stored.LastStartFailure != c.LastStartFailure {
		t.Fatalf("the stored claim records its last start failure as %q, want it persisted as %q", stored.LastStartFailure, c.LastStartFailure)
	}
}
