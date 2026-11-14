package stream

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// agentEndFrame is the frame that arrives chunked: the only worker frame whose size grows with
// the whole transcript (worker-rpc.ts:65-68).
func agentEndFrame(t *testing.T, size int) []byte {
	t.Helper()
	head := `{"type":"agent_end","messages":[{"role":"user","content":"`
	tail := `"}]}`
	return []byte(head + strings.Repeat("x", size-len(head)-len(tail)) + tail)
}

func (p *peer) sendChunked(chunkID string, frame []byte) {
	p.t.Helper()
	chunks, err := shimwire.SplitChunks(chunkID, frame)
	if err != nil {
		p.t.Fatalf("SplitChunks: %v", err)
	}
	for _, chunk := range chunks {
		p.send(chunk)
	}
}

// getState runs GetState against the peer through the negotiation it sends first, and returns
// the get_state frame the peer received and the channel GetState's error lands on.
func (h *harness) getState(ctx context.Context, p *peer) (shimwire.GetState, <-chan error) {
	h.t.Helper()
	conn := h.conn()
	result := async(func() error { _, err := conn.GetState(ctx); return err })
	p.negotiate()
	return p.expect(shimwire.TypeGetState).(shimwire.GetState), result
}

// answerState is OMP answering a get_state: not streaming.
func answerState(t *testing.T, request shimwire.GetState) shimwire.Response {
	return shimwire.Response{ID: request.ID, Command: shimwire.TypeGetState, Success: true,
		Data: mustJSON(t, map[string]any{"isStreaming": false})}
}

// The connection closing before OMP answers is a transport error, never an answer and never a
// refusal.
func TestARequestReturnsATransportErrorWhenTheConnectionClosesFirst(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	_, result := h.getState(context.Background(), p)
	p.conn.Close()
	err := awaitResult(t, result)
	var refused *RefusedError
	if !isClosed(err) || errors.As(err, &refused) {
		t.Fatalf("GetState = %v, want ErrClosed", err)
	}
	// And after the close, at once, with nothing written.
	if event := h.next(); event != (Closed{Claim: testClaim}) {
		t.Fatalf("event = %#v, want Closed", event)
	}
	if _, err := conn.GetState(context.Background()); !isClosed(err) {
		t.Fatalf("GetState on a closed connection = %v, want ErrClosed", err)
	}
}

func TestARequestHonoursItsContextAndTheRPCTimeout(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	ctx, cancel := context.WithCancel(context.Background())
	_, result := h.getState(ctx, p)
	cancel()
	if err := awaitResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetState after its context was cancelled = %v, want context.Canceled", err)
	}

	short := startListener(t, harnessOptions{rpcTimeout: 50 * time.Millisecond})
	q := dial(t, short.listener.Addr())
	q.hello(testToken)
	q.expect(shimwire.TypeHelloAck)
	short.next()
	quiet := short.conn()
	err := awaitResult(t, async(func() error { return quiet.Negotiate(context.Background()) }))
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("a request with no answer = %v, want ErrNoAnswer", err)
	}
}

