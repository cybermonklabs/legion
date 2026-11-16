package supervise

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// architectPromptRefusal is the refusal a runtime answers a launch of the tree's root architect
// with when its composed system prompt, 200,000 bytes, is larger than the 150,000 bytes the
// deployment gives its model's context: runtime.SystemPrompt's own, over a real prompt file,
// wrapped as the sandbox runtime wraps every refusal of its own.
func architectPromptRefusal(t *testing.T) error {
	t.Helper()
	role := filepath.Join(t.TempDir(), "architect.md")
	if err := os.WriteFile(role, []byte(strings.Repeat("x", 200_000)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, refusal := runtime.SystemPrompt(runtime.SpawnSpec{
		Claim: rootToken, Role: rootClaim().Role, ModelContextBytes: 150_000,
		Prompt: runtime.PromptParts{RolePromptPaths: []string{role}},
	})
	if refusal == nil {
		t.Fatal("runtime.SystemPrompt composed a 200,000-byte prompt under a 150,000-byte bound")
	}
	return fmt.Errorf("sandbox launch %s: %w", rootToken, refusal)
}

// A start the runtime refuses because the role's system prompt is larger than its model's context
// is no launch failure: it is tried once, charged nothing, and fails the claim at once rather than
// after the launch budget's three tries, with the refusal's sentence — the role, the prompt's size
// and the bound — as the claim's failure and its last start failure, the two fields read_state
// serves (api.ClaimViewOf's `failure.detail` and `lastStartFailure`), in the store as well as in
// memory.
func TestAPromptTooLargeForItsModelFailsTheClaimChargingNoLaunchFailure(t *testing.T) {
	h := newHarnessOf(t, rootClaim())
	refusal := architectPromptRefusal(t)
	h.rt.ScriptSpawn(fake.SpawnResult{Err: refusal})
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
	const sentence = "the architect's system prompt is 200000 bytes, larger than the 150000 bytes model_context_bytes.architect gives its model's context"
	for name, got := range map[string]Claim{"the machine's claim": h.claim(), "the stored claim": h.store.load(rootToken)} {
		if !strings.Contains(got.Failure, sentence) || !strings.Contains(got.LastStartFailure, sentence) {
			t.Errorf("%s fails with %q after the last start failure %q, want both to name %q", name, got.Failure, got.LastStartFailure, sentence)
		}
		if got.State != StateFailed || got.Budgets != (Budgets{}) {
			t.Errorf("%s is %s with budgets %+v, want failed with nothing charged", name, got.State, got.Budgets)
		}
	}
}
