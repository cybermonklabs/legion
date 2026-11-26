package stream

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

var _ runtime.Conn = (*Conn)(nil)

var (
	// ErrClosed is the transport failing under a request: the connection closed before the
	// answer came, or a write to it failed. It is never an acknowledgement and never a refusal.
	ErrClosed = errors.New("worker stream closed")
	// ErrNoAnswer is a request whose bound ran out before its answer arrived. The bound is the RPC
	// timeout (worker_rpc_timeout_seconds) or the request's own. The shipped client puts the RPC
	// timeout on every request (worker-rpc.ts:407-421). The bound counts from the request's start,
	// so it covers the wait for the writer and the write too.
	ErrNoAnswer = errors.New("no answer on the worker stream within the RPC timeout")
	// ErrNoModelTotals is a read of a launch's model totals on a connection whose hello did not say
	// that its shim counts them. An older shim would forward the frame to its harness. A stand-in
	// shim would not answer it. So the read fails at once and writes nothing.
	ErrNoModelTotals = errors.New("the shim's hello did not offer its launch's model totals")
)

// RefusedError is the far side answering a request with a refusal — OMP's `{success:false}`, or
// a shim's failed adoption, enrollment, token read or workspace read. The connection is
// fine; the agent said no.
//
// A refused prompt is runtime.ErrPromptRefused, which is what the supervisor charges a prompt
// budget on unless Reason is OMP's busy answer (runtime.RefusalReason); a refused adoption is
// runtime.ErrAdoptRefused, charged to that same budget with no such exemption (the busy answer is
// OMP's about a turn, not the shim's about a working copy); and a refused token request is
// runtime.ErrTokenRefused. No other refusal is any of them — a refused negotiation ahead of the
// prompt, a refused get_state — so the match is on the command rather than an unconditional
// Unwrap.
type RefusedError struct {
	Claim   claim.Token
	Command string
	Reason  string
}

func (e *RefusedError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("worker-stream: %s refused %s", e.Claim, e.Command)
	}
	return fmt.Sprintf("worker-stream: %s refused %s: %s", e.Claim, e.Command, e.Reason)
}

// RefusalReason is the words the far side refused with (runtime.RefusalReason).
func (e *RefusedError) RefusalReason() string { return e.Reason }

func (e *RefusedError) Is(target error) bool {
	switch target {
	case runtime.ErrPromptRefused:
		return e.Command == shimwire.TypePrompt
	case runtime.ErrAdoptRefused:
		return e.Command == shimwire.TypeAdoptWorkingCopy
	case runtime.ErrTokenRefused:
		return e.Command == shimwire.TypeAgentSecretsToken
	}
	return false
}

// The RPC protocol version the daemon speaks: v2 is the one with rpc_chunk, without which OMP
// writes a whole transcript's agent_end as one line no reader here accepts (worker-rpc.ts:430).
const protocolVersion = 2

// Conn is one claim's live connection: requests out, answers and turn facts back. It satisfies
// runtime.Conn, which is how everything above the stream holds it.
//
// Every request OMP answers is preceded, once per connection, by the protocol negotiation, so a
// holder of a runtime.Conn never has to know the protocol has versions. Each request has one
// context: the caller's, narrowed by the request's bound where it has one. That context bounds the
// whole request: its wait for the writer, its write and its wait for the answer.
type Conn struct {
	// seq is the order the listener registered the connection in (Sequence).
	seq uint64
	// generation is the launch the connection's hello was registered for (Hello.Generation),
	// which each Answered it reads carries.
	generation uint64
	// modelTotals is the hello saying that the shim counts its launch's calls by model, and
	// answers a read of them (ModelTotals).
	modelTotals bool
	claim       claim.Token
	nc          net.Conn
	writer      *shimwire.Writer
	rpcTimeout  time.Duration
	log         *slog.Logger
	events      *eventQueue

	mu      sync.Mutex
	pending map[string]pendingRequest
	// acked is the prompt OMP last answered `{success:true}` on this connection, until its turn
	// ends or another prompt supersedes it: the only prompt a later `{success:false}` can take
	// back (worker-rpc.ts:359-368). It is set by the reader when the answer is read, so it
	// follows wire order rather than the order the waiting goroutine wakes in.
	acked *ackedPrompt
	// sent holds the request ids of the prompts this connection wrote. A prompt refusal naming one
	// it did not write was replayed from the shim's backlog, answering a prompt an earlier
	// connection sent (promptDelivery).
	sent   map[string]struct{}
	closed bool
	done   chan struct{}

	// negotiation is a one-slot semaphore rather than a mutex so a request queued behind another
	// goroutine's negotiation still answers to its own context.
	negotiation chan struct{}
	negotiated  bool
}