// The supervisor charges an adoption the shim refused, and waits at no charge on what the
// transport lost, telling them apart by runtime.ErrAdoptRefused alone (INFRA-1480). So a refusal
// is the sentinel of the command it refused and of no other: the shim's `ok:false` for the
// adoption — and the refusal an older shim's forwarding of that frame earns from OMP, which does
// not know the command — is ErrAdoptRefused, and a refused negotiation, a refused get_state, a
// close, the RPC timeout and the caller's own deadline are not.
func TestEachRefusalIsOnlyTheSentinelOfTheCommandItRefused(t *testing.T) {
	// sentinels is every runtime error the supervisor splits a failed request on: a row is the one
	// it names and none of the rest, so a match widened to every refusal fails here.
	sentinels := []struct {
		name string
		err  error
	}{
		{"runtime.ErrAdoptRefused", runtime.ErrAdoptRefused},
		{"runtime.ErrTokenRefused", runtime.ErrTokenRefused},
	}
	identity := runtime.GitIdentity{Name: "legion-bot", Email: "bot@legion.dev"}
	const staleLock = "Command failed (exit 1): jj metaedit: git lock /w/.git/index.lock was last written 31m ago"
	var refusal *RefusedError
	for _, test := range []struct {
		name       string
		rpcTimeout time.Duration
		// drive answers (or does not) whatever the connection sent, and returns the error the
		// call under test returned.
		drive func(h *harness, p *peer) error
		// isCase is the failure the case exists to produce, so no row passes by producing
		// another.
		isCase func(error) bool
		// sentinel is the one error of sentinels this failure is, nil for a failure that is none
		// of them, and reason the words the far side refused with, carried into the
		// supervisor's log.
		sentinel error
		reason   string
	}{
		{name: "the shim refuses the adoption", sentinel: runtime.ErrAdoptRefused, reason: staleLock,
			drive: func(h *harness, p *peer) error {
				conn := h.conn()
				result := async(func() error {
					return conn.AdoptWorkingCopy(context.Background(), identity, time.Second)
				})
				request := p.expect(shimwire.TypeAdoptWorkingCopy).(shimwire.AdoptWorkingCopy)
				p.send(shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: false, Error: staleLock})
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool {
				return errors.As(err, &refusal) && refusal.Command == shimwire.TypeAdoptWorkingCopy
			},
		},
		{name: "an older shim forwards the adoption and OMP refuses the command it does not know",
			sentinel: runtime.ErrAdoptRefused, reason: "Unknown command: adopt-working-copy",
			drive: func(h *harness, p *peer) error {
				conn := h.conn()
				result := async(func() error {
					return conn.AdoptWorkingCopy(context.Background(), identity, time.Second)
				})
				request := p.expect(shimwire.TypeAdoptWorkingCopy).(shimwire.AdoptWorkingCopy)
				p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeAdoptWorkingCopy,
					Success: false, Error: "Unknown command: adopt-working-copy"})
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool {
				return errors.As(err, &refusal) && refusal.Command == shimwire.TypeAdoptWorkingCopy
			},
		},
		{name: "the connection closes before the answer",
			drive: func(h *harness, p *peer) error {
				_, result := h.getState(context.Background(), p)
				p.conn.Close()
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.Is(err, ErrClosed) },
		},
		{name: "the connection closes under the adoption",
			drive: func(h *harness, p *peer) error {
				conn := h.conn()
				result := async(func() error {
					return conn.AdoptWorkingCopy(context.Background(), identity, time.Second)
				})
				p.expect(shimwire.TypeAdoptWorkingCopy)
				p.conn.Close()
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.Is(err, ErrClosed) },
		},
		{name: "no answer within the RPC timeout", rpcTimeout: 300 * time.Millisecond,
			drive: func(h *harness, p *peer) error {
				_, result := h.getState(context.Background(), p)
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.Is(err, ErrNoAnswer) },
		},
		{name: "the caller's deadline",
			drive: func(h *harness, p *peer) error {
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				defer cancel()
				_, result := h.getState(ctx, p)
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.Is(err, context.DeadlineExceeded) },
		},
		{name: "OMP refuses the negotiation ahead of the request",
			drive: func(h *harness, p *peer) error {
				conn := h.conn()
				result := async(func() error { _, err := conn.GetState(context.Background()); return err })
				request := p.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
				p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeNegotiateProtocol, Success: false, Error: "unsupported"})
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool {
				return errors.As(err, &refusal) && refusal.Command == shimwire.TypeNegotiateProtocol
			},
		},
		{name: "OMP refuses a get_state",
			drive: func(h *harness, p *peer) error {
				conn := h.conn()
				result := async(func() error { _, err := conn.GetState(context.Background()); return err })
				p.negotiate()
				request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
				p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeGetState, Success: false, Error: "busy"})
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.As(err, &refusal) && refusal.Command == shimwire.TypeGetState },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := startListener(t, harnessOptions{rpcTimeout: test.rpcTimeout})
			err := test.drive(h, h.connect(testToken))
			if !test.isCase(err) {
				t.Fatalf("the call returned %v, not the failure this case is about", err)
			}
			for _, sentinel := range sentinels {
				want := sentinel.err == test.sentinel
				if got := errors.Is(err, sentinel.err); got != want {
					t.Fatalf("errors.Is(%v, %s) = %v, want %v", err, sentinel.name, got, want)
				}
			}
			// The far side's own words reach the supervisor: the shim's, which name the stale
			// lock the adoption failed on, to carry into the log.
			if test.reason != "" {
				if reason := runtime.RefusalReason(err); reason != test.reason {
					t.Fatalf("runtime.RefusalReason(%v) = %q, want the far side's words %q", err, reason, test.reason)
				}
			}
		})
	}
}

