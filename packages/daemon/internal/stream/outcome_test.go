package stream

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ompBusySentence is how Oh My Pi words its refusal of a prompt that reaches it in a turn already
// running. The stream never reads it: a release that gives the refusal no code sends it alone, and
// these tests hold that it is then no busy answer.
const ompBusySentence = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."

// A prompt refusal's outcome is read from its code alone: the busy code mints
// runtime.OutcomeAgentBusy, any other code runtime.OutcomePromptRefused, and no code
// runtime.OutcomeUnrecognised.
func TestPromptRefusalOutcomeReadsTheCodeAlone(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		want       runtime.Outcome
	}{
		{"the busy code", shimwire.CodeSessionBusy, runtime.OutcomeAgentBusy},
		{"another code", "cost_cap", runtime.OutcomePromptRefused},
		{"the busy code in another case", "SESSION_BUSY", runtime.OutcomePromptRefused},
		{"no code", "", runtime.OutcomeUnrecognised},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromptRefusalOutcome(tc.code); got != tc.want {
				t.Fatalf("PromptRefusalOutcome(%q) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

// Each frame that can refuse a prompt carries the outcome its code mints, read where the frame is
// decoded: the refusal of a send (RefusedError), the refusal after an acknowledgement and its
// replay on a later connection (LateRefusal), and a result that ran no turn (PromptResult). The
// words beside the code change nothing. A refused adoption is runtime.OutcomeAdoptionRefused
// whatever the shim says, and a refusal of any other command mints nothing.
func TestEachRefusalFrameCarriesTheOutcomeItsCodeMints(t *testing.T) {
	t.Run("a refused send", func(t *testing.T) {
		for _, tc := range []struct {
			name, reason, code string
			want               runtime.Outcome
		}{
			{"Oh My Pi's busy refusal with its code", ompBusySentence, shimwire.CodeSessionBusy, runtime.OutcomeAgentBusy},
			{"the busy code under other words", "the session is busy", shimwire.CodeSessionBusy, runtime.OutcomeAgentBusy},
			{"Oh My Pi's busy sentence with no code", ompBusySentence, "", runtime.OutcomeUnrecognised},
			{"a provider's refusal with no code", "No API key found for provider anthropic", "", runtime.OutcomeUnrecognised},
			{"a refusal with another code", "the cost cap is reached", "cost_cap", runtime.OutcomePromptRefused},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := startListener(t, harnessOptions{})
				p := h.connect(testToken)
				frame, result := h.prompt(p, "delivery-1", "verify #41")
				p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: tc.reason, Code: tc.code})
				err := awaitResult(t, result)
				var refused *RefusedError
				if !errors.As(err, &refused) || refused.Outcome != tc.want || runtime.RefusalOutcome(err) != tc.want {
					t.Fatalf("Prompt refused %q with code %q = %#v, want a refusal minted %q", tc.reason, tc.code, err, tc.want)
				}
				if !errors.Is(err, runtime.ErrPromptRefused) {
					t.Fatalf("Prompt refused %q = %v, want runtime.ErrPromptRefused", tc.reason, err)
				}
			})
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
		p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: ompBusySentence,
			Code: shimwire.CodeSessionBusy})
		want := LateRefusal{Claim: testClaim, DeliveryID: "delivery-1", Error: ompBusySentence, Outcome: runtime.OutcomeAgentBusy,
			ConnSequence: conn.Sequence()}
		if got := h.next(); got != want {
			t.Fatalf("event = %#v, want %#v", got, want)
		}
	})

	t.Run("Oh My Pi's busy sentence after the acknowledgement, with no code", func(t *testing.T) {
		h := startListener(t, harnessOptions{})
		p := h.connect(testToken)
		frame, result := h.prompt(p, "delivery-1", "verify #41")
		p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
		if err := awaitResult(t, result); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		conn := h.conn()
		p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: ompBusySentence})
		want := LateRefusal{Claim: testClaim, DeliveryID: "delivery-1", Error: ompBusySentence, Outcome: runtime.OutcomeUnrecognised,
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
		replacement.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: "busy",
			Code: shimwire.CodeSessionBusy})
		want := LateRefusal{Claim: testClaim, DeliveryID: "delivery-1", Error: "busy",
			Outcome: runtime.OutcomeAgentBusy, Replayed: true, ConnSequence: replaced.Sequence()}
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
		p.send(shimwire.PromptResult{ID: frame.ID, AgentInvoked: false,
			Error: &shimwire.PromptError{Message: ompBusySentence, Code: shimwire.CodeSessionBusy}})
		p.send(shimwire.PromptResult{ID: frame.ID, AgentInvoked: false, Error: &shimwire.PromptError{Message: ompBusySentence}})
		p.send(shimwire.PromptResult{ID: frame.ID, AgentInvoked: false, Error: &shimwire.PromptError{Message: "the command ran locally"}})
		p.send(shimwire.PromptResult{ID: frame.ID, AgentInvoked: false})
		p.send(shimwire.PromptResult{ID: frame.ID, AgentInvoked: true})
		for _, want := range []Event{
			PromptResult{Claim: testClaim, DeliveryID: "delivery-1", Error: ompBusySentence, Outcome: runtime.OutcomeAgentBusy},
			PromptResult{Claim: testClaim, DeliveryID: "delivery-1", Error: ompBusySentence, Outcome: runtime.OutcomeNoTurnRun},
			PromptResult{Claim: testClaim, DeliveryID: "delivery-1", Error: "the command ran locally", Outcome: runtime.OutcomeNoTurnRun},
			PromptResult{Claim: testClaim, DeliveryID: "delivery-1", Outcome: runtime.OutcomeNoTurnRun},
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
		p.send(shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: false, Error: ompBusySentence})
		var refused *RefusedError
		if err := awaitResult(t, result); !errors.As(err, &refused) || refused.Outcome != runtime.OutcomeAdoptionRefused {
			t.Fatalf("AdoptWorkingCopy = %#v, want a refusal minted %q whatever its words", err, runtime.OutcomeAdoptionRefused)
		}
		other := conn.refusal(shimwire.TypeAgentSecretsToken, ompBusySentence, shimwire.CodeSessionBusy)
		if other.RefusalOutcome() != runtime.OutcomeUnrecognised {
			t.Fatalf("a refused token read carries %q, want no outcome: no budget asks about it", other.RefusalOutcome())
		}
	})
}