type pendingRequest struct {
	answer chan shimwire.Frame
	// deliveryID is set for a prompt, and names the delivery a late refusal would take back.
	deliveryID string
}

type ackedPrompt struct {
	requestID  string
	deliveryID string
}

func newConn(nc net.Conn, token claim.Token, rpcTimeout time.Duration, log *slog.Logger, events *eventQueue) *Conn {
	return &Conn{
		claim:       token,
		nc:          nc,
		writer:      shimwire.NewWriter(connWriter{nc: nc, timeout: rpcTimeout}),
		rpcTimeout:  rpcTimeout,
		log:         log,
		events:      events,
		pending:     map[string]pendingRequest{},
		sent:        map[string]struct{}{},
		done:        make(chan struct{}),
		negotiation: make(chan struct{}, 1),
	}
}

// Negotiate opens protocol v2 on this connection. It is sent once: a connection already
// negotiated answers at once, and one whose negotiation failed tries again on the next call.
func (c *Conn) Negotiate(ctx context.Context) error {
	select {
	case c.negotiation <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("worker-stream: %s %s: %w", c.claim, shimwire.TypeNegotiateProtocol, ctx.Err())
	}
	defer func() { <-c.negotiation }()
	if c.negotiated {
		return nil
	}
	id := rand.Text()
	if _, err := c.call(ctx, shimwire.NegotiateProtocol{ID: id, ProtocolVersion: protocolVersion}, id, ""); err != nil {
		return err
	}
	c.negotiated = true
	return nil
}

// Prompt hands OMP a task and returns on its acknowledgement — which is not delivery: OMP
// answers `{success:true}` before the turn starts, and the turn is observed on Events(). OMP
// refusing the prompt is a *RefusedError that is runtime.ErrPromptRefused; every other failure —
// ErrClosed, ErrNoAnswer, the context, a refused negotiation — is transport and is not.
func (c *Conn) Prompt(ctx context.Context, deliveryID, message string) error {
	if deliveryID == "" {
		return fmt.Errorf("worker-stream: %s: a prompt needs a delivery id, the one thing the shim dedupes a retry on", c.claim)
	}
	if err := c.Negotiate(ctx); err != nil {
		return err
	}
	id := deliveryID + promptIDSeparator + rand.Text()
	_, err := c.call(ctx, shimwire.Prompt{ID: id, DeliveryID: deliveryID, Message: message}, id, deliveryID)
	return err
}

// Sequence is the order the listener registered this connection in (Listener.register).
func (c *Conn) Sequence() uint64 { return c.seq }

// promptIDSeparator ends the delivery id a prompt's request id begins with. OMP's answer carries
// only the request id, and a refusal it gave while no daemon was connected reaches a later
// connection, which never sent that request; the id is how that refusal still names its delivery.
// The random suffix Prompt appends never holds the separator, and neither does either kind of
// delivery id the daemon makes: random text, or an outbox row's `outbox:<row>`.
const promptIDSeparator = "."

// promptDelivery is the delivery a prompt's request id names, or "" for an id no prompt carried.
// It splits at the last separator, the one Prompt put before its own suffix.
func promptDelivery(requestID string) string {
	at := strings.LastIndex(requestID, promptIDSeparator)
	if at < 0 {
		return ""
	}
	return requestID[:at]
}

// GetState asks OMP whether a turn is running. An answer that does not say is an error, never
// "not streaming": a restart decides between confirming a delivery and re-sending it on this.
func (c *Conn) GetState(ctx context.Context) (runtime.ConnState, error) {
	if err := c.Negotiate(ctx); err != nil {
		return runtime.ConnState{}, err
	}
	id := rand.Text()
	response, err := c.call(ctx, shimwire.GetState{ID: id}, id, "")
	if err != nil {
		return runtime.ConnState{}, err
	}
	var data shimwire.StateData
	if len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, &data); err != nil {
			return runtime.ConnState{}, fmt.Errorf("worker-stream: %s get_state answer: %w", c.claim, err)
		}
	}
	if data.IsStreaming == nil {
		return runtime.ConnState{}, fmt.Errorf("worker-stream: %s get_state answer carries no data.isStreaming", c.claim)
	}
	return runtime.ConnState{IsStreaming: *data.IsStreaming}, nil
}