// The turn facts are events, in wire order, and an answer is not one of them: an answered
// get_state, then OMP's agent_start, the tool call it makes, and its agent_end, then the close,
// is exactly Hello, TurnStart, ToolStep, TurnEnd, Closed (F4, B4).
func TestEventsCarryTheTurnInWireOrderAndNoAck(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := dial(t, h.listener.Addr())
	p.hello(testToken)
	p.expect(shimwire.TypeHelloAck)
	request, result := h.getState(context.Background(), p)
	p.send(answerState(t, request))
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("GetState: %v", err)
	}
	conn := h.conn()
	p.send(shimwire.AgentStart{})
	p.send(shimwire.ToolExecutionStart{ToolCallID: "toolu_turn1_0", ToolName: "bash",
		Args: json.RawMessage(`{"command":"echo hi"}`), Intent: "Running the case's step"})
	p.send(shimwire.AgentEnd{})
	p.conn.Close()
	want := []Event{
		Hello{Claim: testClaim, Generation: testGeneration},
		TurnStart{Claim: testClaim},
		// The length and the fingerprint of what the frame carried, and no argument text at all:
		// `{"command":"echo hi"}` is 21 bytes, and the first eight of its SHA-256 are this.
		ToolStep{Claim: testClaim, Tool: "bash", CallID: "toolu_turn1_0",
			ArgsBytes: 21, ArgsHash: "1eab1ef18bb109ae", ConnSequence: conn.Sequence()},
		TurnEnd{Claim: testClaim},
		Closed{Claim: testClaim},
	}
	var got []Event
	for range want {
		got = append(got, h.next())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	if rest := h.stop(); len(rest) != 0 {
		t.Fatalf("further events = %#v, want none", rest)
	}
}

// The end of a tool call is one event too, carrying the call id its start carried and no
// arguments: the frame repeats none, and pairing the two is the supervisor's work. A frame this
// package still models nothing of — a message update, an error — is dropped here as before.
func TestAToolStepEventRecordsACallsEndAndAnUnmodelledFrameIsDropped(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	p.send(shimwire.ToolExecutionEnd{ToolCallID: "toolu_turn1_0", ToolName: "bash"})
	p.send(shimwire.Raw{Type: "message_update", JSON: json.RawMessage(`{"type":"message_update","content":"thinking"}`)})
	p.conn.Close()
	want := ToolStep{Claim: testClaim, Tool: "bash", CallID: "toolu_turn1_0", End: true, ConnSequence: conn.Sequence()}
	if got := h.next(); !reflect.DeepEqual(got, want) {
		t.Fatalf("event = %#v, want %#v", got, want)
	}
	if got := h.next(); got != (Closed{Claim: testClaim}) {
		t.Fatalf("event after the unmodelled frame = %#v, want the close: that frame is no event", got)
	}
	if rest := h.stop(); len(rest) != 0 {
		t.Fatalf("further events = %#v, want none", rest)
	}
}

