package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The store refuses a write of an operator's suspension while a release takes the claim's pod
// (PodTakenError), and the start of a claim's launch there (PodTakenLaunchError), and refuses the
// same write again until the release ends. Such a refusal is no write the database did not take:
// owed, the repair would make it once the pod was gone, so no decision owes it, wrapped or not.
// The launch's refusal is a wait, as the lifecycle check's named wait is; the operator's is not. A
// deadline the write ran out, which the database never answered, is still owed, which shows the
// rule is read and not bypassed.
func TestAStoreRefusalOfATakenPodIsNotOwed(t *testing.T) {
	taken := &PodTakenError{Issue: "the-root-issue", Release: 7, Sandbox: "the-root-issue-pod"}
	for _, err := range []error{taken, fmt.Errorf("put claim the-architect: %w", taken)} {
		if writeOwed(err) {
			t.Fatalf("writeOwed(%v) = true, want the refusal not owed: the same write is refused until the release ends", err)
		}
		var refused *PodTakenError
		if !errors.As(err, &refused) || refused.Release != 7 {
			t.Fatalf("the refusal %v does not unwrap to the PodTakenError", err)
		}
		if errors.Is(err, wait.ErrWaiting) {
			t.Fatalf("the operator's refusal %v is a wait, want the operator answered", err)
		}
	}
	launching := &PodTakenLaunchError{Issue: "the-root-issue", Release: 7, Sandbox: "the-root-issue-pod"}
	for _, err := range []error{launching, fmt.Errorf("put claim the-architect: %w", launching)} {
		if writeOwed(err) {
			t.Fatalf("writeOwed(%v) = true, want the launch's refusal not owed: the same write is refused until the release ends", err)
		}
		if !errors.Is(err, wait.ErrWaiting) {
			t.Fatalf("the launch's refusal %v is no wait, want the caller to try the launch again", err)
		}
	}
	if !writeOwed(context.DeadlineExceeded) {
		t.Fatal("control: a write cut short by its deadline is not owed, so the rule was not read")
	}
}

// takeRefusingStore is the memory store whose claim write refuses the start of a launch, as the
// store does while a release takes the claim's issue pod (PodTakenLaunchError): the take came after
// the launch passed the lifecycle check, which this store's CheckLaunch passes, so the refusal is
// met at the launching write itself. A launch already under way is written.
type takeRefusingStore struct {
	*memStore
	refusing bool
}

func (s *takeRefusingStore) PutClaim(ctx context.Context, c Claim) error {
	if s.refusing && c.State == StateLaunching && s.load(c.Token).State != StateLaunching {
		return &PodTakenLaunchError{Issue: c.Issue, Release: 7, Sandbox: "the-issue-pod"}
	}
	return s.memStore.PutClaim(ctx, c)
}

// A launch the store refuses to start because a release is deleting the claim's issue pod is the
// lifecycle check's wait met one step later: the claim is left exactly as the launch found it, in
// memory and in the store, nothing is started and no retry of the machine's own is armed, and the
// wait goes back to whoever asked, whose retry launches the claim once the release ends. That
// holds where the launch is written (launchNow) and where the wake pacing parks it (waitOnMachine).
// The control is the same launch once the refusal is gone, which is written and started.
func TestALaunchTheStoreRefusesForATakenPodLeavesTheClaimAsItWas(t *testing.T) {
	t.Run("a spawn written at once", func(t *testing.T) {
		h := newHarness(t)
		store := &takeRefusingStore{memStore: h.store, refusing: true}
		h.m.deps.Store = store
		before, live := h.claim(), h.clock.Live()

		err := h.handle(RequestSpawn{Claim: h.token})
		var refused *PodTakenLaunchError
		if !errors.As(err, &refused) || !errors.Is(err, wait.ErrWaiting) {
			t.Fatalf("the spawn while a release takes the pod = %v, want the launch's wait", err)
		}
		if got := h.claim(); got.State != StateQueued || got.Generation != before.Generation || !bytes.Equal(got.BootTokenHash, before.BootTokenHash) {
			t.Fatalf("the claim after the refusal = %s at generation %d, want it left %s at %d", got.State, got.Generation, before.State, before.Generation)
		}
		if stored := h.store.load(h.token); stored.State != StateQueued {
			t.Fatalf("the stored claim is %s, want queued", stored.State)
		}
		h.wantCalls("Spawn", 0)
		h.wantCalls("Resume", 0)
		if armed := h.clock.Live() - live; armed != 0 {
			t.Fatalf("the refusal armed %d timers, want none: the retry is left to whoever asked", armed)
		}

		store.refusing = false
		h.must(RequestSpawn{Claim: h.token})
		h.wantState(StateLaunching)
		h.wantCalls("Spawn", 1)
	})

	t.Run("a launch the wake pacing parks", func(t *testing.T) {
		h := pacedHarness(t, wakeRate)
		h.launch()
		store := &takeRefusingStore{memStore: h.store, refusing: true}
		h.deps.Store = store
		second := h.machineFor(wakeClaim(secondToken, StateQueued))

		err := h.postTo(second, RequestSpawn{Claim: secondToken})
		var refused *PodTakenLaunchError
		if !errors.As(err, &refused) || !errors.Is(err, wait.ErrWaiting) {
			t.Fatalf("the parked spawn while a release takes the pod = %v, want the launch's wait", err)
		}
		if got := second.View(); got.Claim.State != StateQueued || got.WakeWaiting != nil {
			t.Fatalf("the parked claim after the refusal is %s waiting %+v, want it queued with no wake", got.Claim.State, got.WakeWaiting)
		}
		if stored := h.store.load(secondToken); stored.State != StateQueued {
			t.Fatalf("the stored claim is %s, want queued", stored.State)
		}

		store.refusing = false
		h.mustPostTo(second, RequestSpawn{Claim: secondToken})
		if got := second.View(); got.Claim.State != StateLaunching || got.WakeWaiting == nil {
			t.Fatalf("control: the parked spawn once the refusal is gone is %s waiting %+v, want it launching on a parked wake",
				got.Claim.State, got.WakeWaiting)
		}
	})
}