// From the frame to the policy: a prompt refusal carrying the busy code is the agent in a turn of
// its own and charges nothing, and Oh My Pi's busy sentence with no code is the unrecognised
// outcome and is charged — its words are not read — with a line at warn that says so, quoting the
// words and naming what the connection knows of the agent.
func TestABusyCodeOutcomeChargesNothingAndTheCodelessBusySentenceIsChargedAndLogged(t *testing.T) {
	refuse := func(t *testing.T, code string) (*harness, runtime.Outcome) {
		t.Helper()
		h := startListener(t, harnessOptions{})
		p := h.connect(testToken)
		frame, result := h.prompt(p, "delivery-1", "verify #41")
		p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: ompBusySentence, Code: code})
		return h, runtime.RefusalOutcome(awaitResult(t, result))
	}
	codeless := func(lines []string) []int {
		var at []int
		for i, line := range lines {
			if strings.HasPrefix(line, "worker-stream: the agent refused a prompt with no machine-readable code") {
				at = append(at, i)
			}
		}
		return at
	}

	t.Run("the busy code", func(t *testing.T) {
		h, outcome := refuse(t, shimwire.CodeSessionBusy)
		if outcome != runtime.OutcomeAgentBusy || supervise.Charges(outcome) {
			t.Fatalf("a refusal with the busy code mints %q, charged %t; want %q, charged nothing", outcome,
				supervise.Charges(outcome), runtime.OutcomeAgentBusy)
		}
		if lines := h.logs.Lines(); len(codeless(lines)) != 0 {
			t.Fatalf("logs = %q, want no line about a missing code", lines)
		}
	})

	t.Run("Oh My Pi's busy sentence with no code", func(t *testing.T) {
		h, outcome := refuse(t, "")
		if outcome != runtime.OutcomeUnrecognised || !supervise.Charges(outcome) {
			t.Fatalf("Oh My Pi's busy sentence with no code mints %q, charged %t; want %q, charged", outcome,
				supervise.Charges(outcome), runtime.OutcomeUnrecognised)
		}
		lines, levels := h.logs.Lines(), h.logs.Levels()
		at := codeless(lines)
		if len(at) != 1 {
			t.Fatalf("logs = %q, want one line that the refusal carried no code", lines)
		}
		line := lines[at[0]]
		for _, want := range []string{"session_busy", "dispatch://INFRA-3538", "error=" + ompBusySentence,
			"agent=RPC protocol v2, release not named by the hello", "claim=" + string(testClaim), "unloggedSinceLast=0"} {
			if !strings.Contains(line, want) {
				t.Errorf("the line %q does not carry %q", line, want)
			}
		}
		if levels[at[0]] != slog.LevelWarn {
			t.Errorf("the line is logged at %s, want %s", levels[at[0]], slog.LevelWarn)
		}
	})
}

// The line a prompt refusal with no code writes is rate-limited per connection: the first is
// written at once, the next within codelessEvery are counted and not written, the first after it
// says how many were not, and a new connection writes its first at once.
func TestTheCodelessRefusalOutcomeLineIsRateLimitedPerConnection(t *testing.T) {
	const line = "worker-stream: the agent refused a prompt with no machine-readable code"
	count := func(h *harness) (n int, last string) {
		for _, l := range h.logs.Lines() {
			if strings.HasPrefix(l, line) {
				n, last = n+1, l
			}
		}
		return n, last
	}
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	for range 3 {
		conn.refusal(shimwire.TypePrompt, ompBusySentence, "")
	}
	if n, _ := count(h); n != 1 {
		t.Fatalf("three refusals with no code within %s wrote %d lines, want 1: %q", codelessEvery, n, h.logs.Lines())
	}
	conn.mu.Lock()
	conn.codelessAt = conn.codelessAt.Add(-codelessEvery)
	conn.mu.Unlock()
	conn.refusal(shimwire.TypePrompt, "another sentence", "")
	if n, last := count(h); n != 2 || !strings.Contains(last, "unloggedSinceLast=2") || !strings.Contains(last, "error=another sentence") {
		t.Fatalf("after %s the next refusal with no code wrote %d lines, the last %q; want 2, the last counting the 2 not written", codelessEvery, n, last)
	}

	p.conn.Close()
	if event := h.next(); event != (Closed{Claim: testClaim}) {
		t.Fatalf("event = %#v, want Closed", event)
	}
	h.connect(testToken)
	h.conn().refusal(shimwire.TypePrompt, ompBusySentence, "")
	if n, last := count(h); n != 3 || !strings.Contains(last, "unloggedSinceLast=0") {
		t.Fatalf("a new connection's first refusal with no code wrote %d lines in all, the last %q; want its own at once", n, last)
	}
}