// An extension of the worker's Oh My Pi failing — the Legion plugin's session_start without the
// daemon it needs, say — leaves the agent running without what that extension gives it, and Oh My
// Pi says so only on its RPC stream: the daemon logs it, naming the claim, and emits no event.
func TestAWorkersExtensionErrorIsLogged(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := dial(t, h.listener.Addr())
	p.hello(testToken)
	p.expect(shimwire.TypeHelloAck)
	p.send(shimwire.Raw{Type: "extension_error", JSON: json.RawMessage(`{"type":"extension_error","extensionPath":"/opt/legion/pi-legion/dist/legion.js","event":"session_start","error":"LEGION_DAEMON_URL is required for Legion"}`)})
	p.conn.Close()
	for _, want := range []Event{Hello{Claim: testClaim, Generation: testGeneration}, Closed{Claim: testClaim}} {
		if got := h.next(); got != want {
			t.Fatalf("event = %#v, want %#v", got, want)
		}
	}
	h.stop()
	logs := h.logs.Lines()
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "worker-stream: an extension of the worker's Oh My Pi failed") ||
		!strings.Contains(logs[0], "claim="+string(testClaim)) ||
		!strings.Contains(logs[0], "extension=/opt/legion/pi-legion/dist/legion.js") ||
		!strings.Contains(logs[0], "event=session_start") ||
		!strings.Contains(logs[0], "error=LEGION_DAEMON_URL is required for Legion") {
		t.Fatalf("logs = %q, want one warning naming the claim, the extension, its event and its error", logs)
	}
}

// The daemon sends no prompt (a task is a value the worker takes), but a worker older than that,
// or a shim whose backlog holds what Oh My Pi wrote before it, still writes a prompt's frames: the
// `prompt_result` Oh My Pi writes behind a turn's end (its own line, verbatim, with the error
// object of a provider failure), a `prompt` command's answer and refusal, and the start the shim
// replayed for a retried prompt with the delivery it named. Each is dropped quietly — no event,
// no log line — and the turn frames around them are read as ever; a replayed start is the plain
// TurnStart. The same frames replayed on a replacement connection, which never sent anything, are
// dropped the same way, and the connection still answers its next request.
func TestAnOldWorkersPromptFramesAreDroppedQuietly(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	first := h.conn()
	oldFrames := func(p *peer) {
		p.writeRaw(`{"type":"response","id":"delivery-1.XYZ","command":"prompt","success":true}` + "\n")
		p.writeRaw(`{"type":"agent_start","deliveryId":"delivery-1"}` + "\n")
		p.send(shimwire.AgentEnd{})
		p.writeRaw(`{"type":"prompt_result","id":"delivery-1.XYZ","agentInvoked":true,"status":"error",` +
			`"error":{"message":"429 Too Many Requests","provider":"anthropic","model":"claude-opus-4-5","httpStatus":429,"retryable":true},` +
			"\"sessionSettled\":true}\n")
		p.writeRaw(`{"type":"prompt_result","id":"delivery-1.XYZ","agentInvoked":true,"status":"completed","sessionSettled":false}` + "\n")
		p.writeRaw(`{"type":"response","id":"delivery-1.XYZ","command":"prompt","success":false,"error":"Agent is busy"}` + "\n")
	}
	oldFrames(p)
	p.conn.Close()
	for _, want := range []Event{TurnStart{Claim: testClaim}, TurnEnd{Claim: testClaim}, Closed{Claim: testClaim}} {
		if got := h.next(); got != want {
			t.Fatalf("event = %#v, want %#v", got, want)
		}
	}

	replacement := h.connect(testToken)
	replaced := h.conn()
	oldFrames(replacement)
	request, result := h.getState(context.Background(), replacement)
	replacement.send(answerState(t, request))
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("GetState after the old frames: %v", err)
	}
	replacement.conn.Close()
	for _, want := range []Event{TurnStart{Claim: testClaim}, TurnEnd{Claim: testClaim}, Closed{Claim: testClaim}} {
		if got := h.next(); got != want {
			t.Fatalf("event on the replacement = %#v, want %#v", got, want)
		}
	}
	if rest := h.stop(); len(rest) != 0 {
		t.Fatalf("further events = %#v, want none", rest)
	}
	if logs := h.logs.Lines(); len(logs) != 0 {
		t.Fatalf("logs = %q, want none: an old worker's prompt frames are dropped quietly", logs)
	}
	// The listener numbers connections in the order it registers them, which is how the
	// supervisor tells a connection newer than another.
	if first.Sequence() == 0 || replaced.Sequence() <= first.Sequence() {
		t.Fatalf("sequences: first %d, replacement %d; want the replacement's larger", first.Sequence(), replaced.Sequence())
	}
}