// Compact asks Oh My Pi to compact its session's context. The shim answers nothing of it and
// forwards it as it forwards a prompt, so the answer is Oh My Pi's own `response` to the
// command: success for a session compacted, and `success: false` with Oh My Pi's reason for a
// compaction it refused or skipped, which is a *RefusedError here (runtime.RefusalReason reads
// the words). The wait is the request's own bound rather than the RPC timeout: Oh My Pi
// answers only once the summary is written, which is a model call over the whole transcript.
func (c *Conn) Compact(ctx context.Context, customInstructions string, bound time.Duration) error {
	if err := c.Negotiate(ctx); err != nil {
		return err
	}
	id := rand.Text()
	frame := shimwire.Compact{ID: id, CustomInstructions: customInstructions}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("worker-stream: %s: %w", c.claim, err)
	}
	_, err := c.callWithin(ctx, frame, id, "", bound)
	return err
}

// Shutdown asks the shim to end OMP: SIGTERM, then SIGKILL once the shim's grace runs out. The
// shim acts on the frame itself and answers nothing (internal/shim); whether the process ended is
// the runtime's to observe.
func (c *Conn) Shutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return fmt.Errorf("worker-stream: %s %s: %w", c.claim, shimwire.TypeShutdown, ErrClosed)
	}
	return c.write(shimwire.Shutdown{})
}

// AdoptWorkingCopy asks the shim to set the author of its workspace's working copy. The shim
// runs the command within timeout; the answer is awaited for that plus the RPC timeout, the
// round trip (worker-rpc.ts:492-505).
func (c *Conn) AdoptWorkingCopy(ctx context.Context, identity runtime.GitIdentity, timeout time.Duration) error {
	id := rand.Text()
	frame := shimwire.AdoptWorkingCopy{ID: id, JJUser: identity.Name, JJEmail: identity.Email, TimeoutMs: int(timeout.Milliseconds())}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("worker-stream: %s: %w", c.claim, err)
	}
	answer, err := c.request(ctx, frame, id, "", timeout+c.rpcTimeout)
	if err != nil {
		return err
	}
	switch answer := answer.(type) {
	case shimwire.AdoptWorkingCopyResult:
		if answer.OK {
			return nil
		}
		return &RefusedError{Claim: c.claim, Command: shimwire.TypeAdoptWorkingCopy, Reason: answer.Error}
	case shimwire.Response:
		// A shim that does not answer the frame itself forwards it, and OMP refuses a command
		// it does not know.
		if !answer.Success {
			return &RefusedError{Claim: c.claim, Command: shimwire.TypeAdoptWorkingCopy, Reason: answer.Error}
		}
	}
	return fmt.Errorf("worker-stream: %s %s answered by a %s frame", c.claim, shimwire.TypeAdoptWorkingCopy, answer.FrameType())
}

// WorkspaceWork asks the shim whether the workspace it runs in holds work that no branch or tag on
// origin holds. The shim runs its two jj reads within timeout; the answer is awaited for that plus
// the RPC timeout, the round trip. A shim built before the read forwards it to OMP, whose
// unknown-command answer is a refusal like the shim's own.
func (c *Conn) WorkspaceWork(ctx context.Context, issue string, timeout time.Duration) (runtime.WorkspaceWork, error) {
	id := rand.Text()
	frame := shimwire.WorkspaceWork{ID: id, Issue: issue, TimeoutMs: int(timeout.Milliseconds())}
	if err := frame.Validate(); err != nil {
		return runtime.WorkspaceWork{}, fmt.Errorf("worker-stream: %s: %w", c.claim, err)
	}
	answer, err := c.request(ctx, frame, id, "", timeout+c.rpcTimeout)
	if err != nil {
		return runtime.WorkspaceWork{}, err
	}
	switch answer := answer.(type) {
	case shimwire.WorkspaceWorkResult:
		work, refused, err := runtime.WorkspaceWorkOf(answer)
		switch {
		case err != nil:
			return runtime.WorkspaceWork{}, fmt.Errorf("worker-stream: %s: %w", c.claim, err)
		case refused != "":
			return runtime.WorkspaceWork{}, &RefusedError{Claim: c.claim, Command: shimwire.TypeWorkspaceWork, Reason: refused}
		}
		return work, nil
	case shimwire.Response:
		// A shim that does not answer the frame itself forwards it, and OMP refuses a command
		// it does not know.
		if !answer.Success {
			return runtime.WorkspaceWork{}, &RefusedError{Claim: c.claim, Command: shimwire.TypeWorkspaceWork, Reason: answer.Error}
		}
	}
	return runtime.WorkspaceWork{}, fmt.Errorf("worker-stream: %s %s answered by a %s frame", c.claim, shimwire.TypeWorkspaceWork, answer.FrameType())
}

