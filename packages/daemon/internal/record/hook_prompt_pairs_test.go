package record

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A task in another order than today's moves back by its own order, so a hook fires on a move
// back that is a forward pair of phase.Order: the tester of `implementer reviewer tester` sends
// the work back from testing to reviewing. The outbox takes that hook prompt, keeps it whole
// through a stored row, and its message lists the count of that move. A phase paired with itself
// is no move, and the outbox refuses it with its reason. The twin: a move back of today's line is
// taken.
func TestAHookPromptOfAMoveOfAnyOrderIsWritten(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	for _, prompt := range []HookPrompt{
		{Hook: "work-back", Role: claim.RoleTester, From: phase.Testing, To: phase.Reviewing, Count: 1,
			Counts: map[string]int{"testing-reviewing": 1}, Prompt: "Say what is in your way."},
		{Hook: "work-back", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: 1,
			Counts: map[string]int{"reviewing-implementing": 1}, Prompt: "Say what is in your way."},
	} {
		row, err := NewOutboxRow("LEGION-208", prompt, now)
		if err != nil {
			t.Fatalf("NewOutboxRow(%s to %s): %v", prompt.From, prompt.To, err)
		}
		decoded, err := DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("DecodeOutboxPayload(%s to %s): %v", prompt.From, prompt.To, err)
		}
		pair := phase.Pair(prompt.From, prompt.To)
		if message := decoded.(HookPrompt).Message("LEGION-208"); !strings.Contains(message, "Counts by move: "+pair+"=1.") {
			t.Fatalf("the message of a hook prompt of %s lists no count of it:\n%s", pair, message)
		}
	}
	_, err := NewOutboxRow("LEGION-208", HookPrompt{Hook: "work-back", Role: claim.RoleTester, From: phase.Testing, To: phase.Testing,
		Count: 1, Counts: map[string]int{"testing-testing": 1}, Prompt: "Say what is in your way."}, now)
	want := `a hook prompt names the move back that fired it; "testing" to "testing" is no move back`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("NewOutboxRow(testing to testing) error = %v, want %q", err, want)
	}
}