// rpc_chunk sequences between whole frames reassemble through shimwire and dispatch exactly as
// a plain line would: a chunked agent_end ends the turn, a chunked response answers its request.
func TestChunkSequencesInterleavedWithWholeFramesReassemble(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()

	p.send(shimwire.AgentStart{})
	p.sendChunked("agent-end-1", agentEndFrame(t, 3<<20+17))
	var state runtime.ConnState
	result := async(func() error {
		var err error
		state, err = conn.GetState(context.Background())
		return err
	})
	p.negotiate()
	request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
	p.send(shimwire.AgentStart{})
	// A get_state answer too large for one line: padded data, chunked like any other frame.
	big := `{"id":"` + request.ID + `","type":"response","command":"get_state","success":true,"data":{"isStreaming":true,"pad":"` +
		strings.Repeat("y", shimwire.MaxFrameBytes) + `"}}`
	p.sendChunked("state-1", []byte(big))
	p.sendChunked("agent-end-2", agentEndFrame(t, shimwire.MaxFrameBytes+5))

	if err := awaitResult(t, result); err != nil {
		t.Fatalf("GetState answered by a chunked response: %v", err)
	}
	if !state.IsStreaming {
		t.Fatal("GetState lost the chunked answer's isStreaming")
	}
	want := []Event{TurnStart{Claim: testClaim}, TurnEnd{Claim: testClaim}, TurnStart{Claim: testClaim}, TurnEnd{Claim: testClaim}}
	var got []Event
	for range want {
		got = append(got, h.next())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	if lines := h.logs.Lines(); len(lines) != 0 {
		t.Fatalf("logs = %q, want none", lines)
	}
}

// A whole frame arriving mid-sequence means the sender abandoned it: the sequence is dropped
// and logged, never joined to what follows, and the whole frame is still read
// (worker-rpc.ts:348-349).
func TestAWholeFrameInterruptingASequenceDropsTheSequence(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	chunks, err := shimwire.SplitChunks("agent-end-1", agentEndFrame(t, 3<<20))
	if err != nil {
		t.Fatal(err)
	}
	p.send(chunks[0])
	p.send(chunks[1])
	p.send(shimwire.AgentStart{})
	for _, chunk := range chunks[2:] {
		p.send(chunk) // the tail of a sequence whose head is gone: refused, not a frame
	}
	p.send(shimwire.AgentEnd{})
	if event := h.next(); event != (TurnStart{Claim: testClaim}) {
		t.Fatalf("event = %#v, want the interrupting TurnStart", event)
	}
	if event := h.next(); event != (TurnEnd{Claim: testClaim}) {
		t.Fatalf("event = %#v, want the plain agent_end's TurnEnd and nothing from the dropped sequence", event)
	}
	logs := h.logs.Lines()
	if len(logs) == 0 || !strings.Contains(logs[0], "abandoned after 2 of") {
		t.Fatalf("logs = %q, want the abandoned sequence named first", logs)
	}
}

func TestGetStateReadsIsStreaming(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		h := startListener(t, harnessOptions{})
		p := h.connect(testToken)
		conn := h.conn()
		var state runtime.ConnState
		result := async(func() error {
			var err error
			state, err = conn.GetState(context.Background())
			return err
		})
		p.negotiate()
		request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
		p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeGetState, Success: true,
			Data: mustJSON(t, map[string]any{"isStreaming": streaming, "model": "m"})})
		if err := awaitResult(t, result); err != nil || state.IsStreaming != streaming {
			t.Fatalf("GetState = %#v, %v; want IsStreaming %v", state, err, streaming)
		}
	}
}

// An answer that does not say whether a turn is running is not "idle": the daemon's restart
// decision hangs on it, so it is an error rather than a guess (worker-rpc.ts:476-485).
func TestGetStateRefusesAnAnswerWithoutIsStreaming(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	result := async(func() error { _, err := conn.GetState(context.Background()); return err })
	p.negotiate()
	request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
	p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeGetState, Success: true, Data: mustJSON(t, map[string]any{"model": "m"})})
	if err := awaitResult(t, result); err == nil {
		t.Fatal("GetState read an answer without isStreaming as a state")
	}
}