// AgentSecretsEnrollment hands the shim the broker's enrollment id for its pod; the shim writes it
// beside the key and starts the lease renewer, and answers. Bounded by the RPC timeout: a file
// write, not a command.
func (c *Conn) AgentSecretsEnrollment(ctx context.Context, enrollmentID string) error {
	id := rand.Text()
	frame := shimwire.AgentSecretsEnrollment{ID: id, EnrollmentID: enrollmentID}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("worker-stream: %s: %w", c.claim, err)
	}
	answer, err := c.request(ctx, frame, id, "", c.rpcTimeout)
	if err != nil {
		return err
	}
	switch answer := answer.(type) {
	case shimwire.AgentSecretsEnrollmentResult:
		if answer.OK {
			return nil
		}
		return &RefusedError{Claim: c.claim, Command: shimwire.TypeAgentSecretsEnrollment, Reason: answer.Error}
	case shimwire.Response:
		if !answer.Success {
			return &RefusedError{Claim: c.claim, Command: shimwire.TypeAgentSecretsEnrollment, Reason: answer.Error}
		}
	}
	return fmt.Errorf("worker-stream: %s %s answered by a %s frame", c.claim, shimwire.TypeAgentSecretsEnrollment, answer.FrameType())
}

// AgentSecretsToken asks the shim to read its projected token again and returns what it read.
// Bounded by the RPC timeout: a file read, not a command. A shim built before the request forwards
// it to OMP, whose unknown-command answer is a refusal like the shim's own.
func (c *Conn) AgentSecretsToken(ctx context.Context) (string, error) {
	id := rand.Text()
	answer, err := c.request(ctx, shimwire.AgentSecretsToken{ID: id}, id, "", c.rpcTimeout)
	if err != nil {
		return "", err
	}
	switch answer := answer.(type) {
	case shimwire.AgentSecretsTokenResult:
		if answer.OK {
			return answer.PodToken, nil
		}
		return "", &RefusedError{Claim: c.claim, Command: shimwire.TypeAgentSecretsToken, Reason: answer.Error}
	case shimwire.Response:
		if !answer.Success {
			return "", &RefusedError{Claim: c.claim, Command: shimwire.TypeAgentSecretsToken, Reason: answer.Error}
		}
	}
	return "", fmt.Errorf("worker-stream: %s %s answered by a %s frame", c.claim, shimwire.TypeAgentSecretsToken, answer.FrameType())
}

// ModelTotals reads what the shim counted of its launch's calls, by model, since its harness
// started. The shim answers the frame itself, so no negotiation goes first. The caller's ctx is
// the read's one bound, and no RPC timeout bounds its wait for the answer. Its write also ends at
// the RPC timeout, as every write does. A shim whose hello did not offer its totals fails the read
// at once, with nothing written (ErrNoModelTotals). A shim built before the read forwards the
// frame to OMP, whose unknown-command answer is a refusal.
func (c *Conn) ModelTotals(ctx context.Context) (runtime.LaunchTotals, error) {
	if !c.modelTotals {
		return runtime.LaunchTotals{}, fmt.Errorf("worker-stream: %s %s: %w", c.claim, shimwire.TypeModelTotals, ErrNoModelTotals)
	}
	id := rand.Text()
	answer, err := c.request(ctx, shimwire.ModelTotals{ID: id}, id, "", 0)
	if err != nil {
		return runtime.LaunchTotals{}, err
	}
	switch answer := answer.(type) {
	case shimwire.ModelTotalsResult:
		if err := answer.Validate(); err != nil {
			return runtime.LaunchTotals{}, fmt.Errorf("worker-stream: %s: %w", c.claim, err)
		}
		if !answer.OK {
			return runtime.LaunchTotals{}, &RefusedError{Claim: c.claim, Command: shimwire.TypeModelTotals, Reason: answer.Error}
		}
		return runtime.LaunchTotals{Generation: c.generation, Totals: *answer.Totals}, nil
	case shimwire.Response:
		if !answer.Success {
			return runtime.LaunchTotals{}, &RefusedError{Claim: c.claim, Command: shimwire.TypeModelTotals, Reason: answer.Error}
		}
	}
	return runtime.LaunchTotals{}, fmt.Errorf("worker-stream: %s %s answered by a %s frame", c.claim, shimwire.TypeModelTotals, answer.FrameType())
}

