package claudebridge

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// The bridge's own refusal of a prompt during a turn, as it writes it to the shim, decodes to a
// response carrying the busy code, and the daemon's stream mints that as the agent being in a turn
// of its own: the task goes back unread and uncharged. The stream reads the code and not the
// words, so this holds whatever Busy says. A refusal of a command the bridge does not support
// carries no code, and is no busy answer.
func TestTheBridgesBusyRefusalOutcomeIsAgentBusy(t *testing.T) {
	decode := func(t *testing.T, frame map[string]any) shimwire.Response {
		t.Helper()
		line, err := json.Marshal(frame)
		if err != nil {
			t.Fatalf("encode %v: %v", frame, err)
		}
		decoded, err := shimwire.Decode(line)
		if err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		response, ok := decoded.(shimwire.Response)
		if !ok {
			t.Fatalf("decoded %s as a %T, want a response", line, decoded)
		}
		return response
	}

	r := newRig(t, "-p", "--resume", sessionID)
	r.prompt("p1", backgroundDuringPrompt)
	r.expect("agent_start", nil)
	r.prompt("p2", "say again")
	busy := decode(t, r.expect("response", map[string]any{"id": "p2", "command": "prompt", "success": false}))
	if busy.Code != shimwire.CodeSessionBusy {
		t.Fatalf("the bridge's busy refusal %+v carries code %q, want %q", busy, busy.Code, shimwire.CodeSessionBusy)
	}
	if got := stream.PromptRefusalOutcome(busy.Code); got != runtime.OutcomeAgentBusy {
		t.Fatalf("the bridge's busy refusal %+v mints %q, want %q", busy, got, runtime.OutcomeAgentBusy)
	}
	r.open()
	r.expect("agent_end", nil)
	r.expect("prompt_result", map[string]any{"id": "p1"})

	r.send(shimwire.Raw{Type: "steer", JSON: []byte(`{"type":"steer","id":"s1","message":"x"}`)})
	unsupported := decode(t, r.expect("response", map[string]any{"id": "s1", "command": "steer", "success": false}))
	if got := stream.PromptRefusalOutcome(unsupported.Code); got == runtime.OutcomeAgentBusy {
		t.Fatalf("the bridge's refusal %+v mints %q, want no busy answer", unsupported, got)
	}
}
