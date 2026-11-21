package stream

import (
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Event is one agent fact the stream observed. The set is sealed: a connection arriving
// (Hello), OMP starting and ending a turn (TurnStart, TurnEnd), OMP's answer to whether a turn is
// running (TurnAnswered), one tool call of a turn starting or ending (ToolStep), the model that
// answered (Answered), and the connection going away (Closed); and one mark that is no agent
// fact, the place in the queue a consumer asked for (Drained).
type Event interface{ isEvent() }

// AgentSecretsIdentity is the pod's session identity a hello2 carried (shimwire.AgentSecretsHello):
// the thumbprint of the key the pod generated and its projected token for the secrets broker's
// audience. Nil for a pane, or a pod whose runtime does not enroll.
type AgentSecretsIdentity struct {
	Thumbprint, PodToken string
}

// Hello is a shim's hello accepted: the claim's connection is registered, at the generation its
// boot token was minted for, with the identity the hello carried, if any. Under always-dial the
// shim says hello before it spawns Oh My Pi, so a Hello says the pane's bridge is up, not that the
// agent is.
type Hello struct {
	Claim        claim.Token
	Generation   uint64
	AgentSecrets *AgentSecretsIdentity
}

// TurnStart is OMP's agent_start. It never names what caused the turn; which task a turn is the
// turn of is the supervisor's rule (a turn that starts after an accepted take, supervise's
// take.go).
type TurnStart struct {
	Claim claim.Token
}

// TurnEnd is OMP's agent_end: the turn is over.
type TurnEnd struct {
	Claim claim.Token
}

// TurnAnswered is OMP's answer to a get_state the daemon asked (Conn.GetState): whether a turn is
// running. The reader pushes it as it reads that answer (Conn.dispatch), so it follows every event
// of a frame the connection read before the answer — a shim's replay of the frames it held while
// no daemon was attached among them, which the shim writes before any later frame — and the
// supervisor reads it with those frames already applied. It is pushed only for a get_state the
// connection it arrives on wrote and still waits on: an answer to an earlier connection's ask,
// which the shim replays from its backlog, is none (Conn.stateAnswered). Ask is the id of the
// get_state it answers, which the asker's own answer names too (runtime.ConnState.Ask), so the
// asker can tell this event from any other answer's.
type TurnAnswered struct {
	Claim     claim.Token
	Streaming bool
	Ask       string
}

// ToolStep is one tool call of the agent's turn beginning or ending
// (shimwire.ToolExecutionStart, shimwire.ToolExecutionEnd): the only fact on the wire that says
// an agent mid-turn is still moving. The connection measures the start's arguments (below) and
// nothing else — the supervisor stamps the step and decides what it means — because this is the
// one event that arrives per tool call rather than per turn.
//
// ArgsBytes is the length of the start's arguments as Oh My Pi wrote them and ArgsHash their
// fingerprint (stepArgsHash), both measured here and both zero for an end, which carries no
// arguments. The arguments' own text goes no further than the reader that parsed the frame: it
// routinely holds a credential, and this event waits in two unbounded in-memory queues (the
// queue below and the supervisor's) until a machine handles it. End says which of the two frames
// this is. CallID is what pairs an end with its start, and the only thing that does; the Claude
// Code bridge writes a start with none (internal/claudebridge), which is a step with no call to
// name.
type ToolStep struct {
	Claim     claim.Token
	Tool      string
	CallID    string
	ArgsBytes int
	ArgsHash  string
	End       bool
	// ConnSequence is the registration sequence of the connection the frame arrived on
	// (Conn.Sequence): the supervisor compares it with the claim's live connection, and a frame
	// of a connection that has since been replaced says what the agent was doing, not what it is
	// doing now.
	ConnSequence uint64
}

// Answered is OMP's message_end of an assistant message: the model that answered, under
// message.provider and message.model, which is the model that really served the turn, also after a
// fallback moved it off the one the session started with. A message no model wrote names none (an
// empty model, or Claude Code's `<synthetic>` through the bridge) and is no Answered; a message
// whose provider or model holds U+0000 did answer, and is an Answered that names neither (Conn's
// answeredBy). It carries nothing of the message's text.
//
// Generation is the launch the connection that read the frame was registered for, as its Hello
// names it. A spawn starts a session and a resume continues it, so the machine takes an answer of
// any launch from the one that spawned the claim's session (supervise's Machine.sessionFrom), and
// drops one of a process whose session the claim has left, which can wait in the queues behind the
// registration of the next session.
type Answered struct {
	Claim           claim.Token
	Provider, Model string
	Generation      uint64
}

// Closed is a registered connection gone, whichever side ended it. It is emitted exactly once
// per Hello, after every other event of that connection and before the Hello of any connection
// that replaces it.
type Closed struct {
	Claim claim.Token
}

// Drained is the mark Listener.Drain queued: every event the listener had queued when it was
// asked for comes before it on Events(). It is no agent fact. The consumer closes Done once it
// has handled each event of Claim that came before the mark, which is what its asker waits on.
type Drained struct {
	Claim claim.Token
	Done  chan<- struct{}
}

func (Hello) isEvent()        {}
func (TurnStart) isEvent()    {}
func (TurnEnd) isEvent()      {}
func (TurnAnswered) isEvent() {}
func (ToolStep) isEvent()     {}
func (Answered) isEvent()     {}
func (Closed) isEvent()       {}
func (Drained) isEvent()      {}

// eventQueue is the ordered, unbounded hand-off between every connection's reader and the one
// channel Events() returns.
//
// Unbounded because a connection's reader is also the goroutine that answers its requests: a
// reader parked on a full channel would leave a GetState answer unread behind it, and a consumer
// that asked for that state before draining the channel would wait on itself. The shipped
// listener has the same property by construction — its callbacks never wait on a consumer. What
// grows it is one small struct per turn boundary, not per frame.
type eventQueue struct {
	mu     sync.Mutex
	items  []Event
	wake   chan struct{}
	closed bool
	out    chan Event
}

func newEventQueue() *eventQueue {
	q := &eventQueue{wake: make(chan struct{}, 1), out: make(chan Event)}
	go q.forward()
	return q
}

func (q *eventQueue) push(event Event) {
	q.mu.Lock()
	q.items = append(q.items, event)
	q.mu.Unlock()
	q.signal()
}

// close ends the queue: every event already pushed is still delivered, then the channel closes.
// Nothing may push after it; the listener calls it once every connection's reader has returned.
func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) forward() {
	defer close(q.out)
	for range q.wake {
		q.mu.Lock()
		items, closed := q.items, q.closed
		q.items = nil
		q.mu.Unlock()
		for _, event := range items {
			q.out <- event
		}
		if closed {
			return
		}
	}
}