// Negotiation is protocol v2, sent once per connection — before the first request OMP answers,
// so every holder of a runtime.Conn gets a negotiated stream without asking for it.
func TestNegotiationIsSentOnceAheadOfTheFirstAgentRequest(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	result := async(func() error { return conn.Negotiate(context.Background()) })
	p.negotiate()
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if err := awaitResult(t, async(func() error { return conn.Negotiate(context.Background()) })); err != nil {
		t.Fatalf("a second Negotiate: %v", err)
	}
	// The next request goes out with no second negotiate_protocol ahead of it.
	result = async(func() error { _, err := conn.GetState(context.Background()); return err })
	request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
	p.send(answerState(t, request))
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("GetState: %v", err)
	}
}

func TestANegotiationOMPRefusesFailsTheRequestBehindIt(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	result := async(func() error { _, err := conn.GetState(context.Background()); return err })
	request := p.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
	p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeNegotiateProtocol, Success: false, Error: "unsupported"})
	err := awaitResult(t, result)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Command != shimwire.TypeNegotiateProtocol {
		t.Fatalf("GetState behind a refused negotiation = %v, want its RefusedError", err)
	}
	// Not negotiated, so the next request negotiates again.
	state, result := h.getState(context.Background(), p)
	p.send(answerState(t, state))
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("GetState after a successful negotiation: %v", err)
	}
}

// A response to the wrong command is not an answer the request can use.
func TestAnAnswerForAnotherCommandIsAnError(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	result := async(func() error { return conn.Negotiate(context.Background()) })
	request := p.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
	p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeGetState, Success: true})
	if err := awaitResult(t, result); err == nil {
		t.Fatal("Negotiate accepted a get_state response as its answer")
	}
}

// Shutdown is a frame the shim answers itself by closing OMP's stdin; nothing comes back.
func TestShutdownWritesTheFrame(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	if err := conn.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	p.expect(shimwire.TypeShutdown)
	p.conn.Close()
	if event := h.next(); event != (Closed{Claim: testClaim}) {
		t.Fatalf("event = %#v, want Closed", event)
	}
	if err := conn.Shutdown(context.Background()); !isClosed(err) {
		t.Fatalf("Shutdown on a closed connection = %v, want ErrClosed", err)
	}
}

func TestAdoptWorkingCopyAsksTheShimAndReadsItsResult(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	identity := runtime.GitIdentity{Name: "legion-bot", Email: "bot@legion.dev"}

	result := async(func() error { return conn.AdoptWorkingCopy(context.Background(), identity, 3*time.Second) })
	request := p.expect(shimwire.TypeAdoptWorkingCopy).(shimwire.AdoptWorkingCopy)
	if request.JJUser != "legion-bot" || request.JJEmail != "bot@legion.dev" || request.TimeoutMs != 3000 || request.ID == "" {
		t.Fatalf("adopt-working-copy frame = %#v", request)
	}
	p.send(shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: true})
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("AdoptWorkingCopy: %v", err)
	}

	result = async(func() error { return conn.AdoptWorkingCopy(context.Background(), identity, 3*time.Second) })
	request = p.expect(shimwire.TypeAdoptWorkingCopy).(shimwire.AdoptWorkingCopy)
	p.send(shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: false, Error: "jj metaedit exited 1"})
	err := awaitResult(t, result)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "jj metaedit exited 1" {
		t.Fatalf("AdoptWorkingCopy = %v, want a RefusedError carrying the shim's error", err)
	}

	if err := conn.AdoptWorkingCopy(context.Background(), runtime.GitIdentity{}, time.Second); err == nil {
		t.Fatal("AdoptWorkingCopy sent a frame with no identity")
	}
}

