package claudebridge_test

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claudebridge"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The bridge's own refusal of a prompt during a turn is exactly Busy (TestAPromptDuringATurnIsRefusedBusy,
// TestAPromptDuringAClaudeStartedTurnIsRefusedBusy), and the daemon's stream, the one reader of
// those words, mints it as the agent being in a turn of its own: the task goes back unread and
// uncharged. Its other refusals are no busy answer. This test lives outside the package because
// the stream imports it for the constant, and an in-package test importing the stream would be a
// cycle.
func TestTheBridgesBusyRefusalMintsOutcomeAgentBusy(t *testing.T) {
	if got := stream.PromptRefusalOutcome(claudebridge.Busy); got != supervise.OutcomeAgentBusy {
		t.Fatalf("the bridge's busy refusal %q mints %q, want %q", claudebridge.Busy, got, supervise.OutcomeAgentBusy)
	}
	unsupported := "the Claude Code bridge does not support compact"
	if got := stream.PromptRefusalOutcome(unsupported); got != supervise.OutcomePromptRefused {
		t.Fatalf("the bridge's refusal %q mints %q, want %q", unsupported, got, supervise.OutcomePromptRefused)
	}
}
