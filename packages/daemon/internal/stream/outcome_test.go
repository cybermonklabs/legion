package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claudebridge"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The stream is the one reader of the agent's words for a refusal. Each spelling of the busy
// answer mints supervise.OutcomeAgentBusy — Oh My Pi's, whatever follows its first words, and the
// Claude Code bridge's own constant — and any other words mint supervise.OutcomePromptRefused.
func TestPromptRefusalOutcomeMintsAgentBusyForEachSpellingAndPromptRefusedForAnyOther(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		want         supervise.Outcome
	}{
		{"Oh My Pi's busy answer", "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion.",
			supervise.OutcomeAgentBusy},
		{"Oh My Pi's first words alone", "Agent is already processing", supervise.OutcomeAgentBusy},
		{"the Claude Code bridge's refusal", claudebridge.Busy, supervise.OutcomeAgentBusy},
		{"a provider that refused before any turn", "No API key found for provider anthropic", supervise.OutcomePromptRefused},
		{"another busy wording", "Agent is busy", supervise.OutcomePromptRefused},
		{"Oh My Pi's words not at the start", "the model said: Agent is already processing", supervise.OutcomePromptRefused},
		{"Oh My Pi's words in another case", "agent is already processing", supervise.OutcomePromptRefused},
		{"no words at all", "", supervise.OutcomePromptRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromptRefusalOutcome(tc.reason); got != tc.want {
				t.Fatalf("PromptRefusalOutcome(%q) = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

// Each frame that can refuse a prompt carries the outcome its words mint, read where the frame is
// decoded: the refusal of a send (RefusedError), the refusal after an acknowledgement and its
// replay on a later connection (LateRefusal), and a result that ran no turn (PromptResult). A
// refused adoption is supervise.OutcomeAdoptionRefused whatever the shim's words, and a refusal
// of any other command mints nothing.
func TestEachRefusalFrameCarriesTheOutcomeItsWordsMint(t *testing.T) {
	const busy = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."

	t.Run("a refused send", func(t *testing.T) {
		for reason, want := range map[string]supervise.Outcome{
			busy: supervise.OutcomeAgentBusy, claudebridge.Busy: supervise.OutcomeAgentBusy,
			"No API key found for provider anthropic": supervise.OutcomePromptRefused,
		} {
			h := startListener(t, harnessOptions{})
			p := h.connect(testToken)
			frame, result := h.prompt(p, "delivery-1", "verify #41")
			p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: reason})
			err := awaitResult(t, result)
			var refused *RefusedError
			if !errors.As(err, &refused) || refused.Outcome != want || refused.RefusalOutcome() != want {
				t.Fatalf("Prompt refused %q = %#v, want a refusal minted %q", reason, err, want)
			}
			if !errors.Is(err, runtime.ErrPromptRefused) {
				t.Fatalf("Prompt refused %q = %v, want runtime.ErrPromptRefused", reason, err)
			}
		}
	})

	t.Run("a refusal after the acknowledgement", func(t *testing.T) {
		h := startListener(t, harnessOptions{})
		p := h.connect(testToken)
		frame, result := h.prompt(p, "delivery-1", "verify #41")
		p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
		if err := awaitResult(t, result); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		conn := h.conn()
		p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: busy})
		want := LateRefusal{Claim: testClaim, DeliveryID: "delivery-1", Error: busy, Outcome: supervise.OutcomeAgentBusy,
			ConnSequence: conn.Sequence()}
		if got := h.next(); got != want {
			t.Fatalf("event = %#v, want %#v", got, want)
		}
	})

	t.Run("a refusal replayed on a later connection", func(t *testing.T) {
		h := startListener(t, harnessOptions{})
		p := h.connect(testToken)
		frame, result := h.prompt(p, "delivery-1", "verify #41")
		p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
		if err := awaitResult(t, result); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		p.conn.Close()
		if event := h.next(); event != (Closed{Claim: testClaim}) {
			t.Fatalf("event = %#v, want Closed", event)
		}
		replacement := h.connect(testToken)
		replaced := h.conn()
		replacement.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: claudebridge.Busy})
		want := LateRefusal{Claim: testClaim, DeliveryID: "delivery-1", Error: claudebridge.Busy,
			Outcome: supervise.OutcomeAgentBusy, Replayed: true, ConnSequence: replaced.Sequence()}
		if got := h.next(); got != want {
			t.Fatalf("event = %#v, want %#v", got, want)
		}
	})

	t.Run("a result that ran no turn", func(t *testing.T) {
		h := startListener(t, harnessOptions{})
		p := h.connect(testToken)
		frame, result := h.prompt(p, "delivery-1", "verify #41")
		p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
		if err := awaitResult(t, result); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		p.send(shimwire.PromptResult{ID: frame.ID, AgentInvoked: false, Error: &shimwire.PromptError{Message: busy}})
		p.send(shimwire.PromptResult{ID: frame.ID, AgentInvoked: false, Error: &shimwire.PromptError{Message: "the command ran locally"}})
		p.send(shimwire.PromptResult{ID: frame.ID, AgentInvoked: false})
		p.send(shimwire.PromptResult{ID: frame.ID, AgentInvoked: true})
		for _, want := range []Event{
			PromptResult{Claim: testClaim, DeliveryID: "delivery-1", Error: busy, Outcome: supervise.OutcomeAgentBusy},
			PromptResult{Claim: testClaim, DeliveryID: "delivery-1", Error: "the command ran locally", Outcome: supervise.OutcomeNoTurnRun},
			PromptResult{Claim: testClaim, DeliveryID: "delivery-1", Outcome: supervise.OutcomeNoTurnRun},
			PromptResult{Claim: testClaim, DeliveryID: "delivery-1", AgentInvoked: true},
		} {
			if got := h.next(); got != want {
				t.Fatalf("event = %#v, want %#v", got, want)
			}
		}
	})

	t.Run("a refused adoption and a refused token read", func(t *testing.T) {
		h := startListener(t, harnessOptions{})
		p := h.connect(testToken)
		conn := h.conn()
		identity := runtime.GitIdentity{Name: "legion-bot", Email: "bot@legion.dev"}
		result := async(func() error { return conn.AdoptWorkingCopy(context.Background(), identity, 3*time.Second) })
		request := p.expect(shimwire.TypeAdoptWorkingCopy).(shimwire.AdoptWorkingCopy)
		p.send(shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: false, Error: busy})
		var refused *RefusedError
		if err := awaitResult(t, result); !errors.As(err, &refused) || refused.Outcome != supervise.OutcomeAdoptionRefused {
			t.Fatalf("AdoptWorkingCopy = %#v, want a refusal minted %q whatever its words", err, supervise.OutcomeAdoptionRefused)
		}
		if other := conn.refusal(shimwire.TypeAgentSecretsToken, busy); other.RefusalOutcome() != supervise.OutcomeUnrecognised {
			t.Fatalf("a refused token read carries %q, want no outcome: no budget asks about it", other.RefusalOutcome())
		}
	})
}