// The push check is a request the shim answers itself, with one entry per workspace; a shim built
// before it forwards the frame to its agent, whose refusal is runtime.ErrRescueUnsupported, and a
// shim that answers and could not run the check is a refusal like any other. The frame carries
// both of the request's branches: a shim that reads target falls back to the per-pod prefix only
// when target is missing, so a target dropped here would send the work there with nothing failing.
func TestRescueUnpushedAsksTheShimAndTellsAnOldShimApart(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	request := runtime.RescueRequest{Branch: "legion/LEGION-484-rescue-9d3f0c7a", Target: "legion/LEGION-484-rescue",
		WorkspaceTimeout: 2 * time.Second, Timeout: 3 * time.Second}
	var report []runtime.WorkspaceRescue
	ask := func() <-chan error {
		return async(func() (err error) {
			report, err = conn.RescueUnpushed(context.Background(), request)
			return err
		})
	}

	result := ask()
	frame := p.expect(shimwire.TypeRescueUnpushed).(shimwire.RescueUnpushed)
	if frame.Branch != request.Branch || frame.WorkspaceTimeoutMs != 2000 || frame.TimeoutMs != 3000 || frame.ID == "" {
		t.Fatalf("rescue-unpushed frame = %#v", frame)
	}
	if frame.Target != request.Target {
		t.Fatalf("rescue-unpushed frame carries target %q, want the request's %q: without it a shim pushes to the per-pod prefix %q",
			frame.Target, request.Target, frame.Branch)
	}
	// One workspace as a shim that predates the branch's commits reports it, and one as a shim
	// that knows them: both reach the runtime boundary whole.
	p.send(shimwire.RescueUnpushedResult{ID: frame.ID, OK: true, Workspaces: []shimwire.RescueWorkspace{
		{Dir: "/legion/workspaces/acme/widgets/legion-484", Status: "rescued", Branches: []shimwire.RescueBranch{{Name: request.Branch + "-1", Commit: "c0ffee"}}},
		{Dir: "/legion/workspaces/acme/widgets/legion-485", Status: "rescued", Branches: []shimwire.RescueBranch{{
			Name: request.Target, Commit: "facade", Previous: "decade", Pushed: true,
			Commits: []shimwire.RescueCommit{{ID: "facade", Subject: "rescue: LEGION-485"}, {ID: "beef01", Subject: "fix the widget"}},
		}}},
	}})
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("RescueUnpushed: %v", err)
	}
	want := []runtime.WorkspaceRescue{
		{Dir: "/legion/workspaces/acme/widgets/legion-484", Status: runtime.RescueRescued,
			Branches: []runtime.RescuedBranch{{Name: request.Branch + "-1", Commit: "c0ffee"}}},
		{Dir: "/legion/workspaces/acme/widgets/legion-485", Status: runtime.RescueRescued, Branches: []runtime.RescuedBranch{{
			Name: request.Target, Commit: "facade", Previous: "decade", Pushed: true,
			Commits: []runtime.RescuedCommit{{ID: "facade", Subject: "rescue: LEGION-485"}, {ID: "beef01", Subject: "fix the widget"}},
		}}},
	}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("report = %+v, want %+v", report, want)
	}

	result = ask()
	frame = p.expect(shimwire.TypeRescueUnpushed).(shimwire.RescueUnpushed)
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypeRescueUnpushed, Success: false, Error: "Unknown command: rescue-unpushed"})
	if err := awaitResult(t, result); !errors.Is(err, runtime.ErrRescueUnsupported) {
		t.Fatalf("RescueUnpushed answered by the agent's refusal = %v, want ErrRescueUnsupported", err)
	}

	result = ask()
	frame = p.expect(shimwire.TypeRescueUnpushed).(shimwire.RescueUnpushed)
	p.send(shimwire.RescueUnpushedResult{ID: frame.ID, OK: false, Error: "worker-shim: LEGION_WORKSPACE is not set; no disk to check"})
	err := awaitResult(t, result)
	var refused *RefusedError
	if !errors.As(err, &refused) || errors.Is(err, runtime.ErrRescueUnsupported) || !strings.Contains(refused.Reason, "LEGION_WORKSPACE") {
		t.Fatalf("RescueUnpushed refused by the shim = %v, want a RefusedError carrying its reason", err)
	}

	if _, err := conn.RescueUnpushed(context.Background(), runtime.RescueRequest{Branch: "main", WorkspaceTimeout: time.Second, Timeout: time.Second}); !errors.Is(err, shimwire.ErrMalformedFrame) {
		t.Fatalf("RescueUnpushed for a branch outside legion/ = %v, want the frame's own refusal", err)
	}
}

