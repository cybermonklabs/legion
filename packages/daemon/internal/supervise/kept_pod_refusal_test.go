package supervise

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The pod and the volume a start's refusal names where the store could not record that pod with
// the issue's kept volume: the pod's uid, and when the API server made the pod's Sandbox, which
// names the volume.
const keptPodUID = "5f0c2a7e-8d41-4b9a-a3e6-1c7d9b2f4e60"

var keptVolumeMade = time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)

// keptPodRefused is a start the sandbox runtime refused because the store could not record the
// role's pod with the issue's kept volume (runtime.KeptPodUnrecorded), wrapped as the runtime's
// relaunch wraps that step's error.
func keptPodRefused(h *harness) error {
	return fmt.Errorf("launch %s: record its pod with its kept volume: %w", h.token, runtime.KeptPodUnrecorded{
		PodUID: keptPodUID, KeptSince: keptVolumeMade,
		Err: errors.New("record the pod " + keptPodUID + " of LEGION-209 with its kept volume: dial tcp 10.0.0.7:5432: connect: connection refused"),
	})
}

// A start the runtime refuses because the store could not record the role's pod with the issue's
// kept volume is a launch failure like any other: no process started, the store that refused is
// the daemon's and not the session store a start waits for (runtime.ErrSessionStore), and the pod
// is not one whose update round the launch waits for (runtime.ErrAwaitingPodRound), so each
// refused try is charged one launch failure, and at the limit the claim fails as a launch refused.
// The refusal's sentence names the pod and the volume that are unrecorded. It is the claim's last
// start failure from the first refused try on, and the sentence of its failure once the launches
// run out, both of which the workflow's held and worker-died notices carry to the architect
// (internal/daemon's workflow.go, workflow's claimFailed). It is no leak-check cause.
func TestALaunchRefusedForAnUnrecordedKeptPodIsChargedAndNamesThePodAndItsVolume(t *testing.T) {
	limit := testLimits().LaunchFailures
	h := newHarness(t)
	refused := keptPodRefused(h)
	for range limit {
		h.rt.ScriptSpawn(fake.SpawnResult{Err: refused})
	}

	err := h.handle(RequestSpawn{Claim: h.token})
	var unrecorded runtime.KeptPodUnrecorded
	if !errors.As(err, &unrecorded) || unrecorded.PodUID != keptPodUID {
		t.Fatalf("the spawn returned %v, want the runtime's refusal of pod %s", err, keptPodUID)
	}
	// The sentence the claim carries below names the pod and the volume that are unrecorded.
	for _, says := range []string{keptPodUID, keptVolumeMade.Format(time.RFC3339), "could not be recorded for the issue's update round"} {
		if !strings.Contains(refused.Error(), says) {
			t.Errorf("the refusal reads %q, which does not say %q", refused, says)
		}
	}

	h.wantCalls("Spawn", limit)
	tries := launchingWrites(h)
	if len(tries) != limit {
		t.Fatalf("the claim was written launching %d times, want once per try (%d)", len(tries), limit)
	}
	for i, try := range tries {
		want := refused.Error()
		if i == 0 {
			want = ""
		}
		if try.Budgets.LaunchFailures != i || try.LastStartFailure != want {
			t.Errorf("try %d was written with %d launch failures and the start failure %q, want %d and %q",
				i+1, try.Budgets.LaunchFailures, try.LastStartFailure, i, want)
		}
	}

	h.wantState(StateFailed)
	h.wantBudgets(Budgets{LaunchFailures: limit})
	for _, got := range []Claim{h.claim(), h.store.load(h.token)} {
		if want := "launch failures ran out: " + refused.Error(); got.Failure != want {
			t.Errorf("the claim's failure is %q, want %q", got.Failure, want)
		}
		if got.FailureCause != CauseLaunchRefused || got.LastStartFailure != refused.Error() {
			t.Errorf("the claim carries cause %q and start failure %q, want %q and %q",
				got.FailureCause, got.LastStartFailure, CauseLaunchRefused, refused.Error())
		}
		if got.LeakCheckCause != "" || got.Locator != nil {
			t.Errorf("the claim carries the leak-check cause %q and the process %v, want neither: the runtime started nothing, and no leak check refused it",
				got.LeakCheckCause, got.Locator)
		}
	}
}