// call is a request OMP answers with a response to the same command, successful, within the RPC
// timeout.
func (c *Conn) call(ctx context.Context, frame shimwire.Frame, id, deliveryID string) (shimwire.Response, error) {
	return c.callWithin(ctx, frame, id, deliveryID, c.rpcTimeout)
}

// callWithin is call bounded by the request's own wait rather than the RPC timeout: the bound a
// command whose work is not a frame the far side answers at once carries (Compact).
func (c *Conn) callWithin(ctx context.Context, frame shimwire.Frame, id, deliveryID string, bound time.Duration) (shimwire.Response, error) {
	answer, err := c.request(ctx, frame, id, deliveryID, bound)
	if err != nil {
		return shimwire.Response{}, err
	}
	command := frame.FrameType()
	response, ok := answer.(shimwire.Response)
	if !ok {
		return shimwire.Response{}, fmt.Errorf("worker-stream: %s %s answered by a %s frame", c.claim, command, answer.FrameType())
	}
	if response.Command != command {
		return shimwire.Response{}, fmt.Errorf("worker-stream: %s %s answered by a %s response", c.claim, command, response.Command)
	}
	if !response.Success {
		return shimwire.Response{}, &RefusedError{Claim: c.claim, Command: command, Reason: response.Error}
	}
	return response, nil
}

// request writes frame and waits for the frame that answers id. One context bounds the whole
// request: its wait for the writer, its write and its wait for the answer. That context is the
// caller's, narrowed by bound where bound is above 0. A bound of 0 leaves the caller's context
// alone.
func (c *Conn) request(ctx context.Context, frame shimwire.Frame, id, deliveryID string, bound time.Duration) (shimwire.Frame, error) {
	if bound > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, bound, ErrNoAnswer)
		defer cancel()
	}
	answer := make(chan shimwire.Frame, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("worker-stream: %s %s: %w", c.claim, frame.FrameType(), ErrClosed)
	}
	c.pending[id] = pendingRequest{answer: answer, deliveryID: deliveryID}
	if deliveryID != "" {
		c.sent[id] = struct{}{}
		// A new prompt supersedes the last one's claim to a late refusal (worker-rpc.ts:450).
		c.acked = nil
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.writeWithin(ctx, frame); err != nil {
		return nil, err
	}
	select {
	case got := <-answer:
		return got, nil
	case <-c.done:
		// The reader hands an answer over before it ends, so one that raced the close is
		// still the answer.
		select {
		case got := <-answer:
			return got, nil
		default:
		}
		return nil, fmt.Errorf("worker-stream: %s %s: %w before it was answered", c.claim, frame.FrameType(), ErrClosed)
	case <-ctx.Done():
		return nil, fmt.Errorf("worker-stream: %s %s: %w", c.claim, frame.FrameType(), context.Cause(ctx))
	}
}

// write writes a frame outside a request. No context bounds it, so the RPC timeout of
// connWriter.Write bounds its write.
func (c *Conn) write(frame shimwire.Frame) error {
	return c.writeWithin(context.Background(), frame)
}

// writeWithin writes frame within ctx: its wait for the writer and its write.
func (c *Conn) writeWithin(ctx context.Context, frame shimwire.Frame) error {
	if err := c.writer.WriteFrameContext(ctx, frame); err != nil {
		return fmt.Errorf("worker-stream: %s %s: %w", c.claim, frame.FrameType(), err)
	}
	return nil
}

