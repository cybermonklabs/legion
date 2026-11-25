// Package agentstream relays a live agent session's own conversation — its turns, tool calls and
// streamed output — from the session to a human watching it in Dispatch (LEGION-232).
//
// Nothing here is stored. A frame is forwarded to whoever has the session open and then
// forgotten: no database row, no file, and no log line carrying a frame body. Tool output can
// hold anything the tool printed, secrets included, so the relay treats every frame as opaque
// bytes it never reads into and never records. The transport is core NATS on a subject family
// the notification stream does not capture, so the bus retains nothing either.
package agentstream

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Frame is one frame exactly as the session published it. Its shape belongs to the session and
// the dashboard (packages/contracts/src/agent-stream.ts); the relay never parses it. A nil Frame
// is reserved where frames are delivered (Subscribe below): it is the relay ending a viewer's
// subscription (api/live_agent.go), which Memory.Drop alone produces, and no source publishes one
// as a session's own output. Replay's nil is a different answer, an empty history.
type Frame []byte

// controlReplay asks a session for the history it can still replay; controlWatch tells it a
// viewer is attached, which is what licenses it to publish at all. Both mirror
// AgentStreamControlMessage in packages/contracts/src/agent-stream.ts.
const (
	controlReplay = `{"v":1,"type":"replay"}`
	controlWatch  = `{"v":1,"type":"watch"}`
)

// Source is where a viewer's frames come from.
type Source interface {
	// Subscribe delivers each frame the session publishes until stop is called. deliver runs on
	// the transport's own goroutine and must not block.
	Subscribe(sessionID string, deliver func(Frame)) (stop func(), err error)
	// Replay asks the session for what it can still replay. A session answering with nothing
	// returns a nil frame and no error: an empty history is an answer. A session with nobody
	// listening on its control subject returns ErrNoResponder, which is a different statement
	// and the only thing that tells a session that cannot stream from one that has not spoken
	// yet - every plugin release that can stream answers, and none that cannot does.
	Replay(ctx context.Context, sessionID string) (Frame, error)
	// Watch tells the session a viewer is attached. A session hears nothing for long enough
	// stops publishing, so a viewer that stays open keeps calling it.
	Watch(sessionID string) error
}

// ErrNoResponder is a replay request nobody was listening for. It is not a failure of the relay
// and not an empty history: it says this session is not on the live view's control subject.
var ErrNoResponder = errors.New("agent stream: no responder on the session's control subject")

// NATS is the production Source: core NATS, never JetStream.
type NATS struct {
	conn *nats.Conn
}

// NewNATS returns a Source over conn. conn should be a connection of the relay's own (bus.Dial),
// so a viewer's subscription cannot be lost to another component replacing a shared one.
func NewNATS(conn *nats.Conn) *NATS {
	return &NATS{conn: conn}
}

func (n *NATS) Subscribe(sessionID string, deliver func(Frame)) (func(), error) {
	subscription, err := n.conn.Subscribe(contracts.AgentStreamFramesSubject(sessionID), func(message *nats.Msg) {
		// nats.go reuses the message buffer, so the frame is copied before it leaves the callback.
		frame := make(Frame, len(message.Data))
		copy(frame, message.Data)
		deliver(frame)
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe to agent stream: %w", err)
	}
	return func() { _ = subscription.Unsubscribe() }, nil
}

// Replay asks on a reply subject that names the session (bus.SessionReplySubject under a new
// inbox): a session's own grant (natsgrant.Session) publishes bus.SessionReplies, its replies
// under its own id alone, in place of every _INBOX. subject, so a session on that grant cannot
// answer nats.go's own request inbox, and no other session on such a grant can answer this one. A
// holder of the shared session user or of a host user subscribes every session's control subject
// and may publish to every _INBOX. subject, so it still can, and Replay returns whatever history
// it sends. The answer is taken over a channel subscription of this request's own, as
// bus.Client.RequestCore takes the connection probe's.
func (n *NATS) Replay(ctx context.Context, sessionID string) (Frame, error) {
	reply := bus.SessionReplySubject(nats.NewInbox(), sessionID)
	replies := make(chan *nats.Msg, nats.RequestChanLen)
	subscription, err := n.conn.ChanSubscribe(reply, replies)
	if err != nil {
		return nil, fmt.Errorf("request agent stream replay: %w", err)
	}
	defer func() { _ = subscription.Unsubscribe() }()
	if err := n.conn.PublishRequest(contracts.AgentStreamControlSubject(sessionID), reply, []byte(controlReplay)); err != nil {
		return nil, fmt.Errorf("request agent stream replay: %w", err)
	}
	select {
	case answer := <-replies:
		if len(answer.Data) == 0 && answer.Header.Get("Status") == "503" {
			// Nobody is listening on the session's control subject: the server's no-responders
			// status.
			return nil, ErrNoResponder
		}
		frame := make(Frame, len(answer.Data))
		copy(frame, answer.Data)
		return frame, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Somebody may well be there and slow: the viewer gets the live stream with no history
			// behind it, which is what a session that just started offers anyway.
			return nil, nil
		}
		return nil, fmt.Errorf("request agent stream replay: %w", ctx.Err())
	}
}

func (n *NATS) Watch(sessionID string) error {
	if err := n.conn.Publish(contracts.AgentStreamControlSubject(sessionID), []byte(controlWatch)); err != nil {
		return fmt.Errorf("publish agent stream watch: %w", err)
	}
	return nil
}
