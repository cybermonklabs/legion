package record

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A task in another order than today's moves back by its own order, so a hook fires on a move
// back that is a forward pair of phase.Order: the tester of `implementer reviewer tester` sends
// the work back from testing to reviewing. The outbox takes that hook prompt, its row decodes to
// the same prompt, every field whole, and its message lists the count of that move. The twin: a
// move back of today's line is taken. outbox_test's refusals hold the prompt of a phase to itself.
func TestAHookPromptOfAMoveOfAnyOrderIsWritten(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	for _, prompt := range []HookPrompt{
		{Hook: "work-back", Role: claim.RoleTester, From: phase.Testing, To: phase.Reviewing, Count: 1,
			Counts: map[string]int{"testing-reviewing": 1}, PullRequest: "sjawhar/legion#42", Prompt: "Say what is in your way."},
		{Hook: "work-back", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: 1,
			Counts: map[string]int{"reviewing-implementing": 1}, Prompt: "Say what is in your way."},
	} {
		// A writer of this release names the run and the phase entry it queues the row for (Fenced).
		want := Fenced(prompt, Fence{Generation: 1, Entry: 1})
		row, err := NewOutboxRow("LEGION-208", want, now)
		if err != nil {
			t.Fatalf("NewOutboxRow(%s to %s): %v", prompt.From, prompt.To, err)
		}
		decoded, err := DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("DecodeOutboxPayload(%s to %s): %v", prompt.From, prompt.To, err)
		}
		if !reflect.DeepEqual(decoded, want) {
			t.Fatalf("the row of a hook prompt of %s to %s decodes to %#v, want %#v", prompt.From, prompt.To, decoded, want)
		}
		pair := phase.Pair(prompt.From, prompt.To)
		if message := decoded.(HookPrompt).Message("LEGION-208"); !strings.Contains(message, "Counts by move: "+pair+"=1.") {
			t.Fatalf("the message of a hook prompt of %s lists no count of it:\n%s", pair, message)
		}
	}
}
