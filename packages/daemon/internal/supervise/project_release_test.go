package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The project release's suspension ranks above the idle release's and below the daemon's
// (suspensionRank). So the release takes over a claim an idle release is stopping, and the next
// boot starts it rather than leaving it for a wake; a linger, park or close that lands during the
// release takes over a claim it stopped, and an operator's suspend takes it over too, which is how
// the release holds the claims of a pod it leaves. It takes over nothing stronger: not the
// daemon's, the operator's, a root's exit or one recorded before migration 0038.
func TestTheProjectReleasesSuspensionRanksBetweenTheIdleReleaseAndTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first SuspendSource
		then  SuspendSource
		want  SuspendSource
	}{
		{name: "it takes over a claim the idle release suspended", first: SuspendedByReleased, then: SuspendedByProjectRelease, want: SuspendedByProjectRelease},
		{name: "the idle release does not take a claim it stopped back", first: SuspendedByProjectRelease, then: SuspendedByReleased, want: SuspendedByProjectRelease},
		{name: "the daemon's suspend takes over a claim it stopped", first: SuspendedByProjectRelease, then: SuspendedByDaemon, want: SuspendedByDaemon},
		{name: "the operator's suspend takes over a claim it stopped", first: SuspendedByProjectRelease, then: SuspendedByOperator, want: SuspendedByOperator},
		{name: "it does not take over the daemon's", first: SuspendedByDaemon, then: SuspendedByProjectRelease, want: SuspendedByDaemon},
		{name: "it does not take over the operator's", first: SuspendedByOperator, then: SuspendedByProjectRelease, want: SuspendedByOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			suspendedBy(h, tc.first)
			h.must(RequestSuspend{Claim: testToken, Reason: "the test suspends it again", By: tc.then})
			if got := h.store.load(testToken).SuspendedBy; got != tc.want {
				t.Fatalf("a %s suspend of a claim suspended by %s stored %q, want %q", tc.then, tc.first, got, tc.want)
			}
			h.wantState(StateSuspended)
			h.wantCalls("Suspend", 1)
		})
	}

	t.Run("it does not take over a suspension recorded before migration 0038", func(t *testing.T) {
		h := newHarness(t)
		suspendedBy(h, SuspendedByDaemon)
		c := h.claim()
		c.SuspendedBy = SuspendedByUnknown
		if err := h.store.PutClaim(h.ctx, c); err != nil {
			t.Fatal(err)
		}
		h.restart()
		h.must(RequestSuspend{Claim: testToken, Reason: "the project release stops it", By: SuspendedByProjectRelease})
		if got := h.store.load(testToken).SuspendedBy; got != SuspendedByUnknown {
			t.Fatalf("the project release's suspend of an unknown suspension stored %q, want unknown", got)
		}
	})

	t.Run("it does not take over a root's own exit", func(t *testing.T) {
		h := newHarnessOf(t, rootClaim())
		h.reach(StateIdle)
		h.exit()
		h.wantState(StateSuspended)
		h.must(RequestSuspend{Claim: rootToken, Reason: "the project release stops it", By: SuspendedByProjectRelease})
		if got := h.store.load(rootToken).SuspendedBy; got != SuspendedByExit {
			t.Fatalf("the project release's suspend of a root that exited stored %q, want exit", got)
		}
	})
}

// A phase task queued on a claim survives the project release's suspension, as it survives an
// operator's: the release ends no phase, and the next boot's start resumes the same claim by rule,
// which sends that task again. A turn the release cut at its bound goes back to waiting that way,
// and the boot's start carries no task of its own.
func TestAProjectReleaseSuspensionKeepsThePendingPhaseTaskForTheBootsStart(t *testing.T) {
	h := newHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "Continue testing.", ID: "outbox:7", Phase: phase.Testing, Generation: 1})
	h.must(RequestSuspend{Claim: testToken, Reason: "the project release stops it", By: SuspendedByProjectRelease})
	h.wantState(StateSuspended)
	if pending := h.claim().Pending; pending == nil || pending.Task != "Continue testing." || pending.Phase != phase.Testing {
		t.Fatalf("after the project release's suspension pending %+v, want the phase task kept", pending)
	}
	h.restart()
	h.must(RequestRuleResume{Claim: testToken, Reason: "the boot starts a claim the project release stopped"})
	h.reach(StateReady)
	if pending := h.claim().Pending; pending == nil || pending.Task != "Continue testing." {
		t.Fatalf("after the boot's resume pending %+v, want the original phase task", pending)
	}
	if prompts := h.wantPrompts(2); prompts[1].Message != prompts[0].Message {
		t.Fatalf("the boot's resume sent %q, want the task the cut turn was given, %q", prompts[1].Message, prompts[0].Message)
	}
}
