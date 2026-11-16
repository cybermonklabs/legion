package supervise

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// architectFrameRefusal is the refusal the sandbox runtime answers a launch of the tree's root
// architect with when the start frame carrying its 1,100,000-byte system prompt to the pod's
// launcher would be 1,100,400 bytes, past the 1,048,576 bytes a launcher reads in one frame
// (shimwire.MaxFrameBytes), wrapped as that runtime wraps every refusal of its own.
func architectFrameRefusal() error {
	return fmt.Errorf("sandbox launch %s: %w", rootToken,
		&runtime.PromptTooLarge{Role: rootClaim().Role, Bytes: 1_100_000, Frame: 1_100_400})
}

// A start the runtime refuses because the role's system prompt would not fit the one frame a pod's
// launcher reads is no launch failure: it is tried once, charged nothing, and fails the claim at
// once rather than after the launch budget's three tries, with the refusal's sentence — the role,
// the prompt's size and the limit — as the claim's failure and its last start failure, the two
// fields read_state serves (api.ClaimViewOf's `failure.detail` and `lastStartFailure`), in the
// store as well as in memory.
func TestAPromptPastTheStartFrameFailsTheClaimChargingNoLaunchFailure(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	h.rt.ScriptSpawn(fake.SpawnResult{Err: architectFrameRefusal()})
	since := h.clock.Now()

	err := h.handle(RequestSpawn{Claim: rootToken})

	var tooLarge *runtime.PromptTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("spawn returned %v, want the runtime's PromptTooLarge", err)
	}
	h.wantState(StateFailed)
	h.wantCalls("Spawn", 1)
	h.wantBudgets(Budgets{})
	h.wantFailure(CauseLaunchRefused, "charged no launch failure", since)
	const sentence = "the architect's system prompt is 1100000 bytes, and the start frame that carries it to its pod's launcher would be 1100400 bytes, larger than the 1048576 bytes a launcher reads in one frame"
	for name, got := range map[string]Claim{"the machine's claim": h.claim(), "the stored claim": h.store.load(rootToken)} {
		if !strings.Contains(got.Failure, sentence) || !strings.Contains(got.LastStartFailure, sentence) {
			t.Errorf("%s fails with %q after the last start failure %q, want both to name %q", name, got.Failure, got.LastStartFailure, sentence)
		}
		if got.State != StateFailed || got.Budgets != (Budgets{}) {
			t.Errorf("%s is %s with budgets %+v, want failed with nothing charged", name, got.State, got.Budgets)
		}
	}
}