// read is the connection's reader: every line from the shim, rpc_chunk sequences reassembled,
// dispatched in wire order until the stream ends.
func (c *Conn) read(src io.Reader) {
	reader := shimwire.NewReader(src)
	var chunks shimwire.Reassembler
	for {
		line, err := reader.ReadLine()
		if err != nil {
			if errors.Is(err, shimwire.ErrLineTooLong) {
				c.log.Warn("worker-stream: closing a stream that sent a line over the frame limit", "claim", c.claim, "error", err)
			}
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			c.logNotAFrame(line, err)
			continue
		}
		if chunk, ok := frame.(shimwire.RPCChunk); ok {
			whole, err := chunks.Push(chunk)
			if err != nil {
				c.log.Warn("worker-stream: dropped an rpc_chunk sequence", "claim", c.claim, "error", err)
				continue
			}
			if whole == nil {
				continue
			}
			if frame, err = shimwire.Decode(whole); err != nil {
				c.logNotAFrame(whole, err)
				continue
			}
		} else if err := chunks.Interrupt(); err != nil {
			c.log.Warn("worker-stream: dropped an rpc_chunk sequence", "claim", c.claim, "error", err)
		}
		c.dispatch(frame)
	}
}

// logNotAFrame names a line the reader could not use without quoting it: a line that is not
// JSON at all could be anything the wrapped process printed.
func (c *Conn) logNotAFrame(line []byte, err error) {
	if errors.Is(err, shimwire.ErrNotAFrame) {
		c.log.Warn("worker-stream: ignoring a line that is not a JSON object", "claim", c.claim, "bytes", len(line))
		return
	}
	c.log.Warn("worker-stream: ignoring a malformed frame", "claim", c.claim, "error", err)
}

// dispatch acts on one whole frame. A tool call starting or ending is pushed as the length and
// the fingerprint of its arguments (ToolStep), which is everything the supervisor records of
// them and the only thing computed here: their text stops at this reader, so no queue of the
// daemon's holds a credential a shell command carried. An assistant message's end is pushed as
// the model that answered and nothing of its text (answeredBy). OMP's other events — message
// updates, errors — are not facts the daemon supervises on, and are dropped, except an extension's
// failure, which leaves the agent running without what the extension gives it (the Legion
// plugin's tool, when its session_start fails) and is on no other record the daemon reads, so it
// is logged.
//
// A prompt_result whose request id names no delivery is dropped: every prompt this daemon sends
// carries one (Prompt), so such a frame answers a `prompt` command something else wrote.
//
// A launch's last model totals are pushed as FinalTotals. A model_totals_final frame that is not
// valid is dropped, with one line naming the claim.
func (c *Conn) dispatch(frame shimwire.Frame) {
	switch frame := frame.(type) {
	case shimwire.AgentStart:
		c.events.push(TurnStart{Claim: c.claim, DeliveryID: frame.DeliveryID})
	case shimwire.AgentEnd:
		c.mu.Lock()
		c.acked = nil
		c.mu.Unlock()
		c.events.push(TurnEnd{Claim: c.claim})
	case shimwire.ToolExecutionStart:
		c.events.push(ToolStep{Claim: c.claim, Tool: frame.ToolName, CallID: frame.ToolCallID,
			ArgsBytes: len(frame.Args), ArgsHash: stepArgsHash(frame.Args), ConnSequence: c.seq})
	case shimwire.ToolExecutionEnd:
		c.events.push(ToolStep{Claim: c.claim, Tool: frame.ToolName, CallID: frame.ToolCallID,
			End: true, ConnSequence: c.seq})
	case shimwire.PromptResult:
		if delivery := promptDelivery(frame.ID); delivery != "" {
			event := PromptResult{
				Claim: c.claim, DeliveryID: delivery, AgentInvoked: frame.AgentInvoked, Error: frame.Reason(),
			}
			if frame.Error != nil {
				// Oh My Pi's error object, not the message alone: the structured members are what
				// a failure is judged on, its class (Code) among them.
				event.Retryable = frame.Error.Retryable
				event.HTTPStatus = frame.Error.HTTPStatus
				event.Provider = frame.Error.Provider
				event.Model = frame.Error.Model
				event.Code = frame.Error.Code
			}
			c.events.push(event)
		}
	case shimwire.Response:
		c.answer(frame)
	case shimwire.Raw:
		switch frame.Type {
		case "extension_error":
			var event struct {
				ExtensionPath string `json:"extensionPath"`
				Event         string `json:"event"`
				Error         string `json:"error"`
			}
			_ = json.Unmarshal(frame.JSON, &event)
			c.log.Warn("worker-stream: an extension of the worker's Oh My Pi failed", "claim", c.claim,
				"extension", event.ExtensionPath, "event", event.Event, "error", event.Error)
		case "message_end":
			if answered, ok := c.answeredBy(frame.JSON); ok {
				c.events.push(answered)
			}
		}
	case shimwire.AdoptWorkingCopyResult:
		c.answerShim(frame.ID, frame)
	case shimwire.AgentSecretsEnrollmentResult:
		c.answerShim(frame.ID, frame)
	case shimwire.AgentSecretsTokenResult:
		c.answerShim(frame.ID, frame)
	case shimwire.WorkspaceWorkResult:
		c.answerShim(frame.ID, frame)
	case shimwire.ModelTotalsResult:
		c.answerShim(frame.ID, frame)
	case shimwire.ModelTotalsFinal:
		if err := frame.Validate(); err != nil {
			c.log.Warn("worker-stream: dropped a model_totals_final frame that is not valid", "claim", c.claim, "error", err)
			return
		}
		c.events.push(FinalTotals{Claim: c.claim, Generation: c.generation, Totals: frame.Totals})
	}
}