func TestAgentSecretsEnrollmentIsAnsweredByTheShim(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	result := async(func() error { return h.conn().AgentSecretsEnrollment(context.Background(), "enr-1") })
	request := p.expect(shimwire.TypeAgentSecretsEnrollment).(shimwire.AgentSecretsEnrollment)
	if request.EnrollmentID != "enr-1" || request.ID == "" {
		t.Fatalf("the shim received %+v", request)
	}
	p.send(shimwire.AgentSecretsEnrollmentResult{ID: request.ID, OK: true})
	if err := awaitResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSecretsEnrollmentRefusalIsARefusedError(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	result := async(func() error { return h.conn().AgentSecretsEnrollment(context.Background(), "enr-1") })
	request := p.expect(shimwire.TypeAgentSecretsEnrollment).(shimwire.AgentSecretsEnrollment)
	p.send(shimwire.AgentSecretsEnrollmentResult{ID: request.ID, OK: false, Error: "key dir is read-only"})
	err := awaitResult(t, result)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Command != shimwire.TypeAgentSecretsEnrollment || refused.Reason != "key dir is read-only" {
		t.Fatalf("err = %v, want a RefusedError with the shim's reason", err)
	}
}

func TestAgentSecretsEnrollmentNeedsAnID(t *testing.T) {
	h := startListener(t, harnessOptions{})
	h.connect(testToken)
	if err := h.conn().AgentSecretsEnrollment(context.Background(), ""); err == nil || !errors.Is(err, shimwire.ErrMalformedFrame) {
		t.Fatalf("err = %v, want the frame's own refusal", err)
	}
}

func TestAgentSecretsTokenIsReadByTheShim(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	var token string
	result := async(func() (err error) {
		token, err = h.conn().AgentSecretsToken(context.Background())
		return err
	})
	request := p.expect(shimwire.TypeAgentSecretsToken).(shimwire.AgentSecretsToken)
	if request.ID == "" {
		t.Fatalf("the shim received %+v", request)
	}
	p.send(shimwire.AgentSecretsTokenResult{ID: request.ID, OK: true, PodToken: "eyJhbGciOiJSUzI1NiJ9.e30.fresh"})
	if err := awaitResult(t, result); err != nil {
		t.Fatal(err)
	}
	if token != "eyJhbGciOiJSUzI1NiJ9.e30.fresh" {
		t.Fatalf("token %q, want the one the shim read", token)
	}
}

// A shim that cannot read its token refuses the request, and a shim built before the request
// forwards it to OMP, which refuses it as an unknown command: both are runtime.ErrTokenRefused,
// which no retry with the same shim changes.
func TestATokenRequestTheShimCannotAnswerIsRefused(t *testing.T) {
	for name, answer := range map[string]func(id string) shimwire.Frame{
		"the shim refuses it": func(id string) shimwire.Frame {
			return shimwire.AgentSecretsTokenResult{ID: id, OK: false, Error: "agent-secrets: read the projected token /var/run/token: no such file or directory"}
		},
		"an older shim forwards it to OMP": func(id string) shimwire.Frame {
			return shimwire.Response{ID: id, Command: shimwire.TypeAgentSecretsToken, Success: false, Error: "Unknown command: agent-secrets-token"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := startListener(t, harnessOptions{})
			p := h.connect(testToken)
			result := async(func() error {
				_, err := h.conn().AgentSecretsToken(context.Background())
				return err
			})
			request := p.expect(shimwire.TypeAgentSecretsToken).(shimwire.AgentSecretsToken)
			p.send(answer(request.ID))
			err := awaitResult(t, result)
			var refused *RefusedError
			if !errors.Is(err, runtime.ErrTokenRefused) || !errors.As(err, &refused) || refused.Command != shimwire.TypeAgentSecretsToken {
				t.Fatalf("err = %v, want a RefusedError that is runtime.ErrTokenRefused", err)
			}
			if errors.Is(err, runtime.ErrAdoptRefused) {
				t.Fatal("a refused token request reads as a refused adoption")
			}
		})
	}
}