// stepArgsHash is the fingerprint of a call's arguments: the first eight bytes of their SHA-256,
// hex. It is what tells two calls of one tool with different arguments apart in the record,
// where their text can never be — a shell command routinely holds a credential
// (supervise.StepRecord) — and eight bytes are a fingerprint for a human reading two rows, not a
// proof of anything. Arguments nobody sent hash to nothing at all.
func stepArgsHash(args []byte) string {
	if len(args) == 0 {
		return ""
	}
	sum := sha256.Sum256(args)
	return hex.EncodeToString(sum[:8])
}

// answeredBy is the Answered a message_end read on this connection makes, under the connection's
// claim and the generation its hello was registered for: an assistant message whose message.model
// is set and is not shimwire.SyntheticModel. A user's message, a tool's result and a message no
// model wrote answer nothing, and neither does a frame that is not the shape Oh My Pi writes
// (internal/testbin's omp frames, omp://rpc.md).
//
// A message whose provider or model holds U+0000 did answer, with a model no store can keep
// (PostgreSQL's text and jsonb refuse that character), and the model an answer names is written
// into the record of the step it served, in the transaction that ends the step, which would fail
// with it. This is the one place a frame becomes an Answered, so every reader after it holds a
// storable model: such a message is an Answered that names neither its provider nor its model,
// with one line naming the claim and the field, never its text. So the model that answered before
// it is not the last one, and the step's record names no model, as for an agent that named none.
func (c *Conn) answeredBy(raw json.RawMessage) (Answered, bool) {
	var frame struct {
		Message struct {
			Role     string `json:"role"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return Answered{}, false
	}
	m := frame.Message
	if m.Role != "assistant" || m.Model == "" || m.Model == shimwire.SyntheticModel {
		return Answered{}, false
	}
	var unstorable []string
	if strings.ContainsRune(m.Provider, 0) {
		unstorable = append(unstorable, "message.provider")
	}
	if strings.ContainsRune(m.Model, 0) {
		unstorable = append(unstorable, "message.model")
	}
	answered := Answered{Claim: c.claim, Generation: c.generation}
	if len(unstorable) > 0 {
		c.log.Warn("worker-stream: dropped the model a message_end named: it holds U+0000, which no store can keep",
			"claim", c.claim, "field", strings.Join(unstorable, " and "))
		return answered, true
	}
	answered.Provider, answered.Model = m.Provider, m.Model
	return answered, true
}

// answerShim hands a frame the shim answered itself to the request waiting on its id; one nobody
// waits on any longer (its request timed out) is dropped.
func (c *Conn) answerShim(id string, frame shimwire.Frame) {
	c.mu.Lock()
	request, waiting := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if waiting {
		request.answer <- frame
	}
}

// answer routes a response to the request waiting on its id. One nobody is waiting on is either
// a success for a request that gave up — the shim fans OMP's one answer out to every request id
// of a retried delivery (worker-shim.ts:254-270), and a retry's earlier ids are long abandoned —
// or a refusal. A refusal reverses the acknowledged prompt it names; one of a prompt this
// connection never sent, replayed from the shim's backlog, is a replayed LateRefusal for the
// delivery its request id names; any other is logged.
func (c *Conn) answer(response shimwire.Response) {
	c.mu.Lock()
	request, waiting := c.pending[response.ID]
	_, sentHere := c.sent[response.ID]
	var late *ackedPrompt
	var earlier string
	refusal := response.Command == shimwire.TypePrompt && !response.Success
	switch {
	case waiting:
		delete(c.pending, response.ID)
		if request.deliveryID != "" && response.Command == shimwire.TypePrompt && response.Success {
			c.acked = &ackedPrompt{requestID: response.ID, deliveryID: request.deliveryID}
		}
	case refusal && c.acked != nil && c.acked.requestID == response.ID:
		late, c.acked = c.acked, nil
	case refusal && !sentHere:
		earlier = promptDelivery(response.ID)
	}
	c.mu.Unlock()

	switch {
	case waiting:
		request.answer <- response
	case late != nil:
		c.log.Warn("worker-stream: prompt refused after its acknowledgement",
			"claim", c.claim, "deliveryId", late.deliveryID, "error", response.Error)
		c.events.push(LateRefusal{Claim: c.claim, DeliveryID: late.deliveryID, Error: response.Error, ConnSequence: c.seq})
	case earlier != "":
		c.log.Warn("worker-stream: a refusal replayed from an earlier connection names its delivery",
			"claim", c.claim, "deliveryId", earlier, "error", response.Error)
		c.events.push(LateRefusal{Claim: c.claim, DeliveryID: earlier, Error: response.Error, Replayed: true, ConnSequence: c.seq})
	case refusal:
		c.log.Warn("worker-stream: refusal for a prompt request this connection is not waiting on",
			"claim", c.claim, "request", response.ID, "error", response.Error)
	}
}

// finish marks the connection ended once its reader has returned: every request still waiting
// is released with ErrClosed, and every later one fails at once.
func (c *Conn) finish() {
	c.mu.Lock()
	c.closed = true
	c.acked = nil
	c.mu.Unlock()
	close(c.done)
}

// connWriter is the connection as the frame writer sees it. Every write ends at the RPC timeout
// at the latest, since a shim that stops reading must not hold the writer. A request's write also
// ends with the request's context, whichever comes first. A write that fails closes the
// connection: a line written in part leaves the peer mid-frame, and nothing after it could be
// read as the frame it is. The frame writer makes exactly one write per frame, while the frame
// holds the writer's slot, so the deadline set here is that frame's alone.
type connWriter struct {
	nc      net.Conn
	timeout time.Duration
}

var _ shimwire.ContextWriter = connWriter{}

// aLongTimeAgo is a write deadline already past. Setting it ends a write that waits on the peer.
var aLongTimeAgo = time.Unix(1, 0)

// WriteContext writes p within the RPC timeout and within ctx, whichever ends first. While p's own
// bytes are written, a cut is armed on ctx. The cut sets a deadline already past, which ends the
// blocked write. A cut write returns ctx's cause and closes the connection, as any failed write
// does. A cut that fires after the write returned is waited for, so its deadline is set before the
// next frame sets its own. A context that can never end arms no cut.
func (w connWriter) WriteContext(ctx context.Context, p []byte) (int, error) {
	if ctx.Err() != nil {
		return 0, context.Cause(ctx)
	}
	if err := w.nc.SetWriteDeadline(time.Now().Add(w.timeout)); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrClosed, err)
	}
	if ctx.Done() == nil {
		return w.closeOnError(w.nc.Write(p))
	}
	cutDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(cutDone)
		_ = w.nc.SetWriteDeadline(aLongTimeAgo)
	})
	n, err := w.nc.Write(p)
	if !stop() {
		<-cutDone
		if err != nil {
			w.nc.Close()
			return n, fmt.Errorf("%w: the write was cut: %w", ErrClosed, context.Cause(ctx))
		}
	}
	return w.closeOnError(n, err)
}

// Write writes p within the RPC timeout.
func (w connWriter) Write(p []byte) (int, error) {
	return w.WriteContext(context.Background(), p)
}

// closeOnError closes the connection after a write that failed, since the peer may hold part of a
// frame.
func (w connWriter) closeOnError(n int, err error) (int, error) {
	if err != nil {
		w.nc.Close()
		return n, fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return n, nil
}
